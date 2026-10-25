package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/vtx/platform/internal/domain"
)

type PRTransitioner interface {
	TransitionRun(ctx context.Context, cmd TransitionCommand) (RunSnapshot, error)
}

type OpenPRCommand struct {
	RunID              string
	ExpectedVersion    int64
	ExpectedLeaseEpoch int64
	Title              string
	Body               string
}

type PRService struct {
	store       PRStore
	github      GitHubPort
	transitions PRTransitioner
	baseBranch  string
	allowlist   map[string]struct{}
}

func NewPRService(store PRStore, github GitHubPort, transitions PRTransitioner, baseBranch string, allowlist []string) *PRService {
	if baseBranch == "" {
		baseBranch = "dev"
	}
	allowed := make(map[string]struct{}, len(allowlist))
	for _, repo := range allowlist {
		if repo = strings.TrimSpace(repo); repo != "" {
			allowed[strings.ToLower(repo)] = struct{}{}
		}
	}
	return &PRService{store: store, github: github, transitions: transitions, baseBranch: baseBranch, allowlist: allowed}
}

// OpenDraftPR is the sole PR creation path. It refuses before any GitHub call
// unless the latest persisted verdict for this exact run+SHA is PASS.
func (s *PRService) OpenDraftPR(ctx context.Context, cmd OpenPRCommand) (PullRequestView, error) {
	run, err := s.store.LoadPRRun(ctx, cmd.RunID)
	if err != nil {
		return PullRequestView{}, fmt.Errorf("load PR run: %w", err)
	}
	if run.CommitSHA == nil || *run.CommitSHA == "" {
		return PullRequestView{}, fmt.Errorf("%w: run has no commit sha", ErrGateDecisionStale)
	}
	if run.State != domain.StateQualityGate && run.State != domain.StatePRDraft {
		return PullRequestView{}, fmt.Errorf("%w: run state %s", ErrGateEvidenceMissing, run.State)
	}
	verdict, err := s.store.GetGateVerdict(ctx, run.RunID, *run.CommitSHA)
	if err != nil {
		if errors.Is(err, ErrGateEvidenceMissing) {
			return PullRequestView{}, err
		}
		return PullRequestView{}, fmt.Errorf("read gate verdict: %w", err)
	}
	if verdict.CommitSHA != *run.CommitSHA {
		return PullRequestView{}, fmt.Errorf("%w: verdict %s run %s", ErrGateDecisionStale, verdict.CommitSHA, *run.CommitSHA)
	}
	if verdict.Decision != "pass" {
		return PullRequestView{}, fmt.Errorf("%w: verdict=%s", ErrGateEvidenceMissing, verdict.Decision)
	}

	owner, repo, err := parseGitHubRepo(run.RepoURL)
	if err != nil {
		return PullRequestView{}, err
	}
	if len(s.allowlist) != 0 {
		if _, ok := s.allowlist[strings.ToLower(owner+"/"+repo)]; !ok {
			return PullRequestView{}, fmt.Errorf("%w: %s/%s", ErrRepoNotAllowed, owner, repo)
		}
	}
	if _, err := s.github.GetRepo(ctx, owner, repo); err != nil {
		return PullRequestView{}, err
	}
	if _, err := s.github.GetRefSHA(ctx, owner, repo, s.baseBranch); err != nil {
		return PullRequestView{}, fmt.Errorf("base ref %s: %w", s.baseBranch, err)
	}
	headSHA, err := s.github.GetRefSHA(ctx, owner, repo, run.Branch)
	if err != nil {
		return PullRequestView{}, fmt.Errorf("head ref %s: %w", run.Branch, err)
	}
	if headSHA != *run.CommitSHA {
		return PullRequestView{}, fmt.Errorf("%w: remote=%s gated=%s", ErrHeadSHAMismatch, headSHA, *run.CommitSHA)
	}
	if _, err := s.github.GetCommit(ctx, owner, repo, *run.CommitSHA); err != nil {
		return PullRequestView{}, fmt.Errorf("get gated commit: %w", err)
	}

	currentVersion := run.Version
	if run.State == domain.StateQualityGate {
		metadata, _ := json.Marshal(map[string]string{"gate_decision_id": verdict.DecisionID, "commit_sha": *run.CommitSHA})
		advanced, err := s.transitions.TransitionRun(ctx, TransitionCommand{
			RunID: run.RunID, ExpectedVersion: cmd.ExpectedVersion,
			ExpectedLeaseEpoch: cmd.ExpectedLeaseEpoch, Action: domain.ActionAdvance,
			Actor: domain.ActorOrchestrator, ActorType: domain.ActorTypeSystem, Metadata: metadata,
		})
		if err != nil {
			return PullRequestView{}, fmt.Errorf("advance run to pr_draft: %w", err)
		}
		currentVersion = advanced.Version
	}

	if existing, err := s.store.GetPullRequestByRun(ctx, run.RunID); err == nil {
		return s.completeDraftPRRun(ctx, run.RunID, currentVersion, cmd.ExpectedLeaseEpoch, existing)
	} else if !errors.Is(err, ErrPRNotFound) {
		return PullRequestView{}, fmt.Errorf("read existing PR: %w", err)
	}

	refs, err := s.github.ListOpenPRsForHead(ctx, owner, repo, run.Branch)
	if err != nil {
		return PullRequestView{}, fmt.Errorf("list open PRs: %w", err)
	}
	var pr PRRef
	if len(refs) > 0 {
		pr = refs[0]
		if pr.HeadSHA != "" && pr.HeadSHA != *run.CommitSHA {
			return PullRequestView{}, fmt.Errorf("%w: existing PR head=%s gated=%s", ErrHeadSHAMismatch, pr.HeadSHA, *run.CommitSHA)
		}
	} else {
		body := strings.TrimSpace(cmd.Body)
		if body != "" {
			body += "\n\n"
		}
		body += fmt.Sprintf("VTX evidence\n\n- Run: `%s`\n- Commit: `%s`\n- Gate decision: `%s`\n\n<!-- vtx-pipeline -->", run.RunID, *run.CommitSHA, verdict.DecisionID)
		pr, err = s.github.CreateDraftPR(ctx, owner, repo, CreatePRInput{
			Title: cmd.Title, Body: body, Head: run.Branch, Base: s.baseBranch,
		})
		if errors.Is(err, ErrGitHubValidation) {
			// Another driver may have won after list-before-create. Re-read
			// external truth and adopt rather than creating a duplicate.
			refs, listErr := s.github.ListOpenPRsForHead(ctx, owner, repo, run.Branch)
			if listErr == nil && len(refs) > 0 {
				pr, err = refs[0], nil
			}
		}
		if err != nil {
			return PullRequestView{}, fmt.Errorf("create draft PR: %w", err)
		}
	}
	if !pr.Draft {
		return PullRequestView{}, fmt.Errorf("%w: GitHub returned non-draft PR", ErrGitHubValidation)
	}
	view, err := s.store.SaveOpenedPullRequest(ctx, SavePullRequest{
		RunID: run.RunID, TaskID: run.TaskID, IntentID: run.IntentID,
		RepoOwner: owner, RepoName: repo, PR: pr, HeadBranch: run.Branch,
		HeadSHA: *run.CommitSHA, BaseBranch: s.baseBranch, CorrelationID: run.CorrelationID,
	})
	if err != nil {
		return PullRequestView{}, fmt.Errorf("persist opened PR: %w", err)
	}
	return s.completeDraftPRRun(ctx, run.RunID, currentVersion, cmd.ExpectedLeaseEpoch, view)
}

