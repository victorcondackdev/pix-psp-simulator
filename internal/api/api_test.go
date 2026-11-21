package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/notify"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sim"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

var t0 = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

type receiver struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies [][]byte
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, r)
	rc.bodies = append(rc.bodies, b)
	rc.mu.Unlock()
}

type harness struct {
	t     *testing.T
	h     http.Handler
	srv   *Server
	sim   *sim.Sim
	clock *clock.Fake
	recv  *receiver
	hook  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	c := clock.NewFake(t0)
	ch := chaos.New(1)
	recv := &receiver{}
	hook := httptest.NewServer(recv)
	t.Cleanup(hook.Close)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	n := notify.New(c, hook.Client(), notify.Config{SignTime: c.Now}, ch.Int63n, log)
	t.Cleanup(n.Close)
	s := sim.New(sim.Config{SettlementLatency: time.Second}, sim.Deps{Clock: c, Chaos: ch, Notify: n, Log: log, Rand: rand.New(rand.NewSource(3))})
	srv := New(s, c, log)
	h := &harness{t: t, h: srv.Handler(), srv: srv, sim: s, clock: c, recv: recv, hook: hook}
	h.seed()
	return h
}

func (h *harness) seed() {
	h.t.Helper()
	for _, a := range []string{
		`{"id":"alice","owner_name":"Alice Santos","document":"52998224725","initial_balance_cents":100000}`,
		`{"id":"bob","owner_name":"Bob Oliveira","document":"39053344705","initial_balance_cents":5000}`,
		`{"id":"shop","owner_name":"Loja Exemplo","document":"12ABC34501DE35"}`,
	} {
		h.must("POST", "/accounts", a, http.StatusCreated)
	}
	h.must("POST", "/dict/keys", `{"account_id":"bob","key":"bob@example.com"}`, http.StatusCreated)
	h.must("POST", "/dict/keys", `{"account_id":"shop","key":"12ABC34501DE35"}`, http.StatusCreated)
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("bad json %q: %v", r.body, err)
	}
	return m
}

func (h *harness) do(method, path, body string, headers ...string) resp {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return resp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func (h *harness) must(method, path, body string, want int, headers ...string) map[string]any {
	h.t.Helper()
	r := h.do(method, path, body, headers...)
	if r.status != want {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.status, want, r.body)
	}
	if len(r.body) == 0 {
		return nil
	}
	return r.json(h.t)
}

type tc struct {
	name    string
	method  string
	path    string
	body    string
	headers []string
	want    int
	code    string // problem code expected
	check   func(t *testing.T, m map[string]any)
}

func (h *harness) run(t *testing.T, tests []tc) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := h.do(tt.method, tt.path, tt.body, tt.headers...)
			if r.status != tt.want {
				t.Fatalf("status %d, want %d: %s", r.status, tt.want, r.body)
			}
			if tt.code != "" {
				if ct := r.header.Get("Content-Type"); ct != "application/problem+json" {
					t.Fatalf("content-type %q", ct)
				}
				m := r.json(t)
				if m["code"] != tt.code || m["type"] != "urn:pixsim:problem:"+tt.code || m["instance"] == "" || m["request_id"] == "" {
					t.Fatalf("problem = %v", m)
				}
			}
			if tt.check != nil {
				tt.check(t, r.json(t))
			}
		})
	}
}

func TestSystemEndpoints(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/", "")
	if r.status != 200 || !strings.Contains(string(r.body), "<title>pix-psp-simulator</title>") || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
		t.Fatalf("dashboard: %d %s", r.status, r.header)
	}
	if strings.Contains(string(r.body), "http://") && strings.Contains(string(r.body), "<script src=") {
		t.Fatal("dashboard must not load external scripts")
	}
	h.run(t, []tc{
		{name: "healthz", method: "GET", path: "/healthz", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["status"] != "ok" {
				t.Fatal(m)
			}
		}},
		{name: "openapi", method: "GET", path: "/openapi.json", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["openapi"] != "3.1.0" {
				t.Fatal(m["openapi"])
			}
		}},
		{name: "unknown route", method: "GET", path: "/nope", want: 404, code: "route_not_found"},
		{name: "wrong method", method: "PATCH", path: "/accounts", want: 405, code: "method_not_allowed"},
	})
	r = h.do("GET", "/healthz", "", RequestIDHeader, "abc-123")
	if r.header.Get(RequestIDHeader) != "abc-123" {
		t.Fatal("request id not echoed")
	}
	if r = h.do("GET", "/healthz", ""); !strings.HasPrefix(r.header.Get(RequestIDHeader), "req_") {
		t.Fatal("request id not generated")
	}
	if !bytes.Equal(OpenAPI(), openapiJSON) {
		t.Fatal("OpenAPI()")
	}
}

