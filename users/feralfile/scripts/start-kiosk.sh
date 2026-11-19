#!/bin/bash

# Read saved rotation. The state file may exist but be empty or corrupt; an
# invalid --transform value makes wlr-randr fail, which short-circuits the &&
# chain below and Chromium never starts. Only accept the values wlr-randr
# supports (same whitelist as display-restore.sh), otherwise fall back.
ROTATION="normal"
if [ -f /home/feralfile/.state/screen-orientation ]; then
    ROTATION=$(cat /home/feralfile/.state/screen-orientation)
fi
case "$ROTATION" in
    normal|90|180|270) ;;
    *) ROTATION="normal" ;;
esac

# Block until a display is physically connected before launching cage/Chromium.
# On a headless boot there is no output for wlr-randr to configure and cage would
# exit immediately; with chromium-kiosk.service Restart=always that turns into a
# relaunch storm (and cdp-ready-check would also fire a restart on its 90s
# timeout). sysfs DRM status is readable without a running compositor, unlike
# wlr-randr, so it is the correct probe here and matches the source
# display-restore.sh watches. A monitor may be attached later on any connector,
# so we wait indefinitely rather than time out.
#
# The wait holds until some connector positively reads "connected". An
# "unknown" reading must NOT fail open: FF1's amdgpu persistently reports it on
# connectors with nothing attached, so the earlier "wait only while everything
# reads disconnected" gate never engaged on real headless hardware — every boot
# fell open into cage/Chromium with zero outputs, where CDP can never come up,
# and cdp-ready-check's 90s timeout + Restart=always produced a permanent
# restart storm (observed in the field as sustained high CPU temperature).
# Waiting on "unknown" is safe because attaching a real monitor raises HPD and
# flips that connector to "connected", which the poll below observes; there is
# no display that stays "unknown" while in use on this hardware.
#
# The only fail-open left is NO readable connector status at all: on an
# environment without DRM sysfs the wait could never resolve, so fall through
# to cage and its Restart=always recovery, mirroring the watchdog display gate.
wait_for_display() {
    local announced=0 f status saw_status conn readings
    while true; do
        saw_status=0
        readings=""
        for f in /sys/class/drm/card*-*/status; do
            status=$(cat "$f" 2>/dev/null) || continue
            saw_status=1
            conn=${f%/status}
            readings="$readings ${conn##*/}=$status"
            if [ "$status" = "connected" ]; then
                echo "$(date '+%F %T') [INFO] Display connected, starting kiosk"
                return
            fi
        done
        if [ "$saw_status" -eq 0 ]; then
            echo "$(date '+%F %T') [INFO] No readable DRM connector status, starting kiosk (fail open)"
            return
        fi
        # Log the transition into the waiting state once, not on every poll, to
        # keep the log readable during long headless periods. The per-connector
        # readings make a headless-vs-gate misfire diagnosable from this one line.
        if [ "$announced" -eq 0 ]; then
            echo "$(date '+%F %T') [INFO] No display connected (${readings# }), waiting for one to be attached..."
            announced=1
        fi
        sleep 3
    done
}

wait_for_display

# Developer console guard (ffos#126). The kiosk owns VT1 only: cage runs with
# -s so a developer with a keyboard can Ctrl+Alt+F2 to the password-protected
# getty@tty2, and seatd hands a starting cage whichever VT is active. Without
# this wait a kiosk (re)start (Restart=always after Alt+F4, a watchdog restart)
# would take over the developer's VT within seconds. Wait until tty1 is active
# again (Alt+F1 from the text console). feral-watchdog consults the same file
# and suppresses escalation while it does not read tty1; the two predicates
# must not diverge. Fail open when the file is unreadable (no VT subsystem).
wait_for_vt1() {
    local announced=0 active
    while true; do
        if ! active=$(cat /sys/class/tty/tty0/active 2>/dev/null); then
            echo "$(date '+%F %T') [INFO] No readable VT state, starting kiosk (fail open)"
            return 0
        fi
        [ "$active" = "tty1" ] && return
        if [ "$announced" -eq 0 ]; then
            echo "$(date '+%F %T') [INFO] $active is active (developer console?), waiting for tty1 before starting kiosk"
            announced=1
        fi
        sleep 2
    done
}

wait_for_vt1

# A display is present: from here the native readiness probe and Cage/player can
# make real progress, so start the probe now (starting it earlier under headless
# would just time out and restart the unit for no reason).
/home/feralfile/scripts/player-ready-check.sh &

# Start cage with bash, which auto-detects the active output, applies the saved
# rotation, and starts Chromium. -s allows VT switching (developer console on
# tty2, see wait_for_vt1 above); cage forbids it by default.
exec cage -s -- /bin/bash -c "
    # Rotation must never gate the browser launch: the old code joined it with
    # '&&', so any wlr-randr error left the kiosk with no Chromium and
    # Restart=always looping. But it also must not give up on the first error —
    # display-restore.sh only reapplies rotation on a DRM change event, so a
    # transient failure here would otherwise leave the panel unrotated until a
    # hotplug or reboot. Retry a few times (re-detecting the output, which may
    # still be settling right after cage starts), then launch either way.
    for attempt in 1 2 3; do
        # Detect the first enabled output the same way display-restore.sh does;
        # the connector is not fixed (HDMI-A-1 was a wrong assumption on
        # non-HDMI setups).
        OUTPUT=\$(wlr-randr 2>/dev/null | awk '
            /^(HDMI|DP|eDP|DVI|VGA|DSI|LVDS)/ { current_output = \$1 }
            /Enabled: yes/ { print current_output; exit }
        ')
        if [ -n \"\$OUTPUT\" ] && wlr-randr --output \"\$OUTPUT\" --transform $ROTATION; then
            break
        fi
        echo \"\$(date '+%F %T') [WARN] rotation attempt \$attempt failed (output: \${OUTPUT:-none})\"
        sleep 1
    done
    export SDL_VIDEODRIVER=wayland
    export AMD_DEBUG=nodcc
    exec /usr/bin/env -u DBUS_SESSION_BUS_ADDRESS /usr/lib/ff-player-native/ff-player-native \
      --state-dir=/home/feralfile/.state/player \
      --socket=/run/user/1000/ff-player/player.sock \
      --renderer-dir=/usr/share/ff-player-native/renderers
"
