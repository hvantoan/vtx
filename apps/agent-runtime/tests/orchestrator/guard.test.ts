import { describe, expect, it } from "vitest";
import { assertHeadless, authorizeToolCall, checkBudget, MUTATING_TOOLS, terminatesSession } from "../../src/orchestrator/guard.js";
import type { BudgetLimits, BudgetUsage, LeaseState } from "../../src/orchestrator/guard.js";

const ALLOWED = ["vtx_plan", "vtx_verify", "vtx_qa", "vtx_gate", "vtx_open_pr", "vtx_advance", "vtx_block", "subagent"];

const heldLease = (expiresAt = Number.MAX_SAFE_INTEGER): LeaseState => ({
  held: true,
  owner: "orchestrator",
  epoch: 3,
  expiresAt,
});

const baseInput = {
  allowedTools: ALLOWED,
  lease: heldLease(),
  spineConnected: true,
  now: 1_000,
};

describe("authorizeToolCall", () => {
  it("allows an allowed read tool when the spine is connected", () => {
    expect(authorizeToolCall({ ...baseInput, toolName: "vtx_gate" })).toEqual({ block: false });
  });

  it("blocks a tool outside the run allowlist", () => {
    const d = authorizeToolCall({ ...baseInput, toolName: "bash" });
    expect(d.block).toBe(true);
    expect(d.reason).toMatch(/not in the run's allowed_tools/);
  });

  it("fails closed with terminate when the spine is unreachable", () => {
    const d = authorizeToolCall({ ...baseInput, toolName: "vtx_advance", spineConnected: false });
    expect(d).toEqual({ block: true, reason: "spine unreachable", terminate: true });
  });

  it("blocks a mutating tool with no lease held", () => {
    const d = authorizeToolCall({ ...baseInput, toolName: "vtx_open_pr", lease: { ...heldLease(), held: false } });
    expect(d.block).toBe(true);
    expect(d.terminate).toBe(true);
    expect(d.reason).toMatch(/no lease held/);
  });

  it("blocks a mutating tool when the lease has expired", () => {
    const d = authorizeToolCall({ ...baseInput, toolName: "vtx_advance", lease: heldLease(500), now: 1_000 });
    expect(d.block).toBe(true);
    expect(d.reason).toMatch(/lease expired/);
  });

  it("allows a read tool even when the lease expired (spine still re-fences)", () => {
    const d = authorizeToolCall({ ...baseInput, toolName: "vtx_verify", lease: heldLease(500), now: 1_000 });
    expect(d.block).toBe(false);
  });

  it("treats every mutating tool as lease-gated", () => {
    for (const tool of MUTATING_TOOLS) {
      const d = authorizeToolCall({ ...baseInput, toolName: tool, lease: { ...heldLease(), held: false } });
      expect(d.block, `${tool} should require a lease`).toBe(true);
    }
  });
});

describe("checkBudget", () => {
  const limits: BudgetLimits = { max_turns: 3, max_delegations: 2, wall_clock_ms: 10_000 };
  const usage = (over: Partial<BudgetUsage> = {}): BudgetUsage => ({
    turns: 0,
    delegations: 0,
    startedAt: 0,
    ...over,
  });

  it("is not exhausted within budget", () => {
    expect(checkBudget(usage({ turns: 2 }), limits, 5_000)).toEqual({ exhausted: false });
  });

  it("exhausts on turn budget", () => {
    const v = checkBudget(usage({ turns: 3 }), limits, 1_000);
    expect(v.exhausted).toBe(true);
    expect(v.reason).toMatch(/turn budget/);
  });

  it("exhausts on delegation budget", () => {
    const v = checkBudget(usage({ delegations: 2 }), limits, 1_000);
    expect(v.exhausted).toBe(true);
    expect(v.reason).toMatch(/delegation budget/);
  });

  it("exhausts on wall clock", () => {
    const v = checkBudget(usage({ startedAt: 0 }), limits, 10_000);
    expect(v.exhausted).toBe(true);
    expect(v.reason).toMatch(/wall-clock/);
  });

  it("treats a zero limit as unlimited", () => {
    const v = checkBudget(usage({ turns: 999, delegations: 999 }), { max_turns: 0, max_delegations: 0, wall_clock_ms: 0 }, 0);
    expect(v.exhausted).toBe(false);
  });
});

describe("assertHeadless", () => {
  it("passes when there is no UI", () => {
    expect(() => assertHeadless(false, "json")).not.toThrow();
  });

  it("throws when a UI is present", () => {
    expect(() => assertHeadless(true, "tui")).toThrow(/must run headless/);
  });
});

describe("terminatesSession", () => {
  it("stops the batch on protocol-level failures", () => {
    expect(terminatesSession("unauthenticated")).toBe(true);
    expect(terminatesSession("unknown_tool")).toBe(true);
  });

  it("keeps the session alive on refusals the model can correct", () => {
    for (const code of ["service_error", "tool_unavailable", "invalid_request", "invalid_args"]) {
      expect(terminatesSession(code)).toBe(false);
    }
  });
});
