package resourcelease_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	rl "github.com/NimoTech/NimoOS-Common/pkg/resourcelease"
	"github.com/NimoTech/NimoOS-Common/pkg/resourcelease/leasetest"
)

const leasesPath = "/v1/resources/leases"

// photosOpts mirrors the Options Photos builds (Ph2 table).
func photosOpts(f *leasetest.FakeCore) rl.Options {
	return rl.Options{
		Client:              "photos",
		URLFile:             f.URLFile,
		HTTPClient:          &http.Client{Timeout: 35 * time.Second},
		DefaultClass:        rl.ClassLight,
		Retry:               rl.RetryPolicy{Backoff: rl.DefaultRetry().Backoff, Cooldown: time.Minute, NoRetryInteractive: true, CooldownOnDeadline: true},
		ReplySlack:          2 * time.Second,
		NoWaitTimeout:       2 * time.Second,
		BookkeepingTimeout:  2 * time.Second,
		QueuedCancelTimeout: 2 * time.Second,
		RequeueLimit:        5,
		WaitRelease:         rl.WaitReleasePolicy{Min: time.Second, Slack: 5 * time.Second, GoneMarksLost: true},
		WarnEvery:           5 * time.Minute,
	}
}

// aiOpts mirrors the Options NimoOS-AI builds (AI2 table).
func aiOpts(f *leasetest.FakeCore) rl.Options {
	return rl.Options{
		Client:                 "ai",
		URLFile:                f.URLFile,
		HTTPClient:             &http.Client{Timeout: 35 * time.Second},
		DefaultClass:           rl.ClassHeavy,
		ReplySlack:             500 * time.Millisecond,
		QueuedCancelTimeout:    5 * time.Second,
		AsyncCancelOnPollError: true,
		RequeueLimit:           0,
		WaitRelease:            rl.WaitReleasePolicy{Min: time.Millisecond},
		WarnEvery:              5 * time.Minute,
	}
}

func newClient(t *testing.T, opts rl.Options) *rl.Client {
	t.Helper()
	c, err := rl.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// observed injects an observer logger into opts.
func observed(opts *rl.Options) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	opts.Logger = zap.New(core)
	return logs
}

func failOpenWarns(logs *observer.ObservedLogs) int {
	n := 0
	for _, e := range logs.All() {
		if e.Level == zapcore.WarnLevel && strings.HasPrefix(e.Message, "admission fail-open") {
			n++
		}
	}
	return n
}

func env(data map[string]any) map[string]any {
	return map[string]any{"success": 200, "message": "ok", "data": data}
}

// filter returns the recorded requests with method whose path satisfies
// match ("" = any path).
func filter(f *leasetest.FakeCore, method string, match func(path string) bool) []leasetest.Recorded {
	var out []leasetest.Recorded
	for _, r := range f.Requests() {
		if r.Method == method && (match == nil || match(r.Path)) {
			out = append(out, r)
		}
	}
	return out
}

func isCreate(p string) bool { return p == leasesPath }
func isLease(p string) bool {
	return strings.HasPrefix(p, leasesPath+"/") && !strings.HasSuffix(p, "/heartbeat")
}

func posts(f *leasetest.FakeCore) []leasetest.Recorded { return filter(f, http.MethodPost, isCreate) }
func gets(f *leasetest.FakeCore) []leasetest.Recorded  { return filter(f, http.MethodGet, isLease) }
func deletes(f *leasetest.FakeCore) []leasetest.Recorded {
	return filter(f, http.MethodDelete, isLease)
}

// eventually polls cond for up to d.
func eventually(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal(msg)
	}
}

func waitMs(t *testing.T, r leasetest.Recorded) int {
	t.Helper()
	for _, kv := range strings.Split(r.Query, "&") {
		if v, ok := strings.CutPrefix(kv, "wait_ms="); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("bad wait_ms in %q", r.Query)
			}
			return n
		}
	}
	t.Fatalf("no wait_ms in %q", r.Query)
	return 0
}

func bodyWaitMs(t *testing.T, r leasetest.Recorded) int {
	t.Helper()
	v, ok := r.Body["wait_ms"].(float64)
	if !ok {
		t.Fatalf("no wait_ms in body %v", r.Body)
	}
	return int(v)
}

// dialer counts dial attempts; the first fails attempts are refused (and
// fails < 0 refuses every one), the others dial for real.
type dialer struct {
	fails    int32
	attempts atomic.Int32
}

func (d *dialer) client() *http.Client {
	return &http.Client{
		Timeout: 35 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				n := d.attempts.Add(1)
				if d.fails < 0 || n <= d.fails {
					return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
				}
				var nd net.Dialer
				return nd.DialContext(ctx, network, addr)
			},
		},
	}
}

