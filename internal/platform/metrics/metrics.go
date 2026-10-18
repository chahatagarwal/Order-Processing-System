// Package metrics is a tiny, dependency-free Prometheus-compatible registry.
//
// WHY HAND-ROLLED INSTEAD OF prometheus/client_golang:
// The official client is excellent and is what you would use at work. We
// wrote ~150 lines instead because the goal of this codebase is that you can
// read and explain EVERY line. The exposition format is simply text:
//
//	# TYPE orders_created_total counter
//	orders_created_total{status="PENDING"} 42
//
// That is the entire protocol. Prometheus scrapes GET /metrics and parses it.
//
// THE THREE METRIC TYPES AND WHEN TO USE EACH:
//   - Counter:   only goes up. "how many orders created" -> rate() over time.
//   - Gauge:     goes up and down. "orders currently PENDING".
//   - Histogram: distribution of values. "how long did requests take" -> p99.
//
// A common interview trap: "why not just average the latency?" Because an
// average hides the tail. If 99 requests take 1ms and one takes 10s, the
// average is 100ms — which no user experienced. Histograms give you p99.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry holds all metrics for one process.
//
// CONCURRENCY NOTE: an HTTP server handles requests on many goroutines at
// once, so every metric mutation races. We guard with a mutex. (A production
// client uses atomics and sharding to avoid contention; for our volumes a
// single mutex is simpler and provably correct — the right trade-off here.)
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*counter
	gauges     map[string]*gauge
	histograms map[string]*histogram
	order      []string // preserves registration order for stable /metrics output
}

func NewRegistry() *Registry {
	return &Registry{
		counters:   map[string]*counter{},
		gauges:     map[string]*gauge{},
		histograms: map[string]*histogram{},
	}
}

type counter struct {
	help   string
	values map[string]float64 // labelKey -> value
}

type gauge struct {
	help   string
	values map[string]float64
}

type histogram struct {
	help    string
	buckets []float64
	// series is keyed by label set; each holds cumulative bucket counts.
	series map[string]*histSeries
}

type histSeries struct {
	counts []uint64 // one slot per bucket, plus implicit +Inf
	sum    float64
	total  uint64
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// IncCounter adds 1. labels are flat key/value pairs: "status", "PENDING".
func (r *Registry) IncCounter(name, help string, labels ...string) {
	r.AddCounter(name, help, 1, labels...)
}

func (r *Registry) AddCounter(name, help string, delta float64, labels ...string) {
	key := encodeLabels(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok {
		c = &counter{help: help, values: map[string]float64{}}
		r.counters[name] = c
		r.order = append(r.order, "counter:"+name)
	}
	c.values[key] += delta
}

func (r *Registry) SetGauge(name, help string, value float64, labels ...string) {
	key := encodeLabels(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.gauges[name]
	if !ok {
		g = &gauge{help: help, values: map[string]float64{}}
		r.gauges[name] = g
		r.order = append(r.order, "gauge:"+name)
	}
	g.values[key] = value
}

// DefaultLatencyBuckets are in SECONDS and chosen around real SLO boundaries:
// 5ms (cache hit), 25ms (simple DB read), 100ms (write), 1s (saga), 5s (bad).
var DefaultLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

func (r *Registry) ObserveHistogram(name, help string, value float64, labels ...string) {
	r.ObserveHistogramWithBuckets(name, help, DefaultLatencyBuckets, value, labels...)
}

func (r *Registry) ObserveHistogramWithBuckets(name, help string, buckets []float64, value float64, labels ...string) {
	key := encodeLabels(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.histograms[name]
	if !ok {
		h = &histogram{help: help, buckets: buckets, series: map[string]*histSeries{}}
		r.histograms[name] = h
		r.order = append(r.order, "histogram:"+name)
	}
	s, ok := h.series[key]
	if !ok {
		s = &histSeries{counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	s.sum += value
	s.total++
	for i, b := range h.buckets {
		if value <= b {
			s.counts[i]++
		}
	}
}

// Render produces the Prometheus text exposition format.
func (r *Registry) Render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var sb strings.Builder
	for _, entry := range r.order {
		kind, name, _ := strings.Cut(entry, ":")
		switch kind {
		case "counter":
			c := r.counters[name]
			writeHeader(&sb, name, c.help, "counter")
			for _, lk := range sortedKeys(c.values) {
				fmt.Fprintf(&sb, "%s%s %g\n", name, decodeLabels(lk), c.values[lk])
			}
		case "gauge":
			g := r.gauges[name]
			writeHeader(&sb, name, g.help, "gauge")
			for _, lk := range sortedKeys(g.values) {
				fmt.Fprintf(&sb, "%s%s %g\n", name, decodeLabels(lk), g.values[lk])
			}
		case "histogram":
			h := r.histograms[name]
			writeHeader(&sb, name, h.help, "histogram")
			keys := make([]string, 0, len(h.series))
			for k := range h.series {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, lk := range keys {
				s := h.series[lk]
				for i, b := range h.buckets {
					fmt.Fprintf(&sb, "%s_bucket%s %d\n", name, decodeLabelsWith(lk, "le", formatFloat(b)), s.counts[i])
				}
				fmt.Fprintf(&sb, "%s_bucket%s %d\n", name, decodeLabelsWith(lk, "le", "+Inf"), s.total)
				fmt.Fprintf(&sb, "%s_sum%s %g\n", name, decodeLabels(lk), s.sum)
				fmt.Fprintf(&sb, "%s_count%s %d\n", name, decodeLabels(lk), s.total)
			}
		}
	}
	return sb.String()
}

func writeHeader(sb *strings.Builder, name, help, typ string) {
	if help != "" {
		fmt.Fprintf(sb, "# HELP %s %s\n", name, help)
	}
	fmt.Fprintf(sb, "# TYPE %s %s\n", name, typ)
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatFloat(f float64) string { return fmt.Sprintf("%g", f) }

// encodeLabels turns ["status","PENDING","kind","web"] into a stable map key.
// Odd-length input is tolerated (the stray value is dropped) rather than
// panicking: a metrics bug must never take down a request path.
func encodeLabels(labels []string) string {
	if len(labels) < 2 {
		return ""
	}
	pairs := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		pairs = append(pairs, labels[i]+"\x00"+escape(labels[i+1]))
	}
	sort.Strings(pairs) // sorted => same labels in any order produce one series
	return strings.Join(pairs, "\x01")
}

func decodeLabels(key string) string { return decodeLabelsWith(key, "", "") }

func decodeLabelsWith(key, extraK, extraV string) string {
	var parts []string
	if key != "" {
		for _, p := range strings.Split(key, "\x01") {
			k, v, _ := strings.Cut(p, "\x00")
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
	}
	if extraK != "" {
		parts = append(parts, fmt.Sprintf("%s=%q", extraK, extraV))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escape prevents a label value containing a quote or newline from corrupting
// the whole exposition payload (which would break the scrape silently).
func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
