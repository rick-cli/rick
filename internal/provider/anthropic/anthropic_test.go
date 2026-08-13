package anthropic

import (
	"encoding/json"
	"testing"

	"rick/internal/provider"
)

func TestWireRequestAddsPromptCacheBreakpoints(t *testing.T) {
	body := wireRequest{
		Model:        "claude-test",
		MaxTokens:    128,
		CacheControl: cacheControlFor(provider.CacheRetentionAuto),
		System:       wireSystem("stable instructions\nvolatile environment", "stable instructions", provider.CacheRetentionAuto),
		Messages:     []wireMessage{{Role: provider.RoleUser, Content: []wireBlock{{Type: "text", Text: "hello"}}}},
		Tools: wireTools([]provider.ToolSchema{
			{Name: "read", Description: "read files", InputSchema: map[string]any{"type": "object"}},
			{Name: "write", Description: "write files", InputSchema: map[string]any{"type": "object"}},
		}, provider.CacheRetentionAuto),
		Stream: true,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var decoded struct {
		CacheControl *struct {
			Type string `json:"type"`
		} `json:"cache_control"`
		System []struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
		Tools []struct {
			Name         string `json:"name"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if decoded.CacheControl == nil || decoded.CacheControl.Type != "ephemeral" {
		t.Fatalf("top-level cache control = %#v, want ephemeral", decoded.CacheControl)
	}

	if len(decoded.System) != 2 || decoded.System[0].Type != "text" || decoded.System[0].Text != "stable instructions" ||
		decoded.System[1].Text != "\nvolatile environment" {
		t.Fatalf("system = %#v, want stable and volatile text blocks", decoded.System)
	}
	if decoded.System[0].CacheControl == nil || decoded.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("system cache control = %#v, want ephemeral", decoded.System[0].CacheControl)
	}
	if decoded.System[1].CacheControl != nil {
		t.Fatal("volatile system block unexpectedly has a cache breakpoint")
	}
	if len(decoded.Tools) != 2 {
		t.Fatalf("tools = %#v, want two tools", decoded.Tools)
	}
	if decoded.Tools[0].CacheControl != nil {
		t.Fatal("non-final tool unexpectedly has a cache breakpoint")
	}
	if decoded.Tools[1].CacheControl == nil || decoded.Tools[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("final tool cache control = %#v, want ephemeral", decoded.Tools[1].CacheControl)
	}
}

func TestToWirePlacesBoundaryOnMarkedMessage(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("first"),
		provider.UserText("second"),
		provider.UserText("third"),
	}
	wire := toWire(messages, map[int]bool{1: true}, provider.CacheRetentionAuto)
	if len(wire) != 3 {
		t.Fatalf("got %d wire messages, want 3", len(wire))
	}
	if wire[0].Content[0].CacheControl != nil {
		t.Fatal("unmarked message unexpectedly has a cache breakpoint")
	}
	if wire[1].Content[0].CacheControl == nil || wire[1].Content[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("marked message missing cache breakpoint: %#v", wire[1].Content[0].CacheControl)
	}
	if wire[2].Content[0].CacheControl == nil {
		t.Fatal("newest message missing the eager cache breakpoint")
	}
}

func TestToWireEagerBoundaryCachesNewestMarkableMessage(t *testing.T) {
	// A turn ends with tool results, which cannot carry cache_control. The
	// eager boundary must walk back to the preceding assistant message.
	messages := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "t1", Content: "result"},
		}},
	}
	wire := toWire(messages, nil, provider.CacheRetentionAuto)
	if len(wire) != 2 {
		t.Fatalf("got %d wire messages, want 2", len(wire))
	}
	if wire[0].Content[0].CacheControl == nil {
		t.Fatal("assistant tool_use should carry the eager boundary")
	}
	if wire[1].Content[0].CacheControl != nil {
		t.Fatal("tool_result message must not carry cache_control")
	}
}

func TestToWireEagerBoundarySkipsThinkingAndToolResult(t *testing.T) {
	// An assistant message that is only thinking + tool_use cannot carry the
	// boundary on thinking; it must land on the tool_use block.
	messages := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: "thinking", Text: "reasoning", Signature: "sig"},
			{Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "t1", Content: "result"},
		}},
	}
	wire := toWire(messages, nil, provider.CacheRetentionAuto)
	if wire[0].Content[0].CacheControl != nil {
		t.Fatal("thinking block must not carry cache_control")
	}
	if wire[0].Content[1].CacheControl == nil {
		t.Fatal("eager boundary should land on the tool_use block")
	}
}

func TestToWireNoneRetentionEmitsNoBreakpoints(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("first"),
		provider.UserText("second"),
	}
	wire := toWire(messages, map[int]bool{0: true}, provider.CacheRetentionNone)
	for index, wm := range wire {
		for _, block := range wm.Content {
			if block.CacheControl != nil {
				t.Fatalf("message %d has a cache breakpoint under retention none", index)
			}
		}
	}
	if cc := wireSystem("instructions", "", provider.CacheRetentionNone); cc[0].CacheControl != nil {
		t.Fatal("system block has a cache breakpoint under retention none")
	}
	if tools := wireTools([]provider.ToolSchema{{Name: "read", Description: "d"}}, provider.CacheRetentionNone); tools[0].CacheControl != nil {
		t.Fatal("tool has a cache breakpoint under retention none")
	}
}

func TestCacheControlForLongRetentionAddsTTL(t *testing.T) {
	if cc := cacheControlFor(provider.CacheRetentionLong); cc == nil || cc.TTL != "1h" {
		t.Fatalf("long retention = %#v, want ttl 1h", cc)
	}
	if cc := cacheControlFor(provider.CacheRetentionAuto); cc == nil || cc.TTL != "" {
		t.Fatalf("auto retention = %#v, want no ttl", cc)
	}
	if cc := cacheControlFor(provider.CacheRetentionNone); cc != nil {
		t.Fatalf("none retention = %#v, want nil", cc)
	}
}

// A ttl:"1h" breakpoint is rejected by Anthropic unless the request opts in to
// the extended-cache-ttl beta. "long" retention must therefore always carry
// the beta header, while auto/none must not (they would not even reach a 1h
// TTL, and sending the beta unconditionally can break proxy/gateway routes).
func TestCacheBetaHeaderMatchesRetention(t *testing.T) {
	if got := cacheBetaHeader(provider.CacheRetentionLong); got != extendedCacheTTLBeta {
		t.Fatalf("long retention beta = %q, want %q", got, extendedCacheTTLBeta)
	}
	if got := cacheBetaHeader(provider.CacheRetentionAuto); got != "" {
		t.Fatalf("auto retention beta = %q, want empty", got)
	}
	if got := cacheBetaHeader(provider.CacheRetentionNone); got != "" {
		t.Fatalf("none retention beta = %q, want empty", got)
	}
}

// TestParseCacheUsagePinsAnthropicNormalization pins the keep-alive cold
// detector: a non-streaming Anthropic response's message.usage normalizes the
// cache-creation tokens out of input (matching the Stream contract), so a
// zero-cache-read keep-alive is detectable.
func TestParseCacheUsagePinsAnthropicNormalization(t *testing.T) {
	raw := []byte(`{"message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":1100,"cache_creation_input_tokens":100}}}`)
	usage := parseCacheUsage(raw)
	if usage == nil {
		t.Fatal("parseCacheUsage returned nil")
	}
	if usage.InputTokens != 1100 {
		t.Fatalf("input = %d, want 1100 (creation 100 normalized out)", usage.InputTokens)
	}
	if usage.CacheReadTokens != 1100 {
		t.Fatalf("cache read = %d, want 1100", usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != 100 {
		t.Fatalf("cache write = %d, want 100", usage.CacheWriteTokens)
	}
	// A warm keep-alive that reported zero cache reads is a cold prefix.
	if usage.CacheReadTokens == 0 {
		t.Fatal("this response has cache reads; the cold path must not fire")
	}
	// Cold response: only creation tokens, no reads.
	cold := parseCacheUsage([]byte(`{"message":{"usage":{"input_tokens":200,"cache_creation_input_tokens":200}}}`))
	if cold == nil || cold.CacheReadTokens != 0 {
		t.Fatalf("cold response not detected: %+v", cold)
	}
	// Unparseable -> nil (no cold signal, no false halving).
	if usage := parseCacheUsage([]byte(`not json`)); usage != nil {
		t.Fatal("unparseable response should return nil")
	}
}
