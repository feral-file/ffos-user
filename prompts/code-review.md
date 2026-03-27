### Review priority
1. Repository contract compliance with `AGENTS.md`
2. Safety of service boundaries, daemon lifecycle behavior, and CI protections
3. Go and Rust readability, error handling, and test sufficiency

### Required review posture
- Do not review only for local diff correctness.
- Infer the operational goal and review whether the implementation is the right shape for that goal.
- Prefer calling out clearer designs when the current change adds complexity to an already fragile path.
- Focus on correctness risks, regressions, race conditions, lifecycle bugs, and missing enforcement.
- Treat missing tests or missing intent comments as real findings when they materially weaken future maintenance.

### Tests and docs sufficiency review
Assess only real gaps:
1. Do we have enough unit tests for introduced logic?
2. Do we have enough integration or service-level validation for cross-boundary behavior?
3. Are lint and format checks strong enough for the changed area?
4. Does the change require updates to `AGENTS.md`, `.cursor/rules/`, component docs, or CI docs?

### Preferred output shape
Use only sections that have real content:
1. Critical correctness issues
2. Architecture or service-boundary issues
3. CI and guardrail issues
4. Better alternative designs
5. Test gaps
6. Documentation gaps

If there are no meaningful findings, give a brief approval-style summary only.

### Verdict
End with exactly one line:
- `Verdict: accept`
- `Verdict: revise`
