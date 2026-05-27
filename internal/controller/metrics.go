package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sarataha/warmrunners/internal/version"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	desiredFloor = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_desired_floor", Help: "Desired warm-floor."},
		[]string{"policy", "target"},
	)
	appliedFloor = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_applied_floor", Help: "Applied warm-floor."},
		[]string{"policy", "target"},
	)
	queueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_queue_depth", Help: "Observed GitHub queue depth."},
		[]string{"policy"},
	)
	floorChanges = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "warmrunners_floor_change_total", Help: "Floor change events."},
		[]string{"policy", "direction"},
	)
	// buildInfo exposes build-time identity as a constant-1 gauge labeled by
	// version/commit/build_date (cf. KEDA keda_build_info).
	buildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_build_info", Help: "Build identity; constant 1, labeled by version/commit/build_date."},
		[]string{"version", "commit", "build_date"},
	)
	// reconcileErrors counts reconcile failures per policy, labeled by the
	// failure mode (demand_source, adapter, status_update). Distinct from
	// controller-runtime's per-controller reconcile_errors_total.
	reconcileErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "warmrunners_reconciliation_errors_total", Help: "Reconcile errors by failure mode."},
		[]string{"policy", "error_type"},
	)
)

func init() {
	metricsserver.Registry.MustRegister(desiredFloor, appliedFloor, queueDepth, floorChanges, buildInfo, reconcileErrors)
	buildInfo.WithLabelValues(version.Version, version.Commit, version.BuildDate).Set(1)
}
