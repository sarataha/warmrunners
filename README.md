# warmrunners

[![Tests](https://github.com/sarataha/warmrunners/actions/workflows/test.yml/badge.svg)](https://github.com/sarataha/warmrunners/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/sarataha/warmrunners)](https://github.com/sarataha/warmrunners/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/sarataha/warmrunners)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Predictive warm-floor controller for self-hosted GitHub Actions runners.

Reactive autoscalers ([ARC](https://github.com/actions/actions-runner-controller),
[GARM](https://github.com/cloudbase/garm)) wait for jobs to queue before scaling — so multi-stage
pipelines pay the cold-start of the downstream pool (GPU, large VMs) every time, and runners sit
warm overnight even when no one's pushing. warmrunners watches the GitHub Actions API and adjusts
the warm-floor **ahead of time**, from a schedule plus the codebase-aware predictor (v0.2.0) plus
recent CI activity (v0.3.0).

## How it works

A Kubernetes operator with one CRD, `WarmRunnerPolicy`. You point a policy at one backend
(an ARC `AutoscalingRunnerSet` or a GARM `Pool`) and declare a schedule. The controller polls
the GitHub REST API and sets the backend's warm-floor field (`minRunners` for ARC,
`minIdleRunners` for GARM) to:

```
desiredFloor = clamp(max(scheduleBase, predictedContribution, activityContribution),
                     floor.min, floor.max)
```

Three signals contribute; the strongest wins:

- **Schedule** (v0.1.0): hand-written time windows like "weekdays 9-6 keep 3 warm".
- **Predictor** (v0.2.0): reads the `needs:` graph of active `workflow_run`s. While a job's
  upstream is still in flight, GitHub hasn't queued the downstream job yet — but warmrunners
  parses the workflow YAML at `head_sha` and pre-warms the downstream pool (e.g. GPU). The
  only GitHub Actions autoscaler that does this.
- **Activity** (v0.3.0): when the repo has recent non-bot CI activity, the floor is sized to
  the actual matrix fanout of the workflows being run. Quiet repo → floor drops to 0 so no
  warm capacity is paid for overnight or on weekends.

Each signal is independently togglable via `spec.{schedule,predictor.enabled,activity.enabled}`.
Floor decreases are rate-limited by a cooldown. The controller never deletes runners — it only
moves the floor; the backend drains naturally.

```yaml
apiVersion: autoscaling.warmrunners.io/v1alpha1
kind: WarmRunnerPolicy
metadata: { name: example }
spec:
  github:
    owner: my-org
    repository: my-repo
    labels: [self-hosted, linux, x64]
    auth: { secretRef: { name: gh-token, key: token } }
  target:
    arc: { runnerSet: { name: prod-runners, namespace: arc-system } }
  floor: { min: 0, max: 50 }
  schedule:
    - days: [Mon, Tue, Wed, Thu, Fri]
      from: "08:00"
      to:   "19:00"
      tz:   "UTC"
      base: 3
  queueRule:
    pollInterval: 30s
    cooldown: 2m
  activity:
    enabled: true
    windowSeconds: 900
    botLoginDenylist: []   # appended to the built-in list, not replacing it
```

More samples in [`examples/`](examples/) (ARC + GARM).

## Install

```sh
helm install warmrunners oci://ghcr.io/sarataha/charts/warmrunners --version 0.3.0
```

Then create a `Secret` with a GitHub token and a `WarmRunnerPolicy` (see [`examples/`](examples/)).

## Verifying releases

Release images and charts are signed with [cosign](https://github.com/sigstore/cosign)
(keyless, via GitHub OIDC). Verify before deploying:

```sh
cosign verify \
  --certificate-identity-regexp="^https://github.com/sarataha/warmrunners/.github/workflows/release.yml@refs/tags/v.*$" \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/sarataha/warmrunners:v0.3.0

cosign verify \
  --certificate-identity-regexp="^https://github.com/sarataha/warmrunners/.github/workflows/release.yml@refs/tags/v.*$" \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/sarataha/charts/warmrunners:0.3.0
```

Each image also carries an attested SPDX SBOM:

```sh
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp="^https://github.com/sarataha/warmrunners/.github/workflows/release.yml@refs/tags/v.*$" \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/sarataha/warmrunners:v0.3.0
```

## Backends

- **ARC** ([actions-runner-controller](https://github.com/actions/actions-runner-controller)) — `AutoscalingRunnerSet.spec.minRunners`.
- **GARM** ([cloudbase/garm](https://github.com/cloudbase/garm)) — `Pool.spec.minIdleRunners`.

Exposes Prometheus metrics (`warmrunners_desired_floor`, `_applied_floor`, `_queue_depth`).

## Roadmap

- **v0.4.0** — validating admission webhook (cross-policy conflict detection);
  richer queue rules.
- **later** — extended predictor mechanisms (`workflow_run` chains,
  `environment` approval gates); forecasting via rolling day-of-week × hour-of-day
  histogram; webhook-based demand source.

## Non-goals

- Not a generic Kubernetes autoscaler — self-hosted GitHub Actions runners only.
- Not a replacement for ARC or GARM — it sits on top.
- No runner deletion — floor adjustments only; backends drain naturally.

## License

MIT — see [LICENSE](LICENSE).
