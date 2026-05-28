// internal/activity/workflow_runs_sampler.go
package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sarataha/warmrunners/internal/predictor"
	"github.com/sarataha/warmrunners/internal/predictor/workflow"
	"github.com/sarataha/warmrunners/internal/version"
)

// DefaultRunsCap is the per-poll cap on workflow_runs scanned for one
// repository when the constructor is given a non-positive value. Matches
// `internal/predictor.DefaultRunsCap` deliberately — the predictor and the
// sampler each scan up to this many runs per poll, sharing the YAML cache.
const DefaultRunsCap = 50

// allowedEvents are the workflow_run.event values that count as human CI
// signal. Anything outside this set is dropped via the event filter. The list
// is conservative: schedule, check_run, check_suite, and workflow_run are all
// either timer-driven or chained from other workflows and would otherwise
// keep the floor warm even on a quiet repo (spec §3.4 / §3.5).
//
// Stored as a set rather than the four banned values so an unknown future
// event type defaults to "filtered" rather than "human" — the safer of the
// two failure modes for a controller that adjusts a runner pool.
var allowedEvents = map[string]struct{}{
	"push":                        {},
	"pull_request":                {},
	"pull_request_target":         {},
	"pull_request_review_comment": {},
	"workflow_dispatch":           {},
	"repository_dispatch":         {},
}

// Hooks expose per-event metrics signals. All callbacks are optional; nil
// hooks are no-ops. The reconciler in PR 2 wires these to the
// `warmrunners_activity_*` Prometheus collectors.
//
// OnYAMLFetch fires once per fetcher call. Result is "fetched" or "error";
// the underlying cache distinguishes 200 vs 304 internally but does not
// surface that today — same granularity as the predictor's matching hook.
type Hooks struct {
	OnBotFiltered    func(reason string) // bot_type | trigger_bot_type | bot_suffix | denylist
	OnEventFiltered  func(event string)  // schedule | check_run | check_suite | workflow_run | ...
	OnYAMLFetch      func(result string) // fetched | error
	OnDynamicSkipped func(reason string) // runs_on_expr | matrix_expr | remote_uses
}

// WorkflowRunsSampler implements Activity by calling
// GET /repos/{o}/{r}/actions/runs?created=>{since}&per_page=100 and parsing
// each non-bot run's workflow YAML at head_sha via the v0.2.x WorkflowFetcher.
//
// The sampler is intentionally narrow: it does NOT recurse through reusable
// workflows, walk the needs: DAG, or examine the materialized jobs listing.
// That belongs to the predictor; the activity signal asks "did a human push
// something?" and sizes by the triggered workflows' fanout (spec §3.6).
//
// Concurrency: safe for a single reconcile goroutine per repository. The
// embedded fetcher is mutex-guarded; the sampler itself holds no per-call
// mutable state beyond its hooks (set once via WithHooks).
type WorkflowRunsSampler struct {
	httpClient *http.Client
	fetcher    predictor.WorkflowFetcher
	runsCap    int
	userAgent  string
	baseURL    string
	now        func() time.Time

	hooks Hooks
}

// NewWorkflowRunsSampler constructs a sampler reusing the predictor's
// WorkflowFetcher (so the YAML cache is process-shared). runsCap <= 0 falls
// back to DefaultRunsCap.
//
// The Authorization header is set per-request from the token argument to
// Sample (see the v0.2.1 hotfix mirror in fetchRunsList). Do not bake a token
// into httpClient's transport; that would prevent the per-policy auth model.
func NewWorkflowRunsSampler(httpClient *http.Client, fetcher predictor.WorkflowFetcher, runsCap int) *WorkflowRunsSampler {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if runsCap <= 0 {
		runsCap = DefaultRunsCap
	}
	return &WorkflowRunsSampler{
		httpClient: httpClient,
		fetcher:    fetcher,
		runsCap:    runsCap,
		userAgent:  "warmrunners/" + version.Version,
		baseURL:    "https://api.github.com",
		now:        time.Now,
	}
}

