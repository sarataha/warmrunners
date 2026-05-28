// internal/controller/activity_integration_test.go
//
// Reconciler ↔ Activity wiring tests (Tasks 8 + 9, v0.3.0). The Activity
// interface is stubbed; the focus is on how the reconciler folds a Sample
// into the floor via max(scheduled, predicted, activity), surfaces
// ActivityAvailable, and writes the new status fields.
//
// Mirrors predictor_integration_test.go intentionally — both legs follow
// the same shape (interface field, computeX helper, condition transitions,
// top-N populated status, stale label-set pruning).
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/activity"
	"github.com/sarataha/warmrunners/internal/predictor"
	"github.com/sarataha/warmrunners/internal/scheduler"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// stubActivity returns a canned Sample (and optional error). It records
// each call so tests can verify the reconciler skips the sampler when
// disabled and that the window + denylist arguments arrive correctly.
type stubActivity struct {
	sample       activity.Sample
	err          error
	calls        int
	lastOwner    string
	lastRepo     string
	lastToken    string
	lastWindow   time.Duration
	lastDenylist []string
}

func (s *stubActivity) Sample(_ context.Context, owner, repo, token string, window time.Duration, denylist []string) (activity.Sample, error) {
	s.calls++
	s.lastOwner = owner
	s.lastRepo = repo
	s.lastToken = token
	s.lastWindow = window
	s.lastDenylist = denylist
	return s.sample, s.err
}

// reconcileOnceWithActivity builds a reconciler with the supplied policy +
// activity stub and runs one Reconcile. Predictor is intentionally nil so
// the activity contribution is the only non-schedule signal — keeps each
// test scenario's expectations clean.
func reconcileOnceWithActivity(t *testing.T, pol *v1alpha1.WarmRunnerPolicy, a activity.Activity) *v1alpha1.WarmRunnerPolicy {
	t.Helper()
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	arc := newARC(0)
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
		Activity:  a,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: pol.Namespace}}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.WarmRunnerPolicy
	_ = cl.Get(context.Background(), types.NamespacedName{Name: pol.Name, Namespace: pol.Namespace}, &got)
	return &got
}

// 1. Activity contribution folded via max(): single matching label set.
func TestReconcile_ActivityFloor_FoldedViaMax(t *testing.T) {
	pol := newPolicy()
	pol.Name = "act-folded"
	pol.Spec.Floor.Max = 10
	pol.Spec.GitHub.Labels = []string{"gpu", "self-hosted"}
	stub := &stubActivity{sample: activity.Sample{PerLabelSet: map[string]int{
		// LabelSetKey is the sorted-unique comma-join; "gpu,self-hosted" matches.
		"gpu,self-hosted": 3,
	}}}
	got := reconcileOnceWithActivity(t, pol, stub)
	if got.Status.DesiredFloor != 3 {
		t.Fatalf("DesiredFloor = %d, want 3 (activity folded via max)", got.Status.DesiredFloor)
	}
	if got.Status.ActivityFloor != 3 {
		t.Fatalf("ActivityFloor = %d, want 3", got.Status.ActivityFloor)
	}
}

// 2. Activity contribution above floor.max is capped.
func TestReconcile_ActivityFloor_CappedAtFloorMax(t *testing.T) {
	pol := newPolicy()
	pol.Name = "act-capped"
	pol.Spec.Floor.Max = 10
	stub := &stubActivity{sample: activity.Sample{PerLabelSet: map[string]int{
		"self-hosted": 20,
	}}}
	got := reconcileOnceWithActivity(t, pol, stub)
	if got.Status.DesiredFloor != 10 {
		t.Fatalf("DesiredFloor = %d, want 10 (capped at floor.max)", got.Status.DesiredFloor)
	}
	// ActivityFloor is the raw contribution (pre-clamp). The clamp only
	// affects DesiredFloor; the activity signal itself is faithfully reported
	// so operators can see when their floor.max is binding.
	if got.Status.ActivityFloor != 20 {
		t.Fatalf("ActivityFloor = %d, want 20 (raw contribution, pre-clamp)", got.Status.ActivityFloor)
	}
}

