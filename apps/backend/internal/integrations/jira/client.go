// Package jira implements the Jira Cloud REST adapter used by the webhook
// intake service to re-verify issue status before accepting a transition.
package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrRetryable marks failures that entitle Jira's webhook redelivery to retry
// processing. The handler maps it to HTTP 503 — and critically, no inbox row
// is persisted, so the dedup key stays unclaimed.
var ErrRetryable = fmt.Errorf("retryable upstream failure")

var (
	issueIDRe  = regexp.MustCompile(`^\d+$`)
	issueKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)
)

// Total request budget: Jira Cloud aborts webhook delivery after ~10s and a
// stalled response degrades the whole queue — fail fast instead. We never
// sleep on Retry-After: a 429/5xx answer is returned to Jira as 503 and Jira's
// own redelivery is the retry mechanism.
const requestBudget = 3 * time.Second

// FetchedIssue is the authoritative identity record returned by the Jira API:
// the service compares every field against the untrusted webhook payload.
type FetchedIssue struct {
	ID         string
	Key        string
	ProjectKey string
	StatusID   string
}

// IssueFetcher re-fetches an issue's authoritative identity. All failure
// modes — transport, non-200, decode errors, missing id/project/status — are
// wrapped in ErrRetryable so the delivery is retried rather than
// trusted-or-dropped on a Jira anomaly.
type IssueFetcher interface {
	FetchIssue(ctx context.Context, issueID string) (FetchedIssue, error)
}

type Client struct {
	baseURL  *url.URL
	email    string
	apiToken string
	http     *http.Client
}

// NewClient pins the Jira host from configuration — the webhook payload is
// never consulted for a target URL (SSRF guard).
func NewClient(baseURL, email, apiToken string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid JIRA_BASE_URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("JIRA_BASE_URL must have a host, got %q", baseURL)
	}
	if u.Scheme != "https" {
		// https is mandatory for real tenants. http is tolerated only for
		// internal/dev targets: Docker service names (no dot) or loopback.
		host := u.Hostname()
		internal := !strings.Contains(host, ".") ||
			host == "localhost" || host == "127.0.0.1" || host == "::1"
		if u.Scheme != "http" || !internal {
			return nil, fmt.Errorf("JIRA_BASE_URL must be a pinned https:// URL, got %q", baseURL)
		}
	}
	transport := &http.Transport{
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 2 * time.Second,
	}
	c := &Client{
		baseURL:  u,
		email:    email,
		apiToken: apiToken,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestBudget,
			// Redirect guard: only same-host, same-scheme redirects are
			// allowed. Anything else is an SSRF attempt — fail closed.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Scheme != u.Scheme || req.URL.Host != u.Host {
					return fmt.Errorf("%w: redirect to untrusted target %q", ErrRetryable, req.URL)
				}
				if len(via) >= 3 {
					return fmt.Errorf("%w: too many redirects", ErrRetryable)
				}
				return nil
			},
		},
	}
	return c, nil
}

// ValidateIssueID enforces the numeric issue.id used for re-fetch (issue.key
// is display-only and never trusted in the API path).
func ValidateIssueID(id string) error {
	if !issueIDRe.MatchString(id) {
		return fmt.Errorf("issue.id %q fails numeric validation", id)
	}
	return nil
}

func ValidateIssueKey(key string) error {
	if !issueKeyRe.MatchString(key) {
		return fmt.Errorf("issue.key %q fails format validation", key)
	}
	return nil
}

type issueResponse struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Status struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"status"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
	} `json:"fields"`
}

// FetchIssue re-fetches the issue and returns its authoritative identity —
// issue ID, key, project key, and current status ID. The caller compares all
// four against the webhook payload; trusting payload fields alone is what
// allowed forged transitions (review P1-2). Any transport or non-2xx failure
// is wrapped in ErrRetryable (caller returns 503, no dedup claim); so is a
// 200 body whose id/key/project/status fields are missing or mismatched — a
// Jira anomaly we redeliver rather than silently trust.
func (c *Client) FetchIssue(ctx context.Context, issueID string) (FetchedIssue, error) {
	if err := ValidateIssueID(issueID); err != nil {
		return FetchedIssue{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestBudget)
	defer cancel()

	endpoint := *c.baseURL
	endpoint.Path = endpoint.Path + "/rest/api/3/issue/" + url.PathEscape(issueID)
	q := endpoint.Query()
	q.Set("fields", "status,project")
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return FetchedIssue{}, fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.apiToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return FetchedIssue{}, fmt.Errorf("%w: jira api call: %v", ErrRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 429 (any Retry-After value), 5xx, 4xx: all retryable — Jira
		// redelivers; we never park the delivery sleeping.
		return FetchedIssue{}, fmt.Errorf("%w: jira api status %d", ErrRetryable, resp.StatusCode)
	}

	var body issueResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return FetchedIssue{}, fmt.Errorf("%w: decode issue response: %v", ErrRetryable, err)
	}
	if body.ID == "" || body.ID != issueID {
		return FetchedIssue{}, fmt.Errorf("%w: issue response id %q does not match requested %q", ErrRetryable, body.ID, issueID)
	}
	if body.Key == "" {
		return FetchedIssue{}, fmt.Errorf("%w: issue response missing key", ErrRetryable)
	}
	if body.Fields.Project.Key == "" {
		return FetchedIssue{}, fmt.Errorf("%w: issue response missing project.key", ErrRetryable)
	}
	if body.Fields.Status.ID == "" {
		return FetchedIssue{}, fmt.Errorf("%w: issue response missing status.id", ErrRetryable)
	}
	return FetchedIssue{
		ID:         body.ID,
		Key:        body.Key,
		ProjectKey: body.Fields.Project.Key,
		StatusID:   body.Fields.Status.ID,
	}, nil
}
