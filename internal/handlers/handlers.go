// Package handlers builds the complete HTTP mux for the server: the embedded
// UI, the native measurement API, telemetry ingestion, health probes and the
// metrics endpoint. New is the single exported constructor; per-route logic is
// implemented on the unexported server type.
package handlers

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Rake-Pro/go-speedtest/internal/clientip"
	"github.com/Rake-Pro/go-speedtest/internal/config"
	"github.com/Rake-Pro/go-speedtest/internal/measure"
	"github.com/Rake-Pro/go-speedtest/internal/metrics"
	"github.com/Rake-Pro/go-speedtest/internal/payload"
	"github.com/Rake-Pro/go-speedtest/internal/ratelimit"
	"github.com/Rake-Pro/go-speedtest/internal/telemetry"
	"github.com/Rake-Pro/go-speedtest/internal/webui"

	"github.com/rs/zerolog"
	"golang.org/x/net/websocket"
)

// Version is reported by GET /api/v1/info.
const Version = "0.1.0"

// DefaultDownloadChunks is the chunk count served when no chunks= query is
// given (see DESIGN.md API table).
const DefaultDownloadChunks = 4

// WebSocket echo limits.
const (
	wsMaxMessageBytes = 4 << 10 // 4 KiB
	wsIdleTimeout     = 30 * time.Second
	wsWriteTimeout    = 10 * time.Second
)

// Endpoint paths, surfaced machine-readably in /api/v1/info.
const (
	epDownload = "/api/v1/download"
	epUpload   = "/api/v1/upload"
	epPing     = "/api/v1/ping"
	epIP       = "/api/v1/ip"
	epWS       = "/api/v1/ws"
	epResults  = "/api/v1/results"
)

// server carries the dependencies shared by every route handler.
type server struct {
	cfg      *config.Config
	store    telemetry.Store
	limiter  ratelimit.Limiter
	resolver clientip.Resolver
	log      zerolog.Logger
}

// New wires the full application HTTP handler: it constructs the route mux from
// the given configuration, telemetry store, rate limiter and logger, and mounts
// the embedded UI. The returned handler is ready to hand to server.BuildServer.
func New(cfg *config.Config, store telemetry.Store, limiter ratelimit.Limiter, logger zerolog.Logger) http.Handler {
	resolver, _ := clientip.NewResolver(cfg.TrustedProxies)
	s := &server{
		cfg:      cfg,
		store:    store,
		limiter:  limiter,
		resolver: resolver,
		log:      logger,
	}
	return s.logMW(s.corsMW(s.routes()))
}

// routes registers every endpoint on a fresh ServeMux.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	// Native measurement + telemetry API. The heavy test endpoints are wrapped
	// with rate-limit admission + metrics instrumentation.
	mux.Handle("GET /api/v1/download", s.testMW("download", http.HandlerFunc(s.handleDownload)))
	mux.Handle("POST /api/v1/upload", s.testMW("upload", http.HandlerFunc(s.handleUpload)))
	mux.Handle("GET /api/v1/ws", s.testMW("ws", websocket.Handler(s.handleWS)))
	mux.HandleFunc("GET /api/v1/ping", s.handlePing)
	mux.HandleFunc("GET /api/v1/ip", s.handleIP)
	mux.HandleFunc("GET /api/v1/info", s.handleInfo)
	mux.HandleFunc("POST /api/v1/results", s.handleResultsIngest)
	mux.HandleFunc("GET /api/v1/results/{id}", s.handleResultGet)
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)

	// Probes + metrics.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", metrics.Handler())

	// Embedded UI + /config.json (catch-all root).
	mux.Handle("GET /", webui.Handler(s.cfg))

	return mux
}

// --- Middleware ---

// testMW gates a heavy test endpoint on rate-limit admission and records
// tests-started/completed counters and the active-streams gauge.
func (s *server) testMW(typ string, next http.Handler) http.Handler {
	started := typedSeries(metrics.TestsStarted, typ)
	completed := typedSeries(metrics.TestsCompleted, typ)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.resolver.FromRequest(r)
		release, ok := s.limiter.Acquire(ip)
		if !ok {
			http.Error(w, "too many concurrent tests", http.StatusTooManyRequests)
			return
		}
		defer release()

		metrics.Inc(started)
		metrics.AddGauge(metrics.ActiveStreams, 1)
		defer metrics.AddGauge(metrics.ActiveStreams, -1)

		next.ServeHTTP(w, r)
		metrics.Inc(completed)
	})
}

