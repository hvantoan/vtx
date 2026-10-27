import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  drainQueue,
  queueCursorPath,
  spineIntakeDeliver,
} from "../../src/master/delivery.js";
import type { QueueCursor, SpineCaller } from "../../src/master/delivery.js";
import type { QueueEntry } from "../../src/master/state.js";

function entry(key: string, version: string): QueueEntry {
  return {
    discoveredAt: "2026-10-27T00:00:00.000Z",
    version,
    issue: {
      id: key.split("-")[1] ?? "1",
      key,
      summary: `issue ${key}`,
      status: "Ready To Develope",
      updated: version,
    },
  };
}

async function writeQueue(path: string, entries: QueueEntry[]): Promise<void> {
  await writeFile(path, entries.map((e) => JSON.stringify(e)).join("\n") + "\n", "utf8");
}

describe("drainQueue", () => {
  let dir: string;
  let queueFile: string;
  let cursorFile: string;
  const log = vi.fn();

  beforeEach(async () => {
    dir = await mkdtemp(join(tmpdir(), "vtx-queue-"));
    queueFile = join(dir, "jira-queue.jsonl");
    cursorFile = queueCursorPath(dir);
    log.mockClear();
  });

  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  it("delivers in order and advances the cursor", async () => {
    await writeQueue(queueFile, [entry("VTX-1", "v1"), entry("VTX-2", "v2")]);
    const deliver = vi.fn(async (e: QueueEntry) => ({ disposition: "accepted", issueKey: e.issue.key, intentId: `i-${e.issue.key}` }));

    const result = await drainQueue({ queuePath: queueFile, cursorPath: cursorFile, deliver, log, now: () => new Date("2026-10-27T00:00:00Z") });
    expect(result).toEqual({ delivered: 2, failed: 0, remaining: 0 });
    expect(deliver.mock.calls.map((c) => c[0].issue.key)).toEqual(["VTX-1", "VTX-2"]);

    const cursor = JSON.parse(await readFile(cursorFile, "utf8")) as QueueCursor;
    expect(cursor.nextLine).toBe(2);

    // A second drain is a no-op: the cursor is the handoff boundary.
    const again = await drainQueue({ queuePath: queueFile, cursorPath: cursorFile, deliver, log, now: () => new Date() });
    expect(again).toEqual({ delivered: 0, failed: 0, remaining: 0 });
    expect(deliver).toHaveBeenCalledTimes(2);
  });

  it("stops on failure and retries the same line next drain", async () => {
    await writeQueue(queueFile, [entry("VTX-1", "v1"), entry("VTX-2", "v2"), entry("VTX-3", "v3")]);
    const deliver = vi
      .fn<(e: QueueEntry) => Promise<{ disposition: string; issueKey: string }>>()
      .mockResolvedValueOnce({ disposition: "accepted", issueKey: "VTX-1" })
      .mockRejectedValueOnce(new Error("spine unreachable"))
      .mockResolvedValue({ disposition: "accepted", issueKey: "VTX-2" });

    const first = await drainQueue({ queuePath: queueFile, cursorPath: cursorFile, deliver, log, now: () => new Date() });
    expect(first).toEqual({ delivered: 1, failed: 1, remaining: 2 });
    expect(log).toHaveBeenCalledWith(expect.objectContaining({ event: "jira_intake_error", issue_key: "VTX-2" }));

    const cursor = JSON.parse(await readFile(cursorFile, "utf8")) as QueueCursor;
    expect(cursor.nextLine).toBe(1);

    const second = await drainQueue({ queuePath: queueFile, cursorPath: cursorFile, deliver, log, now: () => new Date() });
    expect(second).toEqual({ delivered: 2, failed: 0, remaining: 0 });
    expect(deliver.mock.calls.map((c) => c[0].issue.key)).toEqual(["VTX-1", "VTX-2", "VTX-2", "VTX-3"]);
  });

  it("skips corrupt lines instead of poisoning the queue", async () => {
    await writeFile(queueFile, `${JSON.stringify(entry("VTX-1", "v1"))}\n{not json}\n${JSON.stringify(entry("VTX-3", "v3"))}\n`, "utf8");
    const deliver = vi.fn(async (e: QueueEntry) => ({ disposition: "accepted", issueKey: e.issue.key }));

    const result = await drainQueue({ queuePath: queueFile, cursorPath: cursorFile, deliver, log, now: () => new Date() });
    expect(result.delivered).toBe(2);
    expect(log).toHaveBeenCalledWith(expect.objectContaining({ event: "jira_queue_corrupt", line: 2 }));
  });

  it("returns zero when the queue does not exist", async () => {
    const result = await drainQueue({ queuePath: join(dir, "missing.jsonl"), cursorPath: cursorFile, deliver: vi.fn(), log, now: () => new Date() });
    expect(result).toEqual({ delivered: 0, failed: 0, remaining: 0 });
  });
});

describe("spineIntakeDeliver", () => {
  it("sends the unscoped intake args and maps the result", async () => {
    const call = vi.fn(async () => ({ disposition: "accepted", intent_id: "intent-9", issue_key: "VTX-5" }));
    const client: SpineCaller = { call, close: vi.fn() };

    const outcome = await spineIntakeDeliver(client)(entry("VTX-5", "v1"));
    expect(outcome).toEqual({ disposition: "accepted", issueKey: "VTX-5", intentId: "intent-9", reasonCode: undefined });
    expect(call).toHaveBeenCalledWith("intake", {
      args: { issue_id: "5", issue_key: "VTX-5", version: "v1", correlation_id: "poll:VTX-5:v1" },
    });
  });

  it("fails closed on a malformed spine result", async () => {
    const client: SpineCaller = { call: vi.fn(async () => ({})), close: vi.fn() };
    await expect(spineIntakeDeliver(client)(entry("VTX-5", "v1"))).rejects.toThrow(/no disposition/);
  });
});
