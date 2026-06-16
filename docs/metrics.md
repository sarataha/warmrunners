# warmrunners metrics

All metrics are exposed on the controller's `:8080/metrics` endpoint in
Prometheus text format. The endpoint requires the kube-rbac-proxy auth
flow when `--metrics-secure=true` (the default); use a ServiceMonitor or
scrape the endpoint via a ServiceAccount with `nonResourceURL /metrics`
access.

## Core (v0.1.0)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `warmrunners_desired_floor` | Gauge | `policy` | Last computed `desiredFloor` per policy. |
| `warmrunners_applied_floor` | Gauge | `policy` | Floor value last patched onto the backend CR. |
| `warmrunners_queue_depth` | Gauge | `policy` | Observed GitHub queue depth for the policy's labels. |
| `warmrunners_floor_change_total` | Counter | `policy,direction` | Floor change events. `direction` ∈ `up`/`down`. |

## Build & errors (v0.1.1)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `warmrunners_build_info` | Gauge | `version,commit,build_date` | Constant `1`; labels carry the build identity. |
| `warmrunners_reconciliation_errors_total` | Counter | `policy,error_type` | Reconcile errors by failure mode. |

## Codebase-aware Predictor (v0.2.0)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `warmrunners_predicted_floor` | Gauge | `policy` | Predictor's contribution to `desiredFloor`. |
| `warmrunners_predicted_jobs_total` | Gauge | `policy,labels` | Per-label-set imminent job count from the Predictor. Stale label sets pruned across reconciles. |
| `warmrunners_workflow_yaml_fetch_total` | Counter | `result` | Workflow YAML fetch outcomes. `result` ∈ `fetched`/`error`/`dynamic_skipped`. |

## Activity sampler (v0.3.0)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `warmrunners_activity_floor` | Gauge | `policy` | Activity sampler's contribution to `desiredFloor`. |
| `warmrunners_activity_jobs_total` | Gauge | `policy,labels` | Per-label-set job count from the Activity sampler. Pruned across reconciles. |
| `warmrunners_activity_bot_filtered_total` | Counter | `reason` | Bot-filtered `workflow_run`s. `reason` ∈ `bot_type`/`trigger_bot_type`/`bot_suffix`/`denylist`. |

## Dry-run (v0.4.0)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `warmrunners_dry_run_skipped_patches_total` | Counter | `policy` | Backend patches skipped because `spec.dryRun` was true. Watch this during canary to confirm the controller would have acted. |

## GitHub rate limit (v0.4.0)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `warmrunners_github_rate_limit_remaining` | Gauge | `source` | Last `X-RateLimit-Remaining` observed on a GitHub REST response. `source` ∈ `demand`/`workflow`. |
| `warmrunners_github_rate_limit_reset_seconds` | Gauge | `source` | Last `X-RateLimit-Reset` (unix seconds). Pair with `time()` to estimate seconds until the quota window resets. |

See [`docs/rate-limits.md`](rate-limits.md) for tuning guidance.

## Useful queries

`desiredFloor` not reaching `appliedFloor`:
```promql
warmrunners_desired_floor - warmrunners_applied_floor
```

Which signal is driving the floor for a policy:
```promql
max by (policy) (warmrunners_predicted_floor, warmrunners_activity_floor)
```

Bot noise being filtered:
```promql
rate(warmrunners_activity_bot_filtered_total[5m])
```

Predictor fetch hit-rate (cached vs cold):
```promql
sum by (result) (rate(warmrunners_workflow_yaml_fetch_total[5m]))
```
