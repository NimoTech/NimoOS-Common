package resourcelease

import (
	"sync"
	"testing"
	"time"
)

func TestLeaseSnapshotSetStateClone(t *testing.T) {
	l := &Lease{ID: "a", Device: "gpu.1", Tier: TierUserBatch, State: StateHeld, owner: &Client{}, workload: "w", pid: 7}
	l.SetState(StateQueued, nil)
	if s, rr := l.Snapshot(); s != StateQueued || rr {
		t.Fatalf("snapshot = %q %v", s, rr)
	}
	yes := true
	l.SetState(StateHeld, &yes)
	if s, rr := l.Snapshot(); s != StateHeld || !rr {
		t.Fatalf("snapshot = %q %v", s, rr)
	}
	c := l.Clone()
	if c == l || c.ID != "a" || c.Device != "gpu.1" || c.Tier != TierUserBatch || c.owner != l.owner || c.workload != "w" || c.pid != 7 {
		t.Fatalf("clone lost identity: %+v", c)
	}
	if s, rr := c.Snapshot(); s != StateHeld || !rr {
		t.Fatalf("clone state = %q %v", s, rr)
	}
	c.SetState(StateLost, nil)
	if s, _ := l.Snapshot(); s != StateHeld {
		t.Fatalf("clone shares state with the original: %q", s)
	}
	if (*Lease)(nil).Clone() != nil {
		t.Fatal("nil clone")
	}
}

func TestLeaseSnapshotConcurrent(t *testing.T) {
	l := &Lease{ID: "a", State: StateHeld}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := i%2 == 0
			l.SetState(StateHeld, &rr)
			l.Snapshot()
			_ = l.Usable()
			_ = l.Clone()
		}(i)
	}
	wg.Wait()
}

func TestLeaseUsable(t *testing.T) {
	cases := map[string]struct {
		l    *Lease
		want bool
	}{
		"nil":       {nil, false},
		"held":      {&Lease{ID: "a", State: StateHeld}, true},
		"queued":    {&Lease{ID: "a", State: StateQueued}, false},
		"no id":     {&Lease{State: StateHeld}, false},
		"fail-open": {&Lease{ID: "a", State: StateHeld, FailOpen: true}, false},
		"rejected":  {&Lease{ID: "a", State: StateHeld, Rejected: true}, false},
	}
	for name, tc := range cases {
		if got := tc.l.Usable(); got != tc.want {
			t.Fatalf("%s: Usable = %v", name, got)
		}
	}
}

func TestResultCompat(t *testing.T) {
	held := &Lease{ID: "a", Device: "gpu.1", State: StateHeld}
	if got := (Result{Outcome: OutcomeHeld, Lease: held}).Compat(); got != held {
		t.Fatal("held lease not returned as is")
	}
	budget := &Lease{Device: "gpu.1", State: StateRejected, Rejected: true, Tier: TierAutoBatch}
	if got := (Result{Outcome: OutcomeRejected, Code: CodeInvalidBudget, Lease: budget}).Compat(); got != budget {
		t.Fatal("invalid_budget lease not returned as is")
	}
	odd := &Lease{ID: "b", Device: "gpu.1", State: "weird"}
	if got := (Result{Outcome: OutcomeRejected, Code: CodeNotUsable, Lease: odd}).Compat(); got != odd {
		t.Fatal("not_usable lease not returned as is")
	}
	fo := &Lease{Device: "cpu", State: StateHeld, FailOpen: true, Tier: TierInteractive}
	if got := (Result{Outcome: OutcomeFailOpen, Reason: ReasonUnavailable, Lease: fo}).Compat(); got != fo {
		t.Fatal("fail-open placeholder not returned as is")
	}
	for name, res := range map[string]Result{
		"invalid_request": {Outcome: OutcomeRejected, Code: CodeInvalidRequest, Lease: &Lease{Device: "gpu.1", State: StateRejected, Rejected: true, Tier: TierUserBatch}},
		"withdrawn":       {Outcome: OutcomeWithdrawn, Lease: &Lease{ID: "q", Device: "gpu.1", State: StateQueued, Tier: TierUserBatch}},
	} {
		got := res.Compat()
		if got == res.Lease || !got.FailOpen || got.Rejected || got.ID != "" || got.Device != "gpu.1" || got.Tier != TierUserBatch {
			t.Fatalf("%s: compat = %+v", name, got)
		}
		if s, _ := got.Snapshot(); s != StateHeld {
			t.Fatalf("%s: compat state = %q", name, s)
		}
	}
	if got := (Result{}).Compat(); got == nil || !got.FailOpen {
		t.Fatalf("zero result compat = %+v", got)
	}
}

func TestDefaultRetryIsAFreshCopy(t *testing.T) {
	p := DefaultRetry()
	want := []time.Duration{2, 4, 8, 16, 30, 30, 30}
	if len(p.Backoff) != len(want) {
		t.Fatalf("backoff = %v", p.Backoff)
	}
	for i, s := range want {
		if p.Backoff[i] != s*time.Second {
			t.Fatalf("backoff[%d] = %v", i, p.Backoff[i])
		}
	}
	if p.Cooldown != 60*time.Second || !p.NoRetryInteractive || !p.CooldownOnDeadline {
		t.Fatalf("policy = %+v", p)
	}
	p.Backoff[0] = time.Hour
	if DefaultRetry().Backoff[0] != 2*time.Second {
		t.Fatal("DefaultRetry shares its backoff slice")
	}
}
