package resourcelease_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rl "github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease/leasetest"
)

func isReleasePoll(r leasetest.Recorded) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Query, "wait_for=release")
}

func releasePolls(f *leasetest.FakeCore) []leasetest.Recorded {
	var out []leasetest.Recorded
	for _, r := range f.Requests() {
		if isReleasePoll(r) {
			out = append(out, r)
		}
	}
	return out
}

// watch runs WatchRelease on its own goroutine; the channel closes when it
// returns.
func watch(c *rl.Client, ctx context.Context, l *rl.Lease, on func(*rl.Lease)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.WatchRelease(ctx, l, on)
	}()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

func indexOf(reqs []leasetest.Recorded, match func(leasetest.Recorded) bool) int {
	for i, r := range reqs {
		if match(r) {
			return i
		}
	}
	return -1
}

func deleteOf(id string) func(leasetest.Recorded) bool {
	return func(r leasetest.Recorded) bool {
		return r.Method == http.MethodDelete && r.Path == leasesPath+"/"+id
	}
}

func TestWatchReleaseCallsOnRequestedOnce(t *testing.T) {
	if poll, fast, backoff := rl.WatchTimingForTest(); poll != 30*time.Second || fast != time.Second || backoff != 5*time.Second {
		t.Fatalf("watch timing %v/%v/%v, want 30s/1s/5s", poll, fast, backoff)
	}
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	l := held(t, c)

	var calls atomic.Int32
	var got atomic.Pointer[rl.Lease]
	done := watch(c, context.Background(), l, func(x *rl.Lease) {
		calls.Add(1)
		got.Store(x)
	})
	eventually(t, 2*time.Second, func() bool { return len(releasePolls(f)) == 1 }, "no release long-poll")
	f.RequestRelease(l.ID)
	waitDone(t, done, 5*time.Second, "WatchRelease did not return after a release request")

	if calls.Load() != 1 || got.Load() != l {
		t.Fatalf("onRequested calls %d, lease %p want %p", calls.Load(), got.Load(), l)
	}
	polls := releasePolls(f)
	if len(polls) != 1 || polls[0].Path != leasesPath+"/"+l.ID || polls[0].Query != "wait_ms=30000&wait_for=release" {
		t.Fatalf("release polls %+v", polls)
	}
	if state, rr := l.Snapshot(); state != rl.StateHeld || !rr {
		t.Fatalf("state %q rr %v", state, rr)
	}
}

func TestWatchReleaseBacksOffOnFastReturn(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f))
	l := held(t, c)

	var mu sync.Mutex
	var at []time.Time
	var delay atomic.Int64
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if !isReleasePoll(r) {
			return 0, nil, false
		}
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		time.Sleep(time.Duration(delay.Load()))
		// An old core: answers wait_for=release at once, no release wanted.
		return 200, env(map[string]any{"id": l.ID, "state": rl.StateHeld, "device": "gpu.1", "release_requested": false}), true
	})
	polls := func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), at...)
	}

	// Fast answers (well under the 1 s threshold) pause for the backoff.
	restore := rl.SetWatchTimingForTest(0, 0, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := watch(c, ctx, l, func(*rl.Lease) { t.Error("onRequested without a release request") })
	eventually(t, 3*time.Second, func() bool { return len(polls()) >= 4 }, "watch stopped polling")
	cancel()
	waitDone(t, done, 2*time.Second, "WatchRelease did not return on ctx cancel")
	restore()
	ts := polls()
	for i := 1; i < len(ts); i++ {
		if gap := ts[i].Sub(ts[i-1]); gap < 50*time.Millisecond {
			t.Fatalf("poll %d came %v after the previous one, want >= backoff 50ms", i, gap)
		}
	}

	// A round slower than the fast threshold polls again at once.
	mu.Lock()
	at = nil
	mu.Unlock()
	delay.Store(int64(30 * time.Millisecond))
	restore = rl.SetWatchTimingForTest(0, 10*time.Millisecond, 10*time.Second)
	defer restore()
	ctx, cancel = context.WithCancel(context.Background())
	done = watch(c, ctx, l, nil)
	eventually(t, 2*time.Second, func() bool { return len(polls()) >= 3 }, "a slow round must not back off")
	cancel()
	waitDone(t, done, 2*time.Second, "WatchRelease did not return on ctx cancel during backoff")
}

