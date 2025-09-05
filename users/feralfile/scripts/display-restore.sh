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

apply_rotation() {
    rotation=$(get_rotation)
    case "$rotation" in
        normal|90|180|270)
            echo "$(date '+%F %T') [INFO] Applying rotation=$rotation to $OUTPUT"
            wlr-randr --output "$OUTPUT" --transform "$rotation"
            ;;
        *)
            echo "$(date '+%F %T') [ERROR] Invalid rotation value: $rotation"
            ;;
    esac
}

echo "$(date '+%F %T') [INFO] Starting HDMI restore script"

udevadm monitor --kernel --subsystem-match=drm | while read -r line; do
    if [[ "$line" == *"change"* ]]; then
        status=$(cat /sys/class/drm/card1-${OUTPUT}/status 2>/dev/null)
        echo "$(date '+%F %T') [DEBUG] Event detected: $line, HDMI status=$status"
        if [ "$status" = "connected" ]; then
            apply_rotation
        fi
    fi
done