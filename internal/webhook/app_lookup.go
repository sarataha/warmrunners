package webhook

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/go-logr/logr"
	"github.com/sarataha/warmrunners/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultAppLookupNamespace is the fallback manager namespace used to
// resolve a GitHubApp's webhook Secret when spec.webhookSecretRef.namespace
// is unset and POD_NAMESPACE is also unset.
const defaultAppLookupNamespace = "warmrunners-system"

// CachedAppLookup satisfies AppLookup by resolving a GitHubApp CR by its
// GitHub-side App ID and the referenced webhook Secret. Uses the manager's
// client-runtime cache under the hood, no additional layer here.
type CachedAppLookup struct {
	c   client.Client
	log logr.Logger
}

// NewCachedAppLookup constructs a CachedAppLookup.
func NewCachedAppLookup(c client.Client, log logr.Logger) *CachedAppLookup {
	return &CachedAppLookup{c: c, log: log}
}

// Resolve implements AppLookup. targetID is the raw
// X-GitHub-Hook-Installation-Target-ID header value — the GitHub App ID as
// a decimal string.
func (l *CachedAppLookup) Resolve(ctx context.Context, targetID string) (*v1alpha1.GitHubApp, []byte, error) {
	parsedID, err := strconv.ParseInt(targetID, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("unknown app: bad target id %q", targetID)
	}

	var list v1alpha1.GitHubAppList
	if err := l.c.List(ctx, &list); err != nil {
		return nil, nil, fmt.Errorf("list GitHubApps: %w", err)
	}

	var app *v1alpha1.GitHubApp
	for i := range list.Items {
		if list.Items[i].Spec.AppID == parsedID {
			app = &list.Items[i]
			break
		}
	}
	if app == nil {
		return nil, nil, fmt.Errorf("unknown app: no GitHubApp with appID=%d", parsedID)
	}

	ref := app.Spec.WebhookSecretRef
	ns := ref.Namespace
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	if ns == "" {
		ns = defaultAppLookupNamespace
	}

	var secret corev1.Secret
	if err := l.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &secret); err != nil {
		return nil, nil, fmt.Errorf("webhook secret: %w", err)
	}

	secretBytes, ok := secret.Data[ref.Key]
	if !ok || len(secretBytes) == 0 {
		return nil, nil, fmt.Errorf("webhook secret: key %q not found", ref.Key)
	}

	return app, secretBytes, nil
}
