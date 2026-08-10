package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"rick/internal/budget"
	"rick/internal/distill"
	"rick/internal/provider"
)

// distillSystemPrompt asks a fast model to compress the oldest part of a
// session into a strict Markdown struct a future turn can consume quickly.
const distillSystemPrompt = `You are compressing the oldest part of an AI coding session for a future turn of the same agent.

Produce a strict Markdown struct with exactly these sections:
- **Goal:** What are we doing?
- **Facts:** What do we know is true?
- **Failed Paths:** What did we try and reject?

Be dense. Preserve exact file paths, symbol names, commands, and error strings. Omit conversational filler.`

const (
	distillTimeout   = 45 * time.Second
	distillMaxTokens = 512
)

// providerSummarizer backs distill.Options.Summarizer with a fast provider
// round-trip on the distill model.
type providerSummarizer struct {
	provider provider.Provider
	model    string
}

// Summarize implements distill.Summarizer. On any failure (provider error,
// timeout, empty output) it returns a deterministic LLM-free fallback summary
// built from the transcript instead of failing the whole distillation: after
// a 429 or timeout the session still gets an informative handoff, and no aux
// tokens are spent on the fallback.
func (s providerSummarizer) Summarize(ctx context.Context, messages []provider.Message) (string, error) {
	if s.provider == nil || s.model == "" {
		return "", errors.New("distill: provider or model unavailable")
	}
	req := provider.Request{
		Model:          s.model,
		System:         distillSystemPrompt,
		Messages:       []provider.Message{provider.UserText(renderTranscript(messages))},
		MaxTokens:      distillMaxTokens,
		CacheRetention: provider.CacheRetentionNone,
	}
	ctx, cancel := context.WithTimeout(ctx, distillTimeout)
	defer cancel()

	ch := make(chan provider.Event, 64)
	go s.provider.Stream(ctx, req, ch)

	var b strings.Builder
	for ev := range ch {
		switch ev.Kind {
		case provider.EventText:
			b.WriteString(ev.Text)
		case provider.EventError:
			if ev.Err != nil {
				return staticFallbackSummary(messages), nil
			}
			return staticFallbackSummary(messages), nil
		case provider.EventDone:
			if text := strings.TrimSpace(b.String()); text != "" {
				return text, nil
			}
			return staticFallbackSummary(messages), nil
		}
	}
	return staticFallbackSummary(messages), nil
}

// staticFallbackSummary is a deterministic, LLM-free handoff built from the
// transcript: the most recent user asks, assistant/tool actions, and any
// error text. It is bounded so a huge transcript still yields a small note.
func staticFallbackSummary(messages []provider.Message) string {
	var b strings.Builder
	b.WriteString("**Goal:** continue the conversation from the state below.\n\n")
	b.WriteString("**Facts:**\n")
	turns := 0
	for i := len(messages) - 1; i >= 0 && turns < staticFallbackTurns; i-- {
		text := strings.TrimSpace(messages[i].Text())
		if text != "" && len(text) <= staticFallbackCharLimit {
			b.WriteString("- " + text + "\n")
			turns++
		}
	}
	b.WriteString("\n**Failed Paths:** (see transcript; summarizer was unavailable)\n")
	return b.String()
}

const (
	staticFallbackTurns     = 8
	staticFallbackCharLimit = 400
)

// renderTranscript flattens candidate messages into a compact transcript that
// includes tool calls and results, not just plain text. The transcript is
// bounded: thinking traces are stripped, each message is capped, and only the
// head + tail of the whole transcript survive with an omitted-middle marker,
// so a pathological transcript can never blow past the summarizer's input
// budget. Secrets are force-redacted at this boundary — the summary persists
// and re-enters the prompt on every later turn, so a secret that reaches the
// summarizer would be re-broadcast forever.
func renderTranscript(messages []provider.Message) string {
	var b strings.Builder
	b.WriteString("[transcript head]\n")
	for index, message := range messages {
		if index >= transcriptHeadMessages && index < len(messages)-transcriptTailMessages {
			if index == transcriptHeadMessages {
				b.WriteString(fmt.Sprintf("[%d messages omitted]\n", len(messages)-transcriptHeadMessages-transcriptTailMessages))
			}
			continue
		}
		if message.Role == provider.RoleAssistant {
			b.WriteString("assistant: ")
		} else {
			b.WriteString("user: ")
		}
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				b.WriteString(redactBoundarySecrets(block.Text))
			case "thinking":
				continue // thinking traces never help the summary and leak tokens
			case "tool_use":
				b.WriteString("[tool " + block.Name + " " + summarizeToolArgs(block.Input) + "]")
			case "tool_result":
				b.WriteString("[result " + redactBoundarySecrets(block.Content) + "]")
			}
			b.WriteString(" ")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// CompactBoundMessages returns a bounded, redacted copy of messages for a
// compaction/summary call: thinking traces stripped, per-message text capped,
// and secrets masked. The summary persists and re-enters the prompt on every
// later turn, so a secret that reaches the summarizer would be re-broadcast
// forever; the cap keeps the aux call small and cheap even for a long session.
func CompactBoundMessages(messages []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(messages))
	for _, message := range messages {
		msg := message
		msg.Content = make([]provider.ContentBlock, 0, len(message.Content))
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				block.Text = redactBoundarySecrets(block.Text)
				msg.Content = append(msg.Content, block)
			case "tool_result":
				block.Content = redactBoundarySecrets(block.Content)
				msg.Content = append(msg.Content, block)
			case "thinking":
				continue
			default:
				msg.Content = append(msg.Content, block)
			}
		}
		out = append(out, msg)
	}
	return out
}

