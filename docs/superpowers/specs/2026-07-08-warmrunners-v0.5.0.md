# warmrunners — v0.5.0 Event-Driven Pre-Warm

**Status:** Draft (2026-07-08) — awaiting user review
**Author:** Sara
**Repo:** github.com/sarataha/warmrunners
**Predecessors:**
[v0.1.0 design](./2026-05-27-warmrunners-design.md) ·
[v0.1.1 polish](./2026-05-27-warmrunners-v0.1.1-polish.md) ·
[v0.2.0 codebase-aware](./2026-05-28-warmrunners-v0.2.0.md) ·
[v0.3.0 activity-driven](./2026-05-28-warmrunners-v0.3.0.md)

## 1. Identity

v0.5.0 collapses the reaction gap between a developer pushing code and warmrunners
knowing about it. Today's activity signal is fed by REST polling on a 30s interval; in
the worst case, a push waits the full poll interval before the floor bumps. v0.5.0
switches the primary signal to a GitHub App webhook receiver so `push` and
`workflow_job` events arrive in ~1s. Polling stays as a fallback path when the webhook
is unavailable (App outage, tunnel broken, first install of the day).

Combined with the existing activity window and predictor, the design pushes typical
"push → runner ready" latency in an active session down to the runner boot time itself
(~2s with ARC `minRunners` holding an idle pod) instead of the current ~30–60s worst
case dominated by poll gap.

The v0.3.0 spec explicitly listed "webhook receiver" as a non-goal, citing user infra
burden. v0.5.0 reverses that call: the GitHub App model plus optional tunnel mode
removes the "user must expose an ingress" barrier for private and single-node clusters,
and the poll fallback removes the "webhook outage = warmrunners blind" risk.

### Non-goals

- **Not** a runner-image pre-pull mechanism. That belongs to a later release if
  measured pull latency justifies it. Adapter `minRunners` / `minIdleRunners` already
  provide registered idle pods when the floor is nonzero.
- **Not** a rewrite of the activity or predictor code paths. Both continue to consume
  the same in-memory event stream; v0.5.0 changes the *producer* of events, not the
  consumer.
- **Not** cost telemetry. The savings-metric idea is roadmap for v0.6.
- **Not** a replacement for polling. Polling stays as fallback with a longer default
  interval (300s), acting as a safety net for webhook outages.

## 2. Events

The receiver subscribes to two event families on the GitHub App:

**`push`** — arrives when a developer pushes commits to any watched ref. Carries
`repository.full_name`, `ref`, `after` (head SHA). Used to:
1. Extend `activeUntil = now() + spec.activeWindowSeconds` (default 600s / 10 min).
2. Trigger a workflow YAML fetch at `after` to update predicted fanout (reuses
   `WorkflowFetcher` cache from v0.2.x).

**`workflow_job` (action=queued)** — arrives when GitHub schedules an individual job.
Carries `workflow_job.labels`, `workflow_job.run_id`, `repository.full_name`. Used to:
1. Feed the activity counter (`warmrunners_activity_jobs_total{labels=…}` — existing
   metric, now event-fed instead of poll-fed).
2. Confirm the fanout guess from the `push` event against reality; if the queued
   count exceeds prediction, bump floor to the actual count.

`workflow_run` events are ignored — coarser than `workflow_job` and superseded by it.

### 2.1 HMAC verification

Every request must include `X-Hub-Signature-256`. The receiver computes
`HMAC-SHA256(body, webhookSecret)` and compares in constant time. Missing or invalid
signature: 401, event dropped, `warmrunners_webhook_events_total{event,verified="false"}`
incremented.

### 2.2 Replay guard

Duplicate deliveries are common under GitHub retries. The receiver keeps an in-memory
LRU of the last 10k `X-GitHub-Delivery` UUIDs (24h TTL). Duplicates are 200-acked and
dropped without reprocessing.

## 3. Architecture

### 3.1 New CRD: `GitHubApp`

Cluster-scoped singleton per GitHub App install. WRPs reference it by name.

