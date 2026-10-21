package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/domain"
)

// VTX-3 registry API (§0.D). Mutation callers are internal processes (the
// VTX-5 scheduler later); reads serve the cockpit. The actor always comes from
// the authenticated token, never from the body.

const (
	maxRegistryBody  = 32 * 1024
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// RegistryReader is the read surface the handlers need.
type RegistryReader interface {
	GetTask(ctx context.Context, siteID, issueID string) (application.TaskView, error)
	GetRun(ctx context.Context, runID string) (application.RunView, error)
	ListEvents(ctx context.Context, runID string, afterVersion *int64, limit int32) ([]application.EventView, error)
}

// RegistryTransitioner is the mutation surface (application.RegistryService).
type RegistryTransitioner interface {
	TransitionRun(ctx context.Context, cmd application.TransitionCommand) (application.RunSnapshot, error)
}

// WithRegistry binds the registry surfaces. Routes are only registered when
// both are present, so the dashboard-only bootstrap stays valid.
func (s *Server) WithRegistry(reader RegistryReader, transitioner RegistryTransitioner) *Server {
	s.registryReader = reader
	s.registryTransitions = transitioner
	return s
}

type transitionRequest struct {
	ExpectedVersion    int64           `json:"expected_version"`
	ExpectedLeaseEpoch int64           `json:"expected_lease_epoch"`
	Action             string          `json:"action"`
	Reason             string          `json:"reason"`
	Metadata           json.RawMessage `json:"metadata"`
	// Actor is accepted only so an impersonation attempt can be rejected with
	// 403 instead of being silently ignored. It is never a source of truth.
	Actor string `json:"actor"`
}

// HandleRegistryTransition serves POST /api/v1/registry/runs/{run_id}/transitions.
func (s *Server) HandleRegistryTransition(w http.ResponseWriter, r *http.Request) {
	actor, ok := RegistryActorFromContext(r.Context())
	if !ok {
		writeRegistryError(w, http.StatusUnauthorized, "unauthorized", "no authenticated actor")
		return
	}
	runID := chi.URLParam(r, "run_id")
	if runID == "" {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "run_id is required")
		return
	}

	body, err := readLimitedJSON(w, r)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req transitionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "body is not valid JSON")
		return
	}
	if req.Actor != "" && req.Actor != string(actor) {
		// TR-23: a caller may not act as another principal.
		writeRegistryError(w, http.StatusForbidden, "forbidden", "actor does not match the authenticated principal")
		return
	}
	action := domain.Action(req.Action)
	if !action.Valid() {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "unknown action")
		return
	}
	if req.ExpectedVersion < 0 {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "expected_version must be >= 0")
		return
	}

	run, err := s.registryTransitions.TransitionRun(r.Context(), application.TransitionCommand{
		RunID:              runID,
		ExpectedVersion:    req.ExpectedVersion,
		ExpectedLeaseEpoch: req.ExpectedLeaseEpoch,
		Action:             action,
		Actor:              actor,
		ActorType:          actorTypeFor(actor),
		Reason:             req.Reason,
		Metadata:           req.Metadata,
	})
	if err != nil {
		s.writeRegistryServiceError(w, err)
		return
	}
	writeRegistryJSON(w, http.StatusOK, runResponseOf(run))
}

// HandleRegistryGetRun serves GET /api/v1/registry/runs/{run_id}.
func (s *Server) HandleRegistryGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.registryReader.GetRun(r.Context(), chi.URLParam(r, "run_id"))
	if err != nil {
		s.writeRegistryReadError(w, err)
		return
	}
	writeRegistryJSON(w, http.StatusOK, runViewResponseOf(run))
}

// HandleRegistryListEvents serves GET /api/v1/registry/runs/{run_id}/events.
// Cursor is the audit version (TR-23): stable when timestamps collide.
func (s *Server) HandleRegistryListEvents(w http.ResponseWriter, r *http.Request) {
	afterVersion, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	events, err := s.registryReader.ListEvents(r.Context(), chi.URLParam(r, "run_id"), afterVersion, limit)
	if err != nil {
		s.writeRegistryReadError(w, err)
		return
	}
	out := make([]eventResponse, 0, len(events))
	for _, event := range events {
		out = append(out, eventViewResponse(event))
	}
	writeRegistryJSON(w, http.StatusOK, pageResponse{Items: out, NextCursor: nextCursor(len(events), limit, lastVersion(events))})
}

