# Operator runbook

One section per symptom. Each: cause line, three commands, fix bullets.

## `lastReconcileTime` stuck

**Cause:** controller down, leader lease pinned to a dead pod, or status writes blocked.

```sh
kubectl -n warmrunners-system get pods
kubectl -n warmrunners-system logs deploy/warmrunners-controller-manager --tail=50
kubectl -n warmrunners-system get lease 67406384.warmrunners.io -o yaml
```

- Pod `CrashLoopBackOff` → fix root error in logs, do not just restart.
- Stale lease holder → `kubectl -n warmrunners-system delete lease 67406384.warmrunners.io`.

## `DemandSourceAvailable=False`

**Cause:** GitHub auth failed. Floor held at last value (safety rule).

```sh
kubectl describe wrp <name> | grep -A2 DemandSourceAvailable
kubectl logs -n warmrunners-system deploy/warmrunners-controller-manager --tail=100 | grep -E "401|403|bad credentials"
kubectl get secret <auth.secretRef.name> -o jsonpath='{.data.token}' | base64 -d | head -c 8
```

- Token revoked or rotated → replace Secret, picked up on next poll.
- Token missing `actions:read` → reissue with correct scope.
- Prefix not `ghp_`/`ghs_`/`github_pat_` → file truncated; recreate the Secret.

## `warmrunners_github_rate_limit_remaining` near zero

**Cause:** token quota exhausted. `403` cycle starts, floor freezes.

```sh
curl ... /metrics | grep rate_limit
kubectl get wrp -A -o json | jq '[.items[] | .spec.github.auth.secretRef.name] | group_by(.) | map({secret:.[0], policies:length})'
date -r $(curl ... /metrics | awk '/rate_limit_reset/{print int($2)}')
```

- Raise `spec.queueRule.pollInterval`.
- Lower `spec.predictor.maxRunsPerPoll`.
- Split policies across more tokens. See `docs/rate-limits.md`.

## Controller `OOMKilled`

**Cause:** memory limit too low for policy count.

```sh
kubectl -n warmrunners-system describe pod -l control-plane=controller-manager | grep -i oom
kubectl -n warmrunners-system top pod
kubectl get wrp -A --no-headers | wc -l
```

- Default `128Mi` covers ~25 policies. Raise via `controllerManager.container.resources.limits.memory`.
- Single-policy OOM → capture heap profile, open issue.

## Leader fight (`replicas: 2+`)

**Cause:** two pods alternating the lease.

```sh
kubectl -n warmrunners-system get lease 67406384.warmrunners.io -o jsonpath='{.spec.holderIdentity}'; echo
kubectl -n warmrunners-system logs -l control-plane=controller-manager --prefix --tail=20 | grep -i lease
```

- Stale follower pod → delete it.
- Node clock skew > 10s → check NTP.
- Raise `terminationGracePeriodSeconds` if a graceful exit truncates lease release.

## `status.dryRun` does not match `spec.dryRun`

**Cause:** controller has not reconciled since the spec change.

```sh
kubectl get wrp <name> -o jsonpath='{"spec="}{.spec.dryRun}{" status="}{.status.dryRun}{"\n"}'
kubectl annotate wrp <name> warmrunners.io/reconcile="$(date +%s)" --overwrite
curl ... /metrics | grep dry_run_skipped_patches
```

- After `dryRun: true → false` the next reconcile patches the backend with current `desiredFloor`. Confirm via `warmrunners_floor_change_total{direction="up"}`.

## `AdapterAvailable=False`

**Cause:** backend Patch rejected. `appliedFloor` holds last value.

```sh
kubectl describe wrp <name> | grep -A2 AdapterAvailable
kubectl get autoscalingrunnerset -A | grep <runnerSet.name>
kubectl get pool.garm.cloudbase.com -A | grep <pool.name>
```

- Wrong `spec.target.*.{name,namespace}` → fix the policy.
- Missing `patch` RBAC on the backend CRD → re-apply the chart.
- `floor.max` over the backend's `maxRunners` → the controller clamps, but raise the backend cap if needed.

## One-liners

```sh
# Errors only
kubectl -n warmrunners-system logs -f deploy/warmrunners-controller-manager | jq 'select(.level=="error")'

# Floors at a glance
kubectl get wrp -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,DESIRED:.status.desiredFloor,APPLIED:.status.appliedFloor,QUEUE:.status.lastQueueDepth,DRYRUN:.status.dryRun
```