```yaml
apiVersion: warmrunners.io/v1alpha1
kind: GitHubApp
metadata:
  name: acme-warmrunners-app
spec:
  appID: 123456
  privateKeyRef:
    name: acme-warmrunners-app-key
    key: private-key.pem
  webhookSecretRef:
    name: acme-warmrunners-app-webhook
    key: secret
  ingress:
    mode: ingress                # or "tunnel"
    hostname: warmrunners.acme.io   # ingress mode only
    tunnel:
      relayURL: wss://smee.io/xyz   # tunnel mode only
status:
  installations:
    - id: 987654
      account: acme
      repositories: 42
  webhookHealthy: true
  lastDelivery: 2026-07-08T13:04:11Z
```

### 3.2 Updated CRD: `WarmRunnerPolicy`

New optional fields, additive; existing v0.4 policies remain valid.

```yaml
spec:
  githubAppRef:                # new — optional
    name: acme-warmrunners-app
  activeWindowSeconds: 600     # new — default 600 (10 min), matches HPA norm
  queueRule:
    pollInterval: 5m           # bump default from 30s to 300s when githubAppRef set
```

Status additions:

```yaml
status:
  activeUntil: 2026-07-08T13:14:11Z   # when the current active window expires
  lastEventSource: webhook            # webhook | poll
```

### 3.3 Receiver component

New package `internal/webhook/`. Registered on the manager as a
`webhook.Server`-adjacent HTTP handler at `/github/webhook`. Serves on the existing
manager port; the health/readiness probes and metrics endpoints already terminate TLS
via the ingress or tunnel.

Handler flow:

```
POST /github/webhook
  → resolve GitHubApp CR by X-GitHub-Hook-Installation-Target-ID
  → HMAC verify using webhookSecretRef
  → replay-guard on X-GitHub-Delivery
  → dispatch by X-GitHub-Event:
      push          → activityStore.RecordPush(repo, sha)
      workflow_job  → activityStore.RecordJob(repo, labels)
      other         → 200 no-op
  → 200 ok, ~2 ms
```

`activityStore` is a small in-process struct with `RLock`/`Lock` around the map. The
existing reconciler polls it at each Reconcile — no new watch, no channel plumbing.

### 3.4 Ingress modes

**Ingress mode (default).** The user runs a standard Kubernetes Ingress or Gateway API
resource pointing at the manager Service on `/github/webhook`. TLS is user-managed
(cert-manager, cloud LB). Helm chart ships an example values block; no Ingress
resource is created unless the user opts in.

**Tunnel mode.** The manager opens an outbound WebSocket to
`spec.ingress.tunnel.relayURL` at startup. Relay pushes events down the tunnel. Compatible
with the public smee.io service and any private relay implementing the same protocol.
Auto-reconnects with exponential backoff. Used for kind clusters, laptops, and
air-gapped-but-outbound-allowed setups.

### 3.5 Poll fallback

When `githubAppRef` is set:
- Default `pollInterval` becomes 300s (from 30s).
- Poll runs on schedule as today. When the webhook is healthy, the poll observes
  304 / empty pages and consumes negligible rate limit.
- Any run the poll observes that the webhook did not report — measured by
  `lastDelivery < 2 * pollInterval` — is treated as a fresh event and increments the
  activity counter. This is the fallback path when the webhook is down.

When `githubAppRef` is unset:
- Poll behaviour is unchanged from v0.4.

### 3.6 Rolling active-session window

On every accepted `push` or `workflow_job` event for a repo mentioned by any WRP:

```
activeUntil = max(activeUntil, now() + spec.activeWindowSeconds)
```

Reconciler evaluates on each Reconcile:

```
if now() < status.activeUntil:
  contribution = predicted fanout from parser
else:
  contribution = 0
```

Hard cutoff at `activeUntil`, no decay curve. Matches HPA `stabilizationWindowSeconds`
semantics — the same pattern K8s operators already understand.

## 4. Adapter contract

No adapter change. Both ARC and GARM adapters continue to receive a computed `floor`
and patch their respective backend field (`spec.minRunners` /
`spec.minIdleRunners`). Idle-pod behaviour is entirely the adapter backend's
responsibility, as it is today.

