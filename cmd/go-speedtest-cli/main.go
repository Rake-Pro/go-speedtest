// Command go-speedtest-cli runs a speed test against a go-speedtest server from
// the terminal, using the same measurement math as the browser client
// (internal/measure).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/net/websocket"

	"github.com/Rake-Pro/go-speedtest/internal/measure"
)

const readBufSize = 128 << 10 // 128 KiB read buffer for the download path

// retryBackoff is how long a stream waits after a failed request (429, reset,
// EOF) before retrying, so a transient failure does not permanently thin the
// stream pool for the rest of the measurement window.
const retryBackoff = 500 * time.Millisecond

// serverInfo mirrors the JSON served at GET /api/v1/info (same field names as
// the webui.UIConfig contract). Zero-valued fields fall back to the canonical
// measure defaults.
type serverInfo struct {
	ServerName      string  `json:"server_name"`
	TestDurationMs  int64   `json:"test_duration_ms"`
	GraceDownloadMs int64   `json:"grace_download_ms"`
	GraceUploadMs   int64   `json:"grace_upload_ms"`
	DownloadStreams int     `json:"download_streams"`
	UploadStreams   int     `json:"upload_streams"`
	OverheadFactor  float64 `json:"overhead_factor"`
	ChunkSizeBytes  int64   `json:"chunk_size_bytes"`
	UploadBlobBytes int64   `json:"upload_blob_bytes"`
	PingSamples     int     `json:"ping_samples"`
	DownloadChunks  int     `json:"download_chunks"`
	WebSocketPing   bool    `json:"websocket_ping"`
	Endpoints       struct {
		Download string `json:"download"`
		Upload   string `json:"upload"`
		Ping     string `json:"ping"`
		IP       string `json:"ip"`
		WS       string `json:"ws"`
		Results  string `json:"results"`
	} `json:"endpoints"`
}

// options is the fully-resolved test plan: server info defaults with flag
// overrides applied.
type options struct {
	serverURL  *url.URL
	serverName string

	duration      time.Duration
	graceDownload time.Duration
	graceUpload   time.Duration
	overhead      float64
	streamsDL     int
	streamsUL     int
	pingSamples   int
	chunks        int
	blobBytes     int64
	wsPing        bool

	epDownload string
	epUpload   string
	epPing     string
	epWS       string
	epResults  string

	doPing, doDownload, doUpload bool
	push                         bool
	token                        string
	timeout                      time.Duration

	format   string // "human" | "json" | "csv" | "prometheus"
	progress bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("go-speedtest-cli", flag.ContinueOnError)
	fs.SetOutput(stderr)
	serverURL := fs.String("server", "", "target go-speedtest server base URL (required)")
	jsonOut := fs.Bool("json", false, "emit the Result as JSON")
	csvOut := fs.Bool("csv", false, "emit the Result as CSV (header + one row)")
	promOut := fs.Bool("prometheus", false, "emit the Result in Prometheus textfile-collector format")
	duration := fs.Duration("duration", 0, "measured test duration per direction (0 = server default)")
	streamsDL := fs.Int("streams-download", 0, "parallel download streams, 3-12 (0 = server default)")
	streamsUL := fs.Int("streams-upload", 0, "parallel upload streams, 3-12 (0 = server default)")
	noDownload := fs.Bool("no-download", false, "skip the download phase")
	noUpload := fs.Bool("no-upload", false, "skip the upload phase")
	noPing := fs.Bool("no-ping", false, "skip the ping phase")
	push := fs.Bool("push", false, "POST the result to the server results endpoint")
	token := fs.String("token", "", "bearer token for -push")
	timeout := fs.Duration("timeout", 10*time.Second, "timeout for control requests (info, ping, push)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// SyncWriter guards the writer with a mutex so the per-stream retry warnings
	// emitted concurrently from parallel download/upload goroutines do not race.
	log := zerolog.New(zerolog.SyncWriter(zerolog.ConsoleWriter{Out: stderr, TimeFormat: time.TimeOnly})).
		Level(zerolog.WarnLevel).With().Timestamp().Logger()

	if *serverURL == "" {
		return errors.New("-server is required")
	}
	base, err := url.Parse(*serverURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("invalid -server URL %q", *serverURL)
	}
	nFormats := 0
	format := "human"
	for _, f := range []struct {
		on   bool
		name string
	}{{*jsonOut, "json"}, {*csvOut, "csv"}, {*promOut, "prometheus"}} {
		if f.on {
			nFormats++
			format = f.name
		}
	}
	if nFormats > 1 {
		return errors.New("-json, -csv and -prometheus are mutually exclusive")
	}
	for _, s := range []struct {
		v    int
		name string
	}{{*streamsDL, "streams-download"}, {*streamsUL, "streams-upload"}} {
		if s.v != 0 && (s.v < measure.MinConfigurableStreams || s.v > measure.MaxConfigurableStreams) {
			return fmt.Errorf("-%s must be between %d and %d",
				s.name, measure.MinConfigurableStreams, measure.MaxConfigurableStreams)
		}
	}
	if *noPing && *noDownload && *noUpload {
		return errors.New("all phases disabled; nothing to do")
	}

	info, err := fetchInfo(base, *timeout, log)
	if err != nil {
		return err
	}

	opt := resolveOptions(base, info, *duration, *streamsDL, *streamsUL)
	opt.doPing, opt.doDownload, opt.doUpload = !*noPing, !*noDownload, !*noUpload
	opt.push, opt.token, opt.timeout = *push, *token, *timeout
	opt.format = format
	opt.progress = format == "human" && isTerminal(os.Stderr)

	client := newHTTPClient(max(opt.streamsDL, opt.streamsUL))
	res := measure.NewResult(time.Now(), measure.SourceCLI)
	res.ServerName = opt.serverName
	res.OverheadFactor = opt.overhead

	if opt.doPing {
		ps, err := runPing(client, opt, log)
		if err != nil {
			return fmt.Errorf("ping: %w", err)
		}
		res.SetPing(ps)
	}
	if opt.doDownload {
		m, err := runDownload(client, opt, log)
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}
		res.SetDownload(m)
		res.StreamsDownload = opt.streamsDL
	}
	if opt.doUpload {
		m, err := runUpload(client, opt, log)
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		res.SetUpload(m)
		res.StreamsUpload = opt.streamsUL
	}

	if opt.push {
		id, err := pushResult(client, opt, res)
		if err != nil {
			return fmt.Errorf("push: %w", err)
		}
		res.ID = id
	}

	return output(stdout, opt, res)
}

