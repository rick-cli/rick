package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rick/internal/provider"
)

// TestIsGPT56ModelGate pins the exact model-family gate: GPT-5.6+ matches,
// pre-5.6 (gpt-5.5, gpt-5, gpt-4o, o-series) never does, and a provider
// prefix like "openai/" is stripped.
func TestIsGPT56ModelGate(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5.6", true},
		{"openai/gpt-5.6", true},
		{"gpt-5.7", true},
		{"gpt-5.6-mini", true},
		{"gpt-5.10", true},
		{"gpt-5.55", true},
		{"gpt-5.5", false},
		{"gpt-5", false},
		{"gpt-4o", false},
		{"gpt-4.1", false},
		{"o3", false},
		{"o4-mini", false},
	}
	for _, tc := range cases {
		if got := isGPT56(tc.model); got != tc.want {
			t.Errorf("isGPT56(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestGPT56StreamSendsCacheOptionsNotRetention verifies that a GPT-5.6+ model
// sends prompt_cache_options {explicit, 30m} and never the legacy
// prompt_cache_retention field; pre-5.6 models keep the legacy hint and never
// send prompt_cache_options (which they would reject with a 400).
//
// Explicit mode is used because GPT-5.6+ places the implicit breakpoint on
// the latest (changing) message, which re-writes the whole prompt at 1.25x
// and can report cached_tokens = 0 even when the leading tokens are
// identical (OpenAI Prompt Caching guide). rick emits explicit breakpoints
// on the stable system message and the budget's boundary messages, so
// explicit mode reads/writes only those and bills the volatile tail at 1x.
func TestGPT56StreamSendsCacheOptionsNotRetention(t *testing.T) {
	type wire struct {
		PromptCacheOptions   *wireCacheOptions `json:"prompt_cache_options"`
		PromptCacheRetention string            `json:"prompt_cache_retention"`
	}

	cases := []struct {
		model            string
		wantOptions      bool
		wantRetention    string
		wantExplicitMode bool
	}{
		{"gpt-5.6", true, "", true},
		{"gpt-4o", false, "24h", false},
	}

	for _, tc := range cases {
		var got wire
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
			w.Header().Set("content-type", "text/event-stream")
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
		}))
		client := New("openai", "test-key", server.URL)
		ch := make(chan provider.Event, 16)
		client.Stream(context.Background(), provider.Request{
			Model:          tc.model,
			System:         "sys",
			Messages:       []provider.Message{provider.UserText("hello")},
			SessionID:      "sess-123",
			CacheRetention: provider.CacheRetentionLong,
		}, ch)
		for ev := range ch {
			if ev.Kind == provider.EventError {
				t.Fatalf("stream error: %v", ev.Err)
			}
		}
		server.Close()

		if (got.PromptCacheOptions != nil) != tc.wantOptions {
			t.Errorf("model %s: prompt_cache_options present = %v, want %v", tc.model, got.PromptCacheOptions != nil, tc.wantOptions)
		}
		if got.PromptCacheOptions != nil {
			if tc.wantExplicitMode {
				if got.PromptCacheOptions.Mode != "explicit" {
					t.Errorf("model %s: mode = %q, want explicit", tc.model, got.PromptCacheOptions.Mode)
				}
			} else if got.PromptCacheOptions.Mode != "" {
				t.Errorf("model %s: mode = %q, want empty", tc.model, got.PromptCacheOptions.Mode)
			}
			if got.PromptCacheOptions.TTL != "30m" {
				t.Errorf("model %s: ttl = %q, want 30m", tc.model, got.PromptCacheOptions.TTL)
			}
		}
		if got.PromptCacheRetention != tc.wantRetention {
			t.Errorf("model %s: prompt_cache_retention = %q, want %q", tc.model, got.PromptCacheRetention, tc.wantRetention)
		}
	}
}

