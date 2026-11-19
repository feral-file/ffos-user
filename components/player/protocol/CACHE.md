# Native cache and request-policy API, version 1

Controld owns capture, content-addressed blobs, annotations and eviction. The
native host never parses the cache directory layout or writes cache records.
Its separate broker socket is `<player-socket-directory>/cache.sock`, owned by
the service UID, mode 0600, inside the same private 0700 directory. Both endpoints
validate the other UID. Artist/browser namespaces do not contain this socket.
The broker starts independently of artwork and remains outside renderer failure.

This read-only API uses HTTP/1.1 over a Unix socket, not the command stream.
POST one UTF-8 JSON object, maximum 64 KiB, with `source`, `url`, `method`, and
`headers` (a string-to-string request-header map). Source and URL each have a
16 KiB limit. Unknown fields and trailing JSON are rejected. URI `/v1/` selects
the version. Unsupported versions return 404; malformed input returns 400.

* `/v1/replay` returns the captured GET or HEAD response for the exact source,
  URL and method. HEAD may use a recorded GET, with no response body. It keeps
  original status and end-to-end headers, including repeated CSP fields.
  Transfer encoding/length describe the decoded replay and are removed at
  capture. Redirect Location remains an original-origin browser redirect.
  A legacy native-media entry can instead return bodyless `nativeMime` metadata.
  The host hands the same original source to the native file-open path; those
  legacy response bytes never execute in a browser.
* `/v1/open` returns a broker-verified immutable file path for a native decoder.
  The host opens it read-only with O_NOFOLLOW, checks regular-file type and size,
  then transfers the open descriptor over the private media channel. The worker
  sees the descriptor, not the cache path. GC may remove the name before open:
  that is an explicit unavailable result. Once open, unlink cannot invalidate
  the descriptor. No process besides controld mutates the cache. Large native
  media is decoded/seeks through the file, without JSON/base64 or full buffering.
* `/v1/policy` applies controld's established gateway and User-Agent policy to a
  live request, including its current media-health gateway override. It performs
  no fetch and stores no headers/cookies. `requestUrl` and optional
  `requestHeaders` describe the approved request. Ordinary HTTP requests continue
  unchanged if this optional policy service is temporarily unavailable; this
  never invents a replacement origin. The artwork/browser cannot grant trust.

Successful HTTP 200 bodies start with a four-byte unsigned big-endian length
(1–131072), then that many UTF-8 JSON metadata bytes, then decoded response bytes.
Metadata: `version:1`, `status`, `headers:[{name,value}]`, `size`, `sha256`,
`complete`; native-open additionally has `path`, policy has `requestUrl` and
optional `requestHeaders` and `replayMode`. At occurrence creation, an async
policy lookup selects `isolated` for a qualified manual capture or `mixed` for
a fresh automatic capture. Manual captures replay without WAN, including origin
rot while WAN remains online. Automatic captures try cache first and retain
approved live fallback on misses or native-file eviction. Replay body length equals `size`. Native-open sends
metadata only; `size` describes the file. Policy has zero body size. This is
separate from native playback receipts: a cache hit is never a presentation
success, and coverage is an observation, not proof of future offline execution.

404 means absent/expired data, 409 ambiguous capture, 422 unsupported request or
legacy response policy, 500 corrupt/storage failure, 503 bounded capacity spent.
Replay supports public GET/HEAD snapshots. Cookie/Authorization/Range variants
and state-changing requests fail explicitly. A static native file supports
seeking through the decoder instead of replaying HTTP Range. Browser fulfillment
is limited to 32 MiB per response; larger browser dependencies fail explicitly.
Do not redirect them to a different origin or report them as offline-ready.
Native files have no browser-fulfillment limit.

Four body I/O jobs run concurrently, with 128 queued jobs, with at most64 outstanding operations per occurrence including
completed replies not yet consumed; policy has two separate
workers and a bounded queue. Request headers time out after 1 second, request
bodies after 5 seconds, writes after 30 seconds. Native-open waits at most
30 seconds; browser replay waits 5 seconds. Command/input admission uses the
independent player socket. Browser pipe queues are capped at 64 MiB and
256 operations. Cancellation retires an occurrence's inbox; stale results cannot
configure its replacement.

Version-0/1 browser captures discarded some security headers. They require
recapture for browser replay. New capture records preserve repeated applicable
headers and reject cookie/non-encoding Vary snapshots. Onchfs virtual origins
have their complete synthetic policy recorded by controld. A partial import or
missing dependency stays incomplete and is surfaced in native UI and runtime
status. Network-isolated browser processes cannot silently fetch a missing live
dependency. The operator `cmd/cache-import` is an offline migration tool, not a
second runtime cache writer; stop controld/capture jobs before using it.

Chromium158 worker targets do not implement Fetch.enable (measured -32601),
but page-session interception does replay the tested dedicated worker's runtime
fetch. That domain error alone is not evidence of an unsupported worker. The
original-origin fixture tests worker requests, fonts, WASM, redirects and frames.
Service-worker registration still fails in a network-isolated fresh profile.
Service-worker target attachments set unsupportedWorkerTargets, force coverage
incomplete and produce a native/external notice. An engine-wide service-worker
resource seam and broader worker fixtures remain required for portfolio acceptance.

Go and C++ consume cache-fixtures.json. The Linux cache-client test fills64
accepted policy completions without draining, rejects the65th, then verifies
that draining releases capacity. Peer checking uses the actual connected Unix
socket, independent of libcurl's transfer-stage active-socket reporting.
