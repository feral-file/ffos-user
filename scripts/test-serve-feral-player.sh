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

cat >"$bin_dir/darkhttpd" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

# A no-arg invocation is the serve script's --header capability probe: like
# the real binary, print usage and exit non-zero. Whether the usage text
# advertises --header is scenario-controlled so both a modern and a legacy
# darkhttpd can be modeled.
if (($# == 0)); then
  echo "usage: darkhttpd /path/to/wwwroot [options]"
  if [[ "${FF_PLAYER_TEST_HEADER_SUPPORT:-1}" == "1" ]]; then
    echo "  --header 'X-Header: Value'  add a custom response header"
  fi
  exit 1
fi

printf '%s\n' "$@" >"${FF_PLAYER_TEST_SERVER_ARGS:?missing server args file}"

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
    --header)
      : "${2:?missing header value}"
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

server_args="$tmp_dir/server.args"

run_ready_server() {
  rm -f "$pid_file" "$notify_file" "$notify_args" "$output_file" "$server_args"

  FF_PLAYER_STATIC_ROOT="$root_dir" \
  FF_PLAYER_STATIC_PORT="$port" \
  FF_PLAYER_READY_TIMEOUT_SECONDS=5 \
  FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT="${FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT:-0}" \
  FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT="${FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT:-1}" \
  FF_PLAYER_TEST_HEADER_SUPPORT="${FF_PLAYER_TEST_HEADER_SUPPORT:-1}" \
  FF_PLAYER_TEST_ROOT="$root_dir" \
  FF_PLAYER_TEST_PORT="$port" \
  FF_PLAYER_TEST_PID_FILE="$pid_file" \
  FF_PLAYER_TEST_NOTIFY_FILE="$notify_file" \
  FF_PLAYER_TEST_NOTIFY_ARGS="$notify_args" \
  FF_PLAYER_TEST_SERVER_ARGS="$server_args" \
  PATH="$bin_dir:$PATH" \
  "$@" >"$output_file" 2>&1 || fail "serve script exited non-zero; see $output_file"

  assert_contains "$notify_args" "--ready"
  assert_contains "$notify_args" "feral-player static ready on http://127.0.0.1:${port}/"
  [ -s "$pid_file" ] || fail "expected fake darkhttpd pid file to be written"
}

# The setupDisplay contract is required BY DEFAULT: SoftAP onboarding narrates
# through it, and a bundle without it must not reach READY silently.
rm -f "$root_dir/ffos-player-contract.json"
setup_missing_output="$tmp_dir/setup-display-missing.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  bash "$script_under_test" >"$setup_missing_output" 2>&1; then
  fail "expected missing setupDisplay player contract to fail by default"
fi
assert_contains "$setup_missing_output" "serve-feral-player: missing player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}}}
EOF
setup_absent_output="$tmp_dir/setup-display-absent.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  bash "$script_under_test" >"$setup_absent_output" 2>&1; then
  fail "expected manifest without setupDisplay to fail by default"
fi
assert_contains "$setup_absent_output" "serve-feral-player: invalid setupDisplay player contract"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"setupDisplay":{"version":1,"requestKey":"request","states":["softap_qr","joining"],"acceptedResponse":{"ok":true}}}}
EOF
setup_states_output="$tmp_dir/setup-display-states.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  bash "$script_under_test" >"$setup_states_output" 2>&1; then
  fail "expected setupDisplay contract with missing states to fail by default"
fi
assert_contains "$setup_states_output" "serve-feral-player: invalid setupDisplay player contract"

# A complete setupDisplay v1 contract passes the default gate.
cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"setupDisplay":{"version":1,"requestKey":"request","states":["softap_qr","joining","join_failed","updating","claim_qr","ready","hidden"],"acceptedResponse":{"ok":true}}}}
EOF
run_ready_server bash "$script_under_test"

