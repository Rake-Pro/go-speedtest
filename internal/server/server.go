// Package server builds the *http.Server with the HTTP protocol policy for
// this binary. Default is HTTP/1.1 only. When cfg.HTTP2 is set, cleartext
// HTTP/2 (h2c) is enabled via golang.org/x/net/http2/h2c with tuned frame and
// buffer settings. This binary NEVER terminates TLS; an edge proxy does.
package server

import (
	"net/http"
	"time"

	"github.com/Rake-Pro/go-speedtest/internal/config"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// h2c tuning (see DESIGN.md).
const (
	MaxReadFrameSize             = 256 << 10 // 256 KiB
	MaxUploadBufferPerConnection = 16 << 20  // 16 MiB
	MaxUploadBufferPerStream     = 8 << 20   // 8 MiB
)

// Connection timeouts. Read/Write are 0 (unlimited) because measurement
// handlers set their own per-request deadlines; these bound only idle
// connections and the header-read phase.
const (
	IdleTimeout       = 120 * time.Second
	ReadHeaderTimeout = 10 * time.Second
)

// BuildServer constructs the *http.Server for h. ReadTimeout/WriteTimeout are
// left at 0 (unlimited) because the measurement handlers set their own
// per-request deadlines via http.ResponseController; in internet mode a read
// deadline is applied per handler. When cfg.HTTP2 is true, h is wrapped with an
// h2c handler using a tuned *http2.Server.
func BuildServer(cfg *config.Config, h http.Handler) *http.Server {
	// stub: applies h1-only vs h2c policy in wave-2.
	handler := h
	if cfg.HTTP2 {
		h2s := &http2.Server{
			MaxReadFrameSize:             MaxReadFrameSize,
			MaxUploadBufferPerConnection: MaxUploadBufferPerConnection,
			MaxUploadBufferPerStream:     MaxUploadBufferPerStream,
		}
		handler = h2c.NewHandler(h, h2s)
	}
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       IdleTimeout,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
}
