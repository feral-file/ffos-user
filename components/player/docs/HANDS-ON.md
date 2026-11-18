# FF1-SKYZ2E3A native preview, 2026-11-18

The screen is awake and `ff-native-preview.service` runs the native player over
Cage at 3840×2160/60 Hz. This is an isolated development preview, **not a complete
replacement of normal phone/ff-cli control**. Those commands still reach the
original installed player. The remaining command/session/watchdog/offline
migration is listed in `../../../docs/native-player-migration.md`.

The preview initially restored the existing work “24” using a one-shot export
through the old player's browser APIs. The original profile was not modified.
No persisted rendering success or verification annotation was imported.

## Try an artwork through the native protocol

The development SSH tunnel on Anh's Mac exposes the preview socket at
`/tmp/ff-native-office-preview-v2.sock`. From `components/player`:

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
portfolio/overnight/physical-panel acceptance, and the new Vulkan/retained-frame
matched-Chrome benchmark remain incomplete. Do not infer full feature coverage
from this preview or the passing synthetic suite.
