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

# --- scope: only ffos-user / ffos follow this flow ---------------------------
# Repos with a remote. ff-app has only `main`, so its PRs target main.
with_remote() { # $1 = name, $2 = branch, $3 = origin url
  local dir="$tmp_dir/$1"; git init -q "$dir"; git -C "$dir" symbolic-ref HEAD "refs/heads/$2"
  git -C "$dir" remote add origin "$3"; printf '%s' "$dir"
}
app_dir="$(with_remote app main https://github.com/feral-file/ff-app.git)"
app_feature_dir="$(with_remote app-feat feat-x git@github.com:feral-file/ff-app.git)"
user_dir="$(with_remote ffos-user develop git@github.com:feral-file/ffos-user.git)"
os_main_dir="$(with_remote ffos main https://github.com/feral-file/FFOS)"
fork_dir="$(with_remote fork feat-x https://github.com/someone/ffos-user.git)"
upstream_only_dir="$tmp_dir/upstream-only"; git init -q "$upstream_only_dir"
git -C "$upstream_only_dir" symbolic-ref HEAD refs/heads/main
git -C "$upstream_only_dir" remote add upstream https://github.com/feral-file/ff-app.git

# Out of scope: another repository, named by -R, the cwd, cd, git -C, or an API path.
expect allow 'gh pr create -R feral-file/ff-app --base main --fill' "$feature_dir"
expect allow 'gh pr create --repo=feral-file/ff-app --base main --fill' "$feature_dir"
expect allow 'gh -R feral-file/ff-app pr merge 200' "$feature_dir"
expect allow 'gh pr create --base main --fill' "$app_feature_dir"
expect allow 'git push origin main' "$app_dir"
expect allow 'git commit -m x' "$app_dir"
expect allow 'git -C '"$app_dir"' push origin main' "$user_dir"
expect allow 'git push' "$upstream_only_dir"                   # single remote resolves
expect allow 'git -C '"$app_dir"' checkout main && git -C '"$app_dir"' merge feat-x' "$user_dir"
# A cd is never followed: which directory a later segment runs in cannot be
# read reliably from quote-stripped text, so the command keeps full rules.
# Cross-repo work uses git -C / gh -R instead.
expect deny  'cd '"$app_feature_dir"' && gh pr create --base main --fill' "$user_dir"
expect deny  'cd '"$app_dir"' && git checkout main && git merge feat-x' "$user_dir"

# In scope: by name from any of those sources, any owner, any URL form.
expect deny  'gh pr create -R feral-file/ffos-user --base main --fill' "$app_dir"
expect deny  'gh pr create -R someone/ffos --base main --fill' "$app_dir"
expect deny  'gh pr create --base main --fill' "$user_dir"
expect deny  'gh pr create --base main --fill' "$fork_dir"
expect deny  'git commit -m x' "$os_main_dir"
expect deny  'cd '"$user_dir"' && git push origin main' "$app_dir"
expect deny  'git -C '"$os_main_dir"' commit -m x' "$app_dir"
expect deny  'gh api -X PUT repos/feral-file/ffos-user/pulls/200/merge' "$app_dir"
expect deny  'cd '"$app_dir"' && cd '"$user_dir"' && git push origin release' "$feature_dir"
expect deny  '(cd '"$user_dir"' && git push origin main)' "$app_dir"
expect deny  'cd ../ffos-user && git push origin main' "$app_dir"   # relative, resolves to $user_dir

# Unresolvable target: in scope (fail closed).
expect deny  'gh pr create -R $REPO --base main --fill' "$app_dir"
expect deny  'cd $SOMEWHERE && git push origin main' "$app_dir"
expect deny  'git push origin main' "$tmp_dir"                    # not a repository

# Lookalike flags do not name a repository.
expect deny  'git diff -R HEAD && git push origin main' "$develop_dir"
expect deny  'grep -R x . ; git push origin release' "$develop_dir"

