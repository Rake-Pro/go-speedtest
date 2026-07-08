package measure

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestThroughputMeter(t *testing.T) {
	type add struct {
		bytes int64
		at    time.Duration // offset from t0
	}
	tests := []struct {
		name        string
		overhead    float64
		grace       time.Duration
		adds        []add
		wantBytes   int64
		wantElapsed time.Duration
		wantMbps    float64
	}{
		{
			name:     "no adds zero everything",
			overhead: 1.06,
			grace:    1500 * time.Millisecond,
			wantMbps: 0,
		},
		{
			name:        "zero grace counts from start",
			overhead:    1.0,
			grace:       0,
			adds:        []add{{1_000_000, time.Second}},
			wantBytes:   1_000_000,
			wantElapsed: time.Second,
			wantMbps:    8, // 1e6 B/s = 8e6 bit/s = 8 Mbps
		},
		{
			name:        "overhead factor applied",
			overhead:    1.06,
			grace:       0,
			adds:        []add{{1_000_000, time.Second}},
			wantBytes:   1_000_000,
			wantElapsed: time.Second,
			wantMbps:    8.48,
		},
		{
			name:      "zero duration guard",
			overhead:  1.06,
			grace:     0,
			adds:      []add{{500, 0}}, // add at the exact start instant
			wantBytes: 500,
			wantMbps:  0,
		},
		{
			name:     "pre-grace bytes counted live",
			overhead: 1.0,
			grace:    1500 * time.Millisecond,
			adds: []add{
				{100, 500 * time.Millisecond},
				{100, 1000 * time.Millisecond},
			},
			wantBytes:   200,
			wantElapsed: time.Second,
			wantMbps:    200 * 8 / 1e6, // over 1s
		},
		{
			name:     "reset at exact grace boundary discards straddling sample",
			overhead: 1.0,
			grace:    1500 * time.Millisecond,
			adds: []add{
				{100, 500 * time.Millisecond},
				{100, 1000 * time.Millisecond},
				{50, 1500 * time.Millisecond}, // boundary: reset, discarded
			},
			wantBytes:   0,
			wantElapsed: 0,
			wantMbps:    0,
		},
		{
			name:     "post-grace window measures from reset",
			overhead: 1.06,
			grace:    1500 * time.Millisecond,
			adds: []add{
				{999, 500 * time.Millisecond},        // slow-start, discarded
				{50, 1500 * time.Millisecond},        // boundary sample, discarded
				{1_000_000, 2500 * time.Millisecond}, // 1s after reset
				{1_000_000, 3500 * time.Millisecond}, // 2s after reset
			},
			wantBytes:   2_000_000,
			wantElapsed: 2 * time.Second,
			wantMbps:    2_000_000.0 / 2 * 8 / 1e6 * 1.06, // 8.48
		},
		{
			name:     "sample just past grace triggers reset",
			overhead: 1.0,
			grace:    time.Second,
			adds: []add{
				{100, 999 * time.Millisecond},  // still inside grace
				{100, 1001 * time.Millisecond}, // triggers reset
				{300, 2001 * time.Millisecond}, // 1s after reset
			},
			wantBytes:   300,
			wantElapsed: time.Second,
			wantMbps:    300 * 8 / 1e6,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewThroughputMeter(tc.overhead, tc.grace)
			m.Start(t0)
			for _, a := range tc.adds {
				m.Add(a.bytes, t0.Add(a.at))
			}
			if got := m.Bytes(); got != tc.wantBytes {
				t.Errorf("Bytes() = %d, want %d", got, tc.wantBytes)
			}
			if got := m.Elapsed(); got != tc.wantElapsed {
				t.Errorf("Elapsed() = %v, want %v", got, tc.wantElapsed)
			}
			if got := m.Mbps(); !almostEqual(got, tc.wantMbps) {
				t.Errorf("Mbps() = %v, want %v", got, tc.wantMbps)
			}
		})
	}
}

func TestThroughputMeterImplicitStart(t *testing.T) {
	m := NewThroughputMeter(1.0, 0)
	m.Add(100, t0) // no Start: first Add starts the window
	m.Add(100, t0.Add(time.Second))
	if got := m.Bytes(); got != 200 {
		t.Errorf("Bytes() = %d, want 200", got)
	}
	if got := m.Elapsed(); got != time.Second {
		t.Errorf("Elapsed() = %v, want 1s", got)
	}
}

func TestThroughputMeterAddSample(t *testing.T) {
	m := NewThroughputMeter(1.0, 0)
	m.Start(t0)
	m.AddSample(Sample{Bytes: 250, At: t0.Add(time.Second)})
	if got := m.Bytes(); got != 250 {
		t.Errorf("Bytes() = %d, want 250", got)
	}
	if got := m.Mbps(); !almostEqual(got, 250*8/1e6) {
		t.Errorf("Mbps() = %v, want %v", got, 250*8/1e6)
	}
}

func TestThroughputMeterRestart(t *testing.T) {
	m := NewThroughputMeter(1.0, 0)
	m.Start(t0)
	m.Add(1000, t0.Add(time.Second))
	m.Start(t0.Add(2 * time.Second))
	if m.Bytes() != 0 || m.Elapsed() != 0 || m.Mbps() != 0 {
		t.Errorf("after restart: Bytes=%d Elapsed=%v Mbps=%v, want zeros",
			m.Bytes(), m.Elapsed(), m.Mbps())
	}
}

