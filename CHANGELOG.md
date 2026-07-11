# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.0.0/); this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-07-08

### Added
- **Event-driven pre-warm**: new `GitHubApp` CRD (cluster-scoped) + a webhook
  receiver mounted at `/github/webhook` on the metrics server. When a policy
  references a `GitHubApp` via `spec.githubAppRef`, `push` and `workflow_job`
  events feed the activity signal within ~1s of arrival, instead of waiting
  for the next REST poll. HMAC-SHA256 verification, LRU replay guard
  (10 000 IDs / 24h), 1 MiB body cap.
- **Tunnel mode**: `GitHubApp.spec.ingress.mode: tunnel` opens an outbound
  WebSocket to a smee.io-compatible relay. Zero-ingress path for kind, laptops,
  and single-node clusters. Auto-reconnect with exponential backoff (500ms →
  30s cap, full jitter).
- **`WarmRunnerPolicy` fields**: `spec.githubAppRef`, `spec.activeWindowSeconds`
  (default 600, range 60–3600). Status: `activeUntil`, `lastEventSource`
  (`webhook`/`poll`).
- **Metrics**: `warmrunners_webhook_events_total`, `warmrunners_webhook_lag_seconds`,
  `warmrunners_webhook_deliveries_dropped_total`, `warmrunners_tunnel_connected`,
  `warmrunners_tunnel_reconnects_total`, `warmrunners_active_window_expiries_total`,
  `warmrunners_active_window_seconds_remaining`.
- `docs/webhook.md` covers ingress + tunnel setup, verification, poll fallback,
  and a troubleshooting matrix keyed on the dropped-events reason label.

### Changed
- When `spec.githubAppRef` is set, the default `queueRule.pollInterval` becomes
  300s (from 30s pre-v0.5). Poll continues as a safety net for webhook outages.
- REST polling code path unchanged for policies with no `githubAppRef`; existing
  v0.4 policies keep their prior behavior.

## [0.4.0] - 2026-06-17

### Added
- **Dry-run mode**: new `spec.dryRun` field (default `false`). When `true` every
  signal (demand, predictor, activity, scheduler, status, metrics) still runs but
  the controller never patches the backend's warm-floor field. Canary path for
  observing what would change before letting the controller act. Mirrored to
  `status.dryRun` and the `DryRun` condition; new
  `warmrunners_dry_run_skipped_patches_total{policy}` counter records skips.
- **GitHub rate-limit observability**: two new gauges
  `warmrunners_github_rate_limit_remaining{source}` and
  `warmrunners_github_rate_limit_reset_seconds{source}` populated from response
  headers on every demand poll and workflow fetch. `source` ∈ `demand`/`workflow`.
- `docs/rate-limits.md` covers quotas, cost math, and tuning.
- `docs/installation.md` covers Helm, raw kubectl, Flux `HelmRelease`,
  Argo CD `Application`, and source builds, with verification + upgrade +
  uninstall sections.
- `docs/runbook.md` operator playbook: stuck reconcile, GitHub auth failure,
  rate-limit exhaustion, controller OOM, leader fight, dry-run flip.
- `docs/upgrade.md` v0.3 → v0.4 migration walk-through.
- `examples/alerts.yaml` Prometheus `PrometheusRule` covering the new gauges,
  reconcile errors, and dry-run skip rate.
- `examples/pdb.yaml` PodDisruptionBudget for multi-replica deploys.
- `examples/netpol.yaml` NetworkPolicy restricting egress to the GitHub REST
  API and the kube-apiserver.

### Changed
- Leader election defaults to enabled. Multi-replica deploys are safe out of
  the box; pass `--leader-elect=false` for single-process local development
  (the bundled Helm chart and kustomize manager manifest no longer set the
  flag explicitly).

### Fixed
- Stop "Operation cannot be fulfilled" reconcile spam: `SetupWithManager` now
  uses `GenerationChangedPredicate` so own `Status().Update` events do not
  schedule another reconcile against a stale informer cache, and `setCondition`
  delegates to `meta.SetStatusCondition` so `LastTransitionTime` only advances
  on a real True/False transition. Pre-existing since v0.1.0; caught by the
  v0.4.0 kind exercise.

### Build / tooling
- Bump `golangci-lint` to `v2.12.2` (Go 1.25 compatible).

## [0.3.0] - 2026-05-28

### Added
- **Activity-driven warm-floor signal**: while a repo has recent non-bot
  `workflow_run`s in the last 15 min (configurable), the floor is sized to the
  actual matrix fanout of those workflows. Quiet repo → floor drops to 0 so no
  warm capacity is paid for during off-hours. Bot filter covers `actor.type`,
  `triggering_actor.type`, `[bot]` login suffix, and a built-in + user-appendable
  denylist (Dependabot, Renovate, GitHub Actions, Mergify, Codecov, Copilot,
  self-hosted Renovate).
- New CRD fields: `spec.activity.{enabled, windowSeconds, botLoginDenylist}`
  with sensible defaults (true / 900 / built-in only).
- New status fields: `status.activityFloor`, `status.activityLabelSets`
  (top-8 by count), and `ActivityAvailable` condition.
- New printer column: `Active`.
- New metrics: `warmrunners_activity_floor{policy}`,
  `warmrunners_activity_jobs_total{policy,labels}`,
  `warmrunners_activity_bot_filtered_total{reason}`.

