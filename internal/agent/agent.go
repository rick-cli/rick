// Package agent implements the tool-calling loop: prompt -> model -> tool
// calls -> results -> model, until the model returns plain text.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"rick/internal/budget"
	"rick/internal/compress"
	"rick/internal/config"
	"rick/internal/distill"
	"rick/internal/goal"
	"rick/internal/history"
	"rick/internal/permission"
	"rick/internal/plugin"
	"rick/internal/provider"
	"rick/internal/session"
	"rick/internal/tokens"
	"rick/internal/tools"
	"rick/pkg/contextbudget"
	"rick/pkg/repomap"
)

// EventKind enumerates agent-level events surfaced to the UI.
type EventKind int

// Agent event kinds.
const (
	EvText            EventKind = iota // assistant text delta
	EvThinking                         // reasoning delta
	EvToolStart                        // tool execution began
	EvToolEnd                          // tool execution finished
	EvPermissionAsk                    // waiting on the user
	EvTurnEnd                          // one model turn completed
	EvUsage                            // token accounting
	EvDone                             // whole run finished
	EvError                            // fatal error
	EvAgentBackground                  // background agent started
	EvAgentReattached                  // result surfaced to a parent
	EvAgentMessage                     // live chat or steering message injected
	EvCacheDivergence                  // provider-prefix byte divergence vs the previous turn
	EvCacheBoundary                    // deliberate cache boundary committed (prune/distill/compact)
)

// ToolEvent describes a tool execution.
type ToolEvent struct {
	CallID       string
	Name         string
	Title        string
	Input        json.RawMessage
	Output       string
	Meta         map[string]any
	IsError      bool
	Elapsed      time.Duration
	Optimization *OptimizationStats
	// Repaired describes any tool-call repair applied to this call's args
	// ("" when none). The note is also appended to Output so the model sees
	// it; this field carries it separately for the TUI and telemetry.
	Repaired string
}

// OptimizationStats describes the provider-facing reduction for one tool
// result. Original tool output remains in ToolEvent.Output unchanged.
type OptimizationStats struct {
	Stage            string
	Fallback         bool
	OriginalBytes    int
	CompressedBytes  int
	OriginalTokens   int
	CompressedTokens int
	SavedTokens      int
	Truncated        bool
}

// CacheDivergence describes where the provider-facing prefix of this request
// stopped matching the previous request's bytes — the event that forces a
// full or partial re-bill. Kind is "system", "tools", or "message"; Index is
// the first divergent message position (Kind "message"), else -1. Reason is
// the best-effort cause inferred from the runner's own transforms.
// CachedPrefixTokens is the estimated size (in provider cache blocks, ~256
// tokens each, rounded down) of the prefix this request still shares with the
// previous one — the part the provider will serve from cache — so telemetry
// can distinguish a total cold re-bill from a partial one.
type CacheDivergence struct {
	Kind               string `json:"kind,omitempty"`
	Index              int    `json:"index,omitempty"`
	Reason             string `json:"reason,omitempty"`
	CachedPrefixTokens int    `json:"cached_prefix_tokens,omitempty"`
}

// Event is one item on the agent's output stream.
type Event struct {
	Kind       EventKind
	Text       string
	Tool       *ToolEvent
	Usage      *provider.Usage
	Divergence *CacheDivergence
	Boundary   *CacheBoundary
	// ReasoningTokens is the token size of the reasoning echo sent with this
	// request; it rides the EvUsage event so telemetry rows can measure the
	// deep-reasoning fresh-tail cost per request.
	ReasoningTokens int
	Err             error
}

// CacheBoundary describes one deliberate cache-boundary commit (or deferral)
// on a turn: which transform fired (prune, distill, compact, reasoning-cut),
// what provider-facing bytes it reclaimed, and the shadow price that was
// considered. Persisted per request so the economics of each deliberate
// invalidation are auditable.
type CacheBoundary struct {
	Transform string `json:"transform"`
	// Committed is true when the rewrite was applied, false when deferred.
	Committed bool `json:"committed"`
	// Reason is why the boundary committed or was deferred.
	Reason string `json:"reason,omitempty"`
	// SavedBytes is the provider-facing byte reduction when committed.
	SavedBytes int `json:"saved_bytes,omitempty"`
	// ShadowPriceTokens is the estimated still-warm prefix the rewrite would
	// invalidate (0 when no prior view existed).
	ShadowPriceTokens int `json:"shadow_price_tokens,omitempty"`
	// Originals is the durable per-node shadow-price ledger of a tool-prune
	// commit: the content address and sizes of every replaced tool result,
	// so the session can persist the reclaim and keep the original
	// replayable from the content-addressed store (harness-style
	// compaction/prune event).
	Originals []contextbudget.PruneOriginal `json:"originals,omitempty"`
}

// PermissionDecision is the user's answer to an approval prompt.
type PermissionDecision int

// Decisions.
const (
	DecideReject PermissionDecision = iota
	DecideAccept
	DecideAlways
)

// PermissionAsker prompts the user. Implementations must be safe to call from
// a goroutine and should block until the user answers or ctx is cancelled.
type PermissionAsker func(ctx context.Context, req permission.Request) PermissionDecision

// Snapshotter captures file state before a mutating turn (undo support).
type Snapshotter interface {
	Snapshot(label string) (string, error)
}

// Config wires an agent run.
type Config struct {
	Provider     provider.Provider
	Model        string
	System       string
	SystemStable string
	MaxTokens    int
	// ContextWindow overrides provider/model discovery when positive. A zero
	// value uses provider metadata and then the conservative budget fallback.
	ContextWindow      int
	SafetyMarginTokens int
	TokenEncoding      tokens.Encoding
	Temperature        *float64
	Reasoning          provider.ReasoningEffort
	// Budget is the shared session context manager (content-addressed dedup,
	// cache boundaries, reversible live-zone compression). When nil the runner
	// creates a private one that still deduplicates and picks boundaries but
	// never replaces tool output without a reversible store.
	Budget *contextbudget.Budget
	// RepoMapRoot enables the RepoMap structural skeleton in the system
	// prompt when non-empty.
	RepoMapRoot string
	// RepoMapBlock is a precomputed RepoMap block. When non-empty it is used
	// verbatim on every turn, so a long-lived session keeps a byte-identical
	// system prompt and the provider prompt cache is never disturbed. When
	// empty, the runner builds its own map once per run.
	RepoMapBlock string
	// RepoMapMaxTokens bounds the RepoMap block; zero means the package
	// default (1024).
	RepoMapMaxTokens int
	// EnableDistillation turns on state distillation when the transcript
	// approaches the context budget. Requires a provider (for the default
	// summarizer) or an injected DistillSummarizer.
	EnableDistillation bool
	// DistillModel is the fast model used for the background summary call.
	// Empty falls back to the primary model.
	DistillModel string
	// DistillThresholdPercent overrides the share of the context window at
	// which the oldest stable prefix folds into a summary (0 = package
	// default 55). Resolved from the per-model policy table by the caller.
	DistillThresholdPercent int
	// DistillSummarizer overrides the background summarizer; tests inject a
	// stub here.
	DistillSummarizer distill.Summarizer
	// DistillOptions tunes the distillation policy. A zero value applies the
	// package defaults; the Summarizer field is filled from
	// DistillSummarizer or the primary provider when left nil.
	DistillOptions distill.Options
	Tools          tools.ToolSet
	ToolFilter     func(string) bool
	Perms          *permission.Engine
	Ask            PermissionAsker
	Cwd            string
	SandboxRoot    string
	SessionID      string
	AgentName      string
	Depth          int
	MaxTurns       int // cap on agent turns; <= 0 means unlimited
	Snapshotter    Snapshotter
	Parallel       bool // allow concurrent read-only tools
	Plugins        *plugin.Registry
	Goals          *goal.Store
	Creds          *config.Credentials // for key rotation on rate-limit
	Registry       *Registry           // optional live hierarchy registry
	AgentID        string              // registry ID for this run
	// CacheRetention is the prompt-cache policy for every request of this
	// run: "" = provider default, "long" = extended TTL, "none" = disabled.
	CacheRetention provider.CacheRetention
	// CacheStrategy, when set, is the pluggable prompt-cache policy for the
	// whole run. It overrides the flat cache knobs below (CacheRetention,
	// CacheTTLSeconds, WarmCache, WarmTurn, PassbackReasoning,
	// MaxReasoningTurns, and the divergence reason). Leave nil to use the
	// flat knobs via provider.DefaultStrategy.
	CacheStrategy provider.CacheStrategy
	// CacheTTLSeconds overrides the provider's assumed prompt-cache lifetime
	// for idle-gap pre-warm decisions. Zero uses the per-vendor table.
	CacheTTLSeconds int
	// WarmCache, when true and the provider supports provider.CacheWarmber,
	// submits a small warm request once at session start so the provider
	// populates its prompt cache for the frozen prefix before the first real
	// turn. A cache-warm provider call in this package must remain optional;
	// a provider that doesn't implement CacheWarmber is skipped.
	WarmCache bool
	// WarmTurn, when true, primes the full provider-facing prefix before
	// every real turn (not just session start). Providers whose cache
	// evicts mid-session never pay a full uncached re-bill; the cost is one
	// cheap non-streaming request per turn. Requires WarmCache to be
	// enabled in the runner (WarmTurn only changes how often it fires).
	WarmTurn bool
	// PassbackReasoning, when true, drops reasoning_content from
	// tool-call-free turns on DeepSeek-line providers. The cached prefix is
	// never rewritten (the drop lands at the append-only tail), but the
	// fresh-tail cost of long reasoning chains shrinks. Defaults to false
	// (keep every reasoning block for a byte-identical prefix).
	PassbackReasoning bool
	// CacheScopeKey, when set, is used as the prompt-cache key scope for
	// every request of this run, replacing the session id. Non-interactive
	// runs (cron, rickserve one-shots, CI) each mint a fresh session id that
	// would cold-write the provider prefix every run; deriving the scope
	// from the stable prompt content instead lets identical runs share a
	// warm bucket while separate conversations never collide.
	CacheScopeKey string
	// PriorEpoch, when set, is the durable request-header identity of a
	// resumed session (session.EpochHeader). The runner verifies its own
	// freshly built header against it: a drift (repo-map block changed with
	// cwd, model switched, tools changed) is detected up front and surfaced
	// as a divergence event so the resume re-primes instead of silently
	// cold-starting a new cache bucket mid-session.
	PriorEpoch *session.EpochHeader
	// MaxReasoningTurns caps the prior-turn reasoning echoed back to
	// DeepSeek-line providers (0 = keep all, byte-stable prefix).
	MaxReasoningTurns int
	// MaxToolResultBytes caps each tool_result payload sent to the model
	// (0 = default 16 KiB) so a single turn's fresh tail stays small and the
	// provider prefix cache stays hot.
	MaxToolResultBytes int
	// SpillBytes, when positive, is the tool-result size above which the
	// full output is persisted to the content-addressed store and the model
	// sees a bounded preview plus retrieval locator (spill). 0 disables.
	SpillBytes int
	// PinnedToolSchemas fixes the provider-facing tool list for the whole
	// run, so mid-session tool toggles or plugin churn never change the
	// cached prefix bytes. When nil the registry + ToolFilter are used.
	PinnedToolSchemas []provider.ToolSchema
	// ArchiveDir, when non-empty, is where trimmed/dropped originals are
	// written as JSONL (one line per message) so folded context stays
	// traceable without ever touching the provider-facing view bytes.
	ArchiveDir string
}

