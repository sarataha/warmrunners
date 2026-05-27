// internal/demand/github_poller.go
package demand

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type GitHubRESTPoller struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewGitHubRESTPoller(baseURL, token string) *GitHubRESTPoller {
	return &GitHubRESTPoller{baseURL: baseURL, token: token, client: http.DefaultClient}
}

type runsResp struct {
	TotalCount int `json:"total_count"`
}

func (p *GitHubRESTPoller) count(ctx context.Context, owner, repo, status string) (int32, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=%s&per_page=1", p.baseURL, owner, repo, status)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github api: %s", resp.Status)
	}
	var body runsResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return int32(body.TotalCount), nil
}

func (p *GitHubRESTPoller) CurrentDemand(ctx context.Context, owner, repository string, _ []string) (Snapshot, error) {
	q, err := p.count(ctx, owner, repository, "queued")
	if err != nil {
		return Snapshot{}, err
	}
	r, err := p.count(ctx, owner, repository, "in_progress")
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Queued: q, Running: r}, nil
}
