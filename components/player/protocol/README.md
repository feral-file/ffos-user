# Local player protocol, draft 1

This is the implemented transport/core contract during development. It is not
a frozen shipping contract. Screenshot transfer, trusted annotations and the
complete presentation/input schemas are still being completed;
they are not implied by an advertised transport version.

## Transport and trust

Connect to the configured Unix-domain stream socket as the player service UID.
The default deployment location will be `/run/user/1000/ff-player/player.sock`;
tests use a different directory. The directory is owned by the service UID,
mode 0700; socket mode is 0600. Both peers validate the other UID (`SO_PEERCRED`
on Linux). The host refuses to unlink a foreign/non-socket path and holds an
exclusive socket lock. No TCP listener or arbitrary JavaScript evaluation is
part of this protocol. Controld retains external authentication and authorization.

Each message is a four-byte **unsigned big-endian byte length**, followed by
that many bytes of UTF-8 JSON. Length must be 1 through 4,194,304. Partial reads
and writes are normal. Zero/oversized lengths close the connection before body
allocation. JSON nesting is limited to 64, duplicate keys are rejected at every
level, and invalid UTF-8/non-finite numbers are rejected. A framing error has no
recoverable request boundary: reconnect rather than trying to resynchronize.

This small envelope deliberately omits JSON-RPC batching and notification-only
mutations. Every command needs a receipt, and instance/generation/deadline
preconditions have one consistent location. It uses familiar request IDs,
results and structured errors without claiming JSON-RPC wire compatibility.

## Negotiate and obtain a snapshot

```json
{"v":1,"id":"hello-1","method":"hello","params":{"client":"controld","versions":[1]}}
```

The reply has `v`, the same string `id`, and either `result` or `error`.
The hello result includes the selected `version`, random runtime `instance`,
`maxFrameBytes`, bounded `receiptWindow`, capability names and a complete state
snapshot. An incompatible version returns `version_mismatch`. Send hello before
other methods. `client` is a stable logical name used to namespace retry keys;
it is not an authentication credential. Repeated hello is permitted for
snapshot reconciliation on a persistent connection.

```json
{"v":1,"id":"cast-1","method":"playback.replace","params":{"playlist":{"title":"Example","items":[{"id":"one","source":"https://example.org/art","duration":60}]}},"instance":"FROM-HELLO","generation":1,"deadlineMs":1794916205000,"key":"UNIQUE-LOGICAL-OPERATION"}
```

`id`, `client`, `key` and method names are bounded to 64 UTF-8 bytes. `id`
correlates one transport attempt; `key` identifies the logical mutation across
retries. All non-hello requests require an absolute Unix-millisecond deadline
between now and 30 seconds from now. The deadline is checked at dispatch; it
does not promise that presentation will finish by that time. Controld and the
host use the same device clock. Transport read/write/handshake deadlines use
monotonic time and are unaffected by wall-clock correction.

Mutations also require `instance` and `generation`. Optional `ifRevision`
rejects a mutation unless the entire observed state still has that revision.
Omit `ifRevision` when a change should tolerate unrelated telemetry updates.
Mutations run serially on the host loop in received order per connection.
Across connections the host's dispatch order is authoritative. A caller that
needs total order should use one persistent connection, as the Go client does.

## Identity and outcomes

- **Instance** changes on every host start. Old-instance operations are rejected.
- **Generation** changes when a cast replaces or cancels the playback domain.
  Selecting another item within a cast does not replace the generation.
- **Revision** increases for observable state changes. It is a monotonic cursor
  within one instance, not a durable identifier.
- **Occurrence ID** is random for each selection/mount, including adjacent items
  with identical URLs. A fresh restart creates fresh occurrences.

`selected` and `presented` are separate snapshot objects. An incoming selection
can load while the outgoing occurrence is still shown. Occurrence stages are
`loading`, `committed`, `presented` and `failed`. A navigation commit is not a
frame. A host swap is composition evidence, not proof of correct arbitrary
artwork output or a working physical panel.

`playlist`/`playlistUrl` describe the admitted selection. `presentedPlaylist`/
`presentedPlaylistUrl` retain the document belonging to the presented occurrence,
including while another playlist loads. Never attach the current generation's
verification verdict to an outgoing occurrence from an earlier generation.

