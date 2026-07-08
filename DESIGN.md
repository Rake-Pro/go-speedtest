# go-speedtest — Design

Self-hosted speedtest. A single Go binary serves an embedded vanilla-JS UI plus
a native measurement API; a companion CLI binary runs the same test from a
terminal. Native wire protocol — the API is native and defined here.

- Module: `github.com/Rake-Pro/go-speedtest`
- Go: 1.26
- Binaries: `go-speedtest` (server), `go-speedtest-cli` (CLI)

## Dependency policy (frozen)

Exactly these three third-party modules are allowed, and NO others:

- `github.com/rs/zerolog` — logging
- `modernc.org/sqlite` — pure-Go SQLite driver (telemetry backend)
- `golang.org/x/net` — used for `http2`, `http2/h2c`, `websocket`

`go.mod` / `go.sum` are frozen. Implementers must not add, remove, or bump
dependencies. If a contract genuinely requires another dependency, report back;
do not edit go.mod.

## Measurement methodology (final)

Shared client-side math lives in `internal/measure` (pure, no I/O). The browser
and the CLI both implement the same procedure.

- Test shape: 15s time-based tests (configurable).
- Download: server streams N x 1 MiB incompressible random chunks. Client uses
  6 parallel streams by default (configurable 3–12).
- Upload: client generates a ~20 MB random blob and POSTs it over parallel
  streams (default 3, configurable 3–12). The server MUST fully drain the
  upload body before responding `200`.
- Grace periods: 1.5s (download) / 3s (upload). After the grace period the
  byte and time counters RESET, to discard TCP slow-start.
- Throughput: `bytes / time * 8 * overheadCompensationFactor`. Default overhead
  compensation factor is `1.06` (configurable).
- Ping: 10 samples; the reported metric is the running MINIMUM RTT.
- Jitter: asymmetric EWMA of successive RTT deltas:
  - spike (`inst > j`): `j = 0.3*j + 0.7*inst`
  - decay (`inst <= j`): `j = 0.8*j + 0.2*inst`
- Browser transport: XHR ONLY (fetch cannot measure upload wire delivery). Ping
  prefers a WebSocket echo; falls back to HTTP GET ping.

## Server performance rules (final)

- ONE 1 MiB buffer, generated with `crypto/rand` at startup
  (`internal/payload`), reused read-only for every download response. Never
  regenerate per request; never mutate.
- Download path writes with `w.Write` in a loop plus `http.Flusher.Flush` per
  chunk. NEVER `io.Copy` on the download path.
- Upload path drains via `io.Copy(io.Discard, r.Body)`.
- `http.Server` `ReadTimeout`/`WriteTimeout` = 0. Per-handler deadlines are set
  via `http.ResponseController` in internet mode.
- Payload responses set: `Cache-Control: no-store, no-cache, must-revalidate,
  max-age=0`, `Pragma: no-cache`, `X-Accel-Buffering: no`. Never compress
  payload responses.

## HTTP/2

- Default: HTTP/1.1 only.
- `-http2` opts into h2c (cleartext HTTP/2) via `golang.org/x/net/http2/h2c`
  (there is no built-in cleartext HTTP/2 in net/http). Tuned `http2.Server`:
  - `MaxReadFrameSize` = 256 KiB
  - `MaxUploadBufferPerConnection` = 16 MiB
  - `MaxUploadBufferPerStream` = 8 MiB
- No TLS in this binary EVER. An edge proxy terminates TLS.

## Modes

`-mode lan|internet` selects a profile. Individual flags override profile
defaults.

- `lan`: no limits, no rate limiting.
- `internet`:
  - per-IP concurrency cap (default 8)
  - download chunk cap (default 256 chunks per request)
  - max upload bytes (default 100 MiB, enforced with `http.MaxBytesReader`)
  - token-bucket rate limit on test starts
  - per-handler read deadlines

## Client IP resolution

`internal/clientip`. Honor `X-Forwarded-For` / `X-Real-IP` ONLY when the direct
peer (`RemoteAddr`) is inside one of the `-trusted-proxies` CIDRs. Otherwise use
`RemoteAddr`.

## Configuration

Flags + environment only. No config file. Env prefix `GOSPEEDTEST_`; a flag
always wins over the corresponding env var. Mode profile applies first, then
explicit flag/env overrides win.

## Logging

zerolog, JSON to stdout, global logger. Level via `-log-level`.

## Telemetry storage

`internal/telemetry` `Store` interface. Backends:

- `none` (default) — discards writes.
- `sqlite` — `modernc.org/sqlite`, pure Go, no cgo.

Flags: `-telemetry none|sqlite`, `-telemetry-path <file>`.

## Metrics

Hand-rolled Prometheus text exposition at `/metrics` (`internal/metrics`, no
client_golang). Atomic-backed registry with a process-wide `Default`.

- Counters: tests started / completed (by type), bytes served, bytes received.
- Gauges: active streams.

## API auth

Optional `-api-token`. When set, `Authorization: Bearer <token>` is required on
`POST /api/v1/results` and `GET /api/v1/stats`.

## CORS

Same-origin by default. `-cors` provides an origin allowlist for the API
endpoints.

## Share image

Client-side canvas in the browser. The server has NO image code.

## API surface (native)

