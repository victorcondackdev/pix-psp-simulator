package sim

import (
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
)

func TestCreateChargeValidation(t *testing.T) {
	e := newEnv(t, Config{})
	long := strings.Repeat("A", 36)
	tests := []struct {
		name string
		r    ChargeRequest
		kind Kind
	}{
		{"static ok", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12.ABC.345/01DE-35", AmountCents: 1250, Description: "Pao"}, 0},
		{"static open amount", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35", TxID: "MESA12"}, 0},
		{"dynamic ok", ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: 990}, 0},
		{"dynamic with txid", ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: 1, TxID: strings.Repeat("a", 26)}, 0},
		{"dup txid", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35", TxID: "MESA12"}, KindConflict},
		{"unknown receiver", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "ghost"}, KindNotFound},
		{"bad type", ChargeRequest{Type: "weekly", ReceiverAccountID: "padaria"}, KindInvalid},
		{"static no key", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria"}, KindInvalid},
		{"key of other account", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "alice@example.com"}, KindInvalid},
		{"static expiry", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35", ExpiresInSeconds: 10}, KindInvalid},
		{"static bad txid", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35", TxID: "has space"}, KindInvalid},
		{"dynamic no amount", ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria"}, KindInvalid},
		{"dynamic negative expiry", ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: 1, ExpiresInSeconds: -1}, KindInvalid},
		{"dynamic short txid", ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: 1, TxID: "short"}, KindInvalid},
		{"dynamic long txid", ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: 1, TxID: long}, KindInvalid},
		{"negative amount", ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: -1}, KindInvalid},
		{"name too long for brcode", ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35", MerchantName: strings.Repeat("N", 30)}, KindInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := e.CreateCharge(tt.r)
			if kindOf(err) != tt.kind {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				return
			}
			b, err := pix.Decode(c.BRCode)
			if err != nil {
				t.Fatalf("generated BR Code does not decode: %v", err)
			}
			if issues := pix.Lint(c.BRCode); len(issues) != 0 {
				t.Fatalf("lint: %v", issues)
			}
			if tt.r.Type == ChargeDynamic {
				if !b.Dynamic || !strings.HasPrefix(b.URL, "localhost:8080/qr/v2/") || c.ExpiresAt == nil || !c.ExpiresAt.Equal(t0.Add(time.Hour)) {
					t.Fatalf("dynamic = %+v / %+v", c, b)
				}
			} else if !b.Static || b.Cents != tt.r.AmountCents || b.TxID != c.TxID || b.MerchantName != "Padaria Pao Quente Ltda" {
				t.Fatalf("static = %+v", b)
			}
		})
	}
	if n := len(e.Charges()); n != 4 {
		t.Fatalf("charges = %d", n)
	}
	// Long owner names are truncated to fit the BR Code.
	long2, _ := e.CreateAccount(AccountRequest{ID: "longname", OwnerName: "Associacao dos Moradores do Bairro Grande", Document: "11222333000181"})
	if _, err := e.RegisterKey(long2.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateCharge(ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: long2.ID, AmountCents: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestPayStaticQR(t *testing.T) {
	e := newEnv(t, Config{})
	fixed, _ := e.CreateCharge(ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35", AmountCents: 1250, TxID: "PEDIDO1"})
	open, _ := e.CreateCharge(ChargeRequest{Type: ChargeStatic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35"})

	tr, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: fixed.BRCode}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Initiation != InitStaticQR || tr.AmountCents != 1250 || tr.TxID != "PEDIDO1" || tr.Payee.AccountID != "padaria" {
		t.Fatalf("transfer = %+v", tr)
	}
	// Static codes can be paid again.
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "bob", BRCode: fixed.BRCode, AmountCents: 1250}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "bob", BRCode: fixed.BRCode, AmountCents: 1}, nil); kindOf(err) != KindInvalid {
		t.Fatalf("amount mismatch: %v", err)
	}
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "bob", BRCode: open.BRCode}, nil); kindOf(err) != KindInvalid {
		t.Fatalf("open amount without amount_cents: %v", err)
	}
	if tr, err := e.PayQR(QRPaymentRequest{FromAccountID: "bob", BRCode: open.BRCode, AmountCents: 77}, nil); err != nil || tr.AmountCents != 77 {
		t.Fatal(tr, err)
	}
	e.settle()
	c, _ := e.Charge(fixed.TxID)
	if c.Status != ChargeActive || len(c.Payments) != 2 {
		t.Fatalf("static charge = %+v", c)
	}
	if e.balance(t, "padaria") != 1250*2+77 {
		t.Fatalf("padaria balance = %d", e.balance(t, "padaria"))
	}
	// A static code made elsewhere for a key in the DICT works too.
	ext, _ := pix.NewStatic(pix.StaticOptions{Key: "bob@nowhere.com", MerchantName: "X", MerchantCity: "Y", Cents: 1})
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: ext}, nil); kindOf(err) != KindNotFound {
		t.Fatalf("unknown key: %v", err)
	}
	ext, _ = pix.NewStatic(pix.StaticOptions{Key: "39053344705", MerchantName: "Bob", MerchantCity: "Rio", Cents: 3})
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: ext}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPayQRLintErrors(t *testing.T) {
	e := newEnv(t, Config{})
	good, _ := pix.NewStatic(pix.StaticOptions{Key: "39053344705", MerchantName: "Bob", MerchantCity: "Rio", Cents: 100})
	badCRC := good[:len(good)-4] + "0000"
	comma := strings.Replace(good, "54041.00", "54041,00", 1)
	tests := []struct {
		name, code string
	}{
		{"empty", ""},
		{"garbage", "hello"},
		{"bad crc", badCRC},
		{"comma amount", comma},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: tt.code}, nil)
			se, ok := err.(*Error)
			if !ok || se.Kind != KindInvalid {
				t.Fatalf("err = %v", err)
			}
			if se.Code == "brcode_lint_failed" && len(se.Issues) == 0 {
				t.Fatal("lint error without issues")
			}
		})
	}
}

