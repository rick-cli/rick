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
//
// The summarizer receives the exact system/tools/messages of the live request
// (the "replayed prefix") so a provider-backed implementation can extend it
// with a trailing instruction instead of flattening to a bespoke transcript:
// the auxiliary call then shares the warm request's leading tokens and the
// provider serves the prefix from cache instead of re-billing it cold exactly
// when the conversation is largest.
type Summarizer interface {
	Summarize(ctx context.Context, input SummaryInput) (string, error)
}

// SummaryInput is the conversation prefix a summarizer replays. System, Tools,
// and Messages must reproduce the routed request's provider-facing bytes so a
// provider-backed summarizer can append a trailing instruction and reuse the
// cached prefix. Messages are the oldest stable region to be distilled (already
// redacted and bounded by the caller).
type SummaryInput struct {
	System   string
	Tools    []provider.ToolSchema
	Messages []provider.Message
}

// Options configures Distill.
type Options struct {
	// Summarizer performs the background LLM call. Nil disables distillation.
	Summarizer Summarizer
	// System is the routed request's system prompt, replayed verbatim to the
	// summarizer so a provider-backed one extends the warm prefix instead of
	// cold-starting a bespoke prompt.
	System string
	// Tools are the routed request's tool schemas, replayed verbatim for the
	// same reason (dropping them would misalign the cached token sequence).
	Tools []provider.ToolSchema
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
	// LiveZoneTokens, when positive, prices the retained live tail in tokens
	// (4 chars/token heuristic, harness-style token-meter): the fold boundary
	// is placed so the newest messages kept after the summary cost at most
	// this many tokens. A message-count-only ratio can keep a tail whose
	// token cost still spans the provider's cache-block granularity and cuts
	// a cached block; the token budget keeps the surviving tail inside the
	// cached region. Zero falls back to the ratio-based boundary.
	LiveZoneTokens int
	// DistillRatio is the share of the newest history left untouched: the
	// oldest (1-ratio) share is distilled. Default 0.5 (oldest half).
	DistillRatio float64
	// PlannedPrefixTokens is the estimated size (in provider cache blocks,
	// ~256 tokens each) of the cache prefix this request would still share
	// with the previous one if it were sent right now. It is the shadow
	// price of NOT distilling: when it is tiny, most of the context is
	// already cold and a distill fold can only help; when it is large, the
	// warm prefix covers most of the window and distillation would rewrite
	// bytes the provider would otherwise serve from cache. Zero disables the
	// planned-price check (distill on usage/bytes alone).
	PlannedPrefixTokens int
	// CacheBlockTokens is the provider's prompt-cache block granularity in
	// tokens. The summary insert point (cut) is aligned so the fold never
	// splits a provider cache block: the cut moves to the nearest boundary
	// whose serialized token offset is a whole cache-block multiple, so the
	// bytes the provider still has cached stay inside full blocks and the
	// next request re-reads them from cache instead of re-billing a partial
	// block cold. Zero uses the provider default (256).
	CacheBlockTokens int
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
	if o.CacheBlockTokens <= 0 {
		o.CacheBlockTokens = 256
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

// ErrPlannedPrefixStillWarm reports that the planned cache prefix is still
// large enough that folding would rewrite warm bytes for no gain.
var ErrPlannedPrefixStillWarm = errors.New("planned cache prefix still warm; distill deferred")

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
	// Cache-block alignment (harness-style): move the insert point to the
	// nearest message boundary whose serialized token offset is a whole
	// cache-block multiple. Splitting a provider cache block at the fold
	// re-bills a partial block cold on the very next request; aligning keeps
	// the surviving prefix inside full blocks so the provider serves it from
	// cache. Walk cut back (never forward past the breakpoint) to the
	// closest aligned boundary.
	cut = alignCutToCacheBlock(messages, cut, opts.CacheBlockTokens)

	candidate, end := distillablePrefix(messages, cut, opts.MaxMessages, opts.LiveZoneTurns, opts.LiveZoneTokens, opts.DistillRatio)
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

	// Shadow price: when the planned (estimated) cached prefix still covers
	// most of the window, folding would rewrite bytes the provider would
	// otherwise serve from cache. Only fold when the surviving cache prefix
	// is small enough that the rewrite is cheaper than the re-bill it avoids
	// (the summary itself becomes the new warm prefix). Zero disables.
	if opts.PlannedPrefixTokens > 0 && oldTokens < opts.PlannedPrefixTokens {
		return Result{Messages: messages, Err: ErrPlannedPrefixStillWarm}
	}
	if float64(oldTokens) < opts.MinRatio*float64(providerJSONBytesAll(messages)) {
		return Result{Messages: messages}
	}

	// Summarize first: a summarizer failure must not replace history. The
	// summarizer receives the full replayed prefix (system + tools + ALL
	// messages up to the fold point, including the cached head before the
	// breakpoint) so a provider-backed implementation appends a trailing
	// instruction and the auxiliary call is a genuine byte-prefix-extension
	// of the last routed request — the shared head rides the provider's warm
	// prefix cache instead of re-billing the whole conversation cold.
	replay := append([]provider.Message(nil), messages[:end]...)
	summary, err := opts.Summarizer.Summarize(context.Background(), SummaryInput{
		System:   opts.System,
		Tools:    opts.Tools,
		Messages: replay,
	})
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
func distillablePrefix(messages []provider.Message, cut, maxMessages, liveTurns, liveZoneTokens int, ratio float64) ([]provider.Message, int) {
	liveStart := len(messages)
	if groups := logicalGroups(messages); len(groups) > liveTurns {
		liveStart = groups[len(groups)-liveTurns].start
	}

	end := int(float64(len(messages)) * ratio)
	// Token-priced live tail (harness-style token-meter): when a live-zone
	// token budget is set it REPLACES the ratio boundary — the foldable
	// region ends so the newest messages retained after the summary cost at
	// least liveZoneTokens (4 chars/token heuristic, the harness's
	// selectCompactableRange floor: keep at least retainTokens of tail). The
	// message-count live zone still caps the fold so the newest liveTurns
	// are never distilled.
	if liveZoneTokens > 0 {
		end = tokenPricedFoldEnd(messages, liveZoneTokens)
	}
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

// tokenPricedFoldEnd walks the newest messages backward, pricing each at the
// harness token-meter density (4 chars/token plus framing overhead), until
// the accumulated token cost of the retained tail reaches liveZoneTokens —
// the harness's selectCompactableRange semantics: keep the newest messages
// summing to at least the retain budget verbatim. The returned index is the
// fold boundary (messages before it are distillable). Falls back to the
// whole-tail boundary when the budget exceeds the entire transcript.
func tokenPricedFoldEnd(messages []provider.Message, liveZoneTokens int) int {
	if liveZoneTokens <= 0 || len(messages) == 0 {
		return len(messages)
	}
	accumulated := 0
	boundary := len(messages) // first index NOT retained (foldable region ends here)
	for index := len(messages) - 1; index >= 0 && accumulated < liveZoneTokens; index-- {
		accumulated += 4 + priceMessageTokens(messages[index])
		boundary = index
	}
	if accumulated < liveZoneTokens {
		// The whole transcript is cheaper than the budget: nothing is
		// distillable by token price (the message-count live zone still
		// applies upstream).
		return len(messages)
	}
	return boundary
}

// alignCutToCacheBlock walks the fold insert point back toward the nearest
// message boundary whose serialized token offset (4 chars/token heuristic) is
// a whole multiple of the provider's cache block size. A cut that lands
// mid-block leaves a partially-written cache block at the fold; the provider
// re-bills that partial block cold on the next request even though most of
// the prefix is stable. The walk covers at most one block's residual and
// never moves the cut below 1 (the anchored first message after the system
// is never folded), so correctness of the fold is never traded for alignment.
func alignCutToCacheBlock(messages []provider.Message, cut, blockTokens int) int {
	if blockTokens <= 0 || cut <= 1 || cut > len(messages) {
		return cut
	}
	tokens := 0
	for i := 0; i < cut; i++ {
		tokens += priceMessageTokens(messages[i])
	}
	residual := tokens % blockTokens
	if residual == 0 {
		return cut
	}
	// Walk back exactly the residual (at most one block), never below cut=1.
	// When a single message costs more than the remaining residual, walking
	// further would overshoot past the aligned boundary (and past the
	// previous full block) — stop at the current position, which is the
	// nearest reachable one.
	for cut > 1 && residual > 0 {
		cost := priceMessageTokens(messages[cut-1])
		if cost > residual {
			break
		}
		cut--
		residual -= cost
	}
	return cut
}

// priceMessageTokens prices one message's text at the harness token-meter
// density: 4 chars per token plus a small structural overhead.
func priceMessageTokens(m provider.Message) int {
	total := 0
	for _, block := range m.Content {
		switch block.Type {
		case "text", "thinking":
			total += len(block.Text)/4 + 1
		case "tool_use":
			total += len(block.Name)/4 + len(block.Input)/4 + 1
		case "tool_result":
			total += len(block.Content)/4 + 1
		default:
			total += len(block.Text)/4 + 1
		}
	}
	return total
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
