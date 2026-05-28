// internal/predictor/workflow_needs_graph_test.go
package predictor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeServer captures the HTTP-side behavior of GitHub's REST: a runs
// listing per status, a per-run jobs listing, and a "raw" contents endpoint
// for workflow YAML. Tests configure the bodies by URL prefix and inspect
// hit counts on the way out.
//
// We do not implement ETag conditional behavior here unless a test sets it
// — the predictor's listing cache logic is exercised by the ETag-roundtrip
// test below; other tests skip ETag entirely so they can assert on body
// content without juggling 304 replays.
type fakeServer struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server

	mu    sync.Mutex
	calls map[string]int
	yaml  *yamlRoutes
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeServer{t: t, mux: mux, srv: srv, calls: make(map[string]int)}
}

func (f *fakeServer) handle(path string, h http.HandlerFunc) {
	f.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.URL.Path+"?"+r.URL.RawQuery]++
		f.mu.Unlock()
		h(w, r)
	})
}

// loadTestdata reads a YAML fixture from internal/predictor/testdata.
func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// runsListJSON renders a minimal /actions/runs listing body.
func runsListJSON(runs ...runRef) string {
	var sb strings.Builder
	sb.WriteString(`{"workflow_runs":[`)
	for i, r := range runs {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%d,"path":%q,"head_sha":%q}`, r.ID, r.Path, r.HeadSHA)
	}
	sb.WriteString("]}")
	return sb.String()
}

// jobsListJSON renders a minimal /actions/runs/{id}/jobs listing body.
func jobsListJSON(names ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"jobs":[`)
	for i, n := range names {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"name":%q,"status":"in_progress"}`, n)
	}
	sb.WriteString("]}")
	return sb.String()
}

// newGraph constructs a predictor wired to the fake server. It builds a
// real WorkflowFetcher (the Task 4 cache) also pointing at the same fake
// server so YAML fetches hit our routes.
func newGraph(t *testing.T, fs *fakeServer, runsCap int) *WorkflowNeedsGraph {
	t.Helper()
	rt := &recordingTimer{}
	fetcher := newTestFetcher(t, fs.srv, rt)
	return NewWorkflowNeedsGraph(fs.srv.Client(), fetcher, runsCap).withBaseURL(fs.srv.URL)
}

// servePerStatusRuns wires up handlers for every active-status filter,
// returning the given runs only under the `in_progress` filter (which is
// what every current test exercises) and empty bodies for the others.
// This isolates a test from accidentally getting duplicate IDs across
// status filters.
func (fs *fakeServer) servePerStatusRuns(t *testing.T, runs []runRef) {
	t.Helper()
	const statusForRuns = "in_progress"
	fs.handle("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("status") == statusForRuns {
			_, _ = w.Write([]byte(runsListJSON(runs...)))
			return
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	})
}

// yamlRoutes is a per-server map of workflow path → body for the catchall
// /repos/o/r/contents/ handler. The fetcher PathEscape's the workflow path
// into one URL segment, so a single mux handler at /repos/o/r/contents/
// dispatches on the decoded URL path.
type yamlRoutes struct {
	mu     sync.Mutex
	bodies map[string][]byte
	codes  map[string]int
}

func (fs *fakeServer) ensureContentsMux() *yamlRoutes {
	if fs.yaml != nil {
		return fs.yaml
	}
	fs.yaml = &yamlRoutes{bodies: make(map[string][]byte), codes: make(map[string]int)}
	fs.handle("/repos/o/r/contents/", func(w http.ResponseWriter, r *http.Request) {
		// The fetcher PathEscape's the workflow path into one segment. The
		// httptest mux URL-decodes it back, so r.URL.Path here is the
		// concatenation /repos/o/r/contents/<decoded-path>.
		key := strings.TrimPrefix(r.URL.Path, "/repos/o/r/contents/")
		fs.yaml.mu.Lock()
		body, ok := fs.yaml.bodies[key]
		code := fs.yaml.codes[key]
		fs.yaml.mu.Unlock()
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
	return fs.yaml
}

// serveYAML wires a contents endpoint for one workflow path.
func (fs *fakeServer) serveYAML(path string, body []byte) {
	y := fs.ensureContentsMux()
	y.mu.Lock()
	y.bodies[path] = body
	y.mu.Unlock()
}

// serveYAMLStatus wires an explicit HTTP status (e.g. 404) for one path.
func (fs *fakeServer) serveYAMLStatus(path string, status int) {
	y := fs.ensureContentsMux()
	y.mu.Lock()
	y.bodies[path] = nil
	y.codes[path] = status
	// Mark as present so the mux returns the status rather than 404 from
	// "not registered". A nil body with a non-zero code is the signal.
	y.bodies[path] = []byte{}
	y.mu.Unlock()
}

// Scenario 1: single active run, stage1 in_progress, stage2 needs stage1
// with runs-on [self-hosted, gpu] → PerLabelSet[gpu,self-hosted] = 1.
func TestPredict_TwoStageStage2Imminent(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/two_stage.yml", HeadSHA: "deadbeef"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON("stage1")))
	})
	fs.serveYAML(".github/workflows/two_stage.yml", loadTestdata(t, "two_stage.yml"))

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	wantKey := LabelSetKey([]string{"self-hosted", "gpu"})
	if got := pred.PerLabelSet[wantKey]; got != 1 {
		t.Fatalf("PerLabelSet[%q] = %d, want 1 (full map: %v)", wantKey, got, pred.PerLabelSet)
	}
}

// Scenario 2: stage1 has completed and stage2 is materialized → 0
// contribution for that run.
func TestPredict_Stage2Materialized(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/two_stage.yml", HeadSHA: "deadbeef"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON("stage1", "stage2")))
	})
	fs.serveYAML(".github/workflows/two_stage.yml", loadTestdata(t, "two_stage.yml"))

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(pred.PerLabelSet) != 0 {
		t.Fatalf("want empty map, got %v", pred.PerLabelSet)
	}
}

// Scenario 3: two active runs in different workflows contribute independently.
func TestPredict_MultipleRunsContribute(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{
		{ID: 1, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha1"},
		{ID: 2, Path: ".github/workflows/two_stage_large.yml", HeadSHA: "sha2"},
	})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON("stage1")))
	})
	fs.handle("/repos/o/r/actions/runs/2/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON("stage1")))
	})
	fs.serveYAML(".github/workflows/two_stage.yml", loadTestdata(t, "two_stage.yml"))
	fs.serveYAML(".github/workflows/two_stage_large.yml", loadTestdata(t, "two_stage_large.yml"))

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	gpuKey := LabelSetKey([]string{"self-hosted", "gpu"})
	largeKey := LabelSetKey([]string{"self-hosted", "large"})
	if pred.PerLabelSet[gpuKey] != 1 {
		t.Errorf("gpu key = %d, want 1; map=%v", pred.PerLabelSet[gpuKey], pred.PerLabelSet)
	}
	if pred.PerLabelSet[largeKey] != 1 {
		t.Errorf("large key = %d, want 1; map=%v", pred.PerLabelSet[largeKey], pred.PerLabelSet)
	}
}

// Scenario 4: matrix-expanded job with os ∈ {ubuntu, macos, windows} and
// no other jobs. With no materialized jobs the matrix job is itself
// imminent (empty Needs). The parser collapses runs-on to the first combo,
// so we expect len(Combos) contributions to one key.
func TestPredict_MatrixExpansion(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/matrix_simple.yml", HeadSHA: "sha"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON()))
	})
	fs.serveYAML(".github/workflows/matrix_simple.yml", loadTestdata(t, "matrix_simple.yml"))

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	// All three combos collapse to one key today (parser limitation
	// documented on walkWorkflow). The total should equal len(Combos)=3.
	total := 0
	for _, n := range pred.PerLabelSet {
		total += n
	}
	if total != 3 {
		t.Fatalf("matrix total = %d, want 3 (map=%v)", total, pred.PerLabelSet)
	}
}

// Scenario 5: dynamic runs-on (${{ inputs.x }}) → 0 contribution +
// OnDynamicSkipped("runs_on_expr") called.
func TestPredict_DynamicRunsOnSkipped(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/dynamic.yml", HeadSHA: "sha"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON()))
	})
	fs.serveYAML(".github/workflows/dynamic.yml", loadTestdata(t, "dynamic.yml"))

	var skipReasons []string
	var mu sync.Mutex
	g := newGraph(t, fs, 0)
	g.WithHooks(Hooks{OnDynamicSkipped: func(r string) {
		mu.Lock()
		defer mu.Unlock()
		skipReasons = append(skipReasons, r)
	}})

	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(pred.PerLabelSet) != 0 {
		t.Fatalf("want empty map, got %v", pred.PerLabelSet)
	}
	// dynamic.yml has dyn_runson (runs_on_expr) and dyn_matrix (matrix_expr,
	// which also forces RunsOn.Dynamic). The branch ordering puts
	// runs_on_expr first when RunsOn.Dynamic is true, so dyn_matrix should
	// surface as runs_on_expr too. Verify at least one of each kind appears.
	gotRunsOn := false
	for _, r := range skipReasons {
		if r == "runs_on_expr" {
			gotRunsOn = true
		}
	}
	if !gotRunsOn {
		t.Errorf("expected at least one runs_on_expr skip; got %v", skipReasons)
	}
}

// Scenario 6: local uses: ./.github/workflows/inner.yml → inner job
// contributes back.
func TestPredict_LocalReusableRecurse(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/caller_local.yml", HeadSHA: "sha"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON()))
	})
	fs.serveYAML(".github/workflows/caller_local.yml", loadTestdata(t, "caller_local.yml"))
	fs.serveYAML(".github/workflows/inner.yml", loadTestdata(t, "inner.yml"))

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	wantKey := LabelSetKey([]string{"self-hosted", "inner"})
	if got := pred.PerLabelSet[wantKey]; got != 1 {
		t.Fatalf("inner key %q = %d, want 1; map=%v", wantKey, got, pred.PerLabelSet)
	}
}

// Scenario 7: remote uses: → 0 contribution + OnDynamicSkipped("remote_uses").
func TestPredict_RemoteReusableSkipped(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/reusable_remote.yml", HeadSHA: "sha"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON()))
	})
	fs.serveYAML(".github/workflows/reusable_remote.yml", loadTestdata(t, "reusable_remote.yml"))

	var reasons []string
	var mu sync.Mutex
	g := newGraph(t, fs, 0)
	g.WithHooks(Hooks{OnDynamicSkipped: func(r string) {
		mu.Lock()
		defer mu.Unlock()
		reasons = append(reasons, r)
	}})

	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(pred.PerLabelSet) != 0 {
		t.Fatalf("want empty map, got %v", pred.PerLabelSet)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 1 || reasons[0] != "remote_uses" {
		t.Fatalf("reasons = %v, want [remote_uses]", reasons)
	}
}

// Scenario 8: YAML 404 for one run drops only that run; the other still
// contributes.
func TestPredict_YAMLNotFoundDropsRun(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{
		{ID: 1, Path: ".github/workflows/missing.yml", HeadSHA: "sha1"},
		{ID: 2, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha2"},
	})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON("stage1")))
	})
	fs.handle("/repos/o/r/actions/runs/2/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON("stage1")))
	})
	fs.serveYAMLStatus(".github/workflows/missing.yml", http.StatusNotFound)
	fs.serveYAML(".github/workflows/two_stage.yml", loadTestdata(t, "two_stage.yml"))

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	wantKey := LabelSetKey([]string{"self-hosted", "gpu"})
	if got := pred.PerLabelSet[wantKey]; got != 1 {
		t.Fatalf("survivor key = %d, want 1; map=%v", got, pred.PerLabelSet)
	}
}

// Scenario 9: jobs endpoint network error → per-run drop (not fatal), so
// Predict returns a non-nil Prediction. Listing failure is the only fatal
// case; that's covered separately by TestPredict_RunsListingErrorFatal.
func TestPredict_JobsEndpointErrorDropsRun(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{
		{ID: 1, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha"},
	})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	fs.serveYAML(".github/workflows/two_stage.yml", loadTestdata(t, "two_stage.yml"))

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if len(pred.PerLabelSet) != 0 {
		t.Fatalf("want empty map after per-run drop, got %v", pred.PerLabelSet)
	}
}

// Scenario 9b: hard listing failure → (Prediction{}, err).
func TestPredict_RunsListingErrorFatal(t *testing.T) {
	fs := newFakeServer(t)
	fs.handle("/repos/o/r/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	g := newGraph(t, fs, 0)
	_, err := g.Predict(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("expected error from runs listing failure, got nil")
	}
}

// Scenario 10: runsCap = 1 with 5 active runs → only 1 run is scanned.
func TestPredict_RunsCapLimitsFanOut(t *testing.T) {
	fs := newFakeServer(t)
	runs := []runRef{
		{ID: 1, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha"},
		{ID: 2, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha"},
		{ID: 3, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha"},
		{ID: 4, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha"},
		{ID: 5, Path: ".github/workflows/two_stage.yml", HeadSHA: "sha"},
	}
	fs.servePerStatusRuns(t, runs)

	var jobsHits atomic.Int32
	fs.handle("/repos/o/r/actions/runs/", func(w http.ResponseWriter, r *http.Request) {
		// Match any run-id under /actions/runs/{id}/jobs.
		if !strings.HasSuffix(r.URL.Path, "/jobs") {
			http.NotFound(w, r)
			return
		}
		jobsHits.Add(1)
		_, _ = w.Write([]byte(jobsListJSON("stage1")))
	})
	fs.serveYAML(".github/workflows/two_stage.yml", loadTestdata(t, "two_stage.yml"))

	g := newGraph(t, fs, 1)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	if got := jobsHits.Load(); got != 1 {
		t.Fatalf("jobs endpoint hits = %d, want 1 (runsCap=1)", got)
	}
	wantKey := LabelSetKey([]string{"self-hosted", "gpu"})
	if pred.PerLabelSet[wantKey] != 1 {
		t.Fatalf("expected one contribution, got %v", pred.PerLabelSet)
	}
}

// Scenario 11: reusable-workflow recursion depth > 10 → bail safely with
// "depth_exceeded" skip. We synthesize an 11-deep chain in the fetcher by
// returning a caller pointing at level-N+1 for each level.
func TestPredict_ReusableDepthExceeded(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/level0.yml", HeadSHA: "sha"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON()))
	})
	// Each levelN points at levelN+1. Far past depth 10.
	for i := 0; i <= 15; i++ {
		body := fmt.Sprintf(`name: level%d
