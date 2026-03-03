#!/bin/bash
set -euo pipefail

CHECK_INTERVAL=30
ANALYTICS_TOGGLE_FILE="/home/feralfile/.state/analytics-toggle-off"

detect_cpu_model() {
    if [[ ! -r /proc/cpuinfo ]]; then
        echo "unknown"
        return
    fi
    model=$(awk -F': ' '/model name/ {print $2; exit}' /proc/cpuinfo | tr -s ' ')
    echo "${model:-unknown}"
}

detect_memory_model() {
    if ! command -v dmidecode >/dev/null 2>&1 || ! sudo dmidecode -t memory >/dev/null 2>&1; then
        echo "unknown"
        return
    fi
    model=$(sudo dmidecode -t memory | grep -m 1 "Part Number" | awk -F': ' '{print $2}' | tr -s ' ')
    echo "${model:-unknown}"
}

CPU_MODEL=$(detect_cpu_model)
MEMORY_MODEL=$(detect_memory_model)
SCRAPE_FILE="/home/feralfile/vmagent/scrape.yml"
DISABLED_SCRAPE_FILE="/home/feralfile/vmagent/scrape-disabled.yml"
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

mkdir -p /home/feralfile/.state "$(dirname "$SCRAPE_FILE")" "/home/feralfile/.config"

# Read values from ff1-config.json
REMOTE_URL=$(jq -r '.vmagent_remote_url // "https://ingest-metrics.feralfile.com/api/v1/write"' "${CONFIG_FILE}")
REMOTE_URL_BEARER_TOKEN=$(jq -r '.vmagent_remote_bearer_token // ""' "${CONFIG_FILE}")

build_args() {
  local scrape_config="$1"
  ARGS=(
    -httpListenAddr="${HTTP_HOST}:${HTTP_PORT}"
    -promscrape.config="${scrape_config}"
    -remoteWrite.url="${REMOTE_URL}"
    -remoteWrite.tmpDataPath="${QUEUE_FILE}"
    -remoteWrite.maxDiskUsagePerURL=256MB
    -remoteWrite.label="job=ff1-device"
    -remoteWrite.label="instance=${device_id_esc}"
    -remoteWrite.label="version=${VERSION_esc}"
    -remoteWrite.label="branch=${BRANCH_esc}"
    -remoteWrite.label="cpu_model=${CPU_MODEL}"
    -remoteWrite.label="memory_model=${MEMORY_MODEL}"
  )

  # Add bearer token only if provided
  if [[ -n "${REMOTE_URL_BEARER_TOKEN}" ]]; then
    ARGS+=( -remoteWrite.bearerToken="${REMOTE_URL_BEARER_TOKEN}" )
  fi
}

start_vmagent() {
  local mode="$1"
  local scrape_config="${SCRAPE_FILE}"
  if [[ "${mode}" == "push-only" ]]; then
    scrape_config="${DISABLED_SCRAPE_FILE}"
  fi

  build_args "${scrape_config}"

  echo
  echo "Starting vmagent-prod on ${HTTP_HOST}:${HTTP_PORT} (${mode})"
  echo "Scraping config: ${scrape_config}"
  echo "Remote write:    ${REMOTE_URL}"
  echo "UI (targets):    http://${HTTP_HOST}:${HTTP_PORT}/targets"
  echo "UI (metrics):    http://${HTTP_HOST}:${HTTP_PORT}/metrics"
  [[ -n "${REMOTE_URL_BEARER_TOKEN}" ]] && echo "Bearer token:     [set]"
  echo "Analytics toggle file: ${ANALYTICS_TOGGLE_FILE}"
  echo

  vmagent-prod "${ARGS[@]}" &
  VMAGENT_PID=$!
  CURRENT_MODE="${mode}"
}

stop_vmagent() {
  if [[ -n "${VMAGENT_PID:-}" ]]; then
    if kill -0 "${VMAGENT_PID}" 2>/dev/null; then
      kill "${VMAGENT_PID}" || true
    fi
    wait "${VMAGENT_PID}" 2>/dev/null || true
  fi
  VMAGENT_PID=""
}

desired_mode() {
  if [[ -f "${ANALYTICS_TOGGLE_FILE}" ]]; then
    echo "push-only"
  else
    echo "scrape"
  fi
}

trap stop_vmagent EXIT

initial_mode=$(desired_mode)
start_vmagent "${initial_mode}"

while true; do
  sleep "${CHECK_INTERVAL}"

  next_mode=$(desired_mode)

  if [[ -z "${VMAGENT_PID:-}" ]] || ! kill -0 "${VMAGENT_PID}" 2>/dev/null; then
    echo "vmagent-prod is not running; restarting in ${next_mode} mode"
    stop_vmagent
    start_vmagent "${next_mode}"
    continue
  fi

  if [[ "${next_mode}" != "${CURRENT_MODE}" ]]; then
    echo "Analytics toggle changed, switching vmagent to ${next_mode} mode"
    stop_vmagent
    start_vmagent "${next_mode}"
  fi
done