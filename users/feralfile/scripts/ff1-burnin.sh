#!/usr/bin/env bash
# ff1-burnin.sh: the factory burn-in of an installed FF1 (docs/plans/
# factory-burnin.md). An engineer holds a key at power-on, picks "FF1 - Factory
# burn-in" in the boot menu, and leaves the unit running. ffos boots that
# entry into ff1-burnin.target, where ff1-burnin.service runs this as root on
# tty1. The kiosk, controld, the watchdog and the updaters never start.
#
# Four processes, all ended by pulling the power:
#   - a loopback HTTP server (127.0.0.1:BURNIN_PORT) for the burn-in page
#     (/home/feralfile/burnin), the display-check page and /status.json. It is
#     HTTP, not file://, because Chromium's fetch() and the WebGL works'
#     shader loading refuse file:// URLs;
#   - the screen: the burn-in page in cage, restarted whenever Chromium exits
#     (each restart is counted, and turns the screen red);
#   - the sampler: every SAMPLE_SECONDS it writes status.json from the checks'
#     results and the system sample (temperatures, kernel errors since the
#     session started, adapters present, Chromium restarts). Every minute it
#     copies it to the session folder as last.json, so the next burn-in
#     boot can say how long this one ran;
#   - the unit checks, once: unit-checks.sh (brightness over DDC/CI, a tone on
#     each speaker, a Bluetooth discovery), then wifi-reading.sh.
#
# The page shows the checks as they run, then the result card with a QR, then
# loops the display and load test with a status card after every pass.
#
# Results always go to /var/log/ff1-burnin/<session>/ (the @log subvolume,
# outside the root rollback). When a FAT/exFAT USB stick is plugged in, the
# session folder is then copied to it and the stick unmounted, so pulling the
# power later never cuts a mounted stick. Nothing here writes the owner state,
# NetworkManager's connections, or anything a normal boot reads. The next
# boot is a normal one, because the entry is reached only through the menu.
set -uo pipefail

SCRIPTS="${FF_SCRIPTS:-/home/feralfile/scripts}"
HOME_DIR="${FF_HOME:-/home/feralfile}"
LOG_ROOT="${FF_BURNIN_LOG:-/var/log/ff1-burnin}"
RUN_DIR="${XDG_RUNTIME_DIR:-/run/ff1-burnin}"
HWMON="${FF_HWMON:-/sys/class/hwmon}"
NET="${FF_NET:-/sys/class/net}"
BLUETOOTH="${FF_BLUETOOTH:-/sys/class/bluetooth}"
MOUNT=/mnt/ff1-burnin
BURNIN_PORT=8180     # loopback only; nothing else listens on this boot
SCANS=10             # Wi-Fi scans in the one reading
SAMPLE_SECONDS=5
CHECKS_DELAY=10      # seconds between the screen starting and the checks
KEEP_SESSIONS=20     # older session folders in LOG_ROOT are removed

# Kernel lines that mean a hardware or driver fault: an amdgpu ring timeout
# or GPU reset, an amdgpu VM page fault, an NVMe timeout or reset, a machine
# check, an oops or a hung task (case-insensitive). Counted from the session
# start only, so boot noise does not count. Keep it narrow: one match turns the
# screen red.
KERNEL_ERROR_RE='ring [a-z0-9_.]+ timeout|GPU reset|amdgpu.*page fault|VM_L2_PROTECTION_FAULT|nvme[0-9]+: .*(timeout|I/O error|resetting controller)|I/O error, dev nvme|Machine check events logged|\[Hardware Error\]|BUG: |Oops: |blocked for more than'

wait_for_wifi() {
  local i iface
  for ((i = 0; i < 60; i++)); do
    for iface in "$NET"/wl*; do
      [ -e "$iface" ] || continue
      iface=$(basename "$iface")
      if wpa_cli -i "$iface" status >/dev/null 2>&1; then echo "$iface"; return 0; fi
    done
    sleep 1
  done
  return 1
}

# A removable, unmounted FAT/exFAT partition: the results stick. Never one on
# a disk that also carries an iso9660 partition: that is an installer stick
# left plugged in, and its FAT partition is the ISO's ESP, which the line
# reuses for installs.
results_stick() {
  local name type rm parent fstype mnt isos
  isos=$(lsblk -rno NAME,TYPE,RM,PKNAME,FSTYPE,MOUNTPOINT | gawk '$5 == "iso9660" { print ($4 == "" ? $1 : $4) }')
  while read -r name type rm parent fstype mnt; do
    [ "$type" = part ] && [ "$rm" = 1 ] && [ -z "$mnt" ] || continue
    grep -qxF -- "$parent" <<<"$isos" && continue
    case "$fstype" in vfat|exfat) echo "/dev/$name"; return 0 ;; esac
  done < <(lsblk -rno NAME,TYPE,RM,PKNAME,FSTYPE,MOUNTPOINT)
  return 1
}