An accepted playback/seek response identifies its occurrence. Follow snapshots
for presentation or failure of that occurrence. `outcome: applied` means the
host applied the command's state operation, not that a physical display
changed. Renderer failure includes an occurrence-scoped explanation in both
the snapshot and native UI. The host's health is reported independently of
renderer health. Media telemetry carries its occurrence and `sampledAtMs`;
it is sampled and can lag the current host stage.

Errors have `{code,message}` and never masquerade as empty playlists/history.
Current codes include `invalid_json`, `invalid_params`, `unknown_method`,
`not_negotiated`, `version_mismatch`, `deadline_exceeded`, `stale_instance`,
`stale_generation`, `stale_revision`, `stale_occurrence`, `idempotency_conflict`,
`content_blocked`, `playlist_invalid`, `input_busy`, `input_blocked`,
`unavailable`, `unsupported`, `storage_unavailable` and `storage_limit`.
Unknown codes must be treated as failures, preserving the message for diagnostics.

## Duplicates, cancellation and limits

Successful mutation receipts are retained in a FIFO window of at most 1,024
operations and 32 MiB. The namespace is the pair `(client,key)`, encoded without
delimiter collisions. A retry must preserve method, params and preconditions;
only `id` and deadline may change. An exact retained retry returns the original
receipt without executing again, even if its generation has since advanced.
Reusing a key with changed content returns `idempotency_conflict`.

Receipts are not durable across host restarts and can be evicted. Do not blindly
replay after an unknown result, eviction or new instance. Reconnect, reconcile
the snapshot and decide whether a new operation is appropriate. Closing a
connection cancels waiting on its result, **not** an already committed state
change. The Go client does not automatically replay a failed transport attempt.
Playback cancellation is an explicit `playback.cancel` operation, fenced to the
current instance/generation; it stops the cast and advances its generation.

At most eight clients are admitted. Each input buffer is bounded to one maximum
frame plus its prefix; each output buffer to 8 MiB. A loop handles at most eight
requests per client before returning to presentation. Handshake, incomplete
frame and stalled output each have a five-second absolute monotonic deadline;
trickling bytes cannot extend it. Idle negotiated peers may remain connected.

## Subscriptions and reconnect

`state.subscribe {}` returns `{snapshot,cursor}`. Subsequent unsolicited events
have `v:1`, `event:"state"`, `previous`, `cursor` and `snapshot`. Every event is a
complete snapshot. Slow subscribers receive coalesced snapshots rather than an
unbounded delta queue. A cursor gap is therefore repaired by replacing the
client state with that snapshot; never apply it as a delta. On reconnect,
renegotiate and subscribe for a fresh snapshot. There is no cross-restart event
replay promise.

## Implemented method families

| Method | Params and semantics |
|---|---|
| `state.get`, `health.get` | `{}`; full current state, including host and renderer health |
| `state.subscribe` | `{}`; complete snapshot stream |
| `playback.replace` | `{playlist,index?,context?,playlistUrl?}`; validate/persist a new cast before selection. Context defaults to curated; no allowed item rejects and retains the current cast; an empty list clears it. |
| `playback.update` | `{items,context?,retireBlockedCurrent?}`; source refresh of the current cast; requires `playback.update.v1`. Receipt includes `updateId`; see refresh semantics below. |
| `playback.select` | `{index}`; select an allowed item as a new occurrence |
| `playback.refresh` | `{}`; reload the selected work as a new occurrence |
| `playback.stop`, `playback.cancel` | `{}`; durable stop, release input and presentation |
| `playback.pause` | `{paused}`; pause/resume progression and native media; explicit resume releases an autoPlay hold |
| `playback.sleep` | `{sleeping}`; durable sleep intent; suspend presentation, restore selection on wake |
| `playback.mode` | `{loop?,shuffle?}`; loop is none, playlist or one; shuffle order is host-owned and durable |
| `media.seek` | `{occurrence,seconds}`; only the currently presented seekable native media; acceptance is not seek completion |
| `settings.set` | Saved device settings. Framing is artwork/fit/fill and respects authored userOverrides; full presentation semantics are still under implementation. |
| `presentation.set` | `{occurrence,display:{scaling?,margin?,background?}}`; ephemeral preview on the currently selected AND presented occurrence; requires `presentation.v1`. A loading replacement or stale occurrence rejects; an artwork with `userOverrides:false` rejects with `presentation_locked`. No implicit target. |
| `policy.get`, `policy.set` | Query `{}` or set complete version-1 policy flags; controld retains trust and controller policy decisions |
| `history.list`, `history.resolve` | List retained presented occurrences or resolve `{recordId}`; resolution does not play it |
| `schedule.set` | `{playlist,context?,atMs}` or `{clear:true}`; one durable scheduled cast, executed by host |
| `boot.set` | Store/clear boot intent; boot execution/recovery is still being completed |
| `connectivity.set` | `{online}`; controld's connectivity observation; no renderer can declare device connectivity |
| `overlay.set` | `{owner,state,fields?}`; owner setup/pairing/recovery; only matching owner may hide an overlay; focus/input released on show |
| `input.dispatch` | Occurrence-fenced input; one connection owns held state; disconnect releases it. The host owns pointer coordinates, buttons and physical keys. See the event forms below. |

