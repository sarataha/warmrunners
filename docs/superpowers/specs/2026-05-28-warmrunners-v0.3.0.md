# warmrunners — v0.3.0 Activity-Driven Dynamic Floor

**Status:** Approved (2026-05-28) — ready for implementation planning
**Author:** Sara
**Repo:** github.com/sarataha/warmrunners
**Predecessors:**
[v0.1.0 design](./2026-05-27-warmrunners-design.md) ·
[v0.1.1 polish](./2026-05-27-warmrunners-v0.1.1-polish.md) ·
[v0.2.0 codebase-aware](./2026-05-28-warmrunners-v0.2.0.md)

## 1. Identity

v0.3.0 makes the warm-floor track real developer activity. While a repo has recent CI
activity, warmrunners keeps the pool warm and sized to match the actual fanout of the
workflows being triggered. While the repo is quiet, the floor drops to zero so no warm
capacity is paid for.

In the same release, the v0.1.x `queueRule.headroom` signal is **removed**. It was a
design bug: it added to the floor whenever queued jobs existed, but ARC and KEDA already
react to queued jobs in seconds — so warmrunners was double-counting, over-warming, and
fighting itself.

The combined effect: warmrunners now contributes lead time and magnitude where ARC
cannot — codebase-aware predictor (v0.2.x) for `needs:`-blocked downstream jobs, plus
the new activity-driven signal for "dev is iterating right now, keep the pool sized to
their workflows." Both compose with the existing schedule windows via `max(...)`.

### Non-goals

- **Not** a forecaster. Activity is observed in a rolling 15-min window, not predicted.
  Time-series forecasting stays on the "later" roadmap.
- **Not** a webhook receiver. The signal comes from REST polling
  (`GET /actions/runs?created=>…`), the same call the v0.2.x predictor already makes.
  No new infra burden on users.
