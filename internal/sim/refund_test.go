package sim

import (
	"fmt"
	"strings"
	"testing"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

func chaosFail(code string) *chaos.Overrides { return &chaos.Overrides{Fail: code} }

func settledTransfer(t *testing.T, e *env, from, to string, amount int64) Transfer {
	t.Helper()
	tr, err := e.CreateTransfer(TransferRequest{FromAccountID: from, ToAccountID: to, AmountCents: amount}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.settle()
	tr, _ = e.Transfer(tr.EndToEndID)
	if tr.Status != iso.StatusSettled {
		t.Fatalf("transfer not settled: %+v", tr)
	}
	return tr
}

func TestRefunds(t *testing.T) {
	e := newEnv(t, Config{})
	tr := settledTransfer(t, e, "alice", "padaria", 1000)
	steps := []struct {
		name       string
		req        RefundRequest
		ov         *chaos.Overrides
		kind       Kind
		wantStatus string
		wantReason string
		refunded   int64
	}{
		{"partial", RefundRequest{AmountCents: 300}, nil, 0, iso.StatusSettled, "", 300},
		{"bad reason", RefundRequest{AmountCents: 1, Reason: "XX00"}, nil, KindInvalid, "", "", 300},
		{"fraud reserved for MED", RefundRequest{AmountCents: 1, Reason: iso.ReturnFraud}, nil, KindInvalid, "", "", 300},
		{"exceeds remaining", RefundRequest{AmountCents: 701}, nil, KindInvalid, "", "", 300},
		{"zero", RefundRequest{}, nil, KindInvalid, "", "", 300},
		{"chaos rejected", RefundRequest{AmountCents: 100}, chaosFail(iso.DS04), 0, iso.StatusRejected, iso.DS04, 300},
		{"bank error rest", RefundRequest{AmountCents: 700, Reason: iso.ReturnBankError}, nil, 0, iso.StatusSettled, "", 1000},
		{"nothing left", RefundRequest{AmountCents: 1}, nil, KindInvalid, "", "", 1000},
	}
	var first Refund
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			ref, err := e.CreateRefund(tr.EndToEndID, s.req, s.ov)
			if kindOf(err) != s.kind {
				t.Fatalf("err = %v", err)
			}
			if err == nil {
				if ref.Status != s.wantStatus || ref.Reason != s.wantReason || !strings.HasPrefix(ref.ID, "D99999999") || len(ref.ID) != 32 {
					t.Fatalf("refund = %+v", ref)
				}
				if first.ID == "" {
					first = ref
				}
			}
			got, _ := e.Transfer(tr.EndToEndID)
			if got.RefundedCents != s.refunded {
				t.Fatalf("refunded = %d, want %d", got.RefundedCents, s.refunded)
			}
		})
	}
	if e.balance(t, "padaria") != 0 {
		t.Fatalf("padaria = %d", e.balance(t, "padaria"))
	}
	if got, err := e.Refund(tr.EndToEndID, first.ID); err != nil || got.AmountCents != 300 {
		t.Fatal(got, err)
	}
	if _, err := e.Refund(tr.EndToEndID, "nope"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.Refund("nope", "x"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.CreateRefund("nope", RefundRequest{AmountCents: 1}, nil); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	e.clock.Advance(0)
	n := 0
	for _, typ := range e.hooks.types("alice") {
		if typ == webhook.EventRefundCompleted {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("refund.completed for payer = %d", n)
	}
}

func TestRefundInsufficientAndNotSettled(t *testing.T) {
	e := newEnv(t, Config{})
	tr := settledTransfer(t, e, "alice", "bob", 5000)
	// Bob spends it all.
	settledTransfer(t, e, "bob", "padaria", e.balance(t, "bob"))
	ref, err := e.CreateRefund(tr.EndToEndID, RefundRequest{AmountCents: 5000}, nil)
	if err != nil || ref.Status != iso.StatusRejected || ref.Reason != iso.AM04 {
		t.Fatalf("%+v %v", ref, err)
	}
	pending, _ := e.CreateTransfer(TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 1}, nil)
	if _, err := e.CreateRefund(pending.EndToEndID, RefundRequest{AmountCents: 1}, nil); kindOf(err) != KindConflict {
		t.Fatal(err)
	}
	if _, err := e.ReportInfraction(InfractionRequest{EndToEndID: pending.EndToEndID}); kindOf(err) != KindConflict {
		t.Fatal(err)
	}
}

func TestMEDStateMachine(t *testing.T) {
	type step struct {
		ev   InfractionEvent
		want InfractionStatus
		ok   bool
	}
	tests := []struct {
		name         string
		spend        int64 // payee spends this much before the block
		steps        []step
		payerRefund  int64
		payeeBalance int64
	}{
		{"accepted full", 0, []step{{MEDBlock, MEDBlocked, true}, {MEDAnalyze, MEDAnalysis, true}, {MEDAccept, MEDRefunded, true}}, 2000, 0},
		{"accepted partial block", 1500, []step{{MEDBlock, MEDBlocked, true}, {MEDAnalyze, MEDAnalysis, true}, {MEDAccept, MEDRefunded, true}}, 500, 0},
		{"rejected releases", 0, []step{{MEDBlock, MEDBlocked, true}, {MEDAnalyze, MEDAnalysis, true}, {MEDReject, MEDClosed, true}}, 0, 2000},
		{"cancel after block", 0, []step{{MEDBlock, MEDBlocked, true}, {MEDCancel, MEDCancelled, true}}, 0, 2000},
		{"cancel open", 0, []step{{MEDCancel, MEDCancelled, true}, {MEDBlock, MEDCancelled, false}}, 0, 2000},
		{"invalid order", 0, []step{{MEDAccept, MEDOpen, false}, {MEDAnalyze, MEDOpen, false}, {MEDBlock, MEDBlocked, true}, {MEDReject, MEDBlocked, false}}, 0, 0},
		{"nothing to block", 2000, []step{{MEDBlock, MEDBlocked, true}, {MEDAnalyze, MEDAnalysis, true}, {MEDAccept, MEDRefunded, true}}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			tr := settledTransfer(t, e, "alice", "padaria", 2000)
			if tt.spend > 0 {
				settledTransfer(t, e, "padaria", "bob", tt.spend)
			}
			alice0 := e.balance(t, "alice")
			inf, err := e.ReportInfraction(InfractionRequest{EndToEndID: tr.EndToEndID, Details: "scam"})
			if err != nil || inf.Status != MEDOpen || fmt.Sprint(inf.Allowed) != "[block cancel]" {
				t.Fatalf("%+v %v", inf, err)
			}
			for i, s := range tt.steps {
				got, err := e.ActOnInfraction(inf.ID, s.ev)
				if (err == nil) != s.ok || got.Status != s.want {
					t.Fatalf("step %d %s: status %s err %v", i, s.ev, got.Status, err)
				}
				if e.Ledger.Total() != 0 {
					t.Fatal("ledger unbalanced")
				}
			}
			if d := e.balance(t, "alice") - alice0; d != tt.payerRefund {
				t.Fatalf("payer refunded %d, want %d", d, tt.payerRefund)
			}
			if b := e.balance(t, "padaria"); b != tt.payeeBalance {
				t.Fatalf("payee balance %d, want %d", b, tt.payeeBalance)
			}
			got, _ := e.Infraction(inf.ID)
			if got.Status == MEDRefunded && tt.payerRefund > 0 {
				ref, err := e.Refund(tr.EndToEndID, got.RefundID)
				if err != nil || ref.ReturnReason != iso.ReturnFraud || ref.InfractionID != inf.ID {
					t.Fatalf("refund = %+v %v", ref, err)
				}
			}
			if MEDMachine.Terminal(got.Status) && e.balance(t, ledger.MEDHold) != 0 {
				t.Fatal("hold not cleared in terminal state")
			}
		})
	}
}

