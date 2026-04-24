#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SCRIPT_UNDER_TEST="$ROOT_DIR/users/feralfile/scripts/serve-ff-player-static.sh"
UNIT_FILE="$ROOT_DIR/users/feralfile/systemd-services/feral-ff-player-static.service"

fail() {
	echo "test-serve-ff-player-static: $*" >&2
	exit 1
}

assert_contains() {
	local file="$1"
	local needle="$2"
	grep -Fq -- "$needle" "$file" || fail "expected '$needle' in $file"
}

test_unit_contract() {
	assert_contains "$UNIT_FILE" "Type=notify"
	assert_contains "$UNIT_FILE" "NotifyAccess=all"
}

test_missing_tree_fails_fast() {
	local tmp_dir missing_root output_file
	tmp_dir="$(mktemp -d)"
	missing_root="$tmp_dir/missing"
	output_file="$tmp_dir/output.log"

	if FF_PLAYER_STATIC_ROOT="$missing_root" \
		bash "$SCRIPT_UNDER_TEST" >"$output_file" 2>&1; then
		fail "expected missing bundle to fail"
	fi

	assert_contains "$output_file" "serve-ff-player-static: missing static tree"
	rm -rf "$tmp_dir"
}

test_ready_handshake() {
	local tmp_dir root_dir bin_dir pid_file notify_file notify_args output_file port
	tmp_dir="$(mktemp -d)"
	root_dir="$tmp_dir/ff-player"
	bin_dir="$tmp_dir/bin"
	pid_file="$tmp_dir/darkhttpd.pid"
	notify_file="$tmp_dir/ready.signal"
	notify_args="$tmp_dir/notify.args"
	output_file="$tmp_dir/output.log"
	port="18080"

	mkdir -p "$root_dir" "$bin_dir"
	cat >"$root_dir/index.html" <<'EOF'
<html><body>FF player static smoke test</body></html>
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

exit 0
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

	chmod +x "$bin_dir/darkhttpd" "$bin_dir/systemd-notify"
	chmod +x "$bin_dir/curl"

	FF_PLAYER_STATIC_ROOT="$root_dir" \
	FF_PLAYER_STATIC_PORT="$port" \
	FF_PLAYER_READY_TIMEOUT_SECONDS=5 \
	FF_PLAYER_TEST_ROOT="$root_dir" \
	FF_PLAYER_TEST_PORT="$port" \
	FF_PLAYER_TEST_PID_FILE="$pid_file" \
	FF_PLAYER_TEST_NOTIFY_FILE="$notify_file" \
	FF_PLAYER_TEST_NOTIFY_ARGS="$notify_args" \
	PATH="$bin_dir:$PATH" \
	bash "$SCRIPT_UNDER_TEST" >"$output_file" 2>&1

	assert_contains "$notify_args" "--ready"
	assert_contains "$notify_args" "ff-player static ready on http://127.0.0.1:${port}/"
	[ -s "$pid_file" ] || fail "expected fake darkhttpd pid file to be written"
	rm -rf "$tmp_dir"
}

main() {
	test_unit_contract
	test_missing_tree_fails_fast
	test_ready_handshake
	echo "test-serve-ff-player-static: OK"
}

main
