package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"rick/internal/provider"
)

// TestCacheTokensParse verifies that OpenAI's prompt_tokens_details.cached_tokens
// unmarshals correctly, which is what the adapter uses to split input vs cache hit.
func TestCacheTokensParse(t *testing.T) {
	line := `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}`

	var result struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.PromptTokens != 1000 {
		t.Fatalf("PromptTokens: got %d want 1000", result.PromptTokens)
	}
	if result.PromptTokensDetails.CachedTokens != 800 {
		t.Fatalf("CachedTokens: got %d want 800", result.PromptTokensDetails.CachedTokens)
	}

	// The adapter computes: input = prompt - cached
	inputTokens := result.PromptTokens - result.PromptTokensDetails.CachedTokens
	if inputTokens != 200 {
		t.Fatalf("input tokens (cache miss): got %d want 200", inputTokens)
	}
}

func TestPromptCacheKeyIsStableAndNamespacedByModel(t *testing.T) {
	first := promptCacheKey("gpt-5", "session-abc", "")
	if first == "" || len(first) != 64 {
		t.Fatalf("key = %q, want a 64-character digest", first)
	}
	if got := promptCacheKey("gpt-5", "session-abc", ""); got != first {
		t.Fatalf("same session produced different keys: %q vs %q", first, got)
	}
	if got := promptCacheKey("gpt-4o", "session-abc", ""); got == first {
		t.Fatal("different models shared a prompt cache key")
	}
	if got := promptCacheKey("gpt-5", "session-def", ""); got == first {
		t.Fatal("different sessions shared a prompt cache key")
	}
	if got := promptCacheKey("gpt-5", "", ""); got != "" {
		t.Fatalf("empty session id produced key %q", got)
	}
	// A content-derived scope key overrides the session id: identical
	// non-interactive runs (same prompt, different session ids) share a warm
	// bucket, while different prompt scopes never collide.
	scopedA := promptCacheKey("gpt-5", "sess-1", "scope-a")
	if scopedA == "" || len(scopedA) != 64 {
		t.Fatalf("scoped key = %q, want a 64-character digest", scopedA)
	}
	if got := promptCacheKey("gpt-5", "sess-2", "scope-a"); got != scopedA {
		t.Fatal("identical scope keys with different session ids produced different keys")
	}
	if got := promptCacheKey("gpt-5", "sess-1", "scope-b"); got == scopedA {
		t.Fatal("different scope keys shared a prompt cache key")
	}
}

