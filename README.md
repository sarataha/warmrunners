# warmrunners

Predictive warm-floor controller for self-hosted GitHub Actions runners.

Reactive autoscalers ([ARC](https://github.com/actions/actions-runner-controller),
[GARM](https://github.com/cloudbase/garm)) wait for jobs to queue before scaling — so the first
build of the morning still cold-starts, and 3 a.m. runners sit warm for nothing. warmrunners watches
GitHub demand and adjusts the warm-floor **ahead of time**, from a schedule plus a queue-depth rule.

## How it works

A Kubernetes operator with one CRD, `WarmRunnerPolicy`. You point a policy at one backend
(an ARC `AutoscalingRunnerSet` or a GARM `Pool`) and declare a schedule + queue rule. The
controller polls the GitHub REST API for queue depth and sets the backend's warm-floor field
(`minRunners` for ARC, `minIdleRunners` for GARM) to:

```
desiredFloor = clamp(scheduleBase + queueHeadroom, floor.min, floor.max)
```

Floor decreases are rate-limited by a cooldown. The controller never deletes runners — it only
moves the floor; the backend drains naturally.

```yaml
apiVersion: warmrunners.io/v1alpha1
kind: WarmRunnerPolicy
metadata: { name: example }
spec:
  github:
    owner: my-org
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
    headroom:
      - { whenQueueAtLeast: 5,  addRunners: 3 }
      - { whenQueueAtLeast: 15, addRunners: 8 }
    cooldown: 2m
```

More samples in [`examples/`](examples/) (ARC + GARM).

## Install

```sh
helm install warmrunners ./dist/chart
```

Then create a `Secret` with a GitHub token and a `WarmRunnerPolicy` (see `examples/`).

## Status

**v1 — functional, CI-green (unit + envtest + e2e).** ARC is the primary, validated path; GARM is
supported through the same adapter interface. Live in-cluster validation against a real ARC runner
set is the next milestone before the tagged `v1.0.0` release.

## Roadmap

- **v1.0** — ARC + GARM adapters, schedule + queue-depth heuristic, Prometheus metrics, Helm chart. *(built)*
- **v1.5** — Codebase-aware: discover the paths-to-runner-label mapping from the user's `.github/workflows/*` and pre-warm by runner type.
- **v2.0** — Forecasting from historical job data; webhook-based demand source.

## Non-goals

- Not a generic Kubernetes autoscaler — self-hosted GitHub Actions runners only.
- Not a replacement for ARC or GARM — it sits on top.
- No runner deletion — floor adjustments only; backends drain naturally.

Design and implementation notes live in [`docs/superpowers/`](docs/superpowers/).

## License

MIT — see [LICENSE](LICENSE).
