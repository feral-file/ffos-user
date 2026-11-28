#!/usr/bin/env bash
# The display-check page (users/feralfile/display-check/), which the factory
# burn-in shows. Only the page is checked here; ff1-burnin.sh and its own test
# own how it is shown.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
page_dir="$repo_root/users/feralfile/display-check"

fail() {
  echo "test-display-check: $*" >&2
  exit 1
}

# The page and its clip ship together; the video slide has no other source.
[ -s "$page_dir/index.html" ] || fail "display-check/index.html is missing"
[ -s "$page_dir/motion.mp4" ] || fail "display-check/motion.mp4 is missing"
grep -Fq "'motion.mp4'" "$page_dir/index.html" || fail "the page no longer loads motion.mp4"

# Every set a caller can name (?set=) exists in the page. brightness and panel
# also serve the copy of the page hosted outside the device.
for set in brightness panel assembly fields; do
  grep -Eq "^[[:space:]]*$set:" "$page_dir/index.html" || fail "the page has no '$set' set"
done

echo "test-display-check: ok"
