package ghapi

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordRateLimit_ParsesHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "4321")
	h.Set("X-RateLimit-Reset", "1717000000")

	RecordRateLimit(SourceDemand, h)

	if got := testutil.ToFloat64(rateLimitRemaining.WithLabelValues(SourceDemand)); got != 4321 {
		t.Fatalf("remaining = %v, want 4321", got)
	}
	if got := testutil.ToFloat64(rateLimitResetSeconds.WithLabelValues(SourceDemand)); got != 1717000000 {
		t.Fatalf("reset = %v, want 1717000000", got)
	}
}

func TestRecordRateLimit_MissingHeadersIgnored(t *testing.T) {
	// Set known baseline, then call with empty headers; values must not change.
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "100")
	h.Set("X-RateLimit-Reset", "200")
	RecordRateLimit(SourceWorkflow, h)

	RecordRateLimit(SourceWorkflow, http.Header{})

	if got := testutil.ToFloat64(rateLimitRemaining.WithLabelValues(SourceWorkflow)); got != 100 {
		t.Fatalf("remaining changed to %v after empty headers; want 100", got)
	}
}

func TestRecordRateLimit_GarbageHeadersIgnored(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "abc")
	RecordRateLimit("garbage-source", h)

	// No panic, no metric set — the unparsed header is dropped.
	if got := testutil.ToFloat64(rateLimitRemaining.WithLabelValues("garbage-source")); got != 0 {
		t.Fatalf("remaining = %v, want 0 (header was unparsable)", got)
	}
}
