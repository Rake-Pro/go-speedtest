package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestExpositionExact(t *testing.T) {
	r := NewRegistry()
	r.Add(BytesServed, 123)
	r.Inc(TestsStarted + `{type="download"}`)
	r.Set(ActiveStreams, 3)

	want := strings.Join([]string{
		`# HELP gospeedtest_active_streams Currently active payload streams.`,
		`# TYPE gospeedtest_active_streams gauge`,
		`gospeedtest_active_streams 3`,
		`# HELP gospeedtest_bytes_received_total Total payload bytes received from clients.`,
		`# TYPE gospeedtest_bytes_received_total counter`,
		`gospeedtest_bytes_received_total 0`,
		`# HELP gospeedtest_bytes_served_total Total payload bytes served to clients.`,
		`# TYPE gospeedtest_bytes_served_total counter`,
		`gospeedtest_bytes_served_total 123`,
		`# HELP gospeedtest_tests_completed_total Payload requests completed, by type.`,
		`# TYPE gospeedtest_tests_completed_total counter`,
		`gospeedtest_tests_completed_total{type="download"} 0`,
		`gospeedtest_tests_completed_total{type="upload"} 0`,
		`gospeedtest_tests_completed_total{type="ws"} 0`,
		`# HELP gospeedtest_tests_started_total Payload requests started, by type.`,
		`# TYPE gospeedtest_tests_started_total counter`,
		`gospeedtest_tests_started_total{type="download"} 1`,
		`gospeedtest_tests_started_total{type="upload"} 0`,
		`gospeedtest_tests_started_total{type="ws"} 0`,
	}, "\n") + "\n"

	if got := r.expose(); got != want {
		t.Fatalf("exposition mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestHandler(t *testing.T) {
	r := NewRegistry()
	r.AddGauge(ActiveStreams, 2)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "gospeedtest_active_streams 2\n") {
		t.Fatalf("body missing gauge line:\n%s", rec.Body.String())
	}
}

func TestOps(t *testing.T) {
	tests := []struct {
		name string
		ops  func(r *Registry)
		want string // exact line expected in exposition
	}{
		{"Inc counter", func(r *Registry) { r.Inc(BytesServed) }, "gospeedtest_bytes_served_total 1"},
		{"Add counter", func(r *Registry) { r.Add(BytesReceived, 42) }, "gospeedtest_bytes_received_total 42"},
		{"Set gauge", func(r *Registry) { r.Set(ActiveStreams, 7) }, "gospeedtest_active_streams 7"},
		{"AddGauge up and down", func(r *Registry) {
			r.AddGauge(ActiveStreams, 5)
			r.AddGauge(ActiveStreams, -2)
		}, "gospeedtest_active_streams 3"},
		{"labelled counter", func(r *Registry) {
			r.Inc(TestsCompleted + `{type="upload"}`)
			r.Inc(TestsCompleted + `{type="upload"}`)
		}, `gospeedtest_tests_completed_total{type="upload"} 2`},
		{"lazy unknown series", func(r *Registry) { r.Inc("gospeedtest_custom_total") }, "gospeedtest_custom_total 1"},
		{"lazy unknown labelled gauge", func(r *Registry) {
			r.Set(`gospeedtest_custom_gauge{x="y"}`, -4)
		}, `gospeedtest_custom_gauge{x="y"} -4`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			tc.ops(r)
			got := r.expose()
			if !strings.Contains(got, tc.want+"\n") {
				t.Fatalf("exposition missing %q:\n%s", tc.want, got)
			}
		})
	}
}

func TestLazySeriesType(t *testing.T) {
	r := NewRegistry()
	r.Inc("gospeedtest_custom_total")
	r.AddGauge("gospeedtest_custom_gauge", 1)

	got := r.expose()
	if !strings.Contains(got, "# TYPE gospeedtest_custom_total counter\n") {
		t.Fatalf("missing counter TYPE line:\n%s", got)
	}
	if !strings.Contains(got, "# TYPE gospeedtest_custom_gauge gauge\n") {
		t.Fatalf("missing gauge TYPE line:\n%s", got)
	}
	if strings.Contains(got, "# HELP gospeedtest_custom_total") {
		t.Fatalf("unexpected HELP line for unknown family:\n%s", got)
	}
}

func TestConcurrentIncrements(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 1000

	r := NewRegistry()
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				r.Inc(BytesServed)
				r.Inc(TestsStarted + `{type="download"}`)
				r.AddGauge(ActiveStreams, 1)
				r.AddGauge(ActiveStreams, -1)
				r.Inc("gospeedtest_lazy_total") // exercise lazy creation under race
			}
		}()
	}
	// Concurrent scrapes while writers run.
	var scrape sync.WaitGroup
	for g := 0; g < 2; g++ {
		scrape.Add(1)
		go func() {
			defer scrape.Done()
			for i := 0; i < 50; i++ {
				_ = r.expose()
			}
		}()
	}
	wg.Wait()
	scrape.Wait()

	const total = goroutines * perGoroutine
	got := r.expose()
	for _, want := range []string{
		"gospeedtest_bytes_served_total 8000",
		`gospeedtest_tests_started_total{type="download"} 8000`,
		"gospeedtest_lazy_total 8000",
		"gospeedtest_active_streams 0",
	} {
		_ = total
		if !strings.Contains(got, want+"\n") {
			t.Fatalf("exposition missing %q:\n%s", want, got)
		}
	}
}

func TestPackageLevelHelpers(t *testing.T) {
	// Default is process-wide; only assert deltas via relative reads.
	before := Default.series(Default.counters, BytesServed).Load()
	Inc(BytesServed)
	Add(BytesServed, 9)
	after := Default.series(Default.counters, BytesServed).Load()
	if after-before != 10 {
		t.Fatalf("Default counter delta = %d, want 10", after-before)
	}
	Set(ActiveStreams, 5)
	AddGauge(ActiveStreams, 1)
	if v := Default.series(Default.gauges, ActiveStreams).Load(); v != 6 {
		t.Fatalf("Default gauge = %d, want 6", v)
	}
	if Handler() == nil {
		t.Fatal("Handler returned nil")
	}
}
