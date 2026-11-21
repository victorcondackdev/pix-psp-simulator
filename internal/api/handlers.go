package api

import (
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/dict"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sim"
)

// Accounts

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var req sim.AccountRequest
	if !decode(w, r, &req) {
		return
	}
	a, err := s.sim.CreateAccount(req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/accounts/"+a.ID)
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.sim.Ledger.List(r.URL.Query().Get("include_system") == "true")})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.sim.Account(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) deposit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AmountCents int64 `json:"amount_cents"`
	}
	if !decode(w, r, &req) {
		return
	}
	a, err := s.sim.Deposit(r.PathValue("id"), req.AmountCents)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) setAccountStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status ledger.Status `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	a, err := s.sim.SetAccountStatus(r.PathValue("id"), req.Status)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) accountKeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.sim.Account(id); err != nil {
		writeError(w, r, err)
		return
	}
	keys := s.sim.DICT.Keys(id)
	if keys == nil {
		keys = []dict.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) setWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.sim.Account(id); err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		URL    string `json:"url"`
		Secret string `json:"secret,omitempty"`
	}
	if !decode(w, r, &req) {
		return
	}
	ep, err := s.sim.Notify.SetEndpoint(id, req.URL, req.Secret)
	if err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_webhook_url", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) {
	ep, ok := s.sim.Notify.Endpoint(r.PathValue("id"))
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "webhook_not_found", "no webhook registered for this account")
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.sim.Notify.RemoveEndpoint(r.PathValue("id")) {
		writeProblem(w, r, http.StatusNotFound, "webhook_not_found", "no webhook registered for this account")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DICT

func (s *Server) registerKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
		Key       string `json:"key,omitempty"`
		EVP       bool   `json:"evp,omitempty"`
	}
	if !decode(w, r, &req) {
		return
	}
	e, err := s.sim.RegisterKey(req.AccountID, req.Key, req.EVP)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

// PayerHeader identifies who is looking a key up, for rate limiting.
const PayerHeader = "X-Payer-Id"

func (s *Server) lookupKey(w http.ResponseWriter, r *http.Request) {
	payer := r.Header.Get(PayerHeader)
	if payer == "" {
		payer = r.URL.Query().Get("payer_id")
	}
	res, err := s.sim.DICT.Lookup(r.PathValue("key"), payer)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	acct := r.URL.Query().Get("account_id")
	if acct == "" {
		writeProblem(w, r, http.StatusBadRequest, "account_id_required", "query parameter account_id (the key owner) is required")
		return
	}
	if err := s.sim.DICT.Delete(r.PathValue("key"), acct); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) openClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string         `json:"account_id"`
		Type      dict.ClaimType `json:"type"`
		Key       string         `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	c, err := s.sim.OpenClaim(req.AccountID, req.Type, req.Key)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) listClaims(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"claims": s.sim.DICT.Claims()})
}

