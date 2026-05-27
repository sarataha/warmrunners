# warmrunners v0.1.1 Polish — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close v0.1.0 audit gaps (CRD validation, controller flags, GitHub-client behavior, supply-chain artifacts, repo hygiene) without adding new features. Ship as `v0.1.1`.

**Architecture:** Patch release; no new components. Existing units (CRD types, reconciler, GitHub poller, metrics, RBAC, release workflow) gain tighter validation, configurable knobs, and standard supply-chain steps. Every change is additive — existing v0.1.0 objects validate unchanged; existing manager flags keep current defaults.

**Tech Stack:** Go 1.25, kubebuilder 4.9.0, controller-runtime, `metav1.Condition`, kubebuilder CEL XValidation markers, `httptest`, `envtest`, GitHub Actions, `sigstore/cosign-installer`, `anchore/sbom-action`, `golang/govulncheck-action`.

**Source spec:** `docs/superpowers/specs/2026-05-27-warmrunners-v0.1.1-polish.md`.

**Commit policy (per `CLAUDE.local.md`):** Group commits per component. TDD red→green is the *work* breakdown; commit once the whole component is green. Conventional Commits prefixes (`feat(scope):`, `fix(scope):`, `test(scope):`, `docs:`, `ci:`, `chore:`). Signed (`-S`). No co-author trailers.

**Branch:** `release/v0.1.1` off `main`.

---

## File map

**Modify:**
- `api/v1alpha1/warmrunnerpolicy_types.go` — Conditions markers, printer columns, shortName, categories, CEL XValidation, MaxLength bounds, Pattern markers.
- `api/v1alpha1/groupversion_info.go` — (no change expected; verify).
- `cmd/main.go` — `MaxConcurrentReconciles` flag, `--log-level` flag, `--github-http-timeout` flag, `LeaderElectionReleaseOnCancel=true`, remove cert-manager TODO.
- `internal/controller/warmrunnerpolicy_controller.go` — `setCondition` carries `ObservedGeneration`; reconciler reads `MaxConcurrentReconciles`.
- `internal/controller/metrics.go` — `warmrunners_build_info`, `warmrunners_reconciliation_errors_total`.
- `internal/demand/github_poller.go` — User-Agent, tunable HTTP timeout, ETag cache, retry/backoff, resolve FIX B/C.
- `config/rbac/role.yaml` — drop unused `create` verb on `warmrunnerpolicies`.
- `config/rbac/warmrunnerpolicy_viewer_role.yaml` — `aggregate-to-view: "true"` label.
- `config/rbac/warmrunnerpolicy_editor_role.yaml` — `aggregate-to-edit: "true"` label.
- `config/rbac/warmrunnerpolicy_admin_role.yaml` — `aggregate-to-admin: "true"` label.
- `.github/workflows/test.yml` — `govulncheck-action` step.
- `.github/workflows/release.yml` — `id-token: write` permission, cosign install + sign image + sign chart by digest, SBOM via Anchore, cosign attest SBOM.
- `Makefile` — `-race -covermode=atomic` in `test` target.
- `README.md` — "Verifying releases" section with `cosign verify` + `cosign verify-attestation` commands.
- `CHANGELOG.md` — `## [0.1.1]` section.

**Create:**
- `SECURITY.md` (repo root).
- `internal/version/version.go` — exported `Version`, `Commit`, `BuildDate` populated by `-ldflags` (used by `build_info` metric and User-Agent).

**Tests touched/added:**
- `api/v1alpha1/warmrunnerpolicy_types_test.go` — golden-file generated CRD comparison.
- `internal/controller/warmrunnerpolicy_controller_test.go` — `ObservedGeneration` propagation.
- `internal/controller/metrics_test.go` — `build_info` registered with labels; `reconciliation_errors_total` increments.
- `internal/demand/github_poller_test.go` — ETag conditional + 304 reuse, retry on 5xx, `Retry-After` honored, User-Agent header present.
- `test/e2e/` — `helm install ... --version v0.1.1-rc.1`, smoke-apply existing examples, `cosign verify` + `cosign verify-attestation`.

---

## Task 1: Branch + scaffold

**Files:**
- `internal/version/version.go` (create)

