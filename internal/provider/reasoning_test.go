package provider

import "testing"

func TestDetectReasoningRecognizesNewAndGatewayModels(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		modelID    string
		wantStyle  ReasoningStyle
		wantLevel  ReasoningEffort
	}{
		{name: "glm native", providerID: "zai", modelID: "glm-4.7", wantStyle: ReasoningStyleGLM, wantLevel: ReasoningMedium},
		{name: "glm through openrouter", providerID: "openrouter", modelID: "z-ai/glm-4.7", wantStyle: ReasoningStyleOpenAI, wantLevel: ReasoningMedium},
		{name: "gemini openai compatibility", providerID: "gemini", modelID: "gemini-2.5-pro", wantStyle: ReasoningStyleOpenAI, wantLevel: ReasoningMedium},
		{name: "deepseek hybrid", providerID: "deepseek", modelID: "deepseek-v4-pro", wantStyle: ReasoningStyleDeepSeek, wantLevel: ReasoningMedium},
		{name: "deepseek through openrouter", providerID: "openrouter", modelID: "deepseek/deepseek-v4-pro", wantStyle: ReasoningStyleOpenAI, wantLevel: ReasoningMedium},
		{name: "minimax anthropic compatibility", providerID: "anthropic", modelID: "MiniMax-M3", wantStyle: ReasoningStyleAnthropic, wantLevel: ReasoningMedium},
		{name: "known plain model", providerID: "openai", modelID: "gpt-4o", wantStyle: ReasoningStyleNone, wantLevel: ReasoningOff},
		{name: "unknown custom model", providerID: "gateway", modelID: "vendor/new-model", wantStyle: ReasoningStyleUnknown, wantLevel: ReasoningOff},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			style, level := DetectReasoningForProvider(test.providerID, test.modelID)
			if style != test.wantStyle || level != test.wantLevel {
				t.Fatalf("DetectReasoningForProvider(%q, %q) = (%q, %q), want (%q, %q)",
					test.providerID, test.modelID, style, level, test.wantStyle, test.wantLevel)
			}
		})
	}
}

func TestReasoningCapabilitiesUseModelSpecificEfforts(t *testing.T) {
	tests := []struct {
		name        string
		providerID  string
		modelID     string
		want        []ReasoningEffort
		wantDefault ReasoningEffort
	}{
		{name: "glm boolean thinking", providerID: "zai", modelID: "glm-4.7", want: []ReasoningEffort{ReasoningOff, ReasoningOn}, wantDefault: ReasoningOn},
		{name: "glm effort vocabulary", providerID: "zai", modelID: "glm-5.2", want: []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMax}, wantDefault: ReasoningMax},
		{name: "deepseek vocabulary", providerID: "deepseek", modelID: "deepseek-v4-pro", want: []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningHigh, ReasoningMax}, wantDefault: ReasoningHigh},
		{name: "deepseek through openrouter", providerID: "openrouter", modelID: "deepseek/deepseek-v4-pro", want: []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningHigh, ReasoningMax}, wantDefault: ReasoningHigh},
		{name: "glm through openrouter", providerID: "openrouter", modelID: "z-ai/glm-4.7", want: []ReasoningEffort{ReasoningOff, ReasoningOn}, wantDefault: ReasoningOn},
		{name: "qwen boolean thinking", providerID: "qwen", modelID: "qwen3-235b", want: []ReasoningEffort{ReasoningOff, ReasoningOn}, wantDefault: ReasoningOn},
		{name: "qwen through openrouter", providerID: "openrouter", modelID: "qwen/qwen3-235b", want: []ReasoningEffort{ReasoningOff, ReasoningOn}, wantDefault: ReasoningOn},
		{name: "o-series no off", providerID: "openai", modelID: "o3", want: []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh}, wantDefault: ReasoningMedium},
		{name: "gpt-5 minimal", providerID: "openai", modelID: "gpt-5", want: []ReasoningEffort{ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh}, wantDefault: ReasoningMedium},
		{name: "gpt-5.2 xhigh", providerID: "openai", modelID: "gpt-5.2", want: []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, wantDefault: ReasoningMedium},
		{name: "gpt-5.6-luna through 9router", providerID: "9router", modelID: "cx/gpt-5.6-luna", want: []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMax}, wantDefault: ReasoningMedium},
		{name: "gpt-5.4 through 9router", providerID: "9router", modelID: "cx/gpt-5.4", want: []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, wantDefault: ReasoningMedium},
		{name: "gpt-5.4-mini through 9router", providerID: "9router", modelID: "cx/gpt-5.4-mini", want: []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, wantDefault: ReasoningMedium},
		{name: "gpt-5.5 through 9router", providerID: "9router", modelID: "cx/gpt-5.5", want: []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, wantDefault: ReasoningMedium},
		{name: "gpt-oss through 9router", providerID: "9router", modelID: "ag/gpt-oss-120b-medium", want: []ReasoningEffort{ReasoningOff, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, wantDefault: ReasoningMedium},
		{name: "codex through 9router", providerID: "9router", modelID: "cx/codex", want: []ReasoningEffort{ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}, wantDefault: ReasoningMedium},
		{name: "unknown generic opt-in", providerID: "gateway", modelID: "vendor/new-model", want: []ReasoningEffort{ReasoningOff, ReasoningOn}, wantDefault: ReasoningOff},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caps := ReasoningCapabilitiesForProvider(test.providerID, test.modelID, nil)
			if !equalReasoningEfforts(caps.Efforts, test.want) {
				t.Fatalf("efforts = %v, want %v", caps.Efforts, test.want)
			}
			if caps.Default != test.wantDefault {
				t.Fatalf("default = %q, want %q", caps.Default, test.wantDefault)
			}
		})
	}
}

