package resourcelease

import (
	"context"
	"net/http"
	"strings"
)

// Tier is the business urgency class of a lease request.
type Tier string

const (
	TierInteractive Tier = "interactive"
	TierUserBatch   Tier = "user_batch"
	TierAutoBatch   Tier = "auto_batch"
	TierMaintenance Tier = "maintenance"
)

// TierHeader carries the caller's tier between services; the core itself
// only reads the tier field of a lease request.
const TierHeader = "X-Nimo-Tier"

// rankUnknown is the rank of the empty tier and of any unknown tier.
const rankUnknown = 4

// ParseTier trims and lower-cases s and accepts exactly one of the four tiers.
func ParseTier(s string) (Tier, bool) {
	switch t := Tier(strings.ToLower(strings.TrimSpace(s))); t {
	case TierInteractive, TierUserBatch, TierAutoBatch, TierMaintenance:
		return t, true
	default:
		return "", false
	}
}

// TierRank orders tiers by urgency: interactive 0 … maintenance 3; anything
// else (including "") is 4.
func TierRank(t Tier) int {
	switch t {
	case TierInteractive:
		return 0
	case TierUserBatch:
		return 1
	case TierAutoBatch:
		return 2
	case TierMaintenance:
		return 3
	default:
		return rankUnknown
	}
}

// MoreUrgent returns the tier with the lower rank; a tie keeps a.
func MoreUrgent(a, b Tier) Tier {
	if TierRank(b) < TierRank(a) {
		return b
	}
	return a
}

type tierCtxKey struct{}

// WithTier returns a child context carrying t.
func WithTier(ctx context.Context, t Tier) context.Context {
	return context.WithValue(ctx, tierCtxKey{}, t)
}

// TierFrom returns the tier stored by WithTier, or "" when unset.
func TierFrom(ctx context.Context) Tier {
	if ctx == nil {
		return ""
	}
	t, _ := ctx.Value(tierCtxKey{}).(Tier)
	return t
}

// TierFromRequest reads TierHeader from r. A nil request or a blank header
// yields def; an unknown non-blank value yields TierAutoBatch.
func TierFromRequest(r *http.Request, def Tier) Tier {
	if r == nil {
		return def
	}
	raw := r.Header.Get(TierHeader)
	if strings.TrimSpace(raw) == "" {
		return def
	}
	if t, ok := ParseTier(raw); ok {
		return t
	}
	return TierAutoBatch
}

// SetTierHeader writes t into h as TierHeader; it is a no-op for "".
func SetTierHeader(h http.Header, t Tier) {
	if t == "" || h == nil {
		return
	}
	h.Set(TierHeader, string(t))
}
