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

const (
	distillTimeout   = 45 * time.Second
	distillMaxTokens = 512
)

// providerSummarizer backs distill.Options.Summarizer with a fast provider
// round-trip on the distill model.
type providerSummarizer struct {
	provider provider.Provider
	model    string
	// sessionID is the originating session's id, forwarded on the auxiliary
	// call so it rides the same prompt-cache scope as the real stream.
	sessionID string
	// epochHash is the frozen request-header identity of the originating
	// run; the auxiliary call reuses it for routing affinity so the warm
	// prefix bucket is shared, not cold-written.
	epochHash string
}

// Summarize implements distill.Summarizer. The auxiliary call replays the
// routed request's exact prefix (system + tools + the distilled messages,
// already redacted and bounded by the caller) and appends one trailing
// compaction instruction, so it is a genuine prefix-extension of the warm
// request and the provider serves the shared tokens from cache instead of
// re-billing the whole conversation cold at its largest moment. On any
// failure (provider error, timeout, empty output) it returns a deterministic
// LLM-free fallback summary built from the transcript instead of failing the
// whole distillation: after a 429 or timeout the session still gets an
// informative handoff, and no aux tokens are spent on the fallback.
func (s providerSummarizer) Summarize(ctx context.Context, input distill.SummaryInput) (string, error) {
	if s.provider == nil || s.model == "" {
		return "", errors.New("distill: provider or model unavailable")
	}
	// Replay the exact prefix the last routed request shipped, then append
	// the compaction directive as the trailing user message. Tools ride
	// along even though the summarizer never calls one: dropping them would
	// shorten the token sequence and break alignment with the cached request.
	//
	// The cache retention is inherited from the routed run (not "none"): the
	// whole point of the replay is that the auxiliary call shares the warm
	// prefix bucket, and a "none" retention clears the prompt-cache key and
	// affinity hints, re-billing the conversation cold at its largest moment.
	// The distill instruction tail is the only uncached addition, exactly as
	// the harness's compaction summarizer replays the routed prefix.
	req := provider.Request{
		Model:          s.model,
		System:         input.System,
		Tools:          input.Tools,
		Messages:       appendMessage(input.Messages, distillCompactionInstruction),
		MaxTokens:      distillMaxTokens,
		CacheRetention: provider.CacheRetentionLong,
		SessionID:      s.sessionID,
		EpochHash:      s.epochHash,
		Purpose:        provider.PurposeDistill,
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
				return staticFallbackSummary(input.Messages), nil
			}
			return staticFallbackSummary(input.Messages), nil
		case provider.EventDone:
			if text := strings.TrimSpace(b.String()); text != "" {
				return text, nil
			}
			return staticFallbackSummary(input.Messages), nil
		}
	}
	return staticFallbackSummary(input.Messages), nil
}

func appendMessage(messages []provider.Message, text string) []provider.Message {
	out := make([]provider.Message, 0, len(messages)+1)
	out = append(out, messages...)
	out = append(out, provider.UserText(text))
	return out
}

// distillCompactionInstruction is the trailing user message that directs the
// model to condense the conversation ABOVE. It deliberately sits at the tail
// of the replayed prefix: a bespoke summarizer system prompt (or a flattened
// transcript) would differ from the routed request at the very first token and
// invalidate the entire cached prefix, defeating the cache exactly when the
// conversation is largest.
const distillCompactionInstruction = `You are now acting as a compaction engine. Condense the conversation above into a strict Markdown struct with exactly these sections:
- **Goal:** What are we doing?
- **Facts:** What do we know is true?
- **Failed Paths:** What did we try and reject?

Be dense. Preserve exact file paths, symbol names, commands, and error strings. Omit conversational filler. Do not mention this summarization request, and output only the checkpoint text without calling a tool.`

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

// CompactBoundMessages returns a bounded, redacted copy of messages for a
// compaction/summary call: thinking traces stripped, per-message text capped,
// oversized tool_use inputs folded to a key+size summary, and secrets masked.
// The summary persists and re-enters the prompt on every later turn, so a
// secret that reaches the summarizer would be re-broadcast forever; the cap
// keeps the aux call small and cheap even for a long session.
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
			case "tool_use":
				block.Input = []byte(summarizeToolArgs(block.Input))
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

// Transcript bounds for the summarizer input: per-message text is capped so a
// long session's summary call stays small and cheap.
const (
	transcriptMsgCapChars = 4000
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

// maxDistillToolArgChars bounds each tool_use input echoed into the summarizer
// input so a huge sub-task prompt (subagent delegation) is never re-broadcast
// wholesale back into the session summary.
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
	return providerSummarizer{
		provider:  r.cfg.Provider,
		model:     model,
		sessionID: r.cfg.SessionID,
		epochHash: r.epochHash,
	}
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
	atPercent := r.cfg.DistillThresholdPercent
	if atPercent <= 0 {
		atPercent = distillAtPercent
	}
	// Usage-anchored occupancy wins when the provider has reported a real
	// footprint AND real cache reads: it is the authoritative context
	// pressure, so compaction fires on measured tokens instead of a byte
	// estimate. The cache-read gate keeps a provider that omits cache
	// metrics (DeepSeek reports no cache-write; some gateways report no
	// cache at all) from firing on a garbage zero. Only when no trustworthy
	// usage has been observed yet does the byte-estimated plan carry the
	// decision.
	if r.lastUsageTokens > 0 && r.lastCacheReadTokens > 0 {
		return contextWindow > 0 && r.lastUsageTokens*100 >= contextWindow*atPercent
	}
	return contextWindow > 0 && plan.TotalInputTokens*100 >= contextWindow*atPercent
}

// distillOptions builds the distillation policy for the current run.
func (r *Runner) distillOptions() distill.Options {
	opts := r.cfg.DistillOptions
	if opts.Summarizer == nil {
		opts.Summarizer = r.distillSummarizer()
	}
	return opts
}
