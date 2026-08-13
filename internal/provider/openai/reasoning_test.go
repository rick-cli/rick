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

func TestStreamUsesGLMThinkingAndReasoningContent(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New("zai", "test-key", server.URL)
	client.HTTP = server.Client()
	events := make(chan provider.Event)
	go client.Stream(context.Background(), provider.Request{
		Model:       "glm-4.7",
		Messages:    []provider.Message{provider.UserText("hello")},
		MaxTokens:   4096,
		Reasoning:   provider.ReasoningMedium,
		Temperature: func() *float64 { value := 0.2; return &value }(),
	}, events)

	var thinking string
	for event := range events {
		if event.Kind == provider.EventThinking {
			thinking += event.Text
		}
	}

	if thinking != "thinking" {
		t.Fatalf("thinking stream = %q, want %q", thinking, "thinking")
	}
	if _, ok := request["reasoning_effort"]; ok {
		t.Fatal("GLM request unexpectedly used reasoning_effort")
	}
	thinkingBody, ok := request["thinking"].(map[string]any)
	if !ok || thinkingBody["type"] != "enabled" || thinkingBody["clear_thinking"] != false {
		t.Fatalf("thinking body = %#v, want enabled with preserved thinking", request["thinking"])
	}
	if _, ok := request["temperature"]; ok {
		t.Fatal("GLM reasoning request unexpectedly included temperature")
	}

	messages, ok := request["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", request["messages"])
	}
	prior := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "thinking", Text: "prior reasoning"}}}
	wire := toWireWithReasoning("", []provider.Message{prior}, true, false)
	encoded, err := json.Marshal(wire[0])
	if err != nil {
		t.Fatalf("encode assistant message: %v", err)
	}
	if !strings.Contains(string(encoded), `"reasoning_content":"prior reasoning"`) {
		t.Fatalf("assistant reasoning content missing from %s", encoded)
	}
}

func TestOpenCodeZenPreservesReasoningContent(t *testing.T) {
	prior := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "thinking", Text: "prior reasoning"}}}
	wire := toWireWithReasoning("", []provider.Message{prior}, true, false)
	encoded, err := json.Marshal(wire[0])
	if err != nil {
		t.Fatalf("encode assistant message: %v", err)
	}
	if !strings.Contains(string(encoded), `"reasoning_content":"prior reasoning"`) {
		t.Fatalf("assistant reasoning content missing from %s", encoded)
	}

	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New("opencode-zen", "test-key", server.URL)
	client.HTTP = server.Client()
	events := make(chan provider.Event)
	go client.Stream(context.Background(), provider.Request{
		// Zen models cluster around gpt/gemini names, which would normally be
		// classified as OpenAI-style reasoning and drop reasoning_content.
		Model:     "gemini-3-pro",
		Messages:  []provider.Message{prior, provider.UserText("continue")},
		Reasoning: provider.ReasoningHigh,
	}, events)
	for range events {
	}

	messages, ok := request["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v", request["messages"])
	}
	encoded, err = json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("encode sent assistant message: %v", err)
	}
	if !strings.Contains(string(encoded), `"reasoning_content":"prior reasoning"`) {
		t.Fatalf("zen request dropped reasoning_content, got %s", encoded)
	}
}

