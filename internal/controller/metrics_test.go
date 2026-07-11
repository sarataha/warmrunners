package controller

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/scheduler"
	"github.com/sarataha/warmrunners/internal/version"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// gatherFamily returns the named metric family from the controller-runtime
// registry, or nil if it is not registered.
func gatherFamily(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	mfs, err := metricsserver.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

func TestBuildInfoMetric_Registered(t *testing.T) {
	mf := gatherFamily(t, "warmrunners_build_info")
	if mf == nil {
		t.Fatalf("warmrunners_build_info family not registered")
	}
	if mf.GetType() != dto.MetricType_GAUGE {
		t.Fatalf("warmrunners_build_info type = %v, want GAUGE", mf.GetType())
	}
	if len(mf.GetMetric()) != 1 {
		t.Fatalf("warmrunners_build_info has %d samples, want 1", len(mf.GetMetric()))
	}
	m := mf.GetMetric()[0]
	if v := m.GetGauge().GetValue(); v != 1 {
		t.Fatalf("warmrunners_build_info value = %v, want 1", v)
	}
	labels := map[string]string{}
	for _, lp := range m.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	for k, want := range map[string]string{
		"version":    version.Version,
		"commit":     version.Commit,
		"build_date": version.BuildDate,
	} {
		got, ok := labels[k]
		if !ok {
			t.Fatalf("warmrunners_build_info missing label %q; labels=%v", k, labels)
		}
		if got != want {
			t.Fatalf("warmrunners_build_info label %q = %q, want %q", k, got, want)
		}
	}
}

func TestReconciliationErrorsTotal_Registered(t *testing.T) {
	// A CounterVec with no observed label set produces no Gather() family, so
	// confirm registration by attempting to re-register and expecting an
	// AlreadyRegisteredError carrying the same collector.
	err := metricsserver.Registry.Register(reconcileErrors)
	if err == nil {
		t.Fatalf("warmrunners_reconciliation_errors_total was not pre-registered")
	}
	if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
		t.Fatalf("re-register error = %v, want AlreadyRegisteredError", err)
	}
}

// counterValue reads the value of warmrunners_reconciliation_errors_total for a
// given (policy, error_type) label pair, or 0 if absent.
func counterValue(t *testing.T, policy, errorType string) float64 {
	t.Helper()
	mf := gatherFamily(t, "warmrunners_reconciliation_errors_total")
	if mf == nil {
		return 0
	}
	for _, m := range mf.GetMetric() {
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["policy"] == policy && labels["error_type"] == errorType {
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func TestReconcile_PollerError_IncrementsDemandSourceCounter(t *testing.T) {
	const policyName = "metrics-demand-err"
	// Reset any prior state for this label pair.
	reconcileErrors.WithLabelValues(policyName, "demand_source")

	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	arc := newARC(0)
	pol := newPolicy()
	pol.Name = policyName
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(arc, pol).WithStatusSubresource(pol).Build()

	before := counterValue(t, policyName, "demand_source")

	r := &WarmRunnerPolicyReconciler{
		Client: cl, Scheme: sch,
		Scheduler: scheduler.NewHeuristic(),
		Demand:    stubDemand{err: context.DeadlineExceeded},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}

	if got := counterValue(t, policyName, "demand_source") - before; got != 1 {
		t.Fatalf("demand_source counter delta = %v, want 1", got)
	}
}

func TestMetrics_ActiveWindowGaugePerRepo(t *testing.T) {
	activeWindowRemainingGauge.WithLabelValues("owner/repo-a").Set(42)
	activeWindowRemainingGauge.WithLabelValues("owner/repo-b").Set(7)

	if got := testutil.ToFloat64(activeWindowRemainingGauge.WithLabelValues("owner/repo-a")); got != 42 {
		t.Fatalf("repo-a gauge = %v, want 42", got)
	}
	if got := testutil.ToFloat64(activeWindowRemainingGauge.WithLabelValues("owner/repo-b")); got != 7 {
		t.Fatalf("repo-b gauge = %v, want 7", got)
	}
}

func TestMetrics_ActiveWindowExpiries(t *testing.T) {
	before := testutil.ToFloat64(activeWindowExpiries.WithLabelValues("owner/repo-expiry"))
	activeWindowExpiries.WithLabelValues("owner/repo-expiry").Inc()

	if got := testutil.ToFloat64(activeWindowExpiries.WithLabelValues("owner/repo-expiry")) - before; got != 1 {
		t.Fatalf("active window expiries delta = %v, want 1", got)
	}
}
