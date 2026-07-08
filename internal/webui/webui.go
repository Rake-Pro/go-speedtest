// Package webui serves the embedded vanilla-JS frontend and the runtime
// /config.json document that parameterizes it. All assets are compiled into the
// binary via go:embed. AGENT-UI owns this package; the UIConfig field names
// below are the frozen contract between the server config and the browser.
package webui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"github.com/Rake-Pro/go-speedtest/internal/config"
)

//go:embed assets/*
var assets embed.FS

// Derived measurement parameters that config.Config does not carry as
// first-class fields. They mirror the DESIGN.md methodology numbers.
const (
	// chunkSizeBytes is one download chunk (1 MiB). Mirrors payload.ChunkSize;
	// hardcoded here so webui stays buildable independent of that package's
	// concurrent edit state.
	chunkSizeBytes int64 = 1 << 20
	// downloadChunks is the chunks-per-request the client asks the download
	// endpoint for. The API default for GET /api/v1/download is 4 chunks.
	downloadChunks int = 4
	// UploadBlobBytes is the ~20 MB random blob the client POSTs. Exported so
	// the API info handler reports the same value as /config.json.
	UploadBlobBytes int64 = 20 << 20
)

// assetCacheControl is applied to embedded static assets (brief cache).
const assetCacheControl = "public, max-age=300"

// Endpoints maps logical measurement endpoints to their URL paths, handed to
// the browser so the UI never hardcodes routes.
type Endpoints struct {
	Download string `json:"download"`
	Upload   string `json:"upload"`
	Ping     string `json:"ping"`
	IP       string `json:"ip"`
	WS       string `json:"ws"`
	Results  string `json:"results"`
}

// UIConfig is the JSON shape served at GET /config.json. It surfaces the
// server-resolved measurement parameters to the browser client. Field names are
// frozen; AGENT-UI must consume exactly these.
type UIConfig struct {
	ServerName      string    `json:"server_name"`
	TestDurationMs  int64     `json:"test_duration_ms"`
	GraceDownloadMs int64     `json:"grace_download_ms"`
	GraceUploadMs   int64     `json:"grace_upload_ms"`
	DownloadStreams int       `json:"download_streams"`
	UploadStreams   int       `json:"upload_streams"`
	OverheadFactor  float64   `json:"overhead_factor"`
	ChunkSizeBytes  int64     `json:"chunk_size_bytes"`
	UploadBlobBytes int64     `json:"upload_blob_bytes"`
	PingSamples     int       `json:"ping_samples"`
	DownloadChunks  int       `json:"download_chunks"`
	WebSocketPing   bool      `json:"websocket_ping"`
	Endpoints       Endpoints `json:"endpoints"`
}

// BuildUIConfig projects the server Config into the browser-facing UIConfig.
func BuildUIConfig(cfg *config.Config) UIConfig {
	return UIConfig{
		ServerName:      cfg.ServerName,
		TestDurationMs:  cfg.TestDuration.Milliseconds(),
		GraceDownloadMs: cfg.GraceDownload.Milliseconds(),
		GraceUploadMs:   cfg.GraceUpload.Milliseconds(),
		DownloadStreams: cfg.DownloadStreams,
		UploadStreams:   cfg.UploadStreams,
		OverheadFactor:  cfg.OverheadFactor,
		ChunkSizeBytes:  chunkSizeBytes,
		UploadBlobBytes: UploadBlobBytes,
		PingSamples:     cfg.PingSamples,
		DownloadChunks:  downloadChunks,
		WebSocketPing:   true,
		Endpoints: Endpoints{
			Download: "/api/v1/download",
			Upload:   "/api/v1/upload",
			Ping:     "/api/v1/ping",
			IP:       "/api/v1/ip",
			WS:       "/api/v1/ws",
			Results:  "/api/v1/results",
		},
	}
}

// Handler serves the embedded UI at / and the generated /config.json document.
func Handler(cfg *config.Config) http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Unreachable: the embedded FS always contains the assets directory.
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	// config.json is static for the process lifetime; marshal once.
	configJSON, err := json.Marshal(BuildUIConfig(cfg))
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/config.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(configJSON)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", assetCacheControl)
		fileServer.ServeHTTP(w, r)
	})
	return mux
}