# Review round 1 probes: forms the scoping must not be fooled by. Each was
# denied before scoping existed and must stay denied.
fork_clone_dir="$(with_remote fork-clone main https://github.com/me/my-fork.git)"
git -C "$fork_clone_dir" remote add upstream https://github.com/feral-file/ffos-user.git
# F1: a cd segment's tail is still checked; a background & is not modelled.
expect deny  'cd . & git push origin main' "$user_dir"
expect deny  'cd '"$app_dir"' & git push origin main' "$user_dir"
# F2: a cd that does not move the shell for the later segments.
expect deny  '(cd '"$app_dir"') && git push origin main' "$user_dir"
expect deny  'cd '"$app_dir"' || git push origin main' "$user_dir"
expect deny  'pushd '"$app_dir"' && popd && git push origin main' "$user_dir"
expect deny  'cat <<EOF > x.sh
cd '"$app_dir"'
EOF
git push origin main' "$user_dir"
# F3: repository routes the resolver does not read.
expect deny  'GH_REPO=feral-file/ffos-user gh pr create --base main --fill' "$app_dir"
expect deny  'GIT_DIR='"$user_dir"'/.git git push origin main' "$app_dir"
expect deny  'git --git-dir='"$user_dir"'/.git push origin main' "$app_dir"
expect deny  'env -C '"$user_dir"' git push origin main' "$app_dir"
expect deny  'sh -c "cd '"$user_dir"' && git push origin main"' "$app_dir"
# F4: -R / repos/ that do not really name the target.
expect deny  'gh pr create --base main --title "fix -R feral-file/ff-app"' "$user_dir"
expect deny  'gh -R feral-file/ff-app -R feral-file/ffos-user pr create --base main' "$app_dir"
expect deny  'gh pr create -Rferal-file/ffos-user --base main' "$app_dir"
expect deny  'git push origin main # gh pr list -R feral-file/ff-app' "$user_dir"
expect deny  'gh api -X PUT -f n=/repos/x/ff-app repos/feral-file/ffos-user/pulls/1/merge' "$app_dir"
# F5: the destination is not the directory's origin.
expect deny  'git push git@github.com:feral-file/ffos-user.git HEAD:main' "$app_dir"
expect deny  'git push upstream main' "$fork_clone_dir"
expect deny  'gh pr create --base main --fill' "$fork_clone_dir"
# Legitimate cross-repo forms stay allowed under the stricter model.
expect allow 'gh pr create -R feral-file/ff-app --base main --title "fix (ffos-user#332)" --body-file /tmp/b.md' "$user_dir"
expect allow 'git -C '"$app_dir"' push -u origin feat-x && gh pr create -R feral-file/ff-app --base main --fill' "$user_dir"
expect allow 'git -C '"$app_dir"' commit -m "merge into main" && git -C '"$app_dir"' push origin main' "$user_dir"

# Review round 2 probes: quoted text must never move the target.
trailing_slash_dir="$(with_remote trailing develop https://github.com/feral-file/ffos-user.git/)"
split_push_dir="$(with_remote split-push develop https://github.com/feral-file/ff-app.git)"
git -C "$split_push_dir" remote set-url --push origin git@github.com:feral-file/ffos-user.git
multi_url_dir="$(with_remote multi-url develop https://github.com/feral-file/ff-app.git)"
git -C "$multi_url_dir" remote set-url --add origin https://github.com/feral-file/ffos.git
# F1: a cd inside a commit message or echo, on one line or across lines.
expect deny  'git commit -m "fix; cd '"$app_dir"' to verify" && git push origin HEAD:main' "$user_dir"
expect deny  'git commit -m "wip && cd '"$app_dir"'" ; git push origin HEAD:release' "$user_dir"
expect deny  'echo "| cd '"$app_dir"'"; git push origin staging' "$user_dir"
expect deny  'git commit -m "a
cd '"$app_dir"'
b" && git push origin main' "$user_dir"
# F2: escaped quotes that would expose a -R from inside a title.
expect deny  'gh pr create --base main --title "x\" -R feral-file/ff-app \"y"' "$user_dir"
expect deny  'gh pr create --base main --title $'"'"'a -R feral-file/ff-app'"'"'' "$user_dir"
# F3: a cd in a pipeline runs in a subshell.
expect deny  'cd '"$app_dir"' | cat; git push origin main' "$user_dir"
# F4: a placeholder API path plus a quoted repos/ string.
expect deny  'gh api -X POST repos/:owner/:repo/merges -f base=main -f commit_message="merge repos/x/y"' "$user_dir"
# A -C or -R that exists only inside quotes does not move the target.
expect deny  'git push origin main -o "note git -C '"$app_dir"'"' "$user_dir"
# F5: every URL of every remote counts; a trailing .git/ still names the repo.
expect deny  'git push origin HEAD:main' "$trailing_slash_dir"
expect deny  'git push origin HEAD:main' "$split_push_dir"
expect deny  'git push origin HEAD:main' "$multi_url_dir"