func TestPayDynamicQR(t *testing.T) {
	e := newEnv(t, Config{})
	c, err := e.CreateCharge(ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", Key: "12ABC34501DE35", AmountCents: 4590, Description: "Bolo", ExpiresInSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	loc, err := e.Location(c.LocationID)
	if err != nil || loc.TxID != c.TxID || loc.Valor.Original != "45.90" || loc.Calendario.Expiracao != 600 || loc.Status != ChargeActive || loc.Chave != "12ABC34501DE35" {
		t.Fatalf("location = %+v %v", loc, err)
	}
	if _, err := e.Location("nope"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: c.BRCode, AmountCents: 1}, nil); kindOf(err) != KindInvalid {
		t.Fatalf("amount mismatch: %v", err)
	}
	tr, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: c.BRCode}, nil)
	if err != nil || tr.Initiation != InitDynamicQR || tr.TxID != c.TxID || tr.AmountCents != 4590 {
		t.Fatalf("%+v %v", tr, err)
	}
	// Single use: a second payment while the first is in flight conflicts.
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "bob", BRCode: c.BRCode}, nil); kindOf(err) != KindConflict {
		t.Fatalf("in flight: %v", err)
	}
	if _, err := e.RemoveCharge(c.TxID); kindOf(err) != KindConflict {
		t.Fatalf("remove in flight: %v", err)
	}
	e.settle()
	got, _ := e.Charge(c.TxID)
	if got.Status != ChargeCompleted || len(got.Payments) != 1 {
		t.Fatalf("charge = %+v", got)
	}
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "bob", BRCode: c.BRCode}, nil); kindOf(err) != KindConflict {
		t.Fatalf("paid twice: %v", err)
	}

	// A rejected payment leaves the charge payable.
	c2, _ := e.CreateCharge(ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: 10})
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: c2.BRCode}, chaosFail(iso.AB03)); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if got, _ := e.Charge(c2.TxID); got.Status != ChargeActive {
		t.Fatalf("after rejection = %s", got.Status)
	}
	if _, err := e.RemoveCharge(c2.TxID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: c2.BRCode}, nil); kindOf(err) != KindConflict {
		t.Fatalf("removed charge paid: %v", err)
	}
	if _, err := e.RemoveCharge(c2.TxID); kindOf(err) != KindConflict {
		t.Fatal(err)
	}
	if _, err := e.RemoveCharge("nope"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.Charge("nope"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
}

func TestDynamicChargeExpires(t *testing.T) {
	e := newEnv(t, Config{ChargeExpiry: 30 * time.Minute})
	c, _ := e.CreateCharge(ChargeRequest{Type: ChargeDynamic, ReceiverAccountID: "padaria", AmountCents: 10})
	e.clock.Advance(29 * time.Minute)
	if got, _ := e.Charge(c.TxID); got.Status != ChargeActive {
		t.Fatal(got.Status)
	}
	e.clock.Advance(time.Minute)
	got, _ := e.Charge(c.TxID)
	if got.Status != ChargeExpired || !got.Expired {
		t.Fatalf("not expired: %+v", got)
	}
	if loc, _ := e.Location(c.LocationID); loc.Status != ChargeExpired {
		t.Fatal(loc.Status)
	}
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: c.BRCode}, nil); kindOf(err) != KindConflict {
		t.Fatalf("expired charge paid: %v", err)
	}
}

func TestDynamicForeignLocation(t *testing.T) {
	e := newEnv(t, Config{})
	foreign, _ := pix.NewDynamic(pix.DynamicOptions{URL: "https://psp.example.com/cobv/123", MerchantName: "X", MerchantCity: "Y"})
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: foreign}, nil); kindOf(err) != KindInvalid {
		t.Fatalf("foreign location: %v", err)
	}
	unknown, _ := pix.NewDynamic(pix.DynamicOptions{URL: "https://localhost:8080/qr/v2/ffff", MerchantName: "X", MerchantCity: "Y"})
	if _, err := e.PayQR(QRPaymentRequest{FromAccountID: "alice", BRCode: unknown}, nil); kindOf(err) != KindNotFound {
		t.Fatalf("unknown location: %v", err)
	}
}
