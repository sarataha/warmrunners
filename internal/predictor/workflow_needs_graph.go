// internal/predictor/workflow_needs_graph.go
package predictor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/sarataha/warmrunners/internal/predictor/workflow"
)

// DefaultRunsCap is the per-poll cap on active workflow runs scanned for one
// repository when the constructor is given a non-positive value. It matches
// the v0.2.0 CRD default for spec.predictor.maxRunsPerPoll.
const DefaultRunsCap = 50

// maxReusableDepth bounds the recursion through local reusable workflows. The
// spec sets this at 10. GitHub itself enforces a smaller cap at runtime, but
// 10 is a defensible static walk limit regardless of GitHub's current rule.
const maxReusableDepth = 10

// activeStatuses are the GitHub Actions run statuses that contribute to
// imminent demand. We poll each status separately so we can use a single
// ETag per status (the listing's response varies independently per filter).
var activeStatuses = []string{"in_progress", "queued", "pending", "requested", "waiting"}

// Hooks lets a caller (the v0.2.0 reconciler in Task 7) observe per-event
// signals without coupling the predictor to a metrics library. All callbacks
// are optional; nil hooks are no-ops.
//
// OnDynamicSkipped fires once per skipped job. Reasons:
//   - "runs_on_expr": runs-on is an unresolved expression.
//   - "matrix_expr":  matrix is an unresolved expression (e.g. fromJSON).
//   - "remote_uses":  job is a remote reusable workflow.
//   - "depth_exceeded": local reusable recursion exceeded maxReusableDepth.
//   - "cycle":        local reusable recursion detected a cycle.
//
// OnYAMLFetch fires once per call into the WorkflowFetcher. Granularity is
// "fetched" / "error" only — the underlying cache differentiates 200 vs 304
// internally but does not surface that today. A future fetcher API revision
// can promote "cached_304" to a first-class outcome.
type Hooks struct {
	OnDynamicSkipped func(reason string)
	OnYAMLFetch      func(result string)
}

// WorkflowNeedsGraph is the v0.2.0 Predictor implementation. It walks each
// active workflow run's needs: DAG by fetching the run's YAML at head_sha,
// parsing it via internal/predictor/workflow, and emitting one contribution
// per statically-decidable imminent job grouped by resolved runs-on labels.
//
// Concurrency: a single WorkflowNeedsGraph is safe for use by one reconcile
// goroutine at a time. The internal listing-ETag cache is mutex-guarded so
// concurrent Predict calls do not corrupt state, but the predictor is not
// designed for high parallelism — one reconcile per policy is the cadence.
type WorkflowNeedsGraph struct {
	httpClient *http.Client
	fetcher    WorkflowFetcher
	runsCap    int
	baseURL    string

	mu        sync.Mutex
	listCache map[string]*listCacheEntry // key = METHOD+URL

	hooks Hooks
}

// listCacheEntry holds an ETag-conditional listing payload (runs list or
// jobs list). Bodies are kept as raw bytes; callers decode on touch so the
// cache itself is opaque to schema.
type listCacheEntry struct {
	etag string
	body []byte
}

// NewWorkflowNeedsGraph constructs a WorkflowNeedsGraph. httpClient supplies
// auth via its transport (same seam as the v0.1.1 poller). fetcher is the
// ETag-aware YAML cache from Task 4. runsCap <= 0 falls back to DefaultRunsCap.
func NewWorkflowNeedsGraph(httpClient *http.Client, fetcher WorkflowFetcher, runsCap int) *WorkflowNeedsGraph {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if runsCap <= 0 {
		runsCap = DefaultRunsCap
	}
	return &WorkflowNeedsGraph{
		httpClient: httpClient,
		fetcher:    fetcher,
		runsCap:    runsCap,
		baseURL:    defaultGitHubAPI,
		listCache:  make(map[string]*listCacheEntry),
	}
}

// WithHooks attaches optional observation callbacks. Returns the receiver so
// the constructor chain reads naturally: NewWorkflowNeedsGraph(...).WithHooks(h).
func (g *WorkflowNeedsGraph) WithHooks(h Hooks) *WorkflowNeedsGraph {
	g.hooks = h
	return g
}

