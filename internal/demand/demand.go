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