// HandleRegistryGetTask serves GET /api/v1/registry/tasks/{site_id}/{issue_id}.
func (s *Server) HandleRegistryGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.registryReader.GetTask(r.Context(), chi.URLParam(r, "site_id"), chi.URLParam(r, "issue_id"))
	if err != nil {
		s.writeRegistryReadError(w, err)
		return
	}
	writeRegistryJSON(w, http.StatusOK, taskViewResponseOf(task))
}

func (s *Server) writeRegistryServiceError(w http.ResponseWriter, err error) {
	status := domain.HTTPStatusForError(err)
	code := "internal_error"
	switch {
	case errors.Is(err, domain.ErrNotFound):
		code = "not_found"
	case errors.Is(err, domain.ErrStaleVersion):
		code = "stale_version"
	case errors.Is(err, domain.ErrStaleLease):
		code = "stale_lease"
	case errors.Is(err, domain.ErrAlreadyTerminal):
		code = "already_terminal"
	case errors.Is(err, domain.ErrInvalidTransition):
		code = "invalid_transition"
	case errors.Is(err, domain.ErrConflictActiveRun):
		code = "conflict_active_run"
	case errors.Is(err, domain.ErrStageNotOperational):
		code = "stage_not_operational"
	case errors.Is(err, domain.ErrForbidden):
		code = "forbidden"
	}
	// Structured, sanitized observation: no token, no raw body. `status` and
	// `error_category` are the two fields a log-based counter needs.
	s.logger.Warn("registry transition outcome",
		"metric", "registry_transition",
		"status", status,
		"error_category", code,
		"error", err.Error(),
	)
	writeRegistryError(w, status, code, registryErrorMessage(code))
}

func (s *Server) writeRegistryReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, application.ErrReadNotFound) {
		writeRegistryError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	s.logger.Error("registry read failed", "error", err)
	writeRegistryError(w, http.StatusInternalServerError, "internal_error", "internal error")
}

// registryErrorMessage keeps client-visible text stable; the underlying error
// may carry internal detail and is logged instead.
func registryErrorMessage(code string) string {
	switch code {
	case "not_found":
		return "run not found"
	case "stale_version":
		return "expected_version does not match the current run version"
	case "stale_lease":
		return "lease is missing or expired"
	case "already_terminal":
		return "run is already terminal"
	case "invalid_transition":
		return "transition is not valid for the current state"
	case "conflict_active_run":
		return "task already has an active run"
	case "stage_not_operational":
		return "transition target is not operational in phase 1"
	case "forbidden":
		return "actor is not permitted to perform this action"
	default:
		return "internal error"
	}
}

func actorTypeFor(actor domain.Actor) domain.ActorType {
	// Only the operator is a human principal in phase 1.
	if actor == domain.ActorOperator {
		return domain.ActorTypeHuman
	}
	return domain.ActorTypeSystem
}

type runResponse struct {
	RunID          string  `json:"run_id"`
	TaskID         string  `json:"task_id"`
	State          string  `json:"state"`
	Version        int64   `json:"version"`
	Attempt        int32   `json:"attempt"`
	ResumeState    *string `json:"resume_state,omitempty"`
	RetryDueAt     *string `json:"retry_due_at,omitempty"`
	CommitSHA      *string `json:"commit_sha,omitempty"`
	Branch         *string `json:"branch,omitempty"`
	ErrorCode      *string `json:"error_code,omitempty"`
	ErrorMessage   *string `json:"error_message,omitempty"`
	LeaseOwner     *string `json:"lease_owner,omitempty"`
	LeaseEpoch     int64   `json:"lease_epoch"`
	LeaseExpiresAt *string `json:"lease_expires_at,omitempty"`
	CorrelationID  string  `json:"correlation_id,omitempty"`
	CreatedAt      string  `json:"created_at,omitempty"`
	UpdatedAt      string  `json:"updated_at,omitempty"`
	StartedAt      *string `json:"started_at,omitempty"`
	FinishedAt     *string `json:"finished_at,omitempty"`
}

func runResponseOf(run application.RunSnapshot) runResponse {
	return runResponse{
		RunID: run.RunID, TaskID: run.TaskID, State: string(run.State),
		Version: run.Version, Attempt: run.Attempt,
		ResumeState: stateString(run.ResumeState),
		RetryDueAt:  timeString(run.RetryDueAt),
		CommitSHA:   run.CommitSHA, LeaseOwner: run.LeaseOwner,
		LeaseEpoch: run.LeaseEpoch, LeaseExpiresAt: timeString(run.LeaseExpiresAt),
		CorrelationID: run.CorrelationID,
		StartedAt:     timeString(run.StartedAt), FinishedAt: timeString(run.FinishedAt),
	}
}

