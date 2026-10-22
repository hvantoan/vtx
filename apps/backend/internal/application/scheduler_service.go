package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vtx/platform/internal/domain"
)

// VTX-5 / AUTO-04 scheduler service.
//
// Policy lives here; the store owns transactions. The admission rule set:
//
//   - At most domain.MaxCodingSlots slots held system-wide (AC-01/02).
//   - Jira priority first, FIFO inside one priority; a blocked head never
//     starves eligible entries behind it (AC-04).
//   - "is blocked by" links unresolved or unverifiable ⇒ no slot (AC-05).
//   - Cancelled tasks never receive a slot (AC-06).
//   - Heartbeat loss marks suspected but keeps the slot; only a confirmed
//     agent stop releases, exactly once, never for a stale grant (AC-07/08).
//   - Queue admission is the only path that creates a slot — no run-state
//     count is ever used as the active-coding proxy (AC-09).
//
// The grant is admission evidence only. It does NOT move the run's state
// machine: preparing → coding still belongs to VTX-7, which presents the
// grant before starting its agent.
type SchedulerService struct {
	store    SchedulerStore
	jira     SchedulingDataProvider
	capacity int
	actor    string
	clock    Clock
	logger   *slog.Logger
}

func NewSchedulerService(
	store SchedulerStore,
	jira SchedulingDataProvider,
	actor string,
	clock Clock,
	logger *slog.Logger,
) *SchedulerService {
	if clock == nil {
		clock = SystemClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SchedulerService{
		store:    store,
		jira:     jira,
		capacity: domain.MaxCodingSlots,
		actor:    actor,
		clock:    clock,
		logger:   logger,
	}
}

// Enqueue registers a run in the coding queue. The run must exist and be
// nonterminal; queue admission is the single entry point that may ever lead
// to a slot, so nothing else creates queue rows. The current Jira priority is
// fetched at enqueue; a Jira outage records the unknown rank rather than
// dropping the task — admission re-verifies anyway.
func (s *SchedulerService) Enqueue(ctx context.Context, runID string) (QueueEntryView, error) {
	if runID == "" {
		return QueueEntryView{}, fmt.Errorf("%w: run_id is required", domain.ErrQueueNotFound)
	}
	run, err := s.store.GetRunForSchedule(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrScheduledRunNotFound) {
			return QueueEntryView{}, fmt.Errorf("%w: run %s", domain.ErrQueueNotFound, runID)
		}
		return QueueEntryView{}, err
	}
	if run.State.IsTerminal() {
		return QueueEntryView{}, fmt.Errorf("%w: run %s is %s", domain.ErrRunNotEligible, runID, run.State)
	}

	rank, name := s.priorityNow(ctx, run.JiraIssueID)

	entry, created, err := s.store.Enqueue(ctx, QueueEntryView{
		RunID:        run.RunID,
		TaskID:       run.TaskID,
		JiraSiteID:   run.JiraSiteID,
		JiraIssueID:  run.JiraIssueID,
		JiraIssueKey: run.JiraIssueKey,
		PriorityRank: rank,
		PriorityName: name,
	}, s.actor)
	if err != nil {
		return QueueEntryView{}, err
	}
	s.logger.Info("scheduler run enqueued",
		"metric", "scheduler_enqueued",
		"run_id", entry.RunID,
		"task_id", entry.TaskID,
		"queue_id", entry.QueueID,
		"status", string(entry.Status),
		"priority_rank", entry.PriorityRank,
		"deduplicated", !created,
	)
	return entry, nil
}

// priorityNow fetches the Jira priority without ever failing the caller:
// unverifiable priority ranks below every known value, and admission still
// re-verifies before a grant.
func (s *SchedulerService) priorityNow(ctx context.Context, issueID string) (int, string) {
	if s.jira == nil {
		return domain.PriorityRank(""), ""
	}
	data, err := s.jira.FetchSchedulingData(ctx, issueID)
	if err != nil {
		return domain.PriorityRank(""), ""
	}
	return domain.PriorityRank(data.PriorityName), data.PriorityName
}

// Admit runs one admission pass over every schedulable entry. Jira calls
// happen BEFORE the granting transaction — no database transaction is ever
// held across a Jira call (the ticket's explicit constraint).
func (s *SchedulerService) Admit(ctx context.Context, worker string) (AdmitResult, error) {
	if worker == "" {
		return AdmitResult{}, fmt.Errorf("scheduler: worker is required")
	}
	entries, err := s.store.ListSchedulable(ctx)
	if err != nil {
		return AdmitResult{}, err
	}

	var candidates []domain.Candidate
	result := AdmitResult{Evaluated: len(entries)}

	for _, e := range entries {
		eligible, reason := s.evaluateEligibility(ctx, &e)
		if eligible {
			candidates = append(candidates, domain.Candidate{
				QueueID:      e.QueueID,
				RunID:        e.RunID,
				TaskID:       e.TaskID,
				PriorityRank: e.PriorityRank,
				FifoSeq:      e.FifoSeq,
			})
			continue
		}
		// Ineligible: persist the blocked state + reason so the wait is
		// auditable and re-checkable (AC-05). Entries already blocked on the
		// same class need no rewrite.
		var status domain.QueueStatus
		switch {
		case errors.Is(reason, domain.ErrDependencyBlocked):
			status = domain.QueueBlockedDependency
		default:
			status = domain.QueueBlockedUnverified
		}
		if e.Status != status || e.BlockedReason != reason.Error() {
			if err := s.store.SetQueueBlocked(ctx, e.QueueID, status, reason.Error(), s.actor); err != nil {
				return result, err
			}
			result.Blocked = append(result.Blocked, e.QueueID)
		}
	}

	domain.AdmissionOrder(candidates)
	ordered := make([]string, len(candidates))
	for i, c := range candidates {
		ordered[i] = c.QueueID
	}

	grants, err := s.store.GrantPass(ctx, worker, ordered, s.capacity, s.actor)
	if err != nil {
		return result, err
	}
	result.Granted = grants
	result.Capacity = s.capacity - len(grants)

	for _, g := range grants {
		s.logger.Info("scheduler slot granted",
			"metric", "scheduler_slot_granted",
			"run_id", g.RunID,
			"queue_id", g.QueueID,
			"grant_id", g.GrantID,
			"worker", g.WorkerOwner,
		)
	}
	return result, nil
}

