package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"rick/internal/provider"
)

// TestKeepaliveAdaptiveHalvesOnColdBurst verifies the adaptive floor: a
// provider whose keep-alive POSTs keep observing a cold prefix (zero
// cache-read tokens) gets its effective interval halved toward the configured
// minimum, so the loop refreshes before the endpoint's real eviction point.
func TestKeepaliveAdaptiveHalvesOnColdBurst(t *testing.T) {
	var mu sync.Mutex
	var posts []map[string]any
	// Every response reports cached_tokens: 0, so every keep-alive is a cold
	// one and the burst drives the interval down from 400ms toward the 50ms
	// floor.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		posts = append(posts, m)
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":0,\"cache_write_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := kaClient(t, server)
	client.SetKeepaliveAdaptive(400*time.Millisecond, 50*time.Millisecond)
	defer client.stopKeepalive()

	req := provider.Request{
		Model:          "test-model",
		System:         "stable system",
		Messages:       []provider.Message{provider.UserText("hello")},
		SessionID:      "sess-adaptive",
		CacheRetention: provider.CacheRetentionLong,
		MaxTokens:      2048,
	}
	ch := make(chan provider.Event, 8)
	go client.Stream(context.Background(), req, ch)
	for range ch {
	}

	// Wait for a burst of cold keep-alives to halve the interval from 400ms.
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.kaMu.Lock()
		interval := client.kaInterval
		client.kaMu.Unlock()
		if interval < 400*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("keep-alive interval never adapted (stuck at %v, colds=%d)", interval, client.ColdKeepalives())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := client.ColdKeepalives(); got < 2 {
		t.Fatalf("expected >=2 cold keep-alives, got %d", got)
	}
	if got := client.KaInterval(); got < 50*time.Millisecond {
		t.Fatalf("interval %v fell below the adaptive floor 50ms", got)
	}
}
