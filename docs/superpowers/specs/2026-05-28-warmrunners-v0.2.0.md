# warmrunners — v0.2.0 Codebase-aware Predictor

**Status:** Approved (2026-05-28) — ready for implementation planning
**Author:** Sara
**Repo:** github.com/sarataha/warmrunners
**Predecessors:**
[v0.1.0 design](./2026-05-27-warmrunners-design.md) ·
[v0.1.1 polish](./2026-05-27-warmrunners-v0.1.1-polish.md)

## 1. Identity

v0.2.0 adds a **codebase-aware `Predictor`** that reads the `needs:` dependency graph of
active workflow runs and pre-warms downstream runners while their upstream jobs are still
running. It is the first feature that meaningfully differentiates warmrunners from a cron
plus `kubectl patch`.

The wedge: GitHub does not surface a downstream job until its `needs:` upstream completes.
Every existing GitHub Actions autoscaler (ARC, KEDA, the Feb 2026 scale-set client, and
every commercial provider) reacts to `workflow_job: queued`, which by GitHub's design
fires *after* `needs:` resolve. By then the downstream runner type — typically the slow,
expensive one like GPU — must be cold-started while the user waits. warmrunners is the
only autoscaler that anticipates the downstream demand by **statically parsing the workflow
YAML at the run's `head_sha`**, so a GPU pool is hot the moment the GPU job queues.

### Non-goals

- Not a path-based or "changed-files" trigger. That signal is too noisy (draft PRs, stale
  PRs, no main-branch coverage) and produces no extra lead time over reactive scaling for
  jobs without `needs:`. Activity-volume scaling is v0.3.0; it is additive, not in scope here.
- No webhook receiver. The current `DemandSource` polling model carries v0.2.0 unchanged;
  a webhook is a later alternative implementation behind the same interface.
- No ML, no LSTM, no Vertex AI. The predictor is deterministic YAML parsing plus graph
  arithmetic. The roadmap's forecasting item is a rolling quantile histogram, also no ML.
- No new CRD field changes existing v0.1.0 semantics. The new fields under §3.5 are
  optional tuning knobs with defaults that match v0.1.x behavior when the predictor is off
  (`predictor.enabled: false`) and reasonable defaults when it is on; existing policies
  validate and reconcile unchanged.

## 2. The signal we use

For each active workflow run we already see via the GitHub REST API, the predictor reads
the workflow YAML at the run's `head_sha`, walks the `needs:` DAG, and identifies jobs that
**have not been materialized by GitHub yet** because their upstream is still in flight.
Those jobs' `runs-on` labels are the predicted demand.

This is the only practical lead time we get from GitHub. Three facts settle it:

1. **No API field exposes `needs:`-blocked jobs.** The documented `workflow_job` status
   enum (`queued`, `in_progress`, `completed`, `waiting`, `requested`, `pending`) has no
   value for "blocked on intra-run dependency". `waiting` is for environment / deployment
   gates, not for `needs:`. A job with unresolved `needs:` simply does not appear in
   `GET /repos/{o}/{r}/actions/runs/{run_id}/jobs` until its upstream completes — at which
   point it is already `queued` and reactive scaling has taken over. The API alone is
   sufficient only for reactive warming, not predictive warming.
2. **The lead time is upstream's duration.** Stage 1 (lint, test) typically takes minutes;
   stage 2 (GPU, build, integration tests) typically needs minutes to cold-start a runner.
   The two windows line up — that's the only place predictive warming pays off.
3. **PR open / changed-files signals add noise without adding lead time.** Most jobs
   queue within seconds of a PR push; reactive scaling already handles them. The jobs that
   *don't* queue immediately are exactly the ones waiting on `needs:`. So the `needs:`
   graph is the entire useful signal.

## 3. Architecture

### 3.1 Components

```
+--------------------+              +---------------------+
| WarmRunnerPolicy   |  <-------->  |  Reconciler         |
| (CRD, user-owned)  |              |  (every poll)       |
+--------------------+              +-----------+---------+
                                                |
              +--------------+------------------+------------------+
              |              |                                     |
        +-----v-----+  +-----v-----+   +---------------+   +-------v-----+
        |DemandSource| |Predictor  |   |  Scheduler    |   |   Adapter   |
        | (REST poll | |(workflow_ |   |(clock + queue |   |  (ARC/GARM) |
        |  queued+   | |runs +     |   | + predicted   |   |             |
        |  running)  | | needs-DAG)|   | -> max-of)    |   |             |
        +-----------+  +-----------+   +-------+-------+   +------+------+
                                               |                  |
                                               +----- desiredFloor (per policy) ----+
                                                                  |
                                                       +----------v---------+
                                                       | Backend CR (3rd    |
                                                       | party) — floor     |
                                                       | field patched      |
                                                       +--------------------+
```

