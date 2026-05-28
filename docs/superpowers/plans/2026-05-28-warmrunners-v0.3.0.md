# warmrunners v0.3.0 Activity-Driven Dynamic Floor — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add an activity-driven, magnitude-aware warm-floor signal that tracks recent CI activity in the repo, and remove the v0.1.x `queueRule.headroom` double-counting bug in the same release.

**Architecture:** New `internal/activity/` package with an `Activity` interface and a `WorkflowRunsSampler` implementation that calls `GET /actions/runs?created=>…`, filters out bot-driven runs, and uses the existing v0.2.x `WorkflowFetcher` + `workflow.Parse` to derive per-label-set fanout. Reconciler extends the existing `max(...)` floor composition with the activity contribution. The `bestHeadroom` path in `internal/scheduler/heuristic.go` and the `Headroom`/`HeadroomTier` types on the CRD are deleted outright (no users today; clean break).

**Tech Stack:** Go 1.25, kubebuilder 4.9.0, controller-runtime, `rhysd/actionlint` (already in tree), GitHub REST API (workflow_runs), `prometheus/client_golang`. No new dependencies.

**Source spec:** `docs/superpowers/specs/2026-05-28-warmrunners-v0.3.0.md`. Read it before any task — it carries the bot-filter algorithm, window justification, attribution rule, and metric definitions.

**Branches:** PR 1 = `fix/headroom-and-activity-pkg`; PR 2 = `feat/wire-activity`. Per-component commits on each branch. Conventional Commits (`feat(scope):`, `fix(scope):`, `docs:`); scope is the codebase area, never the version.

**Commit policy** (per `CLAUDE.local.md`): component-grouped commits on the branch; signed (`-S`); no Claude trailers. The squash-merge collapses to one main-line commit per PR.

**Release gate** (per `CLAUDE.local.md`): manual kind exercise against `sarataha/warmrunners-livetest` MANDATORY before tagging `v0.3.0`. Skipping repeats the v0.2.0 mistake.

---

## File map

**Create (PR 1):**
- `internal/activity/activity.go` — `Activity` interface, `Sample` type, `BuiltinDenylist`, exported helper `IsBotActor`.
- `internal/activity/activity_test.go` — table tests for `IsBotActor` covering all bot-filter branches.
- `internal/activity/workflow_runs_sampler.go` — `WorkflowRunsSampler` implementing `Activity` via the GitHub REST workflow_runs endpoint.
- `internal/activity/workflow_runs_sampler_test.go` — httptest scenarios: empty window, single human run, bot filter (4 ways), `event: schedule` filter, window-edge inclusion, matrix fanout, dynamic-form skip, multi-run aggregation, runsCap respected, per-run YAML failure drops only that run.

**Modify (PR 1):**
- `internal/scheduler/heuristic.go` — remove `bestHeadroom` helper and its call site inside `Heuristic.Decide`; the `Decide` signature stays unchanged (still takes `Demand{Queued, Running}`; just ignores the queued count for floor headroom).
- `internal/scheduler/heuristic_test.go` — delete tests that asserted `bestHeadroom` behavior; keep tests that exercise schedule-window and cooldown logic.
- `api/v1alpha1/warmrunnerpolicy_types.go` — delete the `HeadroomTier` struct and the `Headroom []HeadroomTier` field on `QueueRule`. (CRD additions for activity ship in PR 2.)
- `api/v1alpha1/zz_generated.deepcopy.go` — regenerated.
- `config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml` — regenerated.
- `api/v1alpha1/testdata/golden_crd.yaml` — updated golden snapshot.
- `examples/policy-arc.yaml` — remove the `headroom:` block (the field is gone). Leave `pollInterval` and `cooldown`.

**Create (PR 2):**
- `internal/controller/activity_integration_test.go` — reconciler tests with a stub Activity: floor folded via `max(...)`, capped at `floor.max`, `ActivityAvailable` condition, disabled-when-Enabled=false, label-superset attribution, top-N `ActivityLabelSets` population.

**Modify (PR 2):**
- `api/v1alpha1/warmrunnerpolicy_types.go` — add `ActivityConfig` struct, `Spec.Activity *ActivityConfig`, `Status.ActivityFloor`, `Status.ActivityLabelSets`, the `Active` printer column, and the `ActivityAvailable` condition reason constants.
- `api/v1alpha1/zz_generated.deepcopy.go` — regenerated.
- `config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml` — regenerated.
- `api/v1alpha1/testdata/golden_crd.yaml` — updated golden snapshot.
- `internal/controller/warmrunnerpolicy_controller.go` — add `Activity activity.Activity` field on the reconciler; new leg in `Reconcile` that calls `Sample` with the per-policy window + merged denylist + token; fold into the existing `max()` composition; populate `Status.ActivityFloor`/`ActivityLabelSets`; toggle `ActivityAvailable`.
- `internal/controller/metrics.go` — register `warmrunners_activity_floor`, `warmrunners_activity_jobs_total`, `warmrunners_activity_bot_filtered_total`.
- `internal/controller/crd_validation_test.go` — new envtest cases for `spec.activity.windowSeconds` bounds (60 ≤ x ≤ 7200) and `botLoginDenylist` MaxItems=64.
- `cmd/main.go` — construct `WorkflowRunsSampler` reusing the existing `WorkflowFetcher` + `http.Client`; inject onto the reconciler; wire its Hooks to the new metrics.
- `examples/policy-arc.yaml` — add a commented `activity:` block showing defaults.
- `README.md` — extend "How it works" to mention the activity signal alongside schedule/predictor; update the floor formula; bump install version to `0.3.0`.
- `CHANGELOG.md` — add `## [0.3.0]` (Added / Changed / Removed / Fixed sections per spec §3.3 and §3.7).

