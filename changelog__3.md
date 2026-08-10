# CHANGELOG — rick 0.1.16 (tool-call repairs)

Date: 2026-08-10 · Implements the tool-call-repair layer from the Command Code
harness-engineering deep-dive (commandcode.ai/docs/harness-engineering/
tool-call-repairs). All changes verified with `go build ./...`, `go vet ./...`,
`go test ./... -count=1`, and the changed-files gofmt gate in `scripts/verify.sh`.

---

## What this does for you (the short version)

Open models (deepseek, glm, qwen) are great at writing code and bad at calling
tools: they emit `null` for optional fields, hand over `"[\"a\",\"b\"]"` as a
JSON *string* instead of an array, wrap a single arg in `{}` where the schema
wanted an array, or pass a bare string where an array belongs. Command Code
found those same four mistakes repeating across billions of tokens and fixed
them with a **validate-then-repair layer** that runs on every model, repairing
~1M tool calls per 1T tokens. rick now has that layer too:

- **Calls that used to bounce now just work.** A malformed `todowrite`,
  `parallel_tasks`, `websearch` (include/exclude domains) or `swarm` call is
  repaired against the tool's JSON schema instead of erroring and burning a
  whole model turn (and a re-bill) on a retry.
- **The model is told what was repaired.** Every fixed call appends
  `<repaired: …>` to its result and carries the note in the event, so the
  model sees exactly what was changed and can self-correct if the guess was
  wrong. A repaired call is a success, never an error — the TUI doesn't paint
  it red.
- **Read's relational defaults are surfaced.** When the model asks for
  `offset` but not `limit` (or vice versa), read applies the counterpart
  default and says so (`<defaults applied: limit unset → 2000>`), instead of
  silently guessing.
- **Markdown-auto-linked paths no longer break `read`/`edit`/`write`.** A path
  emitted as `[notes.md](http://notes.md)` is unwrapped to `notes.md` before
  hitting the filesystem, and path fields advertise `"format": "path"` in
  their schema.
- **Repair telemetry per model × tool.** `usage.json` now records how many
  calls each model had repaired per tool, so a model regressing on a tool
  contract shows up in the data (via the tracker API) instead of as mysterious
  one-off failures.

**Token impact:** 0 tokens on valid calls (strict decode runs first and valid
input is never touched). On the failure class, each repair saves the whole
failed-call round trip: 1–3 turns of re-emitted arguments plus the provider
re-bill of the same prompt with a new tool call (~2–8k tokens per occurrence),
and the wall-clock time of watching the model fail. The universal four repairs
cover the ~90% of shape errors the Command Code investigation attributes to
open models.

---

## What changed

### 1. Schema-driven, ordered repair pass (`internal/tools/repair.go`)

`tools.RepairDecode` replaces strict decode as the entry point for every tool's
argument parsing:

1. **Strict decode first** — valid input is never touched (byte-identical to
   before, so the canonical-input repeat guard and provider cache are
   unaffected).
2. On failure, the args are decoded loosely and repaired **only at fields
   whose declared schema type disagrees with the value's JSON type**:
   - `null` on an optional field → key omitted;
   - stringified array `"[\"a\",\"b\"]"` → parsed into a real array;
   - `{}` empty placeholder where an array is expected → replaced with `[]`;
   - bare string where an array is expected → wrapped in an array;
   - (family-gated) string number `"5"` where a number is expected → coerced.
3. Re-decode strictly. Success → the repaired call runs, with a note in the
   per-call `RepairOpts`; failure → the original strict error is returned.

Ordering is deliberate and matches the guidance: **array-parse runs before
bare-string-wrap**, so `"[\"a\",\"b\"]"` becomes `["a","b"]`, never the
double-wrapped `"[\"[\"a\",\"b\"]\"]"`. Two hard safety rules:

- **String-typed fields are never unwrapped.** `old_string: "[\"a\",\"b\"]"`
  (a literal search string) stays untouched — only fields the schema declares
  as arrays are array-repaired.
- **Unknown fields are still rejected.** A typo'd field (`commmand`) is never
  repaired away; the repair re-decodes strictly, so `DisallowUnknownFields`
  semantics are preserved. `limits_test.go`'s typo test still passes.

