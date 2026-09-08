# Content policy execution plan

Status: active

## Current state

`feral-controld` resolves URL and dynamic DP-1 playlists in `dp1`, prepares
display-at schedules, scopes offline replay, probes sources, and then sends the
playlist to ff-player. Policy does not exist, and current DP-1 ingestion decodes
JSON without schema or signature verification.

That is a legacy source-trust boundary, not mathematical signature assurance.
This change validates newly introduced label fields before policy use but does
not break existing unsigned feeds by introducing a signature migration.

## Constraints

- The daemon owns an atomic durable ContentPolicy v1 file; the player mirrors it.
- A policy update is active only after durable daemon storage and a matching
  acknowledgement from the current player generation.
- Filtering runs after resolution and before source probing. It creates an
  internal projection and removes now-invalid signatures from that projection.
- Playback origin is external context (`curated` by default, `personal` only
  when explicitly supplied) and must survive refresh and schedule ownership.
- Empty filtered casts fail with `contentBlocked` before replay scope, scheduler,
  or current-player mutation.

## Implementation

1. Add a focused `contentpolicy` package: strict wire parser, pure policy matrix,
   projection filter, and atomic JSON store.
2. Add get/set command types and command-router handling. Set serializes with
   playback admission, persists first, sends the complete policy to ff-player,
   and returns active only after a matching acknowledgement.
3. Carry content context through scheduler source and refresher sends; filter
   every resolved playlist before preflight/replay/player send.
4. Wire startup/current-generation synchronization through player-session
   reconciliation and add focused matrix, persistence, routing, refresh, and
   scheduling regression tests.

## Verification

- `gofmt -s` changed Go files
- `go test ./...`
- `go vet ./...`
- `golangci-lint run --new-from-rev=HEAD~1 ./...`
- fresh-context completion review using `prompts/code-review.md`

## Rollout

Ship inactive defaults first. `blockUnratedCurated` remains false until the
catalog audit and fleet/player acknowledgement proof are complete. No service
unit or deployment changes are part of this patch.
