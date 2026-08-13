// Command cachehit measures rick's provider prompt-cache hit rate over a
// simulated long session. Unlike a raw provider.Stream harness, this drives
// the real agent loop (agent.Runner): session-start warm, stable-head
// trimming, one-shot reasoning cap, content-addressed dedup and prefix
// divergence tracking all run exactly as they do in production, so the
// measured rate is what a real session ships, not a copy.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"rick/internal/agent"
	"rick/internal/config"
	"rick/internal/memory"
	"rick/internal/provider"
	"rick/internal/provider/anthropic"
	"rick/internal/provider/catalog"
	"rick/internal/provider/openai"
	"rick/internal/tools"
)

// taskSteps cycles through plausible coding-agent subtasks. Each turn appends
// the next step verbatim so the transcript grows append-only like a real
// user-threaded session, never a rewritten prefix.
var taskSteps = []string{
	"Read go.mod and report the module path and Go version.",
	"List the top-level packages and one entrypoint per package.",
	"Summarize the caching strategy in the provider layer.",
	"Find the tool that reads files and cite its signature.",
	"Check for any debug prints left in the diff and list them.",
	"Trace how a session is resumed from the session store.",
	"Identify where token accounting is persisted and note the fields.",
	"Search for the flag parser in cmd/ and name each option.",
	"Confirm the model id used when none is configured.",
	"List the tool registry methods and their accessors.",
	"Describe the permission engine's decision levels.",
	"Find where the stable-head trim sentinel is created.",
	"Summarize the reasoning-echo cap behaviour for DeepSeek.",
	"Name the provider wire mode required for OpenAI-compatible endpoints.",
	"Locate the goal budget enforcement and the counter it reads.",
	"Describe what the warm request does with the first message.",
	"Identify the dedup key used for tool results.",
	"Report the wire tools that become the OpenAI tools block.",
	"Find the cache miss noise floor constant and its value.",
	"Summarize the divergence reason inference rules.",
}

// toolDefs mirror the tool signatures the agent registers, so the benchmark
// prefix carries the same stable tool-schema block a real session carries.
var toolDefs = []toolDef{
	{"bash", []string{"command: string", "cwd?: string", "timeout?: number"}},
	{"write", []string{"path: string", "content: string"}},
	{"read", []string{"path: string", "offset?: number", "limit?: number"}},
	{"edit", []string{"path: string", "old_string: string", "new_string: string"}},
	{"grep", []string{"pattern: string", "path?: string", "include?: string", "limit?: number"}},
	{"glob", []string{"pattern: string", "path?: string"}},
	{"list", []string{"path?: string", "depth?: number"}},
	{"apply_patch", []string{"patch: string"}},
	{"todowrite", []string{"todos: object[]"}},
	{"todoread", []string{}},
	{"code_symbols", []string{"action: string", "path: string", "symbol?: string"}},
	{"diagnostics", []string{"scope?: string"}},
	{"test", []string{"scope?: string", "verbose?: boolean"}},
	{"tree", []string{"path?: string", "depth?: string", "pattern?: string"}},
	{"fetch", []string{"url: string", "extract?: string"}},
	{"memory", []string{"action: string", "key?: string", "value?: string"}},
	{"websearch", []string{"query: string", "max_results?: number"}},
	{"git", []string{"action: string", "path?: string", "since?: string"}},
	{"goalwrite", []string{"title: string", "steps?: object[]"}},
	{"goalread", []string{}},
	{"goalstep", []string{"step_id: string", "status: string"}},
	{"retrieve_uncompressed_context", []string{"key: string", "list?: boolean"}},
	{"report", []string{"summary: string", "full_output?: string"}},
	{"task", []string{"prompt: string", "subagent_type?: string"}},
	{"steer", []string{"instruction: string", "target_agent: string"}},
	{"chat", []string{"message: string", "target_agent: string"}},
	{"swarm", []string{"action: string", "goal?: string"}},
	{"goalabort", []string{"reason?: string"}},
}

type toolDef struct {
	name string
	args []string
}

