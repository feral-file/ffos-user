#!/usr/bin/env bash
# Pins ff1-burnin.sh's helpers (docs/plans/factory-burnin.md). The boot side
# (entry, target, updater drop-ins) is pinned in ffos scripts/verify.sh.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
burnin="$repo_root/users/feralfile/scripts/ff1-burnin.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-ff1-burnin: $*" >&2
  exit 1
}

grep -Fq "chmod +x /home/feralfile/scripts/ff1-burnin.sh" "$repo_root/users/feralfile/.file_permissions.sh" || \
  fail "device install does not make ff1-burnin.sh executable"

# The burn-in must leave nothing a normal boot reads: no owner/setup state, no
# NetworkManager connections, no systemctl enable/disable.
if grep -vE '^[[:space:]]*#' "$burnin" | grep -Eq '\.state|NetworkManager/system-connections|nmcli (c|con|connection) (add|modify|delete)|systemctl (enable|disable|mask)'; then
  fail "ff1-burnin.sh writes state a normal boot reads"
fi

# shellcheck source=/dev/null
source "$burnin"

# wifi_line: per band, APs and the best median; none seen is a FAIL.
cat >"$tmp_dir/ok.json" <<'JSON'
{"scans": 10, "summary": [
  {"band": "2.4GHz", "median_dbm": -61.5}, {"band": "2.4GHz", "median_dbm": -48},
  {"band": "5GHz", "median_dbm": -70}]}
JSON
got=$(wifi_line "$tmp_dir/ok.json")
[ "$got" = "== Wi-Fi      PASS  2.4GHz: 2 APs, best -48 dBm; 5GHz: 1 APs, best -70 dBm" ] || fail "wifi_line ok: '$got'"
echo '{"scans": 10, "summary": []}' >"$tmp_dir/none.json"
got=$(wifi_line "$tmp_dir/none.json")
[ "$got" = "== Wi-Fi      FAIL  no access point seen in 10 scans" ] || fail "wifi_line none: '$got'"
got=$(wifi_line "$tmp_dir/missing.json")
[[ "$got" == "== Wi-Fi      FAIL"* ]] || fail "wifi_line missing file: '$got'"

# result_lines keeps the English result lines only, and says so when none came.
got=$(result_lines $'noise\n== Brightness PASS  a\n-- 亮度 通过\n== Speakers PLAYED  b')
[ "$got" = $'== Brightness PASS  a\n== Speakers PLAYED  b' ] || fail "result_lines: '$got'"
[ "$(result_lines 'nothing')" = "== checks did not run" ] || fail "result_lines empty"

# prune_sessions keeps the newest N folders.
for d in 20260101-000000 20260102-000000 20260103-000000 20260104-000000; do mkdir -p "$tmp_dir/log/$d"; done
prune_sessions "$tmp_dir/log" 2
[ "$(find "$tmp_dir/log" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort | tr '\n' ' ')" = "20260103-000000 20260104-000000 " ] || \
  fail "prune_sessions did not keep the newest two"

# results_stick: removable, unmounted, FAT/exFAT partitions, never on a disk
# that carries an iso9660 partition (an installer stick left plugged in).
mkdir -p "$tmp_dir/bin"
cat >"$tmp_dir/bin/lsblk" <<'STUB'
#!/usr/bin/env bash
# NAME TYPE RM PKNAME FSTYPE MOUNTPOINT. sdd is an installer stick: iso9660
# plus the ISO's FAT ESP, and comes first so a missing exclusion picks it.
printf '%s\n' "nvme0n1 disk 0   " "nvme0n1p1 part 0 nvme0n1 vfat /boot" "sdd disk 1   " "sdd1 part 1 sdd iso9660 " "sdd2 part 1 sdd vfat " \
  "sda disk 1   " "sda1 part 1 sda ext4 " "sdb1 part 1 sdb vfat /media/x" "sdc1 part 1 sdc exfat "
STUB
chmod +x "$tmp_dir/bin/lsblk"
got=$(PATH="$tmp_dir/bin:$PATH" results_stick) || fail "results_stick found none"
[ "$got" = /dev/sdc1 ] || fail "results_stick: '$got'"

echo "test-ff1-burnin: ok"
