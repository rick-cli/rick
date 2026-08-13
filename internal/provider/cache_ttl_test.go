package provider

import (
	"testing"
	"time"
)

// TestDefaultCacheTTLVendorTable pins the per-vendor TTL table (D1): the
// pre-warm decision and cache-miss labels both derive from it, so a wrong
// value either wastes warm requests (too short) or surprises with cold
// re-bills (too long).
func TestDefaultCacheTTLVendorTable(t *testing.T) {
	cases := []struct {
		name      string
		retention CacheRetention
		want      time.Duration
	}{
		{"deepseek", CacheRetentionAuto, 24 * time.Hour},
		{"opencode-zen", CacheRetentionAuto, 24 * time.Hour},
		{"opencode-go", CacheRetentionAuto, 24 * time.Hour},
		{"deepseek", CacheRetentionLong, 24 * time.Hour},
		{"anthropic", CacheRetentionAuto, 5 * time.Minute},
		{"anthropic", CacheRetentionLong, time.Hour},
		// OpenAI's in-memory policy evicts after 5-10 min of inactivity; the
		// "24h" requested retention is a maximum, not a guarantee, so the
		// conservative 5 min keeps the pre-warm honest. GPT-5.6+ is refined
		// by CacheTTLForModel to the 30-minute minimum.
		{"openai", CacheRetentionAuto, 5 * time.Minute},
		{"openai", CacheRetentionLong, 5 * time.Minute},
		{"openrouter", CacheRetentionAuto, 5 * time.Minute},
		{"unknown-vendor", CacheRetentionAuto, 5 * time.Minute},
		{"", CacheRetentionAuto, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := DefaultCacheTTL(tc.name, tc.retention); got != tc.want {
			t.Errorf("DefaultCacheTTL(%q, %q) = %s, want %s", tc.name, tc.retention, got, tc.want)
		}
	}
}

// TestCacheTTLForModel pins the model-aware refinement: GPT-5.6+ gets the
// guaranteed 30-minute minimum; pre-5.6 OpenAI models keep the in-memory
// 5-minute default.
func TestCacheTTLForModel(t *testing.T) {
	cases := []struct {
		name, model string
		retention   CacheRetention
		want        time.Duration
	}{
		{"openai", "gpt-5.6", CacheRetentionLong, 30 * time.Minute},
		{"openai", "openai/gpt-5.6", CacheRetentionAuto, 30 * time.Minute},
		{"openai", "gpt-5.7-mini", CacheRetentionLong, 30 * time.Minute},
		{"openai", "gpt-5.5", CacheRetentionLong, 5 * time.Minute},
		{"openai", "gpt-5", CacheRetentionLong, 5 * time.Minute},
		{"openai", "gpt-4o", CacheRetentionLong, 5 * time.Minute},
		// A gpt-5.6+ model on a non-OpenAI gateway has no 30-minute
		// guarantee — the gateway does plain automatic prefix caching (no
		// prompt_cache_options is sent), so the conservative 5-minute
		// automatic-cache default applies instead of 24h.
		{"openrouter", "gpt-5.6", CacheRetentionLong, 5 * time.Minute},
		{"commandcode", "gpt-5.6-luna", CacheRetentionLong, 5 * time.Minute},
		{"kilo", "openai/gpt-5.6-sol", CacheRetentionLong, 5 * time.Minute},
		// Custom gateways serving DeepSeek models keep the DeepSeek day-long
		// prefix cache; the provider name alone would fall to 5 minutes.
		{"commandcode", "deepseek/deepseek-v4-flash", CacheRetentionLong, 24 * time.Hour},
		{"commandcode", "deepseek-v4-pro", CacheRetentionAuto, 24 * time.Hour},
		{"deepseek", "deepseek-v4-flash", CacheRetentionLong, 24 * time.Hour},
	}
	for _, tc := range cases {
		if got := CacheTTLForModel(tc.name, tc.model, tc.retention); got != tc.want {
			t.Errorf("CacheTTLForModel(%q, %q, %q) = %s, want %s", tc.name, tc.model, tc.retention, got, tc.want)
		}
	}
}

// TestIsDeepSeekLine pins the model-name-driven rule the user asked for:
// any model id containing "deepseek" (regardless of the provider id) is
// DeepSeek-line, so custom gateways like commandcode get the DeepSeek wire
// dialect and day-long cache TTL.
func TestIsDeepSeekLine(t *testing.T) {
	cases := []struct {
		providerID, modelID string
		want                bool
	}{
		{"deepseek", "deepseek-v4-flash", true},
		{"deepseek", "deepseek-chat", true},
		{"commandcode", "deepseek/deepseek-v4-flash", true},
		{"commandcode", "deepseek/deepseek-v4-pro", true},
		{"fireworks", "accounts/fireworks/models/deepseek-v4-flash-0731", true},
		{"openrouter", "deepseek/deepseek-chat", true},
		{"opencode-zen", "anything", true}, // provider id alone is enough
		{"commandcode", "gpt-5.6-luna", false},
		{"commandcode", "claude-sonnet-5", false},
		{"", "deepseek-v4-pro", true}, // bare model id
	}
	for _, tc := range cases {
		if got := IsDeepSeekLine(tc.providerID, tc.modelID); got != tc.want {
			t.Errorf("IsDeepSeekLine(%q, %q) = %v, want %v", tc.providerID, tc.modelID, got, tc.want)
		}
	}
	if !IsDeepSeekModel("deepseek-v4-flash") {
		t.Error("IsDeepSeekModel(deepseek-v4-flash) = false, want true")
	}
	if IsDeepSeekModel("gpt-5.6-luna") {
		t.Error("IsDeepSeekModel(gpt-5.6-luna) = true, want false")
	}
}