# The dev/legacy escape hatch: setupDisplay validation off, no manifest at all.
rm -f "$root_dir/ffos-player-contract.json"
FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=0 run_ready_server bash "$script_under_test"

# Mint-pairing-gate scenarios run with the setupDisplay gate disabled so each
# assertion isolates the mint-pairing validation path.
contract_output="$tmp_dir/missing-contract.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=0 \
  FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT=1 \
  bash "$script_under_test" >"$contract_output" 2>&1; then
  fail "expected missing mint-pairing player contract to fail when validation is required"
fi
assert_contains "$contract_output" "serve-feral-player: missing player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}}}
EOF
missing_python_output="$tmp_dir/missing-python.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=0 \
  FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT=1 \
  PATH="$bin_dir" \
  /bin/bash "$script_under_test" >"$missing_python_output" 2>&1; then
  fail "expected required mint-pairing player contract validation to fail without python3"
fi
assert_contains "$missing_python_output" "serve-feral-player: required binary not found: python3"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"other":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}},"loose":"mintPairingDisplay"}
EOF
wrong_path_output="$tmp_dir/wrong-contract-path.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=0 \
  FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT=1 \
  bash "$script_under_test" >"$wrong_path_output" 2>&1; then
  fail "expected wrong-path mint-pairing player contract to fail when validation is required"
fi
assert_contains "$wrong_path_output" "serve-feral-player: invalid player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code"],"acceptedResponse":{"ok":true}}}}
EOF
missing_state_output="$tmp_dir/missing-state-contract.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=0 \
  FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT=1 \
  bash "$script_under_test" >"$missing_state_output" 2>&1; then
  fail "expected missing-state mint-pairing player contract to fail when validation is required"
fi
assert_contains "$missing_state_output" "serve-feral-player: invalid player contract manifest"

cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":false}}}}
EOF
wrong_response_output="$tmp_dir/wrong-response-contract.log"
if FF_PLAYER_STATIC_ROOT="$root_dir" \
  FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=0 \
  FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT=1 \
  bash "$script_under_test" >"$wrong_response_output" 2>&1; then
  fail "expected wrong-response mint-pairing player contract to fail when validation is required"
fi
assert_contains "$wrong_response_output" "serve-feral-player: invalid player contract manifest"

# Image-shaped manifest: both contracts present, both gates on.
cat >"$root_dir/ffos-player-contract.json" <<'EOF'
{"contracts":{"setupDisplay":{"version":1,"requestKey":"request","states":["softap_qr","joining","join_failed","updating","claim_qr","ready","hidden"],"acceptedResponse":{"ok":true}},"mintPairingDisplay":{"version":1,"requestKey":"request","states":["pairing_code","request_received","creating_token","hidden"],"acceptedResponse":{"ok":true}}}}
EOF
FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT=1 run_ready_server bash "$script_under_test"

assert_contains "$notify_args" "--ready"
assert_contains "$notify_args" "feral-player static ready on http://127.0.0.1:${port}/"
[ -s "$pid_file" ] || fail "expected fake darkhttpd pid file to be written"

# Cache policy (#234): with a --header-capable darkhttpd the server must be
# started with a global "Cache-Control: no-cache" — darkhttpd applies custom
# headers to 404s too, which is what stops a mid-swap negative response from
# being heuristically cached by a live Chromium session.
assert_contains "$server_args" "--header"
assert_contains "$server_args" "Cache-Control: no-cache"

# A darkhttpd without --header support (probe finds no such flag in its usage
# text) must degrade to headerless serving — passing the flag anyway would
# fail startup and put the unit in a restart loop, which is strictly worse
# than serving without cache headers.
FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=1 \
FF_PLAYER_TEST_HEADER_SUPPORT=0 \
  run_ready_server bash "$script_under_test"
if grep -Fq -- "--header" "$server_args"; then
  fail "legacy darkhttpd without --header support must be started without the flag"
fi

echo "test-serve-feral-player: OK"
