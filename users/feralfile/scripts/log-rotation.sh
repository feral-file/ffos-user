#!/bin/bash
set -e

# Configuration
MAX_DAYS=7
TODAY=$(date +"%Y-%m-%d")
STATUS_DIR="/var/lib/feral-logrotate"
LOG_CONFIG_FILE="$STATUS_DIR/log_paths.conf"

# Create status directory if it doesn't exist
mkdir -p "$STATUS_DIR"

# Initialize or load config file if it doesn't exist
if [ ! -f "$LOG_CONFIG_FILE" ]; then
    cat > "$LOG_CONFIG_FILE" <<EOL
# Format: LOG_PATH|ROTATED_LOG_DIR
# One entry per line, use | as separator
/home/feralfile/.logs/chrome_debug.log|/home/feralfile/.logs/backup/chromium
/home/feralfile/.logs/connectd.log|/home/feralfile/.logs/backup/connectd
/home/feralfile/.logs/setupd.log|/home/feralfile/.logs/backup/setupd
/home/feralfile/.logs/timesyncd.log|/home/feralfile/.logs/backup/timesyncd
# Add more logs as needed
EOL
fi

# Function to rotate a single log file
rotate_log() {
    LOG_PATH="$1"
    ROTATION_DIR="$2"
    
    # Skip commented lines and empty lines
    [[ "$LOG_PATH" =~ ^#.*$ || -z "$LOG_PATH" ]] && return 0
    
    FILENAME=$(basename "$LOG_PATH")
    FILENAME_WITHOUT_EXT="${FILENAME%.*}"
    EXTENSION="${FILENAME##*.}"
    
    # Create rotation directory if needed
    mkdir -p "$ROTATION_DIR"
    
    ROTATED_LOG="$ROTATION_DIR/${FILENAME_WITHOUT_EXT}_${TODAY}.$EXTENSION"
    
    if [ -f "$LOG_PATH" ] && [ ! -f "$ROTATED_LOG" ]; then
        echo "Rotating log: $LOG_PATH to $ROTATED_LOG"
        cp "$LOG_PATH" "$ROTATED_LOG"
        echo "" > "$LOG_PATH"
        return 0
    else
        echo "Skipping $LOG_PATH (already rotated or doesn't exist)"
        return 1
    fi
}

# Function to clean up old log files
cleanup_old_logs() {
    LOG_PATH="$1"
    
    # Skip commented lines and empty lines
    [[ "$LOG_PATH" =~ ^#.*$ || -z "$LOG_PATH" ]] && return 0
    
    FILENAME=$(basename "$LOG_PATH")
    FILENAME_WITHOUT_EXT="${FILENAME%.*}"
    EXTENSION="${FILENAME##*.}"
    DIRECTORY=$(dirname "$LOG_PATH")
    
    echo "Cleaning up old logs for $FILENAME_WITHOUT_EXT"
    find "$DIRECTORY" -name "${FILENAME_WITHOUT_EXT}_*.$EXTENSION" -mtime +$MAX_DAYS -delete
}

# Main function
main() {
    echo "Starting log rotation on $(date)"
    
    # Process each log file
    while IFS='|' read -r LOG_PATH ROTATION_DIR || [ -n "$LOG_PATH" ]; do
        # Skip commented lines and empty lines
        [[ "$LOG_PATH" =~ ^#.*$ || -z "$LOG_PATH" ]] && continue
        
        # Trim whitespace
        LOG_PATH=$(echo "$LOG_PATH" | xargs)
        ROTATION_DIR=$(echo "$ROTATION_DIR" | xargs)
        
        # Use default rotation directory if not specified
        if [ -z "$ROTATION_DIR" ]; then
            ROTATION_DIR=$(dirname "$LOG_PATH")
        fi
        
        # Rotate log files
        rotate_log "$LOG_PATH" "$ROTATION_DIR"
        
        # Clean up old log files
        cleanup_old_logs "$LOG_PATH"
    done < "$LOG_CONFIG_FILE"
    
    echo "Log rotation completed successfully on $(date)"
}

# Run the main function
main
