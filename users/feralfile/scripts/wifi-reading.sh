#!/usr/bin/env bash
# wifi-reading.sh: record the Wi-Fi signal this board sees, so two placements compare.
#
# Every access point in range is a probe. Record once with the board in the
# enclosure and once with the antennas in the open at the same spot, then
# compare the two files: the per-access-point delta is what the enclosure costs,
# per band. Scan results carry the level in dBm for every AP in range, so no
# network credentials are needed.
#
#   wifi-reading.sh record <label> [-n SCANS] [-o OUT_DIR] [-s SERIAL]
#   wifi-reading.sh compare <baseline.json> <test.json>
#
# Needs only what every FF OS image ships: wpa_cli, gawk, jq. Runs as root, or
# as feralfile (sudo for wpa_cli). Batch-2 requirements item 14 is the reason
# this exists; the factory-check live-ISO entry is the usual way it runs.
set -euo pipefail

SETTLE_SECONDS=5 # a scan on the RTL8821CE finishes well inside this

SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"

die() { echo "wifi-reading: $*" >&2; exit 1; }

wifi_iface() {
  local d
  for d in /sys/class/net/wl*; do
    [ -e "$d" ] && { basename "$d"; return; }
  done
  die "no Wi-Fi interface (no /sys/class/net/wl*)"
}

wpa() { $SUDO wpa_cli -i "$IFACE" "$@"; }

# band <freq_mhz>
band() {
  if [ "$1" -lt 3000 ]; then echo 2.4GHz; elif [ "$1" -lt 5950 ]; then echo 5GHz; else echo 6GHz; fi
}

# scan_once <sample_index> -> TSV lines: sample bssid freq band dbm ssid
scan_once() {
  local i=$1 bssid freq level ssid
  wpa scan >/dev/null
  sleep "$SETTLE_SECONDS"
  while IFS=$'\t' read -r bssid freq level _ ssid; do
    [[ "$bssid" =~ ^([0-9a-f]{2}:){5}[0-9a-f]{2}$ ]] || continue
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$i" "$bssid" "$freq" "$(band "$freq")" "$level" "${ssid:-}"
  done < <(wpa scan_results)
}

# summarize < samples.tsv -> TSV: band freq ssid bssid n median min max (sorted)
summarize() {
  gawk -F'\t' '
    { key = $4 SUBSEP $3 SUBSEP $6 SUBSEP $2; n[key]++; v[key, n[key]] = $5 }
    END {
      for (key in n) {
        split(key, k, SUBSEP)
        c = n[key]; delete a
        for (i = 1; i <= c; i++) a[i] = v[key, i] + 0
        asort(a)
        med = (c % 2) ? a[(c + 1) / 2] : (a[c / 2] + a[c / 2 + 1]) / 2
        printf "%s\t%s\t%s\t%s\t%d\t%.1f\t%d\t%d\n", k[1], k[2], k[3], k[4], c, med, a[1], a[c]
      }
    }' | sort -t$'\t' -k2,2n -k6,6nr
}

print_table() {
  printf '%-7s %7s %5s %5s %3s  %-17s  %s\n' band median min max n bssid ssid
  gawk -F'\t' '{ printf "%-7s %7s %5s %5s %3s  %-17s  %s\n", $1, $6, $7, $8, $5, $4, $3 }'
}

