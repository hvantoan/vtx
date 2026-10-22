package domain

import "errors"

// VTX-5 / AUTO-04 scheduler domain rules. Pure data + ordering: no DB, no
// HTTP, no Jira. The admission transaction re-verifies every decision under
// the scheduler advisory lock; these helpers exist so the ordering and
// eligibility contract has exactly one definition.

// MaxCodingSlots is the system-wide coding slot budget — three slots shared
// across every project and worker. It is a single number, not per-project.
const MaxCodingSlots = 3

// QueueStatus is the lifecycle of a coding_queue row.
type QueueStatus string

const (
	QueueWaiting            QueueStatus = "waiting"
	QueueAllocated          QueueStatus = "allocated"
	QueueBlockedDependency  QueueStatus = "blocked_dependency"
	QueueBlockedUnverified  QueueStatus = "blocked_unverified"
	QueueCancelled          QueueStatus = "cancelled"
	QueueReleased           QueueStatus = "released"
)

// Scheduler errors. Typed so transports and tests classify them by identity.
var (
	ErrRunNotEligible    = errors.New("scheduler: run is not eligible for a coding slot")
	ErrQueueNotFound     = errors.New("scheduler: queue entry not found")
	ErrAlreadyQueued     = errors.New("scheduler: run already has a queue entry")
	ErrStaleGrant        = errors.New("scheduler: grant is stale or already released")
	ErrSlotHeld          = errors.New("scheduler: run holds an active slot; stop the agent first")
	ErrDependencyBlocked = errors.New("scheduler: task is blocked by an unfinished dependency")
	ErrUnverified        = errors.New("scheduler: dependency state could not be verified")
)

// PriorityRank maps a Jira priority name to the admission order the ticket
// fixes: Jira priority first, FIFO inside one priority. Lower ranks schedule
// earlier. Unknown names rank below every known priority (never silently
// promoted) but remain schedulable — a missing Jira priority is not a reason
// to starve a task.
func PriorityRank(name string) int {
	switch name {
	case "Highest":
		return 1
	case "High":
		return 2
	case "Medium":
		return 3
	case "Low":
		return 4
	case "Lowest":
		return 5
	default:
		return 6
	}
}

// Candidate is one schedulable queue entry after eligibility evaluation.
type Candidate struct {
	QueueID      string
	RunID        string
	TaskID       string
	PriorityRank int
	FifoSeq      int64
}

// AdmissionOrder sorts candidates: priority first, FIFO (fifo_seq) inside one
// priority. The sort is stable so callers can feed it in any order.
func AdmissionOrder(candidates []Candidate) {
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && Less(candidates[j], candidates[j-1]); j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
}

// Less is the strict ordering: lower priority_rank first; ties break on the
// durable fifo_seq — never on wall clock, which can tie across replicas.
func Less(a, b Candidate) bool {
	if a.PriorityRank != b.PriorityRank {
		return a.PriorityRank < b.PriorityRank
	}
	return a.FifoSeq < b.FifoSeq
}

// Grants fits the largest prefix of ordered candidates into the remaining
// capacity. A blocked head never starves the tail — the caller removes
// ineligible entries before this runs.
func Grants(candidates []Candidate, held, max int) []Candidate {
	free := max - held
	if free <= 0 {
		return nil
	}
	if free > len(candidates) {
		free = len(candidates)
	}
	return candidates[:free]
}