// typedSeries appends the {type="..."} label the metrics registry expects.
func typedSeries(name, typ string) string {
	return name + `{type="` + typ + `"}`
}

// corsMW applies the CORS allowlist to /api routes and answers preflight.
func (s *server) corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) originAllowed(origin string) bool {
	for _, o := range s.cfg.CORSOrigins {
		if o == origin || o == "*" {
			return true
		}
	}
	return false
}

// logMW records a zerolog line per request. Payload routes are noisy, so they
// are logged only at debug level.
func (s *server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		ev := s.log.Info()
		if isPayloadRoute(r.URL.Path) {
			ev = s.log.Debug()
		}
		ev.Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rec.status).
			Dur("dur", time.Since(start)).
			Str("ip", s.resolver.FromRequest(r).String()).
			Msg("request")
	})
}

func isPayloadRoute(p string) bool {
	switch p {
	case epDownload, epUpload, epPing, epWS:
		return true
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so the download flush loop keeps
// working through the logging wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Hijack delegates to the underlying writer so the websocket handshake works
// through the logging wrapper. ResponseController follows Unwrap chains and
// returns http.ErrNotSupported if no hijacker is found.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(r.ResponseWriter).Hijack()
}

// --- Payload / measurement handlers ---

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	type endpoints struct {
		Download string `json:"download"`
		Upload   string `json:"upload"`
		Ping     string `json:"ping"`
		IP       string `json:"ip"`
		WS       string `json:"ws"`
		Results  string `json:"results"`
		Info     string `json:"info"`
	}
	info := struct {
		ServerName      string    `json:"server_name"`
		Version         string    `json:"version"`
		Mode            string    `json:"mode"`
		TestDurationMs  int64     `json:"test_duration_ms"`
		GraceDownloadMs int64     `json:"grace_download_ms"`
		GraceUploadMs   int64     `json:"grace_upload_ms"`
		DownloadStreams int       `json:"download_streams"`
		UploadStreams   int       `json:"upload_streams"`
		OverheadFactor  float64   `json:"overhead_factor"`
		ChunkSizeBytes  int64     `json:"chunk_size_bytes"`
		UploadBlobBytes int64     `json:"upload_blob_bytes"`
		DownloadChunks  int       `json:"download_chunks"`
		ChunkCap        int       `json:"chunk_cap"`
		MaxUploadBytes  int64     `json:"max_upload_bytes"`
		PingSamples     int       `json:"ping_samples"`
		WebSocketPing   bool      `json:"websocket_ping"`
		Endpoints       endpoints `json:"endpoints"`
	}{
		ServerName:      s.cfg.ServerName,
		Version:         Version,
		Mode:            s.cfg.Mode,
		TestDurationMs:  s.cfg.TestDuration.Milliseconds(),
		GraceDownloadMs: s.cfg.GraceDownload.Milliseconds(),
		GraceUploadMs:   s.cfg.GraceUpload.Milliseconds(),
		DownloadStreams: s.cfg.DownloadStreams,
		UploadStreams:   s.cfg.UploadStreams,
		OverheadFactor:  s.cfg.OverheadFactor,
		ChunkSizeBytes:  payload.ChunkSize,
		UploadBlobBytes: webui.UploadBlobBytes,
		DownloadChunks:  DefaultDownloadChunks,
		ChunkCap:        s.cfg.ChunkCap,
		MaxUploadBytes:  s.cfg.MaxUploadBytes,
		PingSamples:     s.cfg.PingSamples,
		WebSocketPing:   true,
		Endpoints: endpoints{
			Download: epDownload, Upload: epUpload, Ping: epPing,
			IP: epIP, WS: epWS, Results: epResults, Info: "/api/v1/info",
		},
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	chunks := DefaultDownloadChunks
	if q := r.URL.Query().Get("chunks"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			http.Error(w, "invalid chunks parameter", http.StatusBadRequest)
			return
		}
		chunks = n
	}
	if s.cfg.ChunkCap > 0 && chunks > s.cfg.ChunkCap {
		chunks = s.cfg.ChunkCap
	}

	buf := payload.Buffer()
	if buf == nil {
		http.Error(w, "payload not initialized", http.StatusServiceUnavailable)
		return
	}

	setNoStore(w)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(int64(chunks)*payload.ChunkSize, 10))
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	for i := 0; i < chunks; i++ {
		n, err := w.Write(buf)
		if n > 0 {
			metrics.Add(metrics.BytesServed, int64(n))
		}
		if err != nil {
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = r.Body
	if s.cfg.MaxUploadBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	}
	if s.cfg.ReadDeadline > 0 {
		if rc := http.NewResponseController(w); rc != nil {
			_ = rc.SetReadDeadline(time.Now().Add(s.cfg.ReadDeadline))
		}
	}

	// Fully drain the body BEFORE responding, per DESIGN.md.
	n, err := io.Copy(io.Discard, body)
	if n > 0 {
		metrics.Add(metrics.BytesReceived, n)
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "upload read error", http.StatusBadRequest)
		return
	}

	setNoStore(w)
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlePing(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleIP(w http.ResponseWriter, r *http.Request) {
	addr := s.resolver.FromRequest(r)
	ip := ""
	if addr.IsValid() {
		ip = addr.String()
	}
	setNoStore(w)
	writeJSON(w, http.StatusOK, map[string]string{"ip": ip})
}

func (s *server) handleWS(ws *websocket.Conn) {
	ws.MaxPayloadBytes = wsMaxMessageBytes
	for {
		if err := ws.SetReadDeadline(time.Now().Add(wsIdleTimeout)); err != nil {
			return
		}
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err != nil {
			return
		}
		if err := ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
			return
		}
		if err := websocket.Message.Send(ws, data); err != nil {
			return
		}
	}
}