// --- startup ---

func fetchInfo(base *url.URL, timeout time.Duration, log zerolog.Logger) (serverInfo, error) {
	var info serverInfo
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath("/api/v1/info").String(), nil)
	if err != nil {
		return info, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return info, fmt.Errorf("fetch server info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Server reachable but info unavailable: proceed on canonical defaults.
		io.Copy(io.Discard, resp.Body)
		log.Warn().Int("status", resp.StatusCode).
			Msg("server info unavailable, using built-in defaults")
		return info, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return info, fmt.Errorf("decode server info: %w", err)
	}
	return info, nil
}

// resolveOptions merges server-provided parameters with flag overrides; zero
// info fields fall back to the canonical measure defaults.
func resolveOptions(base *url.URL, info serverInfo, duration time.Duration, streamsDL, streamsUL int) *options {
	opt := &options{
		serverURL:     base,
		serverName:    info.ServerName,
		duration:      measure.DefaultTestDuration,
		graceDownload: measure.DefaultGraceDownload,
		graceUpload:   measure.DefaultGraceUpload,
		overhead:      measure.DefaultOverheadFactor,
		streamsDL:     measure.DefaultDownloadStreams,
		streamsUL:     measure.DefaultUploadStreams,
		pingSamples:   measure.DefaultPingSamples,
		chunks:        4,
		blobBytes:     20 << 20,
		wsPing:        info.WebSocketPing,
		epDownload:    "/api/v1/download",
		epUpload:      "/api/v1/upload",
		epPing:        "/api/v1/ping",
		epWS:          "/api/v1/ws",
		epResults:     "/api/v1/results",
	}
	if info.TestDurationMs > 0 {
		opt.duration = time.Duration(info.TestDurationMs) * time.Millisecond
	}
	if info.GraceDownloadMs > 0 {
		opt.graceDownload = time.Duration(info.GraceDownloadMs) * time.Millisecond
	}
	if info.GraceUploadMs > 0 {
		opt.graceUpload = time.Duration(info.GraceUploadMs) * time.Millisecond
	}
	if info.OverheadFactor > 0 {
		opt.overhead = info.OverheadFactor
	}
	if info.DownloadStreams > 0 {
		opt.streamsDL = clampStreams(info.DownloadStreams)
	}
	if info.UploadStreams > 0 {
		opt.streamsUL = clampStreams(info.UploadStreams)
	}
	if info.PingSamples > 0 {
		opt.pingSamples = info.PingSamples
	}
	if info.DownloadChunks > 0 {
		opt.chunks = info.DownloadChunks
	}
	if info.UploadBlobBytes > 0 {
		opt.blobBytes = info.UploadBlobBytes
	}
	for dst, src := range map[*string]string{
		&opt.epDownload: info.Endpoints.Download,
		&opt.epUpload:   info.Endpoints.Upload,
		&opt.epPing:     info.Endpoints.Ping,
		&opt.epWS:       info.Endpoints.WS,
		&opt.epResults:  info.Endpoints.Results,
	} {
		if src != "" {
			*dst = src
		}
	}
	// Flags override everything from the server.
	if duration > 0 {
		opt.duration = duration
	}
	if streamsDL > 0 {
		opt.streamsDL = streamsDL
	}
	if streamsUL > 0 {
		opt.streamsUL = streamsUL
	}
	return opt
}

