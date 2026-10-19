#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
wifi_reading="$repo_root/users/feralfile/scripts/wifi-reading.sh"
factory_check="$repo_root/users/feralfile/scripts/factory-check.sh"
file_permissions="$repo_root/users/feralfile/.file_permissions.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-wifi-reading: $*" >&2
  exit 1
}

for script in wifi-reading.sh factory-check.sh; do
  grep -Fq "chmod +x /home/feralfile/scripts/$script" "$file_permissions" || \
    fail "device install does not make $script executable"
done

fake_bin="$tmp_dir/bin"
mkdir -p "$fake_bin"
export PATH="$fake_bin:$PATH"

# --- factory-check: the results stick is never on the disk the ISO booted from.

iso_uuid=2026-10-14-10-59-01-00
mkdir -p "$tmp_dir/dev" "$tmp_dir/by-uuid"
touch "$tmp_dir/dev/sda" "$tmp_dir/dev/sda1"
printf 'BOOT_IMAGE=/arch/boot/x86_64/vmlinuz-linux archisobasedir=arch archisodevice=UUID=%s script=/home/feralfile/scripts/factory-check.sh\n' \
  "$iso_uuid" > "$tmp_dir/cmdline"

cat > "$fake_bin/lsblk" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  "-ndo PKNAME "*) [ "$(basename "$3")" = sda1 ] && echo sda; exit 0 ;;
  "-rno NAME,TYPE,RM,PKNAME,FSTYPE,MOUNTPOINT") cat "$FF_TEST_LSBLK" ;;
  *) echo "unexpected lsblk $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$fake_bin/lsblk"

# A dd-written installer stick (sda: ISO 9660 + the FAT ARCHISO_EFI partition),
# the NVMe, then the results stick. Raw lsblk prints empty columns as nothing.
installer_stick='sda disk 1  iso9660
sda1 part 1 sda iso9660 /run/archiso/bootmnt
sda2 part 1 sda vfat
nvme0n1 disk 0
nvme0n1p1 part 0 nvme0n1 vfat
nvme0n1p2 part 0 nvme0n1 btrfs '
results_stick_line='sdb disk 1
sdb1 part 1 sdb vfat '

pick() {
  printf '%s\n' "$1" > "$tmp_dir/lsblk"
  FF_TEST_LSBLK="$tmp_dir/lsblk" FF_CMDLINE="${2:-$tmp_dir/cmdline}" \
    FF_DEV_BY_UUID="$tmp_dir/by-uuid" bash -c 'source "$1"; results_stick' _ "$factory_check" || true
}

ln -sfn "$tmp_dir/dev/sda1" "$tmp_dir/by-uuid/$iso_uuid"
[ "$(pick "$installer_stick"$'\n'"$results_stick_line")" = /dev/sdb1 ] || \
  fail "factory check did not pick the second stick"
[ -z "$(pick "$installer_stick")" ] || \
  fail "factory check took the installer stick's EFI partition for a results stick"
[ -z "$(pick "${installer_stick}"$'\n''sdb disk 1
sdb1 part 1 sdb vfat /run/media/sdb1')" ] || fail "factory check took a mounted stick"
[ -z "$(pick "$installer_stick"$'\n''sdb disk 1
sdb1 part 1 sdb ext4 ')" ] || fail "factory check took a stick that is not FAT/exFAT"

# udev may point the ISO's UUID at the whole disk, which shares it.
ln -sfn "$tmp_dir/dev/sda" "$tmp_dir/by-uuid/$iso_uuid"
[ "$(pick "$installer_stick"$'\n'"$results_stick_line")" = /dev/sdb1 ] || \
  fail "factory check did not pick the second stick when the ISO UUID names the disk"

# Without a boot disk to exclude, nothing is written anywhere.
rm -f "$tmp_dir/by-uuid/$iso_uuid"
[ -z "$(pick "$installer_stick"$'\n'"$results_stick_line")" ] || \
  fail "factory check used a stick without knowing which disk it booted from"
echo 'BOOT_IMAGE=/vmlinuz-linux root=UUID=x' > "$tmp_dir/cmdline.installed"
[ -z "$(pick "$results_stick_line" "$tmp_dir/cmdline.installed")" ] || \
  fail "factory check used a stick without an archisodevice"

# --- wifi-reading: a sample is only what this scan saw, read past wpa_cli's 4 KiB cap.

# id bssid freq level age ssid. Age 0 arrived after the scan request;
# 120 is a cached entry from an earlier scan, with its old level.
cat > "$tmp_dir/bss" <<'EOF'
3	66:cf:84:ae:a0:70	2412	-37	0	brdn_wifi
7	66:cf:84:ae:a0:71	5180	-44	120	brdn_wifi
8	aa:fa:c4:76:a5:65	2437	-68	0	\x00\x00\x00\x00
12	60:cf:84:ae:b9:05	5745	-48	0	key=value net
20	e4:fa:c4:76:a5:63	5955	-70	0	six
EOF

# Serves two records per reply, the way a full reply stops at the cap, and
# ends the table's last record with #### as wpa_supplicant does.
cat > "$fake_bin/wpa_cli" <<'EOF'
#!/usr/bin/env bash
shift 2 # -i IFACE
case "$1" in
  scan) echo FAIL-BUSY ;;
  bss)
    start=${2#RANGE=}; start=${start%-}
    gawk -F'\t' -v s="$start" -v last="$(tail -n 1 "$FF_TEST_BSS" | cut -f 1)" '$1 >= s && n < 2 {
      n++; printf "id=%s\nbssid=%s\nfreq=%s\nlevel=%s\nage=%s\nssid=%s\n%s\n", $1, $2, $3, $4, $5, $6, ($1 == last ? "####" : "====")
    }' "$FF_TEST_BSS" ;;
  *) echo "unexpected wpa_cli $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$fake_bin/wpa_cli"

scan() {
  FF_TEST_BSS="$1" bash -c 'source "$1"; SUDO=""; IFACE=wlan0; SCAN_TIMEOUT=2; scan_once 1' _ "$wifi_reading"
}

expected=$'1\t66:cf:84:ae:a0:70\t2412\t2.4GHz\t-37\tbrdn_wifi
1\taa:fa:c4:76:a5:65\t2437\t2.4GHz\t-68\t
1\t60:cf:84:ae:b9:05\t5745\t5GHz\t-48\tkey=value net
1\te4:fa:c4:76:a5:63\t5955\t6GHz\t-70\tsix'
actual=$(scan "$tmp_dir/bss")
[ "$actual" = "$expected" ] || fail "scan sample is wrong:
$actual
expected:
$expected"

gawk -F'\t' 'BEGIN { OFS = "\t" } { $5 = 120; print }' "$tmp_dir/bss" > "$tmp_dir/bss.stale"
[ -z "$(scan "$tmp_dir/bss.stale")" ] || fail "a scan that saw nothing new reported cached access points"

echo "test-wifi-reading: OK"
