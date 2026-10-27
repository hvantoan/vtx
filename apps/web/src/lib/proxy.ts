import { NextRequest } from "next/server";

const INTERNAL_API_URL = (process.env.INTERNAL_API_URL ?? "http://localhost:49080").replace(/\/$/, "");

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
