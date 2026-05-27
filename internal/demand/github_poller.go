// internal/demand/github_poller.go
package demand

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type GitHubRESTPoller struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewGitHubRESTPoller(baseURL, token string) *GitHubRESTPoller {
	return &GitHubRESTPoller{
		baseURL: baseURL,
		// Trim whitespace: tokens loaded from Secrets created via
		// `kubectl --from-file` or `echo` carry a trailing newline, which makes
		// an invalid Authorization header ("invalid header field value").
		token: strings.TrimSpace(token),
		// Bound every call so a hung connection can't stall reconciles.
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

type runsResp struct {
	TotalCount   int `json:"total_count"`
	WorkflowRuns []struct {
		ID int64 `json:"id"`
	} `json:"workflow_runs"`
}

type jobsResp struct {
	TotalCount int `json:"total_count"`
	Jobs       []struct {
		Status string   `json:"status"`
		Labels []string `json:"labels"`
	} `json:"jobs"`
}

func (p *GitHubRESTPoller) get(ctx context.Context, u string, out any) error {
	// FIX C: handle the request-build error instead of discarding it.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// listRunIDs returns the IDs of all workflow runs in the repo with the given
// status, following pagination via per_page=100 + page loop until the returned
// page is short of a full page.
func (p *GitHubRESTPoller) listRunIDs(ctx context.Context, owner, repo, status string) ([]int64, error) {
	const perPage = 100
	var ids []int64
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=%s&per_page=%d&page=%d",
			p.baseURL, url.PathEscape(owner), url.PathEscape(repo), status, perPage, page)
		var body runsResp
		if err := p.get(ctx, u, &body); err != nil {
			return nil, err
		}
		for _, run := range body.WorkflowRuns {
			ids = append(ids, run.ID)
		}
		if len(body.WorkflowRuns) < perPage {
			break
		}
	}
	return ids, nil
}

// countRunJobs walks the jobs of a single run (paginated) and tallies jobs that
// are queued / in_progress AND whose runs-on labels are a superset of want.
func (p *GitHubRESTPoller) countRunJobs(ctx context.Context, owner, repo string, runID int64, want []string) (queued, running int32, err error) {
	const perPage = 100
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?per_page=%d&page=%d",
			p.baseURL, url.PathEscape(owner), url.PathEscape(repo), runID, perPage, page)
		var body jobsResp
		if err := p.get(ctx, u, &body); err != nil {
			return 0, 0, err
		}
		for _, j := range body.Jobs {
			if !labelsMatch(j.Labels, want) {
				continue
			}
			switch j.Status {
			case "queued":
				queued++
			case "in_progress":
				running++
			}
		}
		if len(body.Jobs) < perPage {
			break
		}
	}
	return queued, running, nil
}

// labelsMatch reports whether have is a superset of want (every wanted label is
// present on the job). An empty want matches everything.
func labelsMatch(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, l := range have {
		set[l] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func (p *GitHubRESTPoller) CurrentDemand(ctx context.Context, owner, repository string, labels []string) (Snapshot, error) {
	// FIX B: count individual JOBS whose runs-on labels match the policy's
	// labels, rather than the repo-wide run total_count. This makes a
	// label-scoped policy scale on its own queue instead of the whole repo.
	//
	// Rate-limit tradeoff: this issues N+1 calls per poll (one runs list per
	// status + one jobs call per run, times pagination). Acceptable for v1 on
	// repo-scoped policies with a 30s+ poll interval; watch the GitHub API rate
	// limit in live testing and consider caching/conditional requests if needed.
	var snap Snapshot
	for _, status := range []string{"queued", "in_progress"} {
		runIDs, err := p.listRunIDs(ctx, owner, repository, status)
		if err != nil {
			return Snapshot{}, err
		}
		for _, id := range runIDs {
			q, r, err := p.countRunJobs(ctx, owner, repository, id, labels)
			if err != nil {
				return Snapshot{}, err
			}
			snap.Queued += q
			snap.Running += r
		}
	}
	return snap, nil
}
