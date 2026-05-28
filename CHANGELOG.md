# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.0.0/); this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/sarataha/warmrunners/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/sarataha/warmrunners/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/sarataha/warmrunners/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/sarataha/warmrunners/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/sarataha/warmrunners/releases/tag/v0.1.0
