# AGENTS.md - ffos-user Agent Contract

This file defines the repository-wide principles for coding agents. The detailed operating instructions live in component-level `AGENTS.md` files under `components/`.

## Repository overview
- Project: `ffos-user`, a Linux device-services repository for Feral File OS user-space components.
- Primary languages: Go and Rust.
- Runtime posture: long-running user services, systemd-managed processes, D-Bus integration, local orchestration, and constrained-device operations.

## Core principles
- Delete before adding. If the current shape is wrong, prefer removing or replacing it over layering more compatibility code on top.
- Keep architecture explicit. Components should have narrow responsibilities and communicate through visible boundaries such as D-Bus, files, sockets, or process edges.
- Prefer small helpers and simple coordinators. Use stateful orchestration only where lifecycle, caching, or recovery logic genuinely needs it.
- Optimize for future amendment. Code should be easy for later agentic sessions to understand, modify, and extend safely.
- Add comments for intent, invariants, trade-offs, and operational hazards when the logic is non-obvious. Do not comment obvious syntax.

## Overall architecture direction
- `feral-controld` is the connectivity and command orchestration daemon.
- `feral-setupd` is the setup and recovery daemon.
- `feral-sys-monitord` publishes device health and connectivity signals.
- `feral-watchdog` consumes health signals and takes recovery actions.
- UI code and daemon code should stay clearly separated. Cross-service behavior should be coordinated through explicit contracts, not hidden coupling.

## Architecture and API design
- Architecture direction: `docs/architecture.md`
- API and protocol direction: `docs/api-design.md`
- Both docs are filled. Read them before making cross-service changes or adding new interfaces.

## Release guardrail: two shipping rails
- Component **binaries** ship via the pacman package rail (`feral-service-update.sh`); systemd **unit files and user session scripts** (`users/feralfile/**`) ship ONLY via the full-image rsync rail (`feral-system-update.sh`, in the `ffos` repo).
- A change that touches BOTH rails (e.g. a daemon behavior change paired with unit/script edits, like the headless startup rework) MUST be released as a full-image version bump, never as a package-only bump. A package-only release would run new binaries under old units/scripts, leaving the fix silently inert or broken on fielded devices.
- The cross-rail startup invariants (unconditional daemon start, `chromium-ready.target` decoupling, kiosk display-wait fail-open) are pinned by `scripts/test-headless-startup-contract.sh`, which runs in the setupd test workflow and triggers on `users/feralfile/**` changes.
- The cross-rail RELEASE rule itself is enforced by `scripts/check-release-rail.sh` (release-guardrail workflow, every PR into `staging`/`main`): a PR whose diff touches both `components/**` and `users/**` fails unless it also adds a `full-image` release declaration to `RELEASES.md` (version + the `ffos` `build-image-to-cf.yml` dispatch parameters). The ledger entry is the auditable evidence; dispatching the image build in `ffos` remains the release operator's step.
- The DRM "is a display connected" predicate ("connected" only counts positively; "unknown" is headless; fail open only when NO connector status is readable) exists in THREE lockstep copies: `users/feralfile/scripts/start-kiosk.sh` (`wait_for_display`), `components/feral-watchdog/display.go` (`isDisplayConnected`), and `components/feral-controld/drm/drm.go` (`DisplayConnected`). controld is a separate Go module, so the copy cannot be imported from the watchdog. Any change to what counts as "connected" MUST be applied to all three (each has mirrored table tests; the kiosk/watchdog pair is additionally pinned by the startup contract script).

## Required workflow for substantial work
1. Read this file.
2. Read `PLANS.md` if the work is large, vague, or architectural.
3. Read the relevant component `AGENTS.md` files for the services you touch.
4. Read the relevant `.cursor/rules/` files.
5. Summarize the current flow and constraints before changing behavior.

Canonical sequence:
`context -> design -> implementation -> verification -> review`

## Shared review contract
- Use `prompts/code-review.md` for review posture and verdict shape.
- Do not commit or open a PR until the reviewer loop reaches `Verdict: accept`.

## Agent assets
- Cursor rules: `.cursor/rules/`
- Cursor sub-agents: `.cursor/agents/`
- Codex sub-agents: `.codex/agents/`
- OpenCode sub-agents: `.opencode/agents/`
