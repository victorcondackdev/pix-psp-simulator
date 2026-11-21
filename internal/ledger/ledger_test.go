package ledger

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
)

func newLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	for _, id := range []string{"a", "b"} {
		if _, err := l.Open(Account{ID: id, ISPB: "99999999", Type: Checking, OwnerName: id}); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestOpenAndFind(t *testing.T) {
	l := newLedger(t)
	a, _ := l.Get("a")
	if a.Branch != "0001" || a.Number == "" || a.Status != Active {
		t.Fatalf("defaults not applied: %+v", a)
	}
	got, err := l.Find(a.ISPB, a.Branch, a.Number)
	if err != nil || got.ID != "a" {
		t.Fatalf("Find = %+v, %v", got, err)
	}
	if _, err := l.Open(Account{ID: "a"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate id: %v", err)
	}
	if _, err := l.Open(Account{ID: "c", ISPB: a.ISPB, Branch: a.Branch, Number: a.Number}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate number: %v", err)
	}
	if _, err := l.Find("1", "2", "3"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := l.Get("zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if n := len(l.List(false)); n != 2 {
		t.Fatalf("List(false) = %d", n)
	}
	all := l.List(true)
	if len(all) != 5 || all[0].System || !all[4].System {
		t.Fatalf("List(true) order: %+v", all)
	}
}

func TestMove(t *testing.T) {
	l := newLedger(t)
	tests := []struct {
		name     string
		from, to string
		amount   int64
		want     error
	}{
		{"fund a", Funding, "a", 1000, nil},
		{"a to b", "a", "b", 400, nil},
		{"overdraft", "a", "b", 601, ErrInsufficientFunds},
		{"zero", "a", "b", 0, ErrInvalidAmount},
		{"negative", "a", "b", -1, ErrInvalidAmount},
		{"self", "a", "a", 1, ErrSameAccount},
		{"unknown from", "x", "a", 1, ErrNotFound},
		{"unknown to", "a", "x", 1, ErrNotFound},
		{"exact balance", "a", "b", 600, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := l.Move(tt.from, tt.to, tt.amount, "ref-"+tt.name, tt.name)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	a, _ := l.Get("a")
	b, _ := l.Get("b")
	if a.Balance != 0 || b.Balance != 1000 {
		t.Fatalf("balances a=%d b=%d", a.Balance, b.Balance)
	}
	if l.Total() != 0 {
		t.Fatalf("Total = %d", l.Total())
	}
	if len(l.Postings("")) != 3 || len(l.Postings("ref-a to b")) != 1 {
		t.Fatalf("postings: %+v", l.Postings(""))
	}
}

func TestSetStatusAndTypes(t *testing.T) {
	l := newLedger(t)
	a, err := l.SetStatus("a", Closed)
	if err != nil || a.Status != Closed {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := l.SetStatus(Funding, Closed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("system account status change: %v", err)
	}
	for _, ty := range []Type{Checking, Savings, Salary, Payment} {
		if !ty.Valid() {
			t.Errorf("%s invalid", ty)
		}
	}
	if Type("XXXX").Valid() {
		t.Error("XXXX valid")
	}
}

// Concurrent random transfers never overdraw and conserve money.
func TestConcurrentMovesConserveMoney(t *testing.T) {
	l := New(clock.Real{})
	const n = 8
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("acc%d", i)
		if _, err := l.Open(Account{ID: id, ISPB: "99999999", Type: Checking}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Move(Funding, id, 10_000, "seed", ""); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 500; i++ {
				from := fmt.Sprintf("acc%d", r.Intn(n))
				to := fmt.Sprintf("acc%d", r.Intn(n))
				_, _ = l.Move(from, to, int64(r.Intn(3000)+1), "x", "")
			}
		}(int64(w))
	}
	wg.Wait()
	var customers int64
	for _, a := range l.List(false) {
		if a.Balance < 0 {
			t.Fatalf("negative balance %+v", a)
		}
		customers += a.Balance
	}
	if customers != n*10_000 || l.Total() != 0 {
		t.Fatalf("customers=%d total=%d", customers, l.Total())
	}
}
