package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/websocket"

	"github.com/Rake-Pro/go-speedtest/internal/measure"
)

// testBackend is a minimal go-speedtest server per the DESIGN.md endpoint map.
type testBackend struct {
	info serverInfo

	token string // required bearer for /api/v1/results when non-empty

	wsPings  atomic.Int64 // ws echo messages served
	pings    atomic.Int64 // HTTP ping hits
	uploads  atomic.Int64 // bytes drained
	pushed   atomic.Int64 // results ingested
	lastPush atomic.Pointer[measure.Result]

	dlReqs   atomic.Int64 // total download requests received
	ulReqs   atomic.Int64 // total upload requests received
	dl429    atomic.Int64 // 429 the first N download requests, then serve
	ul429    atomic.Int64 // 429 (without draining) the first N upload requests
	dlAll429 atomic.Bool  // 429 every download request
}

func newTestBackend() *testBackend {
	b := &testBackend{}
	b.info.ServerName = "test-server"
	b.info.TestDurationMs = 200
	b.info.GraceDownloadMs = 40
	b.info.GraceUploadMs = 40
	b.info.DownloadStreams = 3
	b.info.UploadStreams = 3
	b.info.OverheadFactor = 1.06
	b.info.ChunkSizeBytes = 64 << 10
	b.info.UploadBlobBytes = 256 << 10
	b.info.PingSamples = 3
	b.info.DownloadChunks = 2
	b.info.Endpoints.Download = "/api/v1/download"
	b.info.Endpoints.Upload = "/api/v1/upload"
	b.info.Endpoints.Ping = "/api/v1/ping"
	b.info.Endpoints.IP = "/api/v1/ip"
	b.info.Endpoints.WS = "/api/v1/ws"
	b.info.Endpoints.Results = "/api/v1/results"
	return b
}

