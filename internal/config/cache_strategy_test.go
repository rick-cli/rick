package config

import "testing"

// TestCacheStrategyForRouteResolution pins the per-route strategy resolver:
// a provider/model entry wins over a provider-only entry, and both override
// the flat global knobs only for the fields they name.
func TestCacheStrategyForRouteResolution(t *testing.T) {
	yes := true
	c := Config{
		CacheRetention:  "long",
		CacheTTLSeconds: 300,
		CacheStrategies: map[string]CacheStrategyConfig{
			"deepseek": {
				Name:              "deepseek-day",
				Retention:         "long",
				TTLSeconds:        86400,
				KeepaliveSeconds:  3600,
				PassbackReasoning: &yes,
				MaxReasoningTurns: 4,
				DivergenceReason:  "deepseek-wave",
			},
			"deepseek/deepseek-v4-flash": {
				Name:              "flash-aggressive",
				Warm:              &yes,
				MaxReasoningTurns: 2,
			},
		},
	}

	// Provider/model entry wins.
	name, retention, ttl, keepalive, warm, _, passback, maxReasoning, divergence := c.CacheStrategyFor("deepseek", "deepseek-v4-flash")
	if name != "flash-aggressive" {
		t.Fatalf("route name = %q, want flash-aggressive", name)
	}
	if retention != "long" || ttl != 86400 || keepalive != 3600 {
		t.Fatalf("route should inherit provider-only fields: retention=%q ttl=%d keepalive=%d", retention, ttl, keepalive)
	}
	if !warm || !passback || maxReasoning != 2 {
		t.Fatalf("route override mismatch: warm=%v passback=%v maxReasoning=%d", warm, passback, maxReasoning)
	}
	if divergence != "deepseek-wave" {
		t.Fatalf("route divergence = %q, want deepseek-wave", divergence)
	}

	// Provider-only entry applies to a model without its own entry.
	_, retention2, ttl2, _, _, _, passback2, maxReasoning2, _ := c.CacheStrategyFor("deepseek", "deepseek-v4-pro")
	if retention2 != "long" || ttl2 != 86400 {
		t.Fatalf("provider-only entry not applied: retention=%q ttl=%d", retention2, ttl2)
	}
	if !passback2 || maxReasoning2 != 4 {
		t.Fatalf("provider-only passback/maxReasoning = %v/%d", passback2, maxReasoning2)
	}

	// Unconfigured route falls back to global knobs.
	_, retention3, ttl3, _, _, _, _, maxReasoning3, _ := c.CacheStrategyFor("anthropic", "claude")
	if retention3 != "long" || ttl3 != 300 {
		t.Fatalf("fallback mismatch: retention=%q ttl=%d", retention3, ttl3)
	}
	if maxReasoning3 != 0 {
		t.Fatalf("fallback maxReasoning = %d, want 0", maxReasoning3)
	}
}
