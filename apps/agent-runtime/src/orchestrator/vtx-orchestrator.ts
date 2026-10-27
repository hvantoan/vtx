/**
 * vtx-orchestrator — Pi extension that turns a Pi session into the VTX
 * orchestrator agent (design doc: docs/design/vtx-orchestrator-extension.md,
 * Phương án A1, Phase 2 skeleton).
 *
 * The orchestrator plans from a clear spec and delegates to specialist agents.
 * It holds NO database or GitHub credentials: every authoritative action
 * (transition, gate, lease, PR) is an RPC to the Go spine over a unix socket,
 * which recomputes decisions from committed evidence. All safety logic lives in
 * the pure, unit-tested modules under ./ (guard, spec, redact, spine-client);
 * this file is thin glue that maps Pi events onto them and fails closed.
 *
 * Headless: VTX runs `pi --mode json`, so `ctx.hasUI === false` and there is no
 * `ctx.ui.confirm`. Nothing here prompts a human; authorization is programmatic.
 *
 * Loaded by Pi via jiti (transpile-only). Long-lived resources (socket,
 * heartbeat) start in `session_start`, never in the factory, because Pi may load
 * an extension without starting a session.
 */

import { readFile } from "node:fs/promises";
import { defineTool } from "@earendil-works/pi-coding-agent";
import type {
  AgentToolResult,
  ExtensionAPI,
  ToolCallEventResult,
  ToolResultEventResult,
} from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

import { SpineClient } from "./spine-client.js";
import { SpineError, SpineTool } from "./protocol.js";
import { parseRunSpec, renderSpecPrompt } from "./spec.js";
import type { RunSpec } from "./spec.js";
import { sanitizeDelegation, SUBAGENT_TOOL } from "./delegate.js";
import { assertHeadless, authorizeToolCall, checkBudget, terminatesSession } from "./guard.js";
import type { BudgetLimits, BudgetUsage, LeaseState } from "./guard.js";
import { redactText, redactValue } from "./redact.js";

/** Env names mirror the Go worker config (internal/config/config.go). */
const ENV_SOCKET = "SPINE_SOCKET_PATH";
const ENV_TOKEN = "SPINE_AUTH_TOKEN";
const ENV_SPEC = "VTX_RUN_SPEC_PATH";

/** Secret env values masked out of any evidence before it is persisted. */
const SECRET_ENV_NAMES = [
  ENV_TOKEN,
  "GITHUB_TOKEN",
  "JIRA_API_TOKEN",
  "GATEWAY_INTERNAL_AUTH_TOKEN",
];

const NO_ARGS = Type.Object({});

/**
 * Verification runs the whole suite synchronously in the spine (~1 minute on
 * this repo). The 30s RPC default would abandon it while the suite kept
 * running, and each retry would queue another one behind the heartbeat.
 */
const VERIFY_TIMEOUT_MS = 10 * 60_000;

function textResult(text: string, structured?: unknown): AgentToolResult<Record<string, unknown>> {
  const result: AgentToolResult<Record<string, unknown>> = {
    content: [{ type: "text", text }],
    details: { structured: structured ?? null },
  };
  if (structured !== undefined) {
    result.structuredContent = structured as AgentToolResult["structuredContent"];
  }
  return result;
}

/**
 * A refused call is a message, not a dead session: only protocol-level codes
 * (bad token, unknown tool) stop the batch. See {@link terminatesSession}.
 */
function errorResult(tool: string, err: unknown): AgentToolResult<Record<string, unknown>> {
  const message = err instanceof Error ? err.message : String(err);
  const code = err instanceof SpineError ? err.code : "service_error";
  return {
    content: [{ type: "text", text: `${tool} failed (${code}): ${message}` }],
    details: { tool, code, message },
    isError: true,
    ...(terminatesSession(code) ? { terminate: true } : {}),
  };
}