`Predictor` is a new pluggable component. The v0.2.0 implementation is `WorkflowNeedsGraph`.
The reconciler treats `DemandSource` and `Predictor` outputs as two contributions to the
same per-policy floor candidate, taken via `max(...)` and clamped to the policy's
`floor.min`/`floor.max` and the backend's `GetMax()`.

### 3.2 Predictor interface

```go
package predictor

type Prediction struct {
    // Map of label set (sorted, joined for hashability) -> number of imminent jobs.
    PerLabelSet map[string]int
}

type Predictor interface {
    // Predict returns the imminent per-label-set demand inferred from active workflow
    // runs and their parsed needs: DAGs. Pure read-only; idempotent within a poll
    // window; caller decides cadence.
    Predict(ctx context.Context, owner, repository string) (Prediction, error)
}
```

The v0.2.0 implementation is `WorkflowNeedsGraph` which uses the GitHub REST API plus
in-memory ETag caching of workflow YAML.

### 3.3 The prediction algorithm

For each reconcile of a `WarmRunnerPolicy`:

1. **List active workflow runs.** `GET /repos/{o}/{r}/actions/runs?status=in_progress`
   (then `status=queued`, `pending`, `requested`, `waiting`) with `If-None-Match` set
   from the last poll's ETag. A `304` returns the cached run set with no quota cost.
2. **For each active run**, list its currently-materialized jobs via
   `GET /repos/{o}/{r}/actions/runs/{run_id}/jobs?filter=latest` with ETag. The result
   is the set of jobs GitHub has materialized so far — *not* the full workflow.
3. **Fetch the workflow YAML at the run's `head_sha`**:
   `GET /repos/{o}/{r}/contents/{workflow_run.path}?ref={head_sha}` with
   `Accept: application/vnd.github.raw` and ETag conditional. Cached in-memory keyed by
   `(repo, path, head_sha)` — the same `head_sha` is shared by all jobs of a run, so a
   single fetch covers many polls.
4. **Parse the YAML** via `github.com/rhysd/actionlint` to get the typed AST: each job's
   `runs-on` labels (or expression form), its `needs:` list, and `strategy.matrix` literal
   values.
5. **Walk the DAG.** Mark every job currently visible in the API response by its `name`
   as "materialized" (status irrelevant — once it exists, GitHub or the reactive layer
   owns it). Every other job in the YAML is "imminent" if every entry in its
   `needs:` list is either also materialized or completed. For each such imminent job:
   - Resolve `runs-on` to a literal label set. Bare strings, arrays, and literal-matrix
     expansion (`runs-on: ${{ matrix.os }}` with `matrix.os: [ubuntu-latest, self-hosted]`)
     are decidable; expression-driven (`${{ inputs.x }}`, `${{ fromJSON(...) }}`), remote
     `uses:` reusable workflows, and matrices with `fromJSON` are recorded as a
     `Dynamic` outcome and the job is **excluded** from the prediction (logged at `V(2)`,
     surfaced as a `PredictionPartial` condition reason).
   - Expand literal matrices (`include`, `exclude`) to count combinations.
   - Add the resulting count to `PerLabelSet[labels]`.
6. **Reusable workflows.** Local refs (`uses: ./.github/workflows/x.yml`) are followed,
   loading the called workflow at the same `head_sha`, recursing to a depth-10 internal
   ceiling with cycle detection. (GitHub itself enforces a per-call nesting limit on
   reusable workflows; 10 is a safe upper bound for our static walk regardless of GitHub's
   current cap.) Remote refs
   (`owner/repo/.github/workflows/x.yml@ref`) are recorded as `Dynamic` for v0.2.0 and
   excluded; v0.3.x can revisit.
7. **Composite actions.** Ignored — they execute on the calling job's already-selected
   runner and never carry their own `runs-on`.

### 3.4 Reconciler integration

`scheduler.Heuristic.Decide` is left **unchanged**; the predictor's contribution is
folded in at the reconciler level, not inside `Decide`. This keeps the existing
schedule + queue-headroom logic pure and matches the v0.1.0 spec's "each reconcile
changes at most one field on one CR" property — we still compute one floor per policy
per reconcile, we just take the `max` over one more candidate before clamping.

Per policy, the reconciler computes:

