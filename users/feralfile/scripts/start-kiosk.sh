#!/bin/bash

STATE_FILE="/home/feralfile/.state/screen-orientation"
DRM_STATUS_GLOB="${FF_KIOSK_DRM_STATUS_GLOB:-/sys/class/drm/card*-*/status}"
MONITOR_POLL_SECONDS="${FF_KIOSK_MONITOR_POLL_SECONDS:-2}"
CDP_READY_CHECK_SCRIPT="${FF_KIOSK_CDP_READY_CHECK_SCRIPT:-/home/feralfile/scripts/cdp-ready-check.sh}"
CHROMIUM_BIN="${FF_KIOSK_CHROMIUM_BIN:-/usr/bin/chromium}"

# Read saved rotation
ROTATION="normal"
if [ -f "$STATE_FILE" ]; then
    ROTATION=$(cat "$STATE_FILE")
fi

case "$ROTATION" in
    normal|90|180|270)
        ;;
    *)
        echo "Invalid rotation value '$ROTATION'; falling back to normal"
        ROTATION="normal"
        ;;
esac

export WLR_DRM_FORMATS="XR24/I915_FORMAT_MOD_Y_TILED;XR24/I915_FORMAT_MOD_Yf_TILED"

display_status() {
    local status_file connector readable=0
    for status_file in $DRM_STATUS_GLOB; do
        [ -r "$status_file" ] || continue
        readable=1
        case "$(cat "$status_file")" in
            connected)
                connector=$(basename "$(dirname "$status_file")")
                # /sys/class/drm/card0-HDMI-A-1/status maps to wlr-randr output HDMI-A-1.
                printf 'connected:%s\n' "$(printf '%s\n' "$connector" | sed -E 's/^card[0-9]+-//')"
                return 0
                ;;
            disconnected)
                ;;
            *)
                echo "unknown"
                return 2
                ;;
        esac
    done

    if [ "$readable" -eq 0 ]; then
        echo "unknown"
        return 2
    fi

    echo "disconnected"
    return 1
}

wait_for_monitor() {
    local status output wait_count=0
    status=$(display_status)
    case "$status" in
        connected:*)
            printf '%s\n' "${status#connected:}"
            return 0
            ;;
        unknown)
            echo "Monitor state unknown; proceeding with Chromium startup" >&2
            return 0
            ;;
    esac

    echo "No monitor connected; waiting before starting Chromium" >&2
    while true; do
        sleep "$MONITOR_POLL_SECONDS"
        status=$(display_status)
        case "$status" in
            connected:*)
                output="${status#connected:}"
                echo "Monitor connected; starting Chromium" >&2
                printf '%s\n' "$output"
                return 0
                ;;
            unknown)
                echo "Monitor state became unknown; proceeding with Chromium startup" >&2
                return 0
                ;;
        esac

        wait_count=$((wait_count + 1))
        if [ $((wait_count % 15)) -eq 0 ]; then
            echo "Still waiting for monitor before starting Chromium" >&2
        fi
    done
}

# Detect SOC vendor
VENDOR=$(grep -m1 vendor_id /proc/cpuinfo | awk '{print $3}')
if [ "$VENDOR" = "GenuineIntel" ]; then
    FEATURES="UseOzonePlatform,AcceleratedVideoDecodeLinuxGL,AcceleratedVideoDecodeLinuxZeroCopyGL"
else
    # Default to AMD
    FEATURES="UseOzonePlatform,VaapiVideoDecoder,VaapiIgnoreDriverChecks,Vulkan,DefaultANGLEVulkan,VulkanFromANGLE,DiskCacheBackendExperiment:backend/blockfile"
fi

# Features to disable for kiosk mode
# TranslateUI: Disable translate prompt
# InterestFeedContentSuggestions: Disable NTP feed
# CalculateNativeWinOcclusion: Prevent occlusion throttling (mostly Windows but good practice)
# GlobalMediaControls: Hide media control UI
DISABLE_FEATURES="TranslateUI,InterestFeedContentSuggestions,CalculateNativeWinOcclusion,GlobalMediaControls"

ACTIVE_OUTPUT=$(wait_for_monitor)
export ACTIVE_OUTPUT ROTATION FEATURES DISABLE_FEATURES CHROMIUM_BIN

"$CDP_READY_CHECK_SCRIPT" &

# Start cage and Chromium after a physical monitor is present. No-monitor is an
# expected detached state for FF1, so readiness checks are intentionally delayed
# until this point instead of timing out and restarting the kiosk forever.
exec cage -- /bin/bash -c '
    if [ -n "$ACTIVE_OUTPUT" ] && command -v wlr-randr >/dev/null 2>&1; then
        wlr-randr --output "$ACTIVE_OUTPUT" --transform "$ROTATION" || \
            echo "Failed to apply startup rotation $ROTATION to $ACTIVE_OUTPUT; continuing Chromium startup" >&2
    elif [ -z "$ACTIVE_OUTPUT" ]; then
        echo "No display output name detected; skipping startup rotation" >&2
    else
        echo "wlr-randr unavailable; skipping startup rotation" >&2
    fi

    exec "$CHROMIUM_BIN" \
        --kiosk \
        --ozone-platform=wayland \
        --enable-features="$FEATURES" \
        --ignore-gpu-blocklist \
        --enable-gpu-rasterization \
        --remote-debugging-port=9222 \
        --no-first-run \
        --disable-sync \
        --disable-translate \
        --disable-infobars \
        --disable-features="$DISABLE_FEATURES" \
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
        file:///opt/feral/ui/launcher/index.html?step=logo
'
