#!/usr/bin/env bash
# Pre-shell hook that enforces AGENTS.md "Release guardrail: ISO image
# builds" for every coding-agent tool that can run shell commands here:
#
#   Claude Code  .claude/settings.json   PreToolUse(Bash)        --format claude
#   Codex CLI    .codex/hooks.json       PreToolUse(Bash)        --format codex
#   Cursor       .cursor/hooks.json      beforeShellExecution    --format cursor
#   Gemini CLI   .gemini/settings.json   BeforeTool(run_shell_command) --format gemini
#   OpenCode     .opencode/plugins/iso-build-guard.js  tool.execute.before(bash) --format opencode
#
# Reads the hook payload on stdin and decides whether the shell command about
# to run would dispatch one of the ffos publishing workflows, and against
# which target. Every workflow_dispatch workflow in ffos except verify.yml
# publishes to R2 under the dispatch branch's prefix: the three image builds
# upload the ISO, manual-build-components and manual-build-feral-player upload
# packages to {branch}/os/x86_64/, and manual-push-pacman-repo regenerates,
# re-signs, and prunes that package repo. That prefix is where fielded devices
# update from. scripts/verify.sh in ffos pins that every dispatchable workflow
# is listed in guarded_workflow_re below.
#
#   release branch  or environment=Production -> deny  (that IS a production
#                                                        deployment: the ISO
#                                                        lands under release/
#                                                        on R2, where fielded
#                                                        devices update from)
#   staging branch  or environment=Staging    -> ask   (a human must confirm
#                                                        in the session; tools
#                                                        without an "ask"
#                                                        primitive get deny
#                                                        plus an instruction
#                                                        to hand the dispatch
#                                                        to the human)
#   unclassifiable                            -> deny  (any rerun; a numeric
#                                                        workflow ID; a ref,
#                                                        input, or workflow
#                                                        name behind a shell
#                                                        variable, substitution,
#                                                        --input/--json/@file;
#                                                        a REST dispatch with
#                                                        no literal ref)
#   anything else                             -> allow (no output at all)
#
# The decision is emitted on stdout in the calling tool's JSON dialect with
# exit 0. A non-zero exit would surface as a hook *error* in most tools, not a
# block, so every decision path exits 0 deliberately. The only non-zero exit
# is an unknown --format, which is a wiring bug that must fail loudly.
#
# This file is a LOCKSTEP copy: the same script lives in the `ffos` and
# `ffos-user` repositories (agents in ffos-user dispatch the ffos workflows
# cross-repo with `gh workflow run -R feral-file/ffos`). Change both together.
# Pinned by scripts/test-agent-iso-build-guard.sh.
#
# Fail-closed posture: false positives (e.g. a quoted example command inside
# an echo) are acceptable; the user can rerun by hand. A false negative would
# be a silent production deploy, which is the one outcome this exists to make
# impossible from an agent session.
set -euo pipefail

format="claude"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --format) format="${2:-}"; shift 2 ;;
    --format=*) format="${1#--format=}"; shift ;;
    *) shift ;;
  esac
done
case "$format" in
  claude|codex|cursor|gemini|opencode) ;;
  *)
    printf 'agent-iso-build-guard: unknown --format "%s" (expected claude|codex|cursor|gemini|opencode)\n' "$format" >&2
    exit 2
    ;;
esac

payload="$(cat)"

# Pull a string field out of the hook payload. jq is preferred, python3 is
# the fallback; with neither available the matching below runs on the raw
# JSON text, which still catches the plain `--ref release` forms.
extract_field() {
  local field="$1"
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "$payload" | jq -r "$field // empty" 2>/dev/null || true
  elif command -v python3 >/dev/null 2>&1; then
    printf '%s' "$payload" | python3 -c '
import json, sys
path = sys.argv[1].lstrip(".").split(".")
node = json.load(sys.stdin)
for key in path:
    node = node.get(key, "") if isinstance(node, dict) else ""
print(node if isinstance(node, str) else "")
' "$field" 2>/dev/null || true
  fi
}

# Claude/Codex/Gemini/OpenCode carry the command under tool_input; Cursor's
# beforeShellExecution carries it at the top level.
cmd="$(extract_field '.tool_input.command')"
[[ -n "$cmd" ]] || cmd="$(extract_field '.command')"
cwd="$(extract_field '.cwd')"
[[ -n "$cmd" ]] || cmd="$payload"
[[ -n "$cwd" ]] || cwd="${CLAUDE_PROJECT_DIR:-$PWD}"

# All matching is done on a lowercased copy: the workflow display name is
# "Build FFOS Image", the `environment` choices are capitalised, and gh
# accepts either. Branch names we compare against are lowercase anyway.
lc="$(printf '%s' "$cmd" | tr '[:upper:]' '[:lower:]')"

