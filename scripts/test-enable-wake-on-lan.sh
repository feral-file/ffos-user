#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script_under_test="$repo_root/users/feralfile/scripts/enable-wake-on-lan.sh"
start_services="$repo_root/users/feralfile/.start-services.sh"
file_permissions="$repo_root/users/feralfile/.file_permissions.sh"
unit_file="$repo_root/users/feralfile/systemd-services/enable-wake-on-lan.service"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "test-enable-wake-on-lan: $*" >&2
  exit 1
}

grep -Fq \
  'systemctl --user start --no-block "enable-wake-on-lan.service" ||' \
  "$start_services" || fail "Wake-on-LAN unit is not started asynchronously"
if grep -Eq '^/home/feralfile/scripts/enable-wake-on-lan\.sh' "$start_services"; then
  fail "Wake-on-LAN helper must not run synchronously in the startup path"
fi
grep -Fq 'TimeoutStartSec=15' "$unit_file" || \
  fail "Wake-on-LAN unit does not bound potentially wedged system commands"
grep -Fq 'ExecStart=/home/feralfile/scripts/enable-wake-on-lan.sh' "$unit_file" || \
  fail "Wake-on-LAN unit does not execute the shipped helper"
grep -Fq 'chmod +x /home/feralfile/scripts/enable-wake-on-lan.sh' \
  "$file_permissions" || fail "device install does not make the helper executable"

fake_bin="$tmp_dir/bin"
sys_class_net="$tmp_dir/sys/class/net"
calls="$tmp_dir/calls"
mkdir -p "$fake_bin" "$sys_class_net/enp1s0/device/power" \
  "$sys_class_net/enp2s0/device/power" "$sys_class_net/wlp2s0/device" \
  "$sys_class_net/wlp2s0/wireless"
: > "$calls"
printf disabled > "$sys_class_net/enp1s0/device/power/wakeup"
printf disabled > "$sys_class_net/enp2s0/device/power/wakeup"

cat > "$fake_bin/sudo" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = "-n" ] && shift
exec "$@"
EOF

cat > "$fake_bin/nmcli" <<'EOF'
#!/usr/bin/env bash
if [ "$*" = "-t -f UUID,TYPE connection show" ]; then
  printf '%s\n' \
    'ethernet-uuid:802-3-ethernet' \
    'wifi-uuid:802-11-wireless'
  exit 0
fi
printf 'nmcli %s\n' "$*" >> "$FF_WOL_TEST_CALLS"
EOF

cat > "$fake_bin/ethtool" <<'EOF'
#!/usr/bin/env bash
if [ "$#" -eq 1 ]; then
  case "$1" in
    enp1s0) printf '%s\n' 'Supports Wake-on: pumbg' 'Wake-on: d' ;;
    enp2s0) printf '%s\n' 'Supports Wake-on: d' 'Wake-on: d' ;;
    *) exit 1 ;;
  esac
  exit 0
fi
printf 'ethtool %s\n' "$*" >> "$FF_WOL_TEST_CALLS"
EOF

chmod +x "$fake_bin/sudo" "$fake_bin/nmcli" "$fake_bin/ethtool"

PATH="$fake_bin:$PATH" \
FF_SYS_CLASS_NET_ROOT="$sys_class_net" \
FF_WOL_TEST_CALLS="$calls" \
  bash "$script_under_test"

grep -Fxq \
  'nmcli connection modify uuid ethernet-uuid 802-3-ethernet.wake-on-lan magic' \
  "$calls" || fail "Ethernet NetworkManager profile was not persisted"
if grep -Fq 'wifi-uuid' "$calls"; then
  fail "Wi-Fi profile must not be changed"
fi
grep -Fxq 'ethtool -s enp1s0 wol g' "$calls" || \
  fail "supported Ethernet interface was not armed"
if grep -Fq 'ethtool -s enp2s0' "$calls"; then
  fail "unsupported Ethernet interface must not be armed"
fi
if grep -Fq 'wlp2s0' "$calls"; then
  fail "wireless interface must not be inspected or changed"
fi
[ "$(cat "$sys_class_net/enp1s0/device/power/wakeup")" = "enabled" ] || \
  fail "PCI wake source was not enabled"
[ "$(cat "$sys_class_net/enp2s0/device/power/wakeup")" = "disabled" ] || \
  fail "unsupported interface PCI wake source must stay disabled"

echo "test-enable-wake-on-lan: OK"
