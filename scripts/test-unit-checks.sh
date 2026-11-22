#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
unit_checks="$repo_root/users/feralfile/scripts/unit-checks.sh"
factory_check="$repo_root/users/feralfile/scripts/factory-check.sh"
file_permissions="$repo_root/users/feralfile/.file_permissions.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-unit-checks: $*" >&2
  exit 1
}

# expect <what> <pattern> <text>
expect() {
  grep -Eq -- "$2" <<<"$3" || fail "$1: expected /$2/ in:
$3"
}

grep -Fq "chmod +x /home/feralfile/scripts/unit-checks.sh" "$file_permissions" || \
  fail "device install does not make unit-checks.sh executable"

# The base tools the checks lean on, and nothing else: a check's own tool is on
# PATH only when a test puts its stand-in there, so "not on this image" is a
# path every test can take.
base_bin="$tmp_dir/base"
fake_bin="$tmp_dir/bin"
mkdir -p "$base_bin" "$fake_bin"
for tool in bash env cat cut date gawk grep head id jq mkdir mktemp rm sed tail tee timeout tr; do
  path=$(command -v "$tool") || fail "the test needs $tool"
  ln -s "$path" "$base_bin/$tool"
done
# No check may wait on the clock here.
printf '#!/bin/sh\nexit 0\n' > "$base_bin/sleep"
chmod +x "$base_bin/sleep"

stub() { cat > "$fake_bin/$1"; chmod +x "$fake_bin/$1"; }
unstub() { rm -f "$fake_bin/$1"; }

run() { PATH="$fake_bin:$base_bin" FF_TEST_DIR="$tmp_dir" "$base_bin/bash" "$@"; }
check() { run "$unit_checks" "$@" 2>&1; }

# --- brightness: the panel follows, does not follow, is not there.

ddcutil_stub() {
  stub ddcutil <<'EOF'
#!/usr/bin/env bash
# FF_TEST_DDC: follow | stuck | no-display | no-read
state="$FF_TEST_DIR/brightness"
echo "$*" >> "$FF_TEST_DIR/ddcutil.log"
case "$*" in
  "getvcp 10 --brief")
    case "$FF_TEST_DDC" in
      no-display | no-read) echo "Display not found"; exit 1 ;;
    esac
    echo "VCP 10 C $(cat "$state") 100" ;;
  "--noverify setvcp --sleep-multiplier 1.5 10 "*)
    [ "$FF_TEST_DDC" = stuck ] || echo "${!#}" > "$state" ;;
  "detect --brief")
    if [ "$FF_TEST_DDC" = no-display ]; then
      printf 'Invalid display\n   I2C bus:  /dev/i2c-3\n'
    else
      printf 'Display 1\n   I2C bus:  /dev/i2c-15\n'
    fi ;;
  *) echo "unexpected ddcutil $*" >&2; exit 1 ;;
esac
EOF
}

unstub ddcutil
out=$(check brightness) && fail "brightness without ddcutil did not report a skip"
expect "brightness without ddcutil" '^== Brightness +SKIP +ddcutil is not on this image' "$out"

ddcutil_stub
echo 40 > "$tmp_dir/brightness"
out=$(FF_TEST_DDC=follow check brightness) || fail "a panel that follows did not pass:
$out"
expect "brightness pass" '^== Brightness +PASS .*set:read 25:25 75:75, back at 40 \(max 100\)' "$out"
[ "$(cat "$tmp_dir/brightness")" = 40 ] || fail "brightness was not put back"

out=$(FF_TEST_DDC=stuck check brightness) && fail "a panel that does not follow passed"
expect "brightness fail" '^== Brightness +FAIL .*does not follow.*set:read 25:40 75:40, back at 40' "$out"

out=$(FF_TEST_DDC=no-display check brightness) && fail "no DDC/CI display passed"
expect "no DDC/CI display" '^== Brightness +NO-DDC +no DDC/CI display found' "$out"

out=$(FF_TEST_DDC=no-read check brightness) && fail "an unreadable VCP 10 passed"
expect "unreadable brightness" '^== Brightness +FAIL +a DDC/CI display answers but' "$out"

# --- speakers: left then right through PulseAudio, volume put back.

