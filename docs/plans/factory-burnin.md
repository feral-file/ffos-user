# Factory burn-in: a hidden boot entry on the installed FF1

Status: plan rev 2, accepted 2026-11-27 (rev 1 adversarial review: revise, fixed here). Stages 1–4 implemented together (owner's call: one bench at the end): ffos-user feat/factory-burnin, ffos feat/factory-burnin-entry. Spans ffos-user and ffos.

## Goal and decisions already made

An engineer at the factory installs a unit the usual way, reboots it, holds
a key at power-on to open the boot menu, and picks **"FF1 - Factory
burn-in"**. The unit runs the unit checks once (brightness, speakers,
Bluetooth, Wi-Fi), then loops display and load tests until the power is
pulled. A user who never opens the boot menu never sees it, as with "FF1 -
Factory Reset" today.

Decided by the product owner:

1. The entry lives in the **installed system's** boot menu (NVMe ESP), not
   on the installer USB stick.
2. The **factory ISO is deleted**: the antenna-placement repeat readings are
   not needed.
3. No pass duration: the screen shows **how long it has run**, nothing more.
4. The **24H soak test is deleted** and its useful parts are folded in.
5. Growth of the normal image stays within **tens of MB** (target: ≤ 12 MB).

## 1. Current-state summary

**Installed boot (ffos).** systemd-boot on the NVMe ESP:
- `loader.conf` has `default arch.conf`, `timeout 0`, `editor no`.
- Two entries, `arch.conf` and `factory_reset.conf` (`rollback=factory`).
  They share `$FF1_KERNEL_OPTS` from `root/scripts/ff1-boot-options.sh`.
- The entries are written by **three copies** of the same heredocs:
  - `install-to-disk.sh` (manual install);
  - `auto-install.sh` (Auto Install);
  - `post-extraction.sh` (every full-image OTA, run from the new image in a
    chroot).

**Installed services.** `feral-kiosk-startup.service` is a system unit,
`WantedBy=multi-user.target`, `ConditionKernelCommandLine=!archisobasedir`.
It runs `.start-services.sh` as feralfile, and that script explicitly starts
every user service:
- controld, sys-monitord, vmagent, player, chromium-kiosk, watchdog.
- It also enables the system timers `feral-updater@03:00` and
  `feral-recovery-update@5:30`, both `WantedBy=timers.target`.

The feral user units exist for `user@1000` only after `.start-services.sh`
bind-mounts `systemd-services/` over `~/.config/systemd/user`. So
`user@1000` can run (PulseAudio for the speaker check) without the kiosk
stack, as long as that script does not run. kiosk-startup pulls
`user@1000.service` directly and orders after `systemd-user-sessions.service`
(which removes `/run/nologin`, see R4).

Display and plymouth:
- cage reaches DRM through seatd (`LIBSEAT_BACKEND=seatd`).
- Plymouth is shown on boot (`quiet splash`) and is ended by
  `plymouth-quit.service`, which `multi-user.target` pulls in.

**Factory checks today.**
- ffos-user `factory-check.sh`: repeat readings, each `unit-checks.sh`
  (brightness DDC/CI, speakers via feralfile's PulseAudio, Bluetooth
  discovery) then `wifi-reading.sh` (wpa_cli scans). The screen text is the
  record, plus JSON on an optional FAT stick. Since #497 it also runs
  `display-check.sh once` on VT 8 after each reading.
- It runs from:
  - the factory ISO (ffos `factory-iso/`, kmscon on tty1, `factory_check`
    build input refused on device branches);
  - a "Factory check" entry on the normal ISO, whose default is Auto Install
    with `timeout 0`.
- `display-check.sh` (boot entries on the factory ISO) shows
  `users/feralfile/display-check/index.html`. That page is a copy of
  toko-batch-2's, with self-measured fps / late-frame / dropped-frame
  counters.

**Soak test.**
- ffos-user `users/soaktest/` (92 MB, of which 90 MB is two 4K HEVC clips)
  holds:
  - WebGL artworks;
  - a CDP/websocat metrics loop (CPU temp via k10temp, fps via rAF, memory);
  - `summary.py`.
- ffos side:
  - a `soak-test` input on three image workflows;
  - an ISO entry;
  - the soaktest user in passwd/shadow/group/sudoers;
  - `usr/local/bin/websocat` (7.3 MB, deleted at build when soak is off);
  - cleanup lines in both installers.

**Docs.** `docs/architecture.md` / `docs/api-design.md` do not cover
factory test modes. This plan adds no D-Bus or wire contract, so neither
needs a new rule. The boot-mode facts go into ffos
`docs/BOOT_DISPLAY_AND_CONSOLE.md`.

