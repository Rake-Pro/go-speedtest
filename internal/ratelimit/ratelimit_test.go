package ratelimit

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	ip1 = netip.MustParseAddr("192.0.2.1")
	ip2 = netip.MustParseAddr("192.0.2.2")
)

// fakeClock returns a limiter whose clock is manually advanced.
func fakeClock(l Limiter) (*limiter, *time.Time) {
	lim := l.(*limiter)
	cur := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	lim.now = func() time.Time { return cur }
	return lim, &cur
}

func TestNoop(t *testing.T) {
	l := NewNoop()
	for i := 0; i < 100; i++ {
		release, ok := l.Acquire(ip1)
		if !ok {
			t.Fatalf("noop Acquire #%d not ok", i)
		}
		release()
	}
}

func TestConcurrencyGate(t *testing.T) {
	l := New(2, 1000, 1000) // rate/burst high enough to never interfere

	r1, ok := l.Acquire(ip1)
	if !ok {
		t.Fatal("first Acquire not ok")
	}
	r2, ok := l.Acquire(ip1)
	if !ok {
		t.Fatal("second Acquire not ok")
	}
	if _, ok := l.Acquire(ip1); ok {
		t.Fatal("third Acquire ok, want slot exhaustion")
	}

	// Other IPs are unaffected.
	rOther, ok := l.Acquire(ip2)
	if !ok {
		t.Fatal("other-IP Acquire not ok")
	}
	rOther()

	r1()
	r1() // release is idempotent
	r3, ok := l.Acquire(ip1)
	if !ok {
		t.Fatal("Acquire after release not ok")
	}
	r3()
	r2()
}

func TestConcurrencyGateParallel(t *testing.T) {
	const perIP = 4
	l := New(perIP, 1e9, 1e6)

	var current, peak, admitted atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				release, ok := l.Acquire(ip1)
				if !ok {
					continue
				}
				admitted.Add(1)
				c := current.Add(1)
				for {
					p := peak.Load()
					if c <= p || peak.CompareAndSwap(p, c) {
						break
					}
				}
				current.Add(-1)
				release()
			}
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > perIP {
		t.Fatalf("peak concurrency %d exceeds perIP %d", p, perIP)
	}
	if admitted.Load() == 0 {
		t.Fatal("no acquisitions admitted")
	}
}

func TestTokenBucketRefill(t *testing.T) {
	lim, cur := fakeClock(New(100, 1.0, 2))

	steps := []struct {
		name    string
		advance time.Duration
		wantOK  []bool
	}{
		{"burst drains", 0, []bool{true, true, false}},
		{"one token after 1s", time.Second, []bool{true, false}},
		{"half token is not enough", 500 * time.Millisecond, []bool{false}},
		{"full second refills one", 500 * time.Millisecond, []bool{true, false}},
		{"refill caps at burst", time.Hour, []bool{true, true, false}},
	}
	for _, st := range steps {
		*cur = cur.Add(st.advance)
		for i, want := range st.wantOK {
			release, ok := lim.Acquire(ip1)
			if ok != want {
				t.Fatalf("%s: Acquire #%d ok = %v, want %v", st.name, i, ok, want)
			}
			if ok {
				release() // free the concurrency slot; tokens stay consumed
			}
		}
	}
}

func TestTokenBucketPerIP(t *testing.T) {
	lim, _ := fakeClock(New(100, 1.0, 1))

	if _, ok := lim.Acquire(ip1); !ok {
		t.Fatal("ip1 first Acquire not ok")
	}
	if _, ok := lim.Acquire(ip1); ok {
		t.Fatal("ip1 second Acquire ok, want empty bucket")
	}
	// ip2 has its own bucket.
	if _, ok := lim.Acquire(ip2); !ok {
		t.Fatal("ip2 Acquire not ok")
	}
}

func TestIdleCleanup(t *testing.T) {
	lim, cur := fakeClock(New(4, 1000, 1000))

	release, ok := lim.Acquire(ip1)
	if !ok {
		t.Fatal("Acquire not ok")
	}

	// Held entries survive the sweep even when idle for a long time.
	*cur = cur.Add(idleTTL + cleanupEvery + time.Minute)
	if _, ok := lim.Acquire(ip2); !ok {
		t.Fatal("ip2 Acquire not ok")
	}
	lim.mu.Lock()
	_, held := lim.entries[ip1]
	lim.mu.Unlock()
	if !held {
		t.Fatal("entry with active test was evicted")
	}

	release()

	// Idle released entries are evicted on the next sweep.
	*cur = cur.Add(idleTTL + cleanupEvery + time.Minute)
	if _, ok := lim.Acquire(ip2); !ok {
		t.Fatal("ip2 second Acquire not ok")
	}
	lim.mu.Lock()
	_, still := lim.entries[ip1]
	n := len(lim.entries)
	lim.mu.Unlock()
	if still {
		t.Fatal("idle entry was not evicted")
	}
	if n != 1 {
		t.Fatalf("entries map has %d entries, want 1", n)
	}
}

func TestDisabledGates(t *testing.T) {
	// perIP <= 0 disables the concurrency gate; burst <= 0 disables the bucket.
	l := New(0, 0, 0)
	for i := 0; i < 50; i++ {
		if _, ok := l.Acquire(ip1); !ok {
			t.Fatalf("Acquire #%d not ok with gates disabled", i)
		}
	}
}
