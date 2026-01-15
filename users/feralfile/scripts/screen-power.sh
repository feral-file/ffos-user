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
        sudo ddcutil --noverify setvcp D6 02
        echo "$(date '+%F %T') [INFO] Screen sleep complete"
        ;;
    
    wake)
        echo "$(date '+%F %T') [INFO] Turning on display with wlr-randr..."
        wlr-randr --output HDMI-A-1 --on
        
        # Wait for display to initialize DDC/CI interface
        # The I2C bus and monitor need time to be ready for communication
        echo "$(date '+%F %T') [INFO] Waiting for display to initialize..."
        sleep 1.5
        
        echo "$(date '+%F %T') [INFO] Turning on screen with ddcutil..."
        # Retry once if first attempt fails (common on first wake)
        if ! sudo ddcutil --noverify setvcp D6 01 2>/dev/null; then
            echo "$(date '+%F %T') [WARN] First ddcutil attempt failed, retrying after delay..."
            sleep 2
            sudo ddcutil --noverify setvcp D6 01
        fi
        echo "$(date '+%F %T') [INFO] Screen wake complete"
        ;;
    
    *)
        echo "Usage: $0 {sleep|wake}"
        exit 1
        ;;
esac

exit 0
