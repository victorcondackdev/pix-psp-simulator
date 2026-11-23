package sim

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

// t0 is Monday 2026-06-01 09:00 in Brasília; debits run at 06:00.

func TestPixAutomaticoHappyPath(t *testing.T) {
	e := newEnv(t, Config{})
	rec, err := e.CreateRecurrence(RecurrenceRequest{ReceiverAccountID: "padaria", PayerAccountID: "alice", Frequency: FreqMonthly,
		StartDate: "2026-06-01", MaxAmountCents: 5000, Description: "assinatura pão"})
	if err != nil || rec.Status != RecPending || fmt_(rec.Allowed) != "[approve cancel reject]" {
		t.Fatal(rec, err)
	}
	if _, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 100, DueDate: "2026-06-05"}); kindOf(err) != KindConflict {
		t.Fatal("charge before approval", err)
	}
	if rec, _ = e.ActOnRecurrence(rec.ID, RecApprove, 3000); rec.Status != RecActive || rec.PayerMaxCents != 3000 {
		t.Fatal(rec)
	}
	for req, code := range map[AutoChargeRequest]string{
		{AmountCents: 100, DueDate: "2026-06-02"}:  "due_date_notice",
		{AmountCents: 100, DueDate: "2026-06-20"}:  "due_date_notice",
		{AmountCents: 6000, DueDate: "2026-06-05"}: "amount_above_maximum",
		{AmountCents: 4000, DueDate: "2026-06-05"}: "amount_above_payer_limit",
		{AmountCents: 0, DueDate: "2026-06-05"}:    "invalid_amount",
		{AmountCents: 1, DueDate: "5/6"}:           "invalid_due_date",
		{AmountCents: 1, DueDate: "2026-05-30"}:    "due_date_out_of_range",
	} {
		if _, err := e.ScheduleAutoCharge(rec.ID, req); codeOf(err) != code {
			t.Errorf("%+v: %v, want %s", req, err, code)
		}
	}
	c, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 2500, DueDate: "2026-06-05", Description: "junho"})
	if err != nil || c.Status != AutoScheduled || c.NextAttemptAt.Format(time.RFC3339) != "2026-06-05T06:00:00-03:00" {
		t.Fatal(c, err)
	}
	if _, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 2500, DueDate: "2026-06-08"}); codeOf(err) != "period_already_charged" {
		t.Fatal(err)
	}
	e.clock.Advance(c.NextAttemptAt.Sub(e.Clock.Now()))
	e.settle()
	c, _ = e.AutoCharge(c.ID)
	if c.Status != AutoSettled || len(c.Attempts) != 1 || c.Attempts[0].Status != iso.StatusSettled {
		t.Fatalf("%+v", c)
	}
	tr, _ := e.Transfer(c.Attempts[0].EndToEndID)
	if tr.Initiation != InitAutomatic || tr.AutomaticChargeID != c.ID || !strings.Contains(tr.Description, rec.ID) || e.balance(t, "padaria") != 2500 {
		t.Fatalf("%+v", tr)
	}
	types := strings.Join(e.hooks.types("alice"), " ")
	if strings.Count(types, webhook.EventAutomaticRecurrence) != 2 || strings.Count(types, webhook.EventAutomaticCharge) != 2 {
		t.Fatal(types)
	}
	if _, err := e.CancelAutoCharge(c.ID); kindOf(err) != KindConflict {
		t.Fatal("settled charge cancelled")
	}
	if l := e.Recurrences("alice"); len(l) != 1 || len(l[0].Charges) != 1 {
		t.Fatal(l)
	}
	if len(e.Recurrences("bob")) != 0 || len(e.Recurrences("")) != 1 {
		t.Fatal("filter")
	}
}

func TestPixAutomaticoRetriesAndCancel(t *testing.T) {
	e := newEnv(t, Config{})
	rec, _ := e.CreateRecurrence(RecurrenceRequest{ReceiverAccountID: "padaria", PayerAccountID: "bob", Frequency: FreqWeekly,
		StartDate: "2026-06-01", EndDate: "2026-12-31", FixedAmountCents: 20000})
	if _, err := e.ActOnRecurrence(rec.ID, RecApprove, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 1, DueDate: "2026-06-04"}); codeOf(err) != "amount_mismatch" {
		t.Fatal(err)
	}
	c, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 20000, DueDate: "2026-06-04"})
	if err != nil {
		t.Fatal(err)
	}
	// Bob has 100.00: the first two attempts fail with AM04 and are retried
	// daily; a deposit before the third lets it settle.
	e.clock.Advance(c.NextAttemptAt.Sub(e.Clock.Now()))
	e.settle()
	if c, _ = e.AutoCharge(c.ID); c.Status != AutoRetrying || c.Attempts[0].Reason != iso.AM04 || c.NextAttemptAt == nil {
		t.Fatalf("%+v", c)
	}
	e.clock.Advance(24 * time.Hour)
	e.settle()
	if _, err := e.Deposit("bob", 20000); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(24 * time.Hour)
	e.settle()
	if c, _ = e.AutoCharge(c.ID); c.Status != AutoSettled || len(c.Attempts) != 3 {
		t.Fatalf("%+v", c)
	}

	// Exhausting the retries fails the charge (1 attempt + 3 retries).
	c2, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 20000, DueDate: "2026-06-12"})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(c2.NextAttemptAt.Sub(e.Clock.Now()))
	for i := 0; i < 4; i++ {
		e.settle()
		e.clock.Advance(24 * time.Hour)
	}
	if c2, _ = e.AutoCharge(c2.ID); c2.Status != AutoFailed || len(c2.Attempts) != 4 {
		t.Fatalf("%+v", c2)
	}
	// A failed period can be charged again.
	c3, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 20000, DueDate: "2026-06-19"})
	if err != nil {
		t.Fatal(err)
	}
	c4, err := e.ScheduleAutoCharge(rec.ID, AutoChargeRequest{AmountCents: 20000, DueDate: "2026-06-23"})
	if err != nil {
		t.Fatal(err)
	}
	if c3, err = e.CancelAutoCharge(c3.ID); err != nil || c3.Status != AutoCancelled {
		t.Fatal(c3, err)
	}
	// Cancelling the recurrence cancels what is still scheduled.
	if rec, _ = e.ActOnRecurrence(rec.ID, RecCancel, 0); rec.Status != RecCancelled {
		t.Fatal(rec)
	}
	if c4, _ = e.AutoCharge(c4.ID); c4.Status != AutoCancelled {
		t.Fatal(c4)
	}
	e.clock.Advance(30 * 24 * time.Hour)
	if e.Ledger.Total() != 0 {
		t.Fatal("books")
	}
	if _, err := e.ActOnRecurrence(rec.ID, RecApprove, 0); kindOf(err) != KindConflict {
		t.Fatal(err)
	}
}

