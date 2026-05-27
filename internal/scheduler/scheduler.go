// internal/scheduler/scheduler.go
package scheduler

import (
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
)

// Demand is the snapshot of GitHub-side load passed to the Scheduler.
type Demand struct {
	Queued  int32
	Running int32
}

// Decision is the Scheduler's output: the desired warm-floor.
type Decision struct {
	DesiredFloor int32
}

// Scheduler decides the desired warm-floor for a policy.
type Scheduler interface {
	Decide(spec v1alpha1.WarmRunnerPolicySpec, now time.Time, d Demand, currentApplied int32, lastDecreaseAt time.Time) Decision
}
