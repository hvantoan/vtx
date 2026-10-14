export interface JsonRpcRequest<T = unknown> {
  jsonrpc: "2.0";
  id: string | number;
  method: string;
  params?: T;
}

export interface JsonRpcNotification<T = unknown> {
  jsonrpc: "2.0";
  method: string;
  params?: T;
}

export interface JsonRpcResponse<T = unknown> {
  jsonrpc: "2.0";
  id: string | number | null;
  result?: T;
  error?: {
    code: number;
    message: string;
    data?: unknown;
  };
}

export interface InitializeParams {
  protocol_version: string;
  client_name: string;
  client_version?: string;
  agent_type?: "pi" | "omp";
}

export interface InitializeResult {
  protocol_version: "1.0";
  agent_type: "pi" | "omp";
  agent_version: string;
  capabilities: {
    streaming: boolean;
    cancellation: boolean;
  };
}

export interface TaskExecuteParams {
  task_id: string;
  prompt: string;
  workspace_path?: string;
  timeout_ms?: number;
}

export interface TaskExecuteResult {
  task_id: string;
  status: "succeeded" | "failed" | "cancelled";
  output?: string;
  error?: string;
}

export interface TaskEventParams {
  task_id: string;
  seq: number;
  event_type: "stdout" | "stderr" | "progress" | "status_change";
  payload: string;
  timestamp: string;
}

export interface TaskCancelParams {
  task_id: string;
  reason?: string;
}
