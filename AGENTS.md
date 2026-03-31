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
- Repo owner should fill these in so future agents know which architectural direction to aim for.

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
