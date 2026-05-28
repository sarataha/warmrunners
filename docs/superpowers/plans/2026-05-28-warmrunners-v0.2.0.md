# warmrunners v0.2.0 Codebase-aware Predictor — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add a `Predictor` component that reads the `needs:` graph of active GitHub Actions `workflow_runs` and contributes a per-policy floor candidate to the reconciler, so downstream runner pools are warm by the time their jobs queue.

**Architecture:** New `internal/predictor/` package with a `Predictor` interface and a `WorkflowNeedsGraph` implementation. Two new dependencies: `github.com/rhysd/actionlint` for typed workflow-YAML parsing, `github.com/bmatcuk/doublestar/v4` only if we end up matching globs (the v0.2.0 design does not need glob matching — see Task 1). Reconciler is extended to call `Predictor.Predict`, fold the result into a `max(...)`-of-floors per policy, and surface new metrics + status fields. `scheduler.Heuristic.Decide` stays unchanged; integration is at the reconciler level.

**Tech Stack:** Go 1.25, kubebuilder 4.9.0, controller-runtime, `rhysd/actionlint`, GitHub REST API (workflow_runs, jobs, contents endpoints), `prometheus/client_golang`. Same toolchain as v0.1.x.

**Source spec:** `docs/superpowers/specs/2026-05-28-warmrunners-v0.2.0.md`. Read it before starting any task — it carries the matching-direction rule, failure-mode posture, and the §3 architecture.

**Branch:** `release/v0.2.0` off `main`.

**Commit policy (per `CLAUDE.local.md`):** Component-grouped commits. Conventional Commits prefixes (`feat(scope):`, `fix(scope):`, `test(scope):`, `docs:`, `ci:`, `chore:`); scope is the codebase area touched, never the version. Signed (`-S`). No co-author trailers. Aim for ~6 commits across the build (one per component once green), not per-task.

---

## File map

**Create:**
- `internal/predictor/predictor.go` — the `Predictor` interface, the `Prediction` type, helpers.
- `internal/predictor/workflow_needs_graph.go` — the `WorkflowNeedsGraph` implementation.
- `internal/predictor/workflow/parse.go` — actionlint-backed parser; exposes a typed view (runs-on resolution, matrix expansion, `needs:` adjacency, local-`uses:` follow).
- `internal/predictor/cache.go` — ETag-cached fetcher for workflow YAML at a `(repo, path, head_sha)` key.
- `internal/predictor/predictor_test.go`, `internal/predictor/workflow/parse_test.go`, `internal/predictor/cache_test.go`, `internal/predictor/workflow_needs_graph_test.go`.
- `internal/predictor/testdata/` — fixture workflow YAML files.

**Modify:**
- `go.mod` / `go.sum` — `go get github.com/rhysd/actionlint@<latest>`.
- `api/v1alpha1/warmrunnerpolicy_types.go` — add `Spec.Predictor` (`PredictorConfig`), `Status.PredictedFloor`, `Status.PredictedLabelSets`, `PredictorAvailable` condition.
- `api/v1alpha1/zz_generated.deepcopy.go` — regenerated.
- `config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml` — regenerated.
- `api/v1alpha1/testdata/golden_crd.yaml` — updated golden snapshot.
- `cmd/main.go` — instantiate the predictor and wire it onto the reconciler.
- `internal/controller/warmrunnerpolicy_controller.go` — add `Predictor` field, call `Predict` per reconcile, fold result into the floor via `max(...)`, populate new status fields and condition.
- `internal/controller/metrics.go` — register the three new gauges/counter.
- `examples/policy-arc.yaml`, `examples/policy-garm.yaml` — add a `predictor:` block to one example to document the shape.
- `README.md` — short "How it works" extension covering the predictor; keep the v0.2.0 roadmap line already in place.
- `CHANGELOG.md` — `## [0.2.0]` section.

**Removed:** none.

**RBAC:** the controller already has `contents: read` and `actions: read` via the GitHub token; no `kubebuilder:rbac` marker changes needed in this PR — the predictor reads via the user-supplied token, not the cluster ServiceAccount.

---

## Task 1: Branch + dependency

**Files:**
- `go.mod`, `go.sum`