- [ ] **Step 1.** Verify clean state: `git status` shows clean working tree on `main`. If not clean, stop.
- [ ] **Step 2.** Branch: `git checkout -b release/v0.1.1`.
- [ ] **Step 3.** Run baseline: `make test`. Expected: all packages PASS. Record coverage.
- [ ] **Step 4.** Create `internal/version/version.go` exporting three string vars (`Version`, `Commit`, `BuildDate`) defaulting to `"dev"`, `"none"`, `"unknown"`. These get set at build via `-ldflags "-X 'github.com/sarataha/warmrunners/internal/version.Version=$(VERSION)' ..."`.
- [ ] **Step 5.** Verify: `go build ./...`. Expected: build OK.
- [ ] **Step 6.** Commit: `chore(version): add version package for ldflags injection`.

---

## Task 2: CRD markers + validation (`api/v1alpha1/`)

Single grouped commit covers all CRD changes once the regenerated YAML matches the golden file.

**Files:**
- Modify: `api/v1alpha1/warmrunnerpolicy_types.go`
- Test (new or extend): `api/v1alpha1/warmrunnerpolicy_types_test.go`
- Test asset: `api/v1alpha1/testdata/golden_crd.yaml` (generated CRD snapshot)

**Acceptance criteria:**
- Regenerated CRD contains `x-kubernetes-list-type: map` and `x-kubernetes-list-map-keys: [type]` on `status.conditions`.
- `kubectl get warmrunnerpolicies` (or `wrp`) prints the `Age` column.
- `kubectl get warmrunners` returns warmrunnerpolicy resources (category lookup).
- Applying a CR with `floor.min > floor.max` is rejected by the API server.
- Applying a CR with `schedule[0].from >= schedule[0].to` is rejected.
- Applying a CR with `schedule[0].from = "24:00"` or `tz = "Mars/Phobos"` is rejected by pattern match.
- v0.1.0 example YAMLs in `examples/` still apply cleanly (golden compatibility).

- [ ] **Step 1: Write the failing tests first.** Extend `api/v1alpha1/warmrunnerpolicy_types_test.go` with table-driven envtest cases that POST CRs and expect Kubernetes API-server rejection for the four invalid cases above, plus acceptance for the existing `examples/` YAMLs. Use the package's existing envtest harness.
- [ ] **Step 2: Run tests, confirm RED.** `go test ./api/v1alpha1/... -run TestCRDValidation -v`. Expected: FAIL — current CRD lacks CEL rules and patterns, so currently-invalid CRs are accepted.
- [ ] **Step 3: Add the kubebuilder markers.** In `api/v1alpha1/warmrunnerpolicy_types.go`:
  - On `WarmRunnerPolicy` Kind block, add `+kubebuilder:resource:path=warmrunnerpolicies,singular=warmrunnerpolicy,shortName=wrp,categories={warmrunners}` and `+kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`` (keep existing Desired/Applied/Queue columns).
  - On `Status.Conditions` field, add the four upstream-canonical markers above the field:
    ```go
    // +patchMergeKey=type
    // +patchStrategy=merge
    // +listType=map
    // +listMapKey=type
    // +optional
    ```
    Keep the existing JSON tag `patchStrategy:"merge" patchMergeKey:"type"`.
  - On `FloorRange` struct, add `+kubebuilder:validation:XValidation:rule="self.min <= self.max",message="min must be <= max"`.
  - On `ScheduleWindow` struct, add `+kubebuilder:validation:XValidation:rule="self.from < self.to",message="from must be earlier than to"`.
  - On `ScheduleWindow.From` and `ScheduleWindow.To`, add `+kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$``.
  - On `ScheduleWindow.TZ`, add `+kubebuilder:validation:Pattern=`^[A-Za-z]+(?:/[A-Za-z0-9_+\-]+){0,2}$`` and `+kubebuilder:validation:MaxLength=64`.
  - On all free-form string fields (`Owner`, `Repository`, `Auth.SecretRef.Name`, `Auth.SecretRef.Key`, `Target.Arc.RunnerSet.Name`, `.Namespace`, `Target.Garm.Pool.Name`, `.Namespace`, label strings), add `+kubebuilder:validation:MaxLength=...` — 253 for DNS-style names, 64 for short identifiers.
