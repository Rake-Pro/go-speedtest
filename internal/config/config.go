// Package config defines the full runtime configuration for the server and the
// flag+env parsing that produces it. Sources are flags and environment
// variables only (no config file). Env vars use the prefix GOSPEEDTEST_ and a
// flag always wins over the corresponding env var.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/Rake-Pro/go-speedtest/internal/measure"
)

// Mode profiles.
const (
	ModeLAN      = "lan"
	ModeInternet = "internet"
)

// Telemetry backends.
const (
	TelemetryNone   = "none"
	TelemetrySQLite = "sqlite"
)

// EnvPrefix is prepended to the upper-cased flag name (dashes -> underscores)
// to form the environment variable, e.g. -log-level -> GOSPEEDTEST_LOG_LEVEL.
const EnvPrefix = "GOSPEEDTEST_"

// Config is the fully-resolved server configuration. Every command-line flag
// has a corresponding field here.
type Config struct {
	// Network / server
	Listen     string // -listen, host:port to bind
	Mode       string // -mode, lan | internet
	HTTP2      bool   // -http2, opt into h2c (cleartext HTTP/2)
	ServerName string // -server-name, reported in Result and UI

	// Logging
	LogLevel string // -log-level

	// Client-IP resolution
	TrustedProxies []string // -trusted-proxies, CIDR list; XFF/X-Real-IP honored only from these peers

	// Telemetry
	TelemetryBackend string // -telemetry, none | sqlite
	TelemetryPath    string // -telemetry-path, sqlite db file

	// API auth / CORS
	APIToken    string   // -api-token, required Bearer token when non-empty
	CORSOrigins []string // -cors, allowed origins for API endpoints

	// Measurement methodology (surfaced to the UI via /config.json)
	OverheadFactor  float64       // -overhead, throughput overhead compensation factor
	DownloadStreams int           // -download-streams, 3..12
	UploadStreams   int           // -upload-streams, 3..12
	TestDuration    time.Duration // -test-duration
	GraceDownload   time.Duration // -grace-download
	GraceUpload     time.Duration // -grace-upload
	PingSamples     int           // -ping-samples

	// Internet-mode limits (ignored in lan mode unless explicitly set)
	PerIPConcurrency int           // -per-ip-concurrency, concurrent tests per client IP
	ChunkCap         int           // -chunk-cap, max chunks per download request
	MaxUploadBytes   int64         // -max-upload-bytes, http.MaxBytesReader limit
	RateLimitPerSec  float64       // -rate-limit, token-bucket refill (test starts / sec)
	RateLimitBurst   int           // -rate-limit-burst, token-bucket capacity
	ReadDeadline     time.Duration // -read-deadline, per-handler read deadline (internet mode)
}

// ErrNotImplemented is returned by stubbed logic until wave-2 fills it in.
var ErrNotImplemented = errors.New("not implemented")

// Defaults returns a Config populated with the documented default values,
// before flag/env overrides and before mode-profile application.
func Defaults() *Config {
	return &Config{
		Listen:           ":8080",
		Mode:             ModeLAN,
		HTTP2:            false,
		ServerName:       "go-speedtest",
		LogLevel:         "info",
		TrustedProxies:   nil,
		TelemetryBackend: TelemetryNone,
		TelemetryPath:    "speedtest.db",
		APIToken:         "",
		CORSOrigins:      nil,
		OverheadFactor:   1.06,
		DownloadStreams:  6,
		UploadStreams:    3,
		TestDuration:     15 * time.Second,
		GraceDownload:    1500 * time.Millisecond,
		GraceUpload:      3000 * time.Millisecond,
		PingSamples:      10,
		PerIPConcurrency: 8,
		ChunkCap:         256,
		MaxUploadBytes:   100 << 20, // 100 MiB
		RateLimitPerSec:  5,
		RateLimitBurst:   10,
		ReadDeadline:     30 * time.Second,
	}
}

