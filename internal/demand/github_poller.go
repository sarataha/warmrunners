// internal/demand/github_poller.go
package demand

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sarataha/warmrunners/internal/ghapi"
	"github.com/sarataha/warmrunners/internal/version"
)

const (
	// defaultHTTPTimeout bounds every call so a hung connection can't stall reconciles.
	defaultHTTPTimeout = 10 * time.Second
	// defaultBaseDelay is the base unit for exponential backoff on transient errors.
	defaultBaseDelay = 1 * time.Second
	// maxBackoff caps the exponential backoff per the GitHub polling guidance.
	maxBackoff = 60 * time.Second
	// maxRetries is the number of retries (in addition to the initial attempt).
	maxRetries = 3
)

type GitHubRESTPoller struct {
	baseURL string
	token   string
	client  *http.Client

	// baseDelay seeds exponential backoff. Injectable so tests can shrink it.
	baseDelay time.Duration

	// cache holds the ETag + parsed response per (owner, repo, endpoint) key,
	// guarded by mu, for conditional (If-None-Match) requests.
	mu    sync.Mutex
	cache map[string]cacheEntry

	// newTimer and now are indirections for deterministic, fast tests.
	// newTimer returns a channel that fires after d and a stop func (matching
	// time.Timer.Stop semantics). Tests inject a fake to record the computed wait
	// without sleeping real wall-clock time.
	newTimer func(d time.Duration) (<-chan time.Time, func() bool)
	now      func() time.Time
}

type cacheEntry struct {
	etag   string
	parsed any
}

// Option configures a GitHubRESTPoller.
type Option func(*GitHubRESTPoller)

// WithHTTPTimeout overrides the HTTP client timeout (default 10s).
func WithHTTPTimeout(d time.Duration) Option {
	return func(p *GitHubRESTPoller) {
		if d > 0 {
			p.client.Timeout = d
		}
	}
}

// WithBaseDelay overrides the exponential-backoff base delay (default 1s).
// Primarily for tests so retries don't sleep for whole seconds.
func WithBaseDelay(d time.Duration) Option {
	return func(p *GitHubRESTPoller) {
		if d > 0 {
			p.baseDelay = d
		}
	}
}

func NewGitHubRESTPoller(baseURL, token string, opts ...Option) *GitHubRESTPoller {
	p := &GitHubRESTPoller{
		baseURL: baseURL,
		// Trim whitespace: tokens loaded from Secrets created via
		// `kubectl --from-file` or `echo` carry a trailing newline, which makes
		// an invalid Authorization header ("invalid header field value").
		token:     strings.TrimSpace(token),
		client:    &http.Client{Timeout: defaultHTTPTimeout},
		baseDelay: defaultBaseDelay,
		cache:     make(map[string]cacheEntry),
		newTimer: func(d time.Duration) (<-chan time.Time, func() bool) {
			t := time.NewTimer(d)
			return t.C, t.Stop
		},
		now: time.Now,
	}
	for _, o := range opts {
		o(p)
	}
	return p
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

// doWithRetry issues a conditional GET for u and retries on transient failures.
//
// Retry policy (max maxRetries retries, GitHub polling guidance):
//   - 5xx or transient network error: exponential backoff
//     min(2^attempt * baseDelay, 60s) + jitter, then retry.
//   - 429/403 with Retry-After: N → sleep exactly N seconds (no exponential).
//   - 403 with X-RateLimit-Remaining: 0 → sleep until X-RateLimit-Reset (UTC epoch).
//
// On success (200 / 304) it returns the response and the fully-read body.
func (p *GitHubRESTPoller) doWithRetry(ctx context.Context, u string) (*http.Response, []byte, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			// FIX C: surface the request-build error instead of dropping it.
			// Non-retryable (a bad URL won't get better).
			return nil, nil, err
		}
		if p.token != "" {
			req.Header.Set("Authorization", "Bearer "+p.token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		// GitHub rejects requests without a User-Agent and recommends an
		// app-identifying value. Pull the version from the build-time package.
		req.Header.Set("User-Agent", "warmrunners/"+version.Version)
		// Send If-None-Match when we have a cached ETag so a 304 saves quota.
		if etag := p.cachedETag(u); etag != "" {
			req.Header.Set("If-None-Match", etag)
		}

		resp, err := p.client.Do(req)
		if err != nil {
			// Transient network error: back off and retry.
			lastErr = err
			if attempt == maxRetries {
				break
			}
			if err := p.backoff(ctx, attempt); err != nil {
				return nil, nil, err
			}
			continue
		}
		ghapi.RecordRateLimit(ghapi.SourceDemand, resp.Header)

		// Decide whether to retry based on status.
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified:
			body, rerr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if rerr != nil {
				return nil, nil, rerr
			}
			return resp, body, nil

		case resp.StatusCode >= 500:
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("github api: %s", resp.Status)
			if attempt == maxRetries {
				break
			}
			if err := p.backoff(ctx, attempt); err != nil {
				return nil, nil, err
			}
			continue

		case resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusForbidden:
			// Rate limited. Honor explicit signals; otherwise treat as terminal.
			wait, retryable := rateLimitDelay(resp, p.now())
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("github api: %s", resp.Status)
			if !retryable || attempt == maxRetries {
				break
			}
			if wait > 0 {
				if err := p.sleepCtx(ctx, wait); err != nil {
					return nil, nil, err
				}
			} else {
				// The endpoint just rate-limited us but the computed wait is
				// non-positive (Retry-After: 0, or X-RateLimit-Reset already in
				// the past). Spinning here would fire maxRetries requests instantly
				// against the very endpoint that asked us to slow down, so fall back
				// to exponential backoff.
				if err := p.backoff(ctx, attempt); err != nil {
					return nil, nil, err
				}
			}
			continue

		default:
			// Non-retryable 4xx (e.g. 404, 401): surface immediately.
			_ = resp.Body.Close()
			return nil, nil, fmt.Errorf("github api: %s", resp.Status)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("github api: request to %s failed", u)
	}
	return nil, nil, lastErr
}

// backoff sleeps min(2^attempt * baseDelay, 60s) plus jitter in [0, baseDelay).
// It returns ctx.Err() if the context is cancelled mid-wait.
func (p *GitHubRESTPoller) backoff(ctx context.Context, attempt int) error {
	d := p.baseDelay << attempt // 2^attempt * baseDelay
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	if jitterMax := p.baseDelay; jitterMax > 0 {
		d += time.Duration(rand.Int63n(int64(jitterMax)))
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return p.sleepCtx(ctx, d)
}

// sleepCtx waits for d but returns ctx.Err() early if ctx is cancelled. It uses
// an injectable timer (p.newTimer) so the timer is stopped on cancellation and
// no goroutine lingers after the function returns.
func (p *GitHubRESTPoller) sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	c, stop := p.newTimer(d)
	select {
	case <-c:
		return nil
	case <-ctx.Done():
		stop()
		return ctx.Err()
	}
}

// rateLimitDelay computes how long to wait for a 429/403 rate-limit response.
// It returns (wait, retryable). Retry-After takes precedence; otherwise a
// X-RateLimit-Remaining: 0 with X-RateLimit-Reset is honored.
func rateLimitDelay(resp *http.Response, now time.Time) (time.Duration, bool) {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if rs := resp.Header.Get("X-RateLimit-Reset"); rs != "" {
			if epoch, err := strconv.ParseInt(strings.TrimSpace(rs), 10, 64); err == nil {
				wait := time.Unix(epoch, 0).UTC().Sub(now)
				if wait < 0 {
					wait = 0
				}
				return wait, true
			}
		}
	}
	// 403 without rate-limit signals is a hard auth/permission failure: not retryable.
	return 0, false
}

