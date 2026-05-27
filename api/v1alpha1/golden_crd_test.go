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

package v1alpha1

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestGoldenCRD pins the generated CRD YAML against a checked-in golden copy.
// Any drift means `make manifests` produced different output than expected —
// usually because a kubebuilder marker was added, removed, or changed.
// To regenerate after an intentional change:
//
//	cp config/crd/bases/autoscaling.warmrunners.io_warmrunnerpolicies.yaml \
//	   api/v1alpha1/testdata/golden_crd.yaml
func TestGoldenCRD(t *testing.T) {
	generatedPath := filepath.Join("..", "..", "config", "crd", "bases",
		"autoscaling.warmrunners.io_warmrunnerpolicies.yaml")
	goldenPath := filepath.Join("testdata", "golden_crd.yaml")

	generated, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("read generated CRD: %v", err)
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden CRD: %v", err)
	}
	if !bytes.Equal(generated, golden) {
		t.Fatalf("CRD drift detected: %s differs from %s. If the change is intentional, run:\n  cp %s %s",
			generatedPath, goldenPath, generatedPath, goldenPath)
	}
}
