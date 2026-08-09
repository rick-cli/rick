# rick vs. the field: harness comparison & forward plan

**Date:** 2026-08-08
**Subject:** `rick` (G:\projectE) compared against other terminal AI coding-agent harnesses
**Method:** local codebase analysis (rick, plus locally installed harnesses) + GitHub research
(fetch of repo READMEs/docs/APIs). Web search quota capped at 10/session; GitHub REST API
and raw fetches used for the rest. All star counts from shields.io / GitHub API on 2026-08-08.

---

## 0. TL;DR

rick is a **Go, single-static-binary, Bubble Tea TUI agent** with an unusually deep
engineering investment in **prompt-cache stability** (byte-pinned system prompt, append-only
provider view, per-request divergence telemetry, its own `cmd/cachehit` benchmark), a
**resilient multi-provider websearch**, **13+ provider catalog**, **headless NDJSON daemon**
(`rickserve`), **swarm/team/parallel multi-agent orchestration**, **goal tracking with token
budgets**, a **glob-based allow/ask/deny permission engine**, and **shadow-git snapshot undo**.
Where it lags the market leaders (opencode, Codex, oh-my-pi, Hermes) is **IDE integration
(LSP/DAP), browser automation, cross-session learning/memory ("taste"), ecosystem/community
surface, and auto-checkpoint git workflows** — none of which are architectural gaps; they are
additions that slot into existing extension points (`internal/mcp`, `internal/plugin`,
`internal/agent`, `cmd/rickserve`).

---

## 1. How the comparison was done

| Harness | Source | How obtained |
|---|---|---|
| rick | G:\projectE (local, ~950 KB source) | Direct codebase read: `internal/`, `cmd/`, `pkg/`, README.md, RICK.md, CHANGELOG.md, docs/ |
| oh-my-pi | github.com/can1357/oh-my-pi | GitHub API + raw README + rick's own research docs |
| reasonix | github.com/esengine/deepseek-reasonix | GitHub search API + README + rick's `docs/cache-hit-reasonix-plan-2026-08-08.md` (deep source study already done) |
| OpenAI Codex CLI | github.com/openai/codex | Raw README + repo docs listing + blob API |
| opencode | github.com/sst/opencode | Raw README + general knowledge cross-checked against rick docs |
| Command Code | commandcode.ai + local `~/.commandcode` | Site fetch + local config/history (`deepseek/deepseek-v4-pro`, `reasoningEffort`, provider `command-code`) |
| Pi (upstream of oh-my-pi) | bcfg/pi-mono | Referenced via oh-my-pi README + rick docs (repo private/renamed — API returned empty) |
| Hermes Agent | local `~/research_workspace/hermes-agent` (NousResearch) | Local README/AGENTS.md |
| ZCode | GLM ecosystem | rick's `docs/cache-hit-plan-2026-08-07.md` |

**Population (GitHub stars, 2026-08-08):** Hermes ~227k · opencode ~195k · Codex ~105k ·
reasonix ~33k · oh-my-pi ~23k. rick is a private/personal harness (not on the public leaderboard).

---

## 2. rick profile (what it is, from source)

- **Runtime:** Go 1.24, module `rick`, single static exe (~61.7 MB). No Node/Python runtime dependency for the core.
- **UI:** Bubble Tea TUI — leader key (`ctrl+x`), fuzzy `@` file picker, `!cmd` shell escape,
  slash commands, 4 built-in themes + JSON theme system, input history, transcript scroll,
  tool-detail view.
- **Providers (`internal/provider/catalog.go`):** openai, anthropic, openrouter, zai (GLM),
  deepseek, xai, gemini, groq, mistral, together, opencode-zen, azure, ollama; OAuth + probe +
  model filter; small model for compaction.