- [ ] **Step 4: Regenerate.** `make manifests generate`. Confirm `config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml` updated; diff includes `x-kubernetes-list-type: map`, the CEL rules, patterns, and printer columns.
- [ ] **Step 5: Snapshot golden CRD.** Copy the regenerated CRD into `api/v1alpha1/testdata/golden_crd.yaml`. Add a golden-file test in `warmrunnerpolicy_types_test.go` that diffs the generated CRD against the golden via `os.ReadFile` + `bytes.Equal`. This guards against accidental regressions in `make manifests`.
- [ ] **Step 6: Run tests, confirm GREEN.** `go test ./api/v1alpha1/... -v`. Expected: PASS for every envtest case + golden-file match.
- [ ] **Step 7: Sanity-check downstream.** `make test`. Expected: still GREEN across the repo (controller tests use existing fixtures, no schema-incompatible objects).
- [ ] **Step 8: Verify existing examples still apply.** Apply `examples/arc-policy.yaml` and `examples/garm-policy.yaml` against an envtest server (the new test can wrap this). Expected: accepted.
- [ ] **Step 9: Commit.** `git add api/ config/crd/bases/ && git commit -S -m "feat(crd): tighten validation, add Age column, shortName, categories"` — single component commit per CLAUDE.local.md.

---

## Task 3: Controller — `ObservedGeneration` + `MaxConcurrentReconciles`

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller.go`
- Modify: `cmd/main.go`
- Test: `internal/controller/warmrunnerpolicy_controller_test.go`

**Acceptance criteria:**
- Every condition the reconciler writes carries `observedGeneration == policy.Generation` at decision time.
- `--max-concurrent-reconciles` flag exists with default `1` and is wired into `controller.Options`.
- `--log-level` flag exists with default `info`, accepted values `debug|info|warn|error`; controller and poller use `logr.V(1)` and `logr.V(2)` for verbose paths.
- All existing tests pass.

- [ ] **Step 1: Write failing test for ObservedGeneration.** Extend `warmrunnerpolicy_controller_test.go`: reconcile a policy with `.metadata.generation = 7`; assert every status condition has `ObservedGeneration == 7`. Use the existing envtest harness.
- [ ] **Step 2: Run, confirm RED.** `go test ./internal/controller/... -run TestObservedGeneration -v`. Expected: FAIL — current `setCondition` does not populate the field.
- [ ] **Step 3: Update `setCondition`.** Change the helper to take `generation int64` and assign it to the `metav1.Condition.ObservedGeneration` field before calling `meta.SetStatusCondition`. Update every call site to pass `pol.Generation`.
- [ ] **Step 4: Run, confirm GREEN.** Same command. Expected: PASS.
- [ ] **Step 5: Add `--max-concurrent-reconciles` flag.** In `cmd/main.go`, add a `flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 1, ...)`. Plumb it into the reconciler struct (`WarmRunnerPolicyReconciler.MaxConcurrentReconciles`). In `SetupWithManager`, pass `controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}` via `WithOptions(...)`.
- [ ] **Step 6: Smoke-test flag parses.** `go run ./cmd --max-concurrent-reconciles=4 --help`. Expected: prints help, no parse error. (Behavioral parallelism not worth a flaky integration test per spec §3.)
- [ ] **Step 7: Add `--log-level` flag.** In `cmd/main.go`, replace the hardcoded `zap.UseDevMode(true)` with a configurable logger. Add `flag.StringVar(&logLevel, "log-level", "info", ...)`. Map the string to `zapcore.Level`. Switch the manager logger to production encoding with the chosen level.
- [ ] **Step 8: Verify logger init.** `go run ./cmd --log-level=debug` for a few seconds (or use a unit test on a `BuildLogger(level string)` helper if the manager init is awkward to test). Expected: no crash; manager log lines emit at the chosen level.
- [ ] **Step 9: Run full test suite.** `make test`. Expected: GREEN.
- [ ] **Step 10: Commit.** `git add internal/controller/ cmd/main.go && git commit -S -m "feat(controller): add MaxConcurrentReconciles + log-level flags; set ObservedGeneration on conditions"`.

---

## Task 4: Manager cleanup (`cmd/main.go`)

**Files:**
- Modify: `cmd/main.go`

**Acceptance criteria:**
- `LeaderElectionReleaseOnCancel: true` set on `manager.Options`, with inline comment documenting the safety assumption ("safe because main exits immediately after Start returns").
- The leftover cert-manager TODO block at the previously-noted `cmd/main.go:145-148` region is gone.
- No other behavior changes.

- [ ] **Step 1: Locate the comment.** `grep -n "cert-manager" cmd/main.go`. Confirm the leftover TODO scaffold block exists.
- [ ] **Step 2: Edit `cmd/main.go`.** Delete the cert-manager TODO comment block. In the `ctrl.Options{...}` literal passed to `ctrl.NewManager`, set `LeaderElectionReleaseOnCancel: true` and add an inline `// safe because main exits immediately after Start returns` comment.
- [ ] **Step 3: Build.** `go build ./cmd/...`. Expected: build OK.
- [ ] **Step 4: Run full suite.** `make test`. Expected: GREEN.
- [ ] **Step 5: Commit.** `git add cmd/main.go && git commit -S -m "chore(main): enable LeaderElectionReleaseOnCancel; remove cert-manager TODO"`.