- [ ] **Step 1.** Verify clean main on the latest squash commit; `git status` shows clean. Branch: `git checkout -b release/v0.2.0`.
- [ ] **Step 2.** Baseline: `make test`, all GREEN; record coverage. `make lint`, no issues.
- [ ] **Step 3.** Add the parser dependency: `go get github.com/rhysd/actionlint@latest` then `go mod tidy`.
- [ ] **Step 4.** Confirm: `go build ./...` succeeds; `make test` still GREEN.
- [ ] **Step 5.** Commit: `chore(deps): add rhysd/actionlint for workflow YAML parsing`.

---

## Task 2: Predictor interface + Prediction type

**Files:**
- Create: `internal/predictor/predictor.go`
- Create: `internal/predictor/predictor_test.go` (table-driven tests on the helpers only — no impl yet)

**Acceptance criteria:**
- Exported `Predictor` interface with `Predict(ctx, owner, repo, token string) (Prediction, error)`.
- Exported `Prediction` struct with `PerLabelSet map[string]int`. A small helper `LabelSetKey(labels []string) string` returns a deterministic, sorted-comma-joined hash key for a label set (so `[gpu, self-hosted]` and `[self-hosted, gpu]` produce the same key).
- All names match the spec verbatim (`Predictor`, `Prediction`, `PerLabelSet`).
- `go vet ./...` clean.

- [ ] **Step 1.** Write `predictor.go` with the interface, type, and `LabelSetKey` helper.
- [ ] **Step 2.** Write `predictor_test.go` covering `LabelSetKey` (table tests: empty input, single label, duplicate labels, order independence, case-sensitivity preserved).
- [ ] **Step 3.** `go test ./internal/predictor/...` GREEN. `make test` GREEN.
- [ ] **Step 4.** Do not commit yet — Task 3 finishes the parser layer and we commit `internal/predictor/` together once the unit-test layer is green.

---

## Task 3: Workflow parser (`internal/predictor/workflow/`)

**Files:**
- Create: `internal/predictor/workflow/parse.go`
- Create: `internal/predictor/workflow/parse_test.go`
- Create: `internal/predictor/testdata/{simple,matrix,reusable_local,reusable_remote,dynamic,deep_chain}.yml`

**Acceptance criteria:**

The package exposes a typed view of one parsed workflow. Recommended shape (final names settled during implementation):

```go
type Workflow struct {
    Jobs map[string]Job   // keyed by job ID
}

type Job struct {
    ID       string
    Needs    []string                // job IDs from `needs:`
    RunsOn   RunsOnSpec
    Matrix   MatrixSpec              // empty when none
    UsesLocal string                 // empty unless `uses: ./.github/workflows/x.yml`
    UsesRemote bool                  // true for owner/repo/...@ref refs
    Dynamic  bool                    // true when runs-on or matrix uses ${{ }} or fromJSON
}

type RunsOnSpec struct {
    Labels []string                  // populated for literal bare/array/group forms
    Group  string                    // optional, for object form
    Dynamic bool                     // true for ${{ }} expression form
}

type MatrixSpec struct {
    Combos []map[string]string       // expanded combinations from literal matrix
    Dynamic bool                     // true for fromJSON / expression-driven matrices
}

// Parse parses one workflow YAML; resolves runs-on, expands literal matrices, and
// records dynamic forms without erroring.
func Parse(raw []byte) (Workflow, error)
```

Behaviors:
- Bare string `runs-on: ubuntu-latest` → `RunsOnSpec{Labels: ["ubuntu-latest"]}`.
- Array `runs-on: [self-hosted, linux, x64]` → `RunsOnSpec{Labels: [...]}` (preserving order).
- Object form `runs-on: { group: x, labels: [a, b] }` → `RunsOnSpec{Group: "x", Labels: ["a","b"]}`.
- Matrix reference `runs-on: ${{ matrix.os }}` with `matrix.os: [a, b]` → one job entry per matrix combo with concrete `Labels` substituted; respect `include`/`exclude`.
- Any other `${{ }}` form in `runs-on` → `RunsOnSpec{Dynamic: true}`.
- Matrix referencing `fromJSON(...)` or any expression → `MatrixSpec{Dynamic: true}` and downstream `RunsOn.Dynamic = true`.
- `jobs.<id>.uses: ./.github/workflows/x.yml` → `UsesLocal` set, no own `runs-on`.
- `jobs.<id>.uses: owner/repo/.github/workflows/x.yml@ref` → `UsesRemote: true`.
- `needs:` accepts a string or string array; normalize to `[]string`.

