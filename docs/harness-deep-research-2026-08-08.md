# rick (G:\projectE) — Deep Research, Bug Audit & Harness Comparison

**Date:** 2026-08-08
**Scope:** rick source audit (`internal/`, `cmd/`, `pkg/`, docs) + comparison against zcode,
opencode, hermes, kimi-code, plus Codex, Claude Code, oh-my-pi, reasonix.
**Method:** direct codebase reading and `go build`/`go vet`/`go test ./internal/...` (all green
on 1.25.5), local installs inspected (`~/.kimi-code`, `~/.config/opencode`,
`~/.local/share/opencode`, `~/research_workspace/hermes-agent`, npm `codex`), web search for
zcode/opencode marketing. Facts below are labeled `[code]` (read from source),
`[local]` (read from a local install), `[web]` (public marketing/README), `[repo]`
(said in an existing doc in G:\projectE\docs).

---

## 1. TL;DR

- **What rick is:** a Go 1.24, single-static-binary, Bubble Tea TUI agent (~72k LOC in
  `internal/`+`cmd/`+`pkg/`, 280 .go files). Its genuine differentiators are unusual for a
  personal harness: an **engineering-grade prompt-cache stability subsystem** (byte-pinned
  system prompt, append-only provider view, per-request divergence telemetry, warm/stream
  byte parity, CI cache guard), a **resilient multi-backend websearch**, a **glob
  allow/ask/deny permission engine** with compound-command analysis, **shadow-git undo**,
  **multi-agent swarm/team orchestration**, and a headless **rickserve** NDJSON daemon.
- **Where it loses to the field:** no LSP/DAP (opencode, oh-my-pi have it), no browser
  automation (hermes), no cross-session learning loop / skill-from-experience / memory
  search (hermes, Command Code's "taste"), no real-repo auto-checkpoint commits (opencode,
  Codex), no desktop/IDE/cloud backends (hermes: Modal/Daytona; zcode: ADE desktop app).
