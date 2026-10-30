package resourcelease_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rl "github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease/leasetest"
)

func TestHoldHeartbeatsAndClampsInterval(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		30 * time.Second:      20 * time.Second,
		20 * time.Second:      20 * time.Second,
		0:                     20 * time.Second,
		-time.Second:          20 * time.Second,
		30 * time.Millisecond: 30 * time.Millisecond,
	} {
		if got := rl.HoldIntervalForTest(in); got != want {
			t.Fatalf("holdInterval(%v) = %v, want %v", in, got, want)
		}
	}

	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	stop := c.Hold(l, 30*time.Millisecond, nil, nil)
	eventually(t, 2*time.Second, func() bool { return len(heartbeats(f)) >= 3 }, "no heartbeats every 30 ms")
	stop()
	stop() // idempotent
	n := len(heartbeats(f))
	time.Sleep(90 * time.Millisecond)
	if got := len(heartbeats(f)); got != n {
		t.Fatalf("%d heartbeats after stop returned", got-n)
	}
	for _, r := range heartbeats(f) {
		if r.Path != leasesPath+"/"+l.ID+"/heartbeat" || r.Body != nil {
			t.Fatalf("heartbeat %+v", r)
		}
	}
}

func TestHoldInfoFalseSendsEmptyBody(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	var calls atomic.Int32
	stop := c.Hold(l, 20*time.Millisecond, func() (rl.HeartbeatInfo, bool) {
		// Odd calls report info, even ones decline.
		if calls.Add(1)%2 == 1 {
			return rl.HeartbeatInfo{InUse: 2, Loaded: true}, true
		}
		return rl.HeartbeatInfo{InUse: 9}, false
	}, nil)
	eventually(t, 2*time.Second, func() bool { return len(heartbeats(f)) >= 4 }, "no heartbeats")
	stop()
	hbs := heartbeats(f)
	for i, r := range hbs[:4] {
		if i%2 == 0 {
			if r.Body == nil || r.Body["in_use"] != float64(2) || r.Body["loaded"] != true {
				t.Fatalf("heartbeat %d body %v, want the info", i, r.Body)
			}
		} else if r.Body != nil {
			t.Fatalf("heartbeat %d body %v, want none", i, r.Body)
		}
	}
}

func TestHoldOnLostRunsOnceOffTheHoldGoroutine(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	var lost atomic.Int32
	inLost, unblock := make(chan *rl.Lease, 4), make(chan struct{})
	var stop func()
	var stopMu sync.Mutex
	stopMu.Lock()
	stop = c.Hold(l, 15*time.Millisecond, nil, func(got *rl.Lease) {
		lost.Add(1)
		inLost <- got
		<-unblock // a slow onLost must not hold up the hold goroutine
		stopMu.Lock()
		stop() // stop from onLost is safe
		stopMu.Unlock()
	})
	stopMu.Unlock()
	eventually(t, 2*time.Second, func() bool { return len(heartbeats(f)) >= 1 }, "no heartbeat")
	f.Drop(l.ID)

	var got *rl.Lease
	select {
	case got = <-inLost:
	case <-time.After(2 * time.Second):
		t.Fatal("onLost not called")
	}
	if got != l {
		t.Fatal("onLost got another lease")
	}
	// The hold goroutine has already returned although onLost still runs.
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop blocked while onLost runs: onLost is on the hold goroutine")
	}
	n := len(heartbeats(f))
	time.Sleep(60 * time.Millisecond)
	close(unblock)
	time.Sleep(20 * time.Millisecond)
	if lost.Load() != 1 {
		t.Fatalf("onLost called %d times", lost.Load())
	}
	if got := len(heartbeats(f)); got != n {
		t.Fatal("heartbeats after the lease was lost")
	}
	if s, _ := l.Snapshot(); s != rl.StateLost || rl.RegisteredForTest(c, l.ID) {
		t.Fatalf("state %q", s)
	}
	stop()
}

func TestHoldStopsOnAReleasedLease(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	var lost atomic.Int32
	stop := c.Hold(l, 15*time.Millisecond, nil, func(*rl.Lease) { lost.Add(1) })
	defer stop()
	eventually(t, 2*time.Second, func() bool { return len(heartbeats(f)) >= 1 }, "no heartbeat")
	c.Release(context.Background(), l)
	time.Sleep(30 * time.Millisecond)
	n := len(heartbeats(f))
	time.Sleep(60 * time.Millisecond)
	if got := len(heartbeats(f)); got != n || lost.Load() != 0 {
		t.Fatalf("hold kept going on a released lease: %d more heartbeats, %d onLost", got-n, lost.Load())
	}
}

// countingTransport tracks how many requests are in flight at once.
type countingTransport struct {
	base          http.RoundTripper
	cur, max, all atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	n := c.cur.Add(1)
	c.all.Add(1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	defer c.cur.Add(-1)
	return c.base.RoundTrip(r)
}

func TestHoldPerHeartbeatTimeout(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	opts.BookkeepingTimeout = 0 // only the per-heartbeat timeout bounds it
	ct := &countingTransport{base: &http.Transport{}}
	opts.HTTPClient = &http.Client{Timeout: 35 * time.Second, Transport: ct}
	c := newClient(t, opts)
	l := held(t, c)
	ct.max.Store(0)
	ct.all.Store(0)

	f.Hang(time.Second)
	stop := c.Hold(l, 50*time.Millisecond, nil, nil)
	time.Sleep(450 * time.Millisecond)
	stop()
	if m := ct.max.Load(); m != 1 {
		t.Fatalf("%d heartbeats in flight at once, want 1", m)
	}
	// Each heartbeat gives up after 50 ms, so several went out in 450 ms
	// although the core hangs for 1 s.
	if n := ct.all.Load(); n < 3 {
		t.Fatalf("only %d heartbeats in 450 ms: no per-heartbeat timeout", n)
	}
	if s, _ := l.Snapshot(); s != rl.StateHeld {
		t.Fatalf("a timed-out heartbeat changed the state to %q", s)
	}
}
