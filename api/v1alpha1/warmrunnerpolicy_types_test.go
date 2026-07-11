package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// crdSchemaProps unmarshals the generated CRD's OpenAPI v3 property map for
// spec and status so tests can pin admission-time defaults/validation
// against the real generated contract rather than re-deriving it from
// source comments.
func crdSchemaProps(t *testing.T) map[string]interface{} {
	t.Helper()
	path := filepath.Join("..", "..", "config", "crd", "bases",
		"autoscaling.warmrunners.io_warmrunnerpolicies.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated CRD: %v", err)
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal generated CRD: %v", err)
	}
	versions := doc["spec"].(map[string]interface{})["versions"].([]interface{})
	v0 := versions[0].(map[string]interface{})
	schema := v0["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	return schema["properties"].(map[string]interface{})
}

func TestWRP_ActiveWindowDefaultsTo600(t *testing.T) {
	props := crdSchemaProps(t)
	specProps := props["spec"].(map[string]interface{})["properties"].(map[string]interface{})
	aws, ok := specProps["activeWindowSeconds"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.activeWindowSeconds not present in generated CRD")
	}
	def, ok := aws["default"]
	if !ok {
		t.Fatalf("spec.activeWindowSeconds has no default in generated CRD")
	}
	got, ok := def.(float64)
	if !ok || got != 600 {
		t.Fatalf("spec.activeWindowSeconds default = %v, want 600", def)
	}

	var spec WarmRunnerPolicySpec
	if spec.ActiveWindowSeconds != nil {
		t.Fatalf("zero-value WarmRunnerPolicySpec.ActiveWindowSeconds = %v, want nil", spec.ActiveWindowSeconds)
	}
}

func TestWRP_ActiveWindowValidation(t *testing.T) {
	props := crdSchemaProps(t)
	specProps := props["spec"].(map[string]interface{})["properties"].(map[string]interface{})
	aws, ok := specProps["activeWindowSeconds"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.activeWindowSeconds not present in generated CRD")
	}
	if min, ok := aws["minimum"].(float64); !ok || min != 60 {
		t.Fatalf("spec.activeWindowSeconds minimum = %v, want 60", aws["minimum"])
	}
	if max, ok := aws["maximum"].(float64); !ok || max != 3600 {
		t.Fatalf("spec.activeWindowSeconds maximum = %v, want 3600", aws["maximum"])
	}
}

func TestWRP_LastEventSourceEnum(t *testing.T) {
	props := crdSchemaProps(t)
	statusProps := props["status"].(map[string]interface{})["properties"].(map[string]interface{})
	les, ok := statusProps["lastEventSource"].(map[string]interface{})
	if !ok {
		t.Fatalf("status.lastEventSource not present in generated CRD")
	}
	enumRaw, ok := les["enum"].([]interface{})
	if !ok || len(enumRaw) != 2 {
		t.Fatalf("status.lastEventSource enum = %v, want [webhook poll]", les["enum"])
	}
	if enumRaw[0] != "webhook" || enumRaw[1] != "poll" {
		t.Fatalf("status.lastEventSource enum = %v, want [webhook poll]", enumRaw)
	}

	if LastEventSourceWebhook != "webhook" {
		t.Fatalf("LastEventSourceWebhook = %q, want %q", LastEventSourceWebhook, "webhook")
	}
	if LastEventSourcePoll != "poll" {
		t.Fatalf("LastEventSourcePoll = %q, want %q", LastEventSourcePoll, "poll")
	}
}

func TestTargetKind(t *testing.T) {
	cases := []struct {
		name string
		in   Target
		want string
	}{
		{"arc only", Target{Arc: &ArcTarget{}}, "arc"},
		{"garm only", Target{Garm: &GarmTarget{}}, "garm"},
		{"both set", Target{Arc: &ArcTarget{}, Garm: &GarmTarget{}}, ""},
		{"none set", Target{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.Kind(); got != c.want {
				t.Fatalf("Kind() = %q, want %q", got, c.want)
			}
		})
	}
}