func TestAccountEndpoints(t *testing.T) {
	h := newHarness(t)
	h.run(t, []tc{
		{name: "create", method: "POST", path: "/accounts", body: `{"owner_name":"Carol","document":"11222333000181","type":"TRAN"}`, want: 201, check: func(t *testing.T, m map[string]any) {
			if m["type"] != "TRAN" || m["ispb"] != "99999999" {
				t.Fatal(m)
			}
		}},
		{name: "invalid document", method: "POST", path: "/accounts", body: `{"owner_name":"X","document":"123"}`, want: 422, code: "invalid_document"},
		{name: "duplicate", method: "POST", path: "/accounts", body: `{"id":"alice","owner_name":"X","document":"52998224725"}`, want: 409, code: "account_exists"},
		{name: "unknown field", method: "POST", path: "/accounts", body: `{"owner":"x"}`, want: 400, code: "invalid_json"},
		{name: "trailing data", method: "POST", path: "/accounts", body: `{} {}`, want: 400, code: "invalid_json"},
		{name: "wrong content type", method: "POST", path: "/accounts", body: `{}`, headers: []string{"Content-Type", "text/plain"}, want: 415, code: "unsupported_media_type"},
		{name: "list", method: "GET", path: "/accounts", want: 200, check: func(t *testing.T, m map[string]any) {
			if n := len(m["accounts"].([]any)); n != 4 {
				t.Fatal(n)
			}
		}},
		{name: "list with system", method: "GET", path: "/accounts?include_system=true", want: 200, check: func(t *testing.T, m map[string]any) {
			if n := len(m["accounts"].([]any)); n != 7 {
				t.Fatal(n)
			}
		}},
		{name: "get", method: "GET", path: "/accounts/alice", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["balance_cents"].(float64) != 100000 {
				t.Fatal(m)
			}
		}},
		{name: "get missing", method: "GET", path: "/accounts/ghost", want: 404, code: "account_not_found"},
		{name: "get system", method: "GET", path: "/accounts/sys.funding", want: 404, code: "account_not_found"},
		{name: "deposit", method: "POST", path: "/accounts/bob/deposits", body: `{"amount_cents":500}`, want: 200, check: func(t *testing.T, m map[string]any) {
			if m["balance_cents"].(float64) != 5500 {
				t.Fatal(m)
			}
		}},
		{name: "deposit zero", method: "POST", path: "/accounts/bob/deposits", body: `{"amount_cents":0}`, want: 422, code: "invalid_amount"},
		{name: "deposit bad json", method: "POST", path: "/accounts/bob/deposits", body: `nope`, want: 400, code: "invalid_json"},
		{name: "status", method: "PUT", path: "/accounts/shop/status", body: `{"status":"BLOCKED"}`, want: 200},
		{name: "status back", method: "PUT", path: "/accounts/shop/status", body: `{"status":"ACTIVE"}`, want: 200},
		{name: "bad status", method: "PUT", path: "/accounts/shop/status", body: `{"status":"ZOMBIE"}`, want: 422, code: "invalid_status"},
		{name: "status bad json", method: "PUT", path: "/accounts/shop/status", body: `[`, want: 400},
		{name: "keys", method: "GET", path: "/accounts/bob/keys", want: 200, check: func(t *testing.T, m map[string]any) {
			if n := len(m["keys"].([]any)); n != 1 {
				t.Fatal(n)
			}
		}},
		{name: "no keys", method: "GET", path: "/accounts/alice/keys", want: 200, check: func(t *testing.T, m map[string]any) {
			if n := len(m["keys"].([]any)); n != 0 {
				t.Fatal(n)
			}
		}},
		{name: "keys missing account", method: "GET", path: "/accounts/ghost/keys", want: 404},
	})
	big := `{"owner_name":"` + strings.Repeat("x", maxBody) + `","document":"1"}`
	if r := h.do("POST", "/accounts", big); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %d", r.status)
	}
}

func TestWebhookRegistrationEndpoints(t *testing.T) {
	h := newHarness(t)
	h.run(t, []tc{
		{name: "missing webhook", method: "GET", path: "/accounts/bob/webhook", want: 404, code: "webhook_not_found"},
		{name: "set", method: "PUT", path: "/accounts/bob/webhook", body: `{"url":"` + h.hook.URL + `"}`, want: 200, check: func(t *testing.T, m map[string]any) {
			if !strings.HasPrefix(m["secret"].(string), "whsec_") {
				t.Fatal(m)
			}
		}},
		{name: "get", method: "GET", path: "/accounts/bob/webhook", want: 200},
		{name: "bad url", method: "PUT", path: "/accounts/bob/webhook", body: `{"url":"ftp://x"}`, want: 422, code: "invalid_webhook_url"},
		{name: "bad json", method: "PUT", path: "/accounts/bob/webhook", body: `{"x":1}`, want: 400},
		{name: "unknown account", method: "PUT", path: "/accounts/ghost/webhook", body: `{"url":"http://x"}`, want: 404},
		{name: "delete", method: "DELETE", path: "/accounts/bob/webhook", want: 204},
		{name: "delete again", method: "DELETE", path: "/accounts/bob/webhook", want: 404},
	})
}

