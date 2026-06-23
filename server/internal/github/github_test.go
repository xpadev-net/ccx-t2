package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	gh "github.com/google/go-github/v60/github"
	"github.com/xpadev/ccx-t2/internal/testutil"
)

func TestGetPRStatusFetchesPullRequestAndPaginatedChecks(t *testing.T) {
	var prRequests int
	var checkRequests []string
	var srv *httptest.Server
	srv = httptest.NewServer(assertBearerToken(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/hello/pulls/42":
			prRequests++
			if r.Method != http.MethodGet {
				t.Errorf("PR method = %s, want GET", r.Method)
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			fmt.Fprint(w, `{
				"number": 42,
				"title": "Add status tests",
				"state": "open",
				"merged": false,
				"mergeable": true,
				"head": {"sha": "abc123"}
			}`)
		case "/repos/octo/hello/commits/abc123/check-runs":
			checkRequests = append(checkRequests, r.URL.RawQuery)
			if r.Method != http.MethodGet {
				t.Errorf("check-runs method = %s, want GET", r.Method)
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Errorf("per_page = %q, want 100", got)
				http.Error(w, "bad per_page", http.StatusBadRequest)
				return
			}
			switch r.URL.Query().Get("page") {
			case "":
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/octo/hello/commits/abc123/check-runs?per_page=100&page=2>; rel="next"`, srv.URL))
				fmt.Fprint(w, `{
					"total_count": 2,
					"check_runs": [
						{"name": "unit", "status": "completed", "conclusion": "success"}
					]
				}`)
			case "2":
				fmt.Fprint(w, `{
					"total_count": 2,
					"check_runs": [
						{"name": "integration", "status": "in_progress"}
					]
				}`)
			default:
				t.Errorf("unexpected check-runs page query %q", r.URL.RawQuery)
				http.Error(w, "bad page", http.StatusBadRequest)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv)
	status, err := client.GetPRStatus(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetPRStatus() error = %v", err)
	}

	if prRequests != 1 {
		t.Fatalf("PR requests = %d, want 1", prRequests)
	}
	if len(checkRequests) != 2 {
		t.Fatalf("check-runs requests = %#v, want 2 paginated requests", checkRequests)
	}
	if status.Number != 42 || status.Title != "Add status tests" || status.State != "open" {
		t.Fatalf("status metadata = %#v, want PR number/title/open state", status)
	}
	if status.Mergeable == nil || !*status.Mergeable {
		t.Fatalf("Mergeable = %#v, want true pointer", status.Mergeable)
	}
	wantChecks := []Check{
		{Name: "unit", Status: CheckSuccess},
		{Name: "integration", Status: CheckPending},
	}
	if !reflect.DeepEqual(status.Checks, wantChecks) {
		t.Fatalf("Checks = %#v, want %#v", status.Checks, wantChecks)
	}
}

func TestGetPRStatusMapsPullRequestState(t *testing.T) {
	cases := []struct {
		name     string
		apiState string
		merged   bool
		want     string
	}{
		{name: "open", apiState: "open", want: "open"},
		{name: "closed", apiState: "closed", want: "closed"},
		{name: "merged", apiState: "closed", merged: true, want: "merged"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(assertBearerToken(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/octo/hello/pulls/7":
					fmt.Fprintf(w, `{
						"number": 7,
						"title": "State mapping",
						"state": %q,
						"merged": %t,
						"head": {"sha": "state-sha"}
					}`, tc.apiState, tc.merged)
				case "/repos/octo/hello/commits/state-sha/check-runs":
					fmt.Fprint(w, `{"total_count": 0, "check_runs": []}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})))
			t.Cleanup(srv.Close)

			client := newTestClient(t, srv)
			status, err := client.GetPRStatus(context.Background(), 7)
			if err != nil {
				t.Fatalf("GetPRStatus() error = %v", err)
			}
			if status.State != tc.want {
				t.Fatalf("State = %q, want %q", status.State, tc.want)
			}
		})
	}
}

