package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vtx/platform/internal/application"
)

const maxErrorBody = 64 << 10

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) (*Client, error) {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("GITHUB_API_BASE_URL must be an https URL")
	}
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is required")
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (c *Client) GetRepo(ctx context.Context, owner, repo string) (application.RepoRef, error) {
	var out struct {
		FullName string `json:"full_name"`
	}
	if err := c.get(ctx, repoPath(owner, repo), &out); err != nil {
		return application.RepoRef{}, err
	}
	return application.RepoRef{FullName: out.FullName}, nil
}
func (c *Client) GetRefSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.get(ctx, repoPath(owner, repo)+"/git/ref/heads/"+url.PathEscape(branch), &out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}
func (c *Client) GetCommit(ctx context.Context, owner, repo, sha string) (application.CommitInfo, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	if err := c.get(ctx, repoPath(owner, repo)+"/commits/"+url.PathEscape(sha), &out); err != nil {
		return application.CommitInfo{}, err
	}
	return application.CommitInfo{SHA: out.SHA}, nil
}
func (c *Client) ListOpenPRsForHead(ctx context.Context, owner, repo, headBranch string) ([]application.PRRef, error) {
	path := repoPath(owner, repo) + "/pulls?state=open&head=" + url.QueryEscape(owner+":"+headBranch)
	var rows []githubPR
	if err := c.get(ctx, path, &rows); err != nil {
		return nil, err
	}
	out := make([]application.PRRef, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ref())
	}
	return out, nil
}
func (c *Client) CreateDraftPR(ctx context.Context, owner, repo string, in application.CreatePRInput) (application.PRRef, error) {
	// Draft is not caller-controlled. The adapter always sends true.
	body := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}{in.Title, in.Body, in.Head, in.Base, true}
	var out githubPR
	if err := c.doJSON(ctx, http.MethodPost, repoPath(owner, repo)+"/pulls", body, &out); err != nil {
		return application.PRRef{}, err
	}
	return out.ref(), nil
}
func (c *Client) GetPR(ctx context.Context, owner, repo string, number int) (application.PRRef, error) {
	var out githubPR
	if err := c.get(ctx, repoPath(owner, repo)+"/pulls/"+strconv.Itoa(number), &out); err != nil {
		return application.PRRef{}, err
	}
	return out.ref(), nil
}
func (c *Client) GetCheckRollup(ctx context.Context, owner, repo, sha string) (application.CheckRollup, error) {
	var checks struct {
		CheckRuns []struct{ Status, Conclusion string } `json:"check_runs"`
	}
	err := c.get(ctx, repoPath(owner, repo)+"/commits/"+url.PathEscape(sha)+"/check-runs", &checks)
	if err != nil {
		return application.CheckRollup{}, err
	}
	status := "none"
	for _, check := range checks.CheckRuns {
		if check.Status != "completed" {
			status = "pending"
			continue
		}
		switch check.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required":
			status = "failure"
		case "neutral", "skipped", "success":
			if status == "none" {
				status = "success"
			}
		default:
			if status != "failure" {
				status = "error"
			}
		}
	}
	return application.CheckRollup{Status: status}, nil
}

type githubPR struct {
	ID        int64     `json:"id"`
	Number    int       `json:"number"`
	HTMLURL   string    `json:"html_url"`
	Draft     bool      `json:"draft"`
	State     string    `json:"state"`
	Merged    bool      `json:"merged"`
	CreatedAt time.Time `json:"created_at"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p githubPR) ref() application.PRRef {
	return application.PRRef{ID: p.ID, Number: p.Number, URL: p.HTMLURL, HeadSHA: p.Head.SHA, Draft: p.Draft, State: p.State, Merged: p.Merged, CreatedAt: p.CreatedAt}
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode GitHub request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build GitHub request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", application.ErrGitHubUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited := io.LimitReader(resp.Body, maxErrorBody)
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(limited).Decode(&apiErr)
		message := strings.TrimSpace(apiErr.Message)
		if len(message) > 500 {
			message = message[:500]
		}
		base := application.ErrGitHubUnavailable
		switch resp.StatusCode {
		case http.StatusNotFound:
			base = application.ErrGitHubNotFound
		case http.StatusUnauthorized, http.StatusForbidden:
			if resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0" {
				base = application.ErrGitHubRateLimited
			} else {
				base = application.ErrGitHubForbidden
			}
		case http.StatusTooManyRequests:
			base = application.ErrGitHubRateLimited
		case http.StatusUnprocessableEntity:
			base = application.ErrGitHubValidation
		}
		return fmt.Errorf("%w: status=%d message=%s", base, resp.StatusCode, message)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}