The repair config (`RepairOpts` with a note sink and a model-family gate) is
threaded through `tools.Context.Repair` and created per-call by the agent loop
(`execOne`) — no global state, safe under the parallel read-only tools.

### 2. Transparency: repairs and defaults are surfaced to the model

- Every tool that takes the repair path appends `<repaired: …>` to its result
  and stores the note in `Result.Meta["repaired"]`. The agent mirrors it onto
  `ToolEvent.Repaired` (new field) and the tool result text. The TUI and
  headless JSON output both carry it.
- `read`'s relational defaults (`offset` alone → `limit 2000`, `limit` alone →
  `offset 1`) now echo a `<defaults applied: …>` footer instead of being
  silent magic. Plain and `full:true` reads are unaffected.

### 3. Markdown-auto-link path unwrap (`pathProp`)

`resolvePath` now unwraps the degenerate `[text](http://text)` form before
resolving, so `[notes.md](http://notes.md)` reaches the filesystem as
`notes.md`. Only a whole-string markdown link with an http(s) target is
unwrapped — real filenames and markdown text pass through untouched. Path
fields across `read`/`edit`/`write`/`bash`(cwd)/`grep`/`glob`/`list`/`tree`/
`git`/`code_symbols`/`vision` now use the new `pathProp` schema helper, which
adds `"format": "path"`.

### 4. Repair telemetry per model × tool (`internal/usage`)

- `usage.Tracker` gains `RecordRepair(modelID, tool)` and a persisted
  `repairs` map (model → per-tool counts) in `usage.json`, alongside the
  existing date-keyed token usage.
- `Load` handles both legacy files (no `repairs` key) and the new shape;
  `Clear` resets both; `Models()` includes models that only have repairs.
- The TUI records a repair on `EvToolEnd` when `ToolEvent.Repaired` is set;
  headless `ToolRecord` gains a `repaired` field for JSON output.

### 5. Family-conditioned quirks (P5)

`tools.FamilyForModel` derives a family from the model id (`deepseek`, `glm`,
`qwen`, else none). The four universal repairs always run; the `number-string`
coercion is gated to those three families (the open-model lineages Command
Code's investigation names). The quirk table is a `map[family][]repair` — a
new lineage or a new per-family repair is one line. The agent computes the
family once per runner from `cfg.Model` and threads it through every tool call.

---

## Files changed (this change set only)

- `internal/tools/repair.go` (new) — `RepairDecode`, `repairArgs`,
  `repairArray`, `FamilyForModel`, `unwrapMarkdownLink`, `RepairOpts`.
- `internal/tools/args.go` — `repairNote`/`RepairNoteResult`/`NoteOf`.
- `internal/tools/tools.go` — `Context.Repair` field, `pathProp` helper.
- `internal/tools/file.go` — `resolvePath` unwraps auto-links; `read`/`write`/
  `edit` use `RepairDecode` + `repairNote`; read surfaces applied defaults;
  path schemas use `pathProp`.
- `internal/tools/{bash,apply_patch,code_symbols,extra,retrieve,search,todo,
  vision_tool,websearch}.go` — every tool's decode is now `RepairDecode`; array
  and path schemas updated; repair notes surfaced on success.
- `internal/agent/{agent,parallel,subagent,chattool,teamtool,swarmtool}.go` —
  `execOne` builds `RepairOpts` per call, `ToolEvent.Repaired`, `repairFamily`
  on the runner, agent-package tools use `RepairDecode`.
- `internal/headless/headless.go` — `ToolRecord.repaired`.
- `internal/tui/agentbridge.go` — records repairs into the usage tracker.
- `internal/usage/tracker.go` — `RepairCounts`, `RecordRepair`, `Repairs`,
  `RepairsForModel`, versioned `Load`/`persistLocked`.
- Tests: `internal/tools/repair_test.go` (16 cases), read integration tests
  (`TestReadSurfacesRelationalDefaults`, `TestReadRepairsMarkdownAutoLink`),
  `internal/agent/repair_propagation_test.go`, `internal/usage/tracker_test.go`
  (repair persistence + legacy load).

## Cache-impact gate notes (for the PR body)

Cache-sensitive paths touched: `internal/tools/tools.go` (schemas),
`internal/agent/agent.go` (ToolEvent only — no provider-view bytes).

- **Cache-impact:** The `"format": "path"` addition changes the provider-facing
  tool-schema bytes for the path tools, so the *first* request after this
  upgrade cold-starts that prefix once. After that the schemas are frozen per
  run (`PinnedToolSchemas`) and byte-stable. No other change touches the
  provider view: repairs happen inside tool execution and never mutate the
  assistant messages; `canonicalToolInput` and the repeat guard still see the
  model's original bytes.
- **Guard:** `go test ./... -count=1` (all 45 packages pass), the existing
  cache-stability tests, and the new repair tests pin the invariant that valid
  input is byte-identical end to end.

---

# CHANGELOG — rick 0.1.17 (prompt-cache & token optimizations)

Date: 2026-08-10 · Implements the hermes-agent-informed token & prompt-cache
optimizations from `PLAN.md` (P1-4, P1-3, P0-3, P0-2, P1-2, P1-1, P1-5, P2-1,
P2-2, P2-3, P2-4). All changes verified with `go build ./...`, `go vet ./...`,
`go test ./... -count=1`, and gofmt (LF-normalized working tree).

---

## What this does for you (the short version)

Rick already kept the provider prompt cache warm with a byte-pinned system
prompt, append-only views and content-addressed dedup. This release closes the
remaining gaps measured in long sessions — **fewer cold re-bills, a smaller
fresh tail every turn, and zero-billing repeated requests**:

- **OpenRouter response cache (zero-billing repeats).** Identical requests —
  retries, warm, keep-alive, the same sub-agent prompt twice — are now served
  from OpenRouter's response cache at **zero tokens** (`X-OpenRouter-Cache`
  header), on by default, with a `HIT` flag recorded per request in the
  session telemetry.