func clampStreams(n int) int {
	return min(max(n, measure.MinConfigurableStreams), measure.MaxConfigurableStreams)
}

// newHTTPClient builds a client that forces HTTP/1.1 semantics and allows one
// true parallel TCP connection per stream (plus headroom for control calls).
func newHTTPClient(streams int) *http.Client {
	conns := streams + 2
	return &http.Client{
		Transport: &http.Transport{
			ForceAttemptHTTP2:   false,
			TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // disable h2 over TLS
			MaxIdleConns:        conns,
			MaxIdleConnsPerHost: conns,
			MaxConnsPerHost:     conns,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// --- ping phase ---

func runPing(c *http.Client, opt *options, log zerolog.Logger) (*measure.PingStats, error) {
	if opt.wsPing {
		if ps, err := pingWS(opt); err == nil {
			return ps, nil
		} else {
			log.Warn().Err(err).Msg("websocket ping failed, falling back to HTTP")
		}
	}
	return pingHTTP(c, opt)
}

func pingWS(opt *options) (*measure.PingStats, error) {
	wsURL := *opt.serverURL.JoinPath(opt.epWS)
	switch wsURL.Scheme {
	case "https":
		wsURL.Scheme = "wss"
	default:
		wsURL.Scheme = "ws"
	}
	cfg, err := websocket.NewConfig(wsURL.String(), opt.serverURL.String())
	if err != nil {
		return nil, err
	}
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ps := &measure.PingStats{}
	var echo string
	for i := 0; i < opt.pingSamples; i++ {
		if err := conn.SetDeadline(time.Now().Add(opt.timeout)); err != nil {
			return nil, err
		}
		t := time.Now()
		if err := websocket.Message.Send(conn, "ping"); err != nil {
			return nil, err
		}
		if err := websocket.Message.Receive(conn, &echo); err != nil {
			return nil, err
		}
		ps.Add(time.Since(t))
		progressPing(opt, i+1)
	}
	progressDone(opt)
	return ps, nil
}

func pingHTTP(c *http.Client, opt *options) (*measure.PingStats, error) {
	u := opt.serverURL.JoinPath(opt.epPing).String()
	ps := &measure.PingStats{}
	// One warmup request to establish the connection, then timed samples over
	// the kept-alive socket.
	for i := 0; i <= opt.pingSamples; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
		t := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ping endpoint returned %s", resp.Status)
		}
		if i > 0 { // skip the warmup sample
			ps.Add(time.Since(t))
			progressPing(opt, i)
		}
	}
	progressDone(opt)
	return ps, nil
}

// --- download phase ---

// lockedMeter guards a ThroughputMeter (pure, not concurrency-safe) for use
// across parallel streams.
type lockedMeter struct {
	mu sync.Mutex
	m  *measure.ThroughputMeter
}

func (l *lockedMeter) add(n int64) {
	l.mu.Lock()
	l.m.Add(n, time.Now())
	l.mu.Unlock()
}

func (l *lockedMeter) mbps() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Mbps()
}

