# 🚀 rick 0.1.16 → 0.1.21 — Terminal UX & Token-Cost Release

## ✂️ Copy & paste that finally feels normal
- **Instant paste** — Ctrl+V reads the clipboard directly, no per-char lag, even multi-KB.
- **Multi-line paste = one message** — no auto-send per line; 0.1.21 stops pasted newlines from triggering submit.
- **Native text selection** — drag to select in the chat & prompt bar, copy with Ctrl+Shift+C. Mouse capture is off by default (`tui.mouse: true` to opt back in).
- **No double-paste** — the terminal's re-delivery of a paste is detected and dropped.

## 🔎 /auth provider picker
- **A-Z ordering** with **"+ Add Provider" pinned first** — no more typing "add".
- **Type-to-search** — the list narrows live; type "deep" + Enter to configure DeepSeek.

## 🛠️ Tool-call repairs (0.1.16)
- Malformed `todowrite` / `parallel_tasks` / `websearch` / `swarm` calls are **auto-repaired against the schema** instead of erroring and burning a turn.
- The model is told what changed (`<repaired: …>`); markdown-linked paths unwrap automatically.
- Repair telemetry per model × tool in usage.json.

## 💰 Prompt-cache & token optimizations (0.1.17)
- **OpenRouter response cache** — identical requests (retries, warm, keep-alive) served at **zero tokens**, on by default.
- Cron/CI one-shots share a **warm prompt-cache bucket**.
- Old bulky tool results age out to one-line summaries (still retrievable); compaction is **bounded + secret-redacted**; CJK-aware token estimates.

✅ `go build` + `go vet` + `go test` — all green. Details: `changelog__3.md`
