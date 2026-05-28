// internal/controller/predictor_integration_test.go
//
// Reconciler ↔ Predictor wiring tests (Task 7, v0.2.0). The predictor
// interface is stubbed; the focus is on how the reconciler folds a
// Prediction into the floor, surfaces PredictorAvailable, and writes the
// new status fields.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/predictor"
	"github.com/sarataha/warmrunners/internal/scheduler"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// stubPredictor returns a canned Prediction (and optional error) for use
// in the reconciler tests below. It records each call so tests can verify
// the reconciler skips the predictor when disabled.
type stubPredictor struct {
	snap       predictor.Prediction
	err        error
	calls      int
	lastToken  string
	lastOwner  string
	lastRepo   string
	tokensSeen []string
}

func (s *stubPredictor) Predict(_ context.Context, owner, repo, token string) (predictor.Prediction, error) {
	s.calls++
	s.lastOwner = owner
	s.lastRepo = repo
	s.lastToken = token
	s.tokensSeen = append(s.tokensSeen, token)
	return s.snap, s.err
}

// reconcileOnce builds a reconciler with the supplied policy + predictor
// stub and runs one Reconcile. Returns the post-reconcile policy.
func reconcileOnce(t *testing.T, pol *v1alpha1.WarmRunnerPolicy, p predictor.Predictor) *v1alpha1.WarmRunnerPolicy {
	t.Helper()
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	arc := newARC(0)
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
		Predictor: p,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: pol.Namespace}}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.WarmRunnerPolicy
	_ = cl.Get(context.Background(), types.NamespacedName{Name: pol.Name, Namespace: pol.Namespace}, &got)
	return &got
}

// 1. Predicted floor folded via max(...).
func TestReconcile_PredictedFloor_FoldedViaMax(t *testing.T) {
	pol := newPolicy()
	pol.Name = "pred-folded"
	pol.Spec.Floor.Max = 10
	// 5 imminent jobs, all matching the (empty) policy label filter.
	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{
		"self-hosted,gpu": 3,
		"self-hosted":     2,
	}}}
	got := reconcileOnce(t, pol, stub)
	if got.Status.DesiredFloor != 5 {
		t.Fatalf("DesiredFloor = %d, want 5 (predicted folded via max)", got.Status.DesiredFloor)
	}
	if got.Status.PredictedFloor != 5 {
		t.Fatalf("PredictedFloor = %d, want 5", got.Status.PredictedFloor)
	}
}

// 2. Capped at floor.max.
func TestReconcile_PredictedFloor_CappedAtFloorMax(t *testing.T) {
	pol := newPolicy()
	pol.Name = "pred-capped"
	pol.Spec.Floor.Max = 10
	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{
		"self-hosted": 20,
	}}}
	got := reconcileOnce(t, pol, stub)
	if got.Status.DesiredFloor != 10 {
		t.Fatalf("DesiredFloor = %d, want 10 (capped at floor.max)", got.Status.DesiredFloor)
	}
}

// 3. Decrease cooldown still applies after a predictor-driven raise.
func TestReconcile_PredictedFloor_DecreaseCooldownApplies(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	arc := newARC(0)
	pol := newPolicy()
	pol.Name = "pred-cooldown"
	pol.Spec.Floor.Max = 10
	pol.Spec.QueueRule.Cooldown = metav1.Duration{Duration: 5 * time.Minute}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{"self-hosted": 8}}}
	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
		Predictor: stub,
	}
	// Reconcile A: predicts 8 → backend goes to 8.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	if v := arcFloor(t, cl); v != 8 {
		t.Fatalf("after raise: minRunners = %d, want 8", v)
	}
	// Stamp a recent LastDecreaseTime so a drop within cooldown is blocked.
	var snap v1alpha1.WarmRunnerPolicy
	_ = cl.Get(context.Background(), types.NamespacedName{Name: pol.Name, Namespace: "default"}, &snap)
	recent := metav1.NewTime(time.Now())
	snap.Status.LastDecreaseTime = &recent
	_ = cl.Status().Update(context.Background(), &snap)

	// Reconcile B: prediction drops to 0; cooldown must hold floor at 8.
	stub.snap = predictor.Prediction{PerLabelSet: map[string]int{}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	if v := arcFloor(t, cl); v != 8 {
		t.Fatalf("within cooldown: minRunners = %d, want 8 (decrease must be blocked)", v)
	}
}

// 4. PredictorAvailable=True on success.
func TestReconcile_PredictorAvailable_TrueOnSuccess(t *testing.T) {
	pol := newPolicy()
	pol.Name = "pred-ok"
	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{"self-hosted": 1}}}
	got := reconcileOnce(t, pol, stub)
	cond := findCondition(got.Status.Conditions, "PredictorAvailable")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("PredictorAvailable not True; got %+v", cond)
	}
}

