# Incremental review record

These are fresh-context local review observations during implementation. They
are not a completion review or release qualification for the whole rewrite.
Anh remains the named human change owner and release/signoff authority. The
fixes below are engineering dispositions under the requested implementation
scope, not a claim that Anh individually approved each finding.

## Runtime increment, 2026-11-17

Reviewer `review_runtime_increment`, `review_surface: local`, verdict **revise**.

| ID | Severity | Finding | Engineering disposition and evidence |
|---|---|---|---|
| F1 | Major | Policy-blocked due schedule throws out of tick, persists, and restart-loops the host | Fixed: transactional terminal schedule outcome, current artwork retained, socket remains usable. `scheduling-tests` covers initially blocked and policy-tightened tasks, restart and storage retry |
| F2 | Major | Oversized playlist URL is accepted but startup refuses it | Fixed: 16 KiB admission check before persistence; failed request leaves prior state unchanged; shared core/startup tests |
| F3 | Moderate | Restored pause timing adds host uptime to slot duration | Fixed: rebase pause clock on first tick and presentation; deterministic restart/pause tests |

Fresh-context reviewer `review_runtime_updated` inspected the updated full
runtime/Go increment, independently ran five macOS CTest groups, Go race tests
with real C++ interoperability, and the schema exchange test. Verdict **revise**:

| ID | Severity | Finding | Engineering disposition and evidence |
|---|---|---|---|
| F1 | Moderate | Numeric DP1 IDs persist successfully but break Go status decoding | Fixed after review: known identity/text admission types checked in C++, shared numeric-ID request fixtures, migration refusal test. Go status additionally returns explicit metadata failure while retaining host/occurrence observations instead of turning schema disagreement into a socket outage |

The second reviewer also traced policy retirement defects during review. The
updated implementation retires a blocked outgoing occurrence using its own
context, even while a different personal playlist loads, and reselects retained
active intent when policy relaxation makes it playable again. Regression tests
pass; the reviewer inspected those corrections before the final verdict.

The post-review DP1/status revision and packaging changes still need fresh
review. The final completion review must cover the full coordinated diff after
the remaining consumer migration, offline/presentation and FFOS work is complete.

Fresh-context `review_package_increment` inspected the post-review DP1/status
change and initial package tooling. Verdict **revise**: automatic Meson feature
detection had enabled libcurl/libdisplay-info on homelab without declaring the
package dependencies. The updated build disables automatic features, explicitly
selects EGL/GL/VA-API/Pulse/LCMS/network support, and declares curl/libdisplay-info.
Rebuild, DT_NEEDED audit and packaged-worker verification are pending. This
review independently ran the five macOS CTest groups and Go race tests; its Go
test invocation did not enable real C++ interoperability.

Fresh-context `review_recovery_increment` reviewed the full runtime boundary
after bounded renderer retries were added. Verdict **revise**: an outgoing
renderer failure during incoming loading was ignored because it did not match
the selected occurrence. Fixed by retiring the outgoing presentation, releasing
input and retaining a separately identified `presentationFailure`, without
changing the incoming selection or retry budget. A regression test covers this
sequence and duplicate late callbacks. The reviewer independently reproduced
the defect and ran six local CTest groups, schema checks and real Go/C++ race
tests. The subsequent fix and Linux runtime behavior still need fresh review.

Device smoke testing rejected package `.2`: explicit feature selection disabled
mpv's Wayland VA-API native-display path, causing `vaapi-copy`. The correction
explicitly enables `wayland` and `vaapi-wayland` and declares their package/build
dependencies. The failed evidence is retained; software copies are not accepted
as proof of the intended normal accelerated path.

Fresh-context `review_renderer_increment_updated` inspected the revised full
runtime boundary and independently ran six macOS CTest groups, real Go/C++ race
tests and the schema exchanges. Verdict **revise**:

| ID | Severity | Finding | Engineering disposition and evidence |
|---|---|---|---|
| F1 | Major | A busy producer fence was treated as a terminal renderer failure after 100 ms | Fixed: nonblocking poll skips callback-local buffers and requests another paint; no handle retained. GLB Draco/KTX2 and repeated media/input/recovery tests pass |
| F2 | Moderate | A frame copied before renderer readiness could be promoted | Fixed: require a newly copied frame after readiness and request repaint; same device suite |
| F3 | Moderate | Recovery re-applied autoPlay:false after explicit resume | Fixed: preserve the released autoplay hold across internal retries; deterministic recovery test |

