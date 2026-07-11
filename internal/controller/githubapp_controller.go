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
	"os"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/webhook"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Condition type/reason constants for GitHubApp.Status.Conditions.
const (
	GitHubAppConditionReady         = "Ready"
	GitHubAppReasonReconciled       = "Reconciled"
	GitHubAppReasonSecretMissing    = "SecretMissing"
	GitHubAppReasonTunnelURLMissing = "TunnelURLMissing"
)

// defaultPodNamespace is the fallback manager namespace used to resolve
// Secret references that don't set an explicit namespace, when POD_NAMESPACE
// is unset (e.g. running outside the in-cluster Deployment).
const defaultPodNamespace = "warmrunners-system"

// tunnelURLMissingRequeue / secretMissingRequeue / healthyRequeue are the
// RequeueAfter durations for each terminal state in Reconcile.
const (
	tunnelURLMissingRequeue = 60 * time.Second
	secretMissingRequeue    = 30 * time.Second
	steadyStateRequeue      = 60 * time.Second
)

// webhookHealthyWindow bounds how recent status.lastDelivery must be for
// ingress-mode webhookHealthy to read true.
const webhookHealthyWindow = 15 * time.Minute

// GitHubAppReconciler reconciles a GitHubApp CR — the cluster-scoped
// singleton that holds the webhook receiver credentials and picks the
// ingress mode (ingress or tunnel).
type GitHubAppReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Tunnels *webhook.TunnelRegistry
}

// +kubebuilder:rbac:groups=autoscaling.warmrunners.io,resources=githubapps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=autoscaling.warmrunners.io,resources=githubapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.warmrunners.io,resources=githubapps/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile keeps a GitHubApp CR's tunnel (if any) running, verifies its
// referenced Secrets exist, and refreshes status.webhookHealthy /
// Ready condition every 60s regardless of event flow.
func (r *GitHubAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var app v1alpha1.GitHubApp
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch app.Spec.Ingress.Mode {
	case v1alpha1.IngressModeTunnel:
		if app.Spec.Ingress.Tunnel == nil || app.Spec.Ingress.Tunnel.RelayURL == "" {
			setGitHubAppCondition(&app, false, GitHubAppReasonTunnelURLMissing, "spec.ingress.tunnel.relayURL is required in tunnel mode")
			if err := r.Status().Update(ctx, &app); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: tunnelURLMissingRequeue}, nil
		}
		r.Tunnels.Ensure(app.Name, app.Spec.Ingress.Tunnel.RelayURL)
	case v1alpha1.IngressModeIngress:
		r.Tunnels.Stop(app.Name)
	}

	if err := r.checkSecret(ctx, app.Spec.PrivateKeyRef); err != nil {
		setGitHubAppCondition(&app, false, GitHubAppReasonSecretMissing, err.Error())
		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: secretMissingRequeue}, nil
	}
	if err := r.checkSecret(ctx, app.Spec.WebhookSecretRef); err != nil {
		setGitHubAppCondition(&app, false, GitHubAppReasonSecretMissing, err.Error())
		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: secretMissingRequeue}, nil
	}

	switch app.Spec.Ingress.Mode {
	case v1alpha1.IngressModeTunnel:
		tc := r.Tunnels.Get(app.Name)
		app.Status.WebhookHealthy = tc != nil && tc.Connected()
	case v1alpha1.IngressModeIngress:
		app.Status.WebhookHealthy = app.Status.LastDelivery != nil &&
			time.Since(app.Status.LastDelivery.Time) < webhookHealthyWindow
	}

	setGitHubAppCondition(&app, true, GitHubAppReasonReconciled, "")
	if err := r.Status().Update(ctx, &app); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: steadyStateRequeue}, nil
}

// checkSecret verifies the Secret referenced by ref exists. Namespace
// resolution: ref.Namespace when set, otherwise the manager's own namespace
// (POD_NAMESPACE, falling back to defaultPodNamespace).
func (r *GitHubAppReconciler) checkSecret(ctx context.Context, ref v1alpha1.SecretKeyRef) error {
	ns := ref.Namespace
	if ns == "" {
		ns = podNamespace()
	}
	var secret corev1.Secret
	return r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &secret)
}

// podNamespace resolves the manager's own namespace from POD_NAMESPACE,
// falling back to defaultPodNamespace when unset.
func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return defaultPodNamespace
}

// setGitHubAppCondition sets the Ready condition on app.Status.Conditions.
// Delegated to apimachinery so LastTransitionTime only advances on an actual
// status transition, matching the WarmRunnerPolicy controller's setCondition.
func setGitHubAppCondition(app *v1alpha1.GitHubApp, ok bool, reason, msg string) {
	status := metav1.ConditionTrue
	if !ok {
		status = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               GitHubAppConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: app.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitHubAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GitHubApp{}).
		Named("githubapp").
		Complete(r)
}