# copy_to_stick <session_dir>: the session folder onto the results stick, then
# unmount it. Prints where it went, or nothing. A stick that will not mount or
# take the files (write-protected, full) changes nothing: the folder in
# LOG_ROOT is the record either way.
copy_to_stick() {
  local dir=$1 stick
  stick=$(results_stick) || return 0
  mkdir -p "$MOUNT"
  mount "$stick" "$MOUNT" 2>/dev/null || return 0
  if mkdir -p "$MOUNT/ff1-burnin" && cp -r "$dir" "$MOUNT/ff1-burnin/" && sync; then
    echo "USB stick $stick, folder ff1-burnin/$(basename "$dir")"
  fi
  umount "$MOUNT" 2>/dev/null || true
}

# prune_sessions <dir> <keep>: drop all but the newest <keep> session folders.
prune_sessions() {
  local dir=$1 keep=$2
  [ -d "$dir" ] || return 0
  find "$dir" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort -r | tail -n +"$((keep + 1))" |
    while read -r old; do rm -rf "${dir:?}/$old"; done
}

# result_lines <text>: the "== " English result lines of the checks.
result_lines() { grep -E '^== ' <<<"$1" || echo "== Checks     FAIL   did not run"; }

# checks_json <lines>: result lines as [{name, status, detail}]. unit-checks.sh
# prints "== %-10s %-6s %s".
checks_json() {
  gawk '/^== / { name = $2; st = $3; $1 = $2 = $3 = ""; sub(/^ +/, ""); printf "%s\t%s\t%s\n", name, st, $0 }' <<<"$1" |
    jq -R -s 'split("\n") | map(select(length > 0) | split("\t") | {name: .[0], status: .[1], detail: (.[2] // "")})'
}

# wifi_line <reading.json>: one result line from a wifi-reading.sh record, per
# band the access points seen and the strongest median. No access point at
# all is a FAIL: a unit with both antenna leads off still sees none.
wifi_line() {
  jq -r '
    (.summary | group_by(.band) | map("\(.[0].band): \(length) APs, best \(map(.median_dbm) | max | floor) dBm") | join("; ")) as $bands
    | if (.summary | length) == 0 then "== Wi-Fi      FAIL   no access point seen in \(.scans) scans"
      else "== Wi-Fi      PASS   \($bands)" end' "$1" 2>/dev/null || echo "== Wi-Fi      FAIL   the reading could not be read"
}

# temps -> "cpu gpu nvme" in whole °C, "null" for a sensor that is not there.
# k10temp temp1 is Tctl, amdgpu temp1 the edge, nvme temp1 the composite.
temps() {
  local cpu=null gpu=null nvme=null d v
  for d in "$HWMON"/hwmon*; do
    [ -r "$d/name" ] && [ -r "$d/temp1_input" ] || continue
    v=$(($(cat "$d/temp1_input") / 1000))
    case "$(cat "$d/name")" in
      k10temp) cpu=$v ;;
      amdgpu) gpu=$v ;;
      nvme) nvme=$v ;;
    esac
  done
  echo "$cpu $gpu $nvme"
}

# count_kernel_errors -> "<count>\t<last matching line>" over the kernel lines
# on stdin.
count_kernel_errors() {
  # The pattern through ENVIRON, not -v: -v would eat its backslashes.
  RE="$KERNEL_ERROR_RE" gawk 'BEGIN { IGNORECASE = 1 } $0 ~ ENVIRON["RE"] { n++; last = $0 } END { printf "%d\t%s\n", n, substr(last, 1, 120) }'
}

# kernel_errors <cursor>: count_kernel_errors over the kernel log after the
# journal cursor taken at the session start. A cursor, not a time: a clock
# stepped back after the start would hide lines from --since.
kernel_errors() {
  if [ -n "$1" ]; then
    journalctl -k -q --no-pager -o cat --after-cursor "$1" 2>/dev/null | count_kernel_errors
  else
    journalctl -k -q --no-pager -o cat -b 2>/dev/null | count_kernel_errors
  fi
}

# kernel_cursor -> the journal cursor of the newest kernel line now.
kernel_cursor() { journalctl -k -q -n 0 --show-cursor 2>/dev/null | sed -n 's/^-- cursor: //p'; }

# previous_session <log_root> <this_session>: the newest earlier session's
# last.json as {ran, last, kernel_errors, restarts}, or null. Power pulls and
# crashes look the same here; what the screen showed tells them apart.
previous_session() {
  local f p
  f=$(find "$1" -mindepth 2 -maxdepth 2 -name last.json -not -path "*/$2/*" 2>/dev/null | sort -r | head -n 1)
  [ -n "$f" ] || { echo null; return; }
  # A power pull can leave the file empty or torn: jq prints nothing for an
  # empty one and still exits 0, so check the output, not the status.
  p=$(jq -c '{ran: (.updated_at - .started_at), last: (.updated_at | todate), kernel_errors: .sys.kernel_errors, restarts: .sys.restarts}' "$f" 2>/dev/null)
  [ -n "$p" ] && echo "$p" || echo null
}

