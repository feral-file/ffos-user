#!/usr/bin/env bash
# Pre-shell hook enforcing AGENTS.md "Branch flow guardrail":
#
#   develop -> staging -> release, humans do both promotions.
#   Agents branch from develop and merge back into develop, nothing else.
#
# Chained from scripts/agent-iso-build-guard.sh (same hook entry in every
# tool's config, so Codex hook trust is unchanged) and reads the same payload
# on stdin. It is best-effort: the written rule is the constraint, and
# GitHub branch protection on staging/release is the backstop. It refuses
# the common ways an agent would break the flow:
#
#   - a PR whose base is not develop (`gh pr create/edit --base staging`)
#   - merging a PR whose base is not develop (`gh pr merge`; the base is
#     looked up with `gh pr view`, and an unresolvable base is refused)
#   - pushing to staging, release, or main (any refspec form, or a bare
#     `git push` while on one of them)
#   - committing, merging, rebasing, cherry-picking, or resetting while on
#     staging, release, or main; merging or rebasing those branches into
#     anything; `git checkout staging && git merge develop` in one command
#   - cutting a branch from staging, release, or main
#   - REST writes to those refs, to /merges, or /pulls/N/merge
#
# Read-only use of the protected branches (checkout to inspect, log, diff,
# fetch) is untouched. Everything not listed is silent (allow).
#
# LOCKSTEP copy: identical file in ffos and ffos-user. Pinned by
# scripts/test-agent-branch-flow-guard.sh.
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
  *) printf 'agent-branch-flow-guard: unknown --format "%s"\n' "$format" >&2; exit 2 ;;
esac

payload="$(cat)"
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
cmd="$(extract_field '.tool_input.command')"
[[ -n "$cmd" ]] || cmd="$(extract_field '.command')"
cwd="$(extract_field '.cwd')"
[[ -n "$cmd" ]] || cmd="$payload"
[[ -n "$cwd" ]] || cwd="${CLAUDE_PROJECT_DIR:-$PWD}"

# Same normalisation as the ISO guard: shell-literal words, quotes gone.
norm="${cmd//\\$'\n'/}"
norm="$(printf '%s' "$norm" | sed -e 's/\\\(.\)/\1/g' | tr -d "\"'")"
lc="$(printf '%s' "$norm" | tr '[:upper:]' '[:lower:]')"

trace() {
  [[ -n "${AGENT_ISO_GUARD_TRACE:-}" ]] || return 0
  printf '%s %s\n' "$format" "$1" >> "$AGENT_ISO_GUARD_TRACE" 2>/dev/null || true
}
allow() { trace allow; exit 0; }
deny() { # $1 = reason (plain text, no quotes/backslashes)
  local reason="$1 Agents branch from develop and merge back into develop only; staging and release are promoted by a human (AGENTS.md: Branch flow guardrail)."
  trace deny
  case "$format" in
    claude|codex)
      printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"%s"}}\n' "$reason" ;;
    cursor)
      printf '{"permission":"deny","user_message":"%s","agent_message":"%s"}\n' "$reason" "$reason" ;;
    gemini|opencode)
      printf '{"decision":"deny","reason":"%s"}\n' "$reason" ;;
  esac
  exit 0
}

# Fast exit: nothing here touches git, gh, or the GitHub API.
printf '%s' "$lc" | grep -Eq '(^|[[:space:]])(git|gh)[[:space:]]|api\.github\.com' || allow

protected='staging|release|main'
# A protected branch name as a whole word or path tail: `staging`,
# `origin/staging`, `refs/heads/release`, `HEAD:release`, `release:release`.
prot_ref="(^|[[:space:]/:])($protected)([[:space:]:]|$)"

current_branch="$(git -C "$cwd" symbolic-ref --short -q HEAD 2>/dev/null || true)"
current_branch="$(printf '%s' "$current_branch" | tr '[:upper:]' '[:lower:]')"

# Repo flag for gh lookups, from the original-case command (-R vs -r).
repo_flag=()
repo="$(printf '%s\n' "$norm" | sed -nE 's/.*(^|[[:space:]])(-R|--repo)[= ]([A-Za-z0-9._\/-]+).*/\3/p' | head -n 1)"
[[ -n "$repo" ]] && repo_flag=(-R "$repo")

pr_base() { # $1 = pr number/url/branch -> prints base branch, or nothing
  local out=""
  out="$( ( gh pr view "$1" "${repo_flag[@]}" --json baseRefName -q .baseRefName 2>/dev/null & pid=$!
           ( sleep 10; kill "$pid" 2>/dev/null ) & k=$!
           wait "$pid" 2>/dev/null; kill "$k" 2>/dev/null; wait "$k" 2>/dev/null ) || true )"
  printf '%s' "$out" | tr '[:upper:]' '[:lower:]'
}

# Segment the compound command so a `git checkout staging` earlier in the
# line changes which branch the later segments act on.
segs="${lc//&&/$'\n'}"; segs="${segs//||/$'\n'}"; segs="${segs//;/$'\n'}"; segs="${segs//|/$'\n'}"
effective_branch="$current_branch"

