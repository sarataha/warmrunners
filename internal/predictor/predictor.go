// Package predictor defines the interface and value types for warmrunners'
// codebase-aware demand prediction.
//
// A Predictor inspects a repository's currently-active GitHub Actions workflow
// runs together with the workflow YAML at each run's head SHA, walks the
// statically decidable parts of the needs: DAG, and returns the number of jobs
// that are "imminent" — not yet materialized by GitHub, but whose dependencies
// are satisfied — grouped by their resolved runs-on label set.
//
// Implementations are expected to be pure read-only and idempotent within a
// poll window; the caller (the reconciler) decides cadence.
//
// # Label-set keys
//
// Prediction.PerLabelSet is keyed by a deterministic string formed via
// LabelSetKey from a job's resolved runs-on labels. The key is independent of
// the input order and collapses duplicates, so [gpu, self-hosted] and
// [self-hosted, gpu, gpu] produce the same key.
//
// LabelSetKey preserves case. GitHub's own runs-on matching is
// case-insensitive, but the rest of warmrunners (notably
// internal/demand.labelsMatch) operates on labels as they appear in API
// payloads, and we keep the same convention here so the reactive
// (DemandSource) and predictive (Predictor) signals attribute consistently.
// Callers that need case-folded matching should normalize labels before
// constructing or looking up a key.
//
// # Concurrency
//
// The Predictor interface itself makes no thread-safety guarantee; individual
// implementations document their own. Prediction.PerLabelSet is a plain map:
// callers must not mutate it concurrently with reads.
package predictor

import (
	"context"
	"sort"
	"strings"
)

// Prediction is the per-label-set imminent-demand snapshot returned by a
// Predictor.
//
// PerLabelSet maps a label-set key (produced by LabelSetKey) to the number of
// imminent jobs whose resolved runs-on equals that label set. Reconcilers
// attribute a key L to a policy when L is a superset of the policy's declared
// labels — the same direction used by the reactive demand source.
//
// The map is not safe for concurrent mutation alongside reads; treat a
// returned Prediction as immutable.
type Prediction struct {
	PerLabelSet map[string]int
}

// Predictor produces a forward-looking, per-label-set demand estimate for a
// single GitHub repository.
//
// Predict must be safe to call from a single reconcile goroutine; it need not
// be safe for concurrent calls unless the implementation says so. Predict is
// expected to be read-only with respect to GitHub and to return promptly
// (sub-second under cache hits); errors are returned verbatim so the caller
// can surface them as a condition without retry coupling.
type Predictor interface {
	Predict(ctx context.Context, owner, repository string) (Prediction, error)
}

// LabelSetKey returns a deterministic, order-independent key for a runs-on
// label set.
//
// The key is the sorted, comma-joined list of unique labels. Empty input
// returns the empty string. Case is preserved (see the package doc for the
// rationale).
func LabelSetKey(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(labels))
	uniq := make([]string, 0, len(labels))
	for _, l := range labels {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		uniq = append(uniq, l)
	}
	sort.Strings(uniq)
	return strings.Join(uniq, ",")
}