// toolManifest renders the tool block exactly like rick's, so the benchmark
// prefix carries the same stable tool manifest a real session carries.
func toolManifest(tools []toolDef) string {
	var b strings.Builder
	b.WriteString("\n\n## Tools (TypeScript signatures)\n")
	for _, t := range tools {
		b.WriteString(t.name + "(" + strings.Join(t.args, ", ") + "): void\n")
	}
	return b.String()
}

// toolResultCapKB mirrors rick's per-tool_result truncation: the configured
// cache_max_tool_result_bytes (default 16 KiB) or the agent's built-in 32 KiB
// fallback. The benchmark clamps its simulated tool-result payload to this so
// the fresh tail matches what a real session actually sends to the model.
func toolResultCapKB(cfg config.Config) int {
	capBytes := cfg.CacheMaxToolResultBytes
	if capBytes <= 0 {
		capBytes = 32 << 10
	}
	kb := capBytes / 1024
	if kb < 1 {
		return 1
	}
	return kb
}

// payload renders a deterministic mid-size tool-result payload for turn i.
// It is appended as a user message so the transcript grows by roughly the
// requested kilobyte size every turn, like a real tool result.
func payload(i int, kbytes int) string {
	block := "module internal/%02d\n// resolver for %s\nfunc r%d() int { return %d }\n"
	var b strings.Builder
	for len(b.String()) < kbytes*1024 {
		b.WriteString(fmt.Sprintf(block, i%77, taskSteps[i%len(taskSteps)], i, i*7919%1000000))
		b.WriteString("\n")
	}
	return b.String()
}

func buildProviderSet(cfg config.Config) map[string]provider.Provider {
	out := map[string]provider.Provider{}
	for name, p := range cfg.Providers {
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		kind := p.Type
		if kind == "" {
			if e, ok := catalog.Get(name); ok {
				kind = e.Flavor
			} else {
				kind = name
			}
		}
		switch kind {
		case catalog.FlavorAnthropic:
			if p.APIKey == "" && p.BaseURL == "" {
				continue
			}
			out[name] = anthropic.New(p.APIKey, p.BaseURL)
		case "openai", "openrouter", "groq", "deepseek", "together", "openai-compatible":
			if p.APIKey == "" && p.BaseURL == "" {
				continue
			}
			c := openai.New(name, p.APIKey, p.BaseURL)
			c.SetOpenRouterResponseCache(cfg.CacheOpenRouterResponse, cfg.CacheOpenRouterResponseTTL)
			out[name] = c
		default:
			if p.BaseURL != "" {
				c := openai.New(name, p.APIKey, p.BaseURL)
				c.SetOpenRouterResponseCache(cfg.CacheOpenRouterResponse, cfg.CacheOpenRouterResponseTTL)
				out[name] = c
			}
		}
	}
	return out
}

// resolveConfig loads the active config and merges saved credentials into it
// (same as cmd/rick's buildDeps), so the selected model and provider set are
// both available to the test.
func resolveConfig(projectRoot string) (*config.Loaded, string, error) {
	loaded, err := config.Load(projectRoot)
	if err != nil {
		return nil, "", fmt.Errorf("load config: %w", err)
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		return nil, "", fmt.Errorf("load credentials: %w", err)
	}
	config.MergeCredentials(&loaded.Config, creds)
	if loaded.Config.Model == "" {
		for _, id := range creds.IDs() {
			if mid := config.FirstConfiguredModel(creds, id); mid != "" {
				loaded.Config.Model = mid
				break
			}
		}
	}
	return loaded, loaded.Config.Model, nil
}

// unit holds the provider-reported usage for one measured turn. input is the
// uncached input (cache miss), read the cache-hit tokens, and written the
// cache-creation tokens where the provider reports them.
type unit struct {
	input    int
	read     int
	written  int
	resets   int // full-prefix evictions observed this turn
	partial  int // mid-prefix rewrites that kept part of the cache (blocks)
	diverge  int // unexpected client-side prefix rewrites observed this turn
	requests int // provider round-trips in this turn (1 for a text turn)
}

