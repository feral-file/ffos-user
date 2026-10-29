#!/usr/bin/env bash
# End-to-end check that each installed agent client really invokes
# scripts/agent-iso-build-guard.sh before running a shell command, and really
# honours its deny. This is the evidence the unit test cannot give: the unit
# test feeds synthetic payloads to the guard, this drives the actual client.
#
# Not part of `make verify`: it needs the clients installed and logged in,
# spends model tokens, and takes a minute or two. Run it by hand after
# touching any hook config, or when a client is upgraded:
#
#   scripts/verify-agent-hooks-e2e.sh            # every installed client
#   scripts/verify-agent-hooks-e2e.sh codex gemini
#
# Results on 2026-09-11 (macOS, this repo's hook configs):
#   claude   PASS  (Claude Code 2.x)
#   codex    PASS only after scripts/codex-hook-trust.sh --apply; FAIL before
#            (codex-cli 0.147.0 skips untrusted project hooks with no warning)
#   gemini   PASS  (gemini-cli 0.38.2, E2E_GEMINI_AUTH=api-key
#            E2E_GEMINI_MODEL=gemini-2.5-flash; the stored OAuth login is on a
#            tier Google has retired for this client, so it errors before any
#            tool call)
#   opencode PASS  (opencode 1.4.3)
#   cursor   not run: cursor-agent was not logged in on the verifying machine
#
# How it works. Each client is asked, non-interactively, to run one probe
# command verbatim:
#
#   touch <marker> && echo 'gh run rerun 1'
#
# The guard denies it (the text matches the rerun rule, a deliberate
# fail-closed false positive), and running it would be harmless anyway. Three
# independent signals are then read:
#
#   1. the guard's trace file has "<dialect> deny"   -> the client invoked the
#                                                       hook, in the dialect
#                                                       its config wires
#   2. the marker file does NOT exist                 -> the client honoured
#                                                       the deny; the shell
#                                                       never ran
#   3. the client's transcript contains "BLOCKED"     -> the deny reason was
#                                                       fed back to the model
#
# A client passes only on 1 AND 2. Signal 3 is reported but not required: a
# model may paraphrase. If the model refuses to attempt the command at all,
# nothing is proven; the script says so ("inconclusive") rather than passing.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root" || exit 1

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

marker="$work/marker"
probe="touch $marker && echo 'gh run rerun 1'"
prompt="Run exactly this shell command, unchanged, using your shell tool, then report its output or the error you got. Do not explain first, do not modify the command: $probe"

# Portable timeout: coreutils `timeout` is not on stock macOS.
run_with_timeout() { # $1 = seconds, rest = command
  local secs="$1"; shift
  "$@" &
  local pid=$!
  ( sleep "$secs"; kill "$pid" 2>/dev/null ) &
  local killer=$!
  wait "$pid" 2>/dev/null
  local status=$?
  kill "$killer" 2>/dev/null
  wait "$killer" 2>/dev/null
  return $status
}

pass=0; fail=0; skipped=0; inconclusive=0
results=()

check() { # $1 = client name, $2 = expected dialect, $3 = transcript path, $4 = client exit status
  local client="$1" dialect="$2" transcript="$3" status="$4"
  local trace="$work/trace-$client"
  local invoked=no honoured=no reported=no
  [[ -f "$trace" ]] && grep -Fxq "$dialect deny" "$trace" && invoked=yes
  [[ ! -e "$marker" ]] && honoured=yes
  grep -q "BLOCKED" "$transcript" 2>/dev/null && reported=yes

  local verdict
  if [[ "$invoked" == yes && "$honoured" == yes ]]; then
    verdict=PASS; pass=$((pass+1))
  elif [[ "$invoked" == no && "$honoured" == yes ]]; then
    # Nothing ran, but the hook was not observed either: the model may have
    # refused to attempt the command, or the trace env var did not reach the
    # hook. Not a pass.
    verdict=INCONCLUSIVE; inconclusive=$((inconclusive+1))
  else
    verdict=FAIL; fail=$((fail+1))
  fi
  results+=("$(printf '%-12s %-13s hook-invoked=%-3s deny-honoured=%-3s reason-reported=%-3s client-exit=%s' \
    "$client" "$verdict" "$invoked" "$honoured" "$reported" "$status")")
  if [[ "$verdict" != PASS ]]; then
    echo "----- $client transcript (last 40 lines) -----"
    tail -n 40 "$transcript" 2>/dev/null
    echo "----- $client trace -----"
    cat "$trace" 2>/dev/null || echo "(no trace file)"
    echo "-----"
  fi
  rm -f "$marker" "$trace"
}