export default function vtxOrchestrator(pi: ExtensionAPI): void {
  // --- per-session mutable state (reset on session_start) --------------------
  let client: SpineClient | undefined;
  let spec: RunSpec | undefined;
  let lease: LeaseState = { held: false, owner: "", epoch: 0, expiresAt: 0 };
  // CAS baseline from the launched spec, advanced from every spine snapshot.
  // The model never supplies these: it cannot know the current version.
  let runVersion = 0;
  let runState = "";
  const budget: BudgetUsage = { turns: 0, delegations: 0, startedAt: 0 };
  let heartbeat: NodeJS.Timeout | undefined;
  let toolsRegistered = false;

  const secretValues = (): string[] =>
    SECRET_ENV_NAMES.map((n) => process.env[n]).filter((v): v is string => typeof v === "string" && v.length > 0);

  const limits = (): BudgetLimits => ({
    max_turns: spec?.budget.max_turns ?? 0,
    max_delegations: spec?.budget.max_delegations ?? 0,
    wall_clock_ms: spec?.budget.wall_clock_ms ?? 0,
  });

  /** Track the run snapshot the spine returns, so the next CAS uses its version. */
  function syncRun(out: unknown): void {
    if (typeof out !== "object" || out === null) return;
    const snap = out as { version?: unknown; state?: unknown };
    if (typeof snap.version === "number") runVersion = snap.version;
    if (typeof snap.state === "string" && snap.state !== runState) {
      runState = snap.state;
      pi.appendEntry("vtx_run_state", { run_id: spec?.run_id, state: runState, version: runVersion });
    }
  }

  /** Single funnel to the spine; fails closed when unconfigured or unreachable. */
  async function spine(tool: string, args?: unknown): Promise<unknown> {
    if (!client || !spec) throw new SpineError("service_error", "spine not initialized", tool);
    const out = await client.call(tool, { runId: spec.run_id, leaseEpoch: lease.epoch, args });
    syncRun(out);
    return out;
  }

  function startHeartbeat(): void {
    stopHeartbeat();
    const intervalMs = spec?.lease.renew_interval_ms ?? 0;
    if (!client || intervalMs <= 0) return;
    heartbeat = setInterval(() => {
      void (async () => {
        try {
          // A dropped socket must not end the run: the spine accepts a fresh
          // connection for as long as the lease epoch is still valid.
          if (client && !client.isConnected) await client.connect();
          const res = await spine(SpineTool.RenewLease);
          const expires = (res as { lease_expires_at?: string } | undefined)?.lease_expires_at;
          if (typeof expires === "string") {
            const ms = Date.parse(expires);
            if (Number.isFinite(ms)) lease = { ...lease, held: true, expiresAt: ms };
          }
        } catch (err) {
          // Lease loss fences side effects: drop the lease so the gate blocks
          // every mutating tool until the spine re-grants it (design §8).
          lease = { ...lease, held: false };
          const message = err instanceof Error ? err.message : String(err);
          pi.appendEntry("vtx_lease_lost", { run_id: spec?.run_id, error: message });
        }
      })();
    }, intervalMs);
    heartbeat.unref?.();
  }

  function stopHeartbeat(): void {
    if (heartbeat) {
      clearInterval(heartbeat);
      heartbeat = undefined;
    }
  }

  // --- tool definitions ------------------------------------------------------
  // Each is a thin RPC; the service decides, not the agent. gate/open_pr take no
  // verdict argument (FR-QA-002). `defineTool` preserves TypeBox param inference.

  const tools = [
    defineTool({
      name: "vtx_plan",
      label: "VTX Plan",
      description:
        "Persist the task plan (DAG) for this run. Local, durable orchestration state; does not mutate run state in the spine.",
      parameters: Type.Object({
        tasks: Type.Array(
          Type.Object({
            id: Type.String(),
            title: Type.String(),
            depends_on: Type.Optional(Type.Array(Type.String())),
          }),
        ),
      }),
      async execute(_id, params) {
        pi.appendEntry("vtx_plan", { run_id: spec?.run_id, tasks: params.tasks });
        return textResult(`recorded plan with ${params.tasks.length} task(s)`, { tasks: params.tasks });
      },
    }),
    defineTool({
      name: "vtx_verify",
      label: "VTX Verify",
      description: "Run verification for the current commit and record evidence in the spine.",
      parameters: Type.Object({
        task_id: Type.Optional(Type.String()),
        attempt: Type.Optional(Type.Number()),
      }),
      annotations: { readOnlyHint: true },
      async execute(_id, params) {
        try {
          const out = await spine(SpineTool.Verify, params);
          return textResult(JSON.stringify(out), out);
        } catch (err) {
          return errorResult("vtx_verify", err);
        }
      },
    }),
    defineTool({
      name: "vtx_qa",
      label: "VTX QA",
      description:
        "Launch an independent QA session for the exact commit SHA. The QA specialist submits the verdict through its own spine access; the orchestrator cannot author it.",
      parameters: Type.Object({ expected_version: Type.Optional(Type.Number()) }),
      annotations: { readOnlyHint: true },
      async execute(_id, params) {
        try {
          const out = await spine(SpineTool.QALaunch, params);
          return textResult(JSON.stringify(out), out);
        } catch (err) {
          return errorResult("vtx_qa", err);
        }
      },
    }),
    defineTool({
      name: "vtx_gate",
      label: "VTX Gate",
      description:
        "Evaluate the quality gate. Takes NO verdict argument: the spine recomputes PASS/FAIL from committed evidence (FR-QA-002).",
      parameters: NO_ARGS,
      annotations: { readOnlyHint: true },
      async execute() {
        try {
          const out = await spine(SpineTool.Gate);
          return textResult(JSON.stringify(out), out);
        } catch (err) {
          return errorResult("vtx_gate", err);
        }
      },
    }),
    defineTool({
      name: "vtx_open_pr",
      label: "VTX Open PR",
      description:
        "Open a Draft PR. Refuses unless the gate already returned PASS for this exact run+SHA; never merges or deploys.",
      parameters: Type.Object({
        title: Type.String(),
        body: Type.String(),
      }),
      annotations: { destructiveHint: true },
      async execute(_id, params) {
        try {
          const out = await spine(SpineTool.OpenPR, { ...params, expected_version: runVersion });
          return textResult(JSON.stringify(out), out);
        } catch (err) {
          return errorResult("vtx_open_pr", err);
        }
      },
    }),
    defineTool({
      name: "vtx_advance",
      label: "VTX Advance",
      description:
        "Advance the run to the next state (version CAS + lease-epoch fenced by the spine). The extension supplies the tracked run version; do not guess one.",
      parameters: Type.Object({
        reason: Type.Optional(Type.String()),
      }),
      async execute(_id, params) {
        try {
          const out = await spine(SpineTool.Advance, { ...params, expected_version: runVersion });
          return textResult(JSON.stringify(out), out);
        } catch (err) {
          return errorResult("vtx_advance", err);
        }
      },
    }),
    defineTool({
      name: "vtx_block",
      label: "VTX Block",
      description: "Block the run with a reason (spine records it as the policy actor).",
      parameters: Type.Object({
        reason: Type.String(),
      }),
      async execute(_id, params) {
        try {
          const out = await spine(SpineTool.Block, { ...params, expected_version: runVersion });
          return textResult(JSON.stringify(out), out);
        } catch (err) {
          return errorResult("vtx_block", err);
        }
      },
    }),
    defineTool({
      name: "vtx_escalate",
      label: "VTX Escalate",
      description: "Escalate to a human operator: blocks the run and records a correlation id for the operator queue.",
      parameters: Type.Object({
        reason: Type.String(),
        correlation_id: Type.Optional(Type.String()),
      }),
      async execute(_id, params) {
        pi.appendEntry("vtx_escalate", { run_id: spec?.run_id, reason: params.reason, correlation_id: params.correlation_id });
        try {
          const out = await spine(SpineTool.Block, { expected_version: runVersion, reason: `escalated: ${params.reason}` });
          return textResult(JSON.stringify(out), out);
        } catch (err) {
          return errorResult("vtx_escalate", err);
        }
      },
    }),
  ];

  function registerTools(): void {
    if (toolsRegistered) return;
    toolsRegistered = true;
    for (const tool of tools) pi.registerTool(tool);
  }

  // --- lifecycle hooks -------------------------------------------------------

  pi.on("session_start", async (_event, ctx) => {
    assertHeadless(ctx.hasUI, ctx.mode);

    const socketPath = process.env[ENV_SOCKET];
    const token = process.env[ENV_TOKEN];
    const specPath = process.env[ENV_SPEC];
    if (!socketPath || !token || !specPath) {
      // Fail closed: without a spine + spec the orchestrator cannot operate.
      throw new Error(`missing orchestrator env: ${ENV_SOCKET}, ${ENV_TOKEN}, ${ENV_SPEC} are all required`);
    }

    const raw = await readFile(specPath, "utf8");
    spec = parseRunSpec(JSON.parse(raw));
    lease = {
      held: true,
      owner: spec.lease.owner,
      epoch: spec.lease.epoch,
      expiresAt: Date.parse(spec.lease.expires_at),
    };
    runVersion = spec.run_version;
    runState = spec.run_state;
    budget.turns = 0;
    budget.delegations = 0;
    budget.startedAt = Date.now();

    client = new SpineClient({
      socketPath,
      token,
      toolTimeouts: { [SpineTool.Verify]: VERIFY_TIMEOUT_MS },
    });
    await client.connect(); // rejects → session_start fails → no run proceeds

    registerTools();
    startHeartbeat();
    pi.appendEntry("vtx_session_start", { run_id: spec.run_id, lease_epoch: lease.epoch });
  });

  // `subagent` is model-only in Pi: `ctx.executeTool()` cannot reach it, so the
  // model calls it directly and the tool_call gate below authorizes and rewrites
  // the call (allowlist, budget, lease, pinned cwd) before it executes.
  const effectiveTools = (): string[] => (spec ? [...spec.allowed_tools, SUBAGENT_TOOL] : [SUBAGENT_TOOL]);

  pi.on("before_agent_start", (event) => {
    if (!spec) return undefined;
    // Restrict the model's toolset to the spec allowlist (already intersected with
    // the orchestrator tool universe by parseRunSpec), plus the native delegation
    // tool, and append the spec as inert data to the prompt.
    pi.setActiveTools(effectiveTools());
    return { systemPrompt: `${event.systemPrompt}\n\n${renderSpecPrompt(spec)}` };
  });

  /**
   * Gate for Pi's native `subagent` call. The model supplies only agent/task;
   * everything else (cwd, agent scope, foreground execution) is pinned here.
   */
  function gateDelegation(event: { input: Record<string, unknown>; parentToolCallId?: string }): ToolCallEventResult | undefined {
    if (event.parentToolCallId) {
      return { block: true, reason: "nested subagent calls are not authorized" };
    }
    const decision = authorizeToolCall({
      toolName: SUBAGENT_TOOL,
      allowedTools: effectiveTools(),
      lease,
      spineConnected: client?.isConnected ?? false,
      now: Date.now(),
    });
    if (decision.block) {
      return { block: true, reason: decision.reason, terminate: decision.terminate };
    }
    const lim = limits().max_delegations;
    if (lim > 0 && budget.delegations >= lim) {
      return { block: true, reason: `delegation budget exhausted (${budget.delegations}/${lim})` };
    }
    const plan = sanitizeDelegation(event.input, spec);
    if (!plan.ok) return { block: true, reason: plan.reason };

    for (const key of Object.keys(event.input)) delete event.input[key];
    Object.assign(event.input, plan.args);
    budget.delegations += 1;
    pi.appendEntry("vtx_delegation", {
      run_id: spec?.run_id,
      agent: plan.args.agent,
      delegations: budget.delegations,
    });
    return undefined;
  }

  pi.on("tool_call", (event): ToolCallEventResult | undefined => {
    if (!spec) {
      // No spec ⇒ orchestrator not initialized ⇒ nothing is authorized.
      return { block: true, reason: "orchestrator has no active run spec", terminate: true };
    }
    if (event.toolName === SUBAGENT_TOOL) {
      return gateDelegation(event);
    }
    const decision = authorizeToolCall({
      toolName: event.toolName,
      allowedTools: spec.allowed_tools,
      lease,
      spineConnected: client?.isConnected ?? false,
      now: Date.now(),
    });
    if (!decision.block) return undefined;
    return { block: true, reason: decision.reason, terminate: decision.terminate };
  });

  const budgetGuard = () => {
    const verdict = checkBudget(budget, limits(), Date.now());
    if (!verdict.exhausted) return undefined;
    // Stop continuing and record why; a follow-up block/escalate is issued by the
    // agent or operator, but the loop must not run away.
    pi.appendEntry("vtx_budget_exhausted", { run_id: spec?.run_id, reason: verdict.reason });
    return { continue: false };
  };

  pi.on("turn_end", () => {
    budget.turns += 1;
    return budgetGuard();
  });

  pi.on("agent_before_settle", () => budgetGuard());

  pi.on("tool_result", (event): ToolResultEventResult => {
    const secrets = secretValues();
    const content = event.content.map((part) =>
      part.type === "text" ? { ...part, text: redactText(part.text) } : part,
    );
    const structured = redactValue(event.structuredContent, secrets);
    return { content, structuredContent: structured as ToolResultEventResult["structuredContent"] };
  });

  pi.on("session_shutdown", () => {
    // Idempotent: cancel/reload/exit all converge here, possibly more than once.
    stopHeartbeat();
    client?.close();
    client = undefined;
  });
}
