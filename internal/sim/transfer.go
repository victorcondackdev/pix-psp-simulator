package sim

import (
	"errors"
	"time"

	"github.com/victorcondackdev/brpay/document"
	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/dict"
	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/internal/notify"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

// Initiation forms (how the payment was started).
const (
	InitManual    = "MANU" // account details typed by the payer
	InitDICT      = "DICT" // Pix key
	InitStaticQR  = "QRES" // static QR code
	InitDynamicQR = "QRDN" // dynamic QR code
)

// Party is one side of a transfer.
type Party struct {
	AccountID   string `json:"account_id,omitempty"`
	ISPB        string `json:"ispb"`
	Branch      string `json:"branch"`
	Number      string `json:"account_number"`
	AccountType string `json:"account_type,omitempty"`
	Name        string `json:"name,omitempty"`
	TaxIDMasked string `json:"tax_id_masked,omitempty"`
	Key         string `json:"key,omitempty"`
}

func partyOf(a ledger.Account) Party {
	return Party{
		AccountID: a.ID, ISPB: a.ISPB, Branch: a.Branch, Number: a.Number, AccountType: string(a.Type),
		Name: a.OwnerName, TaxIDMasked: document.Mask(a.OwnerDocument),
	}
}

// Transfer is a Pix payment (pacs.008-like) and its status (pacs.002-like).
type Transfer struct {
	EndToEndID        string     `json:"end_to_end_id"`
	TxID              string     `json:"txid,omitempty"`
	Initiation        string     `json:"initiation"`
	Status            string     `json:"status"`
	Reason            string     `json:"reason,omitempty"`
	ReasonDescription string     `json:"reason_description,omitempty"`
	AmountCents       int64      `json:"amount_cents"`
	RefundedCents     int64      `json:"refunded_cents"`
	Description       string     `json:"description,omitempty"`
	Payer             Party      `json:"payer"`
	Payee             Party      `json:"payee"`
	CreatedAt         time.Time  `json:"created_at"`
	SettleAt          time.Time  `json:"expected_settlement_at"`
	SettledAt         *time.Time `json:"settled_at,omitempty"`
	Refunds           []Refund   `json:"refunds"`
	Chaos             []string   `json:"chaos_rules,omitempty"`

	reserved bool
	pending  string // reason decided at initiation, applied at settlement
	notify   notify.PublishOptions
}

func (t *Transfer) snapshot() Transfer {
	c := *t
	c.Refunds = append([]Refund{}, t.Refunds...)
	c.Chaos = append([]string(nil), t.Chaos...)
	return c
}

// AccountRef identifies a destination by account details (manual Pix).
type AccountRef struct {
	ISPB   string `json:"ispb"`
	Branch string `json:"branch"`
	Number string `json:"number"`
}

// TransferRequest starts a transfer. Exactly one of Key, ToAccountID and
// ToAccount must be set.
type TransferRequest struct {
	FromAccountID string      `json:"from_account_id"`
	Key           string      `json:"key,omitempty"`
	ToAccountID   string      `json:"to_account_id,omitempty"`
	ToAccount     *AccountRef `json:"to_account,omitempty"`
	AmountCents   int64       `json:"amount_cents"`
	EndToEndID    string      `json:"end_to_end_id,omitempty"`
	Description   string      `json:"description,omitempty"`

	txid       string
	initiation string
	chargeTx   string
}

// destination resolves the payee; a nil account with a reason means the
// transfer is accepted and later rejected with that reason.
func (s *Sim) destination(r TransferRequest) (Party, *ledger.Account, string, error) {
	n := 0
	for _, set := range []bool{r.Key != "", r.ToAccountID != "", r.ToAccount != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return Party{}, nil, "", invalid("invalid_destination", "set exactly one of key, to_account_id or to_account")
	}
	switch {
	case r.Key != "":
		e, err := s.DICT.Resolve(r.Key)
		if errors.Is(err, dict.ErrKeyNotFound) {
			return Party{}, nil, "", notFound("key_not_found", "pix key not found in DICT")
		}
		if err != nil {
			return Party{}, nil, "", &Error{Kind: KindInvalid, Code: "invalid_key", Msg: err.Error(), Err: err}
		}
		a, err := s.Ledger.Get(e.Owner.AccountID)
		if err != nil {
			return Party{}, nil, iso.AC03, nil
		}
		p := partyOf(a)
		p.Key = e.Key
		return p, &a, "", nil
	case r.ToAccountID != "":
		a, err := s.Account(r.ToAccountID)
		if err != nil {
			return Party{}, nil, "", err
		}
		return partyOf(a), &a, "", nil
	default:
		ref := *r.ToAccount
		p := Party{ISPB: ref.ISPB, Branch: ref.Branch, Number: ref.Number}
		if !digits(ref.ISPB, 8) || ref.Number == "" {
			return Party{}, nil, "", invalid("invalid_destination", "to_account needs an 8-digit ispb and a number")
		}
		a, err := s.Ledger.Find(ref.ISPB, ref.Branch, ref.Number)
		if err != nil || a.System {
			return p, nil, iso.AC03, nil
		}
		return partyOf(a), &a, "", nil
	}
}

// CreateTransfer accepts a transfer (status ACSP) and schedules its
// settlement. Business failures (insufficient funds, closed accounts, chaos
// rules) do not fail the call: the transfer ends RJCT with a reason code.
func (s *Sim) CreateTransfer(r TransferRequest, ov *chaos.Overrides) (Transfer, error) {
	payer, err := s.Account(r.FromAccountID)
	if err != nil {
		return Transfer{}, err
	}
	if payer.Status != ledger.Active {
		return Transfer{}, invalid("payer_account_inactive", "payer account is %s", payer.Status)
	}
	if r.AmountCents <= 0 || r.AmountCents > pix.MaxCents {
		return Transfer{}, invalid("invalid_amount", "amount_cents must be between 1 and %d", int64(pix.MaxCents))
	}
	if len(r.Description) > 140 {
		return Transfer{}, invalid("invalid_description", "description must be at most 140 characters")
	}
	payee, payeeAcct, reason, err := s.destination(r)
	if err != nil {
		return Transfer{}, err
	}
	if payeeAcct != nil && payeeAcct.ID == payer.ID {
		return Transfer{}, invalid("same_account", "payer and payee are the same account")
	}
	now := s.Clock.Now()
	e2e := r.EndToEndID
	if e2e == "" {
		if e2e, err = pix.NewEndToEndID(s.cfg.ISPB, now, s.rand); err != nil {
			return Transfer{}, err
		}
	} else if _, err := pix.ParseEndToEndID(e2e); err != nil {
		return Transfer{}, &Error{Kind: KindInvalid, Code: "invalid_end_to_end_id", Msg: err.Error(), Err: err}
	}
	if r.initiation == "" {
		r.initiation = InitManual
		if r.Key != "" {
			r.initiation = InitDICT
		}
	}

	op := chaos.OpTransfer
	if r.initiation == InitStaticQR || r.initiation == InitDynamicQR {
		op = chaos.OpQRPayment
	}
	dec := s.Chaos.Evaluate(chaos.Op{Operation: op, FromAccount: payer.ID, ToAccount: payee.AccountID, Amount: r.AmountCents}, ov)
	latency := s.cfg.SettlementLatency
	if dec.Latency != nil {
		latency = *dec.Latency
	}

	switch {
	case reason != "":
	case payeeAcct.Status == ledger.Closed:
		reason = iso.AC04
	case payeeAcct.Status == ledger.Blocked:
		reason = iso.AC06
	case s.cfg.MaxTransferCents > 0 && r.AmountCents > s.cfg.MaxTransferCents:
		reason = iso.AM02
	case dec.Fail != "":
		reason = dec.Fail
	}

	t := &Transfer{
		EndToEndID: e2e, TxID: r.txid, Initiation: r.initiation, Status: iso.StatusPending,
		AmountCents: r.AmountCents, Description: r.Description, Payer: partyOf(payer), Payee: payee,
		CreatedAt: now, SettleAt: now.Add(latency), Refunds: []Refund{}, Chaos: dec.Rules,
		pending: reason, notify: notify.PublishOptions{Duplicates: dec.Duplicates, Reorder: dec.Reorder},
	}

	s.mu.Lock()
	if _, dup := s.transfers[e2e]; dup {
		s.mu.Unlock()
		return Transfer{}, conflict("duplicate_end_to_end_id", "a transfer with end_to_end_id %s already exists", e2e)
	}
	if r.chargeTx != "" {
		if err := s.reserveChargeLocked(r.chargeTx, e2e); err != nil {
			s.mu.Unlock()
			return Transfer{}, err
		}
	}
	if t.pending == "" {
		if _, err := s.Ledger.Move(payer.ID, ledger.Settlement, r.AmountCents, e2e, "reserve"); err != nil {
			t.pending = iso.AM04
		} else {
			t.reserved = true
		}
	}
	s.transfers[e2e] = t
	s.order = append(s.order, e2e)
	out := t.snapshot()
	s.mu.Unlock()

	s.timers.Add(1)
	s.Clock.AfterFunc(latency, func() {
		defer s.timers.Done()
		s.settle(e2e, r.chargeTx)
	})
	return out, nil
}

func (s *Sim) settle(e2e, chargeTx string) {
	s.mu.Lock()
	t := s.transfers[e2e]
	if t.Status != iso.StatusPending {
		s.mu.Unlock()
		return
	}
	now := s.Clock.Now()
	if t.pending == "" {
		if _, err := s.Ledger.Move(ledger.Settlement, t.Payee.AccountID, t.AmountCents, e2e, "settle"); err != nil {
			t.pending = iso.AB03 // cannot happen with a consistent ledger
		}
	}
	if t.pending != "" {
		if t.reserved {
			_, _ = s.Ledger.Move(ledger.Settlement, t.Payer.AccountID, t.AmountCents, e2e, "release")
		}
		t.Status, t.Reason, t.ReasonDescription = iso.StatusRejected, t.pending, iso.Reasons[t.pending]
	} else {
		t.Status = iso.StatusSettled
	}
	t.SettledAt = &now
	t.pending = ""
	if chargeTx != "" {
		s.finishChargeLocked(chargeTx, e2e, t.Status == iso.StatusSettled)
	}
	snap := t.snapshot()
	var ids [2]string
	ids[0] = s.nextEventID()
	ids[1] = s.nextEventID()
	s.mu.Unlock()

	if snap.Status == iso.StatusSettled {
		s.publish(ids[0], webhook.EventPixSettled, snap.Payer.AccountID, snap, t.notify)
		s.publish(ids[1], webhook.EventPixReceived, snap.Payee.AccountID, snap, t.notify)
		s.log.Info("transfer settled", "e2e", e2e, "amount_cents", snap.AmountCents)
	} else {
		s.publish(ids[0], webhook.EventPixRejected, snap.Payer.AccountID, snap, t.notify)
		s.log.Info("transfer rejected", "e2e", e2e, "reason", snap.Reason)
	}
}

// Transfer returns a transfer by EndToEndId.
func (s *Sim) Transfer(e2e string) (Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[e2e]
	if !ok {
		return Transfer{}, notFound("transfer_not_found", "transfer %s not found", e2e)
	}
	return t.snapshot(), nil
}

// Transfers lists transfers newest first, optionally for one account (as
// payer or payee), at most limit (0 = all).
func (s *Sim) Transfers(accountID string, limit int) []Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Transfer{}
	for i := len(s.order) - 1; i >= 0; i-- {
		t := s.transfers[s.order[i]]
		if accountID != "" && t.Payer.AccountID != accountID && t.Payee.AccountID != accountID {
			continue
		}
		out = append(out, t.snapshot())
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}
