package application

import (
	"context"
	"errors"
	"time"

	"github.com/vtx/platform/internal/domain"
)

// VTX-5 scheduler ports. Same shape as the registry ports: one production
// implementation (pgx), one fake per test. The store owns transactions; the
// service owns policy.

var (
	// ErrScheduledRunNotFound marks a queue/enqueue target that is not a run.
	ErrScheduledRunNotFound = errors.New("scheduler store: run not found")
)

// SchedulingData is the Jira-side admission input: the issue's current
// priority plus its inward "is blocked by" links, each already resolved to a
// done/not-done verdict by the provider.
type SchedulingData struct {
	PriorityName string
	BlockedBy    []BlockerRef
}

// BlockerRef is one inward "is blocked by" issue link.
type BlockerRef struct {
	IssueID    string
	IssueKey   string
	StatusName string
	// Done means the blocker's status category is "done". Anything else —
	// including an unreadable or missing category — is NOT done.
	Done bool
}

// SchedulingDataProvider reads the Jira admission inputs. Every error is
// treated as "unverified" by the scheduler — a task whose dependencies cannot
// be checked is never granted a slot (AC-05).
type SchedulingDataProvider interface {
	FetchSchedulingData(ctx context.Context, issueID string) (SchedulingData, error)
}

// SchedulingDataProviderFunc adapts a plain function (e.g. a wiring closure
// over the jira adapter) to the SchedulingDataProvider port.
type SchedulingDataProviderFunc func(ctx context.Context, issueID string) (SchedulingData, error)

func (f SchedulingDataProviderFunc) FetchSchedulingData(ctx context.Context, issueID string) (SchedulingData, error) {
	return f(ctx, issueID)
}

// QueueEntryView is a read model of one coding_queue row.
type QueueEntryView struct {
	QueueID       string
	RunID         string
	TaskID        string
	JiraSiteID    string
	JiraIssueID   string
	JiraIssueKey  string
	PriorityRank  int
	PriorityName  string
	FifoSeq       int64
	Status        domain.QueueStatus
	BlockedReason string
	GrantID       string
	EnqueuedAt    time.Time
}

// ScheduledRunView is the minimum run data enqueue validation needs.
type ScheduledRunView struct {
	RunID        string
	TaskID       string
	State        domain.State
	JiraSiteID   string
	JiraIssueID  string
	JiraIssueKey string
}

// SlotGrant is a held coding slot — the evidence VTX-7 must present before
// starting its agent.
type SlotGrant struct {
	GrantID     string
	QueueID     string
	RunID       string
	TaskID      string
	WorkerOwner string
}

// ReleaseResult reports what one stop-confirmation did.
type ReleaseResult struct {
	Released bool // false ⇒ stale/duplicate/late confirmation, nothing released
	GrantID  string
	QueueID  string
	RunID    string
}

// SchedulerMetrics is the queue/active/blocked snapshot (AC-10).
type SchedulerMetrics struct {
	Waiting            int64
	Allocated          int64
	BlockedDependency  int64
	BlockedUnverified  int64
	Cancelled          int64
	Released           int64
	SlotsHeld          int64
	SlotsSuspected     int64
	SlotsMax           int64
}

// AdmitResult summarizes one admission pass.
type AdmitResult struct {
	Granted   []SlotGrant
	Blocked   []string // queue_ids newly classified blocked
	Capacity  int      // slots still held after the pass
	Evaluated int
}

// SchedulerStore is the transactional persistence surface for the scheduler.
// Methods that mutate admission state (GrantPass, ConfirmStop) own their
// transaction and serialize through the scheduler advisory lock.
type SchedulerStore interface {
	// GetRunForSchedule loads the run + task identity for enqueue validation.
	GetRunForSchedule(ctx context.Context, runID string) (ScheduledRunView, error)

	// Enqueue inserts the queue entry. A second enqueue of the same run
	// returns the existing entry (idempotent), never a duplicate.
	Enqueue(ctx context.Context, entry QueueEntryView, actor string) (QueueEntryView, bool, error)

	// ListSchedulable returns waiting + blocked entries in fifo order for
	// re-evaluation. Reads never take the advisory lock.
	ListSchedulable(ctx context.Context) ([]QueueEntryView, error)

	// SetQueuePriority persists the last verified Jira priority.
	SetQueuePriority(ctx context.Context, queueID string, rank int, name string) error

	// SetQueueBlocked marks an entry blocked with a reason, or restores it to
	// waiting (empty reason). The audit row is written by the store in the
	// same transaction.
	SetQueueBlocked(ctx context.Context, queueID string, status domain.QueueStatus, reason, actor string) error

	// GrantPass performs the atomic admission: under the scheduler advisory
	// lock it re-checks each ordered queue entry is still waiting, then
	// inserts the slot grant + marks the entry allocated + audits — never
	// exceeding max total held slots.
	GrantPass(ctx context.Context, worker string, orderedQueueIDs []string, max int, actor string) ([]SlotGrant, error)

	// ConfirmStop releases a held slot. The (run_id, grant_id) pair must
	// match a held grant exactly; releasing is idempotent and a stale grant
	// releases nothing (AC-08). Audits either outcome.
	ConfirmStop(ctx context.Context, runID, grantID, reason, actor string) (ReleaseResult, error)

	// RecordHeartbeat refreshes a held grant's heartbeat. Only the recorded
	// grant_id may refresh its own slot.
	RecordHeartbeat(ctx context.Context, runID, grantID string) error

	// MarkStaleSuspected flags held slots whose heartbeat is older than
	// staleBefore. Flagging only — never releases (AC-07).
	MarkStaleSuspected(ctx context.Context, staleBefore time.Time) ([]string, error)

	// CancelQueued atomically cancels a non-allocated entry. Returns
	// ErrSlotHeld when the entry is allocated — cancelling a coding task is
	// a stop-then-release flow, not a queue delete.
	CancelQueued(ctx context.Context, runID, reason, actor string) (QueueEntryView, error)

	// Metrics returns the queue/slot counter snapshot.
	Metrics(ctx context.Context) (SchedulerMetrics, error)
}
