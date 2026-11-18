# Normal casting and recovery cutover — 2026-11-18

Anh's change request: install the ordinary native replacement on FF1-SKYZ2E3A,
finish phone casting/session/watchdog integration, test real Feral File playlists
from FF-CLI, leave the screen on, push the branch, no PR. Baselines were
ffos-user `0ba580df33ee38273d91ab660da19110a5ac5355` and ffos
`a9f51875fb4853ad2a9c736d24c70c28ff4dfa03`, both clean. Unrelated ff-player
worktree edits were preserved. Both owning branches are `codex/native-player`.

## What changed

The previous preview could render artwork but normal phone casts still targeted
the original browser. The coordinated installation replaces that route with
`playercontrol.Native`, retaining external command names, authorization,
signature verification and source admission. No JavaScript shim is used.

The host owns current selection, durable state and schedule execution. Controld
fetches/validates sources and sends fenced updates. A refresh begun for an old
cast cannot overwrite a newer one or borrow outgoing presentation metadata.
Runtime-instance reconciliation restores device intent; repeated awake state
is idempotent and does not recreate artwork. Boot identity distinguishes an OS
boot from a host restart. A policy-blocked boot playlist leaves a usable host.

Watchdog probes the actual private native command socket. Host and renderer
health are separate. Existing startup grace, bounded restart/fallback/reboot,
headless/VT/update suppressions remain. Thermal and service signals are typed
levels, never page navigation. Native earlyoom names join existing accounting.
The kiosk/session, readiness probe, controld and watchdog were installed together.
Old static server/CDP readiness are rollback-only, with removal conditions in
HANDS-ON.md. Isolated capture Chromium remains a separate offline subsystem.

## Device observations

Device: FF1-SKYZ2E3A, Radeon 680M, 7 GiB RAM, Dell 3840×2160/60 Hz, saved portrait
rotation (2160×3840 compositor). CEF Chromium 156.0.8078.96; libmpv 0.42.

- Signed FF-CLI cast of **Social Codes**, ten works, official Feral File channel
  `682c8a5a-d09c-473a-9356-ba3ba8871c26`, playlist
  `1bbefbc9-0226-439a-921b-12c6cf6a809e`, reached the host. CLI resolves and sends
  inline; a blank playlistUrl is intentional for this path.
- Signed FF-CLI single holz cast reached selection/presentation. Initial
  compositor image was black despite host presentation. Logs showed unsupported
  WebGPU swap-image backing. Enabling Vulkan/VulkanFromANGLE with the Vulkan
  presentation surface disabled produced visible geometry at the original URL.
  This is why navigation or a host frame alone is not a visual pass.
- Restarting controld preserved runtime, generation, playlist and exact artwork
  occurrence after fixing redundant awake replay.
- SIGKILL host: systemd restored the playlist and a presented occurrence in
  **8.145 s**. Signed getDeviceStatus remained available during recovery.
- SIGSTOP host: watchdog detected command-loop failure and restored the playlist
  in **33.211 s**, including its 20-second detection threshold and unit stop/start.
  External device control remained available. This does not claim the host
  survives its own process death or a shared GPU failure.
- Thermal hold/release retired artwork backends and restored the same playlist.
  Threshold/hysteresis behavior was unit tested; hardware was not overheated.
- Signed phone-style drag command and history read succeeded through FF-CLI's
  enrolled transport. The physical phone app still needs Anh's hands-on test.
- Native screenshot backend produced a bounded 607×1080 portrait compositor PNG.
  The raw HTTP screenshot endpoint correctly returned 403 on this enrolled device;
  capture was tested through authorized local diagnostics, without weakening auth.

Recovery timings are single observations, not performance percentiles. Captures
show compositor buffers, not physical HDMI/panel output. All ten Social Codes works were selected through the signed FF-CLI command
transport and produced visible artwork in inspected captures. Scrampled first
showed black during startup, then visible geometry after a longer dwell. This
is smoke evidence, not equivalence to artist reference output. See the evidence
folder for per-work selection outcomes and captures; no overnight/physical-display
acceptance is implied.

## Checks and review

12 macOS core CTest groups and 15 Linux CEF/media CTest groups passed. Shared
schema generation and pinned-vendor verification passed. Go race suites, vet,
changed-diff lint and script contracts are recorded with final evidence.
FFOS verification runs as an unprivileged user in the Linux verification image;
macOS lacks flock semantics and running Linux fixtures as root invalidates
permission-failure tests. Those exploratory failures are not counted as passes.

Fresh-context review found blocked-boot startup and stale-refresh ownership bugs;
these were fixed and covered. A subsequent review reproduced a future-only cast
retaining an older one-shot schedule; the fix cancels superseded pending owners
while retaining current artwork, with a regression test. Latest review and
remaining findings/disposition are recorded in REVIEWS.md. Anh remains the human
change owner and release-signoff authority.

## Remaining acceptance boundaries

This installs the ordinary replacement for hands-on casting; it does not declare
the original broad rewrite finished. Native offline resource replay/onchfs bridge,
fenced trusted-verdict publication, native artwork-only classifier, full portfolio
fidelity, overnight soak and physical-display evidence remain incomplete. Cache
capture coverage must not be called native offline-ready. Legacy per-side/token
presentation options are explicitly unsupported rather than silently accepted.
The prior fair Grit measurement remained about 15.3% slower than Chrome; this
cutover does not claim pacing parity, and the new Vulkan configuration needs a
fresh comparison. Security-update rehearsal remains incomplete.

The pacman recipe and player CI exist; full-image package production and final
OTA carry of native SQLite state still need integration/acceptance before fleet
release. A package-only release is prohibited. RELEASES.md records the coordinated
full-image declaration and human promotion parameters; nothing was published.
Rollback is explicit in HANDS-ON.md and preserves original profile/export.
