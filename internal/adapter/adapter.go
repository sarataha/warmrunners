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
	// GetMax reports the backend's own max-runner cap. The bool is false when
	// the backend leaves it unset (no cap declared).
	GetMax(ctx context.Context, ref Ref) (int32, bool, error)
}

// New is the constructor signature shared by ArcAdapter and GarmAdapter.
type New func(c client.Client) Adapter
