package resourcelease

import (
	"errors"
	"sync"
	"time"
)

const (
	StateHeld     = "held"
	StateQueued   = "queued"
	StateLost     = "lost"
	StateReleased = "released"
	StateRejected = "rejected"

	KindTask     = "task"
	KindResident = "resident"

	ClassHeavy  = "heavy"
	ClassLight  = "light"
	ClassMemory = "memory"

	CodeResourcesDisabled = 90001 // 503: scheduler disabled
	CodeLeaseNotFound     = 90002 // 404: lease gone (expired, deleted, core restarted)

	MaxLongPoll          = 30 * time.Second // core cap for wait_ms
	MaxHeartbeatInterval = 20 * time.Second // core startup window assumes <= 20 s
)

var (
	ErrUnavailable    = errors.New("resource scheduler not available")
	ErrBudgetRejected = errors.New("resource budget description rejected")
	ErrInvalidRequest = errors.New("resource lease request rejected")
	ErrWithdrawn      = errors.New("lease acquire withdrawn by its caller")
	ErrClosed         = errors.New("resource lease client closed")
	ErrLeaseGone      = errors.New("lease not found")
	// ErrNotFound is a 404 without the 90002 envelope. Lease operations treat
	// it like ErrLeaseGone; from GetJSON it means the core does not serve the
	// path (e.g. an older core).
	ErrNotFound = errors.New("resource not found")
)

// Request is one lease request. Every field maps to one wire field; the SDK
// never derives Priority from Tier.
type Request struct {
	Task           string
	Class          string // "" = Options.DefaultClass
	Priority       int
	Tier           Tier   // sent as is, "" included (the core reads auto_batch)
	Kind           string // "" = "task"
	Workload       string // omitted when ""
	DevicePref     []string
	Devices        []string // reserved for phase 3B; when DevicePref is empty it maps to device_pref=Devices, allow_fallback=true
	AllowFallback  bool
	PID            int // omitted when <= 0
	EstVRAMMB      uint64
	EstRAMMB       uint64
	SizeHint       uint64         // omitted when 0
	FloorRAMMB     uint64         // omitted when 0
	FloorVRAMMB    uint64         // omitted when 0
	TTLSec         int            // omitted when <= 0
	Budget         map[string]any // omitted when nil; passed through untouched (no budget = nil; an empty non-nil map is sent as {})
	AlreadyLoaded  bool           // omitted when false
	AlreadyRunning bool           // omitted when false
	MaxWait        time.Duration  // 0 never waits, <0 unbounded, >0 cap counted from before the POST
}

// HeartbeatInfo is the optional heartbeat body.
type HeartbeatInfo struct {
	InUse      int
	LastUsedAt time.Time // zero = omitted
	Tier       Tier      // "" = omitted
	Loaded     bool
}

// ReleaseInfo is the optional release (DELETE) body. With both peaks 0 no
// body is sent at all, Shared included.
type ReleaseInfo struct {
	ObservedPeakMB     uint64 // omitted when 0
	ObservedPeakVRAMMB uint64 // omitted when 0
	Shared             bool
}

// Lease keeps pointer identity for its whole life: re-registration always
// yields a new *Lease. ID, Device, FailOpen, Rejected and Tier never change
// after Acquire returns; State and ReleaseRequested are guarded by mu — read
// them with Snapshot once the lease is shared between goroutines.
type Lease struct {
	ID       string
	Device   string
	FailOpen bool
	Rejected bool
	Tier     Tier

	mu               sync.Mutex
	State            string
	ReleaseRequested bool

	owner    *Client // set by Acquire; nil for leases built by hand
	workload string
	pid      int
}

// Snapshot returns State and ReleaseRequested under the lease mutex.
func (l *Lease) Snapshot() (state string, releaseRequested bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.State, l.ReleaseRequested
}

// SetState sets State and, when releaseRequested is non-nil, ReleaseRequested
// under the lease mutex. A nil releaseRequested leaves ReleaseRequested as is.
func (l *Lease) SetState(state string, releaseRequested *bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.State = state
	if releaseRequested != nil {
		l.ReleaseRequested = *releaseRequested
	}
}

