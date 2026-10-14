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
