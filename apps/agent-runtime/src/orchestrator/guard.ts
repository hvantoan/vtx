/**
 * Fail-closed authorization and budget logic for the orchestrator extension
 * (design doc §3.2 hooks, §8 guardrails). These are pure functions with no Pi
 * or I/O dependencies so they can be unit-tested directly; the extension glue
 * merely translates their verdicts into `tool_call` / boundary-event results.
 */

import { SUBAGENT_TOOL } from "./delegate.js";
import { SpineErrorCode } from "./protocol.js";

/**
 * Protocol failures mean the session can never succeed (bad token, a tool the
 * spine does not implement), so the batch stops. Domain refusals — invalid
 * transition, stale version, an unoperational stage — are recoverable: the
 * model reads the message and takes the missing step, so it must not be killed.
 */
export function terminatesSession(code: string): boolean {
  return code === SpineErrorCode.Unauthenticated || code === SpineErrorCode.UnknownTool;
}

/**
 * Tools that mutate durable state and therefore require a live lease.
 * Delegation writes code in the run worktree, so it is lease-gated too.
 */
export const MUTATING_TOOLS = new Set([
  "vtx_advance",
  "vtx_block",
  "vtx_escalate",
  "vtx_open_pr",
  SUBAGENT_TOOL,
]);

export interface LeaseState {
  held: boolean;
  owner: string;
  epoch: number;
  /** Expiry as epoch milliseconds. */
  expiresAt: number;
}

export interface AuthInput {
  toolName: string;
  allowedTools: readonly string[];
  lease: LeaseState;
  spineConnected: boolean;
  /** Current time as epoch milliseconds. */
  now: number;
}

export interface AuthDecision {
  block: boolean;
  reason?: string;
  /** Hint Pi to stop the batch when blocked (used on lease loss / spine down). */
  terminate?: boolean;
}

const ALLOW: AuthDecision = { block: false };

/**
 * Decide whether a tool call may proceed. Every branch fails closed: an unknown
 * tool, an unreachable spine, or a missing/expired lease on a mutating call all
 * block. The spine remains the real authority (it re-checks CAS + lease epoch);
 * this gate stops obviously-illegal calls before they consume a turn.
 */
export function authorizeToolCall(input: AuthInput): AuthDecision {
  const { toolName, allowedTools, lease, spineConnected, now } = input;

  if (!allowedTools.includes(toolName)) {
    return { block: true, reason: `tool ${toolName} is not in the run's allowed_tools` };
  }

  // The authority channel is down: nothing may proceed.
  if (!spineConnected) {
    return { block: true, reason: "spine unreachable", terminate: true };
  }

  if (MUTATING_TOOLS.has(toolName)) {
    if (!lease.held) {
      return { block: true, reason: "no lease held for this run", terminate: true };
    }
    if (lease.expiresAt <= now) {
      return { block: true, reason: "lease expired", terminate: true };
    }
  }

  return ALLOW;
}

export interface BudgetLimits {
  max_turns: number;
  max_delegations: number;
  wall_clock_ms: number;
}

export interface BudgetUsage {
  turns: number;
  delegations: number;
  startedAt: number;
}

export interface BudgetVerdict {
  exhausted: boolean;
  reason?: string;
}

/**
 * Budget / stop-condition guard evaluated at `turn_end` and
 * `agent_before_settle`. Prevents runaway delegation or loops; when exhausted the
 * extension stops continuing and blocks the run for operator escalation.
 */
export function checkBudget(usage: BudgetUsage, limits: BudgetLimits, now: number): BudgetVerdict {
  if (limits.max_turns > 0 && usage.turns >= limits.max_turns) {
    return { exhausted: true, reason: `turn budget exhausted (${usage.turns}/${limits.max_turns})` };
  }
  if (limits.max_delegations > 0 && usage.delegations >= limits.max_delegations) {
    return { exhausted: true, reason: `delegation budget exhausted (${usage.delegations}/${limits.max_delegations})` };
  }
  if (limits.wall_clock_ms > 0 && now - usage.startedAt >= limits.wall_clock_ms) {
    return { exhausted: true, reason: "wall-clock budget exhausted" };
  }
  return { exhausted: false };
}

/**
 * Headless assertion. VTX runs the orchestrator in `--mode json`, where there is
 * no dialog UI. Any code path that would need `ctx.ui.confirm` is a design bug;
 * this guard makes it explicit and testable instead of silently hanging.
 */
export function assertHeadless(hasUI: boolean, mode: string): void {
  if (hasUI) {
    throw new Error(`orchestrator must run headless, but hasUI=true (mode=${mode})`);
  }
}