### Presentation configuration and completion

`settings.set` retains durable device fallbacks. Authored DP1 layers override
fallback scaling/margin/background. An occurrence's transient adjustment sits
above those layers; explicit device framing applies last when user overrides
are allowed. A new occurrence, including an adjacent identical URL or recovery,
clears transient adjustments. A source refresh of a retained occurrence keeps
its adjustment. Legacy keyless phone writes must resolve an explicit current
occurrence in controld; this protocol never guesses a target.

`presentation.set` returns `acceptedCompositionRevision`, the last host-composed
revision before acceptance. Observe a later `composition.revision` for the same
occurrence and compare its effective display values. Even an unchanged preview
gets a new configuration revision and completion. A superseding preview may
coalesce an earlier one; matching a revision floor alone does not prove that
earlier values were presented. `selected.display`/`presented.display` describe
requested configuration; `composition.display` describes the last submitted
configuration. Renderer readiness or receipt acceptance does not advance it.

The host requires a matching renderer configuration and captured frame before
submitting changed geometry. Bundled renderers acknowledge layout, then the
host requests another frame; mpv replies to the tagged property operations and
tags its next exported frame. Software documents receive viewport resize, with
no injected bridge. Input is released/blocked while an established composition
is changing, then restored with the new coordinate conversion. No composition
success is persisted across process restarts. These acknowledgments establish
host composition, not arbitrary artwork correctness or physical scanout.

Margins are nonnegative pixels or percentages. Percentages use output width
horizontally and height vertically, matching DP1's vw/vh behavior. Excessive
padding is clamped to retain a positive renderable viewport. Preview background
is `#RRGGBB`; preview does not change playback/loop flags, labels or saved state.

Authored backgrounds accept named CSS colors, 3/4/6/8-digit hex, RGB(A)
numbers/percentages and HSL(A) literals. The host resolves them to opaque sRGB
over black for every backend; alpha does not reveal another artwork. Contextual
values (`var()`, `currentColor`, system colors) and other color functions reject
at playlist admission with `playlist_invalid`. Artist documents are unchanged.
The color-name 1.1.4 mapping and MIT license are vendored in `third_party/`.

Each renderer retains separate captured and submitted GPU textures. A superseded
configuration cannot overwrite the frame associated with recorded composition;
covered configuration changes retain that frame until uncovered. This adds one
RGBA texture per renderer (about 14.1 MiB at 1920×1920, 31.6 MiB at 3840×2160),
without an additional copy. Frame pacing and total GPU memory still require
measurement on the final runtime.

### Source refresh semantics

`playback.update` is fenced to the observed instance/generation like other
mutations. It only replaces items; playlist metadata/defaults/URL remain the
current cast's. A changed projection drops `signature` and `signatures` and
advances the generation. Controld must bind fresh verification to that resulting
generation. Artist messages cannot refresh a playlist or supply that annotation.
Curated playback cannot become personal through a refresh. Personal-to-curated
narrowing re-admits immediately under the stricter context.

