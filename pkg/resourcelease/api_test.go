package resourcelease

import (
	"context"
	"net/http"
	"time"
)

// Compile-time assertions: these pin the frozen public API that the Photos
// and AI migrations code against. Changing any signature breaks this file.
var (
	_ func(Options) (*Client, error)                                                                = New
	_ func(*Client) string                                                                          = (*Client).Name
	_ func(*Client) bool                                                                            = (*Client).Enabled
	_ func(*Client, context.Context, Request, ...AcquireOption) Result                              = (*Client).Acquire
	_ func(func(ctx context.Context) error) AcquireOption                                           = WithOnWait
	_ func(time.Duration) AcquireOption                                                             = WithPollWait
	_ func(*Client, context.Context, *Lease)                                                        = (*Client).Heartbeat
	_ func(*Client, context.Context, *Lease, HeartbeatInfo)                                         = (*Client).HeartbeatWith
	_ func(*Client, context.Context, *Lease, time.Duration) bool                                    = (*Client).WaitRelease
	_ func(*Client, context.Context, *Lease)                                                        = (*Client).Release
	_ func(*Client, context.Context, *Lease, ReleaseInfo)                                           = (*Client).ReleaseWith
	_ func(*Client, *Lease, ReleaseInfo)                                                            = (*Client).ReleaseAsync
	_ func(*Client, context.Context, string, any) error                                             = (*Client).GetJSON
	_ func(*Client, *Lease)                                                                         = (*Client).KeepOnClose
	_ func(*Client, context.Context) CloseReport                                                    = (*Client).Close
	_ func(*Client, context.Context) HandoffReport                                                  = (*Client).ReleaseHandoff
	_ func(*Client, *Lease, time.Duration, func() (HeartbeatInfo, bool), func(*Lease)) func()       = (*Client).Hold
	_ func(*Client, context.Context, *Lease, func(*Lease))                                          = (*Client).WatchRelease
	_ func(*Client, context.Context, *Lease, Request) (*Lease, bool)                                = (*Client).ReplaceNewBeforeOld
	_ func(*Singleflight, context.Context, string, func(ctx context.Context) Result) (Result, bool) = (*Singleflight).Do
	_ func(*Singleflight, string) bool                                                              = (*Singleflight).Withdraw

	_ func(*Lease) (string, bool) = (*Lease).Snapshot
	_ func(*Lease, string, *bool) = (*Lease).SetState
	_ func(*Lease) *Lease         = (*Lease).Clone
	_ func(*Lease) bool           = (*Lease).Usable
	_ func(Result) *Lease         = Result.Compat
	_ func() RetryPolicy          = DefaultRetry

	_ func(string) (Tier, bool)                   = ParseTier
	_ func(Tier) int                              = TierRank
	_ func(Tier, Tier) Tier                       = MoreUrgent
	_ func(context.Context, Tier) context.Context = WithTier
	_ func(context.Context) Tier                  = TierFrom
	_ func(*http.Request, Tier) Tier              = TierFromRequest
	_ func(http.Header, Tier)                     = SetTierHeader
)