# Review round 3 probes.
# A flow repo named as a path component: PR URLs (gh prefers them over -R)
# and API paths, including ones a client collapses with `..`.
expect deny  'gh pr merge https://github.com/feral-file/ffos-user/pull/200 --squash' "$app_dir"
expect deny  'gh pr merge https://github.com/feral-file/ffos-user/pull/200 -R feral-file/ff-app' "$user_dir"
expect deny  'gh pr edit https://github.com/feral-file/ffos-user/pull/5 --base main -R feral-file/ff-app' "$user_dir"
expect deny  'command gh api -X POST repos/feral-file/ffos-user/merges -f base=main' "$app_dir"
expect deny  'gh api -X POST repos/feral-file/ff-app/../ffos-user/merges -f base=main' "$app_dir"
# An API call with no repos/ path is never descoped by the cwd.
expect deny  'gh api -X POST repositories/123/merges -f base=main' "$app_dir"
# A -R whose value is another flag is not a repository.
expect deny  'gh pr create --base main --title -R --body x' "$user_dir"
# zsh chdir is cd; any GIT_*/GH_* assignment is an unread route.
expect deny  'chdir '"$app_dir"' && git push origin HEAD:main' "$user_dir"
expect deny  'GIT_COMMON_DIR='"$user_dir"'/.git git push origin HEAD:main' "$app_dir"
expect deny  'GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=x GIT_CONFIG_VALUE_0=y git push origin HEAD:main' "$app_dir"
# Review round 4: a command substitution runs in the shell's cwd, whatever
# the outer command's -R says.
expect deny  'gh pr create -R feral-file/ff-app --base main --title t --body "$(git push origin main)"' "$user_dir"
expect deny  'gh pr view 1 -R feral-file/ff-app $(gh pr merge 200 --squash)' "$user_dir"
# Still fine: a plain merge by number in a non-flow repo, and an ff-app API path.
expect allow 'gh pr merge 12 --squash' "$app_dir"
expect allow 'gh api repos/feral-file/ff-app/pulls/12' "$user_dir"
# REST writes are never descoped: request data can name any repository
# (repository 1026026999 is ffos-user), so even another repo's write keeps
# the full rules. Reads still pass.
expect deny  'gh api -X POST repositories/1026026999/merges -f commit_message=/repos/feral-file/ff-app -f base=main -f head=develop' "$app_dir"
expect deny  'gh -R feral-file/ff-app api -X POST repositories/1026026999/merges -f base=main' "$app_dir"
expect deny  'curl -X POST https://api.github.com/repos/o/ff-app/../../../repositories/1026026999/merges -d base=main' "$app_dir"
expect deny  'gh api -X PUT repos/feral-file/ff-app/pulls/12/merge' "$user_dir"

# A checkout through git -C moves that repository, not the current one.
expect allow 'git -C '"$staging_dir"' checkout staging && git commit -m x' "$feature_dir"

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
