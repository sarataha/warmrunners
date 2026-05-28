/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/adapter"
	"github.com/sarataha/warmrunners/internal/demand"
	"github.com/sarataha/warmrunners/internal/predictor"
	"github.com/sarataha/warmrunners/internal/scheduler"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// githubBaseURL is the GitHub REST API base used when constructing a
// per-policy demand source in production (nil Demand).
const githubBaseURL = "https://api.github.com"

// AdapterFactory resolves the Adapter + Ref for a target. It is a test seam:
// when set on the reconciler it overrides production adapter selection.
type AdapterFactory func(t v1alpha1.Target) (adapter.Adapter, adapter.Ref, bool)

// WarmRunnerPolicyReconciler reconciles a WarmRunnerPolicy object
type WarmRunnerPolicyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Scheduler scheduler.Scheduler
	// Demand is the demand source. Tests inject a stub directly. When nil
	// (production), the reconciler resolves the policy's auth secret and
	// constructs a GitHub REST poller per reconcile.
	Demand      demand.Source
	AdapterFunc AdapterFactory
	// MaxConcurrentReconciles bounds parallel reconciles for this controller.
	// Zero means controller-runtime default (1).
	MaxConcurrentReconciles int
	// GitHubHTTPTimeout bounds each GitHub REST call. Zero falls back to the
	// poller's built-in default (10s).
	GitHubHTTPTimeout time.Duration
	// Predictor is the codebase-aware imminent-demand source (v0.2.0). When
	// nil, the predictor leg is skipped entirely and the reconciler degrades
	// to v0.1.x behavior (schedule + reactive only). The field is an
	// interface so unit tests can inject a stub returning a canned snapshot.
	Predictor predictor.Predictor

	// prevPredictedLabels tracks the label-set keys we emitted as
	// warmrunners_predicted_jobs_total samples for each policy on the
	// previous reconcile. We DeleteLabelValues for keys that disappear so
	// the gauge family does not accumulate cardinality from transient
	// label-sets. Guarded by mu.
	mu                  sync.Mutex
	prevPredictedLabels map[string]map[string]struct{}
}

func (r *WarmRunnerPolicyReconciler) adapterFor(t v1alpha1.Target) (adapter.Adapter, adapter.Ref, bool) {
	if r.AdapterFunc != nil {
		return r.AdapterFunc(t)
	}
	switch t.Kind() {
	case "arc":
		return adapter.NewArcAdapter(r.Client), adapter.Ref{Name: t.Arc.RunnerSet.Name, Namespace: t.Arc.RunnerSet.Namespace}, true
	case "garm":
		return adapter.NewGarmAdapter(r.Client), adapter.Ref{Name: t.Garm.Pool.Name, Namespace: t.Garm.Pool.Namespace}, true
	}
	return nil, adapter.Ref{}, false
}

// demandSource returns the demand source for this reconcile. If r.Demand is set
// (tests), it is returned as-is. Otherwise it resolves the policy's auth secret
// from the policy namespace and builds a GitHub REST poller. A non-nil error
// means the source could not be built and DemandSourceAvailable must be False.
func (r *WarmRunnerPolicyReconciler) demandSource(ctx context.Context, pol *v1alpha1.WarmRunnerPolicy) (demand.Source, error) {
	if r.Demand != nil {
		return r.Demand, nil
	}
	sel := pol.Spec.GitHub.Auth.SecretRef
	if sel.Name == "" || sel.Key == "" {
		return nil, fmt.Errorf("github auth secretRef is incomplete (name=%q key=%q)", sel.Name, sel.Key)
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: sel.Name, Namespace: pol.Namespace}, &secret); err != nil {
		return nil, fmt.Errorf("resolving auth secret %q: %w", sel.Name, err)
	}
	tokenBytes, ok := secret.Data[sel.Key]
	if !ok || len(tokenBytes) == 0 {
		return nil, fmt.Errorf("auth secret %q missing key %q", sel.Name, sel.Key)
	}
	var opts []demand.Option
	if r.GitHubHTTPTimeout > 0 {
		opts = append(opts, demand.WithHTTPTimeout(r.GitHubHTTPTimeout))
	}
	return demand.NewGitHubRESTPoller(githubBaseURL, string(tokenBytes), opts...), nil
}

