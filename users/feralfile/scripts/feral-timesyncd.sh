#!/bin/bash

# FeralFile Time Synchronization Service
# This script handles:
# 1. NTP synchronization when online
# 2. Setting timezone and time manually when offline

# Configuration
STATUS_DIR="/home/feralfile/.state/timesyncd"
STATUS_FILE="$STATUS_DIR/status"
TIMEZONE_FILE="$STATUS_DIR/timezone"

# Create status directory with appropriate permissions
mkdir -p "$STATUS_DIR" 2>/dev/null || true
touch "$STATUS_FILE" 2>/dev/null || true
touch "$TIMEZONE_FILE" 2>/dev/null || true
chmod -R 755 "$STATUS_DIR" 2>/dev/null || true

# Function to check network connectivity
check_network() {
    ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1
    return $?
}

# Function to check if NTP is synchronized
is_ntp_synced() {
    timedatectl show --property=NTPSynchronized | grep -q "yes"
    return $?
}

# Function to trigger NTP sync without restarting service (non-privileged)
sync_ntp() {
    echo "Attempting NTP synchronization..."
    # Instead of restarting service, trigger a time sync using timedatectl
    timedatectl set-ntp true 2>/dev/null
    
    # Wait for NTP sync for up to 30 seconds
    for i in {1..30}; do
        if is_ntp_synced; then
            echo "NTP time synchronized successfully"
            echo "f=true" > "$STATUS_FILE" 2>/dev/null || true
            return 0
        fi
        sleep 1
    done
    
    echo "Failed to synchronize time with NTP"
    echo "ntp_synced=false" > "$STATUS_FILE" 2>/dev/null || true
    return 1
}

# Function to set timezone and time
set_time() {
    # Check if we have a combined date-time format
    if [ -z "$1" ] || [ -z "$2" ]; then
        echo "Usage: set-time TIMEZONE YYYY-MM-DD HH:MM:SS"
        return 1
    fi
    
    timezone="$1"
    
    # Check if we have a combined date-time with space
    if [[ "$2" == *" "* ]] && [ -z "$3" ]; then
        # Split the datetime into date and time parts
        date_str=$(echo "$2" | cut -d ' ' -f1)
        time_str=$(echo "$2" | cut -d ' ' -f2)
    else
        # Use the traditional separate arguments
        date_str="$2"
        time_str="$3"
    fi
    
    # Create the datetime string
    datetime_str="$date_str $time_str"
    
    # Try to set timezone using timedatectl
    if timedatectl set-timezone "$timezone" 2>/dev/null; then
        echo "Timezone set to $timezone"
        echo "timezone_set=true" > "$STATUS_FILE" 2>/dev/null || true
    else
        echo "Warning: Could not set timezone. Saving for later sync..."
        echo "$timezone" > "$TIMEZONE_FILE" 2>/dev/null || true
        echo "timezone_set=false" > "$STATUS_FILE" 2>/dev/null || true
    fi
    
    # Try timedatectl first
    if timedatectl set-time "$datetime_str" 2>/dev/null; then
        echo "System time set to $datetime_str using timedatectl"
        echo "manual_time_set=true" > "$STATUS_FILE" 2>/dev/null || true
        return 0
    fi
    
    # Fallback to date command if timedatectl fails
    if date -s "$datetime_str" 2>/dev/null; then
        echo "System time set to $datetime_str using date command"
        echo "manual_time_set=true" > "$STATUS_FILE" 2>/dev/null || true
        return 0
    fi
    
    echo "Failed to set system time using both timedatectl and date command"
    return 1
}

# Function to apply saved timezone if network is available
apply_saved_timezone() {
    if [ -f "$TIMEZONE_FILE" ]; then
        saved_timezone=$(cat "$TIMEZONE_FILE")
        if timedatectl set-timezone "$saved_timezone" 2>/dev/null; then
            echo "Successfully applied saved timezone: $saved_timezone"
            echo "timezone_set=true" > "$STATUS_FILE" 2>/dev/null || true
            rm -f "$TIMEZONE_FILE" 2>/dev/null || true
            return 0
        fi
    fi
    return 1
}

# Run NTP sync if network is available
if check_network; then
    echo "Network is available. Attempting NTP sync."
    # Try to apply any saved timezone first
    apply_saved_timezone
    sync_ntp
else
    echo "Network is unavailable. Skipping NTP sync."
    echo "ntp_synced=false" > "$STATUS_FILE" 2>/dev/null || true
fi

# Handle service commands
case "$1" in
    "set-time")
        set_time "$2" "$3" "$4"
        exit $?
        ;;
    *)
        # Default behavior - already ran the NTP sync check above
        exit 0
        ;;
esac