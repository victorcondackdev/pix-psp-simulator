package sim

import (
	"sort"
	"strconv"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
)

// MED versions.
const (
	MED1 = "1.0" // block only what is left in the payee's account
	MED2 = "2.0" // follow the funds through the accounts they were moved to
)

type clockTimer = clock.Timer

// Hop statuses.
const (
	HopBlocked  = "BLOCKED"
	HopReturned = "RETURNED" // sent back to the victim (FR01)
	HopReleased = "RELEASED" // given back to the account holder
	HopAtPayer  = "AT_PAYER" // the traced funds went back to the victim: nothing to block
)

// Hop is one account where MED blocked funds.
type Hop struct {
	Depth int `json:"depth"` // 0 is the original payee
	// AccountID holds the funds; ISPB is its institution.
	AccountID string `json:"account_id"`
	ISPB      string `json:"ispb"`
	// ViaEndToEndID is the transfer that brought the traced funds to this
	// account (the reported transfer at depth 0).
	ViaEndToEndID string `json:"via_end_to_end_id"`
	TracedCents   int64  `json:"traced_cents"` // amount followed into the account
	HeldCents     int64  `json:"held_cents"`   // amount actually blocked there
	Status        string `json:"status"`
}

type traceNode struct {
	account string
	ispb    string
	via     string
	since   time.Time
	amount  int64
	depth   int
}

// outgoingLocked lists settled transfers paid by account after since,
// oldest first, that are not already part of the trace.
func (s *Sim) outgoingLocked(account string, since time.Time, used map[string]int64) []*Transfer {
	var out []*Transfer
	for _, e2e := range s.order {
		t := s.transfers[e2e]
		if t.Payer.AccountID != account || t.Status != iso.StatusSettled || t.SettledAt == nil || t.SettledAt.Before(since) {
			continue
		}
		if used[e2e] >= t.AmountCents-t.RefundedCents {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SettledAt.Before(*out[j].SettledAt) })
	return out
}

// blockLocked places the cautionary block. MED 1.0 holds what is left at
// the payee. MED 2.0 walks the funds breadth first: whatever the account no
// longer has is followed, first in first out, into the transfers it made
// after receiving them, up to MEDMaxAccounts accounts, blocking at each one.
func (s *Sim) blockLocked(inf *Infraction, t *Transfer, step *Step) {
	remaining := t.AmountCents - t.RefundedCents
	queue := []traceNode{{account: inf.payeeAcct, ispb: t.Payee.ISPB, via: t.EndToEndID, since: *t.SettledAt, amount: remaining}}
	used := map[string]int64{t.EndToEndID: t.AmountCents} // never trace back through the reported transfer
	visited := 0
	for len(queue) > 0 && visited < s.cfg.MEDMaxAccounts {
		n := queue[0]
		queue = queue[1:]
		visited++
		acct, err := s.Ledger.Get(n.account)
		if err != nil || acct.System {
			continue
		}
		if n.account == t.Payer.AccountID {
			inf.Trace = append(inf.Trace, Hop{Depth: n.depth, AccountID: n.account, ISPB: n.ispb, ViaEndToEndID: n.via, TracedCents: n.amount, Status: HopAtPayer})
			continue
		}
		take := n.amount
		if acct.Balance < take {
			take = acct.Balance
		}
		if take > 0 {
			if _, err := s.Ledger.Move(n.account, ledger.MEDHold, take, inf.ID, "MED cautionary block"); err != nil {
				take = 0
			}
		}
		inf.Trace = append(inf.Trace, Hop{
			Depth: n.depth, AccountID: n.account, ISPB: n.ispb, ViaEndToEndID: n.via,
			TracedCents: n.amount, HeldCents: take, Status: HopBlocked,
		})
		inf.HeldCents += take
		s.Telemetry.MEDBlockedCents.Add(float64(take), itoa(n.depth))
		left := n.amount - take
		if inf.Version != MED2 || left == 0 {
			continue
		}
		for _, out := range s.outgoingLocked(n.account, n.since, used) {
			if left == 0 {
				break
			}
			avail := out.AmountCents - out.RefundedCents - used[out.EndToEndID]
			follow := avail
			if left < follow {
				follow = left
			}
			used[out.EndToEndID] += follow
			left -= follow
			queue = append(queue, traceNode{
				account: out.Payee.AccountID, ispb: out.Payee.ISPB, via: out.EndToEndID,
				since: *out.SettledAt, amount: follow, depth: n.depth + 1,
			})
		}
	}
	s.Telemetry.MEDTracedHops.Observe(float64(len(inf.Trace)))
	switch {
	case inf.HeldCents == 0:
		step.Note = "nothing could be blocked"
	case inf.HeldCents < remaining && inf.Version == MED2:
		step.Note = "partial block after tracing " + itoa(len(inf.Trace)) + " account(s)"
	case inf.HeldCents < remaining:
		step.Note = "partial block: payee balance was insufficient"
	case len(inf.Trace) > 1:
		step.Note = "funds traced across " + itoa(len(inf.Trace)) + " accounts"
	}
	exp := s.Clock.Now().Add(s.cfg.MEDBlockWindow)
	inf.BlockExpiresAt = &exp
	id := inf.ID
	inf.expiry = s.Clock.AfterFunc(s.cfg.MEDBlockWindow, func() {
		s.mu.Lock()
		i, ok := s.infractions[id]
		blocked := ok && i.Status == MEDBlocked
		if blocked {
			i.expiry = nil
		}
		s.mu.Unlock()
		if blocked {
			_, _ = s.ActOnInfraction(id, MEDExpire)
		}
	})
}

// releaseLocked gives every blocked hop back to its account holder.
func (s *Sim) releaseLocked(inf *Infraction, status string) {
	for i := range inf.Trace {
		h := &inf.Trace[i]
		if h.Status != HopBlocked {
			continue
		}
		if h.HeldCents > 0 {
			_, _ = s.Ledger.Move(ledger.MEDHold, h.AccountID, h.HeldCents, inf.ID, "MED block released")
		}
		h.Status = status
	}
	inf.HeldCents = 0
}

func itoa(n int) string { return strconv.Itoa(n) }
