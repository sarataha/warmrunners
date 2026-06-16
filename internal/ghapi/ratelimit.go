// Package ghapi holds GitHub-API-wide observability that both the demand
// poller and the predictor's WorkflowFetcher feed into. Keeping it in its
// own package avoids an import cycle: internal/controller imports both
// demand and predictor, so they cannot reach back into controller to record
// shared metrics.
package ghapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	rateLimitRemaining = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "warmrunners_github_rate_limit_remaining",
			Help: "Last observed X-RateLimit-Remaining from a GitHub REST response, by source.",
		},
		[]string{"source"},
	)
	rateLimitResetSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "warmrunners_github_rate_limit_reset_seconds",
			Help: "Last observed X-RateLimit-Reset (unix seconds) from a GitHub REST response, by source.",
		},
		[]string{"source"},
	)
)

func init() {
	metricsserver.Registry.MustRegister(rateLimitRemaining, rateLimitResetSeconds)
}

// Source labels. Add new ones as call sites grow; keep cardinality small.
const (
	SourceDemand   = "demand"
	SourceWorkflow = "workflow"
)

// RecordRateLimit reads X-RateLimit-* headers and updates the gauges for
// the given source. Missing or unparsable headers are silently ignored
// (GitHub Enterprise variants and 304 responses occasionally omit them).
func RecordRateLimit(source string, h http.Header) {
	if v := strings.TrimSpace(h.Get("X-RateLimit-Remaining")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rateLimitRemaining.WithLabelValues(source).Set(float64(n))
		}
	}
	if v := strings.TrimSpace(h.Get("X-RateLimit-Reset")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rateLimitResetSeconds.WithLabelValues(source).Set(float64(n))
		}
	}
}
