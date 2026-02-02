#!/bin/bash

# FeralFile Time Synchronization Service
# This script handles:
# 1. NTP synchronization when online
# 2. Setting timezone and time manually when offline

set -euo pipefail

# Configuration
STATUS_DIR="/home/feralfile/.state/timesyncd"

# Logging function
log() {
    local level="$1"
    shift
    local message="$*"
    local timestamp
    timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "unknown")
    echo "[$timestamp] [$level] $message"
}

# Create status directory with appropriate permissions
init_dirs() {
    mkdir -p "$STATUS_DIR" 2>/dev/null || true
    chmod -R 755 "$STATUS_DIR" 2>/dev/null || true
}

# Function to check network connectivity
check_network() {
    # Try multiple endpoints for reliability
    ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1 || \
    ping -c 1 -W 2 1.1.1.1 >/dev/null 2>&1 || \
    ping -c 1 -W 2 208.67.222.222 >/dev/null 2>&1
    return $?
}

# Function to check if NTP is synchronized (compatible with older systemd)
is_ntp_synced() {
    # Try modern method first
    if timedatectl show --property=NTPSynchronized --value 2>/dev/null | grep -q "yes"; then
        return 0
    fi
    return 1
}

# Function to check if NTP is active
is_ntp_active() {
    # Try modern method first
    if timedatectl show --property=NTP --value 2>/dev/null | grep -q "yes"; then
        return 0
    fi
    return 1
}

# Function to enable/disable NTP
set_ntp() {
    local enable="$1"
    if ! timedatectl set-ntp "$enable" 2>/dev/null; then
        log "ERROR" "Failed to set NTP to $enable"
        return 1
    fi
    log "INFO" "NTP set to $enable"
    return 0
}

# Function to trigger NTP sync
sync_ntp() {
    log "INFO" "Attempting NTP synchronization..."

    if ! set_ntp true; then
        return 1
    fi

    log "INFO" "Restarting systemd-timesyncd to force immediate sync..."
    if systemctl restart systemd-timesyncd; then
        log "INFO" "Service restarted."
    else
        log "WARN" "Failed to restart service, but continuing..."
    fi

    local timeout=30
    log "INFO" "Waiting up to $timeout seconds for sync..."
    
    for ((i=1; i<=timeout; i++)); do
        if is_ntp_synced; then
            log "INFO" "NTP time synchronized successfully!"
            return 0
        fi
        sleep 1
    done

    log "WARN" "Failed to synchronize time with NTP within $timeout seconds"
    return 1
}

# Function to validate timezone
validate_timezone() {
    local tz="$1"
    if timedatectl list-timezones 2>/dev/null | grep -qx "$tz"; then
        return 0
    fi
    return 1
}

# Function to validate datetime format
validate_datetime() {
    local datetime="$1"
    # Check format: YYYY-MM-DD HH:MM:SS
    if [[ "$datetime" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}\ [0-9]{2}:[0-9]{2}:[0-9]{2}$ ]]; then
        return 0
    fi
    return 1
}

# Function to apply timezone (validate and set)
apply_timezone() {
    local timezone="$1"

    # Validate timezone
    if ! validate_timezone "$timezone"; then
        log "ERROR" "Invalid timezone: $timezone"
        echo "Error: Invalid timezone '$timezone'"
        echo "Use 'timedatectl list-timezones' to see valid timezones"
        return 1
    fi

    # Set timezone
    if timedatectl set-timezone "$timezone" 2>/dev/null; then
        log "INFO" "Timezone set to $timezone"
        echo "Timezone set to $timezone"
        sync
        return 0
    else
        log "ERROR" "Failed to set timezone"
        echo "Error: Failed to set timezone"
        return 1
    fi
}

# Function to disable NTP for manual time setting
disable_ntp_for_manual_time() {
    if is_ntp_active; then
        log "INFO" "Disabling NTP to allow manual time setting"
        if ! set_ntp false; then
            log "ERROR" "Failed to disable NTP"
            echo "Error: Failed to disable NTP. Cannot set time manually."
            return 1
        fi
        # Give systemd a moment to process
        sleep 1
    fi
    return 0
}

