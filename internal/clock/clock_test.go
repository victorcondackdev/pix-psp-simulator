package clock

import (
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)

func TestFakeRunsTimersInOrder(t *testing.T) {
	c := NewFake(epoch)
	var got []int
	c.AfterFunc(3*time.Second, func() { got = append(got, 3) })
	c.AfterFunc(1*time.Second, func() { got = append(got, 1) })
	c.AfterFunc(2*time.Second, func() {
		got = append(got, 2)
		// Scheduled from a callback, still inside the window.
		c.AfterFunc(500*time.Millisecond, func() { got = append(got, 25) })
	})
	stopped := c.AfterFunc(time.Second, func() { got = append(got, -1) })
	if !stopped.Stop() {
		t.Fatal("Stop on pending timer returned false")
	}
	if stopped.Stop() {
		t.Fatal("second Stop returned true")
	}
	c.Advance(10 * time.Second)
	want := []int{1, 2, 25, 3}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if !c.Now().Equal(epoch.Add(10 * time.Second)) {
		t.Fatalf("Now = %v", c.Now())
	}
	if c.Pending() != 0 {
		t.Fatalf("Pending = %d", c.Pending())
	}
}

func TestFakeTimeInsideCallback(t *testing.T) {
	c := NewFake(epoch)
	var at time.Time
	tm := c.AfterFunc(time.Minute, func() { at = c.Now() })
	c.Advance(time.Hour)
	if !at.Equal(epoch.Add(time.Minute)) {
		t.Fatalf("callback saw %v", at)
	}
	if tm.Stop() {
		t.Fatal("Stop after fire returned true")
	}
	c.Set(epoch) // never goes backwards
	if !c.Now().Equal(epoch.Add(time.Hour)) {
		t.Fatalf("clock went backwards: %v", c.Now())
	}
}

func TestReal(t *testing.T) {
	var r Real
	if time.Since(r.Now()) > time.Second {
		t.Fatal("Real.Now is off")
	}
	done := make(chan struct{})
	r.AfterFunc(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Real.AfterFunc did not fire")
	}
}

func TestVirtualAdvanceFiresDueTimers(t *testing.T) {
	c := NewVirtual()
	start := c.Now()
	var mu sync.Mutex
	var order []string
	done := make(chan struct{}, 3)
	rec := func(s string) func() {
		return func() {
			mu.Lock()
			order = append(order, s)
			mu.Unlock()
			done <- struct{}{}
		}
	}
	c.AfterFunc(2*time.Hour, rec("b"))
	c.AfterFunc(1*time.Hour, rec("a"))
	late := c.AfterFunc(48*time.Hour, rec("never"))
	stop := c.AfterFunc(time.Hour, rec("stopped"))
	stop.Stop()

	c.Advance(3 * time.Hour)
	c.Advance(-time.Hour) // ignored
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("virtual timers did not fire after Advance")
		}
	}
	mu.Lock()
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order = %v", order)
	}
	mu.Unlock()
	if c.Offset() != 3*time.Hour {
		t.Fatalf("Offset = %v", c.Offset())
	}
	if c.Now().Sub(start) < 3*time.Hour {
		t.Fatal("Now did not move")
	}
	if !late.Stop() {
		t.Fatal("rescheduled timer should still be pending")
	}
}

func TestVirtualRealTimeFire(t *testing.T) {
	c := NewVirtual()
	done := make(chan struct{})
	c.AfterFunc(5*time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("virtual timer did not fire in real time")
	}
}
