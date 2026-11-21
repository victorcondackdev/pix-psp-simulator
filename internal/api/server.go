// Package api exposes the simulator over HTTP: JSON endpoints with
// problem+json errors, the embedded dashboard and the OpenAPI document.
package api

import (
	"bytes"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sim"
)

//go:embed openapi.json
var openapiJSON []byte

//go:embed static
var staticFS embed.FS

// OpenAPI returns the embedded OpenAPI document.
func OpenAPI() []byte { return bytes.Clone(openapiJSON) }

// Advancer is a clock that can be moved forward (time travel).
type Advancer interface {
	clock.Clock
	Advance(time.Duration)
}

// Route is one registered endpoint.
type Route struct {
	Method  string
	Pattern string
	handler http.HandlerFunc
}

// Server is the HTTP front end of a simulator.
type Server struct {
	sim    *sim.Sim
	clock  Advancer
	log    *slog.Logger
	mux    *http.ServeMux
	routes []Route
	start  time.Time

	idemMu sync.Mutex
	idem   map[string]idemEntry
}

type idemEntry struct {
	hash [32]byte
	e2e  string
	at   time.Time
}

// New builds the server and registers every route.
func New(s *sim.Sim, c Advancer, log *slog.Logger) *Server {
	srv := &Server{sim: s, clock: c, log: log, mux: http.NewServeMux(), start: time.Now(), idem: map[string]idemEntry{}}
	srv.register()
	return srv
}

func (s *Server) handle(method, pattern string, h http.HandlerFunc) {
	s.routes = append(s.routes, Route{Method: method, Pattern: pattern, handler: h})
	s.mux.HandleFunc(method+" "+pattern, h)
}

// Routes returns the registered routes, in registration order.
func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

func (s *Server) register() {
	s.handle("GET", "/healthz", s.healthz)
	s.handle("GET", "/openapi.json", s.openapi)
	s.handle("GET", "/{$}", s.dashboard)

	s.handle("POST", "/accounts", s.createAccount)
	s.handle("GET", "/accounts", s.listAccounts)
	s.handle("GET", "/accounts/{id}", s.getAccount)
	s.handle("POST", "/accounts/{id}/deposits", s.deposit)
	s.handle("PUT", "/accounts/{id}/status", s.setAccountStatus)
	s.handle("GET", "/accounts/{id}/keys", s.accountKeys)
	s.handle("PUT", "/accounts/{id}/webhook", s.setWebhook)
	s.handle("GET", "/accounts/{id}/webhook", s.getWebhook)
	s.handle("DELETE", "/accounts/{id}/webhook", s.deleteWebhook)

	s.handle("POST", "/dict/keys", s.registerKey)
	s.handle("GET", "/dict/keys/{key}", s.lookupKey)
	s.handle("DELETE", "/dict/keys/{key}", s.deleteKey)
	s.handle("POST", "/dict/claims", s.openClaim)
	s.handle("GET", "/dict/claims", s.listClaims)
	s.handle("GET", "/dict/claims/{id}", s.getClaim)
	s.handle("POST", "/dict/claims/{id}/{action}", s.actOnClaim)

	s.handle("POST", "/cob", s.createCharge)
	s.handle("GET", "/cob", s.listCharges)
	s.handle("GET", "/cob/{txid}", s.getCharge)
	s.handle("DELETE", "/cob/{txid}", s.removeCharge)
	s.handle("GET", "/qr/v2/{id}", s.location)

	s.handle("POST", "/pix/transfers", s.createTransfer)
	s.handle("GET", "/pix/transfers", s.listTransfers)
	s.handle("GET", "/pix/transfers/{e2eid}", s.getTransfer)
	s.handle("POST", "/pix/qr-payments", s.payQR)
	s.handle("POST", "/pix/transfers/{e2eid}/refunds", s.createRefund)
	s.handle("GET", "/pix/transfers/{e2eid}/refunds/{id}", s.getRefund)

	s.handle("POST", "/med/infractions", s.reportInfraction)
	s.handle("GET", "/med/infractions", s.listInfractions)
	s.handle("GET", "/med/infractions/{id}", s.getInfraction)
	s.handle("POST", "/med/infractions/{id}/{action}", s.actOnInfraction)

	s.handle("GET", "/webhooks/deliveries", s.listDeliveries)
	s.handle("GET", "/webhooks/deliveries/{id}", s.getDelivery)
	s.handle("POST", "/webhooks/deliveries/{id}/redeliver", s.redeliver)

	s.handle("GET", "/admin/stats", s.stats)
	s.handle("GET", "/admin/clock", s.getClock)
	s.handle("POST", "/admin/clock/advance", s.advanceClock)
	s.handle("GET", "/admin/chaos/rules", s.listRules)
	s.handle("POST", "/admin/chaos/rules", s.addRule)
	s.handle("DELETE", "/admin/chaos/rules", s.clearRules)
	s.handle("DELETE", "/admin/chaos/rules/{id}", s.deleteRule)
	s.handle("GET", "/admin/dict/rate-limit", s.getRateLimit)
	s.handle("PUT", "/admin/dict/rate-limit", s.setRateLimit)
	s.handle("POST", "/admin/scenarios", s.loadScenario)
	s.handle("GET", "/admin/ledger", s.ledgerPostings)
}

// Handler returns the full handler chain.
func (s *Server) Handler() http.Handler {
	var notFoundAware http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := s.mux.Handler(r)
		if pattern == "" {
			// Let the mux decide between 404 and 405, but answer with a problem.
			rec := &probe{header: http.Header{}}
			h.ServeHTTP(rec, r)
			if rec.status == http.StatusMethodNotAllowed {
				w.Header().Set("Allow", rec.header.Get("Allow"))
				writeProblem(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this path")
				return
			}
			writeProblem(w, r, http.StatusNotFound, "route_not_found", "no route for "+r.Method+" "+r.URL.Path)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
	return withRequestID(withRecover(s.log, withLogging(s.log, notFoundAware)))
}

// probe captures the status of the mux's own 404/405 handler.
type probe struct {
	header http.Header
	status int
}

func (p *probe) Header() http.Header         { return p.header }
func (p *probe) Write(b []byte) (int, error) { return len(b), nil }
func (p *probe) WriteHeader(code int)        { p.status = code }

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "uptime_seconds": int(time.Since(s.start).Seconds())})
}

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiJSON)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'")
	_, _ = w.Write(b)
}
