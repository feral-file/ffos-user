#!/usr/bin/env bash
# Pins scripts/agent-iso-build-guard.sh (the pre-shell hook that enforces
# AGENTS.md "Release guardrail: ISO image builds" for Claude Code, Codex,
# Cursor, Gemini CLI, and OpenCode). Each case feeds a hook payload to the
# guard and asserts the decision it emits for a dispatch of any ffos publishing
# workflow (ISO build, package build, pacman repo push): deny for
# release/Production, ask for staging/Staging (deny plus a hand-over instruction for tools without an
# ask primitive), silence for anything else. Also pins that every tool's hook
# config still points at the guard. Run by the repository verify target.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard="$repo_root/scripts/agent-iso-build-guard.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-agent-iso-build-guard: $*" >&2
  exit 1
}

[[ -x "$guard" ]] || fail "$guard is not executable"

# A throwaway git checkout per branch, for the "no --ref given" fallback
# (gh dispatches on the current branch in that case).
checkout_on() { # $1 = branch name -> prints path
  local dir="$tmp_dir/repo-$1"
  git init -q "$dir"
  git -C "$dir" symbolic-ref HEAD "refs/heads/$1"
  printf '%s' "$dir"
}
release_dir="$(checkout_on release)"
staging_dir="$(checkout_on staging)"
develop_dir="$(checkout_on develop)"

# Build the hook payload the way Claude Code does. The command is passed
# through python/jq-free escaping here: only quotes and backslashes matter for
# the cases below.
payload() { # $1 = command, $2 = cwd
  local escaped
  escaped="$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
  printf '{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"%s","tool_input":{"command":"%s"}}' \
    "$2" "$escaped"
}

# $1 = expected decision (deny|ask|allow), $2 = command, $3 = cwd
expect() {
  local expected="$1" command="$2" cwd="$3" out status
  set +e
  out="$(payload "$command" "$cwd" | "$guard")"
  status=$?
  set -e
  [[ $status -eq 0 ]] || fail "guard exited $status for: $command"
  case "$expected" in
    allow)
      [[ -z "$out" ]] || fail "expected silence (allow) for: $command
got: $out"
      ;;
    deny|ask)
      printf '%s' "$out" | grep -Fq "\"permissionDecision\":\"$expected\"" || \
        fail "expected $expected for: $command
got: ${out:-<nothing>}"
      printf '%s' "$out" | grep -Fq '"hookEventName":"PreToolUse"' || \
        fail "decision JSON is missing hookEventName for: $command"
      # The output must be a single valid JSON object or Claude Code ignores it.
      if command -v jq >/dev/null 2>&1; then
        printf '%s' "$out" | jq -e . >/dev/null || fail "decision is not valid JSON for: $command"
      fi
      ;;
    *) fail "bad expectation '$expected'" ;;
  esac
}