## 2. Constraints and invariants

- **Normal boot is untouched.**
  - Default entry, `timeout 0` and `editor no` stay.
  - No unit's behaviour changes on a boot without the burn-in parameter.
  - Nothing the burn-in writes is read by a normal boot.
- **No OTA, no recovery update, no watchdog reboot, no controld during
  burn-in.** A reboot or an image swap mid-test would end it silently. A
  network-connected fielded unit put into burn-in by a curious owner must
  not update or change state.
- **User data and device state stay as they are.**
  - No writes to `/home/feralfile/.state`, the owner/claim state, NM
    connections or the factory snapshot.
  - Burn-in output goes to RAM, an optional FAT stick, and a small
    `/var/log/ff1-burnin/` (the `@log` subvolume, outside the root rollback).
- **Fail open towards the loop.** A check that cannot run says SKIP/FAIL and
  the next phase still starts. A crashed Chromium is restarted and counted.
  The loop runs until the power is pulled.
- **Release rails.**
  - ffos-user's scripts and page ship on the full-image rail
    (`users/feralfile/**`). The ffos units, boot entry and packages are image
    content.
  - Delivery is therefore one full-image version, with the ffos-user ref and
    the ffos ref built together. Nothing here is package-only.
- **Boot entries have one writer.**
  - Adding a third entry to three heredoc copies triples the drift risk. The
    entries move into one function next to `FF1_KERNEL_OPTS`, in
    `ff1-boot-options.sh`, which all three scripts already source.
  - `post-extraction.sh` runs the *new* image's copy, so an OTA writes the new
    entry set.
- **Size.** ≤ 12 MB added to the normal image (§4).

## 3. Risks and unknowns

Bench = needs an FF1. "Now" = can be settled before hardware.

| # | Risk | How it is closed |
|---|---|---|
| R1 | **F2 at power-on may open the firmware setup**, not the systemd-boot menu. systemd-boot opens its menu on any key pressed during its start. | Bench: try F2 and Space on an FF1. The docs and the line instructions name whichever works; Space is the fallback. |
| R2 | **Plymouth keeps the display.** The burn-in target does not pull `multi-user.target`, so `plymouth-quit.service` would never run and cage could not get DRM master. | The burn-in service `Wants=` and `After=` `plymouth-quit.service plymouth-quit-wait.service`. feral-kiosk-startup has only the `After=` because multi-user pulls the quit units; here nothing else does. Pinned in ffos `verify.sh`. Bench confirms. |
| R3 | **Timers still fire.** `basic.target` pulls `timers.target`, which pulls the updater and recovery timers, and NetworkManager may be online. | `ConditionKernelCommandLine=!ff1.burnin` drop-ins on `feral-updater@.service` and `feral-recovery-update.service`, the services rather than the timers, so no path starts them. Pinned in verify.sh: the drop-in files and their exact line. A dependency listing cannot show this, because the services start through the timers' `Unit=`. Runtime proof is bench item 3. |
| R4 | **`user@1000` must start without `multi-user.target`.** tmpfiles creates `/run/nologin`. Only `systemd-user-sessions.service` (multi-user) removes it, and `user@.service`'s PAM stack (`system-login`, `pam_nologin`) refuses while it exists. Without it, no PulseAudio, and the speaker check fails on every unit. | The target pulls `systemd-user-sessions.service`. The burn-in service `Wants=`/`After=` `user@1000.service systemd-user-sessions.service` directly, as kiosk-startup does, not through linger. No feral user unit runs: they appear only when `.start-services.sh` bind-mounts `systemd-services/` over `~/.config/systemd/user`, and that script does not run in burn-in. Bench item 3. |
| R5 | **No firewall in burn-in.** `nftables` is `multi-user`-pulled, NM may join a remembered network on a fielded unit, and the target lists no nftables. | No listening service runs in burn-in (no controld hub, sshd is off by default). Add `nftables.service` to the target anyway: it costs nothing. |
| R6 | **OTA candidate pending.** If burn-in is picked from the menu at the reboot where an OTA armed `arch-candidate.conf` (`LoaderEntryOneShot`), the one-shot is used up and the candidate never boots. The next normal boot's `btrfs-subvolume-manager` deletes `@ota_new` as an orphan. | Harmless: the update is dropped and offered again at the next timer window, and the running image is untouched. Bench once: arm an OTA, burn-in boot, normal boot. Expected: the old image boots, `@ota_new` is gone, and the next update run works. |
| R7 | **Chromium as root.** cage + Chromium in a root service need `--no-sandbox` (as display-check does), unlike the kiosk. | Accepted: offline local page, and only on an explicitly chosen test boot. Alternative: run the page as feralfile inside `user@1000`, as the kiosk does. Decided in implementation review, not here. |
| R8 | **Static pixels for days.** A permanent overlay could leave image retention. | No fixed overlay. The status card (full screen, ~5 s) appears between passes, and the corner status dot and its QR change corner every pass. The QR (≈ 20 vmin, light gray rather than white) still holds one corner for a whole pass (~5 min) right before a Panel Check: the bench looks for a ghost square in the gray field's corners. |
| R9 | **The hang/crash record.** The cmdline sets no `panic=`, so a kernel panic freezes the screen like a hang. A firmware or hardware reset lands in the *default* entry (normal FF1 UI), since the burn-in is chosen only from the menu. | Both are visible on the floor: a running-time counter that stopped, or the normal UI. `/var/log/ff1-burnin/last.json`, written every minute, lets the next burn-in boot show "previous session: ran 6 h 12 m, last OK 14:02". It cannot tell a crash from a power pull; the screen state at the time does. |
| R10 | **Entry on fielded units.** After this ships, every OTA writes the entry on customer devices (`post-extraction.sh` writes the staged boot set and `btrfs-subvolume-manager` promotes it). A factory reset restores the boot set from the install-time `factory_reset_boot` copy, so on a unit installed before this release a reset removes the entry until the next OTA or recovery update. | Same exposure as Factory Reset, and less harmful (no data change). Title says it is for the factory. Accepted; the factory installs from the new image, so it always has the entry. |
| R11 | **The ffos-user/ffos split.** Script and page are in ffos-user, units and entry are in ffos; a mismatched build shows an entry with nothing behind it. | The burn-in service checks for the script and shows a one-line error screen instead of a black one. Both PRs land in one full-image version. |

