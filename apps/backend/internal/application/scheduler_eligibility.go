package application

import (
	"context"
	"fmt"
	"github.com/vtx/platform/internal/integrations/jira"
)

type SchedulerIssueIdentity interface {
	SchedulerIssue(context.Context, string) (site, issueID, project, status string, err error)
}
type SchedulerJiraGuard struct {
	Store          SchedulerIssueIdentity
	Jira           jira.IssueFetcher
	EligibleStatus func(project, status string) bool
}

func (g SchedulerJiraGuard) Eligible(ctx context.Context, runID string) (bool, error) {
	_, id, project, _, err := g.Store.SchedulerIssue(ctx, runID)
	if err != nil {
		return false, err
	}
	issue, err := g.Jira.FetchIssue(ctx, id)
	if err != nil {
		return false, fmt.Errorf("scheduler Jira fetch: %w", err)
	}
	if issue.ID != id || issue.ProjectKey != project || g.EligibleStatus == nil || !g.EligibleStatus(project, issue.StatusID) {
		return false, nil
	}
	for _, link := range issue.Links {
		if link.Inward != nil && link.Type.Inward == "is blocked by" && link.Inward.Fields.Status.StatusCategory.Key != "done" {
			return false, nil
		}
	}
	return true, nil
}