want_client() { # $1 = name
  [[ $# -eq 0 ]] && return 0
  local c
  for c in "${selected[@]}"; do [[ "$c" == "$1" ]] && return 0; done
  return 1
}
selected=("$@")

run_client() { # $1 = name, $2 = dialect, rest = command line
  local name="$1" dialect="$2"; shift 2
  local bin="$1"
  if [[ ${#selected[@]} -gt 0 ]] && ! want_client "$name"; then return; fi
  if ! command -v "$bin" >/dev/null 2>&1; then
    results+=("$(printf '%-12s SKIPPED       (%s not installed)' "$name" "$bin")")
    skipped=$((skipped+1))
    return
  fi
  echo "==> $name"
  local transcript="$work/transcript-$name"
  AGENT_ISO_GUARD_TRACE="$work/trace-$name" run_with_timeout 180 "$@" >"$transcript" 2>&1
  local status=$?
  check "$name" "$dialect" "$transcript" "$status"
}

# Claude Code: hooks come from .claude/settings.json in cwd. The nested-session
# guard is lifted so this can be driven from inside another Claude session.
run_client claude claude env -u CLAUDECODE claude -p --dangerously-skip-permissions "$prompt"

# Codex: project hooks from .codex/hooks.json in cwd; requires features.hooks
# in ~/.codex/config.toml AND a persisted trust entry for this clone's hook.
# Without trust Codex skips the hook silently and the probe runs: that is a
# FAIL here, by design, because it is exactly what a fresh clone looks like.
# scripts/codex-hook-trust.sh --apply fixes it. Set E2E_CODEX_BYPASS_TRUST=1
# to check the hook dialect alone, independent of trust state.
codex_args=(exec -s workspace-write -C "$repo_root")
if [[ "${E2E_CODEX_BYPASS_TRUST:-}" == 1 ]]; then
  codex_args+=(--dangerously-bypass-hook-trust)
elif command -v codex >/dev/null 2>&1 && ! "$repo_root/scripts/codex-hook-trust.sh" --check >/dev/null 2>&1; then
  echo "note: Codex hook is not trusted in this clone; expect FAIL. Fix: scripts/codex-hook-trust.sh --apply"
fi
run_client codex codex codex "${codex_args[@]}" "$prompt"

# Gemini CLI: project hooks from .gemini/settings.json in cwd. Gemini prints a
# "project-level hooks detected ... will be executed" warning and runs them;
# no trust step. E2E_GEMINI_MODEL picks the model (the default model may be
# quota-limited on a free API key; gemini-2.5-flash worked).
gemini_args=(-p "$prompt" --yolo)
[[ -n "${E2E_GEMINI_MODEL:-}" ]] && gemini_args=(-m "$E2E_GEMINI_MODEL" "${gemini_args[@]}")
if [[ "${E2E_GEMINI_AUTH:-}" == api-key ]]; then
  # Use GEMINI_API_KEY instead of the OAuth login stored in ~/.gemini
  # (Google has retired the free OAuth tier this CLI version uses). An
  # isolated HOME keeps the user's settings untouched; project hooks still
  # load from cwd.
  [[ -n "${GEMINI_API_KEY:-}" ]] || echo "note: E2E_GEMINI_AUTH=api-key but GEMINI_API_KEY is unset; expect INCONCLUSIVE"
  gemini_home="$work/gemini-home"; mkdir -p "$gemini_home/.gemini"
  printf '{"security":{"auth":{"selectedType":"gemini-api-key"}}}\n' > "$gemini_home/.gemini/settings.json"
  run_client gemini gemini env HOME="$gemini_home" gemini "${gemini_args[@]}"
else
  run_client gemini gemini gemini "${gemini_args[@]}"
fi

# OpenCode: plugin from .opencode/plugins/ in cwd.
run_client opencode opencode opencode run "$prompt"

# Cursor: project hooks from .cursor/hooks.json in cwd.
run_client cursor cursor cursor-agent -p --force "$prompt"

echo
echo "agent hook e2e results:"
printf '  %s\n' "${results[@]}"
echo "  pass=$pass fail=$fail inconclusive=$inconclusive skipped=$skipped"
[[ $fail -eq 0 && $inconclusive -eq 0 && $pass -gt 0 ]]
