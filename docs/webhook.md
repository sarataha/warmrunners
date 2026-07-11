# Event-driven pre-warm (GitHub App webhook)

## What it does

Without this feature, warmrunners learns about activity only by polling the
GitHub REST API every `pollInterval`. With a `GitHubApp` configured and
referenced from a policy, a `push` fires the App's webhook, the receiver
verifies and parses it, and the policy's floor bumps immediately — usually
under a second, well before the matching `workflow_run` would appear in a poll
cycle. If the webhook path goes down (relay unreachable, App misconfigured,
ingress flaky), the poller keeps running underneath it and the controller
falls back to REST observations automatically.

## Prerequisites

- warmrunners v0.5.0 or newer.
- Either the cluster's ingress is reachable from the internet (ingress mode)
  or the cluster has outbound network access to a relay (tunnel mode).
- Permission to install a GitHub App on the target org or repository.

## Setup — ingress mode

1. Register the App via the maintainer-published install link:
   `https://github.com/apps/warmrunners`. Requested permissions:
   `Actions: Read-only`, `Contents: Read-only`, `Metadata: Read-only`; events
   `push`, `workflow_job`.
2. Install the App on the target org or repository. Download the private
   key, and note the App ID and installation ID.
3. Create the private-key Secret:

   ```sh
   kubectl create secret generic warmrunners-app-key \
     --namespace warmrunners-system \
     --from-file=private-key.pem=./key.pem
   ```

4. Create the webhook-secret Secret (use a 32-byte random value, e.g.
   `openssl rand -hex 32`):

   ```sh
   kubectl create secret generic warmrunners-app-webhook \
     --namespace warmrunners-system \
     --from-literal=secret=<32-byte random>
   ```

5. Apply a `GitHubApp` CR:

   ```yaml
   apiVersion: autoscaling.warmrunners.io/v1alpha1
   kind: GitHubApp
   metadata:
     name: warmrunners-app
   spec:
     appID: 123456
     privateKeyRef: {name: warmrunners-app-key, key: private-key.pem, namespace: warmrunners-system}
     webhookSecretRef: {name: warmrunners-app-webhook, key: secret, namespace: warmrunners-system}
     ingress:
       mode: ingress
       hostname: warmrunners.example.io
   ```

6. Point the App's webhook URL at `https://warmrunners.example.io/github/webhook`.
   The receiver mounts on the metrics server; expose that Service through an
   Ingress with TLS.

## Setup — tunnel mode

> **Dev/kind only.** Tunnel mode subscribes to a smee.io-compatible SSE relay,
> which decodes and re-serialises the webhook payload. That breaks the HMAC
> signature GitHub computed over the original bytes, so tunnel mode
> **does not verify HMAC** — it trusts that anyone who reaches the relay's
> unguessable channel URL is authorised. Use ingress mode in production.



For kind, single-node clusters, or air-gapped clusters that still allow
outbound traffic — no Ingress required.

1. Create a smee.io channel: open `https://smee.io/new` and copy the
   returned URL (e.g. `https://smee.io/abcdef123`).
2. Point the App's webhook URL at that same smee URL.
3. Create the private-key and webhook-secret Secrets as in ingress mode
   (steps 3–4 above).
4. Apply a `GitHubApp` CR with `mode: tunnel`:

   ```yaml
   spec:
     ingress:
       mode: tunnel
       tunnel:
         relayURL: https://smee.io/abcdef123
   ```

5. **Important**: the smee URL *is* the bearer secret — anyone who knows it
   can inject events into your cluster. Do not commit it to git; treat it
   like the webhook secret itself.

## Reference from a `WarmRunnerPolicy`

```yaml
spec:
  githubAppRef:
    name: warmrunners-app
  activeWindowSeconds: 600  # optional, default 600
```

## Verifying it works

- `kubectl get gha` — expect `HEALTHY=true` and a recent `LASTDELIVERY`.
- Push to a repository covered by the policy.
- `kubectl get wrp <name> -o jsonpath='{.status.lastEventSource}'` returns
  `webhook` within about a second.
- `kubectl get wrp <name> -o jsonpath='{.status.activeUntil}'` is non-empty.
- Metrics confirm the same thing:
  `warmrunners_webhook_events_total{event="push",verified="true"}` (ingress)
  or `…,verified="tunnel"` (tunnel) incremented, and
  `warmrunners_active_window_seconds_remaining{repo="..."}` > 0.

## Poll fallback

- When `githubAppRef` is set, the reconciler defaults `pollInterval` to
  300s (up from 30s pre-v0.5).
- The poller still runs. When the webhook has not delivered anything in the
  last `2 * pollInterval`, the reconciler treats poll observations as
  authoritative and sets `status.lastEventSource = poll`.
- Under healthy webhook flow, the poll still runs, sees no new activity, and
  adds negligible rate-limit pressure — typically `304`s.

## Troubleshooting

| Symptom | Where to look | Fix |
| --- | --- | --- |
| `hmac_invalid` dropped events | `warmrunners_webhook_deliveries_dropped_total{reason="hmac_invalid"}` | Webhook secret in the App does not match the Secret in the cluster. Regenerate and update both. |
| `unknown_app` | `warmrunners_webhook_deliveries_dropped_total{reason="unknown_app"}` | `GitHubApp.spec.appID` does not match the App sending the event, or no `GitHubApp` CR is applied. |
| `body_too_large` | `warmrunners_webhook_deliveries_dropped_total{reason="body_too_large"}` | Payload exceeds the 1 MiB cap. Unusual — inspect the event on the App's Deliveries tab. |
| `TunnelConnected = 0` | `warmrunners_tunnel_connected{app=…}` | Relay URL unreachable or wrong. Check `warmrunners_tunnel_reconnects_total{outcome="failure"}` for repeated dial errors. |
| No `push` events after install | `warmrunners_webhook_events_total{event="push"}` | Confirm the App was installed on the target repository (Install App → Only select repositories). |

## Security notes

- HMAC verification is mandatory; unsigned or wrong-secret requests are
  dropped with `hmac_invalid`.
- Replay guard: 10,000 delivery IDs held for 24h.
- Body cap: 1 MiB.
- Requested App scopes are read-only only.
- The tunnel URL is the bearer secret; store it outside git.
