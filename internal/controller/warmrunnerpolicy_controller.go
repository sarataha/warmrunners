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
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/adapter"
	"github.com/sarataha/warmrunners/internal/demand"
	"github.com/sarataha/warmrunners/internal/scheduler"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	return demand.NewGitHubRESTPoller(githubBaseURL, string(tokenBytes)), nil
}

// +kubebuilder:rbac:groups=warmrunners.warmrunners.io,resources=warmrunnerpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=warmrunners.warmrunners.io,resources=warmrunnerpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=warmrunners.warmrunners.io,resources=warmrunnerpolicies/finalizers,verbs=update
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
		setCondition(&pol, "DemandSourceAvailable", false, errReason(srcErr), errMsg(srcErr))
		now := metav1.Now()
		pol.Status.LastReconcileTime = &now
		_ = r.Status().Update(ctx, &pol)
		return ctrl.Result{RequeueAfter: pol.Spec.QueueRule.PollInterval.Duration}, nil
	}

	snap, demErr := src.CurrentDemand(ctx, pol.Spec.GitHub.Owner, pol.Spec.GitHub.Repository, pol.Spec.GitHub.Labels)
	setCondition(&pol, "DemandSourceAvailable", demErr == nil, errReason(demErr), errMsg(demErr))

	current, _ := ad.GetFloor(ctx, ref)
	var lastDec time.Time
	if pol.Status.LastReconcileTime != nil {
		lastDec = pol.Status.LastReconcileTime.Time
	}

	dec := r.Scheduler.Decide(pol.Spec, time.Now(), scheduler.Demand{Queued: snap.Queued, Running: snap.Running}, current, lastDec)

	var setErr error
	if demErr == nil && dec.DesiredFloor != current {
		setErr = ad.SetFloor(ctx, ref, dec.DesiredFloor)
	}
	setCondition(&pol, "AdapterAvailable", setErr == nil, errReason(setErr), errMsg(setErr))

	pol.Status.DesiredFloor = dec.DesiredFloor
	pol.Status.AppliedFloor = dec.DesiredFloor
	pol.Status.LastQueueDepth = snap.Queued
	now := metav1.Now()
	pol.Status.LastReconcileTime = &now
	_ = r.Status().Update(ctx, &pol)

	applied := dec.DesiredFloor
	if demErr != nil || setErr != nil {
		applied = current
	}
	labels := []string{pol.Name, pol.Spec.Target.Kind()}
	desiredFloor.WithLabelValues(labels...).Set(float64(dec.DesiredFloor))
	appliedFloor.WithLabelValues(labels...).Set(float64(applied))
	queueDepth.WithLabelValues(pol.Name).Set(float64(snap.Queued))
	if demErr == nil && setErr == nil {
		if dec.DesiredFloor > current {
			floorChanges.WithLabelValues(pol.Name, "up").Inc()
		} else if dec.DesiredFloor < current {
			floorChanges.WithLabelValues(pol.Name, "down").Inc()
		}
	}

	return ctrl.Result{RequeueAfter: pol.Spec.QueueRule.PollInterval.Duration}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WarmRunnerPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WarmRunnerPolicy{}).
		Named("warmrunnerpolicy").
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

func setCondition(p *v1alpha1.WarmRunnerPolicy, ctype string, ok bool, reason, msg string) {
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
			return
		}
	}
	p.Status.Conditions = append(p.Status.Conditions, metav1.Condition{
		Type: ctype, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.Now(),
	})
}
