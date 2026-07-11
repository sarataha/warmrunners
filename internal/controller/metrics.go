package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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
	// predictedFloorGauge is the codebase-aware Predictor's contribution to the
	// policy's desired floor on the most recent reconcile (v0.2.0).
	predictedFloorGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_predicted_floor", Help: "Predictor's contribution to the desired floor."},
		[]string{"policy"},
	)
	// predictedJobsGauge is the per-label-set imminent job count from the
	// Predictor. One sample per (policy, label-set key) seen on the latest
	// reconcile. Sets that disappear on a subsequent reconcile are pruned via
	// DeleteLabelValues to bound cardinality.
	predictedJobsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_predicted_jobs_total", Help: "Per-label-set imminent job count from the Predictor."},
		[]string{"policy", "labels"},
	)
	// workflowYAMLFetchTotal counts workflow YAML fetch outcomes from the
	// Predictor. result ∈ {fetched, error, dynamic_skipped}. The {policy}
	// label is intentionally dropped — the predictor is shared across all
	// policies in v0.2.0 (one *WorkflowNeedsGraph per process), so attaching
	// a policy label at the hooks layer would require a more invasive wiring
	// (Option B in the v0.2.0 plan). Aggregating across policies is cheap to
	// filter elsewhere by repo if needed.
	workflowYAMLFetchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "warmrunners_workflow_yaml_fetch_total", Help: "Predictor workflow YAML fetch outcomes."},
		[]string{"result"},
	)
	// activityFloorGauge is the Activity sampler's contribution to the policy's
	// desired floor on the most recent reconcile (v0.3.0). Sibling of
	// predictedFloorGauge — the reconciler folds both via max() before clamping.
	activityFloorGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_activity_floor", Help: "Activity sampler's contribution to the desired floor."},
		[]string{"policy"},
	)
	// activityJobsGauge is the per-label-set job-count sample from the Activity
	// sampler. Sibling of predictedJobsGauge; same pruning discipline (label
	// sets seen on a previous reconcile but absent now are DeleteLabelValues'd
	// to bound cardinality).
	activityJobsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "warmrunners_activity_jobs_total", Help: "Per-label-set job count from the Activity sampler."},
		[]string{"policy", "labels"},
	)
	// activityBotFilteredTotal counts bot-filtered workflow_runs by the reason
	// IsBotActor returned. The {policy} label is intentionally dropped — same
	// reasoning as workflowYAMLFetchTotal: the sampler is shared across
	// policies via cmd/main.go's single httpClient + WorkflowFetcher.
	activityBotFilteredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "warmrunners_activity_bot_filtered_total", Help: "Bot-filtered workflow_runs by reason."},
		[]string{"reason"},
	)

	// dryRunSkippedPatches counts every backend Patch the controller
	// would have issued but skipped because spec.dryRun was true. Operators
	// rely on this to confirm "the controller would have acted N times"
	// during canary observation.
	dryRunSkippedPatches = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "warmrunners_dry_run_skipped_patches_total", Help: "Backend patches skipped because spec.dryRun was true."},
		[]string{"policy"},
	)

	// activeWindowExpiries counts the number of times a repo's active window
	// expired and the activity floor dropped to zero (v0.5.0).
	activeWindowExpiries = promauto.With(metricsserver.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "warmrunners_active_window_expiries_total",
		Help: "Number of times a repo's active window expired and the activity floor dropped to zero.",
	}, []string{"repo"})

	// activeWindowRemainingGauge reports the seconds remaining before the
	// active window expires for a repo (v0.5.0).
	activeWindowRemainingGauge = promauto.With(metricsserver.Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "warmrunners_active_window_seconds_remaining",
		Help: "Seconds remaining before the active window expires for a repo.",
	}, []string{"repo"})
)

func init() {
	metricsserver.Registry.MustRegister(
		desiredFloor, appliedFloor, queueDepth, floorChanges, buildInfo, reconcileErrors,
		predictedFloorGauge, predictedJobsGauge, workflowYAMLFetchTotal,
		activityFloorGauge, activityJobsGauge, activityBotFilteredTotal,
		dryRunSkippedPatches,
	)
	buildInfo.WithLabelValues(version.Version, version.Commit, version.BuildDate).Set(1)
}

// RecordWorkflowYAMLFetch bumps the warmrunners_workflow_yaml_fetch_total
// counter. Exported so cmd/main.go can wire the Predictor's Hooks without
// reaching into package-private state. result ∈ {fetched, error, dynamic_skipped}.
func RecordWorkflowYAMLFetch(result string) {
	workflowYAMLFetchTotal.WithLabelValues(result).Inc()
}

// IncActivityBotFiltered bumps the warmrunners_activity_bot_filtered_total
// counter. Exported so cmd/main.go can wire the Activity sampler's Hooks
// without reaching into package-private state. reason is the value returned
// by activity.IsBotActor: {bot_type, trigger_bot_type, bot_suffix, denylist}.
func IncActivityBotFiltered(reason string) {
	activityBotFilteredTotal.WithLabelValues(reason).Inc()
}