These fixes and the subsequent native setup migration require a new review.

Fresh-context `review_native_setup` reviewed the full updated intermediate
implementation, including the three prior renderer corrections and native setup
transport/presentation. Verdict **accept**, no material findings. Independently
passed seven macOS CTest groups, uncached Go race tests with C++ interoperability,
schema freshness and 27 schema-validated exchanges plus an event. Inspected the
setup compositor/QR evidence and final credential-size bound. Physical output,
final UI fidelity and the unfinished coordinated migration remain outside this
intermediate acceptance; this is not final completion review or release signoff.

Fresh-context `review_playlist_update` reviewed the mode/source-refresh increment
and its current runtime/Go status interactions. Verdict **accept** after the
updated implementation; no remaining material findings in the slice. Both
engineering inspection and the reviewer found that comparing entire playlist
documents missed an outgoing occurrence after an earlier source refresh.
Fixed with an internal cast identity, stable across refreshes but renewed on
explicit casts, plus a regression for successive refreshes. The reviewer
independently reproduced source replacement followed by blocked fresh labels
and confirmed the old presentation retires. Nine macOS CTest groups, Go/C++
playerlink/status tests, 28 schema exchanges/event and shared-fixture checks
passed independently. Linux/CEF validation of this increment was still pending
at review time. Final coordinated completion review remains required.

## Source relocation, connectivity and CI slice

Fresh-context reviewer `review_reorg_connectivity` found no material findings
and returned **accept**. It independently ran mediator/playerlink race tests.
Full container compilation was still running at review time; broad migration
remains incomplete. Owner: Anh. No release or deployment qualification implied.

## Hardware finding after the source-relocation review

[BENCH-1][Major] Resolve accelerated buffer synchronization before activation.
Transparent Grit creates a 1920-square canvas but native presentation starves on
busy producer buffers. A two-millisecond poll budget did not fix it; an
implicit-fence copy/completion experiment stalled command delivery. Both changes
were reverted, and the staged runtime was restored. Evidence: BENCHMARK.md and
`evidence/grit-sync-experiments.log`. Initial disposition: **open**, blocking
qualification and activation. The tested-path correction and current disposition
are recorded below. Anh is the change owner; no acceptance of this risk is
inferred from the request to push the development branch.


## Transparent Grit correction, 2026-11-18

Fresh-context `review_grit_frame_loop`, `review_surface: local`, inspected the
full branch and correction at `689933a3`, its lifecycle/copy/readiness callers,
new device harness and both compositor captures. Verdict **accept**, no material
findings. Independently passed nine macOS CTest groups and the JavaScript syntax
check. Linux execution was not independently rerun; final benchmark, shutdown
and media/input/GLB/recovery results were pending during review. This is an
increment review, not whole-rewrite qualification.

BENCH-1 disposition: **fixed for the tested AMD FF1 path**. The root cause was host scheduling/backpressure as well as
poll-and-drop admission, not an established CEF engine defect. The new host
uses scheduled UI work, external BeginFrames, one admitted GPU copy per artwork
per host turn and a deferred refresh for dropped static frames. The real-work
regression passed 154 commands, repeated identical-URL occurrence replacement,
and changing native compositor output. The historical poll-and-skip disposition
above is superseded by this correction. Final qualification limits and benchmark
results remain in BENCHMARK.md; AMD nodcc/NVIDIA fence engine limitations remain.

The final external-frame runtime stopped through SIGTERM in 0.174 seconds,
emitted presentation metrics and exited without hitting its five-second close
or systemd eight-second stop deadline (`evidence/grit-fixed-runtime.log`). Systemd
removed remaining zygote processes after the host exited. A hung GPU can still
block the shared browser/native process and requires external recovery.


## Pacing increment, 2026-11-18

