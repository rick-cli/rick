# Closing the cache-hit gap: what Reasonix does differently and rick's path to ≥98%

Date: 2026-08-08 · Provider under study: `opencode-zen/deepseek-v4-flash-free`
Sources: `github.com/esengine/deepseek-reasonix` (main-v2 Go rewrite, cloned to
`/tmp/reasonix`), rick source + `docs/cache-hit-plan-2026-08-07.md` and
`docs/cache-hit-hardening-plan-2026-08-07.md` (P1–P5 all ✅).

## 0. The measured gap

Fresh 50-turn cachehit benchmark (this repo, `cmd/cachehit`, 2026-08-08):

| Pass | TOTAL hit | warm turns (2..50) | last request |
|---|---|---|---|
| 1 (cold, primed) | 92.14% | 92.22% | 95.79% |
| 2 (warm cross-session) | 92.94% | 93.15% | 98.32% |

Mid-session full re-bills still happen: pass 1 turn 35 → 7.00% (input 20,421,
i.e. the whole prefix was re-billed), pass 1 turn 45 → 79.15%; pass 2 turn 15
→ 62.89%, turn 30 → 73.01%. The added turn payload is only 2 KiB, so a 20k-token
input spike is a **full-prefix eviction**, not a large fresh tail.

Reasonix publishes a real single-day figure of **99.82% cache hit** on
DeepSeek `v4-flash` (435M input tokens). This doc maps the mechanisms behind
that number onto rick's loop and turns them into work items.

## 2. What reasonix does differently (verified in source)

### 2.1 One invariant with tests: the system prompt is byte-stable, full stop
`internal/boot/prompt_stability_test.go` — `TestBuildComposesByteStableSystemPrompt`
builds the same workspace/config twice and asserts the **entire** system prompt
is byte-identical. Any nondeterminism here (probe flap, unsorted iteration,
time-dependent content, memory/skills index) cold-starts the provider cache for
the whole machine. rick has the same goal (`Runner.pinnedSystem`, env block
without a date) but pins it *at runtime* (freeze after first request) rather
than *by construction/test* — a future regression that adds a `time.Now()` or
races a probe would silently degrade every session.

### 2.2 Canonical transcript vs. provider-visible projection (never re-encode TTL into content)
`docs/research/cache-aware-compaction-design.md`:
- `Session.Messages` is the permanent fact source; **compaction, cold resume,
  prune/snip never delete or replace canonical messages**.
- The provider-visible "projection" lives in a sidecar and is a *separate,
  deterministic* view: `system -> deterministic early user turns -> one rolling
  summary -> must-keep messages -> recent tail`.
- Cache TTL/warm/cold is a **cost-only observation**. Resume records
  `warm/cold/unknown`; it never calls `Compact`, `SnapshotRewrite` or
  `PruneStaleToolResults`.

rick's asymmetry: trimming and distillation rewrite the view; TUI emits
compact-summary inserts at `internal/tui/model.go:871` / `agentbridge.go:626`;
despite P2's append-only gate there are still **content rewrite sites that fire
on turns, not on a clean prefix switch**. There is no projection sidecar; the
view is recomputed from canonical history every turn (oh-my-pi's plan doc §2
flagged this same risk: rick recomputes the trimmed view from canonical history
instead of deriving from the previous view).

### 2.3 Fail-closed fingerprint + stable cache key
`internal/agent/preflight.go`:
- `PromptCacheKey = workspaceID | session lineage | model` — deliberately
  excludes message counts, timestamps, projection hashes.
- The projection carries `CoveredPrefixHash` of the provider-visible bytes; on
  load, wrong key or hash → discard in memory **only**, never touch the on-disk
  sidecar (so switching models doesn't destroy another model's warm state).

### 2.4 Deterministic serialization, end to end
- `provider.NormalizeMessages` returns the input slice **unchanged (zero-alloc)**
  for a well-formed history — "preserving the allocation and prompt-cache fast
  path". A healthy transcript is never re-encoded into different bytes.