func (s *Server) getClaim(w http.ResponseWriter, r *http.Request) {
	c, err := s.sim.DICT.GetClaim(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) actOnClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	c, err := s.sim.DICT.ActOnClaim(r.PathValue("id"), dict.ClaimEvent(r.PathValue("action")), req.AccountID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// Charges

func (s *Server) createCharge(w http.ResponseWriter, r *http.Request) {
	var req sim.ChargeRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.sim.CreateCharge(req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/cob/"+c.TxID)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) listCharges(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"charges": s.sim.Charges()})
}

func (s *Server) getCharge(w http.ResponseWriter, r *http.Request) {
	c, err := s.sim.Charge(r.PathValue("txid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) removeCharge(w http.ResponseWriter, r *http.Request) {
	c, err := s.sim.RemoveCharge(r.PathValue("txid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) location(w http.ResponseWriter, r *http.Request) {
	p, err := s.sim.Location(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// Transfers

func chaosHeaders(w http.ResponseWriter, r *http.Request) (*chaos.Overrides, bool) {
	ov, err := chaos.FromHeaders(r.Header)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid_chaos_header", err.Error())
		return nil, false
	}
	return ov, true
}

// IdempotencyHeader makes POST /pix/transfers safe to retry.
const IdempotencyHeader = "Idempotency-Key"

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	var req sim.TransferRequest
	var raw []byte
	if !decodeBytes(w, r, &raw, &req) {
		return
	}
	ov, ok := chaosHeaders(w, r)
	if !ok {
		return
	}
	key := r.Header.Get(IdempotencyHeader)
	if key == "" {
		s.respondTransfer(w, r, func() (sim.Transfer, error) { return s.sim.CreateTransfer(req, ov) })
		return
	}
	// Hold the lock for the whole check-create-remember sequence so that
	// concurrent retries with the same key create one transfer.
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	now := s.clock.Now()
	for k, e := range s.idem {
		if now.Sub(e.at) > 24*time.Hour {
			delete(s.idem, k)
		}
	}
	hash := sha256.Sum256(raw)
	if e, seen := s.idem[key]; seen {
		if e.hash != hash {
			writeProblem(w, r, http.StatusUnprocessableEntity, "idempotency_key_reused", "Idempotency-Key was already used with a different request body")
			return
		}
		t, err := s.sim.Transfer(e.e2e)
		if err != nil {
			writeError(w, r, err)
			return
		}
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, t)
		return
	}
	s.respondTransfer(w, r, func() (sim.Transfer, error) {
		t, err := s.sim.CreateTransfer(req, ov)
		if err == nil {
			s.idem[key] = idemEntry{hash: hash, e2e: t.EndToEndID, at: now}
		}
		return t, err
	})
}

func (s *Server) respondTransfer(w http.ResponseWriter, r *http.Request, f func() (sim.Transfer, error)) {
	t, err := f()
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/pix/transfers/"+t.EndToEndID)
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) listTransfers(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeProblem(w, r, http.StatusBadRequest, "invalid_limit", "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": s.sim.Transfers(r.URL.Query().Get("account_id"), limit)})
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, err := s.sim.Transfer(r.PathValue("e2eid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) payQR(w http.ResponseWriter, r *http.Request) {
	var req sim.QRPaymentRequest
	if !decode(w, r, &req) {
		return
	}
	ov, ok := chaosHeaders(w, r)
	if !ok {
		return
	}
	s.respondTransfer(w, r, func() (sim.Transfer, error) { return s.sim.PayQR(req, ov) })
}

func (s *Server) createRefund(w http.ResponseWriter, r *http.Request) {
	var req sim.RefundRequest
	if !decode(w, r, &req) {
		return
	}
	ov, ok := chaosHeaders(w, r)
	if !ok {
		return
	}
	ref, err := s.sim.CreateRefund(r.PathValue("e2eid"), req, ov)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

func (s *Server) getRefund(w http.ResponseWriter, r *http.Request) {
	ref, err := s.sim.Refund(r.PathValue("e2eid"), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

// MED

func (s *Server) reportInfraction(w http.ResponseWriter, r *http.Request) {
	var req sim.InfractionRequest
	if !decode(w, r, &req) {
		return
	}
	inf, err := s.sim.ReportInfraction(req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, inf)
}

func (s *Server) listInfractions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"infractions": s.sim.Infractions()})
}

func (s *Server) getInfraction(w http.ResponseWriter, r *http.Request) {
	inf, err := s.sim.Infraction(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, inf)
}

func (s *Server) actOnInfraction(w http.ResponseWriter, r *http.Request) {
	inf, err := s.sim.ActOnInfraction(r.PathValue("id"), sim.InfractionEvent(r.PathValue("action")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, inf)
}

// Webhook deliveries

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": s.sim.Notify.Deliveries(q.Get("account_id"), q.Get("event_id"))})
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) {
	d, ok := s.sim.Notify.Delivery(r.PathValue("id"))
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "delivery_not_found", "delivery not found")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) redeliver(w http.ResponseWriter, r *http.Request) {
	d, err := s.sim.Notify.Redeliver(r.PathValue("id"))
	if err != nil {
		status, code := http.StatusConflict, "delivery_pending"
		if strings.Contains(err.Error(), "not found") {
			status, code = http.StatusNotFound, "delivery_not_found"
		}
		writeProblem(w, r, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

// Admin

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sim.Stats())
}

type clockState struct {
	Now    time.Time `json:"now"`
	Wall   time.Time `json:"wall"`
	Offset string    `json:"offset"`
}

func (s *Server) clockState() clockState {
	now, wall := s.clock.Now(), time.Now()
	return clockState{Now: now, Wall: wall, Offset: now.Sub(wall).Round(time.Second).String()}
}

func (s *Server) getClock(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.clockState())
}

func (s *Server) advanceClock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Duration string `json:"duration"`
	}
	if !decode(w, r, &req) {
		return
	}
	d, err := time.ParseDuration(req.Duration)
	if err != nil || d <= 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_duration", "duration must be a positive Go duration such as 90s, 2h or 168h")
		return
	}
	s.clock.Advance(d)
	s.log.Info("clock advanced", "by", d.String())
	writeJSON(w, http.StatusOK, s.clockState())
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rules": s.sim.Chaos.Rules()})
}

func (s *Server) addRule(w http.ResponseWriter, r *http.Request) {
	var rule chaos.Rule
	if !decode(w, r, &rule) {
		return
	}
	rule, err := s.sim.Chaos.Add(rule)
	if err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_rule", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) clearRules(w http.ResponseWriter, r *http.Request) {
	s.sim.Chaos.Clear()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	if !s.sim.Chaos.Remove(r.PathValue("id")) {
		writeProblem(w, r, http.StatusNotFound, "rule_not_found", "chaos rule not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getRateLimit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sim.DICT.Limiter.Config())
}

func (s *Server) setRateLimit(w http.ResponseWriter, r *http.Request) {
	var cfg dict.RateLimit
	if !decode(w, r, &cfg) {
		return
	}
	if cfg.RefillPerMinute < 0 || cfg.NotFoundCost < 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_rate_limit", "refill_per_minute and not_found_cost must not be negative")
		return
	}
	s.sim.DICT.Limiter.SetConfig(cfg)
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) loadScenario(w http.ResponseWriter, r *http.Request) {
	var raw []byte
	var sc sim.Scenario
	if !decodeBytes(w, r, &raw, &sc) {
		return
	}
	if err := s.sim.LoadScenario(sc); err != nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_scenario", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"loaded": sc.Name, "stats": s.sim.Stats()})
}

func (s *Server) ledgerPostings(w http.ResponseWriter, r *http.Request) {
	p := s.sim.Ledger.Postings(r.URL.Query().Get("ref"))
	if p == nil {
		p = []ledger.Posting{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"postings": p, "total_cents": s.sim.Ledger.Total()})
}