// withBaseURL is an internal seam for tests that point at an httptest.Server.
func (g *WorkflowNeedsGraph) withBaseURL(u string) *WorkflowNeedsGraph {
	g.baseURL = u
	return g
}

// runRef is the minimal projection of a workflow run we need from the runs
// listing: ID for the jobs sub-call, path + head_sha for the YAML fetch.
type runRef struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	HeadSHA string `json:"head_sha"`
}

type runsListResponse struct {
	WorkflowRuns []runRef `json:"workflow_runs"`
}

type jobRef struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type jobsListResponse struct {
	Jobs []jobRef `json:"jobs"`
}

// Predict implements the Predictor interface.
//
// Listing failures (runs or jobs HTTP error) surface as (Prediction{}, err)
// so the reconciler can mark PredictorAvailable=False. Per-run failures
// (YAML fetch 404, parse error, single jobs-listing error after we've
// already gathered the run set) drop only that run; the call still returns
// a non-nil Prediction. Reusable-workflow load failures collapse only that
// sub-tree; siblings still contribute.
func (g *WorkflowNeedsGraph) Predict(ctx context.Context, owner, repo string) (Prediction, error) {
	out := Prediction{PerLabelSet: make(map[string]int)}

	runs, err := g.listActiveRuns(ctx, owner, repo)
	if err != nil {
		return out, err
	}
	if len(runs) == 0 {
		return out, nil
	}

	for _, run := range runs {
		g.contributeRun(ctx, owner, repo, run, out.PerLabelSet)
	}
	return out, nil
}

// listActiveRuns issues one listing per active-status filter and unions the
// results, capped at runsCap distinct run IDs. Listings are ETag-cached
// inline (mutex-guarded map keyed by request URL).
//
// A listing-level HTTP error is fatal: we cannot reason about which runs are
// active and silently dropping all of them would understate predicted demand.
func (g *WorkflowNeedsGraph) listActiveRuns(ctx context.Context, owner, repo string) ([]runRef, error) {
	seen := make(map[int64]struct{})
	out := make([]runRef, 0, g.runsCap)
	for _, status := range activeStatuses {
		u := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=%s",
			strings.TrimRight(g.baseURL, "/"),
			url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(status))
		body, err := g.cachedGet(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("list runs (status=%s): %w", status, err)
		}
		var parsed runsListResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("decode runs (status=%s): %w", status, err)
		}
		for _, r := range parsed.WorkflowRuns {
			if _, dup := seen[r.ID]; dup {
				continue
			}
			seen[r.ID] = struct{}{}
			out = append(out, r)
			if len(out) >= g.runsCap {
				return out, nil
			}
		}
	}
	return out, nil
}

// contributeRun walks one run's needs: graph and adds its contributions to
// dst. Per-run failures (jobs listing, YAML fetch, parse) are absorbed:
// this run contributes nothing, the rest of the poll continues. This is the
// "drop one run" leg of the error model in spec §5.
func (g *WorkflowNeedsGraph) contributeRun(ctx context.Context, owner, repo string, run runRef, dst map[string]int) {
	materialized, err := g.listJobs(ctx, owner, repo, run.ID)
	if err != nil {
		// Per-run drop: do not surface to caller, do not poison sibling runs.
		return
	}

	body, err := g.fetcher.Fetch(ctx, owner, repo, run.Path, run.HeadSHA)
	if err != nil {
		g.emitYAMLFetch("error")
		// 404 / network error: drop this run. The caller already returns a
		// non-nil Prediction.
		_ = errors.Is(err, ErrNotFound) // documented contract; no special action beyond drop
		return
	}
	g.emitYAMLFetch("fetched")

	wf, err := workflow.Parse(body)
	if err != nil {
		return
	}

	visited := make(map[string]struct{})
	g.walkWorkflow(ctx, owner, repo, run.HeadSHA, wf, materialized, visited, 0, dst)
}

