#!/usr/bin/env bash
# Pins scripts/agent-branch-flow-guard.sh (AGENTS.md "Branch flow
# guardrail"): agents branch from develop and merge back into develop only.
# Each case feeds a hook payload and asserts deny or silence. Also pins that
# the ISO guard still chains into this one, since that chain is the only
# wiring it has. Run by the repository verify target.
# shellcheck disable=SC2016
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard="$repo_root/scripts/agent-branch-flow-guard.sh"
iso_guard="$repo_root/scripts/agent-iso-build-guard.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() { echo "test-agent-branch-flow-guard: $*" >&2; exit 1; }
[[ -x "$guard" ]] || fail "$guard is not executable"

checkout_on() { local dir="$tmp_dir/repo-$1"; git init -q "$dir"; git -C "$dir" symbolic-ref HEAD "refs/heads/$1"; printf '%s' "$dir"; }
develop_dir="$(checkout_on develop)"
feature_dir="$(checkout_on feat-x)"
staging_dir="$(checkout_on staging)"
release_dir="$(checkout_on release)"
main_dir="$(checkout_on main)"

# Fake gh for `gh pr merge` base lookups: PR 100 -> develop, 200 -> staging,
# 300 -> release, anything else -> lookup failure.
fake_bin="$tmp_dir/bin"; mkdir -p "$fake_bin"
cat > "$fake_bin/gh" <<'EOF'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *"pr view 100 "*) echo develop ;;
  *"pr view 200 "*) echo staging ;;
  *"pr view 300 "*) echo release ;;
  *"pr view https://github.com/feral-file/ffos/pull/100 "*) echo develop ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$fake_bin/gh"
export PATH="$fake_bin:$PATH"

json_escape() { local s="$1"; s="${s//\\/\\\\}"; s="${s//\"/\\\"}"; s="${s//$'\n'/\\n}"; printf '%s' "$s"; }
payload() { printf '{"hook_event_name":"PreToolUse","tool_name":"Bash","cwd":"%s","tool_input":{"command":"%s"}}' "$2" "$(json_escape "$1")"; }

expect() { # $1 = deny|allow, $2 = command, $3 = cwd
  local out status
  set +e; out="$(payload "$2" "$3" | "$guard")"; status=$?; set -e
  [[ $status -eq 0 ]] || fail "guard exited $status for: $2"
  case "$1" in
    allow) [[ -z "$out" ]] || fail "expected silence for: $2
got: $out" ;;
    deny)  printf '%s' "$out" | grep -Fq '"permissionDecision":"deny"' || fail "expected deny for: $2
got: ${out:-<nothing>}"
           if command -v jq >/dev/null 2>&1; then printf '%s' "$out" | jq -e . >/dev/null || fail "not JSON for: $2"; fi ;;
  esac
}

# --- PR base must be develop --------------------------------------------------
expect deny  'gh pr create --base staging --title x --body y' "$feature_dir"
expect deny  'gh pr create --base=release --fill' "$feature_dir"
expect deny  'gh pr create -B staging --fill' "$feature_dir"
expect deny  'gh pr create -B=release --fill' "$feature_dir"
expect deny  'gh pr create -Bmain --fill' "$feature_dir"
expect deny  'gh -R feral-file/ffos pr create --base main --fill' "$feature_dir"
expect deny  'gh pr create --base "staging" --fill' "$feature_dir"
expect deny  'gh pr create --base $TARGET --fill' "$feature_dir"
expect deny  'gh pr edit 123 --base staging' "$feature_dir"
expect allow 'gh pr create --base develop --title x --body y' "$feature_dir"
expect allow 'gh pr create -B develop --fill' "$feature_dir"
expect allow 'gh pr create --base=develop --fill' "$feature_dir"
expect allow 'gh pr create --fill' "$feature_dir"
expect allow 'gh pr edit 123 --title new' "$feature_dir"

# --- gh pr merge: only PRs into develop -----------------------------------
expect allow 'gh pr merge 100 --squash' "$feature_dir"
expect allow 'gh pr merge https://github.com/feral-file/ffos/pull/100 --merge' "$feature_dir"
expect deny  'gh pr merge 200 --merge' "$feature_dir"
expect deny  'gh pr merge 300' "$feature_dir"
expect deny  'gh pr merge 999' "$feature_dir"            # base unresolvable
expect deny  'gh -R feral-file/ffos pr merge 200' "$feature_dir"
expect deny  'gh pr merge $PR' "$feature_dir"

# --- git push to protected branches ------------------------------------------
expect deny  'git push origin staging' "$feature_dir"
expect deny  'git push origin release' "$feature_dir"
expect deny  'git push origin main' "$feature_dir"
expect deny  'git push origin HEAD:release' "$feature_dir"
expect deny  'git push origin feat-x:staging' "$feature_dir"
expect deny  'git push -f origin release' "$feature_dir"
expect deny  'git push --force-with-lease origin staging' "$feature_dir"
expect deny  'git push origin refs/heads/feat-x:refs/heads/staging' "$feature_dir"
expect deny  'git push origin :release' "$feature_dir"
expect deny  'git push' "$staging_dir"
expect deny  'git push -u origin' "$release_dir"
expect deny  'git push origin' "$main_dir"
expect allow 'git push origin feat-x' "$feature_dir"
expect allow 'git push -u origin feat-x' "$feature_dir"
expect allow 'git push origin develop' "$develop_dir"
expect allow 'git push' "$feature_dir"
expect allow 'git push' "$develop_dir"
expect allow 'git push origin feat/release-notes' "$feature_dir"