func TestWatchReleaseStopsOnLostOrCtx(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f)) // GoneMarksLost
	never := func(*rl.Lease) { t.Error("onRequested without a release request") }

	l := held(t, c)
	done := watch(c, context.Background(), l, never)
	eventually(t, 2*time.Second, func() bool { return len(releasePolls(f)) == 1 }, "no release long-poll")
	f.Drop(l.ID)
	waitDone(t, done, time.Second, "WatchRelease did not return at once on a lost lease (no backoff)")
	if state, _ := l.Snapshot(); state != rl.StateLost {
		t.Fatalf("state %q, want lost", state)
	}

	l2 := held(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	done = watch(c, ctx, l2, never)
	eventually(t, 2*time.Second, func() bool { return len(releasePolls(f)) == 2 }, "no release long-poll")
	cancel()
	waitDone(t, done, 2*time.Second, "WatchRelease did not return on ctx cancel")
	if state, _ := l2.Snapshot(); state != rl.StateHeld {
		t.Fatalf("state %q, want held", state)
	}

	// Leases that are not held send nothing and return at once.
	n := len(f.Requests())
	for _, x := range []*rl.Lease{
		nil,
		{State: rl.StateHeld, FailOpen: true},
		leasetest.Lease("9-fake", "cpu", rl.StateReleased),
		leasetest.Lease("9-fake", "cpu", rl.StateQueued),
	} {
		waitDone(t, watch(c, context.Background(), x, never), time.Second, "WatchRelease on an unusable lease blocked")
	}
	if len(f.Requests()) != n {
		t.Fatalf("unusable leases sent %d requests", len(f.Requests())-n)
	}
}

func residentReq() rl.Request {
	return rl.Request{Task: "model:clip", Class: rl.ClassMemory, Priority: 2, Tier: rl.TierAutoBatch,
		Workload: "clip", DevicePref: []string{"gpu.1"}, PID: 4242, EstRAMMB: 1500, MaxWait: 30 * time.Second}
}

func TestReplaceNewBeforeOldReleasesOldAfterNewHeld(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	old := held(t, c)

	nl, ok := c.ReplaceNewBeforeOld(context.Background(), old, residentReq())
	if !ok || nl == nil || !nl.Usable() || nl.ID == old.ID || nl.Device != "gpu.1" {
		t.Fatalf("replace = %+v, %v", nl, ok)
	}
	ps := posts(f)
	if len(ps) != 2 {
		t.Fatalf("posts %d, want 2", len(ps))
	}
	b := ps[1].Body
	if b["already_loaded"] != true || b["kind"] != rl.KindResident || b["wait_ms"] != 0.0 || b["workload"] != "clip" || b["priority"] != 2.0 || b["pid"] != 4242.0 {
		t.Fatalf("new lease body %v", b)
	}
	eventually(t, 2*time.Second, func() bool {
		s, _ := old.Snapshot()
		return s == rl.StateReleased
	}, "old lease not released")
	reqs := f.Requests()
	iNew := indexOf(reqs, func(r leasetest.Recorded) bool {
		return r.Method == http.MethodPost && r.Path == leasesPath && r.Body["already_loaded"] == true
	})
	iDel := indexOf(reqs, deleteOf(old.ID))
	if iNew < 0 || iDel < iNew {
		t.Fatalf("old DELETE at %d, new POST at %d: old must go only after the new one is held", iDel, iNew)
	}
	if indexOf(reqs, deleteOf(nl.ID)) >= 0 {
		t.Fatal("new lease deleted")
	}
	if live := f.Live(); len(live) != 1 || live[0] != nl.ID {
		t.Fatalf("live %v, want [%s]", live, nl.ID)
	}

	// An explicit kind is passed through as is: the core refuses
	// already_loaded on a task lease (400), so nothing is replaced.
	r := residentReq()
	r.Kind = rl.KindTask
	if x, ok := c.ReplaceNewBeforeOld(context.Background(), nl, r); ok || x != nil {
		t.Fatalf("replace with kind task = %+v, %v", x, ok)
	}
	if ps := posts(f); ps[2].Body["kind"] != rl.KindTask {
		t.Fatalf("kind %v, want task", ps[2].Body["kind"])
	}
	if !nl.Usable() || indexOf(f.Requests(), deleteOf(nl.ID)) >= 0 {
		t.Fatal("a refused replacement touched the current lease")
	}
}