| Method | Path | Purpose |
| ------ | ---- | ------- |
| GET  | `/` | Embedded UI (`webui` embed.FS) |
| GET  | `/config.json` | UI runtime config JSON (generated from server config) |
| GET  | `/api/v1/info` | Server info + test parameters + endpoint map (machine-readable, for CLI) |
| GET  | `/api/v1/download?chunks=N` | Streams N x 1 MiB random (default 4, capped per mode) |
| POST | `/api/v1/upload` | Drain body fully, then 200 empty |
| GET  | `/api/v1/ping` | 200 empty (HTTP ping fallback + stream warmup) |
| GET  | `/api/v1/ip` | `{"ip": "..."}` resolved via trusted-proxy rules |
| GET  | `/api/v1/ws` | WebSocket echo (`x/net/websocket`) for ping/jitter |
| POST | `/api/v1/results` | Ingest a completed result JSON (auth if token set), returns `{"id": "..."}` |
| GET  | `/api/v1/results/{id}` | Stored result JSON |
| GET  | `/api/v1/stats` | Recent results list (auth if token set) |
| GET  | `/healthz` `/readyz` | Probes |
| GET  | `/metrics` | Prometheus text |

Note: `GET /api/v1/download` default is 4 chunks. `internal/config` also carries
a `DownloadStreams` default of 6 (parallel client streams); these are distinct
knobs — chunks-per-request vs number of parallel streams.

## Result JSON schema (`measure.Result`)

Used by telemetry, API, CLI and frontend. JSON tags are frozen.

| Field | JSON | Type | Notes |
| ----- | ---- | ---- | ----- |
| ID | `id` | string | server-assigned |
| Timestamp | `timestamp` | string | RFC3339 |
| ClientIP | `client_ip` | string | optional / redactable (`omitempty`) |
| UserAgent | `user_agent` | string | `omitempty` |
| DownloadMbps | `download_mbps` | float64 | |
| UploadMbps | `upload_mbps` | float64 | |
| PingMs | `ping_ms` | float64 | |
| JitterMs | `jitter_ms` | float64 | |
| DownloadBytes | `download_bytes` | int64 | |
| UploadBytes | `upload_bytes` | int64 | |
| DownloadDurationMs | `download_duration_ms` | int64 | |
| UploadDurationMs | `upload_duration_ms` | int64 | |
| StreamsDownload | `streams_download` | int | |
| StreamsUpload | `streams_upload` | int | |
| OverheadFactor | `overhead_factor` | float64 | |
| Source | `source` | string | `web` \| `cli` \| `api` |
| ServerName | `server_name` | string | `omitempty` |

## UI runtime config (`webui.UIConfig`, served at `/config.json`)

Field names are the frozen contract between server config and the browser.

| JSON | Type | Meaning |
| ---- | ---- | ------- |
| `server_name` | string | |
| `test_duration_ms` | int64 | |
| `grace_download_ms` | int64 | |
| `grace_upload_ms` | int64 | |
| `download_streams` | int | parallel download streams (3–12) |
| `upload_streams` | int | parallel upload streams (3–12) |
| `overhead_factor` | float64 | |
| `chunk_size_bytes` | int64 | 1 MiB |
| `upload_blob_bytes` | int64 | ~20 MB client blob |
| `ping_samples` | int | |
| `download_chunks` | int | chunks per download request |
| `websocket_ping` | bool | whether to prefer the WS echo ping |
| `endpoints` | object | `{download, upload, ping, ip, ws, results}` paths |

## Package boundaries and ownership (frozen contracts)

Exported types and function signatures defined at scaffold time are FROZEN. If
an implementer believes a contract is wrong, report it back instead of changing
it. `go.mod` / `go.sum` are frozen for all agents.

- **AGENT-CORE** — owns `internal/config`, `internal/payload`,
  `internal/handlers`, `internal/server`, `internal/clientip`,
  `cmd/go-speedtest`. May adjust wiring in `main.go`. Must not touch other
  packages' files.
- **AGENT-UI** — owns `internal/webui/**` only (including the `/config.json`
  shape). The JSON field names it needs are the `webui.UIConfig` struct above.
- **AGENT-CLI** — owns `internal/measure`, `cmd/go-speedtest-cli` only.
- **AGENT-INFRA** — owns `internal/telemetry`, `internal/ratelimit`,
  `internal/metrics` only.

### Frozen signatures (scaffolded)

- `config.Load(args []string) (*config.Config, error)`; `config.Defaults() *config.Config`
- `payload.Init() error`; `payload.Buffer() []byte`; `payload.ChunkSize`
- `handlers.New(cfg *config.Config, store telemetry.Store, limiter ratelimit.Limiter, logger zerolog.Logger) http.Handler`
- `server.BuildServer(cfg *config.Config, h http.Handler) *http.Server`
- `clientip.NewResolver(cidrs []string) (clientip.Resolver, error)`; `Resolver.FromRequest(*http.Request) netip.Addr`
- `telemetry.Store` interface `{ Save(ctx, measure.Result) (string, error); Get(ctx, id) (measure.Result, error); List(ctx, limit int) ([]measure.Result, error); Close() error }`; `telemetry.NewNone() Store`; `telemetry.NewSQLite(path string) (Store, error)`
- `ratelimit.Limiter` interface `{ Acquire(ip netip.Addr) (release func(), ok bool) }`; `ratelimit.New(perIP int, ratePerSec float64, burst int) Limiter`; `ratelimit.NewNoop() Limiter`
- `metrics` package-level `Inc/Add/Set/AddGauge/Handler`, `metrics.Registry`, `metrics.NewRegistry()`, `metrics.Default`
- `webui.Handler(cfg *config.Config) http.Handler`; `webui.BuildUIConfig(cfg *config.Config) webui.UIConfig`; `webui.UIConfig`, `webui.Endpoints`
- `measure.Result`, `measure.Sample`, `measure.ThroughputMeter`, `measure.JitterEWMA`, `measure.PingStats`, and the `measure.Default*` constants
