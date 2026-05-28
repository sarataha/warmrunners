# warmrunners

[![Tests](https://github.com/sarataha/warmrunners/actions/workflows/test.yml/badge.svg)](https://github.com/sarataha/warmrunners/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/sarataha/warmrunners)](https://github.com/sarataha/warmrunners/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/sarataha/warmrunners)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A Kubernetes operator that keeps self-hosted GitHub Actions runners warm only
when they will be needed. Sits on top of [ARC](https://github.com/actions/actions-runner-controller)
or [GARM](https://github.com/cloudbase/garm) and patches their warm-floor field
(`minRunners` / `minIdleRunners`) so multi-stage pipelines skip the downstream
cold-start and idle runners don't burn money overnight.

## How it works

One CRD, `WarmRunnerPolicy`. Each policy targets one backend pool. Three
signals contribute; the strongest wins:

```
desiredFloor = clamp(max(scheduleBase, predictorContribution, activityContribution),
                     floor.min, floor.max)
```

**Schedule** (v0.1.0). Hand-written time windows like `Mon–Fri 09:00–18:00 base 3`.

**Predictor** (v0.2.0). Parses each active `workflow_run`'s YAML at `head_sha`
and walks the `needs:` graph. While an upstream job is still running, the
predictor pre-warms the downstream pool (e.g. GPU). GitHub does not materialize
`needs:`-blocked jobs until upstream completes, so reactive autoscalers cannot
anticipate them.

**Activity** (v0.3.0). While the repo has recent non-bot CI activity in the
last 15 minutes (configurable), the floor is sized to the matrix fanout of the
workflows being triggered. Quiet repo, floor drops to 0. The bot filter covers
Dependabot, Renovate, GitHub Actions, and PAT-driven machine users.

Each signal is independently togglable. Floor decreases are rate-limited by a
cooldown. The controller never deletes runners; it only moves the floor and
lets the backend drain naturally.

See [`examples/`](examples/) for complete ARC and GARM policies.

## Install

```sh
helm install warmrunners oci://ghcr.io/sarataha/charts/warmrunners --version 0.3.0
```

Then create a `Secret` with a GitHub token and a `WarmRunnerPolicy` (see [`examples/`](examples/)).

## Backends

* **ARC**: patches `AutoscalingRunnerSet.spec.minRunners`.
* **GARM**: patches `Pool.spec.minIdleRunners`.

Prometheus metrics are exposed at `:8080/metrics`. See [`docs/metrics.md`](docs/metrics.md)
for the full list.

## Security

Releases are signed with [cosign](https://github.com/sigstore/cosign) (keyless OIDC)
and ship an attested SPDX SBOM. See [`docs/security.md`](docs/security.md) for
verification commands. Report vulnerabilities via [SECURITY.md](SECURITY.md).

## Roadmap

* **v0.4.0**: validating admission webhook (cross-policy conflict detection); richer queue rules.
* **later**: extended predictor mechanisms (`workflow_run` chains, `environment` approval gates); forecasting from historical job data.

## Contributing

Issues and PRs welcome. Discussion happens in the
[GitHub Discussions](https://github.com/sarataha/warmrunners/discussions) tab.

## License

MIT, see [LICENSE](LICENSE).