Human change owner: Anh. Review surface: local. Source through `07fb9d85`, plus
measurement/docs artifacts. Initial fresh-context `review_pacing` and final
fresh-context `review_pacing_final` both reported `Verdict: accept`, no material
findings. The final review followed exposure/remap invalidation and the rebuilt
binary measurements; it inspected the full branch and bounded feedback/host/media
lifecycle. Independently ran 10 macOS CTest groups, Go playerlink/status/setupui/
mediator tests, JS syntax and whitespace checks; reproduced all four qualified
benchmark summaries and light compositor timing. Linux/device evidence was
inspected, not independently rerun by the reviewer.

Disposition: no material finding to resolve. Performance differences and physical
scanout/hotplug/overnight/portfolio limits remain explicit in PACING.md. These
observations do not authorize deployment or qualify the whole rewrite.


## Native device UI increment, 2026-11-18

Human change owner: Anh. Review surface: local. Fresh-context
`review_native_ui` found three material lifecycle/presentation issues: captured
artwork covered by setup could time out after 60 seconds; nonfatal storage
notices had become full-screen; early media EOF could be lost before uncovered
presentation. Disposition: fixed. The host records frame availability separately,
keeps compact notices, pauses incoming media until presentation and retains a
racing EOF under the selected occurrence. Core and device evidence includes a
65-second covered-video test followed by normal EOF progression.

Fresh-context `review_device_ui_final` inspected the full updated increment and
reported only Moderate F1: the default GL backend still rendered the bundled
setup artwork black despite a working Vulkan override. Disposition: fixed by
selecting ANGLE Vulkan after image, GLB, media, input and Transparent Grit checks.
The rebuilt no-override launch independently reported the AMD Vulkan renderer,
nonzero artwork pixels and a decodable compositor QR.

Fresh-context `review_ui_backend_final` reviewed the full updated branch and
increment, including that default and its evidence: **Verdict: accept**, no
material findings. Prior reviewer independently ran ten macOS CTest groups;
Linux/device checks were performed by the implementer and inspected by review.
The review explicitly excludes full-portfolio performance, physical-panel and
overnight acceptance, and the unfinished normal controller/session/watchdog
migration. DEVICE-UI.md records the tested scope and remaining limitations.


## Display control increment, 2026-11-18

Human change owner: Anh. Review surface: local. Fresh-context
`review_display_control` found Moderate F1 (authored CSS colors disagreed between
host, bundled and mpv renderers) and F2 (a captured frame overwrote the texture
associated with prior composition during a superseding adjustment). Fixed with
shared literal-color resolution at admission/backend configuration and separate
captured/submitted textures. Fresh-context `review_display_control_final` found
Moderate F3: overlay-covered configuration updates still promoted that retained
texture without recording composition. Fixed by holding the last composed
frame/geometry during covered changes; same-configuration animation continues.
Its verdict was revise. Fresh-context `review_display_retention` reviewed the
full updated increment and surrounding code, independently reran 11 macOS CTest
groups and Go playerlink/status tests, and returned accept with no material
findings. Final FF1 runtime, GPU pacing/memory and full migration remain outside
that review's evidence.


## FF1-SKYZ2E3A media/device increment, 2026-11-18

Human change owner: Anh. Review surface: local. Fresh-context
`review_office_media` inspected the full current increment and surrounding media
path, four-thread bound, fixture generator, and viewport-independent checks.
Verdict: accept, no material findings. It independently checked whitespace,
JavaScript syntax and Python parsing; device evidence was inspected, not rerun.
The stale in-progress verification paragraph was corrected after device runs
finished. Software fallback startup/import is demonstrated, not sustained
throughput. Full controller/session/watchdog migration remains incomplete.


## Source classification and HTTPS increment, 2026-11-18

Human change owner: Anh. Review surface: local. Fresh-context
`review_source_probe` checked ownership and suggested establishing the owning
reference before request start; implemented with a Start factory.
`review_source_final` accepted the updated probe and CEF wrapper-identity fix.
`review_source_tls_final` reviewed the complete updated diff against b5afbadb,
including the CA mount and negative TLS evidence: accept, no material findings.
The reviewer inspected the five probe checks, HTTPS playback/rejection and 14
Linux CTest results, but did not rerun device tests. Physical output and normal
phone/session/watchdog migration are outside this evidence.


## Native phone input increment, 2026-11-18

