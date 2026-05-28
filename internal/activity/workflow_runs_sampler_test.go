// internal/activity/workflow_runs_sampler_test.go
package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarataha/warmrunners/internal/predictor"
)

// rewriteTransport redirects every outbound request to a target httptest
// server. We need this because predictor.NewWorkflowFetcher hardcodes the
// GitHub API base and only exposes `withBaseURL` to package-internal callers,
// so we cannot point the real fetcher at our httptest server through public
// API. Wrapping the http.Client transport is the smallest defensible
// workaround — flagged in the task report as a predictor-API rough edge.
type rewriteTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	return rt.inner.RoundTrip(req)
}

func newTestHTTPClient(srv *httptest.Server) *http.Client {
	u, _ := url.Parse(srv.URL)
	return &http.Client{Transport: &rewriteTransport{target: u, inner: http.DefaultTransport}}
}

// simpleWorkflowPath is the canonical fixture path used across tests; the
// activity sampler treats every workflow path opaquely, so a single string
// constant keeps lint quiet and the tests easy to scan.
const simpleWorkflowPath = ".github/workflows/simple.yml"

// loadPredictorTestdata reads a YAML fixture from the predictor's testdata
// directory. We deliberately do not duplicate fixtures — the v0.2.x
// repertoire covers the activity-sampler's parse needs. The name parameter
// is kept for forward-compatibility even though every current caller passes
// "simple.yml"; future scenarios will reach for matrix.yml / two_stage.yml.
func loadPredictorTestdata(t *testing.T, name string) []byte { //nolint:unparam // future tests will pass other fixtures
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "predictor", "testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// fakeServer wires routes for both the workflow_runs listing and the
// contents endpoint that the WorkflowFetcher hits. Each test registers
// runs + YAML payloads; bot filter / event filter tests only need runs.
type fakeServer struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server

	// runs is reset per-test; held under mu only while the server reads it.
	mu         sync.Mutex
	runs       []map[string]interface{}
	yaml       map[string][]byte
	yamlStatus map[string]int
	runsCalls  int32
	yamlCalls  map[string]*int32
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{
		t:          t,
		mux:        http.NewServeMux(),
		yaml:       map[string][]byte{},
		yamlStatus: map[string]int{},
		yamlCalls:  map[string]*int32{},
	}
	fs.srv = httptest.NewServer(fs.mux)
	t.Cleanup(fs.srv.Close)

	fs.mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.runsCalls, 1)
		fs.mu.Lock()
		runs := fs.runs
		fs.mu.Unlock()
		body := map[string]interface{}{"workflow_runs": runs}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	fs.mux.HandleFunc("/repos/o/r/contents/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/repos/o/r/contents/")
		fs.mu.Lock()
		body, ok := fs.yaml[key]
		code := fs.yamlStatus[key]
		c, exists := fs.yamlCalls[key]
		if !exists {
			var zero int32
			c = &zero
			fs.yamlCalls[key] = c
		}
		fs.mu.Unlock()
		atomic.AddInt32(c, 1)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write(body)
	})
	return fs
}

func (fs *fakeServer) setRuns(runs []map[string]interface{}) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.runs = runs
}

func (fs *fakeServer) setYAML(path string, body []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.yaml[path] = body
}

func (fs *fakeServer) setYAMLStatus(path string, code int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.yaml[path] = []byte{}
	fs.yamlStatus[path] = code
}

// run constructs one workflow_run JSON object with sensible defaults.
func run(id int64, event, path, sha, actorLogin, actorType, trigType string, createdAt time.Time) map[string]interface{} {
	return map[string]interface{}{
		"id":               id,
		"event":            event,
		"path":             path,
		"head_sha":         sha,
		"created_at":       createdAt.UTC().Format(time.RFC3339),
		"actor":            map[string]string{"login": actorLogin, "type": actorType},
		"triggering_actor": map[string]string{"login": actorLogin, "type": trigType},
	}
}

