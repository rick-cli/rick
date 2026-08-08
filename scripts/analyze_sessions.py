#!/usr/bin/env python3
"""Analyze rick session files for prompt-cache behaviour.

Usage: python analyze_sessions.py [SESSION_DIR] [GLOB]

Reads every rick session JSON, prints per session:
  - hit rate  = cache_read / (input + cache_read + cache_write)
    (the plan docs' definition; matches provider-reported prompt usage)
  - reset count (cache_read falls more than the 1k-token noise floor below
    the previously sent prompt footprint — matches the Runner's P1b detector)
  - miss turns with their persisted divergence reason (message@index;cause)

The divergence field is written by v0.1.13+ per request when the agent
detects a byte change in the provider-facing prefix before the previous tail.
"""

import glob
import json
import os
import sys


def analyze(path):
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError):
        return None
    requests = data.get("requests") or []
    if not requests:
        return None
    total_input = sum(r.get("input", 0) for r in requests)
    total_read = sum(r.get("cache_read", 0) for r in requests)
    total_write = sum(r.get("cache_write", 0) for r in requests)
    total = total_input + total_read + total_write
    hit = 100.0 * total_read / total if total else 0.0

    prev_prompt = None
    resets = 0
    unexpected = 0
    divergences = []
    for r in requests:
        read = r.get("cache_read", 0)
        # A "reset" is a full or partial prefix eviction: cache_read falls
        # well below the previously sent prompt footprint (the planner's
        # cacheMissNoiseFloor = 1024 tokens). read == 0 alone misses cases
        # where the provider re-bills the tail after an idle gap.
        prompt = r.get("input", 0) + read + r.get("cache_write", 0)
        window = max(prev_prompt, prompt) if prev_prompt is not None else 0
        if prev_prompt is not None and window > 0 and read < window - 1024:
            resets += 1
            reason = r.get("divergence") or "?"
            divergences.append((r.get("index"), reason))
            # A divergence the runner cannot attribute to its own one-shot
            # rewrites (head-trim/distill/reasoning-cut/dedup/compact) is a
            # fail-closed "unexpected" — a regression that silently re-bills
            # the provider cache every turn. It must trip the analyzer.
            if "unexpected" in reason:
                unexpected += 1
        if prompt > 0:
            prev_prompt = prompt

    return {
        "title": (data.get("title") or "")[:60],
        "model": data.get("model") or "",
        "cwd": data.get("cwd") or "",
        "requests": len(requests),
        "hit": hit,
        "input": total_input,
        "read": total_read,
        "write": total_write,
        "resets": resets,
        "unexpected": unexpected,
        "divergences": divergences,
    }


def main():
    roots = sys.argv[1:] or [os.environ.get("SESSIONS", "")]
    paths = []
    for root in roots:
        if os.path.isdir(root):
            paths.extend(glob.glob(os.path.join(root, "*.json")))
        else:
            paths.append(root)
    rows = [analyze(p) for p in paths]
    rows = [r for r in rows if r]
    rows.sort(key=lambda r: r["hit"])
    print(f"{'hit%':>6} {'reqs':>5} {'input':>9} {'read':>9} {'write':>9} {'resets':>6} {'unexp':>5}  model / title")
    for r in rows:
        print(
            f"{r['hit']:6.1f}% {r['requests']:5d} {r['input']:9d} {r['read']:9d} "
            f"{r['write']:9d} {r['resets']:6d} {r['unexpected']:5d}  {r['model'] or '?'} / {r['title']}"
        )
        for index, reason in r["divergences"]:
            print(f"          reset req {index}: {reason}")
    total_unexpected = sum(r["unexpected"] for r in rows)
    if total_unexpected:
        print("\n*** %d unexpected divergence(s) across %d sessions - "
              "mid-prefix rewrites the runner cannot attribute; "
              "investigate before trusting hit rates ***" % (total_unexpected, len(rows)))


if __name__ == "__main__":
    main()