An exact slot match keeps its occurrence, elapsed time and renderer. Matching
uses `(id, source)` plus ordinal among repeated pairs, including absent IDs.
The retained occurrence gets refreshed metadata/index/document/generation, but
does not create another history record. An ID whose source changes starts a
new occurrence. If the selected slot disappears entirely, a nonempty refresh
is persisted as `pendingUpdate` and starts at its first allowed slot when the
current slot finishes, including media EOF. There is no artificial timeout for
an indefinite work: explicit select/cast or another refresh remains available.
An identical refresh cancels a pending replacement without disturbing shuffle.
While deferred, the selected and presented occurrences retain their earlier
document generation; the host generation is the current command fence. Never
substitute that fence for an occurrence's generation when attaching trust.
An empty refresh clears immediately. Stop/cancel/new cast cancel pending updates.

Fresh labels that block the selected or same-cast outgoing occurrence retire
the blocked presentation immediately, even when the new list is entirely
blocked. Controld can request this retirement after its own filtering through
`retireBlockedCurrent`. Otherwise an all-blocked refresh is rejected and retains
the current cast. At a deferred boundary, the host rechecks the current policy.
No allowed item produces a terminal `failed` update outcome and retains any
still-allowed current work. Matching boot projections receive the fresh items.

The receipt is acceptance of durable intent, not presentation. Snapshots expose
`pendingUpdate:{id,itemCount}` and the latest `updateOutcome:{id,outcome,
contentBlocked}` (`deferred`, `applied` or `failed`). They survive restarts;
occurrences and rendering success do not. A new refresh supersedes that result;
clients needing an audit retain their receipts/events. Full pending documents
are persisted but not duplicated in every snapshot. Failed writes preserve the
old state and timers; timer-driven retries use the existing five-second bound.

A scheduled task has a host-issued `id`. When due, the host atomically clears
the pending task and records `scheduleOutcome`: `applied` means selection was
executed, not that it rendered. A policy-blocked task records `failed` with its
structured error and retains the current artwork. The outcome includes the
execution instance/generation and remains available after restart; it is cleared
by a new explicit cast or schedule operation. Storage failure leaves the task
pending and retries no sooner than five seconds later. The device displays a
scheduling failure and controld carries the outcome in its additive runtime
status. A new subscription always includes the latest outcome.

The playlist admission limits are 1 MiB, 4,096 items and 64 KiB per item. Sources
are at most 16 KiB. History retains at most 100 entries and 1 MiB. Durable writes
are transactional and bounded to 4 MiB. These explicit limits are transport/
resource policy, not claims that an entry document makes software offline-ready.

## Input event forms

Each event includes `occurrence`, matching the actually presented occurrence.
Pointer coordinates are **output framebuffer pixels**, before subtracting the
artwork's matte. The host clamps to the output, converts to artwork coordinates
and draws its cursor from that same state. Physical SDL coordinates are scaled
from window coordinates to framebuffer pixels. Contiguous pointer moves are
coalesced before dispatch; every button/key/text/wheel transition flushes the
pending move first. Target/geometry changes and the end of the host input loop
also flush it. Rotation integration and scale factors above 1 remain unqualified;
the current device evidence uses rotation 0 and output scale 1.

- `{"kind":"move","x":400,"y":500}` sets the absolute position;
  `dx`/`dy` apply relative movement. Relative values take precedence on an axis
  when both forms are supplied. Positions/deltas are bounded to ±16,384.
  Optional `referenceWidth` (1–16,384) makes relative deltas use a reference
  width; the host multiplies both axes by current framebuffer width divided by
  that value. It cannot be combined with absolute x/y. Phone gestures use 1920,
  preserving sensitivity across output sizes without client-cached geometry.
  The first artwork initializes the pointer at output center; subsequent target
  changes retain it.
- `{"kind":"button","button":1,"down":true,"clicks":1}` changes one
  button. Buttons are 1 left, 2 middle, 3 right; click counts are 1–3. A move
  while held is a drag. Button events use the host's current pointer position.