no_prompt_note="This agent tool has no confirmation prompt, so the dispatch is blocked here: show the user the exact command and every input, and let the user dispatch it themselves."

emit() { # $1 = deny|ask, $2 = reason (plain text, no quotes/backslashes)
  local decision="$1" reason="$2"
  case "$format" in
    claude)
      printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"%s","permissionDecisionReason":"%s"}}\n' \
        "$decision" "$reason"
      ;;
    codex)
      # Codex parses permissionDecision "ask" but does not support it (the
      # hook run is marked failed), so staging is denied and handed over.
      if [[ "$decision" == "ask" ]]; then decision="deny"; reason="$reason $no_prompt_note"; fi
      printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"%s","permissionDecisionReason":"%s"}}\n' \
        "$decision" "$reason"
      ;;
    cursor)
      printf '{"permission":"%s","user_message":"%s","agent_message":"%s"}\n' \
        "$decision" "$reason" "$reason"
      ;;
    gemini|opencode)
      # Neither has an "ask" decision: deny and hand the dispatch to the human.
      if [[ "$decision" == "ask" ]]; then reason="$reason $no_prompt_note"; fi
      printf '{"decision":"deny","reason":"%s"}\n' "$reason"
      ;;
  esac
  exit 0
}

# --- Is this a guarded dispatch at all? -------------------------------------
# Every publishing workflow_dispatch workflow in ffos, by file name or display
# name. Keep in sync with .github/workflows/ in ffos (verify.sh fails when a
# dispatchable workflow is missing here).
guarded_workflow_re='build-image-to-cf|pure-build-image-to-cf|build-image-from-tags|build ffos image|manual-build-components|manual build component|manual-build-feral-player|manual build local player package|manual-push-pacman-repo|manual update pacman repo db'
# `gh workflow run ...` (CLI) or a REST dispatch (`gh api`/curl against
# .../actions/workflows/<wf>/dispatches).
dispatch_re='workflow[[:space:]]+run|/actions/workflows/[^[:space:]]+/dispatches'
# Reruns: the CLI (`gh run rerun <id>`), and the REST endpoints behind it
# (.../actions/runs/<id>/rerun, .../rerun-failed-jobs, .../actions/jobs/<id>/rerun).
rerun_re='gh[[:space:]]+run[[:space:]]+(rerun|retry)|/actions/(runs|jobs)/[^[:space:]]+/rerun'

unclassifiable() { # $1 = what could not be classified
  emit deny "BLOCKED: $1, so this cannot be checked against the release guardrail and is refused (the guard fails closed). Rewrite the command with the workflow file name and literal --ref / -f values, or hand it to the human release operator (AGENTS.md: Release guardrail: ISO image builds)."
}

# A rerun restarts whatever the original run was, release publishing runs
# included, and the target cannot be established from the command line. The
# contract forbids agent reruns of publishing runs regardless of confirmation,
# so this is a deny, not an ask: the human reruns from the Actions UI.
if printf '%s' "$lc" | grep -Eq "$rerun_re"; then
  emit deny "BLOCKED: agents may not rerun or retry GitHub Actions runs here. A rerun restarts the original run, which may be a release or staging publishing run, and the target cannot be established from the command. Hand the rerun to the human release operator (AGENTS.md: Release guardrail: ISO image builds)."
fi

if ! printf '%s' "$lc" | grep -Eq "$dispatch_re"; then
  exit 0
fi

# --- Which workflow? --------------------------------------------------------
# A dispatch whose workflow cannot be named from the command (numeric workflow
# ID, or a name coming from a shell variable / command substitution) cannot be
# matched against the guarded list, so it is refused outright. AGENTS.md
# forbids dispatching by ID for exactly this reason.
wf_rest="$(printf '%s\n' "$lc" | sed -nE 's#.*/actions/workflows/([^/[:space:]]+)/dispatches.*#\1#p' | head -n 1)"
if [[ "$wf_rest" =~ ^[0-9]+$ ]]; then
  unclassifiable "the workflow is given by numeric ID"
fi
if printf '%s' "$wf_rest" | grep -q '[$`]'; then
  unclassifiable "the workflow name comes from a shell variable or substitution"
