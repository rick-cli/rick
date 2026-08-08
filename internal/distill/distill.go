package distill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"rick/internal/provider"
)

// Summarizer produces a distilled summary of an old conversation prefix. It is
// normally backed by a fast provider round-trip; tests use a stub.
type Summarizer interface {
	Summarize(ctx context.Context, messages []provider.Message) (string, error)
}

// Options configures Distill.
type Options struct {
	// Summarizer performs the background LLM call. Nil disables distillation.
	Summarizer Summarizer
	// MaxMessages caps the number of messages distilled in one step.
	MaxMessages int
	// MaxSummaryBytes bounds the serialized summary message.
	MaxSummaryBytes int
	// MinHistoryTokens is the smallest token cost of the distillable prefix
	// that makes distillation worthwhile.
	MinHistoryTokens int
	// MinRatio requires the old prefix to hold at least this share of the
	// full history before it is replaced.
	MinRatio float64
	// MinLiveRatio is the maximum share of the distilled region's size that
	// the summary may occupy. A summary larger than this is not a saving.
	MinLiveRatio float64
	// MinCacheBreakBytes requires the cache breakpoint prefix to be at least
	// this large before the summary is inserted after it.
	MinCacheBreakBytes int
	// LiveZoneTurns is how many newest logical turns are never distilled.
	LiveZoneTurns int
	// DistillRatio is the share of the newest history left untouched: the
	// oldest (1-ratio) share is distilled. Default 0.5 (oldest half).
	DistillRatio float64
}

func (o Options) withDefaults() Options {
	if o.MaxMessages <= 0 {
		o.MaxMessages = 32
	}
	if o.MaxSummaryBytes <= 0 {
		o.MaxSummaryBytes = 2 << 10
	}
	if o.MinHistoryTokens <= 0 {
		o.MinHistoryTokens = 2000
	}
	if o.MinRatio <= 0 {
		o.MinRatio = 0.4
	}
	if o.MinLiveRatio <= 0 {
		o.MinLiveRatio = 0.5
	}
	if o.MinCacheBreakBytes <= 0 {
		o.MinCacheBreakBytes = 2 << 10
	}
	if o.LiveZoneTurns <= 0 {
		o.LiveZoneTurns = 1
	}
	if o.DistillRatio <= 0 {
		o.DistillRatio = 0.4
	}
	return o
}

// Result describes one distillation.
type Result struct {
	Replaced     bool
	Messages     []provider.Message
	OmittedCount int
	BeforeBytes  int
	AfterBytes   int
	Err          error
}

var errCacheBreakNotStable = errors.New("cache breakpoint prefix is not stable")
var errSummarizerFailed = errors.New("summarizer failed")

// Distill collapses the oldest portion of history into a single structured
// summary message placed just after the cache breakpoint. It never splits a
// tool_use/tool_result pair, never crosses the live zone, and only replaces
// history when the summarizer is available and the replacement is a real
// saving. Distillation is best-effort: every failure mode returns the input
// unchanged with Err set.
func Distill(messages []provider.Message, breakpoints map[int]bool, opts Options) Result {
	opts = opts.withDefaults()
	if opts.Summarizer == nil || len(messages) < 3 {
		return Result{Messages: messages}
	}

	// The summary is anchored just after the oldest stable cache prefix. The
	// cached prefix stays intact (it is the provider cache's stable region);
	// everything after it up to the middle of the chat is distilled away.
	cut := len(messages)
	for index := range breakpoints {
		if index > 0 && index < cut {
			cut = index
		}
	}
	if cut >= len(messages) {
		return Result{Messages: messages, Err: errCacheBreakNotStable}
	}

	candidate, end := distillablePrefix(messages, cut, opts.MaxMessages, opts.LiveZoneTurns, opts.DistillRatio)
	if candidate == nil {
		return Result{Messages: messages}
	}

	oldTokens := 0
	for _, m := range candidate {
		oldTokens += providerJSONBytes(m)
	}
	if oldTokens < opts.MinHistoryTokens {
		return Result{Messages: messages}
	}
	if float64(oldTokens) < opts.MinRatio*float64(providerJSONBytesAll(messages)) {
		return Result{Messages: messages}
	}

	// Summarize first: a summarizer failure must not replace history.
	summary, err := opts.Summarizer.Summarize(context.Background(), candidate)
	if err != nil {
		return Result{Messages: messages, Err: fmt.Errorf("%w: %v", errSummarizerFailed, err)}
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return Result{Messages: messages, Err: errSummarizerFailed}
	}
	summary = capSummary(summary, opts.MaxSummaryBytes)

	// The rest of the chat (the newer half) is non-negotiable.
	live := append([]provider.Message(nil), messages[end:]...)

	// The cached prefix must be big enough to justify inserting after it.
	if serializeBytes(messages[:cut]) < opts.MinCacheBreakBytes {
		return Result{Messages: messages}
	}

	summaryMessage := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.ContentBlock{provider.TextBlock(summary)},
	}
	// The summary must be a real saving over the messages it replaces:
	// smaller than MinLiveRatio of the distilled region's size.
	distilledBytes := serializeBytes(messages[cut:end])
	summaryBytes := serializeBytes([]provider.Message{summaryMessage})
	if float64(summaryBytes) >= opts.MinLiveRatio*float64(distilledBytes) {
		return Result{Messages: messages}
	}

	replaced := make([]provider.Message, 0, len(messages[:cut])+1+len(live))
	replaced = append(replaced, messages[:cut]...)
	replaced = append(replaced, summaryMessage)
	replaced = append(replaced, live...)

	beforeBytes := serializeBytes(messages)
	afterBytes := serializeBytes(replaced)
	if afterBytes >= beforeBytes {
		return Result{Messages: messages}
	}

	return Result{
		Replaced:     true,
		Messages:     replaced,
		OmittedCount: len(candidate),
		BeforeBytes:  beforeBytes,
		AfterBytes:   afterBytes,
	}
}

