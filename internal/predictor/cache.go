// internal/predictor/cache.go
package predictor

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned by WorkflowFetcher.Fetch when GitHub responds with
// 404. Callers (the Task 5 predictor) use errors.Is to drop a single workflow
// from a prediction without failing the whole poll — a workflow file removed
// after the run was queued is normal and recoverable.
var ErrNotFound = errors.New("workflow not found")

const (
	// fetcherDefaultCapacity caps the in-memory LRU at a modest size. The cache
	// holds workflow YAML bodies (typically a few KB each); 256 entries keeps
	// memory bounded while comfortably covering even very busy repos within one
	// poll window.
	fetcherDefaultCapacity = 256
	// fetcherDefaultBaseDelay seeds exponential backoff for transient errors.
	fetcherDefaultBaseDelay = 1 * time.Second
	// fetcherMaxBackoff caps a single backoff sleep.
	fetcherMaxBackoff = 60 * time.Second
	// maxFetchRetries is the number of retries (in addition to the initial
	// attempt) before Fetch surfaces the last error.
	maxFetchRetries = 3
	// defaultGitHubAPI is the production GitHub REST base. Overridable via
	// withBaseURL for tests pointing at httptest.Server.
	defaultGitHubAPI = "https://api.github.com"
)

// WorkflowFetcher retrieves workflow YAML bodies from GitHub with ETag-aware
// caching. It is intentionally narrow: it does not parse the YAML, manage auth
// tokens, or hold GitHub-client state — those concerns belong to the caller.
type WorkflowFetcher interface {
	Fetch(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

// Option configures a workflowFetcher. The exported helpers below mirror the
// v0.1.1 poller's functional-options pattern (WithBaseDelay / WithNewTimer);
// the same names are kept on purpose so a future refactor can extract a shared
// retry helper without renaming the public knobs.
type Option func(*workflowFetcher)

// WithCapacity sets the LRU max-entry count (default 256). Non-positive
// values are ignored.
func WithCapacity(n int) Option {
	return func(f *workflowFetcher) {
		if n > 0 {
			f.capacity = n
		}
	}
}

// WithBaseDelay overrides the exponential-backoff base delay (default 1s).
// Primarily a test seam so retries don't sleep real seconds.
func WithBaseDelay(d time.Duration) Option {
	return func(f *workflowFetcher) {
		if d > 0 {
			f.baseDelay = d
		}
	}
}

// WithNewTimer overrides the timer constructor used for backoff/rate-limit
// sleeps. The returned channel signals expiry; the stop func must match
// time.Timer.Stop semantics. Tests inject a fake to record waits without
// sleeping.
func WithNewTimer(fn func(d time.Duration) (<-chan time.Time, func() bool)) Option {
	return func(f *workflowFetcher) {
		if fn != nil {
			f.newTimer = fn
		}
	}
}

// withBaseURL is an internal option for tests that point the fetcher at a
// httptest.Server. Not exported: production code uses GitHub's API directly.
func withBaseURL(u string) Option {
	return func(f *workflowFetcher) {
		f.baseURL = u
	}
}

// cacheEntry holds the cached body + etag for a single (owner, repo, path,
// ref) key. lruElem points back at the LRU list node so a hit can move it to
// the front in O(1).
type cacheEntry struct {
	key     string
	etag    string
	body    []byte
	lruElem *list.Element
}

type workflowFetcher struct {
	client    *http.Client
	userAgent string
	baseURL   string
	baseDelay time.Duration
	capacity  int
	newTimer  func(d time.Duration) (<-chan time.Time, func() bool)
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]*cacheEntry
	lru   *list.List // front = most-recently-used; values are *cacheEntry
}

// NewWorkflowFetcher constructs a WorkflowFetcher with the supplied HTTP
// client and User-Agent version string. The fetcher does not set
// Authorization headers; the caller is expected to provide an http.Client
// whose transport injects auth (matching the v0.1.1 poller's seam).
func NewWorkflowFetcher(httpClient *http.Client, userAgentVersion string, opts ...Option) WorkflowFetcher {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	f := &workflowFetcher{
		client:    httpClient,
		userAgent: "warmrunners/" + userAgentVersion,
		baseURL:   defaultGitHubAPI,
		baseDelay: fetcherDefaultBaseDelay,
		capacity:  fetcherDefaultCapacity,
		newTimer: func(d time.Duration) (<-chan time.Time, func() bool) {
			t := time.NewTimer(d)
			return t.C, t.Stop
		},
		now:   time.Now,
		cache: make(map[string]*cacheEntry),
		lru:   list.New(),
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Fetch returns the workflow YAML body at (owner, repo, path, ref).
//
// On a cache hit the request carries If-None-Match: <cached ETag>; a 304
// reply returns the cached bytes without re-reading the response body. 200
// replaces the cached entry. 404 returns ErrNotFound so the caller can drop
// a single workflow without failing the whole prediction. 5xx / transient
// network errors back off exponentially up to maxFetchRetries; 429/403 with
// Retry-After honor the server's wait. All sleeps respect ctx.Done().
func (f *workflowFetcher) Fetch(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	key := cacheKey(owner, repo, path, ref)
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		strings.TrimRight(f.baseURL, "/"),
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(path),
		url.QueryEscape(ref))

	var lastErr error
	for attempt := 0; attempt <= maxFetchRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github.raw")
		req.Header.Set("User-Agent", f.userAgent)
		if etag := f.cachedETag(key); etag != "" {
			req.Header.Set("If-None-Match", etag)
		}

		resp, err := f.client.Do(req)
		if err != nil {
			lastErr = err
			if attempt == maxFetchRetries {
				break
			}
			if berr := f.backoff(ctx, attempt); berr != nil {
				return nil, berr
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			body, rerr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if rerr != nil {
				return nil, rerr
			}
			etag := resp.Header.Get("ETag")
			f.store(key, etag, body)
			return body, nil

		case resp.StatusCode == http.StatusNotModified:
			_ = resp.Body.Close()
			body, ok := f.touchAndGet(key)
			if !ok {
				// 304 without a cached entry shouldn't happen (we only send
				// If-None-Match when we have one), but if the entry was evicted
				// between sending and receiving we surface a clean error rather
				// than returning nil bytes.
				return nil, fmt.Errorf("workflow fetcher: 304 for %s but nothing cached", key)
			}
			return body, nil

		case resp.StatusCode == http.StatusNotFound:
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: %s/%s %s@%s", ErrNotFound, owner, repo, path, ref)

		case resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusForbidden:
			wait, retryable := retryAfterDelay(resp, f.now())
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("workflow fetcher: %s", resp.Status)
			if !retryable || attempt == maxFetchRetries {
				break
			}
			if wait > 0 {
				if err := f.sleepCtx(ctx, wait); err != nil {
					return nil, err
				}
			} else {
				// Retry-After: 0 (or missing on a 429 we treat as retryable):
				// fall back to exponential backoff. Spinning here would re-fire
				// requests instantly against the same rate-limited endpoint —
				// the same busy-loop guard the v0.1.1 poller carries.
				if err := f.backoff(ctx, attempt); err != nil {
					return nil, err
				}
			}
			continue

		case resp.StatusCode >= 500:
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("workflow fetcher: %s", resp.Status)
			if attempt == maxFetchRetries {
				break
			}
			if err := f.backoff(ctx, attempt); err != nil {
				return nil, err
			}
			continue

		default:
			_ = resp.Body.Close()
			return nil, fmt.Errorf("workflow fetcher: %s", resp.Status)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("workflow fetcher: request to %s failed", u)
	}
	return nil, lastErr
}

func cacheKey(owner, repo, path, ref string) string {
	// Use NULs as separators so a path containing "/" can't collide with a
	// different (owner, repo) split.
	return owner + "\x00" + repo + "\x00" + path + "\x00" + ref
}

func (f *workflowFetcher) cachedETag(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.cache[key]; ok {
		return e.etag
	}
	return ""
}

// touchAndGet promotes the entry to MRU and returns its body. Returns
// (nil, false) if the key is no longer cached (e.g. concurrently evicted).
func (f *workflowFetcher) touchAndGet(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.cache[key]
	if !ok {
		return nil, false
	}
	f.lru.MoveToFront(e.lruElem)
	return e.body, true
}

// store inserts or refreshes the cache entry for key, evicting the LRU
// element if we exceed capacity. An empty etag is allowed — we still keep
// the body cached, we just can't issue a conditional request next time.
func (f *workflowFetcher) store(key, etag string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.cache[key]; ok {
		e.etag = etag
		e.body = body
		f.lru.MoveToFront(e.lruElem)
		return
	}
	e := &cacheEntry{key: key, etag: etag, body: body}
	e.lruElem = f.lru.PushFront(e)
	f.cache[key] = e
	for f.lru.Len() > f.capacity {
		back := f.lru.Back()
		if back == nil {
			break
		}
		victim := back.Value.(*cacheEntry)
		f.lru.Remove(back)
		delete(f.cache, victim.key)
	}
}

// backoff sleeps min(2^attempt * baseDelay, 60s) + jitter in [0, baseDelay).
// Mirrors internal/demand/github_poller.go:backoff — kept as a near-duplicate
// because extracting a one-method helper into a shared package would cost
// more in indirection than the ~10 lines save. The plan flags this as a
// candidate for future consolidation when a third caller appears.
func (f *workflowFetcher) backoff(ctx context.Context, attempt int) error {
	d := f.baseDelay << attempt
	if d > fetcherMaxBackoff || d <= 0 {
		d = fetcherMaxBackoff
	}
	if jitterMax := f.baseDelay; jitterMax > 0 {
		d += time.Duration(rand.Int63n(int64(jitterMax)))
	}
	if d > fetcherMaxBackoff {
		d = fetcherMaxBackoff
	}
	return f.sleepCtx(ctx, d)
}

// sleepCtx waits for d but returns ctx.Err() early if ctx is cancelled. It
// uses the injectable timer so the timer is stopped on cancellation, avoiding
// the goroutine leak the v0.1.1 poller originally had.
func (f *workflowFetcher) sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	c, stop := f.newTimer(d)
	select {
	case <-c:
		return nil
	case <-ctx.Done():
		stop()
		return ctx.Err()
	}
}

// retryAfterDelay parses Retry-After (seconds form only — GitHub uses that
// shape) and reports (wait, retryable). For 429 we always retry; for 403
// without explicit rate-limit signals we treat it as a hard auth failure and
// return retryable=false.
func retryAfterDelay(resp *http.Response, _ time.Time) (time.Duration, bool) {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	// 429 with no Retry-After: treat as retryable with zero wait so the caller
	// falls back to exponential backoff (no busy loop — see Fetch).
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, true
	}
	return 0, false
}