---

## Task 5: GitHub poller (`internal/demand/github_poller.go`)

This is the largest component. Each behavior change has its own RED→GREEN cycle; one final commit groups them.

**Files:**
- Modify: `internal/demand/github_poller.go`
- Test: `internal/demand/github_poller_test.go`
- Modify: `cmd/main.go` (add `--github-http-timeout` flag)

**Acceptance criteria:**
- Every outgoing request carries `User-Agent: warmrunners/<version>` (using `internal/version.Version`).
- HTTP client timeout is configurable via `--github-http-timeout` (default `10s`).
- Poller caches ETag per `(owner, repo, endpoint)` and sends `If-None-Match` on subsequent requests; on `304 Not Modified`, the cached parsed response is reused without re-parsing.
- On `5xx` or transient network error: exponential backoff `min(2^n, 60s)` with jitter, max 3 retries.
- On `429`/`403` with `Retry-After`: sleep exactly the indicated seconds.
- On `403` with `x-ratelimit-remaining: 0`: sleep until `x-ratelimit-reset` (UTC epoch).
- `FIX C` resolved: request-build error is returned, not dropped.
- `FIX B` resolved or converted to a tracked `// LIMITATION:` comment with a linked issue.

- [ ] **Step 1: Stub a deterministic `httptest.Server` in the test file.** Server records inbound headers per request so we can assert.
- [ ] **Step 2: Write failing test — User-Agent.** Assert: every request hitting the stub server has `User-Agent` matching `^warmrunners/.+$`.
- [ ] **Step 3: Run, RED.** `go test ./internal/demand/... -run TestUserAgent -v`. Expected: FAIL.
- [ ] **Step 4: Add User-Agent.** In `github_poller.go`, after building the request, set `req.Header.Set("User-Agent", "warmrunners/" + version.Version)` (import the new `internal/version` package).
- [ ] **Step 5: GREEN.** Same command. PASS.
- [ ] **Step 6: Write failing test — HTTP timeout flag.** Assert `New(opts ...)` accepts an `httpTimeout time.Duration` and the resulting `http.Client.Timeout` matches. Add `--github-http-timeout` flag in `cmd/main.go`; wire it through. Re-run: RED → implement → GREEN.
- [ ] **Step 7: Write failing tests — ETag cache.** Three cases:
  1. First request returns `ETag: "abc"` + body. Next poll sends `If-None-Match: "abc"`. Server returns 304. Poller returns the *cached* parsed `DemandSnapshot` with no re-parse.
  2. After 304, next response with new `ETag: "def"` updates the cache.
  3. Different `(owner, repo, endpoint)` keys have independent cache entries (no cross-pollution).
- [ ] **Step 8: RED.** `go test ./internal/demand/... -run TestETag -v`. Expected: all three FAIL.
- [ ] **Step 9: Implement ETag cache.** Add an in-memory `map[cacheKey]struct{etag string; parsed DemandSnapshot}` guarded by a mutex on the poller. On request: send `If-None-Match` if a cached etag exists. On 200: store new etag + parsed body. On 304: return cached parsed value, log at `V(1)`.
- [ ] **Step 10: GREEN.** PASS for all three.
- [ ] **Step 11: Write failing tests — retry.** Cases:
  1. Server returns 500, 502, then 200. Poller eventually returns success; recorded request count = 3.
  2. Server returns 500 four times. Poller returns error after 3 retries; recorded request count = 4 (initial + 3 retries).
  3. Backoff durations measured between retries are non-decreasing and bounded by `60s`.
