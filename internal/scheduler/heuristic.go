// internal/scheduler/heuristic.go
package scheduler

import (
	"fmt"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
)

type Heuristic struct{}

func NewHeuristic() *Heuristic { return &Heuristic{} }

func (h *Heuristic) Decide(spec v1alpha1.WarmRunnerPolicySpec, now time.Time, d Demand, currentApplied int32, lastDecreaseAt time.Time) Decision {
	base := scheduleBase(spec.Schedule, now)
	headroom := bestHeadroom(spec.QueueRule.Headroom, d.Queued)
	desired := base + headroom
	if desired < spec.Floor.Min {
		desired = spec.Floor.Min
	}
	if desired > spec.Floor.Max {
		desired = spec.Floor.Max
	}
	// Cooldown applies only to decreases.
	if desired < currentApplied {
		if now.Sub(lastDecreaseAt) < spec.QueueRule.Cooldown.Duration {
			desired = currentApplied
		}
	}
	return Decision{DesiredFloor: desired}
}

var weekdayShort = map[time.Weekday]string{
	time.Sunday: "Sun", time.Monday: "Mon", time.Tuesday: "Tue",
	time.Wednesday: "Wed", time.Thursday: "Thu", time.Friday: "Fri", time.Saturday: "Sat",
}

func scheduleBase(windows []v1alpha1.ScheduleWindow, now time.Time) int32 {
	var best int32 = 0
	for _, w := range windows {
		loc, err := time.LoadLocation(w.TZ)
		if err != nil {
			continue
		}
		local := now.In(loc)
		if !containsDay(w.Days, weekdayShort[local.Weekday()]) {
			continue
		}
		if withinHHMM(local, w.From, w.To) {
			if w.Base > best {
				best = w.Base
			}
		}
	}
	return best
}

func bestHeadroom(tiers []v1alpha1.HeadroomTier, queued int32) int32 {
	var best int32 = 0
	var bestThreshold int32 = -1
	for _, t := range tiers {
		if queued >= t.WhenQueueAtLeast && t.WhenQueueAtLeast > bestThreshold {
			best = t.AddRunners
			bestThreshold = t.WhenQueueAtLeast
		}
	}
	return best
}

func containsDay(days []string, today string) bool {
	for _, d := range days {
		if d == today {
			return true
		}
	}
	return false
}

func withinHHMM(t time.Time, from, to string) bool {
	fh, fm := parseHM(from)
	th, tm := parseHM(to)
	h, m := t.Hour(), t.Minute()
	nowMin := h*60 + m
	fromMin := fh*60 + fm
	toMin := th*60 + tm
	return nowMin >= fromMin && nowMin < toMin
}

func parseHM(s string) (int, int) {
	var h, m int
	_, _ = fmt.Sscanf(s, "%d:%d", &h, &m)
	return h, m
}
