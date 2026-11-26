#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
display_check="$repo_root/users/feralfile/scripts/display-check.sh"
page_dir="$repo_root/users/feralfile/display-check"
file_permissions="$repo_root/users/feralfile/.file_permissions.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-display-check: $*" >&2
  exit 1
}

grep -Fq "chmod +x /home/feralfile/scripts/display-check.sh" "$file_permissions" || \
  fail "device install does not make display-check.sh executable"

# The page and its clip ship together; the video slide has no other source.
[ -s "$page_dir/index.html" ] || fail "display-check/index.html is missing"
[ -s "$page_dir/motion.mp4" ] || fail "display-check/motion.mp4 is missing"
grep -Fq "'motion.mp4'" "$page_dir/index.html" || fail "the page no longer loads motion.mp4"

# Every set a boot entry can name exists in the page.
for set in brightness panel assembly; do
  grep -Eq "^[[:space:]]*$set:" "$page_dir/index.html" || fail "the page has no '$set' set"
done

# The Factory check runs one pass after every reading, fail-open.
# shellcheck disable=SC2016 # the literal line in factory-check.sh
grep -Fq 'bash "$SCRIPTS/display-check.sh" once >/dev/null 2>&1 || true' \
  "$repo_root/users/feralfile/scripts/factory-check.sh" || \
  fail "factory-check.sh no longer runs a fail-open display-check.sh once"

# shellcheck source=/dev/null
source "$display_check"

# ONCE_SECONDS must hold a whole assembly pass, with room for Chromium's start,
# or the factory check would cut the graphics slide (the last) short.
assembly_ids=$(sed -n "s/^[[:space:]]*assembly:[[:space:]]*\[\(.*\)\],*$/\1/p" "$page_dir/index.html" | tr -d "' ")
[ -n "$assembly_ids" ] || fail "cannot read the assembly set from the page"
pass=0
for id in ${assembly_ids//,/ }; do
  s=$(sed -n "s/^[[:space:]]*{ id: '$id',[[:space:]]*s: *\([0-9]*\),.*/\1/p" "$page_dir/index.html")
  [ -n "$s" ] || fail "cannot read slide '$id' length from the page"
  pass=$((pass + s))
done
[ "$ONCE_SECONDS" -ge $((pass + 10)) ] || \
  fail "ONCE_SECONDS=$ONCE_SECONDS does not hold an assembly pass ($pass s) plus 10 s for Chromium"

expect_set() {
  local cmdline=$1 want=$2 got
  printf '%s\n' "$cmdline" > "$tmp_dir/cmdline"
  got=$(FF_CMDLINE="$tmp_dir/cmdline" CMDLINE="$tmp_dir/cmdline" check_set)
  [ "$got" = "$want" ] || fail "cmdline '$cmdline': set '$got', want '$want'"
}

base='BOOT_IMAGE=/arch/boot/x86_64/vmlinuz-linux archisobasedir=arch script=/home/feralfile/scripts/display-check.sh'
expect_set "$base display_check=brightness" brightness
expect_set "$base display_check=panel" panel
expect_set "$base display_check=assembly" assembly
expect_set "$base" assembly
expect_set "$base display_check=typo" assembly

[ "$(page_url assembly)" = "file:///home/feralfile/display-check/index.html" ] || fail "assembly url"
[ "$(page_url brightness)" = "file:///home/feralfile/display-check/index.html?set=brightness" ] || fail "brightness url"
[ "$(page_url panel)" = "file:///home/feralfile/display-check/index.html?set=panel" ] || fail "panel url"

echo "test-display-check: ok"
