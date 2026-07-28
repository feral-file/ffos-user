#!/usr/bin/env bash
# Contract test for the headless startup path (PR #218; display-wait predicate
# revised 2026-07: "unknown" now counts as no-display, see invariant 4).
#
# The headless boot fix spans files that no cargo/go test reads: user session
# scripts and systemd user units. These ship ONLY via the full-image rsync rail
# (see AGENTS.md "Release guardrail"), and a regression here deadlocks boot on
# real devices, so the invariants are pinned at the file level:
#
#   1. Every user shell script parses (bash -n).
#   2. feral-controld runs unconditionally: started --no-block by
#      .start-services.sh and never tied to chromium-ready.target.
#   3. chromium-ready.target stays a pure signal owned by cdp-ready-check.sh.
#   4. start-kiosk.sh keeps the rotation retry and its wait_for_display gate
#      waits until some DRM connector reads "connected" — "unknown" counts as
#      no-display (FF1's amdgpu reports it persistently on empty connectors;
#      failing open on it caused the headless Chromium restart storm) and only
#      a sysfs with no readable status at all fails open (mirrors the watchdog
#      display gate; the two predicates must not diverge).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
user_root="$repo_root/users/feralfile"
kiosk_script="$user_root/scripts/start-kiosk.sh"
start_services="$user_root/.start-services.sh"
units_dir="$user_root/systemd-services"

fail() {
  echo "test-headless-startup-contract: $*" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local needle="$2"
  grep -Fq -- "$needle" "$file" || fail "expected '$needle' in $file"
}

assert_not_contains() {
  local file="$1"
  local needle="$2"
  if grep -Fq -- "$needle" "$file"; then
    fail "must not reference '$needle': $file"
  fi
}

# --- 1. Shell syntax for every user script -----------------------------------

