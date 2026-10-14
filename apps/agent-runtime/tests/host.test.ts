import { describe, it, expect } from "vitest";
import { PassThrough } from "node:stream";
import { StdioChannel } from "../src/protocol/framing.js";
import { AgentHost } from "../src/host.js";
import type { JsonRpcNotification, JsonRpcResponse } from "../src/protocol/types.js";

function setupTestHost() {
  const inputStream = new PassThrough();
  const outputStream = new PassThrough();
  const channel = new StdioChannel(inputStream, outputStream);
  const host = new AgentHost(channel);

  const responses: JsonRpcResponse[] = [];
  const notifications: JsonRpcNotification[] = [];

  const responseWaiters = new Map<string | number, (res: JsonRpcResponse) => void>();

  outputStream.on("data", (chunk: Buffer) => {
    const lines = chunk.toString().trim().split("\n");
    for (const line of lines) {
      if (!line) continue;
      const parsed = JSON.parse(line);
      if ("method" in parsed) {
        notifications.push(parsed as JsonRpcNotification);
      } else {
        const resp = parsed as JsonRpcResponse;
        responses.push(resp);
        if (resp.id !== null && responseWaiters.has(resp.id)) {
          const resolve = responseWaiters.get(resp.id)!;
          responseWaiters.delete(resp.id);
          resolve(resp);
        }
      }
    }
  });

  const send = (msg: unknown) => {
    inputStream.write(JSON.stringify(msg) + "\n");
  };

  const waitForResponse = (id: string | number): Promise<JsonRpcResponse> => {
    const existing = responses.find((r) => r.id === id);
    if (existing) {
      return Promise.resolve(existing);
    }
    const { promise, resolve } = Promise.withResolvers<JsonRpcResponse>();
    responseWaiters.set(id, resolve);
    return promise;
  };

  return { host, send, responses, notifications, waitForResponse };
}

describe("AgentHost Protocol Suite", () => {
  it("completes initialize handshake for Pi", async () => {
    const { send, waitForResponse } = setupTestHost();

    send({
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: {
        protocol_version: "1.0",
        client_name: "test-client",
        agent_type: "pi",
      },
    });

    const resp = await waitForResponse(1);
    expect(resp.id).toBe(1);
    expect(resp.result).toMatchObject({
      protocol_version: "1.0",
      agent_type: "pi",
    });
  });

  it("completes initialize handshake for OMP", async () => {
    const { send, waitForResponse } = setupTestHost();

    send({
      jsonrpc: "2.0",
      id: 2,
      method: "initialize",
      params: {
        protocol_version: "1.0",
        client_name: "test-client",
        agent_type: "omp",
      },
    });

    const resp = await waitForResponse(2);
    expect(resp.result).toMatchObject({
      agent_type: "omp",
    });
  });

  it("executes task and emits streaming events", async () => {
    const { send, waitForResponse, notifications } = setupTestHost();

    send({
      jsonrpc: "2.0",
      id: 10,
      method: "task/execute",
      params: {
        task_id: "task-abc",
        prompt: "Refactor database module",
      },
    });

    const resp = await waitForResponse(10);
    expect(resp.result).toMatchObject({
      task_id: "task-abc",
      status: "succeeded",
    });

    expect(notifications.length).toBeGreaterThanOrEqual(2);
    expect(notifications[0]?.method).toBe("task/event");
  });

  it("cancels running task on task/cancel", async () => {
    const { send, waitForResponse } = setupTestHost();

    send({
      jsonrpc: "2.0",
      id: 20,
      method: "task/execute",
      params: {
        task_id: "task-cancel-me",
        prompt: "Long running prompt",
      },
    });

    send({
      jsonrpc: "2.0",
      id: 21,
      method: "task/cancel",
      params: {
        task_id: "task-cancel-me",
        reason: "User requested cancel",
      },
    });

    const resp = await waitForResponse(20);
    expect(resp.result).toMatchObject({
      status: "cancelled",
    });
  });

  it("returns error for unknown method", async () => {
    const { send, waitForResponse } = setupTestHost();

    send({
      jsonrpc: "2.0",
      id: 99,
      method: "non_existent_method",
      params: {},
    });

    const resp = await waitForResponse(99);
    expect(resp.error).toBeDefined();
    expect(resp.error?.code).toBe(-32601);
  });
});
