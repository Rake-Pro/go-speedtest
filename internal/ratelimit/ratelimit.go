// Package ratelimit provides the internet-mode admission control for test
// starts: a per-IP concurrency gate combined with a per-IP token-bucket rate
// limit. In lan mode a no-op limiter is used so there is no overhead.
package ratelimit

import (
	"net/netip"
	"sync"
	"time"
)

// Limiter decides whether a client may start a test right now. Acquire returns
// a release func (to be deferred by the caller when ok is true) and a boolean
// indicating admission. When ok is false the release func is a no-op.
type Limiter interface {
	Acquire(ip netip.Addr) (release func(), ok bool)
}

// NewNoop returns a Limiter that always admits (used in lan mode).
func NewNoop() Limiter { return noop{} }

type noop struct{}

func (noop) Acquire(ip netip.Addr) (func(), bool) {
	_ = ip
	return func() {}, true
}

const (
	// idleTTL is how long an IP entry with no active tests may sit unused
	// before being evicted from the map.
	idleTTL = 5 * time.Minute
	// cleanupEvery bounds how often Acquire sweeps for idle entries.
	cleanupEvery = time.Minute
)

// New builds the internet-mode limiter: at most perIP concurrent tests per
// client IP, plus a per-IP token bucket refilling at ratePerSec with capacity
// burst on test starts. perIP <= 0 disables the concurrency gate; burst <= 0
// disables the token bucket.
func New(perIP int, ratePerSec float64, burst int) Limiter {
	return &limiter{
		perIP:   perIP,
		rate:    ratePerSec,
		burst:   float64(burst),
		now:     time.Now,
		entries: make(map[netip.Addr]*entry),
	}
}

type entry struct {
	active     int       // in-flight tests for this IP
	tokens     float64   // token bucket level
	lastRefill time.Time // last time tokens were topped up
	lastSeen   time.Time // last Acquire touching this entry
}

type limiter struct {
	perIP int
	rate  float64
	burst float64
	now   func() time.Time // injectable for tests

	mu          sync.Mutex
	entries     map[netip.Addr]*entry
	nextCleanup time.Time
}

func (l *limiter) Acquire(ip netip.Addr) (func(), bool) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	// Lazy sweep of idle entries so the map cannot grow unbounded.
	if now.After(l.nextCleanup) {
		for k, e := range l.entries {
			if e.active == 0 && now.Sub(e.lastSeen) > idleTTL {
				delete(l.entries, k)
			}
		}
		l.nextCleanup = now.Add(cleanupEvery)
	}

	e := l.entries[ip]
	if e == nil {
		e = &entry{tokens: l.burst, lastRefill: now}
		l.entries[ip] = e
	}
	e.lastSeen = now

	if elapsed := now.Sub(e.lastRefill); elapsed > 0 {
		e.tokens = min(l.burst, e.tokens+elapsed.Seconds()*l.rate)
		e.lastRefill = now
	}

	if l.perIP > 0 && e.active >= l.perIP {
		return func() {}, false
	}
	if l.burst > 0 && e.tokens < 1 {
		return func() {}, false
	}
	if l.burst > 0 {
		e.tokens--
	}
	e.active++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			e.active--
			l.mu.Unlock()
		})
	}, true
}
