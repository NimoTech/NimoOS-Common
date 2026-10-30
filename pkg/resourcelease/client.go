package resourcelease

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	defaultWarnEvery           = 5 * time.Minute
	defaultQueuedCancelTimeout = 5 * time.Second
)

// Client talks to the core resource scheduler on behalf of one service.
// It is safe for concurrent use.
type Client struct {
	opts Options

	// closeCtx is cancelled (cause ErrClosed) by Close; every Acquire joins
	// it so a pending wait ends when the client shuts down.
	closeCtx    context.Context
	closeCancel context.CancelCauseFunc

	mu        sync.Mutex
	lastWarn  time.Time
	coolUntil time.Time
	leases    map[string]*Lease // registered leases by id
	closed    bool

	// keep holds the KeepOnClose marks by lease id, with the entry Close
	// writes to the handoff file; holds the running Hold goroutines.
	keep  map[string]HandoffEntry
	holds map[*hold]struct{}

	// handoffMu serialises the handoff file operations. handoffProcessed:
	// ReleaseHandoff took over the file a previous process left;
	// handoffWritten: Close wrote this process' own file.
	handoffMu        sync.Mutex
	handoffProcessed bool
	handoffWritten   bool
}

// New validates opts and returns a Client. It fails when Client is "" or
// both URLFile and BaseURL are "".
func New(opts Options) (*Client, error) {
	if opts.Client == "" {
		return nil, errors.New("resourcelease: Options.Client is required")
	}
	if opts.URLFile == "" && opts.BaseURL == "" {
		return nil, errors.New("resourcelease: Options.URLFile or Options.BaseURL is required")
	}
	closeCtx, closeCancel := context.WithCancelCause(context.Background())
	return &Client{
		opts:        opts,
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
		leases:      map[string]*Lease{},
	}, nil
}

// Name returns Options.Client (the wire "client").
func (c *Client) Name() string {
	return c.opts.Client
}

// Enabled reports whether Acquire may send requests (Options.Disabled false).
func (c *Client) Enabled() bool {
	return !c.opts.Disabled
}

type acquireConfig struct {
	onWait   func(ctx context.Context) error
	pollWait time.Duration
}

// AcquireOption tunes one Acquire call.
type AcquireOption func(*acquireConfig)

// WithOnWait runs fn before every queued long-poll and every backoff sleep.
// A non-nil error cancels the queued lease (DELETE) and ends Acquire with
// OutcomeWithdrawn and Err = that error.
func WithOnWait(fn func(ctx context.Context) error) AcquireOption {
	return func(cfg *acquireConfig) { cfg.onWait = fn }
}

// WithPollWait overrides Options.PollWait for one Acquire call (the queued
// long-poll wait), e.g. a service that keeps a per-call poll period.
func WithPollWait(d time.Duration) AcquireOption {
	return func(cfg *acquireConfig) { cfg.pollWait = d }
}

// logger is Options.Logger, or the global zap logger at the time of the call
// (services replace it after logger.LogInit).
func (c *Client) logger() *zap.Logger {
	if c.opts.Logger != nil {
		return c.opts.Logger
	}
	return zap.L()
}

// warn logs a fail-open warning, at most once per Options.WarnEvery per
// client.
func (c *Client) warn(msg string, fields ...zap.Field) {
	every := c.opts.WarnEvery
	if every <= 0 {
		every = defaultWarnEvery
	}
	c.mu.Lock()
	if !c.lastWarn.IsZero() && time.Since(c.lastWarn) < every {
		c.mu.Unlock()
		return
	}
	c.lastWarn = time.Now()
	c.mu.Unlock()
	c.logger().Warn("admission fail-open: "+msg, fields...)
}

// warnUnlessWithdrawn is warn, except for an Acquire its caller withdrew
// (ctx cancelled with cause ErrWithdrawn): nothing failed, so it is only a
// debug line and does not use up the warning throttle.
func (c *Client) warnUnlessWithdrawn(ctx context.Context, msg string, fields ...zap.Field) {
	if withdrawn(ctx) {
		c.logger().Debug("admission: acquire withdrawn by its caller ("+msg+")", fields...)
		return
	}
	c.warn(msg, fields...)
}

func withdrawn(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrWithdrawn)
}

