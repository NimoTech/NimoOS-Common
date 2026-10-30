package resourcelease

import "time"

// Hooks for the external tests (package resourcelease_test), which cannot be
// internal tests because they import leasetest (an import cycle otherwise).

// RegisteredForTest reports whether a lease with id is registered on c.
func RegisteredForTest(c *Client, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.leases[id]
	return ok
}

// HoldIntervalForTest is the Hold period clamp.
var HoldIntervalForTest = holdInterval

// WatchTimingForTest returns the WatchRelease poll, fast-return threshold
// and backoff.
func WatchTimingForTest() (poll, fast, backoff time.Duration) {
	return watchPoll.Load(), watchFast.Load(), watchBackoff.Load()
}

// SetWatchTimingForTest overrides the WatchRelease timings (0 keeps one) and
// returns a func restoring the previous values.
func SetWatchTimingForTest(poll, fast, backoff time.Duration) (restore func()) {
	p, f, b := WatchTimingForTest()
	set := func(d *atomicDuration, v time.Duration) {
		if v > 0 {
			d.Store(v)
		}
	}
	set(&watchPoll, poll)
	set(&watchFast, fast)
	set(&watchBackoff, backoff)
	return func() {
		watchPoll.Store(p)
		watchFast.Store(f)
		watchBackoff.Store(b)
	}
}

// WaitersForTest returns how many callers wait on the in-flight call for
// key (0 when there is none).
func WaitersForTest(s *Singleflight, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.calls[key]; ok {
		return f.waiters
	}
	return 0
}

// SetHandoffAfterReadForTest installs a hook ReleaseHandoff runs right after
// reading the handoff file (nil removes it).
func SetHandoffAfterReadForTest(fn func()) {
	if fn == nil {
		handoffAfterRead.Store(nil)
		return
	}
	handoffAfterRead.Store(&fn)
}

// ClosedForTest reports whether Close has started on c.
func ClosedForTest(c *Client) bool { return c.isClosed() }
