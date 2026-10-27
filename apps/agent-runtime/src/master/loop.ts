export interface IntervalScheduler {
  set(callback: () => void, intervalMs: number): unknown;
  clear(handle: unknown): void;
}

export const nodeIntervalScheduler: IntervalScheduler = {
  set(callback, intervalMs) {
    const handle = setInterval(callback, intervalMs);
    handle.unref?.();
    return handle;
  },
  clear(handle) {
    clearInterval(handle as NodeJS.Timeout);
  },
};

export interface PollLoop {
  stop(): void;
}

export interface StartPollLoopOptions {
  intervalMs: number;
  runOnce: () => Promise<void>;
  log: (event: Record<string, unknown>) => void;
  scheduler?: IntervalScheduler;
}

export function startPollLoop(options: StartPollLoopOptions): PollLoop {
  const scheduler = options.scheduler ?? nodeIntervalScheduler;
  let running = false;
  let stopped = false;

  const tick = (): void => {
    if (stopped) return;
    if (running) {
      options.log({ event: "jira_poll_overlap", reason: "previous tick still running" });
      return;
    }
    running = true;
    void options
      .runOnce()
      .catch((error: unknown) => {
        options.log({
          event: "jira_poll_error",
          error: error instanceof Error ? error.message : String(error),
        });
      })
      .finally(() => {
        running = false;
      });
  };

  tick();
  const handle = scheduler.set(tick, options.intervalMs);
  return {
    stop() {
      stopped = true;
      scheduler.clear(handle);
    },
  };
}
