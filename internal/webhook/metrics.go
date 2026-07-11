package webhook

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// EventsTotal counts GitHub webhook events accepted by warmrunners, by
	// type and verification outcome.
	EventsTotal = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "warmrunners_webhook_events_total",
		Help: "GitHub webhook events accepted by warmrunners, by type and verification outcome.",
	}, []string{"event", "verified", "source_repo"})

	// LagSeconds observes the delay from GitHub delivery timestamp to
	// receiver dispatch.
	LagSeconds = promauto.With(metrics.Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name:    "warmrunners_webhook_lag_seconds",
		Help:    "Delay from GitHub delivery timestamp to receiver dispatch.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30},
	}, []string{"event"})

	// DeliveriesDropped counts webhook deliveries dropped before dispatch,
	// by reason: hmac_invalid | replay | unknown_app | body_too_large |
	// malformed.
	DeliveriesDropped = promauto.With(metrics.Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "warmrunners_webhook_deliveries_dropped_total",
		Help: "Webhook deliveries dropped before dispatch, by reason.",
	}, []string{"reason"})
)