func assertPlaceholder(t *testing.T, l *rl.Lease, device string, tier rl.Tier) {
	t.Helper()
	if l == nil {
		t.Fatal("nil lease")
	}
	if l.ID != "" || l.Device != device || l.Tier != tier {
		t.Fatalf("placeholder = {ID:%q Device:%q Tier:%q}, want {\"\" %q %q}", l.ID, l.Device, l.Tier, device, tier)
	}
}

func req(maxWait time.Duration) rl.Request {
	return rl.Request{Task: "t", DevicePref: []string{"gpu.1"}, Tier: rl.TierUserBatch, Workload: "w", PID: 42, MaxWait: maxWait}
}

func TestAcquireHeldImmediately(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeHeld || res.Lease.ID != "1-fake" || res.Lease.Device != "gpu.1" || !res.Lease.Usable() {
		t.Fatalf("result = %+v lease %+v", res, res.Lease)
	}
	if res.Lease.Tier != rl.TierUserBatch || res.Compat() != res.Lease {
		t.Fatalf("tier %q / compat identity", res.Lease.Tier)
	}
	if !rl.RegisteredForTest(c, "1-fake") {
		t.Fatal("held lease must be registered")
	}

	// An empty core device falls back to DevicePref[0].
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodPost && r.Path == leasesPath {
			return 200, env(map[string]any{"id": "x-1", "state": "held", "device": ""}), true
		}
		return 0, nil, false
	})
	res = c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeHeld || res.Lease.ID != "x-1" || res.Lease.Device != "gpu.1" {
		t.Fatalf("empty device: %+v", res.Lease)
	}
}

func TestAcquireQueuedThenHeld(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.QueueNext(2)
	res := c.Acquire(context.Background(), req(-1), rl.WithPollWait(20*time.Millisecond))
	if res.Outcome != rl.OutcomeHeld || res.Lease.ID != "1-fake" {
		t.Fatalf("result %+v", res)
	}
	if state, _ := res.Lease.Snapshot(); state != rl.StateHeld {
		t.Fatalf("state %q", state)
	}
	if n := len(posts(f)); n != 1 {
		t.Fatalf("posts = %d", n)
	}
	g := gets(f)
	if len(g) != 2 {
		t.Fatalf("gets = %d", len(g))
	}
	for _, r := range g {
		if r.Path != leasesPath+"/1-fake" || r.Query != "wait_ms=20" {
			t.Fatalf("poll = %s?%s", r.Path, r.Query)
		}
	}
	if !rl.RegisteredForTest(c, "1-fake") {
		t.Fatal("granted lease must stay registered")
	}
}

func TestAcquireNoWaitCancelsQueued(t *testing.T) {
	for name, mk := range map[string]func(*leasetest.FakeCore) rl.Options{"photos": photosOpts, "ai": aiOpts} {
		t.Run(name, func(t *testing.T) {
			f := leasetest.New(t)
			c := newClient(t, mk(f))
			f.QueueNext(0)
			res := c.Acquire(context.Background(), req(0))
			if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonQueuedNoWait {
				t.Fatalf("result %+v", res)
			}
			assertPlaceholder(t, res.Lease, "gpu.1", rl.TierUserBatch)
			p := posts(f)
			if len(p) != 1 || bodyWaitMs(t, p[0]) != 0 {
				t.Fatalf("posts %+v", p)
			}
			d := deletes(f)
			if len(d) != 1 || d[0].Path != leasesPath+"/1-fake" {
				t.Fatalf("deletes %+v", d)
			}
			if len(f.Live()) != 0 || rl.RegisteredForTest(c, "1-fake") {
				t.Fatal("queued lease must be gone")
			}
		})
	}
}

func TestAcquireNoWaitTimeoutBoundsPost(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.Hang(5 * time.Second)
	start := time.Now()
	res := c.Acquire(context.Background(), req(0))
	if el := time.Since(start); el > 2500*time.Millisecond {
		t.Fatalf("took %s", el)
	}
	if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonUnavailable {
		t.Fatalf("result %+v", res)
	}
}

