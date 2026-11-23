package api

import (
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/trace"
)

func TestTraceparentReachesWebhooks(t *testing.T) {
	h := newHarness(t)
	h.must("PUT", "/accounts/bob/webhook", `{"url":"`+h.hook.URL+`","secret":"s"}`, 200)
	parent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	r := h.do("POST", "/pix/transfers", `{"from_account_id":"alice","key":"bob@example.com","amount_cents":100}`, "traceparent", parent)
	if r.status != 202 {
		t.Fatal(r.status, string(r.body))
	}
	echo, ok := trace.Parse(r.header.Get("traceparent"))
	if !ok || echo.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || echo.SpanID == "00f067aa0ba902b7" {
		t.Fatalf("response traceparent %q", r.header.Get("traceparent"))
	}
	m := r.json(t)
	if m["traceparent"] != echo.String() {
		t.Fatalf("stored traceparent %v", m["traceparent"])
	}
	h.clock.Advance(time.Second)
	h.recv.mu.Lock()
	defer h.recv.mu.Unlock()
	if len(h.recv.reqs) != 1 || h.recv.reqs[0].Header.Get("traceparent") != echo.String() {
		t.Fatalf("webhook traceparent: %d reqs", len(h.recv.reqs))
	}

	// No incoming context: the simulator starts a trace.
	r = h.do("GET", "/healthz", "")
	if _, ok := trace.Parse(r.header.Get("traceparent")); !ok {
		t.Error("no traceparent on a fresh request")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t)
	h.must("POST", "/pix/transfers", `{"from_account_id":"alice","key":"bob@example.com","amount_cents":100}`, 202)
	h.must("POST", "/pix/transfers", `{"from_account_id":"bob","key":"12ABC34501DE35","amount_cents":100}`, 202, "X-Sim-Fail", "AM04")
	h.clock.Advance(time.Second)
	h.must("GET", "/dict/keys/bob@example.com", "", 200)
	h.do("GET", "/dict/keys/nobody@example.com", "")
	h.do("GET", "/no/such/route", "")
	r := h.do("GET", "/metrics", "")
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") {
		t.Fatal(r.status, r.header)
	}
	body := string(r.body)
	for _, want := range []string{
		`pixsim_transfers_total{status="ACSC",reason="",initiation="DICT"} 1`,
		`pixsim_transfers_total{status="RJCT",reason="AM04",initiation="DICT"} 1`,
		`pixsim_settlement_latency_seconds_count{status="ACSC"} 1`,
		`pixsim_dict_lookups_total{result="found"} 1`,
		`pixsim_dict_lookups_total{result="not_found"} 1`,
		`pixsim_http_requests_total{route="/pix/transfers",method="POST",code="202"} 2`,
		`pixsim_http_requests_total{route="unmatched",method="GET",code="404"} 1`,
		`pixsim_ledger_total_cents 0`,
		`pixsim_transfers_in_flight 0`,
		`pixsim_accounts 3`,
		`pixsim_ledger_balance_cents{account="sys.settlement"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
