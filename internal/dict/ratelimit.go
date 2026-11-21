package dict

import (
	"math"
	"sync"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
)

// RateLimit configures the per-payer token bucket that mimics the DICT
// anti-scan protection. The real DICT applies policy-specific buckets to PSPs
// and end users, with heavier penalties for lookups of keys that do not
// exist; the defaults here are illustrative only, not the BCB's values.
type RateLimit struct {
	// Capacity is the bucket size. Zero or less disables rate limiting.
	Capacity float64 `json:"capacity"`
	// RefillPerMinute is how many tokens are added back per minute.
	RefillPerMinute float64 `json:"refill_per_minute"`
	// NotFoundCost is how many extra tokens a lookup of a missing key
	// consumes, on top of the one every lookup costs.
	NotFoundCost float64 `json:"not_found_cost"`
}

// DefaultRateLimit is an illustrative policy: bursts of 100 lookups,
// 20 per minute sustained, missing keys cost 3 extra tokens.
var DefaultRateLimit = RateLimit{Capacity: 100, RefillPerMinute: 20, NotFoundCost: 3}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a set of token buckets keyed by payer.
type Limiter struct {
	mu      sync.Mutex
	cfg     RateLimit
	clock   clock.Clock
	buckets map[string]*bucket
}

// NewLimiter returns a limiter using cfg.
func NewLimiter(c clock.Clock, cfg RateLimit) *Limiter {
	return &Limiter{cfg: cfg, clock: c, buckets: map[string]*bucket{}}
}

// Config returns the current policy.
func (l *Limiter) Config() RateLimit {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

// SetConfig replaces the policy and resets every bucket.
func (l *Limiter) SetConfig(cfg RateLimit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = cfg
	l.buckets = map[string]*bucket{}
}

func (l *Limiter) refill(key string) *bucket {
	now := l.clock.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.cfg.Capacity, last: now}
		l.buckets[key] = b
		return b
	}
	if el := now.Sub(b.last); el > 0 {
		b.tokens = math.Min(l.cfg.Capacity, b.tokens+el.Minutes()*l.cfg.RefillPerMinute)
		b.last = now
	}
	return b
}

// Take removes cost tokens from key's bucket. When there are not enough
// tokens it takes nothing and returns how long until there will be.
func (l *Limiter) Take(key string, cost float64) (ok bool, retryAfter time.Duration, remaining float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cfg.Capacity <= 0 {
		return true, 0, math.Inf(1)
	}
	b := l.refill(key)
	if b.tokens >= cost {
		b.tokens -= cost
		return true, 0, b.tokens
	}
	if l.cfg.RefillPerMinute <= 0 {
		return false, time.Duration(math.MaxInt64), b.tokens
	}
	need := cost - b.tokens
	wait := time.Duration(need / l.cfg.RefillPerMinute * float64(time.Minute))
	return false, wait.Round(time.Second) + time.Second, b.tokens
}

// Charge removes cost tokens even if that leaves the bucket negative; used
// for the not-found penalty, which is applied after the lookup.
func (l *Limiter) Charge(key string, cost float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cfg.Capacity <= 0 || cost <= 0 {
		return
	}
	l.refill(key).tokens -= cost
}
