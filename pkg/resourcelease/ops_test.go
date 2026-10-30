package resourcelease_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	rl "github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease/leasetest"
)

// held acquires a held lease from f.
func held(t *testing.T, c *rl.Client) *rl.Lease {
	t.Helper()
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeHeld {
		t.Fatalf("acquire: %+v", res)
	}
	return res.Lease
}

func heartbeats(f *leasetest.FakeCore) []leasetest.Recorded {
	return filter(f, http.MethodPost, func(p string) bool { return len(p) > len(leasesPath) && p[len(p)-len("/heartbeat"):] == "/heartbeat" })
}

func TestHeartbeatEmptyBodyAndWithBody(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	c.Heartbeat(context.Background(), l)
	at := time.Date(2026, 10, 29, 1, 2, 3, 0, time.FixedZone("x", 3600))
	c.HeartbeatWith(context.Background(), l, rl.HeartbeatInfo{InUse: 2, LastUsedAt: at, Tier: rl.TierUserBatch, Loaded: true})
	c.HeartbeatWith(context.Background(), l, rl.HeartbeatInfo{})
	hb := heartbeats(f)
	if len(hb) != 3 || hb[0].Path != leasesPath+"/1-fake/heartbeat" {
		t.Fatalf("heartbeats %+v", hb)
	}
	if hb[0].Body != nil {
		t.Fatalf("Heartbeat must send no body, got %v", hb[0].Body)
	}
	want := map[string]any{"in_use": 2.0, "loaded": true, "last_used_at": "2026-10-29T00:02:03Z", "tier": "user_batch"}
	if !reflect.DeepEqual(hb[1].Body, want) {
		t.Fatalf("body %v, want %v", hb[1].Body, want)
	}
	if want := map[string]any{"in_use": 0.0, "loaded": false}; !reflect.DeepEqual(hb[2].Body, want) {
		t.Fatalf("zero body %v", hb[2].Body)
	}
}

func TestHeartbeatUpdatesStateAndReleaseRequested(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	f.RequestRelease(l.ID)
	c.Heartbeat(context.Background(), l)
	if state, rr := l.Snapshot(); state != rl.StateHeld || !rr {
		t.Fatalf("state %q rr %v", state, rr)
	}
}

func TestHeartbeat404MarksLostAndUnregisters(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f))
	l := held(t, c)
	f.Drop(l.ID)
	c.Heartbeat(context.Background(), l)
	if state, _ := l.Snapshot(); state != rl.StateLost || rl.RegisteredForTest(c, l.ID) {
		t.Fatalf("404+90002: state %q registered %v", state, rl.RegisteredForTest(c, l.ID))
	}

	// A plain 404 (no envelope) counts too.
	l2 := held(t, c)
	f.Respond(func(leasetest.Recorded) (int, any, bool) { return 404, nil, true })
	c.HeartbeatWith(context.Background(), l2, rl.HeartbeatInfo{InUse: 1})
	if state, _ := l2.Snapshot(); state != rl.StateLost || rl.RegisteredForTest(c, l2.ID) {
		t.Fatalf("plain 404: state %q", state)
	}
}

func TestHeartbeat5xxLeavesStateUntouched(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	f.Respond(func(leasetest.Recorded) (int, any, bool) { return 500, map[string]any{"success": 500}, true })
	c.Heartbeat(context.Background(), l)
	if state, rr := l.Snapshot(); state != rl.StateHeld || rr || !rl.RegisteredForTest(c, l.ID) {
		t.Fatalf("state %q rr %v", state, rr)
	}
}

