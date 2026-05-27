Predictive warm-floor controller for self-hosted GitHub Actions runners — sets the backend
warm-floor ahead of demand so the morning's first build skips the cold start and idle runners
don't burn money overnight.

### Install

```sh
helm install warmrunners oci://ghcr.io/sarataha/charts/warmrunners --version <version>
```

Or apply the bundled `install.yaml`. See [`examples/`](https://github.com/sarataha/warmrunners/tree/main/examples) for sample policies.

---
