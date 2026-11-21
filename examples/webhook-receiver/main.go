// Command webhook-receiver is a minimal consumer of the simulator's webhooks.
// It verifies signatures with the webhook package, deduplicates on the event
// ID (the simulator can deliver duplicates on purpose) and logs each event.
//
//	go run ./examples/webhook-receiver -addr :9000 -secret whsec_demo
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	secret := flag.String("secret", os.Getenv("WEBHOOK_SECRET"), "webhook secret")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *secret == "" {
		log.Error("set -secret or WEBHOOK_SECRET")
		os.Exit(2)
	}
	log.Info("listening", "addr", *addr)
	srv := &http.Server{Addr: *addr, Handler: handler([]byte(*secret), log), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func handler(secret []byte, log *slog.Logger) http.Handler {
	var mu sync.Mutex
	seen := map[string]bool{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if err := webhook.Verify(secret, r.Header.Get(webhook.SignatureHeader), body, 5*time.Minute); err != nil {
			log.Warn("rejected delivery", "err", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var ev webhook.Event
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		dup := seen[ev.ID]
		seen[ev.ID] = true
		mu.Unlock()
		if dup {
			log.Info("duplicate ignored", "event_id", ev.ID, "type", ev.Type)
		} else {
			log.Info("event", "event_id", ev.ID, "type", ev.Type, "account", ev.AccountID, "data", json.RawMessage(ev.Data))
		}
		// Acknowledge duplicates too, or the sender keeps retrying.
		w.WriteHeader(http.StatusNoContent)
	})
}