// 3. Cooldown holds appliedFloor at 8 when activity drops to 0 on a later
// reconcile within the cooldown window — v0.1.0 cooldown semantics survive
// the new signal because Decide already returns currentApplied when a
// decrease is blocked, so max(decide, activity) cannot lower the floor.
func TestReconcile_ActivityFloor_DecreaseCooldownApplies(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	arc := newARC(0)
	pol := newPolicy()
	pol.Name = "act-cooldown"
	pol.Spec.Floor.Max = 10
	pol.Spec.QueueRule.Cooldown = metav1.Duration{Duration: 5 * time.Minute}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	stub := &stubActivity{sample: activity.Sample{PerLabelSet: map[string]int{"self-hosted": 8}}}
	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
		Activity:  stub,
	}
	// Reconcile A: activity contributes 8 → backend goes to 8.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	if v := arcFloor(t, cl); v != 8 {
		t.Fatalf("after raise: minRunners = %d, want 8", v)
	}
	// Stamp a recent LastDecreaseTime so the heuristic's cooldown gate fires.
	var snap v1alpha1.WarmRunnerPolicy
	_ = cl.Get(context.Background(), types.NamespacedName{Name: pol.Name, Namespace: "default"}, &snap)
	recent := metav1.NewTime(time.Now())
	snap.Status.LastDecreaseTime = &recent
	_ = cl.Status().Update(context.Background(), &snap)

	// Reconcile B: activity drops to 0; cooldown must hold appliedFloor at 8.
	stub.sample = activity.Sample{PerLabelSet: map[string]int{}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	if v := arcFloor(t, cl); v != 8 {
		t.Fatalf("within cooldown: minRunners = %d, want 8 (decrease must be blocked)", v)
	}
}

// 4. Spec.Activity.Enabled = false → Sample not called; ActivityAvailable
// condition is absent. Mirrors PredictorDisabled_NotCalled exactly: the
// disabled-signal contract is "silent, not False" — a False condition would
// page on-call for a config choice, which is wrong.
func TestReconcile_ActivityDisabled_NotCalled(t *testing.T) {
	pol := newPolicy()
	pol.Name = "act-disabled"
	pol.Spec.Activity = &v1alpha1.ActivityConfig{Enabled: false}
	stub := &stubActivity{sample: activity.Sample{PerLabelSet: map[string]int{"self-hosted": 9}}}
	got := reconcileOnceWithActivity(t, pol, stub)
	if stub.calls != 0 {
		t.Fatalf("activity sampler was called %d times when disabled; want 0", stub.calls)
	}
	if findCondition(got.Status.Conditions, "ActivityAvailable") != nil {
		t.Fatalf("ActivityAvailable condition should be absent when disabled")
	}
	if got.Status.ActivityFloor != 0 {
		t.Fatalf("ActivityFloor = %d, want 0 when disabled", got.Status.ActivityFloor)
	}
}

// 5. Sampler error → ActivityAvailable=False with SampleError reason;
// reconcile succeeds; desiredFloor still derives from schedule + predictor
// (here just schedule via floor.min, since predictor is nil).
func TestReconcile_ActivityAvailable_FalseOnError(t *testing.T) {
	pol := newPolicy()
	pol.Name = "act-err"
	pol.Spec.Floor.Min = 2 // schedule baseline must still drive the floor
	stub := &stubActivity{err: errors.New("rate limited")}
	got := reconcileOnceWithActivity(t, pol, stub)

	cond := findCondition(got.Status.Conditions, "ActivityAvailable")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("ActivityAvailable not False; got %+v", cond)
	}
	if cond.Reason != v1alpha1.ActivityConditionReasonSampleError {
		t.Fatalf("reason = %q, want %q", cond.Reason, v1alpha1.ActivityConditionReasonSampleError)
	}
	if got.Status.DesiredFloor != 2 {
		t.Fatalf("DesiredFloor = %d, want 2 (schedule baseline)", got.Status.DesiredFloor)
	}
	if got.Status.ActivityFloor != 0 {
		t.Fatalf("ActivityFloor = %d, want 0 on error", got.Status.ActivityFloor)
	}
}