func (b *testBackend) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.info)
	})
	mux.HandleFunc("GET /api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		n := b.dlReqs.Add(1)
		if b.dlAll429.Load() || n <= b.dl429.Load() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		chunks, _ := strconv.Atoi(r.URL.Query().Get("chunks"))
		if chunks <= 0 {
			chunks = 4
		}
		chunk := make([]byte, b.info.ChunkSizeBytes)
		w.Header().Set("Content-Type", "application/octet-stream")
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	mux.HandleFunc("POST /api/v1/upload", func(w http.ResponseWriter, r *http.Request) {
		if req := b.ulReqs.Add(1); req <= b.ul429.Load() {
			// 429 WITHOUT draining the request body: the pathology that made the
			// client meter buffered writes and fabricate multi-Gbps uploads.
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		n, _ := io.Copy(io.Discard, r.Body)
		b.uploads.Add(n)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		b.pings.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /api/v1/ws", websocket.Handler(func(ws *websocket.Conn) {
		var msg string
		for {
			if err := websocket.Message.Receive(ws, &msg); err != nil {
				return
			}
			b.wsPings.Add(1)
			if err := websocket.Message.Send(ws, msg); err != nil {
				return
			}
		}
	}))
	mux.HandleFunc("POST /api/v1/results", func(w http.ResponseWriter, r *http.Request) {
		if b.token != "" && r.Header.Get("Authorization") != "Bearer "+b.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var res measure.Result
		if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.lastPush.Store(&res)
		id := fmt.Sprintf("r%d", b.pushed.Add(1))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q}`, id)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runCLI executes run() and returns stdout, stderr and the error.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func TestRunJSONFullTest(t *testing.T) {
	b := newTestBackend()
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res measure.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not valid Result JSON: %v\n%s", err, stdout)
	}
	if res.Source != measure.SourceCLI {
		t.Errorf("Source = %q, want %q", res.Source, measure.SourceCLI)
	}
	if res.ServerName != "test-server" {
		t.Errorf("ServerName = %q, want test-server", res.ServerName)
	}
	if res.DownloadMbps <= 0 || res.DownloadBytes <= 0 {
		t.Errorf("download not measured: mbps=%v bytes=%d", res.DownloadMbps, res.DownloadBytes)
	}
	if res.UploadMbps <= 0 || res.UploadBytes <= 0 {
		t.Errorf("upload not measured: mbps=%v bytes=%d", res.UploadMbps, res.UploadBytes)
	}
	if res.PingMs <= 0 {
		t.Errorf("ping not measured: %v", res.PingMs)
	}
	if res.OverheadFactor != 1.06 {
		t.Errorf("OverheadFactor = %v, want 1.06 from server info", res.OverheadFactor)
	}
	// Streams adopted from server info (3/3).
	if res.StreamsDownload != 3 || res.StreamsUpload != 3 {
		t.Errorf("streams = %d/%d, want 3/3 from server info", res.StreamsDownload, res.StreamsUpload)
	}
	if b.uploads.Load() == 0 {
		t.Error("server drained no upload bytes")
	}
}

func TestWebSocketPingPreferred(t *testing.T) {
	b := newTestBackend()
	b.info.WebSocketPing = true
	srv := b.start(t)

	_, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-download", "-no-upload")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := b.wsPings.Load(); got != int64(b.info.PingSamples) {
		t.Errorf("ws echo count = %d, want %d", got, b.info.PingSamples)
	}
	if b.pings.Load() != 0 {
		t.Errorf("HTTP ping hit %d times, want 0 (ws preferred)", b.pings.Load())
	}
}

func TestHTTPPingFallback(t *testing.T) {
	b := newTestBackend()
	b.info.WebSocketPing = true
	b.info.Endpoints.WS = "/nonexistent-ws" // ws dial fails -> HTTP fallback
	srv := b.start(t)

	_, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-download", "-no-upload")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// warmup + samples
	if got := b.pings.Load(); got != int64(b.info.PingSamples)+1 {
		t.Errorf("HTTP ping hits = %d, want %d", got, b.info.PingSamples+1)
	}
}

func TestPhaseToggles(t *testing.T) {
	b := newTestBackend()
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-upload", "-no-download")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res measure.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.DownloadMbps != 0 || res.UploadMbps != 0 || res.DownloadBytes != 0 {
		t.Errorf("skipped phases produced data: %+v", res)
	}
	if res.PingMs <= 0 {
		t.Error("ping phase should still run")
	}
	if b.uploads.Load() != 0 {
		t.Error("upload endpoint hit despite -no-upload")
	}

	if _, _, err := runCLI(t, "-server", srv.URL, "-no-ping", "-no-download", "-no-upload"); err == nil {
		t.Error("all phases disabled should be an error")
	}
}

func TestPushWithToken(t *testing.T) {
	b := newTestBackend()
	b.token = "sekrit"
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-download",
		"-push", "-token", "sekrit")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res measure.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.ID != "r1" {
		t.Errorf("ID = %q, want r1 from push response", res.ID)
	}
	pushed := b.lastPush.Load()
	if pushed == nil {
		t.Fatal("server received no result")
	}
	if pushed.Source != measure.SourceCLI || pushed.UploadMbps <= 0 {
		t.Errorf("pushed result wrong: %+v", pushed)
	}

	// Wrong token -> push fails -> non-nil error (non-zero exit).
	if _, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-download", "-no-upload",
		"-push", "-token", "wrong"); err == nil {
		t.Error("push with wrong token should fail")
	}
}

func TestFlagOverridesInfo(t *testing.T) {
	b := newTestBackend()
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-upload", "-no-ping",
		"-streams-download", "4", "-duration", "150ms")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res measure.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.StreamsDownload != 4 {
		t.Errorf("StreamsDownload = %d, want flag override 4", res.StreamsDownload)
	}
}

func TestCSVOutput(t *testing.T) {
	b := newTestBackend()
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-csv", "-no-upload", "-no-download")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("CSV output has %d lines, want header + row:\n%s", len(lines), stdout)
	}
	if !strings.HasPrefix(lines[0], "id,timestamp,server_name,source,ping_ms") {
		t.Errorf("unexpected CSV header: %s", lines[0])
	}
	if !strings.Contains(lines[1], "test-server,cli,") {
		t.Errorf("unexpected CSV row: %s", lines[1])
	}
}

func TestPrometheusOutput(t *testing.T) {
	b := newTestBackend()
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-prometheus", "-no-upload")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{
		"# TYPE speedtest_ping_ms gauge",
		"# TYPE speedtest_download_mbps gauge",
		`speedtest_download_mbps{server_name="test-server"} `,
		"speedtest_last_run_timestamp_seconds",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("prometheus output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "speedtest_upload_mbps") {
		t.Error("prometheus output contains upload metrics despite -no-upload")
	}
}

func TestHumanOutput(t *testing.T) {
	b := newTestBackend()
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"Server:", "Ping:", "Download:", "Upload:", "Mbps"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output missing %q:\n%s", want, stdout)
		}
	}
}

func TestFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing server", []string{"-json"}},
		{"invalid server URL", []string{"-server", "not a url"}},
		{"exclusive formats", []string{"-server", "http://x", "-json", "-csv"}},
		{"streams too low", []string{"-server", "http://x", "-streams-download", "2"}},
		{"streams too high", []string{"-server", "http://x", "-streams-upload", "13"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := runCLI(t, tc.args...); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestServerUnreachable(t *testing.T) {
	if _, _, err := runCLI(t, "-server", "http://127.0.0.1:1", "-json", "-timeout", "500ms"); err == nil {
		t.Error("unreachable server should fail")
	}
}

func TestInfoFallbackToDefaults(t *testing.T) {
	// Server whose info handler is not implemented (501): CLI proceeds on
	// built-in defaults and default endpoint paths.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/info", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
	mux.HandleFunc("GET /api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stdout, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-download", "-no-upload")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res measure.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.OverheadFactor != measure.DefaultOverheadFactor {
		t.Errorf("OverheadFactor = %v, want default %v", res.OverheadFactor, measure.DefaultOverheadFactor)
	}
	if res.PingMs <= 0 {
		t.Error("ping did not run against default endpoint")
	}
}

func TestDownloadFailure(t *testing.T) {
	b := newTestBackend()
	srv := b.start(t)
	// Break the download endpoint.
	b.info.Endpoints.Download = "/broken"

	if _, _, err := runCLI(t, "-server", srv.URL, "-json", "-no-upload", "-no-ping",
		"-duration", "150ms"); err == nil {
		t.Error("broken download endpoint should fail the test")
	}
}

// TestDownloadStreamRetry: a 429 storm hits every stream's first request, then
// the server serves normally. The failing streams must warn, back off and retry
// so the phase still yields a full-window, sane result. Pre-fix, every stream
// died on its first 429 and the phase erred with a collapsed window.
func TestDownloadStreamRetry(t *testing.T) {
	b := newTestBackend()
	b.dl429.Store(int64(b.info.DownloadStreams)) // 429 each stream's first hit
	srv := b.start(t)

	stdout, stderr, err := runCLI(t, "-server", srv.URL, "-json",
		"-no-upload", "-no-ping", "-duration", "2s")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res measure.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not valid Result JSON: %v\n%s", err, stdout)
	}
	if res.DownloadBytes <= 0 || res.DownloadMbps <= 0 {
		t.Errorf("download not measured after retry: mbps=%v bytes=%d", res.DownloadMbps, res.DownloadBytes)
	}
	// Full window (not the pre-fix ~few-ms collapse). One 500ms backoff is
	// tolerated inside the 2s window.
	if res.DownloadDurationMs < 1000 {
		t.Errorf("download window collapsed: %d ms, want >= 1000", res.DownloadDurationMs)
	}
	// Sane throughput, never a fabricated multi-Gbps artifact.
	if res.DownloadMbps > 100_000 {
		t.Errorf("absurd download throughput: %v Mbps", res.DownloadMbps)
	}
	if !strings.Contains(stderr, "download stream failed, retrying") {
		t.Errorf("expected a stream-retry warning on stderr, got:\n%s", stderr)
	}
}

// TestDownloadAllStreamsFail: every download request 429s forever. The phase
// must error loudly with no result emitted, and streams must have retried
// (more requests than streams) rather than dying on the first failure.
func TestDownloadAllStreamsFail(t *testing.T) {
	b := newTestBackend()
	b.dlAll429.Store(true)
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-json",
		"-no-upload", "-no-ping", "-duration", "1500ms")
	if err == nil {
		t.Fatal("all download streams failing must error the phase")
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("no result should be emitted on total failure, got:\n%s", stdout)
	}
	if got, streams := b.dlReqs.Load(), int64(b.info.DownloadStreams); got <= streams {
		t.Errorf("download requests = %d, want > %d (streams must retry, not die once)", got, streams)
	}
}

// TestUploadNoDrainNoFabrication: the upload endpoint 429s the first request of
// every stream WITHOUT draining the body, then serves normally. Pre-fix, the
// buffered bytes were metered and the window collapsed to a few milliseconds,
// fabricating a multi-Gbps upload. Post-fix, rejected bytes never count and the
// window is the real post-grace span.
func TestUploadNoDrainNoFabrication(t *testing.T) {
	b := newTestBackend()
	b.info.UploadBlobBytes = 64 << 10 // small blob buffers fully -> pre-fix metered it
	b.ul429.Store(int64(b.info.UploadStreams))
	srv := b.start(t)

	stdout, _, err := runCLI(t, "-server", srv.URL, "-json",
		"-no-download", "-no-ping", "-duration", "2s")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res measure.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not valid Result JSON: %v\n%s", err, stdout)
	}
	// The 429-without-drain requests must not fabricate a sub-second window.
	if res.UploadDurationMs < 800 {
		t.Errorf("upload window collapsed to a short sample: %d ms, want >= 800", res.UploadDurationMs)
	}
	if res.UploadBytes <= 0 || res.UploadMbps <= 0 {
		t.Errorf("upload not measured after retry: mbps=%v bytes=%d", res.UploadMbps, res.UploadBytes)
	}
	if b.uploads.Load() <= 0 {
		t.Error("server drained no upload bytes; result not from real deliveries")
	}
}
