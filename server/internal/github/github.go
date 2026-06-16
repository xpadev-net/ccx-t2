package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	gh "github.com/google/go-github/v60/github"
)

// CheckStatus is the normalized status of a CI check.
type CheckStatus string

const (
	CheckSuccess CheckStatus = "success"
	CheckFailure CheckStatus = "failure"
	CheckPending CheckStatus = "pending"
)

// Check holds the name and normalized status of a single CI check run.
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
}

// PRStatus is the response from GetPRStatus.
type PRStatus struct {
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	State     string  `json:"state"`
	Mergeable *bool   `json:"mergeable"`
	Checks    []Check `json:"checks"`
}

// tokenTransport adds an Authorization: Bearer header to every request.
type tokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Client wraps the GitHub API.
type Client struct {
	owner  string
	repo   string
	client *gh.Client
}

type clientOptions struct {
	httpClient *http.Client
	baseURL    string
}

// ClientOption customizes a GitHub API client.
type ClientOption func(*clientOptions)

// WithHTTPClient configures the HTTP client used for GitHub API requests.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(opts *clientOptions) {
		opts.httpClient = httpClient
	}
}

// WithBaseURL configures the GitHub API base URL.
func WithBaseURL(baseURL string) ClientOption {
	return func(opts *clientOptions) {
		opts.baseURL = baseURL
	}
}

// NewClient creates a GitHub API client using the provided token.
func NewClient(token, owner, repo string, options ...ClientOption) (*Client, error) {
	if token == "" || owner == "" || repo == "" {
		return nil, fmt.Errorf("github: token, owner, and repo are required")
	}
	var opts clientOptions
	for _, option := range options {
		option(&opts)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	if opts.httpClient != nil {
		clientCopy := *opts.httpClient
		httpClient = &clientCopy
	}
	httpClient.Transport = &tokenTransport{token: token, base: httpClient.Transport}

	ghClient := gh.NewClient(httpClient)
	if opts.baseURL != "" {
		baseURL, err := url.Parse(opts.baseURL)
		if err != nil {
			return nil, fmt.Errorf("github: parse base URL: %w", err)
		}
		if baseURL.Scheme == "" || baseURL.Host == "" {
			return nil, fmt.Errorf("github: base URL must be absolute")
		}
		if baseURL.Path == "" || baseURL.Path[len(baseURL.Path)-1] != '/' {
			baseURL.Path += "/"
		}
		ghClient.BaseURL = baseURL
	}

	return &Client{
		owner:  owner,
		repo:   repo,
		client: ghClient,
	}, nil
}

// GetPRStatus returns the current state of a pull request.
func (c *Client) GetPRStatus(ctx context.Context, prNumber int) (*PRStatus, error) {
	pr, _, err := c.client.PullRequests.Get(ctx, c.owner, c.repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("get PR: %w", err)
	}

	state := "open"
	if pr.GetMerged() {
		state = "merged"
	} else if pr.GetState() == "closed" {
		state = "closed"
	}

	headSHA := pr.GetHead().GetSHA()
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var checks []Check
	for {
		runs, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, c.owner, c.repo, headSHA, opts)
		if err != nil {
			return nil, fmt.Errorf("list check runs: %w", err)
		}
		for _, run := range runs.CheckRuns {
			checks = append(checks, Check{
				Name:   run.GetName(),
				Status: normalizeCheckStatus(run),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return &PRStatus{
		Number:    pr.GetNumber(),
		Title:     pr.GetTitle(),
		State:     state,
		Mergeable: pr.Mergeable,
		Checks:    checks,
	}, nil
}

func normalizeCheckStatus(run *gh.CheckRun) CheckStatus {
	conclusion := run.GetConclusion()
	status := run.GetStatus()

	switch conclusion {
	case "success", "neutral", "skipped":
		return CheckSuccess
	case "failure", "timed_out", "cancelled", "action_required", "stale":
		return CheckFailure
	}
	switch status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return CheckPending
	}
	// Unknown conclusion+status combination — treat as failure so the orchestrator
	// does not wait indefinitely for a check that may never reach a known terminal state.
	return CheckFailure
}
