import { describe, expect, it } from "vitest";
import { nextReconnectDelay, parseSseBlock } from "../src/lib/sse";

describe("SSE parsing and reconnect policy", () => {
  it("uses exponential reconnect backoff capped at 30 seconds", () => {
    expect([0, 1, 2, 3, 4, 5, 8].map(nextReconnectDelay)).toEqual([
      1_000,
      2_000,
      4_000,
      8_000,
      16_000,
      30_000,
      30_000,
    ]);
  });

  it("parses the event id and typed platform event payload", () => {
    const parsed = parseSseBlock([
      "id: outbox-9",
      "event: task_run.state_changed.v1",
      'data: {"id":"outbox-9","type":"task_run.state_changed.v1","timestamp":"2026-10-25T00:00:00Z","source":"registry","data":{"run_id":"run-1"}}',
    ].join("\n"));

    expect(parsed.id).toBe("outbox-9");
    expect(parsed.event?.type).toBe("task_run.state_changed.v1");
    expect(parsed.event?.data.run_id).toBe("run-1");
  });

  it("rejects malformed event payloads without throwing", () => {
    expect(parseSseBlock("data: not-json").event).toBeNull();
    expect(parseSseBlock('data: {"type":"missing-required-fields"}').event).toBeNull();
  });
});
