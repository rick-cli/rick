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

// Summarize implements distill.Summarizer.
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
				return "", ev.Err
			}
			return "", errors.New("distill: provider stream error")
		case provider.EventDone:
			if text := strings.TrimSpace(b.String()); text != "" {
				return text, nil
			}
			return "", errors.New("distill: empty summary")
		}
	}
	return "", ctx.Err()
}

// renderTranscript flattens candidate messages into a compact transcript that
// includes tool calls and results, not just plain text.
func renderTranscript(messages []provider.Message) string {
	var b strings.Builder
	for _, message := range messages {
		if message.Role == provider.RoleAssistant {
			b.WriteString("assistant: ")
		} else {
			b.WriteString("user: ")
		}
		for _, block := range message.Content {
			switch block.Type {
			case "text", "thinking":
				b.WriteString(block.Text)
			case "tool_use":
				b.WriteString("[tool " + block.Name + " " + summarizeToolArgs(block.Input) + "]")
			case "tool_result":
				b.WriteString("[result " + block.Content + "]")
			}
			b.WriteString(" ")
		}
		b.WriteString("\n")
	}
	return b.String()
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
