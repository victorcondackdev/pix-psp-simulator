package sim

import (
	"fmt"
	"testing"
	"time"
)

func names(r FraudRisk) string {
	var out []string
	for _, s := range r.Signals {
		out = append(out, fmt.Sprintf("%s=%d", s.Name, s.Weight))
	}
	return fmt.Sprint(out)
}

func TestFraudRisk(t *testing.T) {
	e := newEnv(t, Config{})
	mules(t, e, 6)
	r := e.FraudRisk("m1", e.Clock.Now())
	if r.Score != 35 || r.Level != RiskMedium || names(r) != "[key.new=20 account.new=15]" {
		t.Fatalf("%d %s %s", r.Score, r.Level, names(r))
	}
	e.clock.Advance(8 * 24 * time.Hour)
	if r := e.FraudRisk("m1", e.Clock.Now().Add(-3*24*time.Hour)); r.Score != 10 || r.Level != RiskLow || names(r) != "[key.recent=10]" {
		t.Fatal(names(r))
	}

	// Five victims pay m1, which forwards almost everything; one victim
	// reports and the MED is accepted.
	var first Transfer
	for i := 2; i <= 6; i++ {
		if _, err := e.Deposit(fmt.Sprintf("m%d", i), 1000); err != nil {
			t.Fatal(err)
		}
		tr := settledTransfer(t, e, fmt.Sprintf("m%d", i), "m1", 1000)
		if i == 2 {
			first = tr
		}
	}
	settledTransfer(t, e, "m1", "padaria", 4500)
	old := e.Clock.Now().Add(-30 * 24 * time.Hour)
	r = e.FraudRisk("m1", old)
	if names(r) != "[inbound.fan_in=15 flow.pass_through=15]" || r.Level != RiskMedium {
		t.Fatal(names(r))
	}
	inf, _ := e.ReportInfraction(InfractionRequest{EndToEndID: first.EndToEndID})
	if r := e.FraudRisk("m1", old); names(r) != "[med.open=15 inbound.fan_in=15 flow.pass_through=15]" {
		t.Fatal(names(r))
	}
	for _, ev := range []InfractionEvent{MEDBlock, MEDAnalyze, MEDAccept} {
		if _, err := e.ActOnInfraction(inf.ID, ev); err != nil {
			t.Fatal(err)
		}
	}
	r = e.FraudRisk("m1", old)
	if r.Score != 70 || r.Level != RiskHigh {
		t.Fatalf("%d %s", r.Score, names(r))
	}
	// The downstream account reached by tracing is marked too.
	if r := e.FraudRisk("padaria", old); r.Signals[0].Name != "med.fraud_mark" {
		t.Fatal(names(r))
	}
	// Score is capped at 100.
	for i := 0; i < 3; i++ {
		e2 := settledTransfer(t, e, "alice", "m1", 100)
		in, _ := e.ReportInfraction(InfractionRequest{EndToEndID: e2.EndToEndID})
		for _, ev := range []InfractionEvent{MEDBlock, MEDAnalyze, MEDAccept} {
			_, _ = e.ActOnInfraction(in.ID, ev)
		}
	}
	if r := e.FraudRisk("m1", e.Clock.Now()); r.Score != 100 {
		t.Fatal(r.Score, names(r))
	}
}
