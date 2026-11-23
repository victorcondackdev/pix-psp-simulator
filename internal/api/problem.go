package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/dict"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sim"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sta"
)

// Problem is an RFC 9457 problem details object.
type Problem struct {
	Type       string  `json:"type"`
	Title      string  `json:"title"`
	Status     int     `json:"status"`
	Detail     string  `json:"detail,omitempty"`
	Instance   string  `json:"instance,omitempty"`
	Code       string  `json:"code"`
	RequestID  string  `json:"request_id,omitempty"`
	Issues     []Issue `json:"issues,omitempty"`
	RetryAfter int     `json:"retry_after_seconds,omitempty"`
}

// Issue is a BR Code lint issue.
type Issue struct {
	Severity string `json:"severity"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	writeProblemValue(w, r, Problem{Status: status, Code: code, Detail: detail})
}

func writeProblemValue(w http.ResponseWriter, r *http.Request, p Problem) {
	p.Type = "urn:pixsim:problem:" + p.Code
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	p.Instance = r.URL.Path
	p.RequestID = requestID(r.Context())
	if p.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(p.RetryAfter))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(p)
}

// writeError maps domain errors to problems.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var se *sim.Error
	if errors.As(err, &se) {
		p := Problem{Code: se.Code, Detail: se.Msg}
		switch se.Kind {
		case sim.KindNotFound:
			p.Status = http.StatusNotFound
		case sim.KindConflict:
			p.Status = http.StatusConflict
		default:
			p.Status = http.StatusUnprocessableEntity
		}
		for _, is := range se.Issues {
			p.Issues = append(p.Issues, Issue{Severity: is.Severity.String(), Field: is.Field, Message: is.Message})
		}
		writeProblemValue(w, r, p)
		return
	}
	var inv *sta.InvalidError
	if errors.As(err, &inv) {
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_participant", inv.Error())
		return
	}
	if errors.Is(err, sta.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var rl *dict.RateLimitError
	if errors.As(err, &rl) {
		secs := int(rl.RetryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		writeProblemValue(w, r, Problem{Status: http.StatusTooManyRequests, Code: "rate_limited", Detail: "DICT lookup rate limit exceeded for this payer (simulated anti-scan policy)", RetryAfter: secs})
		return
	}
	table := []struct {
		err    error
		status int
		code   string
	}{
		{dict.ErrKeyNotFound, http.StatusNotFound, "key_not_found"},
		{dict.ErrClaimNotFound, http.StatusNotFound, "claim_not_found"},
		{dict.ErrKeyExists, http.StatusConflict, "key_exists"},
		{dict.ErrClaimInProgress, http.StatusConflict, "claim_in_progress"},
		{dict.ErrClaimSameAccount, http.StatusConflict, "claim_same_account"},
		{dict.ErrInvalidTransition, http.StatusConflict, "invalid_transition"},
		{dict.ErrKeyLimit, http.StatusUnprocessableEntity, "key_limit_reached"},
		{dict.ErrDocumentMismatch, http.StatusUnprocessableEntity, "document_mismatch"},
		{dict.ErrClaimNotAllowed, http.StatusUnprocessableEntity, "claim_not_allowed"},
		{dict.ErrInvalidClaimType, http.StatusUnprocessableEntity, "invalid_claim_type"},
		{dict.ErrNotOwner, http.StatusForbidden, "not_key_owner"},
		{dict.ErrClaimForbidden, http.StatusForbidden, "claim_forbidden"},
		{pix.ErrInvalidKey, http.StatusUnprocessableEntity, "invalid_key"},
	}
	for _, m := range table {
		if errors.Is(err, m.err) {
			writeProblem(w, r, m.status, m.code, err.Error())
			return
		}
	}
	writeProblem(w, r, http.StatusInternalServerError, "internal", err.Error())
}

const maxBody = 1 << 20

// decode reads a JSON body strictly. It writes a 400 problem and returns
// false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBytes(w, r, nil, v)
}

func decodeBytes(w http.ResponseWriter, r *http.Request, raw *[]byte, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "use Content-Type: application/json")
		return false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is larger than 1 MiB")
		return false
	}
	if raw != nil {
		*raw = b
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid JSON body: %v", err))
		return false
	}
	if dec.More() {
		writeProblem(w, r, http.StatusBadRequest, "invalid_json", "trailing data after JSON body")
		return false
	}
	return true
}
