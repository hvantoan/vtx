import type { AgentType, TaskArtifact, TaskEventType, TaskExecuteParams, TaskExecuteResult } from "../protocol/types.js";

export interface AdapterEvent {
  event_type: TaskEventType;
  payload?: string;
  data?: Record<string, unknown>;
}

export interface AdapterRequest {
  params: TaskExecuteParams;
  /** Canonical, host-validated worktree directory; the agent process cwd. */
  workspacePath: string;
  /** Already filtered to the allowlist by the host; the only env the agent child sees. */
  env: NodeJS.ProcessEnv;
  onEvent: (event: AdapterEvent) => void;
  /** Abort reason (`signal.reason`) is the cancel reason string, e.g. "timeout". */
  signal: AbortSignal;
}

/** Thrown by `preflight()` when the agent CLI cannot be executed (-32003 agent_unavailable). */
export class AgentUnavailableError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AgentUnavailableError";
  }
}

export interface AgentAdapter {
  readonly name: AgentType;
  readonly version: string;
  /** FR-AGT-003: true only for TEST DOUBLE adapters; real CLI adapters are always false. */
  readonly isSimulation: boolean;
  /** Real adapters verify the CLI is runnable; resolves or throws AgentUnavailableError. */
  preflight(env: NodeJS.ProcessEnv): Promise<void>;
  execute(request: AdapterRequest): Promise<TaskExecuteResult>;
}

export type { TaskArtifact };