# --- release branch / Production environment: always denied ----------------
expect deny 'gh workflow run build-image-to-cf.yml --ref release -f version=2.0.4' "$develop_dir"
expect deny 'gh workflow run build-image-to-cf.yml --ref=release' "$develop_dir"
expect deny 'gh workflow run pure-build-image-to-cf.yml -r release' "$develop_dir"
expect deny 'gh workflow run build-image-from-tags.yml -R feral-file/ffos --ref release' "$develop_dir"
expect deny 'gh workflow run "Build FFOS Image" --ref release' "$develop_dir"
expect deny 'gh -R feral-file/ffos workflow run build-image-to-cf.yml --ref "release"' "$develop_dir"
expect deny 'gh workflow run build-image-to-cf.yml --ref develop -f environment=Production' "$develop_dir"
expect deny "gh workflow run build-image-to-cf.yml --ref develop -F 'environment=Production'" "$develop_dir"
expect deny 'gh api repos/feral-file/ffos/actions/workflows/build-image-to-cf.yml/dispatches -f ref=release -f inputs[version]=2.0.4' "$develop_dir"
expect deny "curl -X POST -d '{\"ref\":\"release\",\"inputs\":{}}' https://api.github.com/repos/feral-file/ffos/actions/workflows/build-image-to-cf.yml/dispatches" "$develop_dir"
expect deny "gh api --method POST /repos/feral-file/ffos/actions/workflows/pure-build-image-to-cf.yml/dispatches --input - <<< '{\"ref\": \"develop\", \"inputs\": {\"environment\": \"Production\"}}'" "$develop_dir"
# The manual package builds and the pacman repo push all write to
# {branch}/os/x86_64/ and are guarded the same way as the image builds.
expect deny 'gh workflow run manual-build-components.yaml --ref release -f component=feral-controld -f version=2.0.4' "$develop_dir"
expect deny 'gh workflow run "Manual Build Component" --ref release' "$develop_dir"
expect deny 'gh workflow run manual-build-components.yaml --ref develop -f environment=Production' "$develop_dir"
expect deny 'gh workflow run manual-build-feral-player.yaml --ref release -f version=1.2.3' "$develop_dir"
expect deny 'gh workflow run "Manual Build Local Player Package" --ref release' "$develop_dir"
expect deny 'gh api repos/feral-file/ffos/actions/workflows/manual-build-feral-player.yaml/dispatches -f ref=release' "$develop_dir"
expect deny 'gh workflow run manual-build-components.yaml' "$release_dir"
expect deny 'gh workflow run manual-push-pacman-repo.yaml --ref release' "$develop_dir"
expect deny 'gh workflow run "Manual Update Pacman Repo DB" --ref release' "$develop_dir"
expect deny 'gh workflow run manual-push-pacman-repo.yaml --ref develop -f environment=Production' "$develop_dir"
expect deny 'gh api repos/feral-file/ffos/actions/workflows/manual-push-pacman-repo.yaml/dispatches -f ref=release' "$develop_dir"
expect deny 'gh workflow run manual-push-pacman-repo.yaml' "$release_dir"
# No --ref: gh dispatches on the current branch.
expect deny 'gh workflow run build-image-to-cf.yml -f version=2.0.4' "$release_dir"
# Multi-line scripts and chained commands are still inspected as a whole.
expect deny $'set -e\ncd /tmp\ngh workflow run build-image-to-cf.yml --ref release' "$develop_dir"

# --- staging branch / Staging environment: escalated to the human ----------
expect ask 'gh workflow run build-image-to-cf.yml --ref staging -f version=2.0.4' "$develop_dir"
expect ask 'gh workflow run pure-build-image-to-cf.yml -r staging' "$develop_dir"
expect ask 'gh workflow run build-image-to-cf.yml --ref develop -f environment=Staging' "$develop_dir"
expect ask 'gh api repos/feral-file/ffos/actions/workflows/build-image-to-cf.yml/dispatches -f ref=staging' "$develop_dir"
expect ask 'gh workflow run build-image-to-cf.yml' "$staging_dir"
expect ask 'gh workflow run manual-push-pacman-repo.yaml --ref staging' "$develop_dir"
expect ask 'gh workflow run manual-build-components.yaml --ref staging -f component=feral-watchdog' "$develop_dir"
expect ask 'gh workflow run manual-build-feral-player.yaml -f environment=Staging' "$develop_dir"
expect ask 'gh workflow run manual-push-pacman-repo.yaml -f environment=Staging' "$develop_dir"
# A rerun cannot be attributed to a branch, so it is always escalated.
expect ask 'gh run rerun 1234567890' "$develop_dir"
expect ask 'gh run rerun 1234567890 --failed' "$develop_dir"

# --- everything else: the hook stays silent --------------------------------
expect allow 'gh workflow run build-image-to-cf.yml --ref develop -f version=0.0.1' "$develop_dir"
expect allow 'gh workflow run build-image-to-cf.yml --ref develop -f environment=Development' "$develop_dir"
expect allow 'gh workflow run build-image-to-cf.yml --ref feat/some-branch' "$release_dir"
expect allow 'gh workflow run build-image-to-cf.yml' "$develop_dir"
# `ffos_user_ref=release` is an input, not the dispatch ref.
expect allow 'gh workflow run build-image-to-cf.yml --ref develop -f ffos_user_ref=release' "$develop_dir"
expect allow 'gh workflow run manual-push-pacman-repo.yaml --ref develop -f environment=Development' "$develop_dir"
expect allow 'gh workflow run manual-build-components.yaml --ref develop -f component=feral-controld' "$develop_dir"
expect allow 'gh workflow run manual-build-feral-player.yaml --ref feat/player-bump' "$develop_dir"
# Other workflows on release are not guarded.
expect allow 'gh workflow run test-scripts.yaml --ref release' "$develop_dir"
expect allow 'gh workflow run verify.yml --ref release' "$release_dir"
# Read-only gh and ordinary shell are untouched.
expect allow 'gh workflow list' "$release_dir"
expect allow 'gh workflow view build-image-to-cf.yml --ref release' "$develop_dir"
expect allow 'gh run list --workflow build-image-to-cf.yml --branch release' "$develop_dir"
expect allow 'git log --oneline release..staging' "$release_dir"
expect allow 'ls -la' "$release_dir"
expect allow 'cat .github/workflows/build-image-to-cf.yml' "$release_dir"

