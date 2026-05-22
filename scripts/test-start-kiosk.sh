#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script_under_test="$repo_root/users/feralfile/scripts/start-kiosk.sh"

fail() {
  echo "test-start-kiosk: $*" >&2
  exit 1
}

assert_file_exists() {
  local file="$1"
  [[ -f "$file" ]] || fail "expected file $file to exist"
}

assert_file_absent() {
  local file="$1"
  [[ ! -f "$file" ]] || fail "expected file $file to be absent"
}

wait_for_file() {
  local file="$1"
  for _ in {1..30}; do
    [[ -f "$file" ]] && return 0
    sleep 0.1
  done
  return 1
}

assert_file_eventually_exists() {
  local file="$1"
  wait_for_file "$file" || fail "timed out waiting for $file"
}

make_fakes() {
  local bin_dir="$1"
  local record_dir="$2"
  mkdir -p "$bin_dir" "$record_dir"

  cat >"$bin_dir/cage" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"${FF_KIOSK_TEST_CAGE_ARGS:?missing cage args path}"
if [[ "${1:-}" == "--" ]]; then
  shift
fi
exec "$@"
EOS

  cat >"$bin_dir/chromium" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"${FF_KIOSK_TEST_CHROMIUM_ARGS:?missing chromium args path}"
exit 0
EOS

  cat >"$bin_dir/wlr-randr" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FF_KIOSK_TEST_WLR_ARGS:?missing wlr args path}"
if [[ "${FF_KIOSK_TEST_WLR_FAIL:-0}" == "1" ]]; then
  echo "simulated wlr-randr failure" >&2
  exit 1
fi
exit 0
EOS

cat >"$bin_dir/cdp-ready-check.sh" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
: >"${FF_KIOSK_TEST_CDP_STARTED:?missing cdp path}"
EOS

  chmod +x "$bin_dir/cage" "$bin_dir/chromium" "$bin_dir/wlr-randr" "$bin_dir/cdp-ready-check.sh"
}

new_status_tree() {
  local root="$1"
  local status="$2"
  mkdir -p "$root/card0-HDMI-A-1"
  printf '%s\n' "$status" >"$root/card0-HDMI-A-1/status"
}

run_kiosk_background() {
  local tmp_dir="$1"
  local status_glob="$2"
  shift 2
  FF_KIOSK_DRM_STATUS_GLOB="$status_glob" \
  FF_KIOSK_MONITOR_POLL_SECONDS="0.1" \
  FF_KIOSK_CDP_READY_CHECK_SCRIPT="$tmp_dir/bin/cdp-ready-check.sh" \
  FF_KIOSK_CHROMIUM_BIN="$tmp_dir/bin/chromium" \
  FF_KIOSK_TEST_CAGE_ARGS="$tmp_dir/cage.args" \
  FF_KIOSK_TEST_CHROMIUM_ARGS="$tmp_dir/chromium.args" \
  FF_KIOSK_TEST_WLR_ARGS="$tmp_dir/wlr.args" \
  FF_KIOSK_TEST_CDP_STARTED="$tmp_dir/cdp.started" \
  PATH="$tmp_dir/bin:$PATH" \
  "$@" bash "$script_under_test" >"$tmp_dir/output.log" 2>&1 &
  echo $!
}

# Case 1: disconnected monitor waits and does not launch Chromium/cdp checker.
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
make_fakes "$tmp_dir/bin" "$tmp_dir"
new_status_tree "$tmp_dir/drm" disconnected
pid=$(run_kiosk_background "$tmp_dir" "$tmp_dir/drm/card*-*/status" env)
sleep 0.4
assert_file_absent "$tmp_dir/cage.args"
assert_file_absent "$tmp_dir/cdp.started"
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true

# Case 2: monitor appears later and kiosk proceeds.
rm -rf "$tmp_dir"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
make_fakes "$tmp_dir/bin" "$tmp_dir"
new_status_tree "$tmp_dir/drm" disconnected
pid=$(run_kiosk_background "$tmp_dir" "$tmp_dir/drm/card*-*/status" env)
sleep 0.2
assert_file_absent "$tmp_dir/cage.args"
printf 'connected\n' >"$tmp_dir/drm/card0-HDMI-A-1/status"
assert_file_eventually_exists "$tmp_dir/cage.args"
assert_file_eventually_exists "$tmp_dir/chromium.args"
assert_file_eventually_exists "$tmp_dir/cdp.started"
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true

# Case 3: rotation failure is best-effort and does not block Chromium.
rm -rf "$tmp_dir"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
make_fakes "$tmp_dir/bin" "$tmp_dir"
new_status_tree "$tmp_dir/drm" connected
pid=$(run_kiosk_background "$tmp_dir" "$tmp_dir/drm/card*-*/status" env FF_KIOSK_TEST_WLR_FAIL=1)
assert_file_eventually_exists "$tmp_dir/cage.args"
assert_file_eventually_exists "$tmp_dir/chromium.args"
assert_file_eventually_exists "$tmp_dir/cdp.started"
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true

# Case 4: unknown DRM status is fail-open so a detection problem does not
# permanently block kiosk startup on hardware with unexpected sysfs shape.
rm -rf "$tmp_dir"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
make_fakes "$tmp_dir/bin" "$tmp_dir"
pid=$(run_kiosk_background "$tmp_dir" "$tmp_dir/missing/card*-*/status" env)
assert_file_eventually_exists "$tmp_dir/cage.args"
assert_file_eventually_exists "$tmp_dir/chromium.args"
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true


# Case 5: literal DRM "unknown" status is also fail-open.
rm -rf "$tmp_dir"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
make_fakes "$tmp_dir/bin" "$tmp_dir"
new_status_tree "$tmp_dir/drm" unknown
pid=$(run_kiosk_background "$tmp_dir" "$tmp_dir/drm/card*-*/status" env)
assert_file_eventually_exists "$tmp_dir/cage.args"
assert_file_eventually_exists "$tmp_dir/chromium.args"
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true

echo "test-start-kiosk: OK"