// register records l as live; it reports false once the client is closed.
func (c *Client) register(l *Lease) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if c.leases == nil {
		c.leases = map[string]*Lease{}
	}
	c.leases[l.ID] = l
	return true
}

// unregister forgets the lease with this id (any *Lease registered under it)
// and its KeepOnClose mark: the lease is released or gone.
func (c *Client) unregister(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.leases, id)
	delete(c.keep, id)
}

func (c *Client) cooling() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.coolUntil)
}

// coolDown makes later requests try the core once, without backoff, until
// Options.Retry.Cooldown has passed (no-op when Cooldown is 0).
func (c *Client) coolDown() {
	if c.opts.Retry.Cooldown <= 0 {
		return
	}
	c.mu.Lock()
	c.coolUntil = time.Now().Add(c.opts.Retry.Cooldown)
	c.mu.Unlock()
}

// onWaitError carries the error of a WithOnWait callback out of doRetrying.
type onWaitError struct{ err error }

func (e *onWaitError) Error() string { return "on-wait callback: " + e.err.Error() }
func (e *onWaitError) Unwrap() error { return e.err }

// doRetrying is do with the retry policy: only transient failures are
// retried, a non-idempotent request only when it provably was not sent
// (errNotSent), never while cooling down, never past the deadline. An
// exhausted backoff cools the client down.
//
// build returns the path and body of attempt n (0 = the first): a retry
// after a backoff sleep recomputes wait_ms from what is left of the
// deadline, so the long-poll still ends before it.
func (c *Client) doRetrying(ctx context.Context, deadline time.Time, idempotent bool, onWait func(context.Context) error, method string, build func(attempt int) (string, any)) (*leaseWire, error) {
	backoff := c.opts.Retry.Backoff
	cooling := c.cooling()
	for i := 0; ; i++ {
		path, body := build(i)
		lw, err := c.do(ctx, method, path, body)
		if err == nil || cooling || !errors.Is(err, errTransient) {
			return lw, err
		}
		if !idempotent && !errors.Is(err, errNotSent) {
			return lw, err
		}
		if i >= len(backoff) {
			c.coolDown()
			return lw, err
		}
		wait := backoff[i]
		if !deadline.IsZero() && time.Until(deadline) <= wait {
			// No retry fits before the deadline: give up now rather than
			// send a request past MaxWait.
			if c.opts.Retry.CooldownOnDeadline {
				c.coolDown()
			}
			return lw, err
		}
		if onWait != nil {
			if werr := onWait(ctx); werr != nil {
				return nil, &onWaitError{err: werr}
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return lw, err
		case <-t.C:
		}
	}
}

// acquireRun is the state of one Acquire call.
type acquireRun struct {
	c        *Client
	ctx      context.Context // the caller's ctx
	actx     context.Context // ctx + deadline+ReplySlack + Close
	r        Request
	cfg      acquireConfig
	deadline time.Time // zero = unbounded
	poll     time.Duration
	noRetry  bool
	payload  map[string]any
}

// Acquire requests a lease and returns a structured Result; Result.Lease is
// never nil.
func (c *Client) Acquire(ctx context.Context, r Request, opts ...AcquireOption) Result {
	var cfg acquireConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if c.opts.Disabled {
		return failOpenResult(r, ReasonDisabled, nil)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return failOpenResult(r, ReasonClosed, ErrClosed)
	}

	run := &acquireRun{c: c, ctx: ctx, r: r, cfg: cfg}
	// The caller's bound covers the whole Acquire: the POST itself
	// long-polls for a grant, so the deadline starts before it.
	if r.MaxWait > 0 {
		run.deadline = time.Now().Add(r.MaxWait)
	}
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if !run.deadline.IsZero() {
		var cancelDl context.CancelFunc
		actx, cancelDl = context.WithDeadline(actx, run.deadline.Add(c.opts.ReplySlack))
		defer cancelDl()
	}
	if c.closeCtx != nil {
		stop := context.AfterFunc(c.closeCtx, func() { cancel(ErrClosed) })
		defer stop()
	}
	run.actx = actx

	run.poll = cfg.pollWait
	if run.poll <= 0 {
		run.poll = c.opts.PollWait
	}
	if run.poll <= 0 || run.poll > MaxLongPoll {
		run.poll = MaxLongPoll
	}
	run.noRetry = c.opts.Retry.NoRetryInteractive && r.Tier == TierInteractive
	run.payload = buildPayload(c.opts.Client, c.opts.DefaultClass, r, waitMsFor(r, run.poll))
	return run.acquire()
}

// remainingWaitMs is the long-poll capped at what is left of the deadline,
// at least 1 ms.
func (a *acquireRun) remainingWaitMs() int {
	w := a.poll
	if !a.deadline.IsZero() {
		w = min(w, time.Until(a.deadline))
	}
	return max(int(w/time.Millisecond), 1)
}

func (a *acquireRun) pastDeadline() bool {
	return !a.deadline.IsZero() && !time.Now().Before(a.deadline)
}

func (a *acquireRun) closing() bool {
	return errors.Is(context.Cause(a.actx), ErrClosed)
}

func (a *acquireRun) post() (*leaseWire, error) {
	switch {
	case a.r.MaxWait == 0:
		pctx := a.actx
		if a.c.opts.NoWaitTimeout > 0 {
			var cancel context.CancelFunc
			pctx, cancel = context.WithTimeout(pctx, a.c.opts.NoWaitTimeout)
			defer cancel()
		}
		return a.c.do(pctx, http.MethodPost, leasesPath, a.payload)
	case a.noRetry:
		return a.c.do(a.actx, http.MethodPost, leasesPath, a.payload)
	}
	return a.c.doRetrying(a.actx, a.deadline, false, a.cfg.onWait, http.MethodPost, a.postAttempt)
}

// postAttempt is the acquire POST of attempt n. The first attempt sends the
// payload as built (wait_ms of the wire baseline); a retry of a bounded
// Acquire caps wait_ms at what is left of the deadline.
func (a *acquireRun) postAttempt(attempt int) (string, any) {
	if attempt > 0 && !a.deadline.IsZero() {
		a.payload["wait_ms"] = a.remainingWaitMs()
	}
	return leasesPath, a.payload
}

// pollAttempt is the queued long-poll GET of lease id; every attempt caps
// wait_ms at what is left of the deadline.
func (a *acquireRun) pollAttempt(id string) func(int) (string, any) {
	return func(int) (string, any) {
		return leasesPath + "/" + id + "?wait_ms=" + strconv.Itoa(a.remainingWaitMs()), nil
	}
}

func (a *acquireRun) newLease(lw *leaseWire) *Lease {
	l := &Lease{
		ID:               lw.ID,
		Device:           lw.Device,
		Tier:             a.r.Tier,
		State:            lw.State,
		ReleaseRequested: lw.ReleaseRequested,
		owner:            a.c,
		workload:         a.r.Workload,
		pid:              a.r.PID,
	}
	if l.Device == "" {
		l.Device = placeholderDevice(a.r)
	}
	return l
}

func (a *acquireRun) acquire() Result {
	lw, err := a.post()
	if err != nil {
		return a.failed(err, "acquire")
	}
	lease := a.newLease(lw)
	switch lease.State {
	case StateHeld:
		return a.held(lease)
	case StateQueued:
	default:
		return notUsableResult(lease)
	}
	if !a.c.register(lease) {
		a.c.ReleaseAsync(lease, ReleaseInfo{})
		return failOpenResult(a.r, ReasonClosed, ErrClosed)
	}
	if a.r.MaxWait == 0 { // the caller refuses to wait: cancel and proceed
		a.c.release(a.ctx, lease, nil)
		return failOpenResult(a.r, ReasonQueuedNoWait, nil)
	}

	requeues := 0
	for {
		if a.actx.Err() != nil || a.pastDeadline() {
			return a.waitEnded(lease, false)
		}
		if a.cfg.onWait != nil {
			if werr := a.cfg.onWait(a.actx); werr != nil {
				return a.onWaitFailed(lease, werr)
			}
		}
		if a.noRetry {
			path, _ := a.pollAttempt(lease.ID)(0)
			lw, err = a.c.do(a.actx, http.MethodGet, path, nil)
		} else {
			lw, err = a.c.doRetrying(a.actx, a.deadline, true, a.cfg.onWait, http.MethodGet, a.pollAttempt(lease.ID))
		}
		if err == nil && lw.ID == "" { // 204 or an answer without a lease
			err = fmt.Errorf("%w: poll answer without a lease", ErrUnavailable)
		}
		var owe *onWaitError
		switch {
		case errors.As(err, &owe):
			return a.onWaitFailed(lease, owe.err)
		case errors.Is(err, ErrLeaseGone):
			lease.SetState(StateLost, nil)
			a.c.unregister(lease.ID)
			requeues++
			limit := a.c.opts.RequeueLimit
			if limit <= 0 {
				a.c.warnUnlessWithdrawn(a.ctx, "poll", zap.String("task", a.r.Task), zap.Error(err))
				return failOpenResult(a.r, ReasonUnavailable, err)
			}
			if requeues > limit {
				a.c.warn("lease lost repeatedly while queued", zap.String("task", a.r.Task))
				return failOpenResult(a.r, ReasonRequeueLimit, err)
			}
			a.c.logger().Warn("admission lease vanished while queued; re-queueing",
				zap.String("task", a.r.Task), zap.Int("requeues", requeues))
			if a.actx.Err() != nil || a.pastDeadline() {
				return a.waitEnded(nil, false)
			}
			a.payload["wait_ms"] = a.remainingWaitMs()
			if a.noRetry {
				lw, err = a.c.do(a.actx, http.MethodPost, leasesPath, a.payload)
			} else {
				lw, err = a.c.doRetrying(a.actx, a.deadline, false, a.cfg.onWait, http.MethodPost, a.postAttempt)
			}
			if err != nil {
				return a.failed(err, "requeue")
			}
			lease = a.newLease(lw) // re-registration always yields a new *Lease
			switch lease.State {
			case StateHeld:
				return a.held(lease)
			case StateQueued:
			default:
				return notUsableResult(lease)
			}
			if !a.c.register(lease) {
				a.c.ReleaseAsync(lease, ReleaseInfo{})
				return failOpenResult(a.r, ReasonClosed, ErrClosed)
			}
			continue
		case err != nil:
			if a.actx.Err() != nil {
				// The deadline, the caller or Close cut the poll short; the
				// core may be hung, so the cancel follows the poll-error rule.
				return a.waitEnded(lease, a.c.opts.AsyncCancelOnPollError)
			}
			if a.c.opts.AsyncCancelOnPollError {
				go a.c.cancelQueued(lease) // the core may hang: cancel off the caller's path
			} else {
				a.c.cancelQueued(lease)
			}
			a.c.warnUnlessWithdrawn(a.ctx, "poll", zap.String("task", a.r.Task), zap.Error(err))
			return failOpenResult(a.r, ReasonUnavailable, err)
		}
		if !lease.update(lw) {
			// Only Close touches a registered queued lease meanwhile: it
			// released it, so this answer must not bring it back.
			return failOpenResult(a.r, ReasonClosed, ErrClosed)
		}
		if lw.Device != "" {
			lease.Device = lw.Device
		}
		switch lw.State {
		case StateHeld:
			if a.closing() || a.c.isClosed() {
				// Granted while Close is deleting it: Close's DELETE may not
				// have been answered yet, so make sure the lease goes.
				a.c.ReleaseAsync(lease, ReleaseInfo{})
				return failOpenResult(a.r, ReasonClosed, ErrClosed)
			}
			return Result{Outcome: OutcomeHeld, Lease: lease}
		case StateQueued:
		default:
			a.c.unregister(lease.ID)
			return notUsableResult(lease)
		}
	}
}

// held registers a granted lease; a grant that arrives after Close is
// released at once.
func (a *acquireRun) held(lease *Lease) Result {
	if !a.c.register(lease) {
		a.c.ReleaseAsync(lease, ReleaseInfo{})
		return failOpenResult(a.r, ReasonClosed, ErrClosed)
	}
	return Result{Outcome: OutcomeHeld, Lease: lease}
}

// waitEnded ends a wait the deadline, the caller or Close cut short. The
// queued lease (if any) is cancelled (on its own goroutine when async),
// except on Close, which deletes every registered lease itself.
func (a *acquireRun) waitEnded(lease *Lease, async bool) Result {
	if a.closing() {
		return failOpenResult(a.r, ReasonClosed, ErrClosed)
	}
	if lease != nil {
		if async {
			go a.c.cancelQueued(lease)
		} else {
			a.c.cancelQueued(lease)
		}
	}
	if withdrawn(a.ctx) {
		a.c.logger().Debug("admission: acquire withdrawn by its caller (wait exceeded)", zap.String("task", a.r.Task))
		return withdrawnResult(a.r, context.Cause(a.ctx))
	}
	a.c.warn("wait exceeded", zap.String("task", a.r.Task))
	err := context.Cause(a.actx)
	if err == nil {
		err = context.DeadlineExceeded
	}
	return failOpenResult(a.r, ReasonTimedOut, err)
}

func (a *acquireRun) onWaitFailed(lease *Lease, err error) Result {
	if lease != nil {
		a.c.cancelQueued(lease)
	}
	return withdrawnResult(a.r, err)
}

// failed maps an error of an acquire POST to its Result.
func (a *acquireRun) failed(err error, what string) Result {
	var owe *onWaitError
	switch {
	case errors.As(err, &owe):
		return withdrawnResult(a.r, owe.err)
	case errors.Is(err, ErrBudgetRejected):
		return rejectedResult(a.r, CodeInvalidBudget, err)
	case errors.Is(err, ErrInvalidRequest):
		a.c.logger().Error("resource lease request rejected", zap.String("task", a.r.Task), zap.Error(err))
		return rejectedResult(a.r, CodeInvalidRequest, err)
	case a.closing():
		return failOpenResult(a.r, ReasonClosed, ErrClosed)
	case withdrawn(a.ctx):
		a.c.logger().Debug("admission: acquire withdrawn by its caller ("+what+")", zap.String("task", a.r.Task))
		return withdrawnResult(a.r, context.Cause(a.ctx))
	}
	reason := ReasonUnavailable
	switch {
	case errors.Is(err, errDisabled):
		reason = ReasonDisabled
	case errors.Is(err, errRefused), errors.Is(err, ErrNotFound), errors.Is(err, ErrLeaseGone):
		reason = ReasonRefused
	}
	a.c.warn(what, zap.String("task", a.r.Task), zap.Error(err))
	return failOpenResult(a.r, reason, err)
}

// cancelQueued deletes an abandoned queued lease with a fresh context
// bounded by Options.QueuedCancelTimeout (the caller's ctx may be over).
func (c *Client) cancelQueued(l *Lease) {
	timeout := c.opts.QueuedCancelTimeout
	if timeout <= 0 {
		timeout = defaultQueuedCancelTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c.deleteLease(ctx, l, nil)
}

// placeholderDevice is the device a placeholder lease reports:
// DevicePref[0], else Devices[0], else "".
func placeholderDevice(r Request) string {
	switch {
	case len(r.DevicePref) > 0:
		return r.DevicePref[0]
	case len(r.Devices) > 0:
		return r.Devices[0]
	}
	return ""
}

// failOpenLease is the placeholder of every fail-open and withdrawn Result:
// no id, held, fail-open, with the preferred device and the request tier.
func failOpenLease(r Request) *Lease {
	return &Lease{Device: placeholderDevice(r), State: StateHeld, FailOpen: true, Tier: r.Tier}
}

func failOpenResult(r Request, reason FailReason, err error) Result {
	return Result{Outcome: OutcomeFailOpen, Lease: failOpenLease(r), Reason: reason, Err: err}
}

func withdrawnResult(r Request, err error) Result {
	if err == nil {
		err = ErrWithdrawn
	}
	return Result{Outcome: OutcomeWithdrawn, Lease: failOpenLease(r), Err: err}
}

func rejectedResult(r Request, code RejectCode, err error) Result {
	l := &Lease{Device: placeholderDevice(r), State: StateRejected, Rejected: true, Tier: r.Tier}
	return Result{Outcome: OutcomeRejected, Lease: l, Code: code, Err: err}
}

// notUsableResult hands back the lease the core answered in a state that is
// neither held nor queued; it is not registered and not deleted.
func notUsableResult(l *Lease) Result {
	state, _ := l.Snapshot()
	return Result{
		Outcome: OutcomeRejected,
		Lease:   l,
		Code:    CodeNotUsable,
		Err:     errors.New("resourcelease: core answered lease state " + strconv.Quote(state)),
	}
}