# --- payload robustness -----------------------------------------------------
# Missing command field: nothing to judge, stay silent.
out="$(printf '{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}' | "$guard")"
[[ -z "$out" ]] || fail "expected silence for a payload without a command, got: $out"
# Malformed payload: the raw text is still scanned, so a plain release
# dispatch cannot slip through on a parser failure.
out="$(printf 'not json: gh workflow run build-image-to-cf.yml --ref release' | "$guard")"
printf '%s' "$out" | grep -Fq '"permissionDecision":"deny"' || \
  fail "malformed payload with a release dispatch was not denied, got: ${out:-<nothing>}"

# --- per-tool output dialects ----------------------------------------------
# The same decision must come out in each hook contract's JSON shape. A wrong
# shape is silently ignored by the tool, i.e. fails open.
run_fmt() { # $1 = format, $2 = command, $3 = cwd
  payload "$2" "$3" | "$guard" --format "$1"
}
assert_json() { # $1 = out, $2 = description
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "$1" | jq -e . >/dev/null || fail "$2: not valid JSON: $1"
  fi
}
release_cmd='gh workflow run build-image-to-cf.yml --ref release'
staging_cmd='gh workflow run build-image-to-cf.yml --ref staging'
dev_cmd='gh workflow run build-image-to-cf.yml --ref develop'

# Codex: same shape as Claude, but "ask" is unsupported there, so staging is
# denied and the reason tells the agent to hand the dispatch to the human.
out="$(run_fmt codex "$release_cmd" "$develop_dir")"
printf '%s' "$out" | grep -Fq '"permissionDecision":"deny"' || fail "codex release: expected deny, got: $out"
assert_json "$out" "codex release"
out="$(run_fmt codex "$staging_cmd" "$develop_dir")"
printf '%s' "$out" | grep -Fq '"permissionDecision":"deny"' || fail "codex staging: expected deny (no ask support), got: $out"
printf '%s' "$out" | grep -Fq 'let the user dispatch it themselves' || fail "codex staging: deny reason must hand the dispatch to the human, got: $out"
[[ -z "$(run_fmt codex "$dev_cmd" "$develop_dir")" ]] || fail "codex develop: expected silence"

# Cursor: beforeShellExecution carries the command at the top level and
# answers with permission/user_message/agent_message.
cursor_payload() { # $1 = command, $2 = cwd
  local escaped
  escaped="$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
  printf '{"hook_event_name":"beforeShellExecution","command":"%s","cwd":"%s","sandbox":false}' "$escaped" "$2"
}
out="$(cursor_payload "$release_cmd" "$develop_dir" | "$guard" --format cursor)"
printf '%s' "$out" | grep -Fq '"permission":"deny"' || fail "cursor release: expected deny, got: $out"
printf '%s' "$out" | grep -Fq '"user_message":"' || fail "cursor release: missing user_message, got: $out"
printf '%s' "$out" | grep -Fq '"agent_message":"' || fail "cursor release: missing agent_message, got: $out"
assert_json "$out" "cursor release"
out="$(cursor_payload "$staging_cmd" "$develop_dir" | "$guard" --format cursor)"
printf '%s' "$out" | grep -Fq '"permission":"ask"' || fail "cursor staging: expected ask, got: $out"
[[ -z "$(cursor_payload "$dev_cmd" "$develop_dir" | "$guard" --format cursor)" ]] || fail "cursor develop: expected silence"
# No --ref in a Cursor payload: the cwd field is the branch source.
out="$(cursor_payload 'gh workflow run build-image-to-cf.yml' "$release_dir" | "$guard" --format cursor)"
printf '%s' "$out" | grep -Fq '"permission":"deny"' || fail "cursor current-branch release: expected deny, got: $out"

