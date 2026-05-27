// internal/demand/github_poller_test.go
package demand

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runsForStatus returns a workflow_runs list body for a given status query.
func runsBody(ids ...int64) map[string]any {
	runs := make([]any, 0, len(ids))
	for _, id := range ids {
		runs = append(runs, map[string]any{"id": id})
	}
	return map[string]any{"total_count": len(ids), "workflow_runs": runs}
}

func jobsBody(jobs ...map[string]any) map[string]any {
	js := make([]any, 0, len(jobs))
	for _, j := range jobs {
		js = append(js, j)
	}
	return map[string]any{"total_count": len(jobs), "jobs": js}
}

func job(status string, labels ...string) map[string]any {
	ls := make([]any, 0, len(labels))
	for _, l := range labels {
		ls = append(ls, l)
	}
	return map[string]any{"status": status, "labels": ls}
}

// TestGitHubRESTPoller_CountsMatchingJobs verifies that CurrentDemand counts
// only the jobs whose runs-on labels are a superset of the policy labels, and
// buckets them by job status (queued vs in_progress).
func TestGitHubRESTPoller_CountsMatchingJobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			// Two queued runs and two in_progress runs at the repo level.
			// Old (broken) code counted total_count -> {Queued:2, Running:2}.
			// New code must count only matching JOBS -> {Queued:1, Running:1}.
			switch r.URL.Query().Get("status") {
			case "queued":
				_ = json.NewEncoder(w).Encode(runsBody(1, 3))
			case "in_progress":
				_ = json.NewEncoder(w).Encode(runsBody(2, 4))
			default:
				_ = json.NewEncoder(w).Encode(runsBody())
			}
		case strings.HasSuffix(r.URL.Path, "/runs/1/jobs"):
			// run 1: a queued matching job, a queued non-matching job,
			// and a completed job that must be ignored.
			_ = json.NewEncoder(w).Encode(jobsBody(
				job("queued", "self-hosted", "linux", "gpu"), // matches {self-hosted, gpu}
				job("queued", "self-hosted", "linux"),        // missing gpu -> excluded
				job("completed", "self-hosted", "gpu"),       // completed -> excluded
			))
		case strings.HasSuffix(r.URL.Path, "/runs/3/jobs"):
			// run 3: only non-matching queued jobs -> contributes nothing.
			_ = json.NewEncoder(w).Encode(jobsBody(
				job("queued", "ubuntu-latest"),
			))
		case strings.HasSuffix(r.URL.Path, "/runs/2/jobs"):
			// run 2: an in_progress matching job and an in_progress non-matching job.
			_ = json.NewEncoder(w).Encode(jobsBody(
				job("in_progress", "self-hosted", "gpu", "large"), // matches
				job("in_progress", "ubuntu-latest"),               // excluded
			))
		case strings.HasSuffix(r.URL.Path, "/runs/4/jobs"):
			// run 4: only non-matching in_progress jobs -> contributes nothing.
			_ = json.NewEncoder(w).Encode(jobsBody(
				job("in_progress", "windows-latest"),
			))
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	snap, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted", "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Queued != 1 || snap.Running != 1 {
		t.Fatalf("snap = %+v, want {Queued:1, Running:1}", snap)
	}
}

func TestGitHubRESTPoller_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	_, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGitHubRESTPoller_SendsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(runsBody())
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody())
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "secret-token")
	_, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
}