// Runner executes the loop.
type Runner struct {
	cfg Config

	// budget is the shared session context manager, or a private fallback.
	budget *contextbudget.Budget

	// repairFamily is the model-family gate for tool-call repair quirks
	// ("deepseek", "glm", "qwen", ...), derived once from cfg.Model.
	repairFamily string

	// repoMapOnce/repoMapBlock compute the RepoMap once per run using the
	// active chat prompt; the block is byte-identical across turns so it does
	// not disturb the provider prompt cache.
	repoMapOnce  sync.Once
	repoMapBlock string

	// Stable-head trimming state: once the conversation first exceeds the
	// context budget, the oldest logical groups are pinned behind a
	// byte-stable sentinel so the provider view only ever grows at the tail
	// (the provider prefix cache stays warm) instead of silently dropping
	// from the front every turn.
	trimEngaged bool
	trimStart   int
	trimHead    provider.Message

	// systemOnce/pinnedSystem freeze the full volatile system block (env +
	// RepoMap + tool manifest) after the first request, so the system prompt
	// bytes can never drift between turns and cold-start the prefix cache.
	systemOnce   sync.Once
	pinnedSystem string

	// schemasOnce/frozenSchemas freeze the provider-facing tool schema list
	// once per run (the request-header half of the cache prefix). A mid-run
	// registry change cannot rewrite the cached prefix bytes; the frozen
	// list is what the epoch hash is derived from.
	schemasOnce    sync.Once
	frozenSchemas  []provider.ToolSchema

	// epochHash is the content-addressed identity of this run's frozen
	// header (model + stable system prefix + canonical tool list), computed
	// once from the first built request. It routes the prompt-cache key and
	// session-affinity hints, and survives resume: a restarted session with
	// an identical header derives the same hash and rides the same warm
	// bucket (mirrors the harness's request-header epoch). The runner
	// exposes it on every request so the provider never sees a different
	// cache scope across turns.
	epochHash     string
	epochHashOnce sync.Once

	// Prefix-divergence tracking: hashes of the last sent view, used to
	// detect (and attribute) any byte change before the previous tail.
	prevSystemHash string
	prevToolsHash  string
	prevMsgHashes  []string
	// pendingBoundary carries the deliberate cache-boundary decision made
	// while building the current request (prune/distill commit or shadow-
	// price deferral). It rides the EvUsage event so telemetry rows can
	// audit each deliberate invalidation.
	pendingBoundary *CacheBoundary
	// lastMutation names the runner's own transform that fired on the
	// current turn ("head-trim", "distill"), to attribute divergences.
	lastMutation string

	// Reasoning-cap one-shot state (Phase C2): when
	// cfg.MaxReasoningTurns > 0 the stale deep-reasoning echo is stripped
	// exactly once at the first request and then byte-pinned, so the prefix
	// changes once and stays append-only instead of rotating a moving window
	// that re-bills the tail every turn.
	reasoningCutSet   bool
	reasoningCutIndex int
	// lastReasoningTokens is the most recent request's client-side reasoning
	// echo size; it rides the EvUsage event for per-request telemetry.
	lastReasoningTokens int
	// lastRequest is when the most recent provider request (stream or warm)
	// was dispatched, used to spot idle gaps past the provider cache TTL so
	// the next turn can re-prime the full prefix before streaming (P1c).
	lastRequest time.Time
	// gapBeforeTurn is the idle gap between the previous dispatch and the
	// current turn's dispatch. When a provider eviction is observed on this
	// turn's usage, that gap is the cause; recording it lets the warm
	// threshold adapt to the endpoint's real eviction point.
	gapBeforeTurn time.Duration
	// observedEvictionGap is the shortest idle gap that has provably
	// evicted the provider's prefix cache this run (0 = none observed).
	// cacheTTL() tightens the warm threshold to just below it, so a
	// provider that evicts at minutes-scale (free flash tiers) gets re-warmed
	// before the eviction point instead of at the vendor table's day-long
	// assumption.
	observedEvictionGap time.Duration
	// lastViewBytes is the serialized size of the provider view sent last
	// turn; the per-turn growth feeds the prune-rearm accumulator so a
	// disarmed prune commits again only after the history regrows a
	// trigger-sized runway.
	lastViewBytes int
	// lastUsageTokens is the provider-reported footprint of the previous
	// request (input + cache read + cache write), the usage-anchored
	// baseline for budget/distillation decisions. It is authoritative where
	// byte estimates are not: a provider that reports cache reads knows the
	// real cached region, so compaction fires on measured occupancy instead
	// of a heuristic byte estimate.
	lastUsageTokens int
	// lastCacheReadTokens is the provider-reported cache-read (hit) tokens
	// of the previous request. A provider that reports zero cache reads may
	// simply omit the field (DeepSeek reports no cache-write metric; some
	// gateways report no cache at all), so usage-anchored distillation must
	// only trust occupancy when cache reads were actually reported — a
	// garbage zero would otherwise fire distillation on turn one.
	lastCacheReadTokens int
	// lastPlannedPrefixTokens is the most recent pre-flight estimate of how
	// many provider cache-block tokens this request still shares with the
	// previous one (see cachePrefixTokens). It is the shadow price of a
	// distill fold: the estimated cost of the warm prefix that a fold would
	// rewrite. Zero means "no prior view to compare" (first request).
	lastPlannedPrefixTokens int
	// usageRing is a bounded ring of the most recent per-turn usage records
	// (prompt footprint + cache-read tokens), used to infer the eviction
	// point of a partial (not total) prefix-cache drop. DeepSeek-style
	// bounded-LRU caches evict in waves: a turn that reads far less than
	// the previous turn sent, but more than zero, marks where the eviction
	// cut — the re-warm must fire just before that point on the next gap.
	usageRing   [usageRingDepth]usageSample
	usageRingAt int
	usageRingN  int
	// warmErrWarned dedupes warming-failure notices to one per distinct
	// error message per run, so a broken warm is surfaced without spamming.
	warmErrWarned string
}

// usageSample is one turn's provider-reported prompt accounting.
type usageSample struct {
	prompt     int // input + cache read + cache write
	cacheRead  int // cache-read (hit) tokens
	gapBefore  time.Duration
	reasoning  int // client-side reasoning echo size this request
}

// usageRingDepth bounds the per-turn usage ring used for eviction-point
// inference (the last few turns are enough to spot a wave).
const usageRingDepth = 8

// New builds a Runner.
func New(cfg Config) *Runner {
	if cfg.SandboxRoot == "" && cfg.Perms != nil {
		cfg.SandboxRoot = cfg.Perms.SandboxRoot()
	}
	budget := cfg.Budget
	if budget == nil {
		budget = contextbudget.New(contextbudget.Options{})
	}
	return &Runner{cfg: cfg, budget: budget, repairFamily: tools.FamilyForModel(cfg.Model)}
}

// Cfg exposes the runner configuration (read-only use).
func (r *Runner) Cfg() Config { return r.cfg }