on: [push, workflow_call]
jobs:
  call:
    uses: ./.github/workflows/level%d.yml
`, i, i+1)
		fs.serveYAML(".github/workflows/level"+itoa(i)+".yml", []byte(body))
	}

	var reasons []string
	var mu sync.Mutex
	g := newGraph(t, fs, 0)
	g.WithHooks(Hooks{OnDynamicSkipped: func(r string) {
		mu.Lock()
		defer mu.Unlock()
		reasons = append(reasons, r)
	}})

	_, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	gotDepth := false
	for _, r := range reasons {
		if r == "depth_exceeded" {
			gotDepth = true
		}
	}
	if !gotDepth {
		t.Fatalf("expected depth_exceeded skip; reasons=%v", reasons)
	}
}

// Scenario 12: reusable-workflow cycle (A→B→A) bails safely.
func TestPredict_ReusableCycle(t *testing.T) {
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/cycle_a.yml", HeadSHA: "sha"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON()))
	})
	fs.serveYAML(".github/workflows/cycle_a.yml", loadTestdata(t, "cycle_a.yml"))
	fs.serveYAML(".github/workflows/cycle_b.yml", loadTestdata(t, "cycle_b.yml"))

	var reasons []string
	var mu sync.Mutex
	g := newGraph(t, fs, 0)
	g.WithHooks(Hooks{OnDynamicSkipped: func(r string) {
		mu.Lock()
		defer mu.Unlock()
		reasons = append(reasons, r)
	}})

	_, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	gotCycle := false
	for _, r := range reasons {
		if r == "cycle" {
			gotCycle = true
		}
	}
	if !gotCycle {
		t.Fatalf("expected cycle skip; reasons=%v", reasons)
	}
}

// Group-handling test: a runs-on group yields a "group:<name>" pseudo-label
// prepended to the labels list. This is the v0.2.0 pragmatic choice; see
// the resolveLabels doc comment.
func TestPredict_GroupHandling(t *testing.T) {
	// matrix.yml's groupform job: group: my-group, labels: [a, b].
	// We need a workflow with a single group-form job and no other jobs so
	// no Needs blockers obscure the contribution.
	yamlBody := []byte(`name: gtest