Edge cases the parser MUST handle without crashing:
- The YAML `on:` key parsed as boolean `true` (the Norway problem) — both keys must be looked up.
- Anchors and merge keys.
- Workflow file missing `jobs:` — return an empty `Workflow{}` and no error.
- Multi-document YAML — reject with a clear error.

- [ ] **Step 1.** Write a failing test that loads `testdata/simple.yml` (one job, `runs-on: ubuntu-latest`, no needs) and asserts the parsed `Workflow` shape. RED.
- [ ] **Step 2.** Implement the simplest `Parse` using `actionlint.Parse([]byte) (*actionlint.Workflow, []*actionlint.Error)`. Map the typed AST into the package-local `Workflow` shape. GREEN.
- [ ] **Step 3.** Add fixtures and tests one behavior at a time (array form, group object, literal matrix expansion incl. include/exclude, dynamic runs-on, dynamic matrix, local uses, remote uses, deep `needs:` chain, Norway-`on:` key). Each test starts RED, then implement until GREEN.
- [ ] **Step 4.** `go test ./internal/predictor/workflow/... -v -race` GREEN. `make test` GREEN.
- [ ] **Step 5.** Commit the parser + interface + helper together: `feat(predictor): workflow parser and Predictor interface`. Files staged: `internal/predictor/predictor.go`, `internal/predictor/predictor_test.go`, `internal/predictor/workflow/...`, `go.mod`/`go.sum` if any indirect deps shifted.

---

## Task 4: ETag-cached YAML fetcher

**Files:**
- Create: `internal/predictor/cache.go`
- Create: `internal/predictor/cache_test.go`

**Acceptance criteria:**
- Exposes `WorkflowFetcher` with `Fetch(ctx, owner, repo, path, ref string) ([]byte, error)`.
- Caches the response body per `(owner, repo, path, ref)`. On a cache hit, sends `If-None-Match: <etag>` and on `304` returns the cached body without re-parsing.
- 200 responses update the cached etag and body.
- 404 responses are reported as a typed `ErrNotFound`-style error so the caller can degrade gracefully when a workflow path no longer exists at a given `head_sha`.
- Network errors and 5xx responses retry with the existing backoff pattern used by `internal/demand/github_poller.go` (extract the helper into a shared package only if it stays small — otherwise duplicate cleanly; do not bloat the demand package).
- Bounded by a configurable max entry count (default 256) using a simple LRU; eviction is by least-recently-used. No on-disk persistence in v0.2.0.
- HTTP requests carry `User-Agent: warmrunners/<version>` consistent with the v0.1.1 poller behavior.
- Concurrency-safe — internal mutex guards the cache.

