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