// Load parses flags (from args) and environment variables into a Config,
// applies the mode profile, and validates. Flags win over env vars; explicitly
// set flags win over mode-profile defaults.
func Load(args []string) (*Config, error) {
	cfg := Defaults()
	fs := flag.NewFlagSet("go-speedtest", flag.ContinueOnError)
	registerFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// Track which flags were explicitly set on the command line.
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// Merge environment for flags not set on the command line. The flag's own
	// Value.Set does the type parsing, so cfg is updated in place.
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if envErr != nil || explicit[f.Name] {
			return
		}
		key := EnvPrefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		v, ok := os.LookupEnv(key)
		if !ok {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			envErr = fmt.Errorf("env %s: %w", key, err)
			return
		}
		explicit[f.Name] = true
	})
	if envErr != nil {
		return nil, envErr
	}

	applyModeProfile(cfg, explicit)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// listFlag is a flag.Value that accumulates a comma-separated string list.
type listFlag struct{ p *[]string }

func (l listFlag) String() string {
	if l.p == nil {
		return ""
	}
	return strings.Join(*l.p, ",")
}

func (l listFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l.p = append(*l.p, part)
		}
	}
	return nil
}

// registerFlags binds every configuration flag onto fs. It is intentionally
// separated so Load and any help/usage tooling share one definition.
func registerFlags(fs *flag.FlagSet, cfg *Config) {
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "host:port to bind")
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "profile: lan | internet")
	fs.BoolVar(&cfg.HTTP2, "http2", cfg.HTTP2, "opt into cleartext HTTP/2 (h2c)")
	fs.StringVar(&cfg.ServerName, "server-name", cfg.ServerName, "server name reported to clients")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "zerolog level: trace|debug|info|warn|error|fatal|panic")
	fs.Var(listFlag{&cfg.TrustedProxies}, "trusted-proxies", "comma-separated trusted-proxy CIDRs for XFF/X-Real-IP")
	fs.StringVar(&cfg.TelemetryBackend, "telemetry", cfg.TelemetryBackend, "telemetry backend: none | sqlite")
	fs.StringVar(&cfg.TelemetryPath, "telemetry-path", cfg.TelemetryPath, "sqlite database file path")
	fs.StringVar(&cfg.APIToken, "api-token", cfg.APIToken, "bearer token required on results ingest / stats when set")
	fs.Var(listFlag{&cfg.CORSOrigins}, "cors", "comma-separated allowed origins for /api endpoints")
	fs.Float64Var(&cfg.OverheadFactor, "overhead", cfg.OverheadFactor, "throughput overhead compensation factor")
	fs.IntVar(&cfg.DownloadStreams, "download-streams", cfg.DownloadStreams, "parallel download streams (3..12)")
	fs.IntVar(&cfg.UploadStreams, "upload-streams", cfg.UploadStreams, "parallel upload streams (3..12)")
	fs.DurationVar(&cfg.TestDuration, "test-duration", cfg.TestDuration, "per-direction test duration")
	fs.DurationVar(&cfg.GraceDownload, "grace-download", cfg.GraceDownload, "download grace period before counters reset")
	fs.DurationVar(&cfg.GraceUpload, "grace-upload", cfg.GraceUpload, "upload grace period before counters reset")
	fs.IntVar(&cfg.PingSamples, "ping-samples", cfg.PingSamples, "number of ping samples")
	fs.IntVar(&cfg.PerIPConcurrency, "per-ip-concurrency", cfg.PerIPConcurrency, "internet mode: concurrent tests per client IP")
	fs.IntVar(&cfg.ChunkCap, "chunk-cap", cfg.ChunkCap, "internet mode: max chunks per download request")
	fs.Int64Var(&cfg.MaxUploadBytes, "max-upload-bytes", cfg.MaxUploadBytes, "internet mode: max upload body bytes")
	fs.Float64Var(&cfg.RateLimitPerSec, "rate-limit", cfg.RateLimitPerSec, "internet mode: token-bucket refill (test starts/sec)")
	fs.IntVar(&cfg.RateLimitBurst, "rate-limit-burst", cfg.RateLimitBurst, "internet mode: token-bucket capacity")
	fs.DurationVar(&cfg.ReadDeadline, "read-deadline", cfg.ReadDeadline, "internet mode: per-handler read deadline")
}