- **Audit outcome:** `go build`, `go vet`, `go test ./internal/...` all pass. Found **1
  real bug** (unreachable session-prune in the new cache keep-alive loop, dead code that
  contradicts the CHANGELOG claim), **1 correctness wart** (ripgrep error swallowed in the
  `glob` tool's fast path), and **6 concrete optimization opportunities** already measured
  in rick's own CPU/RAM plans (3 already shipped, 3 outstanding). See §3.
- **Highest-leverage "do better" list:** ship the deferred FTS session-search + SQLite
  index (opencode ships SQLite now), add an LSP-free approximation or real LSP client,
  stand up a skills/learning loop from the existing `internal/plugin/skill.go` + memo +
  session history, land real-repo checkpoint commits mode, and finish the multi-agent UX
  (`/agents`, `/jobs`) that is already planned in `docs/multi-agent-plan.md`.

---

## 2. What rick is (verified from source)

| Layer | What exists | Evidence |
|---|---|---|
| Runtime | Go 1.24, single static exe (~61.8 MB); no Node/Python needed for core | `go.mod`, `README.md`, `rick.exe` [code] |
| UI | Bubble Tea TUI, leader key `ctrl+x`, `@` fuzzy file picker, `!cmd` escape, slash command system, 4 built-in themes + JSON themes, transcript scroll, tool-detail view | `internal/tui/model.go`, `keys.go`, `modals.go` |
| Providers | openai, anthropic, openrouter, zai, deepseek, xai, gemini, groq, mistral, together, opencode-zen, azure, ollama; OAuth, stream probes, model filter, small model for compaction | `internal/provider/catalog.go` |
| Tools | read, write, edit (exact-string + whitespace-tolerant fallback, refuses unread files), bash, grep, glob, list, apply_patch, todowrite/todoread, task (subagents), code_symbols, websearch (10+ backends with health/budget/dedup), fetch, retrieve, memo, goal tools, swarm/team/parallel tools | `internal/tools/`, `internal/agent/` |
| Permissions | allow/ask/deny; globs matched per sub-command of compound bash lines; most-specific-wins; outside-project writes auto-upgrade to ask; traversal/prefix-confusion tests | `internal/permission/` + `sandbox_approval_test.go` |
| Sessions/undo | per-dir resumable sessions; **shadow-git snapshot undo/redo** outside the real repo; snapshot/session pruning; well-known personal folders never shadow-repo'd | `internal/session/snapshot.go:44-49` |
| Cache engineering (differentiator) | frozen system prompt, append-only view, canonical sorted tool schema, `divergence`/resets telemetry in session `requests[]`, byte-parity warm, per-vendor cache TTL, keep-alive loop, `cmd/cachehit` benchmark, CI cache guard | `internal/agent/agent.go`, `internal/provider/openai/openai.go`, `docs/cache-hit-*.md` |
| Headless | `rickserve` NDJSON TCP daemon bound to 127.0.0.1 (run, permission-response, sessions, models, tools, config, snapshots) | `cmd/rickserve/main.go:581` |
| Ops | rickdoctor, rickverify, ricksec, ricke2e, rickauth, maintenance, self-update/uninstall | `cmd/` |
| Extensibility | MCP client (stdio+http), plugin hook dispatcher with skills, custom slash commands, JSONC config with `{env:}`/`{file:}` substitution + generated JSON schemas | `internal/mcp/`, `internal/plugin/`, `internal/config/` |

---

## 3. rick audit: bugs, risks, optimizations

### 3.1 Bugs found

#### BUG-1 (real, dead code) — keep-alive session prune branch is unreachable
`internal/provider/openai/openai.go:212-222`

```go
for sid, s := range c.kaSessions {
    if idle := now.Sub(s.last); idle > c.kaInterval {
        if !s.inFlight {
            s.inFlight = true
            toSend = append(toSend, due{sid, s})
        }
    } else if idle > 24*time.Hour {
        // Prune long-dead sessions…
        delete(c.kaSessions, sid)
    }
}
```

With any realistic `cache_keepalive_seconds` (< 24 h — the docs examples are
60–300 s), **every** session idle > 24 h is already `idle > c.kaInterval`, so the first
branch always wins and the `else if` prune can never execute. The CHANGELOG explicitly
claims "sessions are pruned after a day"; instead the map grows forever, and every
abandoned session keeps costing one extra API call per interval until process exit.
Fix: prune unconditionally when `idle > 24h` (or bound the map size), independent of the
interval branch — e.g. `if idle > maxIdle { delete …; continue }` before the interval check.

#### BUG-2 (medium) — glob fast path swallows ripgrep failures
`internal/tools/search.go:318-326`

```go
cmd := exec.CommandContext(runCtx, ripgrepPath, "--files", "--color=never", "--glob", a.Pattern, searchPath)
…
_ = cmd.Run()
```

Exit code is discarded. `rg --files` returns 1 both for the *normal* "no matches" case
and for genuine failures (permission denied on a subtree, unreadable path, missing binary).
The agent is told "no files" instead of "search failed"; it then silently proceeds on an
empty result set. Distinguish `exit 1` (fine) from other exit codes (return an error).

#### BUG-3 (low) — headless mode can hang on a runner panic
`internal/headless/headless.go:161-164,253`

```go
go func() { appended, runErr = runner.Run(ctx, history, ch); close(done) }()
…
<-done
```

`Runner.Run` closes `out` via `defer`, so a normal return drains fine, but a panic in the
runner leaves `done` forever open and the headless command hangs with no error. Wrap the
goroutine or the `<-done` in a recover that reports the panic (noted; this is the same
shape as the TUI's own `recover` usage elsewhere).

#### BUG-4 (low) — snapshot failures are silent
`internal/agent/agent.go:736-744` — `_, _ = r.cfg.Snapshotter.Snapshot(calls[i].Name)`.
If shadow-git snapshotting fails (disk full, repo lock), the agent keeps mutating and the
user's undo promise silently breaks. At minimum surface one `EvAgentMessage` warning when
a snapshot fails.

#### BUG-5 (low, cosmetic) — `keepaliveSend` never validates the response's cache fields
`internal/provider/openai/openai.go:234-258` — after re-POSTing the keep-alive it discards
1 KiB of body and closes. It would be cheap (and useful) to log whether the keep-alive
actually resulted in `cache_read>0`, which is the very metric the loop exists to preserve.

### 3.2 Optimizations available (from rick's own CPU/RAM plans — 3 shipped, 3 open)

Already done in current HEAD (`[repo]`, CHANGELOG + `CPU_OPTIMIZATION_PLAN.md` §1):
- truncation off-by-one in `trimTranscript` (the 18× per-chunk cost) — fixed with a
  500-message comptat cap;
- per-message token-count memoization inside `tokens.Count` (measured 2.0× on the
  cache-boundary pass);
- incremental cache-boundary selection (reuses previous pass byte/token/hash data);
- `http.Client` per-request allocation removed for streaming.

Still open / partially open (`[repo]` `CPU_OPTIMIZATION_PLAN.md` §2, `RAM_OPTIMIZATION_PROMPT.md`):
- **O1 — session listing & search RAM churn.** `Store.List()`/`Search()` still unmarshal
  whole session JSONs to read metadata. The RAM plan's manifest/sidecar idea (`metadata in
  a small index`, load the full JSON only for matches) is the single biggest heap-churn
  target; the codebase now writes `session_index.jsonl`-style data for some paths, but the
  deferred FTS index (`F-06`) means search is still a linear full-file scan.
- **O2 — TUI double storage.** `m.msgs []ChatMsg` (with `ToolInput`/`ToolOutput` copies)
  runs in parallel with `m.history []provider.Message`; every turn rebuilds history from
  msgs. Making `m.history` the single source of truth and deriving the render list lazily
  saves ~20–40% of session RAM on tool-heavy sessions (still on the plan).
- **O3 — duplicated `json.Marshal` + sha256 per message** in `ChooseBoundaries`
  (`pkg/contextbudget/contextbudget.go`): compute byteLen from the hash payload pass
  instead of marshalling twice (explicitly still-open in `CPU_OPTIMIZATION_PLAN.md` S2).

### 3.3 Strengths worth keeping (verified, do not regress)

- **Cache byte-stability invariant is credible.** System prompt is frozen after request #1
  (`pinnedSystem`), the provider view trims only at the head with a pinned sentinel, and
  telemetry labels each divergence — with an append-only property test. The 92–93% /
  measured-0-client-resets figure against the free flash tier rest on the `cmd/cachehit`
  benchmark rather than anecdote `[repo]`.
- **Permission engine has adversarial tests.** outside-fence → ask, `root + "-evil"` prefix
  confusion → ask, `../parent` traversal → ask (`internal/permission/sandbox_approval_test.go`)
- **Rickserve binds only 127.0.0.1** — no accidental LAN exposure.
- **Snapshotter refuses personal folders** (Downloads/Documents/Desktop/profile root) —
  protects the whole user tree from being shadow-repo'd (`internal/session/snapshot.go:44-47`).

---

## 4. Competitor profiles

### 4.1 zcode (Z.ai / GLM) — "the agent-first ADE"
`[web]` zcode.z.ai; official harness for GLM-5.2 (754B total / 40B active, 1M ctx, 128k
out). Positioning: an **Agentic Development Environment**, not a TUI — agent conversation
is the center; file manager, terminal, Git panel, and **live browser preview** arranged
around it. Also ships a **goal system, remote control, SSH development, multi-agent
coordination**, desktop app + GLM Coding Plans (subscription for GLM models usable inside
Claude Code, Cline, OpenCode, Kilo).
**What it teaches rick:** a browser-preview loop and a desktop shell are product-level
surfaces rick has no equivalent of; GLM's per-month coding-plan model is a monetization /
gateway pattern rick's `local` provider config could serve.

### 4.2 opencode (sst/opencode) — the ecosystem leader among open terminal agents
`[web]` TypeScript/Bun, MIT, ~120–195k stars (doc cites 195k). Provider-agnostic via the
AI SDK (75+ model providers, free models included). **LSP integration** (diagnostics,
hover, definitions, references) as first-class. AGENTS.md conventions, plugins, MCP
client *and* server, git-diff review + checkpoints, shareable sessions.
`[local]` the local install shows the modern shape: session state already lives in a
**SQLite DB** (`~/.local/share/opencode/opencode.db`), repos in `repos/`, shell history in
`shell/`, snapshot dir, and a `service.json` with a **password** (their auth story for
the web/headless server).
**What it teaches rick:** SQLite session store beats JSONL-with-sidecar; plugin registry +
docs site; LSP on by default; `opencode run` + `serve` maturity.

### 4.3 Hermes (NousResearch/hermes-agent) — the self-improving agent
`[code]` (local repo `~/research_workspace/hermes-agent`, MIT, Python). The only harness
with a **closed learning loop**: agent-curated memory with periodic nudges, autonomous
skill creation after complex tasks, skills that self-improve during use, **FTS5 session
search + LLM summarization**, Honcho user modeling, and compatibility with the
agentskills.io standard. Multi-platform gateway (Telegram/Discord/Slack/WS/Signal + CLI
from one process), scheduled **cron automations**, **six execution backends** (local,
Docker, SSH, Singularity, Modal, Daytona — serverless hibernation), subagents + Python
RPC script tools, provider API-server breadth (Nous Portal, OpenRouter, NIM, z, Kimi,
MiniMax, HF, …).
`[code]` AGENTS.md is explicit: **"Per-conversation prompt caching is sacred"** and *"the
core is a narrow waist; capability lives at the edges"* — the same two governing values as
rick's cache engineering, but with an opposite tool-schema philosophy (they discard core
tools to keep the model-tool schema small; rick ships ~20 core tools). This is a design
tension worth a decision: does rick trade schema size for tool breadth?
**What it teaches rick:** skills/memory/cron = the missing "personality"; backends = the
"runs anywhere" story; multi-platform gateways.

### 4.4 kimi-code (Moonshot AI)
`[local]` (installed at `~/.kimi-code`): features a `config.toml`, `tui.toml`, per-workspace
`workspaces.json`, `session_index.jsonl`, a `skills/` directory, `credentials`,
`telemetry/`, `logs/`, `updates/`. So Moonshot's CLI is: Go, K2-line Moonshot models
centered, Anthropic-compatible API client, **context/auto-compression** built-in,
**skills** directory, **workspace-scoped session indexing**, own telemetry + self-update.
Public positioning (2025-07 launch) is "free/open-source terminal agent for Kimi K2,
with built-in context compression."
**What it needs rick:** a first-party skills directory + per-workspace sessions are cheap
to mirror; context compression is a feature rick already implements *better* (byte-pinned
append-only) — kimi's one-round compression is the naive version.

### 4.5 Honorable/other (from existing docs + web)
- **Codex CLI** (`[local]` npm `codex`): Rust, OS sandboxing (landlock/Seatbelt/AppContainer), `codex exec` scriptability, checkpointing.
- **Claude Code:** the UX benchmark for hooks/subagents/checkpoint UX.
- **oh-my-pi / Pi** (`[repo]`): TS + Rust, 60+ providers, **14 LSP + 28 DAP ops**, browser automation, append-only "persistent tree" reference implementation for cache-safe history.
- **reasonix** (`[repo]`): DeepSeek cache-hit extremism (published 99.82% day) — the cache-performance ceiling rick is chasing with an honest keep-alive story.

---

## 5. Feature matrix

Legend: ● full in v1 · ◐ partial · ○ absent. rick `[code]`; others from local `[code]`/`[web]`.

| Dimension | rick | zcode | opencode | hermes | kimi-code | codex / claude |
|---|---|---|---|---|---|---|
| Runtime | Go static exe | Desktop/GUI (GLM) | TS/Bun | Python | Go binary | Rust / Node |
| Provider breadth | ● 13+ catalog | ◐ GLM-first (300+ via gateway) | ● any/AI SDK 75+ | ● many (6 backends) | ◐ Moonshot-first | ◐ proprietary-ish |
| LSP/IDE-in-terminal | ○ | ● (full ADE) | ● | ◐ | ◐ | ○ |
| Debugger (DAP) | ○ | ◐ | ○ | ○ | ○ | ○ |
| Browser automation | ○ (fetch/websearch only) | ● live preview | ○ | ● cloud browser | ○ | ◐ |
| Subagents | ● general+explore, depth cap | ◐ multi-agent | ● task | ● delegates | ◐ | ● |
| Swarm/team/goals | ● goal budgets + team/swarm | ● goal system | ○ | ◐ | ○ | ○ |
| Sessions/resume | ● per-dir, JSONL | ● | ● SQLite | ● + FTS5 search | ● index.jsonl | ● |
| Undo, real-repo checkpoints | ● shadow-git undo; ○ auto-commit | ● git panel | ● checkpoints | ◐ | ◐ | ● (codex worktrees) |
| Permission engine | ● globs+compound-rule | ◐ | ◐ | ◐ | ◐ | ● approval modes |
| OS sandbox | ◐ per-OS exec sandbox | ▽ | ○ | ● Docker/SSH/Modal backends | ○ | ● landlock etc. |
| Prompt-cache engineering | ●★ byte-pinned+telemetry+keepalive | ◐ | ◐ | ● "cache sacred" | ● K2 compression | ◐ |
| Compaction/distill | ● /compact + distill + live-zone | ● | ◐ | ● | ● auto-compress | ● |
| Headless/CI | ● rickserve NDJSON | ● | ● serve / run | ○ | ○ | ● exec |
| Learning/memory/skills | ◐ memo tool, skills hooks exist | ◐ | ○ | ●★ full closed loop (AGENT store) | ● skills | ◐ |
| Plugins/hooks | ● plugin dispatcher + skills | ● | ● plugin registry | ● plugins+skills | ○ | ● hooks/plugins |
| Cloud/IDE/desktop | ○ (rickserve only) | ● ADE | ● desktop polls, IDE ext | ● desktop, Telegram etc. | ◐ | ● desktop |

*(Note: matrix is my synthesis of the above sources; exact cell-by-cell parity for
zcode/kimi is marketing-level, not ground truth.)*

---

## 6. Gaps — what rick should do better (prioritized)

### P0 — finish what's already half-built (cheapest wins)
1. **Fix the keep-alive prune bug (BUG-1)** — it's new code, dead branch, and contradicts
   the CHANGELOG; one `continue` branch before the interval check fixes it.
2. **Disambiguate rg failures (BUG-2)** + surface snapshot warnings (BUG-4).
3. **Ship the multi-agent UX** already specced in `docs/multi-agent-plan.md` (agent
   registry exists; `/agents`, `/jobs`, view/steer/chat/kill are the missing half) —
   this is rick's one feature the others don't have; make it usable or nobody will claim it.
4. **Move sessions to SQLite with FTS (deferred F-06).** opencode proves the pattern
   (opencode.db); rick already keeps `session_index.jsonl`. An FTS5 index resolved in
   `/search` + `@` picker = "recall" parity with Hermes (their FTS5 + LLM summarization)
   without building anything fancier.

### P1 — the "learning/taste loop" (where rick is most exposed)
- Hermes' closed loop (skills auto-created, self-improved, memory nudges, session search)
  and Command Code's "taste" both target the same gap: rick has the raw material
  (session history, memo tool, `internal/plugin/skill.go`, distill summarizer) but no
  feedback loop that turns accepted/rejected edits into a per-user profile.
  Deliverable: extract accepted-edit patterns with distill, write a `skills/<topic>.md`,
  surface in the `@` picker via `/skills list`, auto-inject only at turn 0 (pinned, to
  preserve the byte-stable cache — same constraint Hermes respects).

### P2 — harness-correctness & ecosystem surfacing
- **LSP in a lighter form:** full LSP server client is a big lift, but opencode's
  diagnostics loop is the single most useful missing feature. A cheap first step:
  wire `gopls`-style `--format=json` diagnostics for the languages in the repo map into a
  `diagnostics` tool (extend `internal/agent/` — actually `pkg/repomap` + a new
  `internal/tools/diagnostics.go`), then grow to real LSP.
- **Real-repo checkpoint mode** (opencode/codex have it): an opt-in
  `autocommit: after each accepted diff batch, commit to a `rick/checkpoints` branch
  (not main) — solves "undo across sessions" and CI replay stories cleanly, without
  touching the shadow-git snapshot path.
- **AGENTS.md layering** (global → repo → `.rick/`) is 90% done in
  `internal/agent/prompt.go`; document precedence + pin the composed order in a
  cache-stability test so it can't regress the byte-pinned prefix.
- **Provider breadth & gateway monetization** — add a `discover` command that probes
  OpenAI-compatible endpoints (fits the passing `probe.go`); document how rick's
  `local`/OpenAI-compatible provider config can act as a model gateway the way Command
  Code / GLM Coding Plans do.
- **Observability dashboard** — `rickserve /stats` (cache hit%, cost, divergence resets,
  snapshot disk) fits existing `mcpui` data.

### P3 — roadmap-shaped, lower priority
- Docker/SSH/Modal-style backends (hermes parity) if rick ever wants "runs anywhere";
- cloud-browser preview (z, hermes) — the single biggest jump for UI-testing workflows;
- plugin ecosystem surface (registry/docs/XD) tuned toward the rick "no build step"
  advantage (`.rick/commands`, skills).

---

## 7. Bugs & optimizations summary table

| # | Kind | Location | Severity | Status in HEAD |
|---|---|---|---|---|
| 1 | bug — unreachable prune branch (keepalive map leaks) | `internal/provider/openai/openai.go:212-223` | high | unfixed |
| 2 | bug — `rg --files` nonzero exit swallowed | `internal/tools/search.go:326` | medium | unfixed |
| 3 | bug — headless hang on runner panic | `internal/headless/headless.go:152-163,253` | low | unfixed |
| 4 | bug — snapshot failure swallowed | `internal/agent/agent.go:740` | low | unfixed |
| 5 | opt — session list/search full-JSON unmarshal | `internal/session/session.go` | med (RAM plan CHANGE 1) | partially addressed |
| 6 | opt — TUI doubles tool data (`msgs` vs `history`) | `internal/tui/model.go`, `agentbridge.go` | med | open |
| 7 | opt — double `json.Marshal`+sha in boundaries | `pkg/contextbudget/contextbudget.go` | low | open |
| 8 | opt — trimTranscript, memoized tokens, reuse boundary pass | `internal/tui/model.go`, `internal/tokens` | was high | fixed ✅ |
| 9 | good — cache byte-stability + telemetry + CI guard | `internal/agent`, `internal/provider`, scripts | keep | shipping |

---

## 8. Evidence & sources

- **rick:** `G:\projectE\README.md`, `RICK.md`, `CHANGELOG.md`;
  `CPU_OPTIMIZATION_PLAN.md`; `RAM_OPTIMIZATION_PROMPT.md`; `_hardening.txt`; `_plan.txt`;
  `docs/{cache-hit-plan-2026-08-07.md, cache-hit-hardening-plan-2026-08-07.md,
  cache-miss-analysis-2026-08-08.md, big-plan-2026-08-08.md, multi-agent-plan.md,
  harness-comparison-2026-08-08.md}`; source in `internal/{agent,provider,tools,
  permission,config,session,sandbox,security,mcp,plugin,goal,swarm,headless}` and
  `cmd/rickserve`. `go build ./...`, `go vet ./...`, `go test ./internal/...` all green.
- **opencode:** `~/.config/opencode/opencode.jsonc`, `~/.config/opencode/service.json`,
  `~/.local/share/opencode/{opencode.db,repos,shell}` (locally installed binary + db);
  opencode.ai/docs `[web]`.
- **hermes:** local clone `~/research_workspace/hermes-agent` (README.md, AGENTS.md,
  skills/, mcp_serve.py, acp_adapter).
- **kimi-code:** local install `~/.kimi-code/` (config.toml, skills/, sessions/,
  workspaces.json).
- **zcode:** zcode.z.ai + z.ai/coding-plan `[web]`.
- **codex:** npm global install.
- **oh-my-pi / reasonix / Claude Code / Command Code:** existing in-repo
  `docs/harness-comparison-2026-08-08.md` (its star counts are point-in-time and
  market-derived, not re-verified here).

*All line numbers above are against current HEAD (`d0a7107`).*