func TestOpenCodeZenThinkingOnlyAssistantGetsContent(t *testing.T) {
	// A turn that produced only thinking (e.g. a truncated "continue"
	// exchange) must still serialize with a non-empty "content" so
	// OpenAI-compatible endpoints don't reject the assistant message with
	// "content or tool_calls must be set". An empty assistant block would
	// fail the request and force an uncached retry, dropping the provider
	// prefix-cache hit rate for the whole session.
	thinkingOnly := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{
		Type: "thinking", Text: "deep reasoning with no text and no tool call",
	}}}
	wire := toWireWithReasoning("", []provider.Message{thinkingOnly}, true, false)
	if len(wire) != 1 {
		t.Fatalf("expected one assistant message, got %d", len(wire))
	}
	wm := wire[0]
	if wm.Content == nil {
		t.Fatalf("assistant message has nil content: %#v", wm)
	}
	if len(wm.ToolCalls) != 0 {
		t.Fatalf("unexpected tool calls: %#v", wm.ToolCalls)
	}
	if got, ok := wm.Content.(string); !ok || got == "" {
		t.Fatalf("assistant content is empty (%T %#v); want reasoning echoed as content", wm.Content, wm.Content)
	}
	if wm.ReasoningContent == nil || *wm.ReasoningContent == "" {
		t.Fatalf("expected reasoning_content preserved")
	}
	encoded, err := json.Marshal(wm)
	if err != nil {
		t.Fatalf("encode assistant message: %v", err)
	}
	if !strings.Contains(string(encoded), "deep reasoning") {
		t.Fatalf("serialized assistant message lost reasoning text: %s", encoded)
	}
	if strings.Contains(string(encoded), `"tool_calls"`) {
		t.Fatalf("serialized assistant message carries tool_calls on a thinking-only turn: %s", encoded)
	}
}

func TestOpenCodeZenKeepsEmptyReasoningFieldOnToolTurns(t *testing.T) {
	toolTurn := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{
		Type: "tool_use", ID: "call-1", Name: "read", Input: json.RawMessage(`{"path":"a"}`),
	}}}

	request := reasoningEchoClient(t, "opencode-zen", "deepseek-v4-flash-free", []provider.Message{toolTurn}, provider.ReasoningHigh)
	encoded, err := json.Marshal(request["messages"])
	if err != nil {
		t.Fatalf("encode sent messages: %v", err)
	}
	if !strings.Contains(string(encoded), `"reasoning_content":""`) {
		t.Fatalf("assistant tool turn omitted empty reasoning_content: %s", encoded)
	}
}

// reasoningEchoClient runs Stream against a probe server and returns the
// decoded JSON request it received.
func reasoningEchoClient(t *testing.T, providerID, model string, messages []provider.Message, level provider.ReasoningEffort) map[string]any {
	t.Helper()
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New(providerID, "test-key", server.URL)
	client.HTTP = server.Client()
	events := make(chan provider.Event)
	go client.Stream(context.Background(), provider.Request{
		Model:     model,
		Messages:  messages,
		Reasoning: level,
	}, events)
	for range events {
	}
	return request
}

// When history contains thinking but the model name classifies as non-reasoning
// (OpenAI-effort or none), rick must still echo reasoning_content back. The
// DeepSeek-style endpoint rejects the exchange otherwise, regardless of the
// provider id — so the old hardcoded opencode-zen/open-code-go whitelist was
// not enough.
func TestReasoningEchoedForAnyProviderWhenHistoryHasThinking(t *testing.T) {
	prior := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "thinking", Text: "prior reasoning"}}}
	for _, test := range []struct {
		id    string
		model string
	}{
		// gemini/gpt names classify as OpenAI-effort style; on a plain OpenAI
		// provider that would normally strip reasoning_content.
		{"custom-gateway", "gemini-3-pro"},
		{"openwebui", "gpt-5"},
		{"ollama", "deepseek-r1"},
	} {
		request := reasoningEchoClient(t, test.id, test.model, []provider.Message{prior, provider.UserText("continue")}, provider.ReasoningHigh)
		messages, ok := request["messages"].([]any)
		if !ok || len(messages) == 0 {
			t.Fatalf("%s: messages = %#v", test.id, request["messages"])
		}
		encoded, err := json.Marshal(messages[0])
		if err != nil {
			t.Fatalf("%s: encode sent assistant message: %v", test.id, err)
		}
		if !strings.Contains(string(encoded), `"reasoning_content":"prior reasoning"`) {
			t.Fatalf("%s: history with thinking dropped reasoning_content, got %s", test.id, encoded)
		}
	}
}