- **Cron/CI runs share a warm prompt-cache bucket.** One-shot runs (headless,
  rickserve, CI) derive the prompt-cache key from the *stable prompt content*
  instead of a fresh session id, so a scheduled job that runs the same prompt
  every hour hits the provider's prefix cache instead of cold-writing it each
  run. Separate conversations still never collide.
- **Old bulky tool results are aged out.** Once a tool result is older than a
  couple of turns and bigger than ~8 KiB, it is replaced once by a
  deterministic one-line summary (tool + command/path + size), with the
  original still retrievable via `retrieve_uncompressed_context`. The commit
  is gated on a real reclaim (≥4 KiB) and re-armed by growth, so it fires
  episodically — one cache boundary — instead of rewriting the head every
  turn. 30–60% fresh-tail shrink on tool-heavy sessions.
- **The trim budget charges only what the wire ships.** Stale reasoning blocks
  that a one-shot cap strips are no longer counted when deciding how much
  history to keep, so trimming keeps more *useful* content at the same token
  budget and distillation fires later.
- **Compaction is bounded and redacted.** The distill/`/compact` aux call now
  sends a capped head+tail transcript (no thinking traces, per-message cap),
  with secrets (API keys, bearer tokens, passwords) masked at the boundary so
  a credential can never persist in a summary that re-enters the prompt.
- **Qwen/Kimi/MiniMax get explicit caching.** These OpenAI-compatible
  gateways honor an Anthropic-style `cache_control` marker on the stable
  system message; rick now emits it (plain OpenAI / DeepSeek-line endpoints
  are untouched).
- **Summarizer outages degrade gracefully.** If the distill or compaction
  provider fails (429, timeout), a deterministic LLM-free handoff summary is
  emitted instead of dead air — the session keeps usable continuity at zero
  aux tokens.
- **Anti-thrash + better accounting.** Two compactions that save <10% pause
  automatic compaction (with a notice) instead of burning aux tokens on
  dead-end folds. Images are charged a flat ~1600 tokens in the budget (not
  their base64 byte length), and the fallback token estimator is CJK-aware.

---