## 5. Metrics

New series (Prometheus, on the existing `/metrics` endpoint):

```
warmrunners_webhook_events_total{event,verified,source_repo}   counter
warmrunners_webhook_lag_seconds{event}                          histogram
warmrunners_webhook_deliveries_dropped_total{reason}            counter
    reason: hmac_invalid | replay | unknown_app | body_too_large
warmrunners_tunnel_connected                                    gauge (0/1)
warmrunners_tunnel_reconnects_total                             counter
warmrunners_active_window_expiries_total{repo}                  counter
warmrunners_active_window_seconds_remaining{repo}               gauge
```

`warmrunners_activity_jobs_total{labels}` (v0.3) is retained; its increments now come
from webhook events when `githubAppRef` is set, from polling otherwise.

## 6. Security posture

- **Private key at rest.** The App private key lives in a Kubernetes Secret referenced
  by `GitHubApp.spec.privateKeyRef`. The manager only reads it; RBAC restricts read to
  the manager SA.
- **Webhook secret at rest.** Same as above.
- **HMAC verify.** Constant-time compare, mandatory. Unverified requests never reach
  dispatch.
- **Replay guard.** LRU-of-deliveries prevents accidental double-warm on GitHub retries.
- **Body size cap.** 1 MiB; larger requests are 413'd and counted under
  `body_too_large`.
- **Tunnel authentication.** For smee.io compatibility, the tunnel URL itself is the
  bearer secret (path is unguessable). Documented as such — do not commit tunnel URLs.
- **No delegated calls on user's behalf.** The App is used only to receive events. No
  `write` scopes requested; `read:actions`, `read:metadata`, `read:contents` only.
- **Rate limiting on the receiver.** Per-installation token bucket, 100 req/s soft cap,
  429 above; guards against replay-storm DoS.

## 7. Operational model

- Install the App on the org via a public install link. The App is registered once
  by the warmrunners maintainer; users do not register their own App.
- Create the `GitHubApp` CR pointing at the installation ID + Secret.
- Reference from each WRP via `spec.githubAppRef`.
- The App install adds warmrunners to the repos automatically — no
  per-repo webhook configuration.

## 8. Rollout

- Additive change: v0.4 WRPs work unchanged, poll path is the sole code path when
  `githubAppRef` is not set.
- Documentation includes a quickstart for kind users using tunnel mode and smee.io
  (zero-infra path to try the feature).
- Migration guide notes the recommended `pollInterval` bump when adopting webhook.

## 9. Testing

- Unit tests: HMAC verify (positive + tampered body), replay guard hit/miss, event
  dispatch table, active-window arithmetic across DST and clock skew.
- Envtest: `GitHubApp` CR + WRP referencing it, reconciler observes both event
  sources, floor recomputed on window expiry.
- End-to-end (kind + real GitHub + livetest repo):
  - Install App on livetest, apply `GitHubApp` + WRP.
  - Push commit → observe `warmrunners_webhook_events_total{event="push"}` increment
    within 3s, `activeUntil` set to `now()+600s`.
  - Push commit that triggers matrix of 3 → observe floor patched to 3 within 5s.
  - Kill tunnel / block webhook → observe poll fallback picks up within 2 poll
    intervals, `lastEventSource="poll"`.
  - Wait past `activeUntil` → observe floor drops to 0.
- Kind exercise mandatory before `git tag v0.5.0`, per CLAUDE.local.md.

## 10. Open questions

- **App hosting.** Register a single "warmrunners" GitHub App under a stable
  namespace (personal, then org when adoption warrants). Marketplace listing is
  optional and can wait; direct install links work today.
- **Tunnel relay hosting.** Ship compatibility with the public smee.io first. A
  self-hostable relay is a v0.6 candidate for orgs that refuse third-party proxies.
- **Multi-tenancy.** One manager can watch multiple `GitHubApp` CRs. Whether to
  advertise as a supported multi-tenant deployment or document as best-effort is a
  documentation call, not an architectural one.
