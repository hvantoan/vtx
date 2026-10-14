export interface HealthResponse {
  status: "ok" | "down";
  timestamp: string;
  uptime_seconds: number;
}

export interface DependencyCheck {
  status: "up" | "down" | "degraded";
  latency_ms: number;
  error?: string | null;
}

export interface ReadyResponse {
  status: "ready" | "not_ready";
  timestamp: string;
  dependencies: {
    database?: DependencyCheck;
    temporal?: DependencyCheck;
  };
}

export interface SystemStatusResponse {
  version: string;
  commit: string;
  environment: string;
  go_version: string;
  goroutines: number;
  uptime_seconds: number;
  timestamp: string;
}

export interface WorkerItem {
  id: string;
  queue: string;
  status: "running" | "idle" | "stopped";
  active_workflows: number;
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
