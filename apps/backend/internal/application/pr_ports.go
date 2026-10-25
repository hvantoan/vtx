package application

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// GitHubPort deliberately has no merge or close mutation. The only write is a
// draft-only creation operation whose adapter hardcodes draft=true.
type GitHubPort interface {
	GetRepo(ctx context.Context, owner, repo string) (RepoRef, error)
	GetRefSHA(ctx context.Context, owner, repo, branch string) (string, error)
	GetCommit(ctx context.Context, owner, repo, sha string) (CommitInfo, error)
	ListOpenPRsForHead(ctx context.Context, owner, repo, headBranch string) ([]PRRef, error)
	CreateDraftPR(ctx context.Context, owner, repo string, in CreatePRInput) (PRRef, error)
	GetPR(ctx context.Context, owner, repo string, number int) (PRRef, error)
	GetCheckRollup(ctx context.Context, owner, repo, sha string) (CheckRollup, error)
}

type RepoRef struct{ FullName string }
type CommitInfo struct{ SHA string }
type CreatePRInput struct {
	Title string
	Body  string
	Head  string
	Base  string
}
type PRRef struct {
	ID        int64
	Number    int
	URL       string
	HeadSHA   string
	Draft     bool
	State     string
	Merged    bool
	CreatedAt time.Time
}
type CheckRollup struct{ Status string }

type PRRun struct {
	RunSnapshot
	RepoURL  string
	Branch   string
	IssueKey string
}
type GateVerdict struct {
	DecisionID    string          `json:"decision_id"`
	RunID         string          `json:"run_id"`
	CommitSHA     string          `json:"subject_commit_sha"`
	Decision      string          `json:"decision"`
	PolicyName    string          `json:"policy_name"`
	PolicyVersion string          `json:"policy_version"`
	Evidence      json.RawMessage `json:"evidence"`
	EvaluatedAt   time.Time       `json:"decided_at"`
}
type PullRequestView struct {
	PullRequestID string     `json:"pull_request_id"`
	RunID         string     `json:"run_id"`
	RepoOwner     string     `json:"repo_owner"`
	RepoName      string     `json:"repo_name"`
	Number        *int       `json:"pr_number"`
	URL           *string    `json:"pr_url"`
	Draft         bool       `json:"draft"`
	HeadBranch    string     `json:"head_branch"`
	HeadSHA       string     `json:"head_sha"`
	BaseBranch    string     `json:"base_branch"`
	Status        string     `json:"status"`
	CIStatus      *string    `json:"ci_status"`
	LastSyncedAt  *time.Time `json:"last_synced_at"`
	ErrorCode     *string    `json:"error_code"`
	CreatedAt     time.Time  `json:"created_at"`
}

type SavePullRequest struct {
	RunID, TaskID, IntentID                        string
	RepoOwner, RepoName                            string
	PR                                             PRRef
	HeadBranch, HeadSHA, BaseBranch, CorrelationID string
}

type PRStore interface {
	LoadPRRun(ctx context.Context, runID string) (PRRun, error)
	GetGateVerdict(ctx context.Context, runID, sha string) (GateVerdict, error)
	GetPullRequestByRun(ctx context.Context, runID string) (PullRequestView, error)
	SaveOpenedPullRequest(ctx context.Context, in SavePullRequest) (PullRequestView, error)
	UpdatePullRequestSync(ctx context.Context, runID, status, ciStatus string) (PullRequestView, error)
	ListPullRequests(ctx context.Context, status *string, before *time.Time, limit int32) ([]PullRequestView, error)
}

var (
	ErrPRNotFound          = errors.New("pull request not found")
	ErrGateEvidenceMissing = errors.New("gate evidence missing or not passing")
	ErrGateDecisionStale   = errors.New("gate decision is stale")
	ErrHeadSHAMismatch     = errors.New("remote head sha does not match gated sha")
	ErrRepoNotAllowed      = errors.New("repository is not allowed")
	ErrGitHubNotFound      = errors.New("github resource not found")
	ErrGitHubForbidden     = errors.New("github resource forbidden")
	ErrGitHubRateLimited   = errors.New("github rate limited")
	ErrGitHubUnavailable   = errors.New("github unavailable")
	ErrGitHubValidation    = errors.New("github validation failed")
)
