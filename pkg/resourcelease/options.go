package resourcelease

import (
	"net/http"
	"time"

	"go.uber.org/zap"
)

// RetryPolicy controls transport retries of lease requests.
type RetryPolicy struct {
	Backoff            []time.Duration // empty = never retry
	Cooldown           time.Duration   // 0 = no cool-down after an exhausted backoff
	NoRetryInteractive bool            // Tier interactive: one attempt per request, no backoff
	CooldownOnDeadline bool            // also cool down when the next step would overrun the deadline
}

// DefaultRetry returns a fresh copy of 2,4,8,16,30,30,30 s + 60 s cool-down,
// NoRetryInteractive and CooldownOnDeadline set (Photos' current policy).
func DefaultRetry() RetryPolicy {
	return RetryPolicy{
		Backoff: []time.Duration{
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
			16 * time.Second,
			30 * time.Second,
			30 * time.Second,
			30 * time.Second,
		},
		Cooldown:           60 * time.Second,
		NoRetryInteractive: true,
		CooldownOnDeadline: true,
	}
}

// WaitReleasePolicy controls WaitRelease (the resident release long-poll).
type WaitReleasePolicy struct {
	Min           time.Duration // wait clamp floor; 0 = 1 ms
	Slack         time.Duration // extra request time on top of wait; 0 = none
	GoneMarksLost bool          // 404 marks the lease lost (else state untouched)
}

// Options configures a Client.
type Options struct {
	Client                 string        // required: the wire "client"
	URLFile                string        // path of nimoos.url, re-read on every request; required unless BaseURL
	BaseURL                string        // fixed base URL (tests); wins over URLFile
	HTTPClient             *http.Client  // nil = &http.Client{Timeout: 35 * time.Second}; used as is (never copied)
	Disabled               bool          // every Acquire fails open with ReasonDisabled, no request is sent
	DefaultClass           string        // "" = "light"
	PollWait               time.Duration // 0 = 30 s; capped at MaxLongPoll
	Retry                  RetryPolicy   // zero = never retry, no cool-down
	ReplySlack             time.Duration // bounded Acquire: requests and sleeps end at deadline+ReplySlack
	NoWaitTimeout          time.Duration // MaxWait == 0: cap on the single POST; 0 = none
	BookkeepingTimeout     time.Duration // cap on each heartbeat / release request; 0 = caller ctx only
	QueuedCancelTimeout    time.Duration // DELETE of an abandoned queued lease (fresh ctx); 0 = 5 s
	AsyncCancelOnPollError bool          // a failed queued poll cancels the lease in a goroutine
	RequeueLimit           int           // queued lease gone (404+90002): re-POST up to this many times; 0 = fail open
	WaitRelease            WaitReleasePolicy
	WarnEvery              time.Duration // fail-open warning throttle; 0 = 5 min
	CloseTimeout           time.Duration // Close total budget; 0 = 3 s
	HandoffFile            string        // "" = KeepOnClose leases are only left alone, nothing is written; one file per Client (entries carry only the Client name)
	Logger                 *zap.Logger   // nil = zap.L() at each log call
}
