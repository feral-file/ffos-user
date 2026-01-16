#!/bin/bash
#
# Screen Power Management Script
# Controls display and monitor power state
#
# Usage: screen-power.sh [sleep|wake]
#

set -e

ACTION="${1:-}"

case "$ACTION" in
    sleep)
        echo "$(date '+%F %T') [INFO] Turning off screen with ddcutil..."
        # Samsung Frame TVs often have issues with DDC/CI. 
        # We try to set the power state, but also use wlr-randr as a fallback.
        # --noscantable helps avoid "EDID has changed" errors by not scanning all buses.
        sudo ddcutil --noverify --noscantable setvcp D6 02 || echo "$(date '+%F %T') [WARN] ddcutil sleep failed"
        
        echo "$(date '+%F %T') [INFO] Turning off display signal with wlr-randr..."
        wlr-randr --output HDMI-A-1 --off
        echo "$(date '+%F %T') [INFO] Screen sleep complete"
        ;;
    
    wake)
        echo "$(date '+%F %T') [INFO] Turning on display with wlr-randr..."
        wlr-randr --output HDMI-A-1 --on
        
        # Wait for display to initialize DDC/CI interface
        # The I2C bus and monitor need time to be ready for communication
        # Samsung TVs often need more time to wake up the DDC interface
        echo "$(date '+%F %T') [INFO] Waiting for display to initialize..."
        sleep 3
        
        echo "$(date '+%F %T') [INFO] Turning on screen with ddcutil..."
        # Retry with increased delay and --noscantable
        if ! sudo ddcutil --noverify --noscantable setvcp D6 01 2>/dev/null; then
            echo "$(date '+%F %T') [WARN] First ddcutil attempt failed, retrying after additional delay..."
            sleep 2
            sudo ddcutil --noverify --noscantable --retry 3 setvcp D6 01 || true
        fi
        echo "$(date '+%F %T') [INFO] Screen wake complete"
        ;;
    
    *)
        echo "Usage: $0 {sleep|wake}"
        exit 1
        ;;
esac

exit 0