## What changed

### 1. OpenRouter response cache (`internal/provider/openai`, config)
- New config: `cache_openrouter_response` (default **true**) and
  `cache_openrouter_response_ttl`.
- `SetOpenRouterResponseCache` on the OpenAI client; `doCompletions` sends
  `X-OpenRouter-Cache: true` (+ optional TTL) for OpenRouter endpoints;
  `X-OpenRouter-Cache-Status: HIT` surfaces on `provider.Usage.ResponseCacheHit`
  and is persisted per request in the session `requests` telemetry.

### 2. Content-scoped prompt-cache key (`provider.Request.CacheScopeKey`)
- New `CacheScopeKey` on `provider.Request`; `promptCacheKey` uses it when
  set, else the session id.
- `agent.CacheScopeKeyFor(model, stableSystem, tools)` derives a digest of the
  stable prompt; headless (the cron/CI/one-shot path) passes it so repeated
  identical runs share a warm bucket. Interactive sessions keep session-scoped
  keys.

### 3. Proactive tool-result pruning (`pkg/contextbudget`)
- `Budget.PruneOldToolResults` replaces old (>2-turn live zone) bulky (>8 KiB)
  tool results with deterministic one-line summaries, stored by content
  address for retrieval. Commits are gated on ≥`PruneMinReclaimBytes` (4 KiB)
  and re-armed by view growth (`NotePruneGrowth`) — episodic, write-once per
  tool_use_id.
- `buildRequest` runs the prune after trimming, before distillation
  (distillation supersedes it), and resets the stable-head sentinel on commit.

### 4. Wire-accurate trim budget (`internal/agent`)
- The budget plan now counts the **capped view** (post one-shot reasoning
  cut) instead of the raw transcript, so stripped stale thinking is not
  charged against the retained-history budget.

### 5. Bounded + redacted compaction input (`internal/agent/distill.go`, TUI)
- `renderTranscript` keeps the first 12 / last 6 messages, caps each at
  4 000 chars, strips thinking, and masks secrets (`redactBoundarySecrets`).
- `agent.CompactBoundMessages` bounds + redacts the TUI `/compact` head before
  it reaches the summarizer.

### 6. Provider cache-dialect hook (`internal/provider/openai`)
- `cacheControlMarked()` emits an Anthropic-style `cache_control` marker on
  the stable system message for Kimi/Moonshot, MiniMax, and Qwen-line
  endpoints; others are untouched.

### 7. Deterministic fallback summary (`internal/agent/distill.go`)
- `providerSummarizer.Summarize` returns a static, LLM-free handoff (recent
  asks, bounded) on any provider error/timeout/empty output instead of failing
  distillation.

### 8. Compression anti-thrash (TUI)
- Compactions saving <10% of the context count as ineffective; two strikes
  pause automatic compaction with a system notice (reset on new/resume).

### 9. Token accounting (`internal/tokens`, `internal/history`)
- `tokens.CountMessage` charges base64 image blocks at a flat
  `ImageTokenEstimate` (1600) instead of their byte length; used by the agent
  and history trim budgets.
- `conservativeFallback` is CJK-aware (Han/Hangul/Kana ≈ 1 token each), so
  CJK transcripts trim/distill at the right threshold.

---

## Files changed (this change set only)

- `internal/provider/provider.go` — `Usage.ResponseCacheHit`,
  `Request.CacheScopeKey`.
- `internal/provider/openai/openai.go` — response-cache header + status,
  scope-keyed prompt cache, `wireCacheControl` marker, `cacheControlMarked`.
- `internal/agent/agent.go` — prune wiring, capped-view budget plan,
  `CacheScopeKeyFor`, `serializeViewBytes`, image-aware count.
- `internal/agent/distill.go` — bounded/redacted transcript,
  `CompactBoundMessages`, static fallback summary.
- `pkg/contextbudget/contextbudget.go` — `PruneOldToolResults`,
  `NotePruneGrowth`, `summarizeToolResult`, prune options + state.
- `internal/history/history.go` — image-aware `messageTokens`.
- `internal/tokens/tokens.go` — `CountMessage`, `ImageTokenEstimate`,
  CJK-aware `conservativeFallback`.
