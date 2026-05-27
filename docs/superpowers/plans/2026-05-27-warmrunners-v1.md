# warmrunners v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship warmrunners v1 — a kubebuilder operator that adjusts the warm-floor on self-hosted GitHub Actions runner backends (ARC + GARM) using a schedule + queue-depth heuristic.

**Architecture:** Single CRD `WarmRunnerPolicy` reconciled by one controller. Three pluggable interfaces — `DemandSource` (GitHub REST poller), `Scheduler` (clock + queue heuristic), `Adapter` (ARC + GARM). Adapters patch third-party CRDs via `unstructured.Unstructured` to avoid vendoring large dependency trees. Reconciler never deletes runners — only adjusts the warm-floor field; backends drain naturally.

**Tech Stack:** Go 1.26 (latest stable), kubebuilder 4.9.0, controller-runtime (latest, scaffolded), `client-go`, `unstructured.Unstructured` for third-party CRDs, `prometheus/client_golang` for metrics, `httptest` + `envtest` (k8s 1.34) for testing.

---

## File Structure

```
warmrunners/
├── api/v1alpha1/
│   ├── warmrunnerpolicy_types.go        # CRD types + validation helpers
│   ├── groupversion_info.go             # kubebuilder-generated
│   └── zz_generated.deepcopy.go         # kubebuilder-generated
├── cmd/main.go                          # entry point (kubebuilder-generated, light edits)
├── internal/
│   ├── controller/
│   │   ├── warmrunnerpolicy_controller.go      # reconciler
│   │   └── warmrunnerpolicy_controller_test.go # envtest-based integration
│   ├── demand/
│   │   ├── demand.go                    # DemandSource interface + DemandSnapshot type
│   │   ├── github_poller.go             # GitHubRESTPoller impl
│   │   └── github_poller_test.go
│   ├── scheduler/
│   │   ├── scheduler.go                 # Scheduler interface + Decision type
│   │   ├── heuristic.go                 # clock + queue impl (HeuristicScheduler)
│   │   └── heuristic_test.go
│   └── adapter/
│       ├── adapter.go                   # Adapter interface
│       ├── arc_adapter.go               # ArcAdapter (unstructured)
│       ├── arc_adapter_test.go
│       ├── garm_adapter.go              # GarmAdapter (unstructured)
│       └── garm_adapter_test.go
├── config/                              # kubebuilder-generated manifests
├── examples/policy-arc.yaml             # sample policy targeting ARC
├── examples/policy-garm.yaml            # sample policy targeting GARM
├── Makefile                             # kubebuilder-generated + custom targets
├── PROJECT                              # kubebuilder
├── go.mod
├── go.sum
└── README.md                            # already exists
```

Each file has one responsibility. Adapter / DemandSource / Scheduler files are small and focused so they're easy to test in isolation.

---

## Phase 0 — Scaffolding

### Task 0.1: Initialize Go module

**Files:**
- Create: `go.mod`

- [ ] **Step 1: Initialize module**

```bash
cd ~/gh/warmrunners
go mod init github.com/sarataha/warmrunners
```

- [ ] **Step 2: Verify**

```bash
cat go.mod
```

Expected: `module github.com/sarataha/warmrunners` with Go directive.

- [ ] **Step 3: Commit**

```bash
git add go.mod
git commit -m "chore: init go module"
```

---

### Task 0.2: Scaffold kubebuilder project

**Files:**
- Create: `PROJECT`, `Makefile`, `cmd/main.go`, `config/*`, `hack/*`, `.dockerignore`, `Dockerfile`

- [ ] **Step 1: Scaffold**

```bash
cd ~/gh/warmrunners
kubebuilder init --domain warmrunners.io --repo github.com/sarataha/warmrunners --plugins go.kubebuilder.io/v4
```

- [ ] **Step 2: Sanity build**

```bash
make build
```

Expected: builds successfully, produces `bin/manager`.

- [ ] **Step 3: Commit**

```bash
git add .
git commit -m "chore: kubebuilder init"
```

---

### Task 0.3: Generate WarmRunnerPolicy API + controller scaffold

**Files:**
- Create: `api/v1alpha1/warmrunnerpolicy_types.go`, `api/v1alpha1/groupversion_info.go`, `internal/controller/warmrunnerpolicy_controller.go`, `config/crd/bases/warmrunners.io_warmrunnerpolicies.yaml`

- [ ] **Step 1: Create API + controller**

```bash
kubebuilder create api --group warmrunners --version v1alpha1 --kind WarmRunnerPolicy --resource --controller
```

- [ ] **Step 2: Generate CRD manifest**

```bash
make manifests
```

- [ ] **Step 3: Verify the empty controller compiles + tests run**

```bash
make test
```

Expected: passes (kubebuilder ships an empty test scaffold).

- [ ] **Step 4: Commit**

```bash
git add .
git commit -m "feat(api): scaffold WarmRunnerPolicy v1alpha1"
```

---

## Phase 1 — API types

### Task 1.1: Define WarmRunnerPolicy spec types

**Files:**
- Modify: `api/v1alpha1/warmrunnerpolicy_types.go`

- [ ] **Step 1: Replace generated spec with v1 fields**

```go
// api/v1alpha1/warmrunnerpolicy_types.go
package v1alpha1

import (
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GitHubConfig struct {
    // +kubebuilder:validation:Required
    Owner string `json:"owner"`
    // Required: v1 polls repo-level workflow runs (`/repos/{owner}/{repo}/...`).
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    Repository string `json:"repository"`
    // +kubebuilder:validation:MinItems=1
    Labels []string  `json:"labels"`
    Auth   AuthRef   `json:"auth"`
}

type AuthRef struct {
    SecretRef corev1.SecretKeySelector `json:"secretRef"`
}

type ArcTarget struct {
    RunnerSet RefNS `json:"runnerSet"`
}

type GarmTarget struct {
    Pool RefNS `json:"pool"`
}

type RefNS struct {
    Name      string `json:"name"`
    Namespace string `json:"namespace"`
}

// Exactly one of Arc or Garm MUST be set. Validated by the controller and at admission.
// +kubebuilder:validation:XValidation:rule="(has(self.arc) ? 1 : 0) + (has(self.garm) ? 1 : 0) == 1",message="exactly one of target.arc or target.garm must be set"
type Target struct {
    Arc  *ArcTarget  `json:"arc,omitempty"`
    Garm *GarmTarget `json:"garm,omitempty"`
}

type FloorRange struct {
    // +kubebuilder:validation:Minimum=0
    Min int32 `json:"min"`
    // +kubebuilder:validation:Minimum=0
    Max int32 `json:"max"`
}

type ScheduleWindow struct {
    // +kubebuilder:validation:MinItems=1
    Days []string `json:"days"`
    From string   `json:"from"` // "HH:MM"
    To   string   `json:"to"`   // "HH:MM"
    TZ   string   `json:"tz"`   // IANA name, e.g. "UTC", "Europe/London"
    // +kubebuilder:validation:Minimum=0
    Base int32 `json:"base"`
}

type HeadroomTier struct {
    // +kubebuilder:validation:Minimum=1
    WhenQueueAtLeast int32 `json:"whenQueueAtLeast"`
    // +kubebuilder:validation:Minimum=0
    AddRunners int32 `json:"addRunners"`
}

type QueueRule struct {
    // +kubebuilder:default="30s"
    PollInterval metav1.Duration `json:"pollInterval"`
    Headroom     []HeadroomTier  `json:"headroom,omitempty"`
    // +kubebuilder:default="2m"
    Cooldown metav1.Duration `json:"cooldown"`
}

type WarmRunnerPolicySpec struct {
    GitHub    GitHubConfig     `json:"github"`
    Target    Target           `json:"target"`
    Floor     FloorRange       `json:"floor"`
    Schedule  []ScheduleWindow `json:"schedule,omitempty"`
    QueueRule QueueRule        `json:"queueRule"`
}

type WarmRunnerPolicyStatus struct {
    DesiredFloor      int32        `json:"desiredFloor,omitempty"`
    AppliedFloor      int32        `json:"appliedFloor,omitempty"`
    LastQueueDepth    int32        `json:"lastQueueDepth,omitempty"`
    LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
    // LastDecreaseTime is when the floor was last lowered. The scheduler reads it to
    // rate-limit decreases via the cooldown; it must NOT be conflated with LastReconcileTime.
    LastDecreaseTime *metav1.Time `json:"lastDecreaseTime,omitempty"`
    Conditions       []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredFloor`
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=`.status.appliedFloor`
// +kubebuilder:printcolumn:name="Queue",type=integer,JSONPath=`.status.lastQueueDepth`
type WarmRunnerPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   WarmRunnerPolicySpec   `json:"spec,omitempty"`
    Status WarmRunnerPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WarmRunnerPolicyList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items []WarmRunnerPolicy `json:"items"`
}