func runDownload(c *http.Client, opt *options, log zerolog.Logger) (*measure.ThroughputMeter, error) {
	lm := &lockedMeter{m: measure.NewThroughputMeter(opt.overhead, opt.graceDownload)}
	// The measured window is opt.duration after the grace reset, so the wall
	// deadline is grace + duration (librespeed counts test time post-grace).
	total := opt.graceDownload + opt.duration
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	lm.m.Start(time.Now())

	stop := startProgress(opt, "download", total, lm.mbps)
	u := opt.serverURL.JoinPath(opt.epDownload)
	q := u.Query()
	q.Set("chunks", strconv.Itoa(opt.chunks))
	u.RawQuery = q.Encode()

	// Each stream runs for the whole window. A failed request (429, reset, EOF)
	// only affects that stream: it warns, backs off and retries. The phase as a
	// whole fails only if no stream ever transferred any bytes.
	var wg sync.WaitGroup
	for i := 0; i < opt.streamsDL; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			buf := make([]byte, readBufSize)
			for ctx.Err() == nil {
				if err := downloadOnce(ctx, c, u.String(), buf, lm); err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Warn().Err(err).Int("stream", id).
						Msg("download stream failed, retrying after backoff")
					if !sleepCtx(ctx, retryBackoff) {
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	stop()

	lm.m.Stop(time.Now())
	if lm.m.Bytes() == 0 {
		return nil, errors.New("all download streams failed; no data transferred in the measurement window")
	}
	warnIfTruncated(log, "download", opt.duration, lm.m.Elapsed())
	return lm.m, nil
}

func downloadOnce(ctx context.Context, c *http.Client, u string, buf []byte, lm *lockedMeter) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("download endpoint returned %s", resp.Status)
	}
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			lm.add(int64(n))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// sleepCtx waits for d or until ctx is cancelled. It returns true if the full
// duration elapsed, false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// warnIfTruncated warns on stderr when the achieved measurement window fell
// below 80% of what was requested, so a short/thin sample is not silently
// reported as a full-window throughput figure.
func warnIfTruncated(log zerolog.Logger, phase string, want, got time.Duration) {
	if got < time.Duration(float64(want)*0.8) {
		log.Warn().Str("phase", phase).
			Dur("requested", want).Dur("achieved", got).
			Msg("measurement window truncated; throughput derived from a short sample")
	}
}

// --- upload phase ---

// countingReader tallies bytes handed to the HTTP transport for a single
// request. The tally is committed to the shared meter only after a 200
// response, so bytes pushed into socket buffers for a request the peer rejects
// (e.g. a 429 that never drains the body) never contribute throughput.
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

func runUpload(c *http.Client, opt *options, log zerolog.Logger) (*measure.ThroughputMeter, error) {
	// One incompressible random blob, generated once and reused by every
	// stream. math/rand/v2 is fine client-side: incompressibility is the goal.
	blob := make([]byte, opt.blobBytes)
	var seed [32]byte
	for i := 0; i < len(seed); i += 8 {
		binary.LittleEndian.PutUint64(seed[i:], rand.Uint64())
	}
	rand.NewChaCha8(seed).Read(blob)

	lm := &lockedMeter{m: measure.NewThroughputMeter(opt.overhead, opt.graceUpload)}
	total := opt.graceUpload + opt.duration
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	lm.m.Start(time.Now())

	stop := startProgress(opt, "upload", total, lm.mbps)
	u := opt.serverURL.JoinPath(opt.epUpload).String()

	// Each stream runs for the whole window; a failed request warns, backs off
	// and retries. The phase fails only if no stream ever delivered any bytes.
	var wg sync.WaitGroup
	for i := 0; i < opt.streamsUL; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := uploadOnce(ctx, c, u, blob, lm); err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Warn().Err(err).Int("stream", id).
						Msg("upload stream failed, retrying after backoff")
					if !sleepCtx(ctx, retryBackoff) {
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	stop()

	lm.m.Stop(time.Now())
	if lm.m.Bytes() == 0 {
		return nil, errors.New("all upload streams failed; no data transferred in the measurement window")
	}
	warnIfTruncated(log, "upload", opt.duration, lm.m.Elapsed())
	return lm.m, nil
}

func uploadOnce(ctx context.Context, c *http.Client, u string, blob []byte, lm *lockedMeter) error {
	cr := &countingReader{r: bytes.NewReader(blob)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, cr)
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(blob))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload endpoint returned %s", resp.Status)
	}
	// Only a fully-accepted (200) upload contributes bytes.
	lm.add(cr.n)
	return nil
}

// --- push ---