// 6. Label-superset filter: only label sets ⊇ policy.GitHub.Labels contribute.
func TestReconcile_ActivityLabelSupersetMatching(t *testing.T) {
	pol := newPolicy()
	pol.Name = "act-supers"
	pol.Spec.Floor.Max = 50
	pol.Spec.GitHub.Labels = []string{"gpu"}
	stub := &stubActivity{sample: activity.Sample{PerLabelSet: map[string]int{
		"gpu,self-hosted": 2, // superset of {gpu} → contributes
		"ubuntu-latest":   5, // not a superset → excluded
	}}}
	got := reconcileOnceWithActivity(t, pol, stub)
	if got.Status.ActivityFloor != 2 {
		t.Fatalf("ActivityFloor = %d, want 2 (only superset matches contribute)", got.Status.ActivityFloor)
	}
	if got.Status.DesiredFloor != 2 {
		t.Fatalf("DesiredFloor = %d, want 2", got.Status.DesiredFloor)
	}
}

// 7. Status.ActivityFloor + ActivityLabelSets populated; top-N ordering
// deterministic (count desc, then LabelSetKey asc for ties).
func TestReconcile_ActivityLabelSets_TopNDeterministic(t *testing.T) {
	pol := newPolicy()
	pol.Name = "act-topn"
	pol.Spec.Floor.Max = 100
	pol.Spec.GitHub.Labels = []string{"self-hosted"}
	per := map[string]int{
		"self-hosted,a": 5,
		"self-hosted,b": 4,
		"self-hosted,c": 3,
		"self-hosted,d": 2,
		"self-hosted,e": 2, // tied with d → alphabetic key tiebreak
		"self-hosted,f": 1,
		"self-hosted,g": 1,
		"self-hosted,h": 1,
		"self-hosted,i": 1,
		"self-hosted,j": 1,
	}
	stub := &stubActivity{sample: activity.Sample{PerLabelSet: per}}
	got := reconcileOnceWithActivity(t, pol, stub)

	if got.Status.ActivityFloor != 21 {
		t.Fatalf("ActivityFloor = %d, want 21 (sum of all matched counts)", got.Status.ActivityFloor)
	}
	if n := len(got.Status.ActivityLabelSets); n != 8 {
		t.Fatalf("ActivityLabelSets len = %d, want 8 (top-N cap)", n)
	}
	want := []struct {
		labels []string
		count  int32
	}{
		{[]string{"self-hosted", "a"}, 5},
		{[]string{"self-hosted", "b"}, 4},
		{[]string{"self-hosted", "c"}, 3},
		{[]string{"self-hosted", "d"}, 2},
		{[]string{"self-hosted", "e"}, 2},
	}
	for i, w := range want {
		ls := got.Status.ActivityLabelSets[i]
		if ls.Count != w.count {
			t.Fatalf("[%d] count = %d, want %d", i, ls.Count, w.count)
		}
		if !equalStrSlices(ls.Labels, w.labels) {
			t.Fatalf("[%d] labels = %v, want %v", i, ls.Labels, w.labels)
		}
	}
}

// 8. Three signals together: schedule=2, predicted=4, activity=7 →
// desiredFloor=7 (max wins, no signal additively stacks).
func TestReconcile_AllThreeSignals_MaxWins(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	arc := newARC(0)
	pol := newPolicy()
	pol.Name = "three-signal-max"
	pol.Spec.Floor.Min = 2 // schedule baseline
	pol.Spec.Floor.Max = 100
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	predStub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{"self-hosted": 4}}}
	actStub := &stubActivity{sample: activity.Sample{PerLabelSet: map[string]int{"self-hosted": 7}}}

	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
		Predictor: predStub,
		Activity:  actStub,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.WarmRunnerPolicy
	_ = cl.Get(context.Background(), types.NamespacedName{Name: pol.Name, Namespace: "default"}, &got)
	if got.Status.DesiredFloor != 7 {
		t.Fatalf("DesiredFloor = %d, want 7 (max(schedule=2, predicted=4, activity=7))", got.Status.DesiredFloor)
	}
	if got.Status.PredictedFloor != 4 {
		t.Fatalf("PredictedFloor = %d, want 4", got.Status.PredictedFloor)
	}
	if got.Status.ActivityFloor != 7 {
		t.Fatalf("ActivityFloor = %d, want 7", got.Status.ActivityFloor)
	}
}
