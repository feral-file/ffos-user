#!/usr/bin/env bash
################################################################################
# Soak Test Script
#
# Iterates through a list of local HTML files, running a 5-hour test on each.
# Uses CDP to switch URLs between tests.
# Logs average metrics and failures to a JSON file.
# Handles CPU overheat and Chromium unresponsiveness by restarting Chromium.
################################################################################
set -euo pipefail

stop_requested=0
handle_signal() {
  stop_requested=1
  log_warn "⏹  Signal received, cleaning up..."
  kill_chromium
}
trap handle_signal INT TERM HUP

# --- Configuration ------------------------------------------------------------
# !!! IMPORTANT: UPDATE THESE PATHS !!!
FILES_TO_TEST=(
  "file:///home/soaktest/files/36-point/index.html?edition_number=0&artwork_number=1&blockchain=bitmark#02_hex_hole_open"
  "file:///home/soaktest/files/e-volved-formula-23/index.html"
  "file:///home/soaktest/files/TransparentGrit/index.html"
  "file:///home/soaktest/files/uneasy-dream/index.html"
  "file:///home/soaktest/files/autoplay_10bits.html"
  "file:///home/soaktest/files/autoplay_8bits.html"
)

RESULTS_JSON_FILE="soak_results.json"
SUMMARY_SCRIPT_PATH="/home/soaktest/scripts/summary.py" # Path to your Python summary script

FILE_TARGET_DURATION_SECONDS=$((5 * 60 * 60)) # 5 hours per file
#FILE_TARGET_DURATION_SECONDS=$((3 * 60)) # For testing: 3 minutes per file

LOOP_SAMPLING_DELAY_SECONDS=5       # Interval for collecting metrics
CHROMIUM_START_DELAY_SECONDS=5     # Time to wait for Chromium to launch and CDP to be ready
CHROMIUM_CMD="chromium"             # Command to launch Chromium

DEBUG_PORT=9222
CPU_TEMP_THRESHOLD=90               # Celsius, threshold for CPU overheat restart
MAX_CONSECUTIVE_CDP_FAILURES=5      # Number of failed CDP health checks before restarting Chromium
MAX_TIME_WITHOUT_CDP_OK_SECONDS=60  # Max seconds without a good CDP response before restart
MAX_POWER=30                        # Max power in Watts for power meter percentage calculation

# --- Colour Helpers -----------------------------------------------------------
RED=$'\e[0;31m'
YEL=$'\e[1;33m'
GRN=$'\e[0;32m'
BLU=$'\e[0;34m'
NC=$'\e[0m'

pct() {
  { (($(bc -l <<<"$1 > 80"))); printf "${RED}%.1f%%%s" "$1" "$NC"; } ||
  { (($(bc -l <<<"$1 > 50"))); printf "${YEL}%.1f%%%s" "$1" "$NC"; } ||
    printf "${GRN}%.1f%%%s" "$1" "$NC"
}
tmp() {
  { (($(bc -l <<<"$1 > 75"))); printf "${RED}%.1f°C%s" "$1" "$NC"; } ||
  { (($(bc -l <<<"$1 > 60"))); printf "${YEL}%.1f°C%s" "$1" "$NC"; } ||
    printf "${GRN}%.1f°C%s" "$1" "$NC"
}

log_info() { echo -e "${BLU}[INFO] $(date '+%Y-%m-%d %H:%M:%S') $1${NC}"; }
log_warn() { echo -e "${YEL}[WARN] $(date '+%Y-%m-%d %H:%M:%S') $1${NC}"; }
log_error() { echo -e "${RED}[ERROR] $(date '+%Y-%m-%d %H:%M:%S') $1${NC}"; }

# --- System Info --------------------------------------------------------------
NUM_CORES=$(nproc)
MEM_TOTAL_KB=$(awk '/MemTotal/ {print $2}' /proc/meminfo)
MEM_TOTAL_MB=$((MEM_TOTAL_KB / 1024))

