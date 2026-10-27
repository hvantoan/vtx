import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { proxyToApi } from "../src/lib/proxy";

describe("cockpit API proxy", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("ok")));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it.each([
    ["POST", "/api/v1/integrations/jira/webhooks"],
    ["GET", "/api/v1/integrations/jira/webhooks"],
    ["POST", "/api/v1/worktrees/allocate"],
    ["POST", "/api/v1/future-internal-api"],
    ["POST", "/api/v1/auth/me"],
    ["GET", "/api/v1/auth/login"],
    ["GET", "/api/v1/operator/runs/../../integrations/jira/webhooks"],
    ["GET", "/api/v1/registry/runs/%2F..%2F..%2Fintegrations%2Fjira%2Fwebhooks"],
  ])("rejects %s %s before calling the backend", async (method, path) => {
    const request = new NextRequest(`http://cockpit.test${path}`, {
      method,
      headers: { Authorization: "Bearer gateway-credential", Cookie: "vtx_session=browser-session" },
    });

    expect((await proxyToApi(request)).status).toBe(404);
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    ["GET", "/health"],
    ["GET", "/ready"],
    ["GET", "/api/v1/system/status"],
    ["GET", "/api/v1/workers"],
    ["GET", "/api/v1/auth/me"],
    ["GET", "/api/v1/operator/overview"],
    ["GET", "/api/v1/operator/workers"],
    ["GET", "/api/v1/operator/runs"],
    ["GET", "/api/v1/operator/runs/run-1"],
    ["GET", "/api/v1/registry/runs/run-1"],
    ["GET", "/api/v1/registry/runs/run-1/events"],
    ["GET", "/api/v1/registry/runs/run-1/pull_request"],
    ["GET", "/api/v1/operator/outbox"],
    ["GET", "/api/v1/operator/outbox/dlq"],
    ["GET", "/api/v1/pull_requests"],
    ["POST", "/api/v1/auth/login"],
    ["POST", "/api/v1/auth/logout"],
    ["POST", "/api/v1/auth/password"],
    ["POST", "/api/v1/registry/runs/run-1/transitions"],
    ["POST", "/api/v1/operator/outbox/dlq/event-1/redrive"],
    ["POST", "/api/v1/pull_requests/run-1/sync"],
  ])("forwards cockpit route %s %s", async (method, path) => {
    const request = new NextRequest(`http://cockpit.test${path}?limit=50`, { method });

    expect((await proxyToApi(request)).status).toBe(200);
    const [target] = vi.mocked(fetch).mock.calls[0];
    expect(new URL(String(target)).pathname).toBe(path);
    expect(new URL(String(target)).search).toBe("?limit=50");
  });

  it("preserves client identity, request bodies and renewed cookies", async () => {
    vi.mocked(fetch).mockResolvedValueOnce(new Response("ok", {
      headers: { "Set-Cookie": "vtx_session=renewed; Path=/; HttpOnly; Secure; SameSite=Lax" },
    }));
    const payload = JSON.stringify({ username: "operator", password: "test-password" });
    const request = new NextRequest("https://cockpit.test/api/v1/auth/login", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Forwarded-For": "192.0.2.1",
        Cookie: "vtx_session=browser-session",
      },
      body: payload,
    });

    const response = await proxyToApi(request);
    const [, init] = vi.mocked(fetch).mock.calls[0];
    const headers = new Headers(init?.headers);
    expect(headers.get("x-forwarded-for")).toBe("192.0.2.1");
    expect(headers.get("x-forwarded-proto")).toBe("https");
    expect(headers.get("cookie")).toBe("vtx_session=browser-session");
    expect(new TextDecoder().decode(init?.body as ArrayBuffer)).toBe(payload);
    expect(response.headers.getSetCookie()).toEqual([
      "vtx_session=renewed; Path=/; HttpOnly; Secure; SameSite=Lax",
    ]);
  });

  it("streams SSE without buffering and preserves cancellation", async () => {
    const body = new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("data: first\n\n"));
      },
    });
    vi.mocked(fetch).mockResolvedValueOnce(new Response(body, { headers: { "Content-Type": "text/event-stream" } }));
    const request = new NextRequest("http://cockpit.test/api/v1/events?run_id=run-1");

    const response = await proxyToApi(request);

    expect(response.body).toBe(body);
    expect(response.headers.get("x-accel-buffering")).toBe("no");
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(vi.mocked(fetch).mock.calls[0][1]?.signal).toBe(request.signal);
    const reader = response.body!.getReader();
    expect(new TextDecoder().decode((await reader.read()).value)).toBe("data: first\n\n");
    await reader.cancel();
  });

  it("returns a gateway error when an allowed upstream is unreachable", async () => {
    vi.mocked(fetch).mockRejectedValueOnce(new Error("connection refused"));
    expect((await proxyToApi(new NextRequest("http://cockpit.test/health"))).status).toBe(502);
  });
});