// Run drives the loop until the model stops requesting tools.
//
// history is the conversation so far; it is NOT mutated. The appended messages
// produced during the run are returned so the caller can persist them.
// The runner owns out and closes it exactly once.
func (r *Runner) Run(ctx context.Context, history []provider.Message, out chan<- Event) (appended []provider.Message, runErr error) {
	defer close(out)
	if r.cfg.Registry != nil && r.cfg.AgentID != "" {
		r.cfg.Registry.Update(r.cfg.AgentID, AgentRunning, "", nil)
		defer func() {
			status := AgentDone
			if runErr != nil {
				status = AgentFailed
				if ctx.Err() != nil {
					status = AgentKilled
				}
			}
			output := ""
			if runErr == nil {
				for i := len(appended) - 1; i >= 0; i-- {
					for _, block := range appended[i].Content {
						if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
							output = block.Text
							break
						}
					}
					if output != "" {
						break
					}
				}
			}
			r.cfg.Registry.Update(r.cfg.AgentID, status, output, runErr)
		}()
	}

	emit := func(ev Event) bool {
		if r.cfg.Registry != nil && r.cfg.AgentID != "" {
			r.cfg.Registry.Publish(r.cfg.AgentID, ev)
		}
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	msgs := append([]provider.Message(nil), history...)
	var lastErr error
	lastCallBatch := ""
	repeatedCallCount := 0

	schemas := r.cfg.PinnedToolSchemas
	if len(schemas) == 0 {
		schemas = r.cfg.Tools.Schemas(r.cfg.ToolFilter)
	}
	// Structural freeze (harness-style): the tool schema list and the system
	// prompt are the request-header half of the cache prefix. Freeze them
	// once at run start so a mid-run registry change (plugin churn, /tools
	// toggle, subagent re-registration) can never silently rewrite the
	// cached prefix bytes. The epoch hash is derived from this frozen
	// snapshot, so any drift would change the cache bucket — fail closed
	// rather than cold-start every later turn.
	schemas = r.freezeSchemas(schemas)

	// Resume header-drift guard (harness-style request/header event): when
	// this run resumed a session whose durable header no longer matches the
	// bytes the next request will send (repo-map block changed with cwd,
	// model switched, tools changed), surface it as a divergence once before
	// the first turn so the caller can re-warm — never silently pay a cold
	// re-bill under a stale cache scope. The drift also latches the P1c
	// re-warm (see the warm gate below): the new header must be primed
	// before the first real turn streams, even when general warming is off,
	// because the drift itself invalidated the prefix the session had warm.
	var resumeDrifted bool
	if prior := r.checkPriorEpoch(r.systemBlock(msgs, schemas), r.cfg.SystemStable, schemas); prior != "" {
		resumeDrifted = true
		emit(Event{Kind: EvCacheDivergence, Divergence: &CacheDivergence{
			Kind:   "epoch",
			Index:  -1,
			Reason: "resume-header-drift:" + prior,
		}})
	}

	// P1: session-start prompt-cache warm. Submit a small best-effort request
	// so the provider populates its cache for the frozen system+tools prefix
	// before the first real turn. Only providers that implement
	// provider.CacheWarmber are warmed; failures are never fatal.
	if r.cfg.WarmCache || (r.cfg.CacheStrategy != nil && r.cfg.CacheStrategy.WarmCache()) {
		if warmer, ok := r.cfg.Provider.(provider.CacheWarmber); ok {
			warmReq := r.buildRequest(msgs, schemas)
			// D1: prime only the stable prefix. The system prompt + tool
			// list are frozen per run, so warming that head is byte-exact
			// and costs a handful of tokens instead of the whole transcript;
			// the volatile message tail differs every turn anyway. Keep a
			// single head message so OpenAI-style endpoints accept a
			// non-empty messages array.
			warmReq.Messages = stableWarmHead(warmReq.Messages)
			warmReq.CacheBoundaries = nil
			warmReq.Purpose = provider.PurposeWarm
			if warmReq.CacheRetention == "" {
				warmReq.CacheRetention = provider.CacheRetentionLong
			}
			// A silent warm failure would leave the whole first turn cold
			// without the user knowing why; surface it once per run.
			if err := warmer.Warm(ctx, warmReq); err != nil {
				r.warnWarmOnce(emit, err)
			}
			r.lastRequest = time.Now()
		}
	}

	// MaxTurns caps the loop; <= 0 disables the cap so long tasks can run to
	// completion. Pathological loops are still caught by the repeated-call
	// guard below and by the goal token budget.
	//
	// cacheEvicted latches a provider prefix eviction observed on the previous
	// turn (an idle gap outliving the cache TTL) so the next turn re-primes
	// before it pays a full uncached re-bill.
	var (
		cacheEvicted  bool
		prevPrompt    int
		prevCacheRead int
	)
	for turn := 0; r.cfg.MaxTurns <= 0 || turn < r.cfg.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return appended, ctx.Err()
		}
		r.lastMutation = ""
		r.injectControlMessages(&msgs, &appended, emit)

		// Lifecycle hook: turn start.
		if r.cfg.Plugins != nil && r.cfg.Plugins.Len() > 0 {
			pluginErrs := r.cfg.Plugins.DispatchTurnStart(ctx, &plugin.TurnStartEvent{
				SessionID: r.cfg.SessionID, Agent: r.cfg.AgentName, TurnNumber: turn,
			})
			for _, pluginErr := range pluginErrs {
				emit(Event{Kind: EvAgentMessage, Text: "plugin hook error: " + pluginErr.Error()})
			}
		}

		req := r.buildRequest(msgs, schemas)
		r.lastReasoningTokens = countThinkingTokens(req.Messages, r.requestEncoding())
		if !r.lastRequest.IsZero() {
			r.gapBeforeTurn = time.Since(r.lastRequest)
		} else {
			r.gapBeforeTurn = 0
		}
		// Surface any deliberate cache-boundary decision this request's
		// build made (prune/distill commit or shadow-price deferral) before
		// the stream starts, so the TUI and telemetry can audit it.
		if r.pendingBoundary != nil {
			boundary := r.pendingBoundary
			r.pendingBoundary = nil
			if !emit(Event{Kind: EvCacheBoundary, Boundary: boundary}) {
				return appended, ctx.Err()
			}
		}
		if div := r.trackPrefix(req); div != nil {
			if !emit(Event{Kind: EvCacheDivergence, Divergence: div}) {
				return appended, ctx.Err()
			}
			// Pre-send structural invariant (harness-style): within one run
			// the provider view may only ever grow at the tail. A mid-prefix
			// rewrite that none of the runner's declared transforms accounts
			// for is a cache-correctness bug — the provider would re-bill the
			// whole tail cold and every later turn would too. Fail closed
			// instead of silently paying the re-bill. The deliberate
			// at-most-once rewrites (head-trim, distill, reasoning-cut,
			// tool-prune, dedup, previous compaction summary) are already
			// re-warmed by P1c below and are allowed through.
			if div.Reason == "unexpected" {
				at := ""
				if div.Index >= 0 {
					at = fmt.Sprintf(" at message %d", div.Index)
				}
				err := fmt.Errorf("agent: provider view diverged mid-prefix with no declared transform (%s%s); refusing to re-bill the cache cold", div.Kind, at)
				emit(Event{Kind: EvError, Err: err})
				return appended, err
			}
		}

		// P1c: full-prefix re-warm before this request. The provider's
		// prefix cache is expected to miss this turn when a previous turn
		// observed an eviction (P1b), the view head was just rewritten
		// (head-trim), a long transcript was resumed (turn 0 with a tail), or
		// the gap since the last request outlived the cache TTL. The warm
		// carries the exact bytes this stream will send, so the stream reads
		// the prefix back from cache instead of re-billing it cold. Errors
		// are surfaced once instead of swallowed.
		//
		// WarmTurn primes before every real turn: providers whose cache evicts
		// mid-session (short-TTL free tiers) never pay a full uncached re-bill.
		//
		// A head-trim rewrites the provider-facing prefix by definition (the
		// trim sentinel replaces the dropped head), so the next request is a
		// guaranteed cold re-bill unless the new prefix is primed first. That
		// warm is mandatory even when general warming is disabled — it is the
		// only way to avoid a re-bill the trim itself caused, and it costs
		// one small request per trim (trims are rare, once per long session).
		if warmer, ok := r.cfg.Provider.(provider.CacheWarmber); ok {
			strategy := r.cacheStrategy()
			// A partial eviction (bounded-LRU wave) that dropped a mid-prefix
			// region without zeroing the whole cache: re-warm before this
			// request so the stream re-reads the surviving prefix instead of
			// re-billing the dropped wave cold.
			partialEvicted := r.inferredEvictionPoint() > 0
			warmNeeded := strings.HasPrefix(r.lastMutation, "head-trim") ||
				resumeDrifted ||
				(strategy.WarmCache() && (strategy.WarmTurn() ||
					cacheEvicted ||
					partialEvicted ||
					(turn == 0 && len(req.Messages) > 1) ||
					(!r.lastRequest.IsZero() && time.Since(r.lastRequest) > r.cacheTTL())))
			if warmNeeded {
				warmReq := req
				warmReq.Purpose = provider.PurposeWarm
				if warmReq.CacheRetention == "" {
					warmReq.CacheRetention = provider.CacheRetentionLong
				}
				if err := warmer.Warm(ctx, warmReq); err != nil {
					r.warnWarmOnce(emit, err)
				} else {
					r.warmErrWarned = ""
				}
				r.lastRequest = time.Now()
			}
		}
		cacheEvicted = false

		ch := make(chan provider.Event, 256)
		streamCtx, cancelStream := context.WithCancel(ctx)
		r.lastRequest = time.Now()
		// A panic inside the provider's stream goroutine would otherwise
		// crash the whole process and (in headless/cachehit callers) skip
		// Stream's `defer close(ch)`, leaving the event loop blocked forever.
		// Recover it and surface an EvError so the turn fails cleanly.
		// Closing the channel on the panic path is best-effort: a provider
		// that panicked before its own `defer close(ch)` ran leaves the
		// channel open (the runner's range would block), so we close it; a
		// provider that panicked after closing (e.g. in a later defer) is
		// already closed, and a double close is swallowed by the nested
		// recover.
		var closeOnce sync.Once
		closeCh := func() { closeOnce.Do(func() { close(ch) }) }
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					func() {
						defer func() { _ = recover() }()
						closeCh()
					}()
					// The runner may already have returned and closed `out`;
					// a send to a closed channel panics, so swallow that
					// here rather than crash the process.
					defer func() { _ = recover() }()
					emit(Event{Kind: EvError, Err: fmt.Errorf("provider stream panicked: %v", rec)})
				}
			}()
			r.cfg.Provider.Stream(streamCtx, req, ch)
		}()

		var (
			textBuf    strings.Builder
			thinkBuf   strings.Builder
			calls      []provider.ToolCall
			streamErr  error
			stopReason string
			streamDone bool
		)

	streamEvents:
		for ev := range ch {
			switch ev.Kind {
			case provider.EventText:
				textBuf.WriteString(ev.Text)
				if !emit(Event{Kind: EvText, Text: ev.Text}) {
					cancelStream()
					go drain(ch)
					return appended, ctx.Err()
				}
			case provider.EventThinking:
				thinkBuf.WriteString(ev.Text)
				if !emit(Event{Kind: EvThinking, Text: ev.Text}) {
					cancelStream()
					go drain(ch)
					return appended, ctx.Err()
				}
			case provider.EventToolCall:
				if ev.ToolCall != nil {
					calls = append(calls, *ev.ToolCall)
				}
			case provider.EventUsage:
				if ev.Usage != nil {
					emit(Event{Kind: EvUsage, Usage: ev.Usage, ReasoningTokens: r.lastReasoningTokens})
					// Detect a prefix eviction for the mid-session re-warm
					// (P1b): if the first turn read a large cached prefix but
					// this turn reads far less than what was previously sent,
					// the provider dropped the prefix cache (idle-gap TTL).
					// Latched so the next request re-primes before streaming.
					prompt := ev.Usage.InputTokens + ev.Usage.CacheReadTokens + ev.Usage.CacheWriteTokens
					if prevCacheRead > 0 && prompt > 0 &&
						ev.Usage.CacheReadTokens < min(prevPrompt, prompt)-cacheMissNoiseFloor {
						cacheEvicted = true
						// Learn the endpoint's real eviction point: the idle
						// gap before this turn is what caused the provider to
						// drop the prefix. The shortest provable eviction gap
						// tightens cacheTTL() so the next idle-gap re-warm
						// fires before the eviction instead of after it.
						if r.gapBeforeTurn > 0 && (r.observedEvictionGap == 0 || r.gapBeforeTurn < r.observedEvictionGap) {
							r.observedEvictionGap = r.gapBeforeTurn
						}
					}
					prevPrompt = prompt
					if ev.Usage.CacheReadTokens+ev.Usage.CacheWriteTokens > 0 {
						prevCacheRead = ev.Usage.CacheReadTokens + ev.Usage.CacheWriteTokens
					}
					// Usage-anchored baseline: the provider's own footprint (miss
					// + hit + write) is the authoritative occupancy for the next
					// compaction decision. Byte estimates are only a proxy; a
					// provider that reports cache reads knows the real cached
					// region, so shouldDistill can fire on measured occupancy.
					r.lastUsageTokens = prompt
					r.lastCacheReadTokens = ev.Usage.CacheReadTokens
					// Ring the per-turn usage so a partial eviction (a wave
					// that drops a mid-prefix region but not the whole cache)
					// can be inferred from the last few turns' read/prompt
					// ratio, not just the immediately previous one.
					r.recordUsageSample(usageSample{
						prompt:    prompt,
						cacheRead: ev.Usage.CacheReadTokens,
						gapBefore: r.gapBeforeTurn,
						reasoning: r.lastReasoningTokens,
					})
					// Enforce the active goal's token budget, if any.
					if r.cfg.Goals != nil {
						if g, _ := r.cfg.Goals.GetActive(); g != nil && g.Status == "active" {
							// Providers report InputTokens as uncached input
							// and cache reads/writes disjointly, so summing
							// all four counts every token once.
							total := ev.Usage.InputTokens + ev.Usage.OutputTokens +
								ev.Usage.CacheReadTokens + ev.Usage.CacheWriteTokens
							_ = r.cfg.Goals.AddTokens(g.ID, total)
							if g2, err := r.cfg.Goals.Load(g.ID); err == nil {
								if ok, _ := goal.CheckBudget(g2); !ok {
									budgetErr := fmt.Errorf("goal token budget exhausted")
									emit(Event{Kind: EvError, Err: budgetErr})
									cancelStream()
									drain(ch)
									return appended, budgetErr
								}
							}
						}
					}
				}
			case provider.EventDone:
				streamDone = true
				stopReason = ev.StopReason
				break streamEvents
			case provider.EventError:
				if ev.Err == nil {
					ev.Err = fmt.Errorf("provider returned an unspecified error")
				}
				streamErr = ev.Err
				break streamEvents
			}
		}
		cancelStream()

		if streamErr != nil {
			// Check for rate-limit / quota errors and rotate keys if configured.
			if r.cfg.Provider != nil && r.cfg.Creds != nil && isRateLimitError(streamErr) {
				provID, _ := config.SplitModel(r.cfg.Model)
				key, rotateErr := rotateKeyForProviderWithCreds(r.cfg.Creds, provID)
				if rotateErr != nil {
					rotationErr := fmt.Errorf("rate-limit key rotation failed: %w", rotateErr)
					emit(Event{Kind: EvError, Err: rotationErr})
					return appended, rotationErr
				}
				if key != "" {
					keySetter, ok := r.cfg.Provider.(interface{ SetAPIKey(string) })
					if !ok {
						rotationErr := fmt.Errorf("provider %q cannot accept rotated API keys", r.cfg.Provider.Name())
						emit(Event{Kind: EvError, Err: rotationErr})
						return appended, rotationErr
					}
					keySetter.SetAPIKey(key)
					emit(Event{Kind: EvAgentMessage, Text: "rate limited; retrying with next key"})
					continue
				}
			}
			emit(Event{Kind: EvError, Err: streamErr})
			return appended, streamErr
		}
		if !streamDone {
			streamErr = fmt.Errorf("agent: provider stream ended without a completion event")
			emit(Event{Kind: EvError, Err: streamErr})
			return appended, streamErr
		}
		if strings.TrimSpace(textBuf.String()) == "" && strings.TrimSpace(thinkBuf.String()) == "" && len(calls) == 0 {
			streamErr = fmt.Errorf("agent: provider returned an empty completion")
			emit(Event{Kind: EvError, Err: streamErr})
			return appended, streamErr
		}
		seenCallIDs := make(map[string]struct{}, len(calls))
		for index := range calls {
			call := &calls[index]
			if strings.TrimSpace(call.Name) == "" {
				streamErr = fmt.Errorf("agent: malformed tool call: missing function name")
				emit(Event{Kind: EvError, Err: streamErr})
				return appended, streamErr
			}
			input := strings.TrimSpace(string(call.Input))
			if !json.Valid(call.Input) {
				streamErr = fmt.Errorf("agent: malformed arguments for tool %q", call.Name)
				emit(Event{Kind: EvError, Err: streamErr})
				return appended, streamErr
			}
			if input == "" || input[0] != '{' {
				streamErr = fmt.Errorf("agent: arguments for tool %q must be a JSON object", call.Name)
				emit(Event{Kind: EvError, Err: streamErr})
				return appended, streamErr
			}
			if call.ID == "" {
				call.ID = fmt.Sprintf("rick-tool-%d-%d", time.Now().UnixNano(), index)
			}
			if _, duplicate := seenCallIDs[call.ID]; duplicate {
				streamErr = fmt.Errorf("agent: duplicate tool call ID %q", call.ID)
				emit(Event{Kind: EvError, Err: streamErr})
				return appended, streamErr
			}
			seenCallIDs[call.ID] = struct{}{}
		}
		assistant := provider.Message{Role: provider.RoleAssistant}
		if thinkBuf.Len() > 0 {
			assistant.Content = append(assistant.Content, provider.ContentBlock{
				Type: "thinking", Text: thinkBuf.String(),
			})
		}
		if strings.TrimSpace(textBuf.String()) != "" {
			assistant.Content = append(assistant.Content, provider.TextBlock(textBuf.String()))
		}
		for _, c := range calls {
			assistant.Content = append(assistant.Content, provider.ContentBlock{
				Type: "tool_use", ID: c.ID, Name: c.Name, Input: c.Input,
			})
		}
		if len(assistant.Content) > 0 {
			msgs = append(msgs, assistant)
			appended = append(appended, assistant)
		}

		emit(Event{Kind: EvTurnEnd, Text: stopReason})

		// Lifecycle hook: turn end.
		if r.cfg.Plugins != nil && r.cfg.Plugins.Len() > 0 {
			pluginErrs := r.cfg.Plugins.DispatchTurnEnd(ctx, &plugin.TurnEndEvent{
				SessionID: r.cfg.SessionID, Agent: r.cfg.AgentName,
				TurnNumber: turn, StopReason: stopReason,
			})
			for _, pluginErr := range pluginErrs {
				emit(Event{Kind: EvAgentMessage, Text: "plugin hook error: " + pluginErr.Error()})
			}
		}

		if len(calls) == 0 {
			emit(Event{Kind: EvDone})
			return appended, nil
		}

		batchKey := ""
		for _, c := range calls {
			batchKey += c.Name + "\x00" + canonicalToolInput(c.Input) + "\x01"
		}
		if batchKey == lastCallBatch {
			repeatedCallCount++
		} else {
			lastCallBatch = batchKey
			repeatedCallCount = 1
		}
		if repeatedCallCount > 2 {
			message := "agent: repeated tool call limit reached"
			if len(calls) == 1 {
				message = fmt.Sprintf("agent: repeated tool call limit reached for %s", calls[0].Name)
			}
			err := errors.New(message)
			emit(Event{Kind: EvError, Err: err})
			return appended, err
		}
		results := r.execTools(ctx, calls, emit)
		if ctx.Err() != nil {
			return appended, ctx.Err()
		}

		userMsg := provider.Message{Role: provider.RoleUser, Content: results}
		msgs = append(msgs, userMsg)
		appended = append(appended, userMsg)
	}

	lastErr = fmt.Errorf("agent: stopped after %d turns without a final answer (max-turns reached; set --max-turns 0 for unlimited)", r.cfg.MaxTurns)
	emit(Event{Kind: EvError, Err: lastErr})
	return appended, lastErr
}

