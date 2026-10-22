package application

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/vtx/platform/internal/integrations/jira"
)

// VTX-5 / AUTO-04 scheduler service: the admission authority for coding
// slots. It owns queue ordering (Jira priority, then FIFO), Jira dependency
// gating, and the grant/release/heartbeat lifecycle. The store owns the
// transactions; this service owns the decisions — and never holds a DB
// transaction across a Jira call.

// defaultCandidateLimit bounds one scheduling pass. The queue is small by
// design (one entry per nonterminal run); this is a safety rail, not a knob.
const defaultCandidateLimit = 64

// SchedulerService grants at most max_slots concurrent coding slots across
// the whole system, regardless of project or worker count.
type SchedulerService struct {
	store           SchedulerStore
	jira            SchedulingDataFetcher
	owner           string
	clock           Clock
	recheckInterval time.Duration
	heartbeatTTL    time.Duration
	logger          *slog.Logger
}

func NewSchedulerService(store SchedulerStore, jira SchedulingDataFetcher, owner string, clock Clock, logger *slog.Logger) *SchedulerService {
	if owner == "" {
		owner = "scheduler"
	}
	if clock == nil {
		clock = SystemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SchedulerService{
		store:           store,
		jira:            jira,
		owner:           owner,
		clock:           clock,
		recheckInterval: DefaultRecheckInterval,
		heartbeatTTL:    DefaultHeartbeatTTL,
		logger:          logger,
	}
}

// SetRecheckInterval overrides the park duration after a failed eligibility
// check (tests use it; production keeps the default).
func (s *SchedulerService) SetRecheckInterval(d time.Duration) { s.recheckInterval = d }

// SetHeartbeatTTL overrides the suspect threshold (tests use it).
func (s *SchedulerService) SetHeartbeatTTL(d time.Duration) { s.heartbeatTTL = d }

// Owner returns the identity written to grants and audit rows.
func (s *SchedulerService) Owner() string { return s.owner }

// EnqueueForCoding admits a run to the slot queue. The store verifies the
// run is nonterminal, in 'preparing', and has an allocated worktree —
// preparation never occupies a slot, it only unlocks queue eligibility
// (AC-09).
func (s *SchedulerService) EnqueueForCoding(ctx context.Context, runID string) (QueueEntry, error) {
	entry, err := s.store.EnqueueRun(ctx, runID)
	if err != nil {
		return QueueEntry{}, err
	}
	s.logger.Info("coding queue entry ready",
		"metric", "scheduler_queue_enqueued",
		"run_id", entry.RunID,
		"entry_id", entry.EntryID,
		"fifo_seq", entry.FifoSeq,
		"status", entry.Status,
	)
	return entry, nil
}

// ScheduleReport summarizes one scheduling pass.
type ScheduleReport struct {
	Candidates int
	Granted    []SlotGrant
	Blocked    int // entries parked with a reason this pass
	Closed     int // entries closed inside the grant tx (cancel race)
}

// ScheduleOnce evaluates the waiting queue and grants free slots in Jira
// priority order (FIFO tie-break). Each Jira read happens outside any DB
// transaction; each grant commits atomically inside its own.
//
// Ordering guarantee: candidates are sorted by (priority rank desc,
// fifo_seq asc) before any grant is attempted, so a higher-priority task
// that became eligible earlier wins over a lower-priority one that was
// waiting longer — matching the ticket's priority-then-FIFO rule.
func (s *SchedulerService) ScheduleOnce(ctx context.Context) (ScheduleReport, error) {
	now := s.clock.Now()
	entries, err := s.store.ListWaitingEntries(ctx, now, defaultCandidateLimit)
	if err != nil {
		return ScheduleReport{}, fmt.Errorf("list waiting entries: %w", err)
	}
	rep := ScheduleReport{Candidates: len(entries)}

	type candidate struct {
		entry WaitingEntryView
		data  jira.SchedulingIssue
	}
	eligible := make([]candidate, 0, len(entries))
	recheckAt := now.Add(s.recheckInterval)

	for _, e := range entries {
		if e.RunState == "" {
			continue
		}
		// A terminal run's entry is dead weight — close it via the block
		// path is wrong; the grant tx handles cancel races under lock.
		// Here we only gate on Jira data.
		data, err := s.jira.FetchSchedulingIssue(ctx, e.JiraIssueID)
		if err != nil {
			// Unverifiable ⇒ no new grant for this task; record the reason
			// and re-check later (AC-05). Never block the rest of the queue.
			if berr := s.store.SetEntryBlock(ctx, e.EntryID, "jira_unverified", recheckAt); berr != nil {
				return rep, fmt.Errorf("mark jira_unverified: %w", berr)
			}
			rep.Blocked++
			s.logger.Warn("scheduler candidate parked: jira unverifiable",
				"metric", "scheduler_entry_blocked",
				"run_id", e.RunID, "issue_key", e.JiraIssueKey, "error", err)
			continue
		}
		if len(data.UnresolvedBlockers) > 0 {
			reason := fmt.Sprintf("dependency_open:%v", data.UnresolvedBlockers)
			if berr := s.store.SetEntryBlock(ctx, e.EntryID, reason, recheckAt); berr != nil {
				return rep, fmt.Errorf("mark dependency_open: %w", berr)
			}
			rep.Blocked++
			s.logger.Info("scheduler candidate parked: open dependency",
				"metric", "scheduler_entry_blocked",
				"run_id", e.RunID, "issue_key", e.JiraIssueKey,
				"blockers", data.UnresolvedBlockers)
			continue
		}
		eligible = append(eligible, candidate{entry: e, data: data})
	}

	// Priority first, FIFO inside equal priority — the ticket's rule.
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].data.PriorityRank != eligible[j].data.PriorityRank {
			return eligible[i].data.PriorityRank > eligible[j].data.PriorityRank
		}
		return eligible[i].entry.FifoSeq < eligible[j].entry.FifoSeq
	})

	for _, c := range eligible {
		outcome, err := s.store.GrantSlot(ctx, GrantCommand{
			EntryID: c.entry.EntryID,
			RunID:   c.entry.RunID,
			TaskID:  c.entry.TaskID,
			Owner:   s.owner,
		}, fmt.Sprintf("priority:%s", c.data.PriorityName))
		if err != nil {
			return rep, fmt.Errorf("grant slot for run %s: %w", c.entry.RunID, err)
		}
		switch {
		case outcome.Granted:
			rep.Granted = append(rep.Granted, outcome.Grant)
			s.logger.Info("coding slot granted",
				"metric", "scheduler_slot_granted",
				"run_id", outcome.Grant.RunID,
				"grant_id", outcome.Grant.GrantID,
				"slot_no", outcome.Grant.SlotNo,
				"session_epoch", outcome.Grant.SessionEpoch,
				"priority", c.data.PriorityName,
				"issue_key", c.entry.JiraIssueKey)
		case outcome.AlreadyHeld:
			// Re-grant is idempotent: report the existing slot, never a
			// second allocation (AC-03).
			rep.Granted = append(rep.Granted, outcome.Grant)
		case outcome.Closed:
			rep.Closed++
			s.logger.Info("queue entry closed during grant",
				"metric", "scheduler_entry_closed",
				"run_id", c.entry.RunID)
		case outcome.NoFreeSlot:
			// Pool exhausted — remaining candidates stay waiting.
			s.logMetrics(ctx)
			return rep, nil
		case outcome.Skipped:
			// Another scheduler granted/closed it first; harmless.
		}
	}

	s.logMetrics(ctx)
	return rep, nil
}