func TestOpenRouterUsesNormalizedReasoningObject(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New("openrouter", "test-key", server.URL)
	client.HTTP = server.Client()
	client.SetModels([]provider.ModelInfo{{
		ID:                    "vendor/model",
		ReasoningKnown:        true,
		ReasoningEffortsKnown: true,
		ReasoningEfforts:      []provider.ReasoningEffort{provider.ReasoningLow, provider.ReasoningHigh, provider.ReasoningMax},
	}})
	events := make(chan provider.Event)
	go client.Stream(context.Background(), provider.Request{
		Model:     "vendor/model",
		Messages:  []provider.Message{provider.UserText("hello")},
		Reasoning: provider.ReasoningMax,
	}, events)
	for range events {
	}

	reasoning, ok := request["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != string(provider.ReasoningMax) {
		t.Fatalf("reasoning = %#v, want normalized max effort", request["reasoning"])
	}
	if _, ok := request["reasoning_effort"]; ok {
		t.Fatal("OpenRouter request unexpectedly used root reasoning_effort")
	}
}

func TestOpenRouterEnablementOnlyUsesEnabledFlag(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New("openrouter", "test-key", server.URL)
	client.HTTP = server.Client()
	client.SetModels([]provider.ModelInfo{{ID: "qwen/model", ReasoningKnown: true, ReasoningSupportsMaxTokens: true}})
	events := make(chan provider.Event)
	go client.Stream(context.Background(), provider.Request{
		Model:     "qwen/model",
		Messages:  []provider.Message{provider.UserText("hello")},
		Reasoning: provider.ReasoningOn,
	}, events)
	for range events {
	}

	reasoning, ok := request["reasoning"].(map[string]any)
	if !ok || reasoning["enabled"] != true {
		t.Fatalf("reasoning = %#v, want enabled flag", request["reasoning"])
	}
	if _, ok := reasoning["effort"]; ok {
		t.Fatal("enablement-only request unexpectedly selected an effort")
	}
}

func TestUnknownModelOnlyGetsGenericReasoningWhenEnabled(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New("custom-gateway", "test-key", server.URL)
	client.HTTP = server.Client()
	for _, test := range []struct {
		reasoning provider.ReasoningEffort
		wantField bool
	}{
		{reasoning: provider.ReasoningOff, wantField: false},
		{reasoning: provider.ReasoningMedium, wantField: true},
	} {
		request = nil
		events := make(chan provider.Event)
		go client.Stream(context.Background(), provider.Request{
			Model:     "vendor/new-model",
			Messages:  []provider.Message{provider.UserText("hello")},
			Reasoning: test.reasoning,
		}, events)
		for range events {
		}
		_, found := request["reasoning_effort"]
		if found != test.wantField {
			t.Fatalf("reasoning_effort present=%v for level %q, want %v", found, test.reasoning, test.wantField)
		}
		if test.wantField && request["reasoning_effort"] != string(provider.ReasoningMedium) {
			t.Fatalf("reasoning_effort = %#v, want %q", request["reasoning_effort"], provider.ReasoningMedium)
		}
	}
}

