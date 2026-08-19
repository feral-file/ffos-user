#!/usr/bin/env bash
set -u

SYS_CLASS_NET_ROOT="${FF_SYS_CLASS_NET_ROOT:-/sys/class/net}"

warn() {
    echo "WARN: enable-wake-on-lan: $*" >&2
}

enable_networkmanager_profiles() {
    local profiles

    if ! command -v nmcli >/dev/null 2>&1; then
        warn "nmcli is unavailable; Ethernet profile policy was not persisted"
        return
    fi

    if ! profiles=$(nmcli -t -f UUID,TYPE connection show 2>/dev/null); then
        warn "could not enumerate NetworkManager connection profiles"
        return
    fi

    local uuid type
    while IFS=: read -r uuid type; do
        [ "$type" = "802-3-ethernet" ] || continue
        if ! sudo -n nmcli connection modify uuid "$uuid" \
            802-3-ethernet.wake-on-lan magic; then
            warn "could not persist magic-packet wake for profile $uuid"
        fi
    done <<< "$profiles"
}

enable_ethernet_interfaces() {
    local device_path interface details supported wakeup_path

    if ! command -v ethtool >/dev/null 2>&1; then
        warn "ethtool is unavailable; Ethernet wake was not activated"
        return
    fi

    for device_path in "$SYS_CLASS_NET_ROOT"/*; do
        [ -d "$device_path" ] || continue
        [ -e "$device_path/device" ] || continue
        [ ! -e "$device_path/wireless" ] || continue

        interface=${device_path##*/}
        if ! details=$(sudo -n ethtool "$interface" 2>/dev/null); then
            warn "could not read Wake-on-LAN capabilities for $interface"
            continue
        fi
        supported=$(awk '/Supports Wake-on:/ { print $3; exit }' <<< "$details")
        case "$supported" in
            *g*) ;;
            *)
                warn "$interface does not advertise magic-packet wake support"
                continue
                ;;
        esac

        if ! sudo -n ethtool -s "$interface" wol g; then
            warn "could not activate magic-packet wake on $interface"
        fi

        wakeup_path="$device_path/device/power/wakeup"
        if [ -e "$wakeup_path" ] && \
            ! printf enabled | sudo -n tee "$wakeup_path" >/dev/null; then
            warn "could not enable the PCI wake source for $interface"
        fi
    done
}

# NetworkManager owns reconnect/reboot persistence; ethtool applies the policy
# to the live NIC, while the PCI wake flag covers suspend-capable platform paths.
# Every leg is best-effort because an unsupported or absent adapter must never
# block the FF OS user-session startup sequence.
enable_networkmanager_profiles
enable_ethernet_interfaces
