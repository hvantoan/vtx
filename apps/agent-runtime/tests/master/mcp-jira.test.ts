import { describe, expect, it } from "vitest";

import { normalizeIssue, normalizeSearchPayload } from "../../src/master/mcp-jira.js";

const rawIssue = {
  id: "10014",
  key: "VTX-5",
  summary: "Scheduler & Resource Limits",
  browse_url: "https://example.atlassian.net/browse/VTX-5",
  status: { name: "Ready To Develope", category: "To Do", color: "blue-gray" },
  issue_type: { name: "Task" },
  assignee: { account_id: "acc-1", display_name: "Dev", email: "dev@example.com" },
  updated: "2026-10-22 18:14:53 +07",
  statuscategorychangedate: "2026-10-22 18:14:53 +07",
};

describe("normalizeIssue", () => {
  it("maps the mcp-atlassian snake_case shape", () => {
    expect(normalizeIssue(rawIssue)).toEqual({
      id: "10014",
      key: "VTX-5",
      summary: "Scheduler & Resource Limits",
      status: "Ready To Develope",
      statusCategory: "To Do",
      issueType: "Task",
      updated: "2026-10-22 18:14:53 +07",
      statusChangedAt: "2026-10-22 18:14:53 +07",
      browseUrl: "https://example.atlassian.net/browse/VTX-5",
      assignee: { accountId: "acc-1", displayName: "Dev", email: "dev@example.com" },
    });
  });

  it("rejects entries without identity", () => {
    expect(normalizeIssue({ id: "1" })).toBeUndefined();
    expect(normalizeIssue({ key: "VTX-1" })).toBeUndefined();
    expect(normalizeIssue(null)).toBeUndefined();
  });

  it("falls back to fields.statuscategorychangedate", () => {
    const issue = normalizeIssue({
      id: "1",
      key: "VTX-1",
      status: "Ready To Develope",
      fields: { statuscategorychangedate: "2026-10-26T10:00:00.000Z" },
    });
    expect(issue?.statusChangedAt).toBe("2026-10-26T10:00:00.000Z");
  });
});

describe("normalizeSearchPayload", () => {
  it("normalizes the issues array", () => {
    const page = normalizeSearchPayload({ total: 1, issues: [rawIssue, { id: "x" }] });
    expect(page.total).toBe(1);
    expect(page.issues.map((issue) => issue.key)).toEqual(["VTX-5"]);
  });

  it("fails loudly when the array is missing", () => {
    expect(() => normalizeSearchPayload({})).toThrow(/issues array/);
  });

  it("unwraps the mcp-atlassian { result: json } envelope", () => {
    const page = normalizeSearchPayload({ result: JSON.stringify({ total: -1, issues: [rawIssue] }) });
    expect(page.issues.map((issue) => issue.key)).toEqual(["VTX-5"]);
    expect(page.total).toBeUndefined();
  });
});
