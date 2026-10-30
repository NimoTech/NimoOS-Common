package resourcelease

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"go.uber.org/zap"
)

const defaultCloseTimeout = 3 * time.Second

// CloseReport describes what Close did with the registered leases.
type CloseReport struct {
	Released        []string // DELETE answered 204/404
	Kept            []string // KeepOnClose leases (written to HandoffFile when set)
	Failed          []string // DELETE failed or ran out of time
	CancelledQueued int      // queued leases among Released
	HandoffErr      error
}

// KeepOnClose marks l so Close leaves it with the core (and writes it to
// Options.HandoffFile when set) instead of deleting it.
//
// The mark is kept by lease id in the client's registry, so it also applies
// when l is a clone (or any other *Lease) of the registered lease. A lease
// that is not registered (built by hand) is recorded as kept all the same.
// The mark goes away when the lease is released or found gone. Nil,
// fail-open and id-less leases are ignored, as is a call after Close.
func (c *Client) KeepOnClose(l *Lease) {
	if l == nil || l.FailOpen || l.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	// The caller holds l only after Acquire returned, so its identity
	// fields are settled; read them here, under c.mu, once.
	e := HandoffEntry{ID: l.ID, Workload: l.workload, PID: l.pid, Device: l.Device}
	if reg, ok := c.leases[l.ID]; ok && reg != l {
		if e.Workload == "" {
			e.Workload = reg.workload
		}
		if e.PID == 0 {
			e.PID = reg.pid
		}
	}
	if c.keep == nil {
		c.keep = map[string]HandoffEntry{}
	}
	c.keep[l.ID] = e
}

// closeResult is the answer to one DELETE Close sent.
type closeResult struct {
	id       string
	queued   bool
	released bool
}

// Close releases every registered lease except the KeepOnClose ones within
// Options.CloseTimeout. It is idempotent; later calls return an empty report.
//
// Close stops new Acquire calls (they fail open with ReasonClosed, and a
// grant that arrives afterwards is deleted at once), ends pending queued
// waits, stops every Hold, deletes the held and queued leases concurrently
// under one deadline (the earlier of ctx's and now+CloseTimeout) and writes
// the KeepOnClose leases to Options.HandoffFile. Ids whose DELETE is not
// answered by the deadline are reported in Failed; the core expires them by
// TTL. A grant that arrives after Close is deleted with ReleaseAsync, which
// Close does not wait for: if the process exits first, that lease too is
// left to the core's TTL.
func (c *Client) Close(ctx context.Context) CloseReport {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return CloseReport{}
	}
	// Closing and taking the snapshot in one critical section: a lease is
	// either registered before (and in the snapshot) or refused by register
	// (and released by its Acquire).
	c.closed = true
	leases, keep, holds := c.leases, c.keep, c.holds
	c.leases, c.keep, c.holds = map[string]*Lease{}, nil, nil
	c.mu.Unlock()
	if c.closeCancel != nil {
		c.closeCancel(ErrClosed)
	}

	timeout := c.opts.CloseTimeout
	if timeout <= 0 {
		timeout = defaultCloseTimeout
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for h := range holds {
		h.cancel()
	}

	var rep CloseReport
	for id := range keep {
		rep.Kept = append(rep.Kept, id)
	}
	sort.Strings(rep.Kept)

	pending := map[string]bool{}
	results := make(chan closeResult, len(leases))
	for id, l := range leases {
		if _, kept := keep[id]; kept {
			continue
		}
		pending[id] = true
		state, _ := l.Snapshot()
		go func(id string, l *Lease, queued bool) {
			_, err := c.do(dctx, http.MethodDelete, leasesPath+"/"+id, nil)
			l.SetState(StateReleased, nil)
			ok := err == nil || errors.Is(err, ErrLeaseGone) || errors.Is(err, ErrNotFound)
			results <- closeResult{id: id, queued: queued, released: ok}
		}(id, l, state == StateQueued)
	}

	if c.opts.HandoffFile != "" && len(rep.Kept) > 0 {
		entries := make([]HandoffEntry, 0, len(rep.Kept))
		for _, id := range rep.Kept {
			entries = append(entries, keep[id])
		}
		if err := c.writeHandoff(entries); err != nil {
			rep.HandoffErr = err
			c.logger().Error("resource lease handoff file not written",
				zap.String("client", c.opts.Client), zap.String("file", c.opts.HandoffFile),
				zap.Strings("kept", rep.Kept), zap.Error(err))
		}
	}

	// No heartbeat may go out once Close returns: wait for the holds to
	// stop (they were cancelled above), within the deadline.
	for h := range holds {
		select {
		case <-h.done:
		case <-dctx.Done():
		}
	}

collect:
	for len(pending) > 0 {
		select {
		case r := <-results:
			delete(pending, r.id)
			if r.released {
				rep.Released = append(rep.Released, r.id)
				if r.queued {
					rep.CancelledQueued++
				}
			} else {
				rep.Failed = append(rep.Failed, r.id)
			}
		case <-dctx.Done():
			break collect
		}
	}
	for id := range pending {
		rep.Failed = append(rep.Failed, id)
	}
	sort.Strings(rep.Released)
	sort.Strings(rep.Failed)

	c.logger().Info("resource leases closed",
		zap.String("client", c.opts.Client),
		zap.Int("released", len(rep.Released)),
		zap.Int("kept", len(rep.Kept)),
		zap.Int("failed", len(rep.Failed)),
		zap.Int("cancelled_queued", rep.CancelledQueued))
	return rep
}

// isClosed reports whether Close has run.
func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// retired reports whether l was released by Close (or after it): further
// bookkeeping on it sends nothing.
func (c *Client) retired(l *Lease) bool {
	if !c.isClosed() {
		return false
	}
	state, _ := l.Snapshot()
	return state == StateReleased
}
