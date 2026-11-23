// Command pixsim runs the Pix PSP simulator: a local sandbox that simulates a
// Pix PSP, the SPI and the DICT for offline integration testing.
//
// Every flag can also be set with an environment variable (PIXSIM_ + the flag
// name in upper case with dashes as underscores); flags win.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/api"
	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/notify"
	"github.com/victorcondackdev/pix-psp-simulator/internal/sim"
)

var version = "0.1.0"

type config struct {
	Addr             string
	ISPB             string
	BaseURL          string
	Latency          time.Duration
	ChargeExpiry     time.Duration
	MaxTransferCents int64
	MEDBlockWindow   time.Duration
	MEDMaxAccounts   int
	Scenarios        string
	Seed             int64
	WebhookAttempts  int
	WebhookBackoff   time.Duration
	WebhookTimeout   time.Duration
	LogLevel         string
	LogFormat        string
	HealthCheck      bool
	Version          bool
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet("pixsim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	env := func(name, def string) string {
		if v := getenv("PIXSIM_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))); v != "" {
			return v
		}
		return def
	}
	str := func(p *string, name, def, usage string) { fs.StringVar(p, name, env(name, def), usage) }
	var latency, expiry, backoff, timeout, maxCents, seed, attempts, medWindow, medAccounts string
	str(&c.Addr, "addr", ":8080", "listen address")
	str(&c.ISPB, "ispb", "99999999", "simulator ISPB (8 digits), used in EndToEndIds")
	str(&c.BaseURL, "base-url", "", "public base URL for dynamic QR locations (default http://localhost:<port>)")
	str(&latency, "settlement-latency", "500ms", "delay before a transfer reaches ACSC/RJCT")
	str(&expiry, "charge-expiry", "1h", "default expiration of dynamic charges")
	str(&maxCents, "max-transfer-cents", "0", "reject larger transfers with AM02 (0 = no limit)")
	str(&medWindow, "med-block-window", "72h", "how long an unanalysed MED cautionary block lasts")
	str(&medAccounts, "med-max-accounts", "5", "accounts MED 2.0 tracing may visit, including the payee")
	str(&c.Scenarios, "scenarios", "", "comma-separated scenario files or globs to load at startup")
	str(&seed, "seed", "0", "random seed for chaos probabilities and jitter (0 = time based)")
	str(&attempts, "webhook-max-attempts", "6", "webhook attempts before giving up")
	str(&backoff, "webhook-base-backoff", "1s", "delay before the first webhook retry; doubles each time")
	str(&timeout, "webhook-timeout", "5s", "per-attempt webhook HTTP timeout")
	str(&c.LogLevel, "log-level", "info", "debug, info, warn or error")
	str(&c.LogFormat, "log-format", "json", "json or text")
	fs.BoolVar(&c.HealthCheck, "healthcheck", false, "probe /healthz on -addr and exit (for container health checks)")
	fs.BoolVar(&c.Version, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	var err error
	parse := func(name, v string, f func(string) error) {
		if err == nil {
			if e := f(v); e != nil {
				err = fmt.Errorf("invalid -%s %q: %v", name, v, e)
			}
		}
	}
	dur := func(p *time.Duration) func(string) error {
		return func(v string) error { d, e := time.ParseDuration(v); *p = d; return e }
	}
	parse("settlement-latency", latency, dur(&c.Latency))
	parse("charge-expiry", expiry, dur(&c.ChargeExpiry))
	parse("webhook-base-backoff", backoff, dur(&c.WebhookBackoff))
	parse("webhook-timeout", timeout, dur(&c.WebhookTimeout))
	parse("max-transfer-cents", maxCents, func(v string) error { n, e := strconv.ParseInt(v, 10, 64); c.MaxTransferCents = n; return e })
	parse("seed", seed, func(v string) error { n, e := strconv.ParseInt(v, 10, 64); c.Seed = n; return e })
	parse("med-block-window", medWindow, dur(&c.MEDBlockWindow))
	parse("med-max-accounts", medAccounts, func(v string) error { n, e := strconv.Atoi(v); c.MEDMaxAccounts = n; return e })
	parse("webhook-max-attempts", attempts, func(v string) error { n, e := strconv.Atoi(v); c.WebhookAttempts = n; return e })
	if err != nil {
		return c, err
	}
	if len(c.ISPB) != 8 || strings.Trim(c.ISPB, "0123456789") != "" {
		return c, fmt.Errorf("invalid -ispb %q: must have 8 digits", c.ISPB)
	}
	return c, nil
}

func newLogger(c config, w io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return nil, fmt.Errorf("invalid -log-level %q", c.LogLevel)
	}
	opts := &slog.HandlerOptions{Level: level}
	switch c.LogFormat {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("invalid -log-format %q", c.LogFormat)
}

// build wires the simulator for a listener on addr.
func build(c config, addr string, log *slog.Logger) (*sim.Sim, *api.Server, error) {
	seed := c.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	clk := clock.NewVirtual()
	ch := chaos.New(seed)
	tel := sim.NewTelemetry()
	n := notify.New(clk, &http.Client{}, notify.Config{
		MaxAttempts: c.WebhookAttempts, BaseBackoff: c.WebhookBackoff, Timeout: c.WebhookTimeout,
		OnAttempt: tel.WebhookAttempt,
	}, ch.Int63n, log)
	base := c.BaseURL
	if base == "" {
		_, port, _ := net.SplitHostPort(addr)
		base = "http://localhost:" + port
	}
	s := sim.New(sim.Config{
		ISPB: c.ISPB, BaseURL: base, SettlementLatency: c.Latency,
		ChargeExpiry: c.ChargeExpiry, MaxTransferCents: c.MaxTransferCents,
		MEDBlockWindow: c.MEDBlockWindow, MEDMaxAccounts: c.MEDMaxAccounts,
	}, sim.Deps{Clock: clk, Chaos: ch, Notify: n, Log: log, Telemetry: tel})
	if c.Scenarios != "" {
		var patterns []string
		for _, p := range strings.Split(c.Scenarios, ",") {
			if p = strings.TrimSpace(p); p != "" {
				patterns = append(patterns, p)
			}
		}
		names, err := s.LoadScenarioFiles(patterns...)
		if err != nil {
			return nil, nil, err
		}
		log.Info("scenarios loaded", "names", names)
	}
	return s, api.New(s, clk, log), nil
}

// serve runs the HTTP server on ln until ctx is done.
func serve(ctx context.Context, c config, ln net.Listener, log *slog.Logger) error {
	s, srv, err := build(c, ln.Addr().String(), log)
	if err != nil {
		ln.Close()
		return err
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	log.Info("pixsim listening", "addr", ln.Addr().String(), "ispb", c.ISPB, "version", version,
		"base_url", s.Config().BaseURL, "settlement_latency", c.Latency.String())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = hs.Shutdown(shutdown)
	s.Notify.Close()
	log.Info("pixsim stopped")
	return err
}

func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	c, err := parseConfig(args, getenv, stderr)
	if err != nil {
		return err
	}
	switch {
	case c.Version:
		fmt.Fprintln(stdout, "pixsim", version)
		return nil
	case c.HealthCheck:
		return healthcheck(c.Addr)
	}
	log, err := newLogger(c, stdout)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", c.Addr)
	if err != nil {
		return err
	}
	return serve(ctx, c, ln, log)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "pixsim:", err)
		os.Exit(1)
	}
}
