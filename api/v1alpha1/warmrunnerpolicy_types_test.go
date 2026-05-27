package v1alpha1

import "testing"

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
