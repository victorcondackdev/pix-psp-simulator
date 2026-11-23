package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/sim"
	"github.com/victorcondackdev/pix-psp-simulator/internal/trace"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestIDHeader carries the request ID in both directions.
const RequestIDHeader = "X-Request-Id"

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// withRequestID accepts a client-provided X-Request-Id (up to 128 chars) or
// generates one, and echoes it in the response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// withTrace continues the caller's W3C trace (or starts one), stores the
// simulator's span in the context and returns it in the traceparent response
// header.
func withTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc := trace.Start(r.Header.Get(trace.Header))
		w.Header().Set(trace.Header, tc.String())
		next.ServeHTTP(w, r.WithContext(trace.WithContext(r.Context(), tc)))
	})
}

// traceparent returns the traceparent to propagate from this request.
func traceparent(r *http.Request) string {
	if tc, ok := trace.FromContext(r.Context()); ok {
		return tc.String()
	}
	return ""
}

// withMetrics counts requests and latency by route pattern. Unknown paths
// are recorded under "unmatched" so a scanner cannot explode cardinality.
func withMetrics(t *sim.Telemetry, route func(*http.Request) string, next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		rt := route(r)
		t.HTTPRequests.Inc(rt, r.Method, strconv.Itoa(rec.status))
		t.HTTPDuration.ObserveDuration(time.Since(start), rt)
	})
}

// withLogging logs one structured line per request.
func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		tc, _ := trace.FromContext(r.Context())
		log.LogAttrs(r.Context(), level, "http request",
			slog.String("request_id", requestID(r.Context())),
			slog.String("trace_id", tc.TraceID),
			slog.String("span_id", tc.SpanID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// withRecover turns panics into 500 problems.
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic", "request_id", requestID(r.Context()), "panic", v)
				writeProblem(w, r, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