func TestHeartbeatSkipsFailOpenAndEmptyID(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.Respond(func(leasetest.Recorded) (int, any, bool) { return 409, map[string]any{"success": 409}, true })
	placeholder := c.Acquire(context.Background(), req(-1)).Lease
	f.Respond(nil)
	before := len(f.Requests())
	for _, l := range []*rl.Lease{nil, placeholder, {State: rl.StateHeld}, {ID: "x", FailOpen: true, State: rl.StateHeld}} {
		c.Heartbeat(context.Background(), l)
		c.HeartbeatWith(context.Background(), l, rl.HeartbeatInfo{InUse: 1})
		c.Release(context.Background(), l)
		c.ReleaseWith(context.Background(), l, rl.ReleaseInfo{ObservedPeakMB: 1})
		c.ReleaseAsync(l, rl.ReleaseInfo{})
		if c.WaitRelease(context.Background(), l, time.Second) {
			t.Fatal("WaitRelease on a skipped lease must be false")
		}
	}
	// A disabled client sends nothing either.
	opts := photosOpts(f)
	opts.Disabled = true
	d := newClient(t, opts)
	hand := &rl.Lease{ID: "9-fake", State: rl.StateHeld}
	d.Heartbeat(context.Background(), hand)
	d.Release(context.Background(), hand)
	d.WaitRelease(context.Background(), hand, time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := len(f.Requests()) - before; n != 0 {
		t.Fatalf("requests = %d", n)
	}
}

func TestBookkeepingTimeoutBoundsHeartbeatAndRelease(t *testing.T) {
	t.Run("photos", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		l := held(t, c)
		f.Hang(5 * time.Second)
		start := time.Now()
		c.Heartbeat(context.Background(), l)
		if el := time.Since(start); el > 2300*time.Millisecond {
			t.Fatalf("heartbeat took %s", el)
		}
		start = time.Now()
		c.Release(context.Background(), l)
		if el := time.Since(start); el > 2300*time.Millisecond {
			t.Fatalf("release took %s", el)
		}
		if state, _ := l.Snapshot(); state != rl.StateReleased {
			t.Fatalf("a timed-out release still ends released, got %q", state)
		}
	})
	t.Run("ai uses the caller ctx only", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, aiOpts(f))
		l := held(t, c)
		f.Hang(700 * time.Millisecond)
		start := time.Now()
		c.Heartbeat(context.Background(), l)
		if el := time.Since(start); el < 650*time.Millisecond {
			t.Fatalf("heartbeat cut after %s without a caller deadline", el)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start = time.Now()
		c.Release(ctx, l)
		if el := time.Since(start); el > 500*time.Millisecond {
			t.Fatalf("release ignored the caller ctx: %s", el)
		}
	})
}

func TestReleaseBodyRules(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	a, b := held(t, c), held(t, c)
	c.ReleaseWith(context.Background(), a, rl.ReleaseInfo{})
	c.ReleaseWith(context.Background(), b, rl.ReleaseInfo{ObservedPeakMB: 512, Shared: true, ObservedPeakVRAMMB: 300})
	d := deletes(f)
	if len(d) != 2 || d[0].Body != nil {
		t.Fatalf("deletes %+v", d)
	}
	want := map[string]any{"observed_peak_mb": 512.0, "shared": true, "observed_peak_vram_mb": 300.0}
	if !reflect.DeepEqual(d[1].Body, want) {
		t.Fatalf("body %v", d[1].Body)
	}
	for _, l := range []*rl.Lease{a, b} {
		if state, _ := l.Snapshot(); state != rl.StateReleased || rl.RegisteredForTest(c, l.ID) {
			t.Fatalf("state %q", state)
		}
	}
	// A 404 (already gone) is tolerated and still ends released.
	gone := held(t, c)
	f.Drop(gone.ID)
	c.Release(context.Background(), gone)
	if state, _ := gone.Snapshot(); state != rl.StateReleased || rl.RegisteredForTest(c, gone.ID) {
		t.Fatalf("404: state %q", state)
	}
}

// Leases built by hand (no owning client), as the AI tests do, are handled by
// id; Release marks them released whatever the answer, a transport failure
// included.
func TestOpsOnLeasesBuiltByHand(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f))
	id := held(t, c).ID
	hand := &rl.Lease{ID: id, State: rl.StateHeld}
	f.RequestRelease(id)
	c.Heartbeat(context.Background(), hand)
	if _, rr := hand.Snapshot(); !rr {
		t.Fatal("heartbeat on a hand-built lease must write back")
	}
	if !c.WaitRelease(context.Background(), hand, time.Second) {
		t.Fatal("WaitRelease on a hand-built lease")
	}
	c.Release(context.Background(), hand)
	if state, _ := hand.Snapshot(); state != rl.StateReleased || len(deletes(f)) != 1 || len(f.Live()) != 0 {
		t.Fatalf("state %q deletes %d", state, len(deletes(f)))
	}
	unknown := &rl.Lease{ID: "nope", State: rl.StateHeld}
	c.Release(context.Background(), unknown)
	if state, _ := unknown.Snapshot(); state != rl.StateReleased {
		t.Fatalf("404 release: %q", state)
	}

	// Transport failure: nothing listens on the port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	dead := newClient(t, rl.Options{Client: "ai", BaseURL: "http://" + addr})
	l := &rl.Lease{ID: "1-x", State: rl.StateHeld}
	dead.Release(context.Background(), l)
	if state, _ := l.Snapshot(); state != rl.StateReleased {
		t.Fatalf("failed release: %q", state)
	}
}

