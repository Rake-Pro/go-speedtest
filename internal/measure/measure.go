// Package measure holds the shared client-side measurement math and the
// canonical Result schema used by the telemetry store, the HTTP API, the CLI
// and the browser frontend. Everything here is pure: no I/O, no globals.
//
// Methodology (librespeed-derived, see DESIGN.md):
//   - Time-based tests (default 15s) with a grace period after which the
//     byte/time counters RESET to compensate for TCP slow-start.
//   - throughput = bytes / time * 8 * overheadCompensationFactor.
//   - Ping metric is the running minimum RTT over the samples.
//   - Jitter is an asymmetric EWMA (spike vs decay).
package measure

import (
	"math"
	"time"
)

// Source enumerates where a Result originated.
const (
	SourceWeb = "web"
	SourceCLI = "cli"
	SourceAPI = "api"
)

// Defaults for the measurement methodology. These are the canonical numbers;
// the server may override the configurable ones via internal/config.
const (
	DefaultTestDuration    = 15 * time.Second
	DefaultGraceDownload   = 1500 * time.Millisecond
	DefaultGraceUpload     = 3000 * time.Millisecond
	DefaultOverheadFactor  = 1.06
	DefaultDownloadStreams = 6
	DefaultUploadStreams   = 3
	DefaultPingSamples     = 10
	MinConfigurableStreams = 3
	MaxConfigurableStreams = 12
)

// Result is the canonical schema for a completed speed test. It is the wire
// format for POST /api/v1/results, the stored telemetry row, and the CLI
// output. JSON tags are frozen; do not rename.
type Result struct {
	ID                 string  `json:"id"`
	Timestamp          string  `json:"timestamp"` // RFC3339
	ClientIP           string  `json:"client_ip,omitempty"`
	UserAgent          string  `json:"user_agent,omitempty"`
	DownloadMbps       float64 `json:"download_mbps"`
	UploadMbps         float64 `json:"upload_mbps"`
	PingMs             float64 `json:"ping_ms"`
	JitterMs           float64 `json:"jitter_ms"`
	DownloadBytes      int64   `json:"download_bytes"`
	UploadBytes        int64   `json:"upload_bytes"`
	DownloadDurationMs int64   `json:"download_duration_ms"`
	UploadDurationMs   int64   `json:"upload_duration_ms"`
	StreamsDownload    int     `json:"streams_download"`
	StreamsUpload      int     `json:"streams_upload"`
	OverheadFactor     float64 `json:"overhead_factor"`
	Source             string  `json:"source"` // web | cli | api
	ServerName         string  `json:"server_name,omitempty"`
}

// NewResult returns a Result stamped with the given timestamp (RFC3339, UTC)
// and source, ready to be filled in by the Set* helpers.
func NewResult(now time.Time, source string) Result {
	return Result{
		Timestamp: now.UTC().Format(time.RFC3339),
		Source:    source,
	}
}

// SetDownload fills the download fields of the Result from a completed meter.
func (r *Result) SetDownload(m *ThroughputMeter) {
	r.DownloadMbps = m.Mbps()
	r.DownloadBytes = m.Bytes()
	r.DownloadDurationMs = m.Elapsed().Milliseconds()
}

// SetUpload fills the upload fields of the Result from a completed meter.
func (r *Result) SetUpload(m *ThroughputMeter) {
	r.UploadMbps = m.Mbps()
	r.UploadBytes = m.Bytes()
	r.UploadDurationMs = m.Elapsed().Milliseconds()
}

// SetPing fills the ping/jitter fields of the Result from the ping stats.
func (r *Result) SetPing(p *PingStats) {
	r.PingMs = float64(p.Min()) / float64(time.Millisecond)
	r.JitterMs = p.JitterMs()
}

// Sample is a single point of transferred bytes at a moment in time, used to
// feed a ThroughputMeter.
type Sample struct {
	Bytes int64
	At    time.Time
}

// ThroughputMeter accumulates transferred bytes over time and computes
// throughput in Mbps. It implements the grace-period reset (to discard TCP
// slow-start) and applies the overhead compensation factor.
type ThroughputMeter struct {
	overhead float64
	grace    time.Duration

	started    time.Time
	graceEnded bool
	baseTime   time.Time
	bytes      int64
	elapsed    time.Duration
}

