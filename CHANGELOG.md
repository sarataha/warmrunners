# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.0.0/); this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/sarataha/warmrunners/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/sarataha/warmrunners/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/sarataha/warmrunners/releases/tag/v0.1.0