**No change:**
- `internal/predictor/`, `internal/demand/`, `internal/adapter/` — unchanged.
- The cooldown machinery (`Status.LastDecreaseTime`) — already correct; activity composes with it via `max()` exactly like the predictor.
- Helm chart templates — predictor and activity share a process-wide `WorkflowFetcher`; no new flags or env vars.

---

## Task 1: Branch + bug-fix baseline (PR 1 starts here)

**Files:** none yet — just branching and asserting baseline.

- [ ] **Step 1.** Verify clean main on `bcaa037` (or later v0.2.1 finalize commit). `git status` clean.
- [ ] **Step 2.** Branch: `git checkout -b fix/headroom-and-activity-pkg`.
- [ ] **Step 3.** Baseline: `make test` GREEN; `make lint` 0 issues; `go test -race ./internal/...` clean. Record coverage to compare against later.

---

## Task 2: Remove `bestHeadroom` from the scheduler (the bug fix)

**Files:**
- Modify: `internal/scheduler/heuristic.go` (line 17 + lines 59–69)
- Modify: `internal/scheduler/heuristic_test.go` (remove headroom-specific tests; keep schedule+cooldown tests)

**Acceptance:**
- `bestHeadroom` function and its call from `Heuristic.Decide` are gone. `Decide` still takes `d Demand` (signature unchanged — predictor and demand source still use it; we just stop using `d.Queued` as a floor adder).
- All existing `Decide` tests that asserted schedule windows + cooldown still pass.
- `go test ./internal/scheduler/...` GREEN.

- [ ] **Step 1.** Read `internal/scheduler/heuristic.go` to confirm current `bestHeadroom` callsite + signature. Read `heuristic_test.go` to enumerate tests that depend on `Headroom`.
- [ ] **Step 2.** Delete the `bestHeadroom` function (lines ~59-69). Delete the `headroom := bestHeadroom(...)` line inside `Decide` and any addition it made to the candidate floor.
- [ ] **Step 3.** Update `Decide`'s docstring to reflect the new shape ("returns `max(scheduleBase)` clamped to `floor.min`/`floor.max`, with cooldown applied to decreases"; remove any mention of queue-headroom).
- [ ] **Step 4.** Delete the tests that asserted `bestHeadroom` directly OR that relied on the floor rising from queued-count alone. Keep schedule-window + cooldown + floor-clamp tests.
- [ ] **Step 5.** `go test ./internal/scheduler/... -v -race` GREEN.
- [ ] **Step 6.** Smoke check that `internal/controller/...` still compiles — the reconciler reads `dec.DesiredFloor`, no API change.
- [ ] **Step 7.** `make test` GREEN end-to-end.
- [ ] **Step 8.** Commit: `fix(scheduler): remove bestHeadroom — queue-depth double-counted ARC reactive scaling`.

---

## Task 3: Remove `Headroom`/`HeadroomTier` from the CRD

**Files:**
- Modify: `api/v1alpha1/warmrunnerpolicy_types.go` (lines 96-101 for `HeadroomTier`; line 106 for `Headroom` field on `QueueRule`)
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml`
- Update: `api/v1alpha1/testdata/golden_crd.yaml`
- Modify: `examples/policy-arc.yaml` (delete the `headroom:` block under `queueRule:`)

**Acceptance:**
- `HeadroomTier` type definition is gone.
- `QueueRule.Headroom` field is gone.
- Regenerated deepcopy no longer references either.
- Regenerated CRD YAML schema for `spec.queueRule` lists only `pollInterval` and `cooldown`.
- `examples/policy-arc.yaml` validates against the regenerated CRD.
- Golden-CRD test passes against the new snapshot.

- [ ] **Step 1.** Delete the `HeadroomTier` struct (~lines 96-101) and the `Headroom []HeadroomTier` field on `QueueRule` (~line 106) in `warmrunnerpolicy_types.go`.
- [ ] **Step 2.** Regenerate: `make manifests generate`. Inspect the diff: `zz_generated.deepcopy.go` should remove the `DeepCopyInto` for `HeadroomTier` and the Headroom-copying block from `QueueRule`. CRD YAML should drop the `headroom` schema fragment under `spec.queueRule`.
- [ ] **Step 3.** Update `api/v1alpha1/testdata/golden_crd.yaml` to the new generated CRD.
- [ ] **Step 4.** Edit `examples/policy-arc.yaml`. Remove the entire `headroom:` block (the array under `queueRule:`). Keep `pollInterval` and `cooldown` intact. Verify YAML still parses.
- [ ] **Step 5.** Run `go test ./api/v1alpha1/...`. The golden test must pass; if it doesn't, the diff between expected and generated is the bug — fix the snapshot or the markers.
- [ ] **Step 6.** Run the existing `internal/controller/crd_validation_test.go` envtest. Verify `examples/policy-arc.yaml` still applies against the regenerated CRD (it should — we only removed an optional field).
- [ ] **Step 7.** `make test` GREEN. `make lint` 0 issues.
- [ ] **Step 8.** Commit: `fix(crd): drop queueRule.headroom and HeadroomTier type`.

---

## Task 4: `Activity` interface + `IsBotActor` helper + `BuiltinDenylist`

**Files:**
- Create: `internal/activity/activity.go`
- Create: `internal/activity/activity_test.go`

**Acceptance — exact API surface:**

```go
package activity

