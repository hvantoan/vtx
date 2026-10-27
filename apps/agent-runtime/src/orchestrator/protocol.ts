/**
 * Wire contract for the VTX Go spine tool server, mirrored exactly from
 * apps/backend/internal/orchestration/spineserver/protocol.go.
 *
 * The spine is the authority: it owns state transitions, lease fencing, gate
 * decisions, and PR creation. The orchestrator only asks it to run a tool and
 * reads back a structured result; it never forges evidence or decides a gate.
 */

/** Tool names the orchestrator may invoke. Mirror of the Go `Tool*` consts. */
export const SpineTool = {
  Intake: "intake",
  Verify: "verify",
  Gate: "gate",
  QALaunch: "qa_launch",
  QAVerdict: "qa_verdict",
  OpenPR: "open_pr",
  Advance: "advance",
  Block: "block",
  RenewLease: "renew_lease",
} as const;

export type SpineToolName = (typeof SpineTool)[keyof typeof SpineTool];

/** Stable error codes returned in `Response.error.code`. Mirror of Go `ErrCode*`. */
export const SpineErrorCode = {
  Unauthenticated: "unauthenticated",
  UnknownTool: "unknown_tool",
  ToolUnavailable: "tool_unavailable",
  InvalidRequest: "invalid_request",
  InvalidArgs: "invalid_args",
  Service: "service_error",
} as const;

export type SpineErrorCodeValue = (typeof SpineErrorCode)[keyof typeof SpineErrorCode];

/** One NDJSON line sent to the spine. */
export interface SpineRequest {
  id: string;
  token: string;
  tool: string;
  run_id: string;
  lease_epoch: number;
  args?: unknown;
}

export interface SpineResponseError {
  code: string;
  message: string;
}

/** One NDJSON line returned by the spine. */
export interface SpineResponse {
  id: string;
  ok: boolean;
  tool: string;
  result?: unknown;
  error?: SpineResponseError;
}

/**
 * A failed spine call surfaced to callers as a typed error so the extension can
 * fail closed and treat transport failures the same as service refusals.
 */
export class SpineError extends Error {
  readonly code: string;
  readonly tool: string;

  constructor(code: string, message: string, tool: string) {
    super(message);
    this.name = "SpineError";
    this.code = code;
    this.tool = tool;
  }
}

/** Type guard: is `value` shaped like a spine response line? */
export function isSpineResponse(value: unknown): value is SpineResponse {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  return typeof v.id === "string" && typeof v.ok === "boolean" && typeof v.tool === "string";
}
