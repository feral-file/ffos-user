# Offline resource ownership and migration

Implementation status: the native resource service and capture replacement are
not implemented. Live playback currently requires live resources. This document
records verified constraints and the interface design for the next increment;
it does not grant an offline-ready capability.

## Current source evidence

The controld content-addressed store is reusable for immutable body bytes.
`offlinecache/store.go` streams writes through a hash/temp-file/rename, supports
large blobs, and replaces corrupted same-hash files rather than trusting their
existence. Its existing replay path reads smaller blobs into memory, while
`replay.go:fulfillFromBlob` redirects large bodies to a loopback HTTP server.
That redirect changes origin/response behavior and is not the native design.

Existing `Resource.Headers` is deliberately a narrow allowlist in
`offlinecache/types.go:replayableResponseHeaders`. It retains CORS and selected
cross-origin headers, but discards CSP, X-Frame-Options, COOP, Permissions-Policy,
Referrer-Policy, nosniff and Vary. `capture.go:attachHandlers` stores method/URL
without the request headers or body that selected the response. The capture
body path can refetch an unranged GET. `mediacapture.go` follows redirects and
records the final body/status against the original source. These records cannot
establish complete original response semantics, even if `Coverage.Complete` is
true. Missing headers cannot be inferred to have been absent at the origin.

The pinned CEF 156 distribution's `cef_request_context_handler.h` explicitly
allows a context-level resource handler for requests whose browser/frame are
null, including service workers. It also warns that contexts sharing storage
may supply an arbitrary handler for worker requests. The native host already
creates non-shared contexts per occurrence. Browser-client-only interception
would miss workers; storage isolation and worker interception still require
runtime fixtures, not just this API observation.

## Selected boundary

Controld remains the sole cache writer, downloader scheduler, trust owner and
garbage collector. Native playback never opens or mutates its internal paths.
A separate, bounded Unix resource socket supplies response metadata and a
read-only body descriptor. It is separate from product control and screenshot
transfer. The host binds every lookup to the admitted occurrence's source;
artist requests cannot choose another cache scope or a filesystem path.

The resource transport will use Linux `SOCK_SEQPACKET`: one versioned JSON
request/reply per packet, at most 64 KiB, optionally one `SCM_RIGHTS` body FD in
the reply. Packet truncation, extra descriptors, unknown versions and invalid
peer UIDs close that connection. Parent/socket permissions match the product
socket. Per-client outstanding requests, server workers and queued disk work
are bounded. A slow lookup must not block the CEF UI/IO thread or product loop.
Response handlers read the descriptor incrementally; large assets never require
whole-file allocation or a redirect to another origin. Cancellation closes the
lookup/descriptor and releases queued work.

The lookup must include source identity, exact request URL/method and captured
variant inputs. Responses distinguish hit, miss, incomplete capture, expired
automatic entry, unsupported live dependency and corrupt/unavailable storage.
No miss is silently converted to successful empty HTML. The host decides
whether a live network attempt is permitted by the selected execution mode.
Offline mode denies uncaptured network dependencies and records them as missing;
it cannot treat an entry HTML hit as an offline-ready work.

CEF serves a hit at its original URL through resource handling, preserving
status, redirects and end-to-end headers. Normal browser origin/CSP/frame/CORS
checks still apply. Decoded bodies require recalculated length and removal of
transfer/content encodings that no longer describe them. Range/HEAD handling
must describe the stored representation, including 206/416 and validators.
Request bodies, credentials, `Vary:*`, streaming/live dependencies and mutable
session state need explicit capture support or an incomplete classification;
URL equality alone cannot safely match them. Repeated response headers must not
be truncated to the first value. Cookie-setting responses cannot be replayed as
ordinary public snapshots without an explicit session model.

## Capture and migration

The replacement capture path must retain the response actually observed,
including request variant provenance and relevant headers. Re-fetching a GET
after observing a different method/variant is not equivalent. Prefer a separate
CEF capture process from the same pinned engine package, with its own context,
resource/cgroup limits and temporary spool, feeding the controld store. This
removes the independent unpinned capture-Chromium update lifecycle. A response
filter must bound its queue and mark capture incomplete if disk backpressure
exceeds it; it may not stall live playback or call an incomplete body complete.

New records require a versioned envelope that old readers refuse, with explicit
coverage/provenance and variant identity. Reuse verified immutable blobs where
the new capture establishes their identity, but do not promote legacy records
to faithful replay by adding a version number. Preserve old records deliberately
for rollback until recapture/cutover has been verified. Garbage collection must
mark every retained record format before any new writer is enabled.

## Required verification before activation

The fixture suite must compare live and offline browser-observed behavior for
redirect chains, nested cross-origin frames, CSP/X-Frame-Options/COOP/nosniff,
CORS fonts, WASM, dedicated/shared/service workers, dynamically generated
requests, variant headers, ranges, missing dependencies and cold restart.
Include a body larger than the old fulfill threshold and measure bounded host
RSS. Test descriptor cancellation, corrupt blobs, deletion/GC with open readers,
slow disk, queue exhaustion and cache-service death while product commands stay
responsive. Retain separate statuses for observed capture coverage and verified
offline execution. Corpus and physical-output acceptance remain separate.
