# warmrunners v0.5.0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make warmrunners react to `push` / `workflow_job` events via a GitHub App webhook instead of REST polling, cutting the poll-gap from ~30s to ~1s while keeping polling as the fallback path.

**Architecture:** New cluster-scoped `GitHubApp` CRD holds the App credentials + ingress mode. New `internal/webhook` package hosts the HMAC-verified receiver, LRU replay guard, and event dispatcher that feeds the existing `internal/activity` store. `WarmRunnerPolicy` gains `spec.githubAppRef` + `spec.activeWindowSeconds`; the reconciler consumes the event-fed activity when the ref is set and treats poll as fallback. Tunnel mode (WebSocket-out to a smee.io-style relay) is an optional path for private / dev clusters.

**Tech Stack:** Go 1.25, kubebuilder 4.9.0, controller-runtime, `unstructured.Unstructured` for adapters, `prometheus/client_golang`, `nhooyr.io/websocket` for the tunnel client (permissive, minimal), `envtest` for integration, `httptest` for GitHub stubs.

## Global Constraints

- Signed commits only (`git commit -S`). NEVER `--no-verify`. NEVER `--no-gpg-sign`.
- No Claude co-author trailers.
- Conventional Commits, scope = component (`feat(webhook):`, `feat(crd):`, `docs:`), never version.
- Component-grouped commits — one commit per green component, not per task.
- Branch: `feat/v0.5.0`. Three PRs stacked in order (PR1 → PR2 → PR3), each merged before the next.
- Pin every dep, tool, base image, Action to latest stable; justify exceptions in commit body.
- One responsibility per file; keep files small.
- Interface-first for new packages; reconciler must not know receiver implementation details.
- TDD enforced: no implementation code without a failing test first.
- Public names match spec verbatim (`GitHubApp`, `activeWindowSeconds`, `activeUntil`, `lastEventSource`).
- Never delete runners; never exceed `floor.max`; never patch on error; never break `Adapter` / `Scheduler` / `DemandSource` interfaces.
- No zencargo strings anywhere. Examples generic.
- Reuse the v0.3.x `internal/activity` package and the v0.2.x `internal/predictor` `WorkflowFetcher` cache — do not fork them.
- Manual kind exercise MANDATORY before `git tag -s v0.5.0` (per `CLAUDE.local.md`). CI green is not enough.

---

## File Structure

**New files:**

```
api/v1alpha1/githubapp_types.go              # GitHubApp CRD types
api/v1alpha1/githubapp_types_test.go         # types + defaulting tests
internal/webhook/receiver.go                 # HTTP handler mounted at /github/webhook
internal/webhook/receiver_test.go
internal/webhook/verify.go                   # HMAC-SHA256, constant-time compare
internal/webhook/verify_test.go
internal/webhook/replay.go                   # LRU of X-GitHub-Delivery
internal/webhook/replay_test.go
internal/webhook/dispatcher.go               # event → activity store bridge
internal/webhook/dispatcher_test.go
internal/webhook/tunnel.go                   # WebSocket-out relay client
internal/webhook/tunnel_test.go
internal/webhook/metrics.go                  # Prometheus series for the receiver
internal/controller/githubapp_controller.go  # reconciles GitHubApp: starts tunnel, tracks health
internal/controller/githubapp_controller_test.go
config/crd/bases/autoscaling.warmrunners.io_githubapps.yaml   # generated
config/rbac/githubapp_editor_role.yaml
config/rbac/githubapp_viewer_role.yaml
config/samples/autoscaling_v1alpha1_githubapp.yaml
docs/webhook.md                              # quickstart (ingress + tunnel modes)
test/e2e/webhook_livetest.sh                 # kind + smee.io + livetest repo
```

**Modified files:**

```
api/v1alpha1/warmrunnerpolicy_types.go       # add spec.githubAppRef, spec.activeWindowSeconds, status.activeUntil, status.lastEventSource
api/v1alpha1/zz_generated.deepcopy.go        # regenerated
config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml  # regenerated
config/rbac/role.yaml                        # +githubapps get/list/watch, +secrets get on referenced Secrets
internal/activity/activity.go                # add EventFeed interface (RecordPush, RecordJob)
internal/activity/event_feed.go              # new in-memory implementation
internal/controller/warmrunnerpolicy_controller.go   # read activeUntil, prefer event-fed activity, set lastEventSource, bump default poll
cmd/main.go                                  # start receiver, register GitHubApp controller, wire EventFeed
CHANGELOG.md                                 # v0.5.0 entry
Makefile                                     # webhook-test target for local receiver
README.md                                    # webhook section link
```

Rationale for split: receiver primitives (verify, replay, dispatch) live in one package because they share the same request; the tunnel is separate because it's swappable and I/O-shaped. The GitHubApp controller lives in `internal/controller/` next to the WRP controller so RBAC and manager wiring stays in one place.

---

# PR1 — `GitHubApp` CRD + receiver primitives

Branch: `feat/v0.5.0` (from `main`)
Component commits at PR end (in order):
1. `feat(crd): add GitHubApp CRD types and manifests`
2. `feat(webhook): HMAC verify and replay guard`
3. `feat(webhook): HTTP receiver and event dispatcher`

## Task 1: `GitHubApp` types

**Files:**
- Create: `api/v1alpha1/githubapp_types.go`
- Create: `api/v1alpha1/githubapp_types_test.go`
- Modify: `api/v1alpha1/zz_generated.deepcopy.go` (regenerated by `make generate`)

**Interfaces:**
- Produces: `GitHubApp`, `GitHubAppSpec`, `GitHubAppStatus`, `GitHubAppIngress`, `GitHubAppIngressMode`, `SecretKeyRef`, `TunnelSpec`, `Installation` — cluster-scoped, category `warmrunners`.

