package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vtx/platform/internal/application"
)

// VTX-4 worktree API. Same trust model as the registry surface: mutation
// callers are internal processes (VTX-5/VTX-7 later), actor comes from the
// authenticated token. Allocation is idempotent per run.

type worktreeResponse struct {
	WorktreeID       string     `json:"worktree_id"`
	RunID            string     `json:"run_id"`
	TaskID           string     `json:"task_id"`
	Status           string     `json:"status"`
	Path             string     `json:"path"`
	Branch           string     `json:"branch"`
	RepoURL          string     `json:"repo_url"`
	BaseSHA          *string    `json:"base_sha,omitempty"`
	QuarantineReason *string    `json:"quarantine_reason,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ReleasedAt       *time.Time `json:"released_at,omitempty"`
}

// WorktreeService is the surface the handlers need (application.WorktreeService).
type WorktreeService interface {
	AllocateWorktree(ctx context.Context, cmd application.AllocateCommand) (application.Allocation, error)
	ReleaseWorktree(ctx context.Context, runID, reason string) (application.WorktreeView, error)
	GetWorktree(ctx context.Context, runID string) (application.WorktreeView, error)
	ListAllocated(ctx context.Context) ([]application.AllocatedWorktreeView, error)
}

// WithWorktree binds the VTX-4 surface; routes register only when set.
func (s *Server) WithWorktree(svc WorktreeService) *Server {
	s.worktree = svc
	return s
}

func toWorktreeResponse(wt application.WorktreeView) worktreeResponse {
	return worktreeResponse{
		WorktreeID:       wt.WorktreeID,
		RunID:            wt.RunID,
		TaskID:           wt.TaskID,
		Status:           wt.Status,
		Path:             wt.Path,
		Branch:           wt.Branch,
		RepoURL:          wt.RepoURL,
		BaseSHA:          wt.BaseSHA,
		QuarantineReason: wt.QuarantineReason,
		CreatedAt:        wt.CreatedAt,
		UpdatedAt:        wt.UpdatedAt,
		ReleasedAt:       wt.ReleasedAt,
	}
}

type allocateRequest struct {
	RunID   string `json:"run_id"`
	RepoURL string `json:"repo_url"`
	BaseRef string `json:"base_ref"`
}

// HandleWorktreeAllocate serves POST /api/v1/worktrees/allocate.
func (s *Server) HandleWorktreeAllocate(w http.ResponseWriter, r *http.Request) {
	body, err := readLimitedJSON(w, r)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req allocateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "body is not valid JSON")
		return
	}
	if req.RunID == "" {
		writeRegistryError(w, http.StatusBadRequest, "invalid_request", "run_id is required")
		return
	}
	alloc, err := s.worktree.AllocateWorktree(r.Context(), application.AllocateCommand{
		RunID:   req.RunID,
		RepoURL: req.RepoURL,
		BaseRef: req.BaseRef,
	})
	if err != nil {
		switch {
		case errors.Is(err, application.ErrRunNotFound):
			writeRegistryError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, application.ErrAdmissionCap):
			writeRegistryError(w, http.StatusConflict, "admission_full", "all three execution slots are occupied")
		case errors.Is(err, application.ErrWorktreeRunTerminal):
			writeRegistryError(w, http.StatusConflict, "run_terminal", err.Error())
		case errors.Is(err, application.ErrWorktreeNoRepo):
			writeRegistryError(w, http.StatusUnprocessableEntity, "no_repo_url", err.Error())
		default:
			s.logger.Error("worktree allocate failed", "error", err)
			writeRegistryError(w, http.StatusInternalServerError, "internal", "allocation failed")
		}
		return
	}
	resp := struct {
		worktreeResponse
		LeaseOwner string `json:"lease_owner"`
		LeaseEpoch int64  `json:"lease_epoch"`
		Existing   bool   `json:"existing"`
	}{
		worktreeResponse: toWorktreeResponse(alloc.Worktree),
		LeaseOwner:       alloc.LeaseOwner,
		LeaseEpoch:       alloc.LeaseEpoch,
		Existing:         alloc.Existing,
	}
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusCreated
	if alloc.Existing {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleWorktreeGet serves GET /api/v1/worktrees/runs/{run_id}.
func (s *Server) HandleWorktreeGet(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	wt, err := s.worktree.GetWorktree(r.Context(), runID)
	if err != nil {
		if errors.Is(err, application.ErrWorktreeNotFound) {
			writeRegistryError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeRegistryError(w, http.StatusInternalServerError, "internal", "read failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toWorktreeResponse(wt))
}

// HandleWorktreeRelease serves POST /api/v1/worktrees/runs/{run_id}/release.
func (s *Server) HandleWorktreeRelease(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	var req struct {
		Reason string `json:"reason"`
	}
	if body, err := readLimitedJSON(w, r); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	wt, err := s.worktree.ReleaseWorktree(r.Context(), runID, req.Reason)
	if err != nil {
		if errors.Is(err, application.ErrWorktreeNotFound) {
			writeRegistryError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		s.logger.Error("worktree release failed", "run_id", runID, "error", err)
		writeRegistryError(w, http.StatusInternalServerError, "internal", "release failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toWorktreeResponse(wt))
}

// HandleWorktreeListAllocated serves GET /api/v1/worktrees (allocated only).
func (s *Server) HandleWorktreeListAllocated(w http.ResponseWriter, r *http.Request) {
	rows, err := s.worktree.ListAllocated(r.Context())
	if err != nil {
		writeRegistryError(w, http.StatusInternalServerError, "internal", "list failed")
		return
	}
	out := make([]worktreeResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, toWorktreeResponse(row.WorktreeView))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
