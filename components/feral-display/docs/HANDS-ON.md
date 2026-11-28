# FF1-SKYZ2E3A native cutover, 2026-11-18

The ordinary kiosk now runs the native player. Phone and FF-CLI casts enter
controld's authenticated route and reach the host over the local protocol.
The preview below is historical; it is stopped. See CUTOVER.md for current
results, installation paths, rollback and remaining limitations.

## Current installation and rollback

Runtime: `/usr/lib/feral-display`; resources: `/usr/share/feral-display`.
State: `/home/feralfile/.state/player/state.sqlite`.
Socket: `/run/user/1000/ff-player/player.sock`.
`chromium-kiosk.service` now launches Cage and the native host, preserving its
name for watchdog/sudoers. Controld/watchdog are independent of renderer health.
`chromium-ready.target` observes the native socket, not CDP.

Run normal FF-CLI from Anh's ff1-cli checkout (enrolled alias `office`):

```sh
node dist/index.js display https://feed.feralfile.com/api/v1/playlists/1bbefbc9-0226-439a-921b-12c6cf6a809e -d office
```

This is Social Codes from the Feral File channel. CLI submits the document
inline; a blank native playlist URL is expected. Acceptance is not visual proof.

A fresh legacy browser API export was validated/imported before startup. Original
profile files remain untouched. Rollback over authorized SSH:

```sh
/home/feralfile/native-rollback-20261118/rollback.sh
```

That directory contains system.tar, legacy-export.json and hashes. Rollback
restores original binaries/session, starts the old services and retains native
state for diagnosis. It does not silently back-port new native preferences or
history. Old server/CDP readiness paths are rollback-only; remove them after
portfolio/offline/upgrade acceptance moves rollback to the previous FFOS image.
No normal playback code depends on them. Leave the display on for Anh's tests.

## Historical preview evidence (superseded deployment)

The preview initially restored the existing work “24” using a one-shot export
through the old player's browser APIs. The original profile was not modified.
No persisted rendering success or verification annotation was imported.

## Try an artwork through the native protocol

The development SSH tunnel on Anh's Mac exposes the preview socket at
`/tmp/ff-native-office-preview-v2.sock`. From `components/feral-display`:

```sh
python3 tools/playerctl.py --socket /tmp/ff-native-office-preview-v2.sock playback.replace \
  '{"playlist":{"items":[{"source":"https://example.org/artwork","title":"My artwork","duration":0}]}}'
python3 tools/playerctl.py --socket /tmp/ff-native-office-preview-v2.sock state.get
```

Replace the example source with the actual artwork URL. A receipt reports
acceptance. Inspect `selected`, `presented`, `composition`, and `renderer` for
execution and host composition; these do not prove physical output or arbitrary
artwork correctness. Direct development calls have no controld verification
annotation. Phone commands do not reach this preview.

The tunnel depends on temporary SSH access. Renew with ff1-cli's
`ssh enable --pubkey ~/.ssh/id_ed25519.pub --ttl 30m -d office`. A fresh tunnel
can use any unused local socket path:

```sh
ssh -N -L /tmp/ff-native-preview-new.sock:/run/user/1000/ff-native-preview/player.sock \
  feralfile@192.168.31.52
```

Use that socket path with playerctl. Follow normal SSH host-key validation; the
address is a discovery observation, not a permanent device identity.

## Installed development paths and rollback

- Runtime/resources: `/home/feralfile/ff-native-stage/usr/`.
- Preview state: `/home/feralfile/.state/native-office-preview/`.
- Remote socket: `/run/user/1000/ff-native-preview/player.sock`.
- Logs: `journalctl --user -u ff-native-preview`.
- Stop preview: `systemctl --user stop ff-native-preview` over SSH. This reveals
  the original kiosk; its controld, watchdog, profile and service files remain
  unchanged. Reboot also returns to the original session.

The transient preview service has a 3 GiB aggregate memory limit, 512-task
limit, and at most three restart attempts within five minutes. It is not enabled
as the boot session. CEF's sandbox remains enabled. The private libmpv worker
uses bubblewrap, a 3 GiB address-space cap and four decoder threads.

## Checked and remaining

14 Linux CTest groups passed. At 4K, the device suite passed display controls,
12 setup states, exact Wi-Fi/portal/claim QR decoding, static cursor and timed
labels, five input checks, image orientation/animation/alpha pixels, three GLB
loading/interaction checks, 13 media checks, and 65-second covered playback.
Transparent Grit passed two occurrences and the command-responsiveness check.
See `evidence/office-20261118` for results and hashes.

QR and tombstone use bundled PP Mori and the existing layout rules, with the
approved exhibition-name addition and optional curator content. See
`DEVICE-UI.md` for exact matching scope. The 4096-square H.264 worker crash was
fixed by bounding decoder threads; sustained software-decoding frame pacing
remains unqualified.

Normal phone casting/input/sleep coordination, playlist refresh/scheduling
ownership, offline resource handling, full session/watchdog activation, full
portfolio/overnight/physical-panel acceptance remain incomplete. The matched
4K benchmark is in OFFICE-PACING.md; the offscreen host remains slower than Chrome. Do not infer full feature coverage
from this preview or the passing synthetic suite.