func TestReplaceKeepsOldWhenNewNotHeld(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	old := held(t, c)

	f.QueueNext(0)
	nl, ok := c.ReplaceNewBeforeOld(context.Background(), old, residentReq())
	if ok || nl != nil {
		t.Fatalf("replace = %+v, %v; want nil, false", nl, ok)
	}
	if ds := deletes(f); len(ds) != 1 || ds[0].Path != leasesPath+"/2-fake" {
		t.Fatalf("deletes %+v, want only the queued new lease", ds)
	}
	if live := f.Live(); len(live) != 1 || live[0] != old.ID || !old.Usable() {
		t.Fatalf("live %v, old usable %v", live, old.Usable())
	}

	// A new lease the core answers in an odd state is deleted too.
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodPost && r.Path == leasesPath {
			return 200, env(map[string]any{"id": "77-odd", "state": "expired", "device": "gpu.1"}), true
		}
		return 0, nil, false
	})
	if nl, ok := c.ReplaceNewBeforeOld(context.Background(), old, residentReq()); ok || nl != nil {
		t.Fatalf("replace = %+v, %v", nl, ok)
	}
	eventually(t, 2*time.Second, func() bool { return indexOf(f.Requests(), deleteOf("77-odd")) >= 0 }, "odd-state new lease not deleted")
	if indexOf(f.Requests(), deleteOf(old.ID)) >= 0 || !old.Usable() {
		t.Fatal("old lease released")
	}

	// A fail-open (core unreachable) keeps old and sends no DELETE.
	f.Respond(func(r leasetest.Recorded) (int, any, bool) { return 503, nil, true })
	n := len(deletes(f))
	if nl, ok := c.ReplaceNewBeforeOld(context.Background(), old, residentReq()); ok || nl != nil {
		t.Fatalf("replace = %+v, %v", nl, ok)
	}
	time.Sleep(50 * time.Millisecond)
	if len(deletes(f)) != n || !old.Usable() {
		t.Fatal("fail-open replace touched old")
	}
}

func TestReplaceSkippedWhileOldReleaseRequested(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	old := held(t, c)
	yes := true
	old.SetState(rl.StateHeld, &yes)
	n := len(f.Requests())
	if nl, ok := c.ReplaceNewBeforeOld(context.Background(), old, residentReq()); ok || nl != nil {
		t.Fatalf("replace = %+v, %v", nl, ok)
	}
	if len(f.Requests()) != n {
		t.Fatalf("sent %d requests while old has a release request", len(f.Requests())-n)
	}
	if !old.Usable() {
		t.Fatal("old changed")
	}

	// A release request that arrives while the new lease is being granted:
	// the new lease goes again, old stays.
	no := false
	old.SetState(rl.StateHeld, &no)
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodPost && r.Path == leasesPath {
			old.SetState(rl.StateHeld, &yes)
		}
		return 0, nil, false
	})
	if nl, ok := c.ReplaceNewBeforeOld(context.Background(), old, residentReq()); ok || nl != nil {
		t.Fatalf("replace = %+v, %v", nl, ok)
	}
	eventually(t, 2*time.Second, func() bool { return indexOf(f.Requests(), deleteOf("2-fake")) >= 0 }, "new lease not released")
	if indexOf(f.Requests(), deleteOf(old.ID)) >= 0 || !old.Usable() {
		t.Fatal("old lease released")
	}
}

// gatedFn returns an fn that counts its runs, reports its ctx on started and
// returns res once gate is closed.
func gatedFn(runs *atomic.Int32, started chan<- context.Context, gate <-chan struct{}, res func() rl.Result) func(context.Context) rl.Result {
	return func(ctx context.Context) rl.Result {
		runs.Add(1)
		if started != nil {
			started <- ctx
		}
		<-gate
		return res()
	}
}

