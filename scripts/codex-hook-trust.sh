#!/usr/bin/env bash
# One-time Codex CLI setup for the ISO build guard (AGENTS.md "Release
# guardrail: ISO image builds").
#
# Codex runs a project-level hook (.codex/hooks.json) ONLY after the hook has
# been trusted in the user's ~/.codex/config.toml. Until then it is skipped
# silently: `codex exec` prints no warning, and the shell command runs
# unguarded (verified 2026-09-11 with codex-cli 0.147.0 by
# scripts/verify-agent-hooks-e2e.sh). Trust is keyed by the absolute path of
# this clone's .codex/hooks.json and by a hash of the hook entry, so it cannot
# ship in the repository, and it must be redone after any edit to
# .codex/hooks.json or after moving the clone.
#
#   scripts/codex-hook-trust.sh          print the config.toml entry
#   scripts/codex-hook-trust.sh --check  exit 0 if this clone's hook is trusted
#   scripts/codex-hook-trust.sh --apply  append the entry to ~/.codex/config.toml
#
# The hash is Codex's hook trust identity: sha256 over the canonical JSON
# (sorted keys, no whitespace) of
#   {event_name, matcher, hooks: [{type, command, timeout, async: false}]}
# with the event name in snake_case and timeout defaulting to 600. This is the
# same identity oh-my-codex writes for its own hooks, and those entries are
# demonstrably honoured by Codex; verify after applying with
# scripts/verify-agent-hooks-e2e.sh codex.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
hooks_json="$repo_root/.codex/hooks.json"
config="${CODEX_HOME:-$HOME/.codex}/config.toml"
mode="${1:-print}"

[[ -f "$hooks_json" ]] || { echo "codex-hook-trust: $hooks_json not found" >&2; exit 1; }

# Read the single PreToolUse handler. jq preferred, python3 fallback, like the guard.
read_hook() { # prints: matcher \n command-as-json-string \n timeout
  if command -v jq >/dev/null 2>&1; then
    jq -r '.hooks.PreToolUse[0] | (.matcher // ""), (.hooks[0].command | tojson), (.hooks[0].timeout // 600)' "$hooks_json"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$hooks_json" <<'EOF'
import json, sys
g = json.load(open(sys.argv[1]))["hooks"]["PreToolUse"][0]
h = g["hooks"][0]
print(g.get("matcher", ""))
print(json.dumps(h["command"]))
print(h.get("timeout", 600))
EOF
  else
    echo "codex-hook-trust: need jq or python3" >&2; exit 1
  fi
}
{ read -r matcher; read -r command_json; read -r timeout; } < <(read_hook)

identity="{\"event_name\":\"pre_tool_use\",\"hooks\":[{\"async\":false,\"command\":${command_json},\"timeout\":${timeout},\"type\":\"command\"}]"
[[ -n "$matcher" ]] && identity="${identity},\"matcher\":\"${matcher}\""
identity="${identity}}"

if command -v shasum >/dev/null 2>&1; then
  digest="$(printf '%s' "$identity" | shasum -a 256 | cut -d' ' -f1)"
else
  digest="$(printf '%s' "$identity" | sha256sum | cut -d' ' -f1)"
fi

key="${hooks_json}:pre_tool_use:0:0"
entry="$(printf '\n# %s project hook: AGENTS.md "Release guardrail: ISO image builds"\n[hooks.state."%s"]\ntrusted_hash = "sha256:%s"\n' "$(basename "$repo_root")" "$key" "$digest")"

is_trusted() {
  [[ -f "$config" ]] && grep -Fq "[hooks.state.\"$key\"]" "$config" && grep -Fq "sha256:$digest" "$config"
}

case "$mode" in
  print)
    printf '%s\n' "$entry"
    if is_trusted; then echo "# (already present in $config)"; fi
    ;;
  --check)
    if is_trusted; then
      echo "codex-hook-trust: trusted ($config)"
    else
      echo "codex-hook-trust: NOT trusted. Codex will skip the ISO build guard silently in this clone. Run: scripts/codex-hook-trust.sh --apply" >&2
      exit 1
    fi
    ;;
  --apply)
    if is_trusted; then echo "codex-hook-trust: already trusted ($config)"; exit 0; fi
    mkdir -p "$(dirname "$config")"
    if [[ -f "$config" ]] && grep -Fq "[hooks.state.\"$key\"]" "$config"; then
      echo "codex-hook-trust: $config has a stale entry for $key (hook edited since it was trusted). Remove that [hooks.state] block by hand, then rerun --apply." >&2
      exit 1
    fi
    cp "$config" "$config.bak" 2>/dev/null || true
    printf '%s\n' "$entry" >> "$config"
    echo "codex-hook-trust: appended to $config (backup: $config.bak). Verify with scripts/verify-agent-hooks-e2e.sh codex"
    ;;
  *)
    echo "usage: $0 [--check|--apply]" >&2; exit 2 ;;
esac
