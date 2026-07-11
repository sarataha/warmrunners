package webhook

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics_TunnelGaugeTransitions(t *testing.T) {
	TunnelConnected.WithLabelValues("app1").Set(1)
	if got := testutil.ToFloat64(TunnelConnected.WithLabelValues("app1")); got != 1 {
		t.Fatalf("tunnel connected gauge = %v, want 1", got)
	}

	TunnelConnected.WithLabelValues("app1").Set(0)
	if got := testutil.ToFloat64(TunnelConnected.WithLabelValues("app1")); got != 0 {
		t.Fatalf("tunnel connected gauge = %v, want 0", got)
	}
}

func TestMetrics_TunnelReconnectsCounts(t *testing.T) {
	beforeSuccess := testutil.ToFloat64(TunnelReconnects.WithLabelValues("app2", "success"))
	beforeFailure := testutil.ToFloat64(TunnelReconnects.WithLabelValues("app2", "failure"))

	TunnelReconnects.WithLabelValues("app2", "success").Inc()
	TunnelReconnects.WithLabelValues("app2", "failure").Inc()
	TunnelReconnects.WithLabelValues("app2", "failure").Inc()

	if got := testutil.ToFloat64(TunnelReconnects.WithLabelValues("app2", "success")) - beforeSuccess; got != 1 {
		t.Fatalf("success counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(TunnelReconnects.WithLabelValues("app2", "failure")) - beforeFailure; got != 2 {
		t.Fatalf("failure counter delta = %v, want 2", got)
	}
}
