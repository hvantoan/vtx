export type HealthStatus = "ok" | "down";
export type DependencyStatus = "up" | "down" | "degraded";
export type ReadinessStatus = "ready" | "not_ready";
export type WorkerRuntimeStatus = "running" | "idle" | "stopped";
export type RunState =
  | "queued"
  | "preparing"
  | "coding"
  | "verifying"
  | "qa"
  | "quality_gate"
  | "pr_draft"
  | "completed"
  | "blocked"
  | "retry_wait"
  | "failed"
  | "cancelled";
export type OperatorAction = "block" | "resume" | "cancel";

export interface HealthResponse {
  status: HealthStatus;
  timestamp: string;
  uptime_seconds: number;
}

export interface DependencyCheck {
  status: DependencyStatus;
  latency_ms: number;
  error?: string | null;
}

export interface ReadyResponse {
  status: ReadinessStatus;
  timestamp: string;
  dependencies: {
    database?: DependencyCheck;
    temporal?: DependencyCheck;
    outbox?: DependencyCheck;
  };
}

export interface SystemStatusResponse {
  version: string;
  commit: string;
  environment: string;
  go_version: string;
  goroutines: number;
  uptime_seconds?: number;
}

export interface WorkerItem {
  id: string;
  queue: string;
  status: WorkerRuntimeStatus;
  active_workflows?: number;
}

export interface WorkerStatusResponse {
  status: string;
  workers: WorkerItem[];
}

export interface PlatformEvent {
  id: string;
  type: string;
  timestamp: string;
  source: string;
  data: Record<string, unknown>;
}

export interface ErrorResponse {
  error: string;
  message: string;
}

export interface RegistryRun {
  run_id: string;
  task_id: string;
  state: RunState;
  version: number;
  attempt: number;
  resume_state?: string | null;
  retry_due_at?: string | null;
  commit_sha?: string;
  branch?: string;
  error_code?: string;
  error_message?: string;
  lease_owner?: string;
  lease_epoch: number;
  lease_expires_at?: string;
  correlation_id?: string;
  created_at?: string;
  updated_at?: string;
  started_at?: string;
  finished_at?: string;
}

export interface RegistryRunEvent {
  previous_state: string;
  current_state: string;
  action: string;
  actor: string;
  actor_type: "system" | "human";
  version: number;
  metadata?: Record<string, unknown>;
  created_at: string;
}

export interface PullRequest {
  pull_request_id?: string;
  run_id?: string;
  repo_owner?: string;
  repo_name?: string;
  pr_number?: number | null;
  pr_url?: string | null;
  head_branch?: string;
  head_sha?: string;
  base_branch?: string;
  draft?: boolean;
  status?: "requested" | "open" | "closed" | "merged" | "failed";
  ci_status?: string | null;
  error_code?: string | null;
  created_at?: string;
  last_synced_at?: string | null;
}

export interface OperatorOverview {
  runs_by_state?: Record<string, number>;
  deferred_count?: number;
  admission?: {
    max?: 3;
    active?: number;
  };
  outbox?: OutboxTypeCounters;
  workers?: {
    up?: number;
    down?: number;
  };
}

export interface RunListItem {
  run_id?: string;
  issue_key?: string;
  state?: string;
  attempt?: number;
  commit_sha?: string | null;
  pr_url?: string | null;
  lease_owner?: string | null;
  error_code?: string | null;
  created_at?: string;
  updated_at?: string;
}

export interface RunDetailView {
  run?: RunListItem;
  worktree?: {
    path?: string;
    branch?: string;
    base_sha?: string;
    status?: string;
  } | null;
  pull_request?: PullRequest | null;
  gate_decision?: {
    decision?: "pass" | "fail";
    subject_commit_sha?: string;
    decided_at?: string;
  } | null;
  last_error?: {
    error_code?: string;
    error_message?: string;
  } | null;
}

export interface OutboxTypeCounters {
  pending?: number;
  retrying?: number;
  dead_lettered?: number;
  oldest_age_seconds?: number;
}

export interface DlqEvent {
  outbox_id?: string;
  event_type?: string;
  dead_letter_reason?: string;
  attempt?: number;
  dead_lettered_at?: string;
  redrive_count?: number;
}

export interface OperatorWorkerStatus {
  worker_id?: string;
  worker_kind?: "scheduler" | "dispatcher" | "agent_host" | "sweeper";
  run_id?: string | null;
  last_beat_at?: string;
  status?: "up" | "down";
  metadata?: Record<string, unknown>;
}

export interface CursorPage<T> {
  items?: T[];
  next_cursor?: string | number | null;
}

export interface ApiResult<T> {
  data: T | null;
  error: ErrorResponse | null;
  latencyMs: number;
  status: number | null;
}
