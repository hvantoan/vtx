package application

import (
	"context"
	"errors"
	"time"

	"github.com/vtx/platform/internal/integrations/jira"
)

// VTX-5 coding slot scheduler ports. Same discipline as registry/worktree:
// exactly one production implementation per port (pgx store, Jira client) and
// fakes in tests. The store owns the transactions the invariants are stated
// in; the service owns ordering and Jira gating.

var (
	// ErrRunNotEligible — the run may not compete for a coding slot: not in
	// 'preparing', no allocated worktree, or already terminal.
	ErrRunNotEligible = errors.New("scheduler: run not eligible for coding")
	// ErrEntryNotFound — no queue entry for the requested target.
	ErrEntryNotFound = errors.New("scheduler: queue entry not found")
	// ErrGrantNotFound — no slot grant for the requested id.
	ErrGrantNotFound = errors.New("scheduler: grant not found")
)

// DefaultHeartbeatTTL is how long a held grant may go without a heartbeat
// before the sweep marks it suspect. It marks a slot for operator review —
// it never releases it (AC-07).
const DefaultHeartbeatTTL = 2 * time.Minute

// DefaultRecheckInterval parks an entry that just failed eligibility
// (dependency open, Jira unreadable) before the next evaluation.
const DefaultRecheckInterval = time.Minute

// SchedulingDataFetcher reads priority + dependency links for one issue.
// jira.SchedulingIssue is the Jira-side admission input, fetched fresh at
// decision time — never cached in the queue row and never read inside a DB
// tx. Transient failures are wrapped in jira.ErrRetryable by the production
// client; the scheduler treats ANY error as "cannot verify -> no new grant".
type SchedulingDataFetcher interface {
	FetchSchedulingIssue(ctx context.Context, issueID string) (jira.SchedulingIssue, error)
}

// QueueEntry is a coding_queue_entries row.
type QueueEntry struct {
	EntryID     string
	RunID       string
	TaskID      string
	FifoSeq     int64
	Status      string
	BlockReason *string
	RecheckAt   *time.Time
	EnqueuedAt  time.Time
}

// Queue entry statuses (mirror the CHECK constraint).
const (
	QueueStatusWaiting = "waiting"
	QueueStatusGranted = "granted"
	QueueStatusClosed  = "closed"
)

// WaitingEntryView joins a waiting entry to its run + task for the
// scheduling pass. JiraIssueID is the numeric issue.id used for the re-fetch.
type WaitingEntryView struct {
	QueueEntry
	RunState     string
	JiraIssueID  string
	JiraIssueKey string
}

// SlotGrant is a coding_slot_grants row.
type SlotGrant struct {
	GrantID            string
	SlotNo             int
	RunID              string
	TaskID             string
	EntryID            string
	Owner              string
	SessionEpoch       int64
	Status             string
	HeartbeatAt        *time.Time
	ReleaseReason      *string
	ReleaseConfirmedAt *time.Time
	CreatedAt          time.Time
}

// Grant statuses (mirror the CHECK constraint).
const (
	GrantStatusHeld     = "held"
	GrantStatusSuspect  = "suspect"
	GrantStatusReleased = "released"
)

// GrantCommand is one slot request decided inside the grant transaction.
type GrantCommand struct {
	EntryID string
	RunID   string
	TaskID  string
	Owner   string
	Reason  string // admission note written to audit (e.g. "priority:High")
}

// GrantOutcome reports what the grant transaction did.
type GrantOutcome struct {
	Grant SlotGrant
	// Granted — a new slot was allocated.
	Granted bool
	// AlreadyHeld — the run already had an active grant; Grant is the
	// existing one. Re-grant is idempotent, never a second allocation.
	AlreadyHeld bool
	// NoFreeSlot — the pool is exhausted; the entry stays waiting.
	NoFreeSlot bool
	// Closed — the entry died inside the tx (run cancelled/terminal); no
	// slot was allocated and the entry is closed for good.
	Closed bool
	// Skipped — the entry was no longer waiting when the tx locked it.
	Skipped bool
}

// ConfirmOutcome classifies a stop-confirmation result (AC-08).
type ConfirmOutcome string

const (
	// ConfirmReleased — the held/suspect grant was released (exactly once).
	ConfirmReleased ConfirmOutcome = "released"
	// ConfirmAlreadyReleased — duplicate or late confirmation after the
	// release committed. Safe no-op.
	ConfirmAlreadyReleased ConfirmOutcome = "already_released"
	// ConfirmStaleSession — the confirmation carried an epoch from an older
	// session; the current grant is untouched.
	ConfirmStaleSession ConfirmOutcome = "stale_confirmation"
)

// SchedulerMetrics is the queue/active/blocked snapshot (AC-10).
type SchedulerMetrics struct {
	QueueWaiting  int
	QueueBlocked  int
	SlotsHeld     int
	SlotsSuspect  int
	SlotsFree     int
	SlotsMax      int
}

// SchedulerStore is the transactional persistence surface for the scheduler.
type SchedulerStore interface {
	// EnqueueRun locks the run FOR UPDATE, verifies it is nonterminal, in
	// 'preparing', and has an allocated worktree (the "preparation done"
	// signal), then inserts the queue entry. Idempotent per run: a repeat
	// call returns the existing entry. ErrRunNotEligible on guard failure.
	EnqueueRun(ctx context.Context, runID string) (QueueEntry, error)

	// ListWaitingEntries returns waiting entries whose recheck backoff has
	// expired, joined to run state and Jira identity. FIFO order by
	// fifo_seq; the service re-sorts by Jira priority.
	ListWaitingEntries(ctx context.Context, now time.Time, limit int32) ([]WaitingEntryView, error)

	// SetEntryBlock records a block reason and recheck time on a waiting
	// entry. A blocked head must never stall entries behind it.
	SetEntryBlock(ctx context.Context, entryID, reason string, recheckAt time.Time) error

	// GrantSlot runs the atomic grant transaction: pool row FOR UPDATE →
	// re-check entry waiting + run nonterminal (a cancel that landed since
	// the candidate list wins → entry closed, no grant) → active count <
	// max_slots → existing active grant for the run returned idempotently →
	// insert grant on the lowest free slot_no → mark entry granted → audit.
	// The audit row commits in the same tx.
	GrantSlot(ctx context.Context, cmd GrantCommand, auditReason string) (GrantOutcome, error)

	// ConfirmAgentStopped releases a slot only on a valid fencing epoch:
	// the locked grant must be non-released and its session_epoch must match
	// the presented one. Returns ConfirmReleased / ConfirmAlreadyReleased /
	// ConfirmStaleSession — never releases twice, never lets an old-session
	// confirmation free a new session's slot (AC-08).
	ConfirmAgentStopped(ctx context.Context, grantID string, sessionEpoch int64, reason string, at time.Time, actor string) (ConfirmOutcome, error)

	// RecordHeartbeat stores the worker heartbeat on a non-released grant
	// only when the presented epoch matches. A stale heartbeat is a no-op.
	RecordHeartbeat(ctx context.Context, grantID string, sessionEpoch int64, at time.Time) error

	// MarkSuspectGrants flips held grants with missing/stale heartbeats to
	// 'suspect' and writes one audit row each. Slots are KEPT — this marks
	// "wait for operator", never auto-release (AC-07). Returns the count.
	MarkSuspectGrants(ctx context.Context, cutoff time.Time, actor string) (int, error)

	// Metrics returns the queue/active/blocked snapshot for observability.
	Metrics(ctx context.Context) (SchedulerMetrics, error)
}