# --- history changes on or from protected branches ---------------------------
expect deny  'git merge develop' "$staging_dir"
expect deny  'git merge staging' "$release_dir"
expect deny  'git commit -m x' "$release_dir"
expect deny  'git commit -am x' "$main_dir"
expect deny  'git rebase develop' "$staging_dir"
expect deny  'git cherry-pick abc123' "$release_dir"
expect deny  'git reset --hard origin/develop' "$staging_dir"
expect deny  'git pull origin develop' "$staging_dir"
expect deny  'git merge staging' "$feature_dir"
expect deny  'git merge origin/release' "$develop_dir"
expect deny  'git rebase staging' "$feature_dir"
expect deny  'git rebase origin/main' "$feature_dir"
expect deny  'git checkout staging && git merge develop && git push' "$develop_dir"
expect deny  'git switch release; git merge staging; git push origin release' "$develop_dir"
expect allow 'git commit -m x' "$feature_dir"
expect allow 'git commit -m x' "$develop_dir"
expect allow 'git merge develop' "$feature_dir"
expect allow 'git rebase develop' "$feature_dir"
expect allow 'git rebase origin/develop' "$feature_dir"
expect allow 'git pull --ff-only origin develop' "$develop_dir"
expect allow 'git merge feat-x' "$develop_dir"

# --- cutting branches -------------------------------------------------------
expect deny  'git checkout -b hotfix staging' "$develop_dir"
expect deny  'git checkout -b hotfix origin/release' "$develop_dir"
expect deny  'git switch -c hotfix main' "$develop_dir"
expect deny  'git branch hotfix release' "$develop_dir"
expect deny  'git checkout -b hotfix' "$staging_dir"
expect deny  'git switch -c hotfix' "$release_dir"
expect deny  'git checkout staging && git checkout -b hotfix' "$develop_dir"
expect allow 'git checkout -b feat-y' "$develop_dir"
expect allow 'git checkout -b feat-y develop' "$feature_dir"
expect allow 'git checkout -b feat-y origin/develop' "$staging_dir"
expect allow 'git switch -c feat-y' "$develop_dir"
expect allow 'git branch feat-y' "$develop_dir"

# --- REST writes --------------------------------------------------------------
expect deny  'gh api -X POST repos/feral-file/ffos/merges -f base=staging -f head=develop' "$develop_dir"
expect deny  'gh api -X PATCH repos/feral-file/ffos/git/refs/heads/release -f sha=abc' "$develop_dir"
expect deny  'gh api -X PUT repos/feral-file/ffos/pulls/200/merge' "$develop_dir"
expect deny  'gh api -X POST repos/feral-file/ffos/pulls -f base=staging -f head=feat-x -f title=x' "$develop_dir"
expect deny  'curl -X PUT https://api.github.com/repos/feral-file/ffos/pulls/200/merge' "$develop_dir"
expect allow 'gh api -X POST repos/feral-file/ffos/pulls -f base=develop -f head=feat-x -f title=x' "$develop_dir"
expect allow 'gh api repos/feral-file/ffos/pulls/200' "$develop_dir"

# --- read-only use of protected branches is fine ---------------------------
expect allow 'git checkout staging' "$develop_dir"
expect allow 'git switch release' "$develop_dir"
expect allow 'git log --oneline release..staging' "$develop_dir"
expect allow 'git diff staging...develop' "$develop_dir"
expect allow 'git fetch origin staging' "$develop_dir"
expect allow 'git branch -a' "$staging_dir"
expect allow 'git status' "$release_dir"
expect allow 'gh pr list --base staging' "$develop_dir"
expect allow 'gh pr view 200' "$develop_dir"
expect allow 'gh pr checks 200' "$develop_dir"
expect allow 'git show origin/release:README.md' "$develop_dir"
expect allow 'ls -la' "$release_dir"
expect allow 'make verify' "$staging_dir"
expect allow 'echo staging release main' "$develop_dir"

# --- chained from the ISO guard --------------------------------------------
grep -Fq 'agent-branch-flow-guard.sh' "$iso_guard" || fail "ISO guard no longer chains into the branch flow guard"
out="$(payload 'git push origin release' "$feature_dir" | "$iso_guard")"
printf '%s' "$out" | grep -Fq '"permissionDecision":"deny"' || fail "ISO guard entry point did not chain: ${out:-<nothing>}"
out="$(payload 'git push origin feat-x' "$feature_dir" | "$iso_guard")"
[[ -z "$out" ]] || fail "ISO guard chain not silent on allow: $out"
# Every dialect renders a deny.
for fmt in codex cursor gemini opencode; do
  out="$(payload 'git push origin release' "$feature_dir" | "$guard" --format "$fmt")"
  printf '%s' "$out" | grep -Eq '"(permissionDecision|permission|decision)":"deny"' || fail "$fmt dialect did not deny: $out"
done
for doc in AGENTS.md CLAUDE.md .cursor/rules/branch-flow-policy.mdc; do
  grep -q 'Branch flow guardrail' "$repo_root/$doc" || fail "$doc lost the Branch flow guardrail section"
done

echo "test-agent-branch-flow-guard: OK"