func (r *Runner) injectControlMessages(msgs *[]provider.Message, appended *[]provider.Message, emit func(Event) bool) {
	if r.cfg.Registry == nil || r.cfg.AgentID == "" {
		return
	}
	input, ok := r.cfg.Registry.Input(r.cfg.AgentID)
	if !ok {
		return
	}
	for {
		select {
		case msg := <-input:
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			prefix := "Message"
			if msg.Steering {
				prefix = "Steering instruction"
			}
			text := fmt.Sprintf("[%s from %s]\n%s", prefix, msg.From, msg.Content)
			block := provider.TextBlock(text)
			message := provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{block}}
			*msgs = append(*msgs, message)
			*appended = append(*appended, message)
			emit(Event{Kind: EvAgentMessage, Text: text})
		default:
			return
		}
	}
}

func drain(ch <-chan provider.Event) {
	for range ch {
	}
}

// isRateLimitError reports whether err looks like a rate-limit or quota error.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "rate-limit") ||
		(strings.Contains(s, "limit") && strings.Contains(s, "exceeded")) ||
		strings.Contains(s, "too many requests")
}

// rotateKeyForProviderWithCreds rotates the key for the given provider using the credentials store.
// Returns the new key, or "" if rotation is not possible.
func rotateKeyForProviderWithCreds(creds *config.Credentials, provID string) (string, error) {
	if creds == nil {
		return "", nil
	}
	return creds.RotateKeyAndSave(provID)
}

// execTools runs a batch of tool calls, honouring permissions and executing
// read-only tools concurrently when enabled.
func (r *Runner) execTools(ctx context.Context, calls []provider.ToolCall, emit func(Event) bool) []provider.ContentBlock {
	results := make([]provider.ContentBlock, len(calls))
	events := make([]*ToolEvent, len(calls))
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("rick-tool-%d-%d", time.Now().UnixNano(), i)
		}
	}

	// Partition: read-only calls can run in parallel; the rest run in order.
	parallelIdx := []int{}
	serialIdx := []int{}
	for i, c := range calls {
		t, ok := r.cfg.Tools.Get(c.Name)
		if r.cfg.Parallel && ok && t.ReadOnly() {
			parallelIdx = append(parallelIdx, i)
		} else {
			serialIdx = append(serialIdx, i)
		}
	}
	if r.cfg.Snapshotter != nil {
		for _, i := range serialIdx {
			t, ok := r.cfg.Tools.Get(calls[i].Name)
			if ok && !t.ReadOnly() {
				if _, snapErr := r.cfg.Snapshotter.Snapshot(calls[i].Name); snapErr != nil {
					// A failed snapshot silently breaks the undo promise
					// (the user's file state was not captured), so surface
					// one warning instead of letting the agent keep mutating
					// as if nothing happened.
					emit(Event{Kind: EvAgentMessage,
						Text: "warning: file snapshot failed before " + calls[i].Name +
							" — undo may be unavailable: " + snapErr.Error()})
				}
				break
			}
		}
	}

	emitStart := func(i int) {
		emit(Event{Kind: EvToolStart, Tool: &ToolEvent{
			CallID: calls[i].ID, Name: calls[i].Name, Title: calls[i].Name, Input: calls[i].Input,
		}})
	}
	emitEnd := func(i int) {
		if events[i] != nil {
			emit(Event{Kind: EvToolEnd, Tool: events[i]})
		}
	}

	run := func(i int) {
		res, ev := r.execOne(ctx, calls[i])
		results[i] = res
		events[i] = ev
	}

	if len(parallelIdx) > 1 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, i := range parallelIdx {
			wg.Add(1)
			emitStart(i)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				run(i)
			}(i)
		}
		wg.Wait()
		for _, i := range parallelIdx {
			emitEnd(i)
		}
	} else {
		for _, i := range parallelIdx {
			emitStart(i)
			run(i)
			emitEnd(i)
		}
	}
	for _, i := range serialIdx {
		if ctx.Err() != nil {
			results[i] = provider.ToolResultBlock(calls[i].ID, "cancelled by user", true)
			continue
		}
		emitStart(i)
		run(i)
		emitEnd(i)
	}

	return results
}

