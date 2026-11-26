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

# shellcheck source=/dev/null
source "$display_check"

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
