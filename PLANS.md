# PLANS.md - Execution Plan Contract

Use this document when the task is large enough, risky enough, or vague enough that implementation should not begin immediately.

## Use an execution plan when
- the request changes behavior across multiple services
- the work affects CI, release safety, or shared contracts
- multiple designs are plausible
- the current architecture is unclear or under-documented

## Do not use an execution plan when
- the change is a narrow fix with obvious scope
- the user already supplied a concrete step-by-step plan
- the work is a small documentation or tooling edit

## Required output
1. Current-state summary
2. Constraints and invariants
3. Risks and unknowns
4. Viable design branches with trade-offs
5. Test and verification plan first
6. Recommended staged rollout

## Repository-specific planning rules
- Prefer deleting or simplifying complex paths before adding new abstractions.
- Treat system boundaries as first-class: D-Bus contracts, systemd behavior, local files, network boundaries, and device lifecycle constraints.
- If architecture or API guidance is needed, read `docs/architecture.md` and `docs/api-design.md` first. If those docs don't cover the case, call it out explicitly rather than inventing permanent rules silently.
- For CI changes, include failure modes such as jobs not triggering, overly broad triggers, flaky coverage tools, or silent lint regressions.
