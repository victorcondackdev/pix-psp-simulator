// Package clock provides the time source used by the simulator.
//
// Every component that reads the time or schedules work does so through a
// Clock, so tests can drive time deterministically (Fake) and the running
// server can be moved forward on demand (Virtual) to exercise expirations,
// resolution periods and retry schedules without waiting.
package clock

import (
	"container/heap"
	"sort"
	"sync"
	"time"
)

// Clock is a source of time that can schedule callbacks.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f in its own goroutine (Real, Virtual) or synchronously
	// from Advance (Fake) once d has elapsed on this clock.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a scheduled callback.
type Timer interface {
	// Stop prevents the callback from running. It reports whether the call
	// stopped the timer (false if it already ran or was stopped).
	Stop() bool
}

// Real is the wall clock.
type Real struct{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// AfterFunc wraps time.AfterFunc.
func (Real) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// scheduler holds timers ordered by deadline; shared by Fake and Virtual.
type scheduler struct {
	timers timerHeap
	seq    uint64
}

type entry struct {
	at      time.Time
	seq     uint64
	f       func()
	index   int
	stopped bool
	fired   bool
	real    *time.Timer // Virtual only
}

type timerHeap []*entry

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h timerHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *timerHeap) Push(x any)   { e := x.(*entry); e.index = len(*h); *h = append(*h, e) }
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

func (s *scheduler) add(at time.Time, f func()) *entry {
	s.seq++
	e := &entry{at: at, seq: s.seq, f: f}
	heap.Push(&s.timers, e)
	return e
}

func (s *scheduler) remove(e *entry) {
	if e.index >= 0 && e.index < len(s.timers) && s.timers[e.index] == e {
		heap.Remove(&s.timers, e.index)
	}
}

// popDue removes and returns the earliest timer due at or before now.
func (s *scheduler) popDue(now time.Time) *entry {
	if len(s.timers) == 0 || s.timers[0].at.After(now) {
		return nil
	}
	e := heap.Pop(&s.timers).(*entry)
	e.fired = true
	return e
}

// Fake is a manually driven clock for tests. Callbacks run synchronously,
// in deadline order, from Advance and Set.
type Fake struct {
	mu  sync.Mutex
	now time.Time
	s   scheduler
}

// NewFake returns a Fake clock set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now returns the fake time.
func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f at Now()+d. A non-positive d still waits for the
// next Advance (or Set) call, keeping tests deterministic.
func (c *Fake) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &fakeTimer{c: c, e: c.s.add(c.now.Add(d), f)}
}

// Advance moves the clock forward by d, running every callback that becomes
// due, including callbacks scheduled by callbacks within the window.
func (c *Fake) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	c.Set(target)
}

// Set moves the clock to t (never backwards), running due callbacks.
func (c *Fake) Set(t time.Time) {
	for {
		c.mu.Lock()
		e := c.s.popDue(t)
		if e == nil {
			if t.After(c.now) {
				c.now = t
			}
			c.mu.Unlock()
			return
		}
		if e.at.After(c.now) {
			c.now = e.at
		}
		c.mu.Unlock()
		e.f()
	}
}

// Pending returns the number of scheduled callbacks.
func (c *Fake) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.s.timers)
}

type fakeTimer struct {
	c *Fake
	e *entry
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.e.fired || t.e.stopped {
		return false
	}
	t.e.stopped = true
	t.c.s.remove(t.e)
	return true
}

// Virtual is the wall clock plus an offset that only grows. Advance moves it
// forward ("time travel"): timers whose virtual deadline has passed fire
// immediately, and the rest are rescheduled for their remaining time.
type Virtual struct {
	mu     sync.Mutex
	offset time.Duration
	s      scheduler
	wall   func() time.Time
}

// NewVirtual returns a Virtual clock (UTC) with no offset.
func NewVirtual() *Virtual { return &Virtual{wall: func() time.Time { return time.Now().UTC() }} }

// Now returns the wall time plus the offset.
func (c *Virtual) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall().Add(c.offset)
}

// Offset returns how far the clock has been advanced.
func (c *Virtual) Offset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

// AfterFunc schedules f to run d from now on this clock.
func (c *Virtual) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.s.add(c.wall().Add(c.offset).Add(d), f)
	c.arm(e, d)
	return &virtualTimer{c: c, e: e}
}

// arm starts the real timer for e; the caller holds c.mu.
func (c *Virtual) arm(e *entry, d time.Duration) {
	e.real = time.AfterFunc(d, func() { c.fire(e) })
}

func (c *Virtual) fire(e *entry) {
	c.mu.Lock()
	if e.fired || e.stopped {
		c.mu.Unlock()
		return
	}
	e.fired = true
	c.s.remove(e)
	c.mu.Unlock()
	e.f()
}

// Advance moves the clock forward by d (negative values are ignored).
func (c *Virtual) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	c.offset += d
	now := c.wall().Add(c.offset)
	var due []*entry
	for _, e := range c.s.timers {
		if e.real != nil {
			e.real.Stop()
		}
		if !e.at.After(now) {
			due = append(due, e)
		} else {
			c.arm(e, e.at.Sub(now))
		}
	}
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return timerHeap(due).Less(i, j) })
	go func() {
		for _, e := range due {
			c.fire(e)
		}
	}()
}

type virtualTimer struct {
	c *Virtual
	e *entry
}

func (t *virtualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.e.fired || t.e.stopped {
		return false
	}
	t.e.stopped = true
	if t.e.real != nil {
		t.e.real.Stop()
	}
	t.c.s.remove(t.e)
	return true
}
