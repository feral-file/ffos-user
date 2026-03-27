# Agent Notes: `feral-watchdog`

Scope: `components/feral-watchdog/**`

Repository-wide principles from the root `AGENTS.md` also apply here.

## Purpose

`feral-watchdog` is the recovery-policy daemon for device health failures.

It is responsible for:
- monitoring Chromium health via CDP
- consuming system metrics and events from `feral-sys-monitord`
- deciding when to restart kiosk services, clean disk pressure, or reboot
- feeding incident metrics to vmagent when configured
- keeping recovery behavior explicit, bounded, and observable

This daemon owns recovery policy. It should not become the source of raw health telemetry collection, which belongs in `feral-sys-monitord`.

## Language and style
- Language: Go
- Follow standard Go readability guidance.
- Prefer explicit policy checks over deeply nested recovery heuristics.
- Add comments for thresholds, cooldowns, escalation logic, and operational trade-offs.
- Any `nolint` or panic-retained invariant should be deliberate and easy to justify.

## Architecture

### Shape
- `main.go` wires config, DBus, vmagent, CDP, handlers, mediator, and background monitors.
- resource-specific handlers such as memory, disk, GPU, and CPU encapsulate threshold logic
- `Mediator` consumes D-Bus signals and fans them into the appropriate handlers
- systemd and Chromium monitors are long-running goroutines with explicit shutdown semantics
- vmagent integration is a reporting side effect, not the policy source

### Architectural direction
- Keep policy logic close to the relevant handler instead of scattering it through goroutines.
- Distinguish clearly between:
  - observation
  - decision
  - action
  - incident reporting
- If a threshold or escalation path changes, document the reason and the recovery trade-off.

### Amendment hazards
- Recovery thresholds, cooldowns, and reboot paths can easily become surprising if changed without comments.
- Changes to D-Bus event handling must stay aligned with the contracts emitted by `feral-sys-monitord`.
- Long-running monitors must keep clean cancellation and shutdown behavior.

## Verification for touched work
- Format changed Go files with `gofmt -s -w <changed-go-files>`.
- Run `go test ./...` in `components/feral-watchdog`.
- Run `go vet ./...` in `components/feral-watchdog`.
- Run changed-diff linting with `golangci-lint run --new-from-rev=HEAD~1 ./...` in `components/feral-watchdog`.

## Definition of done
A task in this component is done only when:
1. recovery policy remains explicit and understandable
2. tests and vet pass for this module, or blockers are documented
3. comments preserve the why behind thresholds, cooldowns, or escalation rules
4. shutdown behavior for background monitors remains correct
5. the README or agent docs stay accurate when behavior changes

## Review flow
1. Prepare a handoff that states which recovery policy changed and what system behavior it affects.
2. Call out threshold changes, reboot or restart semantics, and reporting side effects.
3. Run the reviewer loop using `prompts/code-review.md`.
4. Only commit or ship after the review loop returns `Verdict: accept`.
