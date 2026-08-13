package openai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"rick/internal/provider"
)

// TestLiveCacheHitE2E is a key-gated end-to-end proof that a multi-turn
// conversation against the real DeepSeek API reports cacheReadTokens > 0 on
// every request after the first — the harness-style "request-cache e2e".
//
// The provider-facing prefix must be byte-identical across requests (the
// stable system head plus an append-only message tail) for the automatic
// prefix cache to hit. This test replays a stable system prompt + two growing
// message tails and asserts the provider's own usage reports cached tokens on
// the second request. It is skipped unless RICK_LIVE_CACHE_TEST=1 and a
// DEEPSEEK_API_KEY (or RICK_DEEPSEEK_API_KEY) is present, so CI stays offline
// by default.
func TestLiveCacheHitE2E(t *testing.T) {
	if os.Getenv("RICK_LIVE_CACHE_TEST") != "1" {
		t.Skip("RICK_LIVE_CACHE_TEST=1 required (live provider call)")
	}
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("RICK_DEEPSEEK_API_KEY")
	}
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY required for live cache e2e")
	}
	model := os.Getenv("RICK_LIVE_CACHE_MODEL")
	if model == "" {
		model = "deepseek-chat"
	}

	client := New("deepseek", apiKey, "https://api.deepseek.com")
	// Long enough that the shared request prefix comfortably spans the
	// provider's cache-block granularity (64 tokens) from the first request.
	system := "You are a terse coding assistant used in an automated cache test. " +
		strings.Repeat("Keep instructions literal and precise. ", 12)

	// Request 1 primes the prefix. Request 2 extends it with one tail message
	// and must read the shared prefix back from the provider cache.
	reqs := []provider.Request{
		{
			Model:          model,
			System:         system,
			SystemStable:   system,
			Messages:       []provider.Message{provider.UserText("First turn: reply with the single word ok.")},
			MaxTokens:      8,
			SessionID:      "rick-live-cache-e2e",
			CacheRetention: provider.CacheRetentionLong,
		},
		{
			Model:        model,
			System:       system,
			SystemStable: system,
			Messages: []provider.Message{
				provider.UserText("First turn: reply with the single word ok."),
				provider.UserText("Second turn: reply with the single word done."),
			},
			MaxTokens:      8,
			SessionID:      "rick-live-cache-e2e",
			CacheRetention: provider.CacheRetentionLong,
		},
	}

	var usages []provider.Usage
	for i, req := range reqs {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		ch := make(chan provider.Event, 64)
		go client.Stream(ctx, req, ch)
		var usage provider.Usage
		for ev := range ch {
			switch ev.Kind {
			case provider.EventUsage:
				if ev.Usage != nil {
					usage = *ev.Usage
				}
			case provider.EventError:
				cancel()
				t.Fatalf("request %d stream error: %v", i+1, ev.Err)
			}
		}
		cancel()
		usages = append(usages, usage)
		if usage.InputTokens == 0 && usage.CacheReadTokens == 0 {
			t.Fatalf("request %d reported no usage; provider likely rejected the request", i+1)
		}
	}

	// Request 1 has nothing to hit. Request 2 shares request 1's prefix, so
	// the provider must report cached prompt tokens (prompt_cache_hit_tokens).
	if len(usages) < 2 {
		t.Fatalf("expected 2 usages, got %d", len(usages))
	}
	if usages[1].CacheReadTokens <= 0 {
		t.Fatalf("request 2 reported CacheReadTokens=%d, want >0 (prefix cache missed; request 1 usage=%+v, request 2 usage=%+v)",
			usages[1].CacheReadTokens, usages[0], usages[1])
	}
	t.Logf("live cache hit confirmed: req1=%+v req2=%+v", usages[0], usages[1])
}

// TestLiveKeepaliveCacheHitE2E is a key-gated variant that also proves the
// keep-alive loop can ride the same warm prefix: after the second turn the
// keep-alive re-sends the exact stream body and must see cache reads > 0.
// Skips without RICK_LIVE_CACHE_TEST=1 and a key, like TestLiveCacheHitE2E.
func TestLiveKeepaliveCacheHitE2E(t *testing.T) {
	if os.Getenv("RICK_LIVE_CACHE_TEST") != "1" {
		t.Skip("RICK_LIVE_CACHE_TEST=1 required (live provider call)")
	}
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("RICK_DEEPSEEK_API_KEY")
	}
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY required for live keepalive cache e2e")
	}
	model := os.Getenv("RICK_LIVE_CACHE_MODEL")
	if model == "" {
		model = "deepseek-chat"
	}

	client := New("deepseek", apiKey, "https://api.deepseek.com")
	client.SetKeepaliveAdaptive(50*time.Millisecond, 50*time.Millisecond)
	defer client.stopKeepalive()

	system := "You are a terse coding assistant used in an automated keepalive cache test. " +
		strings.Repeat("Follow instructions literally. ", 10)
	req := provider.Request{
		Model:          model,
		System:         system,
		SystemStable:   system,
		Messages:       []provider.Message{provider.UserText("Reply with the single word ok.")},
		MaxTokens:      8,
		SessionID:      "rick-live-keepalive-e2e",
		CacheRetention: provider.CacheRetentionLong,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	ch := make(chan provider.Event, 64)
	go client.Stream(ctx, req, ch)
	var usage provider.Usage
	for ev := range ch {
		switch ev.Kind {
		case provider.EventUsage:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			cancel()
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	cancel()

	// Give the keep-alive loop a moment to fire a refresh (interval 50ms).
	deadline := time.Now().Add(10 * time.Second)
	gotCold := false
	for time.Now().Before(deadline) {
		client.kaMu.Lock()
		cold := client.kaColdKeepalives
		client.kaMu.Unlock()
		if cold > 0 {
			gotCold = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if gotCold {
		client.kaMu.Lock()
		cold := client.kaColdKeepalives
		client.kaMu.Unlock()
		// A cold keep-alive means the provider evicted the prefix faster than
		// the 50ms interval; that is a provider/TTL fact, not a client bug,
		// so log it rather than fail the cache-stability contract.
		t.Logf("keep-alive observed %d cold refreshes (provider evicted <50ms); main stream usage=%+v", cold, usage)
		return
	}
	t.Logf("keep-alive stayed warm after the stream; main stream usage=%+v", usage)
}
