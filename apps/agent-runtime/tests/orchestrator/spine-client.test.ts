import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import net from "node:net";
import { mkdtemp, rm } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { SpineClient } from "../../src/orchestrator/spine-client.js";
import { SpineError, isSpineResponse } from "../../src/orchestrator/protocol.js";
import type { SpineRequest } from "../../src/orchestrator/protocol.js";

const TOKEN = "test-spine-token";

type Responder = (req: SpineRequest) => SpineResponse | null;

let dir: string;
let socketPath: string;
let server: net.Server | undefined;

/** Start a unix-socket spine stub that answers each request via `responder`. */
function startStub(responder: Responder): Promise<void> {
  server = net.createServer((conn) => {
    let buffer = "";
    conn.setEncoding("utf8");
    conn.on("data", (chunk: string) => {
      buffer += chunk;
      let nl = buffer.indexOf("\n");
      while (nl !== -1) {
        const line = buffer.slice(0, nl).trim();
        buffer = buffer.slice(nl + 1);
        nl = buffer.indexOf("\n");
        if (!line) continue;
        const req = JSON.parse(line) as SpineRequest;
        const resp = responder(req);
        if (resp) conn.write(`${JSON.stringify(resp)}\n`);
      }
    });
  });
  return new Promise((resolve) => server.listen(socketPath, () => resolve()));
}

beforeEach(async () => {
  dir = await mkdtemp(path.join(os.tmpdir(), "vtx-spine-"));
  socketPath = path.join(dir, "spine.sock");
});

afterEach(async () => {
  if (server) {
    const s = server;
    server = undefined;
    await new Promise<void>((resolve) => s.close(() => resolve()));
  }
  await rm(dir, { recursive: true, force: true });
});

function newClient(overrides: Partial<ConstructorParameters<typeof SpineClient>[0]> = {}): SpineClient {
  return new SpineClient({ socketPath, token: TOKEN, requestTimeoutMs: 60_000, ...overrides });
}

describe("SpineClient", () => {
  it("rejects a call made before connect", async () => {
    const client = newClient();
    await expect(client.call("verify", { runId: "r", leaseEpoch: 1 })).rejects.toThrow(/not connected/);
    client.close();
  });

  it("fails closed when the socket path does not exist", async () => {
    const client = new SpineClient({ socketPath: path.join(dir, "missing.sock"), token: TOKEN, connectTimeoutMs: 500 });
    await expect(client.connect()).rejects.toBeInstanceOf(SpineError);
    client.close();
  });

  it("round-trips a request and resolves the spine result", async () => {
    await startStub((req) => ({ id: req.id, ok: true, tool: req.tool, result: { decision: "pass", seen_token: req.token } }));
    const client = newClient();
    await client.connect();
    const out = (await client.call("gate", { runId: "run-1", leaseEpoch: 9 })) as Record<string, unknown>;
    expect(out.decision).toBe("pass");
    expect(out.seen_token).toBe(TOKEN);
    client.close();
  });

  it("sends run_id and lease_epoch at the top level", async () => {
    let captured: SpineRequest | undefined;
    await startStub((req) => {
      captured = req;
      return { id: req.id, ok: true, tool: req.tool, result: {} };
    });
    const client = newClient();
    await client.connect();
    await client.call("advance", { runId: "run-7", leaseEpoch: 4, args: { expected_version: 2 } });
    expect(captured?.run_id).toBe("run-7");
    expect(captured?.lease_epoch).toBe(4);
    expect(captured?.args).toEqual({ expected_version: 2 });
    client.close();
  });

  it("surfaces a non-ok response as a typed SpineError with its code", async () => {
    await startStub((req) => ({ id: req.id, ok: false, tool: req.tool, error: { code: "tool_unavailable", message: "nope" } }));
    const client = newClient();
    await client.connect();
    await expect(client.call("open_pr", { runId: "r", leaseEpoch: 1 })).rejects.toMatchObject({ code: "tool_unavailable" });
    client.close();
  });

  it("reassembles a response split across chunks", async () => {
    server = net.createServer((conn) => {
      let buffer = "";
      conn.setEncoding("utf8");
      conn.on("data", (chunk: string) => {
        buffer += chunk;
        const nl = buffer.indexOf("\n");
        if (nl === -1) return;
        const req = JSON.parse(buffer.slice(0, nl).trim()) as SpineRequest;
        buffer = buffer.slice(nl + 1);
        const resp = JSON.stringify({ id: req.id, ok: true, tool: req.tool, result: { split: true } });
        conn.write(resp.slice(0, 12));
        conn.write(`${resp.slice(12)}\n`);
      });
    });
    await new Promise<void>((resolve) => server.listen(socketPath, () => resolve()));
    const client = newClient();
    await client.connect();
    const out = (await client.call("verify", { runId: "r", leaseEpoch: 1 })) as Record<string, unknown>;
    expect(out.split).toBe(true);
    client.close();
  });

  it("rejects pending calls when the client is closed", async () => {
    await startStub(() => null); // never responds
    const client = newClient();
    await client.connect();
    const pending = client.call("verify", { runId: "r", leaseEpoch: 1 });
    client.close();
    await expect(pending).rejects.toBeInstanceOf(SpineError);
  });

  it("close is idempotent", async () => {
    await startStub((req) => ({ id: req.id, ok: true, tool: req.tool, result: {} }));
    const client = newClient();
    await client.connect();
    client.close();
    expect(() => client.close()).not.toThrow();
    expect(client.isConnected).toBe(false);
  });

  it("gives a slow tool its own timeout instead of the fast RPC default", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      await startStub(() => null); // never responds
      const client = new SpineClient({
        socketPath,
        token: TOKEN,
        requestTimeoutMs: 30_000,
        toolTimeouts: { verify: 600_000 },
      });
      await client.connect();

      const verify = client.call("verify", { runId: "r", leaseEpoch: 1 });
      const gate = client.call("gate", { runId: "r", leaseEpoch: 1 });
      // Attach both handlers before advancing so neither rejection is unhandled.
      const verifyOutcome = expect(verify).rejects.toThrow(/verify timed out/);
      const gateOutcome = expect(gate).rejects.toThrow(/gate timed out/);

      // The fast default still fires for every other tool.
      await vi.advanceTimersByTimeAsync(30_000);
      await gateOutcome;

      // The long-running tool outlives it and only fails at its own deadline.
      await vi.advanceTimersByTimeAsync(569_999);
      const settled = await Promise.race([verifyOutcome.then(() => "rejected"), Promise.resolve("pending")]);
      expect(settled).toBe("pending");

      await vi.advanceTimersByTimeAsync(1);
      await verifyOutcome;
      client.close();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("isSpineResponse", () => {
  it("accepts a well-formed frame and rejects junk", () => {
    expect(isSpineResponse({ id: "1", ok: true, tool: "gate" })).toBe(true);
    expect(isSpineResponse({ id: "1", tool: "gate" })).toBe(false);
    expect(isSpineResponse(null)).toBe(false);
    expect(isSpineResponse("nope")).toBe(false);
  });
});