- [ ] **Step 12: RED.** `go test ./internal/demand/... -run TestRetry -v`. FAIL.
- [ ] **Step 13: Implement retry.** Wrap the request in a `for attempt := 0; attempt <= 3; attempt++` loop. On retryable status (5xx) or transient net errors, sleep `min(2^attempt * baseDelay, 60s) + jitter(0..baseDelay)`, retry. On non-retryable (4xx other than 429/rate-limit) or success, exit.
- [ ] **Step 14: GREEN.** PASS.
- [ ] **Step 15: Write failing test — Retry-After.** Server returns 429 with `Retry-After: 2` then 200. Assert wall-clock gap ≥ 2s and final result success.
- [ ] **Step 16: RED.** FAIL.
- [ ] **Step 17: Implement Retry-After handling.** On 429 or 403 with `Retry-After` header, sleep the exact duration before next attempt (do not apply exponential backoff for this branch).
- [ ] **Step 18: GREEN.** PASS.
- [ ] **Step 19: Write failing test — x-ratelimit-remaining: 0.** Server returns 403 with `X-RateLimit-Remaining: 0` and `X-RateLimit-Reset: <now+2s>`. Assert wall-clock sleep ≥ 2s.
- [ ] **Step 20: RED → implement → GREEN.** Same loop. The handler reads `x-ratelimit-reset` as UTC epoch seconds and sleeps until then.
- [ ] **Step 21: Resolve FIX C.** Locate the `FIX C` comment near `github_poller.go:48`. Return the request-build error instead of silently discarding. Add a unit test asserting an invalid URL surfaces a non-nil error to the caller.
- [ ] **Step 22: Resolve FIX B.** Locate `github_poller.go:139`. Either: (a) implement label filtering in the response loop, with a test asserting only matching jobs are counted; or (b) convert to a `// LIMITATION:` comment with a linked GitHub issue URL. Choose (a) if a 30-min implementation suffices; (b) otherwise.
- [ ] **Step 23: Final full-suite run.** `make test`. Expected: GREEN.
- [ ] **Step 24: Commit.** `git add internal/demand/ cmd/main.go && git commit -S -m "feat(demand): User-Agent, tunable timeout, ETag cache, retry+backoff, rate-limit respect"`.

---

## Task 6: Metrics

**Files:**
- Modify: `internal/controller/metrics.go`
- Test: `internal/controller/metrics_test.go`
- Modify: `internal/controller/warmrunnerpolicy_controller.go` (call new error counter)

**Acceptance criteria:**
- `warmrunners_build_info{version,commit,build_date}` gauge registered with constant value `1`. Labels populated from `internal/version`.
- `warmrunners_reconciliation_errors_total{policy,error_type}` counter registered.
- Reconciler increments the error counter on failures, labeled by error kind (`demand_source`, `adapter`, `status_update`).
- Existing four metrics from v0.1.0 continue to register.

- [ ] **Step 1: Write failing test — `build_info` registered.** Assert `prometheus.DefaultGatherer.Gather()` returns a metric family named `warmrunners_build_info` with one sample whose label set is `{version, commit, build_date}` and value `1`.
- [ ] **Step 2: RED → implement → GREEN.** Add a `prometheus.NewGaugeVec` in `metrics.go` and register it on package init. Initialize a single sample using `internal/version` values.
- [ ] **Step 3: Write failing test — `reconciliation_errors_total`.** Assert the counter family is registered. Then assert reconciler increments it on a forced poller failure (use the existing test fake to return an error); the resulting counter has `policy="example", error_type="demand_source"` with value `1`.
- [ ] **Step 4: RED → implement → GREEN.** Add the `CounterVec` in `metrics.go`. In the reconciler, on each error return path, call the counter with the appropriate `error_type` label. Map: poller errors → `demand_source`; adapter Get/Set errors → `adapter`; status patch errors → `status_update`.
- [ ] **Step 5: Run full suite.** `make test`. Expected: GREEN.
- [ ] **Step 6: Commit.** `git add internal/controller/ && git commit -S -m "feat(metrics): add build_info gauge and reconciliation_errors_total counter"`.

---

## Task 7: RBAC

**Files:**
- Modify: `config/rbac/role.yaml`
- Modify: `config/rbac/warmrunnerpolicy_viewer_role.yaml`
- Modify: `config/rbac/warmrunnerpolicy_editor_role.yaml`
- Modify: `config/rbac/warmrunnerpolicy_admin_role.yaml`