- [ ] **Step 1: Write failing test** — `TestGitHubAppTypes_DefaultsAndValidation` in `githubapp_types_test.go`. Assert: unset `ingress.mode` defaults to `"ingress"`; `appID <= 0` fails validation; `webhookSecretRef.name` required.

- [ ] **Step 2: Run test to verify it fails**
  ```
  go test ./api/v1alpha1/ -run TestGitHubAppTypes -v
  ```
  Expected: FAIL — `GitHubApp` undefined.

- [ ] **Step 3: Define types**

  Exact file body (kubebuilder markers included):

  ```go
  package v1alpha1

  import (
      metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
  )

  // +kubebuilder:validation:Enum=ingress;tunnel
  type GitHubAppIngressMode string

  const (
      IngressModeIngress GitHubAppIngressMode = "ingress"
      IngressModeTunnel  GitHubAppIngressMode = "tunnel"
  )

  type SecretKeyRef struct {
      // +kubebuilder:validation:Required
      Name string `json:"name"`
      // +kubebuilder:validation:Required
      Key  string `json:"key"`
      // +optional
      Namespace string `json:"namespace,omitempty"`
  }

  type TunnelSpec struct {
      // +kubebuilder:validation:Pattern=`^wss://.+`
      RelayURL string `json:"relayURL"`
  }

  type GitHubAppIngress struct {
      // +kubebuilder:default=ingress
      Mode GitHubAppIngressMode `json:"mode"`
      // +optional
      Hostname string `json:"hostname,omitempty"`
      // +optional
      Tunnel *TunnelSpec `json:"tunnel,omitempty"`
  }

  type GitHubAppSpec struct {
      // +kubebuilder:validation:Minimum=1
      AppID int64 `json:"appID"`
      // +kubebuilder:validation:Required
      PrivateKeyRef SecretKeyRef `json:"privateKeyRef"`
      // +kubebuilder:validation:Required
      WebhookSecretRef SecretKeyRef `json:"webhookSecretRef"`
      // +kubebuilder:validation:Required
      Ingress GitHubAppIngress `json:"ingress"`
  }

  type Installation struct {
      ID           int64  `json:"id"`
      Account      string `json:"account"`
      Repositories int32  `json:"repositories"`
  }

  type GitHubAppStatus struct {
      // +optional
      Installations []Installation `json:"installations,omitempty"`
      // +optional
      WebhookHealthy bool `json:"webhookHealthy,omitempty"`
      // +optional
      LastDelivery *metav1.Time `json:"lastDelivery,omitempty"`
      // +patchMergeKey=type
      // +patchStrategy=merge
      // +listType=map
      // +listMapKey=type
      // +optional
      Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
  }

  // +kubebuilder:object:root=true
  // +kubebuilder:resource:scope=Cluster,path=githubapps,singular=githubapp,shortName=gha,categories={warmrunners}
  // +kubebuilder:subresource:status
  // +kubebuilder:printcolumn:name="App",type=integer,JSONPath=`.spec.appID`
  // +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.ingress.mode`
  // +kubebuilder:printcolumn:name="Healthy",type=boolean,JSONPath=`.status.webhookHealthy`
  // +kubebuilder:printcolumn:name="LastDelivery",type=date,JSONPath=`.status.lastDelivery`
  type GitHubApp struct {
      metav1.TypeMeta   `json:",inline"`
      metav1.ObjectMeta `json:"metadata,omitempty"`
      Spec   GitHubAppSpec   `json:"spec,omitempty"`
      Status GitHubAppStatus `json:"status,omitempty"`
  }

  // +kubebuilder:object:root=true
  type GitHubAppList struct {
      metav1.TypeMeta `json:",inline"`
      metav1.ListMeta `json:"metadata,omitempty"`
      Items []GitHubApp `json:"items"`
  }

  func init() {
      SchemeBuilder.Register(&GitHubApp{}, &GitHubAppList{})
  }
  ```

- [ ] **Step 4: Regenerate deepcopy + CRD YAML**
  ```
  make generate
  make manifests
  ```
  Expected: creates `config/crd/bases/autoscaling.warmrunners.io_githubapps.yaml` and updates `zz_generated.deepcopy.go`.

- [ ] **Step 5: Rerun tests, expect PASS**
  ```
  go test ./api/v1alpha1/ -run TestGitHubAppTypes -v
  ```

- [ ] **Step 6: RBAC + samples**

  Create `config/rbac/githubapp_editor_role.yaml` and `config/rbac/githubapp_viewer_role.yaml` following the existing `warmrunnerpolicy_editor_role.yaml` / `_viewer_role.yaml` pattern (verbs `get,list,watch,create,update,patch,delete` for editor; `get,list,watch` for viewer; API group `autoscaling.warmrunners.io`, resource `githubapps`, sub-resource `githubapps/status`).

  Create `config/samples/autoscaling_v1alpha1_githubapp.yaml`:

  ```yaml
  apiVersion: autoscaling.warmrunners.io/v1alpha1
  kind: GitHubApp
  metadata:
    name: example-warmrunners-app
  spec:
    appID: 123456
    privateKeyRef:
      name: example-warmrunners-app-key
      key: private-key.pem
    webhookSecretRef:
      name: example-warmrunners-app-webhook
      key: secret
    ingress:
      mode: ingress
      hostname: warmrunners.example.io
  ```

- [ ] **Step 7: RBAC role update**

  Modify `config/rbac/role.yaml` — add resource `githubapps` (verbs `get,list,watch,update,patch`) and `githubapps/status` (verbs `get,update,patch`) under the same group. Add `secrets` (verbs `get,list,watch`) so the receiver can read the referenced private key + webhook secret.

## Task 2: HMAC verify

**Files:**
- Create: `internal/webhook/verify.go`
- Create: `internal/webhook/verify_test.go`

**Interfaces:**
- Produces: `VerifySignature(body []byte, sigHeader string, secret []byte) error` where `sigHeader` is the raw value of `X-Hub-Signature-256` (with the `sha256=` prefix). Returns `ErrMissingSignature`, `ErrInvalidSignature`, or nil.

- [ ] **Step 1: Failing tests** — `TestVerifySignature_ValidBody`, `TestVerifySignature_TamperedBody`, `TestVerifySignature_MissingHeader`, `TestVerifySignature_WrongPrefix`, `TestVerifySignature_ConstantTime`. The constant-time test asserts `hmac.Equal` is used (i.e. two mismatched signatures of equal length still return `ErrInvalidSignature`, not a length-based early exit).

- [ ] **Step 2: Run tests, expect FAIL** — `go test ./internal/webhook/ -run TestVerifySignature -v`

- [ ] **Step 3: Implement**

  Pseudocode for `verify.go`:

  ```
  const signaturePrefix = "sha256="

  var (
      ErrMissingSignature = errors.New("webhook: X-Hub-Signature-256 missing")
      ErrInvalidSignature = errors.New("webhook: X-Hub-Signature-256 invalid")
  )

  func VerifySignature(body []byte, sigHeader string, secret []byte) error {
      if sigHeader == "" { return ErrMissingSignature }
      if !strings.HasPrefix(sigHeader, signaturePrefix) { return ErrInvalidSignature }
      got, err := hex.DecodeString(strings.TrimPrefix(sigHeader, signaturePrefix))
      if err != nil { return ErrInvalidSignature }
      mac := hmac.New(sha256.New, secret)
      mac.Write(body)
      want := mac.Sum(nil)
      if !hmac.Equal(got, want) { return ErrInvalidSignature }
      return nil
  }
  ```

- [ ] **Step 4: Run tests, expect PASS**

## Task 3: LRU replay guard

**Files:**
- Create: `internal/webhook/replay.go`
- Create: `internal/webhook/replay_test.go`

**Interfaces:**
- Produces: `type ReplayGuard struct { ... }`; `NewReplayGuard(size int, ttl time.Duration) *ReplayGuard`; `(*ReplayGuard).Seen(deliveryID string) bool` — returns true if already seen, false otherwise; entries expire when `now - insertedAt > ttl` OR when the LRU size cap forces eviction. Concurrency-safe (`sync.Mutex`).

- [ ] **Step 1: Failing tests** — `TestReplayGuard_FirstSeenReturnsFalse`, `TestReplayGuard_DuplicateReturnsTrue`, `TestReplayGuard_TTLExpiry` (uses a `nowFunc` shim), `TestReplayGuard_LRUEviction` (fills past `size`, oldest evicted), `TestReplayGuard_Concurrent` (100 goroutines against the same guard, no race under `-race`).

- [ ] **Step 2: Run — FAIL.** `go test ./internal/webhook/ -run TestReplayGuard -race -v`

- [ ] **Step 3: Implement**

  Use `container/list` for LRU order + `map[string]*list.Element` for lookup. Store `struct{ key string; insertedAt time.Time }` in each element. `Seen` path:

  ```
  Lock()
  if elem, ok := lookup[id]; ok {
      if now.Sub(elem.Value.insertedAt) <= ttl {
          Unlock(); return true
      }
      // expired: fall through, re-insert
      lru.Remove(elem); delete(lookup, id)
  }
  if lru.Len() >= size {
      oldest := lru.Back(); lru.Remove(oldest); delete(lookup, oldest.Value.key)
  }
  push to front; Unlock(); return false
  ```

  Default `size=10000`, default `ttl=24*time.Hour` (spec §2.2).

- [ ] **Step 4: Run — PASS with `-race`.**

## Task 4: Event dispatcher

**Files:**
- Create: `internal/webhook/dispatcher.go`
- Create: `internal/webhook/dispatcher_test.go`

**Interfaces:**
- Consumes: `activity.EventFeed` interface (defined in PR2 Task 8; in PR1 use a local stub interface `type eventFeed interface { RecordPush(repo, headSHA string); RecordJob(repo string, labels []string) }` and adapt in PR2).
- Produces: `type Dispatcher struct { feed eventFeed; parser Parser; log logr.Logger }`; `NewDispatcher(feed eventFeed, parser Parser, log logr.Logger) *Dispatcher`; `(*Dispatcher).Handle(ctx, eventType string, deliveryID string, body []byte) error`.

- [ ] **Step 1: Failing tests** — `TestDispatcher_PushExtendsActivity`, `TestDispatcher_WorkflowJobQueued`, `TestDispatcher_WorkflowJobNonQueuedIgnored`, `TestDispatcher_UnknownEventNoop`, `TestDispatcher_MalformedBodyReturnsError`.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement**

  Two payload structs (unmarshal only fields consumed):

  ```go
  type pushPayload struct {
      Ref        string `json:"ref"`
      After      string `json:"after"`
      Repository struct { FullName string `json:"full_name"` } `json:"repository"`
  }

  type workflowJobPayload struct {
      Action      string   `json:"action"`
      WorkflowJob struct {
          Labels []string `json:"labels"`
          RunID  int64    `json:"run_id"`
      } `json:"workflow_job"`
      Repository struct { FullName string `json:"full_name"` } `json:"repository"`
  }
  ```

  `Handle` switch table:

  ```
  case "push":
      unmarshal → feed.RecordPush(repo.FullName, p.After)
  case "workflow_job":
      unmarshal; if p.Action != "queued" { return nil }
      feed.RecordJob(repo.FullName, p.WorkflowJob.Labels)
  case "ping":
      return nil                       // GitHub health check on install
  default:
      return nil                        // silently ignored per spec §2
  ```

  `Parser` is `internal/predictor.WorkflowFetcher`-adjacent — for PR1 the dispatcher only records the event; per-SHA fanout refresh is wired in PR2 when the WRP controller is reconciled.

- [ ] **Step 4: Run — PASS.**

## Task 5: HTTP receiver

**Files:**
- Create: `internal/webhook/receiver.go`
- Create: `internal/webhook/receiver_test.go`

**Interfaces:**
- Consumes: `Dispatcher` (Task 4), `ReplayGuard` (Task 3), `VerifySignature` (Task 2), `AppLookup` — an interface `type AppLookup interface { Resolve(ctx context.Context, targetID string) (*v1alpha1.GitHubApp, secret []byte, err error) }` where `targetID` is `X-GitHub-Hook-Installation-Target-ID`.
- Produces: `type Receiver struct { ... }`; `NewReceiver(lookup AppLookup, guard *ReplayGuard, disp *Dispatcher, log logr.Logger) *Receiver`; `(*Receiver).ServeHTTP(w, r)`. Registered by `cmd/main.go` at `/github/webhook` on the manager's existing HTTP server (same port as `/metrics`, TLS terminated by Ingress).

- [ ] **Step 1: Failing tests using `httptest.NewRecorder`** — happy path (200), missing headers → 400, invalid signature → 401, replay → 200 (idempotent), body larger than 1 MiB → 413 (spec §6), unknown installation target → 404.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement**

  Handler steps in order (short-circuit on first failure, emit corresponding metric):

  1. Reject non-POST → 405.
  2. `http.MaxBytesReader(w, r.Body, 1<<20)`; read all; on `MaxBytesError` → 413, `body_too_large`.
  3. Read headers: `X-GitHub-Event`, `X-GitHub-Delivery`, `X-Hub-Signature-256`, `X-GitHub-Hook-Installation-Target-ID`. Missing any → 400.
  4. `AppLookup.Resolve(ctx, targetID)` → 404 + `unknown_app` on error.
  5. `VerifySignature(body, sigHeader, secret)` → 401 + `hmac_invalid` on error.
  6. `guard.Seen(deliveryID)` → 200 + `replay`, no dispatch.
  7. `Dispatcher.Handle(ctx, eventType, deliveryID, body)` → 500 on unexpected error, otherwise 200.
  8. Record `warmrunners_webhook_events_total{event, verified="true"}` on success and `warmrunners_webhook_lag_seconds` from `X-GitHub-Delivery` timestamp header when present.

- [ ] **Step 4: Run — PASS.**

## Task 6: Webhook metrics

**Files:**
- Create: `internal/webhook/metrics.go`

- [ ] **Step 1: Register series** (matches spec §5 verbatim). Use `promauto.With(metrics.Registry)` where `metrics.Registry` is the existing `sigs.k8s.io/controller-runtime/pkg/metrics.Registry`.

  ```go
  var (
      EventsTotal = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{
          Name: "warmrunners_webhook_events_total",
          Help: "GitHub webhook events accepted by warmrunners, by type and verification outcome.",
      }, []string{"event", "verified", "source_repo"})

      LagSeconds = promauto.With(metrics.Registry).NewHistogramVec(prometheus.HistogramOpts{
          Name:    "warmrunners_webhook_lag_seconds",
          Help:    "Delay from GitHub delivery timestamp to receiver dispatch.",
          Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30},
      }, []string{"event"})

      DeliveriesDropped = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{
          Name: "warmrunners_webhook_deliveries_dropped_total",
          Help: "Webhook deliveries dropped before dispatch, by reason.",
      }, []string{"reason"}) // reason: hmac_invalid | replay | unknown_app | body_too_large | malformed
  )
  ```

- [ ] **Step 2: No test file** — series names are asserted in the receiver test via `testutil.CollectAndCount`.

## PR1 commit sequence

- [ ] **Commit 1** — after Tasks 1 green + `make manifests` regenerated:
  ```
  git add api/v1alpha1/githubapp_types.go api/v1alpha1/githubapp_types_test.go \
          api/v1alpha1/zz_generated.deepcopy.go \
          config/crd/bases/autoscaling.warmrunners.io_githubapps.yaml \
          config/rbac/githubapp_editor_role.yaml config/rbac/githubapp_viewer_role.yaml \
          config/rbac/role.yaml \
          config/samples/autoscaling_v1alpha1_githubapp.yaml
  git commit -S -m "feat(crd): add GitHubApp CRD types and manifests"
  ```

- [ ] **Commit 2** — after Tasks 2 + 3 green:
  ```
  git add internal/webhook/verify.go internal/webhook/verify_test.go \
          internal/webhook/replay.go internal/webhook/replay_test.go
  git commit -S -m "feat(webhook): HMAC verify and replay guard"
  ```

- [ ] **Commit 3** — after Tasks 4 + 5 + 6 green:
  ```
  git add internal/webhook/dispatcher.go internal/webhook/dispatcher_test.go \
          internal/webhook/receiver.go internal/webhook/receiver_test.go \
          internal/webhook/metrics.go
  git commit -S -m "feat(webhook): HTTP receiver and event dispatcher"
  ```

- [ ] **Push + open PR1** targeting `main`. Title: `feat: GitHubApp CRD + webhook receiver primitives`. Wait for green CI + review before starting PR2.

---

# PR2 — WRP fields + activity feed + `GitHubApp` controller + tunnel

Branch continues on `feat/v0.5.0` (rebase on `main` if PR1 already merged).
Component commits at PR end:
1. `feat(crd): githubAppRef and activeWindow fields on WarmRunnerPolicy`
2. `feat(activity): event-fed feed adapter`
3. `feat(controller): GitHubApp controller with tunnel client`
4. `feat(controller): consume event-fed activity and set lastEventSource`
5. `feat(main): wire receiver and event feed`

## Task 7: WRP additions

**Files:**
- Modify: `api/v1alpha1/warmrunnerpolicy_types.go`

**Interfaces:**
- Produces: `WarmRunnerPolicySpec.GitHubAppRef *LocalObjectRef`, `WarmRunnerPolicySpec.ActiveWindowSeconds *int32` (default 600, min 60, max 3600), `WarmRunnerPolicyStatus.ActiveUntil *metav1.Time`, `WarmRunnerPolicyStatus.LastEventSource string` (`"webhook"` | `"poll"` | `""`). Also add const `LastEventSourceWebhook = "webhook"` and `LastEventSourcePoll = "poll"`.

- [ ] **Step 1: Failing test** — `TestWRP_ActiveWindowDefaultsTo600`, `TestWRP_ActiveWindowValidation`, `TestWRP_LastEventSourceEnum`.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Add fields** to `WarmRunnerPolicySpec`:

  ```go
  // +optional
  GitHubAppRef *LocalObjectRef `json:"githubAppRef,omitempty"`

  // ActiveWindowSeconds is the duration the activity floor is held at the
  // predicted fanout after the most recent push/workflow_job event.
  // +kubebuilder:default=600
  // +kubebuilder:validation:Minimum=60
  // +kubebuilder:validation:Maximum=3600
  // +optional
  ActiveWindowSeconds *int32 `json:"activeWindowSeconds,omitempty"`
  ```

  And to `WarmRunnerPolicyStatus`:

  ```go
  // +optional
  ActiveUntil *metav1.Time `json:"activeUntil,omitempty"`

  // LastEventSource records the origin of the most recent activity signal.
  // +kubebuilder:validation:Enum=webhook;poll
  // +optional
  LastEventSource string `json:"lastEventSource,omitempty"`
  ```

  Define `LocalObjectRef` if not already present:

  ```go
  type LocalObjectRef struct {
      // +kubebuilder:validation:Required
      Name string `json:"name"`
  }
  ```

- [ ] **Step 4: `make generate manifests`; tests PASS.**

## Task 8: `activity.EventFeed`

**Files:**
- Modify: `internal/activity/activity.go` (interface only)
- Create: `internal/activity/event_feed.go`
- Create: `internal/activity/event_feed_test.go`

**Interfaces:**
- Produces: new interface

  ```go
  // EventFeed accepts near-real-time webhook events and merges them into a
  // per-repo activity snapshot. The reconciler reads snapshots via Snapshot;
  // events arriving after Snapshot was called are visible on the next call.
  type EventFeed interface {
      RecordPush(repo, headSHA string)
      RecordJob(repo string, labels []string)
      // Snapshot returns the merged fanout per label-set for repo, plus the
      // timestamp of the most recent event. LastEvent is zero when no event
      // has been recorded.
      Snapshot(repo string) (perLabelSet map[string]int, lastEvent time.Time)
  }
  ```

- Also produce `NewInMemoryEventFeed(fetcher predictor.WorkflowFetcher, log logr.Logger) EventFeed` returning a struct that:
  - On `RecordPush`, asynchronously fetches the workflow YAML at `headSHA` via `fetcher`, parses the matrix fanout, and merges the per-label-set fanout into the repo entry.
  - On `RecordJob`, increments the per-label-set counter directly using the queued labels (deterministic ground truth).
  - Stores `map[string]repoState` under an `sync.RWMutex`; `repoState` = `{ perLabelSet map[string]int; lastEvent time.Time }`.
  - Coalesces bursts: identical `(repo, headSHA)` pairs within 5 seconds fetch only once (reuse `WorkflowFetcher`'s existing ETag cache — do not add a new cache).

- [ ] **Step 1: Failing tests** — `TestInMemoryEventFeed_RecordJobUpdatesSnapshot`, `TestInMemoryEventFeed_RecordPushMergesFanout`, `TestInMemoryEventFeed_SnapshotZeroWhenEmpty`, `TestInMemoryEventFeed_Concurrent_race`.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement** per §3.3 spec.

- [ ] **Step 4: Run — PASS with `-race`.**

## Task 9: `GitHubApp` controller

**Files:**
- Create: `internal/controller/githubapp_controller.go`
- Create: `internal/controller/githubapp_controller_test.go`

**Interfaces:**
- Consumes: `internal/webhook.TunnelClient` interface (Task 10).
- Produces: `type GitHubAppReconciler struct { client.Client; Tunnels *webhook.TunnelRegistry; ... }`; `(*GitHubAppReconciler).SetupWithManager(mgr)` registers on `GitHubApp` CR.

  Also produces `type TunnelRegistry` in `internal/webhook/tunnel.go` — thin wrapper over `map[string]TunnelClient` keyed by GitHubApp name with `Ensure(name, relayURL string) TunnelClient` and `Stop(name string)`. `Ensure` reuses an existing entry when both name and URL match; replaces on URL change; noop when already correct.

  Reconcile responsibilities (order):
  1. Fetch `GitHubApp` CR; if `spec.ingress.mode == "tunnel"`, call `Tunnels.Ensure(name, spec.ingress.tunnel.relayURL)`. If `mode == "ingress"`, call `Tunnels.Stop(name)`.
  2. Read the referenced Secrets (`privateKeyRef`, `webhookSecretRef`); on missing Secret, set `Ready=False, Reason=SecretMissing`.
  3. Update `status.webhookHealthy`: for tunnel mode, read `Tunnels.Get(name).Connected()`; for ingress mode, `time.Since(status.lastDelivery) < 15m`. Update `status.lastDelivery` by watching the shared `dispatcher.LastDeliveryPerApp()` map — populated by the receiver every accepted event.
  4. Requeue every 60s to refresh gauges regardless of event flow.

  Does NOT itself run the HTTP receiver — that's mounted once on the manager by `cmd/main.go` and dispatches based on the `AppLookup` (which walks the CR list via the reconciler's shared cache).

- [ ] **Step 1: Failing tests using controller-runtime envtest** — `TestGitHubAppController_TunnelStartedOnCreate`, `TestGitHubAppController_TunnelReplacedOnURLChange`, `TestGitHubAppController_SecretMissingSetsCondition`, `TestGitHubAppController_HealthGaugeReflectsDelivery`.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement.** Follow the existing WRP controller idioms in `internal/controller/warmrunnerpolicy_controller.go` for Reconcile shape, condition setting (`setCondition` helper), and RequeueAfter.

- [ ] **Step 4: Run — PASS.**

## Task 10: Tunnel WebSocket client

**Files:**
- Create: `internal/webhook/tunnel.go`
- Create: `internal/webhook/tunnel_test.go`

**Interfaces:**
- Produces:
  ```go
  type TunnelClient interface {
      // Start dials relayURL and delivers each event to the dispatcher until
      // ctx is cancelled. Auto-reconnects with exponential backoff (500ms →
      // 30s cap, full jitter). Returns nil on ctx.Done(), error on
      // unrecoverable auth failures.
      Start(ctx context.Context, relayURL string) error
      // Connected reports the last-known connection state (for the gauge).
      Connected() bool
  }

  func NewTunnelClient(disp *Dispatcher, log logr.Logger) TunnelClient
  ```

  Relay protocol: smee.io-compatible. Each frame is a JSON object with keys `x-github-event`, `x-github-delivery`, `x-hub-signature-256`, `x-github-hook-installation-target-id`, and `body` (already-parsed JSON — re-marshal for HMAC recheck). Every frame goes through the same `Dispatcher.Handle` path as HTTP requests — the tunnel is a *transport*, not a *bypass* of verify/replay.

- [ ] **Step 1: Failing tests** using an `httptest.Server` upgraded via `nhooyr.io/websocket`. Assert reconnect on server close, HMAC still enforced on tunnelled events, `Connected()` transitions.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement.** Add `nhooyr.io/websocket` to `go.mod` (latest stable, `go get`, commit `go.mod` + `go.sum` change with this task). Reconnect loop:

  ```
  backoff = 500ms
  for ctx not done:
      conn, err = websocket.Dial(ctx, relayURL, nil)
      if err != nil:
          sleep min(backoff, 30s) with full jitter
          backoff *= 2; continue
      backoff = 500ms
      setConnected(true)
      for msg := range conn.Read:
          decode frame → dispatch (reuses verify + replay)
      setConnected(false)
  ```

- [ ] **Step 4: Run — PASS.**

## Task 11: Reconciler wiring

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller.go`