// Activity samples recent CI activity in a repo and returns per-label-set
// fanout demand. Implementations must honor ctx.Done() and never block past
// the caller's timeout. token is the per-policy GitHub credential; window
// bounds how far back the implementation looks; denylist is the merged
// built-in + user-supplied bot login list.
type Activity interface {
    Sample(ctx context.Context, owner, repository, token string,
           window time.Duration, denylist []string) (Sample, error)
}

// Sample is the per-poll output. PerLabelSet keys are predictor.LabelSetKey
// of the runs-on labels; values are the max-fanout count derived from the
// triggered workflow's YAML at head_sha.
type Sample struct {
    PerLabelSet map[string]int
}

// BuiltinDenylist is the compiled-in set of well-known bot actor logins.
// User-supplied entries from spec.activity.botLoginDenylist are appended
// to (not replacing) this list before being passed to Sample.
var BuiltinDenylist = []string{
    "dependabot[bot]",
    "renovate[bot]",
    "github-actions[bot]",
    "mergify[bot]",
    "codecov[bot]",
    "copilot-pull-request-reviewer[bot]",
    "self-hosted-renovate[bot]",
}

// IsBotActor decides whether a workflow_run is bot-driven and should be
// excluded from the activity signal. actorType and triggeringActorType are
// the "type" field of the Simple User objects on workflow_run.actor and
// .triggering_actor ("User" or "Bot"). actorLogin is the actor's login.
// denylist is the merged built-in + user-supplied list. Returns true (filter)
// if any of: actorType == "Bot", triggeringActorType == "Bot",
// actorLogin ends in "[bot]", or actorLogin appears in denylist.
func IsBotActor(actorType, triggeringActorType, actorLogin string, denylist []string) (bool, string)
```

`IsBotActor` returns `(true, reason)` where reason ∈ `{"bot_type", "trigger_bot_type", "bot_suffix", "denylist"}` — the metrics layer (PR 2) uses this as a label.

- [ ] **Step 1.** Create `internal/activity/activity.go` with the interface, type, `BuiltinDenylist`, and `IsBotActor`. Package GoDoc explains the role of Activity (sibling of Predictor) and the merge-not-replace semantics of the denylist.
- [ ] **Step 2.** Create `internal/activity/activity_test.go`. Table tests for `IsBotActor` cover:
  - User actor + User triggering + non-bot login + empty denylist → `(false, "")`.
  - `actorType == "Bot"` → `(true, "bot_type")`.
  - `triggeringActorType == "Bot"` (actor User) → `(true, "trigger_bot_type")`.
  - Login `"dependabot[bot]"` → `(true, "bot_suffix")` because of the suffix check (or `"denylist"` if it matches the builtin list first — order matters; implement suffix BEFORE denylist so generic suffix wins over named entry; document the precedence).
  - Login `"custom-thing[bot]"` not in builtin list → `(true, "bot_suffix")`.
  - Login `"snyk-bot"` (no suffix, no builtin) but appears in user-supplied denylist → `(true, "denylist")`.
  - Login matches a builtin entry exactly without `[bot]` suffix (none exist today but defensive) → `(true, "denylist")`.
  - Empty actorLogin → `(false, "")` (don't accidentally classify "" as bot).
- [ ] **Step 3.** `go test ./internal/activity/... -v -race` GREEN.
- [ ] **Step 4.** `go vet ./...` clean. `gofmt -l` empty.
- [ ] **Step 5.** Do NOT commit yet — Task 5 finishes the package and we commit together.

---

## Task 5: `WorkflowRunsSampler` implementation

**Files:**
- Create: `internal/activity/workflow_runs_sampler.go`
- Create: `internal/activity/workflow_runs_sampler_test.go`

**Acceptance — API:**

```go
package activity

// WorkflowRunsSampler implements Activity by calling
// GET /repos/{o}/{r}/actions/runs?created=>{since}&per_page=100 and parsing
// each non-bot run's workflow YAML at head_sha via the v0.2.x WorkflowFetcher.
type WorkflowRunsSampler struct { /* unexported */ }

// NewWorkflowRunsSampler constructs a sampler reusing the predictor's
// WorkflowFetcher (so YAML cache is process-shared). runsCap caps the number
// of recent runs scanned per call (default 50 when <= 0; same default as
// predictor.WorkflowNeedsGraph.RunsCap).
func NewWorkflowRunsSampler(
    httpClient *http.Client,
    fetcher predictor.WorkflowFetcher,
    runsCap int,
) *WorkflowRunsSampler

