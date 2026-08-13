package agent

import (
	"testing"
	"time"

	"rick/internal/provider"
)

// TestCacheStrategyOverridesFlatKnobs pins the strategy seam: a configured
// CacheStrategy wins over the flat knobs for retention, warm, passback, TTL
// and the divergence reason.
func TestCacheStrategyOverridesFlatKnobs(t *testing.T) {
	strategy := provider.DefaultStrategy{
		NameVal:         "deepseek-aggressive",
		RetentionVal:    provider.CacheRetentionLong,
		TTLVal:          24 * time.Hour,
		WarmVal:         true,
		WarmTurnVal:     false,
		PassbackVal:     true,
		MaxReasoningVal: 4,
		DivergenceVal:   "strategy-declared",
	}
	r := New(Config{
		Model:             "deepseek/deepseek-v4-flash",
		CacheRetention:    provider.CacheRetentionAuto,
		WarmCache:         false,
		PassbackReasoning: false,
		CacheStrategy:     strategy,
	})
	got := r.cacheStrategy()
	if got.Retention() != provider.CacheRetentionLong {
		t.Fatalf("strategy retention = %q, want long", got.Retention())
	}
	if !got.WarmCache() {
		t.Fatal("strategy warm should override the flat off knob")
	}
	if !got.PassbackReasoning() {
		t.Fatal("strategy passback should override the flat off knob")
	}
	if got.MaxReasoningTurns() != 4 {
		t.Fatalf("strategy max reasoning = %d, want 4", got.MaxReasoningTurns())
	}
	if got.Name() != "deepseek-aggressive" {
		t.Fatalf("strategy name = %q", got.Name())
	}
}

// TestCacheStrategyTTLHonorsObservedEviction pins that a provably observed
// eviction gap still tightens below a strategy's (possibly optimistic) TTL.
func TestCacheStrategyTTLHonorsObservedEviction(t *testing.T) {
	strategy := provider.DefaultStrategy{
		TTLVal: 24 * time.Hour,
	}
	r := New(Config{CacheStrategy: strategy})
	r.observedEvictionGap = 4 * time.Minute
	ttl := r.cacheTTL()
	if ttl >= 4*time.Minute {
		t.Fatalf("observed eviction must tighten the TTL, got %v", ttl)
	}
	if ttl <= 0 {
		t.Fatal("TTL must stay positive")
	}
}
