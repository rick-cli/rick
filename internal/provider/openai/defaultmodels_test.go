package openai

import (
	"testing"

	"rick/internal/provider"
)

func TestDefaultModelsAdvertiseDeepSeekV4(t *testing.T) {
	tests := map[string]map[string]int{
		"deepseek": {
			"deepseek-v4-flash": 1_000_000,
			"deepseek-v4-pro":   1_000_000,
			"deepseek-chat":     128_000,
		},
		"opencode-zen": {
			"deepseek-v4-flash-free": 200_000,
			"deepseek-v4-flash":      1_000_000,
		},
		"opencode-go": {
			"deepseek-v4-flash-free": 200_000,
			"deepseek-v4-flash":      1_000_000,
		},
	}
	for providerID, want := range tests {
		got := defaultModels(providerID)
		for _, model := range got {
			if expect, ok := want[model.ID]; ok && model.ContextWindow != expect {
				t.Errorf("[%s] %s ContextWindow = %d, want %d", providerID, model.ID, model.ContextWindow, expect)
			}
		}
		for id := range want {
			if !containsModel(got, id) {
				t.Errorf("[%s] defaultModels is missing %q", providerID, id)
			}
		}
	}
}

func containsModel(models []provider.ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}