func TestInfractionValidation(t *testing.T) {
	e := newEnv(t, Config{})
	tr := settledTransfer(t, e, "alice", "padaria", 100)
	if _, err := e.ReportInfraction(InfractionRequest{EndToEndID: tr.EndToEndID, Reason: "BORED"}); kindOf(err) != KindInvalid {
		t.Fatal(err)
	}
	if _, err := e.ReportInfraction(InfractionRequest{EndToEndID: "nope"}); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	inf, err := e.ReportInfraction(InfractionRequest{EndToEndID: tr.EndToEndID, Reason: "REFUND_REQUEST"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReportInfraction(InfractionRequest{EndToEndID: tr.EndToEndID}); kindOf(err) != KindConflict {
		t.Fatal(err)
	}
	if _, err := e.ActOnInfraction("nope", MEDBlock); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.Infraction("nope"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if list := e.Infractions(); len(list) != 1 || list[0].ID != inf.ID {
		t.Fatal(list)
	}
	// Refund failing during MED accept (chaos rule on refunds) releases the hold.
	if _, err := e.Chaos.Add(chaos.Rule{Match: chaos.Match{Operation: chaos.OpRefund}, Fail: iso.DS04}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []InfractionEvent{MEDBlock, MEDAnalyze, MEDAccept} {
		if _, err := e.ActOnInfraction(inf.ID, ev); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := e.Infraction(inf.ID)
	if got.Status != MEDRefunded || got.RefundID != "" || e.balance(t, "padaria") != 100 || e.balance(t, ledger.MEDHold) != 0 {
		t.Fatalf("infraction = %+v padaria=%d", got, e.balance(t, "padaria"))
	}
	if !strings.Contains(got.History[2].Note, "refund failed") {
		t.Fatalf("history = %+v", got.History)
	}
}