func init() {
    SchemeBuilder.Register(&WarmRunnerPolicy{}, &WarmRunnerPolicyList{})
}
```

- [ ] **Step 2: Regenerate**

```bash
make manifests generate
```

- [ ] **Step 3: Build to catch syntax errors**

```bash
go build ./...
```

Expected: builds cleanly.

- [ ] **Step 4: Commit**

```bash
git add api/ config/
git commit -m "feat(api): WarmRunnerPolicy spec + status types"
```

---

### Task 1.2: Helper — `(*Target).Kind()` returns `"arc"`, `"garm"`, or empty

**Files:**
- Modify: `api/v1alpha1/warmrunnerpolicy_types.go`
- Create: `api/v1alpha1/warmrunnerpolicy_types_test.go`

- [ ] **Step 1: Write failing test**

```go
// api/v1alpha1/warmrunnerpolicy_types_test.go
package v1alpha1

import "testing"

func TestTargetKind(t *testing.T) {
    cases := []struct {
        name string
        in   Target
        want string
    }{
        {"arc only", Target{Arc: &ArcTarget{}}, "arc"},
        {"garm only", Target{Garm: &GarmTarget{}}, "garm"},
        {"both set", Target{Arc: &ArcTarget{}, Garm: &GarmTarget{}}, ""},
        {"none set", Target{}, ""},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            if got := c.in.Kind(); got != c.want {
                t.Fatalf("Kind() = %q, want %q", got, c.want)
            }
        })
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./api/v1alpha1/... -run TestTargetKind -v
```

Expected: FAIL — `Kind` undefined.

- [ ] **Step 3: Implement**

Append to `api/v1alpha1/warmrunnerpolicy_types.go`:

```go
// Kind returns "arc" or "garm" when exactly one target is set, "" otherwise.
func (t Target) Kind() string {
    arc, garm := t.Arc != nil, t.Garm != nil
    if arc && !garm {
        return "arc"
    }
    if garm && !arc {
        return "garm"
    }
    return ""
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./api/v1alpha1/... -run TestTargetKind -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/
git commit -m "feat(api): Target.Kind helper"
```

---

## Phase 2 — Scheduler (pure logic, no Kubernetes)

This phase is the heart of v1 — pure functions, deeply unit-tested.

### Task 2.1: Define `Scheduler` interface and types

**Files:**
- Create: `internal/scheduler/scheduler.go`

- [ ] **Step 1: Write the interface + types**

```go
// internal/scheduler/scheduler.go
package scheduler

import (
    "time"

    "github.com/sarataha/warmrunners/api/v1alpha1"
)

// Demand is the snapshot of GitHub-side load passed to the Scheduler.
type Demand struct {
    Queued  int32
    Running int32
}

// Decision is the Scheduler's output: the desired warm-floor.
type Decision struct {
    DesiredFloor int32
}

// Scheduler decides the desired warm-floor for a policy.
type Scheduler interface {
    Decide(spec v1alpha1.WarmRunnerPolicySpec, now time.Time, d Demand, currentApplied int32, lastDecreaseAt time.Time) Decision
}
```

- [ ] **Step 2: Build**

```bash
go build ./internal/scheduler/...
```

Expected: builds.

- [ ] **Step 3: Commit**

```bash
git add internal/scheduler/scheduler.go
git commit -m "feat(scheduler): interface + types"
```

---

### Task 2.2: HeuristicScheduler — base case, empty schedule, zero demand

**Files:**
- Create: `internal/scheduler/heuristic.go`
- Create: `internal/scheduler/heuristic_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/scheduler/heuristic_test.go
package scheduler

import (
    "testing"
    "time"

    "github.com/sarataha/warmrunners/api/v1alpha1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mustParseTime(t *testing.T, s string) time.Time {
    t.Helper()
    ts, err := time.Parse(time.RFC3339, s)
    if err != nil {
        t.Fatal(err)
    }
    return ts
}

func TestHeuristic_NoSchedule_NoDemand_ReturnsMin(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor:     v1alpha1.FloorRange{Min: 0, Max: 10},
        QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
    }
    got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
    if got.DesiredFloor != 0 {
        t.Fatalf("DesiredFloor = %d, want 0", got.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/scheduler/... -run TestHeuristic_NoSchedule -v
```

Expected: FAIL — `NewHeuristic` undefined.

- [ ] **Step 3: Minimal implementation**

```go
// internal/scheduler/heuristic.go
package scheduler

import (
    "time"

    "github.com/sarataha/warmrunners/api/v1alpha1"
)

type Heuristic struct{}

func NewHeuristic() *Heuristic { return &Heuristic{} }

func (h *Heuristic) Decide(spec v1alpha1.WarmRunnerPolicySpec, now time.Time, d Demand, currentApplied int32, lastDecreaseAt time.Time) Decision {
    return Decision{DesiredFloor: spec.Floor.Min}
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/scheduler/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/
git commit -m "feat(scheduler): Heuristic returns floor.Min on empty input"
```

---

### Task 2.3: Schedule window — return `base` inside window

**Files:**
- Modify: `internal/scheduler/heuristic.go`, `internal/scheduler/heuristic_test.go`

- [ ] **Step 1: Add failing test**

Append to `heuristic_test.go`:

```go
func TestHeuristic_InsideWindow_ReturnsBase(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
        Schedule: []v1alpha1.ScheduleWindow{{
            Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
        }},
        QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
    }
    // Wed 10:00 UTC
    got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
    if got.DesiredFloor != 3 {
        t.Fatalf("DesiredFloor = %d, want 3", got.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/scheduler/... -run TestHeuristic_InsideWindow -v
```

Expected: FAIL.

- [ ] **Step 3: Implement schedule lookup**

Replace `heuristic.go` body:

```go
package scheduler

import (
    "fmt"
    "time"

    "github.com/sarataha/warmrunners/api/v1alpha1"
)

type Heuristic struct{}

func NewHeuristic() *Heuristic { return &Heuristic{} }

func (h *Heuristic) Decide(spec v1alpha1.WarmRunnerPolicySpec, now time.Time, d Demand, currentApplied int32, lastDecreaseAt time.Time) Decision {
    base := scheduleBase(spec.Schedule, now)
    desired := base
    if desired < spec.Floor.Min {
        desired = spec.Floor.Min
    }
    if desired > spec.Floor.Max {
        desired = spec.Floor.Max
    }
    return Decision{DesiredFloor: desired}
}

var weekdayShort = map[time.Weekday]string{
    time.Sunday: "Sun", time.Monday: "Mon", time.Tuesday: "Tue",
    time.Wednesday: "Wed", time.Thursday: "Thu", time.Friday: "Fri", time.Saturday: "Sat",
}

func scheduleBase(windows []v1alpha1.ScheduleWindow, now time.Time) int32 {
    var best int32 = 0
    for _, w := range windows {
        loc, err := time.LoadLocation(w.TZ)
        if err != nil {
            continue
        }
        local := now.In(loc)
        if !containsDay(w.Days, weekdayShort[local.Weekday()]) {
            continue
        }
        if withinHHMM(local, w.From, w.To) {
            if w.Base > best {
                best = w.Base
            }
        }
    }
    return best
}

func containsDay(days []string, today string) bool {
    for _, d := range days {
        if d == today {
            return true
        }
    }
    return false
}

func withinHHMM(t time.Time, from, to string) bool {
    fh, fm := parseHM(from)
    th, tm := parseHM(to)
    h, m := t.Hour(), t.Minute()
    nowMin := h*60 + m
    fromMin := fh*60 + fm
    toMin := th*60 + tm
    return nowMin >= fromMin && nowMin < toMin
}

func parseHM(s string) (int, int) {
    var h, m int
    _, _ = fmt.Sscanf(s, "%d:%d", &h, &m)
    return h, m
}
```

(`heuristic.go` imports `fmt` and `time` directly — no wrapper file needed.)

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/scheduler/... -v
```

Expected: both tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/
git commit -m "feat(scheduler): schedule window matching with TZ"
```

---

### Task 2.4: Outside window → return 0

**Files:**
- Modify: `internal/scheduler/heuristic_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestHeuristic_OutsideWindow_ReturnsMin(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
        Schedule: []v1alpha1.ScheduleWindow{{
            Days: []string{"Mon"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
        }},
        QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
    }
    // Wed (no Mon window applies)
    got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
    if got.DesiredFloor != 0 {
        t.Fatalf("DesiredFloor = %d, want 0", got.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/scheduler/... -v
```

Expected: PASS (existing logic already handles this — no change needed).

- [ ] **Step 3: Commit**

```bash
git add internal/scheduler/
git commit -m "test(scheduler): cover outside-window case"
```

---

### Task 2.5: Queue headroom — highest matching tier wins

**Files:**
- Modify: `internal/scheduler/heuristic.go`, `internal/scheduler/heuristic_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestHeuristic_QueueHeadroom_HighestTierWins(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor: v1alpha1.FloorRange{Min: 0, Max: 50},
        Schedule: []v1alpha1.ScheduleWindow{{
            Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
        }},
        QueueRule: v1alpha1.QueueRule{
            Cooldown: metav1.Duration{Duration: time.Minute},
            Headroom: []v1alpha1.HeadroomTier{
                {WhenQueueAtLeast: 5, AddRunners: 3},
                {WhenQueueAtLeast: 15, AddRunners: 8},
            },
        },
    }
    // queue 20 → highest tier (15) wins → base 3 + 8 = 11
    got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{Queued: 20}, 0, time.Time{})
    if got.DesiredFloor != 11 {
        t.Fatalf("DesiredFloor = %d, want 11", got.DesiredFloor)
    }
}

func TestHeuristic_QueueHeadroom_NoTierMatches(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor: v1alpha1.FloorRange{Min: 0, Max: 50},
        Schedule: []v1alpha1.ScheduleWindow{{
            Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
        }},
        QueueRule: v1alpha1.QueueRule{
            Cooldown: metav1.Duration{Duration: time.Minute},
            Headroom: []v1alpha1.HeadroomTier{{WhenQueueAtLeast: 10, AddRunners: 5}},
        },
    }
    got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{Queued: 3}, 0, time.Time{})
    if got.DesiredFloor != 3 {
        t.Fatalf("DesiredFloor = %d, want 3", got.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/scheduler/... -run QueueHeadroom -v
```

Expected: FAIL (first test).

- [ ] **Step 3: Implement headroom logic**

In `heuristic.go`, replace the body of `Decide`:

```go
func (h *Heuristic) Decide(spec v1alpha1.WarmRunnerPolicySpec, now time.Time, d Demand, currentApplied int32, lastDecreaseAt time.Time) Decision {
    base := scheduleBase(spec.Schedule, now)
    headroom := bestHeadroom(spec.QueueRule.Headroom, d.Queued)
    desired := base + headroom
    if desired < spec.Floor.Min {
        desired = spec.Floor.Min
    }
    if desired > spec.Floor.Max {
        desired = spec.Floor.Max
    }
    return Decision{DesiredFloor: desired}
}

func bestHeadroom(tiers []v1alpha1.HeadroomTier, queued int32) int32 {
    var best int32 = 0
    var bestThreshold int32 = -1
    for _, t := range tiers {
        if queued >= t.WhenQueueAtLeast && t.WhenQueueAtLeast > bestThreshold {
            best = t.AddRunners
            bestThreshold = t.WhenQueueAtLeast
        }
    }
    return best
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/scheduler/... -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/
git commit -m "feat(scheduler): queue headroom (highest matching tier wins)"
```

---

### Task 2.6: Clamp to floor.Min / floor.Max

**Files:**
- Modify: `internal/scheduler/heuristic_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestHeuristic_ClampToMax(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor: v1alpha1.FloorRange{Min: 0, Max: 5},
        Schedule: []v1alpha1.ScheduleWindow{{
            Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
        }},
        QueueRule: v1alpha1.QueueRule{
            Cooldown: metav1.Duration{Duration: time.Minute},
            Headroom: []v1alpha1.HeadroomTier{{WhenQueueAtLeast: 1, AddRunners: 100}},
        },
    }
    got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{Queued: 50}, 0, time.Time{})
    if got.DesiredFloor != 5 {
        t.Fatalf("DesiredFloor = %d, want 5 (clamped to Max)", got.DesiredFloor)
    }
}

func TestHeuristic_ClampToMin(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor:     v1alpha1.FloorRange{Min: 2, Max: 10},
        QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
    }
    got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
    if got.DesiredFloor != 2 {
        t.Fatalf("DesiredFloor = %d, want 2 (clamped to Min)", got.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/scheduler/... -v
```

Expected: PASS (existing logic already clamps — no change).

- [ ] **Step 3: Commit**

```bash
git add internal/scheduler/
git commit -m "test(scheduler): cover clamp to min/max"
```

---

### Task 2.7: Cooldown — only the decrease direction is rate-limited

**Files:**
- Modify: `internal/scheduler/heuristic.go`, `internal/scheduler/heuristic_test.go`

- [ ] **Step 1: Add failing tests**

```go
func TestHeuristic_Cooldown_BlocksDecrease(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor:     v1alpha1.FloorRange{Min: 0, Max: 10},
        QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: 2 * time.Minute}},
    }
    now := mustParseTime(t, "2026-05-27T10:00:00Z")
    lastDec := now.Add(-30 * time.Second) // 30s ago, inside 2m cooldown
    // Current applied = 5, computed desired = 0 → should stay at 5 until cooldown lapses.
    got := h.Decide(spec, now, Demand{}, 5, lastDec)
    if got.DesiredFloor != 5 {
        t.Fatalf("DesiredFloor = %d, want 5 (cooldown blocks decrease)", got.DesiredFloor)
    }
}

func TestHeuristic_Cooldown_AllowsIncrease(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
        Schedule: []v1alpha1.ScheduleWindow{{
            Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 7,
        }},
        QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: 2 * time.Minute}},
    }
    now := mustParseTime(t, "2026-05-27T10:00:00Z")
    lastDec := now.Add(-10 * time.Second)
    got := h.Decide(spec, now, Demand{}, 3, lastDec)
    if got.DesiredFloor != 7 {
        t.Fatalf("DesiredFloor = %d, want 7 (increase always allowed)", got.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/scheduler/... -run Cooldown -v
```

Expected: first test fails (returns 0 instead of 5).

- [ ] **Step 3: Implement cooldown**

In `heuristic.go`, update `Decide`:

```go
func (h *Heuristic) Decide(spec v1alpha1.WarmRunnerPolicySpec, now time.Time, d Demand, currentApplied int32, lastDecreaseAt time.Time) Decision {
    base := scheduleBase(spec.Schedule, now)
    headroom := bestHeadroom(spec.QueueRule.Headroom, d.Queued)
    desired := base + headroom
    if desired < spec.Floor.Min {
        desired = spec.Floor.Min
    }
    if desired > spec.Floor.Max {
        desired = spec.Floor.Max
    }
    // Cooldown applies only to decreases.
    if desired < currentApplied {
        if now.Sub(lastDecreaseAt) < spec.QueueRule.Cooldown.Duration {
            desired = currentApplied
        }
    }
    return Decision{DesiredFloor: desired}
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/scheduler/... -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/
git commit -m "feat(scheduler): cooldown rate-limits decreases only"
```

---

### Task 2.8: TZ + DST sanity test

**Files:**
- Modify: `internal/scheduler/heuristic_test.go`

- [ ] **Step 1: Add test**

```go
func TestHeuristic_TZ_RespectsLocation(t *testing.T) {
    h := NewHeuristic()
    spec := v1alpha1.WarmRunnerPolicySpec{
        Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
        Schedule: []v1alpha1.ScheduleWindow{{
            // 09:00–17:00 New York time on a Friday.
            Days: []string{"Fri"}, From: "09:00", To: "17:00", TZ: "America/New_York", Base: 4,
        }},
        QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
    }
    // 2026-05-29 13:00 UTC = 09:00 EDT on Friday → inside window.
    got := h.Decide(spec, mustParseTime(t, "2026-05-29T13:00:00Z"), Demand{}, 0, time.Time{})
    if got.DesiredFloor != 4 {
        t.Fatalf("DesiredFloor = %d, want 4 (TZ-aware)", got.DesiredFloor)
    }
    // 2026-05-29 12:30 UTC = 08:30 EDT → before window.
    got2 := h.Decide(spec, mustParseTime(t, "2026-05-29T12:30:00Z"), Demand{}, 0, time.Time{})
    if got2.DesiredFloor != 0 {
        t.Fatalf("DesiredFloor = %d, want 0 (before window)", got2.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/scheduler/... -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/scheduler/
git commit -m "test(scheduler): TZ-aware window matching"
```

---

## Phase 3 — DemandSource (GitHub REST)

### Task 3.1: DemandSource interface

**Files:**
- Create: `internal/demand/demand.go`

- [ ] **Step 1: Write the interface**

```go
// internal/demand/demand.go
package demand

import "context"

type Snapshot struct {
    Queued  int32
    Running int32
}

type Source interface {
    CurrentDemand(ctx context.Context, owner, repository string, labels []string) (Snapshot, error)
}
```

- [ ] **Step 2: Build**

```bash
go build ./internal/demand/...
```

Expected: builds.

- [ ] **Step 3: Commit**

```bash
git add internal/demand/demand.go
git commit -m "feat(demand): Source interface + Snapshot"
```

---

### Task 3.2: GitHubRESTPoller — queued + running, single page

**Files:**
- Create: `internal/demand/github_poller.go`
- Create: `internal/demand/github_poller_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/demand/github_poller_test.go
package demand

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
)

func TestGitHubRESTPoller_CountsByStatus(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // /repos/{owner}/{repo}/actions/runs?status=queued
        // /repos/{owner}/{repo}/actions/runs?status=in_progress
        body := map[string]any{
            "total_count": func() int {
                if r.URL.Query().Get("status") == "queued" {
                    return 4
                }
                return 7
            }(),
            "workflow_runs": []any{},
        }
        json.NewEncoder(w).Encode(body)
    }))
    defer srv.Close()

    p := NewGitHubRESTPoller(srv.URL, "tok")
    snap, err := p.CurrentDemand(context.Background(), "org", "repo", nil)
    if err != nil {
        t.Fatal(err)
    }
    if snap.Queued != 4 || snap.Running != 7 {
        t.Fatalf("snap = %+v, want {4, 7}", snap)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/demand/... -v
```

Expected: FAIL — `NewGitHubRESTPoller` undefined.

- [ ] **Step 3: Implement**

```go
// internal/demand/github_poller.go
package demand

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
)

type GitHubRESTPoller struct {
    baseURL string
    token   string
    client  *http.Client
}

func NewGitHubRESTPoller(baseURL, token string) *GitHubRESTPoller {
    return &GitHubRESTPoller{baseURL: baseURL, token: token, client: http.DefaultClient}
}

type runsResp struct {
    TotalCount int `json:"total_count"`
}

func (p *GitHubRESTPoller) count(ctx context.Context, owner, repo, status string) (int32, error) {
    u := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=%s&per_page=1", p.baseURL, owner, repo, status)
    req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
    if p.token != "" {
        req.Header.Set("Authorization", "Bearer "+p.token)
    }
    req.Header.Set("Accept", "application/vnd.github+json")
    resp, err := p.client.Do(req)
    if err != nil {
        return 0, err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return 0, fmt.Errorf("github api: %s", resp.Status)
    }
    var body runsResp
    if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
        return 0, err
    }
    return int32(body.TotalCount), nil
}

func (p *GitHubRESTPoller) CurrentDemand(ctx context.Context, owner, repository string, _ []string) (Snapshot, error) {
    q, err := p.count(ctx, owner, repository, "queued")
    if err != nil {
        return Snapshot{}, err
    }
    r, err := p.count(ctx, owner, repository, "in_progress")
    if err != nil {
        return Snapshot{}, err
    }
    return Snapshot{Queued: q, Running: r}, nil
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/demand/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/demand/
git commit -m "feat(demand): GitHubRESTPoller counts queued + running"
```

---

### Task 3.3: GitHubRESTPoller — surface API errors

**Files:**
- Modify: `internal/demand/github_poller_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestGitHubRESTPoller_ErrorOnNon200(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        http.Error(w, "boom", http.StatusInternalServerError)
    }))
    defer srv.Close()

    p := NewGitHubRESTPoller(srv.URL, "tok")
    _, err := p.CurrentDemand(context.Background(), "org", "repo", nil)
    if err == nil {
        t.Fatal("expected error, got nil")
    }
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/demand/... -v
```

Expected: PASS (existing logic already returns error on non-200).

- [ ] **Step 3: Commit**

```bash
git add internal/demand/
git commit -m "test(demand): error path on non-200"
```

---

### Task 3.4: GitHubRESTPoller — auth header sent when token provided

**Files:**
- Modify: `internal/demand/github_poller_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestGitHubRESTPoller_SendsAuthHeader(t *testing.T) {
    var gotAuth string
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        gotAuth = r.Header.Get("Authorization")
        json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}})
    }))
    defer srv.Close()

    p := NewGitHubRESTPoller(srv.URL, "secret-token")
    _, err := p.CurrentDemand(context.Background(), "org", "repo", nil)
    if err != nil {
        t.Fatal(err)
    }
    if gotAuth != "Bearer secret-token" {
        t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
    }
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/demand/... -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/demand/
git commit -m "test(demand): auth header is sent"
```

---

## Phase 4 — Adapter interface + ArcAdapter

### Task 4.1: Adapter interface + RefNS resolution

**Files:**
- Create: `internal/adapter/adapter.go`

- [ ] **Step 1: Write the interface**

```go
// internal/adapter/adapter.go
package adapter