// NewThroughputMeter builds a meter with the given overhead compensation
// factor and grace period.
func NewThroughputMeter(overhead float64, grace time.Duration) *ThroughputMeter {
	return &ThroughputMeter{overhead: overhead, grace: grace}
}

// Start marks the beginning of the measurement window.
func (m *ThroughputMeter) Start(now time.Time) {
	m.started = now
	m.baseTime = now
	m.graceEnded = m.grace <= 0
	m.bytes = 0
	m.elapsed = 0
}

// Add records that n additional bytes have been transferred as of now,
// resetting the counters once the grace period has elapsed. The sample that
// crosses the grace boundary is discarded along with all earlier bytes, since
// it straddles the slow-start window.
func (m *ThroughputMeter) Add(n int64, now time.Time) {
	if m.started.IsZero() {
		m.Start(now)
	}
	if !m.graceEnded && now.Sub(m.started) >= m.grace {
		m.graceEnded = true
		m.baseTime = now
		m.bytes = 0
		m.elapsed = 0
		return
	}
	m.bytes += n
	if d := now.Sub(m.baseTime); d > m.elapsed {
		m.elapsed = d
	}
}

// AddSample records a Sample; equivalent to Add(s.Bytes, s.At).
func (m *ThroughputMeter) AddSample(s Sample) {
	m.Add(s.Bytes, s.At)
}

// Mbps returns the current throughput in megabits per second, with the
// overhead compensation factor applied.
func (m *ThroughputMeter) Mbps() float64 {
	secs := m.elapsed.Seconds()
	if secs <= 0 || m.bytes <= 0 {
		return 0
	}
	return float64(m.bytes) / secs * 8 / 1e6 * m.overhead
}

// Bytes returns the counted bytes in the active (post-grace) window.
func (m *ThroughputMeter) Bytes() int64 {
	return m.bytes
}

// Elapsed returns the duration of the active (post-grace) window.
func (m *ThroughputMeter) Elapsed() time.Duration {
	return m.elapsed
}

// JitterEWMA computes jitter as an asymmetric exponentially weighted moving
// average of successive inter-arrival / RTT deltas:
//
//	spike (inst > j): j = 0.3*j + 0.7*inst
//	decay (inst <= j): j = 0.8*j + 0.2*inst
type JitterEWMA struct {
	value float64
	init  bool
}

// Update folds a new instantaneous jitter sample (in the same unit, typically
// milliseconds) into the running EWMA and returns the new value. The first
// sample initializes the average.
func (j *JitterEWMA) Update(inst float64) float64 {
	if !j.init {
		j.value = inst
		j.init = true
		return j.value
	}
	if inst > j.value {
		j.value = 0.3*j.value + 0.7*inst
	} else {
		j.value = 0.8*j.value + 0.2*inst
	}
	return j.value
}

// Value returns the current jitter estimate.
func (j *JitterEWMA) Value() float64 {
	return j.value
}

// PingStats tracks ping samples and exposes the running-minimum RTT metric
// plus jitter computed via JitterEWMA.
type PingStats struct {
	min    time.Duration
	last   time.Duration
	count  int
	jitter JitterEWMA
}

// Add folds a new RTT sample into the stats. Jitter is fed with the absolute
// delta (in milliseconds) between successive RTTs, so it needs at least two
// samples to become non-zero.
func (p *PingStats) Add(rtt time.Duration) {
	if p.count == 0 || rtt < p.min {
		p.min = rtt
	}
	if p.count > 0 {
		inst := math.Abs(float64(rtt-p.last)) / float64(time.Millisecond)
		p.jitter.Update(inst)
	}
	p.last = rtt
	p.count++
}

// Min returns the running-minimum RTT (the reported ping metric).
func (p *PingStats) Min() time.Duration {
	return p.min
}

// JitterMs returns the current jitter estimate in milliseconds.
func (p *PingStats) JitterMs() float64 {
	return p.jitter.Value()
}

// Count returns the number of samples folded in so far.
func (p *PingStats) Count() int {
	return p.count
}