func newSampler(t *testing.T, fs *fakeServer, runsCap int) *WorkflowRunsSampler {
	t.Helper()
	client := newTestHTTPClient(fs.srv)
	// Real predictor fetcher; the rewriteTransport sends its api.github.com
	// requests to fs.srv. Shares the cache shape production uses.
	fetcher := predictor.NewWorkflowFetcher(client, "test")
	return NewWorkflowRunsSampler(client, fetcher, runsCap).withBaseURL(fs.srv.URL)
}

// fixedNow returns a clock function pinned at t.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// ---------- Scenarios ----------

// 1. Empty window → empty sample, no fetcher calls.
func TestSample_EmptyWindow(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns(nil)
	s := newSampler(t, fs, 0)

	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got.PerLabelSet) != 0 {
		t.Fatalf("expected empty map, got %v", got.PerLabelSet)
	}
}

// 2. Single non-bot push run with runs-on [self-hosted, gpu] + matrix os=[a,b,c] → 3 contributions to that key.
func TestSample_MatrixFanout(t *testing.T) {
	fs := newFakeServer(t)
	wfPath := ".github/workflows/m.yml"
	yaml := []byte(`name: m
on: push
jobs:
  test:
    runs-on: [self-hosted, gpu]
    strategy:
      matrix:
        os: [a, b, c]
    steps: [{run: "echo"}]
`)
	fs.setYAML(wfPath, yaml)
	fs.setRuns([]map[string]interface{}{
		run(1, "push", wfPath, "sha1", "alice", "User", "User", time.Now().Add(-1*time.Minute)),
	})

	s := newSampler(t, fs, 0)
	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	key := predictor.LabelSetKey([]string{"self-hosted", "gpu"})
	if got.PerLabelSet[key] != 3 {
		t.Fatalf("PerLabelSet[%q]=%d want 3 (map=%v)", key, got.PerLabelSet[key], got.PerLabelSet)
	}
}

// 3. actor.type == "Bot" → filtered with reason "bot_type".
func TestSample_BotFilter_ActorType(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns([]map[string]interface{}{
		run(1, "push", ".github/workflows/x.yml", "sha", "somebot", "Bot", "User", time.Now()),
	})
	s := newSampler(t, fs, 0)

	var reasons []string
	var mu sync.Mutex
	s.WithHooks(Hooks{OnBotFiltered: func(r string) { mu.Lock(); defer mu.Unlock(); reasons = append(reasons, r) }})

	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got.PerLabelSet) != 0 {
		t.Fatalf("want empty map, got %v", got.PerLabelSet)
	}
	if len(reasons) != 1 || reasons[0] != "bot_type" {
		t.Fatalf("reasons=%v want [bot_type]", reasons)
	}
}

// 4. triggering_actor.type == "Bot" with actor User → trigger_bot_type.
func TestSample_BotFilter_TriggerType(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns([]map[string]interface{}{
		run(1, "push", ".github/workflows/x.yml", "sha", "alice", "User", "Bot", time.Now()),
	})
	s := newSampler(t, fs, 0)
	var reasons []string
	var mu sync.Mutex
	s.WithHooks(Hooks{OnBotFiltered: func(r string) { mu.Lock(); defer mu.Unlock(); reasons = append(reasons, r) }})

	_, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(reasons) != 1 || reasons[0] != "trigger_bot_type" {
		t.Fatalf("reasons=%v want [trigger_bot_type]", reasons)
	}
}

// 5. login dependabot[bot] → bot_suffix.
func TestSample_BotFilter_Suffix(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns([]map[string]interface{}{
		run(1, "push", ".github/workflows/x.yml", "sha", "dependabot[bot]", "User", "User", time.Now()),
	})
	s := newSampler(t, fs, 0)
	var reasons []string
	var mu sync.Mutex
	s.WithHooks(Hooks{OnBotFiltered: func(r string) { mu.Lock(); defer mu.Unlock(); reasons = append(reasons, r) }})

	_, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, BuiltinDenylist)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(reasons) != 1 || reasons[0] != "bot_suffix" {
		t.Fatalf("reasons=%v want [bot_suffix]", reasons)
	}
}