```
scheduledFloor    = Heuristic.Decide(spec, now, demand, ...).DesiredFloor   // v0.1.0
predictedContrib  = Σ Prediction.PerLabelSet[L]  where  L ⊇ policy.github.labels
desiredFloor      = clamp(
                      max(scheduledFloor, predictedContrib),
                      floor.min,
                      min(floor.max, backendMax))
```

**Matching direction.** A predicted job's `runs-on` label set `L` attributes to a
policy iff `L ⊇ policy.github.labels` — the predicted job's labels are a superset of
the policy's declared labels. This is the **same direction** as the existing
`labelsMatch(have=job.Labels, want=policy.labels)` rule in `internal/demand/github_poller.go`
(which counts a job toward a policy when `job.Labels ⊇ policy.labels`). So both signals
(reactive `DemandSource` and codebase-aware `Predictor`) apply consistent attribution:
the policy's `github.labels` declares a *filter*, and every job whose `runs-on` contains
that filter contributes — whether the job is currently queued (reactive) or imminent
(predicted).

If multiple policies match a given label set, **every** matching policy receives the
contribution — the user is responsible for partitioning their backends; warmrunners
does not pick one. The existing cooldown on decreases (v0.1.0) drains the predicted
floor naturally once upstreams complete and the prediction returns zero for that label
set.

Failure modes — all degrade gracefully to v0.1.0 behavior:
- Predictor returns an error → log + emit `PredictorAvailable=False` condition, ignore
  predicted floor for this reconcile, keep schedule + reactive.
- Workflow YAML unparseable at `head_sha` → that one run contributes `0`; rest proceed.
- Job's `runs-on` is dynamic / undecidable → that job is excluded; the run still
  contributes its decidable jobs.

### 3.5 CRD additions

Additive, optional, defaults preserve v0.1.0 semantics.

```yaml
spec:
  # ... existing fields unchanged ...
  predictor:
    enabled: true                        # default true; set false to disable predictor
    workflowRefreshInterval: 5m          # how often to refresh workflow YAML (ETag-cheap)
    maxRunsPerPoll: 50                   # cap on active workflow runs scanned per repo
status:
  # ... existing fields unchanged ...
  predictedFloor: 4                      # last predictor contribution for visibility
  predictedLabelSets:                    # debug aid; capped
    - { labels: [self-hosted, gpu], count: 3 }
    - { labels: [self-hosted, large], count: 1 }
  conditions:
    - type: PredictorAvailable           # mirrors DemandSourceAvailable / AdapterAvailable
      status: "True"
```

### 3.6 New metrics

- `warmrunners_predicted_floor{policy}` (gauge) — predictor's contribution to the floor.
- `warmrunners_predicted_jobs_total{policy,labels}` (gauge) — per-label-set imminent count.
- `warmrunners_workflow_yaml_fetch_total{policy,result}` (counter) — `result` ∈
  `cached_304`, `fetched`, `error`, `dynamic_skipped`.

The existing four v0.1.0 metrics and the v0.1.1 `build_info` / `reconciliation_errors_total`
counter stay unchanged.

## 4. Quota and performance

Per the GitHub API research, the cost model per repo per poll is `1` (list runs) `+ N`
(list jobs for each of `N` active runs) `+ M` (workflow YAML fetches at new `head_sha`s).
ETag conditional requests on all three endpoints return `304` for unchanged data, which
**does not count against the primary rate limit** as long as the request is authenticated.
Workflow YAML changes only when the user pushes; jobs lists change only on state
transition; the run list changes only when a run starts or ends. Steady-state 304-hit rate
is therefore high. **Order-of-magnitude estimate, not a guarantee:** for a moderately
active repo (~10 active runs, runs transitioning every few minutes, pushes a few times
per hour) at the default 30 s `pollInterval`, expect ~50–200 non-304 requests per hour
against the 5 000 req/hr authenticated budget. A burst-active repo (dozens of concurrent
runs, frequent pushes) can be an order of magnitude higher; quiet repos an order
of magnitude lower. `maxRunsPerPoll` caps the per-poll fan-out so a runaway repo cannot
exhaust the budget; raising `pollInterval` is the other knob.

The hard ceiling is the per-installation rate limit. A `WarmRunnerPolicy` whose token has
many repos sharing the same limit must throttle — `maxRunsPerPoll` and the existing
`pollInterval` give the operator the knobs. A future GitHub App credential type would
raise the ceiling to 15 000 req/hr on Enterprise Cloud; out of scope here.

## 5. Error handling and safety

Same posture as v0.1.0, extended to the new component:

- Predictor errors **never** raise the applied floor. They only fail to *raise* it; the
  schedule + reactive layers continue to compute their candidate, and `max(...)` is taken
  over whichever candidates are healthy. The result is at-worst v0.1.0 behavior on a
  fully-broken predictor.
