#!/bin/bash

# Read saved rotation
ROTATION="normal"
if [ -f /home/feralfile/.state/screen-orientation ]; then
    ROTATION=$(cat /home/feralfile/.state/screen-orientation)
fi

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
# Waiting is only safe while the state is POSITIVELY known: every connector
# readable and every one reading exactly "disconnected". Anything else — no
# readable connectors, or a readable value the kernel could not resolve (DRM
# reports "unknown" for unprobeable connectors, which may well have a display
# attached) — must fall through to cage and its pre-existing Restart=always
# recovery, mirroring the watchdog's fail-open display gate. Otherwise an
# unknown-state box could sit in this loop forever with a working monitor and
# no recovery path.
wait_for_display() {
    local announced=0 f status saw_status all_disconnected
    while true; do
        saw_status=0
        all_disconnected=1
        for f in /sys/class/drm/card*-*/status; do
            status=$(cat "$f" 2>/dev/null) || continue
            saw_status=1
            case "$status" in
                connected)
                    echo "$(date '+%F %T') [INFO] Display connected, starting kiosk"
                    return
                    ;;
                disconnected) ;;
                *)
                    # e.g. "unknown": not a positive no-display reading.
                    all_disconnected=0
                    ;;
            esac
        done
        if [ "$saw_status" -eq 0 ] || [ "$all_disconnected" -eq 0 ]; then
            echo "$(date '+%F %T') [INFO] Display state not positively disconnected, starting kiosk (fail open)"
            return
        fi
        # Log the transition into the waiting state once, not on every poll, to
        # keep the log readable during long headless periods.
        if [ "$announced" -eq 0 ]; then
            echo "$(date '+%F %T') [INFO] No display connected, waiting for one to be attached..."
            announced=1
        fi
        sleep 3
    done
}

wait_for_display

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
    file:///opt/feral/ui/launcher/index.html?step=logo"