Changes (surgical — do NOT restructure the reconciler):

- [ ] **Step 1: Failing test** in `activity_integration_test.go` — `TestReconciler_UsesEventFeedWhenAppRefSet`, `TestReconciler_FallsBackToPollWhenFeedStale`, `TestReconciler_ActiveUntilExtendedOnEvent`, `TestReconciler_ActiveUntilExpiryDropsFloor`, `TestReconciler_LastEventSourceReflectsOrigin`.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Add** — the reconciler dependency struct grows a new optional field:

  ```go
  EventFeed activity.EventFeed // nil when no GitHubApp is configured cluster-wide
  ```

  In the reconcile loop, at the spot where activity is currently sampled:

  ```
  if spec.GitHubAppRef != nil && r.EventFeed != nil:
      perLabelSet, lastEvent = r.EventFeed.Snapshot(fullName)
      windowSec = defaultOr(spec.ActiveWindowSeconds, 600)
      if !lastEvent.IsZero() and now.Sub(lastEvent) <= windowSec:
          status.ActiveUntil = &metav1.Time{Time: lastEvent.Add(windowSec)}
          status.LastEventSource = LastEventSourceWebhook
          activityContribution = sum(perLabelSet)
      else:
          fall through to poll sampler (existing code path)
          if poll returned events:
              status.LastEventSource = LastEventSourcePoll
              status.ActiveUntil = &metav1.Time{Time: now.Add(windowSec)}
  else:
      existing poll path unchanged
  ```