on:
  push:
jobs:
  groupform:
    runs-on:
      group: my-group
      labels: [a, b]
    steps:
      - run: echo hi
`)
	fs := newFakeServer(t)
	fs.servePerStatusRuns(t, []runRef{{ID: 1, Path: ".github/workflows/group.yml", HeadSHA: "sha"}})
	fs.handle("/repos/o/r/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsListJSON()))
	})
	fs.serveYAML(".github/workflows/group.yml", yamlBody)

	g := newGraph(t, fs, 0)
	pred, err := g.Predict(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("Predict: %v", err)
	}
	wantKey := LabelSetKey([]string{"group:my-group", "a", "b"})
	if got := pred.PerLabelSet[wantKey]; got != 1 {
		t.Fatalf("group key %q = %d, want 1; map=%v", wantKey, got, pred.PerLabelSet)
	}
}

// runsCap defaulting: a non-positive value coerces to DefaultRunsCap.
func TestNewWorkflowNeedsGraph_DefaultRunsCap(t *testing.T) {
	g := NewWorkflowNeedsGraph(nil, nil, 0)
	if g.runsCap != DefaultRunsCap {
		t.Fatalf("runsCap = %d, want %d", g.runsCap, DefaultRunsCap)
	}
	g2 := NewWorkflowNeedsGraph(nil, nil, -5)
	if g2.runsCap != DefaultRunsCap {
		t.Fatalf("negative runsCap should default; got %d", g2.runsCap)
	}
	g3 := NewWorkflowNeedsGraph(nil, nil, 7)
	if g3.runsCap != 7 {
		t.Fatalf("explicit runsCap dropped; got %d", g3.runsCap)
	}
}

// itoa is a tiny helper to avoid importing strconv only for fmt.Sprintf
// usage in the depth test (we already import fmt).
func itoa(i int) string { return fmt.Sprintf("%d", i) }
