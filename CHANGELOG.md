# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.0.0/); this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0]

### Added
- `WarmRunnerPolicy` CRD and Kubernetes operator (kubebuilder / controller-runtime).
- ARC adapter — patches `AutoscalingRunnerSet.spec.minRunners`.
- GARM adapter — patches `Pool.spec.minIdleRunners`.
- Schedule + queue-depth-headroom scheduler with a decrease cooldown.
- GitHub REST demand source with label-aware job counting and per-policy token auth.
- Prometheus metrics (`warmrunners_desired_floor`, `_applied_floor`, `_queue_depth`, `_floor_change_total`).
- Helm chart, published to GHCR as an OCI artifact.
- Multi-arch container image (linux/amd64, linux/arm64) on GHCR.

[Unreleased]: https://github.com/sarataha/warmrunners/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/sarataha/warmrunners/releases/tag/v1.0.0