// +kubebuilder:rbac:groups=autoscaling.warmrunners.io,resources=warmrunnerpolicies,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.warmrunners.io,resources=warmrunnerpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.warmrunners.io,resources=warmrunnerpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=actions.github.com,resources=autoscalingrunnersets,verbs=get;update
// +kubebuilder:rbac:groups=garm-operator.mercedes-benz.com,resources=pools,verbs=get;update

// Reconcile moves the warm-floor of the target runner backend toward the value
// the scheduler decides from observed GitHub demand. It never deletes runners,
// never exceeds floor.max, and never patches the backend on a demand error.
func (r *WarmRunnerPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pol v1alpha1.WarmRunnerPolicy
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ad, ref, ok := r.adapterFor(pol.Spec.Target)
	if !ok {
		return ctrl.Result{}, nil // invalid target; admission should catch this
	}

	src, srcErr := r.demandSource(ctx, &pol)
	if srcErr != nil {
		// Could not build a demand source (e.g. missing secret). Surface the
		// condition and hold last-known state — do NOT patch the backend.
		reconcileErrors.WithLabelValues(pol.Name, "demand_source").Inc()
		setCondition(&pol, "DemandSourceAvailable", false, errReason(srcErr), errMsg(srcErr), pol.Generation)
		now := metav1.Now()
		pol.Status.LastReconcileTime = &now
		if statusErr := r.Status().Update(ctx, &pol); statusErr != nil {
			reconcileErrors.WithLabelValues(pol.Name, "status_update").Inc()
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: pol.Spec.QueueRule.PollInterval.Duration}, nil
	}

	snap, demErr := src.CurrentDemand(ctx, pol.Spec.GitHub.Owner, pol.Spec.GitHub.Repository, pol.Spec.GitHub.Labels)
	if demErr != nil {
		reconcileErrors.WithLabelValues(pol.Name, "demand_source").Inc()
	}
	setCondition(&pol, "DemandSourceAvailable", demErr == nil, errReason(demErr), errMsg(demErr), pol.Generation)

	current, _ := ad.GetFloor(ctx, ref)
	var lastDec time.Time
	if pol.Status.LastDecreaseTime != nil {
		lastDec = pol.Status.LastDecreaseTime.Time
	}

	dec := r.Scheduler.Decide(pol.Spec, time.Now(), scheduler.Demand{Queued: snap.Queued, Running: snap.Running}, current, lastDec)

	// Predictor leg (v0.2.0). Folded in at the reconciler — Decide is
	// unchanged per spec §3.4. The contribution is added to the candidate
	// max BEFORE the backend-max clamp; the existing cooldown behavior is
	// preserved because Decide already returns currentApplied when a
	// decrease is blocked, so max(decide, predicted) cannot lower the
	// floor inside cooldown.
	//
	// Spec.Predictor.WorkflowRefreshInterval is parsed but not yet wired —
	// the metav1.Duration omitempty + apiserver default does not round-trip
	// through the typed client (zero value emits "0s" and skips defaulting),
	// so a zero value here must be treated as "use the 5m fallback". This
	// matters for future cache-TTL plumbing into the WorkflowFetcher; today
	// the cache TTL is the fetcher's concern, not the reconciler's, so we
	// only note the contract here.
	predictedContrib, predictedLabelSets, perLabelSet, predErr := r.computePredicted(ctx, &pol)
	if predErr != nil {
		log.FromContext(ctx).Error(predErr, "predictor error", "policy", pol.Name)
		setCondition(&pol, "PredictorAvailable", false, "PredictError", predErr.Error(), pol.Generation)
		predictedContrib = 0
	} else if r.Predictor != nil && (pol.Spec.Predictor == nil || pol.Spec.Predictor.Enabled) {
		setCondition(&pol, "PredictorAvailable", true, "Available", "", pol.Generation)
	}

	if predictedContrib > dec.DesiredFloor {
		dec.DesiredFloor = predictedContrib
	}
	// Re-clamp to floor.max (predicted may have raised desired above the cap).
	if dec.DesiredFloor > pol.Spec.Floor.Max {
		dec.DesiredFloor = pol.Spec.Floor.Max
	}

	// Clamp to the backend's own max-runner cap. floor.max may exceed it, in
	// which case the backend would reject the patch live.
	if backendMax, set, maxErr := ad.GetMax(ctx, ref); maxErr == nil && set && dec.DesiredFloor > backendMax {
		dec.DesiredFloor = backendMax
	}

	now := metav1.Now()
	var setErr error
	if demErr == nil && dec.DesiredFloor != current {
		setErr = ad.SetFloor(ctx, ref, dec.DesiredFloor)
		// Stamp LastDecreaseTime only when a decrease actually landed.
		if setErr == nil && dec.DesiredFloor < current {
			pol.Status.LastDecreaseTime = &now
		}
	}
	if setErr != nil {
		reconcileErrors.WithLabelValues(pol.Name, "adapter").Inc()
	}
	setCondition(&pol, "AdapterAvailable", setErr == nil, errReason(setErr), errMsg(setErr), pol.Generation)

	// applied = what's actually on the backend now. On a demand or patch
	// failure the floor was not changed, so it stays at current.
	applied := dec.DesiredFloor
	if demErr != nil || setErr != nil {
		applied = current
	}

	pol.Status.DesiredFloor = dec.DesiredFloor
	pol.Status.AppliedFloor = applied
	pol.Status.LastQueueDepth = snap.Queued
	pol.Status.LastReconcileTime = &now
	pol.Status.PredictedFloor = predictedContrib
	if len(predictedLabelSets) > 0 {
		pol.Status.PredictedLabelSets = predictedLabelSets
	} else {
		pol.Status.PredictedLabelSets = nil
	}
	statusErr := r.Status().Update(ctx, &pol)

	labels := []string{pol.Name, pol.Spec.Target.Kind()}
	desiredFloor.WithLabelValues(labels...).Set(float64(dec.DesiredFloor))
	appliedFloor.WithLabelValues(labels...).Set(float64(applied))
	queueDepth.WithLabelValues(pol.Name).Set(float64(snap.Queued))
	predictedFloorGauge.WithLabelValues(pol.Name).Set(float64(predictedContrib))
	r.emitPredictedJobsMetrics(pol.Name, perLabelSet)
	if demErr == nil && setErr == nil {
		if dec.DesiredFloor > current {
			floorChanges.WithLabelValues(pol.Name, "up").Inc()
		} else if dec.DesiredFloor < current {
			floorChanges.WithLabelValues(pol.Name, "down").Inc()
		}
	}

	// A failed status update (e.g. 409 conflict) must be retried, not dropped.
	if statusErr != nil {
		reconcileErrors.WithLabelValues(pol.Name, "status_update").Inc()
		return ctrl.Result{}, statusErr
	}

	return ctrl.Result{RequeueAfter: pol.Spec.QueueRule.PollInterval.Duration}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WarmRunnerPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WarmRunnerPolicy{}).
		Named("warmrunnerpolicy").
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}