// WithHooks attaches optional observation callbacks. Returns the receiver so
// the constructor chain reads naturally:
// NewWorkflowRunsSampler(...).WithHooks(h).
func (s *WorkflowRunsSampler) WithHooks(h Hooks) *WorkflowRunsSampler {
	s.hooks = h
	return s
}

// withBaseURL is an internal seam for tests pointing at an httptest.Server.
// Not exported: production code talks to GitHub directly.
func (s *WorkflowRunsSampler) withBaseURL(u string) *WorkflowRunsSampler {
	s.baseURL = u
	return s
}

// withNow overrides the clock for window-edge tests.
func (s *WorkflowRunsSampler) withNow(fn func() time.Time) *WorkflowRunsSampler {
	if fn != nil {
		s.now = fn
	}
	return s
}

// workflowRun is the projection of /actions/runs we care about. Fields the
// JSON payload may omit (actor / triggering_actor) decode as zero-value
// structs, which IsBotActor treats as "User" (the falsy default), so a
// malformed-but-present run still flows through the filter chain rather than
// crashing the poll.
type workflowRun struct {
	ID              int64      `json:"id"`
	Event           string     `json:"event"`
	Path            string     `json:"path"`
	HeadSHA         string     `json:"head_sha"`
	CreatedAt       time.Time  `json:"created_at"`
	Actor           simpleUser `json:"actor"`
	TriggeringActor simpleUser `json:"triggering_actor"`
}

type simpleUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type runsListResponse struct {
	WorkflowRuns []workflowRun `json:"workflow_runs"`
}

// Sample implements Activity. See spec §3 (especially §3.5 + §3.6).
//
// Error model:
//   - Listing-level HTTP / decode error → (Sample{}, err). Hooks not called.
//     The reconciler surfaces this as ActivityAvailable=False.
//   - Per-run YAML fetch error → drop that run only, fire OnYAMLFetch("error"),
//     other runs still contribute. The call still returns nil error.
//   - Per-run parse error → drop that run only, no hook (the underlying YAML
//     either parses or it doesn't; further granularity would be noise).
func (s *WorkflowRunsSampler) Sample(
	ctx context.Context,
	owner, repo, token string,
	window time.Duration,
	denylist []string,
) (Sample, error) {
	out := Sample{PerLabelSet: map[string]int{}}

	runs, err := s.fetchRunsList(ctx, owner, repo, token, window)
	if err != nil {
		return Sample{}, err
	}

	scanned := 0
	for _, run := range runs {
		if scanned >= s.runsCap {
			break
		}
		// Filter precedence: bot → event → parse. Bot/event filters are
		// cheap and decouple us from the YAML fetch quota; do them first.
		if isBot, reason := IsBotActor(run.Actor.Type, run.TriggeringActor.Type, run.Actor.Login, denylist); isBot {
			s.emitBotFiltered(reason)
			scanned++
			continue
		}
		if _, ok := allowedEvents[run.Event]; !ok {
			s.emitEventFiltered(run.Event)
			scanned++
			continue
		}

		s.contributeRun(ctx, owner, repo, token, run, out.PerLabelSet)
		scanned++
	}
	return out, nil
}

