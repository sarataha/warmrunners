# warmrunners — v1 Design

**Status:** Approved (2026-05-27) — ready for implementation planning
**Author:** Sara
**Repo:** github.com/sarataha/warmrunners

## 1. Identity

**warmrunners** is a predictive warm-floor controller for self-hosted GitHub Actions runners.
It plugs into existing runner managers (ARC and GARM in v1) and adjusts the warm-floor knob
(`AutoscalingRunnerSet.minRunners` for ARC, `Pool.minIdleRunners` for GARM) based on a
configured schedule plus observed GitHub job queue depth.

v1 is heuristic: clock + queue-depth headroom. Forecasting from historical data is v2.

### Non-goals

- Not a generic Kubernetes autoscaler — only self-hosted GitHub Actions runners.
- Not a replacement for ARC or GARM — it sits on top of them.
- No webhook receiver in v1 (REST polling only).
- No codebase-aware mapping in v1 (no parsing of `.github/workflows/*` to infer paths→label
  mapping — that is v0.2.0).
- No machine learning in v1.
- No runner deletion. The controller only adjusts the warm-floor; backends drain naturally.

## 2. Architecture

A `kubebuilder`-based Kubernetes operator with one CRD: `WarmRunnerPolicy`. Users declare a
policy that points at exactly one backend target (an ARC `AutoscalingRunnerSet` or a GARM
`Pool`), plus a schedule and a queue-depth rule.

The reconciler polls the GitHub REST API for queue state at `pollInterval`, evaluates
`desiredFloor = clamp(scheduleBase + queueHeadroom, floor.min, floor.max)`, and patches the
backend CR via a pluggable `Adapter`. Outside any scheduled window, `scheduleBase` is `0`.
`queueHeadroom` is `0` when no tier matches. Each reconcile changes at most one field
on one CR. Floor decreases are rate-limited by a `cooldown` to prevent churn.

```
+--------------------+         +-----------------+
| WarmRunnerPolicy   |  <--->  |  Reconciler     |
| (CRD, user-owned)  |         |  (every poll)   |
+--------------------+         +--------+--------+
                                        |
                  +---------------------+----------------------+
                  |                     |                      |
            +-----v-----+         +-----v-----+         +------v------+
            |DemandSource|        |Scheduler  |         |   Adapter   |
            | (GitHub    |        |(clock +   |         |  (ARC/GARM) |
            |  REST poll)|        | rules)    |         |             |
            +-----------+         +-----------+         +------+------+
                                                               |
                                                  +------------v-------------+
                                                  | Backend CR (3rd party)   |
                                                  |  AutoscalingRunnerSet OR |
                                                  |  Pool                    |
                                                  +--------------------------+
```

Three components sit behind interfaces, all swappable:

- `DemandSource` — v1: `GitHubRESTPoller`. v2: webhook receiver. Same interface.
- `Scheduler` — v1: clock + queue rule. v2: forecaster. Same interface.
- `Adapter` — v1: `ArcAdapter`, `GarmAdapter`. v2+: e.g. `TerraformAwsAdapter`. Same interface.

This is the key extensibility contract — switching a `DemandSource` or adding an `Adapter`
must not require changes to the reconciler.

## 3. CRD: `WarmRunnerPolicy` (v1alpha1)

A `WarmRunnerPolicy` targets exactly one backend (`target.arc` or `target.garm`, mutually
exclusive — validated by the CRD schema and a defensive runtime check).

```yaml
apiVersion: autoscaling.warmrunners.io/v1alpha1
kind: WarmRunnerPolicy
metadata:
  name: example-arc
spec:
  github:
    owner: my-org
    repository: my-repo           # required (v1 polls repo-level runs)
    labels: [self-hosted, linux, x64]
    auth:
      secretRef: { name: gh-token, key: token }

  target:
    arc:
      runnerSet:
        name: prod-runners
        namespace: arc-system

  floor:
    min: 0                        # absolute floor when nothing else applies
    max: 50                       # safety cap; controller never sets above this

  schedule:                       # outside any window → scheduleBase = 0 (desired still clamped to floor.min)
    - days: [Mon, Tue, Wed, Thu, Fri]
      from: "08:00"
      to:   "19:00"
      tz:   "UTC"
      base: 3

  queueRule:
    pollInterval: 30s
    headroom:                     # additive on top of scheduleBase, taken at the highest matching tier
      - whenQueueAtLeast: 5
        addRunners: 3
      - whenQueueAtLeast: 15
        addRunners: 8
    cooldown: 2m                  # floor cannot DECREASE more than once per cooldown

status:
  desiredFloor: 6
  appliedFloor: 6
  lastQueueDepth: 12
  lastReconcileTime: "2026-05-27T10:00:00Z"
  conditions:
    - type: DemandSourceAvailable
      status: "True"
    - type: AdapterAvailable
      status: "True"
```

GARM target example (mutually exclusive with `arc`):

```yaml
target:
  garm:
    pool:
      name: gcp-runner-m
      namespace: garm-operator-system
```

Multiple policies can target different pools. Conflicting policies on the *same* backend CR are
not guarded in v0.1.0 (last writer wins); see §6.

## 4. Reconcile flow

1. Read `WarmRunnerPolicy`.
2. `DemandSource.CurrentDemand(labels)` → `DemandSnapshot{ queued, running }` from GitHub REST.
3. `Scheduler.Decide(spec, now, demand)` → `desiredFloor = clamp(scheduleBase + queueHeadroom, floor.min, floor.max)`.
4. Read current backend floor via `Adapter.GetFloor(target)`.
5. If `desiredFloor != currentFloor`:
   - If `desiredFloor < currentFloor` and within `cooldown` window → skip (preserve drain).
   - Else `Adapter.SetFloor(target, desiredFloor)`.