- `internal/tui/{modals,model,agentbridge}.go` — `/compact` bound+redact,
  anti-thrash strikes, response-cache telemetry.
- `internal/headless/headless.go` — content-scoped cache key for one-shots.
- `cmd/rick/main.go`, `cmd/rickserve/main.go`, `cmd/cachehit/main.go` —
  `SetOpenRouterResponseCache` wiring.
- `internal/config/{config,load}.go`, `rick.json.schema.json` —
  `cache_openrouter_response`, `cache_openrouter_response_ttl`.
- `internal/session/session.go` — `RequestUsage.ResponseCacheHit`.
- Tests: OpenRouter response-cache header/HIT, scope-key sharing,
  prune commit-once-and-rearm, deterministic summaries, cache_control marker,
  CJK estimator.

## Cache-impact gate notes (for the PR body)

Cache-sensitive paths touched: `internal/agent/`, `internal/budget/`,
`internal/config/`, `internal/history/`, `internal/provider/`,
`pkg/contextbudget/`, `internal/tui/agentbridge.go`.

- **Cache-impact:** high — this change set is entirely about prompt-cache
  hit rate and token spend. Byte-stability invariants are preserved: every
  rewrite (prune, distill, reasoning cut) is a one-shot, write-once boundary
  with the stable-head sentinel reset, so the provider view resumes
  append-only growth after each. Response caching and scope keys are opt-in
  per provider.
- **Cache-guard:** `go test ./... -count=1` (all packages pass), the existing
  cache-stability/append-only tests, plus the new write-once prune test
  (`TestBuildRequestPrunesOldToolResultsOnce`) and the OpenRouter
  header/HIT tests.
- **System-prompt-review:** byte-stability of the system prefix is unchanged —
  `SystemStable` splitting still sends the stable head first; the new
  cache_control marker only *labels* the existing stable system message for
  marker-capable gateways and is omitted everywhere else.


---

# CHANGELOG — rick 0.1.18 (terminal copy & paste)

Date: 2026-08-10 · Fixes copy/paste in the rick TUI so it behaves like a
normal terminal: instant full-text paste, multi-line paste as a single
message, and Shift+drag terminal-native text selection. All changes verified
with `go build ./...`, `go vet ./...`, `go test ./... -count=1`, and gofmt.

---

## What this does for you (the short version)

- **Paste is instant, not typed.** Previously, pasting into the chatbar
  delivered the text character-by-character (each pasted char arrived as a
  separate key event on Windows), which visibly lagged on long pastes. Rick
  now reads the Windows clipboard directly (`CF_UNICODETEXT`) and inserts the
  whole string into the input in one operation — no per-char retyping, no
  lag, even for multi-kilobyte pastes.
- **Multi-line paste works.** Pasting several lines at once now lands as a
  single multi-line input in the chatbar. It no longer fires one submit per
  pasted line (the old behaviour when each pasted newline arrived as an Enter
  key event). Enter still sends the whole message.
- **Select and copy like a normal terminal.** Shift+click / Shift+drag is
  reserved for the terminal's own selection (Windows Terminal's Shift+drag
  selects even while rick captures the mouse), so you can select transcript
  text and copy it with Ctrl+Shift+C exactly like a terminal. Rick's own
  click features (wheel scroll, tool expand/collapse, double-click path copy)
  are untouched.
- **No double-paste.** When the terminal also delivers its native paste after
  rick's direct clipboard read (Windows Terminal converts Ctrl+V into
  per-character events), rick detects the duplicate and drops it, so a paste
  is never inserted twice.
- **Path copy uses the native clipboard.** Double-clicking a file path now
  writes to the Windows clipboard directly (with OSC52 as a fallback), so the
  copied path is available everywhere, not just in terminals that support
  OSC52.

---

## What changed

### 1. Direct clipboard text read/write (`internal/tui/clipboard_win.go`)
- `readClipboardText` reads `CF_UNICODETEXT` (NUL-terminated, CRLF→LF
  normalized, 16 MiB cap).