import (
    "context"

    "sigs.k8s.io/controller-runtime/pkg/client"
)

type Ref struct {
    Name      string
    Namespace string
}

type Adapter interface {
    GetFloor(ctx context.Context, ref Ref) (int32, error)
    SetFloor(ctx context.Context, ref Ref, floor int32) error
}

// New is the constructor signature shared by ArcAdapter and GarmAdapter.
type New func(c client.Client) Adapter
```

- [ ] **Step 2: Build**

```bash
go build ./internal/adapter/...
```

- [ ] **Step 3: Commit**

```bash
git add internal/adapter/adapter.go
git commit -m "feat(adapter): interface + Ref type"
```

---

### Task 4.2: ArcAdapter — read `minRunners` from `AutoscalingRunnerSet`

**Files:**
- Create: `internal/adapter/arc_adapter.go`
- Create: `internal/adapter/arc_adapter_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/adapter/arc_adapter_test.go
package adapter

import (
    "context"
    "testing"

    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// arcTestGVK is named distinctly from the implementation's package-level arcGVK var
// to avoid a same-package identifier collision.
func arcTestGVK() schema.GroupVersionKind {
    return schema.GroupVersionKind{Group: "actions.github.com", Version: "v1alpha1", Kind: "AutoscalingRunnerSet"}
}

func newArc(minRunners int64) *unstructured.Unstructured {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(arcTestGVK())
    u.SetName("prod-runners")
    u.SetNamespace("arc-system")
    _ = unstructured.SetNestedField(u.Object, minRunners, "spec", "minRunners")
    return u
}

func TestArcAdapter_GetFloor(t *testing.T) {
    obj := newArc(3)
    cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(obj).Build()
    a := NewArcAdapter(cl)
    got, err := a.GetFloor(context.Background(), Ref{Name: "prod-runners", Namespace: "arc-system"})
    if err != nil {
        t.Fatal(err)
    }
    if got != 3 {
        t.Fatalf("GetFloor = %d, want 3", got)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/adapter/... -v
```

Expected: FAIL — `NewArcAdapter` undefined.

- [ ] **Step 3: Implement ArcAdapter (GetFloor only for now)**

```go
// internal/adapter/arc_adapter.go
package adapter

import (
    "context"
    "fmt"

    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

var arcGVK = schema.GroupVersionKind{
    Group: "actions.github.com", Version: "v1alpha1", Kind: "AutoscalingRunnerSet",
}

type ArcAdapter struct {
    c client.Client
}

func NewArcAdapter(c client.Client) *ArcAdapter { return &ArcAdapter{c: c} }

func (a *ArcAdapter) GetFloor(ctx context.Context, ref Ref) (int32, error) {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(arcGVK)
    if err := a.c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, u); err != nil {
        return 0, err
    }
    v, found, err := unstructured.NestedInt64(u.Object, "spec", "minRunners")
    if err != nil {
        return 0, fmt.Errorf("spec.minRunners: %w", err)
    }
    if !found {
        return 0, nil
    }
    return int32(v), nil
}

func (a *ArcAdapter) SetFloor(ctx context.Context, ref Ref, floor int32) error {
    return fmt.Errorf("not implemented")
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/adapter/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/
git commit -m "feat(adapter): ArcAdapter.GetFloor reads minRunners"
```

---

### Task 4.3: ArcAdapter.SetFloor

**Files:**
- Modify: `internal/adapter/arc_adapter.go`, `internal/adapter/arc_adapter_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestArcAdapter_SetFloor(t *testing.T) {
    obj := newArc(2)
    cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(obj).Build()
    a := NewArcAdapter(cl)
    if err := a.SetFloor(context.Background(), Ref{Name: "prod-runners", Namespace: "arc-system"}, 6); err != nil {
        t.Fatal(err)
    }
    // Re-read and verify.
    got, _ := a.GetFloor(context.Background(), Ref{Name: "prod-runners", Namespace: "arc-system"})
    if got != 6 {
        t.Fatalf("post-Set GetFloor = %d, want 6", got)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/adapter/... -v
```

Expected: FAIL — SetFloor returns "not implemented".

- [ ] **Step 3: Implement SetFloor**

Replace `SetFloor` in `arc_adapter.go`:

```go
func (a *ArcAdapter) SetFloor(ctx context.Context, ref Ref, floor int32) error {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(arcGVK)
    if err := a.c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, u); err != nil {
        return err
    }
    if err := unstructured.SetNestedField(u.Object, int64(floor), "spec", "minRunners"); err != nil {
        return err
    }
    return a.c.Update(ctx, u)
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/adapter/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/
git commit -m "feat(adapter): ArcAdapter.SetFloor patches minRunners"
```

---

## Phase 5 — GarmAdapter

### Task 5.1: GarmAdapter.GetFloor / SetFloor

**Files:**
- Create: `internal/adapter/garm_adapter.go`
- Create: `internal/adapter/garm_adapter_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/adapter/garm_adapter_test.go
package adapter

import (
    "context"
    "testing"

    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// garmTestGVK is named distinctly from the implementation's package-level garmGVK var
// to avoid a same-package identifier collision.
func garmTestGVK() schema.GroupVersionKind {
    return schema.GroupVersionKind{Group: "garm-operator.mercedes-benz.com", Version: "v1beta1", Kind: "Pool"}
}

func newGarmPool(min int64) *unstructured.Unstructured {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(garmTestGVK())
    u.SetName("gcp-runner-m")
    u.SetNamespace("garm-operator-system")
    _ = unstructured.SetNestedField(u.Object, min, "spec", "minIdleRunners")
    return u
}

func TestGarmAdapter_GetFloor(t *testing.T) {
    cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(newGarmPool(2)).Build()
    g := NewGarmAdapter(cl)
    got, err := g.GetFloor(context.Background(), Ref{Name: "gcp-runner-m", Namespace: "garm-operator-system"})
    if err != nil {
        t.Fatal(err)
    }
    if got != 2 {
        t.Fatalf("GetFloor = %d, want 2", got)
    }
}

func TestGarmAdapter_SetFloor(t *testing.T) {
    cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(newGarmPool(0)).Build()
    g := NewGarmAdapter(cl)
    if err := g.SetFloor(context.Background(), Ref{Name: "gcp-runner-m", Namespace: "garm-operator-system"}, 5); err != nil {
        t.Fatal(err)
    }
    got, _ := g.GetFloor(context.Background(), Ref{Name: "gcp-runner-m", Namespace: "garm-operator-system"})
    if got != 5 {
        t.Fatalf("post-Set GetFloor = %d, want 5", got)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/adapter/... -v
```

Expected: FAIL — `NewGarmAdapter` undefined.

- [ ] **Step 3: Implement**

```go
// internal/adapter/garm_adapter.go
package adapter

import (
    "context"

    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

var garmGVK = schema.GroupVersionKind{
    Group: "garm-operator.mercedes-benz.com", Version: "v1beta1", Kind: "Pool",
}

type GarmAdapter struct {
    c client.Client
}

func NewGarmAdapter(c client.Client) *GarmAdapter { return &GarmAdapter{c: c} }

func (g *GarmAdapter) GetFloor(ctx context.Context, ref Ref) (int32, error) {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(garmGVK)
    if err := g.c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, u); err != nil {
        return 0, err
    }
    v, found, err := unstructured.NestedInt64(u.Object, "spec", "minIdleRunners")
    if err != nil || !found {
        return 0, err
    }
    return int32(v), nil
}

func (g *GarmAdapter) SetFloor(ctx context.Context, ref Ref, floor int32) error {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(garmGVK)
    if err := g.c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, u); err != nil {
        return err
    }
    if err := unstructured.SetNestedField(u.Object, int64(floor), "spec", "minIdleRunners"); err != nil {
        return err
    }
    return g.c.Update(ctx, u)
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/adapter/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/
git commit -m "feat(adapter): GarmAdapter Get/SetFloor on Pool.spec.minIdleRunners"
```

---

## Phase 6 — Reconciler

### Task 6.1: Wire reconciler — fetch policy, no-op when desired equals current

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller.go`
- Create: `internal/controller/warmrunnerpolicy_controller_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/controller/warmrunnerpolicy_controller_test.go
package controller

import (
    "context"
    "testing"
    "time"

    "github.com/sarataha/warmrunners/api/v1alpha1"
    "github.com/sarataha/warmrunners/internal/demand"
    "github.com/sarataha/warmrunners/internal/scheduler"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type stubDemand struct {
    s   demand.Snapshot
    err error
}

func (s stubDemand) CurrentDemand(_ context.Context, _, _ string, _ []string) (demand.Snapshot, error) {
    return s.s, s.err
}

func newARC(minRunners int64) *unstructured.Unstructured {
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(schema.GroupVersionKind{Group: "actions.github.com", Version: "v1alpha1", Kind: "AutoscalingRunnerSet"})
    u.SetName("prod-runners")
    u.SetNamespace("arc-system")
    _ = unstructured.SetNestedField(u.Object, minRunners, "spec", "minRunners")
    return u
}

func newPolicy() *v1alpha1.WarmRunnerPolicy {
    return &v1alpha1.WarmRunnerPolicy{
        ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
        Spec: v1alpha1.WarmRunnerPolicySpec{
            GitHub: v1alpha1.GitHubConfig{Owner: "org", Repository: "repo"},
            Target: v1alpha1.Target{Arc: &v1alpha1.ArcTarget{RunnerSet: v1alpha1.RefNS{Name: "prod-runners", Namespace: "arc-system"}}},
            Floor:  v1alpha1.FloorRange{Min: 0, Max: 10},
            QueueRule: v1alpha1.QueueRule{
                PollInterval: metav1.Duration{Duration: time.Minute},
                Cooldown:     metav1.Duration{Duration: time.Minute},
            },
        },
    }
}

func TestReconcile_NoChange_NoPatch(t *testing.T) {
    sch := runtime.NewScheme()
    _ = v1alpha1.AddToScheme(sch)
    arc := newARC(0)
    pol := newPolicy()
    cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

    r := &WarmRunnerPolicyReconciler{
        Client:    cl,
        Scheme:    sch,
        Scheduler: scheduler.NewHeuristic(),
        Demand:    stubDemand{},
    }
    _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p", Namespace: "default"}})
    if err != nil {
        t.Fatal(err)
    }
    // Floor was 0, desired is 0 → still 0.
    got := &unstructured.Unstructured{}
    got.SetGroupVersionKind(arc.GroupVersionKind())
    _ = cl.Get(context.Background(), types.NamespacedName{Name: "prod-runners", Namespace: "arc-system"}, got)
    v, _, _ := unstructured.NestedInt64(got.Object, "spec", "minRunners")
    if v != 0 {
        t.Fatalf("minRunners = %d, want 0", v)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/controller/... -v
```

Expected: FAIL — reconciler not wired.

- [ ] **Step 3: Implement reconciler**

Replace `internal/controller/warmrunnerpolicy_controller.go`:

```go
package controller

import (
    "context"
    "time"

    "github.com/sarataha/warmrunners/api/v1alpha1"
    "github.com/sarataha/warmrunners/internal/adapter"
    "github.com/sarataha/warmrunners/internal/demand"
    "github.com/sarataha/warmrunners/internal/scheduler"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

type WarmRunnerPolicyReconciler struct {
    client.Client
    Scheme    *runtime.Scheme
    Scheduler scheduler.Scheduler
    Demand    demand.Source
}

func (r *WarmRunnerPolicyReconciler) adapterFor(t v1alpha1.Target) (adapter.Adapter, adapter.Ref, bool) {
    switch t.Kind() {
    case "arc":
        return adapter.NewArcAdapter(r.Client), adapter.Ref{Name: t.Arc.RunnerSet.Name, Namespace: t.Arc.RunnerSet.Namespace}, true
    case "garm":
        return adapter.NewGarmAdapter(r.Client), adapter.Ref{Name: t.Garm.Pool.Name, Namespace: t.Garm.Pool.Namespace}, true
    }
    return nil, adapter.Ref{}, false
}

func (r *WarmRunnerPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var pol v1alpha1.WarmRunnerPolicy
    if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    ad, ref, ok := r.adapterFor(pol.Spec.Target)
    if !ok {
        return ctrl.Result{}, nil // invalid target; admission should catch this
    }

    snap, demErr := r.Demand.CurrentDemand(ctx, pol.Spec.GitHub.Owner, pol.Spec.GitHub.Repository, pol.Spec.GitHub.Labels)

    current, _ := ad.GetFloor(ctx, ref)

    // Cooldown reads LastDecreaseTime — the time the floor was last *lowered* — NOT
    // LastReconcileTime (which changes every poll and would block decreases forever).
    var lastDec time.Time
    if pol.Status.LastDecreaseTime != nil {
        lastDec = pol.Status.LastDecreaseTime.Time
    }

    dec := r.Scheduler.Decide(pol.Spec, time.Now(), scheduler.Demand{Queued: snap.Queued, Running: snap.Running}, current, lastDec)

    now := metav1.Now()
    applied := current
    if demErr == nil && dec.DesiredFloor != current {
        if err := ad.SetFloor(ctx, ref, dec.DesiredFloor); err != nil {
            return ctrl.Result{RequeueAfter: pol.Spec.QueueRule.PollInterval.Duration}, err
        }
        applied = dec.DesiredFloor
        // Stamp LastDecreaseTime only when the floor actually went down.
        if dec.DesiredFloor < current {
            pol.Status.LastDecreaseTime = &now
        }
    }

    pol.Status.DesiredFloor = dec.DesiredFloor
    pol.Status.AppliedFloor = applied
    pol.Status.LastQueueDepth = snap.Queued
    pol.Status.LastReconcileTime = &now
    if err := r.Status().Update(ctx, &pol); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{RequeueAfter: pol.Spec.QueueRule.PollInterval.Duration}, nil
}

func (r *WarmRunnerPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.WarmRunnerPolicy{}).Complete(r)
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/controller/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): reconcile loop wires Demand+Scheduler+Adapter"
```

---

### Task 6.2: Patch when desired != current

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestReconcile_PatchesWhenDesiredDiffers(t *testing.T) {
    sch := runtime.NewScheme()
    _ = v1alpha1.AddToScheme(sch)
    arc := newARC(0)
    pol := newPolicy()
    pol.Spec.Floor.Min = 4 // force desired >= 4
    cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

    r := &WarmRunnerPolicyReconciler{
        Client: cl, Scheme: sch,
        Scheduler: scheduler.NewHeuristic(),
        Demand:    stubDemand{},
    }
    if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p", Namespace: "default"}}); err != nil {
        t.Fatal(err)
    }
    got := &unstructured.Unstructured{}
    got.SetGroupVersionKind(arc.GroupVersionKind())
    _ = cl.Get(context.Background(), types.NamespacedName{Name: "prod-runners", Namespace: "arc-system"}, got)
    v, _, _ := unstructured.NestedInt64(got.Object, "spec", "minRunners")
    if v != 4 {
        t.Fatalf("minRunners = %d, want 4", v)
    }
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/controller/... -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/
git commit -m "test(controller): patches backend when desired differs"
```

---

### Task 6.3: Sets conditions on demand error

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller.go`, `internal/controller/warmrunnerpolicy_controller_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestReconcile_DemandError_SetsCondition(t *testing.T) {
    sch := runtime.NewScheme()
    _ = v1alpha1.AddToScheme(sch)
    arc := newARC(0)
    pol := newPolicy()
    cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

    r := &WarmRunnerPolicyReconciler{
        Client: cl, Scheme: sch,
        Scheduler: scheduler.NewHeuristic(),
        Demand:    stubDemand{err: context.DeadlineExceeded},
    }
    if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p", Namespace: "default"}}); err != nil {
        t.Fatal(err)
    }
    var got v1alpha1.WarmRunnerPolicy
    _ = cl.Get(context.Background(), types.NamespacedName{Name: "p", Namespace: "default"}, &got)
    found := false
    for _, c := range got.Status.Conditions {
        if c.Type == "DemandSourceAvailable" && c.Status == "False" {
            found = true
        }
    }
    if !found {
        t.Fatalf("DemandSourceAvailable=False condition not set; got %+v", got.Status.Conditions)
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/controller/... -v
```

Expected: FAIL.

- [ ] **Step 3: Implement condition setting**

In `Reconcile`, after the demand call, add:

```go
// (replace the existing demErr handling block)
setCondition(&pol, "DemandSourceAvailable", demErr == nil, errReason(demErr), errMsg(demErr))
```

Add helpers at the bottom of the file:

```go
func errReason(err error) string {
    if err == nil {
        return "OK"
    }
    return "Error"
}

func errMsg(err error) string {
    if err == nil {
        return ""
    }
    return err.Error()
}

func setCondition(p *v1alpha1.WarmRunnerPolicy, ctype string, ok bool, reason, msg string) {
    status := metav1.ConditionTrue
    if !ok {
        status = metav1.ConditionFalse
    }
    for i := range p.Status.Conditions {
        if p.Status.Conditions[i].Type == ctype {
            p.Status.Conditions[i].Status = status
            p.Status.Conditions[i].Reason = reason
            p.Status.Conditions[i].Message = msg
            p.Status.Conditions[i].LastTransitionTime = metav1.Now()
            return
        }
    }
    p.Status.Conditions = append(p.Status.Conditions, metav1.Condition{
        Type: ctype, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now(),
    })
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/controller/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): DemandSourceAvailable condition"
```

---

### Task 6.4: Skip patch when demand errored (use stale data)

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller_test.go`

- [ ] **Step 1: Add failing test**

```go
func TestReconcile_DemandError_DoesNotChangeFloor(t *testing.T) {
    sch := runtime.NewScheme()
    _ = v1alpha1.AddToScheme(sch)
    arc := newARC(5)
    pol := newPolicy()
    pol.Spec.Floor.Min = 0
    cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

    r := &WarmRunnerPolicyReconciler{
        Client: cl, Scheme: sch,
        Scheduler: scheduler.NewHeuristic(),
        Demand:    stubDemand{err: context.DeadlineExceeded},
    }
    _, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p", Namespace: "default"}})

    got := &unstructured.Unstructured{}
    got.SetGroupVersionKind(arc.GroupVersionKind())
    _ = cl.Get(context.Background(), types.NamespacedName{Name: "prod-runners", Namespace: "arc-system"}, got)
    v, _, _ := unstructured.NestedInt64(got.Object, "spec", "minRunners")
    if v != 5 {
        t.Fatalf("minRunners changed to %d during demand error; want 5", v)
    }
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/controller/... -v
```

Expected: PASS — existing logic already gates the patch on `demErr == nil`.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/
git commit -m "test(controller): no patch on demand error"
```

---

### Task 6.5: AdapterError condition on patch failure

**Files:**
- Modify: `internal/controller/warmrunnerpolicy_controller.go`, `internal/controller/warmrunnerpolicy_controller_test.go`

- [ ] **Step 1: Add failing test**

```go
type stubAdapter struct {
    floor int32
    err   error
}

func (s *stubAdapter) GetFloor(_ context.Context, _ adapter.Ref) (int32, error) {
    return s.floor, nil
}
func (s *stubAdapter) SetFloor(_ context.Context, _ adapter.Ref, _ int32) error {
    return s.err
}

func TestReconcile_AdapterError_SetsCondition(t *testing.T) {
    sch := runtime.NewScheme()
    _ = v1alpha1.AddToScheme(sch)
    pol := newPolicy()
    pol.Spec.Floor.Min = 3
    cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(pol).WithStatusSubresource(pol).Build()

    r := &WarmRunnerPolicyReconciler{
        Client: cl, Scheme: sch,
        Scheduler:   scheduler.NewHeuristic(),
        Demand:      stubDemand{},
        AdapterFunc: func(_ v1alpha1.Target) (adapter.Adapter, adapter.Ref, bool) {
            return &stubAdapter{err: context.Canceled}, adapter.Ref{}, true
        },
    }
    _, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p", Namespace: "default"}})

    var got v1alpha1.WarmRunnerPolicy
    _ = cl.Get(context.Background(), types.NamespacedName{Name: "p", Namespace: "default"}, &got)
    found := false
    for _, c := range got.Status.Conditions {
        if c.Type == "AdapterAvailable" && c.Status == "False" {
            found = true
        }
    }
    if !found {
        t.Fatalf("AdapterAvailable=False condition not set")
    }
}
```

- [ ] **Step 2: Run, expect fail**

```bash
go test ./internal/controller/... -v
```

Expected: FAIL — `AdapterFunc` field doesn't exist.

- [ ] **Step 3: Add `AdapterFunc` override + condition setting**

In `warmrunnerpolicy_controller.go`, add:

```go
type AdapterFactory func(t v1alpha1.Target) (adapter.Adapter, adapter.Ref, bool)
```

Add to the struct:

```go
AdapterFunc AdapterFactory
```

Replace `adapterFor` to use the override if set:

```go
func (r *WarmRunnerPolicyReconciler) adapterFor(t v1alpha1.Target) (adapter.Adapter, adapter.Ref, bool) {
    if r.AdapterFunc != nil {
        return r.AdapterFunc(t)
    }
    switch t.Kind() {
    case "arc":
        return adapter.NewArcAdapter(r.Client), adapter.Ref{Name: t.Arc.RunnerSet.Name, Namespace: t.Arc.RunnerSet.Namespace}, true
    case "garm":
        return adapter.NewGarmAdapter(r.Client), adapter.Ref{Name: t.Garm.Pool.Name, Namespace: t.Garm.Pool.Namespace}, true
    }
    return nil, adapter.Ref{}, false
}
```

Replace the SetFloor block in `Reconcile` (preserving the cooldown stamp + `applied` tracking
from the base reconcile, adding the `AdapterAvailable` condition):

```go
now := metav1.Now()
applied := current
var setErr error
if demErr == nil && dec.DesiredFloor != current {
    setErr = ad.SetFloor(ctx, ref, dec.DesiredFloor)
    if setErr == nil {
        applied = dec.DesiredFloor
        if dec.DesiredFloor < current { // stamp only when the floor actually dropped
            pol.Status.LastDecreaseTime = &now
        }
    }
}
setCondition(&pol, "DemandSourceAvailable", demErr == nil, errReason(demErr), errMsg(demErr))
setCondition(&pol, "AdapterAvailable", setErr == nil, errReason(setErr), errMsg(setErr))

pol.Status.DesiredFloor = dec.DesiredFloor
pol.Status.AppliedFloor = applied
pol.Status.LastQueueDepth = snap.Queued
pol.Status.LastReconcileTime = &now
if err := r.Status().Update(ctx, &pol); err != nil {
    return ctrl.Result{}, err
}
```

- [ ] **Step 4: Run, expect pass**

```bash
go test ./internal/controller/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): AdapterAvailable condition; AdapterFunc test seam"
```

---

## Phase 7 — Metrics

### Task 7.1: Prometheus gauges + counter

**Files:**
- Create: `internal/controller/metrics.go`
- Modify: `internal/controller/warmrunnerpolicy_controller.go`

- [ ] **Step 1: Add metrics file**

```go
// internal/controller/metrics.go
package controller

import (
    "github.com/prometheus/client_golang/prometheus"
    metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
    desiredFloor = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "warmrunners_desired_floor", Help: "Desired warm-floor."},
        []string{"policy", "target"},
    )
    appliedFloor = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "warmrunners_applied_floor", Help: "Applied warm-floor."},
        []string{"policy", "target"},
    )
    queueDepth = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "warmrunners_queue_depth", Help: "Observed GitHub queue depth."},
        []string{"policy"},
    )
    floorChanges = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "warmrunners_floor_change_total", Help: "Floor change events."},
        []string{"policy", "direction"},
    )
)

func init() {
    metricsserver.Registry.MustRegister(desiredFloor, appliedFloor, queueDepth, floorChanges)
}
```

- [ ] **Step 2: Emit metrics in `Reconcile`**

Insert at the end of `Reconcile`, before the return:

```go
labels := []string{pol.Name, pol.Spec.Target.Kind()}
desiredFloor.WithLabelValues(labels...).Set(float64(dec.DesiredFloor))
appliedFloor.WithLabelValues(labels...).Set(float64(dec.DesiredFloor))
queueDepth.WithLabelValues(pol.Name).Set(float64(snap.Queued))
if dec.DesiredFloor > current {
    floorChanges.WithLabelValues(pol.Name, "up").Inc()
} else if dec.DesiredFloor < current {
    floorChanges.WithLabelValues(pol.Name, "down").Inc()
}
```

- [ ] **Step 3: Build + run tests**

```bash
go test ./internal/controller/... -v
```

Expected: PASS (existing tests still pass; metrics emission is side-effect).

- [ ] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): prometheus metrics (desired/applied/queue/changes)"
```

---

## Phase 8 — Integration (envtest)

### Task 8.1: envtest harness for the reconciler

**Files:**
- Create: `internal/controller/suite_test.go`

- [ ] **Step 1: Add envtest setup**

```go
// internal/controller/suite_test.go
package controller

import (
    "context"
    "path/filepath"
    "testing"

    "github.com/sarataha/warmrunners/api/v1alpha1"
    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
    testEnv *envtest.Environment
    testCfg client.Client
)

func TestMain(m *testing.M) {
    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
    }
    cfg, err := testEnv.Start()
    if err != nil {
        panic(err)
    }
    sch := runtime.NewScheme()
    _ = v1alpha1.AddToScheme(sch)
    c, err := client.New(cfg, client.Options{Scheme: sch})
    if err != nil {
        panic(err)
    }
    testCfg = c
    code := m.Run()
    _ = testEnv.Stop()
    _ = ctrl.SetLogger
    _ = context.Background
    if code != 0 {
        panic("tests failed")
    }
}
```

- [ ] **Step 2: Run**

```bash
make envtest
go test ./internal/controller/... -tags integration -v
```

Expected: env starts, suite runs.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/
git commit -m "test(controller): envtest harness"
```

---

### Task 8.2: e2e — full reconcile against envtest API server with stub GitHub

**Files:**
- Create: `internal/controller/integration_test.go`

- [ ] **Step 1: Write the test**

```go
// internal/controller/integration_test.go
//go:build integration

package controller

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/sarataha/warmrunners/api/v1alpha1"
    "github.com/sarataha/warmrunners/internal/demand"
    "github.com/sarataha/warmrunners/internal/scheduler"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
    "k8s.io/apimachinery/pkg/runtime/schema"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
)

func TestIntegration_ARC_FullLoop(t *testing.T) {
    ctx := context.Background()
    // Create CRD-less unstructured "ARC" object directly via dynamic client.
    arc := &unstructured.Unstructured{}
    arc.SetGroupVersionKind(schema.GroupVersionKind{Group: "actions.github.com", Version: "v1alpha1", Kind: "AutoscalingRunnerSet"})
    // Note: in envtest you'd register the ARC CRD; for v1 we exercise the WarmRunnerPolicy path
    // and rely on fake client tests for the adapter-level checks.
    // This test focuses on the WarmRunnerPolicy reconcile against a real API server.

    pol := &v1alpha1.WarmRunnerPolicy{
        ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
        Spec: v1alpha1.WarmRunnerPolicySpec{
            GitHub: v1alpha1.GitHubConfig{Owner: "org", Repository: "repo"},
            Target: v1alpha1.Target{Arc: &v1alpha1.ArcTarget{RunnerSet: v1alpha1.RefNS{Name: "prod-runners", Namespace: "arc-system"}}},
            Floor:  v1alpha1.FloorRange{Min: 2, Max: 10},
            QueueRule: v1alpha1.QueueRule{
                PollInterval: metav1.Duration{Duration: time.Second},
                Cooldown:     metav1.Duration{Duration: time.Minute},
            },
        },
    }
    if err := testCfg.Create(ctx, pol); err != nil {
        t.Fatal(err)
    }

    // Stub GitHub.
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}})
    }))
    defer srv.Close()

    r := &WarmRunnerPolicyReconciler{
        Client:    testCfg,
        Scheme:    testCfg.Scheme(),
        Scheduler: scheduler.NewHeuristic(),
        Demand:    demand.NewGitHubRESTPoller(srv.URL, ""),
    }
    _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "p", Namespace: "default"}})
    if err != nil {
        t.Fatal(err)
    }
    var got v1alpha1.WarmRunnerPolicy
    if err := testCfg.Get(ctx, types.NamespacedName{Name: "p", Namespace: "default"}, &got); err != nil {
        t.Fatal(err)
    }
    if got.Status.DesiredFloor != 2 {
        t.Fatalf("status.DesiredFloor = %d, want 2 (floor.Min)", got.Status.DesiredFloor)
    }
}
```

- [ ] **Step 2: Run**

```bash
go test ./internal/controller/... -tags integration -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/
git commit -m "test(controller): envtest end-to-end reconcile"
```

---

## Phase 9 — Packaging & polish

### Task 9.1: Examples

**Files:**
- Create: `examples/policy-arc.yaml`, `examples/policy-garm.yaml`

- [ ] **Step 1: Write the ARC sample**

```yaml
# examples/policy-arc.yaml
apiVersion: autoscaling.warmrunners.io/v1alpha1
kind: WarmRunnerPolicy
metadata:
  name: example-arc
  namespace: default
spec:
  github:
    owner: my-org
    repository: my-repo
    labels: [self-hosted, linux, x64]
    auth:
      secretRef: { name: gh-token, key: token }
  target:
    arc:
      runnerSet:
        name: prod-runners
        namespace: arc-system
  floor: { min: 0, max: 50 }
  schedule:
    - days: [Mon, Tue, Wed, Thu, Fri]
      from: "08:00"
      to:   "19:00"
      tz:   "UTC"
      base: 3
  queueRule:
    pollInterval: 30s
    headroom:
      - { whenQueueAtLeast: 5,  addRunners: 3 }
      - { whenQueueAtLeast: 15, addRunners: 8 }
    cooldown: 2m
```

- [ ] **Step 2: Write the GARM sample**

```yaml
# examples/policy-garm.yaml
apiVersion: autoscaling.warmrunners.io/v1alpha1
kind: WarmRunnerPolicy
metadata:
  name: example-garm
  namespace: default
spec:
  github:
    owner: my-org
    labels: [self-hosted, linux, x64]
    auth:
      secretRef: { name: gh-token, key: token }
  target:
    garm:
      pool:
        name: gcp-runner-m
        namespace: garm-operator-system
  floor: { min: 0, max: 30 }
  schedule:
    - days: [Mon, Tue, Wed, Thu, Fri]
      from: "08:00"
      to:   "19:00"
      tz:   "UTC"
      base: 2
  queueRule:
    pollInterval: 30s
    cooldown: 2m
```

- [ ] **Step 3: Commit**

```bash
git add examples/
git commit -m "docs(examples): sample WarmRunnerPolicy for ARC and GARM"
```

---

### Task 9.2: GitHub Actions CI

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Add CI workflow**

```yaml
# .github/workflows/ci.yml
name: ci
on:
  push: { branches: [main] }
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.26' }
      - run: go vet ./...
      - run: go test ./... -race -count=1
      - run: make manifests generate
      - name: check generated files clean
        run: git diff --exit-code
```

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: go vet + tests + generated-file check"
```

---

### Task 9.3: Helm chart skeleton

**Files:**
- Create: `deploy/helm/warmrunners/Chart.yaml`, `deploy/helm/warmrunners/values.yaml`, `deploy/helm/warmrunners/templates/*`

- [ ] **Step 1: Use the kubebuilder-generated manifests**

```bash
make manifests
# Then hand-port config/crd/bases/* and config/manager/manager.yaml into templates/.
```

- [ ] **Step 2: Add Chart.yaml**

```yaml
apiVersion: v2
name: warmrunners
description: Predictive warm-floor controller for self-hosted GitHub Actions runners.
type: application
version: 0.1.0
appVersion: "0.1.0"
```

- [ ] **Step 3: Add values.yaml**

```yaml
image:
  repository: ghcr.io/sarataha/warmrunners
  tag: 0.1.0
  pullPolicy: IfNotPresent
resources:
  limits: { cpu: 500m, memory: 256Mi }
  requests: { cpu: 50m, memory: 64Mi }
serviceAccount:
  create: true
crd:
  install: true
```

- [ ] **Step 4: Smoke-test the chart**

```bash
helm lint deploy/helm/warmrunners
```

- [ ] **Step 5: Commit**

```bash
git add deploy/
git commit -m "feat(deploy): Helm chart skeleton"
```

---

### Task 9.4: README install instructions

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Set the install section to the published OCI chart**

```markdown
## Install

helm install warmrunners oci://ghcr.io/sarataha/charts/warmrunners --version 0.1.0

Then create a Secret with a GitHub token and a WarmRunnerPolicy (see examples/).
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs(readme): OCI chart install instructions"
```

---

### Task 9.5: Tag the release (CRD is v1alpha1 → stay in 0.x)

The release workflow (`.github/workflows/release.yml`) publishes the image + OCI chart and cuts the
GitHub release automatically when a `v*` tag is pushed. Tag the first release as `v0.1.0`, not
`v1.0.0` — `v1.0.0` is reserved for CRD graduation to `v1`.

- [ ] **Step 1: Tag + push (signed)**

```bash
git tag -s v0.1.0 -m "warmrunners v0.1.0"
git push origin v0.1.0   # release workflow does the rest
```

---

## Self-Review (run after writing the plan)

- [x] **Spec coverage** — every section of the design doc maps to a task:
  - `WarmRunnerPolicy` CRD → Phase 1
  - `Scheduler` interface + heuristic → Phase 2
  - `DemandSource` + GitHubRESTPoller → Phase 3
  - `Adapter` + ARC adapter → Phase 4
  - GARM adapter → Phase 5
  - Reconciler / error conditions / status → Phase 6
  - Prometheus metrics → Phase 7
  - envtest integration → Phase 8
  - Examples / Helm / CI / README / release → Phase 9
- [x] **No placeholders** — every TDD task carries real Go code; every command is exact.
- [x] **Type consistency** — `Demand`, `Snapshot`, `Decision`, `Ref`, `Adapter`, `Scheduler` named identically across tasks.
- [x] **Webhook + codebase-aware deliberately deferred** to later 0.x releases per spec §10. No corresponding tasks here, by design.

---

## Implementation notes (beyond this plan)

Behaviour the shipped operator adds on top of the task code above. These are extensions, not
corrections — the tasks remain the build sequence.

1. **Per-policy auth.** In production the reconciler resolves each policy's
   `spec.github.auth.secretRef` to a token and constructs `demand.NewGitHubRESTPoller("https://api.github.com", token)`
   per reconcile (the injectable `Demand` field is kept for tests). The token is trimmed
   (`strings.TrimSpace`) — Secrets created via `kubectl --from-file` carry a trailing newline that
   otherwise makes an invalid `Authorization` header. Missing secret → `DemandSourceAvailable=False`, no patch.
   `cmd/main.go` wires the reconciler with `Scheduler: scheduler.NewHeuristic()` and `Demand: nil`.

2. **RBAC.** `+kubebuilder:rbac` markers on `Reconcile` grant get/list/watch on secrets and
   get/update on the ARC/GARM resources; `make manifests` regenerates `config/rbac/role.yaml`.

3. **Backend-max clamp.** `Adapter.GetMax` reads the backend's own `maxRunners`/`maxIdleRunners`;
   the reconciler clamps `desired` to it so a `floor.max` larger than the backend cap can't produce
   a rejected patch.

4. **Label-aware demand.** The poller counts individual jobs whose `runs-on` labels are a superset
   of the policy labels (enumerate runs → jobs), so a label-scoped policy scales on its own queue,
   not the whole repo. Trade-off: N+1 GitHub calls per poll — acceptable at 30s+ intervals.

5. **Distribution + CI.** A release workflow publishes the multi-arch image and the OCI Helm chart
   to GHCR on a `v*` tag. Heavy e2e/chart workflows run nightly + on-demand (not every PR); fast
   unit/envtest + lint gate PRs. Workflows use least-privilege `permissions`, concurrency-cancel,
   and job timeouts. The `go` directive is pinned to 1.25 (newest the linter + Docker base support).

### Known limitations (backlog)

- **Overnight schedule windows** (`from` later than `to`, e.g. `22:00`→`06:00`) silently never match
  in `withinHHMM`. Not in v1 spec/tests. Add overnight support or CRD validation rejecting `to < from`.
- **Conflicting policies** on the same backend CR are not detected in v0.1.0 (last writer wins).
  Planned v0.3.0: a validating admission webhook.
- **Org-level demand** not supported (repo-level only); `repository` is required.
