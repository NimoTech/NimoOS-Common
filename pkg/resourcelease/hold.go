package resourcelease

import (
	"context"
	"sync/atomic"
	"time"
)

// hold is one managed heartbeat started by Hold.
type hold struct {
	cancel     context.CancelFunc
	done       chan struct{} // closed when the heartbeat goroutine has returned
	inCallback atomic.Bool   // the goroutine is running the info callback
}

// holdInterval clamps a Hold period to (0, MaxHeartbeatInterval]: a period
// that is not positive or longer than the core allows becomes
// MaxHeartbeatInterval.
func holdInterval(every time.Duration) time.Duration {
	if every <= 0 || every > MaxHeartbeatInterval {
		return MaxHeartbeatInterval
	}
	return every
}

// Hold heartbeats l every `every` (clamped to (0, MaxHeartbeatInterval]) on
// its own goroutine with a per-heartbeat timeout of `every`. info returning
// false sends an empty body. onLost runs once on a new goroutine when a
// heartbeat finds the lease gone; Hold then stops. stop is idempotent.
//
// Heartbeats never overlap: a tick that comes while the previous heartbeat
// is still out is skipped. A lease that has been released stops the hold
// without onLost. stop cancels an outstanding heartbeat and returns once the
// goroutine has stopped, except while the goroutine is running info: stop
// then returns at once (so info may call it) and the goroutine exits right
// after info without sending a heartbeat. It is safe to call from onLost.
// Close stops every hold. Nil, fail-open and id-less leases, a disabled
// client and a closed client start nothing.
func (c *Client) Hold(l *Lease, every time.Duration, info func() (HeartbeatInfo, bool), onLost func(*Lease)) (stop func()) {
	if c.skip(l) {
		return func() {}
	}
	every = holdInterval(every)
	ctx, cancel := context.WithCancel(context.Background())
	h := &hold{cancel: cancel, done: make(chan struct{})}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return func() {}
	}
	if c.holds == nil {
		c.holds = map[*hold]struct{}{}
	}
	c.holds[h] = struct{}{}
	c.mu.Unlock()

	go c.runHold(ctx, h, l, every, info, onLost)
	return func() {
		h.cancel()
		if !h.inCallback.Load() {
			<-h.done
		}
	}
}

func (c *Client) runHold(ctx context.Context, h *hold, l *Lease, every time.Duration, info func() (HeartbeatInfo, bool), onLost func(*Lease)) {
	defer close(h.done)
	defer func() {
		c.mu.Lock()
		delete(c.holds, h)
		c.mu.Unlock()
	}()
	// A time.Ticker drops ticks while the goroutine is busy, so a slow
	// heartbeat skips the ticks it overlaps instead of piling up.
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if state, _ := l.Snapshot(); state == StateReleased || state == StateLost {
			return
		}
		var body map[string]any
		if info != nil {
			h.inCallback.Store(true)
			hb, ok := info()
			h.inCallback.Store(false)
			if ok {
				body = heartbeatBody(hb)
			}
		}
		if ctx.Err() != nil {
			return
		}
		hctx, cancel := context.WithTimeout(ctx, every)
		c.heartbeat(hctx, l, body)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if state, _ := l.Snapshot(); state == StateLost {
			if onLost != nil {
				go onLost(l)
			}
			return
		}
	}
}
