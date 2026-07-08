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

// run parses configuration, wires every component, and serves. Logic inside
// each component is stubbed for wave-2; this wiring compiles and is the shape
// AGENT-CORE fills in.
func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	setupLogging(cfg)

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

func newLimiter(cfg *config.Config) ratelimit.Limiter {
	if cfg.Mode == config.ModeInternet {
		return ratelimit.New(cfg.PerIPConcurrency, cfg.RateLimitPerSec, cfg.RateLimitBurst)
	}
	return ratelimit.NewNoop()
}