# max <a> <b>: the larger, where "null" means no reading.
max() {
  if [ "$1" = null ]; then echo "$2"
  elif [ "$2" = null ] || [ "$1" -ge "$2" ]; then echo "$1"
  else echo "$2"; fi
}

# status_json <cpu> <gpu> <nvme> <cpu_max> <gpu_max> <nvme_max> <kernel "n\tlast">
# <wifi> <bt> (adapter_state): one status.json. The rest comes from the globals and from the
# files in RUN_DIR the checks write: checks.json, checks_done_at, where,
# restarts.
status_json() {
  jq -n \
    --arg board "$BOARD" --arg version "$VERSION" --argjson started "$STARTED_AT" \
    --argjson checks "$(cat "$RUN_DIR/checks.json" 2>/dev/null || echo '[]')" \
    --argjson checks_done "$(cat "$RUN_DIR/checks_done_at" 2>/dev/null || echo null)" \
    --arg where "$(cat "$RUN_DIR/where" 2>/dev/null)" --argjson previous "$PREVIOUS" \
    --argjson cpu "$1" --argjson gpu "$2" --argjson nvme "$3" \
    --argjson cpu_max "$4" --argjson gpu_max "$5" --argjson nvme_max "$6" \
    --argjson kerr "${7%%$'\t'*}" --arg klast "${7#*$'\t'}" \
    --arg wifi "$8" --arg bt "$9" \
    --argjson restarts "$(cat "$RUN_DIR/restarts" 2>/dev/null || echo 0)" '
    {board: $board, version: $version, started_at: $started, updated_at: (now | floor),
     checks: $checks, checks_done_at: $checks_done, where: $where, previous: $previous,
     sys: {cpu: $cpu, gpu: $gpu, nvme: $nvme, cpu_max: $cpu_max, gpu_max: $gpu_max, nvme_max: $nvme_max,
           kernel_errors: $kerr, last_error: (if $klast == "" then null else $klast end),
           wifi: $wifi, bt: $bt, restarts: $restarts}}'
}

# adapter_state <state> <glob> -> the next state of one adapter: unseen until
# it first appears (the Wi-Fi interface can take up to a minute at boot),
# then present, then lost for good once it has gone. A card that drops off
# the bus and comes back between two status cards still counts.
adapter_state() {
  if compgen -G "$2" >/dev/null; then
    case "$1" in lost) echo lost ;; *) echo present ;; esac
  else
    case "$1" in unseen) echo unseen ;; *) echo lost ;; esac
  fi
}

# sample_loop: status.json every SAMPLE_SECONDS (written whole, then renamed,
# so the page never reads half a file), last.json every minute.
sample_loop() {
  local tick=0 cpu gpu nvme cpu_max=null gpu_max=null nvme_max=null ke=$'0\t' wifi=unseen bt=unseen
  while true; do
    read -r cpu gpu nvme < <(temps)
    cpu_max=$(max "$cpu" "$cpu_max"); gpu_max=$(max "$gpu" "$gpu_max"); nvme_max=$(max "$nvme" "$nvme_max")
    ((tick % 6 == 0)) && ke=$(kernel_errors "$KERNEL_CURSOR")
    wifi=$(adapter_state "$wifi" "$NET/wl*"); bt=$(adapter_state "$bt" "$BLUETOOTH/hci*")
    status_json "$cpu" "$gpu" "$nvme" "$cpu_max" "$gpu_max" "$nvme_max" "$ke" "$wifi" "$bt" >"$WWW/status.json.tmp" &&
      mv -f "$WWW/status.json.tmp" "$WWW/status.json"
    # Copied and renamed, as status.json is: cp -f would truncate last.json in
    # place, and a power pull or a full disk could leave it empty.
    ((tick % (60 / SAMPLE_SECONDS) == 0)) && cp -f "$WWW/status.json" "$RESULTS/last.json.tmp" 2>/dev/null &&
      mv -f "$RESULTS/last.json.tmp" "$RESULTS/last.json"
    tick=$((tick + 1))
    sleep "$SAMPLE_SECONDS"
  done
}

