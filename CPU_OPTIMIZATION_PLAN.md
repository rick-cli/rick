# rick (G:\projectE) — CPU Usage Optimization Plan

Date: 2026-08-08 · Symptom: process CPU climbs with normal use and spikes
around compaction; grows worse the longer a session runs.

## 1. Root cause (found & fixed)

**`trimTranscript` off-by-one re-renders the whole transcript every frame once
a session passes 500 messages.**

- `internal/tui/model.go:1127` — `trimTranscript` dropped `len-500` (=1)
  messages but its replacement (`1 system note + tail`) left the count at 501,
  still over `maxTranscriptMessages = 500`.
- `refresh()` (`model.go:1171`) calls `trimTranscript()` on **every** frame:
  the 40 ms agent-drain poll (`drainCmd`, `agentbridge.go:146`, ~25 Hz while
  streaming) and the 90 ms spinner tick (`model.go:716`) during long work.
- 501+ messages therefore re-entered the branch on every frame, called
  `m.tx.invalidateAll(...)`, and forced a **full re-render of all ~500 blocks**
  (glamour markdown, tool diffs, wrapping) once per frame even when only one
  token was streamed — defeating the block cache in `internal/tui/transcript.go`.
- Compaction is where this bites hardest: auto-compact only triggers in late,
  large sessions (context ≥70-100%, i.e. 500+ message transcripts), and the
  long "compacting…" summary stream keeps the hot frame loop ticking the whole
  time.

### Evidence

- Per-streamed-chunk cost (CPU profile of `internal/tui` + micro-benchmark):

| transcript | per chunk |
|---|---|
| ≤ 500 msgs | ~50 µs |
| 501 msgs | 953 µs (18×) |
| 1000 msgs | 899 µs; 3.14 ms in the app-aware selftest |

- Profile top sink: `(*Model).renderMsg` 64% cumulative — every block being
  re-rendered per chunk instead of only the dirty one.
- Repo's own guard was already failing: `cmd/rickui` selftest "A STREAM CHUNK
  IS UNDER 2ms" = 3.14 ms.

### Fix applied

`internal/tui/model.go:1137` — trim to the cap instead of 1 over it:

```go
remove := len(m.msgs) - maxTranscriptMessages + 1
m.msgs = append([]ChatMsg{{Kind: MsgSystem, Text: fmt.Sprintf("... %d earlier messages omitted to fit the transcript", remove), Time: time.Now()}}, m.msgs[remove:]...)
```

Transcript now sits at ≤ 500; steady-state refreshes short-circuit and cache
invalidation only happens when messages are actually dropped.

### Verification (post-fix)

- Per-chunk cost flat: 10/400/500/501/1000 msgs ≈ 32-55 µs (was 953 µs @ 501).
- `go test ./internal/...` all pass.
- `cmd/rickui` selftest: "A STREAM CHUNK IS UNDER 2ms" and "scales
  sub-linearly with backlog" now PASS (were failing).

## 2. Secondary findings (not yet fixed — ordered by expected impact)

| # | Area | Location | Description |
|---|---|---|---|
| S1 | Per-turn tokenization | `internal/agent/agent.go` `buildRequest` (~772-830) | `countMessages`/`countJSONValues` BPE-count the whole history every turn; `history.Retain`/`retainStable` (`internal/history/history.go`) tokenize it again (json.Marshal + `tokens.Count` per message). O(history) × several passes per turn. |
| S2 | Boundary marshalling | `pkg/contextbudget/contextbudget.go` `ChooseBoundaries` (~316-336) | `json.Marshal` every message twice (once for the hash, once for byteLen) + sha256 per message; runs every turn and again after a distill. |
| S3 | Per-frame scans | `internal/tui/model.go` `rebuildToolRowMap`/`rebuildChoiceButtonMap` (~1197-1250) | O(transcript) scan of all blocks on every refresh (25 Hz). Cheap today; revisit if profiles show it. |
| S4 | Usage persistence | `internal/usage/tracker.go` `persistLocked` | Full `usage.json` marshal + rename every 30 s while turns are active (already debounced; fine unless the file grows). |
| S5 | One-shot compaction costs | `internal/tui/agentbridge.go:653`, `model.go:874` | `/compact` completion runs `ChooseBoundaries` over the full history once (marshal+hash per message) before inserting the summary. |
| S6 | Tool-call HTTP churn | `internal/tools/websearch.go` | New `http.Client` (+TLS config) per call and `regexp.MustCompile` per provider invocation (prior audit `G:\cpuusageopti.md`). |

