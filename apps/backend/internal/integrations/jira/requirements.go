package jira

import (
	"encoding/json"
	"fmt"
	"strings"
)

// IssueLink retains only dependency identity and status, never comments or users.
type IssueLink struct {
	Type struct {
		Inward  string `json:"inward"`
		Outward string `json:"outward"`
	} `json:"type"`
	Inward  *LinkedIssue `json:"inwardIssue,omitempty"`
	Outward *LinkedIssue `json:"outwardIssue,omitempty"`
}
type LinkedIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Status struct {
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
	} `json:"fields"`
}
type NamedValue struct {
	Name string `json:"name"`
}
type IssueContent struct {
	Summary            string      `json:"summary"`
	Description        string      `json:"description"`
	AcceptanceCriteria string      `json:"acceptance_criteria,omitempty"`
	Labels             []string    `json:"labels"`
	Priority           string      `json:"priority"`
	Links              []IssueLink `json:"issuelinks"`
	Subtasks           []string    `json:"subtasks"`
	FixVersions        []string    `json:"fixVersions"`
}

// Markdown accepts Jira's legacy string or Cloud Atlassian Document Format.
// Unknown node properties (users, links, embedded credentials) are not retained.
func Markdown(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var node struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Attrs struct {
			Level int    `json:"level"`
			State string `json:"state"`
		} `json:"attrs"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &node) != nil {
		return ""
	}
	var b strings.Builder
	for _, child := range node.Content {
		b.WriteString(Markdown(child))
	}
	s := b.String()
	switch node.Type {
	case "text":
		return node.Text
	case "hardBreak":
		return "\n"
	case "heading":
		return strings.Repeat("#", node.Attrs.Level) + " " + strings.TrimSpace(s) + "\n"
	case "paragraph":
		return s + "\n"
	case "listItem":
		return "- " + strings.TrimSpace(s) + "\n"
	case "taskItem":
		return "- [ ] " + strings.TrimSpace(s) + "\n"
	default:
		return s
	}
}
func contentFromFields(raw json.RawMessage, acField string) (IssueContent, error) {
	var fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Labels      []string        `json:"labels"`
		Priority    NamedValue      `json:"priority"`
		Links       []IssueLink     `json:"issuelinks"`
		Subtasks    []struct {
			Key string `json:"key"`
		} `json:"subtasks"`
		FixVersions []NamedValue `json:"fixVersions"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return IssueContent{}, fmt.Errorf("decode Jira content: %w", err)
	}
	out := IssueContent{Summary: fields.Summary, Description: Markdown(fields.Description), Labels: fields.Labels, Priority: fields.Priority.Name, Links: fields.Links, Subtasks: []string{}, FixVersions: []string{}}
	for _, s := range fields.Subtasks {
		out.Subtasks = append(out.Subtasks, s.Key)
	}
	for _, v := range fields.FixVersions {
		out.FixVersions = append(out.FixVersions, v.Name)
	}
	if acField != "" {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return IssueContent{}, err
		}
		out.AcceptanceCriteria = Markdown(values[acField])
	}
	return out, nil
}

// HTTPError carries only the numeric status; upstream response bodies can
// contain secrets and must never be used as policy/audit messages.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string                                 { return fmt.Sprintf("jira api status %d", e.Status) }
func (e *HTTPError) Unwrap() error                                 { return ErrRetryable }
func (c *Client) WithAcceptanceCriteriaField(field string) *Client { c.acField = field; return c }
