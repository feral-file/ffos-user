#!/bin/bash

STATE_FILE="/home/feralfile/.state/screen-orientation"
CONTROLD_STATE_FILE="/home/feralfile/.state/controld.state"

get_rotation() {
    if [ -f "$STATE_FILE" ]; then
        cat "$STATE_FILE"
    else
        echo "normal"
    fi
}

is_sleep_mode() {
    if [ ! -f "$CONTROLD_STATE_FILE" ]; then
        return 1  # Not in sleep mode if state file doesn't exist
    fi
    
    # Check if sleep mode is enabled in the state file
    if grep -q '"sleepMode":{[^}]*"enabled":true' "$CONTROLD_STATE_FILE" 2>/dev/null; then
        return 0  # In sleep mode
    else
        return 1  # Not in sleep mode
    fi
}

# Auto-detect first connected/enabled output
detect_output() {
    # Parse wlr-randr output to find enabled outputs
    # Each output block starts with a non-indented line (output name)
    # and contains "Enabled: yes" if connected
    local wlr_output=$(wlr-randr 2>/dev/null)

    # Find first connected output
    local detected=$(echo "$wlr_output" | awk '
        /^(HDMI|DP|eDP|DVI|VGA|DSI|LVDS)/ { current_output = $1 }
        /Enabled: yes/ { print current_output; exit }
    ')

    if [ -n "$detected" ]; then
        echo "$detected"
    else
        echo "" # No output found
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

    # Detect available output
    local active_output=$(detect_output)
    if [ -z "$active_output" ]; then
        echo "$(date '+%F %T') [ERROR] No active output detected. Available outputs:"
        wlr-randr 2>&1 | head -n 10
        return
    fi

    # Get current resolution from line containing "(current)"
    current_res=$(wlr-randr --output "$active_output" 2>/dev/null | grep "current" | head -n1 | awk '{print $1}')

    if [ -z "$current_res" ]; then
        echo "$(date '+%F %T') [WARN] Could not detect current resolution for $active_output, applying only rotation=$rotation"
        wlr-randr --output "$active_output" --transform "$rotation" 2>&1
        return
    fi

    # Get all refresh rates for current resolution
    modes=$(wlr-randr --output "$active_output" 2>/dev/null \
        | grep -E "^\s*${current_res} px," \
        | grep -Eo '[0-9]+\.[0-9]+')

    if [ -z "$modes" ]; then
        echo "$(date '+%F %T') [WARN] No valid modes found for resolution=$current_res on $active_output, applying only rotation=$rotation"
        wlr-randr --output "$active_output" --transform "$rotation" 2>&1
        return
    fi

    # Pick the highest refresh rate
    best_refresh=$(echo "$modes" | sort -nr | head -n1)
    best_mode="${current_res}@${best_refresh}Hz"

    echo "$(date '+%F %T') [INFO] Applying mode=$best_mode with rotation=$rotation to $active_output"
    wlr-randr --output "$active_output" --mode "$best_mode" --transform "$rotation" 2>&1
}

echo "$(date '+%F %T') [INFO] Starting display restore script"

udevadm monitor --kernel --subsystem-match=drm | while read -r line; do
    if [[ "$line" == *"change"* ]]; then
        # Wait a bit for kernel to update status files
        sleep 0.3
        
        # Skip processing if device is in sleep mode
        if is_sleep_mode; then
            echo "$(date '+%F %T') [DEBUG] Skipping display event processing - device is in sleep mode"
            continue
        fi
        
        # Check if any display is connected
        status=$(cat /sys/class/drm/card*-*/status 2>/dev/null | grep -m1 "^connected$")
        [ -n "$status" ] && status="connected"

        echo "$(date '+%F %T') [DEBUG] Event detected: $line, Display status=$status"
        if [ "$status" = "connected" ]; then
            apply_rotation_and_best_mode
        fi
    fi
done