// hitPct uses the plan docs' definition (docs/cache-hit-plan-2026-08-07.md):
// cache_read over the whole prompt footprint (input + cache_read +
// cache_write), like tracker.Day.CacheHitRate.
func hitPct(input, read, written int) float64 {
	denom := input + read + written
	if denom <= 0 {
		return 0
	}
	return 100 * float64(read) / float64(denom)
}

// runTurn drives one request through the Runner and drains its events,
// accumulating usage, divergence and eviction counters for the turn. Events
// are consumed concurrently because the Runner emits synchronously and a long
// turn can fill the buffer before Run returns.
func runTurn(ctx context.Context, runner *agent.Runner, history []provider.Message) (unit, []provider.Message, error) {
	out := make(chan agent.Event, 512)
	var used unit
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range out {
			switch ev.Kind {
			case agent.EvUsage:
				if ev.Usage == nil {
					continue
				}
				mu.Lock()
				used.input += ev.Usage.InputTokens
				used.read += ev.Usage.CacheReadTokens
				used.written += ev.Usage.CacheWriteTokens
				used.requests++
				mu.Unlock()
			case agent.EvCacheDivergence:
				if ev.Divergence == nil || ev.Divergence.Kind == "" {
					continue
				}
				mu.Lock()
				if ev.Divergence.Reason == "unexpected" {
					used.diverge++
				}
				// A full prefix change (system, tools, or a message rewrite
				// before the previous tail) is billed as a reset. A rewrite
				// that kept part of the cache (the provider still serves the
				// surviving prefix blocks) is a partial miss, not a reset.
				if ev.Divergence.Index >= 0 || ev.Divergence.Kind == "system" || ev.Divergence.Kind == "tools" {
					if ev.Divergence.CachedPrefixTokens > 0 {
						used.partial++
					} else {
						used.resets++
					}
				}
				mu.Unlock()
			}
		}
	}()

	appended, runErr := runner.Run(ctx, history, out)
	wg.Wait()
	return used, appended, runErr
}

// runTurnSafe wraps runTurn so a panic in the runner (which would skip
// Run's `defer close(out)` and leave the drain goroutine blocked forever)
// surfaces as an error instead of deadlocking the benchmark.
func runTurnSafe(ctx context.Context, runner *agent.Runner, history []provider.Message) (u unit, appended []provider.Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cachehit: runner panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return runTurn(ctx, runner, history)
}

// rawTranscript is the original benchmark's append-only user-only transcript:
// never echoes the model reply, only grows by the user turn + payload each
// request. This is the shape whose numbers are comparable to any pre-runner
// measurements.
type rawTranscript struct {
	messages []provider.Message
}

// runRawTurn streams one request directly through the provider and returns
// usage. This mirrors the pre-Runner harness exactly (including its warm-once
// per pass at the caller).
func runRawTurn(ctx context.Context, p provider.Provider, model, system, sessionID string,
	messages []provider.Message, maxTokens int) (unit, error) {
	out := make(chan provider.Event, 512)
	var used unit
	var wg sync.WaitGroup
	wg.Add(1)
	var streamErr error
	go func() {
		defer wg.Done()
		for ev := range out {
			switch ev.Kind {
			case provider.EventUsage:
				if ev.Usage == nil {
					continue
				}
				used.input += ev.Usage.InputTokens
				used.read += ev.Usage.CacheReadTokens
				used.written += ev.Usage.CacheWriteTokens
				used.requests++
			case provider.EventError:
				if ev.Err != nil {
					streamErr = ev.Err
				}
			}
		}
	}()

	p.Stream(ctx, provider.Request{
		Model:          model,
		System:         system,
		Messages:       messages,
		MaxTokens:      maxTokens,
		CacheRetention: provider.CacheRetentionLong,
		SessionID:      sessionID,
	}, out)
	wg.Wait()
	return used, streamErr
}

