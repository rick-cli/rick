package config

import "testing"

func TestValidateCacheRetention(t *testing.T) {
	valid := []string{"", "long", "none"}
	for _, v := range valid {
		if err := ValidateCacheRetention(v); err != nil {
			t.Errorf("ValidateCacheRetention(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"short", "LONG", "24h", "true"} {
		if err := ValidateCacheRetention(v); err == nil {
			t.Errorf("ValidateCacheRetention(%q) = nil, want error", v)
		}
	}
}

// TestCacheDefaultsOnByDefault locks the "cache on by default" behaviour:
// prompt-cache retention is "long" and the session-start warm request is
// enabled, so users get the cache-hit improvements without setting anything.
func TestCacheDefaultsOnByDefault(t *testing.T) {
	c, _ := Defaults()
	if c.CacheRetention != "long" {
		t.Fatalf("CacheRetention default = %q, want \"long\"", c.CacheRetention)
	}
	if !c.WarmCache {
		t.Fatal("WarmCache default = false, want true (session-start cache warm)")
	}
	// cache_max_reasoning_turns stays 0 = keep all reasoning (byte-stable,
	// append-only prefix), which is the cache-optimal default.
	if c.CacheMaxReasoningTurns != 0 {
		t.Fatalf("CacheMaxReasoningTurns default = %d, want 0 (keep-all, stable prefix)", c.CacheMaxReasoningTurns)
	}
	if c.CacheMaxToolResultBytes != 16<<10 {
		t.Fatalf("CacheMaxToolResultBytes default = %d, want %d", c.CacheMaxToolResultBytes, 16<<10)
	}
}

// TestCacheDefaultsOptOut confirms a user can still turn the features back off.
func TestCacheDefaultsOptOut(t *testing.T) {
	c, tui := Defaults()
	if err := mergeInto(&c, &tui, []byte(`{"cache_warm": false, "cache_retention": "none"}`), "", "probe.json"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if c.WarmCache {
		t.Fatal("cache_warm:false did not take effect")
	}
	if c.CacheRetention != "none" {
		t.Fatalf("cache_retention:none did not take effect, got %q", c.CacheRetention)
	}
	if err := mergeInto(&c, &tui, []byte(`{"cache_max_tool_result_bytes": 4096}`), "", "probe.json"); err != nil {
		t.Fatalf("merge cache_max_tool_result_bytes: %v", err)
	}
	if c.CacheMaxToolResultBytes != 4096 {
		t.Fatalf("cache_max_tool_result_bytes:4096 did not take effect, got %d", c.CacheMaxToolResultBytes)
	}
}

// TestCacheTTLAndKeepaliveMerge pins the gateway-TTL override and idle
// keep-alive config keys: zero stays the default, positive values merge.
func TestCacheTTLAndKeepaliveMerge(t *testing.T) {
	c, tui := Defaults()
	if c.CacheTTLSeconds != 0 || c.CacheKeepaliveSeconds != 0 {
		t.Fatalf("defaults not zero: ttl=%d keepalive=%d", c.CacheTTLSeconds, c.CacheKeepaliveSeconds)
	}
	if err := mergeInto(&c, &tui, []byte(`{"cache_ttl_seconds": 300, "cache_keepalive_seconds": 120}`), "", "probe.json"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if c.CacheTTLSeconds != 300 {
		t.Fatalf("cache_ttl_seconds:300 did not take effect, got %d", c.CacheTTLSeconds)
	}
	if c.CacheKeepaliveSeconds != 120 {
		t.Fatalf("cache_keepalive_seconds:120 did not take effect, got %d", c.CacheKeepaliveSeconds)
	}
}

// TestPerProviderCacheConfig pins the Phase G per-provider cache policy: a
// provider block can override retention/TTL/keepalive/warm per backend, the
// overrides merge across config layers, and CacheForProvider resolves them
// over the global defaults.
func TestPerProviderCacheConfig(t *testing.T) {
	c, tui := Defaults()
	c.CacheRetention = "long"
	c.CacheTTLSeconds = 300
	c.CacheKeepaliveSeconds = 120
	c.WarmCache = true

	// Per-provider overrides for openai; deepseek inherits globals.
	err := mergeInto(&c, &tui, []byte(`{
		"provider": {
			"openai": {
				"type": "openai",
				"cache_retention": "none",
				"cache_ttl_seconds": 30,
				"cache_keepalive_seconds": 240,
				"cache_warm": false
			}
		}
	}`), "", "probe.json")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	openai, ok := c.Providers["openai"]
	if !ok {
		t.Fatal("openai provider missing after merge")
	}
	if openai.CacheRetention != "none" {
		t.Fatalf("openai cache_retention = %q, want none", openai.CacheRetention)
	}
	if openai.CacheTTLSeconds != 30 {
		t.Fatalf("openai cache_ttl_seconds = %d, want 30", openai.CacheTTLSeconds)
	}
	if openai.CacheKeepaliveSeconds != 240 {
		t.Fatalf("openai cache_keepalive_seconds = %d, want 240", openai.CacheKeepaliveSeconds)
	}
	if openai.WarmCache == nil || *openai.WarmCache {
		t.Fatalf("openai cache_warm = %v, want false", openai.WarmCache)
	}

	// CacheForProvider resolves the override for openai.
	retention, ttl, keepalive, warm := c.CacheForProvider("openai")
	if retention != "none" || ttl != 30 || keepalive != 240 || warm {
		t.Fatalf("openai resolved = (%q,%d,%d,%v), want (none,30,240,false)", retention, ttl, keepalive, warm)
	}
	// deepseek has no override -> global values.
	retention, ttl, keepalive, warm = c.CacheForProvider("deepseek")
	if retention != "long" || ttl != 300 || keepalive != 120 || !warm {
		t.Fatalf("deepseek resolved = (%q,%d,%d,%v), want (long,300,120,true)", retention, ttl, keepalive, warm)
	}
	// Unknown provider -> global values.
	retention, ttl, keepalive, warm = c.CacheForProvider("nope")
	if retention != "long" || ttl != 300 || keepalive != 120 || !warm {
		t.Fatalf("unknown resolved = (%q,%d,%d,%v), want globals", retention, ttl, keepalive, warm)
	}
}
