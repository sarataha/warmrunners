# warmrunners — v0.1.1 Polish

**Status:** Approved (2026-05-27)
**Author:** Sara
**Repo:** github.com/sarataha/warmrunners
**Predecessor:** [`2026-05-27-warmrunners-design.md`](./2026-05-27-warmrunners-design.md) (v0.1.0)

## 1. Scope

A patch release that closes correctness, validation, supply-chain, and operability gaps surfaced by a full audit of v0.1.0. **No new features**, no API breaking changes — all CRD additions are additive (new markers, new optional metadata), all controller additions are additive flags with safe defaults, all CI additions are new steps. Semver `0.1.0 → 0.1.1` is correct for 0.x patch convention (cf. ARC's v0.x and garm-operator's v0.2.x patch cadence).

This release exists so that v0.2.0 (codebase-aware) builds on a clean foundation: well-shaped Conditions, tunable concurrency, supply-chain-attested artifacts, and a GitHub client that behaves under rate limits.

### Non-goals

- No new CRD fields with semantic effect (only validation tightening and metadata).
- No new metrics beyond the two missing-but-standard ones (`build_info`, reconcile error counter).
- No EventRecorder, no predicate filters, no structured-logging refactor — those carry design choices and ride in v0.2.0+.
- No PodDisruptionBudget / PriorityClass / topology helm knobs — single-replica operator, defer to v0.3.x.
- No CONTRIBUTING.md / CODE_OF_CONDUCT.md / issue templates — solo OSS, no contributors yet; add when traction warrants.

## 2. Changes by area

### 2.1 CRD (`api/v1alpha1/warmrunnerpolicy_types.go`)

All additive; no schema-breaking change for existing v0.1.0 objects.

- **Conditions list semantics.** Add upstream-canonical markers so server-side apply treats `status.conditions` as a map keyed by `type` (current shape lacks `+listType=map`, breaks SSA merge):
  ```go
  // +patchMergeKey=type
  // +patchStrategy=merge
  // +listType=map
  // +listMapKey=type
  // +optional
  Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
  ```
  Both old-style JSON tags and the four marker comments are required — they target different code paths (SSA schema vs. strategic merge patch). Per `k8s.io/apimachinery/pkg/apis/meta/v1/types.go` and the SIG-architecture API conventions doc.

- **`observedGeneration` on conditions.** Every condition the controller writes must carry the `metav1.Generation` it observed (`obj.GetGeneration()` at decision time). Required by k8s API conventions; without it, consumers can't tell if a condition reflects the latest spec. Controller change covered in §2.2.