// evaluateEligibility re-reads the Jira admission inputs for one entry:
// refresh the stored priority, then resolve every "is blocked by" link. Any
// unfinished blocker or any read failure means "not eligible" — verified
// admission only.
func (s *SchedulerService) evaluateEligibility(ctx context.Context, e *QueueEntryView) (bool, error) {
	if s.jira == nil {
		return false, fmt.Errorf("%w: no Jira provider configured", domain.ErrUnverified)
	}
	data, err := s.jira.FetchSchedulingData(ctx, e.JiraIssueID)
	if err != nil {
		return false, fmt.Errorf("%w: jira read failed for %s: %v", domain.ErrUnverified, e.JiraIssueKey, err)
	}
	rank := domain.PriorityRank(data.PriorityName)
	if rank != e.PriorityRank || data.PriorityName != e.PriorityName {
		if err := s.store.SetQueuePriority(ctx, e.QueueID, rank, data.PriorityName); err != nil {
			return false, fmt.Errorf("%w: persist priority: %v", domain.ErrUnverified, err)
		}
		e.PriorityRank = rank
		e.PriorityName = data.PriorityName
	}
	for _, b := range data.BlockedBy {
		if !b.Done {
			return false, fmt.Errorf("%w: %s is blocked by %s (%s)",
				domain.ErrDependencyBlocked, e.JiraIssueKey, b.IssueKey, b.StatusName)
		}
	}
	return true, nil
}

// ConfirmAgentStopped releases exactly one slot. The caller presents the
// run_id + grant_id the agent was started under; a stale, duplicate, or late
// confirmation releases nothing and is still audited (AC-08).
func (s *SchedulerService) ConfirmAgentStopped(ctx context.Context, runID, grantID, reason string) (ReleaseResult, error) {
	res, err := s.store.ConfirmStop(ctx, runID, grantID, sanitizeReason(reason), s.actor)
	if err != nil {
		return ReleaseResult{}, err
	}
	s.logger.Info("scheduler slot stop confirmed",
		"metric", "scheduler_slot_released",
		"run_id", res.RunID,
		"grant_id", res.GrantID,
		"released", res.Released,
	)
	return res, nil
}

// RecordHeartbeat refreshes a held slot's heartbeat. A missing heartbeat is
// NOT a release trigger — see SweepStaleHeartbeats.
func (s *SchedulerService) RecordHeartbeat(ctx context.Context, runID, grantID string) error {
	return s.store.RecordHeartbeat(ctx, runID, grantID)
}

// SweepStaleHeartbeats flags held slots whose last heartbeat is older than
// maxAge. The slot is RETAINED (AC-07): an unconfirmable worker blocks and
// waits for a human, it is never auto-released.
func (s *SchedulerService) SweepStaleHeartbeats(ctx context.Context, maxAge time.Duration) ([]string, error) {
	stale, err := s.store.MarkStaleSuspected(ctx, s.clock.Now().Add(-maxAge))
	if err != nil {
		return nil, err
	}
	for _, runID := range stale {
		s.logger.Warn("scheduler slot heartbeat lost — slot retained, awaiting operator",
			"metric", "scheduler_slot_suspected",
			"run_id", runID,
		)
	}
	return stale, nil
}

// CancelQueued cancels a waiting/blocked entry. An allocated entry rejects
// with ErrSlotHeld — cancelling a running coding task goes through agent
// stop + ConfirmAgentStopped (AC-06).
func (s *SchedulerService) CancelQueued(ctx context.Context, runID, reason string) (QueueEntryView, error) {
	entry, err := s.store.CancelQueued(ctx, runID, sanitizeReason(reason), s.actor)
	if err != nil {
		return QueueEntryView{}, err
	}
	s.logger.Info("scheduler queue entry cancelled",
		"metric", "scheduler_queue_cancelled",
		"run_id", entry.RunID,
		"queue_id", entry.QueueID,
	)
	return entry, nil
}

// Metrics returns the live counter snapshot (AC-10): waiting / allocated /
// blocked (split dependency vs unverified) / held / suspected.
func (s *SchedulerService) Metrics(ctx context.Context) (SchedulerMetrics, error) {
	m, err := s.store.Metrics(ctx)
	if err != nil {
		return SchedulerMetrics{}, err
	}
	m.SlotsMax = int64(s.capacity)
	return m, nil
}
