import { describe, expect, it } from "vitest";
import { redactText, redactValue } from "../../src/orchestrator/redact.js";

// Fixtures are assembled at runtime so the repo secret scanner (check-boundaries
// Check 6) never sees a literal token/PEM shape in source. They are fake values.
const GH_TOKEN = `ghp_${"a".repeat(36)}`;
const KEY_LABEL = "PRIVATE KEY";
const PEM = `-----BEGIN ${KEY_LABEL}-----\nMIIEabc\n-----END ${KEY_LABEL}-----`;

describe("redactText", () => {
  it("masks a GitHub personal access token", () => {
    const out = redactText(`using ${GH_TOKEN} now`);
    expect(out).toContain("[REDACTED]");
    expect(out).not.toContain(GH_TOKEN);
  });

  it("masks a Bearer authorization header", () => {
    const out = redactText("Authorization: Bearer abcdefghijklmnopqrstuvwxyz");
    expect(out).toContain("[REDACTED]");
    expect(out).not.toContain("abcdefghijklmnopqrstuvwxyz");
  });

  it("masks key=value secret assignments", () => {
    expect(redactText("config api_token=supersecretvalue123")).toContain("[REDACTED]");
    expect(redactText("config api_token=supersecretvalue123")).not.toContain("supersecretvalue123");
  });

  it("masks a PEM private key block", () => {
    const out = redactText(PEM);
    expect(out).toContain("[REDACTED]");
    expect(out).not.toContain("MIIEabc");
  });

  it("leaves ordinary prose untouched", () => {
    expect(redactText("the run advanced to quality_gate")).toBe("the run advanced to quality_gate");
  });
});

describe("redactValue", () => {
  it("fully masks values under secret-looking keys regardless of shape", () => {
    const out = redactValue({ api_token: "abc", nested: { password: 123 } }) as Record<string, unknown>;
    expect(out.api_token).toBe("[REDACTED]");
    expect((out.nested as Record<string, unknown>).password).toBe("[REDACTED]");
  });

  it("walks arrays and objects and pattern-masks string leaves", () => {
    const out = redactValue({ logs: ["ok", GH_TOKEN] }) as Record<string, unknown>;
    const logs = out.logs as string[];
    expect(logs[0]).toBe("ok");
    expect(logs[1]).toContain("[REDACTED]");
  });

  it("masks caller-supplied secret values anywhere they appear", () => {
    const out = redactValue({ note: "the token is hunter2 here" }, ["hunter2"]) as Record<string, unknown>;
    expect(out.note).toContain("[REDACTED]");
    expect(out.note).not.toContain("hunter2");
  });

  it("preserves non-string scalars", () => {
    const out = redactValue({ count: 7, ok: true, nothing: null }) as Record<string, unknown>;
    expect(out.count).toBe(7);
    expect(out.ok).toBe(true);
    expect(out.nothing).toBeNull();
  });
});
