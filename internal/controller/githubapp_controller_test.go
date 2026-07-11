package controller

import (
	"context"
	"testing"
	"time"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/webhook"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeTunnel is a controllable TunnelClient stand-in so tests can assert on
// registry identity/state without dialing a real WebSocket.
type fakeTunnel struct {
	connected bool
}

func (f *fakeTunnel) Start(ctx context.Context, relayURL string) error {
	<-ctx.Done()
	return nil
}

func (f *fakeTunnel) Connected() bool { return f.connected }

func newTunnelGitHubApp(name, relayURL string) *v1alpha1.GitHubApp {
	return &v1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.GitHubAppSpec{
			AppID:            1,
			PrivateKeyRef:    v1alpha1.SecretKeyRef{Name: "pk", Key: "key", Namespace: "default"},
			WebhookSecretRef: v1alpha1.SecretKeyRef{Name: "whs", Key: "key", Namespace: "default"},
			Ingress: v1alpha1.GitHubAppIngress{
				Mode:   v1alpha1.IngressModeTunnel,
				Tunnel: &v1alpha1.TunnelSpec{RelayURL: relayURL},
			},
		},
	}
}

func TestGitHubAppController_TunnelStartedOnCreate(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)

	app := newTunnelGitHubApp("app-a", "wss://a")
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(app).WithStatusSubresource(app).Build()

	reg := webhook.NewTunnelRegistry(func() webhook.TunnelClient { return &fakeTunnel{connected: true} })
	r := &GitHubAppReconciler{Client: cl, Scheme: sch, Tunnels: reg}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "app-a"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if r.Tunnels.Get("app-a") == nil {
		t.Fatal("expected tunnel to be running for app-a")
	}
}

func TestGitHubAppController_TunnelReplacedOnURLChange(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)

	app := newTunnelGitHubApp("app-b", "wss://a")
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(app).WithStatusSubresource(app).Build()

	reg := webhook.NewTunnelRegistry(func() webhook.TunnelClient { return &fakeTunnel{connected: true} })
	r := &GitHubAppReconciler{Client: cl, Scheme: sch, Tunnels: reg}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "app-b"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	first := r.Tunnels.Get("app-b")
	if first == nil {
		t.Fatal("expected tunnel after first reconcile")
	}

	var got v1alpha1.GitHubApp
	if err := cl.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Ingress.Tunnel.RelayURL = "wss://b"
	if err := cl.Update(ctx, &got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	second := r.Tunnels.Get("app-b")
	if second == nil {
		t.Fatal("expected tunnel after second reconcile")
	}
	if first == second {
		t.Fatal("expected a new tunnel instance after relayURL change")
	}
}

func TestGitHubAppController_SecretMissingSetsCondition(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)

	app := &v1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app-c"},
		Spec: v1alpha1.GitHubAppSpec{
			AppID:            1,
			PrivateKeyRef:    v1alpha1.SecretKeyRef{Name: "missing-pk", Key: "key", Namespace: "default"},
			WebhookSecretRef: v1alpha1.SecretKeyRef{Name: "missing-whs", Key: "key", Namespace: "default"},
			Ingress:          v1alpha1.GitHubAppIngress{Mode: v1alpha1.IngressModeIngress},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(app).WithStatusSubresource(app).Build()

	reg := webhook.NewTunnelRegistry(func() webhook.TunnelClient { return &fakeTunnel{connected: true} })
	r := &GitHubAppReconciler{Client: cl, Scheme: sch, Tunnels: reg}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "app-c"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got v1alpha1.GitHubApp
	if err := cl.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, GitHubAppConditionReady)
	if cond == nil {
		t.Fatal("expected Ready condition to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready.Status = %v, want False", cond.Status)
	}
	if cond.Reason != GitHubAppReasonSecretMissing {
		t.Fatalf("Ready.Reason = %q, want %q", cond.Reason, GitHubAppReasonSecretMissing)
	}
}

func TestGitHubAppController_HealthGaugeReflectsDelivery(t *testing.T) {
	sch := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)

	pk := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pk", Namespace: "default"},
		Data:       map[string][]byte{"key": []byte("private-key")},
	}
	whs := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "whs", Namespace: "default"},
		Data:       map[string][]byte{"key": []byte("webhook-secret")},
	}

	recent := metav1.NewTime(time.Now())
	app := &v1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app-d"},
		Spec: v1alpha1.GitHubAppSpec{
			AppID:            1,
			PrivateKeyRef:    v1alpha1.SecretKeyRef{Name: "pk", Key: "key", Namespace: "default"},
			WebhookSecretRef: v1alpha1.SecretKeyRef{Name: "whs", Key: "key", Namespace: "default"},
			Ingress:          v1alpha1.GitHubAppIngress{Mode: v1alpha1.IngressModeIngress},
		},
		Status: v1alpha1.GitHubAppStatus{LastDelivery: &recent},
	}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(app, pk, whs).WithStatusSubresource(app).Build()

	reg := webhook.NewTunnelRegistry(func() webhook.TunnelClient { return &fakeTunnel{connected: true} })
	r := &GitHubAppReconciler{Client: cl, Scheme: sch, Tunnels: reg}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "app-d"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	var got v1alpha1.GitHubApp
	if err := cl.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Status.WebhookHealthy {
		t.Fatal("expected webhookHealthy=true with recent lastDelivery")
	}

	stale := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	got.Status.LastDelivery = &stale
	if err := cl.Status().Update(ctx, &got); err != nil {
		t.Fatalf("Status().Update: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	var got2 v1alpha1.GitHubApp
	if err := cl.Get(ctx, req.NamespacedName, &got2); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got2.Status.WebhookHealthy {
		t.Fatal("expected webhookHealthy=false with stale lastDelivery")
	}
}
