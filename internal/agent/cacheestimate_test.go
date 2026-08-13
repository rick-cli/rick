package agent

import (
	"context"
	"strings"
	"testing"

	"rick/internal/distill"
	"rick/internal/provider"
	"rick/internal/tokens"
	"rick/internal/tools"
)

// recSumm records the SummaryInput a summarizer received, so tests can assert
// the exact prefix replayed to the auxiliary compaction call.
type recSumm struct {
	input distill.SummaryInput
}

func (r *recSumm) Summarize(_ context.Context, input distill.SummaryInput) (string, error) {
	r.input = input
	return "**Goal:** keep working", nil
}

// TestDistillReplaysRequestPrefix pins the KV-cache-reuse contract: the
// summarizer receives the routed request's system + tools + the distilled
// messages, so the provider-backed implementation can append a trailing
// instruction and extend the warm request's prefix instead of cold-starting a
// bespoke transcript.
func TestDistillReplaysRequestPrefix(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})
	schemas := registry.Schemas(nil)
	runner := New(Config{
		System:             "You are rick, a terse coding agent. Follow AGENTS.md.",
		Model:              "deepseek-v4-flash",
		Tools:              registry,
		ContextWindow:      24_000,
		MaxTokens:          10,
		SafetyMarginTokens: 10,
		EnableDistillation: true,
		DistillOptions: distill.Options{
			MinHistoryTokens:   1,
			MinCacheBreakBytes: 1,
			MinRatio:           0.001,
			MinLiveRatio:       0.5,
			LiveZoneTurns:      2,
			MaxMessages:        48,
		},
	})

	// History that crosses 55% of the window: five big old tool turns plus a
	// two-turn live zone. Payloads are distinct so content-addressed
	// deduplication does not shrink the transcript before distillation.
	var messages []provider.Message
	for i := 0; i < 5; i++ {
		messages = append(messages, provider.UserText("old request "+string(rune('a'+i))))
		messages = append(messages, pairMessage("t"+string(rune('a'+i)), strings.Repeat("payload", 4000+i*10))...)
	}
	messages = append(messages, provider.UserText("live request"))
	messages = append(messages, pairMessage("tlive", strings.Repeat("live", 400))...)
	messages = append(messages, provider.UserText("latest request"))

	summ := &recSumm{}
	runner.cfg.DistillSummarizer = summ

	runner.buildRequest(messages, schemas)
	if summ.input.System == "" {
		t.Fatal("summarizer received no system prefix")
	}
	if summ.input.System != runner.pinnedSystem {
		t.Fatalf("summarizer system prefix does not match the routed request (got %d bytes, want %d)", len(summ.input.System), len(runner.pinnedSystem))
	}
	if len(summ.input.Tools) == 0 {
		t.Fatal("summarizer received no tool schemas")
	}
	if len(summ.input.Messages) == 0 {
		t.Fatal("summarizer received no distilled messages")
	}
	// The distilled messages must be a contiguous ordered subslice of the
	// original conversation: the aux summarization call replays them verbatim
	// (plus the trailing instruction), so its leading tokens align with the
	// warm request's prefix and the provider serves them from cache.
	if !isOrderedSubslice(messages, summ.input.Messages) {
		t.Fatal("summarizer input is not a contiguous subslice of the conversation")
	}
}

func isOrderedSubslice(haystack, needle []provider.Message) bool {
	if len(needle) > len(haystack) {
		return false
	}
outer:
	for start := 0; start+len(needle) <= len(haystack); start++ {
		for i := range needle {
			if string(tokens.Marshal(needle[i])) != string(tokens.Marshal(haystack[start+i])) {
				continue outer
			}
		}
		return true
	}
	return false
}

// TestCachePrefixTokensEstimatesSurvivingBlocks pins the pre-flight estimator:
// a mid-prefix rewrite that keeps N message blocks reports those blocks as
// still cached (floored to the 256-token granularity), while a system-prompt
// rewrite reports zero.
func TestCachePrefixTokensEstimatesSurvivingBlocks(t *testing.T) {
	enc := encodingForTest()
	// System prompt long enough that system + first message clears one
	// 256-token cache block on its own (≈4 chars/token on cl100k).
	system := "You are a terse coding assistant. " + repeatString("guidance and policy for the session. ", 60)
	msgs := []provider.Message{
		provider.UserText("first stable turn with enough content to exceed a cache block " + repeatString("detailed context about the repo layout and build tooling. ", 40)),
		provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "text", Text: "second stable turn"}}},
		provider.UserText("volatile tail that changes"),
	}
	req := provider.Request{System: system, Messages: msgs}
	sysHash := hashBytes(marshalBytes([]byte(system)))
	msgHashes := make([]string, len(msgs))
	for i := range msgs {
		msgHashes[i] = hashBytes(marshalBytes(msgs[i]))
	}

	// Identical view: everything is a prefix, estimate == full prefix floor.
	full := cachePrefixTokens(req, sysHash, "", msgHashes, enc)
	if full == 0 {
		t.Fatal("identical view reported zero cached prefix")
	}

	// System rewrite: nothing survives.
	if got := cachePrefixTokens(req, hashBytes(marshalBytes([]byte("different system "))), "", msgHashes, enc); got != 0 {
		t.Fatalf("system rewrite reported %d cached tokens, want 0", got)
	}

	// Tail change only: first two messages still match.
	tailChanged := append([]provider.Message(nil), msgs...)
	tailChanged[2] = provider.UserText("rewritten tail")
	got := cachePrefixTokens(provider.Request{System: system, Messages: tailChanged}, sysHash, "", msgHashes, enc)
	if got == 0 {
		t.Fatal("tail rewrite reported zero cached prefix; the stable head should still hit")
	}
	if got > full {
		t.Fatalf("tail rewrite estimated %d cached tokens, more than the identical view's %d", got, full)
	}
	if got%cacheBlockTokens != 0 {
		t.Fatalf("estimate %d is not a multiple of the 256-token cache block", got)
	}
}

func encodingForTest() tokens.Encoding {
	return tokens.EncodingForModel("gpt-4o")
}
