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

FEATURES="UseOzonePlatform,VaapiVideoDecoder,VaapiIgnoreDriverChecks,Vulkan,DefaultANGLEVulkan,VulkanFromANGLE,DiskCacheBackendExperiment:backend/blockfile"

# Features to disable for kiosk mode
# TranslateUI: Disable translate prompt
# InterestFeedContentSuggestions: Disable NTP feed
# CalculateNativeWinOcclusion: Prevent occlusion throttling (mostly Windows but good practice)
# GlobalMediaControls: Hide media control UI
DISABLE_FEATURES="TranslateUI,InterestFeedContentSuggestions,CalculateNativeWinOcclusion,GlobalMediaControls"

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

# Chromium's HTTP cache can outlive a player-bundle swap and poison the app
# shell: darkhttpd serves the bundle with no Cache-Control headers, so Chromium
# heuristically caches the route HTML and even negative (404) chunk responses
# captured while a swap is in flight — entries that survive kiosk restarts and
# full reboots and leave the wall running the previous app or a ChunkLoadError
# page (#234). The purge is keyed to a content fingerprint of the bundle, not
# done unconditionally, so an unchanged bundle keeps the cache (remote artwork
# stays warm across the frequent Restart=always kiosk restarts) and any bundle
# change — OTA, pacman, or manual dev swap — wipes it exactly once. This must
# run only where no Chromium instance holds the cache open; this script IS the
# unit's ExecStart, so systemd has already torn down the previous cgroup and
# the delete cannot race a live browser. Keep it that way: do not move this
# into serve-feral-player.sh, which restarts independently of Chromium.
PLAYER_BUNDLE_ROOT="/opt/feral/feral-player"
CHROMIUM_CACHE_DIR="/home/feralfile/.cache/chromium"
PLAYER_FINGERPRINT_FILE="/home/feralfile/.state/player-bundle-fingerprint"

clear_chromium_cache_on_bundle_change() {
    local fingerprint previous
    # cksum over file contents, not names/mtimes: chunk filenames are
    # content-hashed but index.html and other stable-name files change in
    # place, so contents must be read. The bundle is tens of MB on local
    # disk — well under a second per kiosk start. LC_ALL=C pins sort to
    # byte order; collation drift would reorder the digest input and force
    # one spurious purge. A MISSING bundle skips the purge (cd fails):
    # serve-feral-player.service is about to fail anyway, and churning the
    # cache on a broken tree helps nothing. An existing-but-empty tree
    # still fingerprints and purges once — fail-safe, self-correcting when
    # the tree returns. Any pipeline failure returns 0: this guard must
    # never block the Chromium launch.
    fingerprint=$( (cd "$PLAYER_BUNDLE_ROOT" 2>/dev/null && \
        find . -type f -print0 | LC_ALL=C sort -z | xargs -0 cksum | cksum) 2>/dev/null ) || return 0
    previous=$(cat "$PLAYER_FINGERPRINT_FILE" 2>/dev/null || true)
    if [ "$fingerprint" = "$previous" ]; then
        return 0
    fi
    echo "$(date '+%F %T') [INFO] Player bundle changed, clearing Chromium cache"
    rm -rf "$CHROMIUM_CACHE_DIR"
    # A failed record (read-only /home, full disk) degrades into a purge on
    # EVERY kiosk restart — the exact cache churn the fingerprint exists to
    # avoid — so make that state legible in chromium.log instead of silent.
    if ! { mkdir -p "$(dirname "$PLAYER_FINGERPRINT_FILE")" && \
           printf '%s\n' "$fingerprint" > "$PLAYER_FINGERPRINT_FILE"; } 2>/dev/null; then
        echo "$(date '+%F %T') [WARN] Could not record player bundle fingerprint; cache will be purged on every kiosk start"
    fi
}

clear_chromium_cache_on_bundle_change

# A display is present: from here the CDP readiness probe and cage/Chromium can
# make real progress, so start the probe now (starting it earlier under headless
# would just time out and restart the unit for no reason).
/home/feralfile/scripts/cdp-ready-check.sh &

# Start cage with bash, which auto-detects the active output, applies the saved
# rotation, and starts Chromium.
exec cage -- /bin/bash -c "
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
    exec /usr/bin/chromium \
    --kiosk \
    --ozone-platform=wayland \
    --enable-features=$FEATURES \
    --ignore-gpu-blocklist \
    --enable-gpu-rasterization \
    --remote-debugging-port=9222 \
    --no-first-run \
    --disable-sync \
    --disable-translate \
    --disable-infobars \
    --disable-features=$DISABLE_FEATURES \
    --disable-background-networking \
    --noerrdialogs \
    --disable-extensions \
    --autoplay-policy=no-user-gesture-required \
    --disable-client-side-phishing-detection \
    --allow-file-access-from-files \
    --enable-logging=stderr \
    --v=0 \
    --hide-scrollbars \
    --disable-search-engine-choice-screen \
    --ash-no-nudges \
    --no-default-browser-check \
    --propagate-iph-for-testing \
    --disable-background-timer-throttling \
    --disable-renderer-backgrounding \
    --disable-hang-monitor \
    --deny-permission-prompts \
    --disable-external-intent-requests \
    --disable-component-extensions-with-background-pages \
    http://127.0.0.1:8080/"