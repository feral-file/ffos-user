#!/bin/bash
set -euo pipefail

# Function to detect CPU type
detect_cpu_type() {
  # Check if /proc/cpuinfo exists
  if [[ ! -f /proc/cpuinfo ]]; then
      echo "intel"
      return
  fi

  # Read /proc/cpuinfo and look for vendor_id
  while IFS= read -r line; do
      # Convert line to lowercase for case-insensitive matching
      line=$(echo "$line" | tr '[:upper:]' '[:lower:]')
      
      # Check if line contains vendor_id
      if [[ "$line" =~ vendor_id ]]; then
          if [[ "$line" =~ genuineintel ]]; then
              echo "intel"
              return
          elif [[ "$line" =~ authenticamd ]]; then
              echo "amd"
              return
          fi
      fi
  done < /proc/cpuinfo

  # Default to intel if no match found
  echo "intel"
}

CPU_TYPE=$(detect_cpu_type)
SCRAPE_FILE="/home/feralfile/vmagent/scrape.yml"
QUEUE_FILE="/home/feralfile/.state/vmagent_queue"
CONFIG_FILE="/home/feralfile/ff1-config.json"
HTTP_HOST="0.0.0.0"   # 0.0.0.0 so you can open UI from your laptop
HTTP_PORT="9431"

# Check if the JSON config file exists
if [ ! -f "$CONFIG_FILE" ]; then
    echo "Error: Configuration file $CONFIG_FILE not found!"
    exit 1
fi

# Read BRANCH and VERSION from ff1-config.json
BRANCH=$(jq -r '.branch' "$CONFIG_FILE" 2>/dev/null)
VERSION=$(jq -r '.version' "$CONFIG_FILE" 2>/dev/null)

# Check if BRANCH and VERSION are non-empty
if [ -z "$BRANCH" ]; then
    echo "Error: Failed to read 'branch' from $CONFIG_FILE or it is empty!"
    exit 1
fi
if [ -z "$VERSION" ]; then
    echo "Error: Failed to read 'version' from $CONFIG_FILE or it is empty!"
    exit 1
fi

# Read FF_DEVICE_ID from /etc/hostname
if [ ! -f "/etc/hostname" ]; then
    echo "Error: /etc/hostname file not found!"
    exit 1
fi

device_id=$(cat /etc/hostname)
if [ -z "$device_id" ]; then
    echo "Error: FF_DEVICE_ID read from /etc/hostname is empty!"
    exit 1
fi

# Sanitize variables by escaping special characters for sed
device_id_esc=$(printf '%s' "$device_id" | sed 's/[\/&]/\\&/g')
VERSION_esc=$(printf '%s' "$VERSION" | sed 's/[\/&]/\\&/g')
BRANCH_esc=$(printf '%s' "$BRANCH" | sed 's/[\/&]/\\&/g')

mkdir -p /home/feralfile/.state

# Read values from ff1-config.json
REMOTE_URL=$(jq -r '.vmagent_remote_url // "https://ingest-metrics.feralfile.com/api/v1/write"' "${CONFIG_FILE}")
REMOTE_URL_BEARER_TOKEN=$(jq -r '.vmagent_remote_bearer_token // ""' "${CONFIG_FILE}")

echo
echo "Starting vmagent-prod on ${HTTP_HOST}:${HTTP_PORT}"
echo "Scraping config: ${SCRAPE_FILE}"
echo "Remote write:    ${REMOTE_URL}"
echo "UI (targets):    http://${HTTP_HOST}:${HTTP_PORT}/targets"
echo "UI (metrics):    http://${HTTP_HOST}:${HTTP_PORT}/metrics"
[[ -n "${REMOTE_URL_BEARER_TOKEN}" ]] && echo "Bearer token:     [set]"
echo

ARGS=(
  -httpListenAddr="${HTTP_HOST}:${HTTP_PORT}"
  -promscrape.config="${SCRAPE_FILE}"
  -remoteWrite.url="${REMOTE_URL}"
  -remoteWrite.tmpDataPath="${QUEUE_FILE}"
  -remoteWrite.maxDiskUsagePerURL=256MB
  -remoteWrite.label="instance=${device_id_esc}"
  -remoteWrite.label="version=${VERSION_esc}"
  -remoteWrite.label="branch=${BRANCH_esc}"
  -remoteWrite.label="cpu=${CPU_TYPE}"
)

# Add bearer token only if provided
if [[ -n "${REMOTE_URL_BEARER_TOKEN}" ]]; then
  ARGS+=( -remoteWrite.bearerToken="${REMOTE_URL_BEARER_TOKEN}" )
fi

exec vmagent-prod "${ARGS[@]}"