func (r *Runner) buildRequest(messages []provider.Message, schemas []provider.ToolSchema) provider.Request {
	encoding := r.cfg.TokenEncoding
	if encoding == "" {
		encoding = tokens.EncodingForModel(r.cfg.Model)
	}

	contextWindow := r.cfg.ContextWindow
	if contextWindow <= 0 && r.cfg.Provider != nil {
		contextWindow = provider.KnownProviderContextWindow(r.cfg.Provider.Name(), r.cfg.Model)
	}
	reservedOutput := r.cfg.MaxTokens
	if reservedOutput <= 0 {
		reservedOutput = 4096
	}

	system := r.systemBlock(messages, schemas)
	stableSystem := r.cfg.SystemStable
	volatileSystem := system
	if stableSystem != "" && strings.HasPrefix(volatileSystem, stableSystem) {
		volatileSystem = strings.TrimPrefix(volatileSystem, stableSystem)
	}
	// The budget is planned against the capped view (not the raw transcript):
	// when a one-shot reasoning cap strips stale thinking, the wire only
	// ships the newest turn's reasoning, so charging all of it for the trim
	// decision would reserve budget for bytes that never reach the provider
	// and fire distillation prematurely.
	view := r.cappedMessages(messages, encoding)
	if r.budget.Enabled() {
		view = r.budget.ApplyDedup(view).View
	}
	plan := budget.Plan(budget.Input{
		ContextWindow:        contextWindow,
		StableSystemTokens:   countTokens(stableSystem, encoding),
		VolatileSystemTokens: countTokens(volatileSystem, encoding),
		ToolSchemaTokens:     countJSONValues(schemas, encoding),
		MessageTokens:        countMessages(view, encoding),
		ReservedOutputTokens: reservedOutput,
		SafetyMarginTokens:   r.cfg.SafetyMarginTokens,
	})

	retained := r.retainStable(view, plan.RetainedMessageTokens, encoding)
	boundaries := r.budget.ChooseBoundaries(retained)

	// Proactive tool-result pruning: deterministically shrink old bulky tool
	// results into 1-line summaries (P0-2/P1-2). The commit is gated on the
	// measured reclaim and rearmed by growth, so it fires episodically —
	// one cache boundary per commit — instead of rewriting every turn. The
	// growth signal is the view's byte growth since the last sent view.
	// Distillation supersedes it: when the transcript is close enough to the
	// budget that the oldest prefix will be collapsed into a summary anyway,
	// let that single deliberate rewrite happen instead of also churning the
	// head with per-result summaries.
	if r.budget.Enabled() && len(retained) > 0 && !r.shouldDistill(plan, contextWindow) {
		viewBytes := serializeViewBytes(retained)
		if r.lastViewBytes > 0 && viewBytes > r.lastViewBytes {
			r.budget.NotePruneGrowth(viewBytes - r.lastViewBytes)
		}
		if pruned := r.budget.PruneOldToolResults(retained); pruned.Committed {
			retained = pruned.View
			boundaries = r.budget.ChooseBoundaries(retained)
			r.lastMutation = "tool-prune"
			r.pendingBoundary = &CacheBoundary{
				Transform:  "tool-prune",
				Committed:  true,
				Reason:     "reclaim-gated episodic prune",
				SavedBytes: pruned.SavedBytes,
				// Durable per-node shadow-price ledger: each replaced tool
				// result's content address + sizes ride the boundary so the
				// session persists the reclaim and the original stays
				// replayable from the content-addressed store.
				Originals: append([]contextbudget.PruneOriginal(nil), pruned.Originals...),
			}
			// A prune rewrites the old head; reset the stable-head sentinel
			// so the new head stays fixed and the view resumes append-only
			// growth (same contract as distill).
			r.trimEngaged = false
			r.trimStart = 0
			r.trimHead = provider.Message{}
		}
		r.lastViewBytes = serializeViewBytes(retained)
	}

	// State distillation: when the transcript approaches the context budget,
	// collapse the oldest stable prefix into a structured summary placed just
	// after the cache breakpoint. Best-effort: every failure keeps the view.
	// The summarizer receives this request's system+tools so its auxiliary
	// call replays the exact cached prefix (see providerSummarizer).
	if r.shouldDistill(plan, contextWindow) {
		opts := r.distillOptions()
		opts.System = system
		opts.Tools = schemas
		// Shadow price: the planned cache prefix this request would still
		// share with the previous one. When it covers more than the region
		// being folded, deferring avoids rewriting warm bytes for no gain.
		opts.PlannedPrefixTokens = r.lastPlannedPrefixTokens
		result := distill.Distill(retained, boundaries, opts)
		if result.Replaced {
			retained = result.Messages
			boundaries = r.budget.ChooseBoundaries(retained)
			r.lastMutation = "distill"
			r.pendingBoundary = &CacheBoundary{
				Transform:         "distill",
				Committed:         true,
				Reason:            "budget-threshold fold",
				SavedBytes:        result.BeforeBytes - result.AfterBytes,
				ShadowPriceTokens: r.lastPlannedPrefixTokens,
			}
			// Distillation rebuilds the head (the one deliberate, whole-prefix
			// cache invalidation). Reset the stable-head sentinel so the new
			// head stays fixed and the view resumes append-only growth.
			r.trimEngaged = false
			r.trimStart = 0
			r.trimHead = provider.Message{}
		} else if result.Err != nil && errors.Is(result.Err, distill.ErrPlannedPrefixStillWarm) {
			// Shadow-price deferral is a deliberate non-event worth
			// recording: the warm prefix outweighed the fold, so the
			// session kept its bytes. Telemetry can verify the shadow
			// price saved a re-bill instead of guessing.
			r.pendingBoundary = &CacheBoundary{
				Transform:         "distill",
				Committed:         false,
				Reason:            "planned prefix still warm",
				ShadowPriceTokens: r.lastPlannedPrefixTokens,
			}
		}
	}

	strategy := r.cacheStrategy()
	return provider.Request{
		Model:             r.cfg.Model,
		System:            system,
		SystemStable:      r.cfg.SystemStable,
		Messages:          retained,
		Tools:             schemas,
		MaxTokens:         r.cfg.MaxTokens,
		Temperature:       r.cfg.Temperature,
		Reasoning:         r.cfg.Reasoning,
		CacheBoundaries:   boundaries,
		CacheRetention:    strategy.Retention(),
		MaxReasoningTurns: r.wireReasoningTurns(),
		PassbackReasoning: r.passbackReasoning(),
		SessionID:         r.cfg.SessionID,
		CacheScopeKey:     r.cacheScopeKey(),
		EpochHash:         r.runEpochHash(system, r.cfg.SystemStable, schemas),
	}
}

// passbackReasoning resolves the DeepSeek reasoning-passback rule for the
// run: the strategy wins when set; otherwise the flat cfg knob.
func (r *Runner) passbackReasoning() bool {
	if r.cfg.CacheStrategy != nil {
		return r.cfg.CacheStrategy.PassbackReasoning()
	}
	return r.cfg.PassbackReasoning
}

// cacheScopeKey returns the prompt-cache routing scope for this run. An
// explicitly configured CacheScopeKey (non-interactive runs) always wins.
// Parallel agents (subagents, swarm workers) share the parent SessionID and
// a byte-stable system head, so without a partition every concurrent agent
// would pile onto one hot prompt_cache_key. OpenAI recommends keeping each
// key near ~15 requests/minute — a swarm or /task burst of 4+ agents can
// blow past that and force cold misses. Deriving the scope from session +
// agent identity spreads concurrent agents across routing keys while each
// agent stays stable across its own turns (same scope for every request of
// that agent), so per-agent prefixes still hit.
func (r *Runner) cacheScopeKey() string {
	if r.cfg.CacheScopeKey != "" {
		return r.cfg.CacheScopeKey
	}
	if r.cfg.Parallel && r.cfg.SessionID != "" {
		identity := r.cfg.AgentID
		if identity == "" {
			identity = r.cfg.AgentName
		}
		if identity != "" {
			return r.cfg.SessionID + "\x00agent:" + identity
		}
	}
	return ""
}

// runEpochHash returns the content-addressed identity of this run's frozen
// request header: the model, the byte-stable system prefix, and the
// canonical tool list. It is computed once from the first built request and
// pinned for the whole run, so every turn (and every warm/keep-alive replay)
// routes to the same provider cache bucket. A resumed session that derives
// the identical header reuses the same hash and rides the warm prefix instead
// of cold-writing a fresh bucket under a new session id.
func (r *Runner) runEpochHash(system, stable string, schemas []provider.ToolSchema) string {
	r.epochHashOnce.Do(func() {
		stableSystem := stable
		if stableSystem == "" {
			stableSystem = system
		}
		r.epochHash = CacheScopeKeyFor(r.cfg.Model, stableSystem, schemas)
	})
	return r.epochHash
}

// epochHeader returns the durable provider-request header identity of this
// run (harness-style request/header event): the frozen model, system prompt
// bytes, canonical tool list, and the derived epoch hash. It is persisted on
// the session at save time so a resumed session can prove its header is
// byte-identical (or detect drift) instead of recomputing from config.
func (r *Runner) epochHeader(system, stable string, schemas []provider.ToolSchema) session.EpochHeader {
	return session.EpochHeader{
		Model:        r.cfg.Model,
		System:       system,
		SystemStable: stable,
		Tools:        append([]provider.ToolSchema(nil), schemas...),
		Hash:         r.runEpochHash(system, stable, schemas),
	}
}

// checkPriorEpoch verifies a resumed session's durable header against this
// run's freshly built one. A drift — the repo-map block changed with cwd, the
// model switched, the tool list changed — means the provider prefix this
// session last warmed no longer matches the bytes the next request will send;
// the provider would re-bill the whole tail cold. Detecting it up front (and
// letting the caller re-warm) is strictly better than silently building a
// different request and paying the miss. Returns the divergence reason, or ""
// when the headers match or no prior header exists.
func (r *Runner) checkPriorEpoch(system, stable string, schemas []provider.ToolSchema) string {
	prior := r.cfg.PriorEpoch
	if prior == nil || prior.Hash == "" {
		return ""
	}
	current := r.epochHeader(system, stable, schemas)
	if current.Hash == prior.Hash {
		return ""
	}
	// Named cause: which header field drifted.
	switch {
	case prior.Model != "" && current.Model != prior.Model:
		return "model:" + prior.Model + "->" + current.Model
	case prior.System != "" && current.System != prior.System:
		return "system"
	case prior.SystemStable != "" && current.SystemStable != prior.SystemStable:
		return "system-stable"
	case len(prior.Tools) != len(current.Tools):
		return "tools:" + itoa(len(prior.Tools)) + "->" + itoa(len(current.Tools))
	default:
		return "header-drift"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// wireReasoningTurns is what the provider adapter sees. The one-shot cap
// already rewritten the message list structurally (cappedMessages), so the
// wire must retain everything it now holds: a rotating strip would slide the
// kept window every turn and re-bill the provider cache. 0 = the adapter's
// retain-all behaviour.
func (r *Runner) wireReasoningTurns() int {
	max := r.cfg.MaxReasoningTurns
	if r.cfg.CacheStrategy != nil {
		max = r.cfg.CacheStrategy.MaxReasoningTurns()
	}
	if max > 0 {
		return 0
	}
	return max
}

// cappedMessages returns the provider-visible message list after applying the
// optional one-shot deep-reasoning cap. When cfg.MaxReasoningTurns > 0, stale
// reasoning echo is stripped exactly once at the first request and then
// byte-pinned: the prefix changes once and afterwards only grows, so the
// provider cache is never re-billed by a rotating reasoning window.
func (r *Runner) cappedMessages(messages []provider.Message, encoding tokens.Encoding) []provider.Message {
	max := r.cfg.MaxReasoningTurns
	if r.cfg.CacheStrategy != nil {
		max = r.cfg.CacheStrategy.MaxReasoningTurns()
	}
	if max <= 0 {
		return messages
	}
	if !r.reasoningCutSet {
		r.reasoningCutSet = true
		r.reasoningCutIndex = reasoningCutIndex(messages, max)
		r.lastMutation = "reasoning-cut"
	}
	cut := r.reasoningCutIndex
	if cut <= 0 {
		return messages
	}
	out := append([]provider.Message(nil), messages...)
	for i := 0; i < cut && i < len(out); i++ {
		if !containsBlock(messages[i], "thinking") {
			continue
		}
		replaced := messages[i]
		replaced.Content = stripThinking(messages[i].Content)
		out[i] = replaced
	}
	return out
}

// stripThinking returns the content blocks without their "thinking" blocks.
func stripThinking(blocks []provider.ContentBlock) []provider.ContentBlock {
	kept := make([]provider.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "thinking" {
			continue
		}
		kept = append(kept, block)
	}
	return kept
}

// reasoningCutIndex returns the first message index at which the one-shot
// reasoning cap starts stripping: everything strictly before the oldest of the
// newest `keep` assistant reasoning turns loses its thinking blocks. It
// returns 0 when there are at most `keep` reasoning turns (nothing to strip).
func reasoningCutIndex(messages []provider.Message, keep int) int {
	if keep <= 0 {
		return 0
	}
	var thinking []int
	for i := 0; i < len(messages); i++ {
		if !containsBlock(messages[i], "thinking") {
			continue
		}
		thinking = append(thinking, i)
	}
	if len(thinking) <= keep {
		return 0
	}
	return thinking[len(thinking)-keep]
}

// countThinkingTokens sums the token size of every deep-reasoning "thinking"
// block across the view, i.e. the client-side reasoning echo that rides the
// fresh tail of each request.
func countThinkingTokens(messages []provider.Message, encoding tokens.Encoding) int {
	total := 0
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "thinking" {
				total += countTokens(block.Text, encoding)
			}
		}
	}
	return total
}

// systemBlock returns the full system prompt for the run, frozen after the
// first build: env block, Repo map, and tool manifest are computed once so
// their bytes can never drift between turns and cold-start the prefix cache.
// freezeSchemas returns the frozen provider-facing tool schema list for the
// run. The first call pins the schemas; every later call returns the same
// byte-identical slice, so the epoch hash and the wire prefix never see a
// mid-run schema change.
func (r *Runner) freezeSchemas(schemas []provider.ToolSchema) []provider.ToolSchema {
	r.schemasOnce.Do(func() {
		r.frozenSchemas = append([]provider.ToolSchema(nil), schemas...)
	})
	return r.frozenSchemas
}