Human change owner: Anh. Fresh-context `review_native_input` returned revise
with Moderate F1: CEF wheel direction is opposite CDP, so copying the sign
reversed phone pinch gestures. Fixed the adapter sign and verified both DOM
WheelEvent directions on FF1. Its suggested Enter test also found missing
default editing; added a separate carriage-return text event and verified
newline, Tab focus and form submission. Fresh-context `review_native_input_final`
reviewed the full updated increment, traced ownership/fences/cleanup/geometry,
inspected FF1 evidence and independently passed focused Go tests and diff checks.
Verdict: accept, no material findings. Full-daemon activation and physical
scanout remain outside this evidence.


## Native history increment, 2026-11-18

Human change owner: Anh. Fresh-context `review_native_history` returned revise
with Moderate F1: hardcoding activeOccurrenceKnown true stopped the phone's
polling after restart and could claim a retained showing after a failed history
write. Fixed the flag to require a presented ID matching retained history;
added null-active and unrecorded-active regression tests. Fresh-context
`review_native_history_final` reviewed the complete updated increment and its
public/mobile and replay callers, independently passed commandrouter/playerlink
tests and diff checks, and inspected the FF1 read-only evidence. Verdict: accept,
no material findings. Normal cast/session/watchdog activation remains incomplete.

## Ordinary casting/session/watchdog cutover — 2026-11-18

Human owner: Anh. Fresh-context `review_native_cutover` returned revise: blocked
boot intent could crash startup, and source refresh could bind outgoing metadata
to a new generation. Both fixed with regression coverage. `review_phone_cutover_final`
found a future-only plan retaining an older schedule; fixed pending ownership
clearing and regression, then reported accept for inspected source (rebuild was
still pending then and subsequently passed). `review_cutover_installed` found
cohort progression erasing a later accepted schedule and source-bundle omission
of fonts/color license. Both fixed; reviewer independently reproduced the corrected
schedule behavior and checked schema/tests. It returned accept after durable
ownership documentation was updated. CI interoperability now runs in the same
build container to avoid a newer-libc artifact running on the older runner.
These verdicts are observations, not human release approval. Hardware timings
and captures are retained; full acceptance gaps remain in CUTOVER.md.

Fresh-context `review_cutover_push` inspected the full updated cutover at
ffos-user 41ccf417 / ffos bf937db, including CI container alignment. It independently
passed 12 current CTests, Go/C++ interoperability, focused controld tests,
watchdog tests and diff checks. Verdict: accept, no material findings. It did
not operate hardware; newly pushed CI was pending when reviewed. Broad gaps
remain explicit in CUTOVER.md; this is not full rewrite/release acceptance.

## Sean public-engine diagnostic trial — 2026-11-18

Human owner: Anh. Fresh-context review_sean_trial inspected the bounded diagnostic
runner, semantic replay/namespace observations and explicit product-integration
limits; independently ran the three malformed-pack checks. Verdict: accept,
no material findings. A non-material capture-entry-cap caution was resolved with
a 4096-entry admission bound. Reviewer did not operate the device or certify
visual fidelity. Casey's exact engine run is deferred at Anh's request.
No production engine/backend was activated by this increment. Ordinary launch
was restored and the screen remains on. See SEAN-TRIAL-RESULTS.md.

## Public engine cutover and FF1 pacing — 2026-11-19

Human owner: Anh. The public-engine reviews found and corrected cache result
admission, native media handoff, mixed-cache fallback, and retained-frame fencing
issues. A later device test reproduced a static artwork stuck at `committed`.
Scheduling a scene redraw at readiness resolved it. Root cache mode now fails
closed when controld is unavailable, preventing a manual capture from silently
opening live. Both corrections passed the homelab reproductions.

The pacing review accepted the separation of frame callbacks from actual
presentation acknowledgements. The FF1 evidence review found that a diagnostic
unit named `public` could be treated as standalone Chrome by the output summary.
The summary now selects native Wayland feedback for that unit, with a regression
test. The fresh reviewer reran all seven matched-summary cases and the Linux
OOM hook test and reported accept for the development handoff. The full-image
workflow still lacks native package staging. Hardware results are recorded in
[PUBLIC-ENGINE-FF1.md](PUBLIC-ENGINE-FF1.md).
