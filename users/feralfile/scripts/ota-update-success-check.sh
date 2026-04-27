#!/bin/bash
set -uo pipefail

# List of services to check
SERVICES=(
  "feral-player.service"
  "feral-setupd.service"
  "feral-controld.service"
  "feral-sys-monitord.service"
  "chromium-kiosk.service"
)

# Timeout settings
TIMEOUT=300  # 5 minutes in seconds
INTERVAL=10  # Check interval in seconds
MAX_ATTEMPTS=$((TIMEOUT / INTERVAL))
CONFIRMATIONS_NEEDED=3
CONFIRM_COUNT=0

# Metric endpoint
VMAGENT_IMPORT_API="http://0.0.0.0:9431/api/v1/import/prometheus"
FILE_TO_DELETE="/etc/FF_OS_OTA_AUTO_TEST"

CONFIG_FILE="/home/feralfile/ff1-config.json"
VERSION=$(jq -r '.version' "$CONFIG_FILE" 2>/dev/null)

# Trap: always delete the flag file on exit
cleanup() {
  if [ -f "$FILE_TO_DELETE" ]; then
    sudo rm -f "$FILE_TO_DELETE" && echo "Deleted $FILE_TO_DELETE"
  fi
}
trap cleanup EXIT

# Function to check if all services are actively running
check_services() {
  for service in "${SERVICES[@]}"; do
    if ! systemctl --user is-active "$service"; then
      echo "Service $service is not active"
      return 1
    fi
  done
  echo "All services are active"
  return 0
}

# Main loop
attempt=0
while [ $attempt -lt $MAX_ATTEMPTS ]; do
  echo "Attempt $((attempt + 1)) of $MAX_ATTEMPTS: Checking services..."
  
  if check_services; then
    ((CONFIRM_COUNT++))
    echo "Confirmation $CONFIRM_COUNT of $CONFIRMATIONS_NEEDED"
    
    if [ $CONFIRM_COUNT -ge $CONFIRMATIONS_NEEDED ]; then
      echo "All services confirmed active $CONFIRMATIONS_NEEDED times"
      METRIC="ff_ota_update{event=\"success\",target_version=\"$VERSION\"} 1"
      if curl -sS -X POST "$VMAGENT_IMPORT_API" --data-binary "$METRIC" -w "%{http_code}" | grep -q "204"; then
        echo "Successfully sent OTA update done notification to $VMAGENT_IMPORT_API"
      else
        echo "Failed to send OTA update done notification to $VMAGENT_IMPORT_API"
      fi
      exit 0
    fi
  else
    CONFIRM_COUNT=0
  fi
  
  ((attempt++))
  if [ $attempt -lt $MAX_ATTEMPTS ]; then
    sleep $INTERVAL
  fi
done

echo "Failed: Not all services were active after $TIMEOUT seconds"
exit 1