func (r *Runner) systemBlock(messages []provider.Message, schemas []provider.ToolSchema) string {
	r.systemOnce.Do(func() {
		system := r.cfg.System
		if block := r.repoMap(lastUserText(messages)); block != "" {
			system += "\n\n" + block
		}
		if manifest := toolManifest(schemas); manifest != "" {
			system += "\n\n" + manifest
		}
		r.pinnedSystem = system
	})
	return r.pinnedSystem
}

// stableWarmHead keeps only the oldest message of a warm request: the stable
// prologue that every later request repeats verbatim. Empty input keeps
// system+tools alone.
func stableWarmHead(messages []provider.Message) []provider.Message {
	if len(messages) <= 1 {
		return messages
	}
	return messages[:1]
}

// trackPrefix compares this request's serialized view with the previous
// one and reports the first byte that failed to match before the previous
// tail. A nil result means the prefix is append-only (cache kept warm).
func (r *Runner) trackPrefix(req provider.Request) *CacheDivergence {
	sysHash := hashBytes([]byte(req.System))
	toolsHash := hashBytes(marshalBytes(req.Tools))

	if r.prevSystemHash != "" || len(r.prevMsgHashes) > 0 {
		// Snapshot the previous view before capturePrefix refreshes it, so
		// the pre-flight estimate compares against the bytes the provider
		// actually has cached.
		prevSystemHash, prevToolsHash, prevMsgHashes := r.prevSystemHash, r.prevToolsHash, r.prevMsgHashes
		d := &CacheDivergence{Index: -1}
		switch {
		case sysHash != prevSystemHash:
			d.Kind = "system"
			d.Reason = "system-prompt"
		case toolsHash != prevToolsHash:
			d.Kind = "tools"
			d.Reason = "tool-schema"
		default:
			cur := make([]string, len(req.Messages))
			for i := range req.Messages {
				cur[i] = hashBytes(marshalBytes(req.Messages[i]))
			}
			mismatch := -1
			for i := 0; i < len(cur) && i < len(prevMsgHashes); i++ {
				if cur[i] != prevMsgHashes[i] {
					mismatch = i
					break
				}
			}
			switch {
			case mismatch >= 0:
				d.Kind = "message"
				d.Index = mismatch
				d.Reason = r.inferReason(req, mismatch)
			case len(cur) < len(prevMsgHashes):
				d.Kind = "message"
				d.Index = len(cur)
				d.Reason = r.inferReason(req, -1)
			}
		}
		if d.Kind != "" {
			// Pre-flight estimate of what still hits: the common prefix in
			// provider cache blocks.
			d.CachedPrefixTokens = cachePrefixTokens(req, prevSystemHash, prevToolsHash, prevMsgHashes, r.requestEncoding())
			r.lastPlannedPrefixTokens = d.CachedPrefixTokens
		}
		// Refresh the previous view regardless of the outcome.
		r.capturePrefix(sysHash, toolsHash, &req)
		if d.Kind == "" {
			// No divergence: the whole stable head still matches, so the
			// planned prefix is the shared bytes (system + tools + common
			// messages), floored to cache blocks. Record it for the shadow
			// price even though no divergence event is emitted.
			r.lastPlannedPrefixTokens = cachePrefixTokens(req, prevSystemHash, prevToolsHash, prevMsgHashes, r.requestEncoding())
			return nil
		}
		return d
	}
	r.capturePrefix(sysHash, toolsHash, &req)
	return nil
}

// cachePrefixTokens estimates how many tokens of this request's provider
// prefix will still be served from cache, given the previous request's
// serialized hashes. The estimate counts the byte-identical system + tool
// schema + message prefix (the parts that survived) and floors it to the
// provider's cache-block granularity (~256 tokens), because DeepSeek-line
// caches round down to the last full block. A zero result means the rewrite
// sits at or before the first byte of the shared head (a total cold re-bill).
func cachePrefixTokens(req provider.Request, prevSystemHash, prevToolsHash string, prevMsgHashes []string, encoding tokens.Encoding) int {
	if len(req.Messages) == 0 && req.System == "" && len(req.Tools) == 0 {
		return 0
	}
	// System and tools are stable for a run; a mismatch there kills the whole
	// head before the first message token.
	if prevSystemHash != "" && hashBytes(marshalBytes([]byte(req.System))) != prevSystemHash {
		return 0
	}
	if prevToolsHash != "" && hashBytes(marshalBytes(req.Tools)) != prevToolsHash {
		return 0
	}
	prefixTokens := countTokens(req.System, encoding) + countJSONValues(req.Tools, encoding)
	for i := 0; i < len(req.Messages) && i < len(prevMsgHashes); i++ {
		if hashBytes(marshalBytes(req.Messages[i])) != prevMsgHashes[i] {
			break
		}
		prefixTokens += countMessages(req.Messages[i:i+1], encoding)
	}
	return (prefixTokens / cacheBlockTokens) * cacheBlockTokens
}

// cacheBlockTokens is the provider's prompt-cache block granularity (~256
// tokens, rounded down to the last full block per deepseek-harness probe_5).
const cacheBlockTokens = 256

// CachePrefixWarnTokens is the minimum cached prefix (in tokens) whose loss
// is worth surfacing as a warning on a divergence event. Below it the
// re-bill is small enough to ignore in telemetry.
const CachePrefixWarnTokens = 4 << 10

func (r *Runner) capturePrefix(sysHash, toolsHash string, req *provider.Request) {
	r.prevSystemHash = sysHash
	r.prevToolsHash = toolsHash
	hashes := make([]string, len(req.Messages))
	for i := range req.Messages {
		hashes[i] = hashBytes(marshalBytes(req.Messages[i]))
	}
	r.prevMsgHashes = hashes
}

// inferReason attributes a divergence to the runner's own one-shot
// transforms, or to visible content produced by them. Anything else is a
// fail-closed "unexpected" — a mid-prefix rewrite we cannot account for, so
// it shows up in telemetry/regression instead of silently re-billing the
// provider cache. The deliberate, at-most-once rewrites the runner is
// allowed to make are the head-trim sentinel, the one-shot distill fold,
// and the one-shot reasoning cap cut.
func (r *Runner) inferReason(req provider.Request, index int) string {
	if r.cfg.CacheStrategy != nil {
		if reason := r.cfg.CacheStrategy.DivergenceReason(); reason != "" {
			return reason
		}
	}
	switch {
	case r.lastMutation == "head-trim" || strings.HasPrefix(r.lastMutation, "head-trim+"):
		return "head-trim"
	case r.lastMutation == "distill" || r.lastMutation == "reasoning-cut":
		return r.lastMutation
	}
	if index >= 0 && index < len(req.Messages) {
		text := ""
		for _, block := range req.Messages[index].Content {
			if block.Type == "text" {
				text = block.Text
				break
			}
			if block.Type == "tool_result" {
				text = block.Content
				break
			}
		}
		switch {
		case strings.HasPrefix(text, "[duplicate payload sha256:"):
			return "dedup"
		case strings.HasPrefix(text, "[Internal: the earliest"):
			return "head-trim"
		case strings.HasPrefix(text, "Summary of the conversation so far:") ||
			strings.HasPrefix(text, "Earlier conversation compacted:"):
			return "compact-summary"
		}
	}
	if r.lastMutation == "" {
		return "unexpected"
	}
	return r.lastMutation
}

func hashBytes(b []byte) string {
	return contextbudget.Hash(string(b))
}

// CacheScopeKeyFor derives a content-addressed prompt-cache scope for a
// non-interactive run: a digest of the model, stable system prefix, and
// canonical tool list. Repeated runs with identical prompts (cron,
// rickserve one-shots, CI) share a warm provider bucket, while separate
// conversations never collide. Interactive sessions keep using the session
// id instead (see provider.Request.CacheScopeKey).
func CacheScopeKeyFor(model, stableSystem string, tools []provider.ToolSchema) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		model,
		stableSystem,
		string(marshalBytes(provider.CanonicalToolSchemas(tools))),
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func marshalBytes(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// repoMap renders the repository skeleton once per run, keyed to the active
// chat prompt. The result is stable for the whole run so the provider cache
// sees an identical system suffix on every turn.
func (r *Runner) repoMap(prompt string) string {
	if r.cfg.RepoMapRoot == "" && r.cfg.RepoMapBlock == "" {
		return ""
	}
	// A precomputed block (built once per session by the caller) is used
	// verbatim so every turn sends a byte-identical system suffix and the
	// provider prompt cache stays warm.
	if r.cfg.RepoMapBlock != "" {
		return r.cfg.RepoMapBlock
	}
	r.repoMapOnce.Do(func() {
		block, err := repomap.Build(repomap.Options{
			Root:      r.cfg.RepoMapRoot,
			Prompt:    prompt,
			MaxTokens: r.cfg.RepoMapMaxTokens,
			Encoding:  tokens.EncodingForModel(r.cfg.Model),
		})
		if err == nil {
			r.repoMapBlock = block
		}
	})
	return r.repoMapBlock
}

// lastUserText returns the most recent plain-text user request, which is the
// "active chat prompt" used to weight the RepoMap.
func lastUserText(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != provider.RoleUser {
			continue
		}
		for _, block := range messages[i].Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				return block.Text
			}
		}
	}
	return ""
}

// requestEncoding resolves the tokenizer for this run, matching buildRequest.
func (r *Runner) requestEncoding() tokens.Encoding {
	if r.cfg.TokenEncoding != "" {
		return r.cfg.TokenEncoding
	}
	return tokens.EncodingForModel(r.cfg.Model)
}

func countTokens(text string, encoding tokens.Encoding) int {
	return tokens.Count(text, encoding).Count
}

func countJSONValues(value any, encoding tokens.Encoding) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return countTokens(fmt.Sprint(value), encoding)
	}
	return countTokens(string(encoded), encoding) + 4
}

func countMessages(messages []provider.Message, encoding tokens.Encoding) int {
	total := 0
	for _, message := range messages {
		total += tokens.CountMessage(message, encoding)
	}
	return total
}

// serializeViewBytes returns the serialized byte size of a provider view,
// used as the growth signal for the proactive-prune rearm accumulator.
func serializeViewBytes(messages []provider.Message) int {
	total := 0
	for _, message := range messages {
		total += len(tokens.Marshal(message))
	}
	return total
}

// retainStable returns the token-bounded provider view, applying the
// stable-head trim (P2). The first time the conversation exceeds budget the
// oldest logical groups are dropped once and pinned behind a byte-identical
// sentinel; afterwards the tail only grows, so the provider prefix cache is
// never invalidated again by trimming. Prior to the first trim it is exactly
// history.Retain's behaviour.
func (r *Runner) retainStable(messages []provider.Message, maxTokens int, encoding tokens.Encoding) []provider.Message {
	retained, omitted := history.Retain(messages, maxTokens, encoding)
	if omitted <= 0 {
		return retained
	}
	if !r.trimEngaged {
		r.trimEngaged = true
		r.trimStart = omitted
		r.lastMutation = "head-trim"
		r.trimHead = provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("[Internal: the earliest %d message%s were trimmed to fit the context window; subsequent messages remain authoritative.]",
				omitted, pluralS(omitted)),
		}}}
		// Archive the folded originals so dropped context stays traceable;
		// the provider-facing view bytes are untouched.
		if dropped := history.TakeFirstGroups(messages, omitted, encoding); len(dropped) > 0 {
			if err := ArchiveTrimmed(r.cfg.ArchiveDir, r.cfg.SessionID, "head-trim", dropped); err == nil {
				r.lastMutation += "+archive"
			}
		}
	}
	head := []provider.Message{r.trimHead}
	// Pin the first small user turn next to the sentinel: it is the original
	// task/goal, and keeping it verbatim keeps the fold boundary stable while
	// the model retains the instruction that started the session.
	if pinned := r.pinnedFirstTurn(messages, maxTokens, encoding); pinned.Role != "" {
		head = append([]provider.Message{pinned}, head...)
	}
	kept := history.DropFirstGroups(messages, r.trimStart, encoding)
	return append(append([]provider.Message(nil), head...), kept...)
}

