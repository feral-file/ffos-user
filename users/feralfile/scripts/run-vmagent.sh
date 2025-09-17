#!/bin/bash
set -euo pipefail

SCRAPE_FILE="/home/feralfile/vmagent/scrape.yml"
QUEUE_FILE="/home/feralfile/.state/vmagent_queue"
CONFIG_FILE="/home/feralfile/ff1-config.json"

mkdir -p /home/feralfile/.state

# Read values from ff1-config.json
REMOTE_URL=$(jq -r '.vmagent_remote_url // "https://ingest-metrics.feralfile.com/api/v1/write"' "${CONFIG_FILE}")
REMOTE_URL_BEARER_TOKEN=$(jq -r '.vmagent_remote_bearer_token // ""' "${CONFIG_FILE}")

echo
echo "Scraping config: ${SCRAPE_FILE}"
echo "Remote write:    ${REMOTE_URL}"
[[ -n "${REMOTE_URL_BEARER_TOKEN}" ]] && echo "Bearer token:     [set]"
echo

ARGS=(
  -promscrape.config="${SCRAPE_FILE}"
  -remoteWrite.url="${REMOTE_URL}"
  -remoteWrite.tmpDataPath="${QUEUE_FILE}"
  -remoteWrite.maxDiskUsagePerURL=256MB
)

# Add bearer token only if provided
if [[ -n "${REMOTE_URL_BEARER_TOKEN}" ]]; then
  ARGS+=( -remoteWrite.bearerToken="${REMOTE_URL_BEARER_TOKEN}" )
fi

exec victoria-metrics-prod "${ARGS[@]}"