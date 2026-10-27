import { describe, expect, it } from "vitest";
import {
  DELEGATE_AGENT_SCOPE,
  sanitizeDelegation,
  SUBAGENT_TOOL,
} from "../../src/orchestrator/delegate.js";
import { parseRunSpec } from "../../src/orchestrator/spec.js";
import type { RunSpec } from "../../src/orchestrator/spec.js";

function spec(overrides?: Partial<RunSpec>): RunSpec {
  const base = parseRunSpec({
    run_id: "11111111-1111-4111-8111-111111111111",
    run_version: 4,
    run_state: "coding",
    goal: "Add a retry button",
    acceptance_criteria: ["button visible when run failed"],
    repo: { url: "https://git.example/vtx/app.git", base_ref: "dev", worktree_path: "/var/wt/run-1" },
    test_baseline: "make test",
    allowed_tools: ["vtx_plan", "vtx_verify"],
    delegation: { agents: ["vtx-coder", "vtx-qa"], max_parallel: 3, qa_independent: true },
    budget: { max_turns: 40, max_tokens: 1_000_000, wall_clock_ms: 3_600_000, max_delegations: 12 },
    stop_conditions: ["gate_pass_and_pr_open"],
    lease: { owner: "orchestrator", epoch: 5, expires_at: "2030-01-01T00:00:00Z", renew_interval_ms: 60_000 },
  });
  return { ...base, ...overrides };
}

describe("sanitizeDelegation", () => {
  it("refuses when there is no active run spec", () => {
    const plan = sanitizeDelegation({ agent: "vtx-coder", task: "do it" }, undefined);
    expect(plan.ok).toBe(false);
    if (!plan.ok) expect(plan.reason).toMatch(/no active run spec/);
  });

  it("refuses a non-object input", () => {
    const plan = sanitizeDelegation("vtx-coder", spec());
    expect(plan.ok).toBe(false);
    if (!plan.ok) expect(plan.reason).toMatch(/must be an object/);
  });

  it("refuses an agent outside the delegation allowlist", () => {
    const plan = sanitizeDelegation({ agent: "vtx-reviewer", task: "review it" }, spec());
    expect(plan.ok).toBe(false);
    if (!plan.ok) expect(plan.reason).toMatch(/not in the spec delegation allowlist/);
  });

  it("refuses a missing or empty agent", () => {
    const missing = sanitizeDelegation({ task: "do it" }, spec());
    expect(missing.ok).toBe(false);
    if (!missing.ok) expect(missing.reason).toMatch(/agent is required/);
  });

  it("refuses an empty task", () => {
    const plan = sanitizeDelegation({ agent: "vtx-coder", task: "   " }, spec());
    expect(plan.ok).toBe(false);
    if (!plan.ok) expect(plan.reason).toMatch(/task is empty/);
  });

  it("refuses options that escape the run rails", () => {
    for (const extra of ["workflow", "tasks", "chain", "action", "async", "model", "machine", "worktree", "output", "options"]) {
      const plan = sanitizeDelegation({ agent: "vtx-coder", task: "do it", [extra]: true }, spec());
      expect(plan.ok, `${extra} should be refused`).toBe(false);
      if (!plan.ok) expect(plan.reason).toMatch(/not authorized for this run/);
    }
  });

  it("pins cwd, scope, and foreground execution", () => {
    const plan = sanitizeDelegation({ agent: "vtx-coder", task: "implement the spec", cwd: "/tmp/elsewhere" }, spec());
    expect(plan.ok).toBe(true);
    if (!plan.ok) return;
    expect(plan.args).toEqual({
      agent: "vtx-coder",
      task: "implement the spec",
      cwd: "/var/wt/run-1",
      agentScope: DELEGATE_AGENT_SCOPE,
      async: false,
    });
  });

  it("trims the agent name and task", () => {
    const plan = sanitizeDelegation({ agent: "  vtx-qa  ", task: "  verify SHA  " }, spec());
    expect(plan.ok).toBe(true);
    if (!plan.ok) return;
    expect(plan.args.agent).toBe("vtx-qa");
    expect(plan.args.task).toBe("verify SHA");
  });

  it("targets Pi's native subagent tool with the project-inclusive scope", () => {
    expect(SUBAGENT_TOOL).toBe("subagent");
    expect(DELEGATE_AGENT_SCOPE).toBe("both");
  });
});