const (
	// pinnedFirstTurnTokenCap is the ceiling for pinning the first user turn
	// verbatim next to the trim sentinel (reasonix pins its first small
	// user turn the same way; without the cap a giant first paste would push
	// the pinned prefix well past the budget).
	pinnedFirstTurnTokenCap = 1500
	// pinnedFirstTurnCapFrac bounds the pin by a fraction of the trim budget
	// so small windows never pin more than they retain.
	pinnedFirstTurnCapFrac = 0.15
	// pinnedFirstTurnFloor keeps the pin useful even at tiny budgets.
	pinnedFirstTurnFloor = 64
)

// pinnedFirstTurn returns the first user message when it is small enough to
// pin verbatim at the trim boundary; an empty message means "don't pin".
func (r *Runner) pinnedFirstTurn(messages []provider.Message, budget int, encoding tokens.Encoding) provider.Message {
	if len(messages) == 0 || messages[0].Role != provider.RoleUser {
		return provider.Message{}
	}
	cost := countTokens(messages[0].Text(), encoding)
	cap := pinnedFirstTurnTokenCap
	if frac := maxInt(int(float64(budget)*pinnedFirstTurnCapFrac), 0); frac > 0 && frac < cap {
		cap = maxInt(frac, pinnedFirstTurnFloor)
	}
	if cost > cap {
		return provider.Message{}
	}
	return messages[0]
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func containsBlock(message provider.Message, blockType string) bool {
	for _, block := range message.Content {
		if block.Type == blockType {
			return true
		}
	}
	return false
}

func (r *Runner) execOne(ctx context.Context, call provider.ToolCall) (provider.ContentBlock, *ToolEvent) {
	start := time.Now()
	ev := &ToolEvent{CallID: call.ID, Name: call.Name, Input: call.Input, Title: call.Name}

	t, ok := r.cfg.Tools.Get(call.Name)
	if !ok {
		ev.Output = fmt.Sprintf("unknown tool %q; available: %s", call.Name, strings.Join(r.cfg.Tools.Names(), ", "))
		ev.IsError = true
		return provider.ToolResultBlock(call.ID, ev.Output, true), ev
	}

	preq := describe(call, r.cfg.Cwd)
	level := permission.Level(permission.Allow)
	if r.cfg.Perms != nil {
		level = r.cfg.Perms.Check(preq)
	}
	switch level {
	case permission.Deny:
		ev.Output = fmt.Sprintf("permission denied by policy: %s", preq.Title)
		ev.IsError = true
		ev.Title = "denied: " + preq.Title
		return provider.ToolResultBlock(call.ID, ev.Output, true), ev
	case permission.Ask:
		if r.cfg.Ask == nil {
			ev.Output = "permission required but no approval channel is available"
			ev.IsError = true
			return provider.ToolResultBlock(call.ID, ev.Output, true), ev
		}
		switch r.cfg.Ask(ctx, preq) {
		case DecideReject:
			ev.Output = "the user rejected this action; stop and ask them how to proceed"
			ev.IsError = true
			ev.Title = "rejected: " + preq.Title
			return provider.ToolResultBlock(call.ID, ev.Output, true), ev
		case DecideAlways:
			if r.cfg.Perms != nil {
				r.cfg.Perms.GrantSession(permission.SessionKey(preq))
			}
		}
	}

	input := call.Input

	// plugin hook: tool.execute.before
	if r.cfg.Plugins != nil && r.cfg.Plugins.Len() > 0 {
		before := &plugin.ToolBeforeEvent{
			SessionID: r.cfg.SessionID, Agent: r.cfg.AgentName,
			Tool: call.Name, CallID: call.ID, Input: input,
		}
		if err := r.cfg.Plugins.DispatchToolBefore(ctx, before); err != nil {
			ev.Output = "plugin error: " + err.Error()
			ev.IsError = true
			return provider.ToolResultBlock(call.ID, ev.Output, true), ev
		}
		if before.Skip {
			reason := before.Reason
			if reason == "" {
				reason = "blocked by a plugin"
			}
			ev.Output = reason
			ev.IsError = true
			ev.Title = "blocked: " + preq.Title
			return provider.ToolResultBlock(call.ID, reason, true), ev
		}
		if len(before.Input) > 0 {
			input = before.Input
		}
	}
	if !bytes.Equal(input, call.Input) && r.cfg.Perms != nil {
		modifiedRequest := describe(provider.ToolCall{Name: call.Name, Input: input}, r.cfg.Cwd)
		switch r.cfg.Perms.Check(modifiedRequest) {
		case permission.Deny:
			ev.Output = fmt.Sprintf("permission denied by policy: %s", modifiedRequest.Title)
			ev.IsError = true
			return provider.ToolResultBlock(call.ID, ev.Output, true), ev
		case permission.Ask:
			if r.cfg.Ask == nil || r.cfg.Ask(ctx, modifiedRequest) == DecideReject {
				ev.Output = "the user rejected the plugin-modified action"
				ev.IsError = true
				return provider.ToolResultBlock(call.ID, ev.Output, true), ev
			}
		}
	}

	var repairNoteVar string
	tc := tools.Context{
		Cwd:       r.cfg.Cwd,
		SessionID: r.cfg.SessionID,
		Agent:     r.cfg.AgentName,
		AgentID:   r.cfg.AgentID,
		CallID:    call.ID,
		Depth:     r.cfg.Depth,
		Repair:    &tools.RepairOpts{Note: &repairNoteVar, Family: r.repairFamily},
	}
	res, err := t.Run(ctx, tc, input)
	ev.Elapsed = time.Since(start)
	if err != nil {
		ev.Output = err.Error()
		ev.IsError = true
		return provider.ToolResultBlock(call.ID, "tool error: "+err.Error(), true), ev
	}
	ev.Output = res.Output
	ev.Meta = res.Meta
	ev.IsError = res.IsError
	if res.Title != "" {
		ev.Title = res.Title
	}
	// Surface any tool-call repair the tool applied: the note is already in
	// res.Output (each tool appends "<repaired: …>"), and it is mirrored in
	// ToolEvent.Repaired for the TUI and per-model telemetry. A repaired call
	// succeeded, so it is never an error.
	if repairNoteVar != "" {
		ev.Repaired = repairNoteVar
	}

	// plugin hook: tool.execute.after
	if r.cfg.Plugins != nil && r.cfg.Plugins.Len() > 0 {
		after := &plugin.ToolAfterEvent{
			SessionID: r.cfg.SessionID, Agent: r.cfg.AgentName,
			Tool: call.Name, CallID: call.ID, Input: input,
			Output: ev.Output, IsError: ev.IsError,
		}
		if err := r.cfg.Plugins.DispatchToolAfter(ctx, after); err == nil {
			ev.Output = after.Output
			ev.IsError = after.IsError
		}
	}

	modelOutput, stats := r.capToolOutput(call, ev.Output, ev.IsError)
	ev.Optimization = stats
	return provider.ToolResultBlock(call.ID, modelOutput, ev.IsError), ev
}

const maxModelToolResultBytes = 32 << 10

// cacheMissNoiseFloor is the smallest cache-read shortfall (in prompt tokens)
// treated as a genuine prefix eviction, matching the TUI's miss detector. A
// turn that reads most of the previously sent prompt within this margin is
// just a small fresh tail, not an expired-idle-gap re-bill.
const cacheMissNoiseFloor = 1024

// toolResultMaxBytes returns the per-tool_result cap for this run, falling
// back to the built-in default when the config leaves it at 0.
func (r *Runner) toolResultMaxBytes() int {
	if r.cfg.MaxToolResultBytes > 0 {
		return r.cfg.MaxToolResultBytes
	}
	return maxModelToolResultBytes
}

// capToolOutput applies the deterministic command-aware reducer and then the
// reversible live-zone pass. The pre-live-zone payload is stored under the
// call id so the model can retrieve it via retrieve_uncompressed_context.
func (r *Runner) capToolOutput(call provider.ToolCall, output string, isError bool) (string, *OptimizationStats) {
	// Spill (harness-style tool-output spill): when the raw output is huge
	// and spilling is enabled, persist the full bytes under their content
	// address and send only a bounded preview plus a retrieval locator. The
	// model can pull the whole blob back with retrieve_uncompressed_context,
	// so the fresh tail stays small without losing data. Error output and
	// already-small results are never spilled.
	if r.cfg.SpillBytes > 0 && !isError && len(output) > r.cfg.SpillBytes {
		if key := r.budget.StorePayload(output); key != "" {
			preview := spillPreview(output, r.toolResultMaxBytes())
			modelOutput := fmt.Sprintf("[spilled %d bytes; retrieve with retrieve_uncompressed_context key=%s]\n\n%s",
				len(output), key, preview)
			return modelOutput, &OptimizationStats{
				Stage:            "spill",
				OriginalBytes:    len(output),
				CompressedBytes:  len(modelOutput),
				OriginalTokens:   countTokens(output, tokens.EncodingCl100kBase),
				CompressedTokens: countTokens(modelOutput, tokens.EncodingCl100kBase),
				SavedTokens:      maxInt(0, countTokens(output, tokens.EncodingCl100kBase)-countTokens(modelOutput, tokens.EncodingCl100kBase)),
			}
		}
	}
	modelOutput, stats := capToolOutputStatic(call, output, isError, r.toolResultMaxBytes())
	if r.budget != nil && r.budget.Enabled() && !isError && len(modelOutput) > 0 {
		key := call.ID
		if key == "" {
			key = "tool:" + call.Name
		}
		live, changed := r.budget.CompressLive(key, modelOutput)
		if changed {
			stats = &OptimizationStats{
				Stage:            stats.Stage + "+live-zone",
				Fallback:         stats.Fallback,
				OriginalBytes:    stats.OriginalBytes,
				CompressedBytes:  len(live),
				OriginalTokens:   stats.OriginalTokens,
				CompressedTokens: countTokens(live, tokens.EncodingCl100kBase),
				SavedTokens:      maxInt(0, stats.OriginalTokens-stats.CompressedTokens),
				Truncated:        stats.Truncated,
			}
			modelOutput = live
		}
	}
	return modelOutput, stats
}

// spillPreview renders a bounded head+marker+tail preview of a spilled tool
// result, mirroring the deterministic truncation in compress.finish so the
// model still sees the beginning and the diagnostic tail. The full output is
// retrievable by its content-address key.
func spillPreview(output string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = maxModelToolResultBytes
	}
	if len(output) <= maxBytes {
		return output
	}
	omitted := len(output) - maxBytes
	marker := fmt.Sprintf("\n… <spilled output truncated; %d bytes omitted — use retrieve_uncompressed_context for the full payload>", omitted)
	remaining := maxBytes - len(marker)
	if remaining <= 0 {
		return marker
	}
	headBytes := remaining / 2
	tailBytes := remaining - headBytes
	return safeHead(output, headBytes) + marker + safeTail(output, tailBytes)
}

