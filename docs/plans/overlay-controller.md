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
arbitration". The player does not arbitrate either. Result: a paint from one
side can silently replace the other's, and the state each side keeps is stale
after that (#382: a claim QR is overwritten by a pairing code after expiry or a
re-tap; the app cannot read which overlay is on screen, #381).

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

1. **Player contract.** The player does not arbitrate two overlays. Confirm with
   the ff-player owners that a single controller-driven `setupDisplay` or
   `mintPairingDisplay` stream is the contract, and that `hidden` clears only the
   overlay it was sent for.
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