func runViewResponseOf(run application.RunView) runViewResponse {
	return runViewResponse{
		RunID: run.RunID, TaskID: run.TaskID, State: string(run.State),
		Version: run.Version, Attempt: run.Attempt,
		ResumeState: stateString(run.ResumeState),
		RetryDueAt:  timeString(run.RetryDueAt),
		CommitSHA:   run.CommitSHA, Branch: run.Branch,
		ErrorCode: run.ErrorCode, ErrorMessage: run.ErrorMessage,
		LeaseOwner: run.LeaseOwner, LeaseEpoch: run.LeaseEpoch,
		LeaseExpiresAt: timeString(run.LeaseExpiresAt),
		CorrelationID:  run.CorrelationID,
		CreatedAt:      run.CreatedAt.Format(time.RFC3339),
		UpdatedAt:      run.UpdatedAt.Format(time.RFC3339),
		StartedAt:      timeString(run.StartedAt), FinishedAt: timeString(run.FinishedAt),
	}
}

type runViewResponse = runResponse

type taskViewResponse struct {
	TaskID       string `json:"task_id"`
	JiraSiteID   string `json:"jira_site_id"`
	JiraIssueID  string `json:"jira_issue_id"`
	JiraIssueKey string `json:"jira_issue_key"`
	ProjectKey   string `json:"project_key"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func taskViewResponseOf(task application.TaskView) taskViewResponse {
	return taskViewResponse{
		TaskID: task.TaskID, JiraSiteID: task.JiraSiteID, JiraIssueID: task.JiraIssueID,
		JiraIssueKey: task.JiraIssueKey, ProjectKey: task.ProjectKey,
		CreatedAt: task.CreatedAt.Format(time.RFC3339), UpdatedAt: task.UpdatedAt.Format(time.RFC3339),
	}
}

type eventResponse struct {
	PreviousState string          `json:"previous_state"`
	CurrentState  string          `json:"current_state"`
	Action        string          `json:"action"`
	Actor         string          `json:"actor"`
	ActorType     string          `json:"actor_type"`
	Version       int64           `json:"version"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	CreatedAt     string          `json:"created_at"`
}

func eventViewResponse(event application.EventView) eventResponse {
	return eventResponse{
		PreviousState: string(event.PreviousState), CurrentState: string(event.CurrentState),
		Action: string(event.Action), Actor: string(event.Actor), ActorType: string(event.ActorType),
		Version: event.Version, Metadata: event.Metadata,
		CreatedAt: event.CreatedAt.Format(time.RFC3339),
	}
}

type pageResponse struct {
	Items      any    `json:"items"`
	NextCursor *int64 `json:"next_cursor,omitempty"`
}

func nextCursor(got int, limit int32, last int64) *int64 {
	if int32(got) < limit {
		return nil
	}
	return &last
}

func lastVersion(events []application.EventView) int64 {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Version
}

// pageParams parses ?after_version=&limit= and writes the error itself.
func pageParams(w http.ResponseWriter, r *http.Request) (*int64, int32, bool) {
	// nil means "from the beginning": the creation audit lives at version 0, so
	// defaulting the cursor to 0 would hide it.
	var afterVersion *int64
	if raw := r.URL.Query().Get("after_version"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeRegistryError(w, http.StatusBadRequest, "invalid_request", "after_version must be a non-negative integer")
			return nil, 0, false
		}
		afterVersion = &parsed
	}
	limit := int32(defaultPageLimit)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > maxPageLimit {
			writeRegistryError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 200")
			return nil, 0, false
		}
		limit = int32(parsed)
	}
	return afterVersion, limit, true
}

func readLimitedJSON(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
		return nil, errors.New("Content-Type must be application/json")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRegistryBody))
	if err != nil {
		return nil, errors.New("request body too large or unreadable")
	}
	if len(body) == 0 {
		return nil, errors.New("request body is empty")
	}
	return body, nil
}

func stateString(state *domain.State) *string {
	if state == nil {
		return nil
	}
	value := string(*state)
	return &value
}

func timeString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

type registryErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeRegistryError(w http.ResponseWriter, status int, code, message string) {
	writeRegistryJSON(w, status, registryErrorResponse{Error: code, Message: message})
}

func writeRegistryJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
