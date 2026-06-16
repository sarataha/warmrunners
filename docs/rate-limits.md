# GitHub API rate limits

warmrunners makes two kinds of GitHub REST calls per `WarmRunnerPolicy`:

1. **Demand polling.** Once per `spec.queueRule.pollInterval` (default 30s),
   it lists queued workflow runs to compute the policy's queue depth.
2. **Workflow fetching.** The codebase-aware Predictor (v0.2.0) and Activity
   sampler (v0.3.0) fetch each active workflow's YAML at its `head_sha`.
   Responses are ETag-cached, so steady-state fetches return `304 Not
   Modified` and cost a fraction of the quota of a fresh fetch.

Both kinds run under the GitHub token attached to the policy's
`auth.secretRef`. The token's quota applies per authenticated user / App
installation.

## Quotas

| Token type | Per-hour limit |
|---|---|
| Personal access token (classic or fine-grained) | 5,000 |
| GitHub App installation | 5,000 (15,000 for Enterprise Cloud accounts) |
| Unauthenticated | 60 (do not use) |

## What warmrunners costs

A single policy in steady state costs roughly:

- `3600 / pollInterval` demand calls per hour (120 at 30s).
- 1–N workflow fetches per active `workflow_run`. ETag hits are billed at
  full quota cost only on first fetch; subsequent 304s also count.

So a busy policy with 30s polling and ~10 active workflows uses on the
order of a few hundred requests per hour — well under the 5,000 cap.

Sharing one token across many policies multiplies the cost. At 50 policies
the demand poll alone is `50 × 120 = 6,000/hr`, over the cap.

## When you are near the limit

The poller already honors GitHub's documented retry semantics: a `403`
with `X-RateLimit-Remaining: 0` waits until `X-RateLimit-Reset` before
retrying, and a `429` with `Retry-After` honors that header.

When the quota is exhausted the controller does not patch the backend —
it holds the last-known floor. This is the same safety rule that applies
to a `DemandSource` error.

## Observability

Two gauges record the last header values seen:

- `warmrunners_github_rate_limit_remaining{source}` — `source` is
  `demand` for the queue poller and `workflow` for the YAML fetcher.
- `warmrunners_github_rate_limit_reset_seconds{source}` — unix seconds.

Useful queries:

```promql
# Headroom by source
warmrunners_github_rate_limit_remaining

# Seconds until the quota window resets
warmrunners_github_rate_limit_reset_seconds - time()

# Alert: less than 500 requests left in the current window
min by (source) (warmrunners_github_rate_limit_remaining) < 500
```

## Tuning

If you run many policies against one token, lower the call rate before
raising the quota:

- Increase `spec.queueRule.pollInterval`. Going from 30s to 60s halves
  the demand cost.
- Lower `spec.predictor.maxRunsPerPoll` (default 50) so the Predictor
  fetches fewer workflows per cycle.
- Split the token: one GitHub App installation per repository or per
  team. Each installation gets its own quota.

If you cannot lower the rate, switch the policy's `auth.secretRef` to a
GitHub App installation token on a GitHub Enterprise Cloud account — the
quota is 15,000/hr there.
