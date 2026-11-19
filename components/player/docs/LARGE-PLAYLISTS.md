# Large playlists on the native player

[Feral File #3597](https://github.com/feral-file/feral-file/issues/3597) records
five published series that exceeded the original 4 MiB limit. Its acceptance
case is the complete Crystalline Work SDK series cast by URL. On 2026-11-19 the
[published playlist](https://feralfile.com/series/crystalline-work-sdk/playlist.json)
returned 9,303,374 bytes, or 8,905,202 bytes after compacting JSON. It had 9,048
distinct sources and one inline manifest per item. A projection with only
`source` and `duration` still used 1,274,947 bytes.
Both signature entries verified against the downloaded document using the
current controld verifier on the development Mac.

| Boundary | Current behavior |
| --- | --- |
| Phone, web and CLI to controld | LAN bodies, legacy relayer messages and reassembled relay v2 messages remain capped at 4 MiB. The authenticated envelope also consumes space. A `playlistUrl` request already carries just a URL. The current FF-CLI fetches the document and sends it inline as `dp1_call`, even when the input was a URL. |
| Controld fetch and verification | The bounded URL fetch and signature verifier both allow 16 MiB. Verify the original bytes before deriving playback data. The 255-item series expansion limit applies to registry expansion. The Crystalline document supplies flat entries directly. |
| Controld to player | `playercontrol.Native` serializes the complete playback document into a `playback.replace` request. The Go/C++ Unix socket frame limit is 4 MiB in each direction. |
| Native validation and storage | `Playback::validate_playlist` caps each playlist at 1 MiB and 4,096 items. The 4 MiB SQLite state JSON limit also covers history, schedules and other durable state. The selected and presented occurrences retain separate playlist copies. |
| Status | Hello, snapshot reads and state events include full current and presented playlists, and may also carry a scheduled playlist. Raising the write ceiling alone can still break reads and reconnect. |

An isolated core test submitted a 143,419-byte request with 4,097 items and
received `At most 4096 items are allowed`. A separate 1,124,024-byte request with
1,000 items received `Playlist exceeds 1 MiB`. Both were below the socket frame
ceiling. No transport or playlist limit changed in the transition port.

## Proposed sequence

1. Send published playlists by URL from FF-CLI and the web Display button on
   devices that advertise support. Resolve and verify the original document
   inside controld under its existing fetch quota. Keep inline casts for small,
   private documents. Supporting a large private document without publishing
   it would require a separate authenticated upload and receipt contract.
2. Separate the verified source from the playback plan. Store an immutable
   plan containing order, occurrence identity, effective display settings,
   timings, and the metadata needed for playback and labels. Retain provenance
   and the signed source behind its digest when they are needed. Avoid
   reserializing or embedding the source document in each item or occurrence.
   Keep trust decisions in controld and bind the verdict to the admitted plan.
3. Add a bounded local playlist import outside the command loop. For example,
   controld can stage the immutable plan in a private inbox and pass a digest
   and expected byte count. The player validates and imports items off the
   presentation thread, then activates the completed revision in one SQLite
   transaction. The command carries a plan ID and start occurrence. Reject
   corrupt, interrupted, expired or over-budget imports before activation.
   Preserve the current playlist on failure. Use a fixed private inbox with
   digest-addressed lookup rather than accepting filesystem paths from callers.
4. Separate list reads from state. Return playlist identity, revision, item
   count, current index and the selected/presented occurrence in status and
   reconnect messages. Page items by immutable revision under explicit byte
   and item limits. Keep existing small-list responses for old clients and
   negotiate larger-list reads with clients that need the full queue. Keep
   refresh, schedules, shuffle and restart recovery attached to stable plan
   revisions so items beyond the initial window remain playable.

Compacting JSON is useful for storage. It does not remove the item-count cap,
and compression alone would still leave parsing, persistence and snapshot
work proportional to the entire playlist. The reference-and-page design lets
the 4 MiB command limit stay in place.

Acceptance should exercise the published 9,048-item file on an FF1 using the
normal FF-CLI URL cast. Check first presentation, selection above index 4,096,
the last item, shuffle, refresh, schedule and restart recovery. Record command
latency and memory while fetching, verifying, importing and reading the list.
Include corrupted imports and stale pages. The large private-cast path has
separate upload, cancellation and quota acceptance criteria.
