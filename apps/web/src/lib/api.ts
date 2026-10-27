import type {
  ApiResult,
  CursorPage,
  DlqEvent,
  ErrorResponse,
  HealthResponse,
  OperatorAction,
  OperatorOverview,
  OperatorWorkerStatus,
  OutboxTypeCounters,
  PullRequest,
  ReadyResponse,
  RegistryRun,
  RegistryRunEvent,
  RunDetailView,
  RunListItem,
  SystemStatusResponse,
  WorkerStatusResponse,
} from "../types/api";

export interface RunFilters {
  state?: string;
  issueKey?: string;
  cursor?: string;
  limit?: number;
}

export interface EventStreamRequest {
  url: string;
  init: RequestInit;
}

export interface SessionUser {
  username: string;
  expires_at: string;
}

/** Fired when any same-origin request comes back 401 so the guard can redirect. */
export const SESSION_EXPIRED_EVENT = "vtx:session-expired";

function now(): number {
  return typeof performance === "undefined" ? Date.now() : performance.now();
}

function notifySessionExpired(): void {
  if (typeof window !== "undefined") window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT));
}

function isErrorResponse(value: unknown): value is ErrorResponse {
  if (typeof value !== "object" || value === null) return false;
  return "error" in value && "message" in value;
}

async function parseError(response: Response): Promise<ErrorResponse> {
  try {
    const body: unknown = await response.json();
    if (isErrorResponse(body)) {
      return { error: String(body.error), message: String(body.message) };
    }
  } catch {
    // The status text below is the honest fallback for an empty/non-JSON response.
  }

  return {
    error: `http_${response.status}`,
    message: response.statusText || `Request failed with HTTP ${response.status}`,
  };
}

async function requestJson<T>(
  path: string,
  options: RequestInit = {},
  acceptedStatuses: readonly number[] = [200],
): Promise<ApiResult<T>> {
  const startedAt = now();

  const headers = new Headers(options.headers);
  headers.set("Accept", "application/json");
  if (options.body !== undefined) headers.set("Content-Type", "application/json");

  try {
    const response = await fetch(path, {
      ...options,
      headers,
      cache: "no-store",
      credentials: "include",
    });
    const latencyMs = Math.round(now() - startedAt);

    if (!acceptedStatuses.includes(response.status)) {
      const error = await parseError(response);
      // 401 only means "session gone" when the API says so; a wrong current
      // password on /auth/password also returns 401 but must not sign out.
      if (response.status === 401 && error.error === "unauthorized") notifySessionExpired();
      return {
        data: null,
        error,
        latencyMs,
        status: response.status,
      };
    }

    if (response.status === 202 || response.status === 204) {
      return { data: null, error: null, latencyMs, status: response.status };
    }

    const data = (await response.json()) as T;
    return { data, error: null, latencyMs, status: response.status };
  } catch (error: unknown) {
    return {
      data: null,
      error: {
        error: "network_error",
        message: error instanceof Error ? error.message : String(error),
      },
      latencyMs: Math.round(now() - startedAt),
      status: null,
    };
  }
}

export function fetchHealth(): Promise<ApiResult<HealthResponse>> {
  return requestJson<HealthResponse>("/health");
}

export function fetchReady(): Promise<ApiResult<ReadyResponse>> {
  return requestJson<ReadyResponse>("/ready", {}, [200, 503]);
}

export function fetchSystemStatus(): Promise<ApiResult<SystemStatusResponse>> {
  return requestJson<SystemStatusResponse>("/api/v1/system/status");
}

export function fetchWorkers(): Promise<ApiResult<WorkerStatusResponse>> {
  return requestJson<WorkerStatusResponse>("/api/v1/workers");
}

export function fetchOperatorOverview(): Promise<ApiResult<OperatorOverview>> {
  return requestJson<OperatorOverview>("/api/v1/operator/overview");
}

export function fetchOperatorWorkers(): Promise<ApiResult<CursorPage<OperatorWorkerStatus>>> {
  return requestJson<CursorPage<OperatorWorkerStatus>>("/api/v1/operator/workers");
}