# Gemini and OpenCode: {"decision":"deny","reason":...}; no ask primitive.
for fmt in gemini opencode; do
  out="$(run_fmt "$fmt" "$release_cmd" "$develop_dir")"
  printf '%s' "$out" | grep -Fq '"decision":"deny"' || fail "$fmt release: expected deny, got: $out"
  assert_json "$out" "$fmt release"
  out="$(run_fmt "$fmt" "$staging_cmd" "$develop_dir")"
  printf '%s' "$out" | grep -Fq '"decision":"deny"' || fail "$fmt staging: expected deny (no ask support), got: $out"
  printf '%s' "$out" | grep -Fq 'let the user dispatch it themselves' || fail "$fmt staging: deny reason must hand the dispatch to the human, got: $out"
  [[ -z "$(run_fmt "$fmt" "$dev_cmd" "$develop_dir")" ]] || fail "$fmt develop: expected silence"
done

# An unknown format is a wiring bug and must fail loudly (exit 2 = block in
# every tool that honours exit codes), never fall through to allow.
set +e
out="$(payload "$dev_cmd" "$develop_dir" | "$guard" --format nonsense 2>/dev/null)"
status=$?
set -e
[[ $status -eq 2 ]] || fail "unknown --format: expected exit 2, got $status"

# --- wiring: every tool's hook config must still point at the guard --------
# A dropped hook entry fails open with no error anywhere, so the wiring is
# pinned here next to the decisions it enables.
grep -Fq 'scripts/agent-iso-build-guard.sh' "$repo_root/.claude/settings.json" || fail ".claude/settings.json no longer wires the guard"
grep -Fq 'agent-iso-build-guard.sh\" --format codex' "$repo_root/.codex/hooks.json" || fail ".codex/hooks.json no longer wires the guard with --format codex"
grep -Fq 'scripts/agent-iso-build-guard.sh --format cursor' "$repo_root/.cursor/hooks.json" || fail ".cursor/hooks.json no longer wires the guard with --format cursor"
grep -Fq 'agent-iso-build-guard.sh\" --format gemini' "$repo_root/.gemini/settings.json" || fail ".gemini/settings.json no longer wires the guard with --format gemini"
grep -Fq '"--format", "opencode"' "$repo_root/.opencode/plugins/iso-build-guard.js" || fail ".opencode/plugins/iso-build-guard.js no longer runs the guard with --format opencode"
grep -Fq '"tool.execute.before"' "$repo_root/.opencode/plugins/iso-build-guard.js" || fail "OpenCode plugin no longer hooks tool.execute.before"
if command -v jq >/dev/null 2>&1; then
  for cfg in .claude/settings.json .codex/hooks.json .cursor/hooks.json .gemini/settings.json; do
    jq -e . "$repo_root/$cfg" >/dev/null || fail "$cfg is not valid JSON"
  done
  jq -e '.hooks.PreToolUse[] | select(.matcher == "Bash")' "$repo_root/.claude/settings.json" >/dev/null || fail ".claude/settings.json: guard is not a PreToolUse hook matching Bash"
  jq -e '.hooks.PreToolUse[] | select(.matcher == "Bash")' "$repo_root/.codex/hooks.json" >/dev/null || fail ".codex/hooks.json: guard is not a PreToolUse hook matching Bash"
  jq -e '.hooks.beforeShellExecution | length > 0' "$repo_root/.cursor/hooks.json" >/dev/null || fail ".cursor/hooks.json: guard is not a beforeShellExecution hook"
  jq -e '.hooks.BeforeTool[] | select(.matcher == "run_shell_command")' "$repo_root/.gemini/settings.json" >/dev/null || fail ".gemini/settings.json: guard is not a BeforeTool hook matching run_shell_command"
fi
if command -v node >/dev/null 2>&1; then
  node --check "$repo_root/.opencode/plugins/iso-build-guard.js" || fail "OpenCode plugin does not parse"
fi

echo "test-agent-iso-build-guard: OK"