# link -> JSON object for the connected AP, or null
link() {
  local st sp
  st=$(wpa status 2>/dev/null || true)
  if ! grep -q '^wpa_state=COMPLETED' <<<"$st"; then echo null; return; fi
  sp=$(wpa signal_poll 2>/dev/null || true)
  jq -n --arg st "$st" --arg sp "$sp" '
    def kv: split("\n") | map(select(contains("="))) | map(split("=") | {key: .[0], value: .[1:] | join("=")}) | from_entries;
    ($st | kv) as $s | ($sp | kv) as $p |
    {ssid: $s.ssid, bssid: $s.bssid, freq: ($s.freq | tonumber),
     rssi_dbm: ($p.RSSI // "0" | tonumber), avg_rssi_dbm: ($p.AVG_RSSI // "0" | tonumber),
     linkspeed_mbps: ($p.LINKSPEED // "0" | tonumber)}'
}

record() {
  local label=$1; shift
  local scans=10 out_dir=. serial=null
  while [ $# -gt 0 ]; do
    case "$1" in
      -n) scans=$2; shift 2 ;;
      -o) out_dir=$2; shift 2 ;;
      -s) serial=$(jq -n --arg s "$2" '$s'); shift 2 ;;
      *) die "unknown option $1" ;;
    esac
  done
  IFACE=$(wifi_iface)
  local host mac lk stamp path i count
  host=$(cat /etc/hostname 2>/dev/null || hostname)
  mac=$(cat "/sys/class/net/$IFACE/address")
  SAMPLES=$(mktemp); SUMMARY=$(mktemp)
  trap 'rm -f "$SAMPLES" "$SUMMARY"' EXIT

  echo "$host  $IFACE $mac  label=$label  scans=$scans"
  for ((i = 1; i <= scans; i++)); do
    scan_once "$i" >>"$SAMPLES"
    count=$(gawk -F'\t' -v i="$i" '$1 == i' "$SAMPLES" | wc -l)
    echo "  scan $i/$scans: $count access points"
  done
  summarize <"$SAMPLES" >"$SUMMARY"
  lk=$(link)
  echo
  print_table <"$SUMMARY"
  if [ "$lk" != null ]; then
    jq -r '"\nconnected: \(.ssid) \(.bssid) \(.freq) MHz  rssi \(.rssi_dbm) dBm  avg \(.avg_rssi_dbm) dBm  \(.linkspeed_mbps) Mbit/s"' <<<"$lk"
  fi

  mkdir -p "$out_dir"
  stamp=$(date +%Y%m%d-%H%M%S)
  path="$out_dir/wifi-$host-$label-$stamp.json"
  jq -n \
    --arg label "$label" --argjson serial "$serial" --arg host "$host" --arg mac "$mac" \
    --arg iface "$IFACE" --argjson scans "$scans" --argjson link "$lk" \
    --rawfile summary "$SUMMARY" --rawfile samples "$SAMPLES" '
    def tsv: split("\n") | map(select(length > 0) | split("\t"));
    {label: $label, serial: $serial, recorded_at: (now | todate), hostname: $host, wifi_mac: $mac,
     iface: $iface, scans: $scans, link: $link,
     summary: ($summary | tsv | map({band: .[0], freq: (.[1] | tonumber), ssid: .[2], bssid: .[3],
       n: (.[4] | tonumber), median_dbm: (.[5] | tonumber), min_dbm: (.[6] | tonumber), max_dbm: (.[7] | tonumber)})),
     samples: ($samples | tsv | map({sample: (.[0] | tonumber), bssid: .[1], freq: (.[2] | tonumber),
       band: .[3], dbm: (.[4] | tonumber), ssid: .[5]}))}' >"$path"
  echo
  echo "saved $path"
}

compare() {
  local base=$1 test=$2
  jq -r -n --slurpfile b "$base" --slurpfile t "$test" '
    $b[0] as $b | $t[0] as $t |
    ($b.summary | map({key: (.band + "|" + .bssid), value: .}) | from_entries) as $bm |
    ($t.summary | map({key: (.band + "|" + .bssid), value: .}) | from_entries) as $tm |
    ([$bm, $tm | keys[]] | unique | map(select($bm[.] and $tm[.]))) as $common |
    ($common | map({band: $bm[.].band, bssid: $bm[.].bssid, ssid: $tm[.].ssid,
                    base: $bm[.].median_dbm, test: $tm[.].median_dbm,
                    delta: ($tm[.].median_dbm - $bm[.].median_dbm)})) as $rows |
    def median: sort | if length == 0 then null elif length % 2 == 1 then .[(length - 1) / 2] else (.[length / 2 - 1] + .[length / 2]) / 2 end;
    def fmt(n): if n >= 0 then "+" + (n | tostring) else (n | tostring) end;
    "baseline: \($b.label)  (\($b.recorded_at))",
    "test:     \($t.label)  (\($t.recorded_at))",
    "",
    ("band    " + "  base" + "  test" + " delta" + "  bssid              ssid"),
    ($rows | sort_by(.band, -.base)[] |
      "\(.band | . + "        "[:8 - length])\(.base | tostring | "      "[:6 - length] + .)\(.test | tostring | "      "[:6 - length] + .)\(fmt(.delta) | "      "[:6 - length] + .)  \(.bssid)  \(.ssid)"),
    "",
    ($rows | group_by(.band)[] |
      "\(.[0].band): median delta \(fmt(map(.delta) | median)) dB over \(length) access points (test minus baseline; negative = weaker in test)"),
    (([$bm | keys[]] - [$tm | keys[]]) as $lost |
      if ($lost | length) > 0 then "\nseen only in baseline (lost in test): \($lost | length) access points" else empty end)
  '
}

case "${1:-}" in
  record) [ $# -ge 2 ] || die "record needs a label"; shift; record "$@" ;;
  compare) [ $# -eq 3 ] || die "compare needs two JSON files"; compare "$2" "$3" ;;
  *) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
