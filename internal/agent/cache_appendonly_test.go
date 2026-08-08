package agent

import (
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
