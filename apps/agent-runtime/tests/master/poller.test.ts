import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

import { pollOnce } from "../../src/master/poller.js";
import { queuePath, statePath } from "../../src/master/state.js";
import type { JiraIssue, JiraSearchPage, JiraSource } from "../../src/master/types.js";

function issue(key: string, version: string): JiraIssue {
  return {
    id: `id-${key}`,
    key,
    summary: `summary ${key}`,
    status: "Ready To Develope",
    updated: version,
    statusChangedAt: version,
  };
}

class FakeSource implements JiraSource {
  readonly starts: number[] = [];
  constructor(private readonly pages: JiraSearchPage[]) {}
  async search(_jql: string, _limit: number, startAt: number): Promise<JiraSearchPage> {
    this.starts.push(startAt);
    return this.pages[this.starts.length - 1] ?? { issues: [] };
  }
  async close(): Promise<void> {}
}

const logs: Record<string, unknown>[] = [];
const log = (event: Record<string, unknown>): void => {
  logs.push(event);
};

let dirs: string[] = [];
afterEach(async () => {
  await Promise.all(dirs.map((dir) => rm(dir, { recursive: true, force: true })));
  dirs = [];
  logs.length = 0;
});

async function stateDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), "vtx-master-"));
  dirs.push(dir);
  return dir;
}

const now = (): Date => new Date("2026-10-27T03:00:00.000Z");

async function queueEntries(dir: string): Promise<{ version: string; issue: JiraIssue }[]> {
  const text = await readFile(queuePath(dir), "utf8");
  return text
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line) as { version: string; issue: JiraIssue });
}

describe("pollOnce", () => {
  it("baselines an empty state without queueing historical issues", async () => {
    const dir = await stateDir();
    const source = new FakeSource([{ issues: [issue("VTX-1", "v1"), issue("VTX-2", "v1")] }]);

    const result = await pollOnce({ source, stateDir: dir, jql: "jql", pageSize: 50, baseline: true, now, log });

    expect(result).toEqual({ fetched: 2, queued: 0, skipped: 2, baseline: true });
    await expect(readFile(queuePath(dir), "utf8")).rejects.toMatchObject({ code: "ENOENT" });
    const state = JSON.parse(await readFile(statePath(dir), "utf8")) as { baselineDone: boolean; seen: Record<string, unknown> };
    expect(state.baselineDone).toBe(true);
    expect(Object.keys(state.seen)).toEqual(["VTX-1", "VTX-2"]);
    expect(logs[0]?.event).toBe("jira_poll_baseline");
  });

  it("queues only issues whose status-change version advanced", async () => {
    const dir = await stateDir();
    await pollOnce({
      source: new FakeSource([{ issues: [issue("VTX-1", "v1"), issue("VTX-2", "v1")] }]),
      stateDir: dir,
      jql: "jql",
      pageSize: 50,
      baseline: true,
      now,
      log,
    });

    const second = await pollOnce({
      source: new FakeSource([{ issues: [issue("VTX-1", "v1"), issue("VTX-2", "v2")] }]),
      stateDir: dir,
      jql: "jql",
      pageSize: 50,
      baseline: true,
      now,
      log,
    });

    expect(second).toEqual({ fetched: 2, queued: 1, skipped: 1, baseline: false });
    const entries = await queueEntries(dir);
    expect(entries).toHaveLength(1);
    expect(entries[0]?.issue.key).toBe("VTX-2");
    expect(entries[0]?.version).toBe("v2");
  });

  it("polls every matching issue when baseline is disabled", async () => {
    const dir = await stateDir();
    const source = new FakeSource([{ issues: [issue("VTX-1", "v1")] }]);

    const result = await pollOnce({ source, stateDir: dir, jql: "jql", pageSize: 50, baseline: false, now, log });

    expect(result).toEqual({ fetched: 1, queued: 1, skipped: 0, baseline: false });
    expect(await queueEntries(dir)).toHaveLength(1);
  });

  it("paginates until a short page arrives", async () => {
    const dir = await stateDir();
    const source = new FakeSource([
      { issues: [issue("VTX-1", "v1"), issue("VTX-2", "v1")] },
      { issues: [issue("VTX-3", "v1")] },
    ]);

    const result = await pollOnce({ source, stateDir: dir, jql: "jql", pageSize: 2, baseline: false, now, log });

    expect(result.fetched).toBe(3);
    expect(source.starts).toEqual([0, 2]);
  });
});
