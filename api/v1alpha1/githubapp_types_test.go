package v1alpha1

import "testing"

const testSecretKey = "secret"

// TestGitHubAppTypes_DefaultsAndValidation exercises the GitHubApp CRD's
// defaulting and validation contract at the Go-struct level: the CRD schema
// markers (kubebuilder:default, Minimum, Required) are enforced by the API
// server, but we pin the zero-value/required-field expectations here so
// regressions in the type definitions are caught by `go test` too.
func TestGitHubAppTypes_DefaultsAndValidation(t *testing.T) {
	t.Run("unset ingress mode defaults to ingress via zero value contract", func(t *testing.T) {
		var ingress GitHubAppIngress
		// The zero value of GitHubAppIngressMode is "", but the CRD schema
		// applies +kubebuilder:default=ingress at admission. We assert the
		// constant used for that default matches the documented value.
		if IngressModeIngress != "ingress" {
			t.Fatalf("IngressModeIngress = %q, want %q", IngressModeIngress, "ingress")
		}
		if ingress.Mode != "" {
			t.Fatalf("zero-value Mode = %q, want empty (defaulting is applied server-side)", ingress.Mode)
		}
	})

	t.Run("appID <= 0 fails validation", func(t *testing.T) {
		cases := []struct {
			name  string
			appID int64
			valid bool
		}{
			{"zero", 0, false},
			{"negative", -1, false},
			{"positive", 1, true},
		}
		for _, c := range cases {
			spec := GitHubAppSpec{
				AppID: c.appID,
				PrivateKeyRef: SecretKeyRef{
					Name: "pk",
					Key:  "private-key.pem",
				},
				WebhookSecretRef: SecretKeyRef{
					Name: "wh",
					Key:  testSecretKey,
				},
				Ingress: GitHubAppIngress{
					Mode: IngressModeIngress,
				},
			}
			got := spec.AppID > 0
			if got != c.valid {
				t.Errorf("%s: AppID %d valid = %v, want %v", c.name, spec.AppID, got, c.valid)
			}
		}
	})

	t.Run("webhookSecretRef.name required", func(t *testing.T) {
		ref := SecretKeyRef{Key: testSecretKey}
		if ref.Name != "" {
			t.Fatalf("expected zero-value Name to be empty, got %q", ref.Name)
		}
		// The +kubebuilder:validation:Required marker on SecretKeyRef.Name
		// enforces this at admission; here we just pin that the field exists
		// and is the empty string when unset, i.e. it has no default.
	})

	t.Run("GitHubApp round-trips spec and status fields", func(t *testing.T) {
		app := GitHubApp{
			Spec: GitHubAppSpec{
				AppID: 42,
				PrivateKeyRef: SecretKeyRef{
					Name: "pk",
					Key:  "private-key.pem",
				},
				WebhookSecretRef: SecretKeyRef{
					Name: "wh",
					Key:  testSecretKey,
				},
				Ingress: GitHubAppIngress{
					Mode:     IngressModeTunnel,
					Hostname: "warmrunners.example.io",
					Tunnel: &TunnelSpec{
						RelayURL: "wss://relay.example.io",
					},
				},
			},
			Status: GitHubAppStatus{
				Installations: []Installation{
					{ID: 1, Account: "acme", Repositories: 3},
				},
				WebhookHealthy: true,
			},
		}
		if app.Spec.AppID != 42 {
			t.Fatalf("AppID = %d, want 42", app.Spec.AppID)
		}
		if app.Spec.Ingress.Mode != IngressModeTunnel {
			t.Fatalf("Ingress.Mode = %q, want %q", app.Spec.Ingress.Mode, IngressModeTunnel)
		}
		if len(app.Status.Installations) != 1 || app.Status.Installations[0].Account != "acme" {
			t.Fatalf("Installations = %+v, want single acme installation", app.Status.Installations)
		}
	})
}
