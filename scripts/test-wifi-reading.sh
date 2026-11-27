#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
wifi_reading="$repo_root/users/feralfile/scripts/wifi-reading.sh"
file_permissions="$repo_root/users/feralfile/.file_permissions.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-wifi-reading: $*" >&2
  exit 1
}

grep -Fq "chmod +x /home/feralfile/scripts/wifi-reading.sh" "$file_permissions" || \
  fail "device install does not make wifi-reading.sh executable"

fake_bin="$tmp_dir/bin"
mkdir -p "$fake_bin"
export PATH="$fake_bin:$PATH"

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