Note: S1/S2 run once per *turn*, not per frame; they are the next best win for
long sessions but far below the fixed per-frame cost.

## 3. Suggested next steps

- [x] Fix the `trimTranscript` off-by-one (this plan, §1).
- [ ] Add a regression test: stream chunks with 501+ transcript messages and
      assert `Model.RenderCount()` stays flat (cache working) and per-chunk
      cost stays < 2 ms — convert the rickui selftest check into a `go test`.
- [ ] S1: memoize per-message token counts in the agent (key = message
      identity/hash) so steady turns skip re-tokenizing the stable prefix.
- [ ] S2: compute byteLen from the already-marshalled hash payload; drop the
      second `json.Marshal` in `ChooseBoundaries`.
- [ ] S6 (from prior audit): hoist `http.Client` and regexes to package level
      in `internal/tools/websearch.go`.
- [ ] Re-run `G:\cpuusageopti.md` audit items against current HEAD.

## 4. Success metric

- Per-frame cost flat in transcript length: 500 vs 1000 messages within 2×.
- "A STREAM CHUNK IS UNDER 2ms" guard passes in CI.
- No regressions: `go build ./...`, `go test ./internal/... ./cmd/...`,
  `go vet ./...`, `gofmt` on touched files.

## 5. Cache-hit hardening (implemented 2026-08-08)

Follow-up to the session-telemetry analysis (`2026-08-08T02-13-45_75d0`):
286 requests with only ONE client-side divergence (`message@0;head-trim` at req
165), yet 11 full `read=0` re-bills of the ~170k-token prefix and a recurring
"cache_read halves every ~10 turns" pattern — the provider's LRU evicts the
oldest prefix as the live view grows past its recency window.

1. **Full-view pre-warm before likely-miss turns** (`internal/agent/agent.go`):
   the turned-block after `buildRequest` now re-primes the provider cache
   (P1c) using the exact bytes the next stream will send, when (a) an eviction
   was detected on the previous turn, (b) the view head was just rewritten
   (head-trim), (c) turn 0 resumes a long transcript, or (d) the gap past the
   last request exceeded the provider cache TTL (`cacheTTL()`: 5 min default,
   1h for long retention). Replaces the old P1b block; starts the warm with the
   same request the stream uses, so no double `buildRequest` per turn.
2. **Warm failures are surfaced, not swallowed** — `warnWarmOnce` emits one
   `EvAgentMessage` per distinct warm error per run (start warm and P1c).
3. **Honest miss accounting** (`internal/tui/agentbridge.go`): a turn that
   reports no cache fields is now counted as a full re-bill when its fresh
   `input` alone covers the previously sent span (`read=0` with `input~=prompt`
   — the exact signature the sessions showed); a small-tail turn is still not
   a miss. `cacheMissReason` distinguishes
   "idle gap (cache expired)" / "prefix change" / "provider served no prefix
   cache".
4. **Compact earlier, keep the live view inside the provider LRU window**
   (`internal/agent/distill.go` + `internal/distill/distill.go`): distill
   threshold 70% → 55% (`distillAtPercent`), and tighter `distill.Options`
   defaults (`LiveZoneTurns` 2→1, `DistillRatio` 0.5→0.4, `MaxMessages`
   48→32, `MinHistoryTokens` 4000→2000). Distillation is prefix-preserving
   (summary inserts after the cache breakpoint), so folding earlier converts
   the "oldest half evicted" tax into one small summary.

Tests: `internal/agent` `TestRunRewarmsAfterIdleGap`,
`TestRunSurfacesWarmFailure`; `internal/tui`
`TestObserveCacheUsageCountsOnlyTrueRebillsWithoutCacheFields`,
the miss-reason test now covers the no-cache-served branch. `go build ./...`,
`go test ./internal/...`, `go vet` on touched packages — all green.

Expected effect: per-TTL-breach and post-head-trim turns change from
`full-cold re-bill + re-cache` to `warm paid once + stream reads at the
discounted cached rate`; the masked full `read=0` turns are at least visible
(counted + labeled) instead of hidden; longer sessions keep the live view
inside the cached band so the halving pattern stops recurring mid-session.