- **Tools (`internal/tools/`):** read, write, edit (exact-string w/ whitespace-tolerant
  fallback, refuses unread files), bash, grep, glob, list, apply_patch (atomic multi-file
  patch), todowrite/todoread, task (subagents), plus code_symbols, websearch (10+ backends:
  ddg/bing/brave/exa/google_cse/jina/firecrawl/arxiv/gdelt/hackernews/github/crossref with
  health tracking, budgets, in-flight dedup), fetch, retrieve (context decompression),
  memo, goal tools, swarm/team/parallel orchestration, and every MCP tool registered.
- **Permissions (`internal/permission/`):** allow/ask/deny; glob patterns matched per
  sub-command of compound bash lines; most-specific-wins; outside-project writes auto-upgrade
  to ask; session-scoped grants never written to disk.
- **Safety (`internal/sandbox/`, `internal/security/`):** sandboxed exec per-OS
  (`exec_windows.go`, `exec_linux.go`, `exec_darwin.go`, token/session handling), security
  audit log.
- **Sessions/undo (`internal/session/`):** per-directory resumable sessions,
  **shadow-git snapshot undo/redo** (outside the real repo), snapshot pruning, session
  pruning, archives of head-trimmed messages.
- **Context/cache engineering (the differentiator):**
  - system prompt frozen byte-exact after first request (P1, `Runner.pinnedSystem`);
  - append-only provider view, one pinned head-trim sentinel + pinned first user turn
    (`history.DropFirstGroups` / `retainStable`);
  - canonical sorted tool JSON on the wire (`provider.CanonicalToolSchemas`);
  - per-request `divergence` + `resets` telemetry persisted in session `requests[]`;
  - cache warm request byte-parity (`cache_warm`), per-vendor cache TTL
    (`provider.DefaultCacheTTL` — DeepSeek 1d vs Anthropic 5m/1h);
  - `cmd/cachehit` benchmark driving the real `Runner` loop; measured **92–93% hit, 0 resets,
    0 unexpected divergences / 100 turns** on the free `-flash-free` tier;
  - CI gate `scripts/check-cache-guard.sh` + `Cache-impact:` PR label.
- **Agents:** built-in `build`/`plan` + user-defined Markdown agents; `general`/`explore`
  subagents with depth cap; **swarm/team/parallel** (board, coordinator, taskboard); agent
  registry with chat/steer/kill (docs/multi-agent-plan.md).
- **Goals:** goalwrite/goalread/goalstep/goalabort with token budgets (`internal/goal/`).
- **Headless:** `cmd/rickserve` — NDJSON daemon (run, permission-response, sessions, models,
  tools, config, snapshots, goals, compact, mcp, plugins, agents, ping) for editors/CI/desktop.
- **Ops:** rickdoctor, rickverify, ricksec/ricksecurity, ricke2e, rickauth, maintenance
  (snapshot/session pruning), `rick update`/`uninstall`.
- **Extensibility:** MCP client (stdio+http), plugin hook dispatcher with script + **skills**
  (`internal/plugin/skill.go`), custom slash commands (`.rick/commands/*.md`), JSONC config
  with `{env:}`/`{file:}` substitution and generated JSON schemas (`rick.json.schema.json`,
  `tui.json.schema.json`).

---

## 3. Harness profiles

### 3.1 oh-my-pi (can1357/oh-my-pi) — "a coding agent with the IDE wired in"

Fork of **Pi** (bcfg/pi-mono, by @mariozechner). ~23k stars, MIT, TypeScript + **~80k lines of
Rust core** (recent rewrite). Positioned as the most feature-dense agent surface:
**60+ providers · 31 built-in tools · 14 LSP ops · 28 DAP ops · ~80k LOC Rust core.**
Includes **hash-anchored edits**, Python runtime, browser automation, subagents. From rick's
own deep-dive (docs/cache-hit-plan-2026-08-07.md): oh-my-pi enforces an **append-only
provider-view invariant** and derives each provider view from the **previous view**
("persistent tree") so the serialized prompt only ever grows at the tail — the reference
implementation for cache-safe history.

