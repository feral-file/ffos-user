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
# Scope: only the repositories that follow this flow, matched by repository
# NAME (ffos-user, ffos; any owner, so a fork or transfer stays covered).
# Agents work in other repos from the same session — ff-app and play have
# only `main` — and this file's rules would wrongly refuse their normal PRs.
# A segment is skipped only when its target is positively another repository,
# named outside quotes by gh's -R/--repo, git's -C, or a single
# `repos/OWNER/NAME` API path, or else the shell's own working directory when
# the command has no cd/pushd/popd at all. A `cd` is never followed. Anything
# unresolvable or unreadable keeps the full rules: the guard fails closed.
# Details at "can this command be scoped at all?" below.
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
# Repository names this flow applies to; see "Scope" above.
flow_repo_names='ffos-user|ffos'
# A protected branch name as a whole word or path tail: `staging`,
# `origin/staging`, `refs/heads/release`, `HEAD:release`, `release:release`.
prot_ref="(^|[[:space:]/:])($protected)([[:space:]:]|$)"

branch_of() { # $1 = dir -> lowercase current branch, or nothing
  [[ -n "$1" ]] || return 0
  git -C "$1" symbolic-ref --short -q HEAD 2>/dev/null | tr '[:upper:]' '[:lower:]' || true
}

# NAME of a repository from a remote URL or an [HOST/]OWNER/NAME spec,
# lowercased: https://github.com/o/ffos.git, git@github.com:o/ffos.git and
# o/ffos all give `ffos`.
repo_name_of() {
  # Trailing slashes first: `.../ffos-user.git/` must not leave `ffos-user.git`.
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -E 's#/+$##; s#\.git$##; s#.*[/:]##'
}

# Whether a directory is positively OUTSIDE the flow: it has at least one
# remote and no URL of any remote names a flow repository. Every remote
# counts, not just origin (in a fork clone `upstream` is the flow repo, and
# both a bare push to it and gh, which prefers upstream, land there), and
# every URL of each: a remote can fetch from one repository and push to
# another, or carry several URLs.
dir_out_of_flow() { # $1 = dir
  [[ -n "$1" ]] || return 1
  local remotes r url found=0
  remotes="$(git -C "$1" remote 2>/dev/null || true)"
  for r in $remotes; do
    found=1
    while IFS= read -r url; do
      [[ -n "$url" ]] || continue
      [[ "$(repo_name_of "$url")" =~ ^($flow_repo_names)$ ]] && return 1
    done < <(git -C "$1" remote get-url --all "$r" 2>/dev/null; git -C "$1" remote get-url --push --all "$r" 2>/dev/null)
  done
  [[ $found -eq 1 ]]
}