func (s *PRService) completeDraftPRRun(ctx context.Context, runID string, version, leaseEpoch int64, view PullRequestView) (PullRequestView, error) {
	if _, err := s.transitions.TransitionRun(ctx, TransitionCommand{
		RunID: runID, ExpectedVersion: version,
		ExpectedLeaseEpoch: leaseEpoch, Action: domain.ActionComplete,
		Actor: domain.ActorOrchestrator, ActorType: domain.ActorTypeSystem,
	}); err != nil {
		return view, fmt.Errorf("complete PR run: %w", err)
	}
	return view, nil
}

func (s *PRService) GetPullRequest(ctx context.Context, runID string) (PullRequestView, error) {
	return s.store.GetPullRequestByRun(ctx, runID)
}

func (s *PRService) ListPullRequests(ctx context.Context, status *string, before *time.Time, limit int32) ([]PullRequestView, error) {
	return s.store.ListPullRequests(ctx, status, before, limit)
}

func (s *PRService) SyncPullRequest(ctx context.Context, runID string) (PullRequestView, error) {
	view, err := s.store.GetPullRequestByRun(ctx, runID)
	if err != nil {
		return PullRequestView{}, err
	}
	if view.Number == nil {
		return PullRequestView{}, ErrPRNotFound
	}
	remote, err := s.github.GetPR(ctx, view.RepoOwner, view.RepoName, *view.Number)
	if err != nil {
		return PullRequestView{}, err
	}
	status := remote.State
	if remote.Merged {
		status = "merged"
	}
	rollup, err := s.github.GetCheckRollup(ctx, view.RepoOwner, view.RepoName, view.HeadSHA)
	if err != nil {
		return PullRequestView{}, err
	}
	return s.store.UpdatePullRequestSync(ctx, runID, status, rollup.Status)
}

func parseGitHubRepo(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		return "", "", fmt.Errorf("invalid GitHub repository URL")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid GitHub repository URL")
	}
	return parts[0], parts[1], nil
}