type doResult struct {
	res    rl.Result
	shared bool
}

func do(s *rl.Singleflight, ctx context.Context, key string, fn func(context.Context) rl.Result) <-chan doResult {
	ch := make(chan doResult, 1)
	go func() {
		res, shared := s.Do(ctx, key, fn)
		ch <- doResult{res, shared}
	}()
	return ch
}

func recv(t *testing.T, ch <-chan doResult) doResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Do did not return")
	}
	return doResult{}
}

func TestSingleflightJoinsSameKey(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	s := &rl.Singleflight{Client: c}

	var runs atomic.Int32
	started := make(chan context.Context, 1)
	gate := make(chan struct{})
	fn := gatedFn(&runs, started, gate, func() rl.Result { return c.Acquire(context.Background(), req(-1)) })
	chs := []<-chan doResult{do(s, context.Background(), "clip", fn)}
	<-started
	chs = append(chs, do(s, context.Background(), "clip", fn), do(s, context.Background(), "clip", fn))
	eventually(t, 2*time.Second, func() bool { return rl.WaitersForTest(s, "clip") == 3 }, "joiners did not join")
	close(gate)

	shared := 0
	var lease *rl.Lease
	for _, ch := range chs {
		r := recv(t, ch)
		if r.res.Outcome != rl.OutcomeHeld {
			t.Fatalf("result %+v", r.res)
		}
		if lease == nil {
			lease = r.res.Lease
		} else if r.res.Lease != lease {
			t.Fatal("waiters got different leases")
		}
		if r.shared {
			shared++
		}
	}
	if runs.Load() != 1 || shared != 2 || len(posts(f)) != 1 {
		t.Fatalf("runs %d shared %d posts %d", runs.Load(), shared, len(posts(f)))
	}
	if rl.WaitersForTest(s, "clip") != 0 {
		t.Fatal("finished call still in flight")
	}

	// Another key runs on its own; a finished key runs again.
	r1 := recv(t, do(s, context.Background(), "other", func(context.Context) rl.Result { return rl.Result{Outcome: rl.OutcomeFailOpen} }))
	r2 := recv(t, do(s, context.Background(), "clip", func(context.Context) rl.Result { return rl.Result{Outcome: rl.OutcomeFailOpen} }))
	if r1.shared || r2.shared || r1.res.Outcome != rl.OutcomeFailOpen || r2.res.Outcome != rl.OutcomeFailOpen {
		t.Fatalf("independent runs: %+v %+v", r1, r2)
	}
}

func TestSingleflightWithdrawCancelsWithErrWithdrawn(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	logs := observed(&opts)
	c := newClient(t, opts)
	s := &rl.Singleflight{Client: c}

	f.QueueNext(0) // queued, never granted
	var cause atomic.Value
	fn := func(ctx context.Context) rl.Result {
		res := c.Acquire(ctx, req(-1))
		cause.Store(context.Cause(ctx))
		return res
	}
	leader := do(s, context.Background(), "clip", fn)
	eventually(t, 2*time.Second, func() bool { return len(f.Live()) == 1 }, "acquire not queued")
	joiner := do(s, context.Background(), "clip", fn)
	eventually(t, 2*time.Second, func() bool { return rl.WaitersForTest(s, "clip") == 2 }, "joiner did not join")

	if !s.Withdraw("clip") {
		t.Fatal("Withdraw found no call")
	}
	if s.Withdraw("clip") || s.Withdraw("nothing") {
		t.Fatal("Withdraw of no call reported true")
	}
	for _, ch := range []<-chan doResult{leader, joiner} {
		r := recv(t, ch)
		if r.res.Outcome != rl.OutcomeWithdrawn || !errors.Is(r.res.Err, rl.ErrWithdrawn) || r.res.Lease == nil || r.res.Lease.Device != "gpu.1" {
			t.Fatalf("result %+v", r.res)
		}
	}
	if got, _ := cause.Load().(error); got != rl.ErrWithdrawn {
		t.Fatalf("fn ctx cause %v, want ErrWithdrawn", got)
	}
	eventually(t, 2*time.Second, func() bool { return len(f.Live()) == 0 }, "queued lease not cancelled")
	if n := failOpenWarns(logs); n != 0 {
		t.Fatalf("a withdrawn acquire logged %d fail-open warnings", n)
	}
}

