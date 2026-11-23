package sim

import (
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/internal/metrics"
)

// Telemetry holds the simulator's Prometheus metrics. Every metric name is
// prefixed pixsim_ so dashboards can tell the simulator from the PSP under
// test.
type Telemetry struct {
	Registry *metrics.Registry

	HTTPRequests *metrics.CounterVec
	HTTPDuration *metrics.HistogramVec

	Transfers         *metrics.CounterVec
	SettlementLatency *metrics.HistogramVec
	Refunds           *metrics.CounterVec
	DICTLookups       *metrics.CounterVec
	WebhookAttempts   *metrics.CounterVec
	WebhookDuration   *metrics.HistogramVec
	MEDTransitions    *metrics.CounterVec
	MEDTracedHops     *metrics.HistogramVec
	MEDBlockedCents   *metrics.CounterVec
	STASubmissions    *metrics.CounterVec
	Recurrences       *metrics.CounterVec
	AutomaticCharges  *metrics.CounterVec
	ProximityPayments *metrics.CounterVec
}

// NewTelemetry registers the simulator metrics in a new registry. Gauges
// that read simulator state are added by Sim.New.
func NewTelemetry() *Telemetry {
	r := metrics.NewRegistry()
	return &Telemetry{
		Registry:     r,
		HTTPRequests: r.NewCounter("pixsim_http_requests_total", "HTTP requests by route pattern, method and status code.", "route", "method", "code"),
		HTTPDuration: r.NewHistogram("pixsim_http_request_duration_seconds", "HTTP request latency by route pattern.", nil, "route"),
		Transfers: r.NewCounter("pixsim_transfers_total",
			"Transfers that reached a final status, by status (ACSC, RJCT), ISO reason and initiation form.", "status", "reason", "initiation"),
		SettlementLatency: r.NewHistogram("pixsim_settlement_latency_seconds",
			"Time from acceptance (ACSP) to the final status, in simulator time.", nil, "status"),
		Refunds:     r.NewCounter("pixsim_refunds_total", "Devoluções by status and return reason.", "status", "reason"),
		DICTLookups: r.NewCounter("pixsim_dict_lookups_total", "DICT lookups by result (found, not_found, rate_limited, invalid).", "result"),
		WebhookAttempts: r.NewCounter("pixsim_webhook_attempts_total",
			"Webhook HTTP attempts by event type and outcome (delivered, retry, failed).", "event_type", "outcome"),
		WebhookDuration: r.NewHistogram("pixsim_webhook_attempt_duration_seconds", "Webhook HTTP attempt latency (wall clock).", nil, "event_type"),
		MEDTransitions:  r.NewCounter("pixsim_med_transitions_total", "MED infraction transitions by event and resulting status.", "event", "status"),
		MEDTracedHops: r.NewHistogram("pixsim_med_traced_hops", "Accounts reached by MED 2.0 funds tracing per blocked infraction.",
			[]float64{0, 1, 2, 3, 4, 5}),
		MEDBlockedCents: r.NewCounter("pixsim_med_blocked_cents_total", "Cents placed under cautionary block, by tracing depth (0 = the original payee).", "depth"),
		STASubmissions:  r.NewCounter("pixsim_sta_submissions_total", "Regulatory documents received through the STA, by document code and status.", "document", "status"),
		Recurrences:     r.NewCounter("pixsim_automatico_recurrences_total", "Pix Automático recurrence transitions by resulting status.", "status"),
		AutomaticCharges: r.NewCounter("pixsim_automatico_charges_total",
			"Pix Automático charge attempts by outcome (settled, retry, failed, cancelled).", "outcome"),
		ProximityPayments: r.NewCounter("pixsim_proximity_payments_total", "Pix por aproximação requests by result (accepted or the refusal code).", "result"),
	}
}

// WebhookAttempt is a notify.Config.OnAttempt hook.
func (t *Telemetry) WebhookAttempt(eventType, outcome string, d time.Duration) {
	t.WebhookAttempts.Inc(eventType, outcome)
	t.WebhookDuration.ObserveDuration(d, eventType)
}

func (s *Sim) registerGauges() {
	r := s.Telemetry.Registry
	r.NewGaugeFunc("pixsim_ledger_balance_cents", "Balance of the simulator's system accounts.", []string{"account"}, func() []metrics.Sample {
		var out []metrics.Sample
		for _, id := range []string{ledger.Funding, ledger.Settlement, ledger.MEDHold} {
			a, _ := s.Ledger.Get(id)
			out = append(out, metrics.Sample{Labels: []string{id}, Value: float64(a.Balance)})
		}
		return out
	})
	r.NewGaugeFunc("pixsim_ledger_total_cents", "Sum of every balance; anything but 0 is a bug.", nil, func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(s.Ledger.Total())}}
	})
	r.NewGaugeFunc("pixsim_transfers_in_flight", "Transfers accepted (ACSP) and not yet settled or rejected.", nil, func() []metrics.Sample {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, t := range s.transfers {
			if t.Status == "ACSP" {
				n++
			}
		}
		return []metrics.Sample{{Value: float64(n)}}
	})
	r.NewGaugeFunc("pixsim_med_infractions", "MED infractions by current status.", []string{"status"}, func() []metrics.Sample {
		s.mu.Lock()
		defer s.mu.Unlock()
		counts := map[InfractionStatus]int{}
		for _, i := range s.infractions {
			counts[i.Status]++
		}
		var out []metrics.Sample
		for st, n := range counts {
			out = append(out, metrics.Sample{Labels: []string{string(st)}, Value: float64(n)})
		}
		return out
	})
	r.NewGaugeFunc("pixsim_accounts", "Customer accounts.", nil, func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(len(s.Ledger.List(false)))}}
	})
	r.NewGaugeFunc("pixsim_dict_keys", "Registered DICT keys.", nil, func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(len(s.DICT.Keys("")))}}
	})
	r.NewGaugeFunc("pixsim_webhook_pending_deliveries", "Webhook deliveries waiting for their next attempt.", nil, func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(len(s.Notify.Pending()))}}
	})
}
