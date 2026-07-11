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

One CRD, `WarmRunnerPolicy`, per backend pool. The floor is the max of three
signals; the strongest wins.

* **Schedule**: time windows like `Mon–Fri 09:00–18:00 base 3`.
* **Predictor**: reads each active `workflow_run`'s YAML at `head_sha` and pre-warms downstream pools while upstream jobs are still running. The only GHA autoscaler that does this; reactive ones see `needs:`-blocked jobs only after upstream completes.
* **Activity**: while the repo has non-bot CI activity in the last 15 min, the floor matches the matrix fanout of the triggered workflows. Quiet repo, floor drops to 0.

The activity signal is fed by REST polling by default. Point a policy at a
`GitHubApp` CR to switch it to a webhook receiver — floor bumps within ~1s of
push, instead of waiting for the next poll. See
[`docs/webhook.md`](docs/webhook.md).

### What that saves

For a repo running self-hosted runners with `minRunners: 0`, first-CI wait is
`poll gap + runner boot`. Poll gap is up to `pollInterval` (30s default);
runner boot is 15–30s. Webhook mode cuts the poll gap to ~1s, so the first
push in a quiet period drops from **~45–60 s to ~16–31 s**. Every subsequent
push inside the rolling activity window (default 10 min) hits an already-idle
runner and starts in ~2 s. Across a busy dev session that adds up: ~15–30 s
per push saved on average, and the "wait, why is CI slow?" moment on the
first commit largely goes away.

Decreases are rate-limited by a cooldown. The controller never deletes runners.

See [`examples/`](examples/) for full policies.

## Install

```sh
helm install warmrunners oci://ghcr.io/sarataha/charts/warmrunners \
  --version 0.3.0 --namespace warmrunners-system --create-namespace
```

Then create a `Secret` with a GitHub token and a `WarmRunnerPolicy` (see [`examples/`](examples/)).

Other methods (raw YAML, Flux, Argo CD, source) and a verification walkthrough
live in [`docs/installation.md`](docs/installation.md).

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

Issues and PRs welcome. Open an [issue](https://github.com/sarataha/warmrunners/issues)
to report a bug, propose a feature, or ask a question.

## License

MIT, see [LICENSE](LICENSE).