### Changed
- Floor formula folds the activity signal as a third `max(...)` candidate:
  `desiredFloor = clamp(max(scheduleBase, predictedContribution, activityContribution), floor.min, min(floor.max, backendMax))`.

### Removed
- `spec.queueRule.headroom` and the `HeadroomTier` CRD type — deleted outright
  (the project is `v1alpha1` with no known production users). The old field
  double-counted ARC's reactive scaling: with `Headroom: [{whenQueueAtLeast: 5,
  addRunners: 3}]` and 5 jobs queued, ARC requested 5 runners reactively AND
  warmrunners set `minRunners: 8` — net 8 runners for 5 jobs, three idle. The
  activity signal replaces it with the right shape (size from parsed YAML,
  trigger from recent runs rather than queued counts).

## [0.2.1] - 2026-05-28

### Fixed
- Predictor authentication: `Predictor.Predict` and `WorkflowFetcher.Fetch` now
  take a `token` argument and set `Authorization: Bearer <token>` on every
  outbound GitHub REST request (with `strings.TrimSpace` defense against
  newline-in-secret bugs, mirroring the v0.1.x demand source). v0.2.0 shipped a
  non-functional predictor — the GitHub Actions REST API returns 404 to
  unauthenticated requests even on public repos, so the predictor's API calls
  always failed and the codebase-aware contribution was effectively zero.
  Verified end-to-end on real cluster + real `workflow_dispatch` run.

## [0.2.0] - 2026-05-28

### Added
- Codebase-aware `Predictor`: reads the `needs:` graph of active GitHub
  `workflow_run`s and pre-warms downstream runners while their upstream jobs are
  still running. Statically parses workflow YAML at the run's `head_sha`,
  follows local reusable workflows (depth 10, cycle-safe), expands literal
  matrices, and records dynamic forms (`${{ }}` expressions, `fromJSON`
  matrices, remote reusable workflows) as undecidable rather than guessing.
- New CRD fields: `spec.predictor.{enabled,workflowRefreshInterval,maxRunsPerPoll}`
  (defaults: `true`, `5m`, `50`). Existing v0.1.x policies validate unchanged.
- New status fields: `status.predictedFloor`, `status.predictedLabelSets`
  (top-8 by count), and `PredictorAvailable` condition.
- New printer column: `Predicted`.
- New metrics: `warmrunners_predicted_floor{policy}`,
  `warmrunners_predicted_jobs_total{policy,labels}`,
  `warmrunners_workflow_yaml_fetch_total{result}`.
- ETag-conditional + LRU-cached workflow-YAML fetcher honors `Retry-After`
  and `X-RateLimit-Reset` (reuses the v0.1.1 poller's patterns).

### Changed
- Floor formula folds the predictor's contribution via `max(...)`:
  `desiredFloor = clamp(max(scheduledFloor, predictedContrib), floor.min, min(floor.max, backendMax))`.
  When the Predictor is disabled or errors, behavior reduces to v0.1.x exactly.

### Dependencies
- Added `github.com/rhysd/actionlint` (MIT) for typed workflow-YAML parsing.

## [0.1.1] - 2026-05-27

### Added
- CRD: `Age` printer column, `wrp` short name, `warmrunners` category.
- CRD validation: CEL rules (`floor.min <= floor.max`, schedule `from < to`),
  HH:MM and timezone patterns, `MaxLength` bounds on string fields.
- `--max-concurrent-reconciles`, `--log-level`, and `--github-http-timeout` flags.
- GitHub poller: `User-Agent` header, ETag conditional requests, retry with
  backoff honoring `Retry-After` and `X-RateLimit-Reset`.
- Metrics: `warmrunners_build_info`, `warmrunners_reconciliation_errors_total`.
- RBAC aggregation labels (`aggregate-to-view`/`edit`/`admin`).
- Race detector in `make test`; `govulncheck` CI gate.
- `SECURITY.md` and release-verification instructions in the README.

### Changed
- `status.conditions` uses `listType=map` semantics and carries `observedGeneration`.
- `LeaderElectionReleaseOnCancel` enabled for faster leader handoff.

### Fixed
- GitHub poller error handling: surface request-build errors; honor context
  cancellation during backoff.
- Removed cert-manager TODO scaffold from `cmd/main.go`.

### Security
- Release image and Helm chart signed with Sigstore cosign (keyless OIDC).
- SPDX-JSON SBOM generated and attested to the image.

## [0.1.0] - 2026-05-27

### Added
- `WarmRunnerPolicy` CRD and Kubernetes operator (kubebuilder / controller-runtime).
- ARC adapter — patches `AutoscalingRunnerSet.spec.minRunners`.
- GARM adapter — patches `Pool.spec.minIdleRunners`.
- Schedule + queue-depth-headroom scheduler with a decrease cooldown.
- GitHub REST demand source with label-aware job counting and per-policy token auth.
- Prometheus metrics (`warmrunners_desired_floor`, `_applied_floor`, `_queue_depth`, `_floor_change_total`).
- Helm chart, published to GHCR as an OCI artifact.
- Multi-arch container image (linux/amd64, linux/arm64) on GHCR.

[Unreleased]: https://github.com/sarataha/warmrunners/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/sarataha/warmrunners/compare/v0.4.0...v0.5.0
[0.3.0]: https://github.com/sarataha/warmrunners/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/sarataha/warmrunners/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/sarataha/warmrunners/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/sarataha/warmrunners/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/sarataha/warmrunners/releases/tag/v0.1.0
