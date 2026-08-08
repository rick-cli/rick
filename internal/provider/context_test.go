package provider

import "testing"

func TestOpenCodeZenFreeModelContextWindows(t *testing.T) {
	tests := map[string]int{
		"big-pickle":             200_000,
		"deepseek-v4-flash-free": 200_000,
		"mimo-v2.5-free":         200_000,
		"laguna-s-2.1-free":      256_000,
		"ling-3.0-flash-free":    262_144,
		"north-mini-code-free":   256_000,
		"nemotron-3-ultra-free":  1_000_000,
	}

	for modelID, want := range tests {
		got, ok := ProviderContextWindow("opencode-zen", modelID)
		if !ok || got != want {
			t.Errorf("ProviderContextWindow(%q) = (%d, %t), want (%d, true)", modelID, got, ok, want)
		}
	}
}

func TestOpenCodeZenContextOverrideIsProviderScoped(t *testing.T) {
	if got, ok := ProviderContextWindow("openrouter", "nemotron-3-ultra-free"); ok || got != 0 {
		t.Fatalf("non-Zen override = (%d, %t), want (0, false)", got, ok)
	}
}

func TestDeepSeekV4ContextWindows(t *testing.T) {
	tests := map[string]int{
		"deepseek-v4-flash":      1_000_000,
		"deepseek-v4-flash-free": 200_000,
		"deepseek-v4-pro":        1_000_000,
		"deepseek-chat":          128_000,
		"deepseek-reasoner":      128_000,
	}
	for modelID, want := range tests {
		if got := KnownContextWindow(modelID); got != want {
			t.Errorf("KnownContextWindow(%q) = %d, want %d", modelID, got, want)
		}
	}
}