func TestAcquireDeadlineCountsFromBeforePost(t *testing.T) {
	for name, mk := range map[string]func(*leasetest.FakeCore) rl.Options{"photos": photosOpts, "ai": aiOpts} {
		t.Run(name, func(t *testing.T) {
			f := leasetest.New(t)
			opts := mk(f)
			c := newClient(t, opts)
			f.QueueNext(0)
			f.Hang(100 * time.Millisecond) // the POST answers 100 ms in
			start := time.Now()
			res := c.Acquire(context.Background(), req(300*time.Millisecond))
			if el := time.Since(start); el > 300*time.Millisecond+opts.ReplySlack+300*time.Millisecond {
				t.Fatalf("took %s", el)
			}
			if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonTimedOut {
				t.Fatalf("result %+v", res)
			}
			assertPlaceholder(t, res.Lease, "gpu.1", rl.TierUserBatch)
			if w := bodyWaitMs(t, posts(f)[0]); w != 300 {
				t.Fatalf("POST wait_ms = %d", w)
			}
			for _, g := range gets(f) {
				// The deadline started before the POST: what is left after the
				// 100 ms POST is below 300 ms.
				if w := waitMs(t, g); w > 210 || w < 1 {
					t.Fatalf("GET wait_ms = %d", w)
				}
			}
			eventually(t, time.Second, func() bool { return len(deletes(f)) == 1 }, "queued lease must be DELETEd")
		})
	}
}

func TestAcquirePollWaitCappedToRemaining(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.QueueNext(0)
	var polls atomic.Int32
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method != http.MethodGet {
			return 0, nil, false
		}
		state := "queued"
		if polls.Add(1) >= 3 {
			state = "held"
		}
		return 200, env(map[string]any{"id": "1-fake", "state": state, "device": "gpu.1"}), true
	})
	res := c.Acquire(context.Background(), req(15*time.Second))
	if res.Outcome != rl.OutcomeHeld {
		t.Fatalf("result %+v", res)
	}
	if w := bodyWaitMs(t, posts(f)[0]); w != 15000 {
		t.Fatalf("POST wait_ms = %d", w)
	}
	g := gets(f)
	if len(g) != 3 {
		t.Fatalf("gets = %d", len(g))
	}
	prev := 15000
	for _, r := range g {
		w := waitMs(t, r)
		if w < 1 || w > prev {
			t.Fatalf("GET wait_ms = %d after %d", w, prev)
		}
		prev = w
	}
}

func TestAcquireUnboundedUsesPollWait(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	c.Acquire(context.Background(), req(-1))
	c.Acquire(context.Background(), req(-1), rl.WithPollWait(50*time.Millisecond))
	p := posts(f)
	if bodyWaitMs(t, p[0]) != 30000 || bodyWaitMs(t, p[1]) != 50 {
		t.Fatalf("wait_ms %v / %v", p[0].Body["wait_ms"], p[1].Body["wait_ms"])
	}
	opts := photosOpts(f)
	opts.PollWait = 2 * time.Second
	newClient(t, opts).Acquire(context.Background(), req(-1))
	if w := bodyWaitMs(t, posts(f)[2]); w != 2000 {
		t.Fatalf("Options.PollWait: wait_ms %d", w)
	}
}

// dropOnPoll makes the fake core drop a queued lease when it is polled
// (the first n polls; n < 0 = every poll).
func dropOnPoll(f *leasetest.FakeCore, n int) {
	var seen atomic.Int32
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.Path, leasesPath+"/") {
			if n < 0 || int(seen.Add(1)) <= n {
				f.Drop(strings.TrimPrefix(r.Path, leasesPath+"/"))
			}
		}
		return 0, nil, false
	})
}

func TestAcquireQueuedGoneIsRequeued(t *testing.T) {
	t.Run("limit", func(t *testing.T) {
		f := leasetest.New(t)
		opts := photosOpts(f)
		logs := observed(&opts)
		c := newClient(t, opts)
		for i := 0; i < 10; i++ {
			f.QueueNext(0)
		}
		dropOnPoll(f, -1)
		res := c.Acquire(context.Background(), req(-1))
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonRequeueLimit {
			t.Fatalf("result %+v", res)
		}
		assertPlaceholder(t, res.Lease, "gpu.1", rl.TierUserBatch)
		if n := len(posts(f)); n != 6 {
			t.Fatalf("posts = %d, want 1 + 5 requeues", n)
		}
		if n := failOpenWarns(logs); n != 1 {
			t.Fatalf("fail-open warns = %d", n)
		}
	})
	t.Run("granted after a requeue", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		f.QueueNext(0)
		dropOnPoll(f, 1)
		res := c.Acquire(context.Background(), req(-1))
		if res.Outcome != rl.OutcomeHeld || res.Lease.ID != "2-fake" {
			t.Fatalf("result %+v lease %+v", res, res.Lease)
		}
		if rl.RegisteredForTest(c, "1-fake") || !rl.RegisteredForTest(c, "2-fake") {
			t.Fatal("registry must hold only the new lease")
		}
	})
}

func TestAcquireQueuedGoneWithoutRequeueFailsOpen(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f))
	f.QueueNext(0)
	dropOnPoll(f, -1)
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonUnavailable {
		t.Fatalf("result %+v", res)
	}
	if n := len(posts(f)); n != 1 {
		t.Fatalf("posts = %d", n)
	}
	if rl.RegisteredForTest(c, "1-fake") {
		t.Fatal("gone lease must be unregistered")
	}
}

