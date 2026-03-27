# AGENTS.md - ffos-user Agent Contract

This file defines repository-level constraints for coding agents. Detailed implementation guidance lives in `.cursor/rules/`, with service-specific notes allowed in component-local `AGENTS.md` files.

## Repository overview
- Project: `ffos-user`, a Linux device-services repository for Feral File OS user-space components.
- Primary languages: Go and Rust.
- Runtime posture: long-running user services, systemd-managed processes, D-Bus integration, local device orchestration, and constrained-device operations.
- Service shape: prefer explicit daemons and narrow responsibilities over shared mutable frameworks.

## Architecture and API design status
- Architecture rule source: `docs/architecture-tbd.md`
- API design rule source: `docs/api-design-tbd.md`
- Those documents are intentionally placeholders. Repo owners should fill them in before major cross-service refactors or public contract expansions.

## Non-negotiables
- Prefer replacing or deleting flawed code paths over preserving brittle compatibility behavior.
- Keep service boundaries explicit. Cross-service coordination must happen through well-defined interfaces such as D-Bus, files, sockets, or process boundaries.
- Prefer stateless, testable helpers by default. Introduce stateful coordinators only when lifecycle, caching, or orchestration clearly requires them.
- Keep code easy to amend in later agentic sessions. Favor clarity, stable naming, small seams, and explicit invariants over clever compression.
- For non-obvious logic, add intent-rich comments that preserve future maintenance context.
- Those comments should explain why the code exists, what constraints or invariants it must preserve, important failure modes, trade-offs, and any rejected alternatives that future edits could accidentally reintroduce.
- Comments may be longer than usual when they capture design constraints, operational assumptions, or amendment hazards. Do not add comments that merely restate syntax.

## Required workflow for substantial work
Before implementing a major feature, refactor, daemon behavior change, or CI policy change:
1. Read this file.
2. Read `PLANS.md` when the work is large, vague, or architectural.
3. Read the relevant rule files in `.cursor/rules/`.
4. Read any component-local `AGENTS.md` files for touched services.
5. Summarize the current flow, constraints, and operational risk before changing behavior.

Canonical sequence:
`context -> design -> tasks -> implementation -> verification -> review`

## Language-specific expectations
- Go work must follow standard Go readability guidance, especially Effective Go and Go Code Review Comments, while staying practical for service code.
- Rust work must follow standard Rust readability guidance, especially ownership clarity, explicit error propagation, and Clippy-clean code.
- New code should prefer dependency injection, narrow interfaces, explicit error handling, and tests that cover behavior rather than implementation trivia.

## Verification gates
- Verification gates apply only to the files and modules or crates touched by the change.
- Go changes:
  - format only the changed Go files: `gofmt -s -w <changed-go-files>`
  - run `go test ./...` and `go vet ./...` from each touched Go module
  - lint only the changed Go diff: `golangci-lint run --new-from-rev=HEAD~1 ./...`
- Rust changes:
  - format only the changed Rust files: `cargo fmt --all -- --check`
  - run `cargo check --all-targets --all-features`, `cargo clippy --all-targets --all-features -- -D warnings`, and `cargo test --all-targets --all-features` only in each touched crate
- CI and workflow changes:
  - validate the affected workflow logic
  - preserve branch/path protections
  - ensure lint and test jobs still trigger on source and config changes

## Review workflow
After implementation, run a review loop until the reviewer qualifies the change.
1. Prepare a compact handoff with goal, files changed, decisions, trade-offs, and checks run.
2. Invoke the reviewer sub-agent for a fresh-context review.
3. If review says `Verdict: revise`, address findings, rerun checks, and review again.
4. Only commit or open a PR after review qualifies the change.

Shared review prompt:
- `prompts/code-review.md`

## Agent assets
- Cursor rules: `.cursor/rules/`
- Cursor sub-agents: `.cursor/agents/`
- Codex sub-agents: `.codex/agents/`
- OpenCode sub-agents: `.opencode/agents/`

## Definition of done
A task is complete only when:
1. Relevant lint, format, and test checks pass, or blockers are explicitly documented.
2. Comments preserve important design intent for future amendment when the logic is non-obvious.
3. CI still protects the touched code paths.
4. Any touched service docs or agent docs remain accurate.
