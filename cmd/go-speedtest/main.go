// Command go-speedtest is the self-hosted speedtest server: it serves the
// embedded UI and the native measurement API from a single binary.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Rake-Pro/go-speedtest/internal/config"
	"github.com/Rake-Pro/go-speedtest/internal/handlers"
	"github.com/Rake-Pro/go-speedtest/internal/metrics"
	"github.com/Rake-Pro/go-speedtest/internal/payload"
	"github.com/Rake-Pro/go-speedtest/internal/ratelimit"
	"github.com/Rake-Pro/go-speedtest/internal/server"
	"github.com/Rake-Pro/go-speedtest/internal/telemetry"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// shutdownTimeout bounds the graceful-drain window on SIGINT/SIGTERM.
const shutdownTimeout = 20 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Error().Err(err).Msg("fatal")
		os.Exit(1)
	}
}

// run parses configuration, wires every component, and serves.
func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	setupLogging(cfg)
	warnIfUnguarded(cfg)

	if err := payload.Init(); err != nil {
		return err
	}

	store, err := newStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	limiter := newLimiter(cfg)

	// Pre-register the metric series on the default registry.
	_ = metrics.Default

	h := handlers.New(cfg, store, limiter, log.Logger)
	srv := server.BuildServer(cfg, h)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Info().Str("listen", cfg.Listen).Str("mode", cfg.Mode).Msg("starting go-speedtest")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info().Msg("shutdown signal received, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("graceful shutdown failed, forcing close")
			_ = srv.Close()
			return err
		}
		return nil
	}
}

func setupLogging(cfg *config.Config) {
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	lvl, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
}

func newStore(cfg *config.Config) (telemetry.Store, error) {
	switch cfg.TelemetryBackend {
	case config.TelemetrySQLite:
		return telemetry.NewSQLite(cfg.TelemetryPath)
	case config.TelemetryNone, "":
		return telemetry.NewNone(), nil
	default:
		return nil, errors.New("unknown telemetry backend: " + cfg.TelemetryBackend)
	}
}

// newLimiter builds the admission limiter from the effective limits rather than
// from the mode name. Gating on cfg.Mode alone silently dropped an explicit
// -per-ip-concurrency / -rate-limit-burst override in lan mode, even though the
// config layer preserves such overrides. Both zero => nothing to enforce, so
// use the no-op limiter (the real one would admit everything anyway).
func newLimiter(cfg *config.Config) ratelimit.Limiter {
	if cfg.PerIPConcurrency > 0 || cfg.RateLimitBurst > 0 {
		return ratelimit.New(cfg.PerIPConcurrency, cfg.RateLimitPerSec, cfg.RateLimitBurst)
	}
	return ratelimit.NewNoop()
}

// warnIfUnguarded logs a startup warning when the effective config leaves every
// abuse guardrail disabled. The lan profile zeroes them by design, so this makes
// a missing -mode internet visible in the log instead of silent.
func warnIfUnguarded(cfg *config.Config) {
	if cfg.PerIPConcurrency > 0 || cfg.ChunkCap > 0 || cfg.MaxUploadBytes > 0 ||
		cfg.RateLimitBurst > 0 || cfg.ReadDeadline > 0 {
		return
	}
	log.Warn().
		Str("mode", cfg.Mode).
		Msg("no abuse guardrails active: per-IP concurrency, chunk cap, upload cap, rate limit and read deadline are all disabled; use -mode internet, or set the individual limits, if this server is reachable beyond a trusted LAN")
}