export function listRuns(filters: RunFilters = {}): Promise<ApiResult<CursorPage<RunListItem>>> {
  const query = new URLSearchParams();
  if (filters.state) query.set("state", filters.state);
  if (filters.issueKey) query.set("issue_key", filters.issueKey);
  if (filters.cursor) query.set("cursor", filters.cursor);
  if (filters.limit !== undefined) query.set("limit", String(filters.limit));
  const suffix = query.size > 0 ? `?${query.toString()}` : "";
  return requestJson<CursorPage<RunListItem>>(`/api/v1/operator/runs${suffix}`);
}

export function getRunDetail(runId: string): Promise<ApiResult<RunDetailView>> {
  return requestJson<RunDetailView>(`/api/v1/operator/runs/${encodeURIComponent(runId)}`);
}

export function getRegistryRun(runId: string): Promise<ApiResult<RegistryRun>> {
  return requestJson<RegistryRun>(`/api/v1/registry/runs/${encodeURIComponent(runId)}`);
}

export function listRunEvents(runId: string): Promise<ApiResult<CursorPage<RegistryRunEvent>>> {
  return requestJson<CursorPage<RegistryRunEvent>>(
    `/api/v1/registry/runs/${encodeURIComponent(runId)}/events?limit=200`,
  );
}

export function postTransition(
  runId: string,
  expectedVersion: number,
  action: OperatorAction,
  reason: string,
): Promise<ApiResult<RegistryRun>> {
  return requestJson<RegistryRun>(
    `/api/v1/registry/runs/${encodeURIComponent(runId)}/transitions`,
    {
      method: "POST",
      body: JSON.stringify({
        expected_version: expectedVersion,
        action,
        reason,
      }),
    },
  );
}

export function fetchOutboxStats(): Promise<ApiResult<Record<string, OutboxTypeCounters>>> {
  return requestJson<Record<string, OutboxTypeCounters>>("/api/v1/operator/outbox");
}

export function listDlq(): Promise<ApiResult<CursorPage<DlqEvent>>> {
  return requestJson<CursorPage<DlqEvent>>("/api/v1/operator/outbox/dlq?limit=50");
}

export function redriveDlq(outboxId: string, reason: string): Promise<ApiResult<never>> {
  return requestJson<never>(
    `/api/v1/operator/outbox/dlq/${encodeURIComponent(outboxId)}/redrive`,
    { method: "POST", body: JSON.stringify({ reason }) },
    [202],
  );
}

export function syncPullRequest(runId: string): Promise<ApiResult<PullRequest>> {
  return requestJson<PullRequest>(
    `/api/v1/pull_requests/${encodeURIComponent(runId)}/sync`,
    { method: "POST" },
  );
}

export function login(username: string, password: string): Promise<ApiResult<SessionUser>> {
  return requestJson<SessionUser>("/api/v1/auth/login", {
    method: "POST",
    body: JSON.stringify({ username: username.trim(), password }),
  });
}

export function fetchCurrentUser(): Promise<ApiResult<SessionUser>> {
  return requestJson<SessionUser>("/api/v1/auth/me");
}

export function logoutSession(): Promise<ApiResult<null>> {
  return requestJson<null>("/api/v1/auth/logout", { method: "POST" }, [204]);
}

export function changePassword(currentPassword: string, newPassword: string): Promise<ApiResult<null>> {
  return requestJson<null>(
    "/api/v1/auth/password",
    { method: "POST", body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }) },
    [204],
  );
}

export function buildEventStreamRequest(runId?: string, lastEventId?: string): EventStreamRequest {
  const query = new URLSearchParams();
  if (runId) query.set("run_id", runId);
  const suffix = query.size > 0 ? `?${query.toString()}` : "";

  const headers = new Headers({ Accept: "text/event-stream" });
  if (lastEventId) headers.set("Last-Event-ID", lastEventId);

  return {
    url: `/api/v1/events${suffix}`,
    init: { method: "GET", headers, cache: "no-store", credentials: "include" },
  };
}

export async function openEventStream(
  runId: string | undefined,
  lastEventId: string | undefined,
  signal: AbortSignal,
): Promise<Response> {
  const request = buildEventStreamRequest(runId, lastEventId);
  const response = await fetch(request.url, { ...request.init, signal });
  if (response.status === 401) notifySessionExpired();
  return response;
}
