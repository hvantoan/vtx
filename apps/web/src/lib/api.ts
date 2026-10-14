import type {
  HealthResponse,
  ReadyResponse,
  SystemStatusResponse,
  WorkerStatusResponse,
} from "../types/api";

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";

export async function fetchHealth(): Promise<{ data: HealthResponse | null; error: string | null; latencyMs: number }> {
  const start = performance.now();
  try {
    const res = await fetch(`${API_BASE_URL}/health`, { cache: "no-store" });
    const latencyMs = Math.round(performance.now() - start);
    if (!res.ok) {
      return { data: null, error: `HTTP ${res.status}: ${res.statusText}`, latencyMs };
    }
    const data = (await res.json()) as HealthResponse;
    return { data, error: null, latencyMs };
  } catch (err: unknown) {
    const latencyMs = Math.round(performance.now() - start);
    const message = err instanceof Error ? err.message : String(err);
    return { data: null, error: message, latencyMs };
  }
}

export async function fetchReady(): Promise<{ data: ReadyResponse | null; error: string | null; latencyMs: number }> {
  const start = performance.now();
  try {
    const res = await fetch(`${API_BASE_URL}/ready`, { cache: "no-store" });
    const latencyMs = Math.round(performance.now() - start);
    // 503 is a valid response for Ready (dependencies down)
    if (res.status === 200 || res.status === 503) {
      const data = (await res.json()) as ReadyResponse;
      return { data, error: null, latencyMs };
    }
    return { data: null, error: `HTTP ${res.status}: ${res.statusText}`, latencyMs };
  } catch (err: unknown) {
    const latencyMs = Math.round(performance.now() - start);
    const message = err instanceof Error ? err.message : String(err);
    return { data: null, error: message, latencyMs };
  }
}

export async function fetchSystemStatus(): Promise<{ data: SystemStatusResponse | null; error: string | null }> {
  try {
    const res = await fetch(`${API_BASE_URL}/api/v1/system/status`, { cache: "no-store" });
    if (!res.ok) {
      return { data: null, error: `HTTP ${res.status}` };
    }
    const data = (await res.json()) as SystemStatusResponse;
    return { data, error: null };
  } catch (err: unknown) {
    const message = err instanceof Error ? err.message : String(err);
    return { data: null, error: message };
  }
}

export async function fetchWorkers(): Promise<{ data: WorkerStatusResponse | null; error: string | null }> {
  try {
    const res = await fetch(`${API_BASE_URL}/api/v1/workers`, { cache: "no-store" });
    if (!res.ok) {
      return { data: null, error: `HTTP ${res.status}` };
    }
    const data = (await res.json()) as WorkerStatusResponse;
    return { data, error: null };
  } catch (err: unknown) {
    const message = err instanceof Error ? err.message : String(err);
    return { data: null, error: message };
  }
}
