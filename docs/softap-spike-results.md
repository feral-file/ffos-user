# SoftAP Hardware Spike Results (issue #3469, P0)

Status: **template — hardware pass pending.** Software development proceeded on the
pre-declared automatic gate default (Option A, NetworkManager hotspot) per the approved
plan; this document must be completed on real FF1 hardware before the cutover build ships.

## Gate criteria (pre-declared, P0.4 — automatic, no human sign-off)

Default = **Option A (NM hotspot + NM-spawned dnsmasq + Go portal in controld)**.
Switch to Option B (standalone hostapd + dnsmasq) **if and only if** the spike shows NM AP:

- (a) cannot hold the captive portal reachable long enough to submit credentials, **or**
- (b) does not recover the AP cleanly after a failed join attempt.

If the polkit test fails, ship the polkit rule (see P5.2; a proactive rule is already in
the image branch) and **retest before concluding** — a polkit denial alone does not flip
the gate.

The `softap` package's `Backend` interface is the containment boundary: flipping A→B
later touches only that package.

## P0.2 — Daemon-context AP raise

Run via `systemd-run --user` (or a stub `--user` unit) — **not** interactive SSH, which
counts as an active polkit session and masks the `settings.modify.system` /
`org.freedesktop.NetworkManager.wifi.share` requirement.

| Check | Result |
|---|---|
| `nmcli device wifi hotspot` succeeds from service context without the polkit rule | _pending_ |
| …with the shipped polkit rule (if needed) | _pending_ |
| Catch-all dnsmasq drop-in serves `address=/#/192.0.2.1` (TEST-NET, not the gateway IP: Samsung One UI skips captive detection when probe hostnames resolve to private IPs) | _pending_ |
| nftables redirect `192.0.2.1:80/443 → local portal` active | _pending_ |
| Portal binds :80 via `net.ipv4.ip_unprivileged_port_start=80` sysctl | _pending_ |

## P0.3 — Phone matrix

Phones: iPhone (state iOS version), Pixel (Android version), budget Samsung (One UI version).

Per phone, record:

| Check | iPhone | Pixel | Samsung |
|---|---|---|---|
| `WIFI:` QR join works | _pending_ | _pending_ | _pending_ |
| Captive portal auto-opens (probe → 302) | _pending_ | _pending_ | _pending_ |
| Manual fallback `http://192.0.2.1` reachable | _pending_ | _pending_ | _pending_ |
| AP behavior during STA join attempt (does the join kill the AP?) | _pending_ | _pending_ | _pending_ |
| NM restores AP after failed join | _pending_ | _pending_ | _pending_ |
| Phone re-associates to the AP after the bounce | _pending_ | _pending_ | _pending_ |
| Wrong-password round-trip time (submit → error visible on portal/TV) | _pending_ | _pending_ | _pending_ |

## Gate verdict

_pending — record Option A confirmed or Option B adopted, citing which criterion flipped it._
