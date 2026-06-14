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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	warmrunnersv1alpha1 "github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/activity"
	"github.com/sarataha/warmrunners/internal/controller"
	"github.com/sarataha/warmrunners/internal/predictor"
	"github.com/sarataha/warmrunners/internal/scheduler"
	"github.com/sarataha/warmrunners/internal/version"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(warmrunnersv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var maxConcurrentReconciles int
	var logLevel string
	var githubHTTPTimeout time.Duration
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabled by default so multi-replica deployments are safe; pass "+
			"--leader-elect=false to disable for single-process development.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 1,
		"Maximum number of concurrent reconciles for the WarmRunnerPolicy controller.")
	flag.StringVar(&logLevel, "log-level", "info",
		"Log verbosity. One of: debug, info, warn, error.")
	flag.DurationVar(&githubHTTPTimeout, "github-http-timeout", 10*time.Second,
		"Timeout for each GitHub REST API request made by the demand poller.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	lvl, err := parseLogLevel(logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	opts.Level = lvl
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "67406384.warmrunners.io",
		// Safe because main exits immediately after Start returns; speeds up
		// voluntary leader handoff (no LeaseDuration wait).
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Predictor (v0.2.0) + Activity sampler (v0.3.0). Both share the same
	// *http.Client and *WorkflowFetcher — the fetcher's YAML cache is
	// process-shared, so the activity sampler pays no extra fetch cost for
	// workflows the predictor already touched (and vice versa). One
	// *WorkflowNeedsGraph and one *WorkflowRunsSampler per process are
	// shared across all WarmRunnerPolicy reconciles; the per-policy GitHub
	// token is plumbed at reconcile time (resolved from each policy's auth
	// secretRef and passed positionally to Predict / Sample / Fetch), not
	// at construction. v0.2.0 shipped with no auth wiring here and observed
	// 404s on every /actions/runs call against private/rate-limited repos;
	// v0.2.1 closed that path.
	//
	// TODO(v0.2.x): per-policy MaxRunsPerPoll plumbing. Today the
	// constructor takes a process-global cap (0 = DefaultRunsCap = 50).
	predictorHTTPClient := &http.Client{Timeout: githubHTTPTimeout}
	predictorFetcher := predictor.NewWorkflowFetcher(predictorHTTPClient, version.Version)
	predictorImpl := predictor.NewWorkflowNeedsGraph(predictorHTTPClient, predictorFetcher, 0).
		WithHooks(predictor.Hooks{
			// The {policy} label is intentionally dropped from
			// warmrunners_workflow_yaml_fetch_total — the predictor is
			// shared across all policies, so attaching a policy label
			// here would require moving construction into the reconciler.
			// See metrics.go for the rationale.
			OnYAMLFetch: func(result string) {
				controller.RecordWorkflowYAMLFetch(result)
			},
			OnDynamicSkipped: func(_ string) {
				// All dynamic-skip reasons collapse to one counter label
				// in v0.2.0. The reason granularity stays available via
				// logs; promoting it to a metric label costs cardinality
				// for little operator value.
				controller.RecordWorkflowYAMLFetch("dynamic_skipped")
			},
		})

	// Activity sampler (v0.3.0). Reuses predictorHTTPClient + predictorFetcher
	// so YAML fetches are deduplicated between the two signals. runsCap=0
	// → DefaultRunsCap (50), mirroring the predictor. No CLI flag: activity
	// is fully CRD-driven via spec.activity.*.
	activityImpl := activity.NewWorkflowRunsSampler(predictorHTTPClient, predictorFetcher, 0).
		WithHooks(activity.Hooks{
			OnBotFiltered: func(reason string) {
				controller.IncActivityBotFiltered(reason)
			},
			OnYAMLFetch: func(result string) {
				// Shared counter with the predictor — the fetcher is
				// the same instance, so any consumer's fetch contributes
				// to the same fleet-wide outcome distribution.
				controller.RecordWorkflowYAMLFetch(result)
			},
			OnDynamicSkipped: func(_ string) {
				controller.RecordWorkflowYAMLFetch("dynamic_skipped")
			},
			// OnEventFiltered intentionally left nil: no metric in
			// v0.3.0 (event cardinality is small but not operator-actionable).
		})

	if err := (&controller.WarmRunnerPolicyReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Scheduler:               scheduler.NewHeuristic(),
		Demand:                  nil, // resolved per-policy from the auth secretRef
		MaxConcurrentReconciles: maxConcurrentReconciles,
		GitHubHTTPTimeout:       githubHTTPTimeout,
		Predictor:               predictorImpl,
		Activity:                activityImpl,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WarmRunnerPolicy")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// parseLogLevel maps a string flag value to a zapcore.Level. Accepted values
// are debug, info, warn, error.
func parseLogLevel(s string) (zapcore.Level, error) {
	switch s {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info":
		return zapcore.InfoLevel, nil
	case "warn":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, fmt.Errorf("invalid --log-level %q: want one of debug|info|warn|error", s)
	}
}
