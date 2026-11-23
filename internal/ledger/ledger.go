// Package ledger is a minimal in-memory double-entry ledger.
//
// Every movement is a single posting that takes an amount from one account and
// gives it to another, so the sum of all balances never changes. Money enters
// the system from the Funding account, which is the only one allowed to go
// negative; the total over all accounts is therefore always zero, which the
// tests use as a conservation check. Amounts are int64 cents.
package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
)

// System account IDs.
const (
	Funding    = "sys.funding"    // source of seeded balances; may go negative
	Settlement = "sys.settlement" // funds of transfers in flight
	MEDHold    = "sys.med-hold"   // funds under MED cautionary block
)

// Type is the account type as used by the DICT.
type Type string

// Account types. CACC (checking), SVGS (savings), SLRY (salary) and TRAN
// (payment account) are the four types the DICT accepts.
const (
	Checking Type = "CACC"
	Savings  Type = "SVGS"
	Salary   Type = "SLRY"
	Payment  Type = "TRAN"
)

// Valid reports whether t is a known account type.
func (t Type) Valid() bool {
	switch t {
	case Checking, Savings, Salary, Payment:
		return true
	}
	return false
}

// Status of an account; non-active accounts cannot receive or send.
type Status string

// Account statuses.
const (
	Active  Status = "ACTIVE"
	Blocked Status = "BLOCKED"
	Closed  Status = "CLOSED"
)

// Account is a balance holder.
type Account struct {
	ID            string    `json:"id"`
	ISPB          string    `json:"ispb"`
	Branch        string    `json:"branch"`
	Number        string    `json:"number"`
	Type          Type      `json:"type"`
	OwnerName     string    `json:"owner_name"`
	OwnerDocument string    `json:"owner_document"`
	Status        Status    `json:"status"`
	Balance       int64     `json:"balance_cents"`
	System        bool      `json:"system,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Posting is one balanced movement.
type Posting struct {
	Seq    int64     `json:"seq"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Amount int64     `json:"amount_cents"`
	Ref    string    `json:"ref"`
	Memo   string    `json:"memo"`
	At     time.Time `json:"at"`
}

// Errors.
var (
	ErrNotFound          = errors.New("account not found")
	ErrExists            = errors.New("account already exists")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrInvalidAmount     = errors.New("amount must be positive")
	ErrSameAccount       = errors.New("source and destination are the same account")
)

// Ledger holds accounts and postings. It is safe for concurrent use.
type Ledger struct {
	mu       sync.RWMutex
	clock    clock.Clock
	accounts map[string]*Account
	byNumber map[string]string
	postings []Posting
	seq      int64
	nextNum  int
}

// New returns a ledger with its system accounts.
func New(c clock.Clock) *Ledger {
	l := &Ledger{clock: c, accounts: map[string]*Account{}, byNumber: map[string]string{}, nextNum: 10000}
	for _, id := range []string{Funding, Settlement, MEDHold} {
		l.accounts[id] = &Account{ID: id, OwnerName: id, Status: Active, System: true, CreatedAt: c.Now()}
	}
	return l
}

func numberKey(ispb, branch, number string) string { return ispb + "|" + branch + "|" + number }

// Open creates an account. ID, ISPB and Type must be set by the caller;
// Branch defaults to "0001" and Number is assigned when empty.
func (l *Ledger) Open(a Account) (Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.accounts[a.ID]; ok {
		return Account{}, fmt.Errorf("%w: %s", ErrExists, a.ID)
	}
	if a.Branch == "" {
		a.Branch = "0001"
	}
	if a.Number == "" {
		for {
			l.nextNum++
			a.Number = strconv.Itoa(l.nextNum)
			if _, taken := l.byNumber[numberKey(a.ISPB, a.Branch, a.Number)]; !taken {
				break
			}
		}
	}
	k := numberKey(a.ISPB, a.Branch, a.Number)
	if _, ok := l.byNumber[k]; ok {
		return Account{}, fmt.Errorf("%w: %s/%s/%s", ErrExists, a.ISPB, a.Branch, a.Number)
	}
	a.Status = Active
	a.Balance = 0
	a.System = false
	a.CreatedAt = l.clock.Now()
	l.accounts[a.ID] = &a
	l.byNumber[k] = a.ID
	return a, nil
}

// Get returns an account by ID.
func (l *Ledger) Get(id string) (Account, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.accounts[id]
	if !ok {
		return Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return *a, nil
}

// Find returns the account with the given ISPB, branch and number.
func (l *Ledger) Find(ispb, branch, number string) (Account, error) {
	l.mu.RLock()
	id, ok := l.byNumber[numberKey(ispb, branch, number)]
	l.mu.RUnlock()
	if !ok {
		return Account{}, fmt.Errorf("%w: %s/%s/%s", ErrNotFound, ispb, branch, number)
	}
	return l.Get(id)
}

// List returns every account, customer accounts first, sorted by ID.
func (l *Ledger) List(includeSystem bool) []Account {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Account, 0, len(l.accounts))
	for _, a := range l.accounts {
		if a.System && !includeSystem {
			continue
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].System != out[j].System {
			return !out[i].System
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SetStatus changes an account status.
func (l *Ledger) SetStatus(id string, s Status) (Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.accounts[id]
	if !ok || a.System {
		return Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	a.Status = s
	return *a, nil
}

// Move posts amount from one account to another. Customer accounts may not
// go negative.
func (l *Ledger) Move(from, to string, amount int64, ref, memo string) (Posting, error) {
	if amount <= 0 {
		return Posting{}, ErrInvalidAmount
	}
	if from == to {
		return Posting{}, ErrSameAccount
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	src, ok := l.accounts[from]
	if !ok {
		return Posting{}, fmt.Errorf("%w: %s", ErrNotFound, from)
	}
	dst, ok := l.accounts[to]
	if !ok {
		return Posting{}, fmt.Errorf("%w: %s", ErrNotFound, to)
	}
	if !src.System && src.Balance < amount {
		return Posting{}, ErrInsufficientFunds
	}
	src.Balance -= amount
	dst.Balance += amount
	l.seq++
	p := Posting{Seq: l.seq, From: from, To: to, Amount: amount, Ref: ref, Memo: memo, At: l.clock.Now()}
	l.postings = append(l.postings, p)
	return p, nil
}

// Postings returns the postings with the given ref, or all when ref is "".
func (l *Ledger) Postings(ref string) []Posting {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Posting
	for _, p := range l.postings {
		if ref == "" || p.Ref == ref {
			out = append(out, p)
		}
	}
	return out
}

// Total returns the sum of all balances; it is zero unless there is a bug.
func (l *Ledger) Total() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var t int64
	for _, a := range l.accounts {
		t += a.Balance
	}
	return t
}

// BalanceAt returns the balance of an account after every posting made at or
// before t, by replaying the journal. Regulatory reports use it to read an
// end-of-day balance after the day is over.
func (l *Ledger) BalanceAt(id string, t time.Time) (int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.accounts[id]; !ok {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	var b int64
	for _, p := range l.postings {
		if p.At.After(t) {
			break // postings are appended in clock order
		}
		if p.From == id {
			b -= p.Amount
		}
		if p.To == id {
			b += p.Amount
		}
	}
	return b, nil
}
