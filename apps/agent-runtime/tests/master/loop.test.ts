import { describe, expect, it, vi } from "vitest";

import { startPollLoop } from "../../src/master/loop.js";
import type { IntervalScheduler } from "../../src/master/loop.js";

class ManualScheduler implements IntervalScheduler {
  private callback?: () => void;
  intervalMs?: number;
  cleared = false;
  set(callback: () => void, intervalMs: number): unknown {
    this.callback = callback;
    this.intervalMs = intervalMs;
    return "handle";
  }
  clear(): void {
    this.cleared = true;
  }
  fire(): void {
    this.callback?.();
  }
}

const flush = async (): Promise<void> => {
  for (let index = 0; index < 5; index += 1) await Promise.resolve();
};

describe("startPollLoop", () => {
  it("runs immediately, then on the configured interval, and stops cleanly", async () => {
    const scheduler = new ManualScheduler();
    const runOnce = vi.fn(async (): Promise<void> => {});
    const log = vi.fn();

    const loop = startPollLoop({ intervalMs: 30 * 60 * 1000, runOnce, log, scheduler });
    await flush();

    expect(scheduler.intervalMs).toBe(30 * 60 * 1000);
    expect(runOnce).toHaveBeenCalledTimes(1);

    scheduler.fire();
    await flush();
    expect(runOnce).toHaveBeenCalledTimes(2);

    loop.stop();
    expect(scheduler.cleared).toBe(true);
    scheduler.fire();
    await flush();
    expect(runOnce).toHaveBeenCalledTimes(2);
  });

  it("does not overlap ticks while a poll is still running", async () => {
    const scheduler = new ManualScheduler();
    const gate = Promise.withResolvers<void>();
    const runOnce = vi.fn(() => gate.promise);
    const log = vi.fn();

    const loop = startPollLoop({ intervalMs: 1000, runOnce, log, scheduler });
    scheduler.fire();
    await flush();

    expect(runOnce).toHaveBeenCalledTimes(1);
    expect(log).toHaveBeenCalledWith({ event: "jira_poll_overlap", reason: "previous tick still running" });

    gate.resolve();
    await flush();
    scheduler.fire();
    expect(runOnce).toHaveBeenCalledTimes(2);
    loop.stop();
  });

  it("keeps looping after a failed tick", async () => {
    const scheduler = new ManualScheduler();
    const runOnce = vi.fn(async (): Promise<void> => {
      throw new Error("jira down");
    });
    const log = vi.fn();

    const loop = startPollLoop({ intervalMs: 1000, runOnce, log, scheduler });
    await flush();
    expect(log).toHaveBeenCalledWith({ event: "jira_poll_error", error: "jira down" });

    scheduler.fire();
    await flush();
    expect(runOnce).toHaveBeenCalledTimes(2);
    loop.stop();
  });
});
