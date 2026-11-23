package sim

import (
	"fmt"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
)

// Fraud risk levels.
const (
	RiskLow    = "LOW"
	RiskMedium = "MEDIUM"
	RiskHigh   = "HIGH"
)

// RiskSignal is one input of the fraud score, with what it read.
type RiskSignal struct {
	Name   string `json:"name"`
	Weight int    `json:"weight"`
	Detail string `json:"detail"`
}

// FraudRisk is a synthetic, explainable fraud probability indicator returned
// with DICT lookups, in the spirit of the shared indicators the DICT is
// moving towards: the payer's PSP sees why a beneficiary looks risky before
// sending money. It is computed from the simulator's own history; the
// weights are illustrative, not the BCB's.
type FraudRisk struct {
	Score   int          `json:"score"` // 0..100
	Level   string       `json:"level"` // LOW, MEDIUM, HIGH
	Signals []RiskSignal `json:"signals"`
}

// FraudRisk scores the account that owns a key created at keyCreated.
func (s *Sim) FraudRisk(accountID string, keyCreated time.Time) FraudRisk {
	now := s.Clock.Now()
	r := FraudRisk{Signals: []RiskSignal{}}
	add := func(name string, w int, format string, a ...any) {
		r.Signals = append(r.Signals, RiskSignal{Name: name, Weight: w, Detail: fmt.Sprintf(format, a...)})
		r.Score += w
	}
	if age := now.Sub(keyCreated); age < 24*time.Hour {
		add("key.new", 20, "key registered %s ago", age.Round(time.Minute))
	} else if age < 7*24*time.Hour {
		add("key.recent", 10, "key registered %d day(s) ago", int(age.Hours()/24))
	}
	if a, err := s.Ledger.Get(accountID); err == nil && now.Sub(a.CreatedAt) < 7*24*time.Hour {
		add("account.new", 15, "account opened %s ago", now.Sub(a.CreatedAt).Round(time.Minute))
	}

	s.mu.Lock()
	marks, open := 0, 0
	for _, inf := range s.infractions {
		involved := inf.payeeAcct == accountID
		for _, h := range inf.Trace {
			if h.AccountID == accountID && h.Status != HopAtPayer {
				involved = true
			}
		}
		if !involved || now.Sub(inf.CreatedAt) > 90*24*time.Hour {
			continue
		}
		switch inf.Status {
		case MEDRefunded:
			marks++
		case MEDOpen, MEDBlocked, MEDAnalysis:
			open++
		}
	}
	payers := map[string]bool{}
	var in, out int64
	since := now.Add(-24 * time.Hour)
	for _, t := range s.transfers {
		if t.Status != iso.StatusSettled || t.SettledAt == nil || t.SettledAt.Before(since) {
			continue
		}
		if t.Payee.AccountID == accountID {
			payers[t.Payer.AccountID] = true
			in += t.AmountCents
		}
		if t.Payer.AccountID == accountID {
			out += t.AmountCents
		}
	}
	s.mu.Unlock()

	if marks > 0 {
		w := 40 * marks
		if w > 60 {
			w = 60
		}
		add("med.fraud_mark", w, "%d MED infraction(s) accepted against this account in 90 days", marks)
	}
	if open > 0 {
		add("med.open", 15, "%d MED infraction(s) under way", open)
	}
	if len(payers) >= 5 {
		add("inbound.fan_in", 15, "%d distinct payers in 24h", len(payers))
	}
	if in > 0 && out*100 >= in*80 {
		add("flow.pass_through", 15, "forwarded %d%% of the %d cents received in 24h", out*100/in, in)
	}
	if r.Score > 100 {
		r.Score = 100
	}
	switch {
	case r.Score >= 60:
		r.Level = RiskHigh
	case r.Score >= 30:
		r.Level = RiskMedium
	default:
		r.Level = RiskLow
	}
	return r
}