**Acceptance criteria:**
- `role.yaml` no longer lists `create` under `warmrunnerpolicies` verbs.
- Viewer role has label `rbac.authorization.k8s.io/aggregate-to-view: "true"`.
- Editor role has label `rbac.authorization.k8s.io/aggregate-to-edit: "true"`.
- Admin role has label `rbac.authorization.k8s.io/aggregate-to-admin: "true"`.
- `make test` still green.

- [ ] **Step 1: Inspect `role.yaml`.** `grep -n create config/rbac/role.yaml`. Confirm `warmrunnerpolicies` block lists `create`.
- [ ] **Step 2: Remove the verb.** Edit `role.yaml`; delete the `- create` line from the `warmrunnerpolicies` resource block (keep other verbs).
- [ ] **Step 3: Add aggregation labels.** Each of the three roles gets one label under `metadata.labels`:
  ```yaml
  rbac.authorization.k8s.io/aggregate-to-view: "true"   # viewer
  rbac.authorization.k8s.io/aggregate-to-edit: "true"   # editor
  rbac.authorization.k8s.io/aggregate-to-admin: "true"  # admin
  ```
- [ ] **Step 4: Verify generation is unaffected.** `make manifests`. The hand-edited files should not be regenerated (they were carved out previously per CLAUDE.md; verify diff shows only the four edits). If `make manifests` clobbers a hand edit, surface as a blocker — do not silently re-apply.
- [ ] **Step 5: Run full suite.** `make test`. Expected: GREEN.
- [ ] **Step 6: Commit.** `git add config/rbac/ && git commit -S -m "chore(rbac): drop unused create verb; add aggregation labels"`.

---

## Task 8: CI — `test.yml` (govulncheck + race detector)

**Files:**
- Modify: `.github/workflows/test.yml`
- Modify: `Makefile` (test target)

**Acceptance criteria:**
- `make test` runs with `-race -covermode=atomic`.
- `test.yml` runs `golang/govulncheck-action@v1.0.4` against `./...` on every push/PR.
- Workflow green on a sample run.

- [ ] **Step 1: Add `-race` to Makefile.** Edit the `test` target line. Change `go test $(shell go list ./... | grep -v /e2e) -coverprofile cover.out` to `go test -race -covermode=atomic $(shell go list ./... | grep -v /e2e) -coverprofile cover.out`. (If the Makefile uses shell-style `$(go list …)` instead of `$(shell …)`, only the flag additions apply.)
- [ ] **Step 2: Run locally.** `make test`. Expected: GREEN with race detector enabled. If any test races, fix the race — do not strip the flag.
- [ ] **Step 3: Add govulncheck step to `test.yml`.** Append a step after the existing `go test` step:
  ```yaml
  - name: govulncheck
    uses: golang/govulncheck-action@v1.0.4
    with:
      go-version-input: '1.25'
      go-package: ./...
  ```
- [ ] **Step 4: Validate YAML.** `yq eval '.' .github/workflows/test.yml >/dev/null`. Expected: no error.
- [ ] **Step 5: Commit.** `git add .github/workflows/test.yml Makefile && git commit -S -m "ci(test): enable race detector and govulncheck"`.
- [ ] **Step 6: Push branch and watch CI.** `git push -u origin release/v0.1.1`. Wait for `Tests` workflow. Expected: GREEN. If govulncheck flags a CVE in a dep, surface and decide whether to bump or accept.

---

## Task 9: CI — `release.yml` (cosign + SBOM + attest)

**Files:**
- Modify: `.github/workflows/release.yml`

**Acceptance criteria:**
- Job has `permissions: { contents: write, packages: write, id-token: write }`.
- `sigstore/cosign-installer@v4.1.2` installed before signing.
- Container image signed by digest: `cosign sign --yes ghcr.io/sarataha/warmrunners@<digest>`.
- Helm OCI chart signed by digest: `cosign sign --yes ghcr.io/sarataha/charts/warmrunners@<digest>`.
- SBOM generated via `anchore/sbom-action@v0.24.0` (SPDX-JSON), uploaded as release asset.
- `cosign attest --yes --predicate <sbom>.spdx.json --type spdxjson ghcr.io/sarataha/warmrunners@<digest>` step present.
- Existing tag/release behavior preserved.

- [ ] **Step 1: Add `id-token: write` to permissions.** At job level (or workflow level if no other jobs).
- [ ] **Step 2: Install cosign before build step.**
  ```yaml
  - uses: sigstore/cosign-installer@v4.1.2
  ```
