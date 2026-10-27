package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NimoTech/NimoOS-Common/model"
)

// ErrRegistrationPending is returned by RegisterRoutes when some routes are not
// registered yet after the synchronous window; it keeps retrying in the
// background.
var ErrRegistrationPending = errors.New("gateway route registration pending; retrying in background")

// RegisterOptions tunes RegisterRoutes. The zero value is ready to use.
type RegisterOptions struct {
	// Wait is how long RegisterRoutes retries before returning
	// ErrRegistrationPending. Default 30s.
	Wait time.Duration
	// InitialBackoff and MaxBackoff bound the delay between attempts (doubling,
	// with jitter). Defaults 1s and 60s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// AttemptTimeout bounds one POST to the Gateway. Default 10s.
	AttemptTimeout time.Duration
	// Log receives the few lines worth logging: the first failure, the switch to
	// background retries, a reminder at most every 10 minutes while they last,
	// and the final success (err == nil). Default: the standard log package.
	Log func(msg string, err error)
	// OnRegistered, when set, is called once every route is registered, from
	// the caller's goroutine or the background one.
	OnRegistered func()

	onBackgroundExit func() // tests
}

const backgroundReminderEvery = 10 * time.Minute

// RegisterRoutes registers routes at the Gateway without letting a Gateway
// problem take the calling service down. It retries with backoff for up to
// opts.Wait and returns nil once every route is registered. Otherwise it
// returns an error wrapping ErrRegistrationPending and keeps retrying in a
// background goroutine (backoff capped at opts.MaxBackoff) until every route
// is registered or ctx is done. Every status counts as retryable, 4xx included:
// a Gateway that rejects us because it is older than we are starts accepting
// once it is upgraded.
//
// Each attempt reads the Gateway's address again from
// <runtimePath>/management.url, so a Gateway that is not up yet, or restarts on
// a new port, is picked up. Routes already accepted are not submitted again.
func RegisterRoutes(ctx context.Context, runtimePath string, routes []*model.Route, opts RegisterOptions) error {
	opts = opts.withDefaults()
	r := &registrar{
		addressFile: filepath.Join(runtimePath, ManagementURLFilename),
		all:         append([]*model.Route(nil), routes...),
		pending:     append([]*model.Route(nil), routes...),
		opts:        opts,
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(opts.Wait)
	// Every synchronous attempt shares this deadline, so a Gateway that accepts
	// connections but never answers cannot stretch the phase to AttemptTimeout
	// per route.
	syncCtx, cancelSync := context.WithDeadline(ctx, deadline)
	defer cancelSync()
	backoff := opts.InitialBackoff
	for {
		if r.attempt(syncCtx) {
			r.done()
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if syncCtx.Err() != nil {
			break
		}
		if time.Now().Add(backoff).After(deadline) {
			break
		}
		if !sleep(ctx, jitter(backoff)) {
			return ctx.Err()
		}
		backoff = next(backoff, opts.MaxBackoff)
	}

	err := fmt.Errorf("%w: %d route(s) left, last error: %v", ErrRegistrationPending, len(r.pending), r.lastErr)
	opts.Log("gateway route registration still failing; retrying in background", r.lastErr)
	go r.background(ctx, backoff)
	return err
}

func (o RegisterOptions) withDefaults() RegisterOptions {
	if o.Wait <= 0 {
		o.Wait = 30 * time.Second
	}
	if o.InitialBackoff <= 0 {
		o.InitialBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 60 * time.Second
	}
	if o.MaxBackoff < o.InitialBackoff {
		o.MaxBackoff = o.InitialBackoff
	}
	if o.AttemptTimeout <= 0 {
		o.AttemptTimeout = 10 * time.Second
	}
	if o.Log == nil {
		o.Log = func(msg string, err error) {
			if err != nil {
				log.Printf("%s: %v", msg, err)
			} else {
				log.Print(msg)
			}
		}
	}
	return o
}

type registrar struct {
	addressFile string
	opts        RegisterOptions
	all         []*model.Route

	mu          sync.Mutex
	pending     []*model.Route
	acceptedAt  string // Gateway address the routes not in pending were accepted at
	lastErr     error
	loggedFirst bool
}

// attempt submits every pending route once; it reports whether none is left.
func (r *registrar) attempt(ctx context.Context) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.all) == 0 {
		return true
	}
	address, err := getAddress(r.addressFile)
	if err == nil {
		address = strings.TrimSuffix(strings.TrimSpace(address), "/")
		if address == "" {
			err = errors.New("gateway address file is empty: " + r.addressFile)
		}
	}
	if err != nil {
		r.fail(fmt.Errorf("gateway address: %w", err))
		return false
	}
	if r.acceptedAt != "" && r.acceptedAt != address {
		// A new Gateway instance: what the old one accepted may not exist here.
		r.pending = append([]*model.Route(nil), r.all...)
	}
	r.acceptedAt = address
	left := r.pending[:0]
	for i, route := range r.pending {
		if ctx.Err() != nil {
			left = append(left, r.pending[i:]...)
			break
		}
		if err := r.post(ctx, address, route); err != nil {
			r.fail(fmt.Errorf("register %s: %w", route.Path, err))
			left = append(left, route)
		}
	}
	r.pending = left
	return len(r.pending) == 0
}

func (r *registrar) fail(err error) {
	r.lastErr = err
	if !r.loggedFirst {
		r.loggedFirst = true
		r.opts.Log("gateway route registration failed; retrying", err)
	}
}

func (r *registrar) post(ctx context.Context, address string, route *model.Route) error {
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}
	actx, cancel := context.WithTimeout(ctx, r.opts.AttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost,
		address+"/"+strings.TrimPrefix(APIGatewayRoutes, "/"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}
	return nil
}

func (r *registrar) done() {
	if r.loggedFirst {
		r.opts.Log("gateway routes registered", nil)
	}
	if r.opts.OnRegistered != nil {
		r.opts.OnRegistered()
	}
}

func (r *registrar) background(ctx context.Context, backoff time.Duration) {
	if r.opts.onBackgroundExit != nil {
		defer r.opts.onBackgroundExit()
	}
	lastReminder := time.Now()
	for {
		if !sleep(ctx, jitter(backoff)) {
			return
		}
		if r.attempt(ctx) {
			r.done()
			return
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(lastReminder) >= backgroundReminderEvery {
			lastReminder = time.Now()
			r.mu.Lock()
			err := r.lastErr
			r.mu.Unlock()
			r.opts.Log("gateway route registration still pending", err)
		}
		backoff = next(backoff, r.opts.MaxBackoff)
	}
}

func next(d, max time.Duration) time.Duration {
	if d *= 2; d > max {
		return max
	}
	return d
}

// jitter spreads d by ±20% so services restarted together do not retry in step.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) / 5
	if spread == 0 {
		return d
	}
	return time.Duration(int64(d) - spread + rand.Int63n(2*spread+1))
}

// sleep waits d or until ctx is done; it reports whether the full wait elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