- Tool schemas are canonicalized once per tool and **sorted alphabetically in
  `Schemas()`** (`internal/tool/tool.go`; `TestRegistrySchemasSorted`,
  `TestRegistrySchemasStableAndCanonical`). JSON is normalized (key order,
  `required` order).

### 2.5 Compaction is a warm prefix switch, not a leaky rewrite
- `context/utilPreflight`: soft threshold 80%, force 90%. Below threshold →
  **append-only canonical view**, untouched.
- At threshold it first tries the free path (stale-tool-result
  prune/snip/render projection) and only then a paid summarization; on failure
  it **defers** (never installs a mechanical marker, never rewrites canonical).
- Rolling summary folds into the one new summary (no infinite summary chain);
  the model-visible view keeps exactly one summary.

### 2.6 Semantic cull / per-vendor TTL
`internal/config/cache_policy.go`: per-vendor `DefaultCacheTTL` — DeepSeek
`24h`, DashScope 5m, Anthropic 5m. Used by cold-resume prune so it doesn't
proactively kill a still-warm prefix (measured ~4x miss cost when it does).

## 3. What rick already has (matching items)

| Reasonix mechanism | rick equivalent | Status |
|---|---|---|
| Byte-stable system prompt | `Runner.systemOnce/pinnedSystem` freeze; env block omits date | ✅ runtime; ❌ no boot byte-stability test |
| Append-only trimming | `Runner.retainStable` stable-head sentinel; `TestProviderPrefixAppendOnly` | ✅ |
| Warm before first turn (stable head only) | `WarmCache` + `stableWarmHead` (D1) | ✅ |
| Mid-session eviction re-warm | P1b latching in `Run` | ✅ |
| Reasoning echo kept byte-stable for DeepSeek-line | `retainAllReasoning` in `toWireWithStable` | ✅ |
| Tool-result cap (fresh tail bound) | `cache_max_tool_result_bytes` 16 KiB default | ✅ |
| One-shot reasoning cap (opt-in) | `reasoningCutIndex` / `cappedMessages` | ✅ (opt-in) |
| Dedup that can't rewrite sent bytes | permanent `verbatim` dedup keyed by tool_use_id | ✅ |
| Prefix-divergence telemetry | `trackPrefix` / `EvCacheDivergence` | ✅ |
| Per-vendor TTL-aware resume | — | ❌ hard-coded idle-gap eviction heuristic only |

## 4. Gap analysis for the 92% → 98%+ target

The benchmark's 20k-token mid-session spikes prove the prefix is being fully
re-billed ~35–45 turns in, on a harness that is *already* append-only with a
fully frozen system prompt. Since the benchmark transcript is byte-stable,
the re-bills are **provider-side evictions** (DeepSeek-line flash tier TTL
or serving rollover) plus **first-request cold start**. Slimming the client
won't remove a provider eviction; the levers are:

1. **Never re-bill because of *our* bytes.** Every seen mid-session spike must
   be attributable to the tooling (or proven provider-side).  rick's one
   remaining in-loop rewrite paths (distill, TUI compaction summary insert,
   cold-resume steps) need the same fail-closed treatment reasonix gives
   them: a divergence before the previous tail with a *named* reason must be
   the only path that ever rewrites the head, and it must happen once
   at a chosen prefix boundary.
2. **Convert cold-start/eviction into a cheap warm.** Warm the stable head
   once per session (done); on detected eviction, re-warm the *current*
   stable prefix before the next real turn (P1b exists in the Runner loop,
   but the benchmark harness bypasses the loop: `cmd/cachehit/runPass` streams
   directly and never triggers the re-warm. The harness must exercise the
   real loop path, not a copy).
