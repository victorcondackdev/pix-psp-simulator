// Package chaos decides which failures to inject into an operation, from
// global rules (loaded from scenario files or the admin API) and per-request
// headers. Headers win over rules.
package chaos

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
)

// Operations rules can match.
const (
	OpTransfer  = "transfer"
	OpQRPayment = "qr_payment"
	OpRefund    = "refund"
)

// Request headers understood by FromHeaders.
const (
	HeaderFail             = "X-Sim-Fail"
	HeaderFailProbability  = "X-Sim-Fail-Probability"
	HeaderLatency          = "X-Sim-Latency"
	HeaderTimeout          = "X-Sim-Timeout"
	HeaderWebhookDuplicate = "X-Sim-Webhook-Duplicates"
	HeaderWebhookReorder   = "X-Sim-Webhook-Reorder"
)

// Duration is a time.Duration that marshals as a Go duration string
// ("1.5s") and also accepts a number of milliseconds.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var ms float64
	if err := json.Unmarshal(b, &ms); err != nil {
		return errors.New("duration must be a string like \"2s\" or a number of milliseconds")
	}
	*d = Duration(time.Duration(ms * float64(time.Millisecond)))
	return nil
}

// Match selects the operations a rule applies to. Empty fields match all.
type Match struct {
	Operation   string `json:"operation,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	MinAmount   int64  `json:"min_amount_cents,omitempty"`
	MaxAmount   int64  `json:"max_amount_cents,omitempty"`
}

// Rule is a global failure-injection rule.
type Rule struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// Probability in [0,1] that the rule fires when it matches; 0 means 1.
	Probability float64 `json:"probability,omitempty"`
	Match       Match   `json:"match"`
	// Fail rejects the operation with an ISO reason code.
	Fail string `json:"fail,omitempty"`
	// Timeout rejects with AB03 after the settlement latency.
	Timeout bool `json:"timeout,omitempty"`
	// Latency overrides the settlement latency.
	Latency *Duration `json:"latency,omitempty"`
	// DuplicateWebhooks sends each webhook this many extra times, with the
	// same event ID.
	DuplicateWebhooks int `json:"duplicate_webhooks,omitempty"`
	// ReorderWebhooks delays each delivery by a random amount up to this
	// value, so events can arrive out of order.
	ReorderWebhooks Duration `json:"reorder_webhooks,omitempty"`
}

// Validate checks a rule.
func (r Rule) Validate() error {
	if r.Probability < 0 || r.Probability > 1 {
		return errors.New("probability must be between 0 and 1")
	}
	if r.Fail != "" && !iso.ValidReason(r.Fail) {
		return fmt.Errorf("unknown reason code %q (supported: %s)", r.Fail, strings.Join(iso.Codes(iso.Reasons), ", "))
	}
	switch r.Match.Operation {
	case "", OpTransfer, OpQRPayment, OpRefund:
	default:
		return fmt.Errorf("unknown operation %q", r.Match.Operation)
	}
	if r.Latency != nil && *r.Latency < 0 || r.ReorderWebhooks < 0 || r.DuplicateWebhooks < 0 {
		return errors.New("latency, reorder_webhooks and duplicate_webhooks must not be negative")
	}
	if r.DuplicateWebhooks > 10 {
		return errors.New("duplicate_webhooks must be at most 10")
	}
	return nil
}

func (m Match) matches(op Op) bool {
	switch {
	case m.Operation != "" && m.Operation != op.Operation:
	case m.FromAccount != "" && m.FromAccount != op.FromAccount:
	case m.ToAccount != "" && m.ToAccount != op.ToAccount:
	case m.MinAmount > 0 && op.Amount < m.MinAmount:
	case m.MaxAmount > 0 && op.Amount > m.MaxAmount:
	default:
		return true
	}
	return false
}

// Op describes the operation being evaluated.
type Op struct {
	Operation   string
	FromAccount string
	ToAccount   string
	Amount      int64
}

// Decision is the combined effect of matching rules and headers.
type Decision struct {
	Fail       string         `json:"fail,omitempty"`
	Latency    *time.Duration `json:"latency,omitempty"`
	Duplicates int            `json:"duplicate_webhooks,omitempty"`
	Reorder    time.Duration  `json:"reorder_webhooks,omitempty"`
	Rules      []string       `json:"rules,omitempty"`
}

// Engine holds rules and a random source. It is safe for concurrent use.
type Engine struct {
	mu    sync.Mutex
	rules []Rule
	rnd   *rand.Rand
	seq   int
}

// New returns an engine whose probabilities are drawn from seed.
func New(seed int64) *Engine { return &Engine{rnd: rand.New(rand.NewSource(seed))} }

// Add validates and appends a rule, assigning an ID when empty.
func (e *Engine) Add(r Rule) (Rule, error) {
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	if r.ID == "" {
		r.ID = fmt.Sprintf("rule_%d", e.seq)
	}
	for _, x := range e.rules {
		if x.ID == r.ID {
			return Rule{}, fmt.Errorf("rule %q already exists", r.ID)
		}
	}
	e.rules = append(e.rules, r)
	return r, nil
}

// Remove deletes a rule by ID and reports whether it existed.
func (e *Engine) Remove(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.rules {
		if r.ID == id {
			e.rules = append(e.rules[:i], e.rules[i+1:]...)
			return true
		}
	}
	return false
}

// Clear removes every rule.
func (e *Engine) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = nil
}

// Rules returns a copy of the rules.
func (e *Engine) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Rule(nil), e.rules...)
}

// Float returns a random number in [0,1) from the engine's source.
func (e *Engine) Float() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rnd.Float64()
}

// Int63n returns a random number in [0,n) from the engine's source.
func (e *Engine) Int63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rnd.Int63n(n)
}

// Evaluate applies matching rules in order; later rules override earlier
// ones field by field. Then overrides (from headers) are applied on top.
func (e *Engine) Evaluate(op Op, overrides *Overrides) Decision {
	e.mu.Lock()
	var d Decision
	for _, r := range e.rules {
		if !r.Match.matches(op) {
			continue
		}
		if r.Probability > 0 && r.Probability < 1 && e.rnd.Float64() >= r.Probability {
			continue
		}
		d.Rules = append(d.Rules, r.ID)
		if r.Fail != "" {
			d.Fail = r.Fail
		}
		if r.Timeout {
			d.Fail = iso.AB03
		}
		if r.Latency != nil {
			l := time.Duration(*r.Latency)
			d.Latency = &l
		}
		if r.DuplicateWebhooks > 0 {
			d.Duplicates = r.DuplicateWebhooks
		}
		if r.ReorderWebhooks > 0 {
			d.Reorder = time.Duration(r.ReorderWebhooks)
		}
	}
	if overrides != nil {
		if overrides.Fail != "" && (overrides.FailProbability <= 0 || overrides.FailProbability >= 1 || e.rnd.Float64() < overrides.FailProbability) {
			d.Fail = overrides.Fail
			d.Rules = append(d.Rules, "header")
		}
		if overrides.Latency != nil {
			d.Latency = overrides.Latency
		}
		if overrides.Duplicates > 0 {
			d.Duplicates = overrides.Duplicates
		}
		if overrides.Reorder > 0 {
			d.Reorder = overrides.Reorder
		}
	}
	e.mu.Unlock()
	return d
}

// Overrides are per-request chaos settings taken from headers.
type Overrides struct {
	Fail            string
	FailProbability float64
	Latency         *time.Duration
	Duplicates      int
	Reorder         time.Duration
}

// FromHeaders parses the X-Sim-* headers. It returns nil when none is set.
func FromHeaders(h http.Header) (*Overrides, error) {
	var o Overrides
	set := false
	if v := strings.TrimSpace(h.Get(HeaderFail)); v != "" {
		v = strings.ToUpper(v)
		if !iso.ValidReason(v) {
			return nil, fmt.Errorf("%s: unknown reason code %q (supported: %s)", HeaderFail, v, strings.Join(iso.Codes(iso.Reasons), ", "))
		}
		o.Fail, set = v, true
	}
	if v := strings.TrimSpace(h.Get(HeaderTimeout)); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", HeaderTimeout, err)
		}
		if b {
			o.Fail, set = iso.AB03, true
		}
	}
	if v := strings.TrimSpace(h.Get(HeaderFailProbability)); v != "" {
		p, err := strconv.ParseFloat(v, 64)
		if err != nil || p < 0 || p > 1 {
			return nil, fmt.Errorf("%s: must be a number between 0 and 1", HeaderFailProbability)
		}
		o.FailProbability = p
	}
	if v := strings.TrimSpace(h.Get(HeaderLatency)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("%s: must be a non-negative duration like 2s", HeaderLatency)
		}
		o.Latency, set = &d, true
	}
	if v := strings.TrimSpace(h.Get(HeaderWebhookDuplicate)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 10 {
			return nil, fmt.Errorf("%s: must be an integer between 0 and 10", HeaderWebhookDuplicate)
		}
		o.Duplicates, set = n, true
	}
	if v := strings.TrimSpace(h.Get(HeaderWebhookReorder)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("%s: must be a non-negative duration like 3s", HeaderWebhookReorder)
		}
		o.Reorder, set = d, true
	}
	if !set {
		return nil, nil
	}
	return &o, nil
}
