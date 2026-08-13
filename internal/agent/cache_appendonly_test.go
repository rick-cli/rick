package agent

import (
	"context"
	"encoding/json"
	"testing"

	"rick/internal/provider"
	"rick/internal/tools"
	"rick/pkg/contextbudget"
)

// TestProviderViewAppendOnly is the phase-B gate: after the stable head is
// engaged, every request's serialized message list must be a byte-prefix of
// the next request's list. Dedup, live-zone compression, and budget trims
// must never rewrite a message that was already sent — the only permitted
// whole-prefix change is the head-trim sentinel reset (the deliberate
// invalidation), after which the view resumes append-only growth.
func TestProviderViewAppendOnly(t *testing.T) {
	registry := tools.NewRegistry()
	payload := `{"rows":[{"id":1,"note":"` + repeatString("same-", 200) + `"}]}`
	registry.Register(canonicalOutputTool{output: payload})

	runner := New(Config{
		ContextWindow: 700, // small enough to force head-trimming
		Tools:         registry,
		Budget:        contextbudget.New(contextbudget.Options{}),
	})

	messages := []provider.Message{provider.UserText("boot the session")}
	schemas := registry.Schemas(nil)

	var prev []provider.Message
	trimmed := false
	for turn := 0; turn < 10; turn++ {
		// Each turn appends a fresh user prompt and a duplicated tool pair.
		messages = append(messages, provider.UserText("continue the work"))
		messages = append(messages, pairMessage("call-"+repeatString("x", 0)+string(rune('a'+turn)), payload)...)
		// A duplicate tool result of the same payload, triggering dedup.
		messages = append(messages, pairMessage("call-2-"+string(rune('a'+turn)), payload)...)

		req := runner.buildRequest(messages, schemas)
		if prev != nil && !trimmed {
			if !isPrefixBytes(prev, req.Messages) {
				t.Fatalf("turn %d: provider view is not a byte-prefix of the previous view", turn)
			}
		}
		if runner.trimEngaged && !trimmed {
			trimmed = true // declared invalidation: head trimmed once
		}
		if trimmed {
			if !isPrefixBytes(prev, req.Messages) {
				t.Fatalf("turn %d: post-trim view is not a byte-prefix of the previous view", turn)
			}
		}
		prev = msgCopy(req.Messages)
	}
}

func isPrefixBytes(prev, cur []provider.Message) bool {
	if len(cur) < len(prev) {
		return false
	}
	for i := 0; i < len(prev); i++ {
		p, _ := json.Marshal(prev[i])
		c, _ := json.Marshal(cur[i])
		if string(p) != string(c) {
			return false
		}
	}
	return true
}

func msgCopy(messages []provider.Message) []provider.Message {
	out := make([]provider.Message, len(messages))
	for i := range messages {
		out[i] = messages[i]
		out[i].Content = append([]provider.ContentBlock(nil), messages[i].Content...)
	}
	return out
}

func repeatString(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// trimWarmProvider streams one short answer per turn and counts Warm calls.
// It implements CacheWarmber so the runner can (re)prime the prefix.
type trimWarmProvider struct {
	warmCalls *int
}

func (trimWarmProvider) Name() string                 { return "trim-warm-provider" }
func (trimWarmProvider) Models() []provider.ModelInfo { return nil }
func (p *trimWarmProvider) Warm(_ context.Context, _ provider.Request) error {
	*p.warmCalls++
	return nil
}
func (p *trimWarmProvider) Stream(_ context.Context, _ provider.Request, ch chan<- provider.Event) {
	defer close(ch)
	ch <- provider.Event{Kind: provider.EventText, Text: "ok"}
	ch <- provider.Event{Kind: provider.EventDone, StopReason: "end_turn"}
}

// TestRunRewarmsAfterHeadTrimWithoutWarmCache pins the <96% cache regression:
// a head-trim rewrites the provider-facing prefix (the trim sentinel replaces
// the dropped head), so the next request is a guaranteed cold re-bill unless
// the new prefix is primed first. That warm must fire even when general
// warming (WarmCache) is disabled — the trim caused the invalidation, and it
// is the only way to avoid a re-bill on the very next turn.
func TestRunRewarmsAfterHeadTrimWithoutWarmCache(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})
	warmCalls := 0
	prov := &trimWarmProvider{warmCalls: &warmCalls}
	runner := New(Config{
		Provider:  prov,
		Model:     "work-model",
		Tools:     registry,
		WarmCache: false, // deliberately off: the trim-triggered warm must still fire
	})
	// Build a transcript large enough that the first buildRequest trims it
	// (retainStable sets lastMutation="head-trim" inside the turn, before the
	// warm gate runs). The trim must trigger a warm even with WarmCache off.
	var messages []provider.Message
	for i := 0; i < 30; i++ {
		messages = append(messages, provider.UserText("continue the work "+string(rune('a'+i))))
		messages = append(messages, provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ID: "t" + string(rune('a'+i)), Name: "canonical_output", Input: json.RawMessage(`{"p":"a.go"}`)}}})
		messages = append(messages, provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{provider.ToolResultBlock("t"+string(rune('a'+i)), repeatString("result-", 200), false)}})
	}
	// A tiny context window forces the trim on turn 1. MaxTokens must stay
	// small: the reserved output budget is subtracted from the context window
	// before the message budget, so a large reserve would zero the message
	// capacity and retainStable would short-circuit without trimming.
	runner.cfg.ContextWindow = 700
	runner.cfg.MaxTokens = 32

	events := make(chan Event, 256)
	go func() {
		for range events {
		}
	}()

	if _, err := runner.Run(context.Background(), messages, events); err != nil {
		t.Fatalf("run error: %v", err)
	}
	if !runner.trimEngaged {
		t.Fatal("test setup: expected head-trim to engage")
	}
	if warmCalls == 0 {
		t.Fatal("head-trim did not trigger a warm with WarmCache off — the next turn re-bills cold")
	}
}
