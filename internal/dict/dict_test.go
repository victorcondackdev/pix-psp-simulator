package dict

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
)

const (
	cpfAlice = "52998224725"
	cpfBob   = "39053344705"
	cnpjCo   = "12ABC34501DE35"
)

var (
	alice  = Owner{AccountID: "alice", ISPB: "99999999", Branch: "0001", Number: "1", AccountType: "CACC", Name: "Alice", Document: cpfAlice}
	alice2 = Owner{AccountID: "alice2", ISPB: "88888888", Branch: "0001", Number: "9", AccountType: "CACC", Name: "Alice", Document: cpfAlice}
	bob    = Owner{AccountID: "bob", ISPB: "99999999", Branch: "0001", Number: "2", AccountType: "SVGS", Name: "Bob", Document: cpfBob}
	co     = Owner{AccountID: "co", ISPB: "99999999", Branch: "0001", Number: "3", AccountType: "CACC", Name: "Co Ltda", Document: cnpjCo}
)

func newDir() (*Directory, *clock.Fake) {
	c := clock.NewFake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	d := New(c)
	d.SetRand(bytes.NewReader(bytes.Repeat([]byte{0xab}, 1024)))
	return d, c
}

func TestRegister(t *testing.T) {
	d, _ := newDir()
	tests := []struct {
		name    string
		key     string
		evp     bool
		owner   Owner
		want    error
		wantKey string
		wantTyp string
	}{
		{"cpf", "529.982.247-25", false, alice, nil, cpfAlice, "CPF"},
		{"cpf of someone else", cpfBob, false, alice, ErrDocumentMismatch, "", ""},
		{"alphanumeric cnpj", "12.ABC.345/01DE-35", false, co, nil, cnpjCo, "CNPJ"},
		{"email lowercased", "Alice@Example.com", false, alice, nil, "alice@example.com", "EMAIL"},
		{"phone", "+55 (21) 98765-4321", false, alice, nil, "+5521987654321", "PHONE"},
		{"evp generated", "", true, alice, nil, "abababab-abab-4bab-abab-abababababab", "EVP"},
		{"duplicate same account", "alice@example.com", false, alice, ErrKeyExists, "", ""},
		{"duplicate other account", "alice@example.com", false, bob, ErrKeyExists, "", ""},
		{"invalid", "not a key", false, alice, pix.ErrInvalidKey, "", ""},
		{"empty without evp", "", false, alice, pix.ErrInvalidKey, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := d.Register(tt.key, tt.evp, tt.owner)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if err == nil && (e.Key != tt.wantKey || e.KeyType != tt.wantTyp) {
				t.Fatalf("entry = %+v", e)
			}
		})
	}
	if n := len(d.Keys("alice")); n != 4 {
		t.Fatalf("alice keys = %d", n)
	}
	if n := len(d.Keys("")); n != 5 {
		t.Fatalf("all keys = %d", n)
	}
}

func TestRegisterLimits(t *testing.T) {
	d, _ := newDir()
	for i := 0; i < MaxKeysNaturalPerson; i++ {
		if _, err := d.Register(fmt.Sprintf("a%d@x.com", i), false, alice); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Register("over@x.com", false, alice); !errors.Is(err, ErrKeyLimit) {
		t.Fatalf("got %v", err)
	}
	for i := 0; i < MaxKeysLegalPerson; i++ {
		if _, err := d.Register("", true, co); err != nil {
			t.Fatal(err)
		}
		d.SetRand(bytes.NewReader([]byte(strings.Repeat(fmt.Sprintf("%02d", i+1), 8))))
	}
	if _, err := d.Register("co@x.com", false, co); !errors.Is(err, ErrKeyLimit) {
		t.Fatalf("got %v", err)
	}
	d.SetRand(iotest.ErrReader(errors.New("boom")))
	if _, err := d.Register("", true, bob); err == nil {
		t.Fatal("expected rand error")
	}
}

func TestDeleteAndLookup(t *testing.T) {
	d, _ := newDir()
	if _, err := d.Register(cnpjCo, false, co); err != nil {
		t.Fatal(err)
	}
	r, err := d.Lookup("12.abc.345/01de-35", "payer1")
	if err != nil {
		t.Fatal(err)
	}
	if r.OwnerTaxID != "12.***.***/01DE-**" || r.OwnerType != "LEGAL_PERSON" || r.ISPB != "99999999" || r.AccountID != "co" {
		t.Fatalf("lookup = %+v", r)
	}
	if _, err := d.Register(cpfAlice, false, alice); err != nil {
		t.Fatal(err)
	}
	r, _ = d.Lookup(cpfAlice, "")
	if r.OwnerTaxID != "***.982.247-**" || r.OwnerType != "NATURAL_PERSON" {
		t.Fatalf("lookup = %+v", r)
	}
	tests := []struct {
		name, key, account string
		want               error
	}{
		{"invalid", "??", "co", pix.ErrInvalidKey},
		{"missing", "x@y.com", "co", ErrKeyNotFound},
		{"not owner", cnpjCo, "alice", ErrNotOwner},
		{"ok", cnpjCo, "co", nil},
		{"again", cnpjCo, "co", ErrKeyNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := d.Delete(tt.key, tt.account); !errors.Is(err, tt.want) {
				t.Fatalf("got %v want %v", err, tt.want)
			}
		})
	}
	if _, err := d.Lookup(cnpjCo, "payer1"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal(err)
	}
	if _, err := d.Lookup("bad", "payer1"); !errors.Is(err, pix.ErrInvalidKey) {
		t.Fatal(err)
	}
}

