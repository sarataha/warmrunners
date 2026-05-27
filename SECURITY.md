# Security Policy

## Supported versions

warmrunners is pre-1.0; the `WarmRunnerPolicy` CRD is `v1alpha1` and the API may
change between minor releases. Security fixes land on the latest `0.x` minor only.

| Version | Supported |
| ------- | --------- |
| 0.1.x   | ✅        |
| < 0.1   | ❌        |

## Reporting a vulnerability

Report privately — **do not** open a public issue for a security problem.

Use [GitHub Security Advisories](https://github.com/sarataha/warmrunners/security/advisories/new)
("Report a vulnerability"). This opens a private channel with the maintainer and
supports coordinated disclosure.

Please include: affected version, a description of the issue, and reproduction
steps or a proof of concept.

## Disclosure timeline

- **Acknowledgement:** within 7 days.
- **Fix target:** within 30 days for confirmed issues, severity permitting.
- Disclosure is coordinated: a fix and advisory are published together, crediting
  the reporter unless anonymity is requested.

## Scope

In scope: the controller, its RBAC, secret handling, and the artifacts published
to GHCR (image and Helm chart).

Out of scope: vulnerabilities in transitive dependencies that warmrunners does not
ship or expose (report those upstream); issues requiring cluster-admin access the
operator does not grant itself.
