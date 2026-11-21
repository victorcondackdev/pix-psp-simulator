package webhook_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

// Verifying a delivery inside an HTTP handler.
func ExampleVerify() {
	secret := []byte("whsec_example")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := webhook.Verify(secret, r.Header.Get(webhook.SignatureHeader), body, 5*time.Minute); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		// Deduplicate on r.Header.Get(webhook.EventIDHeader) before acting.
		w.WriteHeader(http.StatusNoContent)
	})

	body := `{"id":"evt_1","type":"pix.received"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pix", strings.NewReader(body))
	req.Header.Set(webhook.SignatureHeader, webhook.Sign(secret, time.Now(), []byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	fmt.Println(rec.Code)

	tampered := httptest.NewRequest(http.MethodPost, "/webhooks/pix", strings.NewReader(`{"id":"evt_1","type":"pix.rejected"}`))
	tampered.Header.Set(webhook.SignatureHeader, req.Header.Get(webhook.SignatureHeader))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, tampered)
	fmt.Println(rec.Code)
	// Output:
	// 204
	// 401
}

// Signatures are deterministic for a given secret, time and body.
func ExampleSign() {
	at := time.Unix(1_800_000_000, 0)
	fmt.Println(webhook.Sign([]byte("secret"), at, []byte(`{}`))[:13])
	err := webhook.VerifyAt([]byte("secret"), webhook.Sign([]byte("secret"), at, []byte(`{}`)), []byte(`{}`), time.Minute, at.Add(2*time.Minute))
	fmt.Println(err)
	// Output:
	// t=1800000000,
	// webhook: timestamp outside tolerance
}
