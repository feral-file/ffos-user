# Content policy execution plan

Status: implemented (feral-file/ffos-user#349), pending the tagged dp1-go
release that carries the content-rating extension.

## Current state

`feral-controld` resolves URL and dynamic DP-1 playlists in `dp1`, prepares
display-at schedules, scopes offline replay, probes sources, and then sends the
playlist to ff-player. The `contentpolicy` package now owns a durable v1 policy
and filters every resolved playlist before preflight, replay scope and the
player send. DP-1 ingestion still decodes JSON without core schema or signature
verification; only the content-rating extension fields are validated.

That is a legacy source-trust boundary, not mathematical signature assurance.
This change validates newly introduced label fields before policy use but does
not break existing unsigned feeds by introducing a signature migration.

## Constraints

- The daemon owns an atomic durable ContentPolicy v1 file; the player mirrors it.
- A policy update is active only after a matching acknowledgement from the
  current player generation AND durable daemon storage — **in that order**. The
  file holds only acknowledged values.
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
   playback admission and with scheduler-owned pushes, sends the candidate
   policy to ff-player, and only after a matching complete acknowledgement
   persists it and makes it the active admission policy.

   **Acknowledgement precedes persistence, deliberately.** Persisting first and
   then discovering the player refused leaves a caller told the update failed
   with a device that changed what it admits anyway — and since the file is the
   only thing a restart restores, that refused policy returns as the active one
   on the next boot. Writing only acknowledged values makes both impossible
   without a second persisted state to reconcile. (Reviewed across passes on
   #349; the earlier persist-first wording is superseded.)

   Persistence-failure recovery: if the write fails after the player accepted,
   the daemon keeps its previous policy and reports `contentPolicyUnavailable`.
   The player is briefly ahead; the reconnect sync re-pushes the stored policy
   on the next generation and puts the two back in step.
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