while IFS= read -r seg; do
  [[ -n "${seg// /}" ]] || continue
  on_protected=0
  [[ "$effective_branch" =~ ^($protected)$ ]] && on_protected=1

  # --- gh pr create / edit: base must be develop --------------------------
  if printf '%s' "$seg" | grep -Eq 'gh([[:space:]]+-[^[:space:]]+([[:space:]]+[^-[:space:]][^[:space:]]*)?)*[[:space:]]+pr[[:space:]]+(create|edit)'; then
    if printf '%s' "$seg" | grep -Eq -- '(--base|(^|[[:space:]])-b)([=[:space:]]|[a-z])'; then
      base="$(printf '%s\n' "$seg" | sed -nE 's/.*(--base|(^|[[:space:]])-b)[=[:space:]]*([a-z0-9._\/-]+).*/\3/p' | head -n 1)"
      [[ "$base" == "develop" ]] || deny "BLOCKED: this PR targets ${base:-an unparseable base} instead of develop."
    fi
  fi

  # --- gh pr merge: resolve the base, refuse anything but develop ----------
  if printf '%s' "$seg" | grep -Eq 'gh([[:space:]]+-[^[:space:]]+([[:space:]]+[^-[:space:]][^[:space:]]*)?)*[[:space:]]+pr[[:space:]]+merge'; then
    target="$(printf '%s\n' "$seg" | sed -nE 's/.*pr[[:space:]]+merge[[:space:]]+([^-[:space:]][^[:space:]]*).*/\1/p' | head -n 1)"
    if printf '%s' "$seg" | grep -Eq '[$`]'; then
      deny "BLOCKED: gh pr merge with a shell expansion cannot be checked against the branch flow."
    fi
    base=""
    [[ -n "$target" ]] && base="$(pr_base "$target")"
    [[ -z "$target" ]] && base="$(pr_base "")"
    [[ "$base" == "develop" ]] || deny "BLOCKED: gh pr merge ${target:-(current branch PR)} targets ${base:-a base that could not be resolved}; only PRs into develop may be merged by an agent."
  fi

  # --- git push to a protected branch ---------------------------------------
  if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+push'; then
    if printf '%s' "$seg" | sed -E 's/.*[[:space:]]push//' | grep -Eq "$prot_ref"; then
      deny "BLOCKED: git push to staging, release, or main."
    fi
    if [[ $on_protected -eq 1 ]] && ! printf '%s' "$seg" | sed -E 's/.*[[:space:]]push//' | grep -Eq '(^|[[:space:]])[^-[:space:]][^[:space:]]*[[:space:]]+[^-[:space:]]'; then
      deny "BLOCKED: git push while on $effective_branch would push that branch."
    fi
  fi

  # --- history-changing git commands on / from a protected branch -----------
  if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+(merge|rebase|cherry-pick|revert|am|commit|reset|pull)([[:space:]]|$)'; then
    [[ $on_protected -eq 1 ]] && deny "BLOCKED: that rewrites $effective_branch locally; agents do not commit on, merge into, or rebase staging, release, or main."
    if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+(merge|rebase)([[:space:]]|$)' && \
       printf '%s' "$seg" | sed -E 's/.*[[:space:]](merge|rebase)//' | grep -Eq "$prot_ref"; then
      deny "BLOCKED: merging or rebasing staging, release, or main into a working branch."
    fi
  fi

  # --- cutting a branch from a protected branch -----------------------------
  if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+(checkout[[:space:]]+-[bB]|switch[[:space:]]+-[cC]|branch)[[:space:]]'; then
    tail_args="$(printf '%s' "$seg" | sed -E 's/.*[[:space:]](checkout[[:space:]]+-[bB]|switch[[:space:]]+-[cC]|branch)[[:space:]]+//')"
    start="$(printf '%s' "$tail_args" | awk '{print $2}')"
    if printf '%s' "${start:-}" | grep -Eq "$prot_ref"; then
      deny "BLOCKED: new branches are cut from develop, not from $start."
    fi
    if [[ -z "$start" && $on_protected -eq 1 ]] && printf '%s' "$tail_args" | grep -Eq '^[^-]'; then
      deny "BLOCKED: new branches are cut from develop, not from $effective_branch."
    fi
  fi

  # --- checkout / switch to a protected branch changes the later segments --
  if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+(checkout|switch)[[:space:]]+('"$protected"')([[:space:]]|$)'; then
    effective_branch="$(printf '%s\n' "$seg" | sed -nE 's/.*(checkout|switch)[[:space:]]+('"$protected"')([[:space:]]|$).*/\2/p' | head -n 1)"
  fi

  # --- REST writes ----------------------------------------------------------
  if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])gh([[:space:]]+-[^[:space:]]+([[:space:]]+[^-[:space:]][^[:space:]]*)?)*[[:space:]]+api[[:space:]]|api\.github\.com'; then
    if printf '%s' "$seg" | grep -Eq "/git/refs/heads/($protected)([[:space:]/]|$)|/merges([[:space:]]|$)|/pulls/[^/[:space:]]+/merge([[:space:]]|$)|base=($protected)([[:space:]]|$)"; then
      deny "BLOCKED: REST write to a protected branch, a merge endpoint, or a PR base that is not develop; use gh pr with --base develop."
    fi
  fi
done <<< "$segs"

allow