func TestReleaseAsyncDoesNotBlock(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f))
	l := held(t, c)
	f.Hang(2 * time.Second)
	start := time.Now()
	c.ReleaseAsync(l, rl.ReleaseInfo{ObservedPeakMB: 7})
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("ReleaseAsync blocked %s", el)
	}
	eventually(t, time.Second, func() bool { return len(deletes(f)) == 1 }, "DELETE must be sent")
	if state, _ := l.Snapshot(); state == rl.StateReleased {
		t.Fatal("the release cannot have completed during the hang")
	}
	eventually(t, 3*time.Second, func() bool { s, _ := l.Snapshot(); return s == rl.StateReleased }, "lease must end released")
	if body := deletes(f)[0].Body; body["observed_peak_mb"] != 7.0 {
		t.Fatalf("body %v", body)
	}
}

func TestWaitReleaseQueryAndClamp(t *testing.T) {
	t.Run("photos", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		l := held(t, c)
		start := time.Now()
		if c.WaitRelease(context.Background(), l, 100*time.Millisecond) {
			t.Fatal("no release requested")
		}
		if el := time.Since(start); el < 900*time.Millisecond {
			t.Fatalf("clamped wait must be 1 s, returned after %s", el)
		}
		f.RequestRelease(l.ID)
		if !c.WaitRelease(context.Background(), l, 45*time.Second) {
			t.Fatal("release requested")
		}
		g := gets(f)
		if g[0].Query != "wait_ms=1000&wait_for=release" || g[1].Query != "wait_ms=30000&wait_for=release" {
			t.Fatalf("queries %q %q", g[0].Query, g[1].Query)
		}
	})
	t.Run("ai", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, aiOpts(f))
		l := held(t, c)
		c.WaitRelease(context.Background(), l, 0)
		if q := gets(f)[0].Query; q != "wait_ms=1&wait_for=release" {
			t.Fatalf("query %q", q)
		}
	})
}

func TestWaitRelease404PerPolicy(t *testing.T) {
	t.Run("gone marks lost", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		l := held(t, c)
		f.Drop(l.ID)
		if c.WaitRelease(context.Background(), l, time.Second) {
			t.Fatal("want false")
		}
		if state, _ := l.Snapshot(); state != rl.StateLost || rl.RegisteredForTest(c, l.ID) {
			t.Fatalf("state %q", state)
		}
	})
	t.Run("gone leaves state", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, aiOpts(f))
		l := held(t, c)
		f.Drop(l.ID)
		if c.WaitRelease(context.Background(), l, time.Second) {
			t.Fatal("want false")
		}
		if state, _ := l.Snapshot(); state != rl.StateHeld {
			t.Fatalf("state %q", state)
		}
	})
	t.Run("other errors", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		l := held(t, c)
		f.Respond(func(leasetest.Recorded) (int, any, bool) { return 500, map[string]any{"success": 500}, true })
		if c.WaitRelease(context.Background(), l, time.Second) {
			t.Fatal("want false")
		}
		if state, _ := l.Snapshot(); state != rl.StateHeld {
			t.Fatalf("state %q", state)
		}
	})
}

func TestWaitReleaseReturnsRequested(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	time.AfterFunc(50*time.Millisecond, func() { f.RequestRelease(l.ID) })
	if !c.WaitRelease(context.Background(), l, 5*time.Second) {
		t.Fatal("want true")
	}
	if state, rr := l.Snapshot(); state != rl.StateHeld || !rr {
		t.Fatalf("state %q rr %v", state, rr)
	}
}

