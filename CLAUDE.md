# CLAUDE.md - ffos-user Contract for Claude Code

This is the Claude Code entry point for this repository. It consolidates every
repository-wide rule that also ships to the other agent tools, so a session can
start from this one file.

## Relationship to the other agent assets

These files are peers, not layers. They must stay in sync:

| File | Audience |
|---|---|
| `CLAUDE.md` (this file) | Claude Code |
| `AGENTS.md` | Codex, OpenCode, and any tool reading the generic contract |
| `.cursor/rules/*.mdc` | Cursor (glob-scoped rules) |
| `.cursor/agents/`, `.codex/agents/`, `.opencode/agents/` | per-tool sub-agent definitions |
| `prompts/code-review.md` | the shared review contract all tools use |
| `PLANS.md` | the shared execution-plan contract |
| `components/*/AGENTS.md` | per-component operating instructions (NOT duplicated here) |

**Amendment duty:** a change to any repository-wide rule must be applied to this
file AND to `AGENTS.md`/`.cursor/rules/` in the same change. A rule that exists
in only one of them is a bug — the next session will read the other one.
Component-level rules live only in `components/*/AGENTS.md`; do not copy them
here, read them there.

**Which file wins.** These are one contract written for different readers, so a
divergence is always a defect to fix, never a choice to make. When you find one:

| Subject | Authority |
|---|---|
| Architecture, API/protocol direction | `docs/architecture.md`, `docs/api-design.md` |
| A specific component's rules | that component's `AGENTS.md` |
| Release rails, review posture, plans | `RELEASES.md`, `prompts/code-review.md`, `PLANS.md` |
| Everything else repository-wide | `AGENTS.md`, with this file as its consolidated copy |

Fix the divergence in the same change rather than following whichever copy you
happened to read — that is exactly how two entry points end up enforcing
incompatible rules. This is not hypothetical: the architecture/API sections
below sat contradicting `AGENTS.md` for exactly that reason (see the note
there).

---

## Repository overview

- Project: `ffos-user`, a Linux device-services repository for Feral File OS
  user-space components.
- Primary language: Go. (The former Rust `feral-setupd` daemon was merged into
  `feral-controld`; no Rust remains. Rust guidance below is retained because the
  contract still carries it, not because there is a crate to run it on.)
- Runtime posture: long-running user services, systemd-managed processes, D-Bus
  integration, local orchestration, and constrained-device operations.

## Core principles

- **Delete before adding.** If the current shape is wrong, prefer removing or
  replacing it over layering more compatibility code on top.
- **Keep architecture explicit.** Components should have narrow responsibilities
  and communicate through visible boundaries such as D-Bus, files, sockets, or
  process edges.
- Prefer small helpers and simple coordinators. Use stateful orchestration only
  where lifecycle, caching, or recovery logic genuinely needs it.
- **Optimize for future amendment.** Code should be easy for later agentic
  sessions to understand, modify, and extend safely.
- Add comments for intent, invariants, trade-offs, and operational hazards when
  the logic is non-obvious. Do not comment obvious syntax.

## Overall architecture direction

- `feral-controld` is the connectivity, command orchestration, and device-setup
  daemon. It absorbed the former `feral-setupd`: SoftAP provisioning, captive
  portal, OTA gate, on-screen setup narration, claiming, factory reset, log
  upload, and the LAN hub.
- `feral-sys-monitord` publishes device health and connectivity signals.
- `feral-watchdog` consumes health signals and takes recovery actions.
- UI code and daemon code should stay clearly separated. Cross-service behavior
  should be coordinated through explicit contracts, not hidden coupling.

## Architecture and API design

- Architecture direction: `docs/architecture.md`
- API and protocol direction: `docs/api-design.md`
- Both docs are filled. Read them before making cross-service changes or adding
  new interfaces.

---

## Release guardrail: two shipping rails

This is the highest-consequence rule in the repository. Read it before touching
anything under `users/**` or shipping a behavior change.

- Component **binaries** ship via the pacman package rail
  (`feral-service-update.sh`); systemd **unit files and user session scripts**
  (`users/feralfile/**`) ship ONLY via the full-image rsync rail
  (`feral-system-update.sh`, in the `ffos` repo).