// runRawTurnSafe wraps runRawTurn so a panic in the provider stream (which
// would skip Stream's `defer close(ch)` and leave the drain goroutine blocked
// forever) surfaces as an error instead of deadlocking the benchmark.
func runRawTurnSafe(ctx context.Context, p provider.Provider, model, system, sessionID string,
	messages []provider.Message, maxTokens int) (u unit, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cachehit: provider stream panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return runRawTurn(ctx, p, model, system, sessionID, messages, maxTokens)
}

// runRawPass runs the exact pre-Runner harness: one warm at session start,
// then one append-only user transcript per turn with no assistant echo.
func runRawPass(ctx context.Context, p provider.Provider, model, system, sessionID string,
	turns, payloadKB int, warm bool) ([]unit, error) {
	if warm {
		if w, warmable := p.(provider.CacheWarmber); warmable {
			_ = w.Warm(ctx, provider.Request{
				Model: model, System: system, SessionID: sessionID,
				CacheRetention: provider.CacheRetentionLong, MaxTokens: 1,
			})
		}
	}
	tr := &rawTranscript{}
	results := make([]unit, 0, turns)
	var shown []float64
	fmt.Printf("  %-4s %8s %10s %12s %10s\n", "turn", "hit%", "input", "cache_read", "cache_write")
	for i := 0; i < turns; i++ {
		step := taskSteps[i%len(taskSteps)]
		tr.messages = append(tr.messages, provider.UserText(step))
		if payloadKB > 0 {
			tr.messages = append(tr.messages, provider.UserText(payload(i, payloadKB)))
		}
		used, err := runRawTurnSafe(ctx, p, model, system, sessionID, tr.messages, 512)
		if err != nil {
			return results, fmt.Errorf("turn %d: %w", i+1, err)
		}
		results = append(results, used)
		turn := i + 1
		if turn%5 == 0 {
			hit := hitPct(used.input, used.read, used.written)
			shown = append(shown, hit)
			fmt.Printf("  %-9d %7.2f%% %10d %12d %10d\n", turn, hit, used.input, used.read, used.written)
		}
	}
	if len(shown) > 0 {
		var sum float64
		for _, hit := range shown {
			sum += hit
		}
		fmt.Printf("\n  average hit%% across shown turns: %6.2f%%\n", sum/float64(len(shown)))
	}
	return results, nil
}

// runPass drives one complete session of turns through the Runner and returns
// the per-turn usage. The transcript is append-only: each turn appends one
// task step plus the simulated tool payload, the Runner answers in text (the
// registry is empty, so the loop is one request per turn), and the reply is
// carried into the next turn's history.
func runPass(ctx context.Context, runner *agent.Runner, turns, payloadKB int) ([]unit, error) {
	results := make([]unit, 0, turns)
	var history []provider.Message
	// Every row shown is a milestone (5, 10, 15, ...); their hit percentages
	// feed the average printed beneath the table.
	var shown []float64
	fmt.Printf("  %-4s %8s %10s %12s %10s %8s %8s\n", "turn", "hit%", "input", "cache_read", "cache_write", "resets", "diverge")
	for i := 0; i < turns; i++ {
		step := taskSteps[i%len(taskSteps)]
		history = append(history, provider.UserText(step))
		if payloadKB > 0 {
			history = append(history, provider.UserText(payload(i, payloadKB)))
		}
		used, appended, err := runTurnSafe(ctx, runner, history)
		if err != nil {
			return results, fmt.Errorf("turn %d: %w", i+1, err)
		}
		history = append(history, appended...)
		results = append(results, used)
		turn := i + 1
		if turn%5 == 0 {
			hit := hitPct(used.input, used.read, used.written)
			shown = append(shown, hit)
			fmt.Printf("  %-9d %7.2f%% %10d %12d %10d %8d %8d\n", turn, hit, used.input, used.read, used.written, used.resets, used.diverge)
		}
	}
	if len(shown) > 0 {
		var sum float64
		for _, hit := range shown {
			sum += hit
		}
		fmt.Printf("\n  average hit%% across shown turns: %6.2f%%\n", sum/float64(len(shown)))
	}
	return results, nil
}