func TestAcquireRetryOnlyWhenNotSent(t *testing.T) {
	t.Run("dial refused is retried", func(t *testing.T) {
		f := leasetest.New(t)
		d := &dialer{fails: 2}
		opts := photosOpts(f)
		opts.HTTPClient = d.client()
		opts.Retry = rl.DefaultRetry()
		opts.Retry.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
		c := newClient(t, opts)
		res := c.Acquire(context.Background(), req(-1))
		if res.Outcome != rl.OutcomeHeld {
			t.Fatalf("result %+v", res)
		}
		if d.attempts.Load() != 3 || len(posts(f)) != 1 {
			t.Fatalf("attempts %d posts %d", d.attempts.Load(), len(posts(f)))
		}
	})
	t.Run("500 after send is not resent", func(t *testing.T) {
		f := leasetest.New(t)
		opts := photosOpts(f)
		opts.Retry.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
		c := newClient(t, opts)
		f.Respond(func(r leasetest.Recorded) (int, any, bool) {
			return 500, map[string]any{"success": 500}, true
		})
		res := c.Acquire(context.Background(), req(-1))
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonUnavailable {
			t.Fatalf("result %+v", res)
		}
		if n := len(posts(f)); n != 1 {
			t.Fatalf("posts = %d", n)
		}
	})
}

func TestAcquirePostTimeoutIsNotResent(t *testing.T) {
	t.Run("past reply slack", func(t *testing.T) {
		f := leasetest.New(t)
		opts := photosOpts(f)
		opts.ReplySlack = 100 * time.Millisecond
		opts.Retry.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
		c := newClient(t, opts)
		f.Hang(2 * time.Second)
		start := time.Now()
		res := c.Acquire(context.Background(), req(200*time.Millisecond))
		if el := time.Since(start); el > time.Second {
			t.Fatalf("took %s", el)
		}
		if res.Outcome != rl.OutcomeFailOpen || len(posts(f)) != 1 {
			t.Fatalf("result %+v posts %d", res, len(posts(f)))
		}
	})
	t.Run("http client timeout", func(t *testing.T) {
		f := leasetest.New(t)
		opts := photosOpts(f)
		opts.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond}
		opts.Retry.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
		c := newClient(t, opts)
		f.Hang(time.Second)
		res := c.Acquire(context.Background(), req(-1))
		if res.Outcome != rl.OutcomeFailOpen || len(posts(f)) != 1 {
			t.Fatalf("result %+v posts %d", res, len(posts(f)))
		}
	})
}

func TestAcquireBackoffExhaustedCoolsDown(t *testing.T) {
	f := leasetest.New(t)
	d := &dialer{fails: -1}
	opts := photosOpts(f)
	opts.HTTPClient = d.client()
	opts.Retry = rl.RetryPolicy{Backoff: []time.Duration{time.Millisecond, time.Millisecond}, Cooldown: time.Minute}
	c := newClient(t, opts)
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonUnavailable {
		t.Fatalf("result %+v", res)
	}
	if n := d.attempts.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
	c.Acquire(context.Background(), req(-1))
	if n := d.attempts.Load(); n != 4 {
		t.Fatalf("attempts during cool-down = %d, want exactly 1 more", n-3)
	}
}

func TestAcquireDeadlineShorterThanBackoffGivesUp(t *testing.T) {
	for _, cool := range []bool{true, false} {
		t.Run("cooldownOnDeadline="+strconv.FormatBool(cool), func(t *testing.T) {
			f := leasetest.New(t)
			d := &dialer{fails: -1}
			opts := photosOpts(f)
			opts.HTTPClient = d.client()
			opts.Retry = rl.RetryPolicy{Backoff: []time.Duration{time.Millisecond, time.Hour}, Cooldown: time.Minute, CooldownOnDeadline: cool}
			c := newClient(t, opts)
			start := time.Now()
			res := c.Acquire(context.Background(), req(500*time.Millisecond))
			if el := time.Since(start); el > 400*time.Millisecond {
				t.Fatalf("took %s: must give up instead of sleeping", el)
			}
			if res.Outcome != rl.OutcomeFailOpen || d.attempts.Load() != 2 {
				t.Fatalf("result %+v attempts %d", res, d.attempts.Load())
			}
			c.Acquire(context.Background(), req(500*time.Millisecond))
			want := int32(4)
			if cool {
				want = 3
			}
			if n := d.attempts.Load(); n != want {
				t.Fatalf("attempts = %d, want %d", n, want)
			}
		})
	}
}

