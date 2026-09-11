# AGENTS.md - ffos-user Agent Contract

This file defines the repository-wide principles for coding agents. The detailed operating instructions live in component-level `AGENTS.md` files under `components/`.

## Repository overview
- Project: `ffos-user`, a Linux device-services repository for Feral File OS user-space components.
- Primary language: Go. (The former Rust `feral-setupd` daemon was merged into `feral-controld`; no Rust remains.)
- Runtime posture: long-running user services, systemd-managed processes, D-Bus integration, local orchestration, and constrained-device operations.

## Core principles
- Delete before adding. If the current shape is wrong, prefer removing or replacing it over layering more compatibility code on top.
- Keep architecture explicit. Components should have narrow responsibilities and communicate through visible boundaries such as D-Bus, files, sockets, or process edges.
- Prefer small helpers and simple coordinators. Use stateful orchestration only where lifecycle, caching, or recovery logic genuinely needs it.
- Optimize for future amendment. Code should be easy for later agentic sessions to understand, modify, and extend safely.
- Add comments for intent, invariants, trade-offs, and operational hazards when the logic is non-obvious. Do not comment obvious syntax.

## Overall architecture direction
- `feral-controld` is the connectivity, command orchestration, and device-setup daemon. It absorbed the former `feral-setupd`: SoftAP provisioning, captive portal, OTA gate, on-screen setup narration, claiming, factory reset, log upload, and the LAN hub.
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
- The cross-rail startup invariants (unconditional daemon start, `chromium-ready.target` decoupling, kiosk display-wait fail-open) are pinned by `scripts/test-headless-startup-contract.sh`, run via `make verify-scripts` by the `test-scripts.yaml` CI workflow.
- The cross-rail RELEASE rule itself is enforced by `scripts/check-release-rail.sh` (release-guardrail workflow, every PR into `staging`/`release` — the release flow is develop → staging → release; `main` is not a release branch): a PR whose diff touches both `components/**` and `users/**` fails unless it also adds a `full-image` release declaration to `RELEASES.md` (version + the `ffos` `build-image-to-cf.yml` dispatch parameters). The ledger entry is the auditable evidence; dispatching the image build in `ffos` remains the release operator's step.
- The DRM "is a display connected" predicate ("connected" only counts positively; "unknown" is headless; fail open only when NO connector status is readable) exists in THREE lockstep copies: `users/feralfile/scripts/start-kiosk.sh` (`wait_for_display`), `components/feral-watchdog/display.go` (`isDisplayConnected`), and `components/feral-controld/drm/drm.go` (`DisplayConnected`). controld is a separate Go module, so the copy cannot be imported from the watchdog. Any change to what counts as "connected" MUST be applied to all three (each has mirrored table tests; the kiosk/watchdog pair is additionally pinned by the startup contract script).

