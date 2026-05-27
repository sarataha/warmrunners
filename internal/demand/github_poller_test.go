// internal/demand/github_poller_test.go
package demand

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	statusQueued = "queued"
	etagABC      = `"abc"`
)

// TestGitHubRESTPoller_SendsUserAgent verifies every outgoing request carries a
// User-Agent matching "warmrunners/<version>".
func TestGitHubRESTPoller_SendsUserAgent(t *testing.T) {
	want := regexp.MustCompile(`^warmrunners/.+$`)
	var mu sync.Mutex
	var bad []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if !want.MatchString(ua) {
			mu.Lock()
			bad = append(bad, ua)
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(runsBody(1))
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	if _, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"}); err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("requests with bad User-Agent: %q", bad)
	}
}

// recordingTimer is a test seam for GitHubRESTPoller.newTimer. It records every
// requested wait duration and fires immediately (closed channel) so tests never
// sleep real wall-clock time. The recorded waits let tests assert the COMPUTED
// backoff/rate-limit delay instead of measuring elapsed time.
type recordingTimer struct {
	mu    sync.Mutex
	waits []time.Duration
}

// fn returns a newTimer-compatible func that records d and fires immediately.
func (rt *recordingTimer) fn() func(d time.Duration) (<-chan time.Time, func() bool) {
	return func(d time.Duration) (<-chan time.Time, func() bool) {
		rt.mu.Lock()
		rt.waits = append(rt.waits, d)
		rt.mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch, func() bool { return true }
	}
}