# --- Process & CDP Helpers ----------------------------------------------------
get_tree_pids() {
  local q=("$1") a=("$1") kids
  while ((${#q[@]})); do
    kids=($(pgrep -P "${q[0]}" 2>/dev/null)) && a+=("${kids[@]}") # Added 2>/dev/null
    q=("${q[@]:1}" "${kids[@]}")
  done
  printf '%s ' "${a[@]}"
}

ws_url() { # first "page" target's WS URL
  curl -s http://127.0.0.1:$DEBUG_PORT/json |
    jq -r '[ .[] | select(.type=="page") ][0].webSocketDebuggerUrl // empty'
}

change_url_cdp() {
  local new_url="$1"
  local ws
  ws=$(ws_url)
  if [[ -z "$ws" || "$ws" == "null" ]]; then
    log_error "CDP: Could not get WebSocket URL to change URL."
    return 1
  fi
  log_info "CDP: Navigating to $new_url"
  printf '{"id":100,"method":"Page.navigate","params":{"url":"%s"}}\n' "$new_url" |
    websocat -n1 "$ws" >/dev/null
  sleep 5 # Give page time to start loading
  return 0
}

# --- Metric Collection Functions (largely from original script) ---------------
get_cpu_usages() {
  get_total_idle() { awk '/^cpu / { print $5 }' /proc/stat; }
  get_total_sum() { awk '/^cpu / { sum=0; for (i=2; i<=5; i++) sum+=$i; print sum }' /proc/stat; }
  get_pids_time() {
    local sum_pids_time=0
    if [ -n "$C_PIDS" ]; then # Check if C_PIDS is set
      for pid in $C_PIDS; do
        if [ -r "/proc/$pid/stat" ]; then
          local t
          t=$(awk '{print $14 + $15}' "/proc/$pid/stat")
          sum_pids_time=$((sum_pids_time + t ))
        fi
      done
    fi
    echo "$sum_pids_time"
  }

  local total1 idle1 pids1 total2 idle2 pids2
  total1=$(get_total_sum)
  idle1=$(get_total_idle)
  pids1=$(get_pids_time)
  sleep 1
  total2=$(get_total_sum)
  idle2=$(get_total_idle)
  pids2=$(get_pids_time)

  local total_delta idle_delta pids_delta sys_pct pid_pct
  total_delta=$((total2 - total1))
  idle_delta=$((idle2 - idle1))
  pids_delta=$((pids2 - pids1))

  if [ "$total_delta" -gt 0 ]; then
    sys_pct=$(awk "BEGIN { printf \"%.1f\", ($total_delta - $idle_delta) / $total_delta * 100 }")
    pid_pct=$(awk "BEGIN { printf \"%.1f\", $pids_delta / $total_delta * 100 }")
  else
    sys_pct="0.0"; pid_pct="0.0"
  fi
  echo "$sys_pct $pid_pct"
}

get_cpu_freq() {
  local sum=0 cnt=0
  for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_cur_freq; do
    [[ -r $f ]] && sum=$((sum + $(<"$f"))) && ((cnt++))
  done
  ((cnt)) && echo $((sum / cnt / 1000)) || echo 0
}

get_cpu_temp() {
  sensors -u 2>/dev/null | awk '
    /^Package id 0:/ { in_pkg=1; next }
    /^$/ { in_pkg=0 }
    in_pkg && /temp1_input:/ {printf "%.1f",$2; exit}' || echo "0.0" # Fallback
}

chrome_mem() {
  local kb=0
  if [ -n "$C_PIDS" ]; then
    for p in $C_PIDS; do
      if [[ -r /proc/$p/smaps ]]; then
        kb=$((kb + $(awk '/^Pss:/ {s+=$2} END{print s}' "/proc/$p/smaps" 2>/dev/null || echo 0)))
      elif [[ -r /proc/$p/status ]]; then # Fallback for smaps not available
        kb=$((kb + $(awk '/VmRSS:/ {print $2}' "/proc/$p/status" 2>/dev/null || echo 0)))
      fi
    done
  fi
  echo $((kb / 1024)) # MB
}

gpu_stats() {
  local raw json pids_j busy freq
  raw=$(timeout 1s sudo intel_gpu_top -J -s 1000 -o - 2>/dev/null)
  json=$(sed '1s/^[[:space:]]*\[//' <<<"$raw")
  [[ -z $json ]] && { echo "0 0"; return; }

  if [ -n "$C_PIDS" ]; then # Ensure C_PIDS is not empty
    pids_j=$(printf '%s\n' $C_PIDS | jq -R . | jq -cs . 2>/dev/null)
  else
    pids_j="[]" # Empty JSON array if C_PIDS is empty
  fi
  
  busy=$(jq -r --argjson p "$pids_j" '
    reduce .clients?[]? as $c (0;
      if $p|index($c.pid) then
        . + ($c["engine-classes"]["Render/3D"].busy|tonumber)
      else . end ) // 0
    ' <<<"$json" 2>/dev/null)
  busy=${busy:-0} # Default to 0 if null

  [[ "$busy" == "0" || "$busy" == "0.0" ]] && busy=$(jq -r '.engines."Render/3D".busy // 0' <<<"$json" 2>/dev/null)
  freq=$(jq -r '.frequency.actual // 0' <<<"$json" | awk '{printf "%d",$1}')
  echo "${busy:-0} ${freq:-0}"
}

fps_from_rAF() {
  command -v websocat &>/dev/null || { echo 0; return; }
  local ws js fps_val
  ws=$(ws_url)
  [[ -z $ws || $ws == null ]] && { echo 0; return; }
  js='(async()=>{let c=0,s=performance.now();function f(ts){c++; if(ts-s<1000) requestAnimationFrame(f);}requestAnimationFrame(f);await new Promise(r=>setTimeout(r,1050));return Math.round(c*1000/(performance.now()-s));})();'
  fps_val=$(printf '{"id":3,"method":"Runtime.evaluate","params":{"expression":"%s","awaitPromise":true,"returnByValue":true}}\n' "$js" |
    websocat -n1 --text "$ws" 2>/dev/null |
    jq -r '.result.result.value // 0')
  echo "${fps_val:-0}"
}

# RAPL Energy helpers
find_zone() {
  local want=$1 base=/sys/class/powercap/intel-rapl:0
  for n in "$base"/*/name; do
    [[ -f "$n" && $(<"$n") == "$want" ]] && { echo "${n%/*}"; return 0; }
  done
  # Try intel-rapl:X:X format too
  for n in /sys/class/powercap/intel-rapl:?:?/name; do
    [[ -f "$n" && $(<"$n") == "$want" ]] && { echo "${n%/*}"; return 0; }
  done
  return 1
}

PKG_ZONE=/sys/class/powercap/intel-rapl:0 # Default package zone
if [ ! -d "$PKG_ZONE" ]; then # Try to find the correct package zone (e.g. intel-rapl:0, intel-rapl:1)
    PKG_ZONE_BASE=$(find /sys/class/powercap/ -name "intel-rapl:*" -type d -print -quit)
    if [ -n "$PKG_ZONE_BASE" ] && [ -d "$PKG_ZONE_BASE" ]; then
        PKG_ZONE="$PKG_ZONE_BASE"
    else # If still not found, try another common pattern
        PKG_ZONE=$(ls -d /sys/class/powercap/intel-rapl-???? 2>/dev/null | head -n1)
    fi
fi

CORE_ZONE=$(find_zone core)
GPU_ZONE=$(find_zone uncore || find_zone gpu) # uncore for older, gpu for newer

PL1_uW=0; PL2_uW=0; PL1=0; PL2=0; MAX_CORE_ENERGY=0; MAX_GPU_ENERGY=0
declare -g c_energy_prev g_energy_prev prev_watts_ts # Make them global for get_watts

initialize_power_metrics() {
    c_energy_prev=0; g_energy_prev=0; prev_watts_ts=$(date +%s.%N)
    if [ -d "$PKG_ZONE" ] && [ -r "$PKG_ZONE/constraint_0_power_limit_uw" ]; then
        PL1_uW=$(sudo cat "$PKG_ZONE/constraint_0_power_limit_uw" 2>/dev/null || echo 0)
        PL2_uW=$(sudo cat "$PKG_ZONE/constraint_1_power_limit_uw" 2>/dev/null || echo 0)
        PL1=$(awk "BEGIN{printf \"%.2f\", $PL1_uW/1e6}")
        PL2=$(awk "BEGIN{printf \"%.2f\", $PL2_uW/1e6}")
    else
        log_warn "RAPL package zone $PKG_ZONE not found or constraint files unreadable. Power limits (PL1/PL2) set to 0."
    fi

    if [ -n "$CORE_ZONE" ] && [ -r "$CORE_ZONE/max_energy_range_uj" ]; then
        MAX_CORE_ENERGY=$(sudo cat "$CORE_ZONE/max_energy_range_uj" 2>/dev/null || echo 0)
        read -r c_energy_prev < <(sudo cat "$CORE_ZONE/energy_uj" 2>/dev/null || echo 0)
    else
        log_warn "RAPL core zone not found or unreadable. Core energy metrics will be 0."
        CORE_ZONE="" # Mark as unusable
    fi

    if [ -n "$GPU_ZONE" ] && [ -r "$GPU_ZONE/max_energy_range_uj" ]; then
        MAX_GPU_ENERGY=$(sudo cat "$GPU_ZONE/max_energy_range_uj" 2>/dev/null || echo 0)
        read -r g_energy_prev < <(sudo cat "$GPU_ZONE/energy_uj" 2>/dev/null || echo 0)
    else
        log_warn "RAPL GPU/uncore zone not found or unreadable. GPU energy metrics will be 0."
        GPU_ZONE="" # Mark as unusable
    fi
    prev_watts_ts=$(date +%s.%N)
}

get_watts() {
  local curr_ts c_energy_curr g_energy_curr
  curr_ts=$(date +%s.%N)
  
  c_energy_curr=$( [ -n "$CORE_ZONE" ] && sudo cat "$CORE_ZONE/energy_uj" 2>/dev/null || echo "$c_energy_prev" )
  g_energy_curr=$( [ -n "$GPU_ZONE" ] && sudo cat "$GPU_ZONE/energy_uj" 2>/dev/null || echo "$g_energy_prev" )

  ((c_energy_curr < c_energy_prev && MAX_CORE_ENERGY > 0)) && c_energy_curr=$((c_energy_curr + MAX_CORE_ENERGY))
  ((g_energy_curr < g_energy_prev && MAX_GPU_ENERGY > 0)) && g_energy_curr=$((g_energy_curr + MAX_GPU_ENERGY))

  local dt w_core w_gpu pc1 pc2 pg1 pg2
  dt=$(awk "BEGIN{print $curr_ts - $prev_watts_ts}")
  [[ $(bc -l <<< "$dt <= 0") -eq 1 ]] && dt="1.0" # Avoid division by zero if time hasn't passed

  w_core=$(awk "BEGIN{printf \"%.2f\", ($c_energy_curr-$c_energy_prev)/1e6/$dt}")
  w_gpu=$(awk "BEGIN{printf \"%.2f\", ($g_energy_curr-$g_energy_prev)/1e6/$dt}")

  pc1=$(awk "BEGIN{printf \"%.1f\", ($PL1 > 0 ? $w_core/$PL1*100 : 0 )}")
  pc2=$(awk "BEGIN{printf \"%.1f\", ($PL2 > 0 ? $w_core/$PL2*100 : 0 )}")
  pg1=$(awk "BEGIN{printf \"%.1f\", ($PL1 > 0 ? $w_gpu/$PL1*100 : 0 )}")
  pg2=$(awk "BEGIN{printf \"%.1f\", ($PL2 > 0 ? $w_gpu/$PL2*100 : 0 )}")

  # Slide window only if zones are valid
  [[ -n "$CORE_ZONE" && "$MAX_CORE_ENERGY" -gt 0 ]] && c_energy_prev=$((c_energy_curr % MAX_CORE_ENERGY)) || c_energy_prev=$c_energy_curr
  [[ -n "$GPU_ZONE" && "$MAX_GPU_ENERGY" -gt 0 ]] && g_energy_prev=$((g_energy_curr % MAX_GPU_ENERGY)) || g_energy_prev=$g_energy_curr
  prev_watts_ts=$curr_ts

  printf "%6s %6s %6s %6s %6s %6s\n" \
    "${w_core:-0}" "${pc1:-0}" "${pc2:-0}" "${w_gpu:-0}" "${pg1:-0}" "${pg2:-0}"
}

get_drop_pct() {
  command -v websocat &>/dev/null || { echo "0"; return; }
  local ws js payload drop_pct
  ws=$(ws_url)
  [[ -z $ws || $ws == null ]] && { echo "0"; return; }
  js='(async()=>{let count=0,expected=0,ts0=performance.now();const start=performance.now();function f(ts){count++;expected+=Math.max(1,Math.floor((ts-ts0)/16.666));ts0=ts;if(performance.now()-start<1000)requestAnimationFrame(f);}requestAnimationFrame(f);await new Promise(r=>setTimeout(r,1100));const dropped=expected-count;return dropped>0?Math.round(dropped*100/(expected||1)):0;})()'
  payload=$(printf '{"id":10,"method":"Runtime.evaluate","params":{"expression":"%s","awaitPromise":true,"returnByValue":true}}' "$js")
  drop_pct=$(printf '%s' "$payload" | websocat -n1 --text "$ws" 2>/dev/null | jq -r '.result.result.value // 0')
  echo "${drop_pct:-0}"
}

js_heap() {
  command -v websocat &>/dev/null || { printf "0 0 0\n"; return 1; }
  local ws t_heap u_heap t_mb u_mb pct_heap
  ws=$(ws_url)
  [[ -z "$ws" || "$ws" == "null" ]] && { printf "0 0 0\n"; return 1; }
  get_js_value() {
    printf '{"id":4,"method":"Runtime.evaluate","params":{"expression":"%s","returnByValue":true}}\n' "$1" |
      websocat -n1 --text "$ws" 2>/dev/null |
      jq -r '.result.result.value // 0'
  }
  t_heap=$(get_js_value "performance.memory ? performance.memory.totalJSHeapSize : 0")
  u_heap=$(get_js_value "performance.memory ? performance.memory.usedJSHeapSize : 0")
  [[ "$t_heap" == "0" || -z "$t_heap" ]] && { printf "0 0 0\n"; return 1; }
  t_mb=$(awk "BEGIN{printf \"%.2f\", $t_heap/1024/1024}")
  u_mb=$(awk "BEGIN{printf \"%.2f\", $u_heap/1024/1024}")
  pct_heap=$(awk "BEGIN{printf \"%.1f\", ($t_heap > 0 ? $u_heap*100/$t_heap : 0)}")
  printf "%.2f %.2f %.1f" "$t_mb" "$u_mb" "$pct_heap"
}

get_power_meter_readings() {
  local output voltage current_ma current power power_pct
  # Check if sigrok-cli and rdtech device are available
  if command -v sigrok-cli &>/dev/null && [ -e /dev/ttyACM0 ]; then # Adjust /dev/ttyACM0 if needed
    output=$(timeout 2s sigrok-cli -d rdtech-tc:conn=/dev/ttyACM0 --frames 1 2>/dev/null)
    voltage=$(echo "$output" | grep -o "V: [0-9.]* V" | awk '{print $2}')
    current_ma=$(echo "$output" | grep -o "I: [0-9.]* mA" | awk '{print $2}')
  else
    if ! command -v sigrok-cli &>/dev/null; then
        # log_warn "sigrok-cli not found. Power meter readings will be zero." # Too noisy for loop
        : # Do nothing, already warned at start if needed
    elif [ ! -e /dev/ttyACM0 ]; then
        # log_warn "/dev/ttyACM0 not found. Power meter readings will be zero." # Too noisy
        :
    fi
    voltage=0; current_ma=0
  fi

  [[ -z "$voltage" ]] && voltage=0
  [[ -z "$current_ma" ]] && current_ma=0
  current=$(awk "BEGIN {printf \"%.3f\", $current_ma/1000}")
  power=$(awk "BEGIN {printf \"%.2f\", $voltage * $current}")
  power_pct=$(awk "BEGIN {printf \"%.1f\", ($MAX_POWER > 0 ? $power * 100 / $MAX_POWER : 0 )}")
  echo "$voltage $current $power $power_pct"
}

calc_1pct_low_fps_for_file() {
  local fps_array_str="$1" # Pass array as string "val1 val2 val3"
  # Convert string back to array
  local -a fps_values=($fps_array_str)
  local n=${#fps_values[@]}
  if (( n == 0 )); then echo "0.0"; return; fi
  
  # Sort numerically
  local sorted_fps=($(printf "%s\n" "${fps_values[@]}" | sort -n))
  
  local n_low=$(( (n + 99) / 100 )) # Calculate 1% of samples, round up
  (( n_low < 1 && n > 0 )) && n_low=1 # Ensure at least one sample if list is not empty
  (( n == 0 )) && n_low=0 # No samples if list is empty

  local sum_low_fps=0
  for (( i=0; i<n_low; i++ )); do
    sum_low_fps=$(bc -l <<< "$sum_low_fps + ${sorted_fps[i]}")
  done

  if (( n_low > 0 )); then
    awk -v s="$sum_low_fps" -v k="$n_low" 'BEGIN { printf "%.1f", s / k }'
  else
    echo "0.0"
  fi
}

# --- Chromium Management ------------------------------------------------------
ROOT_PID=""
C_PIDS=""

start_chromium() {
  local url_to_load="$1"
  log_info "Starting Chromium with URL: $url_to_load"
  
  # Kill existing instances on the same debug port to prevent conflicts
  # This is aggressive, ensure no other critical chromium uses this port.
  local existing_pid
  existing_pid=$(lsof -ti :$DEBUG_PORT 2>/dev/null)
  if [ -n "$existing_pid" ]; then
    log_warn "Found existing process on port $DEBUG_PORT (PID: $existing_pid). Terminating it."
    kill -9 "$existing_pid" &>/dev/null
    sleep 2 # Give it a moment to die
  fi
  
  "$CHROMIUM_CMD" "$url_to_load" \
    --remote-debugging-port=$DEBUG_PORT \
    --kiosk \
    --no-first-run \
    --disable-sync \
    --disable-translate \
    --disable-infobars \
    --disable-features=TranslateUI \
    --disable-popup-blocking \
    --autoplay-policy=no-user-gesture-required \
    --allow-file-access-from-files \
    --enable-features=AcceleratedVideoDecodeLinuxGL,AcceleratedVideoDecodeLinuxZeroCopyGL \
    2>/tmp/chromium_stderr.log & # Log stderr to a file for debugging
  ROOT_PID=$!
  sleep "$CHROMIUM_START_DELAY_SECONDS" # Allow renderer + CDP to become active
  
  C_PIDS=$(get_tree_pids "$ROOT_PID")
  if [ -z "$C_PIDS" ]; then
    log_error "Chromium started (PID $ROOT_PID) but could not find any child processes. Check /tmp/chromium_stderr.log."
    # This is a critical failure for starting chromium
    return 1 
  fi
  log_info "Chromium started. Root PID: $ROOT_PID. Monitored PIDs: $C_PIDS"
  return 0
}

kill_chromium() {
  if [ -n "$ROOT_PID" ] && kill -0 "$ROOT_PID" 2>/dev/null; then
    log_info "Stopping Chromium (PID $ROOT_PID)..."
    kill -TERM "$ROOT_PID" &>/dev/null || true
    sleep 2
    kill -0 "$ROOT_PID" 2>/dev/null && kill -KILL "$ROOT_PID" &>/dev/null || true
    wait "$ROOT_PID" 2>/dev/null || true
  fi

  if [ -n "$C_PIDS" ]; then
    for pid_to_kill in $C_PIDS; do
      kill -0 "$pid_to_kill" 2>/dev/null && kill -KILL "$pid_to_kill" &>/dev/null || true
    done
  fi
  lsof -ti :$DEBUG_PORT 2>/dev/null | xargs -r kill -9 &>/dev/null || true

  ROOT_PID=""
  C_PIDS=""
  log_info "Chromium stopped."
}

# --- JSON Logging -------------------------------------------------------------
initialize_results_json() {
  echo "[]" > "$RESULTS_JSON_FILE"
  log_info "Initialized results file: $RESULTS_JSON_FILE"
}

log_file_results_to_json() {
  local file_url="$1"
  local target_dur="$2"
  local actual_dur="$3"
  local samples="$4"
  local failures_cpu="$5"
  local failures_chrome="$6"
  # The 7th argument is the JSON string of metrics
  local metrics_json_str="$7"

  local result_json
  result_json=$(jq -n \
    --arg file "$file_url" \
    --argjson target_s "$target_dur" \
    --argjson actual_s "$actual_dur" \
    --argjson samples_n "$samples" \
    --argjson fail_cpu "$failures_cpu" \
    --argjson fail_chrome "$failures_chrome" \
    --argjson metrics_obj "$metrics_json_str" \
    '{
      file_url: $file,
      target_duration_seconds: $target_s,
      actual_duration_seconds: $actual_s,
      samples_collected: $samples_n,
      failures: {
        cpu_overheat: $fail_cpu,
        chromium_unresponsive: $fail_chrome
      },
      metrics: $metrics_obj
    }')

  # Append to the JSON array
  if [[ -f "$RESULTS_JSON_FILE" ]]; then
    jq ". + [$result_json]" "$RESULTS_JSON_FILE" > "${RESULTS_JSON_FILE}.tmp" && mv "${RESULTS_JSON_FILE}.tmp" "$RESULTS_JSON_FILE"
  else
    echo "[$result_json]" > "$RESULTS_JSON_FILE"
  fi
  log_info "Results for $file_url appended to $RESULTS_JSON_FILE"
}


# --- Main Test Logic ----------------------------------------------------------
main() {
  if ! command -v jq &>/dev/null; then log_error "jq is not installed. Exiting."; exit 1; fi
  if ! command -v "$CHROMIUM_CMD" &>/dev/null; then log_error "$CHROMIUM_CMD is not installed or not in PATH. Exiting."; exit 1; fi
  if ! command -v websocat &>/dev/null; then log_error "websocat is not installed. Exiting."; exit 1; fi
  if ! command -v sigrok-cli &>/dev/null; then
    log_warn "sigrok-cli not found. External power meter readings will be zero."
  elif [ ! -e /dev/ttyACM0 ]; then # Adjust device path if needed
    log_warn "/dev/ttyACM0 not found (for rdtech power meter). External power meter readings will be zero."
  fi

  initialize_results_json
  initialize_power_metrics # Initialize RAPL vars

  # Start Chromium once, initially with about:blank or first file
  local initial_url="about:blank"
  if ! start_chromium "$initial_url"; then
    log_error "Initial Chromium launch failed. Exiting."
    exit 1
  fi
  
  local overall_start_time
  overall_start_time=$(date +%s)

  for current_file_url in "${FILES_TO_TEST[@]}"; do
    if (( stop_requested == 1 )); then break; fi
    local current_file_basename
    current_file_basename=$(basename "$current_file_url")
    log_info "--- Starting Test for: $current_file_basename ---"

    # Navigate to the current file if not already there (e.g. not the first file)
    # The first file is already loaded by start_chromium if it was the initial_url
    if [[ "$current_file_url" != "$initial_url" || "$current_file_url" == "about:blank" ]]; then
        # If initial_url was about:blank, or this is not the very first file of the list
        # which was already loaded by start_chromium.
        if ! change_url_cdp "$current_file_url"; then
            log_error "Failed to navigate to $current_file_url via CDP. Skipping this file."
            # Try to restart chromium and then navigate for robustness
            kill_chromium
            if ! start_chromium "$current_file_url"; then
                log_error "Restart and navigate to $current_file_url failed. Really skipping."
                continue # Skip to next file
            fi
        fi
        initial_url="" # Clear initial_url to ensure navigation for subsequent files
    else
        # This was the first file, already loaded. Clear initial_url.
        initial_url="" 
    fi


    # Per-file accumulators and stats
    declare -A sum_metrics_file
    local metric_keys=("cu" "su" "cf" "ct" "gu" "gf" "cm" "cmp" "sm" "smp" "fps" "ju" "jt" "jpct" "cw" "cwpct1" "cwpct2" "gw" "gwpct1" "gwpct2" "df" "v" "i" "w" "wpct")
    for k in "${metric_keys[@]}"; do sum_metrics_file[$k]="0"; done
    
    local samples_count_file=0
    declare -a FPS_LIST_FILE=() # Use local array for FPS list per file
    local failures_cpu_overheat_file=0
    local failures_chromium_unresponsive_file=0

    local file_run_start_time last_cdp_ok_time consecutive_cdp_failures_file
    file_run_start_time=$(date +%s)
    last_cdp_ok_time=$(date +%s) # Assume CDP is OK at start of file test
    consecutive_cdp_failures_file=0

    # Inner loop for the current file's duration
    while (( stop_requested == 0 )); do
      local current_loop_time elapsed_this_file
      current_loop_time=$(date +%s)
      elapsed_this_file=$((current_loop_time - file_run_start_time))

      if ((elapsed_this_file >= FILE_TARGET_DURATION_SECONDS)); then
        log_info "Target duration ($FILE_TARGET_DURATION_SECONDS s) for $current_file_basename reached."
        break
      fi

      # --- Health Checks ---
      if ! kill -0 "$ROOT_PID" 2>/dev/null; then
        log_warn "Chromium main process (PID $ROOT_PID) not found! Restarting Chromium for $current_file_basename."
        failures_chromium_unresponsive_file=$((failures_chromium_unresponsive_file + 1))
        kill_chromium # Ensure full cleanup
        if ! start_chromium "$current_file_url"; then # Restart with CURRENT file
            log_error "Failed to restart Chromium for $current_file_url after crash. Ending test for this file."
            break # Exit inner loop for this file
        fi
        last_cdp_ok_time=$(date +%s)
        consecutive_cdp_failures_file=0
        if (( stop_requested == 1 )); then break; fi
        sleep "$LOOP_SAMPLING_DELAY_SECONDS" # Wait before next cycle
        continue
      fi
      
      C_PIDS=$(get_tree_pids "$ROOT_PID") # Keep C_PIDS updated
      if [ -z "$C_PIDS" ]; then
          log_warn "Chromium root process $ROOT_PID is alive, but no child PIDs found. This might indicate an issue."
          # This alone might not trigger restart unless CDP also fails
      fi

      local cdp_version_check
      cdp_version_check=$(timeout 5s curl -s http://127.0.0.1:$DEBUG_PORT/json/version || echo "cdp_timeout")
      
      if [[ "$cdp_version_check" == "cdp_timeout" || -z "$cdp_version_check" ]]; then
        consecutive_cdp_failures_file=$((consecutive_cdp_failures_file + 1))
        log_warn "CDP health check failed ($consecutive_cdp_failures_file/$MAX_CONSECUTIVE_CDP_FAILURES times)."
      else
        consecutive_cdp_failures_file=0
        last_cdp_ok_time=$(date +%s)
      fi

      local current_cpu_temp
      current_cpu_temp=$(get_cpu_temp)
      if [[ "$current_cpu_temp" =~ ^[0-9]+(\.[0-9]+)?$ ]] && (( $(bc -l <<< "$current_cpu_temp > $CPU_TEMP_THRESHOLD") )); then
        log_warn "CPU temperature ($current_cpu_temp°C) exceeded threshold ($CPU_TEMP_THRESHOLD°C)! Restarting Chromium."
        failures_cpu_overheat_file=$((failures_cpu_overheat_file + 1))
        kill_chromium
        if ! start_chromium "$current_file_url"; then
            log_error "Failed to restart Chromium for $current_file_url after overheat. Ending test for this file."
            break 
        fi
        last_cdp_ok_time=$(date +%s)
        consecutive_cdp_failures_file=0
        if (( stop_requested == 1 )); then break; fi
        sleep "$LOOP_SAMPLING_DELAY_SECONDS"
        continue
      fi
      
      local time_since_last_cdp_ok=$(( $(date +%s) - last_cdp_ok_time ))
      if (( consecutive_cdp_failures_file >= MAX_CONSECUTIVE_CDP_FAILURES )) || \
         (( time_since_last_cdp_ok >= MAX_TIME_WITHOUT_CDP_OK_SECONDS )); then
        log_warn "Chromium unresponsive (CDP failures: $consecutive_cdp_failures_file, Time since CDP OK: $time_since_last_cdp_ok s)! Restarting Chromium."
        failures_chromium_unresponsive_file=$((failures_chromium_unresponsive_file + 1))
        kill_chromium
        if ! start_chromium "$current_file_url"; then
            log_error "Failed to restart Chromium for $current_file_url after unresponsiveness. Ending test for this file."
            break
        fi
        last_cdp_ok_time=$(date +%s)
        consecutive_cdp_failures_file=0
        if (( stop_requested == 1 )); then break; fi
        sleep "$LOOP_SAMPLING_DELAY_SECONDS"
        continue
      fi

      # --- Collect Metrics ---
      local sys_cpu_usage ch_cpu_usage cpu_f cpu_t gpu_b gpu_f ch_mem_mb mem_free_kb sys_mem_used_mb ch_mem_pct sys_mem_pct fps_val \
            drop_f_pct js_total_mb js_used_mb js_pct core_w core_w_pl1 core_w_pl2 gpu_w gpu_w_pl1 gpu_w_pl2 \
            pm_v pm_i pm_w pm_wpct

      read sys_cpu_usage ch_cpu_usage <<<"$(get_cpu_usages)"
      cpu_f=$(get_cpu_freq)
      cpu_t="$current_cpu_temp" # Already fetched
      read -r gpu_b gpu_f <<<"$(gpu_stats)"
      ch_mem_mb=$(chrome_mem)
      mem_free_kb=$(awk '/MemAvailable/ {print $2}' /proc/meminfo)
      sys_mem_used_mb=$(((MEM_TOTAL_KB - mem_free_kb) / 1024))
      ch_mem_pct=$(awk "BEGIN{printf \"%.1f\", ($MEM_TOTAL_MB > 0 ? $ch_mem_mb*100/$MEM_TOTAL_MB : 0)}")
      sys_mem_pct=$(awk "BEGIN{printf \"%.1f\", ($MEM_TOTAL_MB > 0 ? $sys_mem_used_mb*100/$MEM_TOTAL_MB : 0)}")
      fps_val=$(fps_from_rAF) # Ensure this returns a number, defaults to 0 on error
      [[ -z "$fps_val" || ! "$fps_val" =~ ^[0-9]+(\.[0-9]+)?$ ]] && fps_val=0
      FPS_LIST_FILE+=("$fps_val")
      drop_f_pct=$(get_drop_pct)
      read -r js_total_mb js_used_mb js_pct <<<"$(js_heap)"
      read core_w core_w_pl1 core_w_pl2 gpu_w gpu_w_pl1 gpu_w_pl2 <<<"$(get_watts)"
      read pm_v pm_i pm_w pm_wpct <<< "$(get_power_meter_readings)"

      # --- Accumulate Data ---
      sum_metrics_file[cu]=$(bc -l <<<"${sum_metrics_file[cu]}+$ch_cpu_usage")
      sum_metrics_file[su]=$(bc -l <<<"${sum_metrics_file[su]}+$sys_cpu_usage")
      sum_metrics_file[cf]=$((sum_metrics_file[cf] + cpu_f))
      sum_metrics_file[ct]=$(bc -l <<<"${sum_metrics_file[ct]}+$cpu_t")
      sum_metrics_file[gu]=$(bc -l <<<"${sum_metrics_file[gu]}+$gpu_b")
      sum_metrics_file[gf]=$((sum_metrics_file[gf] + gpu_f))
      sum_metrics_file[cm]=$((sum_metrics_file[cm] + ch_mem_mb))
      sum_metrics_file[cmp]=$(bc -l <<<"${sum_metrics_file[cmp]}+$ch_mem_pct")
      sum_metrics_file[sm]=$((sum_metrics_file[sm] + sys_mem_used_mb))
      sum_metrics_file[smp]=$(bc -l <<<"${sum_metrics_file[smp]}+$sys_mem_pct")
      sum_metrics_file[fps]=$(bc -l <<<"${sum_metrics_file[fps]}+$fps_val")
      sum_metrics_file[ju]=$(bc -l <<<"${sum_metrics_file[ju]}+$js_used_mb")
      sum_metrics_file[jt]=$(bc -l <<<"${sum_metrics_file[jt]}+$js_total_mb")
      sum_metrics_file[jpct]=$(bc -l <<<"${sum_metrics_file[jpct]}+$js_pct")
      sum_metrics_file[cw]=$(bc -l <<<"${sum_metrics_file[cw]}+$core_w")
      sum_metrics_file[cwpct1]=$(bc -l <<<"${sum_metrics_file[cwpct1]}+$core_w_pl1")
      sum_metrics_file[cwpct2]=$(bc -l <<<"${sum_metrics_file[cwpct2]}+$core_w_pl2")
      sum_metrics_file[gw]=$(bc -l <<<"${sum_metrics_file[gw]}+$gpu_w")
      sum_metrics_file[gwpct1]=$(bc -l <<<"${sum_metrics_file[gwpct1]}+$gpu_w_pl1")
      sum_metrics_file[gwpct2]=$(bc -l <<<"${sum_metrics_file[gwpct2]}+$gpu_w_pl2")
      sum_metrics_file[df]=$(bc -l <<<"${sum_metrics_file[df]}+$drop_f_pct")
      sum_metrics_file[v]=$(bc -l <<<"${sum_metrics_file[v]}+$pm_v")
      sum_metrics_file[i]=$(bc -l <<<"${sum_metrics_file[i]}+$pm_i")
      sum_metrics_file[w]=$(bc -l <<<"${sum_metrics_file[w]}+$pm_w")
      sum_metrics_file[wpct]=$(bc -l <<<"${sum_metrics_file[wpct]}+$pm_wpct")
      
      samples_count_file=$((samples_count_file + 1))

      # --- Live Output (optional, can be very verbose) ---
      if (( samples_count_file % 12 == 0 )); then # Print every minute (12 * 5s = 60s)
         log_info "Status for $current_file_basename (Sample $samples_count_file): CPU(Chr):$(pct $ch_cpu_usage) Sys:$(pct $sys_cpu_usage) Freq:${cpu_f}MHz Temp:$(tmp $cpu_t) | GPU Busy:$(pct $gpu_b) Freq:${gpu_f}MHz | FPS:$fps_val"
      fi
      if (( stop_requested == 1 )); then break; fi
      sleep "$LOOP_SAMPLING_DELAY_SECONDS"
    done # End of inner loop for file duration

    # --- Calculate Averages for the Completed File ---
    local avg_metrics_json="{}" # Start with an empty JSON object string
    if (( samples_count_file > 0 )); then
      local avg_calc
      for key in "${metric_keys[@]}"; do
        # For cf and gf (frequencies), use integer average. For others, float.
        if [[ "$key" == "cf" || "$key" == "gf" ]]; then
            avg_calc=$(awk "BEGIN{printf \"%.0f\", ${sum_metrics_file[$key]:-0}/$samples_count_file}")
        else
            avg_calc=$(awk "BEGIN{printf \"%.2f\", ${sum_metrics_file[$key]:-0}/$samples_count_file}")
        fi
        avg_metrics_json=$(jq -n --argjson current_obj "$avg_metrics_json" --arg key "$key" --argjson val "$avg_calc" '$current_obj + {($key): $val}')
      done
      
      local one_pct_low_fps_val
      one_pct_low_fps_val=$(calc_1pct_low_fps_for_file "$(echo "${FPS_LIST_FILE[*]}")")
      avg_metrics_json=$(jq -n --argjson current_obj "$avg_metrics_json" --arg key "one_pct_low_fps" --argjson val "$one_pct_low_fps_val" '$current_obj + {($key): ($val|tonumber)}') # Ensure it's a number
    else
      log_warn "No samples collected for $current_file_basename. Averages will be zero."
      for key in "${metric_keys[@]}"; do
         avg_metrics_json=$(jq -n --argjson current_obj "$avg_metrics_json" --arg key "$key" --argjson val "0" '$current_obj + {($key): $val}')
      done
      avg_metrics_json=$(jq -n --argjson current_obj "$avg_metrics_json" --arg key "one_pct_low_fps" --argjson val "0" '$current_obj + {($key): $val}')
    fi
    
    local actual_file_duration=$(( $(date +%s) - file_run_start_time ))
    log_file_results_to_json "$current_file_url" "$FILE_TARGET_DURATION_SECONDS" "$actual_file_duration" \
      "$samples_count_file" "$failures_cpu_overheat_file" "$failures_chromium_unresponsive_file" \
      "$avg_metrics_json"

    log_info "--- Finished Test for: $current_file_basename ---"
    # Small pause before next file, unless it's the last one
    if [[ "$current_file_url" != "${FILES_TO_TEST[-1]}" ]]; then
      log_info "Pausing for 10 seconds before next file..."
      sleep 10
    fi
  done # End of outer loop for files

  kill_chromium # Ensure Chromium is stopped after all tests

  local overall_end_time overall_duration_s
  overall_end_time=$(date +%s)
  overall_duration_s=$((overall_end_time - overall_start_time))
  log_info "All soak tests finished. Total script duration: $((overall_duration_s / 60)) minutes ($overall_duration_s seconds)."
}

# Run main function
main

exit 0