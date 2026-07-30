package main

import (
	"net/netip"
	"testing"

	"github.com/Rake-Pro/go-speedtest/internal/config"
)

func TestNewLimiterHonorsExplicitLanOverrides(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantLimited bool // second concurrent Acquire for the same IP is refused
	}{
		{name: "lan default admits everything", args: []string{"-mode", "lan"}, wantLimited: false},
		{name: "internet default limits", args: []string{"-mode", "internet", "-per-ip-concurrency", "1"}, wantLimited: true},
		{name: "lan with explicit per-ip-concurrency limits", args: []string{"-mode", "lan", "-per-ip-concurrency", "1"}, wantLimited: true},
	}
	ip := netip.MustParseAddr("192.0.2.10")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Load(tt.args)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			l := newLimiter(cfg)
			release, ok := l.Acquire(ip)
			if !ok {
				t.Fatal("first Acquire refused")
			}
			defer release()
			if _, ok := l.Acquire(ip); ok == tt.wantLimited {
				t.Errorf("second Acquire ok = %v, want %v", ok, !tt.wantLimited)
			}
		})
	}
}