// Hooks expose the per-event metrics signals. Optional; nil hooks are no-ops.
type Hooks struct {
    OnBotFiltered    func(reason string)              // bot_type | trigger_bot_type | bot_suffix | denylist
    OnEventFiltered  func(event string)               // schedule, check_run, check_suite, ...
    OnYAMLFetch      func(result string)              // fetched | error
    OnDynamicSkipped func(reason string)              // runs_on_expr | matrix_expr | remote_uses
}

func (s *WorkflowRunsSampler) WithHooks(h Hooks) *WorkflowRunsSampler

// Implements Activity.
func (s *WorkflowRunsSampler) Sample(ctx context.Context, owner, repo, token string,
    window time.Duration, denylist []string) (Sample, error)
```

**Behaviors required (test-driven):**

1. **Empty window** (no runs in last `window`) → returns `Sample{PerLabelSet: map[string]int{}}` and nil error. No fetcher calls.
2. **Single non-bot push run** with `runs-on: [self-hosted, gpu]` and matrix `os: [a, b, c]` → `Sample.PerLabelSet[LabelSetKey([self-hosted, gpu])] = 3`. One YAML fetch.
3. **`actor.type == "Bot"`** → filtered, contributes 0, OnBotFiltered("bot_type") fires.
4. **`triggering_actor.type == "Bot"` with actor User** → filtered, OnBotFiltered("trigger_bot_type") fires.
5. **`actor.login = "dependabot[bot]"`** → filtered, OnBotFiltered("bot_suffix") fires.
6. **User-supplied `denylist: ["snyk-bot"]`** + actor login `"snyk-bot"` (type User) → filtered, OnBotFiltered("denylist") fires.
7. **`event: schedule`** → filtered, OnEventFiltered("schedule") fires.
8. **`event: check_run`** → filtered (downstream chain), OnEventFiltered("check_run") fires.
9. **`event: pull_request`** → not filtered (human event).
10. **Window-edge inclusion:** run created `window - 1s` ago → counted; `window + 1s` ago → not counted (verify the `created=>{timestamp}` query param formatting — RFC3339 UTC).
11. **Dynamic-form job** (`runs-on: ${{ inputs.x }}`) → skipped, OnDynamicSkipped("runs_on_expr") fires; other jobs in same run still count.
12. **Multi-run aggregation:** two distinct active runs each contribute → counts sum into the same PerLabelSet key when labels match.
13. **runsCap = 1** with 5 active runs → only first run scanned (deterministic by API order).
14. **Per-run YAML 404** → that run drops, OnYAMLFetch("error") fires, other runs still contribute.
15. **Network error on runs list** → returns `(Sample{}, err)`. Hooks not called.
16. **Authorization header sent** when token non-empty (mirrors the v0.2.1 predictor fix — the bug we don't want to re-ship).

**Implementation guidance:**

- Reuse the v0.2.x predictor's `predictor.WorkflowFetcher` interface — don't reimplement YAML fetching. Same cache key `(owner, repo, path, head_sha)`.
- Reuse the v0.2.x `workflow.Parse` for YAML → typed AST → `Job.RunsOn.Labels` + `Job.Matrix.Combos` → `LabelSetKey`.
- `created=>` query value is RFC3339 UTC computed as `now.UTC().Add(-window).Format(time.RFC3339)`. URL-encode the `>` as `%3E` if your HTTP layer doesn't (Go's `net/url.Values.Encode` does).
- HTTP request mirrors the v0.1.x poller pattern: User-Agent `warmrunners/<version>`, `Authorization: Bearer <TrimSpace(token)>` when non-empty, `Accept: application/vnd.github+json`.
- ETag conditional: cache the last response's ETag per `(owner, repo, since-truncated-to-minute)` key. On 304 return the cached `Sample`. (Since `since` slides per poll, an ETag is most useful when the next poll falls in the same minute bucket; document this as best-effort.)
- Filter precedence: 1) bot filter (any of four reasons); 2) event filter; 3) parse + count.

- [ ] **Step 1.** Write `workflow_runs_sampler_test.go` with the 16 scenarios above using `httptest.Server`. Each test starts RED.
- [ ] **Step 2.** Implement `workflow_runs_sampler.go` to satisfy them one at a time, RED→GREEN per scenario. Reuse `predictor` package's exports; do not duplicate fetch / parse logic.
- [ ] **Step 3.** Match the v0.2.1 auth pattern exactly: set Authorization header per-request from `token` argument; TrimSpace; mirror the v0.1.x newline-defense comment.
- [ ] **Step 4.** Honor `ctx.Done()` during any retry sleep or HTTP wait (reuse the v0.2.x `sleepCtx` pattern if useful; otherwise rely on `http.NewRequestWithContext` handling for the main call).
- [ ] **Step 5.** `go test -race ./internal/activity/...` GREEN. Demand-package and predictor-package tests still pass (no shared-state regressions).
- [ ] **Step 6.** Coverage on `internal/activity/...` ≥ 70%. `make lint` 0 issues. `make test` GREEN end-to-end.
- [ ] **Step 7.** Commit (groups Task 4 + Task 5 together as the package): `feat(activity): activity-driven warm-floor signal from workflow_runs`.

---

## Task 6: PR 1 — open, review, merge

- [ ] **Step 1.** Push: `git push -u origin fix/headroom-and-activity-pkg`.
- [ ] **Step 2.** Open PR to main. Title: `fix: drop queueRule.headroom + add internal/activity package`. Body: short — what + non-impact (activity not wired yet) + tested gates green.
- [ ] **Step 3.** Wait for CI green (`Tests`, `Lint`). If govulncheck flags new deps, fix and re-push.
- [ ] **Step 4.** Squash-merge with subject `fix: drop queueRule.headroom + add activity package`.
- [ ] **Step 5.** Sync local main; delete the merged branch (`git branch -D fix/headroom-and-activity-pkg`; `git push origin --delete fix/headroom-and-activity-pkg`).

---

## Task 7: Branch PR 2 + add `ActivityConfig` CRD types

**Files:**
- Modify: `api/v1alpha1/warmrunnerpolicy_types.go`
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml`
- Update: `api/v1alpha1/testdata/golden_crd.yaml`
- Modify: `internal/controller/crd_validation_test.go` (add envtest cases)
- Modify: `examples/policy-arc.yaml` (add a commented `activity:` block)

