package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

func TestBackoff(t *testing.T) {
	base, max := time.Second, 30*time.Second
	tests := []struct {
		n    int
		want time.Duration
	}{
		{0, time.Second}, {1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second},
		{5, 16 * time.Second}, {6, 30 * time.Second}, {60, 30 * time.Second},
	}
	for _, tt := range tests {
		if got := Backoff(tt.n, base, max); got != tt.want {
			t.Errorf("Backoff(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
	if got := Backoff(3, 10*time.Second, 15*time.Second); got != 15*time.Second {
		t.Errorf("cap: %v", got)
	}
}

func TestJitter(t *testing.T) {
	lo := func(int64) int64 { return 0 }
	hi := func(n int64) int64 { return n - 1 }
	if got := Jitter(10*time.Second, lo); got != 5*time.Second {
		t.Errorf("lo = %v", got)
	}
	if got := Jitter(10*time.Second, hi); got != 10*time.Second {
		t.Errorf("hi = %v", got)
	}
	if got := Jitter(1, lo); got != 1 {
		t.Errorf("tiny = %v", got)
	}
}

type recorder struct {
	mu       sync.Mutex
	failures int // respond 500 to this many requests first
	got      []*http.Request
	bodies   [][]byte
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
	r.bodies = append(r.bodies, b)
	if r.failures > 0 {
		r.failures--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

var t0 = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

func setup(t *testing.T, failures int, cfg Config) (*Dispatcher, *clock.Fake, *recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{failures: failures}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := clock.NewFake(t0)
	cfg.SignTime = c.Now
	d := New(c, srv.Client(), cfg, func(n int64) int64 { return n - 1 }, nil)
	t.Cleanup(d.Close)
	return d, c, rec, srv
}

func event(id string) webhook.Event {
	return webhook.Event{ID: id, Type: webhook.EventPixReceived, CreatedAt: t0, AccountID: "acc", Data: json.RawMessage(`{"x":1}`)}
}

func TestDeliverSigned(t *testing.T) {
	d, c, rec, srv := setup(t, 0, Config{})
	ep, err := d.SetEndpoint("acc", srv.URL+"/hook", "")
	if err != nil || ep.Secret == "" {
		t.Fatalf("%+v %v", ep, err)
	}
	if got, ok := d.Endpoint("acc"); !ok || got.URL != srv.URL+"/hook" {
		t.Fatal("Endpoint")
	}
	ids := d.Publish(event("evt_1"), PublishOptions{})
	if len(ids) != 1 {
		t.Fatalf("ids = %v", ids)
	}
	if len(d.Pending()) != 1 {
		t.Fatal("expected pending")
	}
	c.Advance(0)
	if rec.count() != 1 {
		t.Fatalf("requests = %d", rec.count())
	}
	req, body := rec.got[0], rec.bodies[0]
	if err := webhook.VerifyAt([]byte(ep.Secret), req.Header.Get(webhook.SignatureHeader), body, time.Minute, t0); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if req.Header.Get(webhook.EventIDHeader) != "evt_1" || req.Header.Get(webhook.EventTypeHeader) != webhook.EventPixReceived {
		t.Fatalf("headers: %v", req.Header)
	}
	dl, ok := d.Delivery(ids[0])
	if !ok || dl.Status != StatusDelivered || len(dl.Attempts) != 1 || dl.Attempts[0].StatusCode != 200 {
		t.Fatalf("delivery = %+v", dl)
	}
	// No endpoint: nothing is created.
	if ids := d.Publish(webhook.Event{ID: "x", AccountID: "other"}, PublishOptions{}); ids != nil {
		t.Fatal("published without endpoint")
	}
}

func TestRetryWithBackoff(t *testing.T) {
	d, c, rec, srv := setup(t, 3, Config{MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute})
	if _, err := d.SetEndpoint("acc", srv.URL, "s"); err != nil {
		t.Fatal(err)
	}
	id := d.Publish(event("evt_r"), PublishOptions{})[0]
	c.Advance(0)
	wantGaps := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} // jitter rnd returns max
	for i, gap := range wantGaps {
		dl, _ := d.Delivery(id)
		if dl.Status != StatusPending || dl.NextAttemptAt == nil || !dl.NextAttemptAt.Equal(c.Now().Add(gap)) {
			t.Fatalf("after attempt %d: %+v (now %v)", i+1, dl, c.Now())
		}
		c.Advance(gap - time.Millisecond)
		if rec.count() != i+1 {
			t.Fatalf("retried too early: %d", rec.count())
		}
		c.Advance(time.Millisecond)
	}
	dl, _ := d.Delivery(id)
	if dl.Status != StatusDelivered || len(dl.Attempts) != 4 || dl.Attempts[3].StatusCode != 200 || dl.Attempts[0].StatusCode != 500 {
		t.Fatalf("final = %+v", dl)
	}
}

func TestGiveUpAndRedeliver(t *testing.T) {
	d, c, rec, srv := setup(t, 100, Config{MaxAttempts: 3, BaseBackoff: time.Second})
	if _, err := d.SetEndpoint("acc", srv.URL, "s"); err != nil {
		t.Fatal(err)
	}
	id := d.Publish(event("evt_f"), PublishOptions{})[0]
	c.Advance(time.Hour)
	dl, _ := d.Delivery(id)
	if dl.Status != StatusFailed || len(dl.Attempts) != 3 || rec.count() != 3 {
		t.Fatalf("delivery = %+v", dl)
	}
	rec.mu.Lock()
	rec.failures = 0
	rec.mu.Unlock()
	if _, err := d.Redeliver(id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Redeliver(id); err == nil {
		t.Fatal("redeliver of pending accepted")
	}
	if _, err := d.Redeliver("nope"); err == nil {
		t.Fatal("unknown id accepted")
	}
	c.Advance(0)
	dl, _ = d.Delivery(id)
	if dl.Status != StatusDelivered || len(dl.Attempts) != 1 {
		t.Fatalf("redelivered = %+v", dl)
	}
}

func TestNetworkErrorRetries(t *testing.T) {
	c := clock.NewFake(t0)
	d := New(c, nil, Config{MaxAttempts: 2, Timeout: time.Second}, func(n int64) int64 { return 0 }, nil)
	defer d.Close()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused
	if _, err := d.SetEndpoint("acc", url, "s"); err != nil {
		t.Fatal(err)
	}
	id := d.Publish(event("evt_n"), PublishOptions{})[0]
	c.Advance(time.Minute)
	dl, _ := d.Delivery(id)
	if dl.Status != StatusFailed || len(dl.Attempts) != 2 || dl.Attempts[0].Error == "" {
		t.Fatalf("delivery = %+v", dl)
	}
}

func TestDuplicatesAndReorder(t *testing.T) {
	d, c, rec, srv := setup(t, 0, Config{})
	if _, err := d.SetEndpoint("acc", srv.URL, "s"); err != nil {
		t.Fatal(err)
	}
	ids := d.Publish(event("evt_d"), PublishOptions{Duplicates: 2, Reorder: 10 * time.Second})
	if len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
	c.Advance(9 * time.Second)
	if rec.count() != 0 {
		t.Fatal("reorder delay not applied")
	}
	c.Advance(time.Second)
	if rec.count() != 3 {
		t.Fatalf("requests = %d", rec.count())
	}
	for _, r := range rec.got {
		if r.Header.Get(webhook.EventIDHeader) != "evt_d" {
			t.Fatal("duplicates must share the event id")
		}
	}
	log := d.Deliveries("acc", "evt_d")
	if len(log) != 3 || log[0].Copy != 2 || log[2].Copy != 0 {
		t.Fatalf("log = %+v", log)
	}
	if len(d.Deliveries("other", "")) != 0 {
		t.Fatal("filter")
	}
}

func TestLogSizeAndEndpoints(t *testing.T) {
	d, c, _, srv := setup(t, 0, Config{LogSize: 2})
	if _, err := d.SetEndpoint("acc", "ftp://x", ""); err == nil {
		t.Fatal("ftp accepted")
	}
	if _, err := d.SetEndpoint("acc", srv.URL, "s"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		d.Publish(event(id), PublishOptions{})
		c.Advance(0)
	}
	if got := d.Deliveries("", ""); len(got) != 2 || got[0].EventID != "c" {
		t.Fatalf("log = %+v", got)
	}
	if !d.RemoveEndpoint("acc") || d.RemoveEndpoint("acc") {
		t.Fatal("RemoveEndpoint")
	}
	if _, ok := d.Delivery("nope"); ok {
		t.Fatal("Delivery")
	}
}

func TestCloseCancelsPending(t *testing.T) {
	d, c, rec, srv := setup(t, 0, Config{})
	if _, err := d.SetEndpoint("acc", srv.URL, "s"); err != nil {
		t.Fatal(err)
	}
	d.Publish(event("evt_c"), PublishOptions{})
	d.Close()
	c.Advance(time.Hour)
	if rec.count() != 0 {
		t.Fatal("delivered after Close")
	}
	if ids := d.Publish(event("evt_c2"), PublishOptions{}); ids != nil {
		t.Fatal("published after Close")
	}
}
