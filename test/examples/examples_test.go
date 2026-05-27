package examples_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sarataha/warmrunners/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// TestExamplesAreValid parses every examples/*.yaml into the typed
// WarmRunnerPolicy and checks apiVersion, kind, and required fields. This guards
// against the class of bug where an example ships a wrong apiVersion (e.g.
// warmrunners.io vs warmrunners.warmrunners.io) or omits a now-required field
// and fails the moment a user runs `kubectl apply`.
func TestExamplesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "examples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	const wantAPIVersion = "warmrunners.warmrunners.io/v1alpha1"
	found := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".yaml" && filepath.Ext(e.Name()) != ".yml" {
			continue
		}
		found++
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var p v1alpha1.WarmRunnerPolicy
		if err := yaml.UnmarshalStrict(data, &p); err != nil {
			t.Fatalf("%s: does not parse as WarmRunnerPolicy: %v", e.Name(), err)
		}
		if p.APIVersion != wantAPIVersion {
			t.Errorf("%s: apiVersion = %q, want %q", e.Name(), p.APIVersion, wantAPIVersion)
		}
		if p.Kind != "WarmRunnerPolicy" {
			t.Errorf("%s: kind = %q, want WarmRunnerPolicy", e.Name(), p.Kind)
		}
		if p.Spec.GitHub.Owner == "" || p.Spec.GitHub.Repository == "" {
			t.Errorf("%s: github.owner and github.repository are required", e.Name())
		}
		if len(p.Spec.GitHub.Labels) == 0 {
			t.Errorf("%s: github.labels is required", e.Name())
		}
		if p.Spec.Target.Kind() == "" {
			t.Errorf("%s: exactly one of target.arc/target.garm is required", e.Name())
		}
	}
	if found == 0 {
		t.Fatal("no example YAML files found")
	}
}
