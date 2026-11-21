package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

func TestHandler(t *testing.T) {
	var logs bytes.Buffer
	h := handler([]byte("s"), slog.New(slog.NewJSONHandler(&logs, nil)))
	body := `{"id":"evt_1","type":"pix.received","account_id":"bob","data":{}}`
	send := func(sig, b string) int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(b))
		req.Header.Set(webhook.SignatureHeader, sig)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	good := webhook.Sign([]byte("s"), time.Now(), []byte(body))
	if c := send(good, body); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if c := send(good, body); c != http.StatusNoContent || !strings.Contains(logs.String(), "duplicate ignored") {
		t.Fatal(c, logs.String())
	}
	if c := send(good, body+" "); c != http.StatusUnauthorized {
		t.Fatal(c)
	}
	if c := send(webhook.Sign([]byte("s"), time.Now(), []byte("nope")), "nope"); c != http.StatusBadRequest {
		t.Fatal(c)
	}
	_ = io.Discard
}
