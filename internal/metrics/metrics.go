// Package metrics is a small, dependency-free Prometheus instrumentation
// library: counters and histograms with labels, gauges computed at scrape time,
// and the text exposition format (version 0.0.4).
//
// It implements only what the simulator needs. Names and labels are validated
// at registration so a typo fails at start-up, not in a dashboard.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	nameRE  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// DefaultBuckets are latency buckets in seconds, from 5 ms to 30 s.
var DefaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

type collector interface {
	name() string
	write(w io.Writer)
}

// Registry holds collectors. The zero value is not usable; call NewRegistry.
type Registry struct {
	mu         sync.Mutex
	collectors map[string]collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{collectors: map[string]collector{}} }

func (r *Registry) register(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.collectors[c.name()]; dup {
		panic("metrics: duplicate metric " + c.name())
	}
	r.collectors[c.name()] = c
}

// Names returns the registered metric names, sorted.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.collectors))
	for n := range r.collectors {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Write renders every metric in the text exposition format, sorted by name.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	cs := make([]collector, 0, len(r.collectors))
	for _, c := range r.collectors {
		cs = append(cs, c)
	}
	r.mu.Unlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name() < cs[j].name() })
	for _, c := range cs {
		c.write(w)
	}
}

// Handler serves the registry at /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

func checkName(name string, labels []string) {
	if !nameRE.MatchString(name) {
		panic("metrics: invalid metric name " + name)
	}
	seen := map[string]bool{}
	for _, l := range labels {
		if !labelRE.MatchString(l) || strings.HasPrefix(l, "__") || l == "le" {
			panic("metrics: invalid label name " + l + " on " + name)
		}
		if seen[l] {
			panic("metrics: duplicate label " + l + " on " + name)
		}
		seen[l] = true
	}
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}

func escapeHelp(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(v)
}

func labelString(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escapeLabel(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+escapeLabel(extra[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func header(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
}

const sep = "\xff"

func key(values []string) string { return strings.Join(values, sep) }

func split(k string, n int) []string {
	if n == 0 {
		return nil
	}
	return strings.SplitN(k, sep, n)
}

// CounterVec is a family of counters partitioned by labels.
type CounterVec struct {
	n, help string
	labels  []string
	mu      sync.Mutex
	values  map[string]float64
}

// NewCounter registers a counter family. Counter names should end in _total.
func (r *Registry) NewCounter(name, help string, labels ...string) *CounterVec {
	checkName(name, labels)
	c := &CounterVec{n: name, help: help, labels: labels, values: map[string]float64{}}
	r.register(c)
	return c
}

func (c *CounterVec) name() string { return c.n }

func (c *CounterVec) check(values []string) {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", c.n, len(c.labels), len(values)))
	}
}

// Inc adds one to the counter with the given label values.
func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Add adds v (which must not be negative) to the counter.
func (c *CounterVec) Add(v float64, values ...string) {
	c.check(values)
	if v < 0 {
		panic("metrics: counters cannot decrease: " + c.n)
	}
	c.mu.Lock()
	c.values[key(values)] += v
	c.mu.Unlock()
}

// Value returns the current value (tests).
func (c *CounterVec) Value(values ...string) float64 {
	c.check(values)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key(values)]
}

func (c *CounterVec) write(w io.Writer) {
	header(w, c.n, c.help, "counter")
	c.mu.Lock()
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", c.n, labelString(c.labels, split(k, len(c.labels))), formatFloat(c.values[k]))
	}
	c.mu.Unlock()
}

type hist struct {
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

// HistogramVec is a family of histograms partitioned by labels.
type HistogramVec struct {
	n, help string
	labels  []string
	buckets []float64
	mu      sync.Mutex
	values  map[string]*hist
}

// NewHistogram registers a histogram family; nil buckets mean DefaultBuckets.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	checkName(name, labels)
	if buckets == nil {
		buckets = DefaultBuckets
	}
	b := append([]float64(nil), buckets...)
	if !sort.Float64sAreSorted(b) {
		panic("metrics: buckets must be sorted: " + name)
	}
	h := &HistogramVec{n: name, help: help, labels: labels, buckets: b, values: map[string]*hist{}}
	r.register(h)
	return h
}

func (h *HistogramVec) name() string { return h.n }

// Observe records one value.
func (h *HistogramVec) Observe(v float64, values ...string) {
	if len(values) != len(h.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", h.n, len(h.labels), len(values)))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := key(values)
	s := h.values[k]
	if s == nil {
		s = &hist{counts: make([]uint64, len(h.buckets))}
		h.values[k] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

// ObserveDuration records d in seconds.
func (h *HistogramVec) ObserveDuration(d time.Duration, values ...string) {
	h.Observe(d.Seconds(), values...)
}

// Count returns the number of observations (tests).
func (h *HistogramVec) Count(values ...string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.values[key(values)]; s != nil {
		return s.count
	}
	return 0
}

func (h *HistogramVec) write(w io.Writer) {
	header(w, h.n, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.values))
	for k := range h.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.values[k]
		vals := split(k, len(h.labels))
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.n, labelString(h.labels, vals, "le", formatFloat(b)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.n, labelString(h.labels, vals, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.n, labelString(h.labels, vals), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.n, labelString(h.labels, vals), s.count)
	}
}

// Sample is one gauge value with its label values.
type Sample struct {
	Labels []string
	Value  float64
}

// GaugeFunc is a gauge family whose samples are computed at scrape time.
type GaugeFunc struct {
	n, help string
	labels  []string
	fn      func() []Sample
}

// NewGaugeFunc registers a gauge family computed by fn on every scrape.
func (r *Registry) NewGaugeFunc(name, help string, labels []string, fn func() []Sample) *GaugeFunc {
	checkName(name, labels)
	g := &GaugeFunc{n: name, help: help, labels: labels, fn: fn}
	r.register(g)
	return g
}

func (g *GaugeFunc) name() string { return g.n }

func (g *GaugeFunc) write(w io.Writer) {
	header(w, g.n, g.help, "gauge")
	samples := g.fn()
	sort.Slice(samples, func(i, j int) bool { return key(samples[i].Labels) < key(samples[j].Labels) })
	for _, s := range samples {
		if len(s.Labels) != len(g.labels) {
			continue
		}
		fmt.Fprintf(w, "%s%s %s\n", g.n, labelString(g.labels, s.Labels), formatFloat(s.Value))
	}
}
