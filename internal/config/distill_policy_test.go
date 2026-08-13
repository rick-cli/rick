package config

import "testing"

// TestDistillPolicyFor pins the per-model distillation policy resolution:
// the exact provider/model override wins, then the global knobs, then zero
// (package default) when nothing is configured.
func TestDistillPolicyFor(t *testing.T) {
	c, _ := Defaults()
	c.DistillModelPolicies = map[string]DistillModelPolicy{
		"deepseek/deepseek-v4-flash": {ThresholdPercent: 35, RetainRatio: 0.6},
	}

	// Exact match wins.
	threshold, retain, liveZone := c.DistillPolicyFor("deepseek", "deepseek-v4-flash")
	if threshold != 35 || retain != 0.6 || liveZone != 0 {
		t.Fatalf("exact policy = (%d, %v, %d), want (35, 0.6, 0)", threshold, retain, liveZone)
	}

	// No per-model entry: global knobs apply.
	c.DistillThresholdPercent = 40
	c.DistillRetainRatio = 0.5
	c.DistillLiveZoneTokens = 4000
	threshold, retain, liveZone = c.DistillPolicyFor("deepseek", "other-model")
	if threshold != 40 || retain != 0.5 || liveZone != 4000 {
		t.Fatalf("global fallback = (%d, %v, %d), want (40, 0.5, 4000)", threshold, retain, liveZone)
	}

	// Nothing configured at all: zero = package default.
	c.DistillThresholdPercent = 0
	c.DistillRetainRatio = 0
	c.DistillLiveZoneTokens = 0
	threshold, retain, liveZone = c.DistillPolicyFor("deepseek", "other-model")
	if threshold != 0 || retain != 0 || liveZone != 0 {
		t.Fatalf("default fallback = (%d, %v, %d), want (0, 0, 0)", threshold, retain, liveZone)
	}

	// Per-model entry with only one knob set inherits the global for the rest.
	c.DistillThresholdPercent = 45
	c.DistillLiveZoneTokens = 6000
	c.DistillModelPolicies["deepseek/deepseek-v4-flash"] = DistillModelPolicy{RetainRatio: 0.7}
	threshold, retain, liveZone = c.DistillPolicyFor("deepseek", "deepseek-v4-flash")
	if threshold != 45 || retain != 0.7 || liveZone != 6000 {
		t.Fatalf("partial per-model = (%d, %v, %d), want (45, 0.7, 6000)", threshold, retain, liveZone)
	}
}