// summarize prints the aggregate hit rate for a pass of units, with an
// explicit TOTAL line for the whole run.
func summarize(label string, results []unit) {
	var totIn, totRead, totWritten, totResets, totPartial, totDiverge, totReq int
	for _, u := range results {
		totIn += u.input
		totRead += u.read
		totWritten += u.written
		totResets += u.resets
		totPartial += u.partial
		totDiverge += u.diverge
		totReq += u.requests
	}
	fmt.Printf("\n%s TOTAL hit rate over %d turns: %6.2f%%  (total input=%d, total cache_read=%d, cache_write=%d, resets=%d, partial=%d, diverge=%d)\n",
		label, len(results), hitPct(totIn, totRead, totWritten), totIn, totRead, totWritten, totResets, totPartial, totDiverge)
	if len(results) > 1 {
		var warmIn, warmRead, warmWritten, warmResets int
		for _, u := range results[1:] {
			warmIn += u.input
			warmRead += u.read
			warmWritten += u.written
			warmResets += u.resets
		}
		fmt.Printf("%s warm turns (2..%d): %6.2f%%  (input=%d, cache_read=%d, cache_write=%d, resets=%d)\n",
			label, len(results), hitPct(warmIn, warmRead, warmWritten), warmIn, warmRead, warmWritten, warmResets)
	}
	if totReq > 0 {
		fmt.Printf("%s total requests  : %d over %d turns (%.1f round-trips/turn)\n",
			label, totReq, len(results), float64(totReq)/float64(len(results)))
	}
	last := results[len(results)-1]
	fmt.Printf("%s last request     : %6.2f%%  (input=%d, cache_read=%d, cache_write=%d)\n",
		label, hitPct(last.input, last.read, last.written), last.input, last.read, last.written)
}