// 5. PredictorAvailable=False on Predict error; reconcile still succeeds.
func TestReconcile_PredictorAvailable_FalseOnError(t *testing.T) {
	pol := newPolicy()
	pol.Name = "pred-err"
	pol.Spec.Floor.Min = 2 // reactive/schedule baseline still drives floor
	stub := &stubPredictor{err: errors.New("rate limited")}
	got := reconcileOnce(t, pol, stub)

	cond := findCondition(got.Status.Conditions, "PredictorAvailable")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("PredictorAvailable not False; got %+v", cond)
	}
	if cond.Reason != "PredictError" {
		t.Fatalf("reason = %q, want PredictError", cond.Reason)
	}
	// Floor must still reach the policy minimum from the schedule layer.
	if got.Status.DesiredFloor != 2 {
		t.Fatalf("DesiredFloor = %d, want 2 (schedule baseline)", got.Status.DesiredFloor)
	}
	if got.Status.PredictedFloor != 0 {
		t.Fatalf("PredictedFloor = %d, want 0 on error", got.Status.PredictedFloor)
	}
}

// 6. Predictor disabled → not called, no PredictorAvailable condition.
func TestReconcile_PredictorDisabled_NotCalled(t *testing.T) {
	pol := newPolicy()
	pol.Name = "pred-disabled"
	pol.Spec.Predictor = &v1alpha1.PredictorConfig{Enabled: false}
	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{"self-hosted": 9}}}
	got := reconcileOnce(t, pol, stub)
	if stub.calls != 0 {
		t.Fatalf("predictor was called %d times when disabled; want 0", stub.calls)
	}
	if findCondition(got.Status.Conditions, "PredictorAvailable") != nil {
		t.Fatalf("PredictorAvailable condition should be absent when disabled")
	}
	if got.Status.PredictedFloor != 0 {
		t.Fatalf("PredictedFloor = %d, want 0 when disabled", got.Status.PredictedFloor)
	}
}

// 7. Status fields populated; top-8 deterministic order.
func TestReconcile_PredictedLabelSets_TopNDeterministic(t *testing.T) {
	pol := newPolicy()
	pol.Name = "pred-topn"
	pol.Spec.Floor.Max = 100
	// 10 distinct matching label sets with varying counts. Two tied at
	// count=2 to exercise the alphabetic tie-break.
	per := map[string]int{
		"self-hosted,a": 5,
		"self-hosted,b": 4,
		"self-hosted,c": 3,
		"self-hosted,d": 2,
		"self-hosted,e": 2,
		"self-hosted,f": 1,
		"self-hosted,g": 1,
		"self-hosted,h": 1,
		"self-hosted,i": 1,
		"self-hosted,j": 1,
	}
	pol.Spec.GitHub.Labels = []string{"self-hosted"}
	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: per}}
	got := reconcileOnce(t, pol, stub)

	if got.Status.PredictedFloor != 21 {
		t.Fatalf("PredictedFloor = %d, want 21 (sum of all matched counts)", got.Status.PredictedFloor)
	}
	if n := len(got.Status.PredictedLabelSets); n != 8 {
		t.Fatalf("PredictedLabelSets len = %d, want 8 (top-N cap)", n)
	}
	// First four positions are unambiguous by count desc.
	want := []struct {
		labels []string
		count  int32
	}{
		{[]string{"self-hosted", "a"}, 5},
		{[]string{"self-hosted", "b"}, 4},
		{[]string{"self-hosted", "c"}, 3},
		// Tie at count=2: alphabetical key order → "self-hosted,d" before "self-hosted,e".
		{[]string{"self-hosted", "d"}, 2},
		{[]string{"self-hosted", "e"}, 2},
	}
	for i, w := range want {
		ls := got.Status.PredictedLabelSets[i]
		if ls.Count != w.count {
			t.Fatalf("[%d] count = %d, want %d", i, ls.Count, w.count)
		}
		if !equalStrSlices(ls.Labels, w.labels) {
			t.Fatalf("[%d] labels = %v, want %v", i, ls.Labels, w.labels)
		}
	}
}

