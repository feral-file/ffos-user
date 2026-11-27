#!/usr/bin/env bash
# ff1-burnin.sh: the factory burn-in of an installed FF1 (docs/plans/
# factory-burnin.md). An engineer holds a key at power-on, picks "FF1 - Factory
# burn-in" in the boot menu, and leaves the unit running. ffos boots that
# entry into ff1-burnin.target, where ff1-burnin.service runs this as root on
# tty1. The kiosk, controld, the watchdog and the updaters never start.
#
#   1. The unit checks, once: unit-checks.sh (brightness over DDC/CI, a tone on
#      each speaker, a Bluetooth discovery), then wifi-reading.sh. Their result
#      lines stay on tty1 for RESULT_SECONDS. The JSON goes to
#      /var/log/ff1-burnin/<session>/ (the @log subvolume, outside the root
#      rollback). When a FAT/exFAT USB stick is plugged in, the session folder
#      is then copied to it and the stick unmounted, so pulling the power
#      later never cuts a mounted stick.
#   2. The display test, until the power is pulled: the display-check page's
#      assembly set, looping full screen in cage. Chromium is started again
#      whenever it exits (panel unplugged, GPU process lost).
#
# Nothing here writes the owner state, NetworkManager's connections, or
# anything a normal boot reads. Pull the power to end it; the next boot is a
# normal one, because the entry is reached only through the menu.
set -uo pipefail

SCRIPTS="${FF_SCRIPTS:-/home/feralfile/scripts}"
PAGE="${FF_BURNIN_PAGE:-/home/feralfile/display-check/index.html}"
LOG_ROOT="${FF_BURNIN_LOG:-/var/log/ff1-burnin}"
RUN_DIR="${XDG_RUNTIME_DIR:-/run/ff1-burnin}"
MOUNT=/mnt/ff1-burnin
SCANS=10             # Wi-Fi scans in the one reading
RESULT_SECONDS=60    # the check results stay on screen this long
KEEP_SESSIONS=20     # older session folders in LOG_ROOT are removed

say() { printf '\n%s\n' "$*"; }

wait_for_wifi() {
  local i iface
  for ((i = 0; i < 60; i++)); do
    for iface in /sys/class/net/wl*; do
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
    echo "also on USB stick $stick, folder ff1-burnin/$(basename "$dir")"
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
result_lines() { grep -E '^== ' <<<"$1" || echo "== checks did not run"; }

# wifi_line <reading.json>: one result line from a wifi-reading.sh record, per
# band the access points seen and the strongest median. No access point at
# all is a FAIL: a unit with both antenna leads off still sees none.
wifi_line() {
  jq -r '
    (.summary | group_by(.band) | map("\(.[0].band): \(length) APs, best \(map(.median_dbm) | max | floor) dBm") | join("; ")) as $bands
    | if (.summary | length) == 0 then "== Wi-Fi      FAIL  no access point seen in \(.scans) scans"
      else "== Wi-Fi      PASS  \($bands)" end' "$1" 2>/dev/null || echo "== Wi-Fi      FAIL  the reading could not be read"
}

# The display-check page in cage, until Chromium exits. The flags are the
# kiosk's decode and graphics path (start-kiosk.sh), so the video and graphics
# slides exercise what an installed unit plays. The log holds the latest
# Chromium run and the one before (.1, where the reason for a restart is):
# it lives in RAM for a run of days.
FEATURES="UseOzonePlatform,VaapiVideoDecoder,VaapiIgnoreDriverChecks,Vulkan,DefaultANGLEVulkan,VulkanFromANGLE"
slides() {
  rm -rf "$RUN_DIR/chromium"
  [ -f "$RUN_DIR/chromium.log" ] && mv -f "$RUN_DIR/chromium.log" "$RUN_DIR/chromium.log.1"
  cage -s -- chromium "file://$PAGE" \
    --no-sandbox \
    --kiosk \
    --no-first-run \
    --disable-sync \
    --disable-infobars \
    --disable-features=TranslateUI \
    --autoplay-policy=no-user-gesture-required \
    --allow-file-access-from-files \
    --ozone-platform=wayland \
    --enable-features="$FEATURES" \
    --user-data-dir="$RUN_DIR/chromium" >"$RUN_DIR/chromium.log" 2>&1
}

# Sourced (scripts/test-ff1-burnin.sh), only the functions load.
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

mkdir -p "$RUN_DIR"
session=$(date '+%Y%m%d-%H%M%S')
prune_sessions "$LOG_ROOT" "$((KEEP_SESSIONS - 1))"
results="$LOG_ROOT/$session"
mkdir -p "$results"
where="Results: $results"

clear
echo "Feral File factory burn-in        board $(cat /etc/hostname 2>/dev/null)"
echo "$where"
echo "Unit checks once (brightness, speakers: low tone LEFT then high tone RIGHT, Bluetooth, Wi-Fi), then the display test until the power is pulled."
echo "-------------------------------------------------------------------------------------"

out=$(bash "$SCRIPTS/unit-checks.sh" record r1 -o "$results" 2>&1 | tee /dev/tty) || true
checks=$(result_lines "$out")
echo
if iface=$(wait_for_wifi); then
  out=$(bash "$SCRIPTS/wifi-reading.sh" record r1 -n "$SCANS" -o "$results" 2>&1 | tee /dev/tty) || true
  saved=$(sed -n 's/^saved //p' <<<"$out")
  if [ -n "$saved" ]; then wifi=$(wifi_line "$saved"); else wifi="== Wi-Fi      FAIL  the reading on $iface did not finish"; fi
else
  wifi="== Wi-Fi      FAIL  no Wi-Fi interface came up in 60 s (M.2 card, antenna leads)"
fi
sync
copied=$(copy_to_stick "$results")
[ -n "$copied" ] && where="$where; $copied"

clear
echo "Feral File factory burn-in        board $(cat /etc/hostname 2>/dev/null)"
echo "$where"
echo
echo "$checks"
echo "$wifi"
say "Photograph this screen. The display test starts in $RESULT_SECONDS s and runs until the power is pulled."
sleep "$RESULT_SECONDS"

if [ ! -r "$PAGE" ]; then
  say "Display test: $PAGE is not on this image."
  exec sleep infinity
fi
export XDG_RUNTIME_DIR="$RUN_DIR" LIBSEAT_BACKEND="${LIBSEAT_BACKEND:-seatd}"
while true; do
  slides
  clear
  say "Display test stopped; starting again in 3 s. Log: $RUN_DIR/chromium.log.1 after the restart"
  sleep 3
done