fi
if printf '%s' "$lc" | grep -Eq 'workflow[[:space:]]+run'; then
  # A bare token of 5+ digits anywhere after `workflow run` is a workflow ID
  # (gh accepts the ID positionally, before or after the flags). Version
  # inputs are `-f version=...` and never a bare token, so they do not trip this.
  if printf '%s' "$lc" | sed -nE 's/.*workflow[[:space:]]+run(.*)/\1/p' | grep -Eq '(^|[[:space:]])[0-9]{5,}([[:space:]]|$)'; then
    unclassifiable "the workflow is given by numeric ID"
  fi
  if ! printf '%s' "$lc" | grep -Eq "$guarded_workflow_re" && printf '%s' "$lc" | grep -q '[$`]'; then
    unclassifiable "the workflow name may come from a shell variable or substitution"
  fi
fi

if ! printf '%s' "$lc" | grep -Eq "$guarded_workflow_re"; then
  exit 0
fi

# --- Opaque inputs on a guarded dispatch ------------------------------------
# The ref and inputs must be literal on the command line. A payload file
# (`--input file`, `--input -`), gh's JSON-on-stdin mode (`--json`), an
# `@file` field value, or any shell variable / command substitution can carry
# ref=release or environment=Production where the guard cannot see it.
if printf '%s' "$lc" | grep -Eq '(^|[[:space:]])--input([[:space:]=]|$)|(^|[[:space:]])--json([[:space:]]|$)|=@'; then
  unclassifiable "the dispatch ref or inputs come from a file or stdin"
fi
if printf '%s' "$cmd" | grep -q '[$`]'; then
  unclassifiable "the dispatch ref or inputs involve a shell variable or command substitution"
fi

# --- Which ref / environment is targeted? ----------------------------------
q="[\"']?"
ref=""
# `--ref release`, `--ref=release`, `-r release`. Extracted from the ORIGINAL
# case, not the lowercased copy: gh's `-R owner/repo` would otherwise read as
# `-r owner/repo` and, appearing after `--ref`, win the greedy match.
ref="$(printf '%s\n' "$cmd" | sed -nE "s/.*(--ref[= ]|(^|[[:space:]])-r[[:space:]]+)${q}([A-Za-z0-9._\\/-]+).*/\\3/p" | head -n 1)"
if [[ -z "$ref" ]]; then
  # REST form: `-f ref=release`, `-F 'ref=release'`, `"ref": "release"`.
  # Anchored so `ffos_user_ref=...` is not mistaken for the dispatch ref.
  ref="$(printf '%s\n' "$lc" | sed -nE "s/.*(^|[[:space:]\"'{,])ref${q}[[:space:]]*[=:][[:space:]]*${q}([a-z0-9._\\/-]+).*/\\2/p" | head -n 1)"
fi
ref="$(printf '%s' "$ref" | tr '[:upper:]' '[:lower:]')"
if [[ -z "$ref" ]]; then
  # A ref flag or key is present but did not parse to a plain literal.
  if printf '%s' "$cmd" | grep -Eq -- '--ref|(^|[[:space:]])-r[[:space:]]' || \
     printf '%s' "$lc" | grep -Eq "(^|[[:space:]\"'{,])ref${q}[[:space:]]*[=:]"; then
    unclassifiable "the dispatch ref is not a plain literal"
  fi
  if printf '%s' "$lc" | grep -Eq 'workflow[[:space:]]+run'; then
    # `gh workflow run` without --ref dispatches on the CURRENT branch.
    # symbolic-ref (not rev-parse) so an unborn branch still resolves.
    ref="$(git -C "$cwd" symbolic-ref --short -q HEAD 2>/dev/null || true)"
  else
    # A REST dispatch requires a ref; if it is not on the command line it is
    # coming from somewhere the guard cannot see.
    unclassifiable "the REST dispatch does not carry a literal ref"
  fi
fi

environment="$(printf '%s\n' "$lc" | sed -nE "s/.*environment${q}[[:space:]]*[=:][[:space:]]*${q}(production|staging|development).*/\\1/p" | head -n 1)"
if [[ -z "$environment" ]] && printf '%s' "$lc" | grep -Eq "environment${q}[[:space:]]*[=:]"; then
  unclassifiable "the environment input is not one of the literal choices"
fi

# --- Decide -----------------------------------------------------------------
if [[ "$ref" == "release" || "$environment" == "production" ]]; then
  emit deny "BLOCKED: this dispatches an ffos publishing workflow (ISO build, package build, or pacman repo push) on the release branch or with environment=Production, which deploys straight to fielded devices. Agents must never trigger this; prepare the exact dispatch parameters and hand the dispatch to the human release operator (AGENTS.md: Release guardrail: ISO image builds)."
fi
if [[ "$ref" == "staging" || "$environment" == "staging" ]]; then
  emit ask "This dispatches an ffos publishing workflow (ISO build, package build, or pacman repo push) on STAGING. Requires explicit user confirmation of these exact parameters before it runs (AGENTS.md: Release guardrail: ISO image builds)."
fi

exit 0