func errReason(err error) string {
	if err == nil {
		return "OK"
	}
	return "Error"
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// predictorTopN caps Status.PredictedLabelSets to keep the CR object size
// bounded. Spec §3.5 example showed two entries; plan §11 settled on N=8.
const predictorTopN = 8

// computePredicted runs the predictor (if enabled) and returns the
// contribution to this policy's floor (sum of counts for label sets that
// match the policy's github.labels filter), the top-N label-set entries
// for status visibility, the full per-label-set map (for the gauge metric),
// and any predictor error. A nil Predictor or a disabled config yields zero
// contribution and no error.
func (r *WarmRunnerPolicyReconciler) computePredicted(
	ctx context.Context,
	pol *v1alpha1.WarmRunnerPolicy,
) (int32, []v1alpha1.PredictedLabelSet, map[string]int, error) {
	if r.Predictor == nil {
		return 0, nil, nil, nil
	}
	if pol.Spec.Predictor != nil && !pol.Spec.Predictor.Enabled {
		return 0, nil, nil, nil
	}

	pred, err := r.Predictor.Predict(ctx, pol.Spec.GitHub.Owner, pol.Spec.GitHub.Repository)
	if err != nil {
		return 0, nil, nil, err
	}

	var contrib int32
	matched := make([]v1alpha1.PredictedLabelSet, 0, len(pred.PerLabelSet))
	want := pol.Spec.GitHub.Labels
	for key, count := range pred.PerLabelSet {
		labels := splitLabelSetKey(key)
		if !labelsSuperset(labels, want) {
			continue
		}
		contrib += int32(count)                                                                    //nolint:gosec // job counts are bounded by maxRunsPerPoll
		matched = append(matched, v1alpha1.PredictedLabelSet{Labels: labels, Count: int32(count)}) //nolint:gosec
	}

	// Deterministic ordering: count desc, then key asc.
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].Count != matched[j].Count {
			return matched[i].Count > matched[j].Count
		}
		return predictor.LabelSetKey(matched[i].Labels) < predictor.LabelSetKey(matched[j].Labels)
	})
	if len(matched) > predictorTopN {
		matched = matched[:predictorTopN]
	}
	if len(matched) == 0 {
		matched = nil
	}
	return contrib, matched, pred.PerLabelSet, nil
}

