package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"rick/internal/distill"
	"rick/internal/provider"
	"rick/internal/tokens"
	"rick/internal/tools"
	"rick/pkg/contextbudget"
)

func TestCapModelToolOutputPreservesCanonicalEventOutput(t *testing.T) {
	fullOutput := "\x1b[2K\rprogress 10%\nFAIL: important diagnostic\n" + strings.Repeat("details ", maxModelToolResultBytes)
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: fullOutput})
	runner := New(Config{Tools: registry})

	block, event := runner.execOne(context.Background(), provider.ToolCall{ID: "call-1", Name: "canonical_output", Input: json.RawMessage(`{}`)})
	if event == nil || event.Output != fullOutput {
		t.Fatal("canonical tool event output was compressed or changed")
	}
	if len(block.Content) >= len(fullOutput) {
		t.Fatalf("provider-facing result was not reduced: %d >= %d", len(block.Content), len(fullOutput))
	}
	if strings.Contains(block.Content, "progress 10%") {
		t.Fatal("provider-facing result retained progress noise")
	}
	if !strings.Contains(block.Content, "tool output truncated") && !strings.Contains(block.Content, "live-zone compressed") {
		t.Fatal("provider-facing result lacks truncation marker")
	}
	if event.Optimization == nil || event.Optimization.SavedTokens <= 0 || !event.Optimization.Truncated {
		t.Fatalf("missing compression metrics: %#v", event.Optimization)
	}
}

func TestCapToolOutputHonorsRunCap(t *testing.T) {
	call := provider.ToolCall{ID: "call-1", Name: "canonical_output", Input: json.RawMessage(`{}`)}
	fullOutput := strings.Repeat("details ", 1<<14)
	large, _ := capToolOutputStatic(call, fullOutput, false, maxModelToolResultBytes)
	small, _ := capToolOutputStatic(call, fullOutput, false, 1<<10)
	if len(small) >= len(large) {
		t.Fatalf("smaller cap did not shrink the provider result: small=%d large=%d", len(small), len(large))
	}

	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: fullOutput})
	runner := New(Config{Tools: registry, MaxToolResultBytes: 1 << 10})
	block, _ := runner.execOne(context.Background(), call)
	if len(block.Content) >= len(fullOutput) {
		t.Fatal("configured cap was not applied on the runner path")
	}
}

