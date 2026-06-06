package github

import (
	"context"
	"fmt"
	"net/http"

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
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(req)
}

// Client wraps the GitHub API.
type Client struct {
	owner  string
	repo   string
	client *gh.Client
}

// NewClient creates a GitHub API client using the provided token.
func NewClient(token, owner, repo string) (*Client, error) {
	if token == "" || owner == "" || repo == "" {
		return nil, fmt.Errorf("github: token, owner, and repo are required")
	}
	httpClient := &http.Client{Transport: &tokenTransport{token: token}}
	return &Client{
		owner:  owner,
		repo:   repo,
		client: gh.NewClient(httpClient),
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
	runs, _, err := c.client.Checks.ListCheckRunsForRef(ctx, c.owner, c.repo, headSHA, opts)
	if err != nil {
		return nil, fmt.Errorf("list check runs: %w", err)
	}

	checks := make([]Check, 0, len(runs.CheckRuns))
	for _, run := range runs.CheckRuns {
		checks = append(checks, Check{
			Name:   run.GetName(),
			Status: normalizeCheckStatus(run),
		})
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
	case "failure", "timed_out", "cancelled", "action_required":
		return CheckFailure
	}
	switch status {
	case "queued", "in_progress":
		return CheckPending
	}
	return CheckPending
}