func TestLookupRateLimit(t *testing.T) {
	d, c := newDir()
	d.Limiter.SetConfig(RateLimit{Capacity: 5, RefillPerMinute: 1, NotFoundCost: 2})
	if _, err := d.Register("bob@x.com", false, bob); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := d.Lookup("bob@x.com", "scanner"); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	_, err := d.Lookup("bob@x.com", "scanner")
	var rl *RateLimitError
	if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) || rl.RetryAfter <= 0 {
		t.Fatalf("expected rate limit, got %v", err)
	}
	if rl.Error() == "" {
		t.Fatal("empty message")
	}
	// Another payer has its own bucket.
	if _, err := d.Lookup("bob@x.com", "honest"); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if _, err := d.Lookup("missing@x.com", "scanner"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal(err)
	}
	// Not-found penalty leaves the bucket in debt for 2 more minutes.
	c.Advance(time.Minute)
	if _, err := d.Lookup("bob@x.com", "scanner"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected penalty to apply, got %v", err)
	}
	c.Advance(3 * time.Minute)
	if _, err := d.Lookup("bob@x.com", "scanner"); err != nil {
		t.Fatal(err)
	}
}

func TestLimiterEdgeCases(t *testing.T) {
	c := clock.NewFake(time.Unix(0, 0))
	l := NewLimiter(c, RateLimit{})
	if ok, _, _ := l.Take("x", 1000); !ok {
		t.Fatal("disabled limiter should allow")
	}
	l.Charge("x", 5) // no-op when disabled
	l.SetConfig(RateLimit{Capacity: 1, RefillPerMinute: 0})
	l.Take("x", 1)
	if ok, wait, _ := l.Take("x", 1); ok || wait <= 0 {
		t.Fatal("no-refill bucket should be exhausted forever")
	}
	if l.Config().Capacity != 1 {
		t.Fatal("Config")
	}
}

