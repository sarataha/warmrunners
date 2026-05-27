package controller

import (
	"github.com/prometheus/client_golang/prometheus"
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
)

func init() {
	metricsserver.Registry.MustRegister(desiredFloor, appliedFloor, queueDepth, floorChanges)
}
