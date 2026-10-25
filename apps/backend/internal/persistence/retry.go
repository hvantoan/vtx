package persistence

import (
	"context"
	"fmt"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence/db"
	"strings"
	"time"
)

// DeliveryRedriveAudit is the VTX-13 integration seam. Its operator-side audit
// writes MUST use the supplied tx-bound Queries; failure rolls back redrive.
type DeliveryRedriveAudit interface {
	RecordDeliveryRedrive(context.Context, *db.Queries, string, string, string, string) error
}
type PgRetryStore struct {
	pool    txBeginner
	queries *db.Queries
	Audit   DeliveryRedriveAudit
}

func NewPgRetryStore(pool txBeginner, q *db.Queries) *PgRetryStore {
	return &PgRetryStore{pool: pool, queries: q}
}
func (s *PgRetryStore) DueForRetry(ctx context.Context, now time.Time, limit int32) ([]application.RunSnapshot, error) {
	rows, err := s.queries.ListRetryWaitRunsDue(ctx, db.ListRetryWaitRunsDueParams{Now: timestamptzFrom(&now), RowLimit: limit})
	if err != nil {
		return nil, err
	}
	result := make([]application.RunSnapshot, 0, len(rows))
	for _, r := range rows {
		result = append(result, snapshotFromTaskRun(db.TaskRun(r)))
	}
	return result, nil
}
func (s *PgRetryStore) ExpiredLeaseRuns(ctx context.Context, now time.Time, limit int32) ([]application.RunSnapshot, error) {
	rows, err := s.queries.ListExpiredLeaseRuns(ctx, db.ListExpiredLeaseRunsParams{Now: timestamptzFrom(&now), RowLimit: limit})
	if err != nil {
		return nil, err
	}
	result := make([]application.RunSnapshot, 0, len(rows))
	for _, r := range rows {
		result = append(result, snapshotFromTaskRun(db.TaskRun(r)))
	}
	return result, nil
}
func (s *PgRetryStore) RedriveDelivery(ctx context.Context, eventID, consumer, actor, reason, correlationID string) (bool, error) {
	if actor != "operator" {
		return false, domain.ErrForbidden
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" || len(reason) > 500 || consumer == "" {
		return false, fmt.Errorf("redrive requires consumer and bounded reason: %w", domain.ErrInvalidTransition)
	}
	id, err := uuidFromString(eventID)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)
	n, err := q.RedriveDeadLetteredEvent(ctx, id)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	event, err := q.GetOutboxEvent(ctx, id)
	if err != nil {
		return false, err
	}
	audit, err := q.InsertDeliveryRedrive(ctx, db.InsertDeliveryRedriveParams{OutboxEventID: id, ConsumerName: consumer, Actor: actor, ActorType: "human", Reason: reason, CorrelationID: textFromOptional(correlationID)})
	if err != nil {
		return false, err
	}
	if s.Audit != nil {
		if err = s.Audit.RecordDeliveryRedrive(ctx, q, eventID, actor, reason, correlationID); err != nil {
			return false, err
		}
	}
	payload, err := jsonBytes(map[string]any{"outbox_event_id": eventID, "event_type": event.EventType, "actor_type": "human", "redrive_id": uuidToString(audit.RedriveID), "consumer_name": consumer, "actor": actor, "reason": reason, "correlation_id": correlationID, "occurred_at": audit.RedrivenAt.Time.Format(time.RFC3339Nano)})
	if err != nil {
		return false, err
	}
	_, err = q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{IntentID: event.IntentID, EventType: "outbox.delivery_redriven.v1", Payload: payload})
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
