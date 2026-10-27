import { NextRequest } from "next/server";

const INTERNAL_API_URL = (process.env.INTERNAL_API_URL ?? "http://localhost:49080").replace(/\/$/, "");

// Only the browser cockpit surface belongs here, never internal integrations
// or machine-only APIs. Match paths and methods before forwarding credentials.
const PUBLIC_ROUTES: Record<string, readonly RegExp[]> = {
  GET: [
    /^\/(?:health|ready)$/,
    /^\/api\/v1\/(?:system\/status|workers|events|auth\/me|pull_requests)$/,
    /^\/api\/v1\/operator\/(?:overview|workers|runs|outbox|outbox\/dlq)$/,
    /^\/api\/v1\/operator\/runs\/[A-Za-z0-9_-]+$/,
    /^\/api\/v1\/registry\/runs\/[A-Za-z0-9_-]+(?:\/(?:events|pull_request))?$/,
  ],
  POST: [
    /^\/api\/v1\/auth\/(?:login|logout|password)$/,
    /^\/api\/v1\/registry\/runs\/[A-Za-z0-9_-]+\/transitions$/,
    /^\/api\/v1\/operator\/outbox\/dlq\/[A-Za-z0-9_-]+\/redrive$/,
    /^\/api\/v1\/pull_requests\/[A-Za-z0-9_-]+\/sync$/,
  ],
};

const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);

const DROPPED_REQUEST_HEADERS = new Set([
  ...HOP_BY_HOP,
  "host",
  "content-length",
  // Undici negotiates and decompresses; forwarding the encoding would lie.
  "accept-encoding",
]);

const DROPPED_RESPONSE_HEADERS = new Set([
  ...HOP_BY_HOP,
  "content-encoding",
  "content-length",
  "connection",
]);

/**
 * Same-origin proxy for the Go API. Keeping this in a route handler (instead
 * of a next.config rewrite) lets INTERNAL_API_URL be a runtime value and
 * streams SSE bodies straight through without buffering.
 */
export async function proxyToApi(request: NextRequest): Promise<Response> {
  if (!PUBLIC_ROUTES[request.method]?.some((route) => route.test(request.nextUrl.pathname))) {
    return new Response("not found", { status: 404 });
  }
  const target = new URL(`${request.nextUrl.pathname}${request.nextUrl.search}`, INTERNAL_API_URL);

  const headers = new Headers();
  request.headers.forEach((value, key) => {
    if (!DROPPED_REQUEST_HEADERS.has(key.toLowerCase())) headers.set(key, value);
  });
  if (!headers.has("x-forwarded-proto")) {
    headers.set("x-forwarded-proto", request.nextUrl.protocol.replace(":", ""));
  }

  const hasBody = request.method !== "GET" && request.method !== "HEAD";
  const body = hasBody ? await request.arrayBuffer() : undefined;

  let upstream: Response;
  try {
    upstream = await fetch(target, {
      method: request.method,
      headers,
      body,
      redirect: "manual",
      cache: "no-store",
      signal: request.signal,
    });
  } catch {
    // An unreachable API must look like a gateway failure, not a Next crash.
    return new Response("upstream unavailable", { status: 502 });
  }

  const responseHeaders = new Headers();
  upstream.headers.forEach((value, key) => {
    if (!DROPPED_RESPONSE_HEADERS.has(key.toLowerCase()) && key.toLowerCase() !== "set-cookie") {
      responseHeaders.set(key, value);
    }
  });
  for (const cookie of upstream.headers.getSetCookie()) {
    responseHeaders.append("set-cookie", cookie);
  }
  if (request.nextUrl.pathname === "/api/v1/events") {
    responseHeaders.set("cache-control", "no-store");
    responseHeaders.set("x-accel-buffering", "no");
  }

  return new Response(upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: responseHeaders,
  });
}