- Predictor errors never lower the floor either — the existing cooldown rate-limits
  decreases regardless of which signal contributed.
- `floor.max` and the backend's `GetMax` are hard caps under any combination.
- The predictor is read-only against the user's repo. It never writes to GitHub.

## 6. Testing

- **Unit (`internal/predictor/...`):**
  - YAML parsing: bare/array/group `runs-on`, literal-matrix expansion, `needs:`
    DAG walk, local reusable-workflow follow with depth cap and cycle detection.
  - Dynamic-form handling: every `${{ }}`-form is recorded as `Dynamic` and excluded.
  - Edge cases: `if: failure()` downstream is treated as imminent (the conservative
    over-warm is bounded by `floor.max` and drained by cooldown — see §5).
- **Integration (`httptest.Server` + parsed YAML fixtures):**
  - Two-stage workflow, stage 1 in progress → predictor reports stage-2 labels.
  - Stage 1 completes → predictor reports zero for that run.
  - Run completes / cancelled → predictor drops the run from its set.
  - Reusable-workflow recursion against a stub repo serving multiple files at a
    `head_sha`.
  - ETag conditional returns `304` → no re-parse, no metric blip.
- **Envtest:** reconciler with a real CRD + a stub predictor → confirms the `max(...)`
  composition of `schedule + queueHeadroom` and `predictedContribution`, clamped to
  `floor.max`.
- **Local e2e:** the existing kind harness + a synthetic workflow with `lint` + `gpu-test`
  jobs against a stub GitHub server, observing the floor rise during `lint` and decay
  via cooldown after `gpu-test` is reactively absorbed.

## 7. Observability

The new metrics (§3.6) are sufficient to debug a misbehaving predictor: a low
304-hit ratio in `warmrunners_workflow_yaml_fetch_total{result=cached_304}` signals quota
pressure; a high `dynamic_skipped` count signals workflows that aren't statically
analyzable; `warmrunners_predicted_floor` plotted against `warmrunners_applied_floor`
shows where the predictor is contributing. The `PredictorAvailable` condition surfaces
hard failures to `kubectl describe`.

## 8. Compatibility and rollout

- **Existing v0.1.1 policies validate unchanged.** All new CRD fields are optional with
  safe defaults (`predictor.enabled: true`, sensible interval and cap). A policy that
  does not declare a `predictor` block gets the default-on behavior; one that sets
  `predictor.enabled: false` runs v0.1.x semantics exactly.
- **No change to the GitHub token scope** — the predictor uses the same `repo`-read
  permission the v0.1.x poller already requires (`contents: read` on the workflow file,
  `actions: read` on runs/jobs).
- **Backend CRs** — unchanged. The predictor never touches the backend; only the
  reconciler's existing adapter does, via the existing `SetFloor`.
- **Helm chart** — no template changes; the new flag isn't needed on the manager binary
  (predictor lives inside the reconciler), and existing flags from v0.1.1 are unaffected.

## 9. Repository layout

```
warmrunners/
  api/v1alpha1/                 # CRD types — adds Spec.Predictor + Status.Predicted*
  internal/
    controller/                 # reconciler — extended to consume Predictor
    demand/                     # unchanged
    scheduler/                  # extended to fold predicted into max-of-floors
    predictor/                  # NEW: Predictor interface + WorkflowNeedsGraph impl
      workflow/                 # NEW: actionlint-based YAML parsing helpers
      cache.go                  # NEW: ETag-cached YAML fetcher
    adapter/                    # unchanged
  ...
```

## 10. Roadmap (unchanged from v0.1.1 spec; restated for context)

- v0.1.0, v0.1.1 — shipped.
- **v0.2.0 — this spec.**
- v0.3.0 — activity-based volume multiplier.
- v0.4.0 — validating admission webhook + richer queueRule shapes.
- later — forecasting (rolling histogram, no ML), webhook DemandSource, Terraform adapter.
- v1.0.0 — when the CRD graduates to v1.

## 11. Open questions deferred to implementation

- Exact cap on `predictedLabelSets` in status — surface only the top-N by count to keep
  the object small; pick `N` during planning.
- Whether the workflow-YAML cache lives in memory only (simple) or also as a small
  on-disk LRU (survives pod restarts but adds a PVC requirement). Lean toward in-memory;
  cold-start cost is one re-fetch per active run.
- Exact mapping from a label set `L` to a policy when multiple policies superset-match.
  Default in §3.4 is "every match". An optional `spec.predictor.exclusive: true` flag
  for "only the most-specific match" can come later if users hit double-warming.