// 6. Denylist match (snyk-bot, no suffix) → denylist.
func TestSample_BotFilter_Denylist(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns([]map[string]interface{}{
		run(1, "push", ".github/workflows/x.yml", "sha", "snyk-bot", "User", "User", time.Now()),
	})
	s := newSampler(t, fs, 0)
	var reasons []string
	var mu sync.Mutex
	s.WithHooks(Hooks{OnBotFiltered: func(r string) { mu.Lock(); defer mu.Unlock(); reasons = append(reasons, r) }})

	_, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, []string{"snyk-bot"})
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(reasons) != 1 || reasons[0] != "denylist" {
		t.Fatalf("reasons=%v want [denylist]", reasons)
	}
}

// 7. event: schedule → filtered.
func TestSample_EventFilter_Schedule(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns([]map[string]interface{}{
		run(1, "schedule", ".github/workflows/x.yml", "sha", "alice", "User", "User", time.Now()),
	})
	s := newSampler(t, fs, 0)
	var events []string
	var mu sync.Mutex
	s.WithHooks(Hooks{OnEventFiltered: func(e string) { mu.Lock(); defer mu.Unlock(); events = append(events, e) }})

	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(got.PerLabelSet) != 0 {
		t.Fatalf("want empty, got %v", got.PerLabelSet)
	}
	if len(events) != 1 || events[0] != "schedule" {
		t.Fatalf("events=%v want [schedule]", events)
	}
}

// 8. event: check_run → filtered.
func TestSample_EventFilter_CheckRun(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns([]map[string]interface{}{
		run(1, "check_run", ".github/workflows/x.yml", "sha", "alice", "User", "User", time.Now()),
	})
	s := newSampler(t, fs, 0)
	var events []string
	var mu sync.Mutex
	s.WithHooks(Hooks{OnEventFiltered: func(e string) { mu.Lock(); defer mu.Unlock(); events = append(events, e) }})

	_, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(events) != 1 || events[0] != "check_run" {
		t.Fatalf("events=%v want [check_run]", events)
	}
}

// 9. event: pull_request → NOT filtered.
func TestSample_EventAllowed_PullRequest(t *testing.T) {
	fs := newFakeServer(t)
	wfPath := simpleWorkflowPath
	fs.setYAML(wfPath, loadPredictorTestdata(t, "simple.yml"))
	fs.setRuns([]map[string]interface{}{
		run(1, "pull_request", wfPath, "sha", "alice", "User", "User", time.Now()),
	})
	s := newSampler(t, fs, 0)
	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	key := predictor.LabelSetKey([]string{"ubuntu-latest"})
	if got.PerLabelSet[key] != 1 {
		t.Fatalf("want 1 contribution for ubuntu-latest, got %v", got.PerLabelSet)
	}
}

// 10. Window-edge inclusion. The activity sampler delegates the include/exclude
// decision to GitHub via the `created=>since` query param — the sampler itself
// counts whatever the API returns. The behavior under test here is therefore
// the QUERY PARAM SHAPE: `since` is RFC3339 UTC, computed as now-window. We
// pin `now` and assert the outbound URL.
func TestSample_QueryShape_CreatedSince(t *testing.T) {
	fs := newFakeServer(t)
	fs.setRuns(nil)
	s := newSampler(t, fs, 0)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.withNow(fixedNow(now))

	var capturedQuery string
	var mu sync.Mutex
	// Re-register the runs handler so we can capture the query.
	fs.mux = http.NewServeMux()
	fs.mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedQuery = r.URL.RawQuery
		mu.Unlock()
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	})
	fs.srv.Config.Handler = fs.mux

	_, err := s.Sample(context.Background(), "o", "r", "", 10*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	mu.Lock()
	q := capturedQuery
	mu.Unlock()
	wantSince := now.Add(-10 * time.Minute).Format(time.RFC3339)
	// url-encoded form: created=%3E2026-01-02T02:54:05Z
	if !strings.Contains(q, "created=%3E"+url.QueryEscape(wantSince)) {
		t.Fatalf("query %q missing created=>%s", q, wantSince)
	}
	if !strings.Contains(q, "per_page=100") {
		t.Fatalf("query %q missing per_page=100", q)
	}
}

