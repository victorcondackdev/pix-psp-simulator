// Package webhook signs and verifies the webhook deliveries made by the Pix
// PSP simulator, and defines the event envelope they carry.
//
// Consumers import this package in their own services to verify deliveries:
//
//	body, _ := io.ReadAll(r.Body)
//	if err := webhook.Verify(secret, r.Header.Get(webhook.SignatureHeader), body, 5*time.Minute); err != nil {
//		http.Error(w, "bad signature", http.StatusUnauthorized)
//		return
//	}
//
// The signature scheme follows the widely used Stripe style:
//
//	X-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
//
// Several v1 entries may be present (for example during secret rotation); the
// header is valid if any of them matches. The timestamp is part of the signed
// content and is checked against a tolerance to limit replay.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header names used by the simulator on every delivery.
const (
	SignatureHeader = "X-Signature"
	EventIDHeader   = "X-Event-Id"
	EventTypeHeader = "X-Event-Type"
)

// Event types emitted by the simulator.
const (
	EventPixReceived     = "pix.received"
	EventPixSettled      = "pix.settled"
	EventPixRejected     = "pix.rejected"
	EventRefundCompleted = "refund.completed"
)

// Event is the JSON envelope of every delivery. ID is stable across retries
// and deliberate duplicates, so consumers can deduplicate on it.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	AccountID string          `json:"account_id"`
	Data      json.RawMessage `json:"data"`
}

// Verification errors.
var (
	ErrMissingHeader   = errors.New("webhook: missing signature header")
	ErrMalformedHeader = errors.New("webhook: malformed signature header")
	ErrTimestamp       = errors.New("webhook: timestamp outside tolerance")
	ErrSignature       = errors.New("webhook: signature mismatch")
)

// Sign returns the signature header value for body at time t.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + compute(secret, ts, body)
}

func compute(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks header against body using secret and the current time.
// A tolerance of zero or less disables the timestamp check.
func Verify(secret []byte, header string, body []byte, tolerance time.Duration) error {
	return VerifyAt(secret, header, body, tolerance, time.Now())
}

// VerifyAt is Verify with an explicit current time, for tests and for
// consumers that run on a simulated clock.
func VerifyAt(secret []byte, header string, body []byte, tolerance time.Duration, now time.Time) error {
	if strings.TrimSpace(header) == "" {
		return ErrMissingHeader
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrMalformedHeader
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return ErrMalformedHeader
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrMalformedHeader
	}
	if tolerance > 0 {
		d := now.Sub(time.Unix(sec, 0))
		if d < 0 {
			d = -d
		}
		if d > tolerance {
			return ErrTimestamp
		}
	}
	want := []byte(compute(secret, ts, body))
	for _, s := range sigs {
		if hmac.Equal(want, []byte(strings.ToLower(s))) {
			return nil
		}
	}
	return ErrSignature
}