- [ ] **Step 4: Poll interval default bump** — when `spec.GitHubAppRef != nil` and `spec.QueueRule.PollInterval == 0` (zero value), treat the effective poll interval as `300 * time.Second`. Update the defaulting site in the reconciler (search for existing `defaultPollInterval` constant; add sibling `defaultPollIntervalWithApp = 300 * time.Second`).

- [ ] **Step 5: Run tests, expect PASS.**

## Task 12: `cmd/main.go` wiring

**Files:**
- Modify: `cmd/main.go`

- [ ] **Step 1: Test** — no direct test; verified via e2e in PR3. Include a compile-time check: `go build ./...` must succeed.

- [ ] **Step 2: Additions.** After the existing manager setup:

  ```
  eventFeed := activity.NewInMemoryEventFeed(workflowFetcher, log)
  replayGuard := webhook.NewReplayGuard(10_000, 24*time.Hour)
  appLookup := webhook.NewCachedAppLookup(mgr.GetClient(), log) // walks GitHubApp CRs, resolves target ID
  dispatcher := webhook.NewDispatcher(eventFeed, workflowFetcher, log)
  receiver := webhook.NewReceiver(appLookup, replayGuard, dispatcher, log)

  if err := mgr.AddMetricsServerExtraHandler("/github/webhook", receiver); err != nil {
      setupLog.Error(err, "unable to mount webhook receiver"); os.Exit(1)
  }

  tunnelReg := webhook.NewTunnelRegistry(func() webhook.TunnelClient {
      return webhook.NewTunnelClient(dispatcher, log)
  })
  if err := (&controller.GitHubAppReconciler{
      Client:   mgr.GetClient(),
      Tunnels:  tunnelReg,
  }).SetupWithManager(mgr); err != nil {
      setupLog.Error(err, "unable to create GitHubApp controller"); os.Exit(1)
  }

  wrpReconciler.EventFeed = eventFeed // pre-existing WRP reconciler struct
  ```

  Note: `AddMetricsServerExtraHandler` is controller-runtime v0.19+; confirm current pin covers it. If the pinned version is older, use `mgr.AddReadyzCheck` shape isn't right — instead mount via `mgr.GetWebhookServer().Register(...)` OR expose a separate `HealthProbeBindAddress`-adjacent HTTP server. Prefer `AddMetricsServerExtraHandler` when available.

