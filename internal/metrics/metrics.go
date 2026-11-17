// Package metrics is a small Prometheus text-format registry: counters and
// histograms with a fixed label set. It exists so the proxy has no
// dependencies; it is not a general client library.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// FormatFloat is how bucket bounds are written in "le" labels. Rules that
// select a bucket must use the same function so the strings match.
func FormatFloat(v float64) string {
	if math.IsInf(v, +1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

type Registry struct {
	mu      sync.Mutex
	metrics []writer
}

type writer interface{ write(w io.Writer) }

func (r *Registry) add(m writer) {
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
}

// WriteText writes all metrics in the Prometheus text exposition format.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.Lock()
	ms := append([]writer(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

type family struct {
	name, help, typ string
	labels          []string
}

func (f family) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
}

func key(values []string) string { return strings.Join(values, "\xff") }

func labelPairs(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	n := 0
	put := func(k, v string) {
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escape(v))
		b.WriteByte('"')
		n++
	}
	for i, k := range names {
		put(k, values[i])
	}
	for i := 0; i+1 < len(extra); i += 2 {
		put(extra[i], extra[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

func escape(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// ---- counter ----

type CounterVec struct {
	family
	mu     sync.Mutex
	series map[string]*counter
}

type counter struct {
	values []string
	v      float64
}

func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{family: family{name, help, "counter", labels}, series: map[string]*counter{}}
	r.add(c)
	return c
}

// Add adds v (which must not be negative) to the series for these label values.
func (c *CounterVec) Add(v float64, values ...string) {
	if v < 0 || len(values) != len(c.labels) {
		return
	}
	k := key(values)
	c.mu.Lock()
	s, ok := c.series[k]
	if !ok {
		s = &counter{values: append([]string(nil), values...)}
		c.series[k] = s
	}
	s.v += v
	c.mu.Unlock()
}

func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Value returns the current value, for tests.
func (c *CounterVec) Value(values ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.series[key(values)]; ok {
		return s.v
	}
	return 0
}

func (c *CounterVec) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.header(w)
	for _, k := range sortedKeys(c.series) {
		s := c.series[k]
		fmt.Fprintf(w, "%s%s %s\n", c.name, labelPairs(c.labels, s.values), FormatFloat(s.v))
	}
}

// ---- histogram ----

type HistogramVec struct {
	family
	buckets []float64
	mu      sync.Mutex
	series  map[string]*histogram
}

type histogram struct {
	values []string
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

// Histogram creates a histogram. Buckets are sorted and de-duplicated.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	uniq := b[:0]
	for i, v := range b {
		if i == 0 || v != b[i-1] {
			uniq = append(uniq, v)
		}
	}
	h := &HistogramVec{family: family{name, help, "histogram", labels}, buckets: uniq, series: map[string]*histogram{}}
	r.add(h)
	return h
}

func (h *HistogramVec) Buckets() []float64 { return append([]float64(nil), h.buckets...) }

func (h *HistogramVec) Observe(v float64, values ...string) {
	if len(values) != len(h.labels) || math.IsNaN(v) {
		return
	}
	k := key(values)
	h.mu.Lock()
	s, ok := h.series[k]
	if !ok {
		s = &histogram{values: append([]string(nil), values...), counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	i := sort.SearchFloat64s(h.buckets, v) // first bucket with bound >= v
	if i < len(h.buckets) {
		s.counts[i]++
	}
	s.sum += v
	s.count++
	h.mu.Unlock()
}

// Count returns the number of observations, for tests.
func (h *HistogramVec) Count(values ...string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.series[key(values)]; ok {
		return s.count
	}
	return 0
}

func (h *HistogramVec) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.header(w)
	for _, k := range sortedKeys(h.series) {
		s := h.series[k]
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelPairs(h.labels, s.values, "le", FormatFloat(b)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelPairs(h.labels, s.values, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, labelPairs(h.labels, s.values), FormatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, labelPairs(h.labels, s.values), s.count)
	}
}

// ---- gauge (constant info metric) ----

type info struct {
	family
	values []string
}

// Info registers a gauge that is always 1, such as build information.
func (r *Registry) Info(name, help string, labels, values []string) {
	r.add(&info{family{name, help, "gauge", labels}, values})
}

func (i *info) write(w io.Writer) {
	i.header(w)
	fmt.Fprintf(w, "%s%s 1\n", i.name, labelPairs(i.labels, i.values))
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
