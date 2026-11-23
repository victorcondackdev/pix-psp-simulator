// Package trace implements the part of W3C Trace Context the simulator needs:
// parse an incoming traceparent, start a child span or a new trace, and carry
// it in a context. There is no exporter; the IDs end up in logs and in the
// traceparent header of outgoing webhooks, which is enough to follow one Pix
// across services with a log search.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// Header is the W3C trace context header name.
const Header = "traceparent"

// Context is one span's position in a trace.
type Context struct {
	TraceID string // 32 lowercase hex characters, not all zero
	SpanID  string // 16 lowercase hex characters, not all zero
	Sampled bool
}

func isHex(s string, n int) bool {
	if len(s) != n || strings.Trim(s, "0") == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Parse decodes a version 00 traceparent ("00-<trace>-<span>-<flags>").
// Future versions are accepted when their first four fields are valid, as the
// specification requires.
func Parse(h string) (Context, bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) < 4 || parts[0] == "ff" {
		return Context{}, false
	}
	if v, err := hex.DecodeString(parts[0]); err != nil || len(v) != 1 || strings.ToLower(parts[0]) != parts[0] {
		return Context{}, false
	}
	if parts[0] == "00" && len(parts) != 4 {
		return Context{}, false
	}
	if !isHex(parts[1], 32) || !isHex(parts[2], 16) || len(parts[3]) != 2 {
		return Context{}, false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil {
		return Context{}, false
	}
	return Context{TraceID: parts[1], SpanID: parts[2], Sampled: flags[0]&1 == 1}, true
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	for {
		_, _ = rand.Read(b)
		s := hex.EncodeToString(b)
		if strings.Trim(s, "0") != "" {
			return s
		}
	}
}

// New starts a trace.
func New() Context { return Context{TraceID: randomHex(32), SpanID: randomHex(16), Sampled: true} }

// Child returns a new span in the same trace.
func (c Context) Child() Context {
	return Context{TraceID: c.TraceID, SpanID: randomHex(16), Sampled: c.Sampled}
}

// String encodes c as a traceparent header value.
func (c Context) String() string {
	flags := "00"
	if c.Sampled {
		flags = "01"
	}
	return "00-" + c.TraceID + "-" + c.SpanID + "-" + flags
}

// Valid reports whether c holds IDs.
func (c Context) Valid() bool { return c.TraceID != "" }

type ctxKey struct{}

// WithContext stores c in ctx.
func WithContext(ctx context.Context, c Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the span stored in ctx.
func FromContext(ctx context.Context) (Context, bool) {
	c, ok := ctx.Value(ctxKey{}).(Context)
	return c, ok
}

// Start continues the trace in header (as a child span) or starts a new one.
func Start(header string) Context {
	if parent, ok := Parse(header); ok {
		return parent.Child()
	}
	return New()
}