func pushResult(c *http.Client, opt *options, res measure.Result) (string, error) {
	payload, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		opt.serverURL.JoinPath(opt.epResults).String(), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if opt.token != "" {
		req.Header.Set("Authorization", "Bearer "+opt.token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("results endpoint returned %s", resp.Status)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode push response: %w", err)
	}
	return out.ID, nil
}

// --- progress (stderr, human + TTY only) ---

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func progressPing(opt *options, done int) {
	if opt.progress {
		fmt.Fprintf(os.Stderr, "\rping      %d/%d", done, opt.pingSamples)
	}
}

func progressDone(opt *options) {
	if opt.progress {
		fmt.Fprint(os.Stderr, "\n")
	}
}

// startProgress prints percentage + live Mbps lines to stderr until the
// returned stop function is called. No-op unless progress is enabled.
func startProgress(opt *options, phase string, total time.Duration, mbps func() float64) (stop func()) {
	if !opt.progress {
		return func() {}
	}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				fmt.Fprintf(os.Stderr, "\r%-9s 100%%  %8.2f Mbps\n", phase, mbps())
				close(done)
				return
			case <-tick.C:
				pct := min(int(time.Since(start)*100/total), 99)
				fmt.Fprintf(os.Stderr, "\r%-9s %3d%%  %8.2f Mbps", phase, pct, mbps())
			}
		}
	}()
	return func() {
		done <- struct{}{}
		<-done
	}
}

// --- output ---

func output(w io.Writer, opt *options, res measure.Result) error {
	switch opt.format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	case "csv":
		return outputCSV(w, res)
	case "prometheus":
		return outputPrometheus(w, opt, res)
	default:
		return outputHuman(w, opt, res)
	}
}

func outputCSV(w io.Writer, res measure.Result) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"id", "timestamp", "server_name", "source",
		"ping_ms", "jitter_ms", "download_mbps", "upload_mbps",
		"download_bytes", "upload_bytes",
		"download_duration_ms", "upload_duration_ms",
		"streams_download", "streams_upload", "overhead_factor",
	}); err != nil {
		return err
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	if err := cw.Write([]string{
		res.ID, res.Timestamp, res.ServerName, res.Source,
		f(res.PingMs), f(res.JitterMs), f(res.DownloadMbps), f(res.UploadMbps),
		strconv.FormatInt(res.DownloadBytes, 10), strconv.FormatInt(res.UploadBytes, 10),
		strconv.FormatInt(res.DownloadDurationMs, 10), strconv.FormatInt(res.UploadDurationMs, 10),
		strconv.Itoa(res.StreamsDownload), strconv.Itoa(res.StreamsUpload), f(res.OverheadFactor),
	}); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func outputPrometheus(w io.Writer, opt *options, res measure.Result) error {
	labels := ""
	if res.ServerName != "" {
		labels = fmt.Sprintf(`{server_name=%q}`, res.ServerName)
	}
	var b strings.Builder
	metric := func(name, help string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s%s %g\n", name, help, name, name, labels, value)
	}
	if opt.doPing {
		metric("speedtest_ping_ms", "Minimum round-trip time in milliseconds.", res.PingMs)
		metric("speedtest_jitter_ms", "Asymmetric-EWMA RTT jitter in milliseconds.", res.JitterMs)
	}
	if opt.doDownload {
		metric("speedtest_download_mbps", "Download throughput in Mbps (overhead-compensated).", res.DownloadMbps)
		metric("speedtest_download_bytes", "Bytes counted in the download measurement window.", float64(res.DownloadBytes))
	}
	if opt.doUpload {
		metric("speedtest_upload_mbps", "Upload throughput in Mbps (overhead-compensated).", res.UploadMbps)
		metric("speedtest_upload_bytes", "Bytes counted in the upload measurement window.", float64(res.UploadBytes))
	}
	ts, err := time.Parse(time.RFC3339, res.Timestamp)
	if err == nil {
		metric("speedtest_last_run_timestamp_seconds", "Unix time of the last completed test.", float64(ts.Unix()))
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func outputHuman(w io.Writer, opt *options, res measure.Result) error {
	name := res.ServerName
	if name == "" {
		name = opt.serverURL.Host
	}
	fmt.Fprintf(w, "Server:    %s (%s)\n", name, opt.serverURL)
	if opt.doPing {
		fmt.Fprintf(w, "Ping:      %.2f ms   Jitter: %.2f ms\n", res.PingMs, res.JitterMs)
	}
	if opt.doDownload {
		fmt.Fprintf(w, "Download:  %.2f Mbps  (%s in %.1fs, %d streams)\n",
			res.DownloadMbps, humanBytes(res.DownloadBytes),
			float64(res.DownloadDurationMs)/1000, res.StreamsDownload)
	}
	if opt.doUpload {
		fmt.Fprintf(w, "Upload:    %.2f Mbps  (%s in %.1fs, %d streams)\n",
			res.UploadMbps, humanBytes(res.UploadBytes),
			float64(res.UploadDurationMs)/1000, res.StreamsUpload)
	}
	if res.ID != "" {
		fmt.Fprintf(w, "Result ID: %s\n", res.ID)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