func main() {
	modelFlag := flag.String("model", "", "model to test (default: selected in rick.json)")
	modeFlag := flag.String("mode", "runner", "harness shape: 'raw' = original direct provider.Stream (user-only transcript, one warm/pass, no assistant echo); 'runner' = real agent.Runner loop (replies and reasoning echoed)")
	turnsFlag := flag.Int("turns", 30, "number of task turns per pass")
	longFlag := flag.Bool("long", false, "25-turn long run (fixed -turns 25); prints the run's total hit rate")
	runsFlag := flag.Int("runs", 1, "number of passes (2 = re-run same turns in a fresh session)")
	warmFlag := flag.Bool("warm", true, "enable the session-start and eviction re-warm on the Runner")
	warmTurnFlag := flag.Bool("warm-turn", false, "prime the full prefix before every real turn (cache_warm_turn); models a provider that evicts mid-session")
	maxTokensFlag := flag.Int("max-tokens", 512, "completion budget per turn")
	payloadFlag := flag.Int("payload", 2, "tool-result payload size in KiB appended each turn (0 = none)")
	reasoningFlag := flag.Int("reasoning-turns", 0, "cache_max_reasoning_turns fed to the Runner (0 = keep all reasoning, byte-stable)")
	toolsFlag := flag.Int("tools", -1, "tool count in the manifest block (-1 = full catalogue)")
	systemFlag := flag.String("system", "", "stable system prompt (default: rick's BuildPrompt + project + env + tools)")
	idleFlag := flag.Duration("idle", 0, "pause before each pass after the first (models the idle gap between user turns that evicts the provider prefix cache)")
	strategyFlag := flag.String("strategy", "", "named cache_strategies entry to resolve for the run (provider/model or provider id)")
	sharedMemoryFlag := flag.Bool("shared-memory", false, "run two passes: pass 1 pins a deterministic goal-state snapshot, pass 2 loads it as the leading user message (models cross-session prefix reuse)")
	paidFlag := flag.Bool("paid", false, "assert the paid-tier client ceiling: TOTAL hit rate (write-weighted) must be >= 98% with 0 resets and 0 unexpected divergences, else exit non-zero. Use with a paid (non free-flash) model to prove the client ceiling, matching the harness's key-gated live e2e.")
	flag.Parse()

	if *longFlag {
		*turnsFlag = 25
	}

	model := *modelFlag
	projectRoot := "."
	var loaded *config.Loaded
	if model == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cachehit:", err)
			os.Exit(1)
		}
		projectRoot = cwd
		loaded, model, err = resolveConfig(cwd)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cachehit:", err)
			os.Exit(1)
		}
	}
	if model == "" {
		fmt.Fprintln(os.Stderr, "cachehit: no model selected; pass -model provider/model-id")
		os.Exit(1)
	}

	system := *systemFlag
	if system == "" {
		project := agent.ProjectContext(projectRoot, nil)
		system = agent.BuildPrompt
		if project != "" {
			system += project + "\n"
		}
		system += agent.Environment(projectRoot, model, "cachehit", "")
		toolList := toolDefs
		if *toolsFlag >= 0 && *toolsFlag < len(toolList) {
			toolList = toolList[:*toolsFlag]
		}
		system += toolManifest(toolList)
	}

	if loaded == nil {
		var err error
		loaded, _, err = resolveConfig(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, "cachehit:", err)
			os.Exit(1)
		}
	}

	// Clamp the simulated tool-result payload to rick's per-tool_result
	// truncation so the tail matches what a real session actually sends.
	payloadKB := *payloadFlag
	if capKB := toolResultCapKB(loaded.Config); payloadKB > capKB {
		fmt.Printf("cachehit: clamping -payload %dKB to %dKB (tool-result cap)\n", payloadKB, capKB)
		payloadKB = capKB
	}

	providerID, modelID := config.SplitModel(model)
	if modelID == "" {
		modelID = model
	}

	providers := buildProviderSet(loaded.Config)
	pr, ok := providers[providerID]
	if !ok {
		fmt.Fprintf(os.Stderr, "cachehit: no provider %q configured (have %d)\n", providerID, len(providers))
		os.Exit(1)
	}

	ctx := context.Background()

	reasoningTurns := *reasoningFlag
	fmt.Printf("cache hit benchmark  model=%s  turns=%d  runs=%d  payload=%dKB  system=%d bytes",
		model, *turnsFlag, *runsFlag, payloadKB, len(system))
	if reasoningTurns > 0 {
		fmt.Printf("  reasoning_cap=%d", reasoningTurns)
	}
	fmt.Println()

	for run := 1; run <= *runsFlag; run++ {
		if run > 1 && *idleFlag > 0 {
			fmt.Printf("idle %v before pass %d (provider prefix cache eviction gap)...\n", *idleFlag, run)
			time.Sleep(*idleFlag)
		}
		// A fresh session id per pass: run 1 measures the cold-then-warm
		// curve; later runs measure the warm cross-session prefix reuse.
		sessionID := fmt.Sprintf("cachehit-%d-%s-run%d", os.Getpid(), *modeFlag, run)
		fmt.Printf("\n--- pass %d (session %s, mode %s) ---\n", run, sessionID, *modeFlag)

		var results []unit
		var err error
		if *modeFlag == "raw" {
			results, err = runRawPass(ctx, pr, modelID, system, sessionID, *turnsFlag, payloadKB, *warmFlag)
		} else {
			// Shared-memory scenario: pass 1 pins a deterministic goal-state
			// snapshot from its transcript; pass 2 loads it as the leading
			// user message so the provider prefix (system + tools + snapshot)
			// is byte-identical to a warm prefix pass 1 left behind. The
			// snapshot derives deterministically from the transcript, so no
			// LLM call is involved.
			var snapshotText string
			if *sharedMemoryFlag && run > 1 {
				snapshotText = sharedSnapshotLatest
			}
			strategy := resolveStrategy(loaded.Config, *strategyFlag, providerID, modelID)
			runner := agent.New(agent.Config{
				Provider:           pr,
				Model:              modelID,
				System:             system,
				SystemStable:       stableSystemFor(projectRoot),
				MaxTokens:          *maxTokensFlag,
				Tools:              tools.NewRegistry(),
				Cwd:                projectRoot,
				SessionID:          sessionID,
				AgentName:          "cachehit",
				MaxReasoningTurns:  reasoningTurns,
				MaxToolResultBytes: loaded.Config.CacheMaxToolResultBytes,
				SpillBytes:              loaded.Config.CacheSpillBytes,
				CacheRetention:     provider.CacheRetentionLong,
				CacheStrategy:      strategy,
				WarmCache:          *warmFlag,
				WarmTurn:           *warmTurnFlag,
			})
			if *sharedMemoryFlag && run == 1 {
				results, err = runPassWithSnapshot(ctx, runner, *turnsFlag, payloadKB, &sharedSnapshotLatest)
			} else if snapshotText != "" {
				results, err = runPassWithPrefix(ctx, runner, *turnsFlag, payloadKB, snapshotText)
			} else {
				results, err = runPass(ctx, runner, *turnsFlag, payloadKB)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "cachehit:", err)
			os.Exit(1)
		}
		summarize(fmt.Sprintf("pass %d", run), results)
		// Paid-tier client-ceiling assertion: on a paid (non free-flash)
		// model, the client is the only variable — a TOTAL hit rate below the
		// ceiling with resets or unexpected divergences is a client bug, not
		// a provider eviction. Fail the run loudly (harness-style
		// key-gated e2e) so CI can gate on it.
		if *paidFlag {
			var totIn, totRead, totWritten, totResets, totPartial, totDiverge int
			for _, u := range results {
				totIn += u.input
				totRead += u.read
				totWritten += u.written
				totResets += u.resets
				totPartial += u.partial
				totDiverge += u.diverge
			}
			rate := hitPct(totIn, totRead, totWritten)
			var problems []string
			if rate < paidCeilingHitPct {
				problems = append(problems, fmt.Sprintf("hit rate %.2f%% < %.2f%% ceiling", rate, paidCeilingHitPct))
			}
			if totResets > 0 {
				problems = append(problems, fmt.Sprintf("%d cache resets (paid tier must not evict)", totResets))
			}
			if totDiverge > 0 {
				problems = append(problems, fmt.Sprintf("%d prefix divergences (client must be append-only on a paid tier)", totDiverge))
			}
			if len(problems) > 0 {
				fmt.Fprintf(os.Stderr, "\npaid-tier assertion FAILED: %s\n", strings.Join(problems, "; "))
				os.Exit(1)
			}
			fmt.Printf("\npaid-tier assertion passed: TOTAL %6.2f%%, 0 resets, 0 divergences\n", rate)
		}
	}
}