func TestAcquireInteractiveDoesNotRetry(t *testing.T) {
	f := leasetest.New(t)
	d := &dialer{fails: -1}
	opts := photosOpts(f)
	opts.HTTPClient = d.client()
	opts.Retry.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	c := newClient(t, opts)
	r := req(20 * time.Second)
	r.Tier = rl.TierInteractive
	res := c.Acquire(context.Background(), r)
	if res.Outcome != rl.OutcomeFailOpen || d.attempts.Load() != 1 {
		t.Fatalf("result %+v attempts %d", res, d.attempts.Load())
	}
}

func TestAcquireRefusedAndDisabledFailOpenWithoutRetry(t *testing.T) {
	cases := []struct {
		status int
		body   map[string]any
		want   rl.FailReason
	}{
		{409, map[string]any{"success": 409}, rl.ReasonRefused},
		{429, map[string]any{"success": 429}, rl.ReasonRefused},
		{404, map[string]any{"success": 404}, rl.ReasonRefused},
		{503, map[string]any{"success": rl.CodeResourcesDisabled}, rl.ReasonDisabled},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			f := leasetest.New(t)
			opts := photosOpts(f)
			opts.Retry.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
			c := newClient(t, opts)
			f.Respond(func(leasetest.Recorded) (int, any, bool) { return tc.status, tc.body, true })
			res := c.Acquire(context.Background(), req(-1))
			if res.Outcome != rl.OutcomeFailOpen || res.Reason != tc.want {
				t.Fatalf("result %+v", res)
			}
			if n := len(f.Requests()); n != 1 {
				t.Fatalf("requests = %d", n)
			}
		})
	}
}

func TestAcquireBudgetRejected(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.Respond(func(leasetest.Recorded) (int, any, bool) {
		return 400, map[string]any{"success": 400, "error_code": "invalid_budget"}, true
	})
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeRejected || res.Code != rl.CodeInvalidBudget || !errors.Is(res.Err, rl.ErrBudgetRejected) {
		t.Fatalf("result %+v", res)
	}
	l := res.Lease
	assertPlaceholder(t, l, "gpu.1", rl.TierUserBatch)
	if state, _ := l.Snapshot(); !l.Rejected || state != rl.StateRejected || l.FailOpen {
		t.Fatalf("lease %+v", l)
	}
	if res.Compat() != l {
		t.Fatal("Compat must return the rejected lease")
	}
}

func TestAcquireInvalidRequestLogsError(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	logs := observed(&opts)
	c := newClient(t, opts)
	f.Respond(func(leasetest.Recorded) (int, any, bool) {
		return 400, map[string]any{"success": 4000}, true
	})
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeRejected || res.Code != rl.CodeInvalidRequest {
		t.Fatalf("result %+v", res)
	}
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(errs) != 1 || errs[0].Message != "resource lease request rejected" {
		t.Fatalf("error logs %+v", errs)
	}
	compat := res.Compat()
	if state, _ := compat.Snapshot(); !compat.FailOpen || state != rl.StateHeld || compat.ID != "" || compat.Device != "gpu.1" {
		t.Fatalf("compat %+v", compat)
	}
}

func TestAcquireWithdrawnByCause(t *testing.T) {
	for name, mk := range map[string]func(*leasetest.FakeCore) rl.Options{"photos": photosOpts, "ai": aiOpts} {
		t.Run(name, func(t *testing.T) {
			f := leasetest.New(t)
			opts := mk(f)
			logs := observed(&opts)
			c := newClient(t, opts)
			f.QueueNext(0)
			ctx, cancel := context.WithCancelCause(context.Background())
			time.AfterFunc(100*time.Millisecond, func() { cancel(rl.ErrWithdrawn) })
			res := c.Acquire(ctx, req(-1))
			if res.Outcome != rl.OutcomeWithdrawn || !errors.Is(res.Err, rl.ErrWithdrawn) {
				t.Fatalf("result %+v", res)
			}
			assertPlaceholder(t, res.Lease, "gpu.1", rl.TierUserBatch)
			eventually(t, time.Second, func() bool { return len(deletes(f)) == 1 }, "queued lease must be DELETEd")
			if n := failOpenWarns(logs); n != 0 {
				t.Fatalf("fail-open warns = %d", n)
			}
			if len(logs.FilterLevelExact(zapcore.DebugLevel).All()) == 0 {
				t.Fatal("withdrawal must leave a debug line")
			}
		})
	}
}

func TestAcquireOnWaitErrorDeletesQueued(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.QueueNext(0)
	errX := errors.New("superseded")
	var calls atomic.Int32
	res := c.Acquire(context.Background(), req(-1),
		rl.WithPollWait(20*time.Millisecond),
		rl.WithOnWait(func(context.Context) error {
			if calls.Add(1) == 2 {
				return errX
			}
			return nil
		}))
	if res.Outcome != rl.OutcomeWithdrawn || !errors.Is(res.Err, errX) {
		t.Fatalf("result %+v", res)
	}
	assertPlaceholder(t, res.Lease, "gpu.1", rl.TierUserBatch)
	if len(gets(f)) != 1 || len(deletes(f)) != 1 || len(f.Live()) != 0 {
		t.Fatalf("gets %d deletes %d live %v", len(gets(f)), len(deletes(f)), f.Live())
	}
}