func TestDICTEndpoints(t *testing.T) {
	h := newHarness(t)
	h.run(t, []tc{
		{name: "register email", method: "POST", path: "/dict/keys", body: `{"account_id":"alice","key":"Alice@Example.com"}`, want: 201, check: func(t *testing.T, m map[string]any) {
			if m["key"] != "alice@example.com" || m["key_type"] != "EMAIL" {
				t.Fatal(m)
			}
		}},
		{name: "register evp", method: "POST", path: "/dict/keys", body: `{"account_id":"alice","evp":true}`, want: 201, check: func(t *testing.T, m map[string]any) {
			if m["key_type"] != "EVP" {
				t.Fatal(m)
			}
		}},
		{name: "register phone", method: "POST", path: "/dict/keys", body: `{"account_id":"alice","key":"+55 21 98765-4321"}`, want: 201},
		{name: "duplicate", method: "POST", path: "/dict/keys", body: `{"account_id":"bob","key":"alice@example.com"}`, want: 409, code: "key_exists"},
		{name: "invalid key", method: "POST", path: "/dict/keys", body: `{"account_id":"alice","key":"not-a-key"}`, want: 422, code: "invalid_key"},
		{name: "cpf of other", method: "POST", path: "/dict/keys", body: `{"account_id":"alice","key":"39053344705"}`, want: 422, code: "document_mismatch"},
		{name: "unknown account", method: "POST", path: "/dict/keys", body: `{"account_id":"ghost","key":"x@y.com"}`, want: 404},
		{name: "bad json", method: "POST", path: "/dict/keys", body: `{`, want: 400},
		{name: "lookup cnpj", method: "GET", path: "/dict/keys/12ABC34501DE35", headers: []string{PayerHeader, "alice"}, want: 200, check: func(t *testing.T, m map[string]any) {
			if m["owner_tax_id_masked"] != "12.***.***/01DE-**" || m["owner_type"] != "LEGAL_PERSON" || m["owner_name"] != "Loja Exemplo" {
				t.Fatal(m)
			}
		}},
		{name: "lookup formatted cnpj escaped", method: "GET", path: "/dict/keys/" + url.PathEscape("12.ABC.345/01DE-35"), want: 200},
		{name: "lookup phone", method: "GET", path: "/dict/keys/+5521987654321?payer_id=bob", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["owner_tax_id_masked"] != "***.982.247-**" {
				t.Fatal(m)
			}
		}},
		{name: "lookup missing", method: "GET", path: "/dict/keys/nobody@example.com", want: 404, code: "key_not_found"},
		{name: "lookup invalid", method: "GET", path: "/dict/keys/zzz", want: 422, code: "invalid_key"},
		{name: "delete needs account", method: "DELETE", path: "/dict/keys/alice@example.com", want: 400, code: "account_id_required"},
		{name: "delete not owner", method: "DELETE", path: "/dict/keys/alice@example.com?account_id=bob", want: 403, code: "not_key_owner"},
		{name: "delete", method: "DELETE", path: "/dict/keys/alice@example.com?account_id=alice", want: 204},
		{name: "delete missing", method: "DELETE", path: "/dict/keys/alice@example.com?account_id=alice", want: 404},
	})
}

func TestDICTRateLimitEndpoint(t *testing.T) {
	h := newHarness(t)
	h.run(t, []tc{
		{name: "get policy", method: "GET", path: "/admin/dict/rate-limit", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["capacity"].(float64) != 100 {
				t.Fatal(m)
			}
		}},
		{name: "bad policy", method: "PUT", path: "/admin/dict/rate-limit", body: `{"capacity":2,"refill_per_minute":-1}`, want: 422, code: "invalid_rate_limit"},
		{name: "bad json", method: "PUT", path: "/admin/dict/rate-limit", body: `{"cap":1}`, want: 400},
		{name: "set policy", method: "PUT", path: "/admin/dict/rate-limit", body: `{"capacity":2,"refill_per_minute":1,"not_found_cost":0}`, want: 200},
		{name: "1", method: "GET", path: "/dict/keys/bob@example.com", headers: []string{PayerHeader, "scanner"}, want: 200},
		{name: "2", method: "GET", path: "/dict/keys/bob@example.com", headers: []string{PayerHeader, "scanner"}, want: 200},
		{name: "3 limited", method: "GET", path: "/dict/keys/bob@example.com", headers: []string{PayerHeader, "scanner"}, want: 429, code: "rate_limited"},
		{name: "other payer", method: "GET", path: "/dict/keys/bob@example.com", headers: []string{PayerHeader, "honest"}, want: 200},
	})
	r := h.do("GET", "/dict/keys/bob@example.com", "", PayerHeader, "scanner")
	if r.header.Get("Retry-After") == "" || r.json(t)["retry_after_seconds"].(float64) < 1 {
		t.Fatalf("Retry-After missing: %v %s", r.header, r.body)
	}
	h.clock.Advance(time.Minute)
	h.must("GET", "/dict/keys/bob@example.com", "", 200, PayerHeader, "scanner")
}