- [ ] **Step 3: `go build ./...` clean.**

## PR2 commit sequence

- [ ] **Commit 4** — Task 7 green + `make generate manifests`:
  ```
  git add api/v1alpha1/warmrunnerpolicy_types.go api/v1alpha1/warmrunnerpolicy_types_test.go \
          api/v1alpha1/zz_generated.deepcopy.go \
          config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml
  git commit -S -m "feat(crd): githubAppRef and activeWindow fields on WarmRunnerPolicy"
  ```

- [ ] **Commit 5** — Task 8 green:
  ```
  git add internal/activity/activity.go internal/activity/event_feed.go internal/activity/event_feed_test.go
  git commit -S -m "feat(activity): event-fed feed adapter"
  ```

- [ ] **Commit 6** — Tasks 9 + 10 green:
  ```
  git add internal/controller/githubapp_controller.go internal/controller/githubapp_controller_test.go \
          internal/webhook/tunnel.go internal/webhook/tunnel_test.go \
          go.mod go.sum
  git commit -S -m "feat(controller): GitHubApp controller with tunnel client"
  ```

- [ ] **Commit 7** — Task 11 green:
  ```
  git add internal/controller/warmrunnerpolicy_controller.go internal/controller/activity_integration_test.go
  git commit -S -m "feat(controller): consume event-fed activity and set lastEventSource"
  ```

