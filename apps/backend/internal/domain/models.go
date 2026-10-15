package domain

import (
	"time"
)

type HealthStatus struct {
	Status        string    `json:"status"`
	Timestamp     time.Time `json:"timestamp"`
	UptimeSeconds float64   `json:"uptime_seconds"`
}

type DependencyStatus struct {
	Status    string  `json:"status"`
	LatencyMS float64 `json:"latency_ms"`
	Error     *string `json:"error,omitempty"`
}

type ReadyStatus struct {
	Status       string                      `json:"status"`
	Timestamp    time.Time                   `json:"timestamp"`
	Dependencies map[string]DependencyStatus `json:"dependencies"`
}

type SystemStatus struct {
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	Environment   string    `json:"environment"`
	GoVersion     string    `json:"go_version"`
	Goroutines    int       `json:"goroutines"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	Timestamp     time.Time `json:"timestamp"`
}

type WorkerItem struct {
	ID              string `json:"id"`
	Queue           string `json:"queue"`
	Status          string `json:"status"`
	ActiveWorkflows int    `json:"active_workflows"`
}

type WorkerStatusList struct {
	Status  string       `json:"status"`
	Workers []WorkerItem `json:"workers"`
}

type PlatformEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Source    string         `json:"source"`
	Data      map[string]any `json:"data"`
}

// JiraWebhookEnvelope is the parsed Jira Cloud administrative webhook payload
// (VTX-2). DB row structs are intentionally NOT mirrored here — the service
// uses sqlc-generated types directly.
type JiraWebhookEnvelope struct {
	Timestamp    int64  `json:"timestamp"`
	WebhookEvent string `json:"webhookEvent"`
	Issue        *struct {
		ID     string `json:"id"`
		Key    string `json:"key"`
		Self   string `json:"self"`
		Fields struct {
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
		} `json:"fields"`
	} `json:"issue"`
	Changelog *struct {
		ID    string `json:"id"`
		Items []struct {
			Field   string `json:"field"`
			FieldID string `json:"fieldId"`
			To      string `json:"to"`
		} `json:"items"`
	} `json:"changelog"`
}

// StatusTransitionTo scans changelog.items for the status change and returns
// its target status ID. Positional extraction (items[0]) is forbidden — Jira
// emits multi-item changelogs in arbitrary order.
func (e *JiraWebhookEnvelope) StatusTransitionTo() (string, bool) {
	if e.Changelog == nil {
		return "", false
	}
	for _, item := range e.Changelog.Items {
		if item.Field == "status" || item.FieldID == "status" {
			return item.To, true
		}
	}
	return "", false
}
