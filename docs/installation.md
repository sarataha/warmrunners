# Installation

This guide covers every supported install method for warmrunners, plus
verification, upgrades, and removal. For a one-line quick start, see the
project [README](../README.md).

## Prerequisites

- Kubernetes 1.28 or newer.
- A runner backend already installed in the cluster:
  - [Actions Runner Controller (ARC)](https://github.com/actions/actions-runner-controller), or
  - [GARM](https://github.com/cloudbase/garm) with the
    [garm-operator](https://github.com/mercedes-benz/garm-operator).
- A GitHub personal access token or GitHub App with `actions:read` on every
  repository a `WarmRunnerPolicy` will reference.
- `kubectl` 1.28+. The Helm method also needs `helm` 3.14+ (OCI registry support).

## Methods

Pick one. All methods install the same controller, CRD, RBAC, and metrics
service into the `warmrunners-system` namespace.

### Helm (recommended)

```sh
helm install warmrunners oci://ghcr.io/sarataha/charts/warmrunners \
  --version 0.3.0 \
  --namespace warmrunners-system \
  --create-namespace
```

To override defaults, write a `values.yaml` and pass `--values values.yaml`.
Common overrides:

```yaml
controllerManager:
  replicas: 2                # leader election is on by default
  container:
    resources:
      requests: { cpu: 50m, memory: 128Mi }
      limits:   { cpu: 500m, memory: 256Mi }
prometheus:
  enable: true               # adds a ServiceMonitor
```

The chart README packaged with the release lists every value.

### Raw manifests (kubectl)

```sh
kubectl apply -f https://github.com/sarataha/warmrunners/releases/download/v0.3.0/install.yaml
```

`install.yaml` is the same content the Helm chart renders, with default values.
Edit it before applying to change replicas, resources, or image tag.

### GitOps — Flux

Commit two manifests to your Flux repository:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: warmrunners
  namespace: flux-system
spec:
  type: oci
  url: oci://ghcr.io/sarataha/charts
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: warmrunners
  namespace: warmrunners-system
spec:
  interval: 1h
  chart:
    spec:
      chart: warmrunners
      version: "0.3.0"
      sourceRef:
        kind: HelmRepository
        name: warmrunners
        namespace: flux-system
  install:
    createNamespace: true
  values:
    controllerManager:
      replicas: 2
```

### GitOps — Argo CD

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: warmrunners
  namespace: argocd
spec:
  project: default
  source:
    repoURL: ghcr.io/sarataha/charts
    chart: warmrunners
    targetRevision: "0.3.0"
    helm:
      values: |
        controllerManager:
          replicas: 2
  destination:
    server: https://kubernetes.default.svc
    namespace: warmrunners-system
  syncPolicy:
    automated: { prune: true, selfHeal: true }
    syncOptions: [CreateNamespace=true]
```

### From source

For development or unreleased builds:

```sh
git clone https://github.com/sarataha/warmrunners.git
cd warmrunners
git checkout v0.3.0
make deploy IMG=ghcr.io/sarataha/warmrunners:v0.3.0
```

## Verify the install

Check the controller is running:

```sh
kubectl -n warmrunners-system get deploy,pod
```

Confirm the CRD is registered:

```sh
kubectl get crd warmrunnerpolicies.autoscaling.warmrunners.io
```

Create a GitHub token Secret and a sample policy:

```sh
kubectl create secret generic gh-token \
  --from-literal=token=ghp_xxx \
  --namespace default
kubectl apply -f https://raw.githubusercontent.com/sarataha/warmrunners/v0.3.0/examples/policy-arc.yaml
```

Watch the status fields populate:

```sh
kubectl get warmrunnerpolicy -w
```

`APPLIED` (the floor written to the backend) and `PREDICTED` (the controller's
current target) should converge within one `pollInterval` (30s by default).

## Upgrade

Helm:

```sh
helm upgrade warmrunners oci://ghcr.io/sarataha/charts/warmrunners \
  --version 0.3.0 \
  --namespace warmrunners-system
```

kubectl:

```sh
kubectl apply -f https://github.com/sarataha/warmrunners/releases/download/v0.3.0/install.yaml
```

Flux and Argo CD reconcile automatically once you bump `version` /
`targetRevision` and commit.

Review [CHANGELOG.md](../CHANGELOG.md) for breaking changes between minors.

## Uninstall

Helm:

```sh
helm uninstall warmrunners --namespace warmrunners-system
kubectl delete crd warmrunnerpolicies.autoscaling.warmrunners.io
```

The CRD is annotated `helm.sh/resource-policy: keep`, so Helm leaves it in
place. Delete it explicitly to remove every policy.

kubectl:

```sh
kubectl delete -f https://github.com/sarataha/warmrunners/releases/download/v0.3.0/install.yaml
```

## Next steps

- [`examples/`](../examples/) — full ARC and GARM policy manifests.
- [`docs/metrics.md`](metrics.md) — Prometheus metrics reference.
- [`docs/security.md`](security.md) — cosign + SBOM verification.