- [ ] **Commit 8** — Task 12 green (`go build ./...` clean):
  ```
  git add cmd/main.go
  git commit -S -m "feat(main): wire receiver and event feed"
  ```

- [ ] **Push + open PR2.** Title: `feat: event-fed activity + GitHubApp controller + tunnel`. Wait for green CI + review before starting PR3.

---

# PR3 — Docs, metrics polish, e2e livetest

Branch continues on `feat/v0.5.0`.
Component commits at PR end:
1. `feat(webhook): active-window and tunnel metrics`
2. `docs: v0.5.0 webhook quickstart and reference`
3. `test(e2e): kind + tunnel livetest exercise`

## Task 13: Remaining metrics

**Files:**
- Modify: `internal/webhook/metrics.go`
- Modify: `internal/controller/metrics.go` (existing)

- [ ] **Step 1: Failing test** — `TestMetrics_TunnelGaugeTransitions`, `TestMetrics_ActiveWindowGaugePerRepo`.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Add series (spec §5 remainder):**

  ```go
  TunnelConnected = promauto.With(metrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
      Name: "warmrunners_tunnel_connected",
      Help: "1 when the outbound tunnel client is connected to the relay, 0 otherwise.",
  }, []string{"app"})

  TunnelReconnects = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{
      Name: "warmrunners_tunnel_reconnects_total",
      Help: "Tunnel reconnect attempts by outcome.",
  }, []string{"app", "outcome"}) // outcome: success | failure

  ActiveWindowExpiries = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{
      Name: "warmrunners_active_window_expiries_total",
      Help: "Number of times a repo's active window expired and the activity floor dropped to zero.",
  }, []string{"repo"})

  ActiveWindowRemaining = promauto.With(metrics.Registry).NewGaugeVec(prometheus.GaugeOpts{
      Name: "warmrunners_active_window_seconds_remaining",
      Help: "Seconds remaining before the active window expires for a repo.",
  }, []string{"repo"})
  ```

  Wire `TunnelConnected` from `TunnelClient.Connected()` (poll every 5s from the GitHubApp reconciler). Wire `ActiveWindowExpiries` and `ActiveWindowRemaining` from the WRP reconciler.

