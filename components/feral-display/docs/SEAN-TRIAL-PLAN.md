# Sean's three-scenario trial

Baseline: ffos-user 5cb326f8, ffos bf937db, clean worktrees. Capsule upstream
`display-protocol/dp1-capsule` e6edbc7b41e1122c05ded1baa1ac520f02048b52.

Current host embeds CEF browser/GPU lifecycle; renderer subprocesses do not meet
an independently replaceable engine boundary. Offline native replay is absent.
The capsule pins a complete Chromium 155 rootfs SHA f0d8ced301b3ff85d6e94dbac22ecd991c5a978e03c8af5e003a6f4a5390d400,
not a CEF version. Its archive is not included or published in GitHub releases.

Constraints: preserve normal signed control and durable host authority; use
unmodified public engine bytes; no arbitrary evaluation in product protocol;
no origin rewriting or disabled security; preserve capsule harness semantics.
Keep the current install/rollback usable, screen awake, feature branches only.

Verification before implementation:
1. Instrument holz diagnostically to observe WebGPU adapter/device and worker
   creation, successful geometry messages, console/network errors and frames.
2. Run Casey's existing capsule using its exact engine/harness and signature;
   compare frame hashes, mismatch/tamper refusal, network isolation and restart.
3. Exercise offline cold starts with empty browser profiles and kernel-blocked
   network, retaining response headers/origins and missing-dependency failures.
4. Verify one native playback/control lifecycle across live/offline/capsule,
   including engine process failure with host commands/state/UI surviving.
5. Repeat existing-artwork, media/input/recovery tests after engine changes;
   benchmark matching engine/seed/viewport; retain visual and process evidence.

Design branches: a CEF subprocess worker preserves current GPU composition but
cannot execute an arbitrary capsule's public Chromium binary. A public Chromium
worker can select capsule engines, but needs a real accelerated surface/input
contract rather than screenshot polling. Evaluate compositor embedding and
engine capabilities before choosing; no CPU capture loop for normal artwork.
Implement/test independently while the pinned capsule archive is located.
Required completion review uses the full updated diff and explicit gaps.
