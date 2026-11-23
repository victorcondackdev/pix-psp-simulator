package sim

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
)

// cpf returns a valid CPF built from a 9-digit base.
func cpf(base int) string {
	d := []int{}
	for _, c := range fmt.Sprintf("%09d", base) {
		d = append(d, int(c-'0'))
	}
	for _, n := range []int{10, 11} {
		sum := 0
		for i := 0; i < n-1; i++ {
			sum += d[i] * (n - i)
		}
		r := sum * 10 % 11
		if r == 10 {
			r = 0
		}
		d = append(d, r)
	}
	var b strings.Builder
	for _, x := range d {
		b.WriteByte(byte('0' + x))
	}
	return b.String()
}

func mules(t *testing.T, e *env, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		ispb := "1111111" + fmt.Sprint(i%10)
		if _, err := e.CreateAccount(AccountRequest{ID: fmt.Sprintf("m%d", i), OwnerName: fmt.Sprintf("Mula %d", i), Document: cpf(100000000 + i), ISPB: ispb}); err != nil {
			t.Fatal(err)
		}
	}
}

func block(t *testing.T, e *env, e2e, version string) Infraction {
	t.Helper()
	inf, err := e.ReportInfraction(InfractionRequest{EndToEndID: e2e, MEDVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	inf, err = e.ActOnInfraction(inf.ID, MEDBlock)
	if err != nil {
		t.Fatal(err)
	}
	return inf
}

func hops(inf Infraction) string {
	var parts []string
	for _, h := range inf.Trace {
		parts = append(parts, fmt.Sprintf("%d:%s:%d/%d", h.Depth, h.AccountID, h.HeldCents, h.TracedCents))
	}
	return strings.Join(parts, " ")
}

func TestMED2TracesAChain(t *testing.T) {
	e := newEnv(t, Config{})
	mules(t, e, 3)
	scam := settledTransfer(t, e, "alice", "m1", 10000)
	settledTransfer(t, e, "m1", "m2", 7000) // m1 keeps 3000
	settledTransfer(t, e, "m2", "m3", 4000) // m2 keeps 3000
	settledTransfer(t, e, "m3", "padaria", 1000)
	alice0 := e.balance(t, "alice")

	inf := block(t, e, scam.EndToEndID, "")
	if inf.Version != MED2 || inf.HeldCents != 10000 {
		t.Fatalf("%+v", inf)
	}
	if got := hops(inf); got != "0:m1:3000/10000 1:m2:3000/7000 2:m3:3000/4000 3:padaria:1000/1000" {
		t.Fatal(got)
	}
	if inf.Trace[1].ViaEndToEndID == scam.EndToEndID || inf.Trace[1].ISPB != "11111112" {
		t.Fatalf("%+v", inf.Trace[1])
	}
	if !strings.Contains(inf.History[0].Note, "4 accounts") || inf.BlockExpiresAt == nil {
		t.Fatalf("%+v", inf)
	}
	for _, ev := range []InfractionEvent{MEDAnalyze, MEDAccept} {
		if inf, _ = e.ActOnInfraction(inf.ID, ev); inf.Status == "" {
			t.Fatal(ev)
		}
	}
	if d := e.balance(t, "alice") - alice0; d != 10000 {
		t.Fatalf("victim got back %d", d)
	}
	for _, h := range inf.Trace {
		if h.Status != HopReturned {
			t.Fatal(hops(inf))
		}
	}
	ref, err := e.Refund(scam.EndToEndID, inf.RefundID)
	if err != nil || ref.ReturnReason != iso.ReturnFraud || ref.AmountCents != 10000 {
		t.Fatal(ref, err)
	}
	if e.Ledger.Total() != 0 || e.balance(t, ledger.MEDHold) != 0 {
		t.Fatal("books")
	}
	if e.Telemetry.MEDBlockedCents.Value("3") != 1000 || e.Telemetry.MEDTracedHops.Count() != 1 {
		t.Fatal("metrics")
	}
}

func TestMED2SplitsAndCaps(t *testing.T) {
	e := newEnv(t, Config{MEDMaxAccounts: 3})
	mules(t, e, 5)
	// Money received before the scam is not traced: m1 already paid m5.
	if _, err := e.Deposit("m1", 500); err != nil {
		t.Fatal(err)
	}
	settledTransfer(t, e, "m1", "m5", 500)
	scam := settledTransfer(t, e, "alice", "m1", 6000)
	settledTransfer(t, e, "m1", "m2", 2000)
	settledTransfer(t, e, "m1", "m3", 4000) // m1 is empty, the scam split in two
	settledTransfer(t, e, "m3", "m4", 4000) // depth 2 is beyond the cap of 3 accounts
	inf := block(t, e, scam.EndToEndID, MED2)
	if got := hops(inf); got != "0:m1:0/6000 1:m2:2000/2000 1:m3:0/4000" {
		t.Fatal(got)
	}
	if inf.HeldCents != 2000 || !strings.Contains(inf.History[0].Note, "partial block after tracing 3") {
		t.Fatalf("%+v", inf)
	}
	inf, _ = e.ActOnInfraction(inf.ID, MEDCancel)
	if inf.Status != MEDCancelled || inf.Trace[1].Status != HopReleased || e.balance(t, "m2") != 2000 || inf.BlockExpiresAt != nil {
		t.Fatalf("%+v", inf)
	}
}

func TestMED2NothingAndCycles(t *testing.T) {
	e := newEnv(t, Config{})
	mules(t, e, 2)
	scam := settledTransfer(t, e, "alice", "m1", 1000)
	settledTransfer(t, e, "m1", "m2", 1000)
	settledTransfer(t, e, "m2", "m1", 1000) // back to m1
	settledTransfer(t, e, "m1", "alice", 1000)
	inf := block(t, e, scam.EndToEndID, MED2)
	// m1 (empty) -> m2 (empty) -> m1 again (empty, via the second
	// transfer) -> alice, the victim herself: nothing is blocked there.
	if got := hops(inf); inf.HeldCents != 0 || got != "0:m1:0/1000 1:m2:0/1000 2:m1:0/1000 3:alice:0/1000" || inf.Trace[3].Status != HopAtPayer {
		t.Fatal(got)
	}
	if inf, _ = e.ActOnInfraction(inf.ID, MEDAnalyze); inf.Status != MEDAnalysis {
		t.Fatal(inf.Status)
	}
	if inf, _ = e.ActOnInfraction(inf.ID, MEDReject); inf.Status != MEDClosed || e.balance(t, ledger.MEDHold) != 0 {
		t.Fatal(inf.Status)
	}
}

func TestMEDBlockExpires(t *testing.T) {
	e := newEnv(t, Config{MEDBlockWindow: time.Hour})
	tr := settledTransfer(t, e, "alice", "padaria", 2000)
	inf := block(t, e, tr.EndToEndID, MED1)
	if len(inf.Trace) != 1 || inf.HeldCents != 2000 {
		t.Fatal(hops(inf))
	}
	e.clock.Advance(59 * time.Minute)
	if got, _ := e.Infraction(inf.ID); got.Status != MEDBlocked {
		t.Fatal(got.Status)
	}
	e.clock.Advance(time.Minute)
	got, _ := e.Infraction(inf.ID)
	if got.Status != MEDExpired || got.HeldCents != 0 || e.balance(t, "padaria") != 2000 || got.Trace[0].Status != HopReleased {
		t.Fatalf("%+v", got)
	}
	if got.History[len(got.History)-1].Event != MEDExpire {
		t.Fatal(got.History)
	}

	// Analysed before the window ends: the timer is cancelled.
	tr2 := settledTransfer(t, e, "alice", "padaria", 100)
	inf2 := block(t, e, tr2.EndToEndID, MED2)
	if _, err := e.ActOnInfraction(inf2.ID, MEDAnalyze); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)
	if got, _ := e.Infraction(inf2.ID); got.Status != MEDAnalysis || got.HeldCents != 100 {
		t.Fatalf("%+v", got)
	}
	if _, err := e.ReportInfraction(InfractionRequest{EndToEndID: tr2.EndToEndID, MEDVersion: "3.0"}); kindOf(err) != KindInvalid {
		t.Fatal(err)
	}
}