**What it teaches rick:** IDE-grade semantics (LSP + DAP) inside a terminal agent; provider
view as a persistent tree instead of recompute-from-canonical; hash-anchored edits for
drift-proof patching.

### 3.2 reasonix (esengine/deepseek-reasonix) — cache-hit extremism

~33k stars, Go, single static binary, config/plugin-driven. **DeepSeek-native**, engineered
around prefix-cache stability; published real-user day (2026-05-01): **435M input tokens,
99.82% cache hit, ~$12 vs ~$61 uncached** on v4-flash. Four "Pillar 1" mechanisms (verified in
source, documented in rick's `docs/cache-hit-reasonix-plan-2026-08-08.md`):
1. **byte-stable system prompt by construction** (`TestBuildComposesByteStableSystemPrompt`
   asserts the entire prompt is byte-identical across identical builds);
2. **canonical transcript + provider-visible projection sidecar** — compaction/resume/prune
   never mutate canonical messages; cache TTL is a cost-only observation;
3. **fail-closed fingerprint** — `PromptCacheKey = workspaceID | lineage | model`, projection
   carries `CoveredPrefixHash`; wrong key/hash discards in memory only;
4. **deterministic serialization end-to-end** (zero-alloc `NormalizeMessages` on the fast
   path), canonicalized+sorted tool-schema JSON, and a **CI `cache-impact` gate** that forces
   a `Cache-impact:` declaration on any PR touching cache-sensitive paths.

**What it teaches rick:** rick has adopted most of this already (P1–P5 ✅, `cache_stability_test.go`,
`tool_canonical_test.go`, cache-impact workflow). The one mechanism rick has **not** ported is
the **projection sidecar** (§4.4 in rick's own plan doc — see §6 item 4 below).

### 3.3 OpenAI Codex CLI (openai/codex)

~105k stars, Rust, Apache-2.0. The official OpenAI terminal agent; models GPT-5.x-Codex /
o4-mini etc. via the Responses API. Distinguishing features:
- **OS-level sandboxing:** landlock/seccomp (Linux), Seatbelt (macOS), AppContainer (Windows);
- **approval modes:** read-only / auto / full-auto / suggest;
- **`codex exec`** non-interactive scripting mode + exec policies (`docs/execpolicy.md`),
  making it scriptable in CI;
- **autotick/background tick** for long-running tasks;
- **MCP support, AGENTS.md conventions, plugins**;
- git worktree **checkpointing**, desktop app + IDE extensions.
Proprietary-model-centric; third-party models via config but not the core story.

**What it teaches rick:** OS sandbox parity and `exec`-style scriptability are the two
headliners; rick has `-p`/`rickserve` for the latter and per-OS sandbox code, but Codex's
sandbox is a first-class safety surface with approval-mode UX rick could mirror.

### 3.4 opencode (sst/opencode)

~195k stars, TypeScript/Bun, MIT. The most popular open, provider-agnostic terminal agent:
- any model via AI SDK (Anthropic, OpenAI, OpenAI-compatible, Ollama, …);
- Ink-based TUI with **git diff review, undo/redo checkpoints, themes, shareable sessions**;
- built-in **LSP integration** (diagnostics, hover, definitions, references);
- agents (build/plan/custom) + **`task` subagents**; **plugins** (JS hooks), **MCP client and
  server modes**; `AGENTS.md` conventions; `opencode run` + `opencode serve` (web/API);
- strong docs site, Discord, and a plugin ecosystem.

**What it teaches rick:** ecosystem polish (docs site, plugin registry, LSP, checkpoint
commits into the real repo, web dashboard via serve) is what turns a great engine into a
great product.

### 3.5 Command Code (commandcode.ai) — "the agent with taste"

Proprietary; installed locally at `~/.commandcode` (config: provider `command-code`,
model `deepseek/deepseek-v4-pro`, `reasoningEffort` overrides, `firstMessageSent`). Marketing:
"continuously learns your coding taste … ships, fixes, tests, and refactors with the patterns
you keep — and forgets the ones you delete," powered by its **taste-1** model. Publishes
harness-engineering guidance (why open models struggle in coding agents, orchestration for
open-source models). Also acts as a **model gateway** (the user's history shows it exposing a
paid model plan to other agents — the exact use rick's `local`/OpenAI-compatible provider
config covers).

**What it teaches rick:** **personalization as a differentiator** — a per-user taste profile
derived from accepted/rejected edits, learned across sessions. rick has the raw material
(per-session history, memo tool, `internal/plugin/skill.go`) but no taste/learning loop.

### 3.6 Honorable mentions (local / referenced)

| Harness | What it is | Relevance to rick |
|---|---|---|
| **Pi** (bcfg/pi-mono) | The upstream harness oh-my-pi forked | Same family; append-only invariant lineage |
| **ZCode** (GLM) | GLM-oriented, single-purpose, constant-large system prompt, short lanes | ~99.5% cache ratio model — rick's analysis already covers it |
| **Hermes Agent** (NousResearch, ~227k stars) | Self-improving agent: skills-from-experience, FTS5 session memory, cron automations, multi-platform gateway (Telegram/Discord/CLI), local/Docker/SSH backends | Its AGENTS.md states "per-conversation prompt caching is sacred" — same priority as rick; its **skills/memory loop** is the feature rick lacks for cross-session learning |
| **headroom / rtk** (local `~/repos`) | Context-compression proxy libraries (60–95% token reduction, reversible) | Adjacent infra rick already partially does via distill/RepoMapper; could be consumed as MCP |
| **Claude Code** | Anthropic's proprietary reference agent | The UX benchmark for hooks, subagents, and checkpoint UX |

---

## 4. Feature-by-feature comparison

Legend: ● full · ◐ partial · ○ none/absent. rick data from source; others from READMEs/docs (2026-08-08).

| Dimension | rick | oh-my-pi | reasonix | Codex CLI | opencode | Command Code | Hermes |
|---|---|---|---|---|---|---|---|
| Language / runtime | Go, static exe | TS + Rust core | Go, static exe | Rust | TS/Bun | Proprietary | Python |
| Provider breadth | ● 13+ catalog | ● 60+ | ◐ DeepSeek-first | ◐ OpenAI-first | ● any (AI SDK) | ◐ gateway | ● many |
| TUI quality / themes | ● (Bubble Tea, 4+ themes) | ● | ◐ | ● | ● | ● | ◐ (CLI+TUI) |
| Built-in tool count | ● (~20 core + MCP) | ● 31 | ◐ | ◐ | ● | ? | ● |
| LSP (IDE-in-terminal) | ○ | ● 14 ops | ○ | ○ | ● built-in | ◐ | ○ |
| DAP / debugging | ○ | ● 28 ops | ○ | ○ | ○ | ○ | ○ |
| Browser automation | ○ (fetch/websearch only) | ● | ○ | ◐ plugins | ○ | ? | ● |
| Python/runtime tool | ○ | ● | ○ | ◐ | ○ | ? | ● |
| Subagents | ● general/explore + depth cap | ● | ◐ | ◐ | ● `task` | ◐ | ● delegates |
| Multi-agent swarm/team | ● swarm/team/parallel + registry | ◐ | ○ | ○ | ◐ | ○ | ◐ |
| Goals w/ token budget | ● goalwrite/step/abort | ○ | ○ | ○ | ○ | ○ | ◐ |
| Sessions / resume | ● per-dir, auto | ● | ● | ● | ● shareable | ◐ | ● cross-platform |
| Undo/redo | ● shadow-git snapshots | ◐ | ○ | ● worktrees | ● checkpoints | ◐ | ◐ |
| Real-repo auto-checkpoint commits | ○ | ○ | ○ | ● | ● | ? | ◐ |
| Permission engine (allow/ask/deny) | ● globs, compound cmds | ● | ◐ | ● approval modes | ● rules | ◐ | ◐ |
| OS sandboxing | ◐ per-OS exec sandbox | ◐ | ○ | ● landlock/Seatbelt/AppContainer | ○ | ◐ | ◐ (Docker/SSH backends) |
| Prompt-cache engineering | ● byte-stable, append-only, telemetry, CI gate | ● append-only + persistent tree | ●★ 99.82% claim | ◐ | ◐ | ? | ● "cache is sacred" |
| Provider-visible projection sidecar | ○ (planned §4.4) | ● | ● | ○ | ○ | ○ | ○ |
| Compaction / distillation | ● /compact, distill, live-zone | ● | ● | ● auto-compact | ◐ | ◐ | ● |
| Websearch resilience | ● 10+ backends, health/budget | ◐ | ○ | ○ | ◐ | ○ | ◐ |
| MCP | ● client stdio+http | ● | ◐ | ● | ● client+server | ◐ | ● |
| Plugins / hooks | ● + skills | ● | ● | ● | ● | ◐ | ● skills |
| Headless / scripting mode | ● rickserve NDJSON + `-p` | ◐ | ◐ | ● `codex exec` | ● run/serve | ◐ | ● |
| AGENTS.md conventions | ◐ (prompt.go) | ● | ◐ | ● | ● | ? | ● |
| Config schema (JSON Schema) | ● generated schemas | ◐ | ◐ | ● | ◐ | ? | ◐ |
| Learning / taste / memory | ◐ memo + skills, no loop | ◐ | ○ | ○ | ○ | ● taste-1 | ●★ skills+memory loop |
| Ops tooling (doctor/verify/audit/bench) | ● rickdoctor, cachehit, audits | ◐ | ● cache-impact CI | ◐ | ◐ | ◐ | ◐ |
| Ecosystem / docs / community | ○ (private) | ● | ● | ● | ●★ | ◐ | ●★ |
| Stars (2026-08-08) | — (private) | ~23k | ~33k | ~105k | ~195k | — | ~227k |

---

## 5. Where rick already wins (unusual strengths)

1. **Cache engineering discipline is best-in-class.** Byte-stable system prompt by test,
   canonical tool JSON, append-only view with a single pinned head-trim, fail-closed
   divergence inference, per-request telemetry, and a benchmark (`cmd/cachehit`) that drives
   the **real** Runner — this is the exact playbook reasonix publishes, and rick is the only
   other harness in this set with a CI gate (`Cache-impact:` workflow) enforcing it.
2. **Single static binary, zero runtime deps** (vs TS/Bun, Rust toolchain, Python). Small
   attack surface, trivially deployable, Windows-native (unlike most of the field).
3. **Provider breadth + a genuinely resilient websearch** (10+ backends with health tracking,
   budgets, in-flight dedup — `internal/tools/websearch_*`). Most harnesses have one or two
   backends or none.
4. **Orchestration depth:** swarm/team/parallel + agent registry + goals with token budgets is
   ahead of opencode/Codex/reasonix; multi-agent-plan.md already sketches chat/steer/kill UX.
5. **Ops and self-inspection:** rickdoctor, rickverify, ricksec, audits, `maintenance prune`,
   and the shadow-git undo design (never touches the user's real git history) is safer than
   opencode's in-repo checkpoints.
6. **Headless daemon parity:** rickserve's NDJSON protocol (permission callbacks, goals,
   agents, snapshots) matches codex exec/opencode serve feature-for-feature.

---

## 6. What rick should do better — and how to achieve it

Priority (P0 = differentiator/competitive gap, P1 = important, P2 = polish).

| # | Area | rick today | What to improve | How to achieve it (concrete) |
|---|---|---|---|---|
| P0-1 | **LSP integration (IDE-in-terminal)** | None — oh-my-pi ships 14 LSP ops, opencode ships LSP built-in | Diagnostics, hover, go-to-def, references, rename inside the agent loop | Add an LSP client module (`internal/lsp/`) or, faster, ship a default **MCP server** for LSP (e.g. `mcp-lsp` / `@modelcontextprotocol/server-lsp`) wired into `internal/mcp/manager.go` defaults; expose `lsp_diagnostics`/`lsp_symbols` tools (pattern: `internal/tools/code_symbols.go` already exists — extend it with an LSP backend). Target: diagnostics auto-attached to `edit` results, hover available on demand |
| P0-2 | **Cross-session learning ("taste" / memory loop)** | `memo` tool + `internal/plugin/skill.go` exist but no learning loop; Command Code's whole pitch is taste-1; Hermes has skills-from-experience + FTS5 recall | Learn from accepted/rejected edits per project; persist a taste profile; auto-suggest patterns | Build `internal/taste/`: watch edit/apply_patch outcomes per session (accepted vs reverted via shadow-git snapshots), distill signals (naming, style, structure) into a project `.rick/taste.md` appended to the system prompt (byte-stability: gate via the existing cache-impact review — freeze after first request). Reuse `internal/distill` for summarization; store in session store. Also wire `internal/plugin/skill.go` to auto-persist repeated multi-tool recipes as skills (Hermes/agentskills.io pattern) |
| P0-3 | **Browser automation** | Only fetch/websearch | Headed browser tool (oh-my-pi, Hermes, Codex-plugins have it) | Add a Playwright-backed MCP server to the default MCP config (like `@playwright/mcp`); expose as `<server>_tool` automatically via existing MCP plumbing — no core changes needed; document in README + `rick config` example |
| P0-4 | **Provider-visible projection sidecar** | View recomputed from canonical history each turn (reasonix doc §4.4 already flags this) | Derive each turn's serialized view from the previous view (persistent tree), never rewrite mid-prefix; keep cache TTL as cost-only observation | Implement `internal/agent/projection.go`: `Session.Messages` stays canonical; build `providerView` as `prevView + tail`; on divergence fail closed (reuse `inferReason`); persist `CoveredPrefixHash`; compare serialized views per turn in `cmd/cachehit` (append-only assertion). This is the #1 mechanism from reasonix/oh-my-pi not yet ported |
| P0-5 | **Auto-checkpoint commits + diff review** | Shadow-git snapshots only (safe but invisible) | Optional real-repo checkpoints (opencode/Codex) with a diff-review mode | Add `checkpoint_commits: true` config: before each mutating tool or on `/checkpoint`, commit to a side branch (`refs/rick/checkpoints`) using existing shadow-git plumbing (`internal/session`); add `/diff` command that shows the last checkpoint's diff (reuse `internal/tools/diff.go`); never touch user branches/history — keep the shadow design but make it visible |
| P0-6 | **Ecosystem / community surface** | Private; README references `rick-cli/rick` but no public releases, docs site, or plugin registry | Publish: GitHub releases + install scripts (already exist), docs site, plugin/skill registry, theme gallery, Discord | Promote the existing scripts (`scripts/install.sh`, `scripts/Install-Rick.ps1`); add `scripts/publish-release.sh` (build + `gh release create`); render `docs/` with a static site (e.g. mkdocs); add a `rick plugin search` command hitting a registry index; publish `cmd/cachehit` results as a public benchmark page (the 92–93%/0-resets numbers are a selling point) |
| P1-7 | **Sandbox parity with Codex** | Per-OS exec sandbox exists but no approval-mode UX or docs | Landlock/Seatbelt/AppContainer-grade sandbox profiles + visible approval modes | Map Codex's modes onto the existing permission engine: `read-only` (= deny bash/edit), `auto` (= allow within glob policy), `full-auto` (--yolo equivalent); document per-OS guarantees (`internal/sandbox/exec_windows.go` etc.) in README; add a sandbox probe to `rickdoctor` |
| P1-8 | **`exec`-style scripting for CI** | `-p` flag + rickserve NDJSON | First-class non-interactive mode with JSON output and exit codes for CI | Add `rick run -p "..." --format json --exit-on-error` (`internal/headless/` already exists — extend with JSON output + exit-code mapping); document a GitHub-Actions example using `rickserve` |
| P1-9 | **Session/memory search** | Sessions persist but no cross-session search | FTS5-style recall across past sessions (Hermes has it) | Add `rick sessions search <query>` backed by a SQLite FTS index built on session JSONL (new `internal/session/search.go`); surface in `@` picker or `/search`; reuse `internal/distill` for LLM summarization of hits |
| P1-10 | **AGENTS.md conventions parity** | Loaded in `internal/agent/prompt.go` but undocumented | Auto-load project/global AGENTS.md with layering, documented | Verify current behavior (grep `AGENTS.md` in prompt.go), add layered loading (global → repo → `.rick/`) with `instructions` merge, and document precedence in README; add a test pinning the composed order (cache stability test) |
| P1-11 | **Multi-agent UX polish** | Swarm/team exist; `/agents`, `/jobs` planned (multi-agent-plan.md) | Ship the interactive orchestration UX (view/steer/chat/kill, background jobs) | Implement Phase 1–2 of `docs/multi-agent-plan.md`: `internal/agent/registry.go` (exists in plan), `/agents` + `/jobs` views in `internal/tui/`, wire `steer`/`chat` to running agents via `internal/agent/teamtool.go` |
| P2-12 | **Provider breadth polish** | 13+ catalog, OpenRouter covers the long tail | 60+ (oh-my-pi) parity via auto-discovered OpenAI-compatible endpoints + a provider registry file | Extend `internal/provider/catalog/generated.go` generation script to emit OpenAI-compatible endpoints (groq/together/mistral/… already present); add `provider discover` command using the model-filter/probe code |
| P2-13 | **Context-compression MCP consumption** | distill + RepoMapper internal | Optionally consume headroom/rtk-style compressors as MCP for huge tool outputs | Ship `headroom-ai` / `rtk` as an optional MCP config in the README examples; or expose `pkg/contextbudget` as an MCP server (`rick mcp contextbudget`) so other agents benefit too |
| P2-14 | **Observability dashboard** | `/stats` in TUI (mcpui.go), telemetry in sessions | Web dashboard over rickserve (cache hit %, cost/turn, divergence resets, snapshot disk) | Add `rickserve` endpoint `GET /stats` aggregating `session.Requests` usage rows (already persisted); ship a tiny static HTML dashboard in `cmd/rickserve/` |
| P2-15 | **Windows hardening** | Token/session handling in `exec_windows.go` | Verify + document AppContainer-equivalent story; add integration tests | Extend `internal/sandbox/exec_windows.go` with a documented job-object/restricted-token profile; add `ricke2e` sandbox cases on Windows |
| P2-16 | **Session storage policy** | 415 files / 35 MB today, "fine but growing" (big-plan §2) | Archiving + compaction of stale sessions | `rick sessions archive --older-than 30d` → move to `<store>/archive/` (reuse `prune` command); add size cap + warning in `rickdoctor` |

---

## 7. Sources

- rick: G:\projectE — README.md, RICK.md, CHANGELOG.md, docs/{cache-hit-plan-2026-08-07.md,
  cache-hit-reasonix-plan-2026-08-08.md, multi-agent-plan.md, big-plan-2026-08-08.md},
  internal/{provider,tools,agent,sandbox,permission,mcp,plugin,goal,swarm,headless},
  cmd/rickserve.
- can1357/oh-my-pi (README, GitHub API); esengine/deepseek-reasonix (README, rick's study);
  openai/codex (README, docs listing incl. execpolicy.md); sst/opencode (README);
  commandcode.ai + ~/.commandcode config/history; NousResearch/hermes-agent (local README);
  shields.io star badges for opencode/codex/reasonix/oh-my-pi/hermes (2026-08-08).

*Notes: star counts are point-in-time. reasonix cache figure is its published single-day
case study on raw DeepSeek API; rick's 92–93% is measured against the free `-flash-free`
tier where every remaining re-bill is provider-side (0 client resets).* 
