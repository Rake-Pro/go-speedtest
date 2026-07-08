// Package metrics is a tiny, dependency-free Prometheus text-exposition
// registry backed by atomics. It intentionally does NOT use
// prometheus/client_golang. A process-wide Default registry backs the
// package-level helpers so handlers can record metrics without threading a
// registry through every constructor.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Metric names exposed at /metrics.
const (
	// Counters
	TestsStarted   = "gospeedtest_tests_started_total"   // labelled by type
	TestsCompleted = "gospeedtest_tests_completed_total" // labelled by type
	BytesServed    = "gospeedtest_bytes_served_total"
	BytesReceived  = "gospeedtest_bytes_received_total"

	// Gauges
	ActiveStreams = "gospeedtest_active_streams"
)

// helpText maps a metric family name to its # HELP line. Families without an
// entry are exposed with # TYPE only.
var helpText = map[string]string{
	TestsStarted:   "Payload requests started, by type.",
	TestsCompleted: "Payload requests completed, by type.",
	BytesServed:    "Total payload bytes served to clients.",
	BytesReceived:  "Total payload bytes received from clients.",
	ActiveStreams:  "Currently active payload streams.",
}

// Registry holds the atomic counters and gauges. The zero value is not ready;
// use NewRegistry. Series names may carry a Prometheus label suffix, e.g.
// TestsStarted + `{type="download"}`; the family name is everything before
// the '{'. Unknown series are created lazily on first use.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*atomic.Int64
	gauges   map[string]*atomic.Int64
}

// NewRegistry builds an empty registry with the known metric series
// pre-registered.
func NewRegistry() *Registry {
	r := &Registry{
		counters: map[string]*atomic.Int64{},
		gauges:   map[string]*atomic.Int64{},
	}
	for _, name := range []string{
		BytesServed,
		BytesReceived,
		TestsStarted + `{type="download"}`,
		TestsStarted + `{type="upload"}`,
		TestsStarted + `{type="ws"}`,
		TestsCompleted + `{type="download"}`,
		TestsCompleted + `{type="upload"}`,
		TestsCompleted + `{type="ws"}`,
	} {
		r.counters[name] = new(atomic.Int64)
	}
	r.gauges[ActiveStreams] = new(atomic.Int64)
	return r
}

// Default is the process-wide registry backing the package-level helpers.
var Default = NewRegistry()

// series returns the value cell for name in m, creating it if needed.
func (r *Registry) series(m map[string]*atomic.Int64, name string) *atomic.Int64 {
	r.mu.RLock()
	v := m[name]
	r.mu.RUnlock()
	if v != nil {
		return v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v = m[name]; v == nil {
		v = new(atomic.Int64)
		m[name] = v
	}
	return v
}

// Inc adds 1 to a counter series.
func (r *Registry) Inc(name string) { r.Add(name, 1) }

// Add adds delta to a counter series.
func (r *Registry) Add(name string, delta int64) {
	r.series(r.counters, name).Add(delta)
}

// Set sets a gauge series to v.
func (r *Registry) Set(name string, v int64) {
	r.series(r.gauges, name).Store(v)
}

// AddGauge adds delta to a gauge series (e.g. active-stream +1/-1).
func (r *Registry) AddGauge(name string, delta int64) {
	r.series(r.gauges, name).Add(delta)
}

// Handler returns an http.Handler that writes this registry in Prometheus text
// exposition format (version 0.0.4), sorted for deterministic output.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.expose()))
	})
}

type sample struct {
	name  string
	value int64
}

type family struct {
	kind    string
	samples []sample
}

// expose renders the registry in Prometheus text exposition format v0.0.4.
func (r *Registry) expose() string {
	fams := map[string]*family{}
	collect := func(kind string, m map[string]*atomic.Int64) {
		for name, v := range m {
			fam := name
			if i := strings.IndexByte(fam, '{'); i >= 0 {
				fam = fam[:i]
			}
			f := fams[fam]
			if f == nil {
				f = &family{kind: kind}
				fams[fam] = f
			}
			f.samples = append(f.samples, sample{name, v.Load()})
		}
	}
	r.mu.RLock()
	collect("counter", r.counters)
	collect("gauge", r.gauges)
	r.mu.RUnlock()

	names := make([]string, 0, len(fams))
	for name := range fams {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		f := fams[name]
		if help, ok := helpText[name]; ok {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, f.kind)
		sort.Slice(f.samples, func(i, j int) bool { return f.samples[i].name < f.samples[j].name })
		for _, s := range f.samples {
			fmt.Fprintf(&b, "%s %d\n", s.name, s.value)
		}
	}
	return b.String()
}

// Package-level helpers operating on Default.

// Inc adds 1 to a counter on the default registry.
func Inc(name string) { Default.Inc(name) }

// Add adds delta to a counter on the default registry.
func Add(name string, delta int64) { Default.Add(name, delta) }

// Set sets a gauge on the default registry.
func Set(name string, v int64) { Default.Set(name, v) }

// AddGauge adds delta to a gauge on the default registry.
func AddGauge(name string, delta int64) { Default.AddGauge(name, delta) }

// Handler returns the default registry's exposition handler.
func Handler() http.Handler { return Default.Handler() }