func TestPixAutomaticoValidation(t *testing.T) {
	e := newEnv(t, Config{})
	base := RecurrenceRequest{ReceiverAccountID: "padaria", PayerAccountID: "alice", Frequency: FreqAnnual, StartDate: "2026-06-01"}
	bad := []func(r *RecurrenceRequest){
		func(r *RecurrenceRequest) { r.PayerAccountID = "nope" },
		func(r *RecurrenceRequest) { r.PayerAccountID = "padaria" },
		func(r *RecurrenceRequest) { r.Frequency = "DAILY" },
		func(r *RecurrenceRequest) { r.StartDate = "x" },
		func(r *RecurrenceRequest) { r.EndDate = "x" },
		func(r *RecurrenceRequest) { r.EndDate = "2026-01-01" },
		func(r *RecurrenceRequest) { r.FixedAmountCents, r.MaxAmountCents = 1, 1 },
		func(r *RecurrenceRequest) { r.Description = strings.Repeat("x", 141) },
	}
	for i, f := range bad {
		r := base
		f(&r)
		if _, err := e.CreateRecurrence(r); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	rec, _ := e.CreateRecurrence(base)
	if _, err := e.ActOnRecurrence(rec.ID, RecApprove, -1); kindOf(err) != KindInvalid {
		t.Fatal(err)
	}
	if r, _ := e.ActOnRecurrence(rec.ID, RecReject, 0); r.Status != RecRejected {
		t.Fatal(r)
	}
	for _, f := range []func() error{
		func() error { _, err := e.Recurrence("nope"); return err },
		func() error { _, err := e.ActOnRecurrence("nope", RecApprove, 0); return err },
		func() error {
			_, err := e.ScheduleAutoCharge("nope", AutoChargeRequest{AmountCents: 1, DueDate: "2026-06-05"})
			return err
		},
		func() error { _, err := e.AutoCharge("nope"); return err },
		func() error { _, err := e.CancelAutoCharge("nope"); return err },
	} {
		if kindOf(f()) != KindNotFound {
			t.Error("not found")
		}
	}
	for freq, want := range map[string]string{FreqWeekly: "2026-W23", FreqMonthly: "2026-06", FreqQuarterly: "2026-Q2", FreqSemiannual: "2026-H1", FreqAnnual: "2026"} {
		if got := period(freq, time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)); got != want {
			t.Errorf("%s: %s", freq, got)
		}
	}

	// A blocked payer account cannot be debited: the charge fails at once.
	rec2, _ := e.CreateRecurrence(RecurrenceRequest{ReceiverAccountID: "padaria", PayerAccountID: "bob", Frequency: FreqMonthly, StartDate: "2026-06-01"})
	_, _ = e.ActOnRecurrence(rec2.ID, RecApprove, 0)
	c, _ := e.ScheduleAutoCharge(rec2.ID, AutoChargeRequest{AmountCents: 100, DueDate: "2026-06-04"})
	if _, err := e.CancelAutoCharge(c.ID); err != nil {
		t.Fatal(err)
	}
	c, _ = e.ScheduleAutoCharge(rec2.ID, AutoChargeRequest{AmountCents: 100, DueDate: "2026-06-04"})
	if _, err := e.SetAccountStatus("bob", ledger.Blocked); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(c.NextAttemptAt.Sub(e.Clock.Now()))
	if c, _ = e.AutoCharge(c.ID); c.Status != AutoFailed || c.Attempts[0].Reason != iso.DS04 {
		t.Fatalf("%+v", c)
	}
	// On the due date itself a scheduled charge can no longer be cancelled.
	_, _ = e.SetAccountStatus("bob", ledger.Active)
	rec3, _ := e.CreateRecurrence(RecurrenceRequest{ReceiverAccountID: "padaria", PayerAccountID: "bob", Frequency: FreqMonthly, StartDate: "2026-06-01"})
	_, _ = e.ActOnRecurrence(rec3.ID, RecApprove, 0)
	c, _ = e.ScheduleAutoCharge(rec3.ID, AutoChargeRequest{AmountCents: 100, DueDate: "2026-06-07"})
	e.clock.Advance(c.NextAttemptAt.Sub(e.Clock.Now()) - time.Hour)
	if _, err := e.CancelAutoCharge(c.ID); kindOf(err) != KindConflict {
		t.Fatal(err)
	}
}

func fmt_(v any) string { return fmt.Sprint(v) }

func codeOf(err error) string {
	if se, ok := err.(*Error); ok {
		return se.Code
	}
	return ""
}
