// internal/scheduler/heuristic_test.go
package scheduler

import (
	"testing"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestHeuristic_NoSchedule_NoDemand_ReturnsMin(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor:     v1alpha1.FloorRange{Min: 0, Max: 10},
		QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
	}
	got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
	if got.DesiredFloor != 0 {
		t.Fatalf("DesiredFloor = %d, want 0", got.DesiredFloor)
	}
}

func TestHeuristic_InsideWindow_ReturnsBase(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
		Schedule: []v1alpha1.ScheduleWindow{{
			Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
		}},
		QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
	}
	// Wed 10:00 UTC
	got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
	if got.DesiredFloor != 3 {
		t.Fatalf("DesiredFloor = %d, want 3", got.DesiredFloor)
	}
}

func TestHeuristic_OutsideWindow_ReturnsMin(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
		Schedule: []v1alpha1.ScheduleWindow{{
			Days: []string{"Mon"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
		}},
		QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
	}
	// Wed (no Mon window applies)
	got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
	if got.DesiredFloor != 0 {
		t.Fatalf("DesiredFloor = %d, want 0", got.DesiredFloor)
	}
}

func TestHeuristic_QueueHeadroom_HighestTierWins(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor: v1alpha1.FloorRange{Min: 0, Max: 50},
		Schedule: []v1alpha1.ScheduleWindow{{
			Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
		}},
		QueueRule: v1alpha1.QueueRule{
			Cooldown: metav1.Duration{Duration: time.Minute},
			Headroom: []v1alpha1.HeadroomTier{
				{WhenQueueAtLeast: 5, AddRunners: 3},
				{WhenQueueAtLeast: 15, AddRunners: 8},
			},
		},
	}
	// queue 20 → highest tier (15) wins → base 3 + 8 = 11
	got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{Queued: 20}, 0, time.Time{})
	if got.DesiredFloor != 11 {
		t.Fatalf("DesiredFloor = %d, want 11", got.DesiredFloor)
	}
}

func TestHeuristic_QueueHeadroom_NoTierMatches(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor: v1alpha1.FloorRange{Min: 0, Max: 50},
		Schedule: []v1alpha1.ScheduleWindow{{
			Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
		}},
		QueueRule: v1alpha1.QueueRule{
			Cooldown: metav1.Duration{Duration: time.Minute},
			Headroom: []v1alpha1.HeadroomTier{{WhenQueueAtLeast: 10, AddRunners: 5}},
		},
	}
	got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{Queued: 3}, 0, time.Time{})
	if got.DesiredFloor != 3 {
		t.Fatalf("DesiredFloor = %d, want 3", got.DesiredFloor)
	}
}

func TestHeuristic_ClampToMax(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor: v1alpha1.FloorRange{Min: 0, Max: 5},
		Schedule: []v1alpha1.ScheduleWindow{{
			Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 3,
		}},
		QueueRule: v1alpha1.QueueRule{
			Cooldown: metav1.Duration{Duration: time.Minute},
			Headroom: []v1alpha1.HeadroomTier{{WhenQueueAtLeast: 1, AddRunners: 100}},
		},
	}
	got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{Queued: 50}, 0, time.Time{})
	if got.DesiredFloor != 5 {
		t.Fatalf("DesiredFloor = %d, want 5 (clamped to Max)", got.DesiredFloor)
	}
}

func TestHeuristic_ClampToMin(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor:     v1alpha1.FloorRange{Min: 2, Max: 10},
		QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
	}
	got := h.Decide(spec, mustParseTime(t, "2026-05-27T10:00:00Z"), Demand{}, 0, time.Time{})
	if got.DesiredFloor != 2 {
		t.Fatalf("DesiredFloor = %d, want 2 (clamped to Min)", got.DesiredFloor)
	}
}

func TestHeuristic_Cooldown_BlocksDecrease(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor:     v1alpha1.FloorRange{Min: 0, Max: 10},
		QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: 2 * time.Minute}},
	}
	now := mustParseTime(t, "2026-05-27T10:00:00Z")
	lastDec := now.Add(-30 * time.Second) // 30s ago, inside 2m cooldown
	// Current applied = 5, computed desired = 0 → should stay at 5 until cooldown lapses.
	got := h.Decide(spec, now, Demand{}, 5, lastDec)
	if got.DesiredFloor != 5 {
		t.Fatalf("DesiredFloor = %d, want 5 (cooldown blocks decrease)", got.DesiredFloor)
	}
}

func TestHeuristic_Cooldown_AllowsIncrease(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
		Schedule: []v1alpha1.ScheduleWindow{{
			Days: []string{"Wed"}, From: "08:00", To: "19:00", TZ: "UTC", Base: 7,
		}},
		QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: 2 * time.Minute}},
	}
	now := mustParseTime(t, "2026-05-27T10:00:00Z")
	lastDec := now.Add(-10 * time.Second)
	got := h.Decide(spec, now, Demand{}, 3, lastDec)
	if got.DesiredFloor != 7 {
		t.Fatalf("DesiredFloor = %d, want 7 (increase always allowed)", got.DesiredFloor)
	}
}

func TestHeuristic_TZ_RespectsLocation(t *testing.T) {
	h := NewHeuristic()
	spec := v1alpha1.WarmRunnerPolicySpec{
		Floor: v1alpha1.FloorRange{Min: 0, Max: 10},
		Schedule: []v1alpha1.ScheduleWindow{{
			// 09:00–17:00 New York time on a Friday.
			Days: []string{"Fri"}, From: "09:00", To: "17:00", TZ: "America/New_York", Base: 4,
		}},
		QueueRule: v1alpha1.QueueRule{Cooldown: metav1.Duration{Duration: time.Minute}},
	}
	// 2026-05-29 13:00 UTC = 09:00 EDT on Friday → inside window.
	got := h.Decide(spec, mustParseTime(t, "2026-05-29T13:00:00Z"), Demand{}, 0, time.Time{})
	if got.DesiredFloor != 4 {
		t.Fatalf("DesiredFloor = %d, want 4 (TZ-aware)", got.DesiredFloor)
	}
	// 2026-05-29 12:30 UTC = 08:30 EDT → before window.
	got2 := h.Decide(spec, mustParseTime(t, "2026-05-29T12:30:00Z"), Demand{}, 0, time.Time{})
	if got2.DesiredFloor != 0 {
		t.Fatalf("DesiredFloor = %d, want 0 (before window)", got2.DesiredFloor)
	}
}
