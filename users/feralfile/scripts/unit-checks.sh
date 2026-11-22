#!/usr/bin/env bash
# unit-checks.sh: three per-unit checks of the art panel around an FF1 board.
#
#   brightness  set the panel's brightness over the HDMI cable (DDC/CI), read it
#               back at two levels, and put it back where it was.
#   speakers    a low tone on the left speaker, then a high tone on the right.
#               Nothing listens: the result is PLAYED, and a person judges it.
#   bluetooth   a discovery on the Bluetooth adapter, which is fed by the Wi-Fi
#               card's second antenna lead: how many devices, and how strong.
#
#   unit-checks.sh brightness | speakers | bluetooth
#   unit-checks.sh record <label> [-o OUT_DIR] [-s SERIAL] [-x CHECK]...
#
# record runs all three (-x leaves one out), prints one "== " line per check and
# saves them as JSON. A check that cannot run says SKIP or FAIL and the next one
# still runs: a missing tool never ends the script. Needs only what every FF OS
# image ships: ddcutil, pactl/pacat, busctl, python3, gawk, jq. Runs as root
# (the factory-check live-ISO entry) or as feralfile over SSH on an installed
# unit. Batch-2 requirements items 10 and 11 and the second antenna lead are
# the reason this exists.
# jq programs handed to json() are single-quoted on purpose.
# shellcheck disable=SC2016
set -uo pipefail

TEST_VOLUME=63      # percent; the image's default volume (.start-services.sh)
TONE_SECONDS=1.5
BT_WINDOW=12        # seconds; one BR/EDR inquiry is 10.24 s
AUDIO_USER="${FF_AUDIO_USER:-feralfile}" # owns the PulseAudio the player uses

STATUS="" DETAIL="" DATA=null
BRIGHTNESS_RESTORE=""                         # the level to put the panel back to
# The same level on disk while the panel is away from it, so a later run (the
# factory loop starts one per reading) still knows where the panel belongs.
BRIGHTNESS_OWED="${FF_STATE_DIR:-${TMPDIR:-/tmp}}/unit-checks.brightness"
AUDIO_SINK="" AUDIO_VOLUMES="" AUDIO_MUTE=""  # the sink's state to put back
AUDIO_UID=""

have() { command -v "$1" >/dev/null 2>&1; }
is_root() { [ "$(id -u)" -eq 0 ]; }

# run <seconds> <command...>: nothing here may hold the factory loop for good,
# so every outside command has a deadline, and a kill behind it.
run() { timeout -k 5 "$@"; }

# result <name> <status> <detail>: the one line a person and factory-check read.
result() {
  STATUS=$2 DETAIL=$3
  printf '== %-10s %-6s %s\n' "$1" "$2" "$3"
}

# json <jq args...>: DATA for the JSON record; null when jq is missing.
json() { DATA=$(jq -n "$@" 2>/dev/null) || DATA=null; }

# --- brightness -------------------------------------------------------------

# ddc_get -> "<current> <max>" of VCP 10. A DDC/CI read fails now and then on a
# healthy panel (feral-controld retries too), so three tries.
ddc_get() {
  local i out
  for i in 1 2 3; do
    out=$(run 15 ddcutil getvcp 10 --brief 2>&1 | gawk '$1 == "VCP" && $2 == "10" && $3 == "C" && $4 ~ /^[0-9]+$/ && $5 ~ /^[0-9]+$/ { print $4, $5; exit }')
    if [ -n "$out" ]; then echo "$out"; return 0; fi
    [ "$i" -lt 3 ] && sleep 1
  done
  return 1
}

# The flags feral-controld sets brightness with; the read-back is done here.
ddc_set() { run 15 ddcutil --noverify setvcp --sleep-multiplier 1.5 10 "$1" >/dev/null 2>&1; }

