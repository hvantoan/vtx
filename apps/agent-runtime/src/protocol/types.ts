// Types mirror contracts/agent/v1/agent-ipc.json (v1.1).

export type RequestId = string | number;

export interface JsonRpcRequest<T = unknown> {
  jsonrpc: "2.0";
  id: RequestId;
  method: string;
  params?: T;
}

export interface JsonRpcNotification<T = unknown> {
  jsonrpc: "2.0";
  method: string;
  params?: T;
}

export interface JsonRpcError {
  code: number;
  message: string;
  data?: unknown;
}

export interface JsonRpcResponse<T = unknown> {
  jsonrpc: "2.0";
  id: RequestId | null;
  result?: T;
  error?: JsonRpcError;
}

/** Envelope faults use standard JSON-RPC codes; -320xx is the VTX application range. */
export const ErrorCode = {
  ParseError: -32700,
  InvalidRequest: -32600,
  MethodNotFound: -32601,
  InvalidParams: -32602,
  InternalError: -32603,
  TaskNotFound: -32000,
  UnsupportedProtocolVersion: -32001,
  HostBusy: -32002,
  AgentUnavailable: -32003,
  WorkspaceInvalid: -32004,
} as const;

export type AgentType = "pi" | "omp";

export const PROTOCOL_VERSION = "1.1";
export const MAX_EVENT_BYTES = 65536;
export const DEFAULT_HEARTBEAT_MS = 5000;
export const MIN_HEARTBEAT_MS = 1000;

export interface InitializeParams {
  protocol_version: string;
  client_name: string;
  client_version?: string;
  run_id?: string;
  agent_type?: AgentType;
  capabilities?: {
    cancellation?: boolean;
    streaming?: boolean;
    heartbeat_ms?: number;
  };
  env_allowlist?: string[];
}

export interface InitializeResult {
  protocol_version: string;
  agent_type: AgentType;
  agent_version: string;
  host: {
    node_version: string;
    runtime_pid: number;
  };
  adapters: Array<{ name: AgentType; version: string; test_double: boolean }>;
  capabilities: {
    streaming: boolean;
    cancellation: boolean;
    heartbeat_ms?: number;
    max_event_bytes: number;
  };
}

export interface TaskExecuteParams {
  task_id: string;
  attempt?: number;
  prompt: string;
  workspace_path: string;
  test_baseline?: string;
  timeout_ms?: number;
  env_allowlist?: string[];
  metadata?: Record<string, unknown>;
}

export type TaskStatus = "succeeded" | "failed" | "cancelled";

export interface TaskArtifact {
  kind: "commit" | "file" | "log" | "patch";
  ref: string;
}

export interface TaskExecuteResult {
  task_id: string;
  status: TaskStatus;
  output?: string;
  error?: string;
  test_double?: boolean;
  artifacts?: TaskArtifact[];
  usage?: { duration_ms?: number; tokens?: number };
}

export type TaskEventType =
  | "stdout"
  | "stderr"
  | "progress"
  | "status_change"
  | "tool_call"
  | "file_diff"
  | "error"
  | "log";

export interface TaskEventParams {
  task_id: string;
  seq: number;
  event_type: TaskEventType;
  payload?: string;
  data?: Record<string, unknown>;
  timestamp: string;
}

export const CANCEL_REASONS = [
  "operator_cancel",
  "operator_block",
  "timeout",
  "lease_lost",
  "heartbeat_timeout",
  "shutdown",
  "superseded",
] as const;
export type CancelReason = (typeof CANCEL_REASONS)[number];

export interface TaskCancelParams {
  task_id: string;
  /** Controlled vocabulary is CancelReason; unknown strings are tolerated and passed through. */
  reason?: CancelReason | string;
}

export interface HostHeartbeatParams {
  task_id?: string;
  pid: number;
  last_event_seq?: number;
  timestamp?: string;
}

export interface HostLogParams {
  level: "debug" | "info" | "warn" | "error";
  message: string;
  meta?: Record<string, unknown>;
}
