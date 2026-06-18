#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script_under_test="$repo_root/users/feralfile/scripts/serve-feral-player.sh"
unit_file="$repo_root/users/feralfile/systemd-services/feral-player.service"

fail() {
  echo "test-serve-feral-player: $*" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local needle="$2"
  grep -Fq -- "$needle" "$file" || fail "expected '$needle' in $file"
}

assert_contains "$unit_file" "Type=notify"
assert_contains "$unit_file" "NotifyAccess=all"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

missing_root="$tmp_dir/missing"
output_file="$tmp_dir/missing-tree.log"

if FF_PLAYER_STATIC_ROOT="$missing_root" \
  bash "$script_under_test" >"$output_file" 2>&1; then
  fail "expected missing bundle to fail"
fi

assert_contains "$output_file" "serve-feral-player: missing static tree"

root_dir="$tmp_dir/feral-player"
bin_dir="$tmp_dir/bin"
pid_file="$tmp_dir/darkhttpd.pid"
notify_file="$tmp_dir/ready.signal"
notify_args="$tmp_dir/notify.args"
output_file="$tmp_dir/ready.log"
port="18080"

mkdir -p "$root_dir" "$bin_dir"
cat >"$root_dir/index.html" <<'EOF'
<html><body>FF player static smoke test</body></html>
EOF

contract_output="$tmp_dir/missing-contract.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  bash "$script_under_test" >"$contract_output" 2>&1; then
  fail "expected missing mint-pairing player contract to fail"
fi
assert_contains "$contract_output" "serve-feral-player: missing player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"other":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}},"loose":"mintPairingDisplay"}
EOF
wrong_path_output="$tmp_dir/wrong-contract-path.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  bash "$script_under_test" >"$wrong_path_output" 2>&1; then
  fail "expected wrong-path mint-pairing player contract to fail"
fi
assert_contains "$wrong_path_output" "serve-feral-player: invalid player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code"],"acceptedResponse":{"ok":true}}}}
EOF
missing_state_output="$tmp_dir/missing-state-contract.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  bash "$script_under_test" >"$missing_state_output" 2>&1; then
  fail "expected missing-state mint-pairing player contract to fail"
fi
assert_contains "$missing_state_output" "serve-feral-player: invalid player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":false}}}}
EOF
wrong_response_output="$tmp_dir/wrong-response-contract.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  bash "$script_under_test" >"$wrong_response_output" 2>&1; then
  fail "expected wrong-response mint-pairing player contract to fail"
fi
assert_contains "$wrong_response_output" "serve-feral-player: invalid player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}}}
EOF

cat >"$bin_dir/darkhttpd" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

root="${1:?missing root}"
shift

port=""
addr="127.0.0.1"
while (($#)); do
  case "$1" in
    --port)
      port="${2:?missing port}"
      shift 2
      ;;
    --addr)
      addr="${2:?missing addr}"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

printf '%s\n' "$$" >"${FF_PLAYER_TEST_PID_FILE:?missing pid file}"

while [[ ! -f "${FF_PLAYER_TEST_NOTIFY_FILE:?missing notify file}" ]]; do
  sleep 0.1
done
EOF

cat >"$bin_dir/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

url=""
for arg in "$@"; do
  case "$arg" in
    http://*)
      url="$arg"
      ;;
  esac
done

expected_url="http://127.0.0.1:${FF_PLAYER_TEST_PORT:?missing port}/"
if [[ "$url" == "$expected_url" && -f "${FF_PLAYER_TEST_ROOT:?missing root}/index.html" && -f "${FF_PLAYER_TEST_PID_FILE:?missing pid file}" ]]; then
  exit 0
fi

exit 1
EOF

cat >"$bin_dir/systemd-notify" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >"${FF_PLAYER_TEST_NOTIFY_ARGS:?missing notify args file}"
: >"${FF_PLAYER_TEST_NOTIFY_FILE:?missing notify file}"
EOF

chmod +x "$bin_dir/darkhttpd" "$bin_dir/curl" "$bin_dir/systemd-notify"

FF_PLAYER_STATIC_ROOT="$root_dir" \
FF_PLAYER_STATIC_PORT="$port" \
FF_PLAYER_READY_TIMEOUT_SECONDS=5 \
FF_PLAYER_TEST_ROOT="$root_dir" \
FF_PLAYER_TEST_PORT="$port" \
FF_PLAYER_TEST_PID_FILE="$pid_file" \
FF_PLAYER_TEST_NOTIFY_FILE="$notify_file" \
FF_PLAYER_TEST_NOTIFY_ARGS="$notify_args" \
PATH="$bin_dir:$PATH" \
bash "$script_under_test" >"$output_file" 2>&1

assert_contains "$notify_args" "--ready"
assert_contains "$notify_args" "feral-player static ready on http://127.0.0.1:${port}/"
[ -s "$pid_file" ] || fail "expected fake darkhttpd pid file to be written"

echo "test-serve-feral-player: OK"