func TestBuildRequestTrimsOldGroupsWithoutOrphaningToolResults(t *testing.T) {
	runner := New(Config{
		ContextWindow:      200,
		MaxTokens:          10,
		SafetyMarginTokens: 10,
	})
	messages := []provider.Message{
		provider.UserText(strings.Repeat("old context ", 100)),
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ID: "tool-1", Name: "read", Input: json.RawMessage(`{"path":"x"}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock("tool-1", "result", false)}},
		provider.UserText("latest request"),
	}

	request := runner.buildRequest(messages, nil)
	// P2 stable-head trim: the over-budget oldest group is dropped behind a
	// sentinel rather than silently dropped from the front every turn, so the
	// provider prefix cache stays warm. The "old context" group must be gone.
	for _, m := range request.Messages {
		if strings.Contains(m.Text(), "old context") {
			t.Fatal("buildRequest did not trim the over-budget old head")
		}
	}
	for index, message := range request.Messages {
		if containsBlock(message, "tool_use") {
			if index+1 >= len(request.Messages) || !containsBlock(request.Messages[index+1], "tool_result") {
				t.Fatal("trimmed request left an orphaned tool call")
			}
		}
	}
}

type canonicalOutputTool struct {
	output string
}

func (canonicalOutputTool) Name() string           { return "canonical_output" }
func (canonicalOutputTool) Description() string    { return "test output tool" }
func (canonicalOutputTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (canonicalOutputTool) ReadOnly() bool         { return true }
func (tool canonicalOutputTool) Run(context.Context, tools.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Output: tool.output}, nil
}

func TestLiveZoneCompressionIsReversibleThroughBudget(t *testing.T) {
	payload := `{"items":[` + strings.Repeat(`"a",`, 120) + `"z"],"ok":true}`
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: payload})
	runner := New(Config{Tools: registry})

	block, _ := runner.execOne(context.Background(), provider.ToolCall{ID: "call-live-1", Name: "canonical_output", Input: json.RawMessage(`{}`)})
	if len(block.Content) >= len(payload) {
		t.Fatalf("live zone did not compress: %d >= %d", len(block.Content), len(payload))
	}
	original, ok := runner.budget.LiveOriginal("call-live-1")
	if !ok || original != payload {
		t.Fatal("live-zone original not retrievable from the runner budget")
	}
}

type stubDistillSummarizer struct {
	summary string
	calls   int
}

func (s *stubDistillSummarizer) Summarize(context.Context, []provider.Message) (string, error) {
	s.calls++
	return s.summary, nil
}

func TestBuildRequestDistillsStableOverBudgetHistory(t *testing.T) {
	// Window sized so the ~21K-token history crosses the 85% distillation
	// threshold (24K*0.85 = 20.4K) but stays under the retained budget, so
	// trimming never interferes with the distillable prefix.
	runner := New(Config{
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
		DistillSummarizer: &stubDistillSummarizer{summary: "**Goal:** fix build\n**Facts:** broken\n**Failed Paths:** none"},
	})

	// A history that crosses 85% of the window (without overflowing the
	// retained budget, so trimming does not interfere): five big old tool
	// turns plus a two-turn live zone. Payloads are distinct so
	// content-addressed deduplication does not shrink the transcript before
	// distillation.
	var messages []provider.Message
	for i := 0; i < 5; i++ {
		messages = append(messages, provider.UserText("old request"))
		messages = append(messages, pairMessage("t"+string(rune('a'+i)), strings.Repeat("payload", 4000+i*10))...)
	}
	messages = append(messages, provider.UserText("live request"))
	messages = append(messages, pairMessage("tlive", strings.Repeat("live", 400))...)
	messages = append(messages, provider.UserText("latest request"))

	// The prefix is stable from the first observation (MinStableTurns
	// defaults to 1 now that the system prompt and tools are frozen per
	// session), so an over-budget history is distilled right away.
	summarizer := runner.cfg.DistillSummarizer.(*stubDistillSummarizer)
	request := runner.buildRequest(messages, nil)
	if summarizer.calls == 0 {
		t.Fatal("over-budget stable history was not distilled")
	}
	if !containsDistillSummary(request.Messages) {
		t.Fatalf("distilled summary missing from request messages")
	}
}

func pairMessage(id, payload string) []provider.Message {
	return []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ID: id, Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock(id, payload, false)}},
	}
}

func containsDistillSummary(messages []provider.Message) bool {
	for _, message := range messages {
		if strings.Contains(message.Text(), "**Goal:**") {
			return true
		}
	}
	return false
}

func TestBuildRequestPrunesOldToolResultsOnce(t *testing.T) {
	// A window far above the distillation threshold, so pruning (not
	// distilling) is the only head rewrite that can fire. Prune thresholds
	// are forced down so the small test payloads qualify.
	budget := contextbudget.New(contextbudget.Options{
		PruneMinResultBytes:  256,
		PruneMinReclaimBytes: 400,
		PruneLiveZoneTurns:   1,
	})
	runner := New(Config{
		ContextWindow:      200_000,
		MaxTokens:          10,
		SafetyMarginTokens: 10,
		EnableDistillation: false,
		Budget:             budget,
	})

	var messages []provider.Message
	for i := 0; i < 3; i++ {
		messages = append(messages, provider.UserText("old request"))
		messages = append(messages, pairMessage("pr"+string(rune('a'+i)), strings.Repeat("payload", 300))...)
	}
	messages = append(messages, provider.UserText("live request"))

	first := runner.buildRequest(messages, nil)
	// The first prune commits: old bulky results are summarized.
	pruned := 0
	for _, m := range first.Messages {
		for _, block := range m.Content {
			if block.Type == "tool_result" && strings.HasPrefix(block.Content, "[summarized]") {
				pruned++
			}
		}
	}
	if pruned == 0 {
		t.Fatal("expected old tool results to be pruned into summaries")
	}
	// The summary's original is retrievable via the shared budget.
	for _, m := range messages {
		for _, block := range m.Content {
			if block.Type == "tool_result" && len(block.Content) > 256 {
				if _, ok := runner.budget.StoredPayload(contextbudget.Hash(block.Content)); !ok {
					t.Fatal("pruned original not stored under its content address")
				}
			}
		}
	}

	// A second identical request must not rewrite again: every bulky result
	// is already summarized, so the prune does not commit and the view stays
	// byte-stable (the prefix cache stays warm).
	second := runner.buildRequest(messages, nil)
	if len(second.Messages) != len(first.Messages) {
		t.Fatalf("second build changed message count: %d vs %d", len(second.Messages), len(first.Messages))
	}
	for i := range first.Messages {
		if string(tokens.Marshal(first.Messages[i])) != string(tokens.Marshal(second.Messages[i])) {
			t.Fatalf("second build rewrote message %d (prune not write-once)", i)
		}
	}
}
