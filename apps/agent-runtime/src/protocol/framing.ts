import * as readline from "node:readline";
import type { Readable, Writable } from "node:stream";
import type { JsonRpcNotification, JsonRpcResponse } from "./types.js";

export class StdioChannel {
  private rl: readline.Interface;
  private output: Writable;

  constructor(input: Readable = process.stdin, output: Writable = process.stdout) {
    this.rl = readline.createInterface({
      input,
      crlfDelay: Infinity,
    });
    this.output = output;
  }

  onLine(callback: (line: string) => void): void {
    this.rl.on("line", (line) => {
      const trimmed = line.trim();
      if (trimmed.length > 0) {
        callback(trimmed);
      }
    });
  }

  onClose(callback: () => void): void {
    this.rl.on("close", callback);
  }

  sendResponse(response: JsonRpcResponse): void {
    this.output.write(JSON.stringify(response) + "\n");
  }

  sendNotification(notification: JsonRpcNotification): void {
    this.output.write(JSON.stringify(notification) + "\n");
  }

  close(): void {
    this.rl.close();
  }
}

export function logStderr(message: string, meta?: Record<string, unknown>): void {
  const timestamp = new Date().toISOString();
  const metaStr = meta ? ` ${JSON.stringify(meta)}` : "";
  process.stderr.write(`[${timestamp}] [agent-runtime] ${message}${metaStr}\n`);
}
