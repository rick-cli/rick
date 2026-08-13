package plugin

import "testing"

// TestCacheStrategyHooksFirstHitWins pins the plugin cache-strategy seam:
// the first enabled plugin that returns a non-nil strategy for a route wins,
// and disabled plugins are skipped.
func TestCacheStrategyHooksFirstHitWins(t *testing.T) {
	registry := NewRegistry()
	registry.Register(Hooks{
		Name: "fallback",
		CacheStrategyHook: func(providerID, modelID string) any {
			return nil // no opinion
		},
	})
	registry.Register(Hooks{
		Name: "deepseek-tuner",
		CacheStrategyHook: func(providerID, modelID string) any {
			if providerID == "deepseek" {
				return "deepseek-strategy"
			}
			return nil
		},
	})
	registry.Register(Hooks{
		Name: "late-claim",
		CacheStrategyHook: func(providerID, modelID string) any {
			if providerID == "anthropic" {
				return "should-not-win"
			}
			return nil
		},
	})

	got := registry.CacheStrategyHooks("deepseek", "deepseek-v4-flash")
	if got != "deepseek-strategy" {
		t.Fatalf("first-hit strategy = %v, want deepseek-strategy", got)
	}

	// A route no plugin claims falls through to nil.
	if got := registry.CacheStrategyHooks("openai", "gpt-5.6"); got != nil {
		t.Fatalf("unclaimed route returned %v, want nil", got)
	}

	// Disabling the first claimant leaves no plugin claiming deepseek.
	registry.SetEnabled("deepseek-tuner", false)
	if got := registry.CacheStrategyHooks("deepseek", "deepseek-v4-flash"); got != nil {
		t.Fatalf("after disable, strategy = %v, want nil", got)
	}
	// The remaining claimant still wins on its own route.
	if got := registry.CacheStrategyHooks("anthropic", "claude"); got != "should-not-win" {
		t.Fatalf("late claimant = %v, want should-not-win", got)
	}
}
