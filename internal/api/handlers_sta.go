package api

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/bizday"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sta"
)

// STA (regulatory documents)

func (s *Server) submitDocument(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "documents are limited to 1 MiB")
		return
	}
	p, err := s.sim.STA.Submit(raw)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid_document_xml", err.Error())
		return
	}
	w.Header().Set("Location", "/sta/documents/"+p.ID)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, map[string]any{"documents": s.sim.STA.Protocols(q.Get("cnpj"), q.Get("codigo_documento"))})
}

func (s *Server) getDocument(w http.ResponseWriter, r *http.Request) {
	p, err := s.sim.STA.Protocol(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putParticipant(w http.ResponseWriter, r *http.Request) {
	var req sta.Participant
	if !decode(w, r, &req) {
		return
	}
	req.CNPJ = r.PathValue("cnpj")
	p, err := s.sim.STA.PutParticipant(req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) listParticipants(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"participants": s.sim.STA.Participants()})
}

func (s *Server) getParticipant(w http.ResponseWriter, r *http.Request) {
	st, err := s.sim.STA.Status(r.PathValue("cnpj"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) draft4111(w http.ResponseWriter, r *http.Request) {
	db := r.URL.Query().Get("data_base")
	if db == "" {
		db = s.sim.STA.Calendar().Previous(bizday.Date(s.clock.Now())).Format("2006-01-02")
	}
	raw, err := s.sim.STA.Draft4111(r.PathValue("cnpj"), db)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(raw)
}

func (s *Server) calendar(w http.ResponseWriter, r *http.Request) {
	y := bizday.Date(s.clock.Now()).Year()
	if v := r.URL.Query().Get("year"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1900 || n > 2200 {
			writeProblem(w, r, http.StatusBadRequest, "invalid_year", "year must be between 1900 and 2200")
			return
		}
		y = n
	}
	cal := s.sim.STA.Calendar()
	today := bizday.Date(s.clock.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"year": y, "holidays": bizday.Holidays(y), "today": today.Format("2006-01-02"),
		"today_is_business_day": cal.IsBusinessDay(today),
		"previous_business_day": cal.Previous(today).Format("2006-01-02"),
		"next_business_day":     cal.Add(today, 1).Format("2006-01-02"),
		"zone":                  "UTC-03:00",
		"generated_at":          s.clock.Now().Format(time.RFC3339),
	})
}

func (s *Server) camt053(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	day := q.Get("date")
	if day == "" {
		day = bizday.Date(s.clock.Now()).Format("2006-01-02")
	}
	raw, err := s.sim.Statement(q.Get("account_id"), day)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(raw)
}
