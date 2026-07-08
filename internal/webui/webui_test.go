package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rake-Pro/go-speedtest/internal/config"
)

func testConfig() *config.Config {
	cfg := config.Defaults()
	cfg.ServerName = "test-server"
	return cfg
}

func TestBuildUIConfig(t *testing.T) {
	cfg := testConfig()
	ui := BuildUIConfig(cfg)

	if ui.ServerName != "test-server" {
		t.Errorf("ServerName = %q, want test-server", ui.ServerName)
	}
	if ui.TestDurationMs != cfg.TestDuration.Milliseconds() {
		t.Errorf("TestDurationMs = %d, want %d", ui.TestDurationMs, cfg.TestDuration.Milliseconds())
	}
	if ui.GraceDownloadMs != cfg.GraceDownload.Milliseconds() {
		t.Errorf("GraceDownloadMs = %d, want %d", ui.GraceDownloadMs, cfg.GraceDownload.Milliseconds())
	}
	if ui.GraceUploadMs != cfg.GraceUpload.Milliseconds() {
		t.Errorf("GraceUploadMs = %d, want %d", ui.GraceUploadMs, cfg.GraceUpload.Milliseconds())
	}
	if ui.DownloadStreams != cfg.DownloadStreams {
		t.Errorf("DownloadStreams = %d, want %d", ui.DownloadStreams, cfg.DownloadStreams)
	}
	if ui.UploadStreams != cfg.UploadStreams {
		t.Errorf("UploadStreams = %d, want %d", ui.UploadStreams, cfg.UploadStreams)
	}
	if ui.OverheadFactor != cfg.OverheadFactor {
		t.Errorf("OverheadFactor = %v, want %v", ui.OverheadFactor, cfg.OverheadFactor)
	}
	if ui.PingSamples != cfg.PingSamples {
		t.Errorf("PingSamples = %d, want %d", ui.PingSamples, cfg.PingSamples)
	}
	if ui.ChunkSizeBytes != 1<<20 {
		t.Errorf("ChunkSizeBytes = %d, want %d", ui.ChunkSizeBytes, 1<<20)
	}
	if ui.UploadBlobBytes != 20<<20 {
		t.Errorf("UploadBlobBytes = %d, want %d", ui.UploadBlobBytes, 20<<20)
	}
	if ui.DownloadChunks != 4 {
		t.Errorf("DownloadChunks = %d, want 4", ui.DownloadChunks)
	}
	if !ui.WebSocketPing {
		t.Error("WebSocketPing = false, want true")
	}
	want := Endpoints{
		Download: "/api/v1/download",
		Upload:   "/api/v1/upload",
		Ping:     "/api/v1/ping",
		IP:       "/api/v1/ip",
		WS:       "/api/v1/ws",
		Results:  "/api/v1/results",
	}
	if ui.Endpoints != want {
		t.Errorf("Endpoints = %+v, want %+v", ui.Endpoints, want)
	}
}

func TestConfigJSONShape(t *testing.T) {
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	body, _ := io.ReadAll(resp.Body)

	// Verify every frozen JSON key is present in the wire output.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{
		"server_name", "test_duration_ms", "grace_download_ms", "grace_upload_ms",
		"download_streams", "upload_streams", "overhead_factor", "chunk_size_bytes",
		"upload_blob_bytes", "ping_samples", "download_chunks", "websocket_ping",
		"endpoints",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("config.json missing key %q", key)
		}
	}

	var ep struct {
		Endpoints map[string]json.RawMessage `json:"endpoints"`
	}
	if err := json.Unmarshal(body, &ep); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"download", "upload", "ping", "ip", "ws", "results"} {
		if _, ok := ep.Endpoints[key]; !ok {
			t.Errorf("endpoints missing key %q", key)
		}
	}
}

func TestServeIndex(t *testing.T) {
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != assetCacheControl {
		t.Errorf("Cache-Control = %q, want %q", cc, assetCacheControl)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "go-speedtest") {
		t.Error("index.html body missing expected marker")
	}
}

func TestServeStaticAsset(t *testing.T) {
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	for path, wantCT := range map[string]string{
		"/app.css":      "text/css",
		"/speedtest.js": "javascript",
		"/worker.js":    "javascript",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, wantCT) {
			t.Errorf("%s: Content-Type = %q, want to contain %q", path, ct, wantCT)
		}
		resp.Body.Close()
	}
}