- **Not** Events-API-based. GitHub's Events API has documented 30s–6h latency and
  explicitly "is not built to serve real-time use cases"
  ([REST events](https://docs.github.com/en/rest/activity/events)) — useless for
  a signal that needs to react inside an active dev session.
- **Not** predicting individual PR push behavior. Per-PR prediction is fundamentally
  unpredictable from observable signals; the activity-driven floor reacts to the
  aggregate pattern (recent CI runs of any kind from non-bot actors).

## 2. The signal

`GET /repos/{owner}/{repo}/actions/runs?created=>{windowStart}&per_page=100` with
`If-None-Match: <etag>` on every poll. A 304 response does not count against the primary
rate limit when the request is authenticated
([best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)).

Each `workflow_run` in the result carries `actor`, `triggering_actor`, `event`,
`head_sha`, and `path` (workflow YAML path). The activity layer needs all five:

- `actor` / `triggering_actor` — both are `Simple User` objects with the `type` field
  (`"User"` or `"Bot"`). Used for the bot filter.
- `event` — drops bot-triggered noise that slips past type detection
  (`schedule` runs are filtered as non-human).
- `head_sha` + `path` — fetch the workflow YAML at the exact ref via the existing
  `WorkflowFetcher` cache, parse via `actionlint`, extract matrix fanout — same code
  path the v0.2.x predictor already uses.

A run counts as "activity" when **all** of these are true:

1. `created_at >= now() - spec.activity.windowSeconds`.
2. The actor passes the bot filter (§3.5).
3. The `event` is one of: `push`, `pull_request`, `pull_request_target`,
   `pull_request_review_comment`, `workflow_dispatch`, `repository_dispatch`. Not
   `schedule` (cron noise) or `check_run`/`check_suite` (downstream chain).

## 3. Architecture

### 3.1 New component: `Activity`

```
+--------------------+              +---------------------+
| WarmRunnerPolicy   |  <-------->  |  Reconciler         |
| (CRD, user-owned)  |              |  (every poll)       |
+--------------------+              +-----------+---------+
                                                |
        +------------+--------+-----------------+------------------+
        |            |        |                                    |
  +-----v-----+ +----v---+ +--v----------+ +---------------+ +-----v-----+
  |DemandSource| |Predictor| | Activity   | |  Scheduler    | | Adapter   |
  |(REST poll  | |(needs:  | |(workflow_  | |(clock +       | |(ARC/GARM) |
  | queued+    | | graph)  | | runs +     | | max(...) of   | |           |
  | running)   | |         | | YAML       | | candidates)   | |           |
  +-----------+ +---------+ | parser)    | +-------+-------+ +------+----+
                            +------------+         |                |
                                                   +---- desiredFloor (per policy) ---+
                                                                    |
                                                       +------------v---------+
                                                       | Backend CR (3rd-party)|
                                                       | floor field patched   |
                                                       +-----------------------+
```

`Activity` is a new component with an interface mirroring `Predictor`:

```go
type Activity interface {
    // Sample returns the per-label-set fanout demand inferred from active and
    // recent workflow_runs. token is the per-policy GitHub credential. The map
    // key is workflow.LabelSetKey of the run's runs-on labels; the value is the
    // max-fanout count from that workflow's YAML at head_sha.
    Sample(ctx context.Context, owner, repository, token string,
           window time.Duration, denylist []string) (Sample, error)
}

type Sample struct {
    PerLabelSet map[string]int
}
```

Same shape as `predictor.Prediction` so the reconciler aggregates uniformly.

### 3.2 Reconciler integration

After the existing scheduler + predictor candidates, the reconciler adds a third
by calling `Sample` with the per-policy window and denylist:

```go
window := time.Duration(pol.Spec.Activity.WindowSeconds) * time.Second
denylist := append([]string(nil), activity.BuiltinDenylist...)
denylist = append(denylist, pol.Spec.Activity.BotLoginDenylist...)

sample, aerr := r.Activity.Sample(ctx, owner, repo, token, window, denylist)
```

A nil or `Enabled: false` `Spec.Activity` short-circuits the call exactly as the
predictor's enabled-check does (v0.2.x §3.2). The `token` is the same per-policy
GitHub credential the predictor already resolves; no second Secret lookup needed.

Floor composition:

```
scheduledFloor    = Heuristic.Decide(spec, now, demand, applied, lastDec).DesiredFloor
predictedContrib  = Σ Prediction.PerLabelSet[L]  where L ⊇ policy.github.labels
activityContrib   = Σ Sample.PerLabelSet[L]      where L ⊇ policy.github.labels
desiredFloor      = clamp(
                      max(scheduledFloor, predictedContrib, activityContrib),
                      floor.min,
                      min(floor.max, backendMax))
```

Same label-superset attribution rule used by the predictor (spec §3.4 of v0.2.0).
Same `max(...)` composition. Same cooldown semantics (decreases rate-limited by
`Status.LastDecreaseTime` per v0.1.0).

Each signal is independently toggleable:

- `spec.schedule` nil → schedule contributes 0.
- `spec.predictor.enabled = false` → predictor not called, contributes 0.
- `spec.activity.enabled = false` → activity not called, contributes 0.

If all three are disabled the desiredFloor reduces to `floor.min`.

### 3.3 The bug fix: `queueRule.headroom` removed

The v0.1.0 `bestHeadroom(spec.QueueRule.Headroom, demand.Queued)` call in
`internal/scheduler/heuristic.go` is removed. Reason: every queued job ARC sees, ARC
already reactively scales for; adding a headroom multiplier on top of that count is
double-counting. The bug is provable: with `Headroom: [{whenQueueAtLeast: 5, addRunners: 3}]`
and 5 jobs queued, ARC requests 5 runners reactively AND warmrunners sets
`minRunners: 8` — net 8 runners for 5 jobs, three idle, real cost on a GPU pool.

CRD-side: the `HeadroomTier` type and the `Headroom []HeadroomTier` field on `QueueRule`
are deleted outright. `pollInterval` and `cooldown` stay — they are generic config used
by every signal.

No users exist yet (the project is unreleased to anyone). No deprecation cycle. The
field is gone.

### 3.4 Window: fixed default, configurable

`spec.activity.windowSeconds` (default `900` = 15 min). Every reconcile asks "any
non-bot workflow_run in the last `windowSeconds`?". Stateless — no `LastActivityTime`
field on status, no race conditions on controller restart.

If zero non-bot runs in the window → activity contributes 0. Cooldown drains any
existing applied floor at the v0.1.0 rate, so the pool walks down naturally rather
than dropping the floor in one step.

**Why 15 minutes** — picked at the conservative edge of the industry cluster for
scale-down-style timers, justified by the dev iteration data:

- **Lower bound** from CI run length: CircleCI's 2024 dataset reports a mean workflow
  of 2m 49s ([CircleCI 2024 State of Software Delivery](https://circleci.com/resources/2024-state-of-software-delivery/)).
  A typical fix-push cycle is "push → wait 3min for CI → read result → edit → push"
  ≈ 5 min minimum.
- **Upper bound** from human flow research: Gloria Mark's 23-min recovery time after
  interruption ([summary](https://gitscrum.com/en/solutions/pains/23-minutes-to-refocus-after-each-interruption-context-switching))
  brackets a realistic active-session push-to-push gap at ~5–20 min.
- **Industry cluster of similar timers**: Kubernetes HPA stabilization 5 min,
  Cluster Autoscaler scale-down-unneeded-time 10 min, KEDA cooldownPeriod 5 min,
  AWS EC2 ASG default cooldown 5 min, Buildkite agent idle 10 min, Karpenter
  community guidance 15 min. 15 minutes sits at the conservative edge — biases
  toward experience over cost.
- **Cost differential** is bounded: at $5/hr GPU, 4hr session, 5 days/wk, going
  from 5 min to 15 min costs ~+$18/mo per pool of warm-tail overhang. One
  developer-hour lost to a wrongly-cold pool costs $75-150 at loaded rates —
  the experience bias is the correct trade.

The fixed window covers normal "fix lint → push → push" iteration. The known edge case
("dev pushes, walks away 12 min, comes back, pushes again") re-warms on return.
Acceptable trade vs. carrying a sliding-window state machine that adds restart and
race-condition bug surface. Users with predominantly fast CI (sub-2-min runs) can
dial down to 5 minutes via `spec.activity.windowSeconds: 300`; users with slow
suites can dial up to 30 min.

### 3.5 Bot filter

Activity is human signal; bot noise (Dependabot, Renovate, etc.) must not keep the
pool warm 24/7. A workflow_run is treated as bot-driven when **any** of:

1. `actor.type == "Bot"` (the `Simple User` schema, returned by workflow_runs).
2. `triggering_actor.type == "Bot"`.
3. `actor.login` ends in `[bot]` — GitHub-App identities always carry this suffix
   ([authenticating with a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/about-authentication-with-a-github-app)).
4. `actor.login` matches the union of the built-in denylist plus
   `spec.activity.botLoginDenylist`.

Built-in denylist (compiled in, append-only via CRD):

- `dependabot[bot]`
- `renovate[bot]`
- `github-actions[bot]`
- `mergify[bot]`
- `codecov[bot]`
- `copilot-pull-request-reviewer[bot]`
- `self-hosted-renovate[bot]`

User-supplied entries append to (do not replace) the built-in set. Use cases the
configurable list covers: PAT-driven machine users that don't carry `[bot]`
suffix — `snyk-bot`, `getsentry-bot`, in-house `*-ci` / `*-deploy` accounts.

### 3.6 Magnitude calculation

For each non-bot workflow_run in the window:

1. Fetch the workflow YAML at `(run.path, run.head_sha)` via the existing
   `WorkflowFetcher` cache (v0.2.x).
2. Parse via the existing `workflow.Parse` (v0.2.x).
3. Expand matrix combos for every job (include / exclude / literal lists).
4. Aggregate per-label-set counts via `LabelSetKey(labels)`.
5. The activity contribution per policy is the sum of counts for label sets where
   the run's labels are a superset of the policy's `spec.github.labels` (same rule
   as the predictor — preserves v0.2.x attribution semantics).

The cache key is `(owner, repo, path, head_sha)` — unchanged from v0.2.x; cache is
process-shared and content-addressed.

Dynamic forms (matrix from `${{ fromJSON(...) }}`, `runs-on: ${{ inputs.x }}`,
remote `uses:`) are flagged Dynamic by the parser and excluded from the count, same
as v0.2.x. A `dynamic_skipped` Hook fires for metrics.

### 3.7 CRD additions

Additive on the `Spec`, additive on `Status`, **one removal** on `QueueRule`.

```go
// New: ActivityConfig configures the activity-driven warm-floor signal (v0.3.0).
type ActivityConfig struct {
    // +kubebuilder:default=true
    // +optional
    Enabled bool `json:"enabled,omitempty"`

    // +kubebuilder:validation:Minimum=60
    // +kubebuilder:validation:Maximum=7200
    // +kubebuilder:default=900
    // +optional
    WindowSeconds int32 `json:"windowSeconds,omitempty"`

    // BotLoginDenylist is appended to the built-in denylist. Entries match
    // workflow_run.actor.login exactly (case-sensitive). Use for PAT-driven
    // machine users that lack the [bot] suffix (e.g. "snyk-bot",
    // "getsentry-bot").
    //
    // +kubebuilder:validation:MaxItems=64
    // +listType=set
    // +optional
    BotLoginDenylist []string `json:"botLoginDenylist,omitempty"`
}
```

Spec additions:
```go
// +optional
Activity *ActivityConfig `json:"activity,omitempty"`
```

Status additions:
```go
// +optional
ActivityFloor int32 `json:"activityFloor,omitempty"`

// +listType=atomic
// +optional
ActivityLabelSets []PredictedLabelSet `json:"activityLabelSets,omitempty"`
```

Reuses `PredictedLabelSet` from v0.2.x — same shape.

Printer column:
```go
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activityFloor`
```

Condition vocabulary gains `ActivityAvailable` (mirrors `PredictorAvailable`).

CRD removals (deletes, no deprecation):
- Field: `QueueRule.Headroom []HeadroomTier` → gone.
- Type: `HeadroomTier` struct → gone.

### 3.8 New metrics

- `warmrunners_activity_floor{policy}` (gauge) — per-policy activity contribution.
- `warmrunners_activity_jobs_total{policy,labels}` (gauge) — per-label-set count;
  stale label sets pruned via `DeleteLabelValues` across reconciles (same pattern
  as `warmrunners_predicted_jobs_total`).
- `warmrunners_activity_bot_filtered_total{policy,actor}` (counter) — bumped each
  time a run is filtered as bot-driven. The `actor` label uses the deny-reason
  (`bot_type`, `bot_suffix`, `denylist`). Cardinality bounded by reason values.

The existing v0.2.x metrics (`warmrunners_predicted_floor`,
`warmrunners_predicted_jobs_total`, `warmrunners_workflow_yaml_fetch_total`) stay.

## 4. Quota and performance

Per poll, the activity layer issues at most:

- 1 request: `GET /actions/runs?created=>{since}&per_page=100`, ETag-cached.
- 0 requests for workflow YAML: same `WorkflowFetcher` cache the predictor uses —
  hot for any workflow seen recently.

Steady-state cost at 30s `pollInterval`: roughly 0–1 net request per poll. The
`created=>` filter narrows the list to the activity window only, so the response body
stays small. The predictor's existing `/actions/runs?status=…` calls are unaffected.

Worst case (cold cache, many distinct head_shas): up to N YAML fetches where N is
the number of distinct (path, head_sha) pairs in the window. Bounded by
`spec.predictor.maxRunsPerPoll` (already exists, default 50) — the activity layer
respects the same cap.

## 5. Error handling and safety

Same posture as v0.2.x: the activity signal can only fail to **raise** the floor.

- Network/HTTP errors on the runs listing → return error; reconciler sets
  `ActivityAvailable=False`, contributes 0 to the floor. Schedule + predictor still
  compute; floor degrades to v0.1.x + v0.2.x behavior.
- Per-run YAML fetch or parse error → drop only that run; siblings still contribute.
- Dynamic-form jobs → excluded from the count; Hook fires; the contribution from
  other jobs in the same run still lands.

`floor.max` and the backend's `GetMax` remain hard caps. The existing cooldown on
`Status.LastDecreaseTime` rate-limits decreases regardless of which signal drops.

The activity layer never deletes runners. Same as every prior version, it only
patches the backend's floor field.

## 6. Testing

- **Unit (`internal/activity/...`):**
  - Sample with no recent runs → empty result.
  - Sample with one non-bot run → returns correct fanout from parsed YAML.
  - Bot filter: `type == "Bot"` → filtered; `[bot]` suffix → filtered;
    built-in denylist → filtered; user-appended denylist → filtered;
    `triggering_actor.type == "Bot"` → filtered.
  - `event: schedule` → filtered.
  - Window-edge behavior: run created `windowSeconds - 1` ago → counted;
    `windowSeconds + 1` ago → not counted.
  - Matrix expansion: literal `[a, b, c]` → 3; include/exclude → adjusted;
    `${{ fromJSON(...) }}` → 0 + Hook tick.
- **Integration (envtest + httptest):**
  - Reconciler with all three signals active → desiredFloor =
    `max(schedule, predictor, activity)`.
  - Cooldown applies on decrease when activity drops out.
  - `spec.activity.enabled = false` → activity not consulted, behavior reduces
    to v0.2.x exactly.
  - Removing `queueRule.headroom` doesn't break v0.1.x example policies (they
    didn't use that field) — golden-CRD test confirms.
- **Manual kind exercise (mandatory before tag, per CLAUDE.local.md):**
  - kind cluster + livetest repo.
  - Push to a feature branch on `warmrunners-livetest` → observe
    `status.activityFloor` rise within one poll.
  - Wait 15 minutes silent → observe activityFloor drop to 0, cooldown drains
    appliedFloor naturally.
  - Trigger a Dependabot-style run → confirm bot filter excludes it (no
    activityFloor bump).

## 7. Observability

The three new metrics (§3.8) plus the `ActivityAvailable` condition cover the
common debugging questions:

- "Why isn't the pool warm?" → check `ActivityAvailable` condition + activityFloor
  gauge.
- "Why is the pool warm when nobody pushed?" → check
  `warmrunners_activity_bot_filtered_total` — bots not being filtered properly is
  the most likely cause.
- "Why is activityFloor smaller than I expect?" → check `dynamic_skipped` count
  from the predictor's existing fetch counter — same workflows surface there.

`status.activityLabelSets` shows the top-N (default 8) label sets the activity
layer contributed.

## 8. Compatibility and rollout

- **No known production users.** The project is in `v1alpha1` and no public install
  signal exists yet (no GitHub Discussions, no issues from outside contributors).
  `queueRule.headroom` deletion is treated as a clean break in the 0.x line — no
  migration path. Anyone applying a CR with `headroom: [...]` set after v0.3.0 lands
  gets a "unknown field" error from the API server, which is the correct, immediate
  feedback. The example policies under `examples/` ship updated in the same release
  so anyone copying them gets a v0.3.0-shaped CR.
- v0.2.x policies without `spec.activity` validate unchanged. Activity defaults to
  enabled (`Enabled: true`) with `windowSeconds: 900` and an empty user denylist.
  Activity contributes 0 when no recent non-bot runs exist — behavior matches
  v0.2.x when the repo is idle.
- Helm chart: no template changes; predictor and activity share a single
  `WorkflowFetcher` constructed in `cmd/main.go`.

## 9. Repository layout

```
warmrunners/
  api/v1alpha1/                 # CRD types — adds ActivityConfig + Status fields
  internal/
    activity/                   # NEW: Activity interface + WorkflowRunsSampler impl
    controller/                 # reconciler — extends max() with activity leg
    demand/                     # unchanged
    predictor/                  # unchanged (cache + parser reused by activity)
    scheduler/                  # bug fix: bestHeadroom + Headroom removed
    adapter/                    # unchanged
  ...
```

## 10. Roadmap (post-v0.3.0)

- v0.3.0 — this spec.
- v0.4.0 — validating admission webhook (cross-policy conflict detection);
  richer queueRule shapes (e.g. per-label headroom — re-introduced cleanly).
- later — extended predictor mechanisms (`workflow_run` chains, `environment`
  approval gates); forecasting via rolling day-of-week × hour-of-day histogram;
  webhook `DemandSource`; possible `TerraformAwsAdapter`.
- v1.0.0 — when the CRD graduates `v1alpha1 → v1`.

## 11. Open questions resolved during brainstorming

- Activity window length → fixed default 15min, user-configurable via
  `spec.activity.windowSeconds`.
- Magnitude calculation → max fanout from parsed YAML (reuses v0.2.x parser);
  empirical observation deferred to "later" if YAML-derived counts prove wrong
  in practice.
- Events API as primary signal → dropped; documented 30s–6h latency makes it
  unsuitable for real-time activity detection.
- Sliding window with `LastActivityTime` state → dropped in favor of stateless
  fixed window to avoid restart/race-condition bug surface.
- Bot filter configurability → built-in denylist + user-appended list; no
  override flag (append-only).
- `queueRule.headroom` deprecation cycle → none; deleted outright since no users
  exist.