func TestGetJSONDecodesData(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		switch r.Path {
		case "/v1/resources/npu/control":
			return 200, env(map[string]any{"mode": "cpu", "budget_mb": 512}), true
		case "/v1/resources/down":
			return 503, map[string]any{"success": 503}, true
		}
		return 0, nil, false
	})
	var out struct {
		Mode     string `json:"mode"`
		BudgetMB int    `json:"budget_mb"`
	}
	if err := c.GetJSON(context.Background(), "/v1/resources/npu/control", &out); err != nil || out.Mode != "cpu" || out.BudgetMB != 512 {
		t.Fatalf("out %+v err %v", out, err)
	}
	if err := c.GetJSON(context.Background(), "/v1/resources/missing", &out); !errors.Is(err, rl.ErrNotFound) {
		t.Fatalf("plain 404: %v", err)
	}
	if err := c.GetJSON(context.Background(), "/v1/resources/down", &out); !errors.Is(err, rl.ErrUnavailable) {
		t.Fatalf("503: %v", err)
	}
	if r := f.Requests()[0]; r.Method != http.MethodGet || r.Body != nil {
		t.Fatalf("request %+v", r)
	}
}

func TestLeaseConcurrentHeartbeatReleaseSnapshot(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f))
	l := held(t, c)
	f.RequestRelease(l.ID)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 5 {
			case 0:
				c.Heartbeat(context.Background(), l)
			case 1:
				c.HeartbeatWith(context.Background(), l, rl.HeartbeatInfo{InUse: i})
			case 2:
				c.WaitRelease(context.Background(), l, 10*time.Millisecond)
			case 3:
				if i == 3 {
					c.Release(context.Background(), l)
				} else {
					c.ReleaseAsync(l, rl.ReleaseInfo{})
				}
			default:
				_, _ = l.Snapshot()
				_ = l.Usable()
				_ = l.Clone()
			}
		}()
	}
	wg.Wait()
	eventually(t, 2*time.Second, func() bool { return len(f.Live()) == 0 }, "lease must be released")
}

func TestHeartbeatAnswerWithoutLeaseKeepsState(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodPost && r.Path != leasesPath {
			return http.StatusNoContent, nil, true
		}
		return 0, nil, false
	})
	c.Heartbeat(context.Background(), l)
	if s, _ := l.Snapshot(); s != rl.StateHeld {
		t.Fatalf("state %q after a 204 heartbeat", s)
	}
}

// A heartbeat or WaitRelease answer that arrives after the lease was
// released must not bring it back.
func TestLateAnswerDoesNotReviveReleasedLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		send   func(c *rl.Client, l *rl.Lease)
		match  func(r leasetest.Recorded) bool
	}{
		{"heartbeat 200", http.StatusOK, func(c *rl.Client, l *rl.Lease) { c.Heartbeat(context.Background(), l) },
			func(r leasetest.Recorded) bool { return r.Method == http.MethodPost && r.Path != leasesPath }},
		{"heartbeat 404", http.StatusNotFound, func(c *rl.Client, l *rl.Lease) { c.Heartbeat(context.Background(), l) },
			func(r leasetest.Recorded) bool { return r.Method == http.MethodPost && r.Path != leasesPath }},
		{"wait release 200", http.StatusOK, func(c *rl.Client, l *rl.Lease) {
			if c.WaitRelease(context.Background(), l, time.Second) {
				panic("a released lease reported a release request")
			}
		}, func(r leasetest.Recorded) bool { return r.Method == http.MethodGet }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := leasetest.New(t)
			c := newClient(t, photosOpts(f))
			l := held(t, c)
			f.Respond(func(r leasetest.Recorded) (int, any, bool) {
				if !tc.match(r) {
					return 0, nil, false
				}
				time.Sleep(150 * time.Millisecond)
				if tc.status == http.StatusNotFound {
					return http.StatusNotFound, map[string]any{"success": rl.CodeLeaseNotFound}, true
				}
				return http.StatusOK, env(map[string]any{"id": l.ID, "state": rl.StateHeld, "device": "gpu.1", "release_requested": true}), true
			})
			done := make(chan struct{})
			go func() { defer close(done); tc.send(c, l) }()
			time.Sleep(50 * time.Millisecond)
			c.Release(context.Background(), l)
			<-done
			if s, rr := l.Snapshot(); s != rl.StateReleased || rr {
				t.Fatalf("state %q release_requested %v, want released", s, rr)
			}
		})
	}
}
