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
