#!/usr/bin/env bash
# factory-check.sh: the live-ISO "Factory check" boot entry (ffos
# archiso-x86_64-linux-factory-check.conf). Boots a bare FF1 board from the
# installer USB stick into a text screen and takes Wi-Fi readings back to back
# on its own, nothing installed, nothing on the NVMe touched. Hands-off by
# design: inside the art panel only the USB-C port is reachable once the pocket
# cover is on, so the keyboard is needed for the boot menu only. Boot with the
# cover off, pick the entry, unplug the keyboard, close the box, and read the
# panel. Each reading is a numbered run of wifi-reading.sh, shown against
# reading 1, so moving the antennas between readings shows as a delta on the
# screen. The screen is the record (photograph it); the JSON also lands on a
# second FAT/exFAT USB stick when one is plugged in. Runs as root on tty1 via
# /root/.automated_script.sh. Ends when the power is pulled.
set -uo pipefail

SCRIPTS=/home/feralfile/scripts
RESULTS=/root/factory-check
MOUNT=/mnt/factory-check
CMDLINE="${FF_CMDLINE:-/proc/cmdline}"
BY_UUID="${FF_DEV_BY_UUID:-/dev/disk/by-uuid}"
SCANS=10          # each waits for a finished scan: 4-16 s on an RTL8821CE
PAUSE_SECONDS=10  # between readings

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

# The disk the live ISO booted from (sda, not sda1). archisodevice names its
# ISO 9660 partition, but the same stick also carries the ISO's FAT EFI
# partition (ARCHISO_EFI), which looks exactly like a results stick.
boot_disk() {
  local uuid dev parent
  uuid=$(sed -n 's/.*archisodevice=UUID=\([^ ]*\).*/\1/p' "$CMDLINE")
  [ -n "$uuid" ] || return 1
  dev=$(readlink -e "$BY_UUID/$uuid") || return 1
  parent=$(lsblk -ndo PKNAME "$dev" 2>/dev/null)
  basename "${parent:-$dev}"
}

# A removable FAT/exFAT partition on any disk but the one the ISO booted from.
# When that disk cannot be told, no stick is used: the screen is the record,
# and the installer stick is never written.
results_stick() {
  local boot name type rm parent fstype mnt
  boot=$(boot_disk) || return 1
  while read -r name type rm parent fstype mnt; do
    if [ "$type" != part ] || [ "$rm" != 1 ] || [ "$parent" = "$boot" ]; then continue; fi
    case "$fstype" in vfat|exfat) ;; *) continue ;; esac
    [ -n "$mnt" ] && continue
    echo "/dev/$name"; return 0
  done < <(lsblk -rno NAME,TYPE,RM,PKNAME,FSTYPE,MOUNTPOINT)
  return 1
}

header() {
  clear
  echo "Feral File factory check: Wi-Fi reading        board $(cat /etc/hostname 2>/dev/null)"
  echo "$results_line"
  echo "Readings repeat on their own; the keyboard can be unplugged and the box closed."
  echo "Photograph the screen for each antenna placement. Pull the power to finish."
  echo "-------------------------------------------------------------------------------------"
}

# Sourced (scripts/test-wifi-reading.sh), only the functions load.
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

if iface=$(wait_for_wifi); then
  :
else
  clear
  say "No Wi-Fi interface came up in 60 s. Check the M.2 card and both antenna leads, then power-cycle."
  sleep infinity
fi

mkdir -p "$RESULTS"
results_line="Results: RAM only, the screen is the record"
if stick=$(results_stick); then
  mkdir -p "$MOUNT"
  if mount "$stick" "$MOUNT" 2>/dev/null; then
    RESULTS="$MOUNT/factory-check"; mkdir -p "$RESULTS"
    results_line="Results: JSON on USB stick $stick, folder factory-check (also shown here)"
  fi
fi

first=""
n=0
while true; do
  n=$((n + 1))
  header
  echo "Reading $n  $(date '+%H:%M:%S')  Wi-Fi $iface $(cat "/sys/class/net/$iface/address")"
  echo
  if out=$(bash "$SCRIPTS/wifi-reading.sh" record "r$n" -n "$SCANS" -o "$RESULTS" 2>&1 | tee /dev/tty); then
    current=$(sed -n 's/^saved //p' <<<"$out")
    if [ -z "$first" ]; then
      first=$current
    elif [ -n "$current" ]; then
      say "Reading $n against reading 1:"
      bash "$SCRIPTS/wifi-reading.sh" compare "$first" "$current" | grep -E 'GHz: median|seen only in'
    fi
    sync
  else
    say "Reading failed; trying again."
  fi
  sleep "$PAUSE_SECONDS"
done