- [ ] **Step 4: Run — PASS.**

## Task 14: Docs

**Files:**
- Create: `docs/webhook.md`
- Modify: `README.md` (link only)
- Modify: `CHANGELOG.md` (v0.5.0 entry)

- [ ] **Step 1: Write `docs/webhook.md`** covering:
  1. What the feature does (one paragraph, plain English).
  2. Ingress mode setup: register the App via the maintainer's install link, install on org, create `GitHubApp` CR with `mode: ingress` + `hostname`, apply an Ingress pointing at the manager Service on `/github/webhook`.
  3. Tunnel mode setup: `mode: tunnel` + `tunnel.relayURL: wss://smee.io/<channel>`. No Ingress needed. Emphasise: the smee.io URL IS the bearer secret — treat it as sensitive.
  4. Secret creation examples (private key, webhook secret).
  5. Reference from a `WarmRunnerPolicy` via `spec.githubAppRef.name`.
  6. Verifying it works: `kubectl get gha` printcolumns, `warmrunners_webhook_events_total`, `status.lastEventSource=webhook`.
  7. Poll fallback behaviour: raised default `pollInterval` to 300s when `githubAppRef` is set; runs continuously as safety net.
  8. Troubleshooting matrix: `hmac_invalid` reason → secret mismatch; `unknown_app` → wrong installation target ID; `body_too_large` → 1 MiB cap; `TunnelConnected=0` → check `warmrunners_tunnel_reconnects_total{outcome="failure"}`.

- [ ] **Step 2: Add link to `README.md`** — a single line under an existing "features" or "operating" list, e.g. `- [Event-driven pre-warm via GitHub App webhook](docs/webhook.md).`. Do not reformat surrounding content.