# ddc_follow <value> -> the value the panel reports after being set to it.
ddc_follow() {
  local i got=""
  ddc_set "$1" || true
  for i in 1 2 3; do
    sleep 1
    got=$(ddc_get | cut -d' ' -f1) || got=""
    [ "$got" = "$1" ] && break
  done
  echo "${got:-none}"
}

# Put the panel back where it was. Stays armed until the panel reads the
# original level again, so the exit trap has one more go at a panel that did
# not take it.
brightness_restore() {
  local i back=""
  [ -n "$BRIGHTNESS_RESTORE" ] || return 0
  for i in 1 2 3; do
    back=$(ddc_follow "$BRIGHTNESS_RESTORE")
    if [ "$back" = "$BRIGHTNESS_RESTORE" ]; then
      BRIGHTNESS_RESTORE=""
      rm -f "$BRIGHTNESS_OWED"
      break
    fi
  done
  echo "$back"
}

check_brightness() {
  local reading cur max low high target steps="" ok=1 back owed
  DATA=null
  if ! have ddcutil; then result Brightness SKIP "ddcutil is not on this image"; return; fi
  # ddcutil's package loads i2c-dev at boot; make sure before blaming the panel.
  if is_root && ! compgen -G '/dev/i2c-*' >/dev/null; then run 15 modprobe i2c-dev 2>/dev/null || true; fi

  if ! reading=$(ddc_get); then
    if run 60 ddcutil detect --brief 2>/dev/null | grep -q '^Display [0-9]'; then
      result Brightness FAIL "a DDC/CI display answers but its brightness (VCP 10) cannot be read"
    else
      result Brightness NO-DDC "no DDC/CI display found (a bench monitor without DDC/CI reads this way; on the art panel it is a failure)"
    fi
    json --arg status "$STATUS" --arg detail "$DETAIL" '{status: $status, detail: $detail}'
    return
  fi
  read -r cur max <<<"$reading"
  # An earlier run left the panel away from its level: that level, not the one
  # it reads now, is where this run starts from and returns to.
  owed=$(cat "$BRIGHTNESS_OWED" 2>/dev/null) || owed=""
  if [[ "$owed" =~ ^[0-9]+$ ]] && [ "$owed" -le "$max" ]; then cur=$owed; fi
  if [ "$max" -lt 4 ]; then
    result Brightness FAIL "the panel reports a brightness range of 0-$max, too small to step through"
    json --arg status "$STATUS" --arg detail "$DETAIL" '{status: $status, detail: $detail}'
    return
  fi
  low=$((max / 4)) high=$((max * 3 / 4))

  echo "Brightness: the panel dims, brightens, and returns to $cur. Keep the power on until it has."
  BRIGHTNESS_RESTORE=$cur
  echo "$cur" >"$BRIGHTNESS_OWED" 2>/dev/null || true
  for target in "$low" "$high"; do
    back=$(ddc_follow "$target")
    steps+="${steps:+ }$target:$back"
    [ "$back" = "$target" ] || ok=0
  done
  back=$(brightness_restore)
  # $(...) ran that in a subshell: disarm here only when it did put it back.
  [ "$back" = "$cur" ] && BRIGHTNESS_RESTORE=""

  if [ "$back" != "$cur" ]; then
    result Brightness FAIL "the panel was at $cur and would not go back: it now reads $back; set:read $steps (max $max)"
  elif [ "$ok" -eq 1 ]; then
    result Brightness PASS "the panel follows the set value; set:read $steps, back at $cur (max $max)"
  else
    result Brightness FAIL "the panel does not follow the set value; set:read $steps, back at $cur (max $max)"
  fi
  json --arg status "$STATUS" --arg detail "$DETAIL" --arg steps "$steps" \
    --argjson original "$cur" --argjson max "$max" --arg restored "$back" '
    {status: $status, detail: $detail, original: $original, max: $max,
     steps: ($steps | split(" ") | map(split(":") | {set: (.[0] | tonumber), read: (.[1] | tonumber? // null)})),
     restored: ($restored | tonumber? // null)}'
}

# --- speakers ---------------------------------------------------------------

# pa <seconds> <command...>: run a PulseAudio client against the server the
# player uses. That server belongs to feralfile; root (the live ISO) has none
# of its own and libpulse does not start one for root.
pa() {
  local secs=$1; shift
  if is_root; then
    run "$secs" runuser -u "$AUDIO_USER" -- env LC_ALL=C XDG_RUNTIME_DIR="/run/user/$AUDIO_UID" "$@"
  else
    LC_ALL=C run "$secs" "$@"
  fi
}

# Put the sink's volume and mute back as they were found. Armed only once both
# were read, and before either is touched.
# Stays armed until both took, so the exit trap has one more go; returns 1 when
# they did not.
audio_restore() {
  local i
  [ -n "$AUDIO_SINK" ] || return 0
  for i in 1 2 3; do
    # shellcheck disable=SC2086 # one volume per channel
    if pa 10 pactl set-sink-volume "$AUDIO_SINK" $AUDIO_VOLUMES >/dev/null 2>&1 &&
      pa 10 pactl set-sink-mute "$AUDIO_SINK" "$AUDIO_MUTE" >/dev/null 2>&1; then
      AUDIO_SINK=""
      return 0
    fi
    sleep 1
  done
  return 1
}

# tone <left|right> <hz> -> raw 48 kHz stereo s16le on stdout, one channel
# silent. Half a second of silence leads in: an HDMI sink that was idle drops
# the first moments while the panel locks on.
tone() {
  LC_ALL=C gawk -v side="$1" -v hz="$2" -v secs="$TONE_SECONDS" 'BEGIN {
    rate = 48000; lead = int(rate / 2); n = int(rate * secs); fade = int(rate / 50)
    for (i = 0; i < lead + n + lead; i++) {
      v = 0
      if (i >= lead && i < lead + n) {
        j = i - lead; g = 1
        if (j < fade) g = j / fade; else if (n - j < fade) g = (n - j) / fade
        v = int(12000 * g * sin(6.283185307179586 * hz * j / rate))
      }
      if (v < 0) v += 65536
      lo = v % 256; hi = int(v / 256)
      if (side == "left") printf "%c%c%c%c", lo, hi, 0, 0
      else printf "%c%c%c%c", 0, 0, lo, hi
    }
  }'
}

play() { tone "$1" "$2" | pa 20 pacat "--device=$3" --playback --raw --format=s16le --rate=48000 --channels=2 --channel-map=front-left,front-right 2>/dev/null; }

check_speakers() {
  local i sink="" volumes="" mute="" level played=1
  DATA=null
  for i in pactl pacat gawk; do
    if ! have "$i"; then result Speakers SKIP "$i is not on this image"; return; fi
  done
  if is_root; then
    if ! have runuser || ! AUDIO_UID=$(id -u "$AUDIO_USER" 2>/dev/null); then
      result Speakers SKIP "no $AUDIO_USER user to play the tone as"
      return
    fi
    # On the live ISO nothing has logged feralfile in; its user manager (and
    # the PulseAudio socket in it) may still need starting.
    if ! pa 10 pactl info >/dev/null 2>&1; then
      run 30 systemctl start "user@$AUDIO_UID.service" >/dev/null 2>&1 || true
    fi
  fi
  for i in 1 2 3 4 5 6 7 8 9 10; do
    sink=$(pa 10 pactl get-default-sink 2>/dev/null) && [ -n "$sink" ] && break
    sink=""
    sleep 1
  done
  if [ -z "$sink" ]; then
    result Speakers FAIL "no PulseAudio server answers for $AUDIO_USER, nothing was played"
  elif [ "$sink" = auto_null ]; then
    result Speakers FAIL "PulseAudio has no sound output (only its dummy sink): is the HDMI cable carrying audio?"
  fi
  if [ -z "$sink" ] || [ "$sink" = auto_null ]; then
    json --arg status "$STATUS" --arg detail "$DETAIL" '{status: $status, detail: $detail}'
    return
  fi

  # Play at the image's default volume, unmuted, then put both back: every
  # channel's own raw volume, and the mute. A sink whose state cannot be read
  # in full is played as it stands, since it could not be put back.
  volumes=$(pa 10 pactl get-sink-volume "$sink" 2>/dev/null |
    gawk '{ for (i = 1; i < NF; i++) if ($i ~ /^[0-9]+$/ && $(i + 1) == "/") printf "%s ", $i }') || volumes=""
  mute=$(pa 10 pactl get-sink-mute "$sink" 2>/dev/null | gawk '{ print $2; exit }') || mute=""
  level="as found"
  if [ -n "$volumes" ] && { [ "$mute" = yes ] || [ "$mute" = no ]; }; then
    AUDIO_VOLUMES=$volumes AUDIO_SINK=$sink
    if [ "$mute" = yes ]; then AUDIO_MUTE=1; else AUDIO_MUTE=0; fi
    pa 10 pactl set-sink-mute "$sink" 0 >/dev/null 2>&1 || true
    pa 10 pactl set-sink-volume "$sink" "$TEST_VOLUME%" >/dev/null 2>&1 || true
    level="$TEST_VOLUME%"
  fi

  echo "Speakers: >>> LEFT speaker, LOW tone, now"
  play left 440 "$sink" || played=0
  echo "Speakers: >>> RIGHT speaker, HIGH tone, now"
  play right 880 "$sink" || played=0
  if ! audio_restore; then
    result Speakers FAIL "the tones played, but the volume could not be put back on $sink (it is at $level, unmuted)"
  elif [ "$played" -eq 1 ]; then
    result Speakers PLAYED "listen: low tone LEFT, then high tone RIGHT, each from its own side only (output $sink, volume $level)"
  else
    result Speakers FAIL "the tone could not be played to $sink"
  fi
  json --arg status "$STATUS" --arg detail "$DETAIL" --arg sink "$sink" --arg volume "$level" \
    '{status: $status, detail: $detail, sink: $sink, volume: $volume, left_hz: 440, right_hz: 880}'
}

# --- bluetooth --------------------------------------------------------------

bluez_objects() {
  run 15 busctl --system --json=short call org.bluez / org.freedesktop.DBus.ObjectManager GetManagedObjects 2>/dev/null
}

# bt_adapter -> TSV: object path, address, powered (true/false) of the first adapter.
bt_adapter() {
  bluez_objects | jq -r '
    [.data[0] | to_entries[] | select(.value["org.bluez.Adapter1"])] | sort_by(.key) | .[0] // empty |
    [.key, .value["org.bluez.Adapter1"].Address.data, (.value["org.bluez.Adapter1"].Powered.data | tostring)] | @tsv' 2>/dev/null
}

# bt_discover <adapter path> <seconds> -> the BlueZ object tree as JSON, taken
# at the end of a discovery of that length.
#
# BlueZ ends a discovery the moment the D-Bus client that asked for it goes
# away, and takes the RSSI off every device as it does. busctl disconnects as
# soon as its one call returns, so "busctl call ... StartDiscovery" discovers
# nothing. This holds one connection open through libsystemd for the whole
# window, reads the tree while the discovery is still running, then stops it.
bt_discover() {
  run $(($2 + 40)) python3 - "$1" "$2" <<'PY'
import ctypes, subprocess, sys, time

path, window = sys.argv[1].encode(), float(sys.argv[2])
sd = ctypes.CDLL("libsystemd.so.0")
sd.sd_bus_message_new_method_call.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p)] + [ctypes.c_char_p] * 4
sd.sd_bus_call.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_uint64, ctypes.c_void_p, ctypes.c_void_p]
sd.sd_bus_message_unref.argtypes = [ctypes.c_void_p]

bus = ctypes.c_void_p()
r = sd.sd_bus_open_system(ctypes.byref(bus))
if r < 0:
    sys.exit(f"system bus: errno {-r}")

def adapter(method):
    m = ctypes.c_void_p()
    r = sd.sd_bus_message_new_method_call(bus, ctypes.byref(m), b"org.bluez", path, b"org.bluez.Adapter1", method)
    if r >= 0:
        r = sd.sd_bus_call(bus, m, 20_000_000, None, None)
        sd.sd_bus_message_unref(m)
    return r

r = adapter(b"StartDiscovery")
if r < 0:
    sys.exit(f"StartDiscovery: errno {-r}")
try:
    time.sleep(window)
    tree = subprocess.run(
        ["busctl", "--system", "--json=short", "call", "org.bluez", "/",
         "org.freedesktop.DBus.ObjectManager", "GetManagedObjects"],
        capture_output=True, text=True, timeout=15).stdout
finally:
    adapter(b"StopDiscovery")
sys.stdout.write(tree)
PY
}

check_bluetooth() {
  local i adapter="" path address powered tree err devices count strongest top powered_here=0
  DATA=null
  for i in busctl jq python3; do
    if ! have "$i"; then result Bluetooth SKIP "$i is not on this image"; return; fi
  done
  adapter=$(bt_adapter)
  if [ -z "$adapter" ] && is_root; then
    # bluetooth.service is enabled on the image; on a live boot make sure.
    run 30 systemctl start bluetooth.service >/dev/null 2>&1 || true
  fi
  for i in 1 2 3 4 5 6 7 8 9 10; do
    [ -n "$adapter" ] && break
    sleep 1
    adapter=$(bt_adapter)
  done
  if [ -z "$adapter" ]; then
    if run 15 busctl --system status org.bluez >/dev/null 2>&1; then
      result Bluetooth FAIL "no Bluetooth adapter (bluetoothd runs but has none): check the M.2 card"
    else
      result Bluetooth FAIL "no Bluetooth adapter (bluetoothd is not running)"
    fi
    json --arg status "$STATUS" --arg detail "$DETAIL" '{status: $status, detail: $detail}'
    return
  fi
  IFS=$'\t' read -r path address powered <<<"$adapter"
  if [ "$powered" != true ]; then
    run 15 busctl --system set-property org.bluez "$path" org.bluez.Adapter1 Powered b true >/dev/null 2>&1 || true
    powered_here=1
    sleep 2
  fi

  echo "Bluetooth: listening for $BT_WINDOW s on $address"
  err=$(mktemp)
  tree=$(bt_discover "$path" "$BT_WINDOW" 2>"$err") || tree=""
  if [ "$powered_here" -eq 1 ]; then
    run 15 busctl --system set-property org.bluez "$path" org.bluez.Adapter1 Powered b false >/dev/null 2>&1 || true
  fi
  if [ -z "$tree" ]; then
    result Bluetooth FAIL "the adapter $address would not run a discovery ($(tail -n 1 "$err" | cut -c 1-80))"
    rm -f "$err"
    json --arg status "$STATUS" --arg detail "$DETAIL" --arg address "$address" \
      '{status: $status, detail: $detail, adapter: $address}'
    return
  fi
  rm -f "$err"

  # Only what this discovery heard: BlueZ keeps an RSSI on a device only while
  # a discovery that saw it is running.
  devices=$(jq -c --arg p "$path/" '
    [.data[0] | to_entries[] | select(.key | startswith($p)) | .value["org.bluez.Device1"] |
      select(. != null and .RSSI != null) |
      {address: .Address.data, rssi_dbm: .RSSI.data, name: (.Name.data // null)}] | sort_by(-.rssi_dbm)' <<<"$tree" 2>/dev/null) || devices="[]"
  [ -n "$devices" ] || devices="[]"
  count=$(jq 'length' <<<"$devices")
  if [ "$count" -eq 0 ]; then
    result Bluetooth FAIL "nothing heard in $BT_WINDOW s on $address: check the second antenna lead (a room with no Bluetooth device in it reads the same)"
  else
    strongest=$(jq '.[0].rssi_dbm' <<<"$devices")
    top=$(jq -r '.[:5] | map(.rssi_dbm | tostring) | join(" ")' <<<"$devices")
    result Bluetooth PASS "$count devices heard in $BT_WINDOW s, strongest $strongest dBm (top: $top) on $address"
  fi
  json --arg status "$STATUS" --arg detail "$DETAIL" --arg address "$address" --argjson window "$BT_WINDOW" \
    --argjson devices "$devices" '
    {status: $status, detail: $detail, adapter: $address, window_seconds: $window,
     count: ($devices | length), strongest_dbm: ($devices[0].rssi_dbm // null), devices: $devices}'
}

# --- record -----------------------------------------------------------------

record() {
  local label=$1; shift
  local out_dir=. serial=null skip=" " host stamp path name failed=0
  local -A data=() status=()
  while [ $# -gt 0 ]; do
    case "$1" in
      -o) out_dir=${2:?-o needs a directory}; shift 2 ;;
      -s) serial=$(jq -n --arg s "${2:?-s needs a serial}" '$s' 2>/dev/null) || serial=null; shift 2 ;;
      -x) skip+="${2:?-x needs a check} "; shift 2 ;;
      *) echo "unit-checks: unknown option $1" >&2; return 2 ;;
    esac
  done
  host=$(cat /etc/hostname 2>/dev/null || hostname 2>/dev/null || echo unknown)
  echo "$host  label=$label  unit checks"
  for name in brightness speakers bluetooth; do
    case "$skip" in *" $name "*) continue ;; esac
    "check_$name"
    data[$name]=$DATA status[$name]=$STATUS
    [ "$STATUS" = FAIL ] && failed=1
  done

  if have jq && mkdir -p "$out_dir" 2>/dev/null; then
    stamp=$(date +%Y%m%d-%H%M%S)
    path="$out_dir/checks-$host-$label-$stamp.json"
    if jq -n --arg label "$label" --argjson serial "$serial" --arg host "$host" \
      --argjson brightness "${data[brightness]:-null}" --arg brightness_status "${status[brightness]:-}" \
      --argjson speakers "${data[speakers]:-null}" --arg speakers_status "${status[speakers]:-}" \
      --argjson bluetooth "${data[bluetooth]:-null}" --arg bluetooth_status "${status[bluetooth]:-}" '
      def check(d; s): if d != null then d elif s != "" then {status: s} else null end;
      {label: $label, serial: $serial, recorded_at: (now | todate), hostname: $host,
       checks: ({brightness: check($brightness; $brightness_status), speakers: check($speakers; $speakers_status),
                 bluetooth: check($bluetooth; $bluetooth_status)} | with_entries(select(.value != null)))}' >"$path" 2>/dev/null; then
      echo "saved $path"
    else
      echo "unit-checks: could not write $path; the screen is the record"
    fi
  else
    echo "unit-checks: results not saved (jq or $out_dir unavailable); the screen is the record"
  fi
  return "$failed"
}

one() {
  "check_$1"
  case "$STATUS" in PASS | PLAYED) return 0 ;; SKIP) return 3 ;; *) return 1 ;; esac
}

# Sourced (scripts/test-unit-checks.sh), only the functions load.
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

# A check cut short still puts the panel's brightness and the volume back.
trap 'brightness_restore >/dev/null; audio_restore' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

case "${1:-}" in
  brightness | speakers | bluetooth) one "$1" ;;
  record)
    if [ $# -lt 2 ]; then echo "unit-checks: record needs a label" >&2; exit 2; fi
    shift; record "$@" ;;
  *) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