- A change that touches BOTH rails (e.g. a daemon behavior change paired with
  unit/script edits, like the headless startup rework) MUST be released as a
  full-image version bump, never as a package-only bump. A package-only release
  would run new binaries under old units/scripts, leaving the fix silently inert
  or broken on fielded devices.
- The cross-rail startup invariants (unconditional daemon start,
  `chromium-ready.target` decoupling, kiosk display-wait fail-open) are pinned by
  `scripts/test-headless-startup-contract.sh`, run via `make verify-scripts` by
  the `test-scripts.yaml` CI workflow.
- The cross-rail RELEASE rule itself is enforced by
  `scripts/check-release-rail.sh` (release-guardrail workflow, every PR into
  `staging`/`release` — the release flow is develop → staging → release; `main`
  is not a release branch): a PR whose diff touches both `components/**` and
  `users/**` fails unless it also adds a `full-image` release declaration to
  `RELEASES.md` (version + the `ffos` `build-image-to-cf.yml` dispatch
  parameters). The ledger entry is the auditable evidence; dispatching the image
  build in `ffos` remains the release operator's step.
- The DRM "is a display connected" predicate ("connected" only counts
  positively; "unknown" is headless; fail open only when NO connector status is
  readable) exists in THREE lockstep copies:
  `users/feralfile/scripts/start-kiosk.sh` (`wait_for_display`),
  `components/feral-watchdog/display.go` (`isDisplayConnected`), and
  `components/feral-controld/drm/drm.go` (`DisplayConnected`). controld is a
  separate Go module, so the copy cannot be imported from the watchdog. Any
  change to what counts as "connected" MUST be applied to all three (each has
  mirrored table tests; the kiosk/watchdog pair is additionally pinned by the
  startup contract script).

---

## Required workflow for substantial work

1. Read this file.
2. Read `PLANS.md` if the work is large, vague, or architectural (contract
   summarized below).
3. Read the relevant component `AGENTS.md` files for the services you touch.
4. Read the relevant rules below for the area you are changing.
5. Summarize the current flow and constraints before changing behavior.

Canonical sequence: `context -> design -> implementation -> verification -> review`

### Execution-plan contract (`PLANS.md`)

Use an execution plan when:
- the request changes behavior across multiple services
- the work affects CI, release safety, or shared contracts
- multiple designs are plausible
- the current architecture is unclear or under-documented

Do NOT use an execution plan when:
- the change is a narrow fix with obvious scope
- the user already supplied a concrete step-by-step plan
- the work is a small documentation or tooling edit

Required plan output:
1. Current-state summary
2. Constraints and invariants
3. Risks and unknowns
4. Viable design branches with trade-offs
5. Test and verification plan first
6. Recommended staged rollout

Repository-specific planning rules:
- Prefer deleting or simplifying complex paths before adding new abstractions.
- Treat system boundaries as first-class: D-Bus contracts, systemd behavior,
  local files, network boundaries, and device lifecycle constraints.
- If architecture or API guidance is needed, read `docs/architecture.md` and
  `docs/api-design.md` first. If those docs don't cover the case, call it out
  explicitly rather than inventing permanent rules silently.
- For CI changes, include failure modes such as jobs not triggering, overly broad
  triggers, flaky coverage tools, or silent lint regressions.

---

## Rules by area

Each section notes the scope it came from, so a rule that Cursor applies by glob
is applied here by the same judgment.

### Repository contract — applies to `components/**`, `.github/workflows/**`, `AGENTS.md`

- Prefer replacing or deleting flawed code paths over preserving brittle
  compatibility behavior.
- Keep service boundaries explicit. Cross-service behavior should remain legible
  from interfaces and logs.
- Prefer stateless, testable helpers by default.
- Add intent-rich comments for non-obvious logic, especially around lifecycle,
  retries, race prevention, state transitions, protocol details, and operational
  trade-offs.
- When architecture or API rules are unclear, reference `docs/architecture.md`
  and `docs/api-design.md` instead of silently inventing permanent repository
  policy; if they genuinely do not cover the case, say so rather than deciding
  it in passing.