// Token plumbing: when the policy references an auth Secret, the
// reconciler resolves it and forwards the token through to Predict. This
// is the v0.2.1 fix for the v0.2.0 predictor-auth bug (404 on every
// /actions/runs call against private/rate-limited repos).
func TestReconcile_PredictorToken_PlumbedFromSecret(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	arc := newARC(0)
	pol := newPolicy()
	pol.Name = "pred-tok"
	pol.Spec.GitHub.Auth.SecretRef = corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "gh-token"},
		Key:                  "token",
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-token", Namespace: pol.Namespace},
		Data:       map[string][]byte{"token": []byte("live-test-tok")},
	}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol, secret).WithStatusSubresource(pol).Build()

	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{"self-hosted": 1}}}
	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
		Predictor: stub,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pol.Name, Namespace: pol.Namespace}}); err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("predictor calls = %d, want 1", stub.calls)
	}
	if stub.lastToken != "live-test-tok" {
		t.Fatalf("predictor token = %q, want %q", stub.lastToken, "live-test-tok")
	}
}

// 8. Label-set superset matching.
func TestReconcile_LabelSupersetMatching(t *testing.T) {
	pol := newPolicy()
	pol.Name = "pred-supers"
	pol.Spec.Floor.Max = 50
	pol.Spec.GitHub.Labels = []string{"self-hosted"}
	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{
		"gpu,self-hosted": 3,  // superset → matches
		"self-hosted":     2,  // exact → matches
		"ubuntu-latest":   10, // not a superset → excluded
	}}}
	got := reconcileOnce(t, pol, stub)
	if got.Status.PredictedFloor != 5 {
		t.Fatalf("PredictedFloor = %d, want 5 (only superset matches contribute)", got.Status.PredictedFloor)
	}
}

// Metrics: predicted_floor + predicted_jobs_total are populated, and stale
// label-set samples are pruned from one reconcile to the next.
func TestPredictedMetrics_PrunesStaleLabelSets(t *testing.T) {
	const policyName = "pred-metrics-prune"
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	arc := newARC(0)
	pol := newPolicy()
	pol.Name = policyName
	pol.Spec.Floor.Max = 100
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	stub := &stubPredictor{snap: predictor.Prediction{PerLabelSet: map[string]int{
		"self-hosted,a": 3,
		"self-hosted,b": 2,
	}}}
	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
		Predictor: stub,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	if v := predictedFloorValue(t, policyName); v != 5 {
		t.Fatalf("predicted_floor = %v, want 5", v)
	}
	if v := predictedJobsValue(t, policyName, "self-hosted,a"); v != 3 {
		t.Fatalf("predicted_jobs_total{a} = %v, want 3", v)
	}

	// Reconcile B: label-set "a" disappears.
	stub.snap = predictor.Prediction{PerLabelSet: map[string]int{"self-hosted,b": 4}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	if v := predictedJobsValue(t, policyName, "self-hosted,a"); v != 0 {
		t.Fatalf("stale label set a not pruned: predicted_jobs_total{a} = %v", v)
	}
	if v := predictedJobsValue(t, policyName, "self-hosted,b"); v != 4 {
		t.Fatalf("predicted_jobs_total{b} = %v, want 4", v)
	}
}

// Metrics: workflow_yaml_fetch_total registered + RecordWorkflowYAMLFetch wires through.
func TestWorkflowYAMLFetchTotal_Records(t *testing.T) {
	before := workflowYAMLFetchValue(t, "fetched")
	RecordWorkflowYAMLFetch("fetched")
	if got := workflowYAMLFetchValue(t, "fetched") - before; got != 1 {
		t.Fatalf("workflow_yaml_fetch_total{result=fetched} delta = %v, want 1", got)
	}
	before = workflowYAMLFetchValue(t, "dynamic_skipped")
	RecordWorkflowYAMLFetch("dynamic_skipped")
	if got := workflowYAMLFetchValue(t, "dynamic_skipped") - before; got != 1 {
		t.Fatalf("workflow_yaml_fetch_total{result=dynamic_skipped} delta = %v, want 1", got)
	}
}

// helpers ---------------------------------------------------------------

func findCondition(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func gaugeSample(t *testing.T, family string, match map[string]string) float64 {
	t.Helper()
	mf := gatherFamily(t, family)
	if mf == nil {
		return 0
	}
	for _, m := range mf.GetMetric() {
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		ok := true
		for k, v := range match {
			if labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			if m.Gauge != nil {
				return m.GetGauge().GetValue()
			}
			if m.Counter != nil {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func predictedFloorValue(t *testing.T, policy string) float64 {
	return gaugeSample(t, "warmrunners_predicted_floor", map[string]string{"policy": policy})
}

func predictedJobsValue(t *testing.T, policy, labels string) float64 {
	return gaugeSample(t, "warmrunners_predicted_jobs_total", map[string]string{"policy": policy, "labels": labels})
}

func workflowYAMLFetchValue(t *testing.T, result string) float64 {
	return gaugeSample(t, "warmrunners_workflow_yaml_fetch_total", map[string]string{"result": result})
}

// keep dto import alive when this file's only consumers are helpers
var _ = dto.MetricType_GAUGE
