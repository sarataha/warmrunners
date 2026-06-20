# Upgrade guide

## v0.3.x → v0.4.0

No CRD-breaking change. Three operator-visible changes:

| Change | Action |
|---|---|
| `spec.dryRun` field added (default `false`) | Opt-in only. Existing policies behave identically. |
| Leader election default flipped to `true` | If you ran the controller as a single replica with `--leader-elect=false` baked in, no change. If you customized `args` to remove `--leader-elect`, drop that override — the flag is now on by default. |
| New metrics: `warmrunners_dry_run_skipped_patches_total`, `warmrunners_github_rate_limit_{remaining,reset_seconds}` | Pure addition. Existing dashboards keep working; update them to surface the new gauges. See [`docs/metrics.md`](metrics.md). |

### Helm

```sh
helm upgrade warmrunners oci://ghcr.io/sarataha/charts/warmrunners \
  --version 0.4.0 \
  --namespace warmrunners-system
```

### Raw manifests

```sh
kubectl apply -f https://github.com/sarataha/warmrunners/releases/download/v0.4.0/install.yaml
```

### GitOps (Flux / Argo CD)

Bump `version` / `targetRevision` to `"0.4.0"` and commit. The chart reconciles automatically.

### Verify

```sh
kubectl -n warmrunners-system rollout status deploy/warmrunners-controller-manager
kubectl get wrp -A -o custom-columns=NAME:.metadata.name,DESIRED:.status.desiredFloor,APPLIED:.status.appliedFloor,DRYRUN:.status.dryRun
```

`DRYRUN` should show `<none>` for existing policies (omitted in spec = `false`).

### Rollback

```sh
helm rollback warmrunners
```

The CRD is annotated `helm.sh/resource-policy: keep`, so a rollback never deletes existing `WarmRunnerPolicy` objects.
