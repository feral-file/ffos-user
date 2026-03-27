# Agent Notes: `feral-controld`

Scope: `components/feral-controld/**`

Repository-wide principles from the root `AGENTS.md` also apply here.

## Purpose

`feral-controld` is the connectivity and command orchestration daemon.

It is responsible for:
- connecting the device to the relayer when network state allows it
- routing incoming relayer commands into local command handlers
- coordinating CDP, D-Bus, device control, playlist refresh, and state updates
- bridging health and connectivity signals from `feral-sys-monitord`
- exposing or coordinating hub and local device-facing control flows

This component is the highest-risk Go daemon for accidental architectural sprawl. Keep responsibilities explicit and resist adding hidden cross-package coupling.

## Language and style
- Language: Go
- Follow standard Go readability guidance, especially Effective Go and Go Code Review Comments.
- Favor small interfaces owned by the consumer package.
- Prefer explicit orchestration over reflection-heavy or generic abstractions.
- Add comments when control flow or state mutation carries operational knowledge that future edits could break.

## Architecture

### Shape
- `main.go` owns startup, shutdown, wiring, and lifecycle.
- package directories such as `dbus`, `relayer`, `cdp`, `commandrouter`, `status`, `watchdog`, and `mediator` own concrete integration seams.
- the mediator layer is the orchestration hub between incoming signals, relayer messages, and local side effects
- `state` persists durable local state; treat it as a contract, not casual scratch storage
- `wrapper` exists to keep code testable around time, OS, exec, random, IO, and serialization

### Architectural direction
- Keep `main.go` as composition, not business logic.
- Prefer pushing external-system details to focused packages and keeping orchestration legible in mediator and command flows.
- Avoid turning `feral-controld` into a dumping ground for unrelated device policy.
- When adding a new behavior, decide first whether it belongs in:
  - an existing boundary package
  - the mediator as orchestration glue
  - a new focused package
  - or a different daemon entirely

### Amendment hazards
- Connectivity, relayer readiness, and D-Bus events interact. Do not change one of those flows without checking the others.
- State writes, relayer reconnection, and CDP updates should stay understandable in logs and comments.
- If a new path changes command routing or topic/state persistence, document the invariant close to the code.

## Verification for touched work
- Format changed Go files with `gofmt -s -w <changed-go-files>`.
- Run `go test ./...` in `components/feral-controld`.
- Run `go vet ./...` in `components/feral-controld`.
- Run changed-diff linting with `golangci-lint run --new-from-rev=HEAD~1 ./...` in `components/feral-controld`.

## Definition of done
A task in this component is done only when:
1. touched command, mediator, state, or integration paths still have clear ownership
2. tests and vet pass for this module, or blockers are documented
3. comments capture any non-obvious invariants, retries, state transitions, or trade-offs
4. startup and shutdown behavior remain coherent
5. any affected agent docs stay accurate

## Review flow
1. Prepare a short handoff covering the user-visible or system-visible behavior change, files changed, and checks run.
2. Call out any orchestration trade-offs, especially around connectivity, relayer, D-Bus, or persistence.
3. Run the reviewer loop using `prompts/code-review.md`.
4. Only commit or ship after the review loop returns `Verdict: accept`.
