package resourcelease

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// WatchRelease timings. They are package variables (atomics, so a test can
// change them while an older watch goroutine is still winding down):
// watchPoll is the release long-poll of each round, a round shorter than
// watchFast means the core did not long-poll (an old core, or an error) and
// is followed by a watchBackoff pause.
var (
	watchPoll    atomicDuration
	watchFast    atomicDuration
	watchBackoff atomicDuration
)

func init() {
	watchPoll.Store(30 * time.Second)
	watchFast.Store(time.Second)
	watchBackoff.Store(5 * time.Second)
}

type atomicDuration struct{ v atomic.Int64 }

func (d *atomicDuration) Load() time.Duration   { return time.Duration(d.v.Load()) }
func (d *atomicDuration) Store(v time.Duration) { d.v.Store(int64(v)) }

// WatchRelease long-polls l for a release request until ctx ends or the
// lease is no longer usable, calling onRequested when the core asks for it.
//
// Each round is one WaitRelease of 30 s. A release request runs onRequested
// (on the calling goroutine) once and returns; a lease that is no longer
// held (lost, released, fail-open, id-less) or an ended ctx returns without
// calling it. A round that comes back in under 1 s without a request (a core
// that does not long-poll wait_for=release, or an error) is followed by a
// 5 s pause, so a fast-answering core is not hammered. Close ends the watch
// (a pending long-poll included): a lease kept for the successor is not
// polled after Close.
func (c *Client) WatchRelease(ctx context.Context, l *Lease, onRequested func(*Lease)) {
	if c.closeCtx != nil {
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
		stop := context.AfterFunc(c.closeCtx, func() { cancel(ErrClosed) })
		defer stop()
	}
	for {
		if ctx.Err() != nil || c.isClosed() || !l.Usable() {
			return
		}
		start := time.Now()
		requested := c.WaitRelease(ctx, l, watchPoll.Load())
		if ctx.Err() != nil {
			return
		}
		if requested {
			if onRequested != nil {
				onRequested(l)
			}
			return
		}
		if state, _ := l.Snapshot(); state == StateLost || state == StateReleased {
			return
		}
		if time.Since(start) < watchFast.Load() {
			t := time.NewTimer(watchBackoff.Load())
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// ReplaceNewBeforeOld acquires a lease for r before releasing old, so the
// resident never runs uncovered. It returns the new lease and whether the
// replacement happened.
//
// The new lease is requested with AlreadyLoaded set (the memory is in use
// already), Kind "resident" when r.Kind is "" and MaxWait 0 (a replacement
// never queues: a queued answer is cancelled). Only a held new lease
// replaces old, which is then released with ReleaseAsync. Nothing is sent
// while old has a release request pending, and a new lease granted while
// old got one meanwhile is released again: the resident is about to be
// unloaded, a fresh lease would only hide that. On (nil, false) old is left
// as it was and stays the caller's. A KeepOnClose mark on old moves to the
// new lease.
func (c *Client) ReplaceNewBeforeOld(ctx context.Context, old *Lease, r Request) (*Lease, bool) {
	if releaseRequested(old) {
		return nil, false
	}
	r.AlreadyLoaded = true
	if r.Kind == "" {
		r.Kind = KindResident
	}
	r.MaxWait = 0
	res := c.Acquire(ctx, r)
	nl := res.Lease
	if res.Outcome != OutcomeHeld {
		// A queued answer was cancelled by Acquire already; a lease the core
		// answered in an odd state (not usable, not registered) still has to go.
		if nl != nil && nl.ID != "" && !nl.FailOpen {
			c.ReleaseAsync(nl, ReleaseInfo{})
		}
		return nil, false
	}
	if releaseRequested(old) {
		c.ReleaseAsync(nl, ReleaseInfo{})
		return nil, false
	}
	if !c.inheritKeep(old, nl) {
		// Close ran meanwhile: it deletes the new lease and leaves old
		// (perhaps kept for the successor) as it was.
		return nil, false
	}
	c.ReleaseAsync(old, ReleaseInfo{})
	return nl, true
}

// inheritKeep carries the KeepOnClose mark of old over to its replacement:
// releasing old drops old's mark, and the resident it covers still outlives
// this process. It reports false once the client is closed.
func (c *Client) inheritKeep(old, nl *Lease) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if old == nil || old.ID == "" {
		return true
	}
	e, ok := c.keep[old.ID]
	if !ok {
		return true
	}
	ne := HandoffEntry{ID: nl.ID, Workload: nl.workload, PID: nl.pid, Device: nl.Device}
	if ne.Workload == "" {
		ne.Workload = e.Workload
	}
	if ne.PID == 0 {
		ne.PID = e.PID
	}
	c.keep[nl.ID] = ne
	return true
}

func releaseRequested(l *Lease) bool {
	if l == nil {
		return false
	}
	_, rr := l.Snapshot()
	return rr
}

// Singleflight collapses concurrent Acquire calls that share a key.
//
// The first caller of a key starts fn on its own goroutine with a context
// that keeps ctx's values but not its cancellation; later callers of the
// same key join it. Every caller waits on its own ctx: one whose ctx ends
// gets a fail-open Result (ReasonTimedOut, or OutcomeWithdrawn when the
// cause is ErrWithdrawn) while fn goes on for the others; that is logged as
// an "admission fail-open" Warn through Client (throttled; nothing without
// Client). Do does not know the Request, so that placeholder lease carries
// no Device and no Tier: a caller that runs anyway sets its device itself
// (DevicePref[0]). When the last waiter leaves, the call is withdrawn as by
// Withdraw.
//
// Withdraw cancels fn's context with cause ErrWithdrawn (an Acquire run by
// fn then ends with OutcomeWithdrawn, not as a fail-open) and every waiter
// gets OutcomeWithdrawn once fn returns; a lease fn got granted anyway is
// released with ReleaseAsync. All waiters of one run share the same Result,
// hence the same *Lease.
//
// The zero value is ready to use; Client, when set, releases a lease
// granted to a withdrawn call (else the lease's own client does).
type Singleflight struct {
	Client *Client // releases a lease granted to a withdrawn call

	mu    sync.Mutex
	calls map[string]*flight
}

// flight is one run of fn for a key.
type flight struct {
	cancel    context.CancelCauseFunc
	done      chan struct{} // closed once res is set
	res       Result
	waiters   int  // guarded by Singleflight.mu
	withdrawn bool // guarded by Singleflight.mu
}

// Do runs fn once per key among concurrent callers; shared reports whether
// res came from another caller's run.
func (s *Singleflight) Do(ctx context.Context, key string, fn func(ctx context.Context) Result) (res Result, shared bool) {
	s.mu.Lock()
	if f, ok := s.calls[key]; ok {
		f.waiters++
		s.mu.Unlock()
		return s.wait(ctx, key, f), true
	}
	fctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	f := &flight{cancel: cancel, done: make(chan struct{}), waiters: 1}
	if s.calls == nil {
		s.calls = map[string]*flight{}
	}
	s.calls[key] = f
	s.mu.Unlock()

	go s.run(fctx, key, f, fn)
	return s.wait(ctx, key, f), false
}

func (s *Singleflight) run(ctx context.Context, key string, f *flight, fn func(ctx context.Context) Result) {
	res := fn(ctx)
	s.mu.Lock()
	if s.calls[key] == f {
		delete(s.calls, key)
	}
	if !f.withdrawn || res.Outcome == OutcomeWithdrawn {
		// Published under s.mu: a waiter leaving at the same time either
		// sees the result or withdrew the run before this check.
		s.finishLocked(f, res)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if res.Outcome == OutcomeHeld {
		s.release(res.Lease)
	}
	s.mu.Lock()
	s.finishLocked(f, withdrawnFrom(res.Lease))
	s.mu.Unlock()
}

// finishLocked publishes the result of f; s.mu held.
func (s *Singleflight) finishLocked(f *flight, res Result) {
	f.res = res
	f.cancel(nil)
	close(f.done)
}

// wait blocks until f is done or ctx ends.
func (s *Singleflight) wait(ctx context.Context, key string, f *flight) Result {
	select {
	case <-f.done:
		return f.res
	case <-ctx.Done():
	}
	s.mu.Lock()
	select {
	case <-f.done: // finished while ctx ended: the result is ours to take
		s.mu.Unlock()
		return f.res
	default:
	}
	f.waiters--
	if f.waiters == 0 {
		s.withdrawLocked(key, f) // nobody is left to take a grant
	}
	s.mu.Unlock()
	cause := context.Cause(ctx)
	if withdrawn(ctx) {
		return Result{Outcome: OutcomeWithdrawn, Lease: placeholderFrom(nil), Err: cause}
	}
	if s.Client != nil {
		s.Client.warn("singleflight wait ended", zap.String("key", key), zap.Error(cause))
	}
	return Result{Outcome: OutcomeFailOpen, Lease: placeholderFrom(nil), Reason: ReasonTimedOut, Err: cause}
}

// Withdraw abandons the in-flight call for key and reports whether one existed.
// A later Do of key starts a new run.
func (s *Singleflight) Withdraw(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.calls[key]
	if !ok {
		return false
	}
	s.withdrawLocked(key, f)
	return true
}

// withdrawLocked marks f withdrawn, forgets it and cancels fn; s.mu held.
func (s *Singleflight) withdrawLocked(key string, f *flight) {
	if f.withdrawn {
		return
	}
	f.withdrawn = true
	if s.calls[key] == f {
		delete(s.calls, key)
	}
	f.cancel(ErrWithdrawn)
}

func (s *Singleflight) release(l *Lease) {
	switch {
	case l == nil:
	case s.Client != nil:
		s.Client.ReleaseAsync(l, ReleaseInfo{})
	case l.owner != nil:
		l.owner.ReleaseAsync(l, ReleaseInfo{})
	}
}

// placeholderFrom is the fail-open placeholder of a Singleflight Result that
// fn did not produce: the device and tier of l when there is one.
func placeholderFrom(l *Lease) *Lease {
	if l == nil {
		return &Lease{State: StateHeld, FailOpen: true}
	}
	return &Lease{Device: l.Device, State: StateHeld, FailOpen: true, Tier: l.Tier}
}

func withdrawnFrom(l *Lease) Result {
	return Result{Outcome: OutcomeWithdrawn, Lease: placeholderFrom(l), Err: ErrWithdrawn}
}
