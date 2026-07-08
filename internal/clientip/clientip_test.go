package clientip

import (
	"net/http"
	"testing"
)

func TestFromRequest(t *testing.T) {
	trusted := []string{"10.0.0.0/8", "192.168.0.0/16"}

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		want       string
	}{
		{
			name:       "untrusted peer ignores XFF",
			remoteAddr: "203.0.113.5:44321",
			xff:        "1.2.3.4",
			want:       "203.0.113.5",
		},
		{
			name:       "trusted peer honors single XFF",
			remoteAddr: "10.1.2.3:5000",
			xff:        "198.51.100.7",
			want:       "198.51.100.7",
		},
		{
			name:       "trusted peer honors X-Real-IP when no XFF",
			remoteAddr: "10.1.2.3:5000",
			xRealIP:    "198.51.100.9",
			want:       "198.51.100.9",
		},
		{
			name:       "multi-hop returns rightmost untrusted",
			remoteAddr: "10.0.0.1:5000",
			xff:        "198.51.100.1, 203.0.113.9, 10.0.0.2, 192.168.1.1",
			want:       "203.0.113.9",
		},
		{
			name:       "all hops trusted falls back to leftmost",
			remoteAddr: "10.0.0.1:5000",
			xff:        "10.5.5.5, 192.168.2.2",
			want:       "10.5.5.5",
		},
		{
			name:       "untrusted peer no headers",
			remoteAddr: "8.8.8.8:1234",
			want:       "8.8.8.8",
		},
	}

	rv, err := NewResolver(trusted)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xRealIP != "" {
				r.Header.Set("X-Real-IP", tt.xRealIP)
			}
			got := rv.FromRequest(r)
			if got.String() != tt.want {
				t.Errorf("FromRequest = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestNewResolverBadCIDR(t *testing.T) {
	if _, err := NewResolver([]string{"nope"}); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}