- [ ] **Step 3: Capture image digest from `docker/build-push-action`.** Confirm the existing build step has an `id` (e.g. `id: build`); add one if missing. The digest output is `steps.build.outputs.digest`.
- [ ] **Step 4: Sign image after push.**
  ```yaml
  - name: Sign container image
    env:
      IMAGE: ghcr.io/sarataha/warmrunners
      DIGEST: ${{ steps.build.outputs.digest }}
    run: cosign sign --yes "${IMAGE}@${DIGEST}"
  ```
- [ ] **Step 5: Capture Helm chart digest.** In the existing `helm push` step, capture stdout/stderr and extract `Digest: sha256:...` into `$GITHUB_OUTPUT`. Give the step an `id` (e.g. `id: helm`).
- [ ] **Step 6: Sign Helm chart.**
  ```yaml
  - name: Sign Helm chart
    env:
      CHART_REF: ghcr.io/sarataha/charts/warmrunners
      DIGEST: ${{ steps.helm.outputs.digest }}
    run: cosign sign --yes "${CHART_REF}@${DIGEST}"
  ```
- [ ] **Step 7: Generate SBOM.**
  ```yaml
  - name: Generate SBOM (SPDX-JSON)
    uses: anchore/sbom-action@v0.24.0
    with:
      image: ghcr.io/sarataha/warmrunners@${{ steps.build.outputs.digest }}
      format: spdx-json
      output-file: warmrunners-${{ github.ref_name }}.spdx.json
      upload-release-assets: true
  ```
- [ ] **Step 8: Attest SBOM.**
  ```yaml
  - name: Attest SBOM to image
    env:
      IMAGE: ghcr.io/sarataha/warmrunners
      DIGEST: ${{ steps.build.outputs.digest }}
    run: |
      cosign attest --yes \
        --predicate warmrunners-${{ github.ref_name }}.spdx.json \
        --type spdxjson \
        "${IMAGE}@${DIGEST}"
  ```
- [ ] **Step 9: YAML validate.** `yq eval '.' .github/workflows/release.yml >/dev/null`.
- [ ] **Step 10: Dry-run via rc tag.** Push tag `v0.1.1-rc.1`. Watch `Release` workflow.
  - Verify a green run.
  - Pull image + chart by tag.
  - Run the two `cosign verify` commands documented in Task 11 (use the rc tag).
  - Run `cosign verify-attestation` to confirm SBOM attestation lands.
  - Confirm the `*.spdx.json` is attached to the rc release.
- [ ] **Step 11: Commit.** `git add .github/workflows/release.yml && git commit -S -m "ci(release): sign image + chart with cosign; attach + attest SBOM"`.

---

## Task 10: SECURITY.md + README "Verifying releases"

**Files:**
- Create: `SECURITY.md`
- Modify: `README.md`

