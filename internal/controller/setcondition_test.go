package controller

import (
	"testing"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSetCondition_StableConditionPreservesTransitionTime guards against the
// pre-v0.4.1 bug: setCondition rewrote LastTransitionTime on every call, so
// the steady-state reconcile loop generated a fresh status payload each
// iteration. The resulting cache-thrash produced the "object has been
// modified" resource-version conflicts the kind exercise surfaced.
//
// Spec contract (k8s API conventions, also enforced by apimachinery's
// meta.SetStatusCondition): LastTransitionTime changes ONLY when Status
// transitions between True/False/Unknown.
func TestSetCondition_StableConditionPreservesTransitionTime(t *testing.T) {
	pol := &v1alpha1.WarmRunnerPolicy{}
	setCondition(pol, "X", true, "OK", "", 1)
	first := pol.Status.Conditions[0].LastTransitionTime

	// Force the clock forward so any spurious metav1.Now() write is visible.
	time.Sleep(10 * time.Millisecond)

	setCondition(pol, "X", true, "OK", "", 1)
	second := pol.Status.Conditions[0].LastTransitionTime

	if !first.Equal(&second) {
		t.Fatalf("LastTransitionTime changed for a stable condition: %v -> %v", first, second)
	}
}

func TestSetCondition_StatusFlipUpdatesTransitionTime(t *testing.T) {
	pol := &v1alpha1.WarmRunnerPolicy{}
	setCondition(pol, "X", true, "OK", "", 1)
	first := pol.Status.Conditions[0].LastTransitionTime

	time.Sleep(10 * time.Millisecond)

	setCondition(pol, "X", false, "Err", "boom", 1)
	second := pol.Status.Conditions[0].LastTransitionTime

	if first.Equal(&second) {
		t.Fatalf("LastTransitionTime did not advance after status flipped True -> False")
	}
	if pol.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatalf("Status not flipped: %v", pol.Status.Conditions[0].Status)
	}
}