- [ ] **Step 1.** Write `cache_test.go` using `httptest.Server`. RED:
  - First fetch returns 200 + ETag `"abc"` + body. Cache stores it; second fetch sends `If-None-Match: "abc"`, server returns 304, cache returns the original body.
  - Cache hit for the same key reuses cached body (no second request).
  - Different keys (different `path` or `ref`) are independent.
  - Eviction at capacity: insert 257 entries, verify the LRU is gone.
  - 404 returns `ErrNotFound`.
  - 5xx retries up to N times (use a tiny base delay seam, mirror the poller's `recordingTimer` pattern from v0.1.1).
- [ ] **Step 2.** Implement `cache.go`. GREEN for each.
- [ ] **Step 3.** Decide on the backoff helper: if `internal/demand/github_poller.go`'s `doWithRetry` is easily extracted into `internal/predictor/` shared use, do it as a small refactor. Otherwise duplicate with a comment pointing at the original; we are not building an HTTP framework.
- [ ] **Step 4.** `go test ./internal/predictor/... -v -race` GREEN. `make test` GREEN.
- [ ] **Step 5.** No commit yet — Task 5 finishes the predictor and we commit the full `internal/predictor/` body together.

---

## Task 5: `WorkflowNeedsGraph` predictor

**Files:**
- Create: `internal/predictor/workflow_needs_graph.go`
- Create: `internal/predictor/workflow_needs_graph_test.go`

**Acceptance criteria:**

`WorkflowNeedsGraph` implements `Predictor`. Its constructor accepts: a GitHub REST client (the same `http.Client` shape the v0.1.1 poller uses, including the configurable `--github-http-timeout`), the cache from Task 4, and a `RunsCap int` (defaults to 50, comes from `spec.predictor.maxRunsPerPoll`).

`Predict(ctx, owner, repo, token)` performs:

1. List active runs via `GET /repos/{o}/{r}/actions/runs?status=in_progress` + `status=queued`, plus `pending`, `requested`, `waiting`. ETag-cached. Take the first `RunsCap` after de-dup by ID.
2. For each active run, list its currently-materialized jobs (`GET /actions/runs/{run_id}/jobs?filter=latest`), ETag-cached. Note: the API does not return jobs whose `needs:` are unresolved — that's the v0.2.0 premise.
3. Fetch the workflow YAML at `(run.path, run.head_sha)` via Task 4 cache. On 404, drop the run from this poll's contribution.
4. Parse the YAML via Task 3.
5. Build the materialized-job-name set from step 2. Walk the parsed `Workflow.Jobs`:
   - Skip already-materialized jobs.
   - A job is **imminent** if every entry in its `Needs` is either materialized or completed. Treat completion as: present in the materialized set with a non-pending status. For v0.2.0 simplicity, treat *any* materialized job as a satisfied need — the API's job list only contains materialized jobs, so this is conservative-safe.
   - Skip jobs whose `RunsOn.Dynamic` is true, or whose enclosing `Matrix.Dynamic` is true: increment a `dynamic_skipped` counter for visibility, do not contribute to the prediction.
   - For local `UsesLocal`: load the referenced workflow at the same `head_sha`, recurse with depth bound 10 and a cycle-detection set; treat its jobs as if they were the calling job's expansion (their `runs-on` and matrix combos contribute).
   - For remote `UsesRemote`: increment `dynamic_skipped`, do not contribute.
   - For literal-matrix jobs: emit one contribution per combo using the substituted `RunsOnSpec.Labels`.
   - For non-matrix jobs: emit one contribution with `RunsOnSpec.Labels`.
6. Aggregate contributions into `Prediction.PerLabelSet` keyed by `LabelSetKey(labels)` (Task 2 helper).

Error model:
- Network/HTTP errors on the runs/jobs list calls → return `(Prediction{}, err)`. The reconciler logs + marks the condition `PredictorAvailable=False` and proceeds with the schedule + reactive layers.
- YAML 404 / parse error on a single run → drop only that run; continue with the rest; the overall call still returns a non-nil `Prediction`.

- [ ] **Step 1.** Write `workflow_needs_graph_test.go` against `httptest.Server`. RED for each scenario:
  - Single active run, stage1 in_progress, stage2 `needs: stage1` with runs-on `[self-hosted, gpu]` → predictor returns `PerLabelSet["gpu,self-hosted"] = 1`.
  - Stage1 completes → next poll returns `PerLabelSet = {}` for that run.
  - Two active runs in different workflows → contributions sum.
  - Matrix-expanded job (`os: [a, b, c]`) → contributes 3.
  - Dynamic `runs-on` → 0 contribution + a `dynamic_skipped` metric tick.
  - Local `uses:` recursion → called workflow's downstream jobs contribute.
  - Remote `uses:` → 0 contribution + `dynamic_skipped` tick.
  - 404 on YAML for one run → that run drops, others contribute.
  - Network error on jobs endpoint → `Predict` returns `(Prediction{}, err)`.
  - `RunsCap = 1` with 5 active runs → only 1 run is scanned per call.
- [ ] **Step 2.** Implement. GREEN.
- [ ] **Step 3.** `go test ./internal/predictor/... -v -race` GREEN. `make test` GREEN. Demand-package tests still pass (no shared-state regressions if you refactored the retry helper).
- [ ] **Step 4.** Commit the package together: `feat(predictor): WorkflowNeedsGraph predictor with ETag-cached YAML fetch`.

---

## Task 6: CRD additions (`api/v1alpha1/`)

**Files:**
- Modify: `api/v1alpha1/warmrunnerpolicy_types.go`
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml`
- Update: `api/v1alpha1/testdata/golden_crd.yaml`
- Modify: `examples/policy-arc.yaml` (add a `predictor:` block as a documentation example; do not change examples/policy-garm.yaml's shape unless necessary)

**Acceptance criteria:**

New types in `warmrunnerpolicy_types.go`:

```go
// PredictorConfig configures the codebase-aware Predictor (v0.2.0).
type PredictorConfig struct {
    // +kubebuilder:default=true
    // +optional
    Enabled bool `json:"enabled,omitempty"`

    // +kubebuilder:default="5m"
    // +optional
    WorkflowRefreshInterval metav1.Duration `json:"workflowRefreshInterval,omitempty"`

    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=500
    // +kubebuilder:default=50
    // +optional
    MaxRunsPerPoll int32 `json:"maxRunsPerPoll,omitempty"`
}

// PredictedLabelSet is one entry in Status.PredictedLabelSets.
type PredictedLabelSet struct {
    Labels []string `json:"labels"`
    Count  int32    `json:"count"`
}
```

Additions to `WarmRunnerPolicySpec`:

```go
// +optional
Predictor *PredictorConfig `json:"predictor,omitempty"`
```

Additions to `WarmRunnerPolicyStatus`:

```go
// +optional
PredictedFloor int32 `json:"predictedFloor,omitempty"`

// +listType=atomic
// +optional
PredictedLabelSets []PredictedLabelSet `json:"predictedLabelSets,omitempty"`
```

New printer column under the existing columns:

```go
// +kubebuilder:printcolumn:name="Predicted",type=integer,JSONPath=`.status.predictedFloor`
```

`PredictorAvailable` becomes the third member in the existing condition vocabulary (alongside `DemandSourceAvailable` and `AdapterAvailable`).

- [ ] **Step 1.** Edit `warmrunnerpolicy_types.go`. Add the two new types + the spec/status fields + the printer column marker.
- [ ] **Step 2.** Regenerate: `make manifests generate`. Inspect the diff: deepcopy adds copy funcs for the new types; CRD YAML gains the new fields with `default: true`, `default: 5m`, `default: 50`, `Minimum: 1`, `Maximum: 500`, and the `Predicted` printer column.
- [ ] **Step 3.** Update `api/v1alpha1/testdata/golden_crd.yaml` to the new generated CRD.
- [ ] **Step 4.** Run the existing `TestGoldenCRD`. GREEN.
- [ ] **Step 5.** Run the existing `crd_validation_test.go` envtest. Add new cases: applying a CR with `predictor.maxRunsPerPoll: 0` is rejected; applying with `601` is rejected; applying with no `predictor:` block defaults to `Enabled=true, WorkflowRefreshInterval=5m, MaxRunsPerPoll=50` (assert via re-fetch).
- [ ] **Step 6.** Update `examples/policy-arc.yaml` with a commented `predictor:` block showing the defaults.
- [ ] **Step 7.** `make test` GREEN. `make lint` clean.
- [ ] **Step 8.** Commit: `feat(crd): predictor config and predicted status fields`.

---

## Task 7: Reconciler integration

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller.go`
- Modify: `internal/controller/metrics.go`
- Modify: `cmd/main.go` (instantiate `WorkflowNeedsGraph`, inject into the reconciler)
- Add tests: extend `internal/controller/reconcile_test.go` with predictor scenarios; or a new file `internal/controller/predictor_integration_test.go` (whichever fits the existing pattern — read first, follow the existing style).

**Acceptance criteria:**

Reconciler changes:
- New field on `WarmRunnerPolicyReconciler`: `Predictor predictor.Predictor` (interface, so tests can inject a stub).
- In `Reconcile`, after the existing schedule + reactive demand computation:
  - If `spec.Predictor == nil || spec.Predictor.Enabled`, call `r.Predictor.Predict(ctx, owner, repo, token)` where `token` is the per-policy GitHub credential read from the same Secret the demand source uses.
  - Compute `predictedContrib = Σ Prediction.PerLabelSet[L]` where `L ⊇ policy.github.labels` (the matching-direction rule from spec §3.4: predicted job labels superset-contain the policy filter, mirroring v0.1.0's `labelsMatch(have=job.Labels, want=policy.labels)`).
  - `desiredFloor = clamp(max(scheduledFloor, predictedContrib), floor.min, min(floor.max, backendMax))`.
  - On predictor error: log at `V(0)`, set the `PredictorAvailable=False` condition with `reason` and `message`, and continue with `predictedContrib = 0` (v0.1.x behavior preserved).
  - On success: set `PredictorAvailable=True`.
  - Populate `status.PredictedFloor` with the policy's `predictedContrib`.
  - Populate `status.PredictedLabelSets` with the top-N entries by count (default `N = 8`); if zero matches, leave nil.

Metrics changes (`metrics.go`):
- New `prometheus.NewGaugeVec` `warmrunners_predicted_floor{policy}`.
- New `prometheus.NewGaugeVec` `warmrunners_predicted_jobs_total{policy,labels}`.
- New `prometheus.NewCounterVec` `warmrunners_workflow_yaml_fetch_total{policy,result}` with `result ∈ {cached_304, fetched, error, dynamic_skipped}`.
- All registered via the existing `controller-runtime/pkg/metrics` registry, mirroring v0.1.1's `init()` pattern.

Wiring (`cmd/main.go`):
- Construct an `httpClient := &http.Client{Timeout: githubHTTPTimeout}` (the same flag the v0.1.1 poller uses).
- Construct `cache := predictor.NewWorkflowFetcher(httpClient, version.Version)`.
- Construct `pred := predictor.NewWorkflowNeedsGraph(httpClient, cache, /* RunsCap from CRD or a global default */)`.
- Inject `pred` into the reconciler. The token still comes from the per-policy Secret at reconcile time, not at construction — match the existing pattern in `warmrunnerpolicy_controller.go`.

- [ ] **Step 1.** Stub predictor in tests: a struct returning a canned `Prediction`. Use it in new reconciler tests for: (a) predicted floor folded via `max(...)`, (b) capped at `floor.max`, (c) decrease cooldown still applies, (d) `PredictorAvailable` condition transitions on Predict error, (e) status fields `PredictedFloor` + `PredictedLabelSets` populated.
- [ ] **Step 2.** Implement the reconciler changes. RED → GREEN.
- [ ] **Step 3.** Implement the three new metrics. Add tests in `metrics_test.go` for registration + label cardinality + the `dynamic_skipped` counter incrementing when the stub predictor reports a skipped job (Task 5 already has the underlying behavior; this verifies the wire).
- [ ] **Step 4.** Wire `cmd/main.go`. Run `go run ./cmd --help` and confirm no flag regressions.
- [ ] **Step 5.** `make test` GREEN with `-race`. `make lint` clean.
- [ ] **Step 6.** Commit: `feat(controller): consume Predictor; add predicted-floor metrics and status`.

---

## Task 8: Manual kind verification

Same shape as the v0.1.1 manual run. The e2e Ginkgo suite covers boot/metrics; the v0.2.0-specific behavior needs a manual check.

- [ ] **Step 1.** `kind create cluster --name wrp-v0.2.0`; `make install` (CRDs).
- [ ] **Step 2.** Apply a `WarmRunnerPolicy` against a small test repo that has a 2-stage workflow (lint → gpu-test). Use `sarataha/warmrunners-livetest` (the existing fixture).
- [ ] **Step 3.** Trigger a run on the livetest repo. Watch `kubectl get wrp -o jsonpath='{.items[*].status.predictedFloor}'` rise while stage 1 is `in_progress` and the API hasn't yet materialized stage 2.
- [ ] **Step 4.** Watch the backend's floor field (ARC `AutoscalingRunnerSet.spec.minRunners` via `kubectl get` on a stubbed CRD on kind; we are not running ARC end-to-end here — that's e2e's job — just confirming the operator's intent reaches the patch).
- [ ] **Step 5.** Confirm cooldown drains the predicted contribution after stage 2 queues (reactive layer takes over).
- [ ] **Step 6.** Tear down: `kind delete cluster --name wrp-v0.2.0`; revert `config/manager/kustomization.yaml` if `make install` mutated it.

---

## Task 9: Docs (split — ride with each PR)

Docs ship **alongside the code** they describe, not as a separate trailing PR. The GitHub
release page auto-lists merged PRs between tags; one PR per slice yields one readable entry,
and docs that describe behavior should land in the same PR that introduces the behavior.

Per-PR doc allocation:

- **PR 1 — predictor package.** No user-visible behavior yet (package not wired). Doc
  changes in this PR: only an internal-only addition to `internal/predictor/README.md`
  (optional, one paragraph on the package's responsibility) plus the GoDoc on the exported
  types. **Do not** add a README "How it works" paragraph yet — nothing for the user to
  switch on. **Do not** open the `## [0.2.0]` CHANGELOG section yet either; CHANGELOG
  entries arrive in PR 2 when behavior actually lands.
- **PR 2 — wire-up.** This PR ships user-visible behavior. Doc changes:
  - `README.md`: a paragraph after "How it works" explaining the codebase-aware predictor
    in one sentence; a small YAML snippet showing the optional `predictor:` block in
    `WarmRunnerPolicy.spec`.
  - `examples/policy-arc.yaml`: add a commented `predictor:` block illustrating the
    defaults (already listed in Task 6 Step 6 — confirm here).
  - `CHANGELOG.md`: open `## [0.2.0]` with **Added** entries for the Predictor component,
    new CRD fields, new metrics, `PredictorAvailable` condition, `actionlint` dependency,
    no behavior change when `predictor.enabled: false`. **Do not** mark a release date yet;
    leave the heading as `## [0.2.0]` and add the date when the tag is cut.
  - `~/gh/ideas/warmrunners-next-steps.md`: state-line bump pending — done at tag time.
- **Hot-fix PRs (if any).** Each surfaces an entry under `## [0.2.0]` in `CHANGELOG.md`
  in the appropriate section (**Fixed** / **Changed**). Bug-fix PRs do not need
  README updates unless they change documented behavior.

This shape keeps the release-page entries readable (each PR appears with its own title)
and avoids a final "docs:" PR that adds no shippable value.

---

## Task 10: Rollout

Multiple PRs into `main`, sequential merges (PR 2 depends on PR 1's package). Each PR is
its own short-lived branch; tag the release after the last PR merges. Bug-fix PRs are
welcome between the planned ones — the model is "at least N PRs," not "exactly N."

### PR 1 — `feat(predictor): new package` (Tasks 1–5)

- [ ] **Step 1.** Branch: `git checkout -b feat/predictor-package` off latest `main`.
- [ ] **Step 2.** Execute Tasks 1–5 on this branch with TDD per task. Parallelism inside
      this PR is allowed where files don't collide — Tasks 2 (interface), 3 (parser),
      4 (cache) touch disjoint files and can be implemented by independent subagents
      per `superpowers:dispatching-parallel-agents`; Task 5 (predictor) is sequential
      after, since it glues the three.
- [ ] **Step 3.** GoDoc + the optional `internal/predictor/README.md` paragraph from
      Task 9.
- [ ] **Step 4.** Push, open PR to `main`. Title: `feat(predictor): codebase-aware
      Predictor package`. Body: one-line summary + `See spec for design.`.
- [ ] **Step 5.** Wait for CI green; squash-merge keeping the same title.
- [ ] **Step 6.** Sync local `main`; delete the merged branch locally and on origin.

### PR 2 — `feat: wire codebase-aware predictor` (Tasks 6–7 + PR-2 doc allocation)

- [ ] **Step 1.** Branch: `git checkout -b feat/wire-predictor` off latest `main` (which
      now contains PR 1's package).
- [ ] **Step 2.** Execute Tasks 6 and 7 sequentially (Task 6 first — Task 7 references
      the new CRD field). Both tasks have their own TDD cycles.
- [ ] **Step 3.** PR-2 doc allocation (Task 9 bullet list): README, examples, CHANGELOG
      `## [0.2.0]` with current entries, no date yet.
- [ ] **Step 4.** Run Task 8 (manual kind verification) against this branch. Confirm
      the predicted floor rises during stage 1 and drains via cooldown after stage 2.
- [ ] **Step 5.** Push, open PR to `main`. Title: `feat: wire codebase-aware predictor
      into the reconciler`. Body: one-line summary + `See CHANGELOG [0.2.0] (unreleased).`.
- [ ] **Step 6.** Wait for CI green; squash-merge.
- [ ] **Step 7.** Sync local `main`; delete the merged branch.

### Hot-fix PRs (optional — any number)

For issues found during manual verification or post-merge but pre-tag: open a small PR
per fix, follow Conventional Commits (`fix(scope): …`, `ci: …`, `docs: …`), add a
`CHANGELOG.md` entry under `## [0.2.0]` in the right section, squash-merge.

### Tag and release

- [ ] **Step 1.** When `main` carries all intended PRs for v0.2.0: set the date on the
      `## [0.2.0]` heading in `CHANGELOG.md`, update `[Unreleased]` / `[0.2.0]`
      diff-URL link refs, and update `~/gh/ideas/warmrunners-next-steps.md` to "v0.2.0
      shipped." Open this as a tiny PR (`docs: finalize 0.2.0 changelog and handoff`)
      and merge — or, if the only remaining edits are these dated lines, commit straight
      to `main` (doc-only, same precedent as the v0.1.1 doc commit).
- [ ] **Step 2.** Tag: `git tag -s v0.2.0 -m "v0.2.0"`; `git push origin v0.2.0`.
      Release workflow runs cosign + SBOM.
- [ ] **Step 3.** Verify release artifacts the same way v0.1.1 was verified:
      `cosign verify` against the image and chart, `cosign verify-attestation` against
      the SBOM, `helm install` on a fresh kind from the new chart.
- [ ] **Step 4.** Spot-check the auto-generated release page: it should list both PR 1
      and PR 2 (plus any hot-fix PRs) under "What's Changed", each with the contributor
      link, and the "Full Changelog" link.

---

## Self-review notes

- **Spec coverage.** §1 identity → conceptual, no task. §2 signal → Task 5. §3.1 components → Tasks 2–5. §3.2 interface → Task 2. §3.3 algorithm → Tasks 3, 5. §3.4 reconciler → Task 7. §3.5 CRD → Task 6. §3.6 metrics → Task 7. §4 quota → covered by cache (Task 4) + `MaxRunsPerPoll` (Task 6). §5 error handling → Task 7 condition + Task 5 per-run drop. §6 testing → distributed across Tasks 3–7. §7 observability → Task 7 metrics. §8 compatibility → Task 6 defaults preserving v0.1.x. §9 layout → Task 1+2+3+4+5. §10 roadmap → no code; already amended in the design spec. §11 open questions → resolved during planning here: `PredictedLabelSets` top-N is 8 (Task 7); cache is in-memory LRU 256 (Task 4); "every match" attribution stays default (no exclusive flag in v0.2.0).
- **Placeholder scan.** No TBD / TODO. Every step gives the file path + behavior. Go bodies described, not pasted (per the project's middle-ground rule: include verified-mechanical pieces like markers, YAML, CLI flags, struct field names; skip controller logic and test bodies).
- **Type consistency.** `Predictor`, `Prediction.PerLabelSet`, `WorkflowNeedsGraph`, `WorkflowFetcher`, `PredictorConfig`, `PredictedLabelSet`, `PredictorAvailable` — all match the spec verbatim and appear consistently across tasks.
- **PR count.** At least 2 sequential PRs (predictor package; wire-up). Each is its own
  squash-merge into `main`, contributing one entry to the v0.2.0 release-page's
  auto-generated "What's Changed" list. Hot-fix PRs are welcome between them. Docs ride
  with each PR per Task 9. The final tag step is a tiny `docs:` commit straight to
  `main` (or its own micro-PR if the date-finalization edits ended up larger than
  expected). Within each PR, internal commits stay component-grouped per
  `CLAUDE.local.md`; the squash-merge collapses them to one main-line commit.
