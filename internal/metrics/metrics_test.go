package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("x_total", "A counter.\nTwo lines.", "code")
	c.Inc("200")
	c.Add(2, `a"b\`)
	h := r.NewHistogram("lat_seconds", "Latency.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "/a")
	h.ObserveDuration(500*time.Millisecond, "/a")
	h.Observe(5, "/a")
	r.NewGaugeFunc("g", "Gauge.", nil, func() []Sample { return []Sample{{Value: 3}, {Labels: []string{"extra"}, Value: 9}} })
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	got := rec.Body.String()
	for _, want := range []string{
		"# HELP x_total A counter.\\nTwo lines.\n# TYPE x_total counter\n",
		`x_total{code="200"} 1`,
		`x_total{code="a\"b\\"} 2`,
		`lat_seconds_bucket{route="/a",le="0.1"} 1`,
		`lat_seconds_bucket{route="/a",le="1"} 2`,
		`lat_seconds_bucket{route="/a",le="+Inf"} 3`,
		`lat_seconds_sum{route="/a"} 5.55`,
		`lat_seconds_count{route="/a"} 3`,
		"# TYPE g gauge\ng 3\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "extra") {
		t.Error("sample with wrong label count was written")
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Error(rec.Header())
	}
	if c.Value("200") != 1 || h.Count("/a") != 3 || h.Count("/b") != 0 {
		t.Error("accessors")
	}
	if n := r.Names(); len(n) != 3 || n[0] != "g" {
		t.Error(n)
	}
}

func TestPanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("ok_total", "")
	for name, f := range map[string]func(){
		"duplicate":   func() { r.NewCounter("ok_total", "") },
		"bad name":    func() { r.NewCounter("1bad", "") },
		"bad label":   func() { r.NewCounter("y_total", "", "le") },
		"dup label":   func() { r.NewCounter("z_total", "", "a", "a") },
		"arity":       func() { r.NewCounter("w_total", "", "a").Inc() },
		"negative":    func() { r.NewCounter("v_total", "").Add(-1) },
		"unsorted":    func() { r.NewHistogram("h", "", []float64{2, 1}) },
		"hist arity":  func() { r.NewHistogram("h2", "", nil, "a").Observe(1) },
		"reserved":    func() { r.NewCounter("u_total", "", "__x") },
		"counter val": func() { r.NewCounter("t_total", "", "a").Value() },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

func TestFormatFloat(t *testing.T) {
	for in, want := range map[float64]string{1: "1", 0.25: "0.25", 1e21: "1e+21"} {
		if got := formatFloat(in); got != want {
			t.Errorf("%v: %s", in, got)
		}
	}
}