# Resolve a path the way a shell would from $2: ~, absolute, or relative.
# Prints nothing for anything with an expansion the hook cannot evaluate.
resolve_dir() { # $1 = path word, $2 = base dir
  local p="$1"
  [[ "$p" == *'$'* || "$p" == *'`'* || -z "$2" ]] && return 0
  case "$p" in
    \~) p="$HOME" ;;
    \~/*) p="$HOME/${p#\~/}" ;;
    /*) ;;
    *) p="$2/$p" ;;
  esac
  (cd "$p" 2>/dev/null && pwd -P) || true
}

pr_base() { # $1 = pr number/url/branch, $2 = dir, $3 = -R value -> base branch, or nothing
  local out="" args=()
  [[ -n "$3" ]] && args=(-R "$3")
  out="$( ( cd "${2:-.}" 2>/dev/null || true
           gh pr view "$1" "${args[@]}" --json baseRefName -q .baseRefName 2>/dev/null & pid=$!
           ( sleep 10; kill "$pid" 2>/dev/null ) & k=$!
           wait "$pid" 2>/dev/null; kill "$k" 2>/dev/null; wait "$k" 2>/dev/null ) || true )"
  printf '%s' "$out" | tr '[:upper:]' '[:lower:]'
}

# Segment the compound command so a `git checkout staging` earlier in the
# line changes which branch the later segments act on. Split the original-
# case text: paths and -R values are case-sensitive.
segs="${norm//&&/$'\n'}"; segs="${segs//||/$'\n'}"; segs="${segs//;/$'\n'}"; segs="${segs//|/$'\n'}"
# Subshell parentheses are not part of any word: `(git push origin main)`
# must read `main`, not `main)`. `$(` keeps its `$`, so the expansion checks
# below still see it.
segs="${segs//(/ }"; segs="${segs//)/ }"
effective_branch="$(branch_of "$cwd")"

# --- can this command be scoped at all? --------------------------------------
# Scoping only ever RELAXES the guard, and the segments above come from text
# whose quotes are already gone, so the guard cannot tell a real `;` or `cd`
# from one inside a commit message. Two consequences, both fail closed:
#
# 1. A target is only ever taken from something that names it explicitly and
#    sits OUTSIDE quotes in the original command: gh's -R/--repo, a git -C,
#    or a single repos/OWNER/NAME API path. The shell's own working directory
#    (the hook payload's cwd) is the only implicit target, and it is used
#    only when the command contains no cd/pushd/popd at all — quoted or not
#    — because a cd anywhere makes "which directory is this segment in"
#    unanswerable from this text.
# 2. Commands this model cannot read keep every segment in scope, exactly as
#    before scoping existed:
#    - escapes the quote-stripper below does not understand (any backslash,
#      $'...') or a quoted string spanning lines;
#    - a background `&` (not `&&`, not a `>&`/`&>` redirection);
#    - a heredoc (`<<`): its body lines would read as commands;
#    - another shell or evaluator (`sh -c`, `eval`, `xargs`, `env`, ...);
#    - GH_REPO / GIT_DIR / GIT_WORK_TREE / --git-dir / --work-tree, which
#      name a repository by a route this resolver does not read;
#    - a `#` comment.
# unq is the original command with quoted strings removed: what the shell
# itself acts on. unq_lc is its lowercase form for keyword matching.
unq="$(printf '%s' "$cmd" | sed -E "s/'[^']*'//g; s/\"[^\"]*\"//g")"
unq_lc="$(printf '%s' "$unq" | tr '[:upper:]' '[:lower:]')"
scopable=1
case "$cmd" in *\\*|*"\$'"*) scopable=0 ;; esac
# A quote left unpaired on a line means a quoted string spans lines.
printf '%s' "$unq" | grep -q "[\"']" && scopable=0
bg="$(printf '%s' "$unq_lc" | sed -E 's/[0-9]*>&[0-9-]*//g; s/&>+//g; s/&&//g')"
[[ "$bg" == *'&'* ]] && scopable=0
# Any GIT_* / GH_* assignment counts, not a list: GIT_COMMON_DIR,
# GIT_CONFIG_COUNT/KEY/VALUE (an insteadOf rewrite), GH_HOST, ... can each
# point a command at another repository.
printf '%s' "$unq_lc" | grep -Eq '<<|(^|[^a-z_])(eval|xargs|env|exec|source)([^a-z_]|$)|(^|[^a-z_])(sh|bash|zsh|dash|ksh|fish)[[:space:]]+-[a-z]*c|(^|[^a-z_])(git|gh)_[a-z_0-9]+=|--git-dir|--work-tree|(^|[[:space:]])#' && scopable=0
# A command substitution (`$(...)`, backticks, `<(...)`) runs in the shell's
# own directory whatever the outer command targets, so one that runs git or
# gh inside a segment descoped by -R / -C would go unchecked.
# shellcheck disable=SC2016  # literal $( and backtick, not an expansion
printf '%s' "$cmd" | tr '[:upper:]' '[:lower:]' | grep -Eq '(\$\(|`|[<>]\()([^)`]*[^a-z_-])?(git|gh)([^a-z_-]|$)' && scopable=0
cwd_usable=1
# chdir is zsh's alias of cd.
printf '%s' "$cmd" | tr '[:upper:]' '[:lower:]' | grep -Eq '(^|[^a-z_-])(cd|chdir|pushd|popd)([^a-z_-]|$)' && cwd_usable=0

