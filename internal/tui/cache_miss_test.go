package tui

import (
	"testing"
	"time"

	"rick/internal/provider"
)

// TestObserveCacheUsageCountsOnlyTrueRebillsWithoutCacheFields verifies the
// heuristic that separates a cache-less "provider omitted the fields on a
// normal growing turn" (small fresh input, not a miss) from a cache-less
// "the whole previously sent span was re-billed" turn (fresh input alone
// covers the span — this is what the analysed sessions showed: read=0 with
// input ≈ the full prompt). Only the latter may count as a miss.
func TestObserveCacheUsageCountsOnlyTrueRebillsWithoutCacheFields(t *testing.T) {
	m := &Model{}

	// Turn 1: warm request, cache read reported. Baseline established.
	m.observeCacheUsage(&provider.Usage{InputTokens: 100, CacheReadTokens: 10000})
	if m.cacheMissCount != 0 {
		t.Fatalf("turn 1 miss count = %d, want 0", m.cacheMissCount)
	}

	// Turn 2: the provider re-billed the whole footprint with no cache
	// fields reported at all (read=0, input covers the previous span). This
	// is a genuine full re-bill and must be counted — it is exactly what
	// the analysed sessions showed (read=0, input ~= the entire prompt).
	m.observeCacheUsage(&provider.Usage{InputTokens: 10100})
	if m.cacheMissCount != 1 {
		t.Fatalf("full-rebroadcast turn miss count = %d, want 1", m.cacheMissCount)
	}

	// Turn 3: back to a cached turn. The count must not double-count.
	m.observeCacheUsage(&provider.Usage{InputTokens: 120, CacheReadTokens: 10200})
	if m.cacheMissCount != 1 {
		t.Fatalf("cached turn after a miss reported count = %d, want 1", m.cacheMissCount)
	}

	// Turn 4: a masked turn whose fresh input is only the small tail (the
	// provider happens to omit cache fields on an otherwise cached turn).
	// That is not a re-bill and must not be counted.
	m.observeCacheUsage(&provider.Usage{InputTokens: 500})
	if m.cacheMissCount != 1 {
		t.Fatalf("tail-only cache-less turn miss count = %d, want 1 (unchanged)", m.cacheMissCount)
	}
}

// TestObserveCacheUsageCountsRealMiss verifies a genuine drop in cache reads
// (prefix change or idle gap) is still detected and counted.
func TestObserveCacheUsageCountsRealMiss(t *testing.T) {
	m := &Model{}

	m.observeCacheUsage(&provider.Usage{InputTokens: 100, CacheReadTokens: 10000})
	m.observeCacheUsage(&provider.Usage{InputTokens: 9000, CacheReadTokens: 1200})

	if m.cacheMissCount != 1 {
		t.Fatalf("miss count = %d, want 1", m.cacheMissCount)
	}
	// missed = min(prev 10100, prompt 10200) - read 1200 = 8900 > floor 1024
	if m.cacheMissTokens != 8900 {
		t.Fatalf("miss tokens = %d, want 8900", m.cacheMissTokens)
	}
}

// TestObserveCacheUsageNoiseFloorUntouched verifies steady cache growth with
// only a small new tail stays under the 1024-token noise floor.
func TestObserveCacheUsageNoiseFloorUntouched(t *testing.T) {
	m := &Model{}

	m.observeCacheUsage(&provider.Usage{InputTokens: 100, CacheReadTokens: 10000})
	m.observeCacheUsage(&provider.Usage{InputTokens: 500, CacheReadTokens: 10200})

	if m.cacheMissCount != 0 {
		t.Fatalf("noise-floor turn miss count = %d, want 0", m.cacheMissCount)
	}
}

// TestObserveCacheUsageMissReasonDistinguishesIdleGap verifies the miss
// notice can tell an idle-gap cache expiry (gap outlived the TTL) apart from
// a genuine prefix change, so regressions are diagnosable at a glance.
func TestObserveCacheUsageMissReasonDistinguishesIdleGap(t *testing.T) {
	m := &Model{}

	// Gap longer than the default 5-minute TTL is an idle-gap expiry.
	m.cacheLastUsage = time.Now().Add(-10 * time.Minute)
	if got := m.cacheMissReason(true); got != "idle gap (cache expired)" {
		t.Fatalf("stale gap reason = %q, want idle gap (cache expired)", got)
	}

	// A fresh turn (or nil deps with zero cacheLastUsage) is a prefix change.
	m.cacheLastUsage = time.Now()
	if got := m.cacheMissReason(true); got != "prefix change" {
		t.Fatalf("fresh turn reason = %q, want prefix change", got)
	}

	// A cache-less turn with no divergence and no idle gap reports the
	// provider itself stopped serving the prefix cache.
	m.cacheLastUsage = time.Now()
	if got := m.cacheMissReason(false); got != "provider served no prefix cache" {
		t.Fatalf("no-cache-served reason = %q", got)
	}
}
