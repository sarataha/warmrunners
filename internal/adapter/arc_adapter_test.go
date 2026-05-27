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

func TestArcAdapter_GetMax(t *testing.T) {
	obj := newArc(3)
	_ = unstructured.SetNestedField(obj.Object, int64(9), "spec", "maxRunners")
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(obj).Build()
	a := NewArcAdapter(cl)
	got, set, err := a.GetMax(context.Background(), Ref{Name: "prod-runners", Namespace: "arc-system"})
	if err != nil {
		t.Fatal(err)
	}
	if !set || got != 9 {
		t.Fatalf("GetMax = (%d, %v), want (9, true)", got, set)
	}
}

func TestArcAdapter_GetMax_Unset(t *testing.T) {
	obj := newArc(3) // no maxRunners
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(obj).Build()
	a := NewArcAdapter(cl)
	_, set, err := a.GetMax(context.Background(), Ref{Name: "prod-runners", Namespace: "arc-system"})
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Fatalf("GetMax set = true, want false (maxRunners unset)")
	}
}

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
