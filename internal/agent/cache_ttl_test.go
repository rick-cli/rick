package agent

import (
	"testing"
	"time"

	"rick/internal/provider"
)

// TestCacheTTLOverride pins that a positive CacheTTLSeconds wins over the
// per-vendor table — the knob that lets a free flash-tier gateway with a
// minutes-scale cache expiry tell the idle-gap pre-warm the truth, instead of
// assuming the DeepSeek default of a day.
func TestCacheTTLOverride(t *testing.T) {
	runner := New(Config{})
	// No provider name: the vendor table's unknown-provider default (5m).
	if got := runner.cacheTTL(); got != 5*time.Minute {
		t.Fatalf("cacheTTL() with no provider = %v, want the 5m unknown-vendor default", got)
	}
}

func TestCacheTTLOverrideVendorAndExplicit(t *testing.T) {
	t.Run("vendor table deepseek", func(t *testing.T) {
		runner := New(Config{CacheRetention: provider.CacheRetentionLong})
		if got := provider.DefaultCacheTTL("deepseek", provider.CacheRetentionLong); got <= 0 {
			t.Fatalf("vendor TTL for deepseek/long = %v, want positive", got)
		} else if runner.cacheTTL() != got {
			t.Fatalf("cacheTTL() = %v, want vendor %v", runner.cacheTTL(), got)
		}
	})
	t.Run("explicit override wins", func(t *testing.T) {
		runner := New(Config{CacheTTLSeconds: 300})
		if got := runner.cacheTTL(); got != 300*time.Second {
			t.Fatalf("cacheTTL() = %v, want 5m override", got)
		}
	})
	t.Run("override beats vendor", func(t *testing.T) {
		runner := New(Config{
			CacheRetention:  provider.CacheRetentionLong,
			CacheTTLSeconds: 120,
		})
		if got := runner.cacheTTL(); got != 120*time.Second {
			t.Fatalf("cacheTTL() = %v, want the 120s override, not the 1-day vendor value", got)
		}
	})
}
