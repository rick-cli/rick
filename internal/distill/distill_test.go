package distill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"rick/internal/provider"
)

type stubSummarizer struct {
	summary string
	err     error
	calls   int
}

func (s *stubSummarizer) Summarize(_ context.Context, input SummaryInput) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	return fmt.Sprintf("%s\n%s", s.summary, input.Messages[0].Text()), nil
}

// pair builds one atomic assistant tool_use + user tool_result turn.
func pair(id, payload string) []provider.Message {
	return []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ID: id, Name: "read", Input: []byte(`{"path":"a.go"}`)}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock(id, payload, false)}},
	}
}

// bigHistory builds 3 old turns (a user request plus a tool pair each) plus a
// live zone of two logical turns and a final request.
func bigHistory() []provider.Message {
	var messages []provider.Message
	for i := 0; i < 3; i++ {
		messages = append(messages, provider.UserText(fmt.Sprintf("old request %d", i)))
		messages = append(messages, pair(fmt.Sprintf("t%d", i), strings.Repeat("payload", 500))...)
	}
	messages = append(messages, provider.UserText("live request"))
	messages = append(messages, pair("tlive", strings.Repeat("live", 200))...)
	messages = append(messages, provider.UserText("latest request"))
	return messages
}

func baseOptions(s *stubSummarizer) Options {
	return Options{
		Summarizer:         s,
		MaxMessages:        12,
		LiveZoneTurns:      2,
		MinCacheBreakBytes: 1,
		MinHistoryTokens:   1,
		MinRatio:           0.0,
		MinLiveRatio:       0.5,
	}
}

func TestDistillReplacesOldPrefix(t *testing.T) {
	messages := bigHistory()
	result := Distill(messages, map[int]bool{1: true}, baseOptions(&stubSummarizer{summary: "goal: fix build"}))

	if !result.Replaced {
		t.Fatalf("expected distillation: %+v", result.Err)
	}
	// Cache prefix + summary + the newer half (13 - 6 = 7 messages).
	if len(result.Messages) != 9 {
		t.Fatalf("got %d messages, want 9: %d", len(result.Messages), len(result.Messages))
	}
	if result.Messages[0].Text() != "old request 0" {
		t.Fatalf("cache prefix lost: %+v", result.Messages[0])
	}
	if !strings.Contains(result.Messages[1].Text(), "goal: fix build") {
		t.Fatalf("summary message missing: %+v", result.Messages[1])
	}
	if result.Messages[len(result.Messages)-1].Text() != "latest request" {
		t.Fatalf("live zone lost: %+v", result.Messages[len(result.Messages)-1])
	}
	// The oldest half after the breakpoint (index 1..5) is distilled away.
	if result.OmittedCount != 5 {
		t.Fatalf("omitted %d messages, want 5", result.OmittedCount)
	}
	if result.AfterBytes >= result.BeforeBytes {
		t.Fatal("distillation did not save bytes")
	}
}

func TestDistillNeverSplitsToolPair(t *testing.T) {
	messages := bigHistory()
	result := Distill(messages, map[int]bool{1: true}, baseOptions(&stubSummarizer{summary: "s"}))

	if !result.Replaced {
		t.Fatalf("expected distillation: %+v", result.Err)
	}
	// Every remaining tool_use must be immediately followed by its tool_result.
	for index, message := range result.Messages {
		if hasBlock(message, "tool_use") {
			if index+1 >= len(result.Messages) || !hasBlock(result.Messages[index+1], "tool_result") {
				t.Fatalf("tool pair was split at index %d", index)
			}
		}
	}
}

func TestDistillRequiresCacheBreakpoint(t *testing.T) {
	messages := bigHistory()
	result := Distill(messages, map[int]bool{}, baseOptions(&stubSummarizer{summary: "s"}))
	if result.Replaced {
		t.Fatal("distillation without a stable cache breakpoint must not replace history")
	}
	if result.Err == nil {
		t.Fatal("expected an error explaining why distillation was skipped")
	}
}

func TestDistillDisabledWithoutSummarizer(t *testing.T) {
	result := Distill(bigHistory(), map[int]bool{1: true}, Options{})
	if result.Replaced || result.Err != nil {
		t.Fatal("distillation without a summarizer must be a no-op")
	}
}

func TestDistillSummarizerFailureLeavesHistoryUntouched(t *testing.T) {
	messages := bigHistory()
	result := Distill(messages, map[int]bool{1: true}, baseOptions(&stubSummarizer{err: context.Canceled}))
	if result.Replaced {
		t.Fatal("a failed summarizer must not replace history")
	}
	if result.Err == nil {
		t.Fatal("expected a wrapped summarizer error")
	}
	if len(result.Messages) != len(messages) {
		t.Fatal("history must be returned unchanged")
	}
}

