// Package clientip resolves the real client IP of an HTTP request. Forwarded
// headers (X-Forwarded-For, X-Real-IP) are honored ONLY when the direct peer
// (RemoteAddr) falls within one of the configured trusted-proxy CIDRs;
// otherwise the direct peer address is used.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver resolves client IPs against a set of trusted proxy networks.
type Resolver struct {
	// TrustedCIDRs is the set of networks whose forwarded headers are trusted.
	TrustedCIDRs []netip.Prefix
}

// NewResolver parses the given CIDR strings into a Resolver.
func NewResolver(cidrs []string) (Resolver, error) {
	var prefixes []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return Resolver{}, err
		}
		prefixes = append(prefixes, p.Masked())
	}
	return Resolver{TrustedCIDRs: prefixes}, nil
}

// FromRequest returns the resolved client address for r. When the direct peer
// is trusted it consults X-Forwarded-For / X-Real-IP; otherwise it returns the
// RemoteAddr. Returns the zero netip.Addr if nothing parses.
func (rv Resolver) FromRequest(r *http.Request) netip.Addr {
	peer := parseAddr(r.RemoteAddr)
	if !peer.IsValid() || !rv.trusted(peer) {
		return peer
	}

	// Peer is a trusted proxy: honor forwarded headers.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// Walk right-to-left and return the rightmost address that is not one of
		// our trusted proxies: that is the closest node we cannot vouch for.
		for i := len(parts) - 1; i >= 0; i-- {
			a := parseAddr(parts[i])
			if !a.IsValid() {
				continue
			}
			if rv.trusted(a) {
				continue
			}
			return a
		}
		// Every hop was trusted: the originating client is the leftmost entry.
		for i := 0; i < len(parts); i++ {
			if a := parseAddr(parts[i]); a.IsValid() {
				return a
			}
		}
	}

	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		if a := parseAddr(xr); a.IsValid() {
			return a
		}
	}

	return peer
}

func (rv Resolver) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range rv.TrustedCIDRs {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseAddr extracts a netip.Addr from either a "host:port" RemoteAddr or a
// bare address (as found in forwarded headers). Zones and surrounding spaces
// are stripped; the result is unmapped.
func parseAddr(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}
