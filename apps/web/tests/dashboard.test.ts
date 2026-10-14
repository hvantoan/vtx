import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fetchHealth, fetchReady, fetchSystemStatus, fetchWorkers } from "../src/lib/api";

describe("Web Dashboard API Client", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("fetchHealth handles success response", async () => {
    const mockHealth = {
      status: "ok",
      timestamp: "2026-10-14T00:00:00Z",
      uptime_seconds: 120.5,
    };

    vi.mocked(fetch).mockResolvedValueOnce(
      new Response(JSON.stringify(mockHealth), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      })
    );

    const res = await fetchHealth();
    expect(res.data?.status).toBe("ok");
    expect(res.data?.uptime_seconds).toBe(120.5);
    expect(res.error).toBeNull();
  });

  it("fetchHealth handles network error gracefully", async () => {
    vi.mocked(fetch).mockRejectedValueOnce(new Error("Network connection refused"));

    const res = await fetchHealth();
    expect(res.data).toBeNull();
    expect(res.error).toContain("Network connection refused");
  });

  it("fetchReady handles 503 degraded status gracefully", async () => {
    const mockReady = {
      status: "not_ready",
      timestamp: "2026-10-14T00:00:00Z",
      dependencies: {
        database: { status: "down", latency_ms: 0, error: "timeout" },
        temporal: { status: "up", latency_ms: 5 },
      },
    };

    vi.mocked(fetch).mockResolvedValueOnce(
      new Response(JSON.stringify(mockReady), {
        status: 503,
        headers: { "Content-Type": "application/json" },
      })
    );

    const res = await fetchReady();
    expect(res.data?.status).toBe("not_ready");
    expect(res.data?.dependencies.database?.status).toBe("down");
    expect(res.error).toBeNull();
  });

  it("fetchSystemStatus handles valid response", async () => {
    const mockSystem = {
      version: "1.0.0",
      commit: "abcdef1",
      environment: "development",
      go_version: "go1.25.7",
      goroutines: 12,
      uptime_seconds: 400,
      timestamp: "2026-10-14T00:00:00Z",
    };

    vi.mocked(fetch).mockResolvedValueOnce(
      new Response(JSON.stringify(mockSystem), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      })
    );

    const res = await fetchSystemStatus();
    expect(res.data?.go_version).toBe("go1.25.7");
    expect(res.error).toBeNull();
  });

  it("fetchWorkers handles valid response", async () => {
    const mockWorkers = {
      status: "healthy",
      workers: [
        {
          id: "worker-1",
          queue: "vtx-task-queue",
          status: "running",
          active_workflows: 0,
        },
      ],
    };

    vi.mocked(fetch).mockResolvedValueOnce(
      new Response(JSON.stringify(mockWorkers), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      })
    );

    const res = await fetchWorkers();
    expect(res.data?.workers).toHaveLength(1);
    expect(res.data?.workers[0]?.queue).toBe("vtx-task-queue");
    expect(res.error).toBeNull();
  });
});