func TestAcquirePollErrorCancelsQueued(t *testing.T) {
	fail := func(f *leasetest.FakeCore) {
		f.Respond(func(r leasetest.Recorded) (int, any, bool) {
			if r.Method == http.MethodGet {
				return 500, map[string]any{"success": 500}, true
			}
			return 0, nil, false
		})
	}
	t.Run("photos sync", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		f.QueueNext(0)
		fail(f)
		// The 2 s backoff step does not fit into 1 s: no GET retry.
		res := c.Acquire(context.Background(), req(time.Second))
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonUnavailable {
			t.Fatalf("result %+v", res)
		}
		if len(deletes(f)) != 1 {
			t.Fatal("DELETE must be sent before Acquire returns")
		}
	})
	t.Run("ai async", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, aiOpts(f))
		f.QueueNext(0)
		fail(f)
		res := c.Acquire(context.Background(), req(-1))
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonUnavailable {
			t.Fatalf("result %+v", res)
		}
		eventually(t, time.Second, func() bool { return len(deletes(f)) == 1 }, "queued lease must be DELETEd")
	})
}

func TestAcquireNotUsableState(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.Respond(func(leasetest.Recorded) (int, any, bool) {
		return 200, env(map[string]any{"id": "w-1", "state": "weird"}), true
	})
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeRejected || res.Code != rl.CodeNotUsable {
		t.Fatalf("result %+v", res)
	}
	if res.Compat() != res.Lease || res.Lease.Usable() {
		t.Fatal("Compat must return the odd-state lease, which is not usable")
	}
	if len(deletes(f)) != 0 || rl.RegisteredForTest(c, "w-1") {
		t.Fatal("odd-state lease: no DELETE, not registered")
	}
}

func TestAcquireDisabled(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	opts.Disabled = true
	c := newClient(t, opts)
	if c.Enabled() || c.Name() != "photos" {
		t.Fatal("Enabled/Name")
	}
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonDisabled {
		t.Fatalf("result %+v", res)
	}
	assertPlaceholder(t, res.Lease, "gpu.1", rl.TierUserBatch)
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("requests = %d", n)
	}
}

func TestAcquireWarnThrottledPerClient(t *testing.T) {
	f := leasetest.New(t)
	f.Respond(func(leasetest.Recorded) (int, any, bool) {
		return 503, map[string]any{"success": 503}, true
	})
	opts := aiOpts(f)
	logs := observed(&opts)
	c1, c2 := newClient(t, opts), newClient(t, opts)
	for i := 0; i < 3; i++ {
		if res := c1.Acquire(context.Background(), req(-1)); res.Outcome != rl.OutcomeFailOpen {
			t.Fatalf("result %+v", res)
		}
	}
	warns := logs.FilterMessage("admission fail-open: acquire").All()
	if len(warns) != 1 || warns[0].Level != zapcore.WarnLevel {
		t.Fatalf("warns after 3 fail-opens = %d", len(warns))
	}
	c2.Acquire(context.Background(), req(-1))
	if n := len(logs.FilterMessage("admission fail-open: acquire").All()); n != 2 {
		t.Fatalf("second client must warn on its own: %d", n)
	}
}

func TestAcquireNilLoggerUsesGlobalAtCallTime(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f)) // Logger nil
	core, logs := observer.New(zapcore.DebugLevel)
	undo := zap.ReplaceGlobals(zap.New(core))
	defer undo()
	f.Respond(func(leasetest.Recorded) (int, any, bool) { return 503, map[string]any{"success": 503}, true })
	c.Acquire(context.Background(), req(-1))
	if n := failOpenWarns(logs); n != 1 {
		t.Fatalf("global logger warns = %d", n)
	}
}

