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

const DEFAULT_API_BASE_URL = "http://localhost:49080";
const TOKEN_STORAGE_KEY = "vtx.operator_token";
const OPERATOR_ID_STORAGE_KEY = "vtx.operator_id";

export interface OperatorCredentials {
  token: string;
  operatorId: string;
}

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

function now(): number {
  return typeof performance === "undefined" ? Date.now() : performance.now();
}

export function getApiBaseUrl(): string {
  return (
    process.env.NEXT_PUBLIC_API_BASE_URL ??
    process.env.NEXT_PUBLIC_API_URL ??
    DEFAULT_API_BASE_URL
  ).replace(/\/$/, "");
}

function readStorage(key: string): string {
  if (typeof window === "undefined") return "";

  try {
    return window.sessionStorage.getItem(key) ?? window.localStorage.getItem(key) ?? "";
  } catch {
    return "";
  }
}

export function getOperatorCredentials(): OperatorCredentials {
  return {
    token: readStorage(TOKEN_STORAGE_KEY) || process.env.NEXT_PUBLIC_API_AUTH_TOKEN || "",
    operatorId: readStorage(OPERATOR_ID_STORAGE_KEY),
  };
}

export function saveOperatorCredentials(token: string, operatorId: string): void {
  if (typeof window === "undefined") return;
  const normalizedToken = token.trim();
  const normalizedOperatorId = operatorId.trim();
  window.sessionStorage.setItem(TOKEN_STORAGE_KEY, normalizedToken);
  window.sessionStorage.setItem(OPERATOR_ID_STORAGE_KEY, normalizedOperatorId);
  window.localStorage.setItem(TOKEN_STORAGE_KEY, normalizedToken);
  window.localStorage.setItem(OPERATOR_ID_STORAGE_KEY, normalizedOperatorId);
}