func TestClaimEndpoints(t *testing.T) {
	h := newHarness(t)
	h.must("POST", "/accounts", `{"id":"bob2","owner_name":"Bob Oliveira","document":"39053344705","ispb":"11111111"}`, 201)
	h.must("POST", "/dict/keys", `{"account_id":"bob","key":"+5511912345678"}`, 201)
	c := h.must("POST", "/dict/claims", `{"account_id":"bob2","type":"PORTABILITY","key":"bob@example.com"}`, 201)
	id := c["id"].(string)
	h.run(t, []tc{
		{name: "in progress", method: "POST", path: "/dict/claims", body: `{"account_id":"bob2","type":"PORTABILITY","key":"bob@example.com"}`, want: 409, code: "claim_in_progress"},
		{name: "ownership same person", method: "POST", path: "/dict/claims", body: `{"account_id":"bob2","type":"OWNERSHIP","key":"+5511912345678"}`, want: 422, code: "claim_not_allowed"},
		{name: "bad type", method: "POST", path: "/dict/claims", body: `{"account_id":"bob2","type":"X","key":"+5511912345678"}`, want: 422, code: "invalid_claim_type"},
		{name: "own key", method: "POST", path: "/dict/claims", body: `{"account_id":"bob","type":"PORTABILITY","key":"+5511912345678"}`, want: 409, code: "claim_same_account"},
		{name: "missing key", method: "POST", path: "/dict/claims", body: `{"account_id":"bob2","type":"PORTABILITY","key":"z@z.com"}`, want: 404, code: "key_not_found"},
		{name: "unknown claimer", method: "POST", path: "/dict/claims", body: `{"account_id":"ghost","type":"PORTABILITY","key":"z@z.com"}`, want: 404},
		{name: "bad json", method: "POST", path: "/dict/claims", body: `{"x":1}`, want: 400},
		{name: "list", method: "GET", path: "/dict/claims", want: 200},
		{name: "get", method: "GET", path: "/dict/claims/" + id, want: 200},
		{name: "get missing", method: "GET", path: "/dict/claims/nope", want: 404, code: "claim_not_found"},
		{name: "complete too early", method: "POST", path: "/dict/claims/" + id + "/complete", body: `{"account_id":"bob2"}`, want: 409, code: "invalid_transition"},
		{name: "confirm by claimer", method: "POST", path: "/dict/claims/" + id + "/confirm", body: `{"account_id":"bob2"}`, want: 403, code: "claim_forbidden"},
		{name: "confirm", method: "POST", path: "/dict/claims/" + id + "/confirm", body: `{"account_id":"bob"}`, want: 200},
		{name: "act bad json", method: "POST", path: "/dict/claims/" + id + "/complete", body: `x`, want: 400},
		{name: "complete", method: "POST", path: "/dict/claims/" + id + "/complete", body: `{"account_id":"bob2"}`, want: 200, check: func(t *testing.T, m map[string]any) {
			if m["status"] != "COMPLETED" {
				t.Fatal(m)
			}
		}},
		{name: "lookup moved", method: "GET", path: "/dict/keys/bob@example.com", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["ispb"] != "11111111" || m["account_id"] != "bob2" {
				t.Fatal(m)
			}
		}},
	})
}

