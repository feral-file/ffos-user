# Overlay controller: one owner for the FF1 screen overlay

Status: proposed. Tracks feral-file/ffos-user#382 (design). Supersedes the
per-painter ownership flags introduced in the abandoned branch
`fix/382-overlay-repaint` (PR #383, closed unmerged).

## Current state

The player (ff-player) renders one overlay layer on the artwork page. Two
feral-controld components paint into it, each with its own bookkeeping and no
knowledge of the other:

- `setupui` paints setup, claim and status narration through `setupDisplay`.
  Its last intent is `s.last`; `Narrating()` is registered as a playersession
  overlay owner; `SweepStaleOverlay` hides only when `s.last == nil`.
- `mintpairing` paints the browser-pairing code and request state through
  `mintPairingDisplay` (`qrdisplay`). Its ownership is `displayOwner`,
  `displayGeneration`, and a `restoring` channel, restored by a goroutine when
  a session exits.

`executor.go` documents this as a known limitation: "no cross-surface
arbitration". The player does arbitrate, last command wins, in both directions
(`ff-player` at `dda7fec`, `MintPairingOverlay.tsx` and `SetupOverlay.tsx`):

- a renderable `setupDisplay` state makes a showing mint panel yield (hidden);
- any non-hidden `mintPairingDisplay` state makes a showing setup panel yield.

Two consequences the earlier draft got wrong:

- A `mintPairingDisplay` `hidden` never clears a setup panel, and a
  `setupDisplay` `hidden` never clears a mint panel. A session's restore cannot
  erase a claim QR on the player.
- `ffos-player-contract.json` (`notes`) and `docs/DEVICE_LOCAL_PLAYER.md` still
  say the player renders without arbitration. The code does arbitrate. These
  notes are stale. Fixing them is a documentation task in the player repo and
  does not block this design.

The player is correct: it shows the last command it receives. The defect is on
the device side. Two controld paths send a pairing code while the owner wants a
claim QR:

- Expiry refresh (`refreshExpiredPairingCode` → `showPairingCode`): nobody asked
  for a new code, and it replaces the claim QR. This is #382.
- Re-show on a repeated Browser Pairing tap (`already_started`): this one the
  owner asked for, so it is kept.

Controld has no record of what is on screen, so it cannot tell an automatic
paint from an owner paint over an owner-requested overlay. The app cannot read
which overlay is on screen either (#381).

## Required outcomes

1. At any time the controller knows which overlay is on screen, who painted it,
   and its payload. Status, sweep, resync and the expiry guard read that state
   from the controller, not from separate probes.
2. The most recent overlay wins. A newer overlay that replaces an older one
   notifies the older owner through `onOverride`.
3. Automatic re-displays never replace a newer overlay. Only an owner action
   (a Browser Pairing tap, a relayer `show`) can take the screen.
4. Claim QR replaces a browser-pairing code, and the pairing session is closed
   as part of that override.
5. A replaced owner never sends a hide or restore for the screen. Only the
   controller hides or restores, and only for the overlay it currently holds.

## Design

One controller owns the overlay. It stores the current overlay record:
`{handle, kind, payload}`. Callers hold a handle, not a flag.

```
show(handle, overlay) -> ok | rejected
hide(handle)          -> ok | notCurrent
current()             -> {kind, payload, handle} | none
```

- `show` commits the paint, sets the current record, then calls `onOverride`
  of the previous handle, if any. The callback runs outside the controller lock.
- Each handle gets at most one terminal event: `onOverride` or `onClose`. After
  `onOverride`, the handle receives no `onClose`.
- `hide` is accepted only from the current handle. A replaced handle gets
  `notCurrent`.
- Paints are serialized through one queue inside the controller. Callers never
  issue CDP sends directly.

### Policy on `show`

| Incoming | Current | Result |
|---|---|---|
| owner action (tap, relayer show) | any | accepted, current overridden |
| automatic (expiry refresh, sweep) | any other overlay | rejected |
| automatic | none | accepted |

### Callbacks and ownership

- `setupui` on `onOverride`: clears its claim intent. Replaces `ClaimQRReplaced`.
- `mintpairing` on `onOverride`: closes the session in the callback (decided by
  the owner). Its session does not send hide, because it no longer holds the
  screen. Replaces `SupersedeDisplayRestores`, `restoring`, and
  `displayGeneration`.
- Expiry refresh asks `current()` before `show`. Replaces `ShowingClaimQR`.

## Invariants touched

- `docs/api-design.md` protocol invariants 5 and 6 (claim QR `device_info` string,
  shared command envelope): not touched.
- `docs/setup-flow.md` narrator policy (claim-primary, gate-level): the
  controller must keep the same claim-primary decision; to confirm during design.

## Open questions (to settle before implementation)

1. **Player contract.** Not a blocker. The player shows the last command it
   receives, and the design keeps that behavior. The change is entirely in
   controld. Correcting the stale notes in the player repo is a separate
   documentation task.
2. **Resync.** After a CDP reconnect, the controller re-sends `current()`, not the
   last intent of one painter. Confirm that a reconnect with no current overlay
   sends nothing.
3. **Boot sweep.** The one-time sweep hides an overlay the controller did not
   paint in this process. Define "not painted by this process" in terms of the
   controller's own record.
4. **Overlay kinds.** Which setup states are overlays the controller owns, and
   which are narration that never competes (for example, progress text).

## Test plan (red first)

Controller unit tests, written before any caller changes:

- `show` sets current and calls `onOverride` exactly once for the previous handle.
- Automatic `show` over a current owner is rejected and emits nothing.
- `hide` from a replaced handle returns `notCurrent` and sends nothing.
- No handle receives both `onOverride` and `onClose`.
- Callbacks run outside the controller lock: a callback that calls `show` or
  `current` does not deadlock.
- Serialization: concurrent `show` calls produce a single ordered paint sequence.

Then caller tests: claim QR closes a pending mint session; an expiry refresh
during a claim QR is rejected; a closed session sends no hidden state; boot sweep
hides nothing the controller painted.

## Out of scope

- Re-shaping the device status payload. Tracked in #381 and implemented on top of
  `current()` after this lands.
- Factory reset and the session revoke path, except where they call
  `CloseActivePairing` (they keep their current behavior until the controller
  lands).

## Migration

1. Land the controller with unit tests and no callers.
2. Move `setupui` onto it, then `mintpairing`, one PR each.
3. Remove `ShowingClaimQR`, `ClaimQRReplaced`, `SupersedeDisplayRestores`,
   `restoring`, and `displayGeneration` in the final PR.