func TestJitterEWMA(t *testing.T) {
	tests := []struct {
		name    string
		samples []float64
		want    []float64 // expected value after each Update
	}{
		{
			name:    "first sample initializes",
			samples: []float64{10},
			want:    []float64{10},
		},
		{
			name:    "spike then decay",
			samples: []float64{10, 20, 5},
			want: []float64{
				10,
				0.3*10 + 0.7*20,             // 17
				0.8*(0.3*10+0.7*20) + 0.2*5, // 14.6
			},
		},
		{
			name:    "equal sample decays",
			samples: []float64{10, 10},
			want:    []float64{10, 0.8*10 + 0.2*10}, // stays 10
		},
		{
			name:    "zero samples stay zero",
			samples: []float64{0, 0, 0},
			want:    []float64{0, 0, 0},
		},
		{
			name:    "repeated spikes converge upward",
			samples: []float64{1, 100, 100},
			want: []float64{
				1,
				0.3*1 + 0.7*100,               // 70.3
				0.3*(0.3*1+0.7*100) + 0.7*100, // 91.09
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var j JitterEWMA
			for i, s := range tc.samples {
				got := j.Update(s)
				if !almostEqual(got, tc.want[i]) {
					t.Fatalf("Update(%v) [step %d] = %v, want %v", s, i, got, tc.want[i])
				}
				if v := j.Value(); !almostEqual(v, got) {
					t.Fatalf("Value() = %v != Update return %v", v, got)
				}
			}
		})
	}
}

func TestPingStats(t *testing.T) {
	tests := []struct {
		name       string
		rtts       []time.Duration
		wantMin    time.Duration
		wantCount  int
		wantJitter float64
	}{
		{
			name:      "empty",
			wantMin:   0,
			wantCount: 0,
		},
		{
			name:       "single sample no jitter",
			rtts:       []time.Duration{10 * time.Millisecond},
			wantMin:    10 * time.Millisecond,
			wantCount:  1,
			wantJitter: 0,
		},
		{
			name: "running minimum",
			rtts: []time.Duration{
				10 * time.Millisecond,
				8 * time.Millisecond,
				12 * time.Millisecond,
			},
			wantMin:   8 * time.Millisecond,
			wantCount: 3,
			// deltas: |8-10|=2 (init), |12-8|=4 (spike): 0.3*2+0.7*4 = 3.4
			wantJitter: 3.4,
		},
		{
			name: "steady rtts give zero jitter",
			rtts: []time.Duration{
				5 * time.Millisecond,
				5 * time.Millisecond,
				5 * time.Millisecond,
			},
			wantMin:    5 * time.Millisecond,
			wantCount:  3,
			wantJitter: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p PingStats
			for _, rtt := range tc.rtts {
				p.Add(rtt)
			}
			if got := p.Min(); got != tc.wantMin {
				t.Errorf("Min() = %v, want %v", got, tc.wantMin)
			}
			if got := p.Count(); got != tc.wantCount {
				t.Errorf("Count() = %d, want %d", got, tc.wantCount)
			}
			if got := p.JitterMs(); !almostEqual(got, tc.wantJitter) {
				t.Errorf("JitterMs() = %v, want %v", got, tc.wantJitter)
			}
		})
	}
}

func TestResultAssembly(t *testing.T) {
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.FixedZone("X", 3600))
	r := NewResult(ts, SourceCLI)
	if r.Timestamp != "2026-07-08T11:00:00Z" {
		t.Errorf("Timestamp = %q, want UTC RFC3339", r.Timestamp)
	}
	if r.Source != SourceCLI {
		t.Errorf("Source = %q, want %q", r.Source, SourceCLI)
	}

	dl := NewThroughputMeter(1.06, 0)
	dl.Start(t0)
	dl.Add(2_000_000, t0.Add(2*time.Second))
	r.SetDownload(dl)
	if r.DownloadBytes != 2_000_000 || r.DownloadDurationMs != 2000 {
		t.Errorf("download bytes/duration = %d/%d, want 2000000/2000",
			r.DownloadBytes, r.DownloadDurationMs)
	}
	if !almostEqual(r.DownloadMbps, 2_000_000.0/2*8/1e6*1.06) {
		t.Errorf("DownloadMbps = %v", r.DownloadMbps)
	}

	ul := NewThroughputMeter(1.0, 0)
	ul.Start(t0)
	ul.Add(500_000, t0.Add(time.Second))
	r.SetUpload(ul)
	if r.UploadBytes != 500_000 || r.UploadDurationMs != 1000 {
		t.Errorf("upload bytes/duration = %d/%d, want 500000/1000",
			r.UploadBytes, r.UploadDurationMs)
	}
	if !almostEqual(r.UploadMbps, 4) {
		t.Errorf("UploadMbps = %v, want 4", r.UploadMbps)
	}

	var p PingStats
	p.Add(10 * time.Millisecond)
	p.Add(8 * time.Millisecond)
	p.Add(12 * time.Millisecond)
	r.SetPing(&p)
	if !almostEqual(r.PingMs, 8) {
		t.Errorf("PingMs = %v, want 8", r.PingMs)
	}
	if !almostEqual(r.JitterMs, 3.4) {
		t.Errorf("JitterMs = %v, want 3.4", r.JitterMs)
	}
}

func TestDefaults(t *testing.T) {
	// Guard the canonical methodology numbers from DESIGN.md.
	if DefaultTestDuration != 15*time.Second ||
		DefaultGraceDownload != 1500*time.Millisecond ||
		DefaultGraceUpload != 3000*time.Millisecond ||
		DefaultOverheadFactor != 1.06 ||
		DefaultDownloadStreams != 6 ||
		DefaultUploadStreams != 3 ||
		DefaultPingSamples != 10 ||
		MinConfigurableStreams != 3 ||
		MaxConfigurableStreams != 12 {
		t.Fatal("methodology defaults drifted from DESIGN.md")
	}
}