func TestChargeEndpoints(t *testing.T) {
	h := newHarness(t)
	dyn := h.must("POST", "/cob", `{"type":"dynamic","receiver_account_id":"shop","amount_cents":2590,"description":"Pedido 42","expires_in_seconds":300}`, 201)
	txid := dyn["txid"].(string)
	loc := dyn["location"].(string)
	if !strings.HasPrefix(loc, "http://localhost:8080/qr/v2/") {
		t.Fatal(loc)
	}
	locPath := strings.TrimPrefix(loc, "http://localhost:8080")
	h.run(t, []tc{
		{name: "static", method: "POST", path: "/cob", body: `{"type":"static","receiver_account_id":"shop","key":"12ABC34501DE35","amount_cents":100,"txid":"MESA7"}`, want: 201, check: func(t *testing.T, m map[string]any) {
			if _, err := pix.Decode(m["brcode"].(string)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "dup txid", method: "POST", path: "/cob", body: `{"type":"static","receiver_account_id":"shop","key":"12ABC34501DE35","txid":"MESA7"}`, want: 409, code: "duplicate_txid"},
		{name: "static without key", method: "POST", path: "/cob", body: `{"type":"static","receiver_account_id":"shop"}`, want: 422, code: "key_required"},
		{name: "bad type", method: "POST", path: "/cob", body: `{"type":"x","receiver_account_id":"shop"}`, want: 422},
		{name: "bad json", method: "POST", path: "/cob", body: `{"amount":1}`, want: 400},
		{name: "list", method: "GET", path: "/cob", want: 200, check: func(t *testing.T, m map[string]any) {
			if n := len(m["charges"].([]any)); n != 2 {
				t.Fatal(n)
			}
		}},
		{name: "get", method: "GET", path: "/cob/" + txid, want: 200},
		{name: "get missing", method: "GET", path: "/cob/nope", want: 404, code: "charge_not_found"},
		{name: "location", method: "GET", path: locPath, want: 200, check: func(t *testing.T, m map[string]any) {
			if m["txid"] != txid || m["valor"].(map[string]any)["original"] != "25.90" || m["status"] != "ATIVA" {
				t.Fatal(m)
			}
		}},
		{name: "location missing", method: "GET", path: "/qr/v2/nope", want: 404},
		{name: "remove", method: "DELETE", path: "/cob/MESA7", want: 200},
		{name: "remove again", method: "DELETE", path: "/cob/MESA7", want: 409, code: "charge_not_active"},
	})
	// Time travel expires the dynamic charge.
	h.must("POST", "/admin/clock/advance", `{"duration":"5m"}`, 200)
	if m := h.must("GET", "/cob/"+txid, "", 200); m["status"] != "REMOVIDA_PELO_PSP" || m["expired"] != true {
		t.Fatalf("not expired: %v", m)
	}
}

func TestTransferFlowAndWebhooks(t *testing.T) {
	h := newHarness(t)
	ep := h.must("PUT", "/accounts/bob/webhook", `{"url":"`+h.hook.URL+`/bob","secret":"bob-secret"}`, 200)
	if ep["secret"] != "bob-secret" {
		t.Fatal(ep)
	}
	h.must("PUT", "/accounts/alice/webhook", `{"url":"`+h.hook.URL+`/alice","secret":"alice-secret"}`, 200)

	tr := h.must("POST", "/pix/transfers", `{"from_account_id":"alice","key":"bob@example.com","amount_cents":1234,"description":"almoço"}`, 202)
	if tr["status"] != "ACSP" || tr["initiation"] != "DICT" {
		t.Fatal(tr)
	}
	e2e := tr["end_to_end_id"].(string)
	if _, err := pix.ParseEndToEndID(e2e); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Second)
	got := h.must("GET", "/pix/transfers/"+e2e, "", 200)
	if got["status"] != "ACSC" {
		t.Fatal(got)
	}
	if m := h.must("GET", "/accounts/bob", "", 200); m["balance_cents"].(float64) != 5000+1234 {
		t.Fatal(m)
	}

	h.recv.mu.Lock()
	if len(h.recv.reqs) != 2 {
		t.Fatalf("webhooks = %d", len(h.recv.reqs))
	}
	for i, r := range h.recv.reqs {
		secret := "alice-secret"
		if r.URL.Path == "/bob" {
			secret = "bob-secret"
			if r.Header.Get(webhook.EventTypeHeader) != webhook.EventPixReceived {
				t.Fatal(r.Header)
			}
		}
		if err := webhook.VerifyAt([]byte(secret), r.Header.Get(webhook.SignatureHeader), h.recv.bodies[i], time.Minute, t0.Add(time.Second)); err != nil {
			t.Fatalf("signature %d: %v", i, err)
		}
	}
	h.recv.mu.Unlock()

	dl := h.must("GET", "/webhooks/deliveries?account_id=bob", "", 200)["deliveries"].([]any)
	if len(dl) != 1 || dl[0].(map[string]any)["status"] != "delivered" {
		t.Fatal(dl)
	}
	dlID := dl[0].(map[string]any)["id"].(string)

	h.run(t, []tc{
		{name: "list", method: "GET", path: "/pix/transfers?account_id=alice&limit=5", want: 200, check: func(t *testing.T, m map[string]any) {
			if n := len(m["transfers"].([]any)); n != 1 {
				t.Fatal(n)
			}
		}},
		{name: "bad limit", method: "GET", path: "/pix/transfers?limit=-1", want: 400, code: "invalid_limit"},
		{name: "missing", method: "GET", path: "/pix/transfers/E00000000202601010000AAAAAAAAAAA", want: 404, code: "transfer_not_found"},
		{name: "unknown key", method: "POST", path: "/pix/transfers", body: `{"from_account_id":"alice","key":"x@y.com","amount_cents":1}`, want: 404, code: "key_not_found"},
		{name: "invalid amount", method: "POST", path: "/pix/transfers", body: `{"from_account_id":"alice","to_account_id":"bob","amount_cents":0}`, want: 422, code: "invalid_amount"},
		{name: "bad json", method: "POST", path: "/pix/transfers", body: `{"from":"a"}`, want: 400},
		{name: "bad chaos header", method: "POST", path: "/pix/transfers", body: `{"from_account_id":"alice","to_account_id":"bob","amount_cents":1}`, headers: []string{"X-Sim-Fail", "ZZ99"}, want: 400, code: "invalid_chaos_header"},
		{name: "duplicate e2e", method: "POST", path: "/pix/transfers", body: `{"from_account_id":"alice","to_account_id":"bob","amount_cents":1,"end_to_end_id":"` + e2e + `"}`, want: 409, code: "duplicate_end_to_end_id"},
		{name: "deliveries filter", method: "GET", path: "/webhooks/deliveries?event_id=none", want: 200, check: func(t *testing.T, m map[string]any) {
			if len(m["deliveries"].([]any)) != 0 {
				t.Fatal(m)
			}
		}},
		{name: "delivery", method: "GET", path: "/webhooks/deliveries/" + dlID, want: 200},
		{name: "delivery missing", method: "GET", path: "/webhooks/deliveries/nope", want: 404, code: "delivery_not_found"},
		{name: "redeliver", method: "POST", path: "/webhooks/deliveries/" + dlID + "/redeliver", want: 202},
		{name: "redeliver pending", method: "POST", path: "/webhooks/deliveries/" + dlID + "/redeliver", want: 409, code: "delivery_pending"},
		{name: "redeliver missing", method: "POST", path: "/webhooks/deliveries/nope/redeliver", want: 404},
	})
}

func TestChaosHeaders(t *testing.T) {
	h := newHarness(t)
	h.must("PUT", "/accounts/shop/webhook", `{"url":"`+h.hook.URL+`/shop"}`, 200)
	tests := []struct {
		name       string
		headers    []string
		wantStatus string
		wantReason string
		advance    time.Duration
	}{
		{"fail AM04", []string{"X-Sim-Fail", "AM04"}, "RJCT", "AM04", time.Second},
		{"timeout", []string{"X-Sim-Timeout", "true"}, "RJCT", "AB03", time.Second},
		{"latency", []string{"X-Sim-Latency", "30s"}, "ACSC", "", 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := h.must("POST", "/pix/transfers", `{"from_account_id":"alice","to_account_id":"shop","amount_cents":10}`, 202, tt.headers...)
			e2e := tr["end_to_end_id"].(string)
			h.clock.Advance(tt.advance - time.Millisecond)
			if m := h.must("GET", "/pix/transfers/"+e2e, "", 200); m["status"] != "ACSP" {
				t.Fatalf("settled early: %v", m["status"])
			}
			h.clock.Advance(time.Millisecond)
			m := h.must("GET", "/pix/transfers/"+e2e, "", 200)
			if m["status"] != tt.wantStatus || (m["reason"] != nil && m["reason"] != tt.wantReason) || (tt.wantReason != "" && m["reason"] != tt.wantReason) {
				t.Fatalf("got %v %v", m["status"], m["reason"])
			}
		})
	}
	t.Run("duplicate webhooks", func(t *testing.T) {
		h.must("POST", "/pix/transfers", `{"from_account_id":"alice","to_account_id":"shop","amount_cents":10}`, 202, "X-Sim-Webhook-Duplicates", "2", "X-Sim-Webhook-Reorder", "1s")
		h.clock.Advance(3 * time.Second)
		dl := h.must("GET", "/webhooks/deliveries?account_id=shop", "", 200)["deliveries"].([]any)
		first := dl[0].(map[string]any)["event_id"]
		n := 0
		for _, d := range dl {
			if d.(map[string]any)["event_id"] == first {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("copies = %d", n)
		}
	})
}

func TestIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	body := `{"from_account_id":"alice","to_account_id":"bob","amount_cents":700}`
	first := h.must("POST", "/pix/transfers", body, 202, IdempotencyHeader, "k1")
	r := h.do("POST", "/pix/transfers", body, IdempotencyHeader, "k1")
	if r.status != 200 || r.header.Get("Idempotent-Replayed") != "true" || r.json(t)["end_to_end_id"] != first["end_to_end_id"] {
		t.Fatalf("replay: %d %s", r.status, r.body)
	}
	h.run(t, []tc{
		{name: "reused with other body", method: "POST", path: "/pix/transfers", body: `{"from_account_id":"alice","to_account_id":"bob","amount_cents":701}`, headers: []string{IdempotencyHeader, "k1"}, want: 422, code: "idempotency_key_reused"},
		{name: "failed create not remembered", method: "POST", path: "/pix/transfers", body: `{"from_account_id":"ghost","to_account_id":"bob","amount_cents":1}`, headers: []string{IdempotencyHeader, "k2"}, want: 404},
	})
	h.clock.Advance(25 * time.Hour)
	h.must("POST", "/pix/transfers", body, 202, IdempotencyHeader, "k1")
	if n := len(h.sim.Transfers("", 0)); n != 2 {
		t.Fatalf("transfers = %d", n)
	}
}