// distillablePrefix returns the oldest messages eligible for distillation:
// the slice after the cache breakpoint up to the middle of the chat (clamped
// to the live zone). It always includes complete tool_use/tool_result pairs.
// The second return is the end index.
func distillablePrefix(messages []provider.Message, cut, maxMessages, liveTurns int, ratio float64) ([]provider.Message, int) {
	liveStart := len(messages)
	if groups := logicalGroups(messages); len(groups) > liveTurns {
		liveStart = groups[len(groups)-liveTurns].start
	}

	end := int(float64(len(messages)) * ratio)
	if end < cut {
		return nil, end
	}
	if end > liveStart {
		end = liveStart
	}

	// Grow from cut to end, but never split an atomic tool_use/tool_result
	// pair and never exceed maxMessages.
	end = walkPairs(messages, cut, end, maxMessages)
	if end <= cut {
		return nil, end
	}
	return messages[cut:end], end
}

// walkPairs advances end from start toward limit, always including complete
// tool pairs, and returns the final end index.
func walkPairs(messages []provider.Message, start, limit, maxMessages int) int {
	end := start
	for end < limit && end < len(messages) && end-start < maxMessages {
		if hasBlock(messages[end], "tool_use") && end+1 < len(messages) && hasBlock(messages[end+1], "tool_result") {
			end += 2
			continue
		}
		end++
	}
	return end
}

// logicalGroups groups messages so a tool_use and its tool_result count as one
// logical turn.
func logicalGroups(messages []provider.Message) []group {
	groups := make([]group, 0, len(messages))
	for index := 0; index < len(messages); index++ {
		start := index
		if hasBlock(messages[index], "tool_use") && index+1 < len(messages) && hasBlock(messages[index+1], "tool_result") {
			index++
		}
		groups = append(groups, group{start: start})
	}
	return groups
}

type group struct{ start int }

func hasBlock(message provider.Message, kind string) bool {
	for _, block := range message.Content {
		if block.Type == kind {
			return true
		}
	}
	return false
}

func providerJSONBytes(m provider.Message) int {
	raw, err := json.Marshal(m)
	if err != nil {
		return len(m.Text())
	}
	return len(raw)
}

func providerJSONBytesAll(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += providerJSONBytes(m)
	}
	return total
}

func serializeBytes(msgs []provider.Message) int {
	raw, err := json.Marshal(msgs)
	if err != nil {
		return providerJSONBytesAll(msgs)
	}
	return len(raw)
}

func capSummary(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	marker := "\n… (summary truncated)"
	if len(marker) >= maxBytes {
		return marker
	}
	keep := maxBytes - len(marker)
	for keep > 0 && !utf8RuneStart(text[keep]) {
		keep--
	}
	return text[:keep] + marker
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
