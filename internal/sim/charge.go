package sim

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
)

// Charge types.
const (
	ChargeStatic  = "static"
	ChargeDynamic = "dynamic"
)

// Charge statuses, named as in the BCB Pix API (cob).
const (
	ChargeActive    = "ATIVA"
	ChargeCompleted = "CONCLUIDA"
	ChargeRemoved   = "REMOVIDA_PELO_USUARIO_RECEBEDOR"
	ChargeExpired   = "REMOVIDA_PELO_PSP" // the simulator uses it for expiry
)

// LocationPath is the path prefix of dynamic QR code payload locations.
const LocationPath = "/qr/v2/"

// Charge is a cob: a request for payment rendered as a BR Code.
type Charge struct {
	TxID              string     `json:"txid"`
	Type              string     `json:"type"`
	Status            string     `json:"status"`
	ReceiverAccountID string     `json:"receiver_account_id"`
	Key               string     `json:"key,omitempty"`
	AmountCents       int64      `json:"amount_cents"`
	Description       string     `json:"description,omitempty"`
	MerchantName      string     `json:"merchant_name"`
	MerchantCity      string     `json:"merchant_city"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	Expired           bool       `json:"expired,omitempty"`
	LocationID        string     `json:"location_id,omitempty"`
	Location          string     `json:"location,omitempty"`
	BRCode            string     `json:"brcode"`
	Payments          []string   `json:"payments"`

	inFlight string
}

func (c *Charge) refresh(now time.Time) {
	if c.Status == ChargeActive && c.inFlight == "" && c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		c.Status, c.Expired = ChargeExpired, true
	}
}

func (c *Charge) snapshot() Charge {
	x := *c
	x.Payments = append([]string{}, c.Payments...)
	return x
}

// ChargeRequest creates a charge.
type ChargeRequest struct {
	Type              string `json:"type"`
	ReceiverAccountID string `json:"receiver_account_id"`
	// Key must be one of the receiver's DICT keys; required for static
	// charges, optional for dynamic ones (shown in the payload).
	Key              string `json:"key,omitempty"`
	AmountCents      int64  `json:"amount_cents,omitempty"`
	Description      string `json:"description,omitempty"`
	MerchantName     string `json:"merchant_name,omitempty"`
	MerchantCity     string `json:"merchant_city,omitempty"`
	ExpiresInSeconds int64  `json:"expires_in_seconds,omitempty"`
	TxID             string `json:"txid,omitempty"`
}

func validTxID(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(alnum, c) {
			return false
		}
	}
	return true
}

// CreateCharge creates a static or dynamic charge and its BR Code.
func (s *Sim) CreateCharge(r ChargeRequest) (Charge, error) {
	acct, err := s.Account(r.ReceiverAccountID)
	if err != nil {
		return Charge{}, err
	}
	if r.AmountCents < 0 || r.AmountCents > pix.MaxCents {
		return Charge{}, invalid("invalid_amount", "amount_cents must be between 0 and %d", int64(pix.MaxCents))
	}
	if r.MerchantName == "" {
		r.MerchantName = acct.OwnerName
		if len(r.MerchantName) > pix.MaxMerchantNameLength {
			r.MerchantName = strings.TrimSpace(r.MerchantName[:pix.MaxMerchantNameLength])
		}
	}
	if r.MerchantCity == "" {
		r.MerchantCity = s.cfg.MerchantCity
	}
	if r.Key != "" {
		e, err := s.DICT.Resolve(r.Key)
		if err != nil || e.Owner.AccountID != acct.ID {
			return Charge{}, invalid("key_not_owned", "key is not registered to the receiver account")
		}
		r.Key = e.Key
	}
	now := s.Clock.Now()
	c := &Charge{
		Type: r.Type, Status: ChargeActive, ReceiverAccountID: acct.ID, Key: r.Key, AmountCents: r.AmountCents,
		Description: r.Description, MerchantName: r.MerchantName, MerchantCity: r.MerchantCity,
		CreatedAt: now, Payments: []string{},
	}
	switch r.Type {
	case ChargeStatic:
		if r.Key == "" {
			return Charge{}, invalid("key_required", "static charges need the receiver's key")
		}
		if r.ExpiresInSeconds != 0 {
			return Charge{}, invalid("invalid_expiration", "static BR Codes do not expire")
		}
		if r.TxID == "" {
			r.TxID = s.randomAlnum(pix.MaxTxIDLength)
		} else if !validTxID(r.TxID, 1, pix.MaxTxIDLength) {
			return Charge{}, invalid("invalid_txid", "static txid must be 1-25 characters [A-Za-z0-9]")
		}
		c.TxID = r.TxID
		c.BRCode, err = pix.NewStatic(pix.StaticOptions{
			Key: r.Key, MerchantName: r.MerchantName, MerchantCity: r.MerchantCity,
			Cents: r.AmountCents, TxID: r.TxID, Description: r.Description,
		})
	case ChargeDynamic:
		if r.AmountCents == 0 {
			return Charge{}, invalid("invalid_amount", "dynamic charges need amount_cents")
		}
		exp := s.cfg.ChargeExpiry
		if r.ExpiresInSeconds < 0 {
			return Charge{}, invalid("invalid_expiration", "expires_in_seconds must be positive")
		} else if r.ExpiresInSeconds > 0 {
			exp = time.Duration(r.ExpiresInSeconds) * time.Second
		}
		at := now.Add(exp)
		c.ExpiresAt = &at
		if r.TxID == "" {
			r.TxID = s.randomAlnum(32)
		} else if !validTxID(r.TxID, 26, 35) {
			return Charge{}, invalid("invalid_txid", "dynamic txid must be 26-35 characters [A-Za-z0-9]")
		}
		c.TxID = r.TxID
		c.LocationID = s.randomHex(12)
		base := strings.TrimRight(s.cfg.BaseURL, "/")
		c.Location = base + LocationPath + c.LocationID
		// The BR Code stores the location without scheme and payers fetch
		// it over HTTPS; Location keeps the URL the simulator really serves.
		hostPath := strings.TrimPrefix(strings.TrimPrefix(c.Location, "https://"), "http://")
		c.BRCode, err = pix.NewDynamic(pix.DynamicOptions{URL: "https://" + hostPath, MerchantName: r.MerchantName, MerchantCity: r.MerchantCity})
	default:
		return Charge{}, invalid("invalid_charge_type", "type must be static or dynamic")
	}
	if err != nil {
		return Charge{}, &Error{Kind: KindInvalid, Code: "brcode_generation_failed", Msg: err.Error(), Err: err}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.charges[c.TxID]; dup {
		return Charge{}, conflict("duplicate_txid", "a charge with txid %s already exists", c.TxID)
	}
	s.charges[c.TxID] = c
	s.chargeOrder = append(s.chargeOrder, c.TxID)
	if c.LocationID != "" {
		s.locations[c.LocationID] = c.TxID
	}
	return c.snapshot(), nil
}

// Charge returns a charge by txid.
func (s *Sim) Charge(txid string) (Charge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.charges[txid]
	if !ok {
		return Charge{}, notFound("charge_not_found", "charge %s not found", txid)
	}
	c.refresh(s.Clock.Now())
	return c.snapshot(), nil
}

// Charges lists charges newest first.
func (s *Sim) Charges() []Charge {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Clock.Now()
	out := []Charge{}
	for i := len(s.chargeOrder) - 1; i >= 0; i-- {
		c := s.charges[s.chargeOrder[i]]
		c.refresh(now)
		out = append(out, c.snapshot())
	}
	return out
}

// RemoveCharge cancels an active charge (receiver action).
func (s *Sim) RemoveCharge(txid string) (Charge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.charges[txid]
	if !ok {
		return Charge{}, notFound("charge_not_found", "charge %s not found", txid)
	}
	c.refresh(s.Clock.Now())
	if c.Status != ChargeActive || c.inFlight != "" {
		return c.snapshot(), conflict("charge_not_active", "charge is %s", c.Status)
	}
	c.Status = ChargeRemoved
	return c.snapshot(), nil
}

// LocationPayload is the JSON served at a dynamic location. Field names
// follow the BCB Pix API; the real payload is a signed JWS.
type LocationPayload struct {
	Calendario struct {
		Criacao      time.Time `json:"criacao"`
		Apresentacao time.Time `json:"apresentacao"`
		Expiracao    int64     `json:"expiracao"`
	} `json:"calendario"`
	TxID    string `json:"txid"`
	Revisao int    `json:"revisao"`
	Status  string `json:"status"`
	Valor   struct {
		Original string `json:"original"`
	} `json:"valor"`
	Chave              string `json:"chave,omitempty"`
	SolicitacaoPagador string `json:"solicitacaoPagador,omitempty"`
}

// Location returns the payload of a dynamic charge by location ID.
func (s *Sim) Location(id string) (LocationPayload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	txid, ok := s.locations[id]
	if !ok {
		return LocationPayload{}, notFound("location_not_found", "location %s not found", id)
	}
	c := s.charges[txid]
	now := s.Clock.Now()
	c.refresh(now)
	var p LocationPayload
	p.Calendario.Criacao = c.CreatedAt
	p.Calendario.Apresentacao = now
	p.Calendario.Expiracao = int64(c.ExpiresAt.Sub(c.CreatedAt) / time.Second)
	p.TxID, p.Status, p.Chave, p.SolicitacaoPagador = c.TxID, c.Status, c.Key, c.Description
	p.Valor.Original = fmt.Sprintf("%d.%02d", c.AmountCents/100, c.AmountCents%100)
	return p, nil
}

// reserveChargeLocked marks a dynamic charge as being paid; caller holds s.mu.
func (s *Sim) reserveChargeLocked(txid, e2e string) error {
	c := s.charges[txid]
	c.refresh(s.Clock.Now())
	if c.Type == ChargeStatic {
		return nil
	}
	if c.Status != ChargeActive {
		return conflict("charge_not_active", "charge is %s", c.Status)
	}
	if c.inFlight != "" {
		return conflict("charge_payment_in_progress", "charge is already being paid by %s", c.inFlight)
	}
	c.inFlight = e2e
	return nil
}

// finishChargeLocked records the outcome of a charge payment; caller holds s.mu.
func (s *Sim) finishChargeLocked(txid, e2e string, ok bool) {
	c := s.charges[txid]
	if c.inFlight == e2e {
		c.inFlight = ""
	}
	if ok {
		c.Payments = append(c.Payments, e2e)
		if c.Type == ChargeDynamic {
			c.Status = ChargeCompleted
		}
	}
}

// QRPaymentRequest pays a BR Code ("copia e cola").
type QRPaymentRequest struct {
	FromAccountID string `json:"from_account_id"`
	BRCode        string `json:"brcode"`
	// AmountCents is required when a static code has no amount, and must
	// match the code otherwise.
	AmountCents int64  `json:"amount_cents,omitempty"`
	EndToEndID  string `json:"end_to_end_id,omitempty"`
	// Traceparent is set by the HTTP layer (see TransferRequest).
	Traceparent string `json:"-"`

	proximity *ProximityInfo
}

// PayQR decodes, lints and pays a BR Code.
func (s *Sim) PayQR(r QRPaymentRequest, ov *chaos.Overrides) (Transfer, error) {
	var errs []pix.Issue
	for _, is := range pix.Lint(r.BRCode) {
		if is.Severity == pix.SeverityError {
			errs = append(errs, is)
		}
	}
	if len(errs) > 0 {
		return Transfer{}, &Error{Kind: KindInvalid, Code: "brcode_lint_failed", Msg: "the BR Code has errors", Issues: errs}
	}
	b, err := pix.Decode(r.BRCode)
	if err != nil {
		return Transfer{}, &Error{Kind: KindInvalid, Code: "brcode_invalid", Msg: err.Error(), Err: err}
	}
	tr := TransferRequest{FromAccountID: r.FromAccountID, EndToEndID: r.EndToEndID, AmountCents: r.AmountCents, Traceparent: r.Traceparent, proximity: r.proximity}
	if b.Static {
		tr.Key, tr.txid, tr.initiation, tr.Description = b.Key, b.TxID, InitStaticQR, b.Description
		if b.Cents > 0 {
			if r.AmountCents != 0 && r.AmountCents != b.Cents {
				return Transfer{}, invalid("amount_mismatch", "amount_cents %d differs from the BR Code amount %d", r.AmountCents, b.Cents)
			}
			tr.AmountCents = b.Cents
		}
		s.mu.Lock()
		if c, ok := s.charges[b.TxID]; ok && c.Type == ChargeStatic && c.Key == normalizeOrEmpty(b.Key) {
			tr.chargeTx = b.TxID
		}
		s.mu.Unlock()
		return s.CreateTransfer(tr, ov)
	}
	u, err := url.Parse("https://" + b.URL)
	if err != nil || !strings.HasPrefix(u.Path, LocationPath) {
		return Transfer{}, invalid("location_not_served", "dynamic location %q is not served by this simulator", b.URL)
	}
	s.mu.Lock()
	txid, ok := s.locations[strings.TrimPrefix(u.Path, LocationPath)]
	var c Charge
	if ok {
		ch := s.charges[txid]
		ch.refresh(s.Clock.Now())
		c = ch.snapshot()
	}
	s.mu.Unlock()
	if !ok {
		return Transfer{}, notFound("location_not_found", "dynamic location %q not found", b.URL)
	}
	if c.Status != ChargeActive {
		return Transfer{}, conflict("charge_not_active", "charge is %s", c.Status)
	}
	if r.AmountCents != 0 && r.AmountCents != c.AmountCents {
		return Transfer{}, invalid("amount_mismatch", "amount_cents %d differs from the charge amount %d", r.AmountCents, c.AmountCents)
	}
	tr.ToAccountID, tr.AmountCents, tr.txid, tr.initiation, tr.chargeTx = c.ReceiverAccountID, c.AmountCents, c.TxID, InitDynamicQR, c.TxID
	tr.Description = c.Description
	return s.CreateTransfer(tr, ov)
}

func normalizeOrEmpty(k string) string {
	n, _, err := pix.NormalizeKey(k)
	if err != nil {
		return ""
	}
	return n
}
