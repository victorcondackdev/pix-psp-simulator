// Package sim is the simulator core: it ties the ledger, the DICT mock, the
// chaos engine and the webhook dispatcher together and implements accounts,
// transfers, charges (cob), refunds (devoluções) and MED infractions.
package sim

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/victorcondackdev/brpay/document"
	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/bizday"
	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/dict"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/internal/notify"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sta"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

// Config configures a simulator.
type Config struct {
	// ISPB is the simulator's own 8-digit ISPB, used in EndToEndIds and as
	// the default ISPB of new accounts.
	ISPB string
	// BaseURL is the public URL of the simulator, used for dynamic QR code
	// locations (e.g. http://localhost:8080).
	BaseURL string
	// SettlementLatency is the delay between accepting a transfer and its
	// final status.
	SettlementLatency time.Duration
	// MaxTransferCents rejects larger transfers with AM02; 0 means no limit
	// beyond the BR Code maximum.
	MaxTransferCents int64
	// ChargeExpiry is the default expiration of dynamic charges.
	ChargeExpiry time.Duration
	// MerchantCity is used in BR Codes when a charge does not set one.
	MerchantCity string
	// MEDBlockWindow is how long a cautionary block lasts without analysis
	// before it lapses and the funds are released (default 72h).
	MEDBlockWindow time.Duration
	// MEDMaxAccounts caps how many accounts MED 2.0 tracing visits,
	// including the original payee (default 5).
	MEDMaxAccounts int
}

func (c Config) withDefaults() Config {
	if c.ISPB == "" {
		c.ISPB = "99999999"
	}
	if c.BaseURL == "" {
		c.BaseURL = "http://localhost:8080"
	}
	if c.SettlementLatency < 0 {
		c.SettlementLatency = 0
	}
	if c.ChargeExpiry <= 0 {
		c.ChargeExpiry = time.Hour
	}
	if c.MerchantCity == "" {
		c.MerchantCity = "RIO DE JANEIRO"
	}
	if c.MEDBlockWindow <= 0 {
		c.MEDBlockWindow = 72 * time.Hour
	}
	if c.MEDMaxAccounts <= 0 {
		c.MEDMaxAccounts = 5
	}
	return c
}

// Kind classifies errors for the HTTP layer.
type Kind int

// Error kinds.
const (
	KindInvalid  Kind = iota + 1 // 422: well-formed but semantically invalid
	KindNotFound                 // 404
	KindConflict                 // 409
)

// Error is a simulator error with a kind and optional lint issues.
type Error struct {
	Kind   Kind
	Code   string
	Msg    string
	Issues []pix.Issue
	Err    error
}

func (e *Error) Error() string { return e.Msg }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

func invalid(code, format string, a ...any) *Error {
	return &Error{Kind: KindInvalid, Code: code, Msg: fmt.Sprintf(format, a...)}
}

func notFound(code, format string, a ...any) *Error {
	return &Error{Kind: KindNotFound, Code: code, Msg: fmt.Sprintf(format, a...)}
}