// walkWorkflow walks one parsed workflow's jobs and folds contributions
// into dst. depth is the current reusable-workflow recursion depth; visited
// is the set of (owner/repo/path@sha) keys already on the call stack for
// cycle detection.
//
// For matrix jobs the parser resolves only the FIRST combo's runs-on (see
// internal/predictor/workflow/parse.go:resolveMatrixRunsOn). We emit
// len(Combos) contributions all keyed by that resolved label set. When the
// matrix expands runs-on across multiple distinct label sets (e.g.
// os: [linux, windows]), v0.2.0 collapses them to one key; a future parser
// revision can return per-combo label sets to lift this.
func (g *WorkflowNeedsGraph) walkWorkflow(
	ctx context.Context,
	owner, repo, headSHA string,
	wf workflow.Workflow,
	materialized map[string]struct{},
	visited map[string]struct{},
	depth int,
	dst map[string]int,
) {
	for _, job := range wf.Jobs {
		// Materialized jobs are already owned by the reactive layer.
		if _, ok := materialized[job.ID]; ok {
			continue
		}
		// Imminent = every entry in Needs is materialized. The API only
		// surfaces materialized jobs, so treating any materialized job as a
		// satisfied need is the conservative-safe simplification (spec §3.3
		// step 5; plan Task 5 step 5).
		if !allMaterialized(job.Needs, materialized) {
			continue
		}

		// Reusable-workflow handling (decided before Dynamic so a remote
		// `uses:` registers the right reason).
		if job.UsesRemote {
			g.emitDynamicSkipped("remote_uses")
			continue
		}
		if job.UsesLocal != "" {
			g.recurseLocal(ctx, owner, repo, headSHA, job.UsesLocal, materialized, visited, depth, dst)
			continue
		}

		// Dynamic forms (runs-on expression, matrix expression) are skipped.
		// Job.Dynamic is the OR of the two; differentiate the reason for
		// observability.
		if job.RunsOn.Dynamic {
			g.emitDynamicSkipped("runs_on_expr")
			continue
		}
		if job.Matrix.Dynamic {
			g.emitDynamicSkipped("matrix_expr")
			continue
		}

		// Resolve the contribution label set. RunsOn.Group, if set, is
		// surfaced as a "group:<name>" pseudo-label prepended to the labels
		// list. This is a v0.2.0 pragmatic choice (spec is silent): it lets
		// a policy targeting a runner group filter on the group name via
		// its github.labels while still preserving the literal labels for
		// hashing. Alternative would have been to treat group as Dynamic;
		// that would silently zero out a common configuration shape.
		labels := resolveLabels(job.RunsOn)

		// Emit one contribution per matrix combo (or one if there is no
		// matrix). Each combo is folded into the same key today; see the
		// walkWorkflow doc-comment for the parser limitation.
		count := 1
		if len(job.Matrix.Combos) > 0 {
			count = len(job.Matrix.Combos)
		}
		key := LabelSetKey(labels)
		dst[key] += count
	}
}

// recurseLocal loads a local reusable workflow at headSHA and walks it as
// if its jobs were the calling job's expansion. Bounded by maxReusableDepth
// with cycle detection on (owner/repo/path@sha).
func (g *WorkflowNeedsGraph) recurseLocal(
	ctx context.Context,
	owner, repo, headSHA, usesPath string,
	materialized map[string]struct{},
	visited map[string]struct{},
	depth int,
	dst map[string]int,
) {
	if depth+1 > maxReusableDepth {
		g.emitDynamicSkipped("depth_exceeded")
		return
	}
	// Normalize a "./" prefix off the local uses: path so the cache key is
	// the same shape the contents API expects.
	path := strings.TrimPrefix(usesPath, "./")
	visitKey := owner + "/" + repo + "/" + path + "@" + headSHA
	if _, cycle := visited[visitKey]; cycle {
		g.emitDynamicSkipped("cycle")
		return
	}
	visited[visitKey] = struct{}{}
	defer delete(visited, visitKey)

	body, err := g.fetcher.Fetch(ctx, owner, repo, path, headSHA)
	if err != nil {
		g.emitYAMLFetch("error")
		// Reusable-workflow load failure: this sub-tree contributes nothing;
		// siblings continue (handled by the caller's loop).
		return
	}
	g.emitYAMLFetch("fetched")

	wf, err := workflow.Parse(body)
	if err != nil {
		return
	}
	// A reusable workflow's jobs are not represented in the calling run's
	// jobs listing — they execute as their own jobs at runtime. Pass the
	// same materialized set so a job inside the reusable that happens to
	// share a name with a top-level materialized job is still skipped; in
	// practice the materialized set is per-run and the reusable's jobs
	// haven't appeared, so all interior jobs walk normally.
	g.walkWorkflow(ctx, owner, repo, headSHA, wf, materialized, visited, depth+1, dst)
}