- `{"kind":"wheel","dx":0,"dy":-40}` sends integer CEF wheel deltas (positive dy scrolls up, opposite DOM/CDP deltaY),
  each bounded to ±32,768.
- `{"kind":"key","usage":4,"down":true}` changes a **USB HID keyboard-page
  physical key**, not a character or an OS-dependent virtual key. Examples:
  4 KeyA, 40 Enter, 80 ArrowLeft, 225 ShiftLeft. Supported usages are explicitly
  mapped by `src/input.cpp`; unsupported positions return `unsupported`.
  Repeated down events carry the repeat flag; modifiers follow held keys.
- `{"kind":"text","text":"é猫"}` inserts UTF-8 text independently of the
  physical keyboard layout (at most 4,096 bytes). Sending a physical key does
  not infer or insert its printable text.
- `{"kind":"releaseAll"}` releases held keys/buttons and renderer focus.

Showing device UI, losing window focus, changing the presented target, and
disconnecting the owning connection release held input against the **old**
target before it is destroyed. The pointer position survives target changes.
Artwork browser clients receive normalized events and own no pointer state.

## Media worker subprotocol

This is private to host/worker and not exposed to controld. An inherited
`SOCK_SEQPACKET` descriptor carries JSON packets of at most 64 KiB and at most
four `SCM_RIGHTS` DMA-BUF descriptors. One exported frame may be outstanding.
Initialization binds the worker to one source and surface size. Controls are
typed pause/volume/mute/seek/display/resize operations, not arbitrary mpv commands.
The worker emits loaded, frame, property, ended, error and heartbeat observations.
Host copy completion releases the buffer; host swap separately reports
presentation. The host enforces a five-second heartbeat timeout, closes/kills
failed workers without waiting on codec teardown, and retains playback authority.

## Renderer recovery

The host retries a failed selection at most three times, after 1, 2 and 4 seconds.
Every retry gets a new occurrence ID in the same playlist generation. Late
callbacks, input and seeks for the old occurrence cannot affect the replacement.
Snapshots retain `recovery.lastError` and `failedOccurrence` during loading;
`attempt` counts retries already started, `pending` identifies a scheduled retry,
and `exhausted` means the third replacement also failed. `retryDelayMs` is the
delay selected when the failure happened, not a live countdown. Failure and
retry remain observations, never a successful command presentation outcome.

Explicit refresh/selection, stop, sleep or a new cast clears this selection's
budget. Sixty seconds of uninterrupted presented state also replenishes it;
brief successful frames do not hide a crash loop. Recovery is transient and
starts fresh after host restart. Watchdog/systemd must separately bound repeated
whole-process failures. The native host cannot recover its own dead process or
a wedged shared GPU, and this mechanism does not claim otherwise.

If an outgoing occurrence fails while a newer selection loads, the host retires
the outgoing presentation and releases its input. `presentationFailure` retains
that occurrence ID, generation and error separately. The incoming occurrence and
its retry budget are unchanged. A new presentation or explicit selection clears
this failure. It must never be reported as the incoming work's failure.

## Setup overlay capability

`overlay.setup.v1` covers setup states `softap_qr`, `joining`, `join_failed`,
`updating`, `claim_qr`, `ready`, `hidden`, `scanning`, `finalizing`,
`factory_reset`, `connecting`, and `setup_error`. Unknown states fail with
`unsupported`; the existing overlay remains. Ready/hidden clear only setup's
own overlay. Fields are `reason`, `ssid`, `password`, `portal_url`,
`client_attached` (boolean), `url`, `device_name`, and `progress` (finite number,
clamped to 0–100 for display). Text fields are at most 2,048 UTF-8 bytes without
NUL; SSID is required for SoftAP, and SSID/password are limited to 128 bytes. Claim requires an
HTTP(S) URL. Credential fields must not be logged.

The host constructs the single Wi-Fi QR, then switches to the HTTP(S) portal
QR when `client_attached` is true and the portal URL is valid. Setup narration
is transient; controld replays current intent on host reconnect. It remains
independent of artwork navigation, renderer health, and playback persistence.
The receipt means the host accepted the overlay state. Neither a receipt nor
a snapshot proves correct physical output.