// --- Telemetry handlers ---

func (s *server) handleResultsIngest(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		unauthorized(w)
		return
	}
	var res measure.Result
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&res); err != nil {
		http.Error(w, "invalid result JSON", http.StatusBadRequest)
		return
	}
	if res.DownloadMbps < 0 || res.UploadMbps < 0 || res.PingMs < 0 || res.JitterMs < 0 {
		http.Error(w, "result contains negative metrics", http.StatusBadRequest)
		return
	}
	if res.Timestamp == "" {
		res.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if res.Source == "" {
		res.Source = measure.SourceAPI
	}
	if res.ClientIP == "" {
		if addr := s.resolver.FromRequest(r); addr.IsValid() {
			res.ClientIP = addr.String()
		}
	}
	if res.UserAgent == "" {
		res.UserAgent = r.UserAgent()
	}
	if res.ServerName == "" {
		res.ServerName = s.cfg.ServerName
	}

	id, err := s.store.Save(r.Context(), res)
	if err != nil {
		http.Error(w, "failed to store result", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// handleResultGet intentionally does NOT call authorized(). Retrieval by id is
// the share path: a result URL is meant to stay openable by whoever it was
// handed to, so gating it on the API token would break sharing. The id is a
// 128-bit CSPRNG value (telemetry.newID) and is therefore unguessable, and the
// two mutating/enumerating siblings (results ingest, stats list) do enforce the
// token. See the API table in DESIGN.md, which marks auth only on those two.
func (s *server) handleResultGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.store.Get(r.Context(), id)
	if errors.Is(err, telemetry.ErrNotFound) {
		http.Error(w, "result not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to load result", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		unauthorized(w)
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	list, err := s.store.List(r.Context(), limit)
	if err != nil {
		http.Error(w, "failed to list results", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []measure.Result{}
	}
	writeJSON(w, http.StatusOK, list)
}

// --- Probes ---

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if payload.Buffer() == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "payload not ready")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ready")
}

// --- helpers ---

func (s *server) authorized(r *http.Request) bool {
	if s.cfg.APIToken == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.cfg.APIToken)) == 1
}

func setNoStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("X-Accel-Buffering", "no")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