# Whether `needle` (a flag and its value) appears as written outside quotes.
in_unquoted() { # $1 = ERE matching the flag + value
  printf '%s' "$unq" | grep -Eq -- "$1"
}
# Escape a literal for use inside an ERE.
ere_escape() { printf '%s' "$1" | sed -E 's/[][\.*^$+?(){}|/]/\\&/g'; }

while IFS= read -r oseg; do
  [[ -n "${oseg// /}" ]] || continue
  seg="$(printf '%s' "$oseg" | tr '[:upper:]' '[:lower:]')"

  # --- which repository does this segment act on? ---------------------------
  # OUT of scope only when the target was positively resolved, from a source
  # that is outside quotes, to a repository that is not a flow repo:
  #   - gh's -R/--repo, read only when the segment's command IS gh (`git
  #     diff -R`, `grep -R` mean something else) and it appears exactly once,
  #     detached;
  #   - exactly one repos/OWNER/NAME path in a gh api / api.github.com call
  #     (a placeholder like repos/:owner/:repo counts as a second, unreadable
  #     one);
  #   - exactly one git -C <dir>, or (see cwd_usable) the shell's cwd: out
  #     only if the directory has remotes and none names a flow repository.
  # Whatever the source, a segment that names a flow repository in URL form
  # (`git push git@github.com:o/ffos-user.git HEAD:main`) stays in scope.
  seg_repo_spec=""
  out_of_scope=0
  is_gh=0; [[ "$seg" =~ ^[[:space:]]*gh[[:space:]] ]] && is_gh=1
  # Any gh ... api call counts as an API segment, prefixed (`command gh api`)
  # or not: those are never descoped by the cwd, only by an explicit path.
  is_api=0; [[ "$seg" =~ (^|[[:space:]])gh[[:space:]](.*[[:space:]])?api[[:space:]] || "$seg" == *api.github.com* ]] && is_api=1
  repo_flags="$(printf '%s\n' "$oseg" | grep -Eo '(^|[[:space:]])(-R|--repo)([=[:space:]]+[^[:space:]]+|[^[:space:]=]+)' || true)"
  repos_mentions="$(printf '%s\n' "$oseg" | { grep -o 'repos/' || true; } | wc -l | tr -d ' ')"
  repo_paths="$(printf '%s\n' "$oseg" | grep -Eo '(^|[[:space:]/])repos/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+' || true)"
  dash_c="$(printf '%s\n' "$oseg" | grep -Eo '(^|[[:space:]])git[[:space:]]+-C[[:space:]]+[^[:space:]]+' || true)"
  seg_dir=""
  [[ $cwd_usable -eq 1 ]] && seg_dir="$cwd"
  seg_branch="$effective_branch"
  if [[ -n "$dash_c" ]]; then
    seg_dir=""
    c_path=""
    [[ "$dash_c" =~ -C[[:space:]]+([^[:space:]]+)$ ]] && c_path="${BASH_REMATCH[1]}"
    if [[ -n "$c_path" && "$(printf '%s\n' "$dash_c" | wc -l)" -eq 1 ]] &&
       ! [[ "$oseg" =~ git[[:space:]]+-C[[:space:]]+[^[:space:]]+[[:space:]]+-C[[:space:]] ]]; then
      if in_unquoted "(^|[[:space:]])git[[:space:]]+-C[[:space:]]+$(ere_escape "$c_path")([[:space:]]|$)"; then
        seg_dir="$(resolve_dir "$c_path" "$cwd")"
      fi
    fi
    seg_branch="$(branch_of "$seg_dir")"
    # Drop `-C <dir>` so the checks below see `git <subcommand>` (their
    # patterns allow bare flags between git and the subcommand, not a flag
    # with a separate value). Case-sensitive: `-c key=val` stays.
    seg="$(printf '%s' "$oseg" | sed -E 's/(^|[[:space:]])git[[:space:]]+-C[[:space:]]+[^[:space:]]+/\1git/' | tr '[:upper:]' '[:lower:]')"
  fi
  if [[ $is_gh -eq 1 && -n "$repo_flags" ]]; then
    # The value must look like OWNER/NAME: `--title -R --body x` makes gh
    # read `-R` as the title, and `--body` is not a repository.
    if [[ "$(printf '%s\n' "$repo_flags" | wc -l)" -eq 1 && "$repo_flags" =~ (-R|--repo)[=[:space:]]+([A-Za-z0-9._][A-Za-z0-9._-]*/[A-Za-z0-9._/-]+)$ ]]; then
      seg_repo_spec="${BASH_REMATCH[2]}"
      if in_unquoted "(^|[[:space:]])(-R|--repo)[=[:space:]]+$(ere_escape "$seg_repo_spec")([[:space:]]|$)" &&
         ! [[ "$(repo_name_of "$seg_repo_spec")" =~ ^($flow_repo_names)$ ]]; then
        out_of_scope=1
      fi
    fi
  elif [[ $is_api -eq 1 && "$repos_mentions" -gt 0 ]]; then
    if [[ "$repos_mentions" -eq 1 && "$(printf '%s\n' "$repo_paths" | grep -c .)" -eq 1 ]]; then
      api_path="$(printf '%s' "$repo_paths" | sed -E 's#^[[:space:]/]*##')"
      if in_unquoted "$(ere_escape "$api_path")" &&
         ! [[ "$(repo_name_of "${api_path##*/}")" =~ ^($flow_repo_names)$ ]]; then
        out_of_scope=1
      fi
    fi
  elif [[ $is_api -eq 0 ]] && dir_out_of_flow "$seg_dir"; then
    out_of_scope=1
  fi
  # A flow repository named as a path component anywhere — a remote URL, a
  # PR URL (`.../ffos-user/pull/5`, which gh resolves ahead of -R), an API
  # path (`repos/o/ffos-user/merges`, `.../ff-app/../ffos-user/...`) — keeps
  # the segment in scope. `ffos-user#332` in a title has no `/` and passes.
  printf '%s' "$seg" | grep -Eq "[/:]($flow_repo_names)(\\.git)?(/|[[:space:]]|$)" && out_of_scope=0
  if [[ $scopable -eq 1 && $out_of_scope -eq 1 ]]; then
    # Another repository's flow is its own. Its checkouts are not followed
    # either: they move that repository's HEAD.
    continue
  fi

  on_protected=0
  [[ "$seg_branch" =~ ^($protected)$ ]] && on_protected=1

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
    base="$(pr_base "$target" "$seg_dir" "$seg_repo_spec")"
    [[ "$base" == "develop" ]] || deny "BLOCKED: gh pr merge ${target:-(current branch PR)} targets ${base:-a base that could not be resolved}; only PRs into develop may be merged by an agent."
  fi

  # --- git push to a protected branch ---------------------------------------
  if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+push'; then
    if printf '%s' "$seg" | sed -E 's/.*[[:space:]]push//' | grep -Eq "$prot_ref"; then
      deny "BLOCKED: git push to staging, release, or main."
    fi
    if [[ $on_protected -eq 1 ]] && ! printf '%s' "$seg" | sed -E 's/.*[[:space:]]push//' | grep -Eq '(^|[[:space:]])[^-[:space:]][^[:space:]]*[[:space:]]+[^-[:space:]]'; then
      deny "BLOCKED: git push while on $seg_branch would push that branch."
    fi
  fi

  # --- history-changing git commands on / from a protected branch -----------
  if printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+(merge|rebase|cherry-pick|revert|am|commit|reset|pull)([[:space:]]|$)'; then
    [[ $on_protected -eq 1 ]] && deny "BLOCKED: that rewrites $seg_branch locally; agents do not commit on, merge into, or rebase staging, release, or main."
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
      deny "BLOCKED: new branches are cut from develop, not from $seg_branch."
    fi
  fi

  # --- checkout / switch to a protected branch changes the later segments --
  # Only a checkout in the current directory moves effective_branch; one
  # through `git -C elsewhere` moves elsewhere's HEAD, not this one's.
  if [[ -z "$dash_c" ]] && printf '%s' "$seg" | grep -Eq '(^|[[:space:]])git([[:space:]]+-[^[:space:]]+)*[[:space:]]+(checkout|switch)[[:space:]]+('"$protected"')([[:space:]]|$)'; then
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