func conflict(code, format string, a ...any) *Error {
	return &Error{Kind: KindConflict, Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Sim is the simulator. It is safe for concurrent use.
type Sim struct {
	cfg    Config
	Clock  clock.Clock
	Ledger *ledger.Ledger
	DICT   *dict.Directory
	Chaos  *chaos.Engine
	Notify *notify.Dispatcher
	// Telemetry holds the Prometheus metrics served at /metrics.
	Telemetry *Telemetry
	// STA receives regulatory documents (CADOC 4111 and 4010).
	STA  *sta.Service
	log  *slog.Logger
	rand io.Reader

	mu          sync.Mutex
	transfers   map[string]*Transfer
	order       []string
	charges     map[string]*Charge
	locations   map[string]string // location id -> txid
	chargeOrder []string
	infractions map[string]*Infraction
	infOrder    []string
	seq         int
	timers      sync.WaitGroup
}

// Deps are the collaborators of a Sim; nil fields get defaults.
type Deps struct {
	Clock  clock.Clock
	Chaos  *chaos.Engine
	Notify *notify.Dispatcher
	Log    *slog.Logger
	Rand   io.Reader
	// Telemetry receives the simulator's metrics; pass the same value to
	// notify.Config.OnAttempt (Telemetry.WebhookAttempt) to count webhooks.
	Telemetry *Telemetry
}

// New returns a simulator.
func New(cfg Config, deps Deps) *Sim {
	cfg = cfg.withDefaults()
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.Chaos == nil {
		deps.Chaos = chaos.New(time.Now().UnixNano())
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if deps.Telemetry == nil {
		deps.Telemetry = NewTelemetry()
	}
	if deps.Notify == nil {
		deps.Notify = notify.New(deps.Clock, nil, notify.Config{OnAttempt: deps.Telemetry.WebhookAttempt}, deps.Chaos.Int63n, deps.Log)
	}
	if deps.Rand == nil {
		deps.Rand = rand.Reader
	}
	s := &Sim{
		cfg: cfg, Clock: deps.Clock, Ledger: ledger.New(deps.Clock), DICT: dict.New(deps.Clock),
		Chaos: deps.Chaos, Notify: deps.Notify, Telemetry: deps.Telemetry, log: deps.Log, rand: deps.Rand,
		transfers: map[string]*Transfer{}, charges: map[string]*Charge{}, locations: map[string]string{},
		infractions: map[string]*Infraction{},
	}
	s.STA = sta.New(s, bizday.Calendar{}, sta.Config{})
	s.STA.OnSubmit(func(doc, status string) { s.Telemetry.STASubmissions.Inc(doc, status) })
	s.registerGauges()
	return s
}

// Config returns the effective configuration.
func (s *Sim) Config() Config { return s.cfg }

// Wait blocks until every scheduled settlement has run (tests, shutdown).
func (s *Sim) Wait() { s.timers.Wait() }

func (s *Sim) randomHex(n int) string {
	b := make([]byte, n)
	_, _ = io.ReadFull(s.rand, b)
	return hex.EncodeToString(b)
}

const alnum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func (s *Sim) randomAlnum(n int) string {
	b := make([]byte, n)
	_, _ = io.ReadFull(s.rand, b)
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

// nextEventID returns a new event ID; the caller holds s.mu.
func (s *Sim) nextEventID() string {
	s.seq++
	return fmt.Sprintf("evt_%08d", s.seq)
}

// publish emits an event to an account's webhook.
func (s *Sim) publish(id, typ, accountID string, data any, opts notify.PublishOptions) {
	raw, err := json.Marshal(data)
	if err != nil {
		s.log.Error("event marshal", "err", err)
		return
	}
	s.Notify.Publish(webhook.Event{ID: id, Type: typ, CreatedAt: s.Clock.Now(), AccountID: accountID, Data: raw}, opts)
}

// AccountRequest opens an account.
type AccountRequest struct {
	ID           string      `json:"id,omitempty"`
	OwnerName    string      `json:"owner_name"`
	Document     string      `json:"document"`
	ISPB         string      `json:"ispb,omitempty"`
	Branch       string      `json:"branch,omitempty"`
	Number       string      `json:"number,omitempty"`
	Type         ledger.Type `json:"type,omitempty"`
	BalanceCents int64       `json:"initial_balance_cents,omitempty"`
}

func digits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// CreateAccount opens and optionally funds an account.
func (s *Sim) CreateAccount(r AccountRequest) (ledger.Account, error) {
	r.OwnerName = strings.TrimSpace(r.OwnerName)
	if r.OwnerName == "" || len(r.OwnerName) > 140 {
		return ledger.Account{}, invalid("invalid_owner_name", "owner_name is required (at most 140 characters)")
	}
	if document.Detect(r.Document) == document.Unknown {
		return ledger.Account{}, invalid("invalid_document", "document must be a valid CPF or CNPJ (alphanumeric CNPJ accepted)")
	}
	if r.ID == "" {
		r.ID = "acc_" + s.randomHex(6)
	}
	if strings.HasPrefix(r.ID, "sys.") || strings.ContainsAny(r.ID, "/ ?#") {
		return ledger.Account{}, invalid("invalid_id", "id must not start with sys. or contain / ? # or spaces")
	}
	if r.ISPB == "" {
		r.ISPB = s.cfg.ISPB
	}
	if !digits(r.ISPB, 8) {
		return ledger.Account{}, invalid("invalid_ispb", "ispb must have 8 digits")
	}
	if r.Branch != "" && !digits(r.Branch, 4) {
		return ledger.Account{}, invalid("invalid_branch", "branch must have 4 digits")
	}
	if r.Type == "" {
		r.Type = ledger.Checking
	}
	if !r.Type.Valid() {
		return ledger.Account{}, invalid("invalid_account_type", "type must be CACC, SVGS, SLRY or TRAN")
	}
	if r.BalanceCents < 0 {
		return ledger.Account{}, invalid("invalid_amount", "initial_balance_cents must not be negative")
	}
	a, err := s.Ledger.Open(ledger.Account{
		ID: r.ID, ISPB: r.ISPB, Branch: r.Branch, Number: r.Number, Type: r.Type,
		OwnerName: r.OwnerName, OwnerDocument: document.Clean(r.Document),
	})
	if err != nil {
		return ledger.Account{}, &Error{Kind: KindConflict, Code: "account_exists", Msg: err.Error(), Err: err}
	}
	if r.BalanceCents > 0 {
		if _, err := s.Ledger.Move(ledger.Funding, a.ID, r.BalanceCents, "seed:"+a.ID, "initial balance"); err != nil {
			return ledger.Account{}, err
		}
		a.Balance = r.BalanceCents
	}
	return a, nil
}

// Account returns a customer account.
func (s *Sim) Account(id string) (ledger.Account, error) {
	a, err := s.Ledger.Get(id)
	if err != nil || a.System {
		return ledger.Account{}, notFound("account_not_found", "account %s not found", id)
	}
	return a, nil
}

// Deposit adds funds to an account from the funding account.
func (s *Sim) Deposit(id string, cents int64) (ledger.Account, error) {
	if _, err := s.Account(id); err != nil {
		return ledger.Account{}, err
	}
	if cents <= 0 || cents > pix.MaxCents {
		return ledger.Account{}, invalid("invalid_amount", "amount_cents must be between 1 and %d", int64(pix.MaxCents))
	}
	if _, err := s.Ledger.Move(ledger.Funding, id, cents, "deposit:"+id, "deposit"); err != nil {
		return ledger.Account{}, err
	}
	return s.Ledger.Get(id)
}

// SetAccountStatus blocks, closes or reactivates an account (to exercise
// AC04/AC06 rejections).
func (s *Sim) SetAccountStatus(id string, st ledger.Status) (ledger.Account, error) {
	switch st {
	case ledger.Active, ledger.Blocked, ledger.Closed:
	default:
		return ledger.Account{}, invalid("invalid_status", "status must be ACTIVE, BLOCKED or CLOSED")
	}
	if _, err := s.Account(id); err != nil {
		return ledger.Account{}, err
	}
	return s.Ledger.SetStatus(id, st)
}

// Owner converts an account to a DICT owner.
func Owner(a ledger.Account) dict.Owner {
	return dict.Owner{
		AccountID: a.ID, ISPB: a.ISPB, Branch: a.Branch, Number: a.Number,
		AccountType: string(a.Type), Name: a.OwnerName, Document: a.OwnerDocument,
	}
}

// RegisterKey registers a DICT key for an account.
func (s *Sim) RegisterKey(accountID, key string, evp bool) (dict.Entry, error) {
	a, err := s.Account(accountID)
	if err != nil {
		return dict.Entry{}, err
	}
	return s.DICT.Register(key, evp, Owner(a))
}

// OpenClaim opens a DICT claim on behalf of an account.
func (s *Sim) OpenClaim(accountID string, typ dict.ClaimType, key string) (dict.Claim, error) {
	a, err := s.Account(accountID)
	if err != nil {
		return dict.Claim{}, err
	}
	return s.DICT.OpenClaim(typ, key, Owner(a))
}

// Stats summarises the simulator state for the dashboard.
type Stats struct {
	Now          time.Time `json:"now"`
	Accounts     int       `json:"accounts"`
	Keys         int       `json:"keys"`
	Transfers    int       `json:"transfers"`
	Charges      int       `json:"charges"`
	Infractions  int       `json:"infractions"`
	LedgerTotal  int64     `json:"ledger_total_cents"`
	InFlight     int64     `json:"in_flight_cents"`
	HeldByMED    int64     `json:"held_by_med_cents"`
	ChaosRules   int       `json:"chaos_rules"`
	ISPB         string    `json:"ispb"`
	SettlementMS int64     `json:"settlement_latency_ms"`
}

// Stats returns a snapshot of counters.
func (s *Sim) Stats() Stats {
	st, _ := s.Ledger.Get(ledger.Settlement)
	hold, _ := s.Ledger.Get(ledger.MEDHold)
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Now: s.Clock.Now(), Accounts: len(s.Ledger.List(false)), Keys: len(s.DICT.Keys("")),
		Transfers: len(s.transfers), Charges: len(s.charges), Infractions: len(s.infractions),
		LedgerTotal: s.Ledger.Total(), InFlight: st.Balance, HeldByMED: hold.Balance,
		ChaosRules: len(s.Chaos.Rules()), ISPB: s.cfg.ISPB, SettlementMS: s.cfg.SettlementLatency.Milliseconds(),
	}
}