// TestGPT56BreakpointEmission verifies that a GPT-5.6+ request emits a
// prompt_cache_breakpoint on the stable system message and on each cache
// boundary message, and that pre-5.6 models emit none (they reject the field).
func TestGPT56BreakpointEmission(t *testing.T) {
	msgs := []provider.Message{
		provider.UserText("first"),
		provider.UserText("second"),
		provider.UserText("third"),
	}

	// GPT-5.6: stable system message + boundary at message index 1.
	gpt56 := toWireWithStableMarkedGPT56("stable sys\nvolatile tail", "stable sys", msgs, false, false, false, true, map[int]bool{1: true})
	sysBreakpoints := 0
	boundaryBreakpoints := 0
	for _, wm := range gpt56 {
		if wm.Role == "system" && wm.CacheBreakpoint != nil {
			sysBreakpoints++
			if wm.CacheBreakpoint.Mode != "explicit" {
				t.Fatalf("system breakpoint mode = %q, want explicit", wm.CacheBreakpoint.Mode)
			}
		}
		if wm.Role == "user" && wm.CacheBreakpoint != nil {
			boundaryBreakpoints++
		}
	}
	if sysBreakpoints != 1 {
		t.Fatalf("system breakpoints = %d, want 1", sysBreakpoints)
	}
	if boundaryBreakpoints != 1 {
		t.Fatalf("boundary breakpoints = %d, want 1", boundaryBreakpoints)
	}

	// Pre-5.6: no breakpoints at all.
	legacy := toWireWithStableMarkedGPT56("stable sys\nvolatile tail", "stable sys", msgs, false, false, false, false, map[int]bool{1: true})
	for _, wm := range legacy {
		if wm.CacheBreakpoint != nil {
			t.Fatal("pre-5.6 model emitted a prompt_cache_breakpoint")
		}
	}
}

// TestGPT56WarmUsesExplicitMode verifies the warm request on GPT-5.6+ uses
// prompt_cache_options.mode=explicit so it only writes the marked stable-head
// breakpoint (1 write slot), never an implicit breakpoint on the volatile tail.
func TestGPT56WarmUsesExplicitMode(t *testing.T) {
	var got wireCacheOptions
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded struct {
			PromptCacheOptions wireCacheOptions `json:"prompt_cache_options"`
		}
		_ = json.Unmarshal(body, &decoded)
		got = decoded.PromptCacheOptions
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	client := New("openai", "test-key", server.URL)
	client.HTTP = server.Client()

	var cw provider.CacheWarmber = client
	err := cw.Warm(context.Background(), provider.Request{
		Model:          "gpt-5.6",
		System:         "stable sys\nvolatile tail",
		SystemStable:   "stable sys",
		Messages:       []provider.Message{provider.UserText("hello")},
		SessionID:      "sess-1",
		CacheRetention: provider.CacheRetentionLong,
	})
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if got.Mode != "explicit" {
		t.Fatalf("warm prompt_cache_options.mode = %q, want explicit", got.Mode)
	}
	if got.TTL != "30m" {
		t.Fatalf("warm prompt_cache_options.ttl = %q, want 30m", got.TTL)
	}
}

// TestPromptCacheKeyStableAcrossSessions verifies Phase B1: the routing key is
// derived from the byte-stable system head, so two sessions with the same
// frozen head (same cwd/model) share a key and route to the same cache machine,
// while a changed head produces a different key.
func TestPromptCacheKeyStableAcrossSessions(t *testing.T) {
	stable := "stable system instructions\nfrozen tools"
	reqA := provider.Request{Model: "gpt-5.6", SessionID: "sess-a", SystemStable: stable}
	reqB := provider.Request{Model: "gpt-5.6", SessionID: "sess-b", SystemStable: stable}
	reqC := provider.Request{Model: "gpt-5.6", SessionID: "sess-c", SystemStable: stable + "\nchanged"}

	keyA := promptCacheKey("gpt-5.6", promptCacheScope(reqA), "")
	keyB := promptCacheKey("gpt-5.6", promptCacheScope(reqB), "")
	keyC := promptCacheKey("gpt-5.6", promptCacheScope(reqC), "")

	if keyA == "" || keyB == "" || keyC == "" {
		t.Fatal("expected non-empty keys")
	}
	if keyA != keyB {
		t.Fatal("same stable head across sessions produced different cache keys")
	}
	if keyA == keyC {
		t.Fatal("different stable head produced the same cache key")
	}

	// A session with no stable head falls back to the session id.
	noStable := provider.Request{Model: "gpt-5.6", SessionID: "sess-x"}
	if scope := promptCacheScope(noStable); scope != "sess-x" {
		t.Fatalf("scope without stable head = %q, want session id", scope)
	}

	// CacheScopeKey always wins.
	scoped := provider.Request{Model: "gpt-5.6", SessionID: "sess-y", SystemStable: stable, CacheScopeKey: "fixed-scope"}
	if scope := promptCacheScope(scoped); scope != "fixed-scope" {
		t.Fatalf("scope with CacheScopeKey = %q, want fixed-scope", scope)
	}
}