pulse_stubs() {
  stub pactl <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$FF_TEST_DIR/pactl.log"
case "$1" in
  info) [ -n "$FF_TEST_SINK" ] ;;
  get-default-sink) [ -n "$FF_TEST_SINK" ] && echo "$FF_TEST_SINK" ;;
  get-sink-volume) echo "Volume: front-left: 26241 /  40% / -23.85 dB,   front-right: 26241 /  40% / -23.85 dB" ;;
  get-sink-mute) echo "Mute: yes" ;;
  set-sink-volume | set-sink-mute) ;;
  *) echo "unexpected pactl $*" >&2; exit 1 ;;
esac
EOF
  # Which channel carried sound, from the raw stereo s16le frames.
  stub pacat <<'EOF'
#!/usr/bin/env bash
[ "${FF_TEST_PACAT:-ok}" = ok ] || exit 1
od -An -v -tu1 -w4 | gawk '
  $1 + $2 > 0 { l = 1 } $3 + $4 > 0 { r = 1 }
  END { print NR, (l ? "left" : "") (r ? "right" : "") }' >> "$FF_TEST_DIR/pacat.log"
EOF
}
ln -s "$(command -v od)" "$base_bin/od"

unstub pactl; unstub pacat
out=$(check speakers) && fail "speakers without pactl did not report a skip"
expect "speakers without pactl" '^== Speakers +SKIP +pactl is not on this image' "$out"

pulse_stubs
: > "$tmp_dir/pactl.log"; : > "$tmp_dir/pacat.log"
out=$(FF_TEST_SINK=alsa_output.hdmi-stereo check speakers) || fail "speakers did not play:
$out"
expect "speakers played" '^== Speakers +PLAYED .*low tone LEFT, then high tone RIGHT.*alsa_output.hdmi-stereo' "$out"
expect "left is announced first" '^Speakers: >>> LEFT' "$(sed -n 1p <<<"$out")"
[ "$(cat "$tmp_dir/pacat.log")" = $'120000 left\n120000 right' ] || \
  fail "the tones were not one channel each, left then right: $(cat "$tmp_dir/pacat.log")"
expect "volume set for the tone" '^set-sink-volume @DEFAULT_SINK@ 63%$' "$(cat "$tmp_dir/pactl.log")"
[ "$(tail -n 2 "$tmp_dir/pactl.log")" = $'set-sink-volume @DEFAULT_SINK@ 40%\nset-sink-mute @DEFAULT_SINK@ 1' ] || \
  fail "volume and mute were not put back: $(cat "$tmp_dir/pactl.log")"

out=$(FF_TEST_SINK=auto_null check speakers) && fail "the dummy sink counted as played"
expect "dummy sink" '^== Speakers +FAIL .*no sound output' "$out"
out=$(FF_TEST_SINK="" check speakers) && fail "no PulseAudio counted as played"
expect "no PulseAudio" '^== Speakers +FAIL +no PulseAudio server answers' "$out"
out=$(FF_TEST_SINK=alsa_output.hdmi-stereo FF_TEST_PACAT=broken check speakers) && fail "a failed playback counted as played"
expect "failed playback" '^== Speakers +FAIL +the tone could not be played' "$out"

# --- bluetooth: what a discovery heard, strongest first.

adapter='"/org/bluez/hci0":{"org.bluez.Adapter1":{"Address":{"type":"s","data":"3C:3B:AD:00:00:01"},"Powered":{"type":"b","data":true}}}'
printf '{"type":"a{oa{sa{sv}}}","data":[{"/org/bluez":{"org.bluez.AgentManager1":{}},%s}]}\n' "$adapter" > "$tmp_dir/bluez.adapter"
printf '{"type":"a{oa{sa{sv}}}","data":[{"/org/bluez":{"org.bluez.AgentManager1":{}}}]}\n' > "$tmp_dir/bluez.none"
dev() { # dev <path> <address> <props>
  printf '"%s":{"org.bluez.Device1":{"Address":{"type":"s","data":"%s"}%s}}' "$1" "$2" "$3"
}
# Two heard now, one remembered from before (no RSSI), one on another adapter.
printf '{"type":"a{oa{sa{sv}}}","data":[{%s,%s,%s,%s,%s}]}\n' "$adapter" \
  "$(dev /org/bluez/hci0/dev_A AA:00:00:00:00:01 ',"RSSI":{"type":"n","data":-71}')" \
  "$(dev /org/bluez/hci0/dev_B AA:00:00:00:00:02 ',"RSSI":{"type":"n","data":-48},"Name":{"type":"s","data":"phone"}')" \
  "$(dev /org/bluez/hci0/dev_C AA:00:00:00:00:03 '')" \
  "$(dev /org/bluez/hci1/dev_D AA:00:00:00:00:04 ',"RSSI":{"type":"n","data":-30}')" > "$tmp_dir/bluez.heard"