// Older reasoning blocks are dead weight that multiplies prompt size and
// breaks the provider cache. Only the most recent thinking-carrying assistant
// turn must be echoed back (DeepSeek/GLM need the previous turn's reasoning);
// older turns keep an empty reasoning_content so the endpoint accepts them.
func TestOnlyMostRecentReasoningIsEchoed(t *testing.T) {
	old := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: strings.Repeat("old reasoning ", 100)},
		{Type: "text", Text: "old answer"},
	}}
	recent := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "recent reasoning"},
		{Type: "tool_use", ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"x"}`)},
	}}
	msgs := []provider.Message{old, provider.UserText("turn 2"), recent}
	wire := toWireWithReasoning("", msgs, true, false)

	var oldReasoning, recentReasoning *string
	for _, wm := range wire {
		if wm.Role != "assistant" {
			continue
		}
		if wm.ReasoningContent == nil {
			continue
		}
		if *wm.ReasoningContent == "recent reasoning" {
			recentReasoning = wm.ReasoningContent
		}
		if strings.Contains(*wm.ReasoningContent, "old reasoning") {
			oldReasoning = wm.ReasoningContent
		}
	}

	if recentReasoning == nil {
		t.Fatal("most recent thinking must be echoed back")
	}
	if oldReasoning != nil {
		t.Fatalf("stale reasoning must be stripped, got %q (len %d)", *oldReasoning, len(*oldReasoning))
	}
}

// DeepSeek-line providers keys prompt caching off a byte-identical prefix, so
// stripping stale reasoning every turn (the pre-fix behavior) makes the prefix
// change in the middle and re-bills the whole tail. Retaining all reasoning is
// append-only: the previous request's bytes are a prefix of the next, so the
// automatic cache hits and only the new tail is charged.
func TestRetainAllReasoningKeepsAppendOnlyPrefix(t *testing.T) {
	old := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "old reasoning"},
		{Type: "text", Text: "old answer"},
	}}
	recent := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "recent reasoning"},
		{Type: "tool_use", ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"x"}`)},
	}}
	msgs := []provider.Message{old, provider.UserText("turn 2"), recent}
	wire := toWireWithReasoning("", msgs, true, true)

	var oldReasoning, recentReasoning *string
	for _, wm := range wire {
		if wm.Role != "assistant" || wm.ReasoningContent == nil {
			continue
		}
		if *wm.ReasoningContent == "recent reasoning" {
			recentReasoning = wm.ReasoningContent
		}
		if strings.Contains(*wm.ReasoningContent, "old reasoning") {
			oldReasoning = wm.ReasoningContent
		}
	}

	if recentReasoning == nil {
		t.Fatal("most recent thinking must be echoed back")
	}
	if oldReasoning == nil {
		t.Fatal("retain-all must keep the older reasoning block for an append-only prefix")
	}
	if *oldReasoning != "old reasoning" {
		t.Fatalf("older reasoning = %q, want the full original value", *oldReasoning)
	}
}

// A DeepSeek-line end-to-end request (opencode-zen gateway) must keep the older
// reasoning block alongside the newest one so the prompt grows only at the end.
func TestOpenCodeZenRequestRetainsAllReasoning(t *testing.T) {
	old := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "old reasoning"}, {Type: "text", Text: "old answer"},
	}}
	recent := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "recent reasoning"},
		{Type: "tool_use", ID: "call-1", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
	}}
	request := reasoningEchoClient(t, "opencode-zen", "deepseek-v4-flash-free",
		[]provider.Message{old, provider.UserText("turn 2"), recent}, provider.ReasoningHigh)
	for _, raw := range request["messages"].([]any) {
		msg := raw.(map[string]any)
		if msg["role"] != "assistant" {
			continue
		}
		reasoning, _ := msg["reasoning_content"].(string)
		if reasoning == "old reasoning" {
			return // older reasoning retained: prefix stays append-only
		}
	}
	t.Fatalf("opencode-zen request dropped older reasoning; sent: %v", request["messages"])
}

// TestOpenCodeZenCapsReasoningEcho verifies the P3 knob: with a positive
// MaxReasoningTurns, DeepSeek-line providers keep only the most recent turn's
// reasoning and strip older blocks — shrinking the prompt at the cost of one
// deliberate prefix rewrite. Default (0) keeps everything append-only, which is
// why this cap is opt-in.
func TestOpenCodeZenCapsReasoningEcho(t *testing.T) {
	old := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "old reasoning"}, {Type: "text", Text: "old answer"},
	}}
	recent := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "recent reasoning"},
		{Type: "tool_use", ID: "call-1", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
	}}
	request := reasoningEchoClientCapped(t, "opencode-zen", "deepseek-v4-flash-free",
		[]provider.Message{old, provider.UserText("turn 2"), recent}, provider.ReasoningHigh, 1)

	sawOld := false
	sawRecent := false
	for _, raw := range request["messages"].([]any) {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		if reasoning, _ := msg["reasoning_content"].(string); reasoning == "old reasoning" {
			sawOld = true
		} else if reasoning == "recent reasoning" {
			sawRecent = true
		}
		if sawOld && sawRecent {
			break
		}
	}
	if sawOld {
		t.Fatalf("MaxReasoningTurns=1 retained older reasoning; sent: %v", request["messages"])
	}
	if !sawRecent {
		t.Fatalf("MaxReasoningTurns=1 dropped the most recent reasoning; sent: %v", request["messages"])
	}
}

