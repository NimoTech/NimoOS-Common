package resourcelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// skip reports whether a bookkeeping call on l sends nothing: nil, fail-open
// and id-less leases, or a disabled client. Leases built by hand (no owning
// client) are handled like any other: the request goes out by id.
func (c *Client) skip(l *Lease) bool {
	return l == nil || l.FailOpen || l.ID == "" || c.opts.Disabled
}

// bookkeeping bounds ctx by Options.BookkeepingTimeout when it is set.
func (c *Client) bookkeeping(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.opts.BookkeepingTimeout > 0 {
		return context.WithTimeout(ctx, c.opts.BookkeepingTimeout)
	}
	return ctx, func() {}
}

// Heartbeat renews l with an empty body.
func (c *Client) Heartbeat(ctx context.Context, l *Lease) {
	c.heartbeat(ctx, l, nil)
}

// HeartbeatWith renews l with info as the heartbeat body.
func (c *Client) HeartbeatWith(ctx context.Context, l *Lease, info HeartbeatInfo) {
	c.heartbeat(ctx, l, heartbeatBody(info))
}

// heartbeat sends one renewal. Any 404 marks l lost and forgets it; a
// success writes back State and ReleaseRequested (not over a released or
// lost lease, not from an answer without a lease); other errors leave l as
// is.
func (c *Client) heartbeat(ctx context.Context, l *Lease, body map[string]any) {
	if c.skip(l) || c.retired(l) {
		return
	}
	ctx, cancel := c.bookkeeping(ctx)
	defer cancel()
	lw, err := c.do(ctx, http.MethodPost, leasesPath+"/"+l.ID+"/heartbeat", body)
	switch {
	case errors.Is(err, ErrLeaseGone), errors.Is(err, ErrNotFound):
		l.markLost()
		c.unregister(l.ID)
	case err == nil && lw.ID != "":
		l.update(lw)
	}
}

// markLost marks l lost unless it was released already.
func (l *Lease) markLost() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.State != StateReleased {
		l.State = StateLost
	}
}

// update writes State and ReleaseRequested of a core answer back to l,
// unless l already ended on this side (released or lost): a late heartbeat
// or poll answer must not bring a released lease back to held. It reports
// whether it wrote.
func (l *Lease) update(lw *leaseWire) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.State == StateReleased || l.State == StateLost {
		return false
	}
	l.State = lw.State
	l.ReleaseRequested = lw.ReleaseRequested
	return true
}

// WaitRelease long-polls the core for a release request on l for up to wait
// and reports whether a release was requested.
//
// wait is clamped to [max(WaitRelease.Min, 1 ms), MaxLongPoll]; the request
// runs on ctx, cut off wait+WaitRelease.Slack later when Slack > 0. A 404
// marks l lost only with WaitRelease.GoneMarksLost; every error reports
// false.
func (c *Client) WaitRelease(ctx context.Context, l *Lease, wait time.Duration) bool {
	if c.skip(l) {
		return false
	}
	policy := c.opts.WaitRelease
	floor := max(policy.Min, time.Millisecond)
	wait = min(max(wait, floor), MaxLongPoll)
	if policy.Slack > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait+policy.Slack)
		defer cancel()
	}
	// The query order is part of the wire baseline: wait_ms first.
	path := leasesPath + "/" + l.ID + "?wait_ms=" + strconv.Itoa(int(wait/time.Millisecond)) + "&wait_for=release"
	lw, err := c.do(ctx, http.MethodGet, path, nil)
	if errors.Is(err, ErrLeaseGone) || errors.Is(err, ErrNotFound) {
		if policy.GoneMarksLost {
			l.markLost()
			c.unregister(l.ID)
		}
		return false
	}
	if err != nil || lw.ID == "" {
		return false
	}
	return l.update(lw) && lw.ReleaseRequested
}

// Release deletes l with an empty body.
func (c *Client) Release(ctx context.Context, l *Lease) {
	c.release(ctx, l, nil)
}

// ReleaseWith deletes l with info as the release body (no body at all when
// info is the zero value).
func (c *Client) ReleaseWith(ctx context.Context, l *Lease, info ReleaseInfo) {
	c.release(ctx, l, releaseBody(info))
}

// ReleaseAsync releases l on its own goroutine with context.Background()
// bounded by Options.BookkeepingTimeout; it never blocks the caller.
func (c *Client) ReleaseAsync(l *Lease, info ReleaseInfo) {
	if c.skip(l) {
		return
	}
	body := releaseBody(info)
	go c.release(context.Background(), l, body)
}

// release deletes l, bounded by Options.BookkeepingTimeout. A lease Close
// already released sends nothing.
func (c *Client) release(ctx context.Context, l *Lease, body map[string]any) {
	if c.skip(l) || c.retired(l) {
		return
	}
	ctx, cancel := c.bookkeeping(ctx)
	defer cancel()
	c.deleteLease(ctx, l, body)
}

// deleteLease sends the DELETE of l and, whatever the answer (204, 404, a
// transport failure), marks l released and forgets it: the holder is done
// with it, and an undeleted lease expires on the core by TTL.
func (c *Client) deleteLease(ctx context.Context, l *Lease, body map[string]any) {
	_, _ = c.do(ctx, http.MethodDelete, leasesPath+"/"+l.ID, body)
	l.SetState(StateReleased, nil)
	c.unregister(l.ID)
}

// GetJSON sends GET path to the core and decodes envelope.data into out.
//
// Errors are those of the lease requests: a plain 404 is ErrNotFound, a 404
// with 90002 ErrLeaseGone, an unreachable core or 5xx wraps ErrUnavailable.
// A 204 answer leaves out untouched; a nil out only checks the answer.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	data, noContent, err := c.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if noContent || out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: decode %s: %v", ErrUnavailable, path, err)
	}
	return nil
}