# The burn-in page in cage, until Chromium exits. The flags are the kiosk's
# decode and graphics path (start-kiosk.sh), so the video and WebGL segments
# exercise what an installed unit plays. The log holds the latest Chromium run
# and the one before (.1, where the reason for a restart is): it lives in RAM
# for a run of days.
FEATURES="UseOzonePlatform,VaapiVideoDecoder,VaapiIgnoreDriverChecks,Vulkan,DefaultANGLEVulkan,VulkanFromANGLE"
screen_once() {
  rm -rf "$RUN_DIR/chromium"
  [ -f "$RUN_DIR/chromium.log" ] && mv -f "$RUN_DIR/chromium.log" "$RUN_DIR/chromium.log.1"
  cage -s -- chromium "http://127.0.0.1:$BURNIN_PORT/burnin/index.html" \
    --no-sandbox \
    --kiosk \
    --no-first-run \
    --disable-sync \
    --disable-infobars \
    --disable-features=TranslateUI \
    --autoplay-policy=no-user-gesture-required \
    --ozone-platform=wayland \
    --enable-features="$FEATURES" \
    --user-data-dir="$RUN_DIR/chromium" >"$RUN_DIR/chromium.log" 2>&1
}

screen_loop() {
  local restarts=0
  echo 0 >"$RUN_DIR/restarts"
  while true; do
    screen_once
    restarts=$((restarts + 1))
    echo "$restarts" >"$RUN_DIR/restarts"
    sleep 3
  done
}

wait_for_port() {
  local i
  for ((i = 0; i < 50; i++)); do
    (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null && return 0
    sleep 0.2
  done
  return 1
}

# Sourced (scripts/test-ff1-burnin.sh), only the functions load.
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

STARTED_AT=$(date +%s)
KERNEL_CURSOR=$(kernel_cursor)
BOARD=$(cat /etc/hostname 2>/dev/null || echo FF1)
VERSION=$(jq -r '.version // empty' "$HOME_DIR/ff1-config.json" 2>/dev/null)
SESSION=$(date -d "@$STARTED_AT" '+%Y%m%d-%H%M%S')
WWW="$RUN_DIR/www"
RESULTS="$LOG_ROOT/$SESSION"

if [ ! -r "$HOME_DIR/burnin/index.html" ]; then
  clear
  printf '\nFF1 burn-in: %s is not on this image.\n' "$HOME_DIR/burnin/index.html"
  exec sleep infinity
fi

mkdir -p "$RUN_DIR" "$WWW"
rm -f "$RUN_DIR/checks.json" "$RUN_DIR/checks_done_at"
ln -sfn "$HOME_DIR/burnin" "$WWW/burnin"
ln -sfn "$HOME_DIR/display-check" "$WWW/display-check"
prune_sessions "$LOG_ROOT" "$((KEEP_SESSIONS - 1))"
mkdir -p "$RESULTS"
PREVIOUS=$(previous_session "$LOG_ROOT" "$SESSION")
echo "$RESULTS" >"$RUN_DIR/where"
export XDG_RUNTIME_DIR="$RUN_DIR" LIBSEAT_BACKEND="${LIBSEAT_BACKEND:-seatd}"

python3 -m http.server "$BURNIN_PORT" --bind 127.0.0.1 --directory "$WWW" >/dev/null 2>&1 &
sample_loop &
wait_for_port "$BURNIN_PORT"
screen_loop &

# Give cage its modeset first: the brightness check talks to the panel over
# the HDMI cable, and the tones play through its HDMI audio.
sleep "$CHECKS_DELAY"
# Each result line reaches the page as it is printed, not at the end.
bash "$SCRIPTS/unit-checks.sh" record r1 -o "$RESULTS" 2>&1 | tee "$RESULTS/unit-checks.log" |
  while IFS= read -r line; do
    case "$line" in "== "*) progress+="$line"$'\n'; checks_json "$progress" >"$RUN_DIR/checks.json" ;; esac
  done
lines=$(result_lines "$(cat "$RESULTS/unit-checks.log")")
checks_json "$lines" >"$RUN_DIR/checks.json"
if iface=$(wait_for_wifi); then
  out=$(bash "$SCRIPTS/wifi-reading.sh" record r1 -n "$SCANS" -o "$RESULTS" 2>&1) || true
  printf '%s\n' "$out" >"$RESULTS/wifi-reading.log"
  saved=$(sed -n 's/^saved //p' <<<"$out")
  if [ -n "$saved" ]; then wifi=$(wifi_line "$saved"); else wifi="== Wi-Fi      FAIL   the reading on $iface did not finish"; fi
else
  wifi="== Wi-Fi      FAIL   no Wi-Fi interface came up in 60 s (M.2 card, antenna leads)"
fi
checks_json "$lines"$'\n'"$wifi" >"$RUN_DIR/checks.json"
sync
copied=$(copy_to_stick "$RESULTS")
[ -n "$copied" ] && echo "$RESULTS and $copied" >"$RUN_DIR/where"
date +%s >"$RUN_DIR/checks_done_at"

# The sampler, the server and the screen run until the power is pulled.
wait
