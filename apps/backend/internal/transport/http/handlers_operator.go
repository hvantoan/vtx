package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
)

type OperatorService interface {
	Overview(context.Context) (application.OperatorOverview, error)
	ListRuns(context.Context, application.RunFilter) ([]application.OperatorRunView, error)
	GetRunDetail(context.Context, string) (application.RunDetailView, error)
	OutboxStats(context.Context) ([]application.OutboxTypeStat, error)
	ListDeadLettered(context.Context, *string, int32) ([]application.DlqItem, error)
	Workers(context.Context) ([]application.WorkerHeartbeatView, error)
	Redrive(context.Context, string, application.OperatorAuditRecord) error
}
type PullRequestService interface {
	GetPullRequest(context.Context, string) (application.PullRequestView, error)
	ListPullRequests(context.Context, *string, *time.Time, int32) ([]application.PullRequestView, error)
	SyncPullRequest(context.Context, string) (application.PullRequestView, error)
}

func (s *Server) WithOperator(v OperatorService) *Server        { s.operator = v; return s }
func (s *Server) WithPullRequests(v PullRequestService) *Server { s.pullRequests = v; return s }

func (s *Server) HandleOperatorOverview(w http.ResponseWriter, r *http.Request) {
	if !allowOperatorRead(w, r) {
		return
	}
	v, e := s.operator.Overview(r.Context())
	s.writeOperatorResult(w, v, e)
}
func (s *Server) HandleOperatorRuns(w http.ResponseWriter, r *http.Request) {
	if !allowOperatorRead(w, r) {
		return
	}
	limit, ok := operatorLimit(w, r)
	if !ok {
		return
	}
	f := application.RunFilter{Limit: limit}
	if raw := r.URL.Query().Get("state"); raw != "" {
		state := domain.State(raw)
		if !state.Valid() {
			writeRegistryError(w, 400, "invalid_request", "invalid state")
			return
		}
		f.State = &state
	}
	if raw := r.URL.Query().Get("issue_key"); raw != "" {
		f.IssueKey = &raw
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		parts := strings.SplitN(raw, "|", 2)
		if len(parts) != 2 {
			writeRegistryError(w, 400, "invalid_request", "invalid cursor")
			return
		}
		tm, e := time.Parse(time.RFC3339Nano, parts[0])
		if e != nil {
			writeRegistryError(w, 400, "invalid_request", "invalid cursor")
			return
		}
		f.BeforeUpdatedAt = &tm
		f.BeforeRunID = &parts[1]
	}
	rows, e := s.operator.ListRuns(r.Context(), f)
	if e != nil {
		s.writeOperatorResult(w, nil, e)
		return
	}
	var next *string
	if int32(len(rows)) == limit && len(rows) > 0 {
		v := rows[len(rows)-1].UpdatedAt.Format(time.RFC3339Nano) + "|" + rows[len(rows)-1].RunID
		next = &v
	}
	writeRegistryJSON(w, 200, map[string]any{"items": rows, "next_cursor": next})
}
func (s *Server) HandleOperatorRunDetail(w http.ResponseWriter, r *http.Request) {
	if !allowOperatorRead(w, r) {
		return
	}
	v, e := s.operator.GetRunDetail(r.Context(), chi.URLParam(r, "run_id"))
	s.writeOperatorResult(w, v, e)
}
func (s *Server) HandleOperatorOutbox(w http.ResponseWriter, r *http.Request) {
	if !allowOperatorRead(w, r) {
		return
	}
	rows, e := s.operator.OutboxStats(r.Context())
	if e != nil {
		s.writeOperatorResult(w, nil, e)
		return
	}
	out := make(map[string]application.OutboxTypeStat, len(rows))
	for _, v := range rows {
		out[v.EventType] = v
	}
	writeRegistryJSON(w, 200, out)
}
func (s *Server) HandleOperatorDLQ(w http.ResponseWriter, r *http.Request) {
	if !allowOperatorRead(w, r) {
		return
	}
	limit, ok := operatorLimit(w, r)
	if !ok {
		return
	}
	var after *string
	if raw := r.URL.Query().Get("after"); raw != "" {
		after = &raw
	}
	rows, e := s.operator.ListDeadLettered(r.Context(), after, limit)
	if e != nil {
		s.writeOperatorResult(w, nil, e)
		return
	}
	var next *string
	if int32(len(rows)) == limit && len(rows) > 0 {
		v := rows[len(rows)-1].OutboxID
		next = &v
	}
	writeRegistryJSON(w, 200, map[string]any{"items": rows, "next_cursor": next})
}
func (s *Server) HandleOperatorWorkers(w http.ResponseWriter, r *http.Request) {
	if !allowOperatorRead(w, r) {
		return
	}
	rows, e := s.operator.Workers(r.Context())
	if e != nil {
		s.writeOperatorResult(w, nil, e)
		return
	}
	writeRegistryJSON(w, 200, map[string]any{"items": rows})
}
func (s *Server) HandleOperatorRedrive(w http.ResponseWriter, r *http.Request) {
	actor, ok := RegistryActorFromContext(r.Context())
	if !ok {
		writeRegistryError(w, 401, "unauthorized", "no authenticated actor")
		return
	}
	if actor != domain.ActorOperator {
		writeRegistryError(w, 403, "forbidden", "operator actor required")
		return
	}
	body, e := readLimitedJSON(w, r)
	if e != nil {
		writeRegistryError(w, 400, "invalid_request", e.Error())
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &req) != nil {
		writeRegistryError(w, 400, "invalid_request", "body is not valid JSON")
		return
	}
	reason := sanitizeHeaderValue(req.Reason, 500)
	if reason == "" {
		writeRegistryError(w, 400, "invalid_request", "reason is required")
		return
	}
	id := chi.URLParam(r, "outbox_id")
	e = s.operator.Redrive(r.Context(), id, application.OperatorAuditRecord{Actor: string(actor), OperatorID: operatorID(r), Reason: reason, RequestID: chimw.GetReqID(r.Context())})
	if errors.Is(e, application.ErrNotDeadLettered) {
		writeRegistryError(w, 409, "not_dead_lettered", "event is not dead-lettered")
		return
	}
	if e != nil {
		s.writeOperatorResult(w, nil, e)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
func (s *Server) HandleGetRunPullRequest(w http.ResponseWriter, r *http.Request) {
	v, e := s.pullRequests.GetPullRequest(r.Context(), chi.URLParam(r, "run_id"))
	s.writeOperatorResult(w, v, e)
}
func (s *Server) HandleListPullRequests(w http.ResponseWriter, r *http.Request) {
	limit, ok := operatorLimit(w, r)
	if !ok {
		return
	}
	var status *string
	if raw := r.URL.Query().Get("status"); raw != "" {
		switch raw {
		case "requested", "open", "closed", "merged", "failed":
			status = &raw
		default:
			writeRegistryError(w, 400, "invalid_request", "invalid status")
			return
		}
	}
	var before *time.Time
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		v, e := time.Parse(time.RFC3339Nano, raw)
		if e != nil {
			writeRegistryError(w, 400, "invalid_request", "invalid cursor")
			return
		}
		before = &v
	}
	rows, e := s.pullRequests.ListPullRequests(r.Context(), status, before, limit)
	if e != nil {
		s.writeOperatorResult(w, nil, e)
		return
	}
	var next *string
	if int32(len(rows)) == limit && len(rows) > 0 {
		v := rows[len(rows)-1].CreatedAt.Format(time.RFC3339Nano)
		next = &v
	}
	writeRegistryJSON(w, 200, map[string]any{"items": rows, "next_cursor": next})
}
func (s *Server) HandleSyncPullRequest(w http.ResponseWriter, r *http.Request) {
	actor, _ := RegistryActorFromContext(r.Context())
	if actor != domain.ActorOperator && actor != domain.ActorPolicy {
		writeRegistryError(w, 403, "forbidden", "actor is not permitted")
		return
	}
	v, e := s.pullRequests.SyncPullRequest(r.Context(), chi.URLParam(r, "run_id"))
	s.writeOperatorResult(w, v, e)
}

func (s *Server) writeOperatorResult(w http.ResponseWriter, v any, e error) {
	if e == nil {
		writeRegistryJSON(w, 200, v)
		return
	}
	switch {
	case errors.Is(e, application.ErrReadNotFound), errors.Is(e, application.ErrPRNotFound):
		writeRegistryError(w, 404, "not_found", "resource not found")
	default:
		s.logger.Error("operator API failed", "error", e)
		writeRegistryError(w, 500, "internal_error", "internal error")
	}
}
func allowOperatorRead(w http.ResponseWriter, r *http.Request) bool {
	a, ok := RegistryActorFromContext(r.Context())
	if !ok {
		writeRegistryError(w, 401, "unauthorized", "no authenticated actor")
		return false
	}
	if a != domain.ActorOperator && a != domain.ActorPolicy {
		writeRegistryError(w, 403, "forbidden", "actor is not permitted")
		return false
	}
	return true
}
func operatorLimit(w http.ResponseWriter, r *http.Request) (int32, bool) {
	n := int64(50)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var e error
		n, e = strconv.ParseInt(raw, 10, 32)
		if e != nil || n < 1 || n > 100 {
			writeRegistryError(w, 400, "invalid_request", "limit must be between 1 and 100")
			return 0, false
		}
	}
	return int32(n), true
}
func operatorID(r *http.Request) string {
	return sanitizeHeaderValue(r.Header.Get("X-Operator-Id"), 64)
}
func sanitizeHeaderValue(v string, max int) string {
	v = strings.Join(strings.Fields(v), " ")
	if len(v) > max {
		v = v[:max]
	}
	return v
}
