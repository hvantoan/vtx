package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// VTX-5 scheduling read: priority + "is blocked by" links for one issue.
// Fetched fresh at every admission decision — never cached, never trusted
// beyond the single response it came from.

// SchedulingIssue is the scheduler-facing view of a Jira issue.
type SchedulingIssue struct {
	ID                 string
	Key                string
	PriorityName       string
	PriorityRank       int
	UnresolvedBlockers []string
}

// SchedulingDataFetcher extends the adapter for VTX-5: the scheduler gates
// every slot grant on fresh priority + dependency data.
type SchedulingDataFetcher interface {
	FetchSchedulingIssue(ctx context.Context, issueID string) (SchedulingIssue, error)
}

// priorityRanks maps Jira Cloud's default priority names to an ordering rank
// (higher wins). Unknown/absent priorities rank 0 — deterministic, and the
// raw name is preserved in the audit reason so nothing is hidden.
var priorityRanks = map[string]int{
	"highest": 5,
	"high":    4,
	"medium":  3,
	"low":     2,
	"lowest":  1,
}

type schedulingResponse struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Priority   *struct {
			Name string `json:"name"`
		} `json:"priority"`
		IssueLinks []struct {
			Type struct {
				Name   string `json:"name"`
				Inward string `json:"inward"`
			} `json:"type"`
			InwardIssue *struct {
				Key    string `json:"key"`
				Fields struct {
					Status struct {
						StatusCategory struct {
							Key string `json:"key"`
						} `json:"statusCategory"`
					} `json:"status"`
				} `json:"fields"`
			} `json:"inwardIssue"`
		} `json:"issuelinks"`
	} `json:"fields"`
}

// FetchSchedulingIssue reads priority and issuelinks for issueID. A blocker
// is a link whose type.inward is "is blocked by" and whose linked issue's
// statusCategory is not "done". Every transport/non-2xx/decode failure is
// wrapped in ErrRetryable: the scheduler records "jira_unverified" and does
// not grant — it never guesses on a partial answer.
func (c *Client) FetchSchedulingIssue(ctx context.Context, issueID string) (SchedulingIssue, error) {
	if err := ValidateIssueID(issueID); err != nil {
		return SchedulingIssue{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestBudget)
	defer cancel()

	endpoint := *c.baseURL
	endpoint.Path = endpoint.Path + "/rest/api/3/issue/" + url.PathEscape(issueID)
	q := endpoint.Query()
	q.Set("fields", "priority,issuelinks")
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return SchedulingIssue{}, fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.apiToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return SchedulingIssue{}, fmt.Errorf("%w: jira api call: %v", ErrRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return SchedulingIssue{}, fmt.Errorf("%w: jira api status %d", ErrRetryable, resp.StatusCode)
	}

	var body schedulingResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return SchedulingIssue{}, fmt.Errorf("%w: decode scheduling response: %v", ErrRetryable, err)
	}
	if body.ID == "" || body.ID != issueID {
		return SchedulingIssue{}, fmt.Errorf("%w: issue response id %q does not match requested %q", ErrRetryable, body.ID, issueID)
	}

	issue := SchedulingIssue{ID: body.ID, Key: body.Key}
	if body.Fields.Priority != nil {
		issue.PriorityName = body.Fields.Priority.Name
		issue.PriorityRank = priorityRanks[strings.ToLower(body.Fields.Priority.Name)]
	}
	for _, link := range body.Fields.IssueLinks {
		if link.Type.Inward != "is blocked by" || link.InwardIssue == nil {
			continue
		}
		if link.InwardIssue.Fields.Status.StatusCategory.Key != "done" {
			issue.UnresolvedBlockers = append(issue.UnresolvedBlockers, link.InwardIssue.Key)
		}
	}
	return issue, nil
}
