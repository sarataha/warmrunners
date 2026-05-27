// internal/controller/reconcile_test.go
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/adapter"
	"github.com/sarataha/warmrunners/internal/demand"
	"github.com/sarataha/warmrunners/internal/scheduler"
	corev1 "k8s.io/api/core/v1"
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
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{},
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

// Gap A: nil-Demand production path resolves the policy's secretRef. A missing
// secret must yield DemandSourceAvailable=False, no panic, and no patch.
func TestReconcile_NilDemand_MissingSecret_SetsCondition(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	arc := newARC(0)
	pol := newPolicy()
	pol.Spec.GitHub.Auth.SecretRef = corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "gh-token"},
		Key:                  "token",
	}
	// No Secret object created → resolution must fail gracefully.
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    nil, // production path
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
		t.Fatalf("DemandSourceAvailable=False not set on missing secret; got %+v", got.Status.Conditions)
	}
	// Floor must remain untouched.
	gotARC := &unstructured.Unstructured{}
	gotARC.SetGroupVersionKind(arc.GroupVersionKind())
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "prod-runners", Namespace: "arc-system"}, gotARC)
	v, _, _ := unstructured.NestedInt64(gotARC.Object, "spec", "minRunners")
	if v != 0 {
		t.Fatalf("minRunners changed to %d on missing secret; want 0", v)
	}
}
