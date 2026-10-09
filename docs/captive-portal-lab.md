# Test iOS captive-window presentation

This is a diagnostic experiment for [the iOS 26.6.1 reproduction](https://github.com/feral-file/feral-file/issues/3469#issuecomment-5549384829), not a firmware fix. FFOS 2.0.4 answered the phone's captive probe in 95 ms. iOS reached `PresentingUI` about 1.4 seconds after association, then logged `waiting for UI` until Safari entered the foreground roughly 145 seconds later.

The open question is whether a different HTTP response changes iOS's presentation behavior after a Camera QR join. The logs establish where it waited, not whether every other response would take that same path.

The lab serves the same small test page for three response modes:

| Mode | Response to Apple's HTTP probe | Question |
| --- | --- | --- |
| `relative` | 302, `Location: /` | Does a control with the current redirect shape reproduce the wait? |
| `absolute` | 302 to the actual hotspot's numeric HTTP root | Does an explicit destination change presentation? |
| `html` | 200 with the test page | Does content-based detection change presentation? |

The control uses a different HTTP server and a smaller page than production. It must reproduce the original failure before results from the other modes are useful. None of these modes forces an iOS UI action.

## Check without a phone

The HTTP tests require only Python's standard library. They exercise real HTTP/1.0 requests, response differences, body-free HEAD replies, form rejection, minimal logs, and process expiry with an incomplete client request.

```sh
python3 -B scripts/test-captive-portal-lab.py -v
python3 scripts/captive-portal-lab.py --mode absolute \
  --portal-url http://127.0.0.1:18080 --duration 60
```

The default binds loopback and changes no networking. A browser can load `http://127.0.0.1:18080/`; a probe request to `/hotspot-detect.html` shows the selected response.

The network test uses the production Go portal handler, a copy of FF1's captive redirect rule, and a client in a second network namespace. It checks both the gateway and synthetic captive address, all three modes, expiry while the lab is running, recovery after `SIGKILL`, and immediate recovery after `SIGTERM`. The original captive rule must remain unchanged.

On Linux, with Go, Python 3, `ip`, `nft`, `unshare`, and `nsenter` installed, build from the controller module and run from the repository root:

```sh
(cd components/feral-controld && CGO_ENABLED=0 go build \
  -o /tmp/ff1-portal-fixture ../../scripts/fixtures/captive-portal/main.go)
sudo python3 -B scripts/test-captive-portal-network.py \
  --portal-fixture /tmp/ff1-portal-fixture
```

The test creates isolated namespaces and refuses to modify one with existing interfaces or firewall tables. Namespace teardown removes its interfaces and rules. NetworkManager AP metadata is supplied by a fixture; radio transitions, DHCP, and phone behavior are outside this test. Both test suites run in CI.

These network checks passed on physical FF1 hardware running 2.0.4. A separate desktop Chromium check confirmed that a plain HTTP probe emits no visibility beacon and a rendered test page emits one. These results validate the experiment; they do not establish how iOS will present it.

## Run a brief hardware trial

1. Enable temporary SSH using `ff-cli ssh enable --device <name> --pubkey <public-key-file> --ttl 15m`. Copy `scripts/captive-portal-lab.py` to `/tmp/captive-portal-lab.py` on FF1. Use the existing verified SSH host identity.
2. While FF1 is still online, start the following transient unit over SSH. Substitute the device's Wi-Fi interface if it differs. The process waits for setup mode and reads the actual hotspot gateway from NetworkManager.

```sh
sudo systemd-run --unit=ff1-portal-lab --collect \
  --property=RuntimeMaxSec=240 \
  /usr/bin/python3 -u /tmp/captive-portal-lab.py \
  --mode relative --bind 0.0.0.0 --redirect-interface wlp2s0 \
  --wait-for-ap 90 --duration 90
```

3. Trigger `startWifiSetup` through the existing device control interface. Allow the test runner to observe the new AP before scanning. For comparisons, forget the saved setup network on the phone before each join, begin from unlocked Camera, and keep cellular/VPN settings and the QR entry point the same.
4. Scan the on-screen Wi-Fi QR, accept joining, and remain in Camera for 15 seconds. Record whether the native login window appears without another action. If it does not, open Safari as a positive control. Do not type a URL before recording the result.
5. The page says **FF1 connection test** and collects no Wi-Fi details. After the 90-second test window, fresh HTTP connections reach the normal setup portal again. Complete ordinary setup to restore FF1's home-network connection, retrieve `journalctl -u ff1-portal-lab`, and remove the temporary script. Disable SSH through `ff-cli ssh disable --device <name>` when finished.

Repeat with `absolute` and `html` only if the `relative` control reproduces the original wait. A result worth following up is a variant that automatically presents the sheet where the control does not, followed by a second fresh-join reproduction. A visible page reached by opening Safari is a recovery result, not a pass for automatic presentation.

The phone need not be attached by USB for these trials. The server records a `visible_page_beacon` only when page JavaScript reports `document.visibilityState === 'visible'`. That confirms the page ran in a browser context; it does **not** identify the browser, prove the native sheet was visible to the user, or prove automatic opening. The tester's observation remains necessary. If results are ambiguous, collect another short iPhone system log around that join.

## Temporary effects and limits

- Redirection is opt-in and requires root, an active NetworkManager AP with an `FF1-` SSID, and binding the lab listener to `0.0.0.0`. A normal station connection is rejected.
- A separate nftables table intercepts only HTTP port 80 to the hotspot gateway or the existing captive address on that Wi-Fi interface. It does not change the existing captive table or intercept TLS. Its port match expires after the trial duration, even if the process is killed; normal shutdown deletes the table. An abrupt kill may leave an inert table with the `ff1_portal_lab_` prefix, which an operator can inspect and remove.
- The ordinary portal and controller stay running. POST/PUT/PATCH/DELETE requests to the lab are rejected without reading or forwarding their bodies. No request URLs, query strings, headers, passwords, or client addresses are intentionally logged.
- HTTP response changes and page beacons can be verified without iOS. Automatic native-sheet presentation cannot. These trials do not test password submission, successful provisioning, Android compatibility, or a production release.
- CAPPORT is a separate hypothesis. [Apple's documented method](https://developer.apple.com/news/?id=q78sq5rv) advertises a TLS-protected JSON API through DHCP option 114. An HTTP setup-page URL or an untrusted certificate does not implement that method. The saved logs do not establish that CAPPORT would override the observed UI gate.

If the tested response variants all wait until Safari opens, do not ship one as a popup fix. Preserve that negative result and evaluate CAPPORT with a valid certificate, or a phone-side setup flow, as a separate experiment.
