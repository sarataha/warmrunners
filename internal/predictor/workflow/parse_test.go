package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("..", "testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParse_Simple(t *testing.T) {
	wf, err := Parse(load(t, "simple.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(wf.Jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(wf.Jobs))
	}
	j, ok := wf.Jobs["build"]
	if !ok {
		t.Fatalf("missing job 'build'")
	}
	if !reflect.DeepEqual(j.RunsOn.Labels, []string{"ubuntu-latest"}) {
		t.Errorf("labels: got %v", j.RunsOn.Labels)
	}
	if j.RunsOn.Dynamic || j.Dynamic {
		t.Errorf("should not be dynamic: %+v", j)
	}
	if len(j.Needs) != 0 {
		t.Errorf("needs should be empty, got %v", j.Needs)
	}
}

func TestParse_RunsOnArrayAndGroup(t *testing.T) {
	wf, err := Parse(load(t, "matrix.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ar := wf.Jobs["arrayrunner"]
	if !reflect.DeepEqual(ar.RunsOn.Labels, []string{"self-hosted", "linux", "x64"}) {
		t.Errorf("array labels: got %v", ar.RunsOn.Labels)
	}
	gf := wf.Jobs["groupform"]
	if gf.RunsOn.Group != "my-group" {
		t.Errorf("group: got %q", gf.RunsOn.Group)
	}
	if !reflect.DeepEqual(gf.RunsOn.Labels, []string{"a", "b"}) {
		t.Errorf("group labels: got %v", gf.RunsOn.Labels)
	}
}

func TestParse_MatrixExpansion(t *testing.T) {
	wf, err := Parse(load(t, "matrix.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	j := wf.Jobs["test"]
	if j.Matrix.Dynamic {
		t.Fatalf("matrix should not be dynamic")
	}
	// Axes: os ∈ {ubuntu-latest, macos-latest}, node ∈ {18, 20}.
	// Base = 4. Exclude (macos-latest, 18) → 3. Include (ubuntu-latest, 21)
	// is a new combo (21 not in node axis) → 4 total.
	if got := len(j.Matrix.Combos); got != 4 {
		// Build a human-readable rendering on failure.
		var rendered []string
		for _, c := range j.Matrix.Combos {
			rendered = append(rendered, c["os"]+"/"+c["node"])
		}
		sort.Strings(rendered)
		t.Fatalf("want 4 combos, got %d: %v", got, rendered)
	}
	// runs-on: ${{ matrix.os }} should resolve to a concrete label for the
	// first combo (deterministic via sorted axis names).
	if j.RunsOn.Dynamic {
		t.Errorf("runs-on should not be dynamic after matrix resolution: %+v", j.RunsOn)
	}
	if len(j.RunsOn.Labels) != 1 {
		t.Errorf("want 1 resolved label, got %v", j.RunsOn.Labels)
	}
}

func TestParse_DynamicRunsOnAndMatrix(t *testing.T) {
	wf, err := Parse(load(t, "dynamic.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rj := wf.Jobs["dyn_runson"]
	if !rj.RunsOn.Dynamic || !rj.Dynamic {
		t.Errorf("dyn_runson should be dynamic: %+v", rj)
	}
	mj := wf.Jobs["dyn_matrix"]
	if !mj.Matrix.Dynamic {
		t.Errorf("dyn_matrix matrix should be dynamic: %+v", mj.Matrix)
	}
	if !mj.RunsOn.Dynamic {
		t.Errorf("dyn_matrix runs-on should be marked dynamic when matrix is dynamic")
	}
	if !mj.Dynamic {
		t.Errorf("dyn_matrix Job.Dynamic should be true")
	}
}

func TestParse_ReusableLocal(t *testing.T) {
	wf, err := Parse(load(t, "reusable_local.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	j := wf.Jobs["call"]
	if j.UsesLocal != "./.github/workflows/inner.yml" {
		t.Errorf("UsesLocal: got %q", j.UsesLocal)
	}
	if j.UsesRemote {
		t.Errorf("should not be remote")
	}
	if len(j.RunsOn.Labels) != 0 || j.RunsOn.Group != "" {
		t.Errorf("reusable call should have no own runs-on: %+v", j.RunsOn)
	}
}

func TestParse_ReusableRemote(t *testing.T) {
	wf, err := Parse(load(t, "reusable_remote.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	j := wf.Jobs["call"]
	if !j.UsesRemote {
		t.Errorf("should be remote: %+v", j)
	}
	if j.UsesLocal != "" {
		t.Errorf("should not have UsesLocal: %q", j.UsesLocal)
	}
}

func TestParse_NeedsNormalization(t *testing.T) {
	wf, err := Parse(load(t, "deep_chain.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := wf.Jobs["b"].Needs; !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("b.needs: got %v", got)
	}
	if got := wf.Jobs["c"].Needs; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("c.needs: got %v", got)
	}
	if got := wf.Jobs["d"].Needs; !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("d.needs: got %v", got)
	}
	if got := wf.Jobs["a"].Needs; len(got) != 0 {
		t.Errorf("a.needs: want empty, got %v", got)
	}
}

func TestParse_NorwayOn(t *testing.T) {
	wf, err := Parse(load(t, "norway_on.yml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := wf.Jobs["build"]; !ok {
		t.Fatalf("norway_on: jobs.build missing, jobs=%v", wf.Jobs)
	}
}

func TestParse_NoJobs(t *testing.T) {
	wf, err := Parse([]byte("name: empty\non: push\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(wf.Jobs) != 0 {
		t.Errorf("expected empty jobs, got %v", wf.Jobs)
	}
}

func TestParse_MultiDocRejected(t *testing.T) {
	raw := []byte("name: a\non: push\njobs:\n  x:\n    runs-on: ubuntu-latest\n---\nname: b\n")
	_, err := Parse(raw)
	if err == nil {
		t.Fatalf("expected error for multi-document YAML")
	}
	if !strings.Contains(err.Error(), "multi-document") {
		t.Errorf("error should mention multi-document: %v", err)
	}
}
