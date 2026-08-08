#!/usr/bin/env python3
"""Report the prompt-cache hit rate of rick's currently selected model.

Usage: python cache_hit_rate.py [MODEL_ID]

Mirrors the tracker formula in internal/usage/tracker.go (Day.CacheHitRate):
hit rate = cache_read / (input + cache_read), as a percentage in [0,100].

Without an argument, the model is read from the active rick.json (the same
resolution as config.GlobalDir: $RICK_HOME, then %APPDATA%/rick on Windows,
then $XDG_CONFIG_HOME/rick, then ~/.config/rick).
"""

import json
import os
import sys
from collections import defaultdict


def global_dir():
    """Return the rick config directory, matching config.GlobalDir()."""
    if env := os.environ.get("RICK_HOME"):
        return env
    if os.name == "nt":
        if appdata := os.environ.get("APPDATA"):
            return os.path.join(appdata, "rick")
    if xdg := os.environ.get("XDG_CONFIG_HOME"):
        return os.path.join(xdg, "rick")
    return os.path.join(os.path.expanduser("~"), ".config", "rick")


def selected_model():
    """Read the active model id from rick.json, or None if unset."""
    path = os.path.join(global_dir(), "rick.json")
    with open(path, encoding="utf-8") as fh:
        return json.load(fh).get("model")


def model_totals(usage_path):
    """Aggregate input/cache_read per model across every recorded day."""
    with open(usage_path, encoding="utf-8") as fh:
        data = json.load(fh)
    totals = defaultdict(lambda: {"input": 0, "cache_read": 0})
    for models in data.values():
        for model_id, usage in models.items():
            totals[model_id]["input"] += usage["total"].get("input", 0)
            totals[model_id]["cache_read"] += usage["total"].get("cache_read", 0)
    return totals


def hit_rate(input_tokens, read_tokens):
    """Share of prompt tokens served from cache, in [0, 100] (0 when none)."""
    prompt = input_tokens + read_tokens
    if prompt <= 0:
        return 0.0
    return 100.0 * read_tokens / prompt


def main():
    config_dir = global_dir()
    requested = sys.argv[1] if len(sys.argv) > 1 else selected_model()
    totals = model_totals(os.path.join(config_dir, "usage.json"))
    if requested is None:
        print("no model configured in rick.json; pass a model id explicitly")
        sys.exit(1)
    if requested not in totals:
        print(f"model {requested!r} has no recorded usage at {config_dir}\\usage.json")
        sys.exit(1)

    tokens = totals[requested]
    rate = hit_rate(tokens["input"], tokens["cache_read"])
    print(f"model:        {requested}")
    print(f"input (miss): {tokens['input']:,}")
    print(f"cache read:   {tokens['cache_read']:,}")
    print(f"cache hit:    {rate:.4f}%")
    print(f"usage file:   {config_dir}\\usage.json")


if __name__ == "__main__":
    main()