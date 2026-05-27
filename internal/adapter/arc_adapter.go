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