func TestQRPaymentEndpoint(t *testing.T) {
	h := newHarness(t)
	dyn := h.must("POST", "/cob", `{"type":"dynamic","receiver_account_id":"shop","amount_cents":990}`, 201)
	code := dyn["brcode"].(string)
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	badCRC := code[:len(code)-4] + "0000"
	h.run(t, []tc{
		{name: "lint errors", method: "POST", path: "/pix/qr-payments", body: enc(map[string]any{"from_account_id": "alice", "brcode": badCRC}), want: 422, code: "brcode_lint_failed", check: func(t *testing.T, m map[string]any) {
			is := m["issues"].([]any)
			if len(is) == 0 || is[0].(map[string]any)["severity"] != "error" {
				t.Fatal(m)
			}
		}},
		{name: "bad chaos", method: "POST", path: "/pix/qr-payments", body: enc(map[string]any{"from_account_id": "alice", "brcode": code}), headers: []string{"X-Sim-Latency", "x"}, want: 400},
		{name: "bad json", method: "POST", path: "/pix/qr-payments", body: `{"qr":1}`, want: 400},
		{name: "pay", method: "POST", path: "/pix/qr-payments", body: enc(map[string]any{"from_account_id": "alice", "brcode": code}), want: 202, check: func(t *testing.T, m map[string]any) {
			if m["initiation"] != "QRDN" || m["txid"] != dyn["txid"] {
				t.Fatal(m)
			}
		}},
		{name: "pay again in flight", method: "POST", path: "/pix/qr-payments", body: enc(map[string]any{"from_account_id": "bob", "brcode": code}), want: 409, code: "charge_payment_in_progress"},
	})
	h.clock.Advance(time.Second)
	if m := h.must("GET", "/cob/"+dyn["txid"].(string), "", 200); m["status"] != "CONCLUIDA" {
		t.Fatal(m)
	}
}

