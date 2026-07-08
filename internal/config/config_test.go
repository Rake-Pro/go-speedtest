package config

import "testing"

func TestModeProfileApplication(t *testing.T) {
	tests := []struct {
		name             string
		args             []string
		wantChunkCap     int
		wantMaxUpload    int64
		wantPerIP        int
		wantRatePerSec   float64
		wantReadDeadline bool // true => non-zero
	}{
		{
			name:             "lan disables limits",
			args:             []string{"-mode", "lan"},
			wantChunkCap:     0,
			wantMaxUpload:    0,
			wantPerIP:        0,
			wantRatePerSec:   0,
			wantReadDeadline: false,
		},
		{
			name:             "internet keeps defaults",
			args:             []string{"-mode", "internet"},
			wantChunkCap:     256,
			wantMaxUpload:    100 << 20,
			wantPerIP:        8,
			wantRatePerSec:   5,
			wantReadDeadline: true,
		},
		{
			name:             "lan with explicit chunk-cap override",
			args:             []string{"-mode", "lan", "-chunk-cap", "42"},
			wantChunkCap:     42,
			wantMaxUpload:    0,
			wantPerIP:        0,
			wantRatePerSec:   0,
			wantReadDeadline: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.args)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ChunkCap != tt.wantChunkCap {
				t.Errorf("ChunkCap = %d, want %d", cfg.ChunkCap, tt.wantChunkCap)
			}
			if cfg.MaxUploadBytes != tt.wantMaxUpload {
				t.Errorf("MaxUploadBytes = %d, want %d", cfg.MaxUploadBytes, tt.wantMaxUpload)
			}
			if cfg.PerIPConcurrency != tt.wantPerIP {
				t.Errorf("PerIPConcurrency = %d, want %d", cfg.PerIPConcurrency, tt.wantPerIP)
			}
			if cfg.RateLimitPerSec != tt.wantRatePerSec {
				t.Errorf("RateLimitPerSec = %v, want %v", cfg.RateLimitPerSec, tt.wantRatePerSec)
			}
			if (cfg.ReadDeadline != 0) != tt.wantReadDeadline {
				t.Errorf("ReadDeadline = %v, want non-zero=%v", cfg.ReadDeadline, tt.wantReadDeadline)
			}
		})
	}
}

func TestEnvMergeFlagWins(t *testing.T) {
	t.Setenv("GOSPEEDTEST_LISTEN", ":9999")
	t.Setenv("GOSPEEDTEST_SERVER_NAME", "from-env")

	// Flag overrides env for listen; env applies for server-name.
	cfg, err := Load([]string{"-listen", ":7000"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":7000" {
		t.Errorf("Listen = %q, want :7000 (flag wins over env)", cfg.Listen)
	}
	if cfg.ServerName != "from-env" {
		t.Errorf("ServerName = %q, want from-env", cfg.ServerName)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"bad mode", []string{"-mode", "bogus"}, true},
		{"bad telemetry", []string{"-telemetry", "mysql"}, true},
		{"streams too low", []string{"-download-streams", "1"}, true},
		{"streams too high", []string{"-upload-streams", "99"}, true},
		{"bad cidr", []string{"-trusted-proxies", "not-a-cidr"}, true},
		{"good cidr", []string{"-trusted-proxies", "10.0.0.0/8,192.168.0.0/16"}, false},
		{"negative overhead", []string{"-overhead", "-1"}, true},
		{"valid defaults", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("Load(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
		})
	}
}