## 4. Design branches and trade-offs

### 4.1 How the burn-in boot is selected

- **A. `systemd.unit=ff1-burnin.target` on the entry's cmdline (chosen).**
  Only what the target names starts:
  - a short list of system units: NetworkManager, bluetooth, logind,
    `systemd-user-sessions` (removes `/run/nologin`, R4), `user@1000`
    (PulseAudio), seatd, nftables, plymouth-quit;
  - `ff1-burnin.service`.

  The kiosk, owner guard, tailscale and controld never enter the
  transaction. The gating is one place, plus R3's two drop-ins.
- **B. A plain flag (`ff1.burnin`) on a normal boot, with
  `ConditionKernelCommandLine=!ff1.burnin` on every unit to keep off.**
  Rejected: it touches a dozen system and user units, and any unit added
  later runs in burn-in by default.

### 4.2 How the results are shown

- **A. One full-screen page for the whole session (chosen).** One cage +
  Chromium from the start:
  - The page shows the check phase (live per-check progress), then the result
    card, then loops the test passes.
  - The shell side (`ff1-burnin.sh`) runs the checks and writes
    `status.json`. The page, the display-check page and the load works are
    served with `status.json` by a loopback `python3 -m http.server`
    (127.0.0.1:8180), not from `file://`: Chromium's `fetch()` rejects
    `file:` even with `--allow-file-access-from-files`, and both WebGL works
    load their shaders with fetch (uneasy-dream's README says it needs a
    server). nftables does not filter loopback.
  - Chromium draws Chinese, so kmscon, the VT 8 hand-off (#497) and the text
    screen go.
- **B. Keep the kmscon text screen and hand the display back and forth**
  (today's #497). Rejected: two display owners, the riskiest bench unknown
  of #497, and a text screen is unreadable from across a room.

What the page shows:

- **Result card** after the checks.
  - One line per check, with large PASS / FAIL / SKIP / PLAYED (speakers have
    no listener: a person judges them).
  - Device ID (the installed hostname, already FF1-xxxxxxxx), image version,
    time.
  - A **QR code** of a plain-text summary (≤ 600 bytes, ASCII, the verdict on
    the first line; a phone shows it as text). Scanning it with a
    phone replaces photographing the screen.
  - The full JSON also lands on a FAT stick when one is plugged in, as today.
  - The card stays 60 s, then the loop starts.
- **Status card between passes (8 s, full screen).**
  - Background: green if nothing has failed this session, red once anything
    has (latched).
  - It shows:
    - **time running** (h:mm:ss, ticking) and passes completed;
    - worst fps per test, and dropped and late frames in total;
    - max CPU (k10temp Tctl), GPU (amdgpu edge) and NVMe temperatures;
    - Chromium restarts;
    - kernel GPU/NVMe errors (journal -k matches: amdgpu ring timeout / GPU
      reset, nvme timeout / I/O error, MCE);
    - Wi-Fi and Bluetooth adapters still present;
    - the previous session line (R9);
    - the QR, refreshed.
- **During passes:** a small status dot in one corner, green or latched red,
  moving corner each pass, plus the ticking running time inside it and, under
  it, the same QR (redrawn every 10 s), so a scan at any time gives the verdict
  so far. Neither shows during the Panel Check. A frozen
  counter means a hang.

### 4.3 What a pass contains

- **The Panel Check opens each pass** (added 2026-11-27 for
  FF-Acceptance-Packing-Spec v1.0 §1, no key press): 2 min of the
  display-check `?set=panel` loop (black 15 s, gray 15 s) with nothing else
  on screen, once the unit has been on 10 minutes (`/proc/uptime`). The
  status card before it says so. Whoever is at the unit films about 60 s and
  pulls the power while still recording. The same segment gives the spec's
  black-field and gray-field inspection.
- **A. Assembly slides + load (chosen).** Each pass is the nine assembly
  slides (~102 s), then about 3 min of load:
  - two WebGL works from the soak set, about 60 s each (the e-volved shader;
    uneasy-dream, p5). Their frame rate is shown but never judged: GPU-bound
    work at 4K may run below the panel rate on a healthy unit. The slides
    and the video are judged (average under 50 fps, or more than 1 % of the
    clip's frames dropped, latches red);
  - one **4K 10-bit HEVC clip**, looped for 60 s through the kiosk's VA-API
    flags.

  The page measures fps and late frames on all of them, and dropped frames on
  the video with `getVideoPlaybackQuality()`. Each pass takes about 5 min.
- **B. Slides only.** Rejected: they barely load the GPU or the decoder, so
  thermal and decode faults over hours go unseen.
- **C. stress-ng / CPU burn.** Not added: the WebGL and decode load is the
  load an FF1 actually runs, and stress-ng is a new package.

### 4.4 Size budget (normal image)

| Item | Size |
|---|---|
| Page, scripts, units, drop-ins | < 150 KB |
| QR encoder, vendored into the page (MIT, minified) | ≈ 20–50 KB |
| WebGL works (e-volved, uneasy-dream incl. p5.min.js) | ≈ 0.9 MB |
| 4K 10-bit HEVC clip, re-encoded ~25 Mbps, 2–3 s, looped | ≈ 6–10 MB |
| **Total** | **≈ 7–11 MB** |

- Removed from the image: nothing measurable. websocat and the soak files
  were already stripped from non-soak builds.
- Removed from the repos: about 90 MB of clips in ffos-user and the 7.3 MB
  websocat in ffos.
- TransparentGrit (0.7 MB) and 36-point are left out unless the bench shows
  the GPU load is too light.

### 4.5 What is deleted

**ffos:**
- `factory-iso/` entirely;
- the `factory_check` input and its build/permission branches in
  `build-image-to-cf.yml`;
- the normal ISO's `archiso-x86_64-linux-factory-check.conf` entry, which sat
  on an ISO whose default is Auto Install;
- the `soak-test` input in all three image workflows, the soak ISO entry,
  `usr/local/bin/websocat`, the soaktest user (passwd/shadow/group/gshadow,
  sudoers.d) and its install-time cleanup lines;
- the factory/display-check/soak branches of the getty@tty1 autologin
  comments and `verify.sh` sections;
- README text, `docs/BOOT_DISPLAY_AND_CONSOLE.md`, and the installed-entry
  list in `docs/SNAPSHOT_SYSTEM_V2_FLOW.md` (which gains the burn-in entry
  in stage 1).

**ffos-user:**
- `users/soaktest/`;
- `factory-check.sh` (the repeat-reading loop, kmscon notes, `device_id`
  copy: the installed hostname already carries the ID);
- `display-check.sh` (nothing boots it any more, including #497's `once`);
- the matching test-script parts and the `RELEASES.md` mentions;
- `scripts/test-unit-checks.sh` and `test-wifi-reading.sh` drop their
  sourcing of `factory-check.sh`.

`unit-checks.sh` and `wifi-reading.sh` stay: the burn-in calls them.

The display-check page stays and grows the burn-in role. Its `brightness` and
`panel` sets stay: the page is also served from toko-batch-2 / Cloudflare
Pages. Its burn-in additions go upstream to toko-batch-2 in the same way as
#492, or the burn-in page forks from it with a note. That choice is made in
the stage-2 PR.

## 5. Test and verification plan (first)

**Now (CI / containers):**
- ffos `verify.sh`:
  1. The entry writer function is the only writer of `loader/entries` in the
     three install/OTA scripts.
  2. It writes exactly `arch.conf`, `factory_reset.conf` and
     `ff1_burnin.conf`.
  3. `ff1_burnin.conf` carries `systemd.unit=ff1-burnin.target ff1.burnin`
     plus `$FF1_KERNEL_OPTS`.
  4. `loader.conf` keeps `default arch.conf`, `timeout 0` and `editor no`.
  5. The two updater drop-ins exist.
  6. The burn-in service orders after plymouth-quit.
  7. No trace of factory-iso, soak or websocat remains.
- Run the entry function against a temp dir: three files, the right options.
- `systemd-analyze verify` on the target and service. In an Arch container,
  `systemctl list-dependencies ff1-burnin.target`:
  - it must not contain `feral-kiosk-startup`, `tailscaled` or
    `feral-owner-guard`;
  - it must contain `timers.target`, which proves the R3 drop-ins are
    needed. Whether they work is pinned as files in verify.sh and proven on
    the bench.
- ffos-user `scripts/test-burnin.sh`, wired into `make verify-scripts`:
  - `status.json` schema;
  - the QR text stays under the QR byte budget for a worst-case unit
    (all checks FAIL, long error strings);
  - phase transitions with stubbed `unit-checks.sh` / `wifi-reading.sh`
    (SKIP/FAIL still reach the loop);
  - the journal-match patterns against sample amdgpu/nvme lines;
  - the previous-session line from a stale `last.json`.
- Page logic in node, the same method as the #492 review fixes:
  - latching red;
  - pass sequencing and the corner rotation;
  - fps/drop aggregation;
  - QR round-trip (encode, then decode with a test-only decoder).
- Size: ffos-user `scripts/test-burnin.sh` fails when the burn-in assets
  (page folder and load works) exceed 12 MB. The assets live in ffos-user,
  which ffos CI does not check out.
- CI triggers: `scripts/test-burnin.sh`, and any new asset directory outside
  `users/feralfile/{scripts,display-check}/**`, go into both path lists of
  ffos-user `.github/workflows/test-scripts.yaml`.

**Bench (one FF1, then a 24 h run on two units):**
1. Menu key: F2 vs Space (R1).
2. Plymouth quits and the page appears (R2).
3. During burn-in, these must not be running (R3, R4):
   `systemctl list-units` without kiosk/controld/watchdog/updater;
   `systemctl --user` without the kiosk stack.
4. Each check gives the same result as the factory check did on that unit.
5. The QR scans on an iPhone and an Android phone, the corner one included, and the phone shows the whole text (iOS Camera may offer only a banner: the verdict line must be enough).
6. Pull power, then a normal boot: the kiosk comes up and the owner state is
   unchanged.
7. R6 sequence once, with R6's expected result.
8. 24 h: running time keeps ticking, the status card numbers grow sanely, and
   `/var/log/ff1-burnin` stays small.
9. A forced Chromium kill is counted and restarted.

## 6. Recommended staged rollout

Each stage is a PR pair to `develop` (ffos-user + ffos) and is reviewed to
accept, adversarially for code, before push.

1. **Entry and mode.**
   - ffos:
     - the shared entry writer and the `ff1_burnin.conf` entry;
     - `ff1-burnin.target` and `ff1-burnin.service`;
     - the updater/recovery drop-ins and the plymouth ordering;
     - verify.sh pins.
   - ffos-user: `ff1-burnin.sh` runs `unit-checks.sh` + `wifi-reading.sh`
     once, then the current display-check page loops in cage, restarted when
     it exits.
   - Bench items 1–3, 6, 7.
2. **Results page.** Status JSON, result card + QR, status card, corner dot,
   kernel error and temperature sampling, `last.json`. Bench items 4, 5, 9.
3. **Load segment.** WebGL works + re-encoded 10-bit clip into the pass, with
   the size pin. Bench item 8 (24 h).
4. **Deletions.**
   - The factory ISO, the soak test, websocat, the normal-ISO Factory check
     entry, `factory-check.sh`, `display-check.sh`, and their docs and tests.
   - Gate: stages 1–3 are in a released image that the line actually
     installs, and bench items 4, 5 and 8 have passed. Until then the line
     keeps the factory ISO, because stage 1 alone shows no check results
     and installs come only from a released normal ISO.
   - The factory branch (`factory-check`) of both repos is no longer built
     after this.

The release is one full-image version carrying stages 1–3. Stage 4 is a
later version, after its gate. The dispatch parameters are recorded in
ffos-user `RELEASES.md` as usual. Agents never dispatch staging/release.