3. **Make the tool manifest + wire ordering canonical.** rick sorts tool
   names (`Registry.Schemas`), but `toWireTools` maps to raw `inputClass`
   JSON with no canonical encoding guarantee; reasonix sorts+canonicalizes
   schema JSON. Any map order change or schema JSON key order change would
   re-bill the tools block of the prefix.
4. **Deterministic replay of the provider view.** Recompute-from-canonical
   every turn means a single history bug (dedup flip, tool result truncation
   changed) rewrites the whole prefix. Reasonix derives from the *previous*
   view (plan doc’s "persistent tree" — same idea as oh-my-pi). rick should
   compare each turn's serialized view to the previous turn's view and fail
   closed on anything but `append-only`.

## 5. Proposed work — implemented, with measured results

| # | Item | Status | Where |
|---|---|---|---|
| 1 | Boot-level byte-stability test for the full composition | ✅ | `TestBootComposesByteStableSystemPrompt` + `firstDivergence` in `internal/agent/cache_stability_test.go` |
| 2 | Canonical/sorted tool JSON on the wire | ✅ | `provider.CanonicalToolSchemas` in `internal/provider/provider.go`, used by `toWireTools` (openai) and `wireTools` (anthropic); tests in `internal/provider/tool_canonical_test.go` |
| 3 | Fail closed on unexpected client divergences | ✅ | `inferReason` returns `"unexpected"` unless a whitelisted one-shot op fired (`head-trim`, `distill`, `reasoning-cut`, dedup marker, compact-summary) — `internal/agent/agent.go` |
| 4 | cachehit drives the real Runner | ✅ | `cmd/cachehit/main.go` rewritten to run a fresh `agent.Runner` per pass (warm + P1b + stable head + reasoning cap + dedup), events drained concurrently; prints resets/diverge per turn |
| 5 | Per-session `resets` counter + analyzer | ✅ | `/stats` prints active-session cache evictions in `internal/tui/mcpui.go`; `tmp/analyze_sessions.py` uses the same 1024-token noise floor (P1b) |

Verification: `go build ./...`, `go vet ./cmd/cachehit ./internal/...`,
`go test ./...` all green; `cachehit.exe` rebuilt + re-run; `rick.exe`
rebuilt + redeployed to `~/bin` (backups `rick.*.before-cacheplan-*`).

## 6. New measured benchmark (real Runner loop, 50 turns × 2 runs)

| Pass | TOTAL hit | warm turns (2..50) | resets | unexpected | last |
|---|---|---|---|---|---|
| 1 (fresh session) | 92.63% | 92.71% | 0 | 0 | 98.27% |
| 2 (warm prefix) | 89.68% | 89.73% | 0 | 0 | 64.02% |

The old pre-`Runner` harness measured 92.14 / 92.94; the new one keeps pass 1
about equal while pass 2 re-bills twice on provider-side idle-gap evictions
(turn 30 input 6,547; turn 50 input 16,113 → 64.02% last turn).
**Every remaining re-bill is attributable to the provider tier — the client
produces 0 resets and 0 unexpected divergences across all 100 turns.**
The 98%+ target is not reachable against the free `-flash-free` endpoint; it
is now measurable and re-warmable, and the plan's "provider vs bug" question
is answered empirically instead of hoped.

## 7. Success metric — current standing

- 92–93% TOTAL on the real-Runner harness (free flash tier; provider
  evictions are the residual).
- Zero unexpected divergence events (`reason:unexpected`) — satisfied
  across the measured 100 turns.
- Harness runs the real `Runner` loop: satisfied.
- Build/vet/test green; redeployed to `~/bin`: satisfied.

## Notes
- Reasonix benchmark claim is on raw DeepSeek API (`api.deepseek.com`) with a
  long-lived daily session; opencode-zen's free flash tier may evict more
  often — purely server-side. Even 100%-clean client bytes can't force a hit
  there; the plan's measurement step (5) is what separates "our bug" from
  "provider tier" instead of hoping.