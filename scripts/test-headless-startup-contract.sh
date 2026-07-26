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
#   2. feral-setupd/feral-controld run unconditionally: started --no-block by
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

# setupd/controld must start whether or not a display/Chromium ever appears, and
# a pre-READY exit (e.g. relayer handshake failure) must not abort the rest of
# boot under set -e: hence --no-block, with Restart=always as the recovery rail.
assert_contains "$start_services" 'systemctl --user start --no-block "feral-controld.service"'
assert_contains "$start_services" 'systemctl --user start --no-block "feral-setupd.service"'
assert_contains "$units_dir/feral-controld.service" "Restart=always"
assert_contains "$units_dir/feral-setupd.service" "Restart=always"

# The old design tore both daemons down with every Chromium restart via
# PartOf=chromium-ready.target, killing live BLE sessions. They must never be
# re-coupled to it in any dependency direction.
assert_not_contains "$units_dir/feral-setupd.service" "chromium-ready.target"
assert_not_contains "$units_dir/feral-controld.service" "chromium-ready.target"

# setupd must start independently of player/controld in BOTH coupling
# dimensions, or BLE recovery silently re-gates on the very services whose
# failure it exists to recover from:
#   - unit ordering: an After=feral-player/feral-controld delays setupd's start
#     job even when the systemctl start itself is --no-block (a Type=notify
#     controld or a hung player start job holds it back);
#   - script ordering: .start-services.sh runs under set -e, so any BLOCKING
#     service start placed before the --no-block daemon starts can fail and
#     abort the script before setupd ever starts.
# Only dependency directives count as coupling — comments may (and do) name the
# services while documenting exactly this contract.
if grep -E '^(After|Before|Requires|Wants|BindsTo|PartOf|Requisite)=' "$units_dir/feral-setupd.service" | \
   grep -Eq 'feral-(controld|player)\.service'; then
  fail "feral-setupd.service must not declare a dependency on feral-controld/feral-player"
fi

daemon_start_line() {
  grep -nF -- "systemctl --user start --no-block \"$1\"" "$start_services" | head -1 | cut -d: -f1
}
setupd_line="$(daemon_start_line feral-setupd.service)"
controld_line="$(daemon_start_line feral-controld.service)"
[ -n "$setupd_line" ] && [ -n "$controld_line" ] || fail "missing --no-block daemon starts in $start_services"
# First blocking `systemctl --user start "..."` (quoted form; system-ready.target
# is unquoted and the backward-compat block only stops/disables).
first_blocking_line="$(grep -n 'systemctl --user start "' "$start_services" | grep -v -- --no-block | head -1 | cut -d: -f1)"
if [ -n "$first_blocking_line" ]; then
  [ "$setupd_line" -lt "$first_blocking_line" ] || \
    fail "setupd --no-block start (line $setupd_line) must precede the first blocking service start (line $first_blocking_line): a failed blocking start aborts the script under set -e before BLE recovery starts"
  [ "$controld_line" -lt "$first_blocking_line" ] || \
    fail "controld --no-block start (line $controld_line) must precede the first blocking service start (line $first_blocking_line)"
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

echo "test-headless-startup-contract: OK"
