#!/bin/bash

STATE_FILE="/home/feralfile/.config/screen-orientation"
OUTPUT="HDMI-A-1"

get_rotation() {
    if [ -f "$STATE_FILE" ]; then
        cat "$STATE_FILE"
    else
        echo "normal"
    fi
}

apply_rotation_and_best_mode() {
    rotation=$(get_rotation)

    # Verify rotation value
    case "$rotation" in
        normal|90|180|270)
            ;;
        *)
            echo "$(date '+%F %T') [ERROR] Invalid rotation value: $rotation"
            return
            ;;
    esac

    # Get current resolution from line containing "(current)"
    current_res=$(wlr-randr --output "$OUTPUT" | grep "current" | head -n1 | awk '{print $1}')

    if [ -z "$current_res" ]; then
        echo "$(date '+%F %T') [WARN] Could not detect current resolution for $OUTPUT, applying only rotation=$rotation"
        wlr-randr --output "$OUTPUT" --transform "$rotation"
        return
    fi

    # Get all refresh rates for current resolution
    modes=$(wlr-randr --output "$OUTPUT" \
        | grep -E "^\s*${current_res} px," \
        | grep -Eo '[0-9]+\.[0-9]+')

    if [ -z "$modes" ]; then
        echo "$(date '+%F %T') [WARN] No valid modes found for resolution=$current_res on $OUTPUT, applying only rotation=$rotation"
        wlr-randr --output "$OUTPUT" --transform "$rotation"
        return
    fi

    # Pick the highest refresh rate
    best_refresh=$(echo "$modes" | sort -nr | head -n1)
    best_mode="${current_res}@${best_refresh}Hz"

    echo "$(date '+%F %T') [INFO] Applying mode=$best_mode with rotation=$rotation to $OUTPUT"
    wlr-randr --output "$OUTPUT" --mode "$best_mode" --transform "$rotation"
}

echo "$(date '+%F %T') [INFO] Starting HDMI restore script"

udevadm monitor --kernel --subsystem-match=drm | while read -r line; do
    if [[ "$line" == *"change"* ]]; then
        status=$(cat /sys/class/drm/card1-${OUTPUT}/status 2>/dev/null)
        echo "$(date '+%F %T') [DEBUG] Event detected: $line, HDMI status=$status"
        if [ "$status" = "connected" ]; then
            apply_rotation_and_best_mode
        fi
    fi
done