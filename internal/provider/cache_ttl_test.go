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
		{"openai", CacheRetentionAuto, 5 * time.Minute},
		{"openai", CacheRetentionLong, 24 * time.Hour},
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