func TestSingleflightWithdrawnGrantIsReleased(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	for _, s := range []*rl.Singleflight{{Client: c}, {}} { // zero value: the lease's own client releases
		var runs atomic.Int32
		started := make(chan context.Context, 1)
		gate := make(chan struct{})
		var granted atomic.Pointer[rl.Lease]
		fn := gatedFn(&runs, started, gate, func() rl.Result {
			res := c.Acquire(context.Background(), req(-1)) // ignores the withdrawal
			granted.Store(res.Lease)
			return res
		})
		leader := do(s, context.Background(), "clip", fn)
		fctx := <-started
		joiner := do(s, context.Background(), "clip", fn)
		eventually(t, 2*time.Second, func() bool { return rl.WaitersForTest(s, "clip") == 2 }, "joiner did not join")
		s.Withdraw("clip")
		if context.Cause(fctx) != rl.ErrWithdrawn {
			t.Fatalf("fn ctx cause %v", context.Cause(fctx))
		}
		close(gate)
		for _, ch := range []<-chan doResult{leader, joiner} {
			r := recv(t, ch)
			if r.res.Outcome != rl.OutcomeWithdrawn || r.res.Lease == nil || r.res.Lease.ID != "" || r.res.Lease.Device != "gpu.1" || !errors.Is(r.res.Err, rl.ErrWithdrawn) {
				t.Fatalf("result %+v lease %+v", r.res, r.res.Lease)
			}
		}
		l := granted.Load()
		eventually(t, 2*time.Second, func() bool { return indexOf(f.Requests(), deleteOf(l.ID)) >= 0 }, "withdrawn grant not released")
		eventually(t, 2*time.Second, func() bool { st, _ := l.Snapshot(); return st == rl.StateReleased }, "withdrawn grant not marked released")
	}
	if live := f.Live(); len(live) != 0 {
		t.Fatalf("live %v", live)
	}
}

func TestSingleflightJoinerCtxEndsIndependently(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	logs := observed(&opts)
	c := newClient(t, opts)
	s := &rl.Singleflight{Client: c}

	var runs atomic.Int32
	started := make(chan context.Context, 1)
	gate := make(chan struct{})
	fn := gatedFn(&runs, started, gate, func() rl.Result { return c.Acquire(context.Background(), req(-1)) })
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leader := do(s, leaderCtx, "clip", fn)
	fctx := <-started
	jctx, cancelJoiner := context.WithCancel(context.Background())
	joiner := do(s, jctx, "clip", fn)
	eventually(t, 2*time.Second, func() bool { return rl.WaitersForTest(s, "clip") == 2 }, "joiner did not join")

	cancelJoiner()
	r := recv(t, joiner)
	if r.res.Outcome != rl.OutcomeFailOpen || r.res.Reason != rl.ReasonTimedOut || !errors.Is(r.res.Err, context.Canceled) || !r.shared || r.res.Lease == nil || !r.res.Lease.FailOpen {
		t.Fatalf("joiner result %+v", r)
	}
	if n := failOpenWarns(logs); n != 1 {
		t.Fatalf("%d fail-open warnings for the joiner, want 1", n)
	}
	if fctx.Err() != nil || rl.WaitersForTest(s, "clip") != 1 {
		t.Fatalf("leader affected: fn ctx %v, waiters %d", fctx.Err(), rl.WaitersForTest(s, "clip"))
	}
	select {
	case <-leader:
		t.Fatal("leader returned with the joiner")
	case <-time.After(30 * time.Millisecond):
	}
	// A joiner withdrawn by its own ctx cause gets OutcomeWithdrawn.
	wctx, cancelW := context.WithCancelCause(context.Background())
	withdrawnJoiner := do(s, wctx, "clip", fn)
	eventually(t, 2*time.Second, func() bool { return rl.WaitersForTest(s, "clip") == 2 }, "joiner did not join")
	cancelW(rl.ErrWithdrawn)
	if r := recv(t, withdrawnJoiner); r.res.Outcome != rl.OutcomeWithdrawn || !errors.Is(r.res.Err, rl.ErrWithdrawn) {
		t.Fatalf("withdrawn joiner %+v", r.res)
	}
	if n := failOpenWarns(logs); n != 1 {
		t.Fatalf("a withdrawn joiner logged a fail-open warning (%d)", n)
	}

	close(gate)
	lr := recv(t, leader)
	if lr.res.Outcome != rl.OutcomeHeld || !lr.res.Lease.Usable() || lr.shared || runs.Load() != 1 {
		t.Fatalf("leader result %+v runs %d", lr, runs.Load())
	}
}

