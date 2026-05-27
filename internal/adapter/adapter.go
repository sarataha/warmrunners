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
