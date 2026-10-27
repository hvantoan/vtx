import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  buildEventStreamRequest,
  fetchHealth,
  fetchReady,
  login,
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

function lastRequest() {
  const [url, init] = vi.mocked(fetch).mock.calls.at(-1) ?? [];
  return { url: String(url), init };
}

describe("operator API client", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
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

    expect(lastRequest().url).toBe("/ready");
    expect(health.data?.status).toBe("ok");
    expect(ready.data?.status).toBe("not_ready");
    expect(ready.error).toBeNull();
  });

  it("posts transitions to the same-origin path with the session cookie", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response({
      run_id: RUN_ID,
      task_id: RUN_ID,
      state: "blocked",
      version: 8,
      attempt: 1,
      lease_epoch: 2,
    }));

    await postTransition(RUN_ID, 7, "block", "operator review");

    const { url, init } = lastRequest();
    const headers = new Headers(init?.headers);
    expect(url).toBe(`/api/v1/registry/runs/${RUN_ID}/transitions`);
    expect(init?.credentials).toBe("include");
    expect(headers.get("Authorization")).toBeNull();
    expect(headers.get("X-Operator-Id")).toBeNull();
    expect(JSON.parse(String(init?.body))).toEqual({
      expected_version: 7,
      action: "block",
      reason: "operator review",
    });
  });

  it("redrives DLQ events and surfaces typed conflicts", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response({ error: "not_dead_lettered", message: "Already redriven" }, 409));

    const result = await redriveDlq(RUN_ID, "retry after dependency recovery");

    const { init } = lastRequest();
    expect(new Headers(init?.headers).get("Authorization")).toBeNull();
    expect(result.status).toBe(409);
    expect(result.error?.error).toBe("not_dead_lettered");
  });

  it("logs in with credentials only; the server owns the session token", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response({
      username: "operator",
      expires_at: "2026-11-26T00:00:00Z",
    }));

    const result = await login(" operator ", "correct horse battery staple");

    const { url, init } = lastRequest();
    expect(url).toBe("/api/v1/auth/login");
    expect(init?.method).toBe("POST");
    expect(init?.credentials).toBe("include");
    expect(JSON.parse(String(init?.body))).toEqual({
      username: "operator",
      password: "correct horse battery staple",
    });
    expect(result.data?.username).toBe("operator");
  });

  it("streams SSE from the same origin without credentials in the URL", () => {
    const request = buildEventStreamRequest(RUN_ID, "event-42");
    const url = new URL(request.url, "https://operator.example.test");
    expect(url.pathname).toBe("/api/v1/events");
    expect(url.searchParams.get("run_id")).toBe(RUN_ID);
    expect(url.searchParams.has("token")).toBe(false);
    expect(url.searchParams.has("auth")).toBe(false);

    const headers = new Headers(request.init.headers);
    expect(headers.get("Authorization")).toBeNull();
    expect(headers.get("Last-Event-ID")).toBe("event-42");
    expect(request.init.credentials).toBe("include");
  });
});