- **Printer columns: add Age.**
  ```go
  // +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`
  ```
  Standard for all built-in and well-shaped CRDs; kubectl renders relative ages from `type=date`.

- **Short name + categories.**
  ```go
  // +kubebuilder:resource:path=warmrunnerpolicies,singular=warmrunnerpolicy,shortName=wrp,categories={warmrunners}
  ```
  Pattern follows `hpa`, `pdb`, `pvc`. `categories={warmrunners}` enables `kubectl get warmrunners`. We deliberately omit `all` — that category is for workload-shaped resources, not policy CRDs (anti-pattern to pollute).

- **Cross-field CEL validation.**
  ```go
  // +kubebuilder:validation:XValidation:rule="self.min <= self.max",message="min must be <= max"
  type FloorRange struct {
      Min int32 `json:"min"`
      Max int32 `json:"max"`
  }

  // +kubebuilder:validation:XValidation:rule="self.from < self.to",message="from must be earlier than to"
  type ScheduleWindow struct { ... }
  ```
  Marker on the parent struct so `self` binds to the struct (field-level markers can't see siblings). HH:MM lexical comparison is correct because zero-padded HH:MM sorts numerically.

- **HH:MM pattern on `From`/`To`.**
  ```go
  // +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
  ```

- **Timezone validation.** Pattern-based validation cannot prove a zone exists (IANA has ~600 names, edge cases like `Etc/GMT+10`). Add a defense-in-depth syntax check, but the source of truth is runtime `time.LoadLocation`:
  ```go
  // +kubebuilder:validation:Pattern=`^[A-Za-z]+(?:/[A-Za-z0-9_+\-]+){0,2}$`
  // +kubebuilder:validation:MaxLength=64
  ```
  On runtime failure, surface `Ready=False, reason=InvalidTimezone`.

- **MaxLength bounds.** Add `+kubebuilder:validation:MaxLength=...` on every free-form string (`Owner`, `Repository`, secret refs, target names, label strings) — cheap defense against accidentally-large CRs. Conservative caps (e.g. 253 for DNS-style names, 64 for short identifiers).

### 2.2 Controller (`internal/controller/warmrunnerpolicy_controller.go`)

- **`MaxConcurrentReconciles` flag.** Plumb a manager-level flag `--max-concurrent-reconciles` (default `1` to preserve current behavior) into `SetupWithManager`:
  ```go
  return ctrl.NewControllerManagedBy(mgr).
      For(&warmrunnersv1alpha1.WarmRunnerPolicy{}).
      WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
      Complete(r)
  ```
  controller-runtime guarantees same-key reconciles never overlap regardless of this value; only distinct keys parallelize.

- **Set `ObservedGeneration` on every condition.** Update the existing `setCondition` helper to take the policy's generation and assign it to `metav1.Condition.ObservedGeneration` before merging via `meta.SetStatusCondition`.

- **Log level flag.** Add `--log-level` (default `info`). Replace the hardcoded zap Development mode in `cmd/main.go` with a configurable production-shaped logger; controller and poller emit `V(1)`/`V(2)` for trace-level events. (Scope: flag + logger config only; broader `WithValues` sweep deferred to v0.2.0.)

### 2.3 Manager / main (`cmd/main.go`)

- **`LeaderElectionReleaseOnCancel = true`.** Currently commented out. The scaffolded `main` exits immediately after `mgr.Start` returns, which is the documented safe pattern; enabling this speeds up leader transitions on graceful shutdown. Add an inline comment recording the safety assumption ("safe because main exits immediately after Start returns").

- **Remove cert-manager TODO boilerplate.** The `cmd/main.go:145-148` comment is leftover scaffold from a webhook setup we don't ship in v0.1.x. Delete.

### 2.4 GitHub demand source (`internal/demand/github_poller.go`)

GitHub's official polling guidance has four pieces. v0.1.0 hits zero of four.

- **User-Agent header.** GitHub rejects requests without one and recommends app-identifying values:
  ```
  User-Agent: warmrunners/<version>
  ```
  Pulled from a `version` package set at build via `-ldflags`.

- **HTTP client timeout flag.** Currently hardcoded `10s`. Expose as `--github-http-timeout` (default `10s`). Per-policy override deferred — global flag is enough for v0.1.1.

- **ETag conditional requests.** GitHub Actions endpoints return `ETag`; sending `If-None-Match: "<etag>"` on a subsequent authenticated request and receiving `304 Not Modified` does **not** count against the primary rate limit (`x-ratelimit-*`). Reduces quota burn substantially for slow-moving queues. Cache the ETag + the parsed response per `(owner, repo, endpoint)` key in the poller; on 304 reuse the cached payload. Note: 304s still count against per-minute secondary limits.

- **Retry / backoff.** No official Go algorithm from GitHub, but the docs are explicit about behavior:
  - On `Retry-After`: sleep exactly that many seconds, then resume.
  - On `x-ratelimit-remaining: 0`: sleep until `x-ratelimit-reset`.
  - On 5xx / transient network: exponential backoff `min(2^n, 60s)` with jitter, max 3 retries, then surface error (controller-runtime will requeue with its own backoff).
  - On repeated rate-limit hits: stop — GitHub may ban integrations that ignore signals.

- **Resolve `FIX B / FIX C` comments.**
  - `FIX C` (`github_poller.go:48`): request-build error is silently dropped — return it.
  - `FIX B` (`github_poller.go:139`): documented limitation on label-scoped demand; either resolve (preferred — by filtering jobs in the response) or convert to an `// LIMITATION:` comment with a tracking issue.

### 2.5 Metrics (`internal/controller/metrics.go`)

- **`warmrunners_build_info`** gauge with constant value `1` and labels `version`, `commit`, `build_date`. Standard pattern (cf. KEDA `keda_build_info`, cert-manager `certmanager_build_info`).

- **`warmrunners_reconciliation_errors_total`** counter labeled by `policy` and `error_type` (e.g. `demand_source`, `adapter`, `status_update`). Distinct from controller-runtime's built-in `controller_runtime_reconcile_errors_total` — that's per-controller, this is per-policy and labeled by failure mode.

### 2.6 RBAC (`config/rbac/`)

- **`warmrunnerpolicy_viewer_role.yaml`**: add aggregation label so the standard k8s `view` ClusterRole includes our resources:
  ```yaml
  metadata:
    labels:
      rbac.authorization.k8s.io/aggregate-to-view: "true"
  ```
  Add analogous `aggregate-to-edit` and `aggregate-to-admin` on the editor/admin roles (already conventional for each).

- **`role.yaml`**: remove `create` from `warmrunnerpolicies` verbs (unused by reconciler).

### 2.7 CI / supply chain (`.github/workflows/`)

All steps pin to versions verified current as of 2026-05.

- **`govulncheck`** in `test.yml`:
  ```yaml
  - uses: golang/govulncheck-action@v1.0.4
    with:
      go-version-input: '1.25'
      go-package: ./...
  ```

- **Race detector in `make test`** (`Makefile`):
  ```diff
  - go test $(shell go list ./... | grep -v /e2e) -coverprofile cover.out
  + go test -race -covermode=atomic $(shell go list ./... | grep -v /e2e) -coverprofile cover.out
  ```

