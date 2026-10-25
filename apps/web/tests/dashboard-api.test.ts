import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  buildEventStreamRequest,
  fetchHealth,
  fetchReady,
  postTransition,
  redriveDlq,
} from "../src/lib/api";

const RUN_ID = "3f6a2c94-0d1b-4e58-9a7c-12df34ab56cd";

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("operator API client", () => {
  beforeEach(() => {
    vi.stubEnv("NEXT_PUBLIC_API_BASE_URL", "https://operator.example.test");
    vi.stubEnv("NEXT_PUBLIC_API_AUTH_TOKEN", "test-operator-token");
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.unstubAllEnvs();
  });

  it("reads liveness and accepts the readiness 503 contract", async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(response({ status: "ok", timestamp: "2026-10-25T00:00:00Z", uptime_seconds: 12 }))
      .mockResolvedValueOnce(response({
        status: "not_ready",
        timestamp: "2026-10-25T00:00:00Z",
        dependencies: { database: { status: "down", latency_ms: 1, error: "timeout" } },
      }, 503));

    const health = await fetchHealth();
    const ready = await fetchReady();

    expect(health.data?.status).toBe("ok");
    expect(ready.data?.status).toBe("not_ready");
    expect(ready.error).toBeNull();
  });

  it("sends authenticated transition mutations with the current version and reason", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response({
      run_id: RUN_ID,
      task_id: RUN_ID,
      state: "blocked",
      version: 8,
      attempt: 1,
      lease_epoch: 2,
    }));

    await postTransition(RUN_ID, 7, "block", "operator review");

    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? [];
    const headers = new Headers(init?.headers);
    expect(String(url)).toBe(`https://operator.example.test/api/v1/registry/runs/${RUN_ID}/transitions`);
    expect(headers.get("Authorization")).toBe("Bearer test-operator-token");
    expect(JSON.parse(String(init?.body))).toEqual({
      expected_version: 7,
      action: "block",
      reason: "operator review",
    });
  });

  it("authenticates DLQ redrive and surfaces typed conflicts", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response({ error: "not_dead_lettered", message: "Already redriven" }, 409));

    const result = await redriveDlq(RUN_ID, "retry after dependency recovery");

    const [, init] = vi.mocked(fetch).mock.calls[0] ?? [];
    expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer test-operator-token");
    expect(result.status).toBe(409);
    expect(result.error?.error).toBe("not_dead_lettered");
  });

  it("never places credentials in the SSE URL and resumes with Last-Event-ID", () => {
    const request = buildEventStreamRequest(RUN_ID, "event-42");
    expect(request).not.toBeNull();
    const url = new URL(request?.url ?? "https://invalid.test");
    expect(url.searchParams.get("run_id")).toBe(RUN_ID);
    expect(url.searchParams.has("token")).toBe(false);
    expect(url.searchParams.has("auth")).toBe(false);
    const headers = new Headers(request?.init.headers);
    expect(headers.get("Authorization")).toBe("Bearer test-operator-token");
    expect(headers.get("Last-Event-ID")).toBe("event-42");
  });
});