// Transcript bounds for the summarizer input: keep the first and last N
// messages verbatim (capped per message below) and skip the middle, so a
// long session's summary call stays small and cheap.
const (
	transcriptHeadMessages = 12
	transcriptTailMessages = 6
	transcriptMsgCapChars  = 4000
)

// redactBoundarySecrets deterministically masks common secret shapes (API
// keys, bearer tokens, passwords) before a summary transcript is sent to the
// model. This is a blunt, conservative pass: it redacts on shape, not on
// known values, so it never misses a credential even if it was never
// registered anywhere.
func redactBoundarySecrets(text string) string {
	if len(text) > transcriptMsgCapChars {
		// Keep the head and tail of an oversized message so the model still
		// sees the beginning and the error/diagnostic at the end.
		text = text[:transcriptMsgCapChars] + "\n…(truncated)"
	}
	replacer := strings.NewReplacer(
		"sk-", "sk-***",
		"sk-ant-", "sk-ant-***",
		"Bearer ", "Bearer ***",
		"api_key=", "api_key=***",
		"apikey=", "apikey=***",
		"password=", "password=***",
		"passwd=", "passwd=***",
		"token=", "token=***",
		"secret=", "secret=***",
	)
	return replacer.Replace(text)
}

// maxDistillToolArgChars bounds each tool_use input echoed into the distill
// transcript so a huge sub-task prompt (subagent delegation) is never
// re-broadcast wholesale back into the session summary.
const maxDistillToolArgChars = 800

// summarizeToolArgs renders a tool_use input compactly: full text when it is
// small, otherwise a deterministic key+size summary. Key order is sorted so
// identical payloads always render identically.
func summarizeToolArgs(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	if len(input) <= maxDistillToolArgChars {
		return string(input)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err == nil {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return fmt.Sprintf("[%d bytes; keys: %s]", len(input), strings.Join(keys, ","))
	}
	return fmt.Sprintf("[%d bytes]", len(input))
}

// distillSummarizer returns the configured summarizer, or builds one from the
// primary provider when distillation is enabled but no stub was injected.
func (r *Runner) distillSummarizer() distill.Summarizer {
	if r.cfg.DistillSummarizer != nil {
		return r.cfg.DistillSummarizer
	}
	if r.cfg.Provider == nil {
		return nil
	}
	model := r.cfg.DistillModel
	if model == "" {
		model = r.cfg.Model
	}
	return providerSummarizer{provider: r.cfg.Provider, model: model}
}

// distillAtPercent is the share of the context window at which the oldest
// stable prefix is folded into a summary. Kept below the eviction band of
// DeepSeek-style providers: their automatic prefix cache is a bounded LRU, so
// a view sitting near the window top gets its oldest half evicted and
// re-billed on the next turn. Distilling earlier keeps the live view inside
// the cached region and the hit rate high.
const distillAtPercent = 55

// shouldDistill reports whether the transcript is close enough to the context
// budget that distilling the oldest stable prefix is worthwhile.
func (r *Runner) shouldDistill(plan budget.Result, contextWindow int) bool {
	if !r.cfg.EnableDistillation || r.distillSummarizer() == nil {
		return false
	}
	if plan.Truncated {
		return true
	}
	return contextWindow > 0 && plan.TotalInputTokens*100 >= contextWindow*distillAtPercent
}

// distillOptions builds the distillation policy for the current run.
func (r *Runner) distillOptions() distill.Options {
	opts := r.cfg.DistillOptions
	if opts.Summarizer == nil {
		opts.Summarizer = r.distillSummarizer()
	}
	return opts
}
