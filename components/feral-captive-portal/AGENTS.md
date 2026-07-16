# Agent Notes: `feral-captive-portal`

Scope: `components/feral-captive-portal/**`

## Purpose

Standalone SoftAP + captive portal for FF1 Wi-Fi provisioning. Intentionally **does not**
modify `feral-setupd`, `feral-controld`, or existing user systemd units.

## Boundaries

- Owns: hotspot lifecycle, portal HTTP server, nmcli scan/connect for provisioning.
- Does not own: relayer topic assignment, BLE commands, setup phase, player UI, OTA.
- Uses the same nmcli patterns as `feral-setupd/src/wifi_utils.rs` (delete stale profile,
  `nmcli device wifi connect`, tab-separated `device wifi list`).

## Runtime layout on device

- Install root: `/opt/feral/captive-portal/`
- Logs: `/home/feralfile/.logs/feral-captive-portal.log`
- State: `/home/feralfile/.state/captive-portal/` (`previous-wifi-connection`, `portal.pid`,
  `connect-succeeded`, `hotspot-active`)
- Boot: `feral-captive-portal-boot-cleanup.service` is enabled on install and restores
  normal Wi-Fi after reboot.
- Manual: `scripts/start-captive-portal.sh` and `scripts/stop-captive-portal.sh`
- NM profile name: `FeralCaptivePortal` (configurable)
- Hotspot SSID: `/etc/hostname`

## Operational hazards

1. **Single radio:** scanning while AP is up is driver-dependent. `wifi-manager.sh` drops
   the hotspot briefly when a rescan returns no networks.
2. **Connect path:** hotspot is stopped before `nmcli device wifi connect` because the
   same interface cannot reliably stay in AP mode and join a home SSID at once.
3. **dnsmasq:** NetworkManager shared mode fails without the `dnsmasq` package installed.
4. **Port 80:** the portal server binds `0.0.0.0:80`; the user unit grants
   `CAP_NET_BIND_SERVICE` and `CAP_NET_ADMIN` for redirect rules.

## Verification

```sh
python3 -m py_compile components/feral-captive-portal/server/portal_server.py
shellcheck components/feral-captive-portal/scripts/*.sh
```

On FF1 after install: join the hotspot from a phone, confirm captive portal opens, pick a
network, and verify `nmcli` shows the home SSID active afterward.
