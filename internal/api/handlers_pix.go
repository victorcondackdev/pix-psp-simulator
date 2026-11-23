package api

import (
	"net/http"

	"github.com/victorcondackdev/pix-psp-simulator/internal/sim"
)

// Pix Automático

func (s *Server) createRecurrence(w http.ResponseWriter, r *http.Request) {
	var req sim.RecurrenceRequest
	if !decode(w, r, &req) {
		return
	}
	rec, err := s.sim.CreateRecurrence(req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/pix/automatico/recurrences/"+rec.ID)
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) listRecurrences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"recurrences": s.sim.Recurrences(r.URL.Query().Get("account_id"))})
}

func (s *Server) getRecurrence(w http.ResponseWriter, r *http.Request) {
	rec, err := s.sim.Recurrence(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) actOnRecurrence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayerMaxAmountCents int64 `json:"payer_max_amount_cents,omitempty"`
	}
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	rec, err := s.sim.ActOnRecurrence(r.PathValue("id"), sim.RecurrenceEvent(r.PathValue("action")), req.PayerMaxAmountCents)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) scheduleAutoCharge(w http.ResponseWriter, r *http.Request) {
	var req sim.AutoChargeRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.sim.ScheduleAutoCharge(r.PathValue("id"), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/pix/automatico/charges/"+c.ID)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) getAutoCharge(w http.ResponseWriter, r *http.Request) {
	c, err := s.sim.AutoCharge(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) cancelAutoCharge(w http.ResponseWriter, r *http.Request) {
	c, err := s.sim.CancelAutoCharge(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// Pix por aproximação and limits

func (s *Server) payProximity(w http.ResponseWriter, r *http.Request) {
	var req sim.ProximityRequest
	if !decode(w, r, &req) {
		return
	}
	ov, ok := chaosHeaders(w, r)
	if !ok {
		return
	}
	req.Traceparent = traceparent(r)
	s.respondTransfer(w, r, func() (sim.Transfer, error) { return s.sim.PayProximity(req, ov) })
}

func (s *Server) getLimits(w http.ResponseWriter, r *http.Request) {
	l, err := s.sim.Limits(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) setLimits(w http.ResponseWriter, r *http.Request) {
	var req sim.Limits
	if !decode(w, r, &req) {
		return
	}
	l, err := s.sim.SetLimits(r.PathValue("id"), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}