bluez_stubs() {
  stub busctl <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *GetManagedObjects) cat "$FF_TEST_DIR/bluez.$FF_TEST_BLUEZ" ;;
  "--system status org.bluez") [ "$FF_TEST_BLUEZ" != down ] ;;
  *) echo "unexpected busctl $*" >&2; exit 1 ;;
esac
EOF
  # Stands in for the discovery: the adapter path and window it was given,
  # then the tree it would have read.
  stub python3 <<'EOF'
#!/usr/bin/env bash
cat > /dev/null
echo "$2 $3" > "$FF_TEST_DIR/discovery.log"
[ "$FF_TEST_DISCOVERY" != refused ] || { echo "StartDiscovery: errno 113" >&2; exit 1; }
cat "$FF_TEST_DIR/bluez.$FF_TEST_DISCOVERY"
EOF
}

unstub busctl; unstub python3
out=$(check bluetooth) && fail "bluetooth without busctl did not report a skip"
expect "bluetooth without busctl" '^== Bluetooth +SKIP +busctl is not on this image' "$out"

bluez_stubs
out=$(FF_TEST_BLUEZ=adapter FF_TEST_DISCOVERY=heard check bluetooth) || fail "a discovery that heard devices did not pass:
$out"
expect "bluetooth pass" '^== Bluetooth +PASS +2 devices heard in 12 s, strongest -48 dBm \(top: -48 -71\) on 3C:3B:AD:00:00:01' "$out"
[ "$(cat "$tmp_dir/discovery.log")" = "/org/bluez/hci0 12" ] || fail "the discovery ran on the wrong adapter or window"

out=$(FF_TEST_BLUEZ=adapter FF_TEST_DISCOVERY=adapter check bluetooth) && fail "a discovery that heard nothing passed"
expect "nothing heard" '^== Bluetooth +FAIL +nothing heard in 12 s.*second antenna lead' "$out"
out=$(FF_TEST_BLUEZ=adapter FF_TEST_DISCOVERY=refused check bluetooth) && fail "a refused discovery passed"
expect "refused discovery" '^== Bluetooth +FAIL .*would not run a discovery \(StartDiscovery: errno 113\)' "$out"
out=$(FF_TEST_BLUEZ=none FF_TEST_DISCOVERY=heard check bluetooth) && fail "a missing adapter passed"
expect "no adapter" '^== Bluetooth +FAIL +no Bluetooth adapter \(bluetoothd runs but has none\)' "$out"
out=$(FF_TEST_BLUEZ=down FF_TEST_DISCOVERY=heard check bluetooth 2>&1) && fail "no bluetoothd passed"
expect "no bluetoothd" '^== Bluetooth +FAIL +no Bluetooth adapter \(bluetoothd is not running\)' "$out"

# --- record: every check runs whatever the one before it did; JSON carries
# hostname and label as the Wi-Fi reading does.

record() {
  FF_TEST_DDC="$1" FF_TEST_SINK=alsa_output.hdmi-stereo FF_TEST_BLUEZ=adapter FF_TEST_DISCOVERY=heard \
    run "$unit_checks" record "${@:2}" 2>&1
}

