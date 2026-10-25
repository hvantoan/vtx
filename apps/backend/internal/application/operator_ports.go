package application

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vtx/platform/internal/domain"
)

var ErrNotDeadLettered = errors.New("outbox event is not dead-lettered")

type RunFilter struct {
	State           *domain.State
	IssueKey        *string
	BeforeUpdatedAt *time.Time
	BeforeRunID     *string
	Limit           int32
}
type OperatorRunView struct {
	RunID      string       `json:"run_id"`
	IssueKey   string       `json:"issue_key"`
	State      domain.State `json:"state"`
	Attempt    int32        `json:"attempt"`
	CommitSHA  *string      `json:"commit_sha"`
	PRURL      *string      `json:"pr_url"`
	LeaseOwner *string      `json:"lease_owner"`
	ErrorCode  *string      `json:"error_code"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
}
type WorktreeDetail struct {
	Path    string  `json:"path"`
	Branch  string  `json:"branch"`
	BaseSHA *string `json:"base_sha"`
	Status  string  `json:"status"`
}
type RunDetailView struct {
	Run          OperatorRunView  `json:"run"`
	Timeline     []EventView      `json:"timeline"`
	Worktree     *WorktreeDetail  `json:"worktree"`
	PullRequest  *PullRequestView `json:"pull_request"`
	GateDecision *GateVerdict     `json:"gate_decision"`
	LastError    *RunErrorView    `json:"last_error"`
}
type RunErrorView struct{ ErrorCode, ErrorMessage string }
type OutboxTypeStat struct {
	EventType        string `json:"event_type"`
	Pending          int64  `json:"pending"`
	Retrying         int64  `json:"retrying"`
	DeadLettered     int64  `json:"dead_lettered"`
	OldestAgeSeconds int64  `json:"oldest_age_seconds"`
}
type DlqItem struct {
	OutboxID         string    `json:"outbox_id"`
	EventType        string    `json:"event_type"`
	DeadLetterReason *string   `json:"dead_letter_reason"`
	Attempt          int32     `json:"attempt"`
	DeadLetteredAt   time.Time `json:"dead_lettered_at"`
	RedriveCount     int32     `json:"redrive_count"`
}
type WorkerHeartbeatView struct {
	WorkerID   string          `json:"worker_id"`
	WorkerKind string          `json:"worker_kind"`
	Queue      *string         `json:"queue"`
	StartedAt  time.Time       `json:"started_at"`
	LastBeatAt time.Time       `json:"last_beat_at"`
	ActiveRuns int32           `json:"active_runs"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Status     string          `json:"status"`
}
type OperatorAuditRecord struct {
	Actor, OperatorID, Action, TargetType, TargetID, Reason, CorrelationID, RequestID, Outcome string
	Detail                                                                                     []byte
}
type OperatorOverview struct {
	RunsByState   map[domain.State]int64 `json:"runs_by_state"`
	DeferredCount int64                  `json:"deferred_count"`
	Admission     struct {
		Max    int64 `json:"max"`
		Active int64 `json:"active"`
	} `json:"admission"`
	Outbox struct {
		Pending          int64 `json:"pending"`
		Retrying         int64 `json:"retrying"`
		DeadLettered     int64 `json:"dead_lettered"`
		OldestAgeSeconds int64 `json:"oldest_age_seconds"`
	} `json:"outbox"`
	Workers struct {
		Up   int64 `json:"up"`
		Down int64 `json:"down"`
	} `json:"workers"`
}

type OperatorStore interface {
	RunStateHistogram(context.Context) (map[domain.State]int64, error)
	CountDeferredIntents(context.Context) (int64, error)
	CountActiveRuns(context.Context) (int64, error)
	ListRuns(context.Context, RunFilter) ([]OperatorRunView, error)
	GetRunDetail(context.Context, string) (RunDetailView, error)
	OutboxStats(context.Context) ([]OutboxTypeStat, error)
	ListDeadLettered(context.Context, *string, int32) ([]DlqItem, error)
	ListWorkerHeartbeats(context.Context) ([]WorkerHeartbeatView, error)
	RedriveDeadLetter(context.Context, string, OperatorAuditRecord) error
}
