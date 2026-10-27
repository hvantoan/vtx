import { readFile } from "node:fs/promises";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

import type { JiraAssignee, JiraIssue, JiraSearchPage, JiraSource } from "./types.js";

const SEARCH_TOOL = "jira_search";
/**
 * `statuscategorychangedate` is the best proxy for "entered this status".
 * mcp-atlassian falls back gracefully when the tenant does not expose it.
 */
const SEARCH_FIELDS = "summary,status,updated,issuetype,assignee,statuscategorychangedate";
const CALL_TIMEOUT_MS = 60_000;

export interface McpServerConfig {
  command: string;
  args?: string[];
  env?: Record<string, string>;
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  return value as Record<string, unknown>;
}

function asString(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

function asNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function textOf(result: unknown): string {
  const content = asRecord(result)?.content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) => {
      const record = asRecord(part);
      return record?.type === "text" && typeof record.text === "string" ? record.text : "";
    })
    .join("\n");
}

function extractPayload(result: unknown): unknown {
  const record = asRecord(result);
  if (!record) throw new Error("jira_search returned an invalid response");
  if (record.structuredContent !== undefined && record.structuredContent !== null) {
    return record.structuredContent;
  }
  const text = textOf(result).trim();
  if (text.length === 0) {
    // Compatibility result shape used by older MCP servers.
    if (record.toolResult !== undefined) return record.toolResult;
    throw new Error("jira_search returned no content");
  }
  return JSON.parse(text);
}

function normalizeAssignee(value: unknown): JiraAssignee | undefined {
  const raw = asRecord(value);
  if (!raw) return undefined;
  const assignee: JiraAssignee = {
    accountId: asString(raw.account_id),
    displayName: asString(raw.display_name) ?? asString(raw.name),
    email: asString(raw.email),
  };
  return assignee.accountId || assignee.displayName || assignee.email ? assignee : undefined;
}

export function normalizeIssue(value: unknown): JiraIssue | undefined {
  const raw = asRecord(value);
  if (!raw) return undefined;
  const id = asString(raw.id);
  const key = asString(raw.key);
  if (!id || !key) return undefined;

  const status = asRecord(raw.status);
  const issueType = asRecord(raw.issue_type) ?? asRecord(raw.issuetype);
  const fields = asRecord(raw.fields);

  return {
    id,
    key,
    summary: asString(raw.summary) ?? "",
    status: asString(status?.name) ?? asString(raw.status) ?? "unknown",
    statusCategory: asString(status?.category) ?? asString(raw.status_category),
    issueType: asString(issueType?.name),
    updated: asString(raw.updated) ?? "",
    statusChangedAt:
      asString(raw.statuscategorychangedate) ??
      asString(raw.status_category_changed_date) ??
      asString(fields?.statuscategorychangedate),
    browseUrl: asString(raw.browse_url),
    assignee: normalizeAssignee(raw.assignee),
  };
}

/**
 * mcp-atlassian returns the search JSON nested as `{ result: "<json string>" }`
 * inside `structuredContent`. Unwrap either representation.
 */
function unwrapSearchResult(value: unknown): unknown {
  if (typeof value !== "string") return value;
  try {
    return JSON.parse(value);
  } catch {
    return value;
  }
}

export function normalizeSearchPayload(payload: unknown): JiraSearchPage {
  let raw = asRecord(payload);
  if (raw && !Array.isArray(raw.issues) && raw.result !== undefined) {
    raw = asRecord(unwrapSearchResult(raw.result));
  }
  const values = raw?.issues;
  if (!Array.isArray(values)) {
    throw new Error("jira_search response is missing the issues array");
  }
  const issues = values
    .map((value) => normalizeIssue(value))
    .filter((issue): issue is JiraIssue => issue !== undefined);
  const total = asNumber(raw?.total);
  // mcp-atlassian reports -1 when the total is not computed.
  return total === undefined || total < 0 ? { issues } : { issues, total };
}

export async function loadMcpServerConfig(path: string, serverName: string): Promise<McpServerConfig> {
  const parsed: unknown = JSON.parse(await readFile(path, "utf8"));
  const servers = asRecord(asRecord(parsed)?.mcpServers);
  const entry = asRecord(servers?.[serverName]);
  if (!entry) throw new Error(`MCP server "${serverName}" not found in ${path}`);
  const command = asString(entry.command);
  if (!command) throw new Error(`MCP server "${serverName}" has no command`);

  const argsValue = entry.args;
  const args = Array.isArray(argsValue) ? argsValue.filter((arg): arg is string => typeof arg === "string") : [];
  const envValue = asRecord(entry.env);
  const env: Record<string, string> = {};
  if (envValue) {
    for (const [name, value] of Object.entries(envValue)) {
      if (typeof value === "string") env[name] = value;
    }
  }
  return { command, args, env };
}

function mergedEnv(extra: Record<string, string> | undefined): Record<string, string> {
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(process.env)) {
    if (typeof value === "string") env[name] = value;
  }
  return { ...env, ...extra };
}

export class McpJiraSource implements JiraSource {
  private constructor(private readonly client: Client) {}

  static async connect(configPath: string, serverName: string): Promise<McpJiraSource> {
    const config = await loadMcpServerConfig(configPath, serverName);
    const transport = new StdioClientTransport({
      command: config.command,
      args: config.args ?? [],
      env: mergedEnv(config.env),
    });
    const client = new Client({ name: "vtx-agent-master", version: "1.0.0" });
    await client.connect(transport);
    return new McpJiraSource(client);
  }

  async search(jql: string, limit: number, startAt: number): Promise<JiraSearchPage> {
    const result: unknown = await this.client.callTool(
      {
        name: SEARCH_TOOL,
        arguments: { jql, fields: SEARCH_FIELDS, limit, start_at: startAt },
      },
      undefined,
      { timeout: CALL_TIMEOUT_MS },
    );
    if (asRecord(result)?.isError === true) {
      throw new Error(`jira_search failed: ${textOf(result).slice(0, 500)}`);
    }
    return normalizeSearchPayload(extractPayload(result));
  }

  async close(): Promise<void> {
    await this.client.close();
  }
}