func TestClaims(t *testing.T) {
	d, c := newDir()
	mustReg := func(key string, o Owner) {
		t.Helper()
		if _, err := d.Register(key, false, o); err != nil {
			t.Fatal(err)
		}
	}
	mustReg("+5521987654321", alice)
	mustReg("shared@x.com", alice)
	mustReg(cpfAlice, alice)
	mustReg("bob@x.com", bob)

	t.Run("open validation", func(t *testing.T) {
		tests := []struct {
			name  string
			typ   ClaimType
			key   string
			owner Owner
			want  error
		}{
			{"bad type", "STEAL", "shared@x.com", bob, ErrInvalidClaimType},
			{"bad key", Ownership, "??", bob, pix.ErrInvalidKey},
			{"missing key", Ownership, "nobody@x.com", bob, ErrKeyNotFound},
			{"own key", Ownership, "shared@x.com", alice, ErrClaimSameAccount},
			{"ownership of cpf", Ownership, cpfAlice, bob, ErrClaimNotAllowed},
			{"ownership same person", Ownership, "shared@x.com", alice2, ErrClaimNotAllowed},
			{"portability other person", Portability, "shared@x.com", bob, ErrClaimNotAllowed},
		}
		for _, tt := range tests {
			if _, err := d.OpenClaim(tt.typ, tt.key, tt.owner); !errors.Is(err, tt.want) {
				t.Errorf("%s: got %v want %v", tt.name, err, tt.want)
			}
		}
	})

	t.Run("portability happy path", func(t *testing.T) {
		cl, err := d.OpenClaim(Portability, cpfAlice, alice2)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.OpenClaim(Portability, cpfAlice, alice2); !errors.Is(err, ErrClaimInProgress) {
			t.Fatalf("second claim: %v", err)
		}
		steps := []struct {
			ev    ClaimEvent
			actor string
			want  error
			state ClaimStatus
		}{
			{EvComplete, "alice2", ErrInvalidTransition, ClaimOpen},
			{EvConfirm, "alice2", ErrClaimForbidden, ClaimOpen},
			{EvCancel, "bob", ErrClaimForbidden, ClaimOpen},
			{"steal", "alice", ErrInvalidTransition, ClaimOpen},
			{EvConfirm, "alice", nil, ClaimConfirmed},
			{EvComplete, "alice", ErrClaimForbidden, ClaimConfirmed},
			{EvComplete, "alice2", nil, ClaimCompleted},
			{EvCancel, "alice2", ErrInvalidTransition, ClaimCompleted},
		}
		for i, s := range steps {
			got, err := d.ActOnClaim(cl.ID, s.ev, s.actor)
			if !errors.Is(err, s.want) || got.Status != s.state {
				t.Fatalf("step %d (%s by %s): %v state=%s", i, s.ev, s.actor, err, got.Status)
			}
		}
		e, _ := d.Resolve(cpfAlice)
		if e.Owner.AccountID != "alice2" {
			t.Fatalf("key not moved: %+v", e)
		}
	})

	t.Run("ownership timeout confirms", func(t *testing.T) {
		cl, err := d.OpenClaim(Ownership, "+5521987654321", bob)
		if err != nil {
			t.Fatal(err)
		}
		c.Advance(d.ResolutionPeriod)
		got, _ := d.GetClaim(cl.ID)
		if got.Status != ClaimConfirmed {
			t.Fatalf("status = %s", got.Status)
		}
		if got, err = d.ActOnClaim(cl.ID, EvComplete, "bob"); err != nil || got.Status != ClaimCompleted {
			t.Fatalf("%v %s", err, got.Status)
		}
	})

	t.Run("portability timeout cancels", func(t *testing.T) {
		cl, err := d.OpenClaim(Portability, "shared@x.com", alice2)
		if err != nil {
			t.Fatal(err)
		}
		c.Advance(d.ResolutionPeriod + time.Second)
		if all := d.Claims(); all[len(all)-1].Status != ClaimCancelled {
			t.Fatalf("claims = %+v", all)
		}
		got, _ := d.GetClaim(cl.ID)
		if got.Status != ClaimCancelled {
			t.Fatalf("status = %s", got.Status)
		}
	})

	t.Run("delete cancels claim", func(t *testing.T) {
		cl, err := d.OpenClaim(Portability, "shared@x.com", alice2)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Delete("shared@x.com", "alice"); err != nil {
			t.Fatal(err)
		}
		got, _ := d.GetClaim(cl.ID)
		if got.Status != ClaimCancelled {
			t.Fatalf("status = %s", got.Status)
		}
		if _, err := d.ActOnClaim(cl.ID, EvCancel, "alice"); !errors.Is(err, ErrInvalidTransition) {
			t.Fatal(err)
		}
	})

	t.Run("complete after key deleted / limit", func(t *testing.T) {
		mustReg("gone@x.com", alice)
		cl, _ := d.OpenClaim(Ownership, "gone@x.com", bob)
		if _, err := d.ActOnClaim(cl.ID, EvConfirm, "alice"); err != nil {
			t.Fatal(err)
		}
		// Fill bob up to the limit.
		for i := 0; len(d.Keys("bob")) < MaxKeysNaturalPerson; i++ {
			mustReg(fmt.Sprintf("bob%d@x.com", i), bob)
		}
		if _, err := d.ActOnClaim(cl.ID, EvComplete, "bob"); !errors.Is(err, ErrKeyLimit) {
			t.Fatalf("got %v", err)
		}
	})

	if _, err := d.GetClaim("nope"); !errors.Is(err, ErrClaimNotFound) {
		t.Fatal(err)
	}
	if _, err := d.ActOnClaim("nope", EvCancel, "x"); !errors.Is(err, ErrClaimNotFound) {
		t.Fatal(err)
	}
}

func TestNewEVPShape(t *testing.T) {
	k, err := NewEVP(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if _, typ, err := Normalize(k); err != nil || typ != pix.KeyEVP {
		t.Fatalf("%s: %v %v", k, typ, err)
	}
}
