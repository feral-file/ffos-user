// OpenCode plugin enforcing AGENTS.md "Release guardrail: ISO image builds".
//
// OpenCode has no shell-hook config file; plugins under .opencode/plugins/
// are loaded at startup and may veto a tool call from `tool.execute.before`
// by throwing. This one hands every bash command to the shared guard script
// (scripts/agent-iso-build-guard.sh, the same script the Claude Code, Codex,
// Cursor, and Gemini hooks run) and throws with the guard's reason when it
// decides deny. OpenCode has no "ask" primitive, so a staging dispatch is
// denied too, with the instruction that the human must dispatch it.
import { spawnSync } from "node:child_process"
import { join } from "node:path"

// Coarse mirror of the guard's own dispatch detection, used ONLY when the
// guard itself cannot run. A broken guard must not silently let an ISO
// dispatch through, but it also must not brick every other shell command.
const LOOKS_LIKE_DISPATCH = /workflow\s+run|\/actions\/workflows\/\S+\/dispatches|gh\s+run\s+(rerun|retry)/i

export const IsoBuildGuard = async ({ directory, worktree }) => {
  const root = worktree || directory
  const guard = join(root, "scripts", "agent-iso-build-guard.sh")

  return {
    "tool.execute.before": async (input, output) => {
      if (input.tool !== "bash") return
      const command = output?.args?.command
      if (typeof command !== "string" || command.length === 0) return

      const payload = JSON.stringify({
        hook_event_name: "PreToolUse",
        tool_name: "Bash",
        cwd: directory,
        tool_input: { command },
      })
      const result = spawnSync(guard, ["--format", "opencode"], {
        input: payload,
        cwd: root,
        encoding: "utf8",
        timeout: 15000,
      })

      if (result.error || result.status !== 0) {
        if (LOOKS_LIKE_DISPATCH.test(command)) {
          const why = result.error ? result.error.message : `exit ${result.status}`
          throw new Error(
            `agent-iso-build-guard could not run (${why}); refusing to run a possible ISO build dispatch unchecked. ` +
              "See AGENTS.md: Release guardrail: ISO image builds.",
          )
        }
        return
      }

      const out = (result.stdout || "").trim()
      if (!out) return
      let decision
      try {
        decision = JSON.parse(out)
      } catch {
        throw new Error(`agent-iso-build-guard returned unparseable output: ${out}`)
      }
      if (decision.decision === "deny") {
        throw new Error(decision.reason || "Blocked by AGENTS.md: Release guardrail: ISO image builds.")
      }
    },
  }
}
