package config

import "testing"

func TestMergeVisionConfigPreservesGlobalEnabled(t *testing.T) {
	on := true
	base := &VisionConfig{Enabled: &on, APIKey: "global-key"}

	// A project block that only sets a model must keep the global enabled
	// state and key.
	over := &VisionConfig{Model: "gemini-3.5-flash-lite"}
	got := mergeVisionConfig(base, over)
	if got.Enabled == nil || !*got.Enabled {
		t.Fatal("project block cleared inherited enabled state")
	}
	if got.APIKey != "global-key" {
		t.Fatalf("project block cleared inherited API key: %q", got.APIKey)
	}
	if got.Model != "gemini-3.5-flash-lite" {
		t.Fatalf("model not merged: %q", got.Model)
	}
}

func TestMergeVisionConfigExplicitOff(t *testing.T) {
	on := true
	off := false
	base := &VisionConfig{Enabled: &on}
	over := &VisionConfig{Enabled: &off}
	got := mergeVisionConfig(base, over)
	if got.Enabled == nil || *got.Enabled {
		t.Fatal("explicit off must win")
	}
}