// Clone returns a new lease with its own mutex, the same identity fields
// (ID, Device, FailOpen, Rejected, Tier, owning client, workload, pid) and the
// current State/ReleaseRequested. The KeepOnClose mark is registry
// bookkeeping keyed by lease id, so it applies to the clone and the original
// alike. Clone of nil is nil.
func (l *Lease) Clone() *Lease {
	if l == nil {
		return nil
	}
	state, rr := l.Snapshot()
	return &Lease{
		ID:               l.ID,
		Device:           l.Device,
		FailOpen:         l.FailOpen,
		Rejected:         l.Rejected,
		Tier:             l.Tier,
		State:            state,
		ReleaseRequested: rr,
		owner:            l.owner,
		workload:         l.workload,
		pid:              l.pid,
	}
}

// Usable reports whether l is a real lease the core currently holds for us.
func (l *Lease) Usable() bool {
	if l == nil || l.FailOpen || l.Rejected || l.ID == "" {
		return false
	}
	state, _ := l.Snapshot()
	return state == StateHeld
}

// Outcome is the structured result kind of Acquire.
type Outcome int

const (
	OutcomeHeld Outcome = iota + 1
	OutcomeFailOpen
	OutcomeRejected
	OutcomeWithdrawn // ctx cancelled with cause ErrWithdrawn, or an OnWait callback returned an error
)

// FailReason explains an OutcomeFailOpen.
type FailReason string

const (
	ReasonDisabled     FailReason = "disabled"       // Options.Disabled or core 503+90001
	ReasonUnavailable  FailReason = "unavailable"    // url file missing, transport error, 5xx (after the retry policy)
	ReasonRefused      FailReason = "refused"        // other 4xx (409/429, 404 on POST)
	ReasonQueuedNoWait FailReason = "queued_no_wait" // MaxWait == 0 and the core queued it
	ReasonTimedOut     FailReason = "timed_out"      // deadline or caller ctx ended while queued
	ReasonRequeueLimit FailReason = "requeue_limit"  // queued lease vanished more than RequeueLimit times
	ReasonClosed       FailReason = "closed"         // Close was called
)

// RejectCode explains an OutcomeRejected.
type RejectCode string

const (
	CodeInvalidBudget  RejectCode = "invalid_budget"  // 400 error_code=invalid_budget
	CodeInvalidRequest RejectCode = "invalid_request" // any other 400 (logged at Error)
	CodeNotUsable      RejectCode = "not_usable"      // the core answered a state that is neither held nor queued
)

// Result is what Acquire returns.
type Result struct {
	Outcome Outcome
	Lease   *Lease // never nil: the held lease, or a fail-open / rejected placeholder
	Reason  FailReason
	Code    RejectCode
	Err     error
}

// Compat is the lease the pre-SDK clients returned: held and invalid_budget
// leases as is, not_usable as the odd-state lease, everything else
// (fail-open, withdrawn, invalid_request) as a fail-open lease
// {Device: DevicePref[0], State: held, FailOpen: true, Tier: r.Tier}.
//
// The placeholder lease Acquire puts into a non-held Result already carries
// Device = DevicePref[0] and the request tier, so the fail-open lease copies
// those two fields from it. A placeholder that already is such a fail-open
// lease is returned as is.
//
// Call once and keep the result; each call may build a new placeholder.
//
// Compat is a migration shim for the pre-SDK wrappers (Photos admission, AI
// resource_lease); new code branches on r.Outcome instead.
func (r Result) Compat() *Lease {
	switch {
	case r.Outcome == OutcomeHeld && r.Lease != nil:
		return r.Lease
	case r.Outcome == OutcomeRejected && r.Lease != nil &&
		(r.Code == CodeInvalidBudget || r.Code == CodeNotUsable):
		return r.Lease
	}
	l := r.Lease
	if l == nil {
		return &Lease{State: StateHeld, FailOpen: true}
	}
	if l.FailOpen && !l.Rejected {
		if state, _ := l.Snapshot(); state == StateHeld {
			return l
		}
	}
	return &Lease{Device: l.Device, State: StateHeld, FailOpen: true, Tier: l.Tier}
}
