package agent

import (
	"context"
	"strings"
	"testing"

	"rick/internal/budget"
	"rick/internal/provider"
	"rick/internal/tools"
)

// TestRunRefusesUnexpectedMidPrefixDivergence pins the pre-send structural
// invariant: a provider view that stops matching the previous turn at a
// mid-prefix position, with no declared transform to explain it, must fail
// closed instead of re-billing the provider cache cold. Only the deliberate
// at-most-once rewrites (head-trim, distill, reasoning-cut, tool-prune) are
// allowed through.
func TestRunRefusesUnexpectedMidPrefixDivergence(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})

	runner := New(Config{
		Provider: &trimWarmProvider{},
		Model:    "work-model",
		System:   "You are rick, a terse coding agent.",
		Tools:    registry,
	})
	// Prime the previous-view hashes with a first message that the next Run
	// will rewrite at position 0 — an undeclared mid-prefix change.
	prime := runner.buildRequest([]provider.Message{provider.UserText("original-head")}, registry.Schemas(nil))
	runner.trackPrefix(prime)
	runner.lastMutation = ""

	events := make(chan Event, 16)
	_, err := runner.Run(context.Background(),
		[]provider.Message{provider.UserText("rewritten-head")},
		events)
	if err == nil || !strings.Contains(err.Error(), "diverged mid-prefix") {
		t.Fatalf("Run returned err=%v, want a mid-prefix divergence failure", err)
	}
}

// TestRunEpochHashStableAcrossTurnsAndResume pins the epoch-scoped cache key:
// the same frozen header derives the same hash on every turn and across a
// resume (new session id), so the provider routes a restarted session to the
// same warm cache bucket.
func TestRunEpochHashStableAcrossTurnsAndResume(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})
	schemas := registry.Schemas(nil)

	runner := New(Config{Model: "deepseek-v4-flash", System: "stable head", SystemStable: "stable head", Tools: registry})
	req1 := runner.buildRequest([]provider.Message{provider.UserText("turn 1")}, schemas)
	req2 := runner.buildRequest([]provider.Message{provider.UserText("turn 1"), provider.UserText("turn 2")}, schemas)
	if req1.EpochHash == "" {
		t.Fatal("epoch hash is empty")
	}
	if req1.EpochHash != req2.EpochHash {
		t.Fatalf("epoch hash changed across turns: %q vs %q", req1.EpochHash, req2.EpochHash)
	}

	// A resumed session with an identical frozen header but a new session id
	// derives the same hash — the cross-session warm reuse.
	resumed := New(Config{Model: "deepseek-v4-flash", System: "stable head", SystemStable: "stable head", Tools: registry, SessionID: "new-session"})
	req3 := resumed.buildRequest([]provider.Message{provider.UserText("resumed")}, schemas)
	if req1.EpochHash != req3.EpochHash {
		t.Fatalf("resumed session epoch hash diverged: %q vs %q", req1.EpochHash, req3.EpochHash)
	}
}

// TestUsageAnchoredDistillFiresOnMeasuredOccupancy pins the usage-anchored
// distill baseline: once the provider has reported a real footprint, the fold
// decision fires on that measured occupancy instead of the byte estimate.
func TestUsageAnchoredDistillFiresOnMeasuredOccupancy(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})

	// A tiny byte-estimated plan: well below any threshold.
	plan := budget.Result{ContextWindow: 24_000, TotalInputTokens: 500}

	r := New(Config{
		Model:              "deepseek-v4-flash",
		System:             "sys",
		Tools:              registry,
		ContextWindow:      24_000,
		EnableDistillation: true,
		DistillSummarizer:  &recSumm{},
	})
	r.lastUsageTokens = 15_000     // 62.5% of 24k: crosses the default 55% fold.
	r.lastCacheReadTokens = 10_000 // the provider reported real cache reads, so usage-anchored occupancy is trustworthy
	if !r.shouldDistill(plan, 24_000) {
		t.Fatalf("usage-anchored occupancy (%d) should trigger distillation", r.lastUsageTokens)
	}

	// A provider that omits cache metrics (DeepSeek reports no cache-write;
	// some gateways report no cache at all) must not fire distillation on a
	// garbage zero: the byte-estimated plan carries the decision instead.
	r3 := New(Config{
		Model:              "deepseek-v4-flash",
		System:             "sys",
		Tools:              registry,
		ContextWindow:      24_000,
		EnableDistillation: true,
		DistillSummarizer:  &recSumm{},
	})
	r3.lastUsageTokens = 15_000
	r3.lastCacheReadTokens = 0 // no cache metrics reported
	if r3.shouldDistill(plan, 24_000) {
		t.Fatal("usage-anchored distill must not fire when the provider reported no cache reads")
	}

	// No measured usage yet: falls back to the byte-estimated plan.
	r2 := New(Config{
		Model:              "deepseek-v4-flash",
		System:             "sys",
		Tools:              registry,
		ContextWindow:      24_000,
		EnableDistillation: true,
		DistillSummarizer:  &recSumm{},
	})
	if r2.shouldDistill(plan, 24_000) {
		t.Fatal("byte-estimated plan below threshold must not distill without measured usage")
	}
}

