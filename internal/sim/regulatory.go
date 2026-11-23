package sim

import (
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
)

// BalanceAt implements sta.Books.
func (s *Sim) BalanceAt(accountID string, t time.Time) (int64, error) {
	return s.Ledger.BalanceAt(accountID, t)
}

// Now implements sta.Books.
func (s *Sim) Now() time.Time { return s.Clock.Now() }

// Volume implements sta.Books: the sum of the amounts of the transfers
// settled in [from, to] whose payer or payee belongs to ispb. A transfer
// between two accounts of the same ISPB counts once.
func (s *Sim) Volume(ispb string, from, to time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v int64
	for _, t := range s.transfers {
		if t.Status != iso.StatusSettled || t.SettledAt == nil || t.SettledAt.Before(from) || t.SettledAt.After(to) {
			continue
		}
		if t.Payer.ISPB == ispb || t.Payee.ISPB == ispb {
			v += t.AmountCents
		}
	}
	return v
}