func TestDistillShadowPriceDefersWhenPlannedPrefixStillWarm(t *testing.T) {
	messages := bigHistory()
	opts := baseOptions(&stubSummarizer{summary: "s"})
	// Without a planned prefix, distillation proceeds normally.
	base := Distill(messages, map[int]bool{1: true}, opts)
	if !base.Replaced {
		t.Fatal("baseline distillation should replace the old prefix")
	}

	// With a planned prefix larger than the region being folded, the fold is
	// deferred: rewriting warm bytes for no gain is not worth the invalidation.
	opts.PlannedPrefixTokens = 1 << 20 // far larger than the folded region
	deferral := Distill(messages, map[int]bool{1: true}, opts)
	if deferral.Replaced {
		t.Fatal("distill must defer when the planned cache prefix still covers the folded region")
	}
	if deferral.Err != ErrPlannedPrefixStillWarm {
		t.Fatalf("deferral error = %v, want ErrPlannedPrefixStillWarm", deferral.Err)
	}
}

func TestDistillShadowPriceAllowsWhenPlannedPrefixSmall(t *testing.T) {
	messages := bigHistory()
	opts := baseOptions(&stubSummarizer{summary: "s"})
	// A tiny planned prefix means the cache is already cold: folding can only
	// help (the summary becomes the new warm prefix).
	opts.PlannedPrefixTokens = 8
	result := Distill(messages, map[int]bool{1: true}, opts)
	if !result.Replaced {
		t.Fatal("distill should proceed when the planned prefix is tiny")
	}
}

// TestDistillSummarizerReplaysFullPrefix verifies the item-1 fix: the
// summarizer receives the *entire* routed prefix up to the fold point
// (system + tools + all messages, including the cached head before the
// breakpoint), so the auxiliary call is a genuine byte-prefix-extension of
// the warm request instead of a cold-start that skips the cached region.
func TestDistillSummarizerReplaysFullPrefix(t *testing.T) {
	messages := bigHistory() // 13 messages: breakpoint at index 1
	recorder := &recordingSummarizer{stub: &stubSummarizer{summary: "s"}}
	opts := baseOptions(recorder.stub)
	opts.Summarizer = recorder
	result := Distill(messages, map[int]bool{1: true}, opts)

	if !result.Replaced {
		t.Fatalf("expected distillation: %+v", result.Err)
	}
	if recorder.input == nil {
		t.Fatal("summarizer was never called")
	}
	// The replay must start at the cached head (message 0), not at the
	// breakpoint (message 1): skipping the head would misalign the aux
	// call's token sequence with the routed request and defeat warm-prefix
	// reuse exactly when the conversation is largest.
	if len(recorder.input.Messages) < 2 {
		t.Fatalf("replay has %d messages, want the cached head + folded region", len(recorder.input.Messages))
	}
	if recorder.input.Messages[0].Text() != "old request 0" {
		t.Fatalf("replay must start at the cached head, got %q", recorder.input.Messages[0].Text())
	}
	// The replay must extend past the breakpoint into the folded region.
	found := false
	for _, msg := range recorder.input.Messages {
		if strings.Contains(msg.Text(), "old request 1") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("replay must include the folded region after the breakpoint")
	}
}

// recordingSummarizer captures the SummaryInput so the test can assert the
// exact replayed prefix.
type recordingSummarizer struct {
	stub  *stubSummarizer
	input *SummaryInput
}

func (r *recordingSummarizer) Summarize(ctx context.Context, input SummaryInput) (string, error) {
	r.input = &input
	return r.stub.Summarize(ctx, input)
}