// reasoningEchoClientCapped is like reasoningEchoClient but fixes a
// MaxReasoningTurns cap on the sent request.
func reasoningEchoClientCapped(t *testing.T, providerID, model string, messages []provider.Message, level provider.ReasoningEffort, maxTurns int) map[string]any {
	t.Helper()
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := New(providerID, "test-key", server.URL)
	client.HTTP = server.Client()
	events := make(chan provider.Event)
	go client.Stream(context.Background(), provider.Request{
		Model:             model,
		Messages:          messages,
		Reasoning:         level,
		MaxReasoningTurns: maxTurns,
	}, events)
	for range events {
	}
	return request
}

// TestDeepSeekPassbackReasoningDropsToolCallFreeTurns pins the passback rule:
// with PassbackReasoning set, reasoning_content is echoed only for assistant
// turns that carried tool calls. The decision is per-message and stable
// (history never gains or loses a tool call), so the cached prefix is never
// rewritten while the fresh tail shrinks.
func TestDeepSeekPassbackReasoningDropsToolCallFreeTurns(t *testing.T) {
	toolTurn := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "tool-turn reasoning"},
		{Type: "tool_use", ID: "call-1", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
	}}
	plainTurn := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "plain reasoning"},
		{Type: "text", Text: "the answer"},
	}}
	wire := toWireWithStableMarkedGPT56("sys", "", []provider.Message{plainTurn, toolTurn},
		true, true, false, false, true, nil)

	seenPlain := false
	seenTool := false
	for _, wm := range wire {
		if wm.Role != "assistant" {
			continue
		}
		if wm.ReasoningContent != nil && strings.Contains(*wm.ReasoningContent, "tool-turn reasoning") {
			seenTool = true
		}
		// Plain (tool-call-free) turn reasoning must be dropped under passback.
		if wm.ReasoningContent != nil && strings.Contains(*wm.ReasoningContent, "plain reasoning") {
			seenPlain = true
		}
	}
	if !seenTool {
		t.Fatalf("passback dropped tool-call turn reasoning; wire: %#v", wire)
	}
	if seenPlain {
		t.Fatalf("passback kept tool-call-free turn reasoning; wire: %#v", wire)
	}
}

// TestDeepSeekPassbackIsAppendStable verifies the per-message passback decision
// is stable across requests: a prefix that carries the passback decision never
// flips a message's reasoning on/off as the conversation grows, so the bytes
// before the previous tail stay identical.
func TestDeepSeekPassbackIsAppendStable(t *testing.T) {
	toolTurn := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "tool reasoning"},
		{Type: "tool_use", ID: "call-1", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
	}}
	plainTurn := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: "plain reasoning"},
		{Type: "text", Text: "answer"},
	}}
	base := []provider.Message{toolTurn, provider.UserText("turn 2"), plainTurn}
	baseWire := toWireWithStableMarkedGPT56("sys", "", base, true, true, false, false, true, nil)
	grown := append(append([]provider.Message(nil), base...), provider.UserText("turn 3"))
	grownWire := toWireWithStableMarkedGPT56("sys", "", grown, true, true, false, false, true, nil)

	if len(baseWire) != len(grownWire)-1 {
		t.Fatalf("grown wire added %d messages, want exactly the one new tail", len(grownWire)-len(baseWire))
	}
	for i := range baseWire {
		bm, _ := json.Marshal(baseWire[i])
		gm, _ := json.Marshal(grownWire[i])
		if string(bm) != string(gm) {
			t.Fatalf("wire message %d diverged under passback:\nbase=%s\ngrown=%s", i, bm, gm)
		}
	}
}