# Function to apply system time
apply_system_time() {
    local datetime_str="$1"

    if timedatectl set-time "$datetime_str" 2>/dev/null; then
        log "INFO" "System time set to $datetime_str using timedatectl"
        echo "System time set to $datetime_str"
        return 0
    else
        log "WARN" "timedatectl failed, trying date command"
        # Fallback to date command
        if date -s "$datetime_str" >/dev/null 2>&1; then
            log "INFO" "System time set to $datetime_str using date command"
            echo "System time set to $datetime_str (using date command)"
            return 0
        else
            log "ERROR" "Failed to set system time"
            echo "Error: Failed to set system time"
            return 1
        fi
    fi
}

# Function to set timezone and time
set_time() {
    local timezone=""
    local datetime_str=""

    # Parse arguments
    case $# in
        2)
            # set-time TIMEZONE "YYYY-MM-DD HH:MM:SS"
            timezone="$1"
            datetime_str="$2"
            ;;
        3)
            # set-time TIMEZONE YYYY-MM-DD HH:MM:SS
            timezone="$1"
            datetime_str="$2 $3"
            ;;
        *)
            log "ERROR" "Invalid arguments"
            echo "Usage: $0 set-time TIMEZONE \"YYYY-MM-DD HH:MM:SS\""
            echo "   or: $0 set-time TIMEZONE YYYY-MM-DD HH:MM:SS"
            echo "Example: $0 set-time Asia/Taipei \"2025-01-29 15:30:00\""
            return 1
            ;;
    esac

    log "INFO" "Setting time: timezone=$timezone, datetime=$datetime_str"

    # Validate datetime format
    if ! validate_datetime "$datetime_str"; then
        log "ERROR" "Invalid datetime format: $datetime_str"
        echo "Error: Invalid datetime format '$datetime_str'"
        echo "Expected format: YYYY-MM-DD HH:MM:SS"
        return 1
    fi

    # Set timezone
    apply_timezone "$timezone"

    # Disable NTP before setting time manually
    if ! disable_ntp_for_manual_time; then
        return 1
    fi

    # Apply system time
    apply_system_time "$datetime_str"
    return $?
}

# Function to set timezone only
set_timezone() {
    local timezone="$1"

    if [[ -z "$timezone" ]]; then
        log "ERROR" "Timezone argument is required"
        echo "Usage: $0 set-timezone TIMEZONE"
        echo "Example: $0 set-timezone Asia/Taipei"
        return 1
    fi

    log "INFO" "Setting timezone: $timezone"

    # Apply timezone
    apply_timezone "$timezone"
    return $?
}

# Run the default NTP sync flow
run_sync() {
    if check_network; then
        log "INFO" "Network is available. Attempting NTP sync."
        sync_ntp
    else
        log "INFO" "Network is unavailable. Skipping NTP sync."
    fi
}

# Main entry point
main() {
    init_dirs

    case "${1:-}" in
        "set-timezone")
            shift
            set_timezone "$@"
            exit $?
            ;;
        "set-time")
            shift
            set_time "$@"
            exit $?
            ;;
        "sync")
            run_sync
            exit $?
            ;;
        "help"|"-h"|"--help")
            echo "Usage: $0 [command]"
            echo ""
            echo "Commands:"
            echo "  (none)        Run NTP sync if network available"
            echo "  sync          Same as above"
            echo "  set-timezone  Set timezone and run NTP sync"
            echo "                Usage: set-timezone TIMEZONE"
            echo "  set-time      Set timezone and time manually"
            echo "                Usage: set-time TIMEZONE \"YYYY-MM-DD HH:MM:SS\""
            echo "  help          Show this help message"
            exit 0
            ;;
        "")
            run_sync
            exit 0
            ;;
        *)
            log "ERROR" "Unknown command: $1"
            echo "Unknown command: $1"
            echo "Use '$0 help' for usage information"
            exit 1
            ;;
    esac
}

main "$@"