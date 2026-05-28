// internal/predictor/cache_test.go
package predictor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingTimer mirrors the v0.1.1 poller's test seam: it captures every
// requested wait duration and fires immediately so tests don't sleep real
// wall-clock time. Re-implemented here rather than reaching across packages —
// see Task 4 plan note on duplication vs premature extraction.
type recordingTimer struct {
	mu        sync.Mutex
	waits     []time.Duration
	stopCalls int
	// blockUntil, if non-nil, makes the timer block on this channel instead of
	// firing immediately. Used by the cancel-during-sleep test.
	blockUntil chan struct{}
}

func (rt *recordingTimer) fn() func(d time.Duration) (<-chan time.Time, func() bool) {
	return func(d time.Duration) (<-chan time.Time, func() bool) {
		rt.mu.Lock()
		rt.waits = append(rt.waits, d)
		block := rt.blockUntil
		rt.mu.Unlock()
		if block != nil {
			ch := make(chan time.Time)
			stop := func() bool {
				rt.mu.Lock()
				rt.stopCalls++
				rt.mu.Unlock()
				return true
			}
			// Goroutine waits for block; if it fires, deliver tick.
			go func() {
				<-block
				select {
				case ch <- time.Time{}:
				default:
				}
			}()
			return ch, stop
		}
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch, func() bool {
			rt.mu.Lock()
			rt.stopCalls++
			rt.mu.Unlock()
			return true
		}
	}
}

func (rt *recordingTimer) recorded() []time.Duration {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]time.Duration, len(rt.waits))
	copy(out, rt.waits)
	return out
}

func (rt *recordingTimer) stops() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.stopCalls
}

// newTestFetcher builds a WorkflowFetcher pointed at srv with the recording
// timer and a microsecond base delay so retries are effectively instantaneous.
func newTestFetcher(t *testing.T, srv *httptest.Server, rt *recordingTimer, opts ...Option) *workflowFetcher {
	t.Helper()
	all := append([]Option{
		WithBaseDelay(1 * time.Microsecond),
		WithNewTimer(rt.fn()),
		withBaseURL(srv.URL),
	}, opts...)
	f := NewWorkflowFetcher(srv.Client(), "test", all...).(*workflowFetcher)
	return f
}

