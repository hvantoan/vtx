/**
 * Delegation rails for Pi's native `subagent` tool (design doc §3.1, OD-ORCH-02).
 * Pure and side-effect free so the rails are unit-testable without Pi or a
 * subprocess.
 *
 * Pi registers `subagent` with `model-only` exposure, and `ctx.executeTool()`
 * only reaches `codemode`/`deferred`/active-`direct` tools — so a wrapper tool
 * cannot invoke it. The model calls `subagent` directly and the orchestrator's
 * `tool_call` gate rewrites its arguments in place (the design's documented
 * mutation path). The specialist still runs in an isolated child process with
 * its own context window and session, which is the basis of QA independence.
 */

import type { RunSpec } from "./spec.js";

/** Name of Pi's native delegation tool (pi-subagents). */
export const SUBAGENT_TOOL = "subagent";

/**
 * Specialist agents live in the repo under `.pi/agents/`, so discovery must
 * include project-local agents. Headless has no UI to confirm them; the repo is
 * the trust boundary (design §7), and the launcher passes `--approve`.
 */
export const DELEGATE_AGENT_SCOPE = "both";

/**
 * Subagent keys the model may supply. `cwd` is accepted only so a model-supplied
 * value can be overridden rather than silently ignored; the rest of Pi's
 * subagent surface (workflow, tasks, chain, action, async, model, machine,
 * worktree, output, options) would escape the run's rails and is refused.
 */
const ALLOWED_INPUT_KEYS = new Set(["agent", "task", "cwd"]);

export interface SubagentArgs {
  agent: string;
  task: string;
  cwd: string;
  agentScope: typeof DELEGATE_AGENT_SCOPE;
  async: false;
}

export type DelegatePlan = { ok: true; args: SubagentArgs } | { ok: false; reason: string };

/**
 * Validate a raw `subagent` call against the run spec and produce the exact
 * arguments to execute. Fails closed: no spec, an agent outside the allowlist,
 * an empty task, or any unsupported option all refuse. The model can never
 * choose the cwd, the agent scope, or background execution.
 */
export function sanitizeDelegation(input: unknown, spec: RunSpec | undefined): DelegatePlan {
  if (!spec) return { ok: false, reason: "no active run spec" };
  if (typeof input !== "object" || input === null || Array.isArray(input)) {
    return { ok: false, reason: "subagent arguments must be an object" };
  }

  const record = input as Record<string, unknown>;
  const unsupported = Object.keys(record).filter((key) => !ALLOWED_INPUT_KEYS.has(key));
  if (unsupported.length > 0) {
    return {
      ok: false,
      reason: `subagent option(s) not authorized for this run: ${unsupported.join(", ")} (only agent, task, cwd are accepted)`,
    };
  }

  const agent = typeof record.agent === "string" ? record.agent.trim() : "";
  if (agent.length === 0) return { ok: false, reason: "subagent agent is required" };
  if (!spec.delegation.agents.includes(agent)) {
    const allowed = spec.delegation.agents.join(", ") || "none";
    return { ok: false, reason: `agent "${agent}" is not in the spec delegation allowlist (${allowed})` };
  }

  const task = typeof record.task === "string" ? record.task.trim() : "";
  if (task.length === 0) return { ok: false, reason: "delegation task is empty" };

  return {
    ok: true,
    args: {
      agent,
      task,
      // Pinned to the run worktree; a model-supplied cwd is never honored.
      cwd: spec.repo.worktree_path,
      agentScope: DELEGATE_AGENT_SCOPE,
      // Force a foreground child: the orchestrator needs the result (and the
      // committed SHA) before it can advance the spine.
      async: false,
    },
  };
}