out=$(record follow r1 -o "$tmp_dir/out" -s B2-0007) || fail "a record with nothing failing did not exit 0:
$out"
saved=$(sed -n 's/^saved //p' <<<"$out")
[ -f "$saved" ] || fail "record saved nothing:
$out"
jq -e '.label == "r1" and .serial == "B2-0007" and (.hostname | length > 0) and (.recorded_at | length > 0)
  and .checks.brightness.status == "PASS" and .checks.brightness.original == 40
  and .checks.brightness.steps == [{set: 25, read: 25}, {set: 75, read: 75}] and .checks.brightness.restored == 40
  and .checks.speakers.status == "PLAYED" and .checks.speakers.sink == "alsa_output.hdmi-stereo"
  and .checks.bluetooth.status == "PASS" and .checks.bluetooth.count == 2 and .checks.bluetooth.strongest_dbm == -48
  and .checks.bluetooth.devices[0] == {address: "AA:00:00:00:00:02", rssi_dbm: -48, name: "phone"}' "$saved" >/dev/null || \
  fail "the saved record is wrong: $(cat "$saved")"
case "$(basename "$saved")" in
  "checks-$(jq -r .hostname "$saved")-r1-"*.json) ;;
  *) fail "the record's name does not carry hostname and label: $saved" ;;
esac

out=$(record stuck r2 -o "$tmp_dir/out") && fail "a record with a failing check exited 0"
[ "$(grep -c '^== ' <<<"$out")" = 3 ] || fail "a failing check stopped the ones after it:
$out"

out=$(record follow r3 -o "$tmp_dir/out" -x brightness) || fail "a record without brightness failed"
saved=$(sed -n 's/^saved //p' <<<"$out")
jq -e '.checks | has("brightness") | not' "$saved" >/dev/null || fail "-x brightness still ran brightness"

# Nothing a check needs is on the image: three skips, a record, no abort.
unstub ddcutil; unstub pactl; unstub pacat; unstub busctl; unstub python3
out=$(record follow r4 -o "$tmp_dir/out") || fail "skips alone made the record fail"
[ "$(grep -c '^== [A-Za-z]* *SKIP' <<<"$out")" = 3 ] || fail "missing tools did not read as three skips:
$out"
saved=$(sed -n 's/^saved //p' <<<"$out")
jq -e '[.checks[].status] == ["SKIP", "SKIP", "SKIP"]' "$saved" >/dev/null || fail "skips were not recorded: $(cat "$saved")"

# --- factory-check: the checks' lines reach the loop; brightness is stepped
# until it has passed once, then left alone.

ddcutil_stub; pulse_stubs; bluez_stubs
: > "$tmp_dir/ddcutil.log"
# shellcheck disable=SC2016
out=$(FF_TEST_DDC=follow FF_TEST_SINK=alsa_output.hdmi-stereo FF_TEST_BLUEZ=adapter FF_TEST_DISCOVERY=heard \
  FF_SCRIPTS="$(dirname "$unit_checks")" FF_SCREEN=/dev/null run -c '
    source "$1"; RESULTS="$2"
    unit_checks 1; echo "$checks"; sets=$(grep -c setvcp "$FF_TEST_DIR/ddcutil.log")
    unit_checks 2; echo "--"; echo "$checks"
    [ "$sets" = "$(grep -c setvcp "$FF_TEST_DIR/ddcutil.log")" ] || echo "STEPPED AGAIN"' _ "$factory_check" "$tmp_dir/out")
[ "$(sed -n '/^--$/q;p' <<<"$out" | grep -c '^== ')" = 3 ] || fail "the factory check did not collect three result lines:
$out"
second=$(sed '1,/^--$/d' <<<"$out")
expect "brightness line kept from reading 1" '^== Brightness +PASS .*\(reading 1\)$' "$second"
[ "$(grep -c '^== ' <<<"$second")" = 3 ] || fail "reading 2 does not show three result lines:
$second"
grep -q 'STEPPED AGAIN' <<<"$out" && fail "the factory check stepped the brightness again after it passed"

# shellcheck disable=SC2016
out=$(FF_SCRIPTS="$tmp_dir/nowhere" FF_SCREEN=/dev/null run -c '
  source "$1"; RESULTS="$2"; unit_checks 1; echo "$checks"' _ "$factory_check" "$tmp_dir/out")
expect "missing unit-checks.sh" '^== unit checks did not run$' "$out"

echo "test-unit-checks: OK"