// paidCeilingHitPct is the paid-tier client-ceiling assertion: on a model
// whose provider does not evict the prompt cache mid-session (paid tiers,
// not free flash), rick's append-only prefix + warm/keep-alive should hold
// ~100% of the shared prefix cached. Measured write-weighted (the same
// formula as /stats), matching the free-tier floor (92%) minus the
// provider-side eviction noise the free tier adds.
const paidCeilingHitPct = 98.0

// sharedSnapshotLatest is set by runPassWithSnapshot on pass 1 so pass 2 can
// load the exact snapshot bytes pass 1 pinned.
var sharedSnapshotLatest string

// runPassWithSnapshot runs one pass and pins the deterministic goal-state
// snapshot derived from the pass's transcript for a later pass to reuse.
func runPassWithSnapshot(ctx context.Context, runner *agent.Runner, turns, payloadKB int, latest *string) ([]unit, error) {
	results, err := runPass(ctx, runner, turns, payloadKB)
	if err != nil {
		return results, err
	}
	*latest = deterministicSnapshot(taskSteps, payloadKB, turns)
	return results, nil
}

// runPassWithPrefix runs one pass whose history starts with a pinned
// snapshot message, so the provider prefix is byte-identical to the prefix a
// previous pass left warm.
func runPassWithPrefix(ctx context.Context, runner *agent.Runner, turns, payloadKB int, snapshot string) ([]unit, error) {
	results := make([]unit, 0, turns)
	var history []provider.Message
	history = append(history, provider.UserText(snapshot))
	var shown []float64
	fmt.Printf("  %-4s %8s %10s %12s %10s %8s %8s\n", "turn", "hit%", "input", "cache_read", "cache_write", "resets", "diverge")
	for i := 0; i < turns; i++ {
		step := taskSteps[i%len(taskSteps)]
		history = append(history, provider.UserText(step))
		if payloadKB > 0 {
			history = append(history, provider.UserText(payload(i, payloadKB)))
		}
		used, appended, err := runTurnSafe(ctx, runner, history)
		if err != nil {
			return results, fmt.Errorf("turn %d: %w", i+1, err)
		}
		history = append(history, appended...)
		results = append(results, used)
		turn := i + 1
		if turn%5 == 0 {
			hit := hitPct(used.input, used.read, used.written)
			shown = append(shown, hit)
			fmt.Printf("  %-9d %7.2f%% %10d %12d %10d %8d %8d\n", turn, hit, used.input, used.read, used.written, used.resets, used.diverge)
		}
	}
	if len(shown) > 0 {
		var sum float64
		for _, hit := range shown {
			sum += hit
		}
		fmt.Printf("\n  average hit%% across shown turns: %6.2f%%\n", sum/float64(len(shown)))
	}
	return results, nil
}

