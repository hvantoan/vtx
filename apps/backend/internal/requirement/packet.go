// Package requirement freezes the dispatch scope and base-SHA test baseline.
package requirement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/vtx/platform/internal/integrations/jira"
)

const Version = "vtx-req/v1"

type Packet struct {
	IssueKey           string            `json:"issue_key"`
	Summary            string            `json:"summary"`
	Description        string            `json:"description"`
	AcceptanceCriteria []string          `json:"acceptance_criteria"`
	RequirementVersion string            `json:"requirement_version"`
	RequirementHash    string            `json:"requirement_hash"`
	Snapshot           jira.IssueContent `json:"issue_snapshot"`
}

var secretAssignment = regexp.MustCompile(`(?i)(api[_-]?key|api[_-]?token|authorization|password|secret)\s*[:=]\s*[^\s,;]+`)

func redact(s string) string { return secretAssignment.ReplaceAllString(s, "$1=[redacted]") }
func Normalize(issue jira.FetchedIssue) (Packet, error) {
	snapshot := issue.IssueContent
	snapshot.Summary = redact(snapshot.Summary)
	snapshot.Description = redact(snapshot.Description)
	ac := criteria(redact(snapshot.AcceptanceCriteria), snapshot.Description)
	snapshot.AcceptanceCriteria = "" // normalized ACs have one spelling in the packet
	p := Packet{IssueKey: issue.Key, Summary: snapshot.Summary, Description: snapshot.Description, AcceptanceCriteria: ac, RequirementVersion: Version, Snapshot: snapshot}
	// A JSON object map sorts keys recursively in encoding/json. Hash excludes
	// fetched_at and itself so replaying the same fetched content is stable.
	raw, err := json.Marshal(p)
	if err != nil {
		return Packet{}, err
	}
	var canonical map[string]any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return Packet{}, err
	}
	delete(canonical, "requirement_hash")
	raw, err = json.Marshal(canonical)
	if err != nil {
		return Packet{}, err
	}
	sum := sha256.Sum256(raw)
	p.RequirementHash = hex.EncodeToString(sum[:])
	return p, nil
}
func criteria(custom, description string) []string {
	if strings.TrimSpace(custom) != "" {
		return lines(custom, false)
	}
	var section strings.Builder
	in := false
	for _, line := range strings.Split(description, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "## Acceptance Criteria") {
			in = true
			continue
		}
		if in && strings.HasPrefix(trimmed, "#") {
			break
		}
		if in {
			section.WriteString(line)
			section.WriteByte('\n')
		}
	}
	if strings.TrimSpace(section.String()) != "" {
		return lines(section.String(), false)
	}
	return lines(description, true)
}
func lines(text string, checkOnly bool) []string {
	out := []string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if checkOnly && !strings.HasPrefix(line, "- [ ]") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "- [ ]"), "- "))
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
