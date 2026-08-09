# rick v

A fast, focused terminal AI coding agent for building, inspecting, and changing
projects without leaving your shell.

![rick terminal example](rick-term.png)

## Install

### Linux and macOS

Install the latest release directly from GitHub:

```sh
curl -fsSL \
  https://raw.githubusercontent.com/rick-cli/rick/main/scripts/install.sh | bash
```

This installs `rick` to `~/.local/bin`. If that directory is not already on
`PATH`, run:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Open a new terminal after adding it to your shell profile to make the change
permanent.

### Windows PowerShell

Run PowerShell as your normal user:

```powershell
$base = "https://raw.githubusercontent.com/rick-cli/rick/main"
irm "$base/scripts/Install-Rick.ps1" | iex
```

This installs `rick.exe` to `%LOCALAPPDATA%\Rick\bin` and adds that directory
to your user `PATH`. Open a new terminal if the current one does not see the
updated `PATH`.

The installers download the latest release for the detected platform. You can
also download release binaries manually from the
[GitHub Releases page](https://github.com/rick-cli/rick/releases).

### From source

```sh
go install ./cmd/rick
# or: go build -o rick ./cmd/rick

export ANTHROPIC_API_KEY=[REDACTED]
rick                         # in any project directory
```

Requires Go 1.24+. `ripgrep` (`rg`) is strongly recommended — grep and glob
shell out to it. `git` enables snapshot-backed undo/redo.

## Usage

```sh
rick [path]              open a session in a directory (default: cwd)
rick -p "fix the bug"    send an initial prompt
rick -m openai/gpt-5     pick a model
rick -a plan             start in plan mode
rick --new               ignore the resumable session
rick --yolo              skip all permission prompts (dangerous)

rick sessions            list saved sessions
rick config              show the resolved configuration
rick models              list available models
rick update              update to the latest GitHub release
rick uninstall           choose FULL or PART removal
rick version
```

## Keys

| Key | Action |
| --- | --- |
| `enter` | send |
| `esc` | interrupt a run / clear the input |
| `tab` | cycle build ⇄ plan (empty input), or complete a slash command |
| `@` | fuzzy file picker — inserts the file's contents into the prompt |
| `!cmd` | run a shell command directly |
| `/` | slash commands |
| `↑` `↓` | input history |
| `pgup` `pgdn` | scroll the transcript |
| `ctrl+c` | quit |

Leader key is `ctrl+x` (configurable), then:
`h` help · `m` models · `t` themes · `n` new · `l` sessions · `u` undo ·
`r` redo · `d` tool details · `c` compact.

## Slash commands

`/help` `/new` `/sessions` `/models` `/themes` `/agent` `/compact` `/undo`
`/redo` `/details` `/thinking` `/init` `/tools` `/mcp` `/plugins`
`/permissions` `/config` `/update` `/uninstall` `/exit`

`/update` downloads the latest release updater and applies it safely. `/uninstall`
asks for a scope: **FULL** removes Rick and its credentials, sessions, config, and
user data; **PART** removes only the executable and keeps that data.

Custom commands live in `.rick/commands/<name>.md` or the `command` block in
`rick.json`; `$ARGUMENTS` is substituted with whatever follows the command.

## Tools

`read` `write` `edit` `bash` `grep` `glob` `list` `apply_patch` `todowrite`
`todoread` `task`, plus every tool exposed by connected MCP servers
(registered as `<server>_<tool>`).

`edit` is exact string replacement with whitespace-tolerant fallbacks. It
refuses to touch a file the agent has not read. `apply_patch` applies
multi-file add/update/move/delete patches atomically — if any hunk fails to
match, nothing is written.

## Configuration

Two files, both accepting `//` comments and trailing commas:

- `rick.json` — runtime behaviour: providers, model, permissions, tools, MCP,
  agents, commands, instructions.
- `tui.json` — presentation: theme, keybinds, diff layout, notifications.

Precedence, later wins:

1. built-in defaults
2. `~/.config/rick/` (`%APPDATA%\rick\` on Windows)
3. `$RICK_CONFIG` (explicit path)
4. `<project root>/rick.json` and `.rick/rick.json`
5. `$RICK_CONFIG_CONTENT` (inline JSON, for tests and CI)

Tiers merge key by key — a project file overriding `permission.edit` keeps
every default bash pattern.

`{env:VAR}` and `{file:path}` are substituted inside any string value, so API
keys and long instruction blocks stay out of the config file.

```jsonc
{
  "model": "anthropic/claude-sonnet-4-5-20250929",
  "small_model": "anthropic/claude-3-5-haiku-20241022",

  "provider": {
    "openrouter": { "apiKey": "{env:OPENROUTER_API_KEY}" },
    "local":      { "baseUrl": "http://localhost:11434/v1" }
  },

  "permission": {
    "edit": "ask",
    "bash": {
      "*": "ask",
      "go test*": "allow",
      "git push*": "ask",
      "sudo*": "deny"
    }
  },

  "cache_retention": "long",
  "cache_ttl_seconds": 300,
  "cache_keepalive_seconds": 120,

  "instructions": ["docs/conventions.md"],

  "mcp": {
    "docs": {
      "type": "local",
      "command": ["npx", "-y", "@modelcontextprotocol/server-filesystem", "."]
    }
  }
}
```

Prompt-cache keys:

- `cache_retention` — `""` (provider default), `"long"` (extended TTL), or
  `"none"` (caching off). Defaults to `"long"`.
- `cache_ttl_seconds` — how long a warm prompt prefix is assumed to survive
  at the provider before an idle gap forces a re-warm. Zero uses the
  per-vendor table (1 day for DeepSeek-line endpoints). Set this to the real
  retention when a gateway — e.g. a free flash tier — expires entries in
  minutes, so the first turn after an idle gap is pre-warmed instead of
  re-billing the whole prefix cold.
- `cache_keepalive_seconds` — when positive, re-sends a session's last
  stream body as a minimal request during idle gaps so the provider keeps
  the prefix cached (near-100% hit rate across long idle even on gateways
  with minute-scale cache TTLs). Zero disables. A sensible pairing for a
  short-TTL gateway is `cache_ttl_seconds: 300, cache_keepalive_seconds: 120`.

## Permissions

Every tool call resolves to `allow`, `ask` or `deny`.

Bash patterns are globs matched against each sub-command of a compound line —
`git status && sudo reboot` resolves to the strictest level of its parts. The
most specific pattern wins, exact matches beat wildcards.

File writes outside the project root are upgraded from `allow` to `ask`
automatically; an agent can never silently modify files elsewhere on the
machine.

Approving with "allow for this session" grants the pattern for the rest of the
run only — nothing is written to disk.

## Agents

`build` (all tools) and `plan` (edit/write/bash default to ask) ship built in;
`tab` toggles them. Define more in `.rick/agents/<name>.md`:

```markdown
---
description: Reviews code for defects
mode: subagent
model: anthropic/claude-haiku-4-5
temperature: 0.2
tools:
  write: false
  edit: false
permission:
  bash: ask
---
You are a meticulous code reviewer. Report only real defects.
```

## Subagents

Two built-in types: `general` (full tools, no further delegation) and
`explore` (read-only, fast codebase search). The agent spawns them with the
`task` tool; you can invoke one directly with `@explore where is auth handled`.

`subagent_depth` (default 1) caps recursion.

## Sessions, undo, compaction

Sessions persist to `~/.local/share/rick/sessions/` (`%LOCALAPPDATA%\rick\` on
Windows) and resume automatically per directory. `/sessions` lists them.

Undo is backed by a shadow git repository that snapshots the work tree before
each mutating tool. It lives entirely outside your project and never touches
your real git history, branches or index.

`/compact` summarises the conversation with the small model and keeps the last
two exchanges verbatim.

## Themes

Four ship built in: `pickle-rick` (dark green), `rick-black` (pure black ·
neon green), `evil-rick` (blood red · dark romance), and `rick-neon`
(cyberpunk · hot pink). Drop JSON files in `~/.config/rick/themes/` or
`.rick/themes/` to add your own. A `defs` block holds reusable colour tokens;
each role may be a bare colour or `{"dark": ..., "light": ...}`.

## Layout

```text
cmd/rick/            CLI entrypoint
internal/agent/      tool-calling loop, prompts, subagents
internal/provider/   Provider interface + anthropic, openai adapters
internal/tools/      built-in tools, diff engine
internal/permission/ allow/ask/deny engine, glob matching
internal/session/    persistence, git-backed snapshots
internal/config/     layered config, JSONC, substitution
internal/mcp/        MCP client (stdio + http)
internal/plugin/     hook dispatcher
internal/theme/      theme loader, embedded built-ins
internal/tui/        bubbletea model, views, modals
```
