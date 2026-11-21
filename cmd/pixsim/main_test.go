package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noenv(string) string { return "" }

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, c config)
	}{
		{name: "defaults", check: func(t *testing.T, c config) {
			if c.Addr != ":8080" || c.ISPB != "99999999" || c.Latency != 500*time.Millisecond || c.WebhookAttempts != 6 {
				t.Fatalf("%+v", c)
			}
		}},
		{name: "env", env: map[string]string{"PIXSIM_ISPB": "12345678", "PIXSIM_SETTLEMENT_LATENCY": "2s", "PIXSIM_MAX_TRANSFER_CENTS": "100"}, check: func(t *testing.T, c config) {
			if c.ISPB != "12345678" || c.Latency != 2*time.Second || c.MaxTransferCents != 100 {
				t.Fatalf("%+v", c)
			}
		}},
		{name: "flag beats env", args: []string{"-ispb", "87654321"}, env: map[string]string{"PIXSIM_ISPB": "12345678"}, check: func(t *testing.T, c config) {
			if c.ISPB != "87654321" {
				t.Fatal(c.ISPB)
			}
		}},
		{name: "bad ispb", args: []string{"-ispb", "12"}, wantErr: true},
		{name: "bad duration", args: []string{"-settlement-latency", "soon"}, wantErr: true},
		{name: "bad int", args: []string{"-seed", "x"}, wantErr: true},
		{name: "unknown flag", args: []string{"-nope"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseConfig(tt.args, func(k string) string { return tt.env[k] }, io.Discard)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestNewLogger(t *testing.T) {
	for _, c := range []config{{LogLevel: "info", LogFormat: "json"}, {LogLevel: "debug", LogFormat: "text"}} {
		if _, err := newLogger(c, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []config{{LogLevel: "loud", LogFormat: "json"}, {LogLevel: "info", LogFormat: "xml"}} {
		if _, err := newLogger(c, io.Discard); err == nil {
			t.Fatalf("%+v accepted", c)
		}
	}
}

func TestRunVersionAndErrors(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, noenv, &out, io.Discard); err != nil || !strings.Contains(out.String(), version) {
		t.Fatalf("%v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"-log-level", "x"}, noenv, io.Discard, io.Discard); err == nil {
		t.Fatal("bad log level accepted")
	}
	if err := run(context.Background(), []string{"-addr", "256.0.0.1:1"}, noenv, io.Discard, io.Discard); err == nil {
		t.Fatal("bad addr accepted")
	}
	if err := run(context.Background(), []string{"-ispb", "1"}, noenv, io.Discard, io.Discard); err == nil {
		t.Fatal("bad ispb accepted")
	}
	if err := run(context.Background(), []string{"-healthcheck", "-addr", "127.0.0.1:1"}, noenv, io.Discard, io.Discard); err == nil {
		t.Fatal("healthcheck against closed port succeeded")
	}
	if err := healthcheck("nonsense"); err == nil {
		t.Fatal("bad addr accepted")
	}
}

func TestServeEndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	c, _ := parseConfig([]string{"-settlement-latency", "10ms", "-seed", "1",
		"-scenarios", filepath.Join("..", "..", "scenarios", "basic.json")}, noenv, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var logs bytes.Buffer
	log, _ := newLogger(c, &logs)
	go func() { done <- serve(ctx, c, ln, log) }()

	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for healthcheck(addr) != nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Post(base+"/pix/transfers", "application/json",
		strings.NewReader(`{"from_account_id":"alice","key":"39053344705","amount_cents":250}`))
	if err != nil {
		t.Fatal(err)
	}
	var tr map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %v", resp.StatusCode, tr)
	}
	var status string
	for i := 0; i < 200 && status != "ACSC"; i++ {
		time.Sleep(10 * time.Millisecond)
		r, err := http.Get(base + "/pix/transfers/" + tr["end_to_end_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		r.Body.Close()
		status, _ = m["status"].(string)
	}
	if status != "ACSC" {
		t.Fatalf("transfer status %q", status)
	}
	// Dynamic charges point at this server by default.
	r, err := http.Post(base+"/cob", "application/json", strings.NewReader(`{"type":"dynamic","receiver_account_id":"padaria","amount_cents":100}`))
	if err != nil {
		t.Fatal(err)
	}
	var ch map[string]any
	_ = json.NewDecoder(r.Body).Decode(&ch)
	r.Body.Close()
	_, port, _ := net.SplitHostPort(addr)
	if !strings.HasPrefix(ch["location"].(string), "http://localhost:"+port+"/qr/v2/") {
		t.Fatalf("location %v", ch["location"])
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"msg":"http request"`) || !strings.Contains(logs.String(), "scenarios loaded") {
		t.Fatalf("logs: %s", logs.String())
	}
}

func TestServeBadScenario(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := parseConfig([]string{"-scenarios", "missing.json"}, noenv, io.Discard)
	log, _ := newLogger(c, io.Discard)
	if err := serve(context.Background(), c, ln, log); err == nil {
		t.Fatal("missing scenario accepted")
	}
}
