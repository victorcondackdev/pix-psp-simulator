// Package notify delivers signed webhook events to the URLs registered per
// account, with retries, exponential backoff with jitter, deliberate
// duplicates and reordering (for chaos testing), and a delivery log.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

// Config controls delivery.
type Config struct {
	MaxAttempts int           // total attempts per delivery, default 6
	BaseBackoff time.Duration // delay before the 2nd attempt, default 1s
	MaxBackoff  time.Duration // cap, default 5m
	Timeout     time.Duration // per-attempt HTTP timeout, default 5s
	LogSize     int           // deliveries kept in the log, default 1000
	// SignTime is the time put in signatures. It defaults to the wall
	// clock, not the simulator clock, so consumers verifying with their own
	// clock keep working after the simulator time-travels.
	SignTime func() time.Time
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 6
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
	if c.LogSize <= 0 {
		c.LogSize = 1000
	}
	if c.SignTime == nil {
		c.SignTime = time.Now
	}
	return c
}

// Backoff returns the delay before attempt n+1 after n failed attempts
// (n >= 1), before jitter: base * 2^(n-1), capped at max.
func Backoff(n int, base, max time.Duration) time.Duration {
	if n < 1 {
		n = 1
	}
	d := base
	for i := 1; i < n; i++ {
		d *= 2
		if d >= max || d <= 0 {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

// Jitter applies "equal jitter": a value in [d/2, d], drawn with rnd, which
// returns a number in [0, n).
func Jitter(d time.Duration, rnd func(n int64) int64) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rnd(int64(half)+1))
}

// Endpoint is a webhook registration.
type Endpoint struct {
	AccountID string    `json:"account_id"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret"`
	CreatedAt time.Time `json:"created_at"`
}

// Attempt is one HTTP try.
type Attempt struct {
	N          int       `json:"n"`
	At         time.Time `json:"at"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
}

// Delivery statuses.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
)

// Delivery is one copy of an event sent to one endpoint.
type Delivery struct {
	ID            string     `json:"id"`
	EventID       string     `json:"event_id"`
	EventType     string     `json:"event_type"`
	AccountID     string     `json:"account_id"`
	URL           string     `json:"url"`
	Copy          int        `json:"copy"` // 0 for the original, 1.. for chaos duplicates
	Status        string     `json:"status"`
	Attempts      []Attempt  `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`

	body []byte
	ep   Endpoint
}

// PublishOptions carry chaos settings for one event.
type PublishOptions struct {
	Duplicates int           // extra copies with the same event ID
	Reorder    time.Duration // random extra initial delay in [0, Reorder]
}

// Dispatcher sends events. It is safe for concurrent use.
type Dispatcher struct {
	mu         sync.Mutex
	cfg        Config
	clock      clock.Clock
	client     *http.Client
	log        *slog.Logger
	rnd        func(n int64) int64
	endpoints  map[string]Endpoint
	deliveries []*Delivery
	byID       map[string]*Delivery
	timers     map[string]clock.Timer
	seq        int
	closed     bool
	wg         sync.WaitGroup
}

// New returns a dispatcher. rnd supplies jitter randomness ([0,n)).
func New(c clock.Clock, client *http.Client, cfg Config, rnd func(n int64) int64, log *slog.Logger) *Dispatcher {
	if client == nil {
		client = &http.Client{}
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Dispatcher{
		cfg: cfg.withDefaults(), clock: c, client: client, log: log, rnd: rnd,
		endpoints: map[string]Endpoint{}, byID: map[string]*Delivery{}, timers: map[string]clock.Timer{},
	}
}

// NewSecret returns a random webhook secret.
func NewSecret() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "whsec_" + hex.EncodeToString(b[:])
}

// ValidateURL accepts absolute http and https URLs.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("webhook url must be an absolute http(s) URL")
	}
	return nil
}

// SetEndpoint registers (or replaces) the webhook of an account. An empty
// secret generates one.
func (d *Dispatcher) SetEndpoint(accountID, rawURL, secret string) (Endpoint, error) {
	if err := ValidateURL(rawURL); err != nil {
		return Endpoint{}, err
	}
	if secret == "" {
		secret = NewSecret()
	}
	ep := Endpoint{AccountID: accountID, URL: rawURL, Secret: secret, CreatedAt: d.clock.Now()}
	d.mu.Lock()
	d.endpoints[accountID] = ep
	d.mu.Unlock()
	return ep, nil
}

// Endpoint returns the webhook of an account.
func (d *Dispatcher) Endpoint(accountID string) (Endpoint, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ep, ok := d.endpoints[accountID]
	return ep, ok
}

// RemoveEndpoint deletes the webhook of an account.
func (d *Dispatcher) RemoveEndpoint(accountID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.endpoints[accountID]
	delete(d.endpoints, accountID)
	return ok
}

// Publish queues ev for the account's endpoint, if any, and returns the IDs
// of the deliveries created (1 + opts.Duplicates, or none).
func (d *Dispatcher) Publish(ev webhook.Event, opts PublishOptions) []string {
	body, err := json.Marshal(ev)
	if err != nil {
		d.log.Error("webhook marshal", "err", err)
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ep, ok := d.endpoints[ev.AccountID]
	if !ok || d.closed {
		return nil
	}
	var ids []string
	for copyN := 0; copyN <= opts.Duplicates; copyN++ {
		d.seq++
		dl := &Delivery{
			ID: fmt.Sprintf("dlv_%06d", d.seq), EventID: ev.ID, EventType: ev.Type, AccountID: ev.AccountID,
			URL: ep.URL, Copy: copyN, Status: StatusPending, Attempts: []Attempt{}, CreatedAt: d.clock.Now(),
			body: body, ep: ep,
		}
		d.deliveries = append(d.deliveries, dl)
		d.byID[dl.ID] = dl
		if over := len(d.deliveries) - d.cfg.LogSize; over > 0 {
			for _, old := range d.deliveries[:over] {
				if old.Status != StatusPending {
					delete(d.byID, old.ID)
				}
			}
			d.deliveries = append([]*Delivery(nil), d.deliveries[over:]...)
		}
		var delay time.Duration
		if opts.Reorder > 0 {
			delay = time.Duration(d.rnd(int64(opts.Reorder) + 1))
		}
		d.scheduleLocked(dl, delay)
		ids = append(ids, dl.ID)
	}
	return ids
}

// scheduleLocked arms the next attempt; the caller holds d.mu.
func (d *Dispatcher) scheduleLocked(dl *Delivery, delay time.Duration) {
	at := d.clock.Now().Add(delay)
	dl.NextAttemptAt = &at
	d.wg.Add(1)
	d.timers[dl.ID] = d.clock.AfterFunc(delay, func() {
		defer d.wg.Done()
		d.attempt(dl)
	})
}

func (d *Dispatcher) attempt(dl *Delivery) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	delete(d.timers, dl.ID)
	n := len(dl.Attempts) + 1
	ep := dl.ep
	d.mu.Unlock()

	start := d.clock.Now()
	wallStart := time.Now()
	status, err := d.send(ep, dl, n)
	a := Attempt{N: n, At: start, StatusCode: status, DurationMS: time.Since(wallStart).Milliseconds()}
	if err != nil {
		a.Error = err.Error()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	dl.Attempts = append(dl.Attempts, a)
	dl.NextAttemptAt = nil
	switch {
	case err == nil && status >= 200 && status < 300:
		dl.Status = StatusDelivered
	case n >= d.cfg.MaxAttempts || d.closed:
		dl.Status = StatusFailed
		d.log.Warn("webhook delivery failed", "delivery", dl.ID, "event", dl.EventID, "attempts", n)
	default:
		d.scheduleLocked(dl, Jitter(Backoff(n, d.cfg.BaseBackoff, d.cfg.MaxBackoff), d.rnd))
	}
}

func (d *Dispatcher) send(ep Endpoint, dl *Delivery, n int) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(dl.body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pix-psp-simulator/webhook")
	req.Header.Set(webhook.SignatureHeader, webhook.Sign([]byte(ep.Secret), d.cfg.SignTime(), dl.body))
	req.Header.Set(webhook.EventIDHeader, dl.EventID)
	req.Header.Set(webhook.EventTypeHeader, dl.EventType)
	req.Header.Set("X-Delivery-Id", dl.ID)
	req.Header.Set("X-Delivery-Attempt", fmt.Sprint(n))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Deliveries returns the log, newest first, optionally filtered.
func (d *Dispatcher) Deliveries(accountID, eventID string) []Delivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Delivery{}
	for i := len(d.deliveries) - 1; i >= 0; i-- {
		dl := d.deliveries[i]
		if (accountID == "" || dl.AccountID == accountID) && (eventID == "" || dl.EventID == eventID) {
			c := *dl
			c.Attempts = append([]Attempt{}, dl.Attempts...)
			out = append(out, c)
		}
	}
	return out
}

// Delivery returns one delivery by ID.
func (d *Dispatcher) Delivery(id string) (Delivery, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dl, ok := d.byID[id]
	if !ok {
		return Delivery{}, false
	}
	c := *dl
	c.Attempts = append([]Attempt{}, dl.Attempts...)
	return c, true
}

// Redeliver schedules a new attempt of a failed or delivered delivery now.
func (d *Dispatcher) Redeliver(id string) (Delivery, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dl, ok := d.byID[id]
	if !ok {
		return Delivery{}, fmt.Errorf("delivery %s not found", id)
	}
	if dl.Status == StatusPending {
		return *dl, fmt.Errorf("delivery %s is still pending", id)
	}
	dl.Status = StatusPending
	dl.Attempts = dl.Attempts[:0:0]
	if ep, ok := d.endpoints[dl.AccountID]; ok {
		dl.ep, dl.URL = ep, ep.URL
	}
	d.scheduleLocked(dl, 0)
	return *dl, nil
}

// Pending returns the IDs of deliveries waiting for an attempt.
func (d *Dispatcher) Pending() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for id := range d.timers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Close stops scheduling and cancels pending attempts.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	d.closed = true
	for id, t := range d.timers {
		if t.Stop() {
			d.wg.Done()
		}
		delete(d.timers, id)
	}
	d.mu.Unlock()
	d.wg.Wait()
}