**Acceptance — exact CRD additions:**

```go
// ActivityConfig configures the activity-driven warm-floor signal (v0.3.0).
type ActivityConfig struct {
    // +kubebuilder:default=true
    // +optional
    Enabled bool `json:"enabled,omitempty"`

    // WindowSeconds is the rolling window over which recent non-bot
    // workflow_runs are counted. Defaults to 900 (15 minutes); see spec §3.4
    // for the rationale.
    //
    // +kubebuilder:validation:Minimum=60
    // +kubebuilder:validation:Maximum=7200
    // +kubebuilder:default=900
    // +optional
    WindowSeconds int32 `json:"windowSeconds,omitempty"`

    // BotLoginDenylist is appended to the built-in denylist (dependabot[bot],
    // renovate[bot], github-actions[bot], mergify[bot], codecov[bot],
    // copilot-pull-request-reviewer[bot], self-hosted-renovate[bot]). Use
    // for PAT-driven machine users that lack the [bot] suffix (e.g.
    // "snyk-bot", "getsentry-bot", in-house "*-ci" / "*-deploy" accounts).
    // Entries match workflow_run.actor.login exactly (case-sensitive).
    //
    // +kubebuilder:validation:MaxItems=64
    // +listType=set
    // +optional
    BotLoginDenylist []string `json:"botLoginDenylist,omitempty"`
}
```

Spec addition:
```go
// +optional
Activity *ActivityConfig `json:"activity,omitempty"`
```

Status additions (reuse the existing `PredictedLabelSet` type from v0.2.x):
```go
// +optional
ActivityFloor int32 `json:"activityFloor,omitempty"`

// +listType=atomic
// +optional
ActivityLabelSets []PredictedLabelSet `json:"activityLabelSets,omitempty"`
```

New printer column under the existing five:
```go
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activityFloor`
```

Condition reason constants (new file or appended to an existing constants block — match the pattern v0.2.x used for `PredictorAvailable`):
```go
const (
    ActivityConditionReasonAvailable    = "Available"
    ActivityConditionReasonSampleError  = "SampleError"
)
```

- [ ] **Step 1.** Branch: `git checkout -b feat/wire-activity` off latest main (PR 1 squash now visible).
- [ ] **Step 2.** Edit `warmrunnerpolicy_types.go`: add the two new types + spec/status fields + printer column + condition reason constants. Match the surrounding file style.
- [ ] **Step 3.** Regenerate: `make manifests generate`. Inspect the diff: deepcopy gets `DeepCopy*` funcs for `ActivityConfig`; CRD YAML schema gains `spec.activity` block (`enabled` bool default true, `windowSeconds` int32 60..7200 default 900, `botLoginDenylist` array MaxItems=64, listType=set) plus `status.activityFloor` + `status.activityLabelSets` plus the `Active` printer column.
- [ ] **Step 4.** Update `api/v1alpha1/testdata/golden_crd.yaml` to the regenerated CRD.
- [ ] **Step 5.** Extend `internal/controller/crd_validation_test.go` with envtest cases:
  - `spec.activity.windowSeconds: 59` → rejected.
  - `spec.activity.windowSeconds: 7201` → rejected.
  - No `spec.activity` block at all → accepted (field is optional pointer; v0.2.x behavior preserved on apply).
  - Empty `spec.activity: {}` → accepted; re-fetch via unstructured shows defaults `enabled: true`, `windowSeconds: 900`. (Same `metav1.Duration` round-trip gotcha noted in v0.2.0 plan — `WindowSeconds` is `int32` so it doesn't apply here; defaulting through typed clients works.)
  - `botLoginDenylist` of 65 entries → rejected.
- [ ] **Step 6.** Edit `examples/policy-arc.yaml`. Add a commented `activity:` block under `spec:` showing the defaults:
  ```yaml
  # spec.activity — activity-driven warm-floor signal (v0.3.0). Optional; omit to disable.
  # activity:
  #   enabled: true
  #   windowSeconds: 900
  #   botLoginDenylist: []   # appended to the built-in list, not replacing it
  ```
- [ ] **Step 7.** `go test ./api/v1alpha1/...` GREEN (golden + any other api tests). `go test ./internal/controller/... -run CRDValidation -v` GREEN.
- [ ] **Step 8.** `make test` GREEN. `make lint` 0 issues.
- [ ] **Step 9.** Commit: `feat(crd): activity config + activity status fields`.

---

## Task 8: Reconciler integration + metrics

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller.go`
- Modify: `internal/controller/metrics.go`
- Create: `internal/controller/activity_integration_test.go`