// listJobs returns the set of currently-materialized job NAMES for one run.
// We key on Job.ID (the YAML key) when matching against the parsed workflow
// — see the Step 5 note in the plan. GitHub's jobs listing returns the
// job's `name` field which in the absence of an explicit `name:` defaults
// to the job's YAML key, so name-vs-id matching works in the common case.
// (Workflows that set a custom `name:` will under-count materialization,
// which over-warms rather than under-warms — bounded by floor.max.)
func (g *WorkflowNeedsGraph) listJobs(ctx context.Context, owner, repo string, runID int64) (map[string]struct{}, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?filter=latest",
		strings.TrimRight(g.baseURL, "/"),
		url.PathEscape(owner), url.PathEscape(repo), runID)
	body, err := g.cachedGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var parsed jobsListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(parsed.Jobs))
	for _, j := range parsed.Jobs {
		out[j.Name] = struct{}{}
	}
	return out, nil
}

// cachedGet issues a conditional GET and returns the body. The URL is the
// cache key; If-None-Match is set when we have a prior ETag, and a 304
// returns the cached payload without consuming primary-rate-limit quota
// (see spec §4).
func (g *WorkflowNeedsGraph) cachedGet(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if etag := g.cachedETag(u); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		body, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, rerr
		}
		g.storeList(u, resp.Header.Get("ETag"), body)
		return body, nil
	case http.StatusNotModified:
		if cached, ok := g.cachedBody(u); ok {
			return cached, nil
		}
		return nil, fmt.Errorf("304 with no cached payload for %s", u)
	default:
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
}

func (g *WorkflowNeedsGraph) cachedETag(key string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.listCache[key]; ok {
		return e.etag
	}
	return ""
}

func (g *WorkflowNeedsGraph) cachedBody(key string) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.listCache[key]; ok {
		return e.body, true
	}
	return nil, false
}

func (g *WorkflowNeedsGraph) storeList(key, etag string, body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listCache[key] = &listCacheEntry{etag: etag, body: body}
}

func (g *WorkflowNeedsGraph) emitDynamicSkipped(reason string) {
	if g.hooks.OnDynamicSkipped != nil {
		g.hooks.OnDynamicSkipped(reason)
	}
}

func (g *WorkflowNeedsGraph) emitYAMLFetch(result string) {
	if g.hooks.OnYAMLFetch != nil {
		g.hooks.OnYAMLFetch(result)
	}
}

// allMaterialized returns true when every name in needs is present in
// materialized. An empty needs list trivially returns true.
func allMaterialized(needs []string, materialized map[string]struct{}) bool {
	for _, n := range needs {
		if _, ok := materialized[n]; !ok {
			return false
		}
	}
	return true
}

// resolveLabels returns the label set to key a contribution under. A
// non-empty RunsOn.Group is prepended as "group:<name>" so a policy can
// filter on the group while still keying contributions by the literal
// labels too. See the walkWorkflow doc-comment for the rationale.
func resolveLabels(r workflow.RunsOnSpec) []string {
	if r.Group == "" {
		return r.Labels
	}
	out := make([]string, 0, len(r.Labels)+1)
	out = append(out, "group:"+r.Group)
	out = append(out, r.Labels...)
	return out
}