func TestGetPRStatusWrapsPullRequestAPIError(t *testing.T) {
	srv := httptest.NewServer(assertBearerToken(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/hello/pulls/99" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"message":"server unavailable"}`, http.StatusInternalServerError)
	})))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv)
	_, err := client.GetPRStatus(context.Background(), 99)
	if err == nil || !strings.Contains(err.Error(), "get PR") {
		t.Fatalf("GetPRStatus() error = %v, want get PR context", err)
	}
}

func TestGetPRStatusWrapsCheckRunAPIError(t *testing.T) {
	srv := httptest.NewServer(assertBearerToken(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/hello/pulls/100":
			fmt.Fprint(w, `{
				"number": 100,
				"title": "Checks fail",
				"state": "open",
				"head": {"sha": "checks-sha"}
			}`)
		case "/repos/octo/hello/commits/checks-sha/check-runs":
			http.Error(w, `{"message":"checks unavailable"}`, http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv)
	_, err := client.GetPRStatus(context.Background(), 100)
	if err == nil || !strings.Contains(err.Error(), "list check runs") {
		t.Fatalf("GetPRStatus() error = %v, want list check runs context", err)
	}
}

func TestListPRFilesFetchesPaginatedFiles(t *testing.T) {
	var fileRequests []string
	var srv *httptest.Server
	srv = httptest.NewServer(assertBearerToken(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/hello/pulls/42/files" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("files method = %s, want GET", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
			http.Error(w, "bad per_page", http.StatusBadRequest)
			return
		}
		fileRequests = append(fileRequests, r.URL.RawQuery)
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/octo/hello/pulls/42/files?per_page=100&page=2>; rel="next"`, srv.URL))
			fmt.Fprint(w, `[
				{"filename": "server/internal/mcp/handlers.go"}
			]`)
		case "2":
			fmt.Fprint(w, `[
				{
					"filename": "server/internal/github/github.go",
					"previous_filename": "server/internal/github/old.go"
				}
			]`)
		default:
			t.Errorf("unexpected files page query %q", r.URL.RawQuery)
			http.Error(w, "bad page", http.StatusBadRequest)
		}
	})))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv)
	files, err := client.ListPRFiles(context.Background(), 42)
	if err != nil {
		t.Fatalf("ListPRFiles() error = %v", err)
	}
	if len(fileRequests) != 2 {
		t.Fatalf("file requests = %#v, want 2 paginated requests", fileRequests)
	}
	want := []string{"server/internal/mcp/handlers.go", "server/internal/github/github.go", "server/internal/github/old.go"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %#v, want %#v", files, want)
	}
}

func TestListPRFilesWrapsAPIError(t *testing.T) {
	srv := httptest.NewServer(assertBearerToken(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/hello/pulls/99/files" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"message":"files unavailable"}`, http.StatusServiceUnavailable)
	})))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv)
	_, err := client.ListPRFiles(context.Background(), 99)
	if err == nil || !strings.Contains(err.Error(), "list PR files") {
		t.Fatalf("ListPRFiles() error = %v, want list PR files context", err)
	}
}

func TestNormalizeCheckStatusTreatsNonTerminalStatusesAsPending(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		t.Run(status, func(t *testing.T) {
			run := &gh.CheckRun{Status: &status}
			if got := normalizeCheckStatus(run); got != CheckPending {
				t.Fatalf("normalizeCheckStatus(%q) = %s, want %s", status, got, CheckPending)
			}
		})
	}
}

func TestNormalizeCheckStatusTreatsUnknownEmptyConclusionAsFailure(t *testing.T) {
	status := "unknown"
	run := &gh.CheckRun{Status: &status}
	if got := normalizeCheckStatus(run); got != CheckFailure {
		t.Fatalf("normalizeCheckStatus(%q) = %s, want %s", status, got, CheckFailure)
	}
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient("test-token", "octo", "hello", WithBaseURL(srv.URL), WithHTTPClient(testutil.LocalOnlyHTTPClient(t, srv)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func assertBearerToken(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
