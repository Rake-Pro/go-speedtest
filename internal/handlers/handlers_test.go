package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Rake-Pro/go-speedtest/internal/config"
	"github.com/Rake-Pro/go-speedtest/internal/payload"
	"github.com/Rake-Pro/go-speedtest/internal/ratelimit"
	"github.com/Rake-Pro/go-speedtest/internal/telemetry"
	"github.com/Rake-Pro/go-speedtest/internal/webui"

	"github.com/rs/zerolog"
	"golang.org/x/net/websocket"
)

func newTestHandler(cfg *config.Config) http.Handler {
	if err := payload.Init(); err != nil {
		panic(err)
	}
	return New(cfg, telemetry.NewNone(), ratelimit.NewNoop(), zerolog.Nop())
}

func TestDownload(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		chunkCap   int
		wantChunks int
	}{
		{"default chunks", "", 0, DefaultDownloadChunks},
		{"explicit chunks", "?chunks=3", 0, 3},
		{"capped chunks", "?chunks=10", 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.ChunkCap = tt.chunkCap
			h := newTestHandler(cfg)

			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/download"+tt.query, nil)
			h.ServeHTTP(rec, r)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			wantBytes := tt.wantChunks * payload.ChunkSize
			if rec.Body.Len() != wantBytes {
				t.Errorf("body len = %d, want %d", rec.Body.Len(), wantBytes)
			}
			if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(wantBytes) {
				t.Errorf("Content-Length = %q, want %d", cl, wantBytes)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store, no-cache, must-revalidate, max-age=0" {
				t.Errorf("Cache-Control = %q", cc)
			}
			if xa := rec.Header().Get("X-Accel-Buffering"); xa != "no" {
				t.Errorf("X-Accel-Buffering = %q, want no", xa)
			}
		})
	}
}

func TestDownloadBadParam(t *testing.T) {
	h := newTestHandler(config.Defaults())
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/download?chunks=abc", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// trackReader records how many bytes were consumed, to prove the handler drains
// the body before responding.
type trackReader struct {
	data []byte
	pos  int
	read *int
}

func (t *trackReader) Read(p []byte) (int, error) {
	if t.pos >= len(t.data) {
		return 0, io.EOF
	}
	n := copy(p, t.data[t.pos:])
	t.pos += n
	*t.read += n
	return n, nil
}

func TestUploadDrainBefore200(t *testing.T) {
	cfg := config.Defaults()
	cfg.MaxUploadBytes = 0 // no cap
	h := newTestHandler(cfg)

	payloadSize := 5 * payload.ChunkSize
	var readBytes int
	body := &trackReader{data: make([]byte, payloadSize), read: &readBytes}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/upload", body)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if readBytes != payloadSize {
		t.Errorf("drained %d bytes, want %d (body must be fully drained before 200)", readBytes, payloadSize)
	}
}

func TestUploadSizeCap(t *testing.T) {
	cfg := config.Defaults()
	cfg.MaxUploadBytes = 10
	h := newTestHandler(cfg)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/upload", bytes.NewReader(make([]byte, 100)))
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestAuth(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   io.Reader
		token  string
		hdr    string
		want   int
	}{
		{"results no token configured", http.MethodPost, "/api/v1/results", bytes.NewReader([]byte(`{}`)), "", "", http.StatusOK},
		{"results missing bearer", http.MethodPost, "/api/v1/results", bytes.NewReader([]byte(`{}`)), "secret", "", http.StatusUnauthorized},
		{"results wrong bearer", http.MethodPost, "/api/v1/results", bytes.NewReader([]byte(`{}`)), "secret", "Bearer nope", http.StatusUnauthorized},
		{"results correct bearer", http.MethodPost, "/api/v1/results", bytes.NewReader([]byte(`{}`)), "secret", "Bearer secret", http.StatusOK},
		{"stats missing bearer", http.MethodGet, "/api/v1/stats", nil, "secret", "", http.StatusUnauthorized},
		{"stats correct bearer", http.MethodGet, "/api/v1/stats", nil, "secret", "Bearer secret", http.StatusOK},
		{"stats no token configured", http.MethodGet, "/api/v1/stats", nil, "", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.APIToken = tt.token
			h := newTestHandler(cfg)

			rec := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, tt.path, tt.body)
			if tt.hdr != "" {
				r.Header.Set("Authorization", tt.hdr)
			}
			h.ServeHTTP(rec, r)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestPingAndInfo(t *testing.T) {
	h := newTestHandler(config.Defaults())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("ping status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Error("ping missing no-store headers")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/info", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("info status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("info Content-Type = %q", ct)
	}
	var info struct {
		UploadBlobBytes int64 `json:"upload_blob_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("info decode: %v", err)
	}
	if info.UploadBlobBytes != webui.UploadBlobBytes {
		t.Errorf("upload_blob_bytes = %d, want %d", info.UploadBlobBytes, webui.UploadBlobBytes)
	}
}

// TestWebSocketEcho is a regression test for the logging middleware's
// statusRecorder breaking the websocket handshake (it must delegate Hijack to
// the underlying writer). It runs the full handlers.New chain over a real TCP
// listener because the handshake needs an http.Hijacker.
func TestWebSocketEcho(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(config.Defaults()))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws"
	conn, err := websocket.Dial(wsURL, "", srv.URL)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.Close()

	if err := websocket.Message.Send(conn, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	var got []byte
	if err := websocket.Message.Receive(conn, &got); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want %q", got, "ping")
	}
}

// TestTestCountersLabelled asserts the started/completed counters are emitted
// per endpoint type and that no unlabelled aggregate series appears.
func TestTestCountersLabelled(t *testing.T) {
	h := newTestHandler(config.Defaults())

	scrape := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	// value parses the sample for series out of a scrape. Default is
	// process-wide, so assertions below are deltas.
	value := func(body, series string) int64 {
		for _, line := range strings.Split(body, "\n") {
			if rest, ok := strings.CutPrefix(line, series+" "); ok {
				v, err := strconv.ParseInt(rest, 10, 64)
				if err != nil {
					t.Fatalf("bad sample line %q: %v", line, err)
				}
				return v
			}
		}
		t.Fatalf("series %q not found in exposition:\n%s", series, body)
		return 0
	}

	const started = `gospeedtest_tests_started_total{type="download"}`
	const completed = `gospeedtest_tests_completed_total{type="download"}`
	before := scrape()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/download?chunks=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200", rec.Code)
	}

	after := scrape()
	if d := value(after, started) - value(before, started); d != 1 {
		t.Errorf("started{download} delta = %d, want 1", d)
	}
	if d := value(after, completed) - value(before, completed); d != 1 {
		t.Errorf("completed{download} delta = %d, want 1", d)
	}
	for _, line := range strings.Split(after, "\n") {
		if strings.HasPrefix(line, "gospeedtest_tests_started_total ") ||
			strings.HasPrefix(line, "gospeedtest_tests_completed_total ") {
			t.Errorf("unlabelled aggregate sample present: %q", line)
		}
	}
}