- `writeClipboardText` writes text to the clipboard via `GlobalAlloc` +
  `SetClipboardData` (CRLF convention, ownership transferred to the
  clipboard).
- `clipboard_unix.go` gains matching stubs.

### 2. Instant paste in the chatbar (`internal/tui/keys.go`)
- `handleClipboardPaste` now tries **text first**: reads the clipboard and
  `InsertString`s it (the textarea sanitizes and splits on newlines into
  rows). Images and files still work as before.
- Paste suppression: after a direct text paste, per-character key events from
  the terminal's own native paste are buffered and dropped while they
  prefix-match the pasted text; a diverging rune ends suppression and is
  replayed, so real typing is never lost.

### 3. Terminal-native selection (`internal/tui/model.go`)
- Mouse events with Shift held (except wheel) are passed through to the
  terminal instead of being consumed by rick, enabling Shift+drag selection
  and Ctrl+Shift+C copy.
- Double-click path copy prefers `writeClipboardText` (native Windows
  clipboard) with OSC52 fallback.

### 4. Help text (`internal/tui/help.go`)
- Documents `ctrl+v` (instant, multi-line safe) and Shift+click/drag
  selection with Ctrl+Shift+C copy.

### 5. Tests
- `paste_test.go`: suppression drops terminal re-delivery of a paste and
  replays a diverged buffer instead of losing keystrokes.
- `clipboard_win_test.go`: Windows clipboard text round-trip.

---

## Files changed (this change set only)

- `internal/tui/clipboard_win.go` — `readClipboardText`, `writeClipboardText`,
  `CF_UNICODETEXT`, clipboard procs.
- `internal/tui/clipboard_unix.go` — stubs.
- `internal/tui/keys.go` — instant text paste, paste-suppression guard.
- `internal/tui/model.go` — Shift-pass-through for terminal selection,
  native-clipboard path copy.
- `internal/tui/help.go` — key/mouse help for paste & selection.
- `internal/tui/paste_test.go` — new suppression tests.
- `internal/tui/clipboard_win_test.go` — text round-trip test.

## Cache-impact gate notes (for the PR body)

Cache-sensitive paths touched: none (TUI input/clipboard only; no provider
view, system prompt, or history bytes change).

- **Cache-impact:** none — this change only affects how text enters/leaves
  the chat input and how the terminal reports mouse events. No prompt,
  message, or context-budget bytes change.
- **Guard:** `go test ./... -count=1` (all packages pass) plus the new paste
  suppression and clipboard round-trip tests.


---

# CHANGELOG — rick 0.1.19 (/auth provider search & ordering)

Date: 2026-08-10 · Improves the /auth provider picker: A-Z ordering, a
"+ Add Provider" row at the top, and live type-to-search. All changes
verified with `go build ./...`, `go vet ./...`, `go test ./... -count=1`,
and gofmt.

---

## What this does for you (the short version)

- **Providers sort A-Z.** The /auth list is now alphabetical (configured
  providers first, then the rest of the catalog), so you can find what you
  are looking for by reading down the list.
- **"+ Add Provider" is the first row.** You no longer have to remember to
  type "add" in the input — the custom-provider flow is the first selectable
  entry. Typing "add", "a" or "+" still works as a shortcut.
- **Type to search.** As you type, the list narrows live to providers whose
  name, id, or detail contains what you typed (case-insensitive substring).
  The first matching provider is highlighted automatically, so you can type
  "deep" and press Enter to configure DeepSeek — no arrows needed. Esc (or
  Ctrl+U) clears the search; Backspace edits it.
- **Search never hides the add flow.** "+ Add Provider" stays pinned at the
  top even while searching, so adding a custom endpoint is always one Enter
  away.

---

## What changed

### 1. A-Z ordering + sentinel row (`internal/tui/auth.go`)
- `rebuildAuthRows` now sorts both the connected and available runs by
  label, and prepends a `+ Add Provider` sentinel row (`authRow.addProvider`).
- Selecting the sentinel (Enter on row 0) routes to the same custom-provider
  add flow as typing "add" (new `authStartAdd` helper).