- [ ] **Step 3: `CHANGELOG.md`** — append a v0.5.0 section matching the prior v0.4.0 shape (Added / Changed / Deprecated / Fixed headers as used). Include: `Added: GitHubApp CRD, webhook receiver, tunnel mode, activeWindowSeconds, event-fed activity feed`, `Changed: default pollInterval becomes 300s when githubAppRef is set`, `Deprecated: nothing`.

- [ ] **Step 4: Copy pass** — read the finished `docs/webhook.md` back once and cut needless words (Strunk pass).

## Task 15: E2E livetest exercise

**Files:**
- Create: `test/e2e/webhook_livetest.sh`

- [ ] **Step 1: Script skeleton** (executable bash). Sequence, with the actual `kubectl` / `gh` commands inline:

  1. `kind create cluster --name wrp-v050`.
  2. `docker build -t warmrunners:v0.5.0-e2e .`; `kind load docker-image warmrunners:v0.5.0-e2e --name wrp-v050`.
  3. `make install` (CRDs — includes new `githubapps.yaml`).
  4. `make deploy IMG=warmrunners:v0.5.0-e2e`.
  5. Install ARC controller + a runner scale set targeting `sarataha/warmrunners-livetest`.
  6. Create `Secret` `gh-token` (livetest PAT — from `env -u GITHUB_TOKEN`-safe env var, prompted by script).
  7. Create `Secret`s for the App: `example-warmrunners-app-key` (from a local PEM path), `example-warmrunners-app-webhook` (from a local file with the shared secret).
  8. Apply a `GitHubApp` CR in tunnel mode using a smee.io channel (script prints the URL to open in a browser to register the channel first).
  9. Apply a `WarmRunnerPolicy` referencing the App with `spec.activeWindowSeconds: 300` (short for the test).
  10. `gh workflow run ci.yml -R sarataha/warmrunners-livetest` — trigger a workflow.
  11. Assert within 5s: `kubectl get wrp livetest -o jsonpath='{.status.activeUntil}'` non-empty AND `kubectl get wrp livetest -o jsonpath='{.status.lastEventSource}'` == `webhook`.
  12. Assert within 5s: `kubectl get autoscalingrunnerset -n arc-runners livetest-runners -o jsonpath='{.spec.minRunners}'` matches the parsed fanout.
  13. Kill tunnel: patch the CR to `mode: ingress` with an obviously-unreachable hostname → confirm within `2 * pollInterval` that `lastEventSource` flips to `poll`.
  14. Wait past `activeUntil` → confirm `minRunners` drops to 0.
  15. `kind delete cluster --name wrp-v050`. Revert any `config/manager/kustomization.yaml` mutation from `make deploy`.

- [ ] **Step 2: Add `set -euo pipefail`** at the top; make each assertion a `retry(fn, timeout=10s)` bash helper that polls until the condition holds or fails the script.

- [ ] **Step 3: Do NOT commit any Secret contents.** Script reads paths from env vars (`WEBHOOK_SECRET_FILE`, `APP_PRIVATE_KEY_FILE`, `LIVETEST_PAT`).

## PR3 commit sequence

- [ ] **Commit 9** — Task 13 green:
  ```
  git add internal/webhook/metrics.go internal/controller/metrics.go internal/webhook/metrics_test.go
  git commit -S -m "feat(webhook): active-window and tunnel metrics"
  ```

- [ ] **Commit 10** — Task 14:
  ```
  git add docs/webhook.md README.md CHANGELOG.md
  git commit -S -m "docs: v0.5.0 webhook quickstart and reference"
  ```

- [ ] **Commit 11** — Task 15:
  ```
  git add test/e2e/webhook_livetest.sh
  chmod +x test/e2e/webhook_livetest.sh
  git commit -S -m "test(e2e): kind + tunnel livetest exercise"
  ```

- [ ] **Push + open PR3.** Title: `feat: v0.5.0 metrics, docs, e2e livetest`. Wait for green CI + review.

---

# Post-merge — mandatory pre-tag exercise

**Do NOT `git tag -s v0.5.0` until this passes end-to-end.**

- [ ] **Step 1: Merge all three PRs to `main`.**
- [ ] **Step 2: Register the warmrunners GitHub App** (one-time, maintainer):
  - `Settings → Developer settings → GitHub Apps → New GitHub App`.
  - Homepage URL: repo URL. Webhook URL: `https://smee.io/<channel>` for now. Webhook secret: generate 32-byte random; store in a local Secret.
  - Permissions: `Actions: Read-only`, `Contents: Read-only`, `Metadata: Read-only`. Events: `push`, `workflow_job`.
  - Generate + download the private key PEM.
  - Publish install link.
- [ ] **Step 3: Run `test/e2e/webhook_livetest.sh` on your workstation** with all three env vars set. Full script must return 0.
- [ ] **Step 4: Verify metrics** on the running receiver:
  - `warmrunners_webhook_events_total{event="push",verified="true"} >= 1`
  - `warmrunners_webhook_lag_seconds` histogram observed
  - `warmrunners_tunnel_connected{app="example-warmrunners-app"} == 1`
  - `warmrunners_active_window_seconds_remaining{repo="sarataha/warmrunners-livetest"} > 0` during the window, `== 0` after
- [ ] **Step 5: Tear down + revert kustomization mutation.**
- [ ] **Step 6: Tag.**
  ```
  git checkout main && git pull
  git tag -s v0.5.0 -m "v0.5.0 event-driven pre-warm"
  git push --tags
  ```
  The release workflow builds + signs + pushes the multi-arch image and Helm chart.
- [ ] **Step 7: Verify the GitHub release** — auto-generated notes + install manifest attached, cosign signature present on the image. Preserve the "New Contributors" section if the auto-notes contain one.
- [ ] **Step 8: Revoke the livetest PAT** used for the exercise (open task #4).