// Test 1: 200 with ETag is cached; the next call sends If-None-Match and a 304
// reply returns the cached body without re-parsing.
func TestWorkflowFetcher_ETagRoundtrip(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("first call: unexpected If-None-Match: %q", got)
			}
			w.Header().Set("ETag", `"abc"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("yaml-body"))
			return
		}
		if got := r.Header.Get("If-None-Match"); got != `"abc"` {
			t.Errorf("second call: If-None-Match = %q, want \"abc\"", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, &recordingTimer{})

	b1, err := f.Fetch(context.Background(), "o", "r", "p.yml", "sha", "")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if string(b1) != "yaml-body" {
		t.Fatalf("first body = %q, want yaml-body", b1)
	}

	b2, err := f.Fetch(context.Background(), "o", "r", "p.yml", "sha", "")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if string(b2) != "yaml-body" {
		t.Fatalf("second body = %q, want yaml-body (from cache via 304)", b2)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2 (one OK + one 304)", got)
	}
}

// Test 2: repeated calls always go to the wire (with If-None-Match); the
// invariant documented for the fetcher is "always send If-None-Match", not "no
// HTTP request at all". We just assert the body comes back unchanged.
func TestWorkflowFetcher_RepeatedSameKey(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, &recordingTimer{})
	for i := 0; i < 5; i++ {
		b, err := f.Fetch(context.Background(), "o", "r", "p", "ref", "")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if string(b) != "payload" {
			t.Fatalf("call %d body = %q", i, b)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("server calls = %d, want 5 (always send If-None-Match)", got)
	}
}

// Test 3: different keys are cached independently.
func TestWorkflowFetcher_DistinctKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		w.Header().Set("ETag", `"`+ref+`"`)
		_, _ = w.Write([]byte("body-" + ref + "-" + r.URL.Path))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, &recordingTimer{})
	keys := []struct{ o, r, p, ref string }{
		{"o", "r", "a.yml", "sha1"},
		{"o", "r", "a.yml", "sha2"},
		{"o", "r", "b.yml", "sha1"},
		{"o", "r2", "a.yml", "sha1"},
		{"o2", "r", "a.yml", "sha1"},
	}
	seen := make(map[string]string)
	for _, k := range keys {
		b, err := f.Fetch(context.Background(), k.o, k.r, k.p, k.ref, "")
		if err != nil {
			t.Fatalf("%+v: %v", k, err)
		}
		seen[fmt.Sprintf("%+v", k)] = string(b)
	}
	if len(seen) != len(keys) {
		t.Fatalf("expected %d distinct bodies, got %d (%v)", len(keys), len(seen), seen)
	}
}

// Test 4: LRU eviction. With capacity=2, insert three keys; the first
// should be evicted (and refetched on next access).
func TestWorkflowFetcher_LRUEviction(t *testing.T) {
	var byKey sync.Map // key -> hit count
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path + "?" + r.URL.RawQuery
		v, _ := byKey.LoadOrStore(key, new(int32))
		atomic.AddInt32(v.(*int32), 1)
		w.Header().Set("ETag", `"e"`)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv, &recordingTimer{}, WithCapacity(2))

	ctx := context.Background()
	mustFetch := func(p string) {
		t.Helper()
		if _, err := f.Fetch(ctx, "o", "r", p, "sha", ""); err != nil {
			t.Fatalf("fetch %s: %v", p, err)
		}
	}

	mustFetch("a") // cache: [a]
	mustFetch("b") // cache: [a, b]
	mustFetch("c") // cache: [b, c]; a evicted
	mustFetch("a") // a was evicted, so this is a fresh 200, count = 2

	v, _ := byKey.Load("/repos/o/r/contents/a?ref=sha")
	if got := atomic.LoadInt32(v.(*int32)); got != 2 {
		t.Fatalf("key a request count = %d, want 2 (evicted then refetched)", got)
	}
	// b and c should each have been hit only once.
	for _, p := range []string{"b", "c"} {
		v, _ := byKey.Load("/repos/o/r/contents/" + p + "?ref=sha")
		if got := atomic.LoadInt32(v.(*int32)); got != 1 {
			t.Fatalf("key %s request count = %d, want 1", p, got)
		}
	}
}

// Test 5: 404 returns ErrNotFound via errors.Is.
func TestWorkflowFetcher_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	f := newTestFetcher(t, srv, &recordingTimer{})

	_, err := f.Fetch(context.Background(), "o", "r", "missing.yml", "sha", "")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Test 6: 500 → 502 → 200 succeeds with two recorded backoffs.
func TestWorkflowFetcher_RetriesOn5xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq := atomic.AddInt32(&n, 1)
		switch seq {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.Header().Set("ETag", `"x"`)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()

	rt := &recordingTimer{}
	f := newTestFetcher(t, srv, rt)

	b, err := f.Fetch(context.Background(), "o", "r", "p", "s", "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(b) != "ok" {
		t.Fatalf("body = %q", b)
	}
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	if len(rt.recorded()) < 1 {
		t.Fatalf("expected at least one recorded backoff, got %v", rt.recorded())
	}
}

// Test 7: 500 forever exhausts retries; exactly maxRetries+1 requests.
func TestWorkflowFetcher_ExhaustsRetries(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rt := &recordingTimer{}
	f := newTestFetcher(t, srv, rt)

	_, err := f.Fetch(context.Background(), "o", "r", "p", "s", "")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if got := atomic.LoadInt32(&n); got != int32(maxFetchRetries+1) {
		t.Fatalf("requests = %d, want %d", got, maxFetchRetries+1)
	}
}

// Test 8: 429 with Retry-After: 1 sleeps exactly ~1s via the seam.
func TestWorkflowFetcher_RetryAfterHonored(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq := atomic.AddInt32(&n, 1)
		if seq == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("ETag", `"x"`)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	rt := &recordingTimer{}
	f := newTestFetcher(t, srv, rt)

	if _, err := f.Fetch(context.Background(), "o", "r", "p", "s", ""); err != nil {
		t.Fatal(err)
	}
	waits := rt.recorded()
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("recorded waits = %v, want exactly [1s]", waits)
	}
}

// Test 9: 429 with Retry-After: 0 must NOT busy-loop; falls back to exponential
// backoff (recorded wait > 0).
func TestWorkflowFetcher_RetryAfterZeroFallsBackToBackoff(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq := atomic.AddInt32(&n, 1)
		if seq == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("ETag", `"x"`)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	rt := &recordingTimer{}
	// Use a slightly larger base delay (still tiny) so the backoff is observable.
	f := newTestFetcher(t, srv, rt, WithBaseDelay(1*time.Millisecond))

	if _, err := f.Fetch(context.Background(), "o", "r", "p", "s", ""); err != nil {
		t.Fatal(err)
	}
	waits := rt.recorded()
	if len(waits) != 1 {
		t.Fatalf("recorded waits = %v, want exactly one (fallback backoff)", waits)
	}
	if waits[0] <= 0 {
		t.Fatalf("backoff wait = %v, want > 0", waits[0])
	}
}

// Test 10: ctx.Done during sleep returns context.Canceled and stops the timer.
func TestWorkflowFetcher_ContextCancelDuringSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rt := &recordingTimer{blockUntil: make(chan struct{})}
	f := newTestFetcher(t, srv, rt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.Fetch(ctx, "o", "r", "p", "s", "")
		done <- err
	}()

	// Wait until the fetcher has called newTimer (i.e. it's sleeping).
	deadline := time.Now().Add(2 * time.Second)
	for len(rt.recorded()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("fetcher never reached the sleep")
		}
		time.Sleep(1 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Fetch didn't return after cancel")
	}
	if rt.stops() < 1 {
		t.Fatalf("timer stop not called; stops=%d", rt.stops())
	}
}

// Test: User-Agent is set per spec.
func TestWorkflowFetcher_UserAgent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.Header().Set("ETag", `"x"`)
		_, _ = w.Write([]byte("k"))
	}))
	defer srv.Close()
	f := newTestFetcher(t, srv, &recordingTimer{})
	if _, err := f.Fetch(context.Background(), "o", "r", "p", "s", ""); err != nil {
		t.Fatal(err)
	}
	if ua != "warmrunners/test" {
		t.Fatalf("User-Agent = %q, want warmrunners/test", ua)
	}
}

// Test: Authorization header is set when a non-empty token is passed.
func TestWorkflowFetcher_AuthorizationHeader_SetWhenTokenProvided(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("ETag", `"x"`)
		_, _ = w.Write([]byte("k"))
	}))
	defer srv.Close()
	f := newTestFetcher(t, srv, &recordingTimer{})
	if _, err := f.Fetch(context.Background(), "o", "r", "p", "s", "tok-abc"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok-abc" {
		t.Fatalf("Authorization = %q, want %q", auth, "Bearer tok-abc")
	}
}

// Test: Authorization header is NOT set when token is empty.
func TestWorkflowFetcher_AuthorizationHeader_OmittedWhenTokenEmpty(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("ETag", `"x"`)
		_, _ = w.Write([]byte("k"))
	}))
	defer srv.Close()
	f := newTestFetcher(t, srv, &recordingTimer{})
	if _, err := f.Fetch(context.Background(), "o", "r", "p", "s", ""); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Fatalf("Authorization = %q, want empty (no token)", auth)
	}
}

// Test: token whitespace (leading/trailing newline) is trimmed before being
// placed in the Authorization header. Mirrors the v0.1.x poller fix for
// kubectl --from-file / echo-style Secret payloads.
func TestWorkflowFetcher_AuthorizationHeader_TrimsTokenWhitespace(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("ETag", `"x"`)
		_, _ = w.Write([]byte("k"))
	}))
	defer srv.Close()
	f := newTestFetcher(t, srv, &recordingTimer{})
	if _, err := f.Fetch(context.Background(), "o", "r", "p", "s", "  tok-trim\n"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok-trim" {
		t.Fatalf("Authorization = %q, want %q (trimmed)", auth, "Bearer tok-trim")
	}
}

// Test: Accept header is application/vnd.github.raw.
func TestWorkflowFetcher_AcceptHeader(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.Header().Set("ETag", `"x"`)
		_, _ = w.Write([]byte("k"))
	}))
	defer srv.Close()
	f := newTestFetcher(t, srv, &recordingTimer{})
	if _, err := f.Fetch(context.Background(), "o", "r", "p", "s", ""); err != nil {
		t.Fatal(err)
	}
	if accept != "application/vnd.github.raw" {
		t.Fatalf("Accept = %q", accept)
	}
}