func (rt *recordingTimer) recorded() []time.Duration {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]time.Duration, len(rt.waits))
	copy(out, rt.waits)
	return out
}

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
			case statusQueued:
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
				job(statusQueued, "self-hosted", "linux", "gpu"), // matches {self-hosted, gpu}
				job(statusQueued, "self-hosted", "linux"),        // missing gpu -> excluded
				job("completed", "self-hosted", "gpu"),           // completed -> excluded
			))
		case strings.HasSuffix(r.URL.Path, "/runs/3/jobs"):
			// run 3: only non-matching queued jobs -> contributes nothing.
			_ = json.NewEncoder(w).Encode(jobsBody(
				job(statusQueued, "ubuntu-latest"),
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

// TestGitHubRESTPoller_ConfigurableTimeout verifies the HTTP client timeout is
// set from the WithHTTPTimeout option.
func TestGitHubRESTPoller_ConfigurableTimeout(t *testing.T) {
	p := NewGitHubRESTPoller("http://example.invalid", "tok", WithHTTPTimeout(3*time.Second))
	if p.client.Timeout != 3*time.Second {
		t.Fatalf("client.Timeout = %v, want 3s", p.client.Timeout)
	}
	// Default must remain 10s when no option is supplied.
	d := NewGitHubRESTPoller("http://example.invalid", "tok")
	if d.client.Timeout != 10*time.Second {
		t.Fatalf("default client.Timeout = %v, want 10s", d.client.Timeout)
	}
}

func TestGitHubRESTPoller_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Tiny base delay so the retry loop on 500 doesn't sleep real seconds.
	p := NewGitHubRESTPoller(srv.URL, "tok", WithBaseDelay(time.Millisecond))
	_, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestGitHubRESTPoller_ETagReusesCacheOn304 verifies that after a 200 with an
// ETag, the next poll sends If-None-Match and a 304 reuses the cached snapshot.
func TestGitHubRESTPoller_ETagReusesCacheOn304(t *testing.T) {
	var mu sync.Mutex
	var ifNoneMatch []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		inm := r.Header.Get("If-None-Match")
		ifNoneMatch = append(ifNoneMatch, inm)
		mu.Unlock()
		w.Header().Set("ETag", etagABC)
		if inm == etagABC {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			switch r.URL.Query().Get("status") {
			case statusQueued:
				_ = json.NewEncoder(w).Encode(runsBody(1))
			default:
				_ = json.NewEncoder(w).Encode(runsBody())
			}
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	first, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Queued != 1 {
		t.Fatalf("first poll snap = %+v, want Queued:1", first)
	}
	second, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second poll snap = %+v, want equal to first %+v", second, first)
	}
	// The second poll must have sent If-None-Match for the cached endpoints.
	sentConditional := false
	for _, v := range ifNoneMatch {
		if v == etagABC {
			sentConditional = true
		}
	}
	if !sentConditional {
		t.Fatalf("expected at least one If-None-Match: \"abc\", got %v", ifNoneMatch)
	}
}

// TestGitHubRESTPoller_ETagUpdatesAfter304 verifies a new ETag after a 304
// replaces the cache entry.
func TestGitHubRESTPoller_ETagUpdatesAfter304(t *testing.T) {
	// runs endpoint state machine: 200 "abc" -> 304 -> 200 "def".
	runsState := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == statusQueued {
			switch runsState {
			case 0:
				w.Header().Set("ETag", etagABC)
				_ = json.NewEncoder(w).Encode(runsBody(1))
			case 1:
				if r.Header.Get("If-None-Match") == etagABC {
					w.Header().Set("ETag", etagABC)
					w.WriteHeader(http.StatusNotModified)
				} else {
					t.Errorf("poll 2: expected If-None-Match abc, got %q", r.Header.Get("If-None-Match"))
				}
			default:
				w.Header().Set("ETag", `"def"`)
				_ = json.NewEncoder(w).Encode(runsBody(1, 5))
			}
			runsState++
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(runsBody())
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	for i := 0; i < 3; i++ {
		if _, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"}); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	// After the third poll, the cache should hold etag "def".
	got := p.cachedETag(srv.URL + "/repos/org/repo/actions/runs?status=queued&per_page=100&page=1")
	if got != `"def"` {
		t.Fatalf("cached etag = %q, want \"def\"", got)
	}
}

// TestGitHubRESTPoller_ETagIndependentKeys verifies distinct (owner,repo,endpoint)
// keys keep independent cache entries.
func TestGitHubRESTPoller_ETagIndependentKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			// Distinct etag per repo so we can prove independence.
			if strings.Contains(r.URL.Path, "/repoA/") {
				w.Header().Set("ETag", `"A"`)
			} else {
				w.Header().Set("ETag", `"B"`)
			}
			if r.URL.Query().Get("status") == statusQueued {
				_ = json.NewEncoder(w).Encode(runsBody(1))
			} else {
				_ = json.NewEncoder(w).Encode(runsBody())
			}
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	if _, err := p.CurrentDemand(context.Background(), "org", "repoA", []string{"self-hosted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CurrentDemand(context.Background(), "org", "repoB", []string{"self-hosted"}); err != nil {
		t.Fatal(err)
	}
	a := p.cachedETag(srv.URL + "/repos/org/repoA/actions/runs?status=queued&per_page=100&page=1")
	b := p.cachedETag(srv.URL + "/repos/org/repoB/actions/runs?status=queued&per_page=100&page=1")
	if a != `"A"` || b != `"B"` {
		t.Fatalf("etags A=%q B=%q, want \"A\" and \"B\"", a, b)
	}
}

// TestGitHubRESTPoller_RetrySucceedsAfter5xx verifies 500,502,then 200 yields
// success after 3 requests.
func TestGitHubRESTPoller_RetrySucceedsAfter5xx(t *testing.T) {
	var mu sync.Mutex
	runsCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == statusQueued {
			mu.Lock()
			runsCalls++
			n := runsCalls
			mu.Unlock()
			switch n {
			case 1:
				http.Error(w, "boom", http.StatusInternalServerError)
			case 2:
				http.Error(w, "boom", http.StatusBadGateway)
			default:
				_ = json.NewEncoder(w).Encode(runsBody(1))
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(runsBody())
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok", WithBaseDelay(time.Millisecond))
	snap, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Queued != 1 {
		t.Fatalf("snap = %+v, want Queued:1", snap)
	}
	if runsCalls != 3 {
		t.Fatalf("runs endpoint hit %d times, want 3", runsCalls)
	}
}

// TestGitHubRESTPoller_RetryExhaustionReturnsError verifies 500x4 → error after
// 3 retries (4 total requests).
func TestGitHubRESTPoller_RetryExhaustionReturnsError(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok", WithBaseDelay(time.Millisecond))
	_, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err == nil {
		t.Fatal("expected error after retry exhaustion, got nil")
	}
	if calls != 4 {
		t.Fatalf("server hit %d times, want 4 (initial + 3 retries)", calls)
	}
}

// TestGitHubRESTPoller_RetryAfterHeader verifies a 429 with Retry-After:1 then
// 200 sleeps ~1s and succeeds.
func TestGitHubRESTPoller_RetryAfterHeader(t *testing.T) {
	var mu sync.Mutex
	runsCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == statusQueued {
			mu.Lock()
			runsCalls++
			n := runsCalls
			mu.Unlock()
			if n == 1 {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).Encode(runsBody(1))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(runsBody())
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	// Inject a fake timer so the suite never sleeps a real second; assert the
	// COMPUTED wait that was passed to the timer instead of wall-clock elapsed.
	rt := &recordingTimer{}
	p.newTimer = rt.fn()
	snap, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Queued != 1 {
		t.Fatalf("snap = %+v, want Queued:1", snap)
	}
	// Retry-After: 1 must produce exactly a 1s computed wait.
	if got := rt.recorded(); len(got) != 1 || got[0] != 1*time.Second {
		t.Fatalf("recorded waits = %v, want exactly [1s] from Retry-After", got)
	}
}

// TestGitHubRESTPoller_RateLimitReset verifies a 403 with X-RateLimit-Remaining:0
// and Reset=now+1 sleeps ~1s and then succeeds.
func TestGitHubRESTPoller_RateLimitReset(t *testing.T) {
	// Fixed clock so the X-RateLimit-Reset → wait computation is deterministic and
	// the suite never sleeps real seconds. Reset is exactly 5s past fakeNow.
	fakeNow := time.Unix(1_700_000_000, 0).UTC()
	resetEpoch := fakeNow.Add(5 * time.Second).Unix()

	var mu sync.Mutex
	runsCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == statusQueued {
			mu.Lock()
			runsCalls++
			n := runsCalls
			mu.Unlock()
			if n == 1 {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetEpoch, 10))
				http.Error(w, "rate limited", http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(runsBody(1))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(runsBody())
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	p.now = func() time.Time { return fakeNow }
	rt := &recordingTimer{}
	p.newTimer = rt.fn()
	snap, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Queued != 1 {
		t.Fatalf("snap = %+v, want Queued:1", snap)
	}
	// Reset is 5s past the (fixed) now, so the computed wait must be exactly 5s.
	if got := rt.recorded(); len(got) != 1 || got[0] != 5*time.Second {
		t.Fatalf("recorded waits = %v, want exactly [5s] from X-RateLimit-Reset", got)
	}
}

// TestGitHubRESTPoller_RateLimitZeroWaitBacksOff verifies that a retryable
// rate-limit response with a non-positive computed wait (Retry-After: 0) does
// NOT busy-loop: it falls back to exponential backoff (a non-zero wait passed to
// the timer) and still succeeds on the following 200. Without the fix the loop
// would fire maxRetries requests instantly with zero delay.
func TestGitHubRESTPoller_RateLimitZeroWaitBacksOff(t *testing.T) {
	var mu sync.Mutex
	runsCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs") && r.URL.Query().Get("status") == statusQueued {
			mu.Lock()
			runsCalls++
			n := runsCalls
			mu.Unlock()
			if n == 1 {
				// Retryable rate-limit signal but a zero wait.
				w.Header().Set("Retry-After", "0")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).Encode(runsBody(1))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/runs") {
			_ = json.NewEncoder(w).Encode(runsBody())
			return
		}
		_ = json.NewEncoder(w).Encode(jobsBody(job(statusQueued, "self-hosted")))
	}))
	defer srv.Close()

	// Non-trivial base delay so the backoff fallback yields a clearly non-zero
	// computed wait; the fake timer means it costs no real time.
	p := NewGitHubRESTPoller(srv.URL, "tok", WithBaseDelay(50*time.Millisecond))
	rt := &recordingTimer{}
	p.newTimer = rt.fn()
	snap, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Queued != 1 {
		t.Fatalf("snap = %+v, want Queued:1", snap)
	}
	// The backoff path must have run: exactly one wait, and it must be > 0
	// (proving we did not spin with zero delay).
	got := rt.recorded()
	if len(got) != 1 {
		t.Fatalf("recorded waits = %v, want exactly one (backoff) wait", got)
	}
	if got[0] <= 0 {
		t.Fatalf("backoff wait = %v, want > 0 (must not busy-loop on zero wait)", got[0])
	}
	// And only one retry was needed (initial 429 + one 200), not maxRetries spins.
	if runsCalls != 2 {
		t.Fatalf("runs endpoint hit %d times, want 2 (429 then 200)", runsCalls)
	}
}

// TestGitHubRESTPoller_ContextCancelDuringWait verifies that cancelling the
// context while a rate-limit wait is pending aborts promptly with the context
// error and does not leak the timer goroutine. The injected timer never fires,
// so only the ctx.Done() branch of sleepCtx can unblock the call.
func TestGitHubRESTPoller_ContextCancelDuringWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always rate-limit with a positive, retryable wait so sleepCtx is entered.
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var stopped int32
	p := NewGitHubRESTPoller(srv.URL, "tok")
	p.newTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
		// Never-firing channel; cancel the context so the wait must abort via ctx.
		cancel()
		return make(chan time.Time), func() bool {
			atomic.StoreInt32(&stopped, 1)
			return true
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := p.CurrentDemand(ctx, "org", "repo", []string{"self-hosted"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a context error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CurrentDemand did not return promptly after context cancel (timer goroutine likely leaked)")
	}
	if atomic.LoadInt32(&stopped) != 1 {
		t.Fatal("timer stop func was not called on context cancel")
	}
}

// TestGitHubRESTPoller_CacheTypeMismatchFailsSoft verifies that a 304 whose
// cached parsed value has a different type than the caller's out does NOT panic
// (which would crash a reconcile) but returns an error instead.
func TestGitHubRESTPoller_CacheTypeMismatchFailsSoft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always reply 304 with the matching ETag so get() takes the cache path.
		w.Header().Set("ETag", `"x"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	p := NewGitHubRESTPoller(srv.URL, "tok")
	u := srv.URL + "/something"
	// Seed the cache with an ETag and a parsed value of the WRONG type (jobsResp)
	// while the caller below asks for *runsResp.
	cachedWrong := &jobsResp{TotalCount: 7}
	p.mu.Lock()
	p.cache[u] = cacheEntry{etag: `"x"`, parsed: cachedWrong}
	p.mu.Unlock()

	var out runsResp
	err := p.get(context.Background(), u, &out)
	if err == nil {
		t.Fatal("expected a type-mismatch error, got nil (would have panicked unguarded)")
	}
	if !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("error = %v, want a type mismatch error", err)
	}
}

// TestGitHubRESTPoller_RequestBuildError verifies FIX C: a request that cannot
// be built (invalid URL) surfaces a non-nil error rather than being dropped.
func TestGitHubRESTPoller_RequestBuildError(t *testing.T) {
	// A control character in the base URL makes http.NewRequestWithContext fail.
	p := NewGitHubRESTPoller("http://exa\x7fmple.com", "tok", WithBaseDelay(time.Millisecond))
	_, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err == nil {
		t.Fatal("expected non-nil error from invalid request URL, got nil")
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

	// Token carries a trailing newline (as it does when loaded from a Secret
	// created via `kubectl --from-file`). It must be trimmed, or the header is
	// rejected with "invalid header field value". Regression: caught in live test.
	p := NewGitHubRESTPoller(srv.URL, "secret-token\n")
	_, err := p.CurrentDemand(context.Background(), "org", "repo", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
}