func TestReasoningCapabilitiesPreferAdvertisedVocabulary(t *testing.T) {
	advertised := &ModelInfo{
		ReasoningKnown:        true,
		ReasoningEffortsKnown: true,
		ReasoningEfforts:      []ReasoningEffort{ReasoningMax, ReasoningHigh, ReasoningLow},
		ReasoningDefault:      ReasoningMax,
		ReasoningMandatory:    true,
	}
	caps := ReasoningCapabilitiesForProvider("openrouter", "vendor/model", advertised)
	want := []ReasoningEffort{ReasoningLow, ReasoningHigh, ReasoningMax}
	if !equalReasoningEfforts(caps.Efforts, want) {
		t.Fatalf("advertised efforts = %v, want %v", caps.Efforts, want)
	}
	if caps.Default != ReasoningMax || !caps.Mandatory {
		t.Fatalf("advertised controls = default %q, mandatory %t", caps.Default, caps.Mandatory)
	}
}

func TestReasoningCapabilitiesKeepFallbackWhenAdvertisedMetadataOmitsEfforts(t *testing.T) {
	advertised := &ModelInfo{ReasoningKnown: true}
	caps := ReasoningCapabilitiesForProvider("openrouter", "openai/gpt-5.2", advertised)
	want := []ReasoningEffort{ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh}
	if !equalReasoningEfforts(caps.Efforts, want) {
		t.Fatalf("efforts with incomplete metadata = %v, want %v", caps.Efforts, want)
	}
}

func TestReasoningCapabilitiesRespectAdvertisedDefaultEnabled(t *testing.T) {
	advertised := &ModelInfo{
		ReasoningKnown:               true,
		ReasoningEffortsKnown:        true,
		ReasoningEfforts:             []ReasoningEffort{ReasoningLow, ReasoningHigh},
		ReasoningDefault:             ReasoningHigh,
		ReasoningDefaultEnabled:      false,
		ReasoningDefaultEnabledKnown: true,
	}
	caps := ReasoningCapabilitiesForProvider("openrouter", "vendor/model", advertised)
	if caps.Default != ReasoningOff {
		t.Fatalf("default = %q, want off", caps.Default)
	}
}

func equalReasoningEfforts(left, right []ReasoningEffort) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
