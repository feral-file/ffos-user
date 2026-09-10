# Player composition status

The `checkStatus` → typed `status.PlayerStatus` → lightweight `player_status`
bridge preserves `deviceSettings.showingKey`, `compositionRevision`, `scaling`,
`margin` and `background` from ff-player. Omitting a struct field silently drops
it before controllers receive the report.

Composition is the effective committed render after DP-1 merging and ephemeral
Control Center writes; it does not change persistence. `showingKey` is an opaque
slot/work/source identity latched with the visual settings. It must travel with
the composition even when the playlist index advances before an incoming work
has loaded. `compositionRevision` changes on composition commits, including a
quick return to prior values, so notification deduplication still publishes the
result. The revision is not durable across player restarts.

Margin is DP-1's number-in-pixels or CSS string, including percentage strings;
`json.RawMessage` preserves the type and explicit zero. Background is the CSS
color string. Missing fields stay missing for an unmounted stage or older player.
Device orientation, default duration and tombstone retain their existing scope.

The app uses reports as truth when reopening or after another controller writes,
with temporary optimism and rejection rollback. Control Center writes remain
partial and `isSaved: false`. `TestCompositionRoundTrip` exercises the typed
decode and actual lightweight marshal; `TestAbsentCompositionStaysAbsent`
guards against inventing defaults.