**Acceptance:**

Reconciler changes:
- New field on `WarmRunnerPolicyReconciler`: `Activity activity.Activity` (interface so tests inject a stub).
- After the existing scheduler + predictor leg, a new activity leg:
  - If `pol.Spec.Activity == nil || pol.Spec.Activity.Enabled`, build the call arguments:
    - `window := time.Duration(pol.Spec.Activity.WindowSeconds) * time.Second` (substitute the default `15 * time.Minute` when `pol.Spec.Activity == nil` or `WindowSeconds == 0`).
    - `denylist := append(append([]string(nil), activity.BuiltinDenylist...), pol.Spec.Activity.BotLoginDenylist...)`.
    - `token` is the same `predictorToken(ctx, pol)` value the predictor uses; do not re-read the Secret.
  - Call `r.Activity.Sample(ctx, owner, repo, token, window, denylist)`.
  - On success: compute `activityContrib = Σ Sample.PerLabelSet[L]` where `L ⊇ pol.Spec.GitHub.Labels` (same `labelsSuperset` rule the predictor uses; reuse the existing helper).
  - On error: log + set `ActivityAvailable=False` with reason `SampleError`, contribute 0; the reconcile still succeeds and falls back to scheduler + predictor exactly as v0.2.x behaves.
  - On success: set `ActivityAvailable=True` with reason `Available`.
- Fold `activityContrib` into the existing `max()` chain. Final formula:
  ```
  desiredFloor = clamp(
      max(scheduledFloor, predictedContrib, activityContrib),
      floor.min,
      min(floor.max, backendMax))
  ```
- Populate `status.ActivityFloor = activityContrib`.
- Populate `status.ActivityLabelSets` with top-N (default 8, same `activityTopN` constant as `predictorTopN`) entries by count, descending; tie-break by `LabelSetKey` ascending — match the predictor's determinism rule.

Metrics changes (`metrics.go`):
- `warmrunners_activity_floor{policy}` gauge.
- `warmrunners_activity_jobs_total{policy,labels}` gauge (prune stale label sets each reconcile via `DeleteLabelValues` — same pattern as `warmrunners_predicted_jobs_total`).
- `warmrunners_activity_bot_filtered_total{policy,reason}` counter (`reason` ∈ `bot_type`, `trigger_bot_type`, `bot_suffix`, `denylist`).

Tests (8 scenarios in `activity_integration_test.go`):
1. Stub Activity returns 3 jobs matching policy labels → desiredFloor = 3 (with schedule=0, predicted=0, floor.max=10).
2. Stub returns 20 → capped at floor.max.
3. Cooldown holds floor when activity drops to 0 on a later reconcile.
4. `Spec.Activity.Enabled = false` → Activity.Sample not called; condition not set (or set to a documented "Disabled" reason — pick one and document).
5. Stub returns error → ActivityAvailable=False with SampleError reason; reconcile succeeds; desiredFloor still computed from schedule + predictor.
6. Stub returns labels `[gpu, self-hosted]` count=2 + `[ubuntu-latest]` count=5; policy labels `[gpu]` → only the first matches via superset → activityContrib = 2.
7. Status.ActivityFloor + ActivityLabelSets populated correctly; top-N ordering deterministic.
8. Three signals together: schedule=2, predicted=4, activity=7 → desiredFloor=7 (max wins).

- [ ] **Step 1.** Write `activity_integration_test.go` with the 8 scenarios using a stub Activity. RED for each.
- [ ] **Step 2.** Edit `warmrunnerpolicy_controller.go`: add the field, the call site, the floor folding, the condition transitions, the status population. Mirror the predictor leg's structure (helper functions like `computePredicted` → add a parallel `computeActivity`).
- [ ] **Step 3.** Edit `metrics.go`: register the three new metrics. Bot-filter counter takes the `reason` string returned by `IsBotActor` — connect via the sampler's `Hooks.OnBotFiltered` callback (Task 9 wires this).
- [ ] **Step 4.** `go test ./internal/controller/... -v -race` GREEN. `make test` GREEN.
- [ ] **Step 5.** Commit (per-component): `feat(controller): consume Activity, fold into max() floor formula`.
- [ ] **Step 6.** Commit metrics: `feat(metrics): activity_floor, activity_jobs_total, activity_bot_filtered_total`.

---

## Task 9: cmd/main.go wiring

**Files:**
- Modify: `cmd/main.go`

