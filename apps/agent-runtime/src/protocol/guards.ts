import type {
  AgentType,
  InitializeParams,
  JsonRpcNotification,
  JsonRpcRequest,
  RequestId,
  TaskCancelParams,
  TaskExecuteParams,
} from "./types.js";
import { MIN_HEARTBEAT_MS } from "./types.js";

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function isRequestId(value: unknown): value is RequestId {
  return typeof value === "string" || typeof value === "number";
}

/** An incoming envelope: a request when it carries an id, a notification otherwise. */
export type Incoming =
  | { kind: "request"; message: JsonRpcRequest }
  | { kind: "notification"; message: JsonRpcNotification }
  | { kind: "invalid"; id: RequestId | null; reason: string };

export function classifyIncoming(value: unknown): Incoming {
  if (!isRecord(value)) {
    return { kind: "invalid", id: null, reason: "Invalid Request: message must be a JSON object" };
  }
  const id = isRequestId(value.id) ? value.id : null;
  if (value.jsonrpc !== "2.0") {
    return { kind: "invalid", id, reason: "Invalid Request: jsonrpc version must be 2.0" };
  }
  if (typeof value.method !== "string" || value.method.length === 0) {
    return { kind: "invalid", id, reason: "Invalid Request: method must be a non-empty string" };
  }
  if (value.params !== undefined && !isRecord(value.params)) {
    return { kind: "invalid", id, reason: "Invalid Request: params must be an object" };
  }
  if ("id" in value && value.id !== null && !isRequestId(value.id)) {
    return { kind: "invalid", id: null, reason: "Invalid Request: id must be a string or number" };
  }
  if (id !== null) {
    return { kind: "request", message: { jsonrpc: "2.0", id, method: value.method, params: value.params } };
  }
  return { kind: "notification", message: { jsonrpc: "2.0", method: value.method, params: value.params } };
}

function stringArrayOrUndefined(value: unknown): string[] | undefined | "invalid" {
  if (value === undefined) return undefined;
  return Array.isArray(value) && value.every((v): v is string => typeof v === "string") ? value : "invalid";
}

function intAtLeast(value: unknown, min: number): number | undefined | "invalid" {
  if (value === undefined) return undefined;
  return typeof value === "number" && Number.isInteger(value) && value >= min ? value : "invalid";
}

function isAgentType(value: unknown): value is AgentType {
  return value === "pi" || value === "omp";
}

export type Validation<T> = { ok: true; value: T } | { ok: false; message: string };

export function parseInitializeParams(params: unknown): Validation<InitializeParams> {
  if (!isRecord(params)) return { ok: false, message: "params must be an object" };
  const { protocol_version, client_name, client_version, run_id, agent_type, capabilities } = params;
  if (typeof protocol_version !== "string" || !/^[0-9]+\.[0-9]+$/.test(protocol_version)) {
    return { ok: false, message: "protocol_version must be MAJOR.MINOR" };
  }
  if (typeof client_name !== "string" || client_name.length === 0) {
    return { ok: false, message: "client_name is required" };
  }
  if (agent_type !== undefined && !isAgentType(agent_type)) {
    return { ok: false, message: "agent_type must be pi or omp" };
  }
  if (run_id !== undefined && typeof run_id !== "string") {
    return { ok: false, message: "run_id must be a string" };
  }
  const envAllowlist = stringArrayOrUndefined(params.env_allowlist);
  if (envAllowlist === "invalid") return { ok: false, message: "env_allowlist must be an array of strings" };
  if (capabilities !== undefined && !isRecord(capabilities)) {
    return { ok: false, message: "capabilities must be an object" };
  }
  const heartbeatMs = intAtLeast(capabilities?.heartbeat_ms, MIN_HEARTBEAT_MS);
  if (heartbeatMs === "invalid") {
    return { ok: false, message: `capabilities.heartbeat_ms must be an integer >= ${MIN_HEARTBEAT_MS}` };
  }
  return {
    ok: true,
    value: {
      protocol_version,
      client_name,
      client_version: typeof client_version === "string" ? client_version : undefined,
      run_id,
      agent_type,
      capabilities: heartbeatMs === undefined ? undefined : { heartbeat_ms: heartbeatMs },
      env_allowlist: envAllowlist,
    },
  };
}

export function parseTaskExecuteParams(params: unknown): Validation<TaskExecuteParams> {
  if (!isRecord(params)) return { ok: false, message: "params must be an object" };
  const { task_id, prompt, workspace_path, test_baseline, metadata } = params;
  if (typeof task_id !== "string" || task_id.length === 0) return { ok: false, message: "task_id is required" };
  if (typeof prompt !== "string" || prompt.length === 0) return { ok: false, message: "prompt is required" };
  if (typeof workspace_path !== "string" || workspace_path.length === 0) {
    return { ok: false, message: "workspace_path is required" };
  }
  const attempt = intAtLeast(params.attempt, 1);
  if (attempt === "invalid") return { ok: false, message: "attempt must be an integer >= 1" };
  const timeoutMs = intAtLeast(params.timeout_ms, 100);
  if (timeoutMs === "invalid") return { ok: false, message: "timeout_ms must be an integer >= 100" };
  if (test_baseline !== undefined && typeof test_baseline !== "string") {
    return { ok: false, message: "test_baseline must be a string" };
  }
  const envAllowlist = stringArrayOrUndefined(params.env_allowlist);
  if (envAllowlist === "invalid") return { ok: false, message: "env_allowlist must be an array of strings" };
  if (metadata !== undefined && !isRecord(metadata)) return { ok: false, message: "metadata must be an object" };
  return {
    ok: true,
    value: {
      task_id,
      prompt,
      workspace_path,
      attempt,
      timeout_ms: timeoutMs,
      test_baseline,
      env_allowlist: envAllowlist,
      metadata,
    },
  };
}

export function parseTaskCancelParams(params: unknown): TaskCancelParams | undefined {
  if (!isRecord(params) || typeof params.task_id !== "string") return undefined;
  return { task_id: params.task_id, reason: typeof params.reason === "string" ? params.reason : undefined };
}
