# warmrunners — agent context

## Project

Predictive warm-floor controller for self-hosted GitHub Actions runners.
Kubebuilder operator with one CRD (`WarmRunnerPolicy`); plugs into ARC
(`AutoscalingRunnerSet.minRunners`) and GARM (`Pool.minIdleRunners`) via a
pluggable `Adapter` interface. Pure k8s controller — no SaaS, no UI.

**Audience:** any team running self-hosted GitHub Actions runners. **Not**
zencargo-specific; never hardcode service names, labels, or schedules in code.

## Tech stack

- Go 1.25, kubebuilder 4.9.0, controller-runtime
- Dockerfile base `golang:1.25` (must be ≥ the `go` directive)
- `unstructured.Unstructured` for ARC + GARM CRDs (no third-party vendoring)
- `httptest` for GitHub stubs · `envtest` for integration · `client/fake` for unit
- `prometheus/client_golang` for metrics

**Version policy (hard rule):** pin every dependency, tool, base image, and GitHub Action to the
**latest stable**; justify exceptions in the commit message. Standing exception: the `go` directive
tracks the newest version the *whole* toolchain supports (capped at 1.25 by golangci-lint), not
bleeding-edge — it's a compatibility floor, not "use newest".

## Commands

```
make manifests generate   # regenerate CRDs + deepcopy after API changes
make test                 # unit tests (controller-runtime + scheduler + adapter)
go test ./internal/scheduler/... -v    # iterate on scheduler logic fast
go test ./... -tags integration -v     # envtest integration suite
go vet ./...
go build ./...
```

## Code standards

- One responsibility per file. Keep files small and focused; the spec's file
  layout is the contract.
- Interface-first for `DemandSource`, `Scheduler`, `Adapter`. Reconciler must
  not know which implementation it has.
- Public names match the spec verbatim (`WarmRunnerPolicy`, `Decision`, `Demand`,
  `Snapshot`, `Ref`). Rename only via spec amendment.
- TDD enforced (RED → GREEN → REFACTOR). No implementation code before a
  failing test exists. `superpowers:test-driven-development` skill.
- Conventional Commit prefixes: `feat(scope):` / `fix(scope):` / `test(scope):` / `docs:` / `ci:` / `chore:`.
- Make surgical changes: edit only what's needed, preserve existing style, no orthogonal refactors.

## Safety rules

- **Never delete runners.** Only adjust the warm-floor field. Backends drain naturally.
- **Never exceed `floor.max`.** Hard cap under any rule combination.
- **Never patch the backend on a `DemandSource` error.** Hold last-known state.
- **Never break the `Adapter` / `Scheduler` / `DemandSource` interfaces** without
  updating the spec first — they're the extension contract.
- **No zencargo strings in code.** Examples in `examples/` are generic.

## Pointers (load on demand, not every turn)

- Design: `docs/superpowers/specs/2026-05-27-warmrunners-design.md`
- Plan: `docs/superpowers/plans/2026-05-27-warmrunners-v1.md`
- Spec lives there in full — don't duplicate it here.

## Don't

- Don't commit generated artifacts beyond `zz_generated.deepcopy.go` and the
  kubebuilder CRD YAML.
- Don't add transitive deps on `actions-runner-controller` or `garm-operator`
  Go packages. Use `unstructured` instead.
- Don't add a webhook receiver or codebase-aware logic — those are v1.5 / v2.