// cachedETag returns the ETag cached for u (test/inspection helper).
func (p *GitHubRESTPoller) cachedETag(u string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cache[u].etag
}

// get performs a conditional, retrying GET for u and decodes the body into out.
// out must be a non-nil pointer. On 304 Not Modified the cached parsed value is
// copied into out without re-parsing the body.
func (p *GitHubRESTPoller) get(ctx context.Context, u string, out any) error {
	resp, body, err := p.doWithRetry(ctx, u)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusNotModified {
		// Reuse the cached parsed value without touching the (empty) body.
		p.mu.Lock()
		entry, ok := p.cache[u]
		p.mu.Unlock()
		if !ok {
			return fmt.Errorf("github api: 304 for %s but nothing cached", u)
		}
		// Copy the cached parsed value into out via reflection. Guard the types:
		// a mismatch (cache holds a different shape than out expects) must fail
		// soft with an error rather than panic and crash a reconcile.
		dst := reflect.ValueOf(out).Elem()
		src := reflect.ValueOf(entry.parsed).Elem()
		if src.Type() != dst.Type() {
			return fmt.Errorf("github api: 304 cache type mismatch for %s: cached %s, want %s",
				u, src.Type(), dst.Type())
		}
		dst.Set(src)
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api: %s", resp.Status)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return err
	}

	// Cache the parsed value + ETag for subsequent conditional requests.
	if etag := resp.Header.Get("ETag"); etag != "" {
		// Store a copy so later mutations of out don't corrupt the cache.
		cp := reflect.New(reflect.TypeOf(out).Elem())
		cp.Elem().Set(reflect.ValueOf(out).Elem())
		p.mu.Lock()
		p.cache[u] = cacheEntry{etag: etag, parsed: cp.Interface()}
		p.mu.Unlock()
	}
	return nil
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
	// Label scoping (formerly "FIX B"): we count individual JOBS whose runs-on
	// labels are a superset of the policy's labels, rather than the repo-wide
	// run total_count. This makes a label-scoped policy scale on its own queue
	// instead of the whole repo. See countRunJobs + labelsMatch; verified by
	// TestGitHubRESTPoller_CountsMatchingJobs.
	//
	// LIMITATION: this issues N+1 calls per poll (one runs list per status + one
	// jobs call per run, times pagination). ETag conditional requests (see get)
	// cut quota burn on slow-moving queues, but a very busy repo with many
	// distinct runs still fans out. Acceptable for repo-scoped policies at a
	// 30s+ poll interval; a future optimization could batch via the
	// per-repository jobs listing once GitHub exposes label filters server-side.
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
