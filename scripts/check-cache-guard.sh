#!/usr/bin/env bash
# Cache-impact PR gate.
#
# Reads the PR body from $PR_BODY and the PR's changed file paths from stdin
# (one per line). A diff that touches any cache-sensitive path must declare its
# cache impact and its guard; a diff that can change the byte-stable prefix
# (system prompt / tool schemas / context budget) must name a system-prompt
# reviewer. Mirrors reasonix's cache-impact gate so prompt- or tool-shaping
# changes are always reviewed against the byte-stability invariant.
set -euo pipefail

SENSITIVE_PATHS=(
  "internal/agent/" "internal/budget/" "internal/config/" "internal/session/"
  "internal/history/" "internal/provider/" "internal/tools/" "pkg/contextbudget/"
  "internal/tui/agentbridge.go" "internal/tui/subagent.go"
)
SYSTEM_PROMPT_PATHS=(
  "internal/agent/prompt.go" "internal/agent/distill.go" "internal/budget/"
  "internal/config/"
)

field() {
  # Normalize markdown bullets/asterisks so "**Cache-impact:**" and
  # "- Cache-impact:" both match what the gate expects.
  printf '%s\n' "${PR_BODY:-}" | sed -E 's/^[-*# ]+//; s/\*\*//g' | grep -E "^${1}" || true
}

main() {
  local has_sensitive=false has_prompt_sensitive=false
  while IFS= read -r file; do
    for p in "${SENSITIVE_PATHS[@]}"; do
      [[ "$file" == "$p"* ]] && has_sensitive=true
    done
    for p in "${SYSTEM_PROMPT_PATHS[@]}"; do
      [[ "$file" == "$p"* ]] && has_prompt_sensitive=true
    done
  done
  [[ "$has_sensitive" != "true" ]] && exit 0

  local impact cache guard review
  impact=$(field "Cache-impact:")
  cache=$(field "Cache-guard:")
  if [[ -z "$impact" || -z "$cache" ]]; then
    echo "error: PR touches cache-sensitive code but is missing PR-body headings:"
    echo "  Cache-impact: <none|low|medium|high> - <reason>"
    echo "  Cache-guard: <focused guard test/command or existing guard rationale>"
    exit 1
  fi
  case "$impact" in
    Cache-impact:\ none*|Cache-impact:\ low*|Cache-impact:\ medium*|Cache-impact:\ high*) ;;
    *)
      echo "error: Cache-impact must be one of none|low|medium|high - <reason>"
      exit 1
      ;;
  esac

  if [[ "$has_prompt_sensitive" == true ]]; then
    review=$(field "System-prompt-review:")
    if [[ -z "$review" ]]; then
      echo "error: change can rewrite the system-prompt prefix; add:"
      echo "  System-prompt-review: <reviewer name or approval note>"
      exit 1
    fi
  fi
  echo "cache-impact gate ok"
}

main "$@"