**Acceptance:**
- Construct a `WorkflowRunsSampler` using the **same** `httpClient` and `WorkflowFetcher` instances the predictor uses (shared cache, shared timeout flag). No new flag.
- Inject onto the reconciler's `Activity` field.
- Wire `Hooks`:
  - `OnBotFiltered(reason)` → bump `warmrunners_activity_bot_filtered_total{reason=…}` (no `{policy}` label — same reasoning as the predictor's workflow-yaml-fetch counter in v0.2.x).
  - `OnEventFiltered(event)` → optional V(2) log, no metric (cardinality of `event` is small but not interesting enough for a counter).
  - `OnYAMLFetch(result)` → bump the existing `warmrunners_workflow_yaml_fetch_total{result=…}` (shared with the predictor; the activity sampler is just another consumer of the same fetcher).
  - `OnDynamicSkipped(reason)` → bump existing predictor metric for `dynamic_skipped` symmetry.

- [ ] **Step 1.** Edit `cmd/main.go`. In the existing block that constructs the predictor, also construct the sampler using the same `httpClient`, same `fetcher`, and `runsCap = 0` (defaults to 50). Attach Hooks that bump the new + existing metrics.
- [ ] **Step 2.** Set `r.Activity` on the reconciler struct.
- [ ] **Step 3.** Update the inline comment at the predictor/sampler construction site to note that both share the YAML cache.
- [ ] **Step 4.** `go run ./cmd --help` — confirm no flag regressions and no new flag (activity uses CRD-driven config only).
- [ ] **Step 5.** `make test` GREEN. `make lint` 0 issues.
- [ ] **Step 6.** Commit: `feat(main): construct WorkflowRunsSampler, share YAML cache with predictor`.

---

## Task 10: Manual kind verification (MANDATORY — per `CLAUDE.local.md`)

Same shape as the v0.2.1 verification — and the rule that's now non-negotiable because v0.2.0 shipped broken without it.

**Files:** none touched in the repo; produces verification evidence only.

- [ ] **Step 1.** Docker up. `kind create cluster --name wrp-v030`.
- [ ] **Step 2.** `make install` (CRDs). `make docker-build IMG=warmrunners:livetest-v030`. `kind load docker-image warmrunners:livetest-v030 --name wrp-v030`. `make deploy IMG=warmrunners:livetest-v030`. `kubectl wait --for=condition=Available deployment/warmrunners-controller-manager -n warmrunners-system --timeout=120s`.
- [ ] **Step 3.** Create the gh-token Secret in default namespace (pipe `env -u GITHUB_TOKEN gh auth token | tr -d '\n' | kubectl create secret generic gh-token --from-file=token=/dev/stdin -n default`).
- [ ] **Step 4.** Apply the GARM Pool CRD stub + a Pool + a WarmRunnerPolicy. Policy includes `spec.activity.enabled: true` and `spec.predictor.enabled: true` (composition test).
- [ ] **Step 5.** **Verify activity signal fires.** Trigger `env -u GITHUB_TOKEN gh workflow run two-stage.yml -R sarataha/warmrunners-livetest`. Within one poll cycle, `kubectl get wrp gpu-policy -o jsonpath='{.status.activityFloor}'` should rise above 0 (the two-stage workflow's fanout). Also confirm `status.predictedFloor` rises during stage 1 (this is the v0.2.x predictor still working — regression check).
- [ ] **Step 6.** **Verify bot filter.** Optional but recommended: open a fake bot-shaped PR or use a previously-triggered Dependabot run on the livetest repo; confirm `warmrunners_activity_bot_filtered_total{reason="bot_suffix"}` increments and `activityFloor` does NOT include that run's fanout. If livetest has no real bot history, skip and document the gap.
- [ ] **Step 7.** **Verify window decay.** Wait `windowSeconds + 60s` (default 16 min) with no further pushes. Confirm `status.activityFloor` drops to 0; `status.appliedFloor` walks down per cooldown (v0.1.0 semantics).
- [ ] **Step 8.** **Verify bug-fix.** Apply a policy that previously had `queueRule.headroom: [{whenQueueAtLeast: 5, addRunners: 3}]`. The API server must reject it with an unknown-field error. This is the v0.3.0 breaking change — confirm the breakage is loud and clear.
- [ ] **Step 9.** Tear down: `kind delete cluster --name wrp-v030`. Revert `config/manager/kustomization.yaml` if `make install` mutated it (`git checkout config/manager/kustomization.yaml`).
- [ ] **Step 10.** If any of steps 5/6/7/8 fails: STOP. Do NOT proceed to Task 11. Fix on a new hotfix branch (`fix/v0.3.0-<bug>`), restart at Task 10. Repeat until all four verifications pass.

---

## Task 11: Docs (README + CHANGELOG)

**Files:**
- Modify: `README.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1.** Edit `README.md`:
  - Update the "How it works" paragraph to mention the activity signal alongside schedule + predictor.
  - Update the floor formula to the v0.3.0 shape: `desiredFloor = clamp(max(scheduleBase, predictedContrib, activityContrib), floor.min, floor.max)`.
  - Drop any remaining reference to `queueRule.headroom` (the field is gone).
  - Bump install version to `0.3.0` in the helm install snippet and the `cosign verify` examples.
  - One short paragraph on the activity signal: "warmrunners also tracks recent CI activity. While a repo has non-bot workflow_runs in the last 15 min (configurable), the floor is sized to match the max fanout of those workflows' parsed YAML. Quiet repo → floor drops to 0."
- [ ] **Step 2.** Edit `CHANGELOG.md`. Add `## [0.3.0] - <date-at-tag-time>` with sections:
  - **Added** — `Activity` signal; `spec.activity.{enabled, windowSeconds, botLoginDenylist}`; `status.activityFloor`, `status.activityLabelSets`, `ActivityAvailable` condition; new metrics (`warmrunners_activity_floor`, `warmrunners_activity_jobs_total`, `warmrunners_activity_bot_filtered_total`); `Active` printer column.
  - **Changed** — floor formula now `max(scheduleBase, predictedContrib, activityContrib)`.
  - **Removed** — `queueRule.headroom` (deleted: it double-counted ARC's reactive scaling; see commit `fix(scheduler): remove bestHeadroom`); `HeadroomTier` CRD type gone.
  - Update the link refs at the bottom for `[Unreleased]` / `[0.3.0]`.
- [ ] **Step 3.** Commit: `docs: README and CHANGELOG for v0.3.0`.

---

## Task 12: PR 2 — open, review, merge

- [ ] **Step 1.** Push: `git push -u origin feat/wire-activity`.
- [ ] **Step 2.** Open PR to main. Title: `feat: wire activity-driven warm-floor (v0.3.0)`. Body: short — what + verified manual kind exercise + CHANGELOG link.
- [ ] **Step 3.** Wait for CI green. Fix any flake / regression on the branch.
- [ ] **Step 4.** Squash-merge with subject `feat: wire activity-driven warm-floor (v0.3.0)`.
- [ ] **Step 5.** Sync main; delete the merged branch.

---

## Task 13: Tag and release

- [ ] **Step 1.** On `main`, finalize CHANGELOG date (replace `<date-at-tag-time>` placeholder from Task 11 with today's date) if it wasn't done at squash time. Commit straight to main: `docs: finalize 0.3.0 changelog date` (or amend into the doc PR if it hasn't merged yet).
- [ ] **Step 2.** `go build ./...` — final sanity.
- [ ] **Step 3.** Tag: `git tag -s v0.3.0 -m "v0.3.0"`; verify with `git tag -v v0.3.0`; push: `git push origin v0.3.0`.
- [ ] **Step 4.** Watch the Release workflow. Expect: cosign keyless image sign + cosign chart sign + SPDX SBOM generation + cosign attest SBOM. All four claims should validate anon — same verification commands as v0.2.1.
- [ ] **Step 5.** Anon-verify: `cosign verify` against the image and chart; `cosign verify-attestation` against the SBOM. All three return "claims were validated".
- [ ] **Step 6.** Update `~/gh/ideas/warmrunners-next-steps.md` state line to "v0.3.0 shipped, signed, verified."
- [ ] **Step 7.** Spot-check the release page on github.com: latest = v0.3.0; PR titles for PR 1 + PR 2 visible under "What's Changed"; install.yaml + SBOM attached; "Full Changelog" link works.

---

## Self-review notes

- **Spec coverage.** §1 identity → conceptual. §2 signal → Task 5 (sampler implementation). §3.1 architecture → Tasks 4–9. §3.2 reconciler integration → Task 8. §3.3 bug fix → Tasks 2 + 3. §3.4 window → Task 7 (CRD defaults + Min/Max markers) + Task 8 (reconciler substitutes default when nil/0). §3.5 bot filter → Task 4 (`IsBotActor` + `BuiltinDenylist`). §3.6 magnitude → Task 5 (uses existing `WorkflowFetcher` + `workflow.Parse`). §3.7 CRD additions → Task 7. §3.8 metrics → Task 8. §4 quota → covered by ETag-cached fetch (shared with predictor) + `runsCap` (Task 5). §5 error handling → Tasks 5 + 8 (per-run drop / condition flag). §6 testing → Tasks 4/5/8 unit + integration + Task 10 manual kind. §7 observability → Task 8 metrics. §8 compatibility → Task 7 defaults + Task 3 example update. §9 layout → Tasks 4/5 create the package; Tasks 2/3 delete the bug. §10 roadmap → no code. §11 open questions → resolved in spec; no plan ambiguity.
- **Placeholder scan.** No TBD / TODO / "implement later". The one explicit `<date-at-tag-time>` placeholder in Task 11 / Task 13 is a deliberate runtime fill — the date is unknown until the tag command runs and is documented as such.
- **Type consistency.** `Activity`, `Sample`, `WorkflowRunsSampler`, `IsBotActor`, `BuiltinDenylist`, `ActivityConfig`, `ActivityFloor`, `ActivityLabelSets`, `ActivityAvailable` — all match the spec verbatim and appear consistently across tasks.
- **PR count.** 2 PRs total, sequential (PR 2 depends on PR 1's package). Component commits inside each branch collapse to one squash entry on main per PR. Hot-fix PRs welcome between them per `CLAUDE.local.md` "at least N PRs, not exactly N".