func safeHead(s string, n int) string {
	if n >= len(s) {
		return s
	}
	s = s[:n]
	for n > 0 && !utf8.RuneStart(s[n-1]) {
		n--
		s = s[:n]
	}
	return s
}

func safeTail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func capToolOutputStatic(call provider.ToolCall, output string, isError bool, maxBytes int) (string, *OptimizationStats) {
	compressed := compress.ForTool(compress.Input{
		Text:     output,
		Command:  compressionCommand(call),
		Tool:     call.Name,
		MaxBytes: maxBytes,
		IsError:  isError,
	})
	modelOutput := compressed.Text
	encoding := tokens.EncodingCl100kBase
	originalTokens := countTokens(output, encoding)
	compressedTokens := countTokens(modelOutput, encoding)
	return modelOutput, &OptimizationStats{
		Stage:            compressed.Stage,
		Fallback:         compressed.Fallback,
		OriginalBytes:    compressed.OriginalBytes,
		CompressedBytes:  len(modelOutput),
		OriginalTokens:   originalTokens,
		CompressedTokens: compressedTokens,
		SavedTokens:      maxInt(0, originalTokens-compressedTokens),
		Truncated:        compressed.Truncated,
	}
}
func compressionCommand(call provider.ToolCall) string {
	if call.Name == "bash" {
		var input struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(call.Input, &input) == nil && input.Command != "" {
			return input.Command
		}
	}
	return call.Name
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func canonicalToolInput(input json.RawMessage) string {
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return string(input)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return string(input)
	}
	canonical, err := canonicalJSONValue(value)
	if err != nil {
		return string(input)
	}
	return canonical
}

func canonicalJSONValue(value any) (string, error) {
	switch value := value.(type) {
	case nil:
		return "null", nil
	case bool:
		return fmt.Sprintf("bool:%t", value), nil
	case string:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return "string:" + string(encoded), nil
	case json.Number:
		normalized, ok := normalizeJSONNumber(string(value))
		if !ok {
			return "", fmt.Errorf("invalid JSON number %q", value)
		}
		return "number:" + normalized, nil
	case []any:
		var builder strings.Builder
		builder.WriteString("array:[")
		for index, item := range value {
			if index > 0 {
				builder.WriteByte(',')
			}
			canonical, err := canonicalJSONValue(item)
			if err != nil {
				return "", err
			}
			builder.WriteString(canonical)
		}
		builder.WriteByte(']')
		return builder.String(), nil
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		var builder strings.Builder
		builder.WriteString("object:{")
		for index, key := range keys {
			if index > 0 {
				builder.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return "", err
			}
			canonical, err := canonicalJSONValue(value[key])
			if err != nil {
				return "", err
			}
			builder.Write(encodedKey)
			builder.WriteByte(':')
			builder.WriteString(canonical)
		}
		builder.WriteByte('}')
		return builder.String(), nil
	default:
		return "", fmt.Errorf("unsupported JSON value %T", value)
	}
}

func normalizeJSONNumber(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	negative := raw[0] == '-'
	if negative {
		raw = raw[1:]
	}

	exponent := new(big.Int)
	if exponentIndex := strings.IndexAny(raw, "eE"); exponentIndex >= 0 {
		exponentText := raw[exponentIndex+1:]
		if _, ok := exponent.SetString(exponentText, 10); !ok {
			return "", false
		}
		raw = raw[:exponentIndex]
	}

	integerPart := raw
	fractionPart := ""
	if decimalIndex := strings.IndexByte(raw, '.'); decimalIndex >= 0 {
		integerPart = raw[:decimalIndex]
		fractionPart = raw[decimalIndex+1:]
	}
	digits := integerPart + fractionPart
	leadingZeros := 0
	for leadingZeros < len(digits) && digits[leadingZeros] == '0' {
		leadingZeros++
	}
	if leadingZeros == len(digits) {
		return "0", true
	}
	digits = digits[leadingZeros:]
	for len(digits) > 1 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
	}

	decimalPosition := new(big.Int).SetInt64(int64(len(integerPart)))
	decimalPosition.Add(decimalPosition, exponent)
	decimalPosition.Sub(decimalPosition, big.NewInt(int64(leadingZeros)))
	decimalPosition.Sub(decimalPosition, big.NewInt(int64(len(digits))))
	if negative {
		return "-" + digits + "e" + decimalPosition.String(), true
	}
	return digits + "e" + decimalPosition.String(), true
}

// describe converts a tool call into a permission request with a readable
// title and preview body.
func describe(call provider.ToolCall, cwd string) permission.Request {
	req := permission.Request{Tool: call.Name, Title: call.Name}
	var m map[string]any
	_ = json.Unmarshal(call.Input, &m)

	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}

	switch call.Name {
	case "bash":
		req.Command = str("command")
		req.Title = str("description")
		if req.Title == "" {
			req.Title = req.Command
		}
		req.Body = req.Command
	case "write":
		req.Path = str("path")
		req.Title = "write " + req.Path
		body := str("content")
		if len(body) > 4000 {
			body = body[:4000] + "\n…"
		}
		req.Body = body
	case "edit":
		req.Path = str("path")
		req.Title = "edit " + req.Path
		req.Body = "- " + oneLine(str("old_string")) + "\n+ " + oneLine(str("new_string"))
	case "apply_patch":
		req.Title = "apply patch"
		body := str("patch")
		if paths, err := tools.PatchPaths(body); err == nil {
			req.Paths = paths
			if len(paths) > 0 {
				req.Path = paths[0]
			}
		}
		if len(body) > 4000 {
			body = body[:4000] + "\n…"
		}
		req.Body = body
	case "read", "grep", "glob", "list", "tree", "code_symbols":
		req.Path = str("path")
		req.Title = call.Name + " " + str("path") + str("pattern")
	case "webfetch", "fetch":
		raw := str("url")
		if raw == "" {
			raw = str("uri")
		}
		req.Host = hostOf(raw)
		req.Title = "fetch " + raw
		req.Body = raw
	case "websearch":
		req.Title = "web search: " + str("query")
		req.Body = str("query")
	case "vision":
		req.Path = str("path")
		req.Title = "vision " + req.Path
		req.Body = req.Path
		if f := str("focus"); f != "" {
			req.Body += "\nfocus: " + oneLine(f)
		}
	default:
		req.Title = call.Name
		req.Body = string(call.Input)
	}
	return req
}

// hostOf extracts the hostname from a URL so host rules can match it.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// cacheStrategy returns the run's pluggable cache strategy, or a
// DefaultStrategy carrying the flat knobs when none is configured.
func (r *Runner) cacheStrategy() provider.CacheStrategy {
	if r.cfg.CacheStrategy != nil {
		return r.cfg.CacheStrategy
	}
	return provider.DefaultStrategy{
		RetentionVal:    r.cfg.CacheRetention,
		TTLVal:          r.cacheTTLFromKnobs(),
		WarmVal:         r.cfg.WarmCache,
		WarmTurnVal:     r.cfg.WarmTurn,
		MaxReasoningVal: r.cfg.MaxReasoningTurns,
		PassbackVal:     r.cfg.PassbackReasoning,
	}
}

// recordUsageSample pushes a turn's usage into the bounded ring.
func (r *Runner) recordUsageSample(s usageSample) {
	r.usageRing[r.usageRingAt] = s
	r.usageRingAt = (r.usageRingAt + 1) % usageRingDepth
	if r.usageRingN < usageRingDepth {
		r.usageRingN++
	}
}

// inferredEvictionPoint returns the cache-prefix position (in tokens) at
// which the last few turns suggest the provider's cache was dropped, or 0
// when no partial eviction is evident. It scans the ring for the newest
// sample whose cache-read share of its own prompt fell well below the
// previous sample's share: a bounded-LRU (DeepSeek-style) cache evicts the
// oldest region first, so the read/prompt ratio collapses when the provider
// drops a wave. The re-warm must fire before the next request's gap reaches
// the point where the drop happened.
func (r *Runner) inferredEvictionPoint() int {
	if r.usageRingN < 2 {
		return 0
	}
	n := r.usageRingN
	// Iterate oldest → newest.
	oldest := (r.usageRingAt - n + usageRingDepth) % usageRingDepth
	prevRatio := -1.0
	for i := 0; i < n; i++ {
		s := r.usageRing[(oldest+i)%usageRingDepth]
		if s.prompt <= 0 {
			continue
		}
		ratio := float64(s.cacheRead) / float64(s.prompt)
		if prevRatio >= 0 && ratio < prevRatio-0.5 && s.cacheRead > 0 {
			// The read share collapsed from the previous turn to this one
			// while still reading something: a partial eviction cut the
			// cached region between them. The eviction point is roughly the
			// prompt size this turn still read — anything beyond it is gone.
			return s.cacheRead
		}
		prevRatio = ratio
	}
	return 0
}

// cacheTTLFromKnobs is the flat-knob TTL resolution (the pre-strategy
// cacheTTL behaviour); cacheStrategy wraps it so a strategy can override.
func (r *Runner) cacheTTLFromKnobs() time.Duration {
	if r.observedEvictionGap > 0 {
		threshold := time.Duration(float64(r.observedEvictionGap) * 0.9)
		if threshold < time.Second {
			threshold = time.Second
		}
		return threshold
	}
	if r.cfg.CacheTTLSeconds > 0 {
		return time.Duration(r.cfg.CacheTTLSeconds) * time.Second
	}
	name := ""
	if r.cfg.Provider != nil {
		name = r.cfg.Provider.Name()
	}
	return provider.CacheTTLForModel(name, r.cfg.Model, r.cfg.CacheRetention)
}

// cacheTTL bounds how long a warm prefix is assumed to survive at the
// provider before an idle gap forces a re-warm. The vendor table replaces
// the old fixed 5-minute default: DeepSeek-line endpoints keep their prefix
// cache for a day, so re-warming after every idle gap was pure waste. A
// positive CacheTTLSeconds overrides the table for gateways whose real
// retention is far shorter (free flash tiers expire in minutes, not days).
//
// An observed eviction gap wins over both: once this run provably evicted
// the prefix after a gap of G, the warm threshold drops to just below G, so
// a provider that evicts at minutes-scale (despite the vendor table assuming
// a day) is re-warmed before the eviction point instead of after it.
func (r *Runner) cacheTTL() time.Duration {
	if r.cfg.CacheStrategy != nil {
		ttl := r.cfg.CacheStrategy.CacheTTL()
		// The observed eviction gap still tightens below the strategy's TTL:
		// a provider that provably evicts earlier than the strategy assumed
		// must be re-warmed before the real eviction point.
		if r.observedEvictionGap > 0 {
			threshold := time.Duration(float64(r.observedEvictionGap) * 0.9)
			if threshold < time.Second {
				threshold = time.Second
			}
			if ttl <= 0 || threshold < ttl {
				return threshold
			}
		}
		if ttl > 0 {
			return ttl
		}
		return r.cacheTTLFromKnobs()
	}
	return r.cacheTTLFromKnobs()
}

// warnWarmOnce surfaces a warm failure once per distinct error message per
// run. A silently swallowed warm would hide the full cold re-bill that is
// about to happen.
func (r *Runner) warnWarmOnce(emit func(Event) bool, err error) {
	if err == nil || r.warmErrWarned == err.Error() {
		return
	}
	r.warmErrWarned = err.Error()
	emit(Event{Kind: EvAgentMessage, Text: "cache warm failed (this request may re-bill cold): " + err.Error()})
}
