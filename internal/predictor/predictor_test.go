package predictor

import "testing"

func TestLabelSetKey(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{
			name: "empty slice",
			in:   nil,
			want: "",
		},
		{
			name: "empty non-nil slice",
			in:   []string{},
			want: "",
		},
		{
			name: "single label",
			in:   []string{"ubuntu-latest"},
			want: "ubuntu-latest",
		},
		{
			name: "two labels in order",
			in:   []string{"gpu", "self-hosted"},
			want: "gpu,self-hosted",
		},
		{
			name: "two labels reversed produce same key",
			in:   []string{"self-hosted", "gpu"},
			want: "gpu,self-hosted",
		},
		{
			name: "duplicates collapsed",
			in:   []string{"self-hosted", "gpu", "gpu", "self-hosted"},
			want: "gpu,self-hosted",
		},
		{
			name: "case preserved (not folded)",
			in:   []string{"Self-Hosted", "self-hosted"},
			want: "Self-Hosted,self-hosted",
		},
		{
			name: "three labels arbitrary order",
			in:   []string{"linux", "x64", "self-hosted"},
			want: "linux,self-hosted,x64",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LabelSetKey(tc.in)
			if got != tc.want {
				t.Fatalf("LabelSetKey(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLabelSetKey_OrderIndependence(t *testing.T) {
	a := LabelSetKey([]string{"a", "b", "c"})
	b := LabelSetKey([]string{"c", "a", "b"})
	c := LabelSetKey([]string{"b", "c", "a"})
	if a != b || b != c {
		t.Fatalf("keys differ across permutations: %q / %q / %q", a, b, c)
	}
}

func TestLabelSetKey_DoesNotMutateInput(t *testing.T) {
	in := []string{"self-hosted", "gpu"}
	_ = LabelSetKey(in)
	if in[0] != "self-hosted" || in[1] != "gpu" {
		t.Fatalf("input mutated: %v", in)
	}
}

// Compile-time check that Prediction has the expected shape.
var _ = Prediction{PerLabelSet: map[string]int{}}
