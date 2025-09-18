#!/usr/bin/env bash
# vmagent-run.sh
set -euo pipefail

WORKDIR="${HOME}/vmagent"
HTTP_HOST="${HTTP_HOST:-0.0.0.0}"   # 0.0.0.0 so you can open UI from your laptop
HTTP_PORT="${HTTP_PORT:-9431}"
REMOTE_URL="${REMOTE_URL:-https://ingest-metrics.feralfile.com/api/v1/write}"
# If set, will add Authorization: Bearer <token>
REMOTE_URL_BEARER_TOKEN="${REMOTE_URL_BEARER_TOKEN:-}"

cd "${WORKDIR}"

echo
echo "Starting vmagent-prod on ${HTTP_HOST}:${HTTP_PORT}"
echo "Scraping config: ${WORKDIR}/scrape.yml"
echo "Remote write:    ${REMOTE_URL}"
[[ -n "${REMOTE_URL_BEARER_TOKEN}" ]] && echo "Bearer token:     [set]"
echo "UI (targets):    http://${HTTP_HOST}:${HTTP_PORT}/targets"
echo "UI (metrics):    http://${HTTP_HOST}:${HTTP_PORT}/metrics"
echo

ARGS=(
  -httpListenAddr="${HTTP_HOST}:${HTTP_PORT}"
  -promscrape.config="${WORKDIR}/scrape.yml"
  -remoteWrite.url="${REMOTE_URL}"
  -remoteWrite.tmpDataPath="${WORKDIR}/queue"
  -remoteWrite.maxDiskUsagePerURL=256MB
)

# Add bearer token only if provided
if [[ -n "${REMOTE_URL_BEARER_TOKEN}" ]]; then
  ARGS+=( -remoteWrite.bearerToken="${REMOTE_URL_BEARER_TOKEN}" )
fi

exec "${WORKDIR}/vmagent-prod" "${ARGS[@]}"