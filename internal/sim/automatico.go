package sim

import (
	"fmt"
	"strings"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/bizday"
	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/fsm"
	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/notify"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

// Pix Automático — simplified model of recurring payments: the receiver
// proposes a recurrence, the payer authorises it (optionally with its own
// amount cap), and the receiver then schedules one charge per period, which
// the simulator debits on the due date, retrying daily on insufficient funds.
// The notice window, retry count and debit hour are simulator settings; check
// the Regulamento do Pix and the Pix Automático manuals for the real ones.

// InitAutomatic is the initiation form of transfers made by Pix Automático
// (a simulator label).
const InitAutomatic = "AUTO"

// Frequencies of a recurrence.
const (
	FreqWeekly     = "WEEKLY"
	FreqMonthly    = "MONTHLY"
	FreqQuarterly  = "QUARTERLY"
	FreqSemiannual = "SEMIANNUAL"
	FreqAnnual     = "ANNUAL"
)

// RecurrenceStatus is the state of a recurrence.
type RecurrenceStatus string

// Recurrence states.
const (
	RecPending   RecurrenceStatus = "PENDING_APPROVAL"
	RecActive    RecurrenceStatus = "ACTIVE"
	RecRejected  RecurrenceStatus = "REJECTED"
	RecCancelled RecurrenceStatus = "CANCELLED"
)

// RecurrenceEvent drives a recurrence.
type RecurrenceEvent string

// Recurrence events.
const (
	RecApprove RecurrenceEvent = "approve"
	RecReject  RecurrenceEvent = "reject"
	RecCancel  RecurrenceEvent = "cancel"
)

// RecurrenceMachine is PENDING_APPROVAL --approve--> ACTIVE --cancel-->
// CANCELLED; PENDING_APPROVAL --reject--> REJECTED.
var RecurrenceMachine = fsm.New([]fsm.Transition[RecurrenceStatus, RecurrenceEvent]{
	{From: RecPending, Event: RecApprove, To: RecActive},
	{From: RecPending, Event: RecReject, To: RecRejected},
	{From: RecPending, Event: RecCancel, To: RecCancelled},
	{From: RecActive, Event: RecCancel, To: RecCancelled},
}, RecRejected, RecCancelled)

// Automatic charge statuses.
const (
	AutoScheduled = "SCHEDULED"
	AutoRunning   = "IN_PROGRESS"
	AutoRetrying  = "RETRYING"
	AutoSettled   = "SETTLED"
	AutoFailed    = "FAILED"
	AutoCancelled = "CANCELLED"
)

// Recurrence is a Pix Automático authorisation.
type Recurrence struct {
	ID                string            `json:"id"`
	ReceiverAccountID string            `json:"receiver_account_id"`
	PayerAccountID    string            `json:"payer_account_id"`
	Frequency         string            `json:"frequency"`
	StartDate         string            `json:"start_date"`
	EndDate           string            `json:"end_date,omitempty"`
	FixedAmountCents  int64             `json:"fixed_amount_cents,omitempty"`
	MaxAmountCents    int64             `json:"max_amount_cents,omitempty"`
	PayerMaxCents     int64             `json:"payer_max_amount_cents,omitempty"`
	Description       string            `json:"description,omitempty"`
	ContractID        string            `json:"contract_id,omitempty"`
	Status            RecurrenceStatus  `json:"status"`
	Allowed           []RecurrenceEvent `json:"allowed_events"`
	Charges           []string          `json:"charges"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

func (r *Recurrence) snapshot() Recurrence {
	c := *r
	c.Charges = append([]string{}, r.Charges...)
	c.Allowed = RecurrenceMachine.Events(r.Status)
	return c
}

// AutoAttempt is one debit attempt of a charge.
type AutoAttempt struct {
	At         time.Time `json:"at"`
	EndToEndID string    `json:"end_to_end_id,omitempty"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
}

// AutoCharge is one scheduled debit of a recurrence.
type AutoCharge struct {
	ID            string        `json:"id"`
	RecurrenceID  string        `json:"recurrence_id"`
	AmountCents   int64         `json:"amount_cents"`
	DueDate       string        `json:"due_date"`
	Description   string        `json:"description,omitempty"`
	Status        string        `json:"status"`
	Attempts      []AutoAttempt `json:"attempts"`
	NextAttemptAt *time.Time    `json:"next_attempt_at,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	period        string
	timer         clock.Timer
}

func (c *AutoCharge) snapshot() AutoCharge {
	x := *c
	x.Attempts = append([]AutoAttempt{}, c.Attempts...)
	x.timer = nil
	return x
}

// RecurrenceRequest proposes a recurrence (sent by the receiver).
type RecurrenceRequest struct {
	ReceiverAccountID string `json:"receiver_account_id"`
	PayerAccountID    string `json:"payer_account_id"`
	Frequency         string `json:"frequency"`
	StartDate         string `json:"start_date"`
	EndDate           string `json:"end_date,omitempty"`
	FixedAmountCents  int64  `json:"fixed_amount_cents,omitempty"`
	MaxAmountCents    int64  `json:"max_amount_cents,omitempty"`
	Description       string `json:"description,omitempty"`
	ContractID        string `json:"contract_id,omitempty"`
}

// AutoChargeRequest schedules a charge.
type AutoChargeRequest struct {
	AmountCents int64  `json:"amount_cents"`
	DueDate     string `json:"due_date"`
	Description string `json:"description,omitempty"`
}

func parseDate(field, v string) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", v, bizday.Brasilia)
	if err != nil {
		return time.Time{}, invalid("invalid_"+field, "%s must be AAAA-MM-DD", field)
	}
	return d, nil
}

// period returns the billing period a date belongs to.
func period(freq string, d time.Time) string {
	switch freq {
	case FreqWeekly:
		y, w := d.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	case FreqMonthly:
		return d.Format("2006-01")
	case FreqQuarterly:
		return fmt.Sprintf("%d-Q%d", d.Year(), (int(d.Month())-1)/3+1)
	case FreqSemiannual:
		return fmt.Sprintf("%d-H%d", d.Year(), (int(d.Month())-1)/6+1)
	default:
		return fmt.Sprint(d.Year())
	}
}

// CreateRecurrence records a receiver's proposal, pending the payer.
func (s *Sim) CreateRecurrence(r RecurrenceRequest) (Recurrence, error) {
	for _, id := range []string{r.ReceiverAccountID, r.PayerAccountID} {
		if _, err := s.Account(id); err != nil {
			return Recurrence{}, err
		}
	}
	if r.ReceiverAccountID == r.PayerAccountID {
		return Recurrence{}, invalid("same_account", "payer and receiver are the same account")
	}
	switch r.Frequency {
	case FreqWeekly, FreqMonthly, FreqQuarterly, FreqSemiannual, FreqAnnual:
	default:
		return Recurrence{}, invalid("invalid_frequency", "frequency must be WEEKLY, MONTHLY, QUARTERLY, SEMIANNUAL or ANNUAL")
	}
	start, err := parseDate("start_date", r.StartDate)
	if err != nil {
		return Recurrence{}, err
	}
	if r.EndDate != "" {
		end, err := parseDate("end_date", r.EndDate)
		if err != nil {
			return Recurrence{}, err
		}
		if end.Before(start) {
			return Recurrence{}, invalid("invalid_end_date", "end_date is before start_date")
		}
	}
	if r.FixedAmountCents < 0 || r.MaxAmountCents < 0 || (r.FixedAmountCents > 0 && r.MaxAmountCents > 0) {
		return Recurrence{}, invalid("invalid_amount", "set at most one of fixed_amount_cents and max_amount_cents, positive")
	}
	if len(r.Description) > 140 {
		return Recurrence{}, invalid("invalid_description", "description must be at most 140 characters")
	}
	now := s.Clock.Now()
	rec := &Recurrence{
		ID: "rec_" + s.randomHex(6), ReceiverAccountID: r.ReceiverAccountID, PayerAccountID: r.PayerAccountID,
		Frequency: r.Frequency, StartDate: r.StartDate, EndDate: r.EndDate, FixedAmountCents: r.FixedAmountCents,
		MaxAmountCents: r.MaxAmountCents, Description: r.Description, ContractID: r.ContractID,
		Status: RecPending, Charges: []string{}, CreatedAt: now, UpdatedAt: now,
	}
	s.mu.Lock()
	s.recurrences[rec.ID] = rec
	s.recOrder = append(s.recOrder, rec.ID)
	out := rec.snapshot()
	ev := s.nextEventID()
	s.mu.Unlock()
	s.Telemetry.Recurrences.Inc(string(out.Status))
	s.publishRecurrence(ev, out)
	return out, nil
}

// Both parties get the event, each with its own event ID ("-p", "-r").
func (s *Sim) publishRecurrence(id string, r Recurrence) {
	s.publish(id+"-p", webhook.EventAutomaticRecurrence, r.PayerAccountID, r, notify.PublishOptions{})
	s.publish(id+"-r", webhook.EventAutomaticRecurrence, r.ReceiverAccountID, r, notify.PublishOptions{})
}

func (s *Sim) publishAutoCharge(id string, rec Recurrence, c AutoCharge) {
	s.publish(id+"-p", webhook.EventAutomaticCharge, rec.PayerAccountID, c, notify.PublishOptions{})
	s.publish(id+"-r", webhook.EventAutomaticCharge, rec.ReceiverAccountID, c, notify.PublishOptions{})
}

// Recurrence returns a recurrence.
func (s *Sim) Recurrence(id string) (Recurrence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recurrences[id]
	if !ok {
		return Recurrence{}, notFound("recurrence_not_found", "recurrence %s not found", id)
	}
	return r.snapshot(), nil
}

// Recurrences lists recurrences newest first, optionally for one account.
func (s *Sim) Recurrences(accountID string) []Recurrence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Recurrence{}
	for i := len(s.recOrder) - 1; i >= 0; i-- {
		r := s.recurrences[s.recOrder[i]]
		if accountID == "" || r.PayerAccountID == accountID || r.ReceiverAccountID == accountID {
			out = append(out, r.snapshot())
		}
	}
	return out
}

// ActOnRecurrence approves, rejects or cancels. payerMax applies to approve:
// the payer's own cap on each charge (0 = none).
func (s *Sim) ActOnRecurrence(id string, ev RecurrenceEvent, payerMax int64) (Recurrence, error) {
	if payerMax < 0 {
		return Recurrence{}, invalid("invalid_amount", "payer_max_amount_cents must not be negative")
	}
	s.mu.Lock()
	r, ok := s.recurrences[id]
	if !ok {
		s.mu.Unlock()
		return Recurrence{}, notFound("recurrence_not_found", "recurrence %s not found", id)
	}
	next, err := RecurrenceMachine.Next(r.Status, ev)
	if err != nil {
		s.mu.Unlock()
		return r.snapshot(), &Error{Kind: KindConflict, Code: "invalid_transition", Msg: err.Error(), Err: err}
	}
	if ev == RecApprove && payerMax > 0 {
		r.PayerMaxCents = payerMax
	}
	r.Status, r.UpdatedAt = next, s.Clock.Now()
	type pub struct {
		id string
		c  AutoCharge
	}
	var pubs []pub
	if next == RecCancelled {
		for _, cid := range r.Charges {
			c := s.autoCharges[cid]
			if c.Status == AutoScheduled || c.Status == AutoRetrying {
				s.cancelAutoLocked(c)
				pubs = append(pubs, pub{s.nextEventID(), c.snapshot()})
			}
		}
	}
	out := r.snapshot()
	evID := s.nextEventID()
	s.mu.Unlock()
	s.Telemetry.Recurrences.Inc(string(next))
	s.publishRecurrence(evID, out)
	for _, p := range pubs {
		s.publishAutoCharge(p.id, out, p.c)
	}
	return out, nil
}

func (s *Sim) cancelAutoLocked(c *AutoCharge) {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.Status, c.NextAttemptAt = AutoCancelled, nil
	s.Telemetry.AutomaticCharges.Inc("cancelled")
}

// debitTime is when the debit of a due date runs.
func (s *Sim) debitTime(d time.Time) time.Time { return bizday.Date(d).Add(s.cfg.AutomaticDebitHour) }

// ScheduleAutoCharge schedules a charge on an active recurrence. The due
// date must fall inside the recurrence, between AutomaticMinNoticeDays and
// AutomaticMaxNoticeDays from today, in a period with no other live charge,
// and the amount must respect the fixed amount, the receiver's maximum and
// the payer's cap.
func (s *Sim) ScheduleAutoCharge(recID string, r AutoChargeRequest) (AutoCharge, error) {
	due, err := parseDate("due_date", r.DueDate)
	if err != nil {
		return AutoCharge{}, err
	}
	if r.AmountCents <= 0 {
		return AutoCharge{}, invalid("invalid_amount", "amount_cents must be positive")
	}
	s.mu.Lock()
	rec, ok := s.recurrences[recID]
	if !ok {
		s.mu.Unlock()
		return AutoCharge{}, notFound("recurrence_not_found", "recurrence %s not found", recID)
	}
	fail := func(e *Error) (AutoCharge, error) { s.mu.Unlock(); return AutoCharge{}, e }
	if rec.Status != RecActive {
		return fail(conflict("recurrence_not_active", "recurrence is %s", rec.Status))
	}
	start, _ := parseDate("start_date", rec.StartDate)
	if due.Before(start) {
		return fail(invalid("due_date_out_of_range", "due_date is before the recurrence start_date %s", rec.StartDate))
	}
	if rec.EndDate != "" {
		if end, _ := parseDate("end_date", rec.EndDate); due.After(end) {
			return fail(invalid("due_date_out_of_range", "due_date is after the recurrence end_date %s", rec.EndDate))
		}
	}
	today := bizday.Date(s.Clock.Now())
	if days := int(due.Sub(today).Hours() / 24); days < s.cfg.AutomaticMinNoticeDays || days > s.cfg.AutomaticMaxNoticeDays {
		return fail(invalid("due_date_notice", "due_date must be %d to %d days ahead (is %d)", s.cfg.AutomaticMinNoticeDays, s.cfg.AutomaticMaxNoticeDays, days))
	}
	switch {
	case rec.FixedAmountCents > 0 && r.AmountCents != rec.FixedAmountCents:
		return fail(invalid("amount_mismatch", "this recurrence has a fixed amount of %d", rec.FixedAmountCents))
	case rec.MaxAmountCents > 0 && r.AmountCents > rec.MaxAmountCents:
		return fail(invalid("amount_above_maximum", "amount exceeds the recurrence maximum of %d", rec.MaxAmountCents))
	case rec.PayerMaxCents > 0 && r.AmountCents > rec.PayerMaxCents:
		return fail(invalid("amount_above_payer_limit", "amount exceeds the payer's limit of %d", rec.PayerMaxCents))
	}
	p := period(rec.Frequency, due)
	for _, cid := range rec.Charges {
		if c := s.autoCharges[cid]; c.period == p && c.Status != AutoCancelled && c.Status != AutoFailed {
			return fail(conflict("period_already_charged", "charge %s already covers period %s", c.ID, p))
		}
	}
	now := s.Clock.Now()
	at := s.debitTime(due)
	c := &AutoCharge{
		ID: "ach_" + s.randomHex(6), RecurrenceID: rec.ID, AmountCents: r.AmountCents, DueDate: r.DueDate,
		Description: r.Description, Status: AutoScheduled, Attempts: []AutoAttempt{}, CreatedAt: now,
		NextAttemptAt: &at, period: p,
	}
	s.autoCharges[c.ID] = c
	rec.Charges = append(rec.Charges, c.ID)
	s.armAutoLocked(c, at.Sub(now))
	out, rs := c.snapshot(), rec.snapshot()
	ev := s.nextEventID()
	s.mu.Unlock()
	s.publishAutoCharge(ev, rs, out)
	return out, nil
}

func (s *Sim) armAutoLocked(c *AutoCharge, delay time.Duration) {
	id := c.ID
	c.timer = s.Clock.AfterFunc(delay, func() { s.attemptAuto(id) })
}

// attemptAuto debits a charge through an ordinary Pix.
func (s *Sim) attemptAuto(id string) {
	s.mu.Lock()
	c := s.autoCharges[id]
	if c.Status != AutoScheduled && c.Status != AutoRetrying {
		s.mu.Unlock()
		return
	}
	rec := s.recurrences[c.RecurrenceID]
	c.Status, c.timer, c.NextAttemptAt = AutoRunning, nil, nil
	req := TransferRequest{
		FromAccountID: rec.PayerAccountID, ToAccountID: rec.ReceiverAccountID, AmountCents: c.AmountCents,
		Description: strings.TrimSpace("Pix Automático " + rec.ID + " " + c.Description), initiation: InitAutomatic, autoCharge: c.ID,
	}
	if len(req.Description) > 140 {
		req.Description = req.Description[:140]
	}
	s.mu.Unlock()
	if _, err := s.CreateTransfer(req, nil); err != nil {
		// The payer account cannot pay at all (blocked, closed): treat as
		// a rejection that is not retried.
		s.mu.Lock()
		after := s.finishAutoLocked(id, AutoAttempt{At: s.Clock.Now(), Status: iso.StatusRejected, Reason: iso.DS04})
		s.mu.Unlock()
		after()
	}
}

// finishAutoLocked records an attempt outcome and returns the publication to
// run after unlocking.
func (s *Sim) finishAutoLocked(id string, a AutoAttempt) func() {
	c := s.autoCharges[id]
	rec := s.recurrences[c.RecurrenceID]
	c.Attempts = append(c.Attempts, a)
	switch {
	case a.Status == iso.StatusSettled:
		c.Status = AutoSettled
		s.Telemetry.AutomaticCharges.Inc("settled")
	case a.Reason == iso.AM04 && len(c.Attempts) <= s.cfg.AutomaticRetries && rec.Status == RecActive:
		c.Status = AutoRetrying
		at := s.Clock.Now().Add(24 * time.Hour)
		c.NextAttemptAt = &at
		s.armAutoLocked(c, 24*time.Hour)
		s.Telemetry.AutomaticCharges.Inc("retry")
	default:
		c.Status = AutoFailed
		s.Telemetry.AutomaticCharges.Inc("failed")
	}
	out, rs := c.snapshot(), rec.snapshot()
	ev := s.nextEventID()
	return func() { s.publishAutoCharge(ev, rs, out) }
}

// AutoCharge returns a charge.
func (s *Sim) AutoCharge(id string) (AutoCharge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.autoCharges[id]
	if !ok {
		return AutoCharge{}, notFound("charge_not_found", "automatic charge %s not found", id)
	}
	return c.snapshot(), nil
}

// CancelAutoCharge cancels a charge that has not been debited, until the
// day before its due date (the payer's or the receiver's right).
func (s *Sim) CancelAutoCharge(id string) (AutoCharge, error) {
	s.mu.Lock()
	c, ok := s.autoCharges[id]
	if !ok {
		s.mu.Unlock()
		return AutoCharge{}, notFound("charge_not_found", "automatic charge %s not found", id)
	}
	due, _ := parseDate("due_date", c.DueDate)
	switch {
	case c.Status != AutoScheduled && c.Status != AutoRetrying:
		s.mu.Unlock()
		return c.snapshot(), conflict("charge_not_cancellable", "charge is %s", c.Status)
	case c.Status == AutoScheduled && !bizday.Date(s.Clock.Now()).Before(due):
		s.mu.Unlock()
		return c.snapshot(), conflict("charge_not_cancellable", "a charge can be cancelled until the day before its due date")
	}
	s.cancelAutoLocked(c)
	out, rs := c.snapshot(), s.recurrences[c.RecurrenceID].snapshot()
	ev := s.nextEventID()
	s.mu.Unlock()
	s.publishAutoCharge(ev, rs, out)
	return out, nil
}
