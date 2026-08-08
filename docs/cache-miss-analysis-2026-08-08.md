# The 1 cache miss in session 2026-08-08T19-40-23_5b3c — provider or client?

Date: 2026-08-08 · Provider: `deepseek/deepseek-v4-flash` · 192 requests ·
99.4% cache hit (25.2M cache_read / 25.4M prompt tokens)

## 1. The miss

| req | input | cache_read | hit | when |
|---|---|---|---|---|
| 33 | 181 | 60,544 | 99.7% | last warm request before the gap |
| **34** | **51,332** | **7,168** | **12.3%** | first request after the user's 2nd message |
| 35 | 290 | 61,696 | 99.5% | immediately re-warm after the re-bill |

The re-bill is 51,332 of the session's 163,588 total input tokens (31%) on a
single turn; every other request carried only a small fresh tail (100–3,000
tokens). The 7,168 tokens still served from cache ≈ the stable system/tools
head (~6.8k, cached since the previous same-cwd session at 19:39).

## 2. Evidence it is the provider, not rick

1. **Zero divergence events.** The runner's `trackPrefix` hashes every
   provider-facing message and emits `EvCacheDivergence` on any byte change
   before the previous tail; this session persisted none. No head-trim (384
   messages, cap 500; no `[Internal: the earliest` message), no distill
   (transcript ≈ 87k tokens, far below the 55% fold point), no reasoning-cut,
   no compact-summary, no dedup-of-already-sent messages (dedup decisions are
   permanent per `tool_use_id` and only rewrite the duplicate at the tail).
   The rewrite markers found in the transcript were false positives — grep
   output quoting source strings.
2. **The gap-free benchmark evicts too.** The 100-turn cachehit benchmark
   (byte-stable harness, no idle gaps, 0 client divergences) showed the same
   class of mid-session full re-bills on this endpoint: pass 1 turn 35 →
   7.00%, pass 2 turn 15 → 62.89%, turn 30 → 73.01%. The provider evicts the
   transcript chain periodically regardless of client bytes.
3. **Immediate re-cache.** Request 35 reads 61,696 — the prefix re-cached on
   the very next turn (DeepSeek auto-caching after a cold request), so the
   miss was a one-off, not a persistent drift.
4. **The miss sits at the user-turn boundary.** Requests 29–33 were
   perfectly warm; the user read the analysis summary, then sent "yes
   implement all of that carefully". An idle gap of the user's reading time
   is the only external event between req 33 and 34.

## 3. Verdict

**Provider-side automatic-cache eviction during the user's idle gap** — most
likely the serving-side cache TTL/rollover of this flash tier (the same
behavior appears in the gap-free benchmark), possibly amplified by the gap.
Not a client byte bug: the divergence tracker proves the prefix was never
rewritten, and the re-bill healed itself next turn.

Residual client-side factor: the *running* binary's fixed 5-minute cache TTL
decides when the idle-gap pre-warm fires. A warm before req 34 (if the gap
exceeded 5 min) would have re-primed the prefix and turned the 51k re-bill
into a hit — but it cannot prevent the gap-free evictions the benchmark sees,
and the new per-vendor table (24 h for DeepSeek-line) fires such warms even
less often. The two knobs trade a cheap warm request per gap against a rare
full re-bill; the endpoint's *real* TTL is the unknown.

## 4. What could be done (plan)

### Implemented with this analysis
- **D3 — per-request eviction labels.** `RequestUsage.Eviction` now records
  the miss cause on the turn it happens ("idle gap (cache expired)" /
  "provider served no prefix cache" / "prefix change: …"); the analyzer
  prints it. The next gap-miss session will be attributable from the session
  file instead of reconstructed.
- The `cachehit --idle <dur>` harness flag already exists to measure the
  endpoint's real idle-gap TTL empirically (run pass 1, idle N minutes, run
  pass 2, compare hit) — needs a paid key to be meaningful.

### Proposed next (not yet done)
1. **Measure the real TTL** with `cachehit --idle` (5 m / 15 m / 60 m gaps)
   on the paid tier; the vendor table then uses the measured value.
2. **Adaptive warm threshold**: instead of a static TTL, track observed
   evictions per (endpoint, model) in the session store; when a gap-eviction
   recurs at gap G, set the warm threshold just below G for that endpoint.
3. **Optional `cache_warm: turn` mode**: full-view warm before every real
   turn (one cheap non-streaming request per turn, cached input billed at
   the discounted rate) for sessions where a single cold re-bill costs more
   than the warm requests. The P1c warm path already exists; this only
   changes the trigger condition.
4. **Shrink the re-bill surface**: the 51k chain is mostly tool outputs +
   reasoning echo; the existing knobs (dedup, live-zone cap,
   `cache_max_reasoning_turns`, earlier distill) reduce the fresh-tail and
   re-bill cost when the free tier evicts regardless.

## 5. Cost impact

The miss re-billed ~51k tokens once; at flash-tier pricing this is
sub-cent and cost ~0.2 percentage points of session hit rate. Not worth
sacrificing correctness for; worth the telemetry above.
