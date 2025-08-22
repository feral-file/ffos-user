#!/bin/bash
set -euo pipefail

MAX_DAYS=7
TODAY=$(date +"%Y-%m-%d")
LOG_CONFIG_FILE="/home/feralfile/log_paths.conf"

if [ ! -f "$LOG_CONFIG_FILE" ]; then
  cat > "$LOG_CONFIG_FILE" <<'EOL'
# Format: LOG_PATH|ROTATED_LOG_DIR
/home/feralfile/.logs/chromium.log|/home/feralfile/.logs/backup/chromium
/home/feralfile/.logs/connectd.log|/home/feralfile/.logs/backup/connectd
/home/feralfile/.logs/setupd.log|/home/feralfile/.logs/backup/setupd
/home/feralfile/.logs/timesyncd.log|/home/feralfile/.logs/backup/timesyncd
/home/feralfile/.logs/sys-monitord.log|/home/feralfile/.logs/backup/sys-monitord
/home/feralfile/.logs/watchdog.log|/home/feralfile/.logs/backup/watchdog
/home/feralfile/.logs/app-monitord.log|/home/feralfile/.logs/backup/app-monitord
/home/feralfile/.logs/log-rotation.log|/home/feralfile/.logs/backup/log-rotation
EOL
fi

split_name() {
  local fn="$1"
  if [[ "$fn" == *.* ]]; then
    echo "${fn%.*}|.${fn##*.}"     # base|.ext
  else
    echo "$fn|"
  fi
}

rotate_log() {
  local LOG_PATH="$1" ROTATION_DIR="$2"

  [[ -z "$LOG_PATH" || "$LOG_PATH" =~ ^[[:space:]]*# ]] && return 0

  local FILENAME BASE EXT; FILENAME=$(basename "$LOG_PATH")
  IFS='|' read -r BASE EXT < <(split_name "$FILENAME")

  mkdir -p "$ROTATION_DIR"
  local ROTATED_LOG="$ROTATION_DIR/${BASE}_${TODAY}${EXT}"

  if [ -f "$LOG_PATH" ] && [ ! -f "$ROTATED_LOG" ]; then
    echo "Rotating: $LOG_PATH -> $ROTATED_LOG"
    cp --preserve=mode,timestamps "$LOG_PATH" "$ROTATED_LOG"
    truncate -s 0 "$LOG_PATH"
  else
    echo "Skip: $LOG_PATH (already exists or not found)"
  fi
  return 0
}

cleanup_old_logs() {
  local LOG_PATH="$1" ROTATION_DIR="$2"

  [[ -z "$LOG_PATH" || "$LOG_PATH" =~ ^[[:space:]]*# ]] && return 0

  local FILENAME BASE EXT; FILENAME=$(basename "$LOG_PATH")
  IFS='|' read -r BASE EXT < <(split_name "$FILENAME")

  echo "Cleaning old logs in $ROTATION_DIR for $BASE (>$MAX_DAYS days)"
  find "$ROTATION_DIR" -type f -name "${BASE}_*${EXT}" -mtime +"$MAX_DAYS" -delete
  return 0
}

main() {
  echo "Log rotation start: $(date)"

  while IFS='|' read -r LOG_PATH ROTATION_DIR || [ -n "$LOG_PATH" ]; do
    LOG_PATH=$(echo "$LOG_PATH" | xargs)
    ROTATION_DIR=$(echo "${ROTATION_DIR:-}" | xargs)

    [[ -z "$LOG_PATH" || "$LOG_PATH" =~ ^# ]] && continue
    [[ -z "$ROTATION_DIR" ]] && ROTATION_DIR=$(dirname "$LOG_PATH")

    rotate_log "$LOG_PATH" "$ROTATION_DIR"
    cleanup_old_logs "$LOG_PATH" "$ROTATION_DIR"
  done < "$LOG_CONFIG_FILE"

  echo "Log rotation done: $(date)"
}

main