// 11. Dynamic-form job skipped; sibling concrete job in same run still counts.
func TestSample_DynamicJobSkippedSiblingCounts(t *testing.T) {
	fs := newFakeServer(t)
	wfPath := ".github/workflows/mixed.yml"
	yaml := []byte(`name: mixed
on: push
jobs:
  dyn:
    runs-on: ${{ inputs.runner }}
    steps: [{run: "echo"}]
  concrete:
    runs-on: [self-hosted, gpu]
    steps: [{run: "echo"}]
`)
	fs.setYAML(wfPath, yaml)
	fs.setRuns([]map[string]interface{}{
		run(1, "push", wfPath, "sha", "alice", "User", "User", time.Now()),
	})

	var skipped []string
	var mu sync.Mutex
	s := newSampler(t, fs, 0).WithHooks(Hooks{OnDynamicSkipped: func(r string) { mu.Lock(); defer mu.Unlock(); skipped = append(skipped, r) }})

	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	gpuKey := predictor.LabelSetKey([]string{"self-hosted", "gpu"})
	if got.PerLabelSet[gpuKey] != 1 {
		t.Fatalf("want gpu=1, got %v", got.PerLabelSet)
	}
	if len(skipped) != 1 || skipped[0] != "runs_on_expr" {
		t.Fatalf("skipped=%v want [runs_on_expr]", skipped)
	}
}

// 12. Multi-run aggregation. Two runs both pointing at the same workflow → counts sum.
func TestSample_MultiRunAggregation(t *testing.T) {
	fs := newFakeServer(t)
	wfPath := simpleWorkflowPath
	fs.setYAML(wfPath, loadPredictorTestdata(t, "simple.yml"))
	fs.setRuns([]map[string]interface{}{
		run(1, "push", wfPath, "sha", "alice", "User", "User", time.Now()),
		run(2, "push", wfPath, "sha", "bob", "User", "User", time.Now()),
	})
	s := newSampler(t, fs, 0)
	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	key := predictor.LabelSetKey([]string{"ubuntu-latest"})
	if got.PerLabelSet[key] != 2 {
		t.Fatalf("want 2 ubuntu contributions, got %v", got.PerLabelSet)
	}
}

// 13. runsCap=1 with 5 active runs → only first scanned.
func TestSample_RunsCapHonored(t *testing.T) {
	fs := newFakeServer(t)
	wfPath := simpleWorkflowPath
	fs.setYAML(wfPath, loadPredictorTestdata(t, "simple.yml"))
	var runs []map[string]interface{}
	for i := int64(1); i <= 5; i++ {
		runs = append(runs, run(i, "push", wfPath, fmt.Sprintf("sha%d", i), "alice", "User", "User", time.Now()))
	}
	fs.setRuns(runs)
	s := newSampler(t, fs, 1)
	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	key := predictor.LabelSetKey([]string{"ubuntu-latest"})
	if got.PerLabelSet[key] != 1 {
		t.Fatalf("runsCap=1 should yield 1 contribution, got %v", got.PerLabelSet)
	}
}