- Completion checks: relevant lint, format, and test gates were considered and
  run where possible; new complexity is justified in comments or docs when future
  maintainers would otherwise have to rediscover the rationale.

### Go development standards — applies to `components/**/*.go`, `.golangci.yml`

**Readability and structure**
- Follow standard Go guidance, especially Effective Go and Go Code Review
  Comments.
- Prefer short, explicit functions with clear names over highly generic helpers.
- Keep packages cohesive. If a file grows because it mixes transport,
  orchestration, parsing, and state logic, split it.
- Prefer constructor-injected dependencies and small interfaces owned by the
  consumer package.
- Keep exported APIs minimal and documented.
- Avoid hidden global state unless it is a true process singleton and the
  lifecycle is obvious.

**Service architecture**
- Services should remain explicit daemons with clear startup, steady-state, and
  shutdown phases.
- Use structured logging with stable field names.
- Implement graceful shutdown with signal handling, context propagation, and
  cleanup ordering.
- Any retry loops, watchers, or goroutine ownership rules should be documented in
  comments when non-obvious.

**Error handling**
- Return errors with enough context to explain the failed operation.
- Do not hide errors unless the failure is intentionally best-effort and the
  comment explains why that is safe.
- Prefer explicit sentinel or typed handling only when callers need branching
  behavior.
- Avoid panic in production paths. If panic is retained for an invariant,
  document why the invariant is process-fatal.

**Concurrency and lifecycle**
- Propagate `context.Context` through long-lived or cancelable operations.
- Make goroutine ownership obvious. The creator should define shutdown and error
  semantics.
- Guard shared mutable state deliberately and comment on invariants if
  synchronization is subtle.
- Prefer deterministic timers and injected clocks in test-sensitive code.

**Code organization**
- Group related functionality in separate files such as `main.go`, `config.go`,
  `dbus.go`, `watcher.go`, `logger.go`, or `service.go`.
- Prefer `internal` seams via package structure and unexported helpers over
  deeply coupled utility packages.
- Store configuration, filesystem paths, and external command behavior behind
  narrow seams so tests can fake them.

**Lint posture**
- Prefer strict readability-oriented linting for new code.
- `nolint` requires a targeted rule name and a short justification when the
  reason is not obvious from the line itself.
- If an existing file has legacy lint debt, avoid spreading that pattern into new
  code.

### Intent commenting — applies to `components/**`

Comments are additional maintenance context for future amendment sessions, not a
narration of syntax.

Add comments when code is carrying:
- invariants that must stay in sync across functions or tasks
- shutdown or startup ordering assumptions
- protocol or wire-format constraints
- concurrency assumptions
- trade-offs between readability, performance, or operational safety
- reasons a fallback, retry, cache, or guard exists
- known hazards if a future edit simplifies the code incorrectly

Preferred comment shape:
- why this exists
- what must remain true
- what failure mode is being prevented
- why the chosen trade-off was accepted

Anti-pattern: comments that merely restate the next line of code.

### Testing and quality — applies to `components/**`, `.github/workflows/**`, `.golangci.yml`

Required sequence:
1. Isolate the smallest behavior seam.
2. Add or update tests around that seam.
3. Implement the change.
4. Run the relevant format, lint, and test commands.
5. Update docs or comments when future maintainers would otherwise lose context.

Test posture:
- Prefer table-driven tests for behavioral branches.
- Test failure paths and boundary conditions, not only happy paths.
- Use fakes or test servers for D-Bus, HTTP, filesystem, and time-dependent
  behavior when possible.
- Keep tests focused on behavior, not private implementation details.

CI posture:
- Ensure workflows trigger on source and config changes, not only on the
  happy-path file globs.
- Prefer reusable checks, explicit permissions, and fail-fast guardrails for lint
  and test pipelines.
- If a verification command cannot be run locally, state that clearly and keep
  the workflow authoritative.

### systemd services — applies to `components/**`, `users/feralfile/systemd-services/**`

**Runtime expectations**
- Services are long-running user-space daemons. Startup, readiness, shutdown, and
  restart behavior must be explicit.
- Keep unit responsibilities narrow and observable through logs and health
  signals.
- Avoid introducing hidden cross-service coupling through filesystem side effects
  or implicit ordering.