// deterministicSnapshot derives the deterministic goal-state snapshot for the
// task-step transcript shape the benchmark drives, so pass 2's leading
// message matches pass 1's pinned bytes exactly.
func deterministicSnapshot(steps []string, payloadKB, turns int) string {
	likes := make([]memory.MessageLike, 0, len(steps)*2)
	for i := 0; i < turns; i++ {
		likes = append(likes, memory.MessageLike{Role: "user", Text: steps[i%len(steps)]})
		if payloadKB > 0 {
			likes = append(likes, memory.MessageLike{Role: "user", Text: payload(i, payloadKB)})
		}
		likes = append(likes, memory.MessageLike{Role: "assistant", Text: "done"})
	}
	return memory.Derive(likes, memory.Options{}).Text
}

// resolveStrategy resolves the named cache_strategies config entry for a
// provider/model route, or nil when none is configured.
func resolveStrategy(cfg config.Config, strategyName, providerID, modelID string) provider.CacheStrategy {
	if strategyName == "" {
		return nil
	}
	name, retention, ttlSeconds, keepaliveSeconds, warm, warmTurn, passback, maxReasoning, divergence := cfg.CacheStrategyFor(providerID, modelID)
	if name == "" && strategyName != "" {
		// The flag names a route; look up the raw entry.
		if entry, ok := cfg.CacheStrategies[strategyName]; ok {
			name = entry.Name
			retention = entry.Retention
			ttlSeconds = entry.TTLSeconds
			keepaliveSeconds = entry.KeepaliveSeconds
			if entry.Warm != nil {
				warm = *entry.Warm
			}
			if entry.WarmTurn != nil {
				warmTurn = *entry.WarmTurn
			}
			if entry.PassbackReasoning != nil {
				passback = *entry.PassbackReasoning
			}
			maxReasoning = entry.MaxReasoningTurns
			divergence = entry.DivergenceReason
		}
	}
	if name == "" && retention == "" && ttlSeconds == 0 && keepaliveSeconds == 0 && !warm && !warmTurn && !passback && maxReasoning == 0 && divergence == "" {
		return nil
	}
	return provider.DefaultStrategy{
		NameVal:         name,
		RetentionVal:    provider.CacheRetention(retention),
		TTLVal:          time.Duration(ttlSeconds) * time.Second,
		KeepaliveVal:    time.Duration(keepaliveSeconds) * time.Second,
		WarmVal:         warm,
		WarmTurnVal:     warmTurn,
		PassbackVal:     passback,
		MaxReasoningVal: maxReasoning,
		DivergenceVal:   divergence,
	}
}

// stableSystemFor composes the byte-stable prefix shared across all turns of
// a pass: the base build prompt plus project instructions. The volatile
// environment and tool manifest stay in the caller's full system string.
func stableSystemFor(projectRoot string) string {
	project := agent.ProjectContext(projectRoot, nil)
	stable := agent.BuildPrompt
	if project != "" {
		stable += project + "\n"
	}
	return stable
}