// TestPerModelDistillThreshold pins the per-model policy override: a
// configured threshold percent overrides the package default (55) for that
// run's model.
func TestPerModelDistillThreshold(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})

	// Byte-estimated occupancy of 12k/24k = 50%: above a configured 40% fold,
	// below the default 55%.
	plan := budget.Result{ContextWindow: 24_000, TotalInputTokens: 12_000}

	r := New(Config{
		Model:                   "deepseek-v4-flash",
		System:                  "sys",
		Tools:                   registry,
		ContextWindow:           24_000,
		EnableDistillation:      true,
		DistillThresholdPercent: 40,
		DistillSummarizer:       &recSumm{},
	})
	if !r.shouldDistill(plan, 24_000) {
		t.Fatal("per-model threshold 40% should fold at 50% occupancy")
	}
}

// TestCheckPriorEpochDetectsHeaderDrift pins the resume header-drift guard:
// a resumed session whose durable header no longer matches the freshly built
// one (repo-map block changed with cwd, model switched, tools changed) is
// detected by hash mismatch and attributed to the drifting field.
func TestCheckPriorEpochDetectsHeaderDrift(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})
	schemas := registry.Schemas(nil)

	base := New(Config{Model: "deepseek-v4-flash", System: "stable head", SystemStable: "stable head", Tools: registry})
	req := base.buildRequest([]provider.Message{provider.UserText("turn 1")}, schemas)
	prior := base.epochHeader(req.System, req.SystemStable, schemas)

	// Identical resume: no drift.
	same := New(Config{Model: "deepseek-v4-flash", System: "stable head", SystemStable: "stable head", Tools: registry, PriorEpoch: &prior})
	if reason := same.checkPriorEpoch("stable head", "stable head", schemas); reason != "" {
		t.Fatalf("identical resume reported drift: %q", reason)
	}

	// System drift (repo-map block changed with cwd).
	sysDrift := New(Config{Model: "deepseek-v4-flash", System: "stable head changed", SystemStable: "stable head changed", Tools: registry, PriorEpoch: &prior})
	if reason := sysDrift.checkPriorEpoch("stable head changed", "stable head changed", schemas); reason == "" {
		t.Fatal("system drift not detected")
	}

	// Model drift.
	modelDrift := New(Config{Model: "gpt-5", System: "stable head", SystemStable: "stable head", Tools: registry, PriorEpoch: &prior})
	if reason := modelDrift.checkPriorEpoch("stable head", "stable head", schemas); reason != "model:deepseek-v4-flash->gpt-5" {
		t.Fatalf("model drift reason = %q, want model:deepseek-v4-flash->gpt-5", reason)
	}

	// No prior epoch: no drift reported.
	none := New(Config{Model: "deepseek-v4-flash", System: "stable head", SystemStable: "stable head", Tools: registry})
	if reason := none.checkPriorEpoch("stable head", "stable head", schemas); reason != "" {
		t.Fatalf("no prior epoch reported drift: %q", reason)
	}
}

// TestResumeDriftLatchForcesRewarm pins the fail-closed pre-flight guard: a
// resumed session whose header drifted must re-warm before the first real
// turn even when general warming (WarmCache) is disabled — the drift itself
// invalidated the prefix the session had warm.
func TestResumeDriftLatchForcesRewarm(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(canonicalOutputTool{output: `{"rows":[]}`})
	schemas := registry.Schemas(nil)

	// Build the prior epoch from a session with a different system prompt.
	priorRunner := New(Config{Model: "deepseek-v4-flash", System: "old stable head", SystemStable: "old stable head", Tools: registry})
	priorReq := priorRunner.buildRequest([]provider.Message{provider.UserText("turn 1")}, schemas)
	prior := priorRunner.epochHeader(priorReq.System, priorReq.SystemStable, schemas)

	warmCalls := 0
	prov := &trimWarmProvider{warmCalls: &warmCalls}
	runner := New(Config{
		Provider:     prov,
		Model:        "deepseek-v4-flash",
		System:       "new stable head",
		SystemStable: "new stable head",
		Tools:        registry,
		WarmCache:    false, // general warming off; the drift-triggered warm must still fire
		PriorEpoch:   &prior,
	})
	events := make(chan Event, 32)
	_, err := runner.Run(context.Background(), []provider.Message{provider.UserText("resumed")}, events)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// The P1c warm must have fired on the first turn despite WarmCache=false:
	// the resume header drift invalidated the cached prefix, and priming the
	// new header is the only way to avoid re-billing the whole tail cold.
	if warmCalls == 0 {
		t.Fatal("resume header drift did not latch the mandatory re-warm")
	}
}
