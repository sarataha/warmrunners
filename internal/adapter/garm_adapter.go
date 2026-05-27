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
