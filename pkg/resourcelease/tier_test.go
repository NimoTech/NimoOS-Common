package resourcelease

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTier(t *testing.T) {
	for in, want := range map[string]Tier{"interactive": TierInteractive, " USER_BATCH ": TierUserBatch, "auto_batch": TierAutoBatch, "maintenance": TierMaintenance} {
		if got, ok := ParseTier(in); !ok || got != want {
			t.Fatalf("%q -> %q %v", in, got, ok)
		}
	}
	if _, ok := ParseTier("urgent"); ok {
		t.Fatal("unknown tier accepted")
	}
	if _, ok := ParseTier(""); ok {
		t.Fatal("empty tier accepted")
	}
}

func TestTierRankAndMoreUrgent(t *testing.T) {
	for tier, want := range map[Tier]int{TierInteractive: 0, TierUserBatch: 1, TierAutoBatch: 2, TierMaintenance: 3, "": 4, "urgent": 4} {
		if got := TierRank(tier); got != want {
			t.Fatalf("TierRank(%q) = %d, want %d", tier, got, want)
		}
	}
	if got := MoreUrgent(TierAutoBatch, TierInteractive); got != TierInteractive {
		t.Fatalf("MoreUrgent(auto_batch, interactive) = %q", got)
	}
	if got := MoreUrgent(TierUserBatch, ""); got != TierUserBatch {
		t.Fatalf("MoreUrgent(user_batch, \"\") = %q", got)
	}
	if got := MoreUrgent("", TierUserBatch); got != TierUserBatch {
		t.Fatalf("MoreUrgent(\"\", user_batch) = %q", got)
	}
	// A tie keeps a.
	if got := MoreUrgent("bogus", ""); got != "bogus" {
		t.Fatalf("tie must keep a, got %q", got)
	}
}

func TestTierContextRoundTrip(t *testing.T) {
	if got := TierFrom(context.Background()); got != "" {
		t.Fatalf("unset tier = %q", got)
	}
	ctx := WithTier(context.Background(), TierUserBatch)
	if got := TierFrom(ctx); got != TierUserBatch {
		t.Fatalf("round trip = %q", got)
	}
	if got := TierFrom(WithTier(ctx, TierInteractive)); got != TierInteractive {
		t.Fatalf("override = %q", got)
	}
}

func TestTierFromRequestAndHeader(t *testing.T) {
	if got := TierFromRequest(nil, TierUserBatch); got != TierUserBatch {
		t.Fatalf("nil request = %q", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := TierFromRequest(r, TierInteractive); got != TierInteractive {
		t.Fatalf("missing header = %q", got)
	}
	r.Header.Set(TierHeader, "   ")
	if got := TierFromRequest(r, TierInteractive); got != TierInteractive {
		t.Fatalf("blank header = %q", got)
	}
	r.Header.Set(TierHeader, "maintenance")
	if got := TierFromRequest(r, TierInteractive); got != TierMaintenance {
		t.Fatalf("maintenance header = %q", got)
	}
	r.Header.Set(TierHeader, " Interactive ")
	if got := TierFromRequest(r, TierMaintenance); got != TierInteractive {
		t.Fatalf("mixed-case header = %q", got)
	}
	r.Header.Set(TierHeader, "bogus")
	if got := TierFromRequest(r, TierInteractive); got != TierAutoBatch {
		t.Fatalf("unknown header = %q", got)
	}

	h := http.Header{}
	SetTierHeader(h, "")
	if len(h) != 0 {
		t.Fatalf("empty tier wrote a header: %v", h)
	}
	SetTierHeader(h, TierInteractive)
	if got := h.Get("X-Nimo-Tier"); got != "interactive" {
		t.Fatalf("X-Nimo-Tier = %q", got)
	}
	if TierHeader != "X-Nimo-Tier" {
		t.Fatalf("TierHeader = %q", TierHeader)
	}
}