// splitLabelSetKey inverts predictor.LabelSetKey. The key is a sorted,
// comma-joined unique label list; an empty key yields a nil slice.
func splitLabelSetKey(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, ",")
}

// labelsSuperset reports whether have ⊇ want. Empty want is trivially
// satisfied by any have, matching the reactive labelsMatch direction in
// internal/demand/github_poller.go.
func labelsSuperset(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, l := range have {
		set[l] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

// emitPredictedJobsMetrics sets one warmrunners_predicted_jobs_total sample
// per label set in this reconcile's prediction and prunes samples from
// label sets seen on the previous reconcile but absent now.
func (r *WarmRunnerPolicyReconciler) emitPredictedJobsMetrics(policy string, perLabelSet map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prevPredictedLabels == nil {
		r.prevPredictedLabels = make(map[string]map[string]struct{})
	}
	prev := r.prevPredictedLabels[policy]
	curr := make(map[string]struct{}, len(perLabelSet))
	for key, count := range perLabelSet {
		predictedJobsGauge.WithLabelValues(policy, key).Set(float64(count))
		curr[key] = struct{}{}
	}
	for key := range prev {
		if _, ok := curr[key]; !ok {
			predictedJobsGauge.DeleteLabelValues(policy, key)
		}
	}
	r.prevPredictedLabels[policy] = curr
}

func setCondition(p *v1alpha1.WarmRunnerPolicy, ctype string, ok bool, reason, msg string, generation int64) {
	status := metav1.ConditionTrue
	if !ok {
		status = metav1.ConditionFalse
	}
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == ctype {
			p.Status.Conditions[i].Status = status
			p.Status.Conditions[i].Reason = reason
			p.Status.Conditions[i].Message = msg
			p.Status.Conditions[i].LastTransitionTime = metav1.Now()
			p.Status.Conditions[i].ObservedGeneration = generation
			return
		}
	}
	p.Status.Conditions = append(p.Status.Conditions, metav1.Condition{
		Type: ctype, Status: status, Reason: reason, Message: msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: generation,
	})
}
