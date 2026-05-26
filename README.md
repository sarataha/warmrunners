# warmrunners

Predictive warm-floor controller for self-hosted GitHub Actions runners.

Reactive autoscalers (ARC, GARM) wait for jobs to queue before scaling — so
the first build of the morning still cold-starts, and 3 a.m. runners sit
warm for nothing. warmrunners watches GitHub demand and adjusts the warm-floor
ahead of time, based on a schedule and a queue-depth rule.

## Status

**Pre-implementation — design phase, May 2026.**

- Design: [`docs/superpowers/specs/2026-05-27-warmrunners-design.md`](docs/superpowers/specs/2026-05-27-warmrunners-design.md)
- Implementation plan: [`docs/superpowers/plans/2026-05-27-warmrunners-v1.md`](docs/superpowers/plans/2026-05-27-warmrunners-v1.md)

Code starts next. No installable release yet.

## Roadmap

- **v1.0** — ARC and GARM adapters, schedule + queue-depth heuristic, Helm chart.
- **v1.5** — Codebase-aware: discover paths-to-runner-label mapping from the user's `.github/workflows/*`.
- **v2.0** — Forecasting from historical job data. Webhook-based demand source.

## License

MIT (to be added at v1.0).
