# Native player migration — implementation in progress

This development branch is a coordinated rewrite with the sibling
`components/player` source tree. **Do not deploy this controld independently.**
Status and setup narration now use the native host, while the remaining command, playback
refresh, session and watchdog consumers are still being migrated. There is no
automatic CDP fallback in the new transport. Activation must wait for the full
consumer migration and matching FFOS session/package changes.

## Implemented boundary

`playerlink` speaks framed protocol version 1 over a Unix-domain socket. The
default is `$XDG_RUNTIME_DIR/ff-player/player.sock` (falling back to
`/run/user/<uid>/ff-player/player.sock`). `FF_PLAYER_SOCKET` selects an isolated
test instance. Client and server check peer UID. The client keeps one ordered
connection, bounds operations to five seconds, closes it on cancellation or
transport failure, and never blindly retries an uncertain mutation.

`status.NativePlayer` maps typed native snapshots to the existing controller
status DTO. It uses `presentedPlaylist` and the presented occurrence's context
while a new cast loads; incoming failure remains visible under `runtime.selected`.
Render code 2 requires a host-composed occurrence, not a page navigation. Device
status's `displayURL` is a device-owned UI URI. It preserves the older phone
`step=qr` hint for the native claim overlay without exposing artwork URLs or
signed asset credentials. Artwork pages do not
supply either status object.

Native verification lookup takes `(instance, presented generation, playlist ID,
playlist URL)`. The previous ID/URL-only slot is deliberately not consulted for
native snapshots. Publishing the fenced verdict from native cast/schedule
receipts is part of the remaining command migration; until wired, the field is
omitted. Acceptance must never manufacture a trusted displayed verdict.

The old document-stamp observer interface remains only for the session consumers
not yet migrated. Native snapshots never populate that stamp. Remove the
observer and its DTO compatibility fixtures when the last document-session
consumer is replaced by host-instance reconciliation.

## Verification so far

- Shared Go/C++ JSON/framing fixtures and real Unix-socket interoperability.
- Race tests for the client and status boundary.
- Status tests for incoming/outgoing same-URL occurrences, failed incoming
  renderers, absent presentation after restart/stop, and generation-fenced trust.
- Full controld tests, vet and changed-diff lint during this stage. Evidence is
  retained in `components/player/docs/evidence/`.

See the sibling tree's `docs/PLAN.md`, `docs/BEHAVIOR.md`, `docs/VERIFICATION.md`
and `protocol/README.md` for current implementation, device checks and gaps.
The required release remains a full-image release, prepared on `develop` with
human-operated promotion. No publishing workflow has been dispatched.

## Setup narration migration

Setup's shared overlay arbitration, conditional hides, stale-overlay sweep,
claim URL/name refresh and ordered ready/hide delivery are retained. Delivery
uses `playerlink.SetOverlay`, requires `overlay.setup.v1`, and never evaluates
JavaScript. The renderer-independent host watch replays current intent on
reconnect. Failed sends never block provisioning; status uses observed host
snapshots instead of treating queued intent as a visible panel.

The old page-navigation park and setup bundle manifest fallbacks are deleted.
The coordinated release requires all setup states; unknown states fail with
`unsupported` instead of silently disappearing. The remaining legacy status
manifest helper has an explicit removal condition: migration of devicectl boot
recovery and media-health capability consumers. It is not a narration fallback.

Native setup includes the Wi-Fi join/attached-portal QR phases, credential
escaping, claim QR and device-name guidance, neutral connecting copy, failures,
update progress and reset instructions. `ready` clears setup without clearing
another overlay owner. The old animated onboarding backdrop and final visual
layout still need fidelity review; no setup visual acceptance is claimed yet.