**Operational safety**
- Handle termination signals and cleanup paths deliberately.
- Distinguish fatal startup failures from recoverable steady-state failures.
- Be careful with retry loops, watchdog behavior, and system-triggered restarts;
  comment on trade-offs when the behavior is non-obvious.
- When a service interacts with D-Bus, systemd readiness, or external
  subprocesses, preserve clear ownership and timeout handling.

**Documentation expectations**
- If a change affects startup order, readiness signals, required environment,
  service names, or state paths, update the relevant docs or service notes in the
  same change.

### Architecture — applies to `components/**`

`docs/architecture.md` is the canonical contract and is filled in. Read it
before cross-service changes. Alongside it:
- keep service boundaries explicit
- avoid cross-cutting abstractions without strong justification
- record trade-offs in comments when changing architecture-sensitive paths
- update `docs/architecture.md` when you establish a rule it does not yet state

### API and protocol design — applies to `components/**`

`docs/api-design.md` is the canonical contract and is filled in. Read it before
adding or changing an interface. Alongside it:
- keep interfaces explicit and narrow
- prefer additive changes over ambiguous field reuse
- comment on compatibility assumptions near protocol code
- update `docs/api-design.md` when you establish a rule it does not yet state

> Both sections above previously read "the canonical contract has not been
> finalized", copied verbatim from `.cursor/rules/architecture-tbd.mdc` and
> `api-design-tbd.mdc`. Those rule files predated the docs being written and
> contradicted this file's own "Both docs are filled" statement — a conflict
> that only became visible once the rules were consolidated here. They have
> been corrected and renamed to `architecture.mdc` / `api-design.mdc`, since
> the `-tbd` suffix was itself part of the stale claim.

### Debugging and troubleshooting — apply on demand, not always

Use when debugging incidents, flaky tests, service failures, or production-like
behavior.

- Reconstruct the lifecycle first: startup, readiness, steady-state, failure,
  shutdown.
- Check whether the bug is caused by timing, state drift, external process
  assumptions, or interface mismatch.
- Preserve debugging breadcrumbs in logs, tests, or comments if the failure mode
  is likely to recur.
- If a fix depends on an operational assumption that is not obvious in code,
  document it close to the logic.

Preferred artifacts: targeted regression tests, improved structured logs, and
comments on invariants, timing assumptions, or failure boundaries.

---

## Verification gates

Scope every command to what you actually touched.

**Go (per touched module — each `components/<name>` is its own Go module):**

```bash
gofmt -s -w <changed-go-files>
cd components/<component> && go vet ./...
cd components/<component> && go test ./...
cd components/<component> && golangci-lint run --new-from-rev=HEAD~1 ./...
```

**Rust (per touched crate — none remain today):**

```bash
cargo fmt --all -- --check
cargo check --all-targets --all-features
cargo clippy --all-targets --all-features -- -D warnings
cargo test --all-targets --all-features
```

**Whole-repository gates (what CI runs):**

```bash
make verify          # verify-go + verify-scripts
make verify-go       # per-component: go vet, golangci-lint run ./..., gofmt check, go test -v -race
make verify-scripts  # shell contracts that ship only on the full-image rail
```

`make verify-go-component-test` runs `go test -v -race` over
`go list ./... | grep -vE "/mocks|/wrapper"` — generated mocks and thin wrappers
are excluded deliberately.

If a gate cannot be run in this environment, say so explicitly and name the
blocker rather than reporting the step as passed or quietly skipping it.

---

## Review contract

Do not commit, push, or open a PR until the reviewer loop reaches
`Verdict: accept`.

1. Prepare a compact handoff with goal, files changed, key decisions, trade-offs,
   and checks run.
2. Invoke the reviewer with fresh context.
3. If review returns `Verdict: revise`, address the findings and rerun checks.
4. Repeat until the reviewer returns `Verdict: accept`.
5. Only then proceed to commit, push, or PR creation.

### Review posture (`prompts/code-review.md`)

Review priority:
1. Repository contract compliance with this file / `AGENTS.md`
2. Safety of service boundaries, daemon lifecycle behavior, and CI protections
3. Go and Rust readability, error handling, and test sufficiency