// applyModeProfile applies the profile baseline. In lan mode every limit is
// disabled unless the operator explicitly set it. internet mode keeps the
// Defaults() values (which are the internet baseline).
func applyModeProfile(cfg *Config, explicit map[string]bool) {
	if cfg.Mode != ModeLAN {
		return
	}
	if !explicit["per-ip-concurrency"] {
		cfg.PerIPConcurrency = 0
	}
	if !explicit["chunk-cap"] {
		cfg.ChunkCap = 0
	}
	if !explicit["max-upload-bytes"] {
		cfg.MaxUploadBytes = 0
	}
	if !explicit["rate-limit"] {
		cfg.RateLimitPerSec = 0
	}
	if !explicit["rate-limit-burst"] {
		cfg.RateLimitBurst = 0
	}
	if !explicit["read-deadline"] {
		cfg.ReadDeadline = 0
	}
}

var validLogLevels = map[string]bool{
	"trace": true, "debug": true, "info": true, "warn": true,
	"error": true, "fatal": true, "panic": true,
}

func validate(cfg *Config) error {
	if cfg.Listen == "" {
		return errors.New("listen address must not be empty")
	}
	if cfg.Mode != ModeLAN && cfg.Mode != ModeInternet {
		return fmt.Errorf("invalid mode %q (want lan|internet)", cfg.Mode)
	}
	if cfg.TelemetryBackend != TelemetryNone && cfg.TelemetryBackend != TelemetrySQLite {
		return fmt.Errorf("invalid telemetry backend %q (want none|sqlite)", cfg.TelemetryBackend)
	}
	if cfg.TelemetryBackend == TelemetrySQLite && cfg.TelemetryPath == "" {
		return errors.New("telemetry-path must be set when telemetry=sqlite")
	}
	if !validLogLevels[strings.ToLower(cfg.LogLevel)] {
		return fmt.Errorf("invalid log-level %q", cfg.LogLevel)
	}
	if cfg.OverheadFactor <= 0 {
		return fmt.Errorf("overhead must be > 0, got %v", cfg.OverheadFactor)
	}
	if cfg.DownloadStreams < measure.MinConfigurableStreams || cfg.DownloadStreams > measure.MaxConfigurableStreams {
		return fmt.Errorf("download-streams must be in [%d,%d], got %d", measure.MinConfigurableStreams, measure.MaxConfigurableStreams, cfg.DownloadStreams)
	}
	if cfg.UploadStreams < measure.MinConfigurableStreams || cfg.UploadStreams > measure.MaxConfigurableStreams {
		return fmt.Errorf("upload-streams must be in [%d,%d], got %d", measure.MinConfigurableStreams, measure.MaxConfigurableStreams, cfg.UploadStreams)
	}
	if cfg.TestDuration <= 0 {
		return fmt.Errorf("test-duration must be > 0, got %v", cfg.TestDuration)
	}
	if cfg.GraceDownload < 0 || cfg.GraceUpload < 0 {
		return errors.New("grace periods must be >= 0")
	}
	if cfg.PingSamples <= 0 {
		return fmt.Errorf("ping-samples must be > 0, got %d", cfg.PingSamples)
	}
	if cfg.PerIPConcurrency < 0 {
		return fmt.Errorf("per-ip-concurrency must be >= 0, got %d", cfg.PerIPConcurrency)
	}
	if cfg.ChunkCap < 0 {
		return fmt.Errorf("chunk-cap must be >= 0, got %d", cfg.ChunkCap)
	}
	if cfg.MaxUploadBytes < 0 {
		return fmt.Errorf("max-upload-bytes must be >= 0, got %d", cfg.MaxUploadBytes)
	}
	if cfg.RateLimitPerSec < 0 {
		return fmt.Errorf("rate-limit must be >= 0, got %v", cfg.RateLimitPerSec)
	}
	if cfg.RateLimitBurst < 0 {
		return fmt.Errorf("rate-limit-burst must be >= 0, got %d", cfg.RateLimitBurst)
	}
	if cfg.ReadDeadline < 0 {
		return fmt.Errorf("read-deadline must be >= 0, got %v", cfg.ReadDeadline)
	}
	for _, c := range cfg.TrustedProxies {
		if _, err := netip.ParsePrefix(c); err != nil {
			return fmt.Errorf("invalid trusted-proxies CIDR %q: %w", c, err)
		}
	}
	return nil
}