### 2. Live search (`internal/tui/auth.go`)
- New `authState.query`; typing appends to the query, Backspace trims it,
  Esc and Ctrl+U clear it. `authFilteredRows` filters by case-insensitive
  substring over label/id/detail, always keeping the sentinel.
- The list body shows a "N match" line and "esc clears search" while a query
  is active. The hint line now reads "type to search · ↑↓ select · …".
- Esc with an active search clears the search first (a second Esc backs out),
  so search never traps you in the list.
- Enter with a non-empty query that has no exact name/number match selects
  the highlighted filtered row, so "deep" + Enter configures DeepSeek.

### 3. Tests (`internal/tui/auth_search_test.go`)
- A-Z ordering with sentinel first; search filters rows; typing+Enter selects
  the match; Esc clears search before backing out; sentinel selection starts
  the add flow; sentinel renders in the body; Backspace edits the query.

---

## Files changed (this change set only)

- `internal/tui/auth.go` — `authRow.addProvider`, `authState.query`,
  `authFilteredRows`, `authStartAdd`, reworked `authListKey` (search keys),
  sorted `rebuildAuthRows`, list-body search status, hint text.
- `internal/tui/auth_search_test.go` — new tests for ordering, search,
  sentinel selection, and Esc/Backspace behaviour.

## Cache-impact gate notes (for the PR body)

Cache-sensitive paths touched: none (TUI-only change; no provider view,
system prompt, or history bytes change).

- **Cache-impact:** none — this only changes how the /auth provider list is
  ordered, searched, and navigated. No prompt, message, or context-budget
  bytes change.
- **Guard:** `go test ./... -count=1` (all packages pass) plus the new
  auth-search tests.


---

# CHANGELOG — rick 0.1.20 (native text selection in chat & promptbar)

Date: 2026-08-10 · Makes selecting/copying text in the chat and the input
bar work like a normal terminal: the TUI no longer captures the mouse in the
plain chat view, so the terminal owns drag selection and Ctrl+Shift+C copy.
All changes verified with `go build ./...`, `go vet ./...`,
`go test ./... -count=1`, and gofmt.

---

## What this does for you (the short version)

- **Select text in the chat and the prompt bar — for real this time.** Rick
  previously enabled terminal mouse capture in the ordinary chat view, which
  makes the terminal hand every mouse event to the app and disables its own
  drag selection. Now the chat + input view runs **without** mouse capture, so
  you can click-drag to select any text in the transcript or the input bar and
  copy it with Ctrl+Shift+C — exactly like a normal terminal. The earlier
  Shift+drag workaround could not work because the terminal had already
  surrendered the mouse to rick.
- **Scroll wheel still works.** Windows Terminal delivers the wheel as a rapid
  burst of up/down key events when mouse capture is off; rick detects a
  same-direction key burst (3+ within 150 ms) and scrolls the transcript
  instead of navigating prompt history. Real arrow keys (history browsing) are
  unaffected.
- **Interactive overlays keep mouse clicks.** The /auth provider list, web
  search, permission prompts, choice menus, the activity panel, and the resume
  browser still capture the mouse, so their click targets (buttons, rows,
  wheel) keep working.
- **Opt-out switch.** If you prefer full mouse capture everywhere, set
  `tui.mouse: true` in your config (the previous default). The default is now
  `false`.

---

## What changed

### 1. Mouse capture scoped to interactive surfaces (`internal/tui/model.go`)
- `wantsMouseCapture` now returns `false` for the ordinary chat + input view
  (terminal owns selection) and `true` only for overlays that need clicks:
  auth, web, permission modal, choice menus, focused activity panel, resume
  browser. `tui.mouse: true` forces capture everywhere (legacy behavior).
- `Model.Init` no longer unconditionally sends `EnableMouseCellMotion` — it
  only enables mouse capture when an interactive surface is active, and syncs
  `mouseEnabled` so the Update toggle knows the starting state. This is the
  actual fix: previously the terminal was put into mouse mode at startup no
  matter what, which disabled native selection even though the rest of the
  logic wanted it off.