// 14. Per-run YAML 404 drops that run; siblings contribute.
func TestSample_PerRunYAMLErrorDropsOnly(t *testing.T) {
	fs := newFakeServer(t)
	badPath := ".github/workflows/missing.yml"
	goodPath := simpleWorkflowPath
	fs.setYAMLStatus(badPath, http.StatusNotFound)
	fs.setYAML(goodPath, loadPredictorTestdata(t, "simple.yml"))
	fs.setRuns([]map[string]interface{}{
		run(1, "push", badPath, "sha1", "alice", "User", "User", time.Now()),
		run(2, "push", goodPath, "sha2", "bob", "User", "User", time.Now()),
	})

	var fetchResults []string
	var mu sync.Mutex
	s := newSampler(t, fs, 0).WithHooks(Hooks{OnYAMLFetch: func(r string) { mu.Lock(); defer mu.Unlock(); fetchResults = append(fetchResults, r) }})

	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	key := predictor.LabelSetKey([]string{"ubuntu-latest"})
	if got.PerLabelSet[key] != 1 {
		t.Fatalf("want 1 contribution from good run, got %v", got.PerLabelSet)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawError, sawFetched bool
	for _, r := range fetchResults {
		if r == "error" {
			sawError = true
		}
		if r == "fetched" {
			sawFetched = true
		}
	}
	if !sawError || !sawFetched {
		t.Fatalf("want both error+fetched hooks, got %v", fetchResults)
	}
}

// 15. Network error on runs list → (Sample{}, err), no hooks called.
func TestSample_RunsListError(t *testing.T) {
	// Build a sampler pointing at a closed server URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := newTestHTTPClient(srv)
	fetcher := predictor.NewWorkflowFetcher(client, "test")
	s := NewWorkflowRunsSampler(client, fetcher, 0).withBaseURL(srv.URL)

	var hookFired bool
	s.WithHooks(Hooks{OnYAMLFetch: func(string) { hookFired = true }})

	got, err := s.Sample(context.Background(), "o", "r", "", 15*time.Minute, nil)
	if err == nil {
		t.Fatalf("want error, got Sample %v", got)
	}
	if got.PerLabelSet != nil {
		t.Fatalf("want zero-value Sample, got %v", got)
	}
	if hookFired {
		t.Fatal("OnYAMLFetch should not fire when runs list itself fails")
	}
}

// 16. Authorization header set when token non-empty. Mirrors the v0.2.1
// predictor hotfix — the bug we don't want to re-ship.
func TestSample_AuthHeaderSet(t *testing.T) {
	var capturedAuth string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := newTestHTTPClient(srv)
	fetcher := predictor.NewWorkflowFetcher(client, "test")
	s := NewWorkflowRunsSampler(client, fetcher, 0).withBaseURL(srv.URL)

	// Token deliberately carries a trailing newline — same kubectl-from-file
	// shape that bit the v0.1.x demand poller and the v0.2.x predictor.
	_, err := s.Sample(context.Background(), "o", "r", "ghp_test123\n", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	mu.Lock()
	got := capturedAuth
	mu.Unlock()
	if got != "Bearer ghp_test123" {
		t.Fatalf("Authorization=%q want %q (TrimSpace of token)", got, "Bearer ghp_test123")
	}
}

// AuthHeaderOmitted: empty token must NOT produce an Authorization header.
// Catches the inverse of test 16 — a hardcoded Authorization line would be
// invisible to scenario 16 alone.
func TestSample_AuthHeaderOmittedWhenTokenEmpty(t *testing.T) {
	var capturedAuth string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := newTestHTTPClient(srv)
	fetcher := predictor.NewWorkflowFetcher(client, "test")
	s := NewWorkflowRunsSampler(client, fetcher, 0).withBaseURL(srv.URL)

	_, err := s.Sample(context.Background(), "o", "r", "   ", 15*time.Minute, nil)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	mu.Lock()
	got := capturedAuth
	mu.Unlock()
	if got != "" {
		t.Fatalf("Authorization=%q want empty for whitespace-only token", got)
	}
}