func TestSingleflightLastWaiterLeavingWithdraws(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	s := &rl.Singleflight{Client: c}

	var runs atomic.Int32
	started := make(chan context.Context, 1)
	gate := make(chan struct{})
	var granted atomic.Pointer[rl.Lease]
	fn := gatedFn(&runs, started, gate, func() rl.Result {
		res := c.Acquire(context.Background(), req(-1))
		granted.Store(res.Lease)
		return res
	})
	ctx, cancel := context.WithCancel(context.Background())
	only := do(s, ctx, "clip", fn)
	fctx := <-started
	cancel()
	if r := recv(t, only); r.res.Outcome != rl.OutcomeFailOpen || r.res.Reason != rl.ReasonTimedOut {
		t.Fatalf("result %+v", r.res)
	}
	if context.Cause(fctx) != rl.ErrWithdrawn {
		t.Fatalf("fn ctx cause %v, want ErrWithdrawn once nobody waits", context.Cause(fctx))
	}
	if rl.WaitersForTest(s, "clip") != 0 {
		t.Fatal("abandoned call still joinable")
	}
	close(gate)
	eventually(t, 2*time.Second, func() bool {
		l := granted.Load()
		return l != nil && indexOf(f.Requests(), deleteOf(l.ID)) >= 0
	}, "unclaimed grant not released")
}

func TestReplaceCarriesKeepOnCloseToTheNewLease(t *testing.T) {
	f := leasetest.New(t)
	opts := handoffOpts(t, f)
	c := newClient(t, opts)
	old := held(t, c)
	c.KeepOnClose(old)

	nl, ok := c.ReplaceNewBeforeOld(context.Background(), old, residentReq())
	if !ok {
		t.Fatal("replace failed")
	}
	eventually(t, 2*time.Second, func() bool { return indexOf(f.Requests(), deleteOf(old.ID)) >= 0 }, "old lease not released")
	rep := c.Close(context.Background())
	if len(rep.Kept) != 1 || rep.Kept[0] != nl.ID || len(rep.Released) != 0 {
		t.Fatalf("close report %+v, want only %s kept", rep, nl.ID)
	}
	doc := readHandoffDoc(t, opts.HandoffFile)
	if len(doc.Leases) != 1 || doc.Leases[0] != (rl.HandoffEntry{ID: nl.ID, Workload: "clip", PID: 4242, Device: "gpu.1"}) {
		t.Fatalf("handoff %+v", doc.Leases)
	}
	if indexOf(f.Requests(), deleteOf(nl.ID)) >= 0 {
		t.Fatal("kept new lease deleted")
	}
}

func TestWatchReleaseStopsOnClose(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, handoffOpts(t, f))
	l := held(t, c)
	c.KeepOnClose(l) // kept: Close sends no DELETE that would end the poll
	done := watch(c, context.Background(), l, func(*rl.Lease) { t.Error("onRequested on Close") })
	eventually(t, 2*time.Second, func() bool { return len(releasePolls(f)) == 1 }, "no release long-poll")
	c.Close(context.Background())
	waitDone(t, done, time.Second, "WatchRelease did not return on Close")

	for i := 0; i < 50; i++ {
		waitDone(t, watch(c, context.Background(), l, nil), time.Second, "WatchRelease after Close blocked")
	}
	if n := len(releasePolls(f)); n != 1 {
		t.Fatalf("%d release polls, want none after Close", n)
	}
	if indexOf(f.Requests(), deleteOf(l.ID)) >= 0 {
		t.Fatal("kept lease deleted")
	}
}
