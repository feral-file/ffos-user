# feral-captive-portal

Standalone SoftAP + captive portal for FF1 Wi-Fi provisioning.

This package is **independent** of `feral-setupd`, `feral-controld`, and the rest of the
runtime stack. It does not modify existing components. Install it side by side and start
`feral-captive-portal.service` when you want hotspot-based setup.

## What it does

1. Reads the device name from `/etc/hostname` (for example `FF1-X45K6QSZ`).
2. Starts a WPA2 hotspot with that SSID (default password `feralfile-setup`).
3. Serves a captive portal web app on port 80.
4. Lists nearby Wi-Fi networks and accepts SSID + password.
5. Tears down the hotspot and connects FF1 to the chosen network with `nmcli`.

Phones that join the hotspot should get a captive-portal prompt (Android/iOS probes are
handled). The portal UI is also reachable at `http://10.42.0.1/` while the hotspot is up.

## Layout

```
components/feral-captive-portal/
├── config/                 # dnsmasq + NM connection templates
├── scripts/                # hotspot orchestration + nmcli helpers
├── server/portal_server.py # HTTP API + static UI + captive probes
├── web/                    # Portal front-end
├── systemd/                # User unit (copy on install)
└── install.sh              # Deploy to /opt/feral/captive-portal
```

## Requirements on FF1

Install once on the device (needs `sudo`):

```sh
sudo pacman -Sy --needed dnsmasq networkmanager python3
```

`hostapd` is not required when using NetworkManager AP mode. `dnsmasq` is required for
NetworkManager `ipv4.method=shared` (DHCP + DNS for hotspot clients).

## Install

From the repository root on your workstation:

```sh
./components/feral-captive-portal/install.sh FF1-X45K6QSZ
```

Or copy manually:

```sh
sudo rsync -av components/feral-captive-portal/ /opt/feral/captive-portal/
sudo install -m 755 /opt/feral/captive-portal/scripts/*.sh /opt/feral/captive-portal/scripts/
sudo mkdir -p /etc/NetworkManager/dnsmasq-shared.d
sudo cp /opt/feral/captive-portal/config/dnsmasq-shared.d/feral-captive-portal.conf \
  /etc/NetworkManager/dnsmasq-shared.d/
sudo systemctl restart NetworkManager
```

## Boot behavior

Install enables **only** `feral-captive-portal-boot-cleanup.service` at boot. That unit:

1. Stops any leftover hotspot from a previous provisioning session
2. Restores the saved home Wi-Fi profile when one was recorded
3. Otherwise asks NetworkManager to autoconnect the Wi-Fi interface

The main `feral-captive-portal.service` is **not** enabled at boot. Reboot therefore returns
the FF1 to normal Wi-Fi instead of leaving hotspot mode active.

## Manual control (SSH)

```sh
# Turn hotspot + portal ON
/opt/feral/captive-portal/scripts/start-captive-portal.sh

# Turn hotspot OFF and restore home Wi-Fi
/opt/feral/captive-portal/scripts/stop-captive-portal.sh
```

`start-captive-portal.sh` uses the user systemd unit when possible. It saves the active
home Wi-Fi profile name before bringing the hotspot up so `stop-captive-portal.sh` or reboot
can restore it.

Logs: `/home/feralfile/.logs/feral-captive-portal.log`

### Alternative systemd commands

```sh
systemctl --user start feral-captive-portal.service
systemctl --user stop feral-captive-portal.service
```

Do **not** `systemctl --user enable feral-captive-portal.service` unless you want hotspot
provisioning to start on every boot.

If Wi-Fi connection fails during portal setup, the portal attempts to restore the hotspot so
the phone can retry without rebooting the FF1.

Environment overrides (optional, in `~/.config/feral-captive-portal.env`):

| Variable | Default | Meaning |
|----------|---------|---------|
| `CAPTIVE_PORTAL_PORT` | `8090` | HTTP listen port (`iptables` redirects :80 → this port) |
| `CAPTIVE_PORTAL_WIFI_IFACE` | auto | Wi-Fi interface (`wlp2s0`, etc.) |
| `CAPTIVE_PORTAL_CONN_NAME` | `FeralCaptivePortal` | NM connection profile name |
| `CAPTIVE_PORTAL_HOTSPOT_PASSWORD` | `feralfile-setup` | Hotspot WPA2 password |
| `CAPTIVE_PORTAL_STOP_ON_SUCCESS` | `1` | Stop service after successful join |

Hotspot password defaults to `feralfile-setup` because this Wi-Fi stack does not support
open/WEP AP mode through NetworkManager.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/device` | Device hostname / hotspot SSID |
| `GET` | `/api/networks` | Scan and list Wi-Fi networks |
| `GET` | `/api/status` | Hotspot + STA connection status |
| `POST` | `/api/connect` | JSON `{"ssid":"...","password":"..."}` |

Captive-portal probe paths (`/generate_204`, `/hotspot-detect.html`, etc.) redirect to `/`.

## Coexistence with feral-setupd

Both may use `nmcli` on the same interface. Do **not** run captive portal and BLE Wi-Fi
setup at the same time. This package is intended as a spike / alternative path for issue
#3469, not a parallel provisioning surface in production yet.

## Uninstall

```sh
systemctl --user disable --now feral-captive-portal.service
systemctl --user disable --now feral-captive-portal-boot-cleanup.service
rm -f ~/.config/systemd/user/feral-captive-portal.service
rm -f ~/.config/systemd/user/feral-captive-portal-boot-cleanup.service
sudo rm -f /etc/NetworkManager/dnsmasq-shared.d/feral-captive-portal.conf
sudo rm -rf /opt/feral/captive-portal
systemctl --user daemon-reload
sudo systemctl restart NetworkManager
```
