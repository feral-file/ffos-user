# GEMINI.md

Read `AGENTS.md` first; it is the repository contract for every coding agent,
Gemini CLI included. Its "Release guardrail: ISO image builds" section is
mandatory: never dispatch any `ffos` publishing workflow (the image builds,
`manual-build-components.yaml`, `manual-build-feral-player.yaml`,
`manual-push-pacman-repo.yaml`) on the `release` branch or with
`environment=Production`, and never dispatch a `staging` build yourself
(`.gemini/settings.json` wires a `BeforeTool` hook that blocks both; the
human dispatches staging after you have shown them the exact parameters).