- **`release.yml`**: add `permissions: { id-token: write, contents: write, packages: write }` at the job level, then:
  - **Cosign keyless** sign image AND chart by digest:
    ```yaml
    - uses: sigstore/cosign-installer@v4.1.2
    - run: cosign sign --yes "${IMAGE}@${DIGEST}"
    - run: cosign sign --yes "${CHART_REF}@${DIGEST}"
    ```
    Digests come from `docker/build-push-action` output and a captured `helm push` digest.
  - **SBOM** (SPDX-JSON via Anchore/Syft — CNCF/Sigstore-standard for OSS operators):
    ```yaml
    - uses: anchore/sbom-action@v0.24.0
      with:
        image: ghcr.io/sarataha/warmrunners@${{ steps.build.outputs.digest }}
        format: spdx-json
        upload-release-assets: true
    ```
  - **Attest the SBOM** as an in-toto attestation bound to the image digest:
    ```yaml
    - run: cosign attest --yes --predicate warmrunners-${{ github.ref_name }}.spdx.json --type spdxjson "${IMAGE}@${DIGEST}"
    ```

- **README updates**: add a "Verifying releases" section with the exact `cosign verify` and `cosign verify-attestation` commands for the image and chart, pinning the OIDC issuer and certificate-identity regexp.

### 2.8 Repo hygiene

- **`SECURITY.md`** at repo root with a private-disclosure address (Sara's GitHub email or a `SECURITY@` alias) and a short triage commitment. Matches GitHub's "Security policy" surface and unlocks the `Security` tab.

- **`CHANGELOG.md`**: new `## [0.1.1] - <date>` section grouped by area (Added / Changed / Fixed / Security).

- **`README.md`**: extend the existing badges (already added) with a release-tag link refresh after tag.

## 3. Testing

Every change carries a test or proof — TDD throughout, per `superpowers:test-driven-development`.

- **CRD markers**: golden-file test diffing the generated CRD YAML against checked-in expected output. Confirms `x-kubernetes-list-type: map`, printer columns, shortName, categories, validation rules all land in the schema. Run via `make manifests` + `git diff --exit-code`.

- **CEL rules**: envtest applies a malformed CR (e.g. `floor.min=10, max=5`); expect the API server to reject with the configured message.

- **HH:MM / TZ patterns**: envtest with malformed strings; expect rejection. Runtime TZ load tested via existing scheduler unit tests (extend with an invalid-zone case asserting `Ready=False, reason=InvalidTimezone`).

- **`ObservedGeneration`**: unit test on `setCondition` confirming the value is propagated.

- **`MaxConcurrentReconciles`**: smoke-test that the flag parses and is threaded into `controller.Options`; behavioral parallelism isn't worth a flaky integration test.

- **GitHub poller**:
  - `httptest.Server` returning an `ETag` header → next request includes `If-None-Match` → server responds 304 → poller reuses cached payload (no parse error, no metric blip).
  - `httptest.Server` returning 5xx then 200 → poller retries with backoff and succeeds.
  - `httptest.Server` returning 429 with `Retry-After: 1` → poller sleeps before retry.
  - Missing User-Agent test removed because we now always send one; replace with an assertion that the UA matches `warmrunners/<version>`.

- **Metrics**: unit test that `build_info` is registered with the expected label set and that `reconciliation_errors_total` increments on a forced poller failure.

- **CI**: a dry-run of the new release workflow against a `v0.1.1-rc.1` pre-release tag, validating cosign + SBOM steps land in the release assets. (Real tag cuts after merge.)

## 4. Compatibility

- **Existing v0.1.0 CRs**: continue to validate. New CEL rules only reject objects that were already semantically broken (`min > max`, `from >= to`); pattern markers only reject malformed HH:MM and obviously-bogus TZ strings. No supported configuration becomes invalid.
- **Helm upgrade**: in-place `helm upgrade` from 0.1.0 → 0.1.1 supported. The new manager flags have defaults equal to v0.1.0 behavior.
- **Image / chart**: new tag `v0.1.1`; same coordinates as v0.1.0. Old image stays on GHCR.
- **API group / version**: unchanged (`autoscaling.warmrunners.io/v1alpha1`).

## 5. Rollout

1. Branch `release/v0.1.1` off `main`.
2. Implement per §2 in component-grouped commits, TDD throughout (one commit per `internal/<component>/...` or `config/<area>/...` once green).
3. PR → green CI → merge to `main`.
4. Tag `v0.1.1` (signed annotated tag).
5. Release workflow builds + signs image + chart, attaches SBOM, drafts release notes from `CHANGELOG.md`.
6. Smoke: pull image + chart from GHCR anonymously, run `cosign verify` + `cosign verify-attestation`, install on kind, apply existing examples, confirm no behavioral regression.

## 6. Open items deferred to v0.2.0 (recorded so they're not lost)

- EventRecorder + state-change events.
- Predicate filters on the reconciler.
- Structured-logging sweep (`WithValues` across controller + poller).
- Secondary rate-limit handling beyond the basics in §2.4 (e.g. cluster-wide token bucket if scaling to many policies).
- Trivy container scanning (govulncheck covers Go modules; Trivy covers OS layers — keep `golang:1.25` + distroless minimal, low value today).
