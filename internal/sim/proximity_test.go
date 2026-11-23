package sim

import (
	"testing"
	"time"
)

func TestPixPorAproximacao(t *testing.T) {
	e := newEnv(t, Config{})
	ch, err := e.CreateCharge(ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35"})
	if err != nil {
		t.Fatal(err)
	}
	pay := func(amount int64, device string) (Transfer, error) {
		return e.PayProximity(ProximityRequest{QRPaymentRequest: QRPaymentRequest{FromAccountID: "alice", BRCode: ch.BRCode, AmountCents: amount}, DeviceID: device}, nil)
	}
	if _, err := pay(100, "short"); codeOf(err) != "invalid_device" {
		t.Fatal(err)
	}
	tr, err := pay(150000, "pixel-8-abc123")
	if err != nil || tr.Proximity == nil || tr.Proximity.DeviceID != "pixel-8-abc123" || tr.Proximity.Night || tr.Initiation != InitStaticQR {
		t.Fatal(tr, err) // no limit set: any amount
	}
	if _, err := e.SetLimits("alice", Limits{ProximityPerTxCents: 50000, ProximityDailyCents: 200000, NightPerTxCents: 10000}); err != nil {
		t.Fatal(err)
	}
	if _, err := pay(50001, "pixel-8-abc123"); codeOf(err) != "proximity_limit_exceeded" {
		t.Fatal(err)
	}
	if _, err := pay(50000, "pixel-8-abc123"); err != nil {
		t.Fatal(err)
	}
	if _, err := pay(1, "pixel-8-abc123"); codeOf(err) != "proximity_daily_limit_exceeded" {
		t.Fatal(err)
	}
	// 21:00 in Brasília: night limit; the daily total is still today's.
	e.clock.Advance(12 * time.Hour)
	if _, err := pay(10001, "pixel-8-abc123"); codeOf(err) != "night_limit_exceeded" {
		t.Fatal(err)
	}
	// Next day 06:00: day again, new daily total.
	e.clock.Advance(9 * time.Hour)
	tr, err = pay(40000, "pixel-8-abc123")
	if err != nil || tr.Proximity.Night {
		t.Fatal(tr, err)
	}
	if l, _ := e.Limits("alice"); l.NightPerTxCents != 10000 {
		t.Fatal(l)
	}
	if _, err := e.SetLimits("alice", Limits{NightPerTxCents: -1}); codeOf(err) != "invalid_amount" {
		t.Fatal(err)
	}
	if _, err := e.SetLimits("nope", Limits{}); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.Limits("nope"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if e.Telemetry.ProximityPayments.Value("accepted") != 3 || e.Telemetry.ProximityPayments.Value("night_limit_exceeded") != 1 {
		t.Fatal("metrics")
	}
}