### 2. Wheel-as-keys burst detection (`internal/tui/keys.go`)
- New `wheelKey` tracking + `isWheelKeyBurst`: records up/down key events and
  treats a rapid (≤150 ms) run of ≥3 same-direction presses as a scroll-wheel,
  scrolling the viewport. Isolated or direction-changing presses fall through
  to normal arrow-key behavior (prompt history, slash-command cursor).

### 3. Default + docs (`internal/config/config.go`, `internal/tui/help.go`)
- `tui.mouse` default flipped to `false` (terminal selection on).
- Help text now says "drag / shift+drag — select text — copy with
  ctrl+shift+c".

### 4. Tests
- `TestMouseCaptureOffInChatViewForNativeSelection`: chat view does not
  capture; auth/web/activity do; `tui.mouse: true` forces capture.
- `TestWheelKeyBurstRequiresSameDirection`: isolated Up browses history; a
  same-direction burst scrolls; a direction change breaks the burst.
- Test model helper now carries minimal `deps` so these paths are nil-safe.

---

## Files changed (this change set only)

- `internal/tui/model.go` — scoped `wantsMouseCapture`, wheel-burst state.
- `internal/tui/keys.go` — `isWheelKeyBurst`, wheel-scroll handling.
- `internal/config/config.go` — `Mouse` default `false`.
- `internal/tui/help.go` — selection help text.
- `internal/tui/inline_model_test.go` — updated mouse-capture test + deps.
- `internal/tui/paste_test.go` — wheel-burst test.

## Cache-impact gate notes (for the PR body)

Cache-sensitive paths touched: none (TUI input/mouse handling only; no
provider view, system prompt, or history bytes change).

- **Cache-impact:** none — this only changes which terminal events the TUI
  consumes in the chat view. No prompt, message, or context-budget bytes
  change.
- **Guard:** `go test ./... -count=1` (all packages pass) plus the new mouse
  and wheel-burst tests.


---

# CHANGELOG — rick 0.1.21 (fix paste auto-send on Windows)

Date: 2026-08-10 · Fixes a Windows-only bug where pasting multi-line text
with Ctrl+V could auto-submit the message. All changes verified with
`go build ./...`, `go vet ./...`, `go test ./... -count=1`, and gofmt.

---

## What this does for you (the short version)

- **Ctrl+V no longer auto-sends.** When pasting multi-line text, Windows
  Terminal re-delivers the paste as per-character events, and the pasted line
  breaks arrive as Enter-family key presses (enter / ctrl+j / ctrl+m). Rick
  was forwarding those to the submit path, so a paste with newlines could
  send the message immediately. Those re-delivered Enter keys are now dropped
  for a short window after a multi-line paste — the text lands in the input
  as a single multi-line message, exactly as intended.
- Single-line pastes are unaffected, and pressing Enter after the short
  suppression window submits normally.

---

## What changed

### 1. Drop paste-redelivered Enter keys (`internal/tui/keys.go`)
- New `pasteNewlineUntil` timestamp on the Model: set for 500 ms whenever a
  pasted clipboard string contains a newline.
- `handleKey` now drops `enter` / `ctrl+j` / `ctrl+m` while that window is
  active, before any submit or newline-insert logic runs. This covers the
  terminal's re-delivery of the pasted line breaks even after the rune-match
  suppression has consumed the character events.

### 2. Tests (`internal/tui/paste_test.go`)
- `TestPasteNewlineRedeliveryDoesNotSubmit`: Enter-family keys during the
  paste window are dropped (no submit, no stray newline); Enter after the
  window expires submits normally.

---

## Files changed (this change set only)

- `internal/tui/keys.go` — newline-redelivery suppression in `handleKey`.
- `internal/tui/model.go` — `pasteNewlineUntil` field.
- `internal/tui/paste_test.go` — new test.

## Cache-impact gate notes (for the PR body)

Cache-sensitive paths touched: none (TUI input handling only; no provider
view, system prompt, or history bytes change).

- **Cache-impact:** none — this only changes how re-delivered Enter keys are
  handled after a paste. No prompt, message, or context-budget bytes change.
- **Guard:** `go test ./... -count=1` (all packages pass) plus the new paste
  newline test.
