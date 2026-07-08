# go-speedtest

Self-hosted speedtest in a single Go binary: it serves an embedded vanilla-JS UI
and a native measurement API. A companion CLI (`go-speedtest-cli`) runs the same
test from a terminal. No TLS in the binary — put it behind an edge proxy.

See [DESIGN.md](DESIGN.md) for the full methodology, API contract and package
ownership map.

## Build

```
go build ./...
```

Binaries:

- `cmd/go-speedtest` — the server (`go-speedtest`)
- `cmd/go-speedtest-cli` — the CLI (`go-speedtest-cli`)

## Run

```
go run ./cmd/go-speedtest -listen :8080 -mode lan
```

## Flags

Flags may also be set via environment variables with the `GOSPEEDTEST_` prefix
(e.g. `-log-level` -> `GOSPEEDTEST_LOG_LEVEL`). A flag wins over its env var.

| Flag | Default | Purpose |
| ---- | ------- | ------- |
| `-listen` | `:8080` | host:port to bind |
| `-mode` | `lan` | profile: `lan` or `internet` |
| `-http2` | `false` | opt into cleartext HTTP/2 (h2c) |
| `-server-name` | `go-speedtest` | name reported in results and UI |
| `-log-level` | `info` | zerolog level |
| `-trusted-proxies` | (none) | CIDR list whose XFF/X-Real-IP is trusted |
| `-telemetry` | `none` | telemetry backend: `none` or `sqlite` |
| `-telemetry-path` | `speedtest.db` | sqlite db file |
| `-api-token` | (none) | require Bearer token on results/stats when set |
| `-cors` | (none) | allowed origins for API endpoints |
| `-overhead` | `1.06` | throughput overhead compensation factor |
| `-download-streams` | `6` | parallel download streams (3–12) |
| `-upload-streams` | `3` | parallel upload streams (3–12) |
| `-test-duration` | `15s` | test duration |
| `-grace-download` | `1.5s` | download grace period (counter reset) |
| `-grace-upload` | `3s` | upload grace period (counter reset) |
| `-ping-samples` | `10` | ping sample count |
| `-per-ip-concurrency` | `8` | internet mode: concurrent tests per IP |
| `-chunk-cap` | `256` | internet mode: max chunks per download request |
| `-max-upload-bytes` | `104857600` | internet mode: upload byte cap (100 MiB) |
| `-rate-limit` | `5` | internet mode: token-bucket refill (starts/sec) |
| `-rate-limit-burst` | `10` | internet mode: token-bucket capacity |
| `-read-deadline` | `30s` | internet mode: per-handler read deadline |

## API

| Method | Path | Purpose |
| ------ | ---- | ------- |
| GET  | `/` | Embedded UI |
| GET  | `/config.json` | UI runtime config JSON |
| GET  | `/api/v1/info` | Server info + test parameters + endpoint map |
| GET  | `/api/v1/download?chunks=N` | Stream N x 1 MiB random chunks |
| POST | `/api/v1/upload` | Drain body, then 200 empty |
| GET  | `/api/v1/ping` | 200 empty (ping fallback + warmup) |
| GET  | `/api/v1/ip` | `{"ip": "..."}` |
| GET  | `/api/v1/ws` | WebSocket echo for ping/jitter |
| POST | `/api/v1/results` | Ingest a result (auth if token set) |
| GET  | `/api/v1/results/{id}` | Stored result JSON |
| GET  | `/api/v1/stats` | Recent results (auth if token set) |
| GET  | `/healthz` `/readyz` | Probes |
| GET  | `/metrics` | Prometheus text |
