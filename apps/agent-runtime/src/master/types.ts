export interface JiraAssignee {
  accountId?: string;
  displayName?: string;
  email?: string;
}

export interface JiraIssue {
  id: string;
  key: string;
  summary: string;
  status: string;
  statusCategory?: string;
  issueType?: string;
  updated: string;
  /** Jira's statuscategorychangedate — the best available "entered this status" marker. */
  statusChangedAt?: string;
  browseUrl?: string;
  assignee?: JiraAssignee;
}

export interface JiraSearchPage {
  issues: JiraIssue[];
  total?: number;
}

export interface JiraSource {
  search(jql: string, limit: number, startAt: number): Promise<JiraSearchPage>;
  close(): Promise<void>;
}

/**
 * Version marker for dedupe. `updated` alone changes on comments, so prefer the
 * status-category change timestamp when the tenant returns it.
 */
export function issueVersion(issue: JiraIssue): string {
  return issue.statusChangedAt ?? issue.updated;
}
