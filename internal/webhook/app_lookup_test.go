package webhook

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/sarataha/warmrunners/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newAppLookupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return scheme
}

func TestCachedAppLookup_ResolveHappy(t *testing.T) {
	scheme := newAppLookupScheme(t)

	app := &v1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1"},
		Spec: v1alpha1.GitHubAppSpec{
			AppID: 42,
			PrivateKeyRef: v1alpha1.SecretKeyRef{
				Name: "pk",
				Key:  "private-key.pem",
			},
			WebhookSecretRef: v1alpha1.SecretKeyRef{
				Name:      "wh-secret",
				Namespace: "wr-ns",
				Key:       "secret",
			},
			Ingress: v1alpha1.GitHubAppIngress{
				Mode: v1alpha1.IngressModeIngress,
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wh-secret", Namespace: "wr-ns"},
		Data: map[string][]byte{
			"secret": []byte("s3kr3t"),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(app, secret).Build()
	lookup := NewCachedAppLookup(c, testr.New(t))

	gotApp, gotSecret, err := lookup.Resolve(context.Background(), "42")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if gotApp == nil || gotApp.Name != "app-1" {
		t.Fatalf("Resolve() app = %+v, want app-1", gotApp)
	}
	if string(gotSecret) != "s3kr3t" {
		t.Fatalf("Resolve() secret = %q, want %q", gotSecret, "s3kr3t")
	}
}

func TestCachedAppLookup_ResolveUnknownTarget(t *testing.T) {
	scheme := newAppLookupScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	lookup := NewCachedAppLookup(c, testr.New(t))

	_, _, err := lookup.Resolve(context.Background(), "999")
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for unknown target")
	}

	_, _, err = lookup.Resolve(context.Background(), "not-a-number")
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for bad target id")
	}
}

func TestCachedAppLookup_ResolveSecretMissing(t *testing.T) {
	scheme := newAppLookupScheme(t)

	app := &v1alpha1.GitHubApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app-2"},
		Spec: v1alpha1.GitHubAppSpec{
			AppID: 7,
			PrivateKeyRef: v1alpha1.SecretKeyRef{
				Name: "pk",
				Key:  "private-key.pem",
			},
			WebhookSecretRef: v1alpha1.SecretKeyRef{
				Name: "missing-secret",
				Key:  "secret",
			},
			Ingress: v1alpha1.GitHubAppIngress{
				Mode: v1alpha1.IngressModeIngress,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	lookup := NewCachedAppLookup(c, testr.New(t))

	_, _, err := lookup.Resolve(context.Background(), "7")
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for missing secret")
	}
}
