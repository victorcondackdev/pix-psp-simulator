package sim

import (
	"regexp"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/bizday"
	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
)

// Pix por aproximação: the payer's phone reads the receiver's BR Code over
// NFC and pays it without the payer typing or scanning anything. The
// simulator models the parts a PSP must enforce: a registered device, and the
// payer's own limits (per transaction, per day and at night). There is no
// fixed regulatory cap on the amount; limits are per account and optional.

// ProximityInfo is attached to transfers initiated by proximity.
type ProximityInfo struct {
	DeviceID string `json:"device_id"`
	Night    bool   `json:"night"`
}

// Limits are an account's own Pix por aproximação limits; 0 means none.
type Limits struct {
	ProximityPerTxCents int64 `json:"proximity_per_transaction_cents"`
	ProximityDailyCents int64 `json:"proximity_daily_cents"`
	NightPerTxCents     int64 `json:"night_per_transaction_cents"`
}

// ProximityRequest pays a BR Code read over NFC.
type ProximityRequest struct {
	QRPaymentRequest
	DeviceID string `json:"device_id"`
}

var deviceRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,64}$`)

// SetLimits sets an account's limits.
func (s *Sim) SetLimits(accountID string, l Limits) (Limits, error) {
	if _, err := s.Account(accountID); err != nil {
		return Limits{}, err
	}
	if l.ProximityPerTxCents < 0 || l.ProximityDailyCents < 0 || l.NightPerTxCents < 0 {
		return Limits{}, invalid("invalid_amount", "limits must not be negative")
	}
	s.mu.Lock()
	s.limits[accountID] = l
	s.mu.Unlock()
	return l, nil
}

// Limits returns an account's limits.
func (s *Sim) Limits(accountID string) (Limits, error) {
	if _, err := s.Account(accountID); err != nil {
		return Limits{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits[accountID], nil
}

func (s *Sim) isNight(t time.Time) bool {
	since := t.In(bizday.Brasilia).Sub(bizday.Date(t))
	return since >= s.cfg.ProximityNightStart || since < s.cfg.ProximityNightEnd
}

// checkProximity enforces the payer's limits; it runs before the transfer
// is accepted, so a refusal is a 422 and nothing is reserved.
func (s *Sim) checkProximity(payer string, amount int64, p *ProximityInfo) error {
	now := s.Clock.Now()
	p.Night = s.isNight(now)
	refuse := func(code, format string, a ...any) error {
		s.Telemetry.ProximityPayments.Inc(code)
		return invalid(code, format, a...)
	}
	s.mu.Lock()
	l := s.limits[payer]
	var today int64
	day := bizday.Date(now)
	for _, t := range s.transfers {
		if t.Proximity != nil && t.Payer.AccountID == payer && t.Status != iso.StatusRejected && !bizday.Date(t.CreatedAt).Before(day) {
			today += t.AmountCents
		}
	}
	s.mu.Unlock()
	switch {
	case l.ProximityPerTxCents > 0 && amount > l.ProximityPerTxCents:
		return refuse("proximity_limit_exceeded", "amount exceeds the per-transaction proximity limit of %d", l.ProximityPerTxCents)
	case p.Night && l.NightPerTxCents > 0 && amount > l.NightPerTxCents:
		return refuse("night_limit_exceeded", "amount exceeds the night limit of %d", l.NightPerTxCents)
	case l.ProximityDailyCents > 0 && today+amount > l.ProximityDailyCents:
		return refuse("proximity_daily_limit_exceeded", "proximity payments today would reach %d, above the daily limit of %d", today+amount, l.ProximityDailyCents)
	}
	s.Telemetry.ProximityPayments.Inc("accepted")
	return nil
}

// PayProximity pays a BR Code read over NFC from a registered device.
func (s *Sim) PayProximity(r ProximityRequest, ov *chaos.Overrides) (Transfer, error) {
	if !deviceRE.MatchString(r.DeviceID) {
		return Transfer{}, invalid("invalid_device", "device_id must have 8 to 64 letters, digits or _.:-")
	}
	q := r.QRPaymentRequest
	q.proximity = &ProximityInfo{DeviceID: r.DeviceID}
	return s.PayQR(q, ov)
}