Required posture:
- Do not review only for local diff correctness.
- Infer the operational goal and review whether the implementation is the right
  shape for that goal.
- Prefer calling out clearer designs when the current change adds complexity to
  an already fragile path.
- Focus on correctness risks, regressions, race conditions, lifecycle bugs, and
  missing enforcement.
- Treat missing tests or missing intent comments as real findings when they
  materially weaken future maintenance.

Tests and docs sufficiency — assess only real gaps:
1. Do we have enough unit tests for introduced logic?
2. Do we have enough integration or service-level validation for cross-boundary
   behavior?
3. Are lint and format checks strong enough for the changed area?
4. Does the change require updates to `CLAUDE.md`/`AGENTS.md`, `.cursor/rules/`,
   component docs, or CI docs?

Output shape — use only sections that have real content:
1. Critical correctness issues
2. Architecture or service-boundary issues
3. CI and guardrail issues
4. Better alternative designs
5. Test gaps
6. Documentation gaps

If there are no meaningful findings, give a brief approval-style summary only.
End with exactly one line: `Verdict: accept` or `Verdict: revise`.

---

## Sub-agent roles

The other tools define three sub-agents (`.cursor/agents/`, `.codex/agents/`,
`.opencode/agents/`). In Claude Code these are roles for the Agent tool — use
them when the user asks for delegation or when fresh context genuinely helps.

**`reviewer`** — read-only, fresh context, runs after implementation.
Reads and applies `prompts/code-review.md`. Focus: correctness,
service-boundary safety, CI protections, test sufficiency, maintenance clarity.
Calls out missing intent comments when they hide future amendment risk. Ends with
exactly one line: `Verdict: accept` or `Verdict: revise`. Does not edit files
unless explicitly asked.

**`planner-researcher`** — read-only, for work that is BOTH large and ambiguous
enough that implementation should pause for design first.
Reads first: this file, `PLANS.md`, the relevant rules above, any touched
component `AGENTS.md`. Summarizes the current service flow, interfaces,
lifecycle, and operational invariants before proposing anything. Surfaces
unknowns instead of guessing. Prefers simplification, deletion, or boundary
cleanup before additive complexity. For CI work, inspects trigger coverage,
permissions, lint strategy, and failure modes. Says so explicitly when
`docs/architecture.md`/`docs/api-design.md` do not cover a case, rather than
settling it silently. Output shape: the six-part plan
listed under the execution-plan contract. Does not implement unless asked.

**`go-rust-maintainer`** — implementation/review specialist for daemon code.
Reads first: this file, the relevant rules above, any touched component
`AGENTS.md`. Priorities: readable and amendable code; preserved startup,
shutdown, and cross-service invariants; explicit error handling and dependency
seams; intent-rich comments for non-obvious logic; lint and test expectations
aligned with CI. Runs the verification gates above for each touched module.

---

## Component contracts

Read the component's own `AGENTS.md` before touching it — each carries
purpose, package-by-package architecture, amendment hazards, verification
commands, and a definition of done that this file does not duplicate.

- `components/feral-controld/AGENTS.md` — connectivity, command orchestration,
  and the full device-setup domain (SoftAP, captive portal, OTA gate, setup UI,
  claiming, factory reset, log upload, LAN hub, offline artwork cache). Flagged in
  its own notes as the highest-risk Go daemon for architectural sprawl.
- `components/feral-sys-monitord/AGENTS.md` — health-signal publisher: metrics,
  connectivity state, system events over D-Bus, Prometheus endpoint. Must not
  grow recovery policy.
- `components/feral-watchdog/AGENTS.md` — recovery-policy daemon: Chromium health
  polling, restart/cleanup/reboot decisions, incident metrics. Must not become a
  telemetry collector.

## Reference docs

- `docs/architecture.md` — architecture direction
- `docs/api-design.md` — API and protocol direction
- `docs/controld-inbound-controller-messages.md` — inbound command/notification
  wire contract
- `docs/offline-artwork-capture.md` — offline cache capture/replay design
- `RELEASES.md` — release ledger (the full-image declarations the release
  guardrail checks)
- `PLANS.md` — execution-plan contract
- `prompts/code-review.md` — shared review contract
