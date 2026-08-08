package agent

import (
	"strings"
	"testing"

	"rick/internal/provider"
	"rick/internal/tokens"
)

// testReasoningTurn builds an assistant message whose deep-reasoning echo
// rides in a "thinking" block (the DeepSeek-style layout).
func testReasoningTurn(text, reply string) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: "thinking", Text: text},
		{Type: "text", Text: reply},
	}}
}

// thinkingTexts extracts every thinking-block text in view order.
func thinkingTexts(messages []provider.Message) []string {
	var out []string
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "thinking" {
				out = append(out, block.Text)
			}
		}
	}
	return out
}

func reasonCutMessages() []provider.Message {
	return []provider.Message{
		provider.UserText("question one"),
		testReasoningTurn("planning alpha", "start a"),
		provider.UserText("mid input"),
		testReasoningTurn("planning beta", "start b"),
		testReasoningTurn("planning gamma", "start c"),
	}
}

// TestReasoningCutOneShotAppendOnly is the Phase C.2 gate: when
// MaxReasoningTurns > 0 the stale reasoning echo is removed exactly once (an
// attributable structural rewrite) and afterwards the provider view is a
// strict byte-prefix of the next view — never a rotating window that re-bills
// the whole tail every turn.
func TestReasoningCutOneShotAppendOnly(t *testing.T) {
	runner := New(Config{MaxReasoningTurns: 1, ContextWindow: 100000})
	messages := reasonCutMessages()

	req1 := runner.buildRequest(messages, nil)
	if got := thinkingTexts(req1.Messages); strings.Join(got, "|") != "planning gamma" {
		t.Fatalf("first request kept the wrong reasoning turns: %v", got)
	}
	if runner.reasoningCutIndex != 4 {
		t.Fatalf("one-shot cut index = %d, want 4", runner.reasoningCutIndex)
	}

	// More work arrives; the view must grow append-only and the newest
	// reasoning (beyond the cut) rides along whole.
	grown := append(append([]provider.Message(nil), messages...),
		provider.UserText("follow up"),
		testReasoningTurn("planning delta", "start d"),
	)
	req2 := runner.buildRequest(grown, nil)
	if !isPrefixBytes(req1.Messages, req2.Messages) {
		t.Fatal("post-cut request is not a byte-prefix of the first")
	}
	if got := thinkingTexts(req2.Messages); strings.Join(got, "|") != "planning gamma|planning delta" {
		t.Fatalf("post-cut growth changed the reasoning layout: %v", got)
	}

	// One more request: still a byte prefix.
	more := append(append([]provider.Message(nil), grown...), provider.UserText("end"))
	req3 := runner.buildRequest(more, nil)
	if !isPrefixBytes(req2.Messages, req3.Messages) {
		t.Fatal("request three is not a byte-prefix of request two")
	}
}

// TestReasoningCutIndexByCount pins the cut geometry: with three thinking
// turns, keep=1 keeps only the newest, keep=2 keeps the newest two, and
// keep >= the turn count strips nothing.
func TestReasoningCutIndexByCount(t *testing.T) {
	messages := reasonCutMessages()
	if got := reasoningCutIndex(messages, 1); got != 4 {
		t.Fatalf("keep=1 cut = %d, want 4", got)
	}
	if got := reasoningCutIndex(messages, 2); got != 3 {
		t.Fatalf("keep=2 cut = %d, want 3", got)
	}
	if got := reasoningCutIndex(messages, 3); got != 0 {
		t.Fatalf("keep=3 cut = %d, want 0 (nothing to strip)", got)
	}
	if got := reasoningCutIndex([]provider.Message{provider.UserText("x")}, 1); got != 0 {
		t.Fatalf("no reasoning: cut = %d, want 0", got)
	}
}

// TestReasoningCapOffPreservesEverything guards the cache-optimal default
// (cap = 0): the one-shot cut must be a complete no-op, every thinking turn
// stays verbatim, and the projected view is exactly the input.
func TestReasoningCapOffPreservesEverything(t *testing.T) {
	runner := New(Config{ContextWindow: 100000})
	messages := reasonCutMessages()
	req := runner.buildRequest(messages, nil)
	if got := thinkingTexts(req.Messages); strings.Join(got, "|") != "planning alpha|planning beta|planning gamma" {
		t.Fatalf("cap=0 rewrote reasoning: %v", got)
	}
	if runner.reasoningCutSet {
		t.Fatal("cap=0 must not latch the reasoning cut")
	}
}

// TestReasoningTokensMeasured counts the resolved (post-cut) view so the
// per-request telemetry reflects what is actually sent.
func TestReasoningTokensMeasured(t *testing.T) {
	runner := New(Config{MaxReasoningTurns: 1, ContextWindow: 100000})
	messages := reasonCutMessages()
	req := runner.buildRequest(messages, nil)
	counted := countThinkingTokens(req.Messages, tokens.EncodingCl100kBase)
	if counted <= 0 {
		t.Fatalf("countThinkingTokens = %d, want > 0 (newest reasoning survives)", counted)
	}
	// The count only sees the stripped view: strictly smaller than the
	// pre-cut echo.
	full := countThinkingTokens(messages, tokens.EncodingCl100kBase)
	if counted >= full {
		t.Fatalf("post-cut reasoning tokens (%d) must be < pre-cut (%d)", counted, full)
	}
}