// Every non-held placeholder carries DevicePref[0] (else Devices[0]) and the
// request tier, and never an id; Compat keeps both fields.
func TestAcquirePlaceholdersAndCompat(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, photosOpts(f))
	f.Respond(func(leasetest.Recorded) (int, any, bool) { return 409, map[string]any{"success": 409}, true })
	r := req(-1)
	r.Tier = rl.TierMaintenance
	failOpen := c.Acquire(context.Background(), r)
	if failOpen.Outcome != rl.OutcomeFailOpen {
		t.Fatalf("result %+v", failOpen)
	}
	assertPlaceholder(t, failOpen.Lease, "gpu.1", rl.TierMaintenance)
	assertPlaceholder(t, failOpen.Compat(), "gpu.1", rl.TierMaintenance)

	r.DevicePref = nil
	r.Devices = []string{"npu.0", "cpu"}
	assertPlaceholder(t, c.Acquire(context.Background(), r).Compat(), "npu.0", rl.TierMaintenance)

	f.Respond(nil)
	f.QueueNext(0)
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(50*time.Millisecond, func() { cancel(rl.ErrWithdrawn) })
	w := c.Acquire(ctx, req(-1))
	if w.Outcome != rl.OutcomeWithdrawn {
		t.Fatalf("result %+v", w)
	}
	assertPlaceholder(t, w.Lease, "gpu.1", rl.TierUserBatch)
	compat := w.Compat()
	assertPlaceholder(t, compat, "gpu.1", rl.TierUserBatch)
	if state, _ := compat.Snapshot(); !compat.FailOpen || state != rl.StateHeld {
		t.Fatalf("compat %+v", compat)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := rl.New(rl.Options{URLFile: "x"}); err == nil {
		t.Fatal("Client is required")
	}
	if _, err := rl.New(rl.Options{Client: "x"}); err == nil {
		t.Fatal("URLFile or BaseURL is required")
	}
	if _, err := rl.New(rl.Options{Client: "x", BaseURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
}

// As on the real core, a queued POST long-polls its wait_ms: the SDK must
// keep the POST open for wait_ms (+ ReplySlack) and not cut it early.
func TestAcquirePostLongPollIsNotCutEarly(t *testing.T) {
	for name, mk := range map[string]func(*leasetest.FakeCore) rl.Options{"photos": photosOpts, "ai": aiOpts} {
		t.Run(name, func(t *testing.T) {
			f := leasetest.New(t)
			f.HoldQueuedPost(true)
			opts := mk(f)
			c := newClient(t, opts)
			f.QueueNext(0)
			start := time.Now()
			res := c.Acquire(context.Background(), req(time.Second))
			el := time.Since(start)
			// The POST answered queued at ~1 s (had it been cut, the fake core
			// would have dropped the lease and the reason would be unavailable);
			// the deadline then ends the wait with a DELETE.
			if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonTimedOut {
				t.Fatalf("result %+v", res)
			}
			if el < 950*time.Millisecond || el > time.Second+opts.ReplySlack {
				t.Fatalf("took %s", el)
			}
			if len(posts(f)) != 1 || len(gets(f)) != 0 {
				t.Fatalf("posts %d gets %d", len(posts(f)), len(gets(f)))
			}
			eventually(t, time.Second, func() bool { return len(deletes(f)) == 1 && len(f.Live()) == 0 }, "queued lease must be DELETEd")
		})
	}
	t.Run("granted after the held POST", func(t *testing.T) {
		f := leasetest.New(t)
		f.HoldQueuedPost(true)
		c := newClient(t, aiOpts(f))
		f.QueueNext(1)
		res := c.Acquire(context.Background(), req(-1), rl.WithPollWait(200*time.Millisecond))
		if res.Outcome != rl.OutcomeHeld || len(gets(f)) != 1 {
			t.Fatalf("result %+v gets %d", res, len(gets(f)))
		}
	})
}

// failDials refuses the dials whose 1-based number is in fail.
func failDials(fail ...int32) (*http.Client, *atomic.Int32) {
	var n atomic.Int32
	set := map[int32]bool{}
	for _, f := range fail {
		set[f] = true
	}
	return &http.Client{
		Timeout: 35 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if set[n.Add(1)] {
					return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
				}
				var nd net.Dialer
				return nd.DialContext(ctx, network, addr)
			},
		},
	}, &n
}