export function clearOperatorCredentials(): void {
  if (typeof window === "undefined") return;
  window.sessionStorage.removeItem(TOKEN_STORAGE_KEY);
  window.sessionStorage.removeItem(OPERATOR_ID_STORAGE_KEY);
  window.localStorage.removeItem(TOKEN_STORAGE_KEY);
  window.localStorage.removeItem(OPERATOR_ID_STORAGE_KEY);
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
  authenticated = false,
  acceptedStatuses: readonly number[] = [200],
): Promise<ApiResult<T>> {
  const startedAt = now();
  const credentials = getOperatorCredentials();

  if (authenticated && !credentials.token) {
    return {
      data: null,
      error: {
        error: "missing_operator_token",
        message: "Configure an operator token in Settings or NEXT_PUBLIC_API_AUTH_TOKEN.",
      },
      latencyMs: 0,
      status: null,
    };
  }

  const headers = new Headers(options.headers);
  headers.set("Accept", "application/json");
  if (options.body !== undefined) headers.set("Content-Type", "application/json");
  if (authenticated) headers.set("Authorization", `Bearer ${credentials.token}`);
  if (authenticated && credentials.operatorId) {
    headers.set("X-Operator-Id", credentials.operatorId);
  }

  try {
    const response = await fetch(`${getApiBaseUrl()}${path}`, {
      ...options,
      headers,
      cache: "no-store",
    });
    const latencyMs = Math.round(now() - startedAt);

    if (!acceptedStatuses.includes(response.status)) {
      return {
        data: null,
        error: await parseError(response),
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
  return requestJson<ReadyResponse>("/ready", {}, false, [200, 503]);
}

export function fetchSystemStatus(): Promise<ApiResult<SystemStatusResponse>> {
  return requestJson<SystemStatusResponse>("/api/v1/system/status");
}

export function fetchWorkers(): Promise<ApiResult<WorkerStatusResponse>> {
  return requestJson<WorkerStatusResponse>("/api/v1/workers");
}

export function fetchOperatorOverview(): Promise<ApiResult<OperatorOverview>> {
  return requestJson<OperatorOverview>("/api/v1/operator/overview", {}, true);
}

export function fetchOperatorWorkers(): Promise<ApiResult<CursorPage<OperatorWorkerStatus>>> {
  return requestJson<CursorPage<OperatorWorkerStatus>>("/api/v1/operator/workers", {}, true);
}

export function listRuns(filters: RunFilters = {}): Promise<ApiResult<CursorPage<RunListItem>>> {
  const query = new URLSearchParams();
  if (filters.state) query.set("state", filters.state);
  if (filters.issueKey) query.set("issue_key", filters.issueKey);
  if (filters.cursor) query.set("cursor", filters.cursor);
  if (filters.limit !== undefined) query.set("limit", String(filters.limit));
  const suffix = query.size > 0 ? `?${query.toString()}` : "";
  return requestJson<CursorPage<RunListItem>>(`/api/v1/operator/runs${suffix}`, {}, true);
}

export function getRunDetail(runId: string): Promise<ApiResult<RunDetailView>> {
  return requestJson<RunDetailView>(`/api/v1/operator/runs/${encodeURIComponent(runId)}`, {}, true);
}

export function getRegistryRun(runId: string): Promise<ApiResult<RegistryRun>> {
  return requestJson<RegistryRun>(`/api/v1/registry/runs/${encodeURIComponent(runId)}`, {}, true);
}

export function listRunEvents(runId: string): Promise<ApiResult<CursorPage<RegistryRunEvent>>> {
  return requestJson<CursorPage<RegistryRunEvent>>(
    `/api/v1/registry/runs/${encodeURIComponent(runId)}/events?limit=200`,
    {},
    true,
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
    true,
  );
}

export function fetchOutboxStats(): Promise<ApiResult<Record<string, OutboxTypeCounters>>> {
  return requestJson<Record<string, OutboxTypeCounters>>("/api/v1/operator/outbox", {}, true);
}

export function listDlq(): Promise<ApiResult<CursorPage<DlqEvent>>> {
  return requestJson<CursorPage<DlqEvent>>("/api/v1/operator/outbox/dlq?limit=50", {}, true);
}

export function redriveDlq(outboxId: string, reason: string): Promise<ApiResult<never>> {
  return requestJson<never>(
    `/api/v1/operator/outbox/dlq/${encodeURIComponent(outboxId)}/redrive`,
    { method: "POST", body: JSON.stringify({ reason }) },
    true,
    [202],
  );
}

export function syncPullRequest(runId: string): Promise<ApiResult<PullRequest>> {
  return requestJson<PullRequest>(
    `/api/v1/pull_requests/${encodeURIComponent(runId)}/sync`,
    { method: "POST" },
    true,
  );
}

export function buildEventStreamRequest(runId?: string, lastEventId?: string): EventStreamRequest | null {
  const { token } = getOperatorCredentials();
  if (!token) return null;

  const url = new URL(`${getApiBaseUrl()}/api/v1/events`);
  if (runId) url.searchParams.set("run_id", runId);

  const headers = new Headers({
    Accept: "text/event-stream",
    Authorization: `Bearer ${token}`,
  });
  if (lastEventId) headers.set("Last-Event-ID", lastEventId);

  return {
    url: url.toString(),
    init: { method: "GET", headers, cache: "no-store" },
  };
}

export async function openEventStream(
  runId: string | undefined,
  lastEventId: string | undefined,
  signal: AbortSignal,
): Promise<Response | null> {
  const request = buildEventStreamRequest(runId, lastEventId);
  if (!request) return null;

  const configuredBase = new URL(getApiBaseUrl());
  const streamUrl = new URL(request.url);
  if (
    streamUrl.origin !== configuredBase.origin ||
    streamUrl.pathname !== "/api/v1/events" ||
    !["http:", "https:"].includes(streamUrl.protocol)
  ) {
    throw new Error("Refusing event stream URL outside the configured API origin");
  }

  return fetch(streamUrl, { ...request.init, signal });
}
