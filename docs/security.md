# Verifying warmrunners releases

Release container images and Helm charts are signed with [cosign](https://github.com/sigstore/cosign)
(keyless, via GitHub OIDC) and each release carries an attested SPDX SBOM. The
signatures are public — verify them anonymously before deploying.

## Image

```sh
cosign verify \
  --certificate-identity-regexp="^https://github.com/sarataha/warmrunners/.github/workflows/release.yml@refs/tags/v.*$" \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/sarataha/warmrunners:v0.3.0
```

## Helm chart

```sh
cosign verify \
  --certificate-identity-regexp="^https://github.com/sarataha/warmrunners/.github/workflows/release.yml@refs/tags/v.*$" \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/sarataha/charts/warmrunners:0.3.0
```

## SBOM attestation

```sh
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp="^https://github.com/sarataha/warmrunners/.github/workflows/release.yml@refs/tags/v.*$" \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  ghcr.io/sarataha/warmrunners:v0.3.0
```

Each release's GitHub Releases page also has a downloadable `*.spdx.json` SBOM
asset alongside the `install.yaml` for the operator.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