// ConfirmAgentStopped releases a slot after the executing worker confirms
// the agent session actually stopped. The fencing epoch is required: an
// old session's confirmation can never free a newer session's slot, and
// duplicate/late confirmations are safe no-ops (AC-08).
func (s *SchedulerService) ConfirmAgentStopped(ctx context.Context, grantID string, sessionEpoch int64, reason string) (ConfirmOutcome, error) {
	outcome, err := s.store.ConfirmAgentStopped(ctx, grantID, sessionEpoch, sanitizeReason(reason), s.clock.Now(), s.owner)
	if err != nil {
		return ConfirmAlreadyReleased, err
	}
	switch outcome {
	case ConfirmReleased:
		s.logger.Info("coding slot released",
			"metric", "scheduler_slot_released",
			"grant_id", grantID, "session_epoch", sessionEpoch)
	case ConfirmStaleSession:
		s.logger.Warn("stale stop confirmation ignored",
			"metric", "scheduler_stale_confirmation",
			"grant_id", grantID, "session_epoch", sessionEpoch)
	}
	return outcome, nil
}

// RecordHeartbeat keeps a held grant alive. The epoch must match the grant's
// fencing token — a heartbeat from a dead session changes nothing.
func (s *SchedulerService) RecordHeartbeat(ctx context.Context, grantID string, sessionEpoch int64) error {
	return s.store.RecordHeartbeat(ctx, grantID, sessionEpoch, s.clock.Now())
}

// SweepSuspectGrants flags held grants whose heartbeat has gone silent past
// the TTL. Slots stay held: loss of heartbeat marks the grant for operator
// review, it is never evidence the agent stopped (AC-07).
func (s *SchedulerService) SweepSuspectGrants(ctx context.Context) (int, error) {
	cutoff := s.clock.Now().Add(-s.heartbeatTTL)
	n, err := s.store.MarkSuspectGrants(ctx, cutoff, s.owner)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.logger.Warn("coding grants marked suspect",
			"metric", "scheduler_grants_suspect", "count", n)
	}
	return n, nil
}

// Metrics exposes the queue/active/blocked snapshot (AC-10): waiting vs
// blocked entries, held vs suspect slots — heartbeat-held is deliberately
// distinguished from dependency-blocked.
func (s *SchedulerService) Metrics(ctx context.Context) (SchedulerMetrics, error) {
	return s.store.Metrics(ctx)
}

func (s *SchedulerService) logMetrics(ctx context.Context) {
	m, err := s.store.Metrics(ctx)
	if err != nil {
		s.logger.Warn("scheduler metrics unavailable", "error", err)
		return
	}
	s.logger.Info("scheduler metrics",
		"metric", "scheduler_pool_state",
		"queue_waiting", m.QueueWaiting,
		"queue_blocked", m.QueueBlocked,
		"slots_held", m.SlotsHeld,
		"slots_suspect", m.SlotsSuspect,
		"slots_free", m.SlotsFree,
		"slots_max", m.SlotsMax)
}
