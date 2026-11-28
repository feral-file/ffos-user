# Sean trial: live software, offline execution and public engine separation

Date: 2026-11-18. Device FF1-SKYZ2E3A, Radeon 680M / RADV Rembrandt,
FFOS Linux, Cage/Wayland; panel left on. Baseline ffos-user 5cb326f8, ffos
bf937db. Capsule repo inspected read-only at
`display-protocol/dp1-capsule` e6edbc7b41e1122c05ded1baa1ac520f02048b52.
Source request: https://github.com/feral-file/feral-file/issues/3615#issuecomment-6960444152

## Result and scope

The bounded experiments demonstrate live and offline holz in separate public
Chromium processes, including its real WebGPU/worker behavior, on this FF1.
Two public engine versions were selected without rebuilding the trial runner.
Transparent Grit also rendered offline at its original URL after a fresh-profile
start with kernel networking isolated. Missing worker code produces a concrete
failure, rather than an empty-page pass. Original response headers and origins
are retained; no artist script was edited or served from a different URL.

**This is not activation of a unified public-engine backend in the C++ product.**
The ordinary installed host still embeds CEF's browser process. The new test
runner is diagnostic tooling; its private CDP pipes are not a replacement product
control API. It neither composites the external browser inside native device UI
nor forwards phone gestures into it. Those remain required implementation work
before claiming Sean's one-path architecture is delivered. Current CEF and this
public-browser experiment must not be described as one product runtime.

Anh explicitly deferred the Casey capsule experiment because its exact engine
archive is unavailable. Its manifest pins SHA-256
`f0d8ced301b3ff85d6e94dbac22ecd991c5a978e03c8af5e003a6f4a5390d400`,
Chromium 155.0.8059.84, plus a deterministic harness/reference. The repository
contains the capsule and engine manifest, but no engine archive or release
asset. No different-engine render is substituted for capsule reproduction.

## Direct answers supported by observations

| Sean's point | Trial evidence | Claim boundary |
|---|---|---|
| Original live holz | Native CEF 156.0.8078.96 and public Chrome for Testing 156.0.8078.80 loaded https://leanderherzog.ch/2026/holz/; inspected images show geometry | Renderer/compositor captures, not physical panel photographs or artist reference approval |
| WebGPU | Browser exposes WebGPU; geometry is visible; public Grit GPU diagnostic reports RADV Rembrandt via ANGLE Vulkan | No general WebGPU corpus/performance qualification |
| Workers | Original holz creates a module geometry worker; an additional diagnostic instance of unchanged geometry.mjs returned 49 bodies / 235,134 geometry bytes for seed123, batch0, candidates128 | Extra worker probe is labelled diagnostic; artist script itself was not instrumented |
| Offline original origin | Fresh profiles, same HTTPS URL/origin, original CSP; response bodies/headers supplied at browser resource interception | A trusted capture substitutes responses; it is not a new TLS certificate or live server |
| Cold process restart | A second fresh-profile offline run again showed geometry, original URL and WebGPU | This is a process cold start, not an FFOS power-cycle/OTA acceptance test |
| Kernel isolation | Browser net namespace differs from host; down loopback only, zero interface bytes, CapEff/CapBnd zero, NoNewPrivs=1 | Wayland/GPU access is deliberately retained; shared GPU-driver failure remains possible |
| Missing dependency | Removing geometry.mjs yields an explicit recorded miss and visible artist “Geometry worker failed” UI, zero objects | Navigation and canvas existence alone would have been false positives |
| Public engine replacement | Same runner replayed holz with system Chromium 150.0.7871.114 and public CfT 156.0.8078.80 | Operator-selected engines, not capsule-selected engine execution |
| Unmodified public executable | CfT executable hash matches its downloaded archive; system Chromium pacman check: 276 files, zero altered | Distribution builds/graphics flags still matter; no local Chromium source patch was applied |
| Engine process failure | SIGKILL of separate experimental browser left native host instance, generation, occurrence and signed device control intact | Does not imply the embedded CEF browser can die independently of its current host |

## Offline completeness and semantic checks

Holz's artwork code executes offline with its worker and WebGPU. Cloudflare's
analytics POST to `/cdn-cgi/rum?` remains unavailable and explicitly recorded.
Both live capture and offline results therefore retain `captureComplete:false`.
It would be incorrect to claim that every network interaction was preserved.
Removing the worker module produces a distinct failed-work observation, proving
that entry HTML alone is insufficient.

Capture retains actual response-stage bodies and repeated end-to-end headers,
including the artist's CSP. It removes content/transfer encoding and wire length
because the API delivers decoded bytes. Replay retains redirect statuses and
Location headers, original URLs, request methods and declared Vary selections.
Cookies, POST and unsupported variants are refused/recorded. Browser cache is
disabled and every run uses a new profile. The sandbox blocks uncaptured live
network attempts; the host's external control/network stays available.

This is bounded diagnostic capture (32 MiB resources, 4096 records), not the
streaming production cache interface. No conclusion is drawn for credentials,
large media, service/shared workers, arbitrary runtime-generated dependencies or
general offline-ready classification. Dedicated workers used by holz are tested.
The complete native cache/onchfs bridge remains absent.

## Existing-artwork checks beyond holz

Transparent Grit AP was captured live from the official Social Codes playlist's
original IPFS gateway URL, including its redirect to the slash URL. A fresh-profile
kernel-isolated replay produced visible artwork with no replay misses or runtime
exceptions. The live site returned a favicon 404, retained in live observations;
replay preserves that response. GPU diagnostics identify the AMD Vulkan backend.
These are functional observations, not a matched pacing measurement.

The earlier ordinary native-kiosk trial already cast all ten Social Codes works
through signed FF-CLI and retained visible captures, plus systemd/watchdog recovery
and input/history checks. See CUTOVER.md and evidence/cutover-20261118. We did not
repeat the entire portfolio offline. The existing matched Grit result remains
about 15.3% slower than standalone Chrome for the offscreen product path; this
experiment does not erase or remeasure it.

## Evidence and reproducibility

`trial/browser_trial.py` uses private inherited CDP pipes to launch a public
engine. See trial/README.md for commands and limits. The test runner does not
alter the installed binaries. Evidence lives in `evidence/sean-trial-20261118`:
public-live, public-offline, public-150-offline, offline-restart, missing-worker,
grit-live, grit-offline, native holz observations, engine identities and namespace/
process-failure records. Screenshots were inspected for visible artwork/error UI.
The retained runs include earlier tool iterations; the current runner additionally
records its own hash and validates malformed packs before starting an engine.
Three malformed-index safety tests pass. No benchmark is inferred from artwork's
own FPS text or concurrent test runs.

Fresh-context review_sean_trial returned accept with no material findings for
this diagnostic scope. It inspected code/JSON and reran malformed-pack tests;
it did not operate hardware or independently certify visual fidelity. Capsule,
product integration, physical display, security-update and overnight acceptance
are not established by this trial.