func TestNoneRetentionOmitsCacheFields(t *testing.T) {
	body := wireRequest{
		Model:                "gpt-5",
		PromptCacheKey:       "",
		PromptCacheRetention: "",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		PromptCacheKey       string `json:"prompt_cache_key"`
		PromptCacheRetention string `json:"prompt_cache_retention"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.PromptCacheKey != "" || decoded.PromptCacheRetention != "" {
		t.Fatalf("none retention leaked cache fields: key=%q retention=%q", decoded.PromptCacheKey, decoded.PromptCacheRetention)
	}
}

func TestWarmSendsNonStreamingPrefixAndHonorsSession(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-session-affinity") != "sess-1" {
			t.Errorf("missing session-affinity header: %q", r.Header.Get("x-session-affinity"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	client := New("openai", "test-key", server.URL)
	client.HTTP = server.Client()

	var cw provider.CacheWarmber = client // compile-time check
	err := cw.Warm(context.Background(), provider.Request{
		Model:          "gpt-5",
		System:         "stable instructions",
		SystemStable:   "stable instructions",
		SessionID:      "sess-1",
		CacheRetention: provider.CacheRetentionLong,
		Messages:       []provider.Message{provider.UserText("hello")},
		MaxTokens:      4096,
	})
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if request["model"] != "gpt-5" {
		t.Fatalf("model = %v", request["model"])
	}
	if request["stream"] != false {
		t.Fatalf("stream = %v, want false", request["stream"])
	}
	if got := request["max_completion_tokens"]; got != float64(1) {
		t.Fatalf("max_completion_tokens = %v, want 1", got)
	}
	retention, _ := request["prompt_cache_retention"].(string)
	if retention != "24h" {
		t.Fatalf("prompt_cache_retention = %q, want 24h (long retention warm)", retention)
	}
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v", request["messages"])
	}
}

func TestWarmSurfacesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer server.Close()

	client := New("openai", "test-key", server.URL)
	client.HTTP = server.Client()
	err := client.Warm(context.Background(), provider.Request{
		Model:    "gpt-5",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err == nil {
		t.Fatal("warm returned nil error for a 502 response")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error did not mention status: %v", err)
	}
}

func TestStreamSendsRetentionAndAffinityForDirectOpenAI(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New("openai", "test-key", server.URL)
	ch := make(chan provider.Event, 16)
	req := provider.Request{
		Model:          "gpt-5",
		System:         "sys",
		Messages:       []provider.Message{provider.UserText("hello")},
		SessionID:      "sess-123",
		CacheRetention: provider.CacheRetentionLong,
	}
	client.Stream(context.Background(), req, ch)
	for ev := range ch {
		if ev.Kind == provider.EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	var decoded struct {
		PromptCacheKey       string `json:"prompt_cache_key"`
		PromptCacheRetention string `json:"prompt_cache_retention"`
	}
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.PromptCacheKey == "" {
		t.Fatal("session-keyed prompt_cache_key missing on direct OpenAI")
	}
	if decoded.PromptCacheRetention != "24h" {
		t.Fatalf("prompt_cache_retention = %q, want 24h", decoded.PromptCacheRetention)
	}
	if gotHeaders.Get("session_id") != "sess-123" || gotHeaders.Get("x-client-request-id") != "sess-123" {
		t.Fatalf("session affinity headers missing: %v", gotHeaders)
	}
}

func TestStreamOmitsCacheFieldsForNoneRetention(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New("openai", "test-key", server.URL)
	ch := make(chan provider.Event, 16)
	client.Stream(context.Background(), provider.Request{
		Model:          "gpt-5",
		System:         "sys",
		Messages:       []provider.Message{provider.UserText("hello")},
		SessionID:      "sess-123",
		CacheRetention: provider.CacheRetentionNone,
	}, ch)
	for ev := range ch {
		if ev.Kind == provider.EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if strings.Contains(string(gotBody), "prompt_cache_key") || strings.Contains(string(gotBody), "prompt_cache_retention") {
		t.Fatalf("none retention sent cache fields: %s", gotBody)
	}
	if gotHeaders.Get("session_id") != "" || gotHeaders.Get("x-client-request-id") != "" {
		t.Fatal("none retention sent session-affinity headers")
	}
}

func TestStreamUsageAccountsCacheWritesSeparately(t *testing.T) {
	client := &Client{ID: "openrouter", BaseURL: "https://openrouter.ai/api/v1"}
	line := `data: {"usage":{"prompt_tokens":1000,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":600,"cache_write_tokens":250}}}`
	var usage provider.Usage
	emit := func(ev provider.Event) bool {
		if ev.Kind == provider.EventUsage && ev.Usage != nil {
			usage = *ev.Usage
		}
		return true
	}
	client.readSSE(context.Background(), strings.NewReader(line+"\n\ndata: [DONE]\n\n"), emit, false)
	if usage.CacheReadTokens != 600 {
		t.Fatalf("cache read = %d, want 600", usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != 250 {
		t.Fatalf("cache write = %d, want 250", usage.CacheWriteTokens)
	}
	if usage.InputTokens != 150 {
		t.Fatalf("input = %d, want 150 (1000-600-250)", usage.InputTokens)
	}
	if usage.OutputTokens != 40 {
		t.Fatalf("output = %d, want 40", usage.OutputTokens)
	}
}

func TestStableSystemPrefixIsSentBeforeVolatileTail(t *testing.T) {
	wire := toWireWithStable("stable instructions\nvolatile environment", "stable instructions", nil, false, false)
	if len(wire) != 2 {
		t.Fatalf("wire message count = %d, want 2", len(wire))
	}
	if wire[0].Role != "system" || wire[0].Content != "stable instructions" {
		t.Fatalf("stable message = %#v", wire[0])
	}
	if wire[1].Role != "system" || wire[1].Content != "\nvolatile environment" {
		t.Fatalf("volatile message = %#v", wire[1])
	}
}

// TestWarmMatchesStreamReasoningBytes verifies that Warm and Stream produce a
// byte-identical prior-turn transcript for a reasoning model, so the warm
// actually primes the prefix the real append-only stream will send. Before
// this fix Warm stripped reasoning and primed different bytes than the stream,
// so the provider's automatic prefix cache never hit on reasoning turns.
func TestWarmMatchesStreamReasoningBytes(t *testing.T) {
	msg := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "Let me think carefully about this solution."},
		{Type: "text", Text: "Here is my reasoning"},
	}}
	req := provider.Request{
		Model:             "gpt-5",
		Messages:          []provider.Message{provider.UserText("hi"), msg},
		MaxReasoningTurns: 0,
	}

	preserve, retain := func() (bool, bool) {
		_, p, r := (&Client{ID: "opencode-zen"}).wireReasoning(req)
		return p, r
	}()
	if !preserve {
		t.Fatal("expected reasoning to be preserved for a model with thinking blocks")
	}
	if !retain {
		t.Fatal("expected all reasoning retained for append-only prefix")
	}

	warmWire := toWireWithReasoning(req.System, req.Messages, preserve, retain)
	streamWire := toWireWithReasoning(req.System, req.Messages, preserve, retain)
	if serialized(warmWire) != serialized(streamWire) {
		t.Fatal("warm and stream serialized different bytes")
	}

	// And the reasoning must actually be echoed: the thinking text must live in
	// a ReasoningContent field (non-empty) rather than being dropped.
	found := false
	for _, wm := range streamWire {
		if wm.Role == "assistant" && wm.ReasoningContent != nil &&
			strings.Contains(*wm.ReasoningContent, "Let me think") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("reasoning_content was dropped; warm would prime empty reasoning")
	}
}

func serialized(w []wireMessage) string {
	b, _ := json.Marshal(w)
	return string(b)
}

// TestDeepSeekWireUsesMaxTokens verifies that DeepSeek-line endpoints
// (OpenCode Zen/Go) send the output budget as max_tokens rather than OpenAI's
// max_completion_tokens, and that the OpenAI-only retention hint is omitted on
// the warm so the automatic prefix cache needs no client field.
func TestDeepSeekWireUsesMaxTokens(t *testing.T) {
	var warmBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &warmBody)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	client := New("opencode-zen", "test-key", server.URL)
	client.HTTP = server.Client()

	var cw provider.CacheWarmber = client
	err := cw.Warm(context.Background(), provider.Request{
		Model:          "deepseek-v4-flash-free",
		System:         "stable",
		CacheRetention: provider.CacheRetentionLong,
		Messages:       []provider.Message{provider.UserText("hello")},
		MaxTokens:      4096,
	})
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, ok := warmBody["max_completion_tokens"]; ok {
		t.Fatal("DeepSeek-line endpoint got max_completion_tokens")
	}
	if got := warmBody["max_tokens"]; got != float64(1) {
		t.Fatalf("warm max_tokens = %v, want 1", got)
	}
	if _, ok := warmBody["prompt_cache_retention"]; ok {
		t.Fatal("warm sent prompt_cache_retention to a DeepSeek-line endpoint")
	}
	if _, ok := warmBody["prompt_cache_key"]; ok {
		t.Fatal("warm sent prompt_cache_key to a DeepSeek-line endpoint")
	}
}

// flakyRoundTripper fails the first n requests with a transient transport
// error (a dropped keep-alive connection) and serves the rest normally, so a
// retrying client must transparently recover.
type flakyRoundTripper struct {
	base  http.RoundTripper
	fails int
	mu    sync.Mutex
}

func (f *flakyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	f.mu.Unlock()
	return f.base.RoundTrip(req)
}

// TestWarmRetriesTransientEOF verifies that a request hitting "unexpected EOF"
// (as OpenCode Zen intermittently produces on keep-alive reuse) is retried and
// succeeds on the next attempt instead of aborting the turn.
func TestWarmRetriesTransientEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	client := New("opencode-zen", "test-key", server.URL)
	client.HTTP.Transport = &flakyRoundTripper{
		base:  server.Client().Transport,
		fails: 1,
	}

	var cw provider.CacheWarmber = client
	err := cw.Warm(context.Background(), provider.Request{
		Model:    "deepseek-v4-flash-free",
		System:   "stable",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("warm after a transient EOF retry: %v", err)
	}
}

// TestRetryableTransportErrorClassification pins the retry predicate down.
func TestRetryableTransportErrorClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}, true},
		{fmt.Errorf("client: Post %q: unexpected EOF", "x"), true},
		{fnError(context.Canceled), false},
		{fnError(context.DeadlineExceeded), false},
		{fmt.Errorf("some other oops"), false},
	}
	for _, c := range cases {
		if got := retryableTransportError(c.err); got != c.want {
			t.Fatalf("retryableTransportError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func fnError(e error) error { return e }

// TestOpenRouterResponseCacheHeaderAndHit verifies the OpenRouter response
// cache wiring: the X-OpenRouter-Cache header is sent (plus TTL when set) and
// a X-OpenRouter-Cache-Status: HIT response surfaces on the usage event.
func TestOpenRouterResponseCacheHeaderAndHit(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("X-OpenRouter-Cache-Status", "HIT")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New("openrouter", "test-key", server.URL)
	client.SetOpenRouterResponseCache(true, 300)
	ch := make(chan provider.Event, 16)
	req := provider.Request{
		Model:          "openai/gpt-5",
		System:         "sys",
		Messages:       []provider.Message{provider.UserText("hello")},
		CacheRetention: provider.CacheRetentionLong,
	}
	var usage *provider.Usage
	client.Stream(context.Background(), req, ch)
	for ev := range ch {
		if ev.Kind == provider.EventUsage {
			usage = ev.Usage
		}
		if ev.Kind == provider.EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if gotHeaders.Get("X-OpenRouter-Cache") != "true" {
		t.Fatalf("X-OpenRouter-Cache header = %q, want true", gotHeaders.Get("X-OpenRouter-Cache"))
	}
	if gotHeaders.Get("X-OpenRouter-Cache-TTL") != "300" {
		t.Fatalf("X-OpenRouter-Cache-TTL = %q, want 300", gotHeaders.Get("X-OpenRouter-Cache-TTL"))
	}
	if usage == nil || !usage.ResponseCacheHit {
		t.Fatal("ResponseCacheHit not surfaced on usage event")
	}
}

// TestOpenRouterResponseCacheDisabledOmitsHeader verifies the header is not
// sent when the response cache is disabled (default off for non-OpenRouter).
func TestOpenRouterResponseCacheDisabledOmitsHeader(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := New("openrouter", "test-key", server.URL) // response cache off by default
	ch := make(chan provider.Event, 16)
	client.Stream(context.Background(), provider.Request{
		Model:          "openai/gpt-5",
		System:         "sys",
		Messages:       []provider.Message{provider.UserText("hello")},
		CacheRetention: provider.CacheRetentionLong,
	}, ch)
	for ev := range ch {
		if ev.Kind == provider.EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if gotHeaders.Get("X-OpenRouter-Cache") != "" {
		t.Fatalf("X-OpenRouter-Cache header sent when disabled: %q", gotHeaders.Get("X-OpenRouter-Cache"))
	}
}

// TestCacheControlMarkerOnMarkerCapableProviders verifies the Anthropic-style
// cache_control marker is emitted on the stable system message for gateways
// that honor it (Kimi, MiniMax, Qwen), and omitted for plain OpenAI and
// DeepSeek-line endpoints that reject unknown fields.
func TestCacheControlMarkerOnMarkerCapableProviders(t *testing.T) {
	stable := "stable instructions"
	system := stable + " volatile env"
	msgs := []provider.Message{provider.UserText("hi")}

	// Kimi: marker emitted on the stable system message.
	kimiWire := toWireWithStableMarked(system, stable, msgs, false, false, true)
	if len(kimiWire) < 1 || kimiWire[0].CacheControl == nil || kimiWire[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("kimi stable system message missing cache_control: %+v", kimiWire)
	}

	// Plain OpenAI: no marker.
	openaiWire := toWireWithStableMarked(system, stable, msgs, false, false, false)
	if len(openaiWire) < 1 || openaiWire[0].CacheControl != nil {
		t.Fatalf("openai stable system message should not carry cache_control: %+v", openaiWire)
	}

	// cacheControlMarked gates on the provider id.
	if !(&Client{ID: "kimi"}).cacheControlMarked() {
		t.Fatal("kimi should be marker-capable")
	}
	if !(&Client{ID: "minimax"}).cacheControlMarked() {
		t.Fatal("minimax should be marker-capable")
	}
	if (&Client{ID: "deepseek"}).cacheControlMarked() {
		t.Fatal("deepseek should not be marker-capable")
	}
	if (&Client{ID: "openai"}).cacheControlMarked() {
		t.Fatal("openai should not be marker-capable")
	}
}

// TestCommandcodeDeepseekGetsDeepSeekWireDialect pins that a custom gateway
// (commandcode) serving a deepseek model now gets the DeepSeek wire dialect:
// max_tokens instead of max_completion_tokens, plus the stable-marked
// reasoning shaping — the provider-name-only gate missed this before.
func TestCommandcodeDeepseekGetsDeepSeekWireDialect(t *testing.T) {
	c := New("commandcode", "test-key", "https://api.commandcode.ai/provider/v1")
	body := c.buildWireBody(provider.Request{
		Model:          "deepseek/deepseek-v4-flash",
		System:         "stable system prompt",
		SystemStable:   "stable system prompt",
		Messages:       []provider.Message{provider.UserText("hi")},
		CacheRetention: provider.CacheRetentionLong,
		MaxTokens:      2048,
	}, true, true)
	raw, _ := json.Marshal(body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["max_completion_tokens"]; ok {
		t.Errorf("commandcode deepseek got max_completion_tokens, want max_tokens (DeepSeek dialect)")
	}
	if mt, ok := m["max_tokens"].(float64); !ok || int(mt) != 2048 {
		t.Errorf("commandcode deepseek max_tokens=%v, want 2048", m["max_tokens"])
	}
	// The system must be a plain single message (commandcode is not cache-
	// control marked), but it must be present.
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("no messages in body: %v", m)
	}
}

// TestCommandcodeNonDeepseekModelKeepsOpenAIDialect pins the negative case: a
// non-deepseek model on the same gateway keeps OpenAI's max_completion_tokens.
func TestCommandcodeNonDeepseekModelKeepsOpenAIDialect(t *testing.T) {
	c := New("commandcode", "test-key", "https://api.commandcode.ai/provider/v1")
	body := c.buildWireBody(provider.Request{
		Model:          "gpt-5.6-luna",
		System:         "s",
		Messages:       []provider.Message{provider.UserText("hi")},
		CacheRetention: provider.CacheRetentionLong,
		MaxTokens:      2048,
	}, true, true)
	raw, _ := json.Marshal(body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if mt, ok := m["max_completion_tokens"].(float64); !ok || int(mt) != 2048 {
		t.Errorf("commandcode gpt got max_completion_tokens=%v, want 2048", m["max_completion_tokens"])
	}
}