**Acceptance criteria:**
- `SECURITY.md` at repo root with: supported versions, private-disclosure contact (Sara's GitHub email or a `security@` alias), triage commitment timing.
- `README.md` gains a "Verifying releases" section showing the exact `cosign verify` and `cosign verify-attestation` commands. Pin the OIDC issuer (`https://token.actions.githubusercontent.com`) and the certificate-identity regexp (`^https://github.com/sarataha/warmrunners/.github/workflows/release.yml@refs/tags/v.*$`).

- [ ] **Step 1: Draft `SECURITY.md`.** ~30 lines. Sections: "Supported versions" (table; v0.1.x supported), "Reporting a vulnerability" (private email, GitHub Security Advisories link), "Disclosure timeline" (acknowledge ≤7 days, fix target ≤30 days), "Out of scope" (no SLA on dependency CVEs in dev-only tooling).
- [ ] **Step 2: Verify GitHub Security tab.** After commit + push, confirm GitHub displays the policy under the repo's Security tab.
- [ ] **Step 3: Add README section.** Insert "Verifying releases" between Install and Backends. Contain two fenced `bash` blocks: one for `cosign verify` image+chart, one for `cosign verify-attestation` SBOM.
- [ ] **Step 4: Commit.** `git add SECURITY.md README.md && git commit -S -m "docs: add SECURITY.md and release verification instructions"`.

---

## Task 11: CHANGELOG + tag

**Files:**
- Modify: `CHANGELOG.md`

**Acceptance criteria:**
- `CHANGELOG.md` has a new `## [0.1.1] - <YYYY-MM-DD>` section grouped by Added / Changed / Fixed / Security.
- Existing `## [Unreleased]` section header retained at the top (above 0.1.1) for next development cycle.

- [ ] **Step 1: Write the section.** Add entries:
  - **Added**: Age printer column; `wrp` shortName; `warmrunners` category; CEL XValidation rules; HH:MM and TZ pattern validation; MaxLength bounds; `--max-concurrent-reconciles` flag; `--log-level` flag; `--github-http-timeout` flag; User-Agent on GitHub requests; ETag-cached conditional polling; retry + backoff with Retry-After / x-ratelimit-reset handling; `warmrunners_build_info`; `warmrunners_reconciliation_errors_total`; RBAC aggregation labels; race detector in `make test`; govulncheck CI step; cosign image and chart signing; SBOM generation and attestation; `SECURITY.md`; release-verification docs in README.
  - **Changed**: Conditions list semantics use `+listType=map`; status conditions now carry `ObservedGeneration`; `LeaderElectionReleaseOnCancel` enabled.
  - **Fixed**: GitHub poller error-handling (resolved FIX B and FIX C); removed cert-manager TODO scaffold from `cmd/main.go`.
  - **Security**: Image and chart artifacts signed with Sigstore keyless OIDC; SPDX-JSON SBOM attached and attested.
- [ ] **Step 2: Update the `[Unreleased]` and `[0.1.1]` link refs at the bottom of the file** to point at the right diff URLs.
- [ ] **Step 3: Commit.** `git add CHANGELOG.md && git commit -S -m "docs: add 0.1.1 changelog"`.

---

## Task 12: Rollout

- [ ] **Step 1: PR.** `gh pr create --base main --title "release: v0.1.1 polish" --body-file <a generated body listing the 10 commits>`. Per [`finishing-a-development-branch`](https://github.com/anthropics/superpowers/tree/main/skills/finishing-a-development-branch), use the merge-via-PR option not a local merge.
- [ ] **Step 2: Watch CI.** `Tests`, `Lint` workflows green; `test-e2e` + `test-chart` allowed to remain on nightly schedule (no need to gate this PR on them). Govulncheck step inside `Tests` green.
- [ ] **Step 3: Merge.** Regular merge commit, no squash (preserves the 10 component commits).
- [ ] **Step 4: Tag.** On `main`, `git tag -s v0.1.1 -m "v0.1.1"` then `git push origin v0.1.1`. Signed annotated tag.
- [ ] **Step 5: Release workflow.** Watch the `Release` workflow run from the tag.
  - Verify GHCR contains image and chart at the new tag.
  - Verify the GitHub release contains the SBOM file.
  - Run `cosign verify` against the image — must succeed.
  - Run `cosign verify` against the chart — must succeed.
  - Run `cosign verify-attestation` for the SBOM — must succeed.
  - `helm install warmrunners-v0.1.1 oci://ghcr.io/sarataha/charts/warmrunners --version 0.1.1 --namespace warmrunners-test --create-namespace` on a fresh kind cluster.
  - Apply `examples/arc-policy.yaml`. Confirm the controller comes up, condition Age column shows, RBAC aggregates into the standard `view` role.
- [ ] **Step 6: Update [[warmrunners-next-steps]] handoff.** State now: v0.1.1 shipped; resume v0.2.0 brainstorm.

---

## Self-review notes

- **Spec coverage.** Spec §2.1 → Task 2. §2.2 → Task 3. §2.3 → Task 4. §2.4 → Task 5. §2.5 → Task 6. §2.6 → Task 7. §2.7 → Tasks 8 + 9. §2.8 → Tasks 10 + 11. §3 (Testing) covered inline in each task's RED→GREEN cycle. §4 (Compatibility) covered by Task 2 Step 8 (existing examples) and Task 12 Step 5 (helm upgrade smoke). §5 (Rollout) → Task 12. §6 (Deferred items) — out of scope by design.
- **Placeholders.** None — every step gives the file path, command, or exact YAML snippet for verified-mechanical pieces; Go implementation steps describe behavior + acceptance, not pseudocode.
- **Type consistency.** `MaxConcurrentReconciles` named identically in Task 3 (reconciler field, flag) and Task 5 deliberately doesn't introduce a new name. `setCondition` referenced once in Task 3.
- **Commit count.** 10 component commits + 1 PR + 1 tag — matches CLAUDE.local.md target (~14 for a v1 build; v0.1.1 is smaller scope).
