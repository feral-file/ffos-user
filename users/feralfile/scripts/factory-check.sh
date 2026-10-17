#!/usr/bin/env bash
# factory-check.sh: the live-ISO "Factory check" boot entry (ffos
# archiso-x86_64-linux-factory-check.conf). Boots a bare FF1 board from the
# installer USB stick into a text screen and takes Wi-Fi readings on request,
# nothing installed, nothing on the NVMe touched. Each reading is a labeled run
# of wifi-reading.sh; the table shows on the HDMI screen and the JSON lands on a
# second USB stick when one is plugged in (any FAT/exFAT stick), otherwise in
# RAM only and the screen is the record. Runs as root on tty1 via
# /root/.automated_script.sh.
set -uo pipefail

SCRIPTS=/home/feralfile/scripts
RESULTS=/root/factory-check
MOUNT=/mnt/factory-check

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

# A removable FAT/exFAT partition that is not the live ISO's own device.
results_stick() {
  local iso_dev name type rm fstype mnt
  iso_dev=$(sed -n 's/.*archisodevice=UUID=\([^ ]*\).*/\1/p' /proc/cmdline)
  while read -r name type rm fstype mnt; do
    if [ "$type" != part ] || [ "$rm" != 1 ]; then continue; fi
    case "$fstype" in vfat|exfat) ;; *) continue ;; esac
    [ -n "$mnt" ] && continue
    [ -n "$iso_dev" ] && [ "$(lsblk -rno UUID "/dev/$name" 2>/dev/null)" = "$iso_dev" ] && continue
    echo "/dev/$name"; return 0
  done < <(lsblk -rno NAME,TYPE,RM,FSTYPE,MOUNTPOINT)
  return 1
}

clear
echo "Feral File factory check: Wi-Fi reading"
echo "---------------------------------------"
echo "Board: $(cat /etc/hostname 2>/dev/null)"

if iface=$(wait_for_wifi); then
  echo "Wi-Fi: $iface $(cat "/sys/class/net/$iface/address")"
else
  say "No Wi-Fi interface came up in 60 s. Check the M.2 card and both antenna leads, then reboot."
  read -r -p "Press Enter to shut down..." _
  poweroff
fi

mkdir -p "$RESULTS"
if stick=$(results_stick); then
  mkdir -p "$MOUNT"
  if mount "$stick" "$MOUNT" 2>/dev/null; then
    RESULTS="$MOUNT/factory-check"; mkdir -p "$RESULTS"
    echo "Results: $stick (USB stick), folder factory-check"
  else
    echo "Results: could not mount $stick; results stay in RAM, photograph the screen"
  fi
else
  echo "Results: no second USB stick found; results stay in RAM, photograph the screen"
fi

say "Take one reading per placement at the same spot: e.g. 'in-box' with the board in the"
echo "enclosure and everything closed, then 'open' with the antennas on plastic in the open."
echo "Each reading scans 10 times (about a minute)."

previous=""
while true; do
  echo
  read -r -p "Label for this reading (letters, digits, dashes), or q to finish: " label
  case "$label" in
    q|Q) break ;;
    "") continue ;;
  esac
  label=${label//[^A-Za-z0-9_-]/-}
  echo
  if ! out=$(bash "$SCRIPTS/wifi-reading.sh" record "$label" -n 10 -o "$RESULTS" | tee /dev/tty); then
    say "Reading failed."
    continue
  fi
  current=$(sed -n 's/^saved //p' <<<"$out")
  if [ -n "$previous" ] && [ -n "$current" ]; then
    say "Compared with the previous reading:"
    bash "$SCRIPTS/wifi-reading.sh" compare "$previous" "$current"
  fi
  previous=$current
done

sync
if mountpoint -q "$MOUNT"; then umount "$MOUNT" && say "USB stick unmounted, safe to remove."; fi
read -r -p "Press Enter to shut down..." _
poweroff