// TestGPT56NoneRetentionOmitsAllCacheFields verifies that "none" retention on
// a GPT-5.6+ model strips the cache key, options and breakpoints entirely.
func TestGPT56NoneRetentionOmitsAllCacheFields(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New("openai", "test-key", server.URL)
	ch := make(chan provider.Event, 16)
	client.Stream(context.Background(), provider.Request{
		Model:          "gpt-5.6",
		System:         "sys",
		SystemStable:   "sys",
		Messages:       []provider.Message{provider.UserText("hello")},
		SessionID:      "sess-1",
		CacheRetention: provider.CacheRetentionNone,
	}, ch)
	for ev := range ch {
		if ev.Kind == provider.EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	for _, field := range []string{"prompt_cache_key", "prompt_cache_options", "prompt_cache_breakpoint", "prompt_cache_retention"} {
		if strings.Contains(string(gotBody), field) {
			t.Fatalf("none retention sent %q: %s", field, gotBody)
		}
	}
}

// TestGatewayGPT56SendsNoExplicitCacheFields pins that a gpt-5.6+ model on a
// custom gateway (commandcode, openrouter, kilo, ...) does NOT get OpenAI's
// prompt_cache_options / breakpoints / prompt_cache_key — those are direct-
// OpenAI wire features and gateways do plain automatic prefix caching. Sending
// them risks a 400 on gateways that reject unknown fields.
func TestGatewayGPT56SendsNoExplicitCacheFields(t *testing.T) {
	for _, tc := range []struct {
		providerID, model string
	}{
		{"commandcode", "gpt-5.6-luna"},
		{"openrouter", "openai/gpt-5.6"},
		{"kilo", "openai/gpt-5.6-sol"},
		{"nous", "openai/gpt-5.6-terra"},
	} {
		c := New(tc.providerID, "test-key", "https://gateway.test/v1")
		body := c.buildWireBody(provider.Request{
			Model:          tc.model,
			System:         "sys",
			Messages:       []provider.Message{provider.UserText("hi")},
			CacheRetention: provider.CacheRetentionLong,
			MaxTokens:      1024,
		}, true, true)
		raw, _ := json.Marshal(body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["prompt_cache_options"] != nil {
			t.Errorf("%s/%s: sent prompt_cache_options %v, want none", tc.providerID, tc.model, m["prompt_cache_options"])
		}
		if m["prompt_cache_key"] != nil && m["prompt_cache_key"] != "" {
			t.Errorf("%s/%s: sent prompt_cache_key %v, want stripped", tc.providerID, tc.model, m["prompt_cache_key"])
		}
		if m["prompt_cache_retention"] != nil && m["prompt_cache_retention"] != "" {
			t.Errorf("%s/%s: sent prompt_cache_retention %v, want none", tc.providerID, tc.model, m["prompt_cache_retention"])
		}
	}
}

// TestOpenAIGPT56StillSendsExplicitCacheFields is the positive control: direct
// OpenAI gpt-5.6+ still gets prompt_cache_options + breakpoints.
func TestOpenAIGPT56StillSendsExplicitCacheFields(t *testing.T) {
	c := New("openai", "test-key", "https://api.openai.com/v1")
	body := c.buildWireBody(provider.Request{
		Model:          "gpt-5.6",
		System:         "sys",
		Messages:       []provider.Message{provider.UserText("hi")},
		CacheRetention: provider.CacheRetentionLong,
		MaxTokens:      1024,
	}, true, true)
	raw, _ := json.Marshal(body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["prompt_cache_options"] == nil {
		t.Errorf("openai gpt-5.6: prompt_cache_options missing, want present")
	}
	opts := m["prompt_cache_options"].(map[string]any)
	if opts["mode"] != "explicit" {
		t.Errorf("openai gpt-5.6: mode = %v, want explicit", opts["mode"])
	}
}