6. Update `status` (`desiredFloor`, `appliedFloor`, `lastQueueDepth`, conditions).
7. Requeue at `pollInterval`.

Headroom selection: when multiple `queueRule.headroom` tiers match, the **highest** one wins
(deterministic, monotonic with queue depth).

## 5. Error handling and safety

- **GitHub API failure** — do not change floor. Surface `DemandSourceAvailable=False`
  condition. Last applied floor stays in place.
- **Backend patch failure** — surface `AdapterAvailable=False` condition; the reconcile
  returns an error so controller-runtime requeues with backoff.
- **Auth missing or invalid** — `DemandSourceAvailable=False`; no patches attempted.
- **Conflicting policies targeting the same backend CR** — not handled in v0.1.0 (last writer
  wins). Planned for v0.3.0: a validating admission webhook to reject conflicts at apply time.
- **Manual drift** — if a human edits the backend CR's floor field directly between
  reconciles, next reconcile re-applies the desired value. This is documented behavior, not
  a bug.
- **`floor.max` is a hard safety cap.** The controller never sets a floor above it, under
  any rule combination.
- **Natural drain only** — the controller never deletes a runner. It only lowers the floor
  field; ARC/GARM handle the actual drain. The `cooldown` further smooths drops.

## 6. Testing strategy

- **Unit**
  - `Scheduler`: table-driven tests across schedule windows, queue tiers, edge clock cases
    including DST transitions, `min`/`max` clamping, and cooldown logic.
  - `Adapter`: patch payload correctness using `fake.NewClientBuilder()` for each backend CR
    shape. Idempotency.
  - `DemandSource`: against `httptest.Server` simulating GitHub REST.
- **Integration**
  - `envtest` running the CRD + a fake ARC/GARM-shaped CRD + a stub GitHub REST server +
    the real reconciler. End-to-end loop, including failure paths.
- **Local e2e**
  - `kind` cluster + minimal ARC-shaped CRD + a tiny GitHub repo with cron-triggered
    workflows → observe floor changes against expected schedule. Documented in `examples/`
    so contributors can reproduce.
- **TDD enforced** per superpowers methodology — every task is RED → GREEN → REFACTOR.

## 7. Observability

Prometheus metrics, exported by the controller on the standard `:8080/metrics` endpoint:

- `warmrunners_desired_floor{policy,target}` (gauge)
- `warmrunners_applied_floor{policy,target}` (gauge)
- `warmrunners_queue_depth{policy}` (gauge)
- `warmrunners_floor_change_total{policy,direction=up|down}` (counter)

Error states surface as CR status conditions (`DemandSourceAvailable`, `AdapterAvailable`)
rather than counters. Structured JSON logs via `controller-runtime`'s logger.

## 8. Install and packaging

- Local dev: `make deploy` against the current kubecontext.
- Helm chart at `dist/chart`, published to GHCR as an OCI artifact:
  `helm install warmrunners oci://ghcr.io/sarataha/charts/warmrunners --version <version>`.
- Single-file install: the `install.yaml` attached to each GitHub release.
- Container image: `ghcr.io/sarataha/warmrunners:<tag>` (multi-arch: linux/amd64, linux/arm64).

## 9. Repository layout (post-`kubebuilder init`)

```
warmrunners/
  api/v1alpha1/                 # CRD types (generated + hand-written validators)
  internal/
    controller/                 # reconciler
    demand/                     # DemandSource implementations (GitHubRESTPoller)
    scheduler/                  # decision logic (clock + queue headroom)
    adapter/                    # ArcAdapter, GarmAdapter
  config/                       # kubebuilder manifests (CRDs, RBAC, deployment)
  dist/chart/                   # Helm chart (generated by the kubebuilder helm plugin)
  docs/superpowers/specs/       # this spec and future ones
  examples/                     # sample WarmRunnerPolicy YAML, Grafana dashboard
  Makefile
  README.md
```

## 10. Roadmap

The CRD is `v1alpha1` and the API is not yet stable, so releases stay in the `0.x` line (each
feature bumps the minor). `v1.0.0` is reserved for when the CRD graduates to `v1` and the API is
committed-stable — not before. Each feature ships as its own minor release, not bundled.

- **v0.1.0** (shipped) — `ArcAdapter` + `GarmAdapter`, `WarmRunnerPolicy` v1alpha1, REST-poll
  `DemandSource`, clock + queue-headroom `Scheduler`, Prometheus metrics, Helm chart.
- **v0.2.0** — Codebase-aware: parse the user's `.github/workflows/*.yml` to discover the
  paths→`runs-on` label mapping automatically and pre-warm by runner type. The first feature that
  meaningfully differentiates from a cron + `kubectl patch`. (Prioritized ahead of the webhook —
  ordered by value, not by sequence.)
- **v0.3.0** — Validating admission webhook for cross-policy conflict detection; richer
  `queueRule` shapes (e.g. per-label headroom).
- **later** — Forecasting `Scheduler`: time-series over historical job data, per-window
  predictions. Webhook receiver as an alternative `DemandSource`. Possible `TerraformAwsAdapter`.
- **v1.0.0** — when `WarmRunnerPolicy` graduates `v1alpha1` → `v1` (API stability promise).

## 11. Open questions deferred to implementation

- Exact CRD field names and validation rules — will be finalized during `kubebuilder` type
  generation.
- Concrete GitHub REST endpoints and pagination strategy for fetching queued/in-progress
  job counts at the label level — to be confirmed in the first implementation task.
- Initial Helm chart values shape — defaults vs. required.
