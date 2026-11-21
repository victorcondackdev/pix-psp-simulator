package sim

import (
	"errors"
	"time"

	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/fsm"
	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/internal/notify"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

// Refund is a devolução (pacs.004-like): money returned from the payee to
// the payer of a settled transfer, fully or partially.
type Refund struct {
	ID                string    `json:"id"` // ReturnIdentification, "D" + ISPB + time + sequence
	EndToEndID        string    `json:"end_to_end_id"`
	AmountCents       int64     `json:"amount_cents"`
	ReturnReason      string    `json:"return_reason"`
	Status            string    `json:"status"`
	Reason            string    `json:"reason,omitempty"`
	ReasonDescription string    `json:"reason_description,omitempty"`
	Description       string    `json:"description,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	InfractionID      string    `json:"infraction_id,omitempty"`
}

// RefundRequest requests a devolução.
type RefundRequest struct {
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason,omitempty"` // MD06 (default), BE08, FR01, SL02
	Description string `json:"description,omitempty"`
}

func (s *Sim) returnID(now time.Time) (string, error) {
	id, err := pix.NewEndToEndID(s.cfg.ISPB, now, s.rand)
	if err != nil {
		return "", err
	}
	return "D" + id[1:], nil
}

// CreateRefund returns up to the remaining refundable amount of a settled
// transfer. It settles synchronously: the refund is ACSC, or RJCT with AM04
// when the payee no longer has the funds (or a chaos rule says so).
func (s *Sim) CreateRefund(e2e string, r RefundRequest, ov *chaos.Overrides) (Refund, error) {
	if r.Reason == "" {
		r.Reason = iso.ReturnRequested
	}
	if !iso.ValidReturnReason(r.Reason) {
		return Refund{}, invalid("invalid_return_reason", "reason must be one of BE08, FR01, MD06, SL02")
	}
	if r.Reason == iso.ReturnFraud {
		return Refund{}, invalid("invalid_return_reason", "FR01 refunds are created by the MED flow")
	}
	return s.refund(e2e, r, ov, "", "")
}

// refund moves money back; source is the payee account, or MEDHold.
func (s *Sim) refund(e2e string, r RefundRequest, ov *chaos.Overrides, source, infractionID string) (Refund, error) {
	s.mu.Lock()
	ref, after, err := s.refundLocked(e2e, r, ov, source, infractionID)
	s.mu.Unlock()
	if after != nil {
		after()
	}
	return ref, err
}

// refundLocked does the work of refund; the caller holds s.mu and must call
// the returned function, if any, after unlocking (it publishes webhooks).
func (s *Sim) refundLocked(e2e string, r RefundRequest, ov *chaos.Overrides, source, infractionID string) (Refund, func(), error) {
	t, ok := s.transfers[e2e]
	if !ok {
		return Refund{}, nil, notFound("transfer_not_found", "transfer %s not found", e2e)
	}
	if t.Status != iso.StatusSettled {
		return Refund{}, nil, conflict("transfer_not_settled", "only settled transfers can be refunded (status %s)", t.Status)
	}
	if r.AmountCents <= 0 || r.AmountCents > t.AmountCents-t.RefundedCents {
		return Refund{}, nil, invalid("refund_exceeds_amount", "amount_cents must be between 1 and the refundable %d", t.AmountCents-t.RefundedCents)
	}
	now := s.Clock.Now()
	id, err := s.returnID(now)
	if err != nil {
		return Refund{}, nil, err
	}
	ref := Refund{
		ID: id, EndToEndID: e2e, AmountCents: r.AmountCents, ReturnReason: r.Reason, Status: iso.StatusSettled,
		Description: r.Description, CreatedAt: now, InfractionID: infractionID,
	}
	dec := s.Chaos.Evaluate(chaos.Op{Operation: chaos.OpRefund, FromAccount: t.Payee.AccountID, ToAccount: t.Payer.AccountID, Amount: r.AmountCents}, ov)
	if source == "" {
		source = t.Payee.AccountID
	}
	if dec.Fail != "" {
		ref.Status, ref.Reason = iso.StatusRejected, dec.Fail
	} else if _, err := s.Ledger.Move(source, t.Payer.AccountID, r.AmountCents, id, "refund of "+e2e); err != nil {
		code := iso.AM04
		if !errors.Is(err, ledger.ErrInsufficientFunds) {
			code = iso.DS04
		}
		ref.Status, ref.Reason = iso.StatusRejected, code
	}
	if ref.Status == iso.StatusRejected {
		ref.ReasonDescription = iso.Reasons[ref.Reason]
		t.Refunds = append(t.Refunds, ref)
		return ref, nil, nil
	}
	t.RefundedCents += r.AmountCents
	t.Refunds = append(t.Refunds, ref)
	payer, payee := t.Payer.AccountID, t.Payee.AccountID
	evPayer, evPayee := s.nextEventID(), s.nextEventID()
	opts := notify.PublishOptions{Duplicates: dec.Duplicates, Reorder: dec.Reorder}
	return ref, func() {
		s.publish(evPayer, webhook.EventRefundCompleted, payer, ref, opts)
		s.publish(evPayee, webhook.EventRefundCompleted, payee, ref, opts)
	}, nil
}

// Refund returns one refund of a transfer.
func (s *Sim) Refund(e2e, id string) (Refund, error) {
	t, err := s.Transfer(e2e)
	if err != nil {
		return Refund{}, err
	}
	for _, r := range t.Refunds {
		if r.ID == id {
			return r, nil
		}
	}
	return Refund{}, notFound("refund_not_found", "refund %s not found", id)
}

// MED (Mecanismo Especial de Devolução) — simplified model.

// InfractionStatus is the state of a MED infraction report.
type InfractionStatus string

// Infraction states.
const (
	MEDOpen      InfractionStatus = "OPEN"      // infraction reported by the payer's PSP
	MEDBlocked   InfractionStatus = "BLOCKED"   // cautionary block on the payee's funds
	MEDAnalysis  InfractionStatus = "ANALYSIS"  // payee's PSP analysing the report
	MEDRefunded  InfractionStatus = "REFUNDED"  // accepted: held funds returned (FR01)
	MEDClosed    InfractionStatus = "CLOSED"    // rejected after analysis: funds released
	MEDCancelled InfractionStatus = "CANCELLED" // withdrawn by the reporter
)

// InfractionEvent drives the MED state machine.
type InfractionEvent string

// Infraction events.
const (
	MEDBlock   InfractionEvent = "block"
	MEDAnalyze InfractionEvent = "analyze"
	MEDAccept  InfractionEvent = "accept"
	MEDReject  InfractionEvent = "reject"
	MEDCancel  InfractionEvent = "cancel"
)

// MEDMachine is the simplified MED lifecycle:
//
//	OPEN --block--> BLOCKED --analyze--> ANALYSIS --accept--> REFUNDED
//	                                              \--reject--> CLOSED
//	OPEN|BLOCKED --cancel--> CANCELLED
var MEDMachine = fsm.New([]fsm.Transition[InfractionStatus, InfractionEvent]{
	{From: MEDOpen, Event: MEDBlock, To: MEDBlocked},
	{From: MEDOpen, Event: MEDCancel, To: MEDCancelled},
	{From: MEDBlocked, Event: MEDAnalyze, To: MEDAnalysis},
	{From: MEDBlocked, Event: MEDCancel, To: MEDCancelled},
	{From: MEDAnalysis, Event: MEDAccept, To: MEDRefunded},
	{From: MEDAnalysis, Event: MEDReject, To: MEDClosed},
}, MEDRefunded, MEDClosed, MEDCancelled)

// Step is one transition in an infraction's history.
type Step struct {
	From  InfractionStatus `json:"from"`
	Event InfractionEvent  `json:"event"`
	To    InfractionStatus `json:"to"`
	At    time.Time        `json:"at"`
	Note  string           `json:"note,omitempty"`
}

// Infraction is a MED infraction report on a transfer.
type Infraction struct {
	ID         string            `json:"id"`
	EndToEndID string            `json:"end_to_end_id"`
	Reason     string            `json:"reason"`
	Details    string            `json:"details,omitempty"`
	Status     InfractionStatus  `json:"status"`
	HeldCents  int64             `json:"held_cents"`
	RefundID   string            `json:"refund_id,omitempty"`
	Allowed    []InfractionEvent `json:"allowed_events"`
	History    []Step            `json:"history"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	payeeAcct  string
}

func (i *Infraction) snapshot() Infraction {
	c := *i
	c.History = append([]Step{}, i.History...)
	c.Allowed = MEDMachine.Events(i.Status)
	return c
}

// InfractionRequest reports an infraction.
type InfractionRequest struct {
	EndToEndID string `json:"end_to_end_id"`
	Reason     string `json:"reason,omitempty"` // FRAUD (default) or REFUND_REQUEST
	Details    string `json:"details,omitempty"`
}

// ReportInfraction opens a MED infraction on a settled transfer.
func (s *Sim) ReportInfraction(r InfractionRequest) (Infraction, error) {
	if r.Reason == "" {
		r.Reason = "FRAUD"
	}
	if r.Reason != "FRAUD" && r.Reason != "REFUND_REQUEST" {
		return Infraction{}, invalid("invalid_reason", "reason must be FRAUD or REFUND_REQUEST")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[r.EndToEndID]
	if !ok {
		return Infraction{}, notFound("transfer_not_found", "transfer %s not found", r.EndToEndID)
	}
	if t.Status != iso.StatusSettled {
		return Infraction{}, conflict("transfer_not_settled", "infractions apply to settled transfers (status %s)", t.Status)
	}
	for _, id := range s.infOrder {
		if i := s.infractions[id]; i.EndToEndID == r.EndToEndID && !MEDMachine.Terminal(i.Status) {
			return Infraction{}, conflict("infraction_in_progress", "transfer already has open infraction %s", i.ID)
		}
	}
	now := s.Clock.Now()
	inf := &Infraction{
		ID: "inf_" + s.randomHex(6), EndToEndID: r.EndToEndID, Reason: r.Reason, Details: r.Details,
		Status: MEDOpen, History: []Step{}, CreatedAt: now, UpdatedAt: now, payeeAcct: t.Payee.AccountID,
	}
	s.infractions[inf.ID] = inf
	s.infOrder = append(s.infOrder, inf.ID)
	return inf.snapshot(), nil
}

// Infraction returns an infraction by ID.
func (s *Sim) Infraction(id string) (Infraction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.infractions[id]
	if !ok {
		return Infraction{}, notFound("infraction_not_found", "infraction %s not found", id)
	}
	return i.snapshot(), nil
}

// Infractions lists infractions newest first.
func (s *Sim) Infractions() []Infraction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Infraction{}
	for i := len(s.infOrder) - 1; i >= 0; i-- {
		out = append(out, s.infractions[s.infOrder[i]].snapshot())
	}
	return out
}

// ActOnInfraction applies an event. Side effects:
//   - block: moves min(refundable amount, payee balance) to the MED hold.
//   - accept: refunds the held amount to the payer with reason FR01.
//   - reject / cancel after block: releases the hold to the payee.
func (s *Sim) ActOnInfraction(id string, ev InfractionEvent) (Infraction, error) {
	s.mu.Lock()
	inf, ok := s.infractions[id]
	if !ok {
		s.mu.Unlock()
		return Infraction{}, notFound("infraction_not_found", "infraction %s not found", id)
	}
	next, err := MEDMachine.Next(inf.Status, ev)
	if err != nil {
		s.mu.Unlock()
		return inf.snapshot(), &Error{Kind: KindConflict, Code: "invalid_transition", Msg: err.Error(), Err: err}
	}
	t := s.transfers[inf.EndToEndID]
	step := Step{From: inf.Status, Event: ev, To: next, At: s.Clock.Now()}
	switch ev {
	case MEDBlock:
		bal, _ := s.Ledger.Get(inf.payeeAcct)
		hold := t.AmountCents - t.RefundedCents
		if bal.Balance < hold {
			hold = bal.Balance
		}
		if hold > 0 {
			if _, err := s.Ledger.Move(inf.payeeAcct, ledger.MEDHold, hold, inf.ID, "MED cautionary block"); err != nil {
				hold = 0
			}
		}
		inf.HeldCents = hold
		if hold < t.AmountCents-t.RefundedCents {
			step.Note = "partial block: payee balance was insufficient"
		}
	case MEDReject, MEDCancel:
		if inf.HeldCents > 0 {
			_, _ = s.Ledger.Move(ledger.MEDHold, inf.payeeAcct, inf.HeldCents, inf.ID, "MED block released")
			inf.HeldCents = 0
		}
	}
	var after func()
	if ev == MEDAccept {
		if inf.HeldCents > 0 {
			ref, fn, err := s.refundLocked(inf.EndToEndID, RefundRequest{AmountCents: inf.HeldCents, Reason: iso.ReturnFraud, Description: "MED " + inf.ID}, nil, ledger.MEDHold, inf.ID)
			if err != nil || ref.Status != iso.StatusSettled {
				// Return the hold to the payee rather than leaving it stranded.
				_, _ = s.Ledger.Move(ledger.MEDHold, inf.payeeAcct, inf.HeldCents, inf.ID, "MED refund failed, block released")
				step.Note = "refund failed; block released"
			} else {
				inf.RefundID, after = ref.ID, fn
			}
		} else {
			step.Note = "nothing was blocked; no funds returned"
		}
		inf.HeldCents = 0
	}
	inf.Status = next
	inf.UpdatedAt = step.At
	inf.History = append(inf.History, step)
	out := inf.snapshot()
	s.mu.Unlock()
	if after != nil {
		after()
	}
	return out, nil
}