// A retried request recomputes wait_ms from what is left of the deadline:
// a long-poll still sized for the whole MaxWait would run past the request
// deadline and turn a queued timeout into ReasonUnavailable.
func TestAcquireRetryRecomputesWaitMs(t *testing.T) {
	for name, slack := range map[string]time.Duration{"photos slack": 2 * time.Second, "short slack": 200 * time.Millisecond} {
		t.Run("POST "+name, func(t *testing.T) {
			f := leasetest.New(t)
			opts := photosOpts(f)
			opts.Retry.Backoff = []time.Duration{500 * time.Millisecond}
			opts.ReplySlack = slack
			opts.HTTPClient, _ = failDials(1) // the first POST is refused
			c := newClient(t, opts)
			f.HoldQueuedPost(true)
			f.QueueNext(0)
			res := c.Acquire(context.Background(), req(3*time.Second))
			if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonTimedOut {
				t.Fatalf("result %+v, want timed_out", res)
			}
			p := posts(f)
			if len(p) != 1 {
				t.Fatalf("posts on the core = %d", len(p))
			}
			if w := bodyWaitMs(t, p[0]); w > 2500 || w < 2000 {
				t.Fatalf("retried POST wait_ms = %d, want <= 2500", w)
			}
		})
	}
	t.Run("GET", func(t *testing.T) {
		f := leasetest.New(t)
		opts := photosOpts(f)
		opts.Retry.Backoff = []time.Duration{500 * time.Millisecond}
		opts.ReplySlack = 200 * time.Millisecond
		opts.HTTPClient, _ = failDials(2) // the POST goes through, the first poll is refused
		c := newClient(t, opts)
		f.QueueNext(0)
		res := c.Acquire(context.Background(), req(3*time.Second))
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonTimedOut {
			t.Fatalf("result %+v, want timed_out", res)
		}
		g := gets(f)
		if len(g) == 0 {
			t.Fatal("no poll reached the core")
		}
		if w := waitMs(t, g[0]); w > 2500 || w < 2000 {
			t.Fatalf("retried GET wait_ms = %d, want <= 2500", w)
		}
	})
}

// A bounded queued lease that vanishes is re-queued with wait_ms capped at
// what is left, and not re-queued at all past the deadline.
func TestAcquireBoundedQueuedGoneIsRequeued(t *testing.T) {
	t.Run("before the deadline", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		f.QueueNext(0)
		f.QueueNext(0)
		var seen atomic.Int32
		f.Respond(func(r leasetest.Recorded) (int, any, bool) {
			if r.Method == http.MethodGet && seen.Add(1) == 1 {
				time.Sleep(300 * time.Millisecond)
				f.Drop(strings.TrimPrefix(r.Path, leasesPath+"/"))
			}
			return 0, nil, false
		})
		res := c.Acquire(context.Background(), req(2*time.Second))
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonTimedOut {
			t.Fatalf("result %+v", res)
		}
		p := posts(f)
		if len(p) != 2 {
			t.Fatalf("posts = %d, want a re-queue", len(p))
		}
		if w0, w1 := bodyWaitMs(t, p[0]), bodyWaitMs(t, p[1]); w0 != 2000 || w1 > 1700 {
			t.Fatalf("wait_ms = %d then %d, want 2000 then <= 1700", w0, w1)
		}
		if got := deletedIDs(f); !reflect.DeepEqual(got, []string{"2-fake"}) {
			t.Fatalf("DELETEs %v, want the re-queued lease cancelled", got)
		}
	})
	t.Run("past the deadline", func(t *testing.T) {
		f := leasetest.New(t)
		c := newClient(t, photosOpts(f))
		f.QueueNext(0)
		f.Respond(func(r leasetest.Recorded) (int, any, bool) {
			if r.Method == http.MethodGet {
				time.Sleep(400 * time.Millisecond)
				f.Drop(strings.TrimPrefix(r.Path, leasesPath+"/"))
			}
			return 0, nil, false
		})
		res := c.Acquire(context.Background(), req(300*time.Millisecond))
		if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonTimedOut {
			t.Fatalf("result %+v", res)
		}
		if n := len(posts(f)); n != 1 {
			t.Fatalf("posts = %d: no re-queue past the deadline", n)
		}
	})
}

func TestAcquireRequeueWarnsOnlyWhenRequeueing(t *testing.T) {
	f := leasetest.New(t)
	opts := photosOpts(f)
	logs := observed(&opts)
	c := newClient(t, opts)
	for i := 0; i < 10; i++ {
		f.QueueNext(0)
	}
	dropOnPoll(f, -1)
	if res := c.Acquire(context.Background(), req(-1)); res.Reason != rl.ReasonRequeueLimit {
		t.Fatalf("result %+v", res)
	}
	if n := logs.FilterMessage("admission lease vanished while queued; re-queueing").Len(); n != opts.RequeueLimit {
		t.Fatalf("%d re-queueing warnings, want %d (none for the loss over the limit)", n, opts.RequeueLimit)
	}
}

func TestAcquirePollWithoutLeaseFailsOpen(t *testing.T) {
	f := leasetest.New(t)
	c := newClient(t, aiOpts(f))
	f.QueueNext(0)
	f.Respond(func(r leasetest.Recorded) (int, any, bool) {
		if r.Method == http.MethodGet {
			return http.StatusNoContent, nil, true
		}
		return 0, nil, false
	})
	res := c.Acquire(context.Background(), req(-1))
	if res.Outcome != rl.OutcomeFailOpen || res.Reason != rl.ReasonUnavailable {
		t.Fatalf("result %+v", res)
	}
	eventually(t, 2*time.Second, func() bool { return len(deletes(f)) == 1 }, "the queued lease was not cancelled")
}
