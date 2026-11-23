package sim

import (
	"fmt"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/bizday"
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

// Statement returns the camt.053 of an account for one calendar day
// (Brasília): balances before and after the day and every posting in it.
func (s *Sim) Statement(accountID, day string) ([]byte, error) {
	a, err := s.Account(accountID)
	if err != nil {
		return nil, err
	}
	d, err := parseDate("date", day)
	if err != nil {
		return nil, err
	}
	start, end := d, bizday.EndOfDay(d)
	opening, _ := s.Ledger.BalanceAt(accountID, start.Add(-time.Nanosecond))
	closing, _ := s.Ledger.BalanceAt(accountID, end)
	st := iso.Statement{
		ID: "STMT-" + accountID + "-" + d.Format("20060102"), CreatedAt: s.Clock.Now(), AccountID: a.ID, ISPB: a.ISPB,
		Branch: a.Branch, Number: a.Number, OwnerName: a.OwnerName, From: start, To: end,
		OpeningCnts: opening, ClosingCnts: closing,
	}
	for _, p := range s.Ledger.Postings("") {
		if p.At.Before(start) || p.At.After(end) || (p.From != accountID && p.To != accountID) {
			continue
		}
		st.Entries = append(st.Entries, iso.StatementEntry{
			Ref: p.Ref, ServicerRef: fmt.Sprint(p.Seq), AmountCents: p.Amount, Credit: p.To == accountID,
			BookedAt: p.At, Info: p.Memo,
		})
	}
	return iso.Camt053(st)
}