// TestTokenPricedLiveTailPinsTokenBudget pins the harness-style token-meter
// live tail: with a live-zone token budget, the fold boundary keeps the
// newest messages summing to at least the budget verbatim (the harness's
// selectCompactableRange floor semantics), replacing the ratio boundary. The
// fold end never lands mid-pair when routed through distillablePrefix.
func TestTokenPricedLiveTailPinsTokenBudget(t *testing.T) {
	// All-cheap messages: 7 messages, each ~30-40 tokens under the
	// 4 chars/token heuristic. A 200-token budget keeps the newest ~5-6
	// messages, moving the fold boundary well before the default ratio 0.4
	// fold (end = 7*0.4 ≈ 2).
	messages := []provider.Message{
		provider.UserText("old turn 1 " + strings.Repeat("context ", 40)),
		provider.AssistantText("old reply 1 " + strings.Repeat("analysis ", 40)),
		provider.UserText("old turn 2 " + strings.Repeat("context ", 40)),
		provider.AssistantText("old reply 2 " + strings.Repeat("analysis ", 40)),
		provider.UserText("recent question " + strings.Repeat("detail ", 40)),
		provider.AssistantText("recent reply " + strings.Repeat("detail ", 40)),
		provider.UserText("current question " + strings.Repeat("detail ", 40)),
	}
	end := tokenPricedFoldEnd(messages, 200)
	if end <= 0 || end >= len(messages) {
		t.Fatalf("token fold end = %d, want in (0, %d)", end, len(messages))
	}
	// Floor: the retained tail costs at least the budget.
	retained := messages[end:]
	tailTokens := 0
	for _, m := range retained {
		tailTokens += 4 + priceMessageTokens(m)
	}
	if tailTokens < 200 {
		t.Fatalf("retained tail costs %d tokens, want >= 200", tailTokens)
	}
	// A budget larger than the whole transcript folds nothing.
	if got := tokenPricedFoldEnd(messages, 1<<20); got != len(messages) {
		t.Fatalf("oversized budget fold end = %d, want %d (nothing distillable)", got, len(messages))
	}
	// Zero budget disables the token price.
	if got := tokenPricedFoldEnd(messages, 0); got != len(messages) {
		t.Fatalf("zero budget fold end = %d, want %d", got, len(messages))
	}

	// distillablePrefix pair-safety: a fold that lands inside a
	// tool_use/tool_result pair must back up to the pair start via
	// walkPairs, never splitting the pair.
	pairMessages := []provider.Message{
		provider.UserText("old turn 1"),
		provider.AssistantText("old reply 1"),
		provider.UserText("old turn 2"),
		provider.AssistantText("old reply 2"),
		provider.Message{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "call-9", Name: "bash", Input: json.RawMessage(`{"command":"x"}`)},
		}},
		provider.Message{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "call-9", Content: "short output"},
		}},
		provider.UserText("current question"),
	}
	candidate, foldEnd := distillablePrefix(pairMessages, 0, 32, 1, 60, 0.9)
	// Pair-safety: the boundary must never land between a tool_use and its
	// tool_result — i.e. never fold the tool_use while retaining the result
	// (end == 5) or retain the tool_use while folding the result (end == 4
	// with index 4 being the tool_use). Ending at 6 folds the whole pair,
	// which is safe.
	if foldEnd == 5 || foldEnd == 4 {
		t.Fatalf("fold end %d splits a tool_use/tool_result pair", foldEnd)
	}
	if foldEnd <= 0 || foldEnd > len(pairMessages) {
		t.Fatalf("fold end out of range: %d", foldEnd)
	}
	if len(candidate) == 0 {
		t.Fatal("pair-safe distillablePrefix returned no candidate")
	}
}

// TestAlignCutToCacheBlockPinsBlockBoundary pins the harness-style cache-block
// alignment: the fold insert point moves to the nearest message boundary whose
// serialized token offset is a whole cache-block multiple, so the fold never
// splits a provider cache block (which would re-bill a partial block cold on
// the next request). The walk never folds the anchored first message.
func TestAlignCutToCacheBlockPinsBlockBoundary(t *testing.T) {
	// Each 40-char text message prices at 11 tokens (40/4 + 1). With a 22
	// token block, cut=3 (33 tokens) is mid-block; walking back one message
	// reaches cut=2 (22 tokens), a whole block.
	messages := []provider.Message{
		provider.UserText("anchor " + strings.Repeat("a", 34)),
		provider.AssistantText("reply " + strings.Repeat("b", 34)),
		provider.UserText("turn " + strings.Repeat("c", 34)),
		provider.AssistantText("more " + strings.Repeat("d", 34)),
	}
	if priceMessageTokens(messages[0]) != 11 {
		t.Fatalf("fixture cost = %d, want 11", priceMessageTokens(messages[0]))
	}
	midBlockCut := 3 // 33 tokens: 33 % 22 == 11, mid-block
	aligned := alignCutToCacheBlock(messages, midBlockCut, 22)
	if aligned != 2 {
		t.Fatalf("aligned cut = %d, want 2 (33 tokens -> 22 tokens)", aligned)
	}
	if aligned <= 0 {
		t.Fatalf("aligned cut = %d, want > 0 (anchor never folded)", aligned)
	}
	// The aligned prefix cost is a whole block multiple.
	total := 0
	for i := 0; i < aligned; i++ {
		total += priceMessageTokens(messages[i])
	}
	if total%22 != 0 {
		t.Fatalf("aligned prefix costs %d tokens, want a whole 22-token block", total)
	}
	// An already-aligned cut is returned unchanged.
	alignedSame := alignCutToCacheBlock(messages, aligned, 22)
	if alignedSame != aligned {
		t.Fatalf("already-aligned cut moved: %d -> %d", aligned, alignedSame)
	}
	// cut=1 (anchor) is never aligned below itself.
	if got := alignCutToCacheBlock(messages, 1, 22); got != 1 {
		t.Fatalf("anchor cut moved to %d, want 1", got)
	}
}
