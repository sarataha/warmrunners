// internal/demand/github_poller_test.go
package demand

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubRESTPoller_CountsByStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /repos/{owner}/{repo}/actions/runs?status=queued
		// /repos/{owner}/{repo}/actions/runs?status=in_progress
		body := map[string]any{
			"total_count": func() int {
				if r.URL.Query().Get("status") == "queued" {
					return 4
				}
				return 7
			}(),
			"workflow_runs": []any{},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	snap, err := p.CurrentDemand(context.Background(), "org", "repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Queued != 4 || snap.Running != 7 {
		t.Fatalf("snap = %+v, want {4, 7}", snap)
	}
}

func TestGitHubRESTPoller_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	_, err := p.CurrentDemand(context.Background(), "org", "repo", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGitHubRESTPoller_SendsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}})
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "secret-token")
	_, err := p.CurrentDemand(context.Background(), "org", "repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
}