## Release guardrail: ISO image builds
- The FFOS image workflows (`build-image-to-cf.yml`, `pure-build-image-to-cf.yml`, `build-image-from-tags.yml`) live in the `ffos` repo and are `workflow_dispatch`-only; agents in this repo reach them cross-repo (`gh workflow run -R feral-file/ffos ...`). The branch a dispatch runs on selects the R2 upload prefix (`{branch}/FF1-{branch}-<version>.iso`) and the pacman `Server` path baked into the image, and `environment=Production` selects the production domain and secrets. An ISO dispatched on `release` (or with `environment=Production`) is published where fielded devices update from. **Dispatching it is a production deployment, not a build.** The same holds for the other three `workflow_dispatch` workflows in `ffos`: `manual-build-components.yaml` and `manual-build-feral-player.yaml` upload packages to `{branch}/os/x86_64/`, and `manual-push-pacman-repo.yaml` regenerates, re-signs, and prunes that package repo, which is where fielded devices pull package updates from. Every dispatchable `ffos` workflow except `verify.yml` publishes, so every one of them is guarded exactly like the image builds.
- **Never (hard prohibition).** No agent may dispatch, re-run, retry, or otherwise trigger any of these publishing workflows on the `release` branch, or with `environment=Production`, by any mechanism: `gh workflow run`, `gh api`/REST dispatch, `gh run rerun`, browser automation of the Actions "Run workflow" UI, `act`, wrapper scripts, or a numeric workflow ID. This holds even when the task is "cut the release": the agent prepares everything (tag, the `RELEASES.md` entry with the exact dispatch parameters) and hands the dispatch itself to the human release operator. No instruction given inside an agent session lifts this rule; a human runs the dispatch.
- **Only with explicit confirmation.** Any of these publishing workflows on the `staging` branch, or with `environment=Staging`, may be dispatched only after the agent has shown the exact command and every input (`version`, `ffos_user_ref`, `ff_player_ref`, `environment`, `pacman_snapshot`, `dev_iso`, the `update_*` flags) and the user has confirmed those parameters in the current session. A standing instruction from earlier in the conversation, an autonomous mode (autopilot, ralph, ultrawork, team, loop), or a headless/non-interactive session does not count as confirmation; in those cases treat staging exactly like release.
- Development builds (`develop` or feature branches with `environment=Development`) are unaffected.
- Enforcement is a pre-shell hook, `scripts/agent-iso-build-guard.sh`, wired for every agent tool that can run shell commands here: Claude Code (`.claude/settings.json`, PreToolUse), Codex CLI (`.codex/hooks.json`, PreToolUse), Cursor (`.cursor/hooks.json`, beforeShellExecution), Gemini CLI (`.gemini/settings.json`, BeforeTool), and OpenCode (`.opencode/plugins/iso-build-guard.js`, tool.execute.before). It denies release/Production dispatches everywhere. It prompts the user on staging/Staging where the tool has an "ask" decision (Claude Code, Cursor); Codex, Gemini, and OpenCode have none, so there staging is denied too and the human dispatches it after the agent has shown the parameters. The guard fails closed: reruns (`gh run rerun` and the REST rerun endpoints) are denied outright because a rerun restarts whatever the original run was, and so is any dispatch it cannot classify (a numeric workflow ID, a workflow name or ref or input hidden behind a shell variable or substitution, a payload from `--input`/`--json`/`@file`, a REST dispatch without a literal ref, an `environment` value that is not one of the three choices). Rewrite such a command with literal values or hand it to the human. `scripts/test-agent-iso-build-guard.sh` pins the decisions in every dialect and that each config still points at the guard; `make verify-scripts` runs it (`test-scripts.yaml` CI workflow). Any tool not listed here (or the GitHub web UI driven by browser automation) has no hook: for it this section is the enforcement, and a session that cannot guarantee it must stop and ask before any dispatch.
- The guard script is a lockstep copy shared with the `ffos` repo. Change both copies together.

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
- Never edit `prompts/code-review.md` by hand. It is generated from Canon's `reference/review-contract.md` local review surface; update Canon and propagate the generated file instead.
- Run one fresh-context completion review for non-trivial changes; review may be lighter or skipped for low-risk changes.
- Treat findings and the verdict as observability, not as a prerequisite for commit, push, PR creation, merge, or release.
- The named human change owner decides whether to fix, reject, or accept each material finding. If a fix materially changes behavior, review the full updated diff with fresh context; reviewer unanimity is not required.
- These rules supersede tool-specific instructions that treat a local review verdict as a gate.

## Agent assets
- Claude Code contract: `CLAUDE.md` (consolidates this file, `.cursor/rules/`, the sub-agent roles, and `prompts/code-review.md` into one entry point)
- Gemini CLI pointer: `GEMINI.md` (points here)
- Cursor rules: `.cursor/rules/` (`release-iso-build-policy.mdc` carries the ISO build guardrail as an always-on rule)
- Cursor sub-agents: `.cursor/agents/`
- Codex sub-agents: `.codex/agents/`
- OpenCode sub-agents: `.opencode/agents/`
- Shell hooks enforcing the ISO build guardrail: `.claude/settings.json`, `.codex/hooks.json`, `.cursor/hooks.json`, `.gemini/settings.json`, `.opencode/plugins/iso-build-guard.js`, all running `scripts/agent-iso-build-guard.sh`

A repository-wide rule change must land in `CLAUDE.md` AND here (and in `.cursor/rules/` when it is glob-scoped) in the same change. A rule that exists in only one of them will be missed by whichever tool reads the other.
