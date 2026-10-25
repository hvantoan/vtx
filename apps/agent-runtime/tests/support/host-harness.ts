import { PassThrough } from "node:stream";
import { mkdtemp } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import type { AgentAdapter } from "../../src/adapters/types.js";
import { AgentHost } from "../../src/host.js";
import type { AgentHostOptions } from "../../src/host.js";
import { StdioChannel } from "../../src/protocol/framing.js";
import type { JsonRpcNotification, JsonRpcResponse, RequestId, TaskEventParams } from "../../src/protocol/types.js";

export type WireMessage = JsonRpcResponse | JsonRpcNotification;

function isNotification(message: WireMessage): message is JsonRpcNotification {
  return "method" in message;
}

/** In-memory NDJSON pipe around an AgentHost; waits are promise-based (no timers). */
export function createHostHarness(adapters: AgentAdapter[], options: Omit<AgentHostOptions, "adapters"> = {}) {
  const input = new PassThrough();
  const output = new PassThrough();
  const exits: number[] = [];
  const host = new AgentHost(new StdioChannel(input, output), {
    ...options,
    adapters,
    onExit: (code) => exits.push(code),
  });

  const messages: WireMessage[] = [];
  const waiters: Array<{ predicate: (m: WireMessage) => boolean; resolve: (m: WireMessage) => void }> = [];
  let buffered = "";
  output.on("data", (chunk: Buffer) => {
    buffered += chunk.toString();
    let newline = buffered.indexOf("\n");
    while (newline >= 0) {
      const line = buffered.slice(0, newline);
      buffered = buffered.slice(newline + 1);
      newline = buffered.indexOf("\n");
      if (!line) continue;
      const message = JSON.parse(line) as WireMessage;
      messages.push(message);
      for (const waiter of [...waiters]) {
        if (waiter.predicate(message)) {
          waiters.splice(waiters.indexOf(waiter), 1);
          waiter.resolve(message);
        }
      }
    }
  });

  const waitForMessage = (predicate: (m: WireMessage) => boolean): Promise<WireMessage> => {
    const existing = messages.find(predicate);
    if (existing) return Promise.resolve(existing);
    const { promise, resolve } = Promise.withResolvers<WireMessage>();
    waiters.push({ predicate, resolve });
    return promise;
  };

  const sendRaw = (line: string): void => {
    input.write(line + "\n");
  };
  const send = (message: unknown): void => sendRaw(JSON.stringify(message));

  const waitForResponse = async (id: RequestId): Promise<JsonRpcResponse> => {
    const message = await waitForMessage((m) => !isNotification(m) && m.id === id);
    return message as JsonRpcResponse;
  };

  const request = async (id: RequestId, method: string, params?: unknown): Promise<JsonRpcResponse> => {
    send({ jsonrpc: "2.0", id, method, params });
    return waitForResponse(id);
  };

  const notifications = (method: string): JsonRpcNotification[] =>
    messages.filter(isNotification).filter((m) => m.method === method);

  const events = (): TaskEventParams[] => notifications("task/event").map((m) => m.params as TaskEventParams);

  const initialize = (params: Record<string, unknown> = {}): Promise<JsonRpcResponse> =>
    request(1, "initialize", { protocol_version: "1.1", client_name: "test-supervisor", ...params });

  return { host, input, send, sendRaw, request, waitForResponse, waitForMessage, messages, notifications, events, initialize, exits };
}

export async function makeTempDir(prefix: string): Promise<string> {
  return mkdtemp(path.join(os.tmpdir(), prefix));
}