for script in "$user_root"/scripts/*.sh "$start_services"; do
  bash -n "$script" || fail "syntax error in $script"
done

# --- 2. Unconditional daemon startup ------------------------------------------

# controld must start whether or not a display/Chromium ever appears, and a
# pre-READY exit (e.g. relayer handshake failure) must not abort the rest of boot
# under set -e: hence --no-block, with Restart=always as the recovery rail.
assert_contains "$start_services" 'systemctl --user start --no-block "feral-controld.service"'
assert_contains "$units_dir/feral-controld.service" "Restart=always"

# The old design tore controld down with every Chromium restart via
# PartOf=chromium-ready.target, killing live recovery/provisioning sessions. It
# must never be re-coupled to it in any dependency direction.
assert_not_contains "$units_dir/feral-controld.service" "chromium-ready.target"

# controld owns WiFi/SoftAP provisioning and LAN recovery, so its --no-block
# start must precede any BLOCKING service start: .start-services.sh runs under
# set -e, so a failed blocking start placed before it aborts the script before
# recovery ever starts.
daemon_start_line() {
  grep -nF -- "systemctl --user start --no-block \"$1\"" "$start_services" | head -1 | cut -d: -f1
}
controld_line="$(daemon_start_line feral-controld.service)"
[ -n "$controld_line" ] || fail "missing --no-block controld start in $start_services"
# First blocking `systemctl --user start "..."` (quoted form; system-ready.target
# is unquoted and the backward-compat block only stops/disables).
first_blocking_line="$(grep -n 'systemctl --user start "' "$start_services" | grep -v -- --no-block | head -1 | cut -d: -f1)"
if [ -n "$first_blocking_line" ]; then
  [ "$controld_line" -lt "$first_blocking_line" ] || \
    fail "controld --no-block start (line $controld_line) must precede the first blocking service start (line $first_blocking_line): a failed blocking start aborts the script under set -e before recovery starts"
fi

# --- 3. chromium-ready.target stays a pure signal -----------------------------

[ -f "$units_dir/chromium-ready.target" ] || fail "chromium-ready.target unit missing"
for unit in "$units_dir"/*; do
  case "$(basename "$unit")" in
    chromium-ready.target) continue ;;
    chromium-kiosk.service) continue ;; # ExecStopPost stops the signal — allowed
  esac
  assert_not_contains "$unit" "chromium-ready.target"
done

# --- 4. start-kiosk.sh contract ------------------------------------------------

# Rotation must be retried, then Chromium launched regardless (a wlr-randr error
# may neither loop the kiosk nor leave the panel permanently unrotated).
assert_contains "$kiosk_script" "for attempt in 1 2 3"

# wait_for_display behavior, run against a fixture sysfs root. The function is
# extracted verbatim with its DRM glob retargeted, so the assertions exercise
# the exact shipped logic.
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

drm_root="$tmp_dir/drm"
mkdir -p "$drm_root/card0-HDMI-A-1" "$drm_root/card1-DP-1"
fn_file="$tmp_dir/wait_for_display.sh"
sed -n '/^wait_for_display() {/,/^}/p' "$kiosk_script" | \
  sed "s|/sys/class/drm|$drm_root|" > "$fn_file"
grep -q 'wait_for_display() {' "$fn_file" || fail "could not extract wait_for_display"

# Runs wait_for_display and asserts it returns promptly with the expected log
# line. A regression that makes these inputs wait HANGS rather than erroring,
# so the run is bounded by a kill watchdog — otherwise a regression would stall
# CI instead of failing it. (No `timeout(1)` — keep the harness runnable on
# macOS dev machines too.)
expect_proceed() {
  local case_name="$1"
  local expected="$2"
  local out_file="$tmp_dir/out.$$"
  bash -c "source '$fn_file'; wait_for_display" > "$out_file" 2>&1 &
  local pid=$!
  local waited=0
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$waited" -ge 10 ]; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      fail "$case_name: wait_for_display still waiting after ${waited}s (must proceed)"
    fi
    sleep 1
    waited=$((waited + 1))
  done
  wait "$pid" || fail "$case_name: wait_for_display failed"
  local out
  out="$(tail -1 "$out_file")"
  case "$out" in
    *"$expected"*) ;;
    *) fail "$case_name: expected '$expected', got '$out'" ;;
  esac
}

# Starts wait_for_display and requires it to STILL be waiting after a short
# window. A regression toward any fail-open on these inputs returns promptly,
# which is exactly the headless restart storm this gate exists to prevent.
expect_waiting() {
  local case_name="$1"
  bash -c "source '$fn_file'; wait_for_display" >/dev/null 2>&1 &
  local pid=$!
  sleep 2
  if ! kill -0 "$pid" 2>/dev/null; then
    fail "$case_name: wait_for_display returned instead of waiting"
  fi
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

echo connected > "$drm_root/card0-HDMI-A-1/status"
echo disconnected > "$drm_root/card1-DP-1/status"
expect_proceed "connected connector" "Display connected"

# FF1's amdgpu reads "unknown" on empty connectors; failing open here launched
# cage/Chromium with zero outputs on every headless boot (CDP timeout restart
# storm, sustained high CPU temperature). "unknown" must hold the wait.
echo unknown > "$drm_root/card0-HDMI-A-1/status"
expect_waiting "unknown alongside disconnected waits"

echo disconnected > "$drm_root/card0-HDMI-A-1/status"
echo disconnected > "$drm_root/card1-DP-1/status"
expect_waiting "all-disconnected waits"

# Parity with the watchdog unit test (display_test.go "unreadable connector
# alongside readable disconnected"): one unreadable connector next to a
# readable "disconnected" one must also hold the wait — readable statuses
# exist and none reads "connected". status-as-directory makes cat fail the
# same way the Go fixture does.
rm "$drm_root/card0-HDMI-A-1/status"
mkdir "$drm_root/card0-HDMI-A-1/status"
expect_waiting "unreadable alongside disconnected waits"

# No readable status at all is the ONLY remaining fail-open: without DRM sysfs
# the wait could never resolve, so the kiosk must fall through to cage and its
# Restart=always recovery.
rmdir "$drm_root/card0-HDMI-A-1/status"
rm "$drm_root/card1-DP-1/status"
expect_proceed "no readable status fails open" "fail open"

# --- 5. player-bundle cache guard (#234) ---------------------------------------

# Chromium's disk cache can keep serving a previous player app (or
# negatively-cached 404s for its chunks) across kiosk restarts AND reboots
# after a bundle swap. serve-feral-player.sh mitigates with a global
# "Cache-Control: no-cache" when darkhttpd supports --header, but entries
# cached headerless — before that header was active, or under the legacy
# headerless fallback — are reused without revalidation, so the guard in
# start-kiosk.sh purges the cache exactly when the bundle fingerprint
# changes; these cases pin both directions (purge on change, keep when
# unchanged — the keep side is what protects remote-artwork cache warmth).
# The guard must actually be CALLED, and called before Chromium exists —
# extracting and testing the function alone would stay green if the invocation
# were deleted or moved after the launch, where a purge races a live browser.
# `|| true` keeps a no-match from tripping pipefail inside the substitution,
# so the legible fail() below is what reports the regression.
guard_call_line="$(grep -n '^clear_chromium_cache_on_bundle_change$' "$kiosk_script" | head -1 | cut -d: -f1 || true)"
[ -n "$guard_call_line" ] || fail "start-kiosk.sh never calls clear_chromium_cache_on_bundle_change"
cage_line="$(grep -n '^exec cage' "$kiosk_script" | head -1 | cut -d: -f1 || true)"
[ -n "$cage_line" ] || fail "could not locate exec cage in $kiosk_script"
[ "$guard_call_line" -lt "$cage_line" ] || \
  fail "cache guard (line $guard_call_line) must run before Chromium launches (line $cage_line)"

# Pin the production constants: a typo in any of these makes the guard a
# silent no-op while the functional cases below (which override them) stay
# green. `.state` is the OTA-surviving location for the fingerprint, and
# ~/.cache/chromium is Chromium's default cache root for this launch (no
# --disk-cache-dir flag is passed).
assert_contains "$kiosk_script" 'PLAYER_BUNDLE_ROOT="/opt/feral/feral-player"'
assert_contains "$kiosk_script" 'CHROMIUM_CACHE_DIR="/home/feralfile/.cache/chromium"'
assert_contains "$kiosk_script" 'PLAYER_FINGERPRINT_FILE="/home/feralfile/.state/player-bundle-fingerprint"'

# The fingerprint is persistent state, so its write must follow the
# temp-then-rename contract (docs/architecture.md "Persistence and State
# Ownership") — a crash mid-write must not leave a truncated fingerprint.
assert_contains "$kiosk_script" 'mv -f "$PLAYER_FINGERPRINT_FILE.tmp" "$PLAYER_FINGERPRINT_FILE"'

fp_fn_file="$tmp_dir/cache_guard.sh"
sed -n '/^clear_chromium_cache_on_bundle_change() {/,/^}/p' "$kiosk_script" > "$fp_fn_file"
grep -q 'clear_chromium_cache_on_bundle_change() {' "$fp_fn_file" || \
  fail "could not extract clear_chromium_cache_on_bundle_change"

bundle_dir="$tmp_dir/bundle"
cache_dir="$tmp_dir/chromium-cache"
fp_file="$tmp_dir/state/player-bundle-fingerprint"

# Captures guard stdout: the "[INFO] Player bundle changed" line is the only
# field-diagnosable signal the purge fired, so its presence/absence is asserted
# alongside the filesystem effects.
guard_out="$tmp_dir/guard.out"
run_cache_guard() {
  bash -c "
    PLAYER_BUNDLE_ROOT='$bundle_dir'
    CHROMIUM_CACHE_DIR='$cache_dir'
    PLAYER_FINGERPRINT_FILE='$fp_file'
    source '$fp_fn_file'
    clear_chromium_cache_on_bundle_change
  " > "$guard_out" || fail "cache guard exited non-zero"
}

assert_purge_logged() {
  grep -Fq "Player bundle changed" "$guard_out" || \
    fail "$1: purge must log the bundle-changed line"
}

assert_no_purge_logged() {
  if grep -Fq "Player bundle changed" "$guard_out"; then
    fail "$1: keep path must not log the bundle-changed line"
  fi
}

seed_cache() {
  mkdir -p "$cache_dir/Default/Cache"
  touch "$cache_dir/Default/Cache/marker"
}

mkdir -p "$bundle_dir/_next/static/chunks"
echo "chunk-v1" > "$bundle_dir/_next/static/chunks/a.js"
echo "index-v1" > "$bundle_dir/index.html"

# First run (no recorded fingerprint) must purge: devices already poisoned by a
# past swap are healed by the release that ships the guard.
seed_cache
run_cache_guard
[ ! -e "$cache_dir/Default/Cache/marker" ] || fail "first run must clear the cache"
[ -s "$fp_file" ] || fail "first run must record a fingerprint"
assert_purge_logged "first run"

# Unchanged bundle must keep the cache.
seed_cache
run_cache_guard
[ -e "$cache_dir/Default/Cache/marker" ] || fail "unchanged bundle must keep the cache"
assert_no_purge_logged "unchanged bundle"

# In-place content change with an unchanged filename and size (index.html is
# not content-hashed) must purge — this is why the fingerprint reads contents.
echo "index-v2" > "$bundle_dir/index.html"
run_cache_guard
[ ! -e "$cache_dir/Default/Cache/marker" ] || fail "changed bundle must clear the cache"
assert_purge_logged "changed bundle"

# Deleting a file (with all remaining contents unchanged) must also purge:
# filenames are embedded in the cksum lines, so removals change the digest.
seed_cache
rm "$bundle_dir/_next/static/chunks/a.js"
run_cache_guard
[ ! -e "$cache_dir/Default/Cache/marker" ] || fail "file removal must clear the cache"
assert_purge_logged "file removal"

# A missing bundle tree must not purge: serve-feral-player.service is about to
# fail readiness anyway, and the cache may still be wanted when the tree returns.
seed_cache
fp_before="$(cat "$fp_file")"
mv "$bundle_dir" "$bundle_dir.gone"
run_cache_guard
[ -e "$cache_dir/Default/Cache/marker" ] || fail "missing bundle must not clear the cache"
[ "$(cat "$fp_file")" = "$fp_before" ] || fail "missing bundle must not rewrite the fingerprint"
assert_no_purge_logged "missing bundle"
mv "$bundle_dir.gone" "$bundle_dir"

# A failed cache delete must NOT record the new fingerprint: recording it
# would mark the purge as done and leave the stale cache in place on every
# future start — silently reintroducing the bug the guard exists to fix. The
# retained fingerprint makes the next start retry the purge. Write-bit removal
# on the containing dir makes unlink fail; root ignores permission bits, so
# the case only runs unprivileged (CI and dev machines are).
if [ "$(id -u)" -ne 0 ]; then
  echo "index-v3" > "$bundle_dir/index.html"
  fp_before="$(cat "$fp_file")"
  chmod a-w "$cache_dir/Default/Cache"
  run_cache_guard
  chmod u+w "$cache_dir/Default/Cache"
  [ -e "$cache_dir/Default/Cache/marker" ] || fail "failed-delete fixture did not hold (marker gone)"
  [ "$(cat "$fp_file")" = "$fp_before" ] || fail "failed delete must not record the new fingerprint"
  grep -Fq "Could not delete Chromium cache" "$guard_out" || \
    fail "failed delete must log the retry warning"
  run_cache_guard
  [ ! -e "$cache_dir/Default/Cache/marker" ] || fail "retry after failed delete must clear the cache"
  [ "$(cat "$fp_file")" != "$fp_before" ] || fail "successful retry must record the new fingerprint"
fi

# --- 6. controld start-limit never latches -------------------------------------

# controld is the device's only provisioning path (SoftAP portal + LAN hub —
# no BLE fallback), so a systemd start-limit latch ("start-limit-hit") would
# permanently strand an offline device instead of degrading into slow
# Restart=always retries. See docs/architecture.md "Relaxed StartLimit".
assert_contains "$units_dir/feral-controld.service" "StartLimitIntervalSec=0"

echo "test-headless-startup-contract: OK"