func TestRefundAndMEDEndpoints(t *testing.T) {
	h := newHarness(t)
	tr := h.must("POST", "/pix/transfers", `{"from_account_id":"alice","to_account_id":"shop","amount_cents":3000}`, 202)
	e2e := tr["end_to_end_id"].(string)
	h.run(t, []tc{
		{name: "refund pending", method: "POST", path: "/pix/transfers/" + e2e + "/refunds", body: `{"amount_cents":1}`, want: 409, code: "transfer_not_settled"},
	})
	h.clock.Advance(time.Second)
	ref := h.must("POST", "/pix/transfers/"+e2e+"/refunds", `{"amount_cents":1000,"description":"troca"}`, 201)
	refID := ref["id"].(string)
	h.run(t, []tc{
		{name: "get refund", method: "GET", path: "/pix/transfers/" + e2e + "/refunds/" + refID, want: 200, check: func(t *testing.T, m map[string]any) {
			if m["return_reason"] != "MD06" || m["status"] != "ACSC" {
				t.Fatal(m)
			}
		}},
		{name: "refund missing", method: "GET", path: "/pix/transfers/" + e2e + "/refunds/nope", want: 404},
		{name: "exceeds", method: "POST", path: "/pix/transfers/" + e2e + "/refunds", body: `{"amount_cents":2001}`, want: 422, code: "refund_exceeds_amount"},
		{name: "chaos rejected", method: "POST", path: "/pix/transfers/" + e2e + "/refunds", body: `{"amount_cents":1}`, headers: []string{"X-Sim-Fail", "AM04"}, want: 201, check: func(t *testing.T, m map[string]any) {
			if m["status"] != "RJCT" || m["reason"] != "AM04" {
				t.Fatal(m)
			}
		}},
		{name: "bad header", method: "POST", path: "/pix/transfers/" + e2e + "/refunds", body: `{"amount_cents":1}`, headers: []string{"X-Sim-Webhook-Duplicates", "x"}, want: 400},
		{name: "bad json", method: "POST", path: "/pix/transfers/" + e2e + "/refunds", body: `{`, want: 400},
	})

	inf := h.must("POST", "/med/infractions", `{"end_to_end_id":"`+e2e+`","details":"golpe"}`, 201)
	id := inf["id"].(string)
	h.run(t, []tc{
		{name: "duplicate infraction", method: "POST", path: "/med/infractions", body: `{"end_to_end_id":"` + e2e + `"}`, want: 409, code: "infraction_in_progress"},
		{name: "bad reason", method: "POST", path: "/med/infractions", body: `{"end_to_end_id":"` + e2e + `","reason":"X"}`, want: 422},
		{name: "bad json", method: "POST", path: "/med/infractions", body: `{"e2e":1}`, want: 400},
		{name: "invalid transition", method: "POST", path: "/med/infractions/" + id + "/accept", want: 409, code: "invalid_transition"},
		{name: "block", method: "POST", path: "/med/infractions/" + id + "/block", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["status"] != "BLOCKED" || m["held_cents"].(float64) != 2000 {
				t.Fatal(m)
			}
		}},
		{name: "analyze", method: "POST", path: "/med/infractions/" + id + "/analyze", want: 200},
		{name: "accept", method: "POST", path: "/med/infractions/" + id + "/accept", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["status"] != "REFUNDED" || m["refund_id"] == "" || len(m["history"].([]any)) != 3 {
				t.Fatal(m)
			}
		}},
		{name: "get", method: "GET", path: "/med/infractions/" + id, want: 200},
		{name: "get missing", method: "GET", path: "/med/infractions/nope", want: 404},
		{name: "list", method: "GET", path: "/med/infractions", want: 200},
		{name: "alice made whole", method: "GET", path: "/accounts/alice", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["balance_cents"].(float64) != 100000 {
				t.Fatal(m)
			}
		}},
	})
}

