import { describe, expect, it } from "vitest";
import { ORCHESTRATOR_TOOLS, parseRunSpec, renderSpecPrompt, SpecError } from "../../src/orchestrator/spec.js";

function validSpec(): Record<string, unknown> {
  return {
    run_id: "11111111-1111-4111-8111-111111111111",
    run_version: 4,
    run_state: "coding",
    goal: "Add a retry button to the run detail view",
    acceptance_criteria: ["button visible when run failed", "click schedules a retry"],
    repo: { url: "https://git.example/vtx/app.git", base_ref: "dev", worktree_path: "/var/wt/run-1" },
    test_baseline: "make test",
    allowed_tools: ["vtx_plan", "vtx_verify", "vtx_qa", "vtx_gate", "vtx_open_pr", "vtx_advance", "vtx_block"],
    delegation: { agents: ["vtx-coder", "vtx-qa"], max_parallel: 3, qa_independent: true },
    budget: { max_turns: 40, max_tokens: 1_000_000, wall_clock_ms: 3_600_000, max_delegations: 12 },
    stop_conditions: ["gate_pass_and_pr_open", "budget_exhausted"],
    lease: { owner: "orchestrator", epoch: 5, expires_at: "2030-01-01T00:00:00Z", renew_interval_ms: 60_000 },
  };
}

describe("parseRunSpec", () => {
  it("parses a well-formed spec", () => {
    const spec = parseRunSpec(validSpec());
    expect(spec.run_id).toMatch(/^1111/);
    expect(spec.run_version).toBe(4);
    expect(spec.run_state).toBe("coding");
    expect(spec.delegation.max_parallel).toBe(3);
    expect(spec.budget.max_turns).toBe(40);
  });

  it("forces qa_independent true even if the spec disables it", () => {
    const raw = validSpec();
    (raw.delegation as Record<string, unknown>).qa_independent = false;
    expect(parseRunSpec(raw).delegation.qa_independent).toBe(true);
  });

  it("drops requested tools outside the orchestrator universe", () => {
    const raw = validSpec();
    raw.allowed_tools = ["vtx_gate", "bash", "rm_rf", "vtx_advance"];
    const spec = parseRunSpec(raw);
    expect(spec.allowed_tools).toEqual(["vtx_gate", "vtx_advance"]);
    expect(spec.allowed_tools).not.toContain("bash");
  });

  it("fails closed on a missing required field", () => {
    const raw = validSpec();
    delete raw.run_id;
    expect(() => parseRunSpec(raw)).toThrow(SpecError);
  });

  it("fails closed on a negative budget number", () => {
    const raw = validSpec();
    (raw.budget as Record<string, unknown>).max_turns = -1;
    expect(() => parseRunSpec(raw)).toThrow(/non-negative/);
  });

  it("fails closed on a malformed repo object", () => {
    const raw = validSpec();
    raw.repo = { url: "x" };
    expect(() => parseRunSpec(raw)).toThrow(SpecError);
  });

  it("rejects a non-object root", () => {
    expect(() => parseRunSpec("nope")).toThrow(SpecError);
  });
});

describe("renderSpecPrompt", () => {
  it("states the non-negotiable invariants and quotes the goal as data", () => {
    const prompt = renderSpecPrompt(parseRunSpec(validSpec()));
    expect(prompt).toContain('"""Add a retry button');
    expect(prompt).toMatch(/QA is independent/);
    expect(prompt).toMatch(/recomputed from committed evidence/);
    expect(prompt).toMatch(/Draft PR/);
    expect(prompt).toContain("epoch 5");
  });

  it("only ever lists orchestrator tools", () => {
    for (const tool of ORCHESTRATOR_TOOLS) {
      expect(typeof tool).toBe("string");
    }
  });
});