// fetchRunsList issues the single workflow_runs query with the
// `created=>{since}` filter. Mirrors the v0.2.1 predictor's auth pattern
// EXACTLY: TrimSpace(token), Authorization: Bearer when non-empty,
// Accept: application/vnd.github+json, User-Agent: warmrunners/<version>.
//
// ETag caching of the runs list is OPTIONAL for v0.3.0 — best-effort, not
// implemented here. The `since` value slides per poll, which makes an ETag
// most useful only when two polls fall in the same minute bucket; the
// shared YAML fetcher cache absorbs the dominant cost.
func (s *WorkflowRunsSampler) fetchRunsList(ctx context.Context, owner, repo, token string, window time.Duration) ([]workflowRun, error) {
	since := s.now().UTC().Add(-window).Format(time.RFC3339)
	// url.Values handles the `>` → `%3E` percent-encoding for us.
	q := url.Values{}
	q.Set("created", ">"+since)
	q.Set("per_page", "100")
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs?%s",
		strings.TrimRight(s.baseURL, "/"),
		url.PathEscape(owner), url.PathEscape(repo), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", s.userAgent)
	// Trim whitespace on the token at the use site. Tokens loaded from
	// Secrets created via `kubectl --from-file` or `echo` carry a trailing
	// newline, which makes an invalid Authorization header value. This is
	// the v0.2.1 hotfix mirror — do not ship without it.
	if tok := strings.TrimSpace(token); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("activity sampler: GET %s: %s", u, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var parsed runsListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("activity sampler: decode runs: %w", err)
	}
	return parsed.WorkflowRuns, nil
}

// contributeRun fetches the run's YAML, parses it, and folds each surviving
// job's fanout into dst. A YAML fetch error drops only this run; siblings
// still contribute.
func (s *WorkflowRunsSampler) contributeRun(
	ctx context.Context,
	owner, repo, token string,
	run workflowRun,
	dst map[string]int,
) {
	body, err := s.fetcher.Fetch(ctx, owner, repo, run.Path, run.HeadSHA, token)
	if err != nil {
		s.emitYAMLFetch("error")
		return
	}
	s.emitYAMLFetch("fetched")

	wf, err := workflow.Parse(body)
	if err != nil {
		return
	}

	for _, job := range wf.Jobs {
		// Reusable-workflow handling first so a remote uses: registers
		// the right reason. The activity sampler — unlike the predictor —
		// does NOT recurse into local reusable workflows; we skip both
		// remote AND local uses: jobs and let the predictor's needs-walk
		// own that codepath. Local reusables that ARE the entire workflow
		// will simply contribute nothing, which is the safer of the two
		// over/under-warm failure modes.
		if job.UsesRemote {
			s.emitDynamicSkipped("remote_uses")
			continue
		}
		if job.UsesLocal != "" {
			s.emitDynamicSkipped("remote_uses")
			continue
		}
		if job.RunsOn.Dynamic {
			s.emitDynamicSkipped("runs_on_expr")
			continue
		}
		if job.Matrix.Dynamic {
			s.emitDynamicSkipped("matrix_expr")
			continue
		}

		labels := resolveLabels(job.RunsOn)
		count := 1
		if len(job.Matrix.Combos) > 0 {
			count = len(job.Matrix.Combos)
		}
		key := predictor.LabelSetKey(labels)
		dst[key] += count
	}
}

// resolveLabels mirrors the predictor's helper of the same name — duplicated
// here rather than re-exported across packages because (a) the predictor
// keeps its helper unexported by design (one-responsibility-per-file), and
// (b) the activity sampler is small enough that the duplication cost is
// lower than the export-and-couple cost. If a third caller appears, lift
// both to a shared internal/labelset package.
func resolveLabels(r workflow.RunsOnSpec) []string {
	if r.Group == "" {
		return r.Labels
	}
	out := make([]string, 0, len(r.Labels)+1)
	out = append(out, "group:"+r.Group)
	out = append(out, r.Labels...)
	return out
}

func (s *WorkflowRunsSampler) emitBotFiltered(reason string) {
	if s.hooks.OnBotFiltered != nil {
		s.hooks.OnBotFiltered(reason)
	}
}

func (s *WorkflowRunsSampler) emitEventFiltered(event string) {
	if s.hooks.OnEventFiltered != nil {
		s.hooks.OnEventFiltered(event)
	}
}

func (s *WorkflowRunsSampler) emitYAMLFetch(result string) {
	if s.hooks.OnYAMLFetch != nil {
		s.hooks.OnYAMLFetch(result)
	}
}

func (s *WorkflowRunsSampler) emitDynamicSkipped(reason string) {
	if s.hooks.OnDynamicSkipped != nil {
		s.hooks.OnDynamicSkipped(reason)
	}
}