func TestAdminEndpoints(t *testing.T) {
	h := newHarness(t)
	scenario := `{"name":"extra","accounts":[{"id":"dave","owner_name":"Dave","document":"11144477735","initial_balance_cents":10}],
		"keys":[{"account_id":"dave","evp":true}],"chaos_rules":[{"id":"r1","match":{"operation":"refund"},"fail":"DS04"}]}`
	h.run(t, []tc{
		{name: "clock", method: "GET", path: "/admin/clock", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["now"] != t0.Format(time.RFC3339) {
				t.Fatal(m)
			}
		}},
		{name: "advance", method: "POST", path: "/admin/clock/advance", body: `{"duration":"48h"}`, want: 200, check: func(t *testing.T, m map[string]any) {
			if m["now"] != t0.Add(48*time.Hour).Format(time.RFC3339) {
				t.Fatal(m)
			}
		}},
		{name: "advance invalid", method: "POST", path: "/admin/clock/advance", body: `{"duration":"-1h"}`, want: 422, code: "invalid_duration"},
		{name: "advance bad json", method: "POST", path: "/admin/clock/advance", body: `{"seconds":1}`, want: 400},
		{name: "add rule", method: "POST", path: "/admin/chaos/rules", body: `{"name":"slow","latency":"2s"}`, want: 201, check: func(t *testing.T, m map[string]any) {
			if m["id"] != "rule_1" || m["latency"] != "2s" {
				t.Fatal(m)
			}
		}},
		{name: "add invalid rule", method: "POST", path: "/admin/chaos/rules", body: `{"fail":"NOPE"}`, want: 422, code: "invalid_rule"},
		{name: "rule bad json", method: "POST", path: "/admin/chaos/rules", body: `{"fial":"x"}`, want: 400},
		{name: "list rules", method: "GET", path: "/admin/chaos/rules", want: 200, check: func(t *testing.T, m map[string]any) {
			if len(m["rules"].([]any)) != 1 {
				t.Fatal(m)
			}
		}},
		{name: "delete rule", method: "DELETE", path: "/admin/chaos/rules/rule_1", want: 204},
		{name: "delete missing rule", method: "DELETE", path: "/admin/chaos/rules/rule_1", want: 404, code: "rule_not_found"},
		{name: "scenario", method: "POST", path: "/admin/scenarios", body: scenario, want: 201, check: func(t *testing.T, m map[string]any) {
			if m["loaded"] != "extra" {
				t.Fatal(m)
			}
		}},
		{name: "scenario invalid", method: "POST", path: "/admin/scenarios", body: scenario, want: 422, code: "invalid_scenario"},
		{name: "scenario bad json", method: "POST", path: "/admin/scenarios", body: `{"nme":"x"}`, want: 400},
		{name: "clear rules", method: "DELETE", path: "/admin/chaos/rules", want: 204},
		{name: "stats", method: "GET", path: "/admin/stats", want: 200, check: func(t *testing.T, m map[string]any) {
			if m["accounts"].(float64) != 4 || m["chaos_rules"].(float64) != 0 || m["ledger_total_cents"].(float64) != 0 {
				t.Fatal(m)
			}
		}},
		{name: "ledger", method: "GET", path: "/admin/ledger", want: 200, check: func(t *testing.T, m map[string]any) {
			if len(m["postings"].([]any)) != 3 {
				t.Fatal(m)
			}
		}},
		{name: "ledger filter", method: "GET", path: "/admin/ledger?ref=none", want: 200, check: func(t *testing.T, m map[string]any) {
			if len(m["postings"].([]any)) != 0 {
				t.Fatal(m)
			}
		}},
	})
}

func TestRecoverMiddleware(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := withRequestID(withRecover(log, withLogging(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	writeError(rec, httptest.NewRequest("GET", "/x", nil), io.ErrUnexpectedEOF)
	if rec.Code != 500 {
		t.Fatal(rec.Code)
	}
}
