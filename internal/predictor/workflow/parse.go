// Package workflow parses a single GitHub Actions workflow YAML into a typed
// view suitable for static analysis (predictor walking).
//
// The heavy YAML lifting is delegated to github.com/rhysd/actionlint, which
// handles the syntactic quirks (the Norway "on:" problem, anchors/merge keys,
// runs-on object/array/scalar forms, matrix expressions). This package maps
// actionlint's AST into a smaller, opinionated shape tailored to the
// predictor's needs: literal label sets, literal matrix combos, and explicit
// Dynamic flags for any expression-driven form we cannot resolve statically.
package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rhysd/actionlint"
)

// Workflow is one parsed workflow file.
type Workflow struct {
	// Jobs is keyed by job ID (the YAML map key under "jobs:").
	Jobs map[string]Job
}

// Job is the predictor-relevant slice of one workflow job.
type Job struct {
	ID         string
	Needs      []string
	RunsOn     RunsOnSpec
	Matrix     MatrixSpec
	UsesLocal  string // non-empty when uses: ./.github/workflows/x.yml
	UsesRemote bool   // true when uses: owner/repo/...@ref
	Dynamic    bool   // true when RunsOn or Matrix is dynamic
}

// RunsOnSpec describes the resolved runner selection for one job.
type RunsOnSpec struct {
	Labels  []string
	Group   string
	Dynamic bool
}

// MatrixSpec describes the expanded literal matrix for one job.
//
// Combos is empty when the job has no matrix, or when the matrix is dynamic
// (in which case Dynamic == true and downstream RunsOn.Dynamic is also set).
type MatrixSpec struct {
	Combos  []map[string]string
	Dynamic bool
}

// Parse parses one workflow YAML document and returns the typed view.
//
// Behavior contract:
//   - Multi-document YAML (more than one "---" body) is rejected.
//   - A workflow with no "jobs:" key returns Workflow{} with a nil Jobs map
//     and a nil error.
//   - actionlint lint-level errors that still allow producing a *Workflow are
//     ignored (we only need the AST, not the lint).
//   - Expression-driven runs-on / matrix is flagged via Dynamic; we never try
//     to evaluate ${{ }} or fromJSON.
func Parse(raw []byte) (Workflow, error) {
	if err := rejectMultiDoc(raw); err != nil {
		return Workflow{}, err
	}

	wf, errs := actionlint.Parse(raw)
	if wf == nil {
		// actionlint could not produce an AST; surface the first error.
		if len(errs) > 0 {
			return Workflow{}, fmt.Errorf("actionlint parse: %s", errs[0].Message)
		}
		return Workflow{}, errors.New("actionlint parse: no workflow produced")
	}

	out := Workflow{}
	if len(wf.Jobs) == 0 {
		return out, nil
	}
	out.Jobs = make(map[string]Job, len(wf.Jobs))
	for id, j := range wf.Jobs {
		out.Jobs[id] = mapJob(id, j)
	}
	return out, nil
}

// rejectMultiDoc returns an error if raw contains more than one YAML document.
// A leading or trailing "---" by itself is fine; we only refuse when there is
// real content in two separate documents. We look for a "---" line that has
// non-empty content both before and after it.
func rejectMultiDoc(raw []byte) error {
	// Split on lines that are exactly "---" (allowing trailing whitespace).
	// A simple line-scan is enough — we are not implementing a YAML parser
	// here, just detecting the second document marker.
	lines := bytes.Split(raw, []byte("\n"))
	docs := 0
	currentHasContent := false
	flush := func() {
		if currentHasContent {
			docs++
		}
		currentHasContent = false
	}
	for _, l := range lines {
		t := bytes.TrimRight(l, " \t\r")
		if bytes.Equal(t, []byte("---")) {
			flush()
			continue
		}
		// Treat blank or comment-only lines as no content.
		trim := bytes.TrimSpace(t)
		if len(trim) == 0 || trim[0] == '#' {
			continue
		}
		currentHasContent = true
	}
	flush()
	if docs > 1 {
		return errors.New("multi-document YAML not supported")
	}
	return nil
}

func mapJob(id string, j *actionlint.Job) Job {
	out := Job{ID: id}

	// needs:
	for _, n := range j.Needs {
		if n != nil {
			out.Needs = append(out.Needs, n.Value)
		}
	}

	// uses: — reusable workflow call. In this case the job has no own
	// runs-on; we still emit an empty RunsOnSpec.
	if j.WorkflowCall != nil && j.WorkflowCall.Uses != nil {
		u := j.WorkflowCall.Uses.Value
		switch classifyUses(u) {
		case usesLocal:
			out.UsesLocal = u
		case usesRemote:
			out.UsesRemote = true
		}
		// A reusable call does not carry its own runs-on; return early.
		return out
	}

	// runs-on:
	out.RunsOn = mapRunsOn(j.RunsOn)

	// matrix:
	if j.Strategy != nil && j.Strategy.Matrix != nil {
		out.Matrix = mapMatrix(j.Strategy.Matrix)
	}

	// If runs-on references matrix.X and the matrix has literal rows for X,
	// expand the runs-on per combo into concrete label sets. The expanded
	// labels are stored back onto RunsOn.Labels for the *first* combo so
	// callers without matrix awareness see a literal; the full combo list
	// is available in Matrix.Combos for callers that count expansions.
	//
	// Note: we do not flatten per-combo runs-on into separate "Job" entries;
	// the predictor multiplies count by len(Combos) at walk time. We only
	// need to (a) resolve the runs-on labels to literals when possible, and
	// (b) flag Dynamic appropriately.
	if !out.Matrix.Dynamic && len(out.Matrix.Combos) > 0 && j.RunsOn != nil && j.RunsOn.LabelsExpr != nil {
		if resolved, ok := resolveMatrixRunsOn(j.RunsOn.LabelsExpr.Value, out.Matrix.Combos); ok {
			out.RunsOn.Labels = resolved
			out.RunsOn.Dynamic = false
		}
	}

	// Job-level Dynamic is the OR of its components.
	if out.RunsOn.Dynamic || out.Matrix.Dynamic {
		out.Dynamic = true
	}
	// A dynamic matrix forces RunsOn.Dynamic too (we can't know what the
	// matrix will resolve to, so the resulting labels are unknown).
	if out.Matrix.Dynamic {
		out.RunsOn.Dynamic = true
		out.Dynamic = true
	}

	return out
}

type usesKind int

const (
	usesNone usesKind = iota
	usesLocal
	usesRemote
)

func classifyUses(s string) usesKind {
	if s == "" {
		return usesNone
	}
	if strings.HasPrefix(s, "./") {
		return usesLocal
	}
	// owner/repo/.../x.yml@ref  → remote
	if strings.Contains(s, "@") && strings.Contains(s, "/") {
		return usesRemote
	}
	return usesNone
}

func mapRunsOn(r *actionlint.Runner) RunsOnSpec {
	out := RunsOnSpec{}
	if r == nil {
		return out
	}
	if r.Group != nil {
		out.Group = r.Group.Value
	}
	// Plain literal labels.
	for _, l := range r.Labels {
		if l == nil {
			continue
		}
		if l.ContainsExpression() {
			// e.g. runs-on: [self-hosted, "${{ matrix.os }}"] — treat as
			// dynamic. The matrix-expansion path above resolves the simple
			// single-expression case (LabelsExpr); a mixed literal+expr
			// array is rare and we mark it Dynamic for safety.
			out.Dynamic = true
			continue
		}
		out.Labels = append(out.Labels, l.Value)
	}
	if r.LabelsExpr != nil {
		// runs-on: ${{ ... }} — dynamic unless caller can resolve from
		// matrix. mapJob handles the resolvable case afterwards.
		out.Dynamic = true
	}
	return out
}

func mapMatrix(m *actionlint.Matrix) MatrixSpec {
	out := MatrixSpec{}

	// Matrix-level expression (e.g. strategy.matrix: ${{ fromJSON(...) }})
	if m.Expression != nil {
		out.Dynamic = true
		return out
	}

	// Any row whose values are an expression rather than a literal sequence
	// makes the whole matrix non-decidable.
	rows := make([]matrixAxis, 0, len(m.Rows))
	for name, r := range m.Rows {
		if r == nil {
			continue
		}
		if r.Expression != nil {
			out.Dynamic = true
			return out
		}
		vals := make([]rawValue, 0, len(r.Values))
		for _, v := range r.Values {
			vals = append(vals, asRaw(v))
		}
		rows = append(rows, matrixAxis{name: name, values: vals})
	}
	// Stable key order so the cross-product is deterministic.
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	// Cross-product the rows.
	combos := []map[string]string{{}}
	for _, r := range rows {
		next := make([]map[string]string, 0, len(combos)*len(r.values))
		for _, base := range combos {
			for _, v := range r.values {
				cp := make(map[string]string, len(base)+1)
				for k, val := range base {
					cp[k] = val
				}
				cp[r.name] = v.str
				next = append(next, cp)
			}
		}
		combos = next
	}

	// Apply exclude.
	if m.Exclude != nil {
		if m.Exclude.ContainsExpression() {
			out.Dynamic = true
			return out
		}
		combos = applyExclude(combos, m.Exclude)
	}

	// Apply include.
	if m.Include != nil {
		if m.Include.ContainsExpression() {
			out.Dynamic = true
			return out
		}
		combos = applyInclude(combos, rows, m.Include)
	}

	out.Combos = combos
	return out
}

// rawValue is a minimal stringified projection of an actionlint.RawYAMLValue
// suitable for matrix-key substitution. Non-scalar values (object/array) are
// rendered with the AST's String() method as a stable representation; the
// predictor only uses these as map keys, so opaque equality is fine.
type rawValue struct {
	str string
}

func asRaw(v actionlint.RawYAMLValue) rawValue {
	if v == nil {
		return rawValue{}
	}
	switch x := v.(type) {
	case *actionlint.RawYAMLString:
		return rawValue{str: x.Value}
	default:
		return rawValue{str: v.String()}
	}
}

func applyExclude(combos []map[string]string, ex *actionlint.MatrixCombinations) []map[string]string {
	out := make([]map[string]string, 0, len(combos))
	for _, c := range combos {
		if matchesAnyCombination(c, ex.Combinations) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func matchesAnyCombination(c map[string]string, combs []*actionlint.MatrixCombination) bool {
	for _, comb := range combs {
		if comb == nil || comb.Expression != nil {
			continue
		}
		if matchesCombination(c, comb) {
			return true
		}
	}
	return false
}

func matchesCombination(c map[string]string, comb *actionlint.MatrixCombination) bool {
	for k, a := range comb.Assigns {
		if a == nil {
			continue
		}
		want := asRaw(a.Value).str
		got, ok := c[k]
		if !ok || got != want {
			return false
		}
	}
	return true
}

// matrixAxis is one axis of a literal matrix (one row in actionlint terms).
type matrixAxis struct {
	name   string
	values []rawValue
}

func applyInclude(combos []map[string]string, rows []matrixAxis, inc *actionlint.MatrixCombinations) []map[string]string {
	// Per GitHub semantics: include entries that match an existing combo (on
	// the row keys they specify) augment it; entries that don't match any
	// existing combo are appended as a new combo. Augmentation only adds
	// keys that aren't part of the matrix's primary axes (i.e. extra
	// dimensions), but for our string-map representation we copy all assigns.
	rowKeys := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		rowKeys[r.name] = struct{}{}
	}
	for _, comb := range inc.Combinations {
		if comb == nil || comb.Expression != nil {
			continue
		}
		merged := false
		for i, c := range combos {
			if combinationMatchesOnAxes(c, comb, rowKeys) {
				cp := copyMap(c)
				for k, a := range comb.Assigns {
					if a == nil {
						continue
					}
					cp[k] = asRaw(a.Value).str
				}
				combos[i] = cp
				merged = true
			}
		}
		if !merged {
			newCombo := map[string]string{}
			for k, a := range comb.Assigns {
				if a == nil {
					continue
				}
				newCombo[k] = asRaw(a.Value).str
			}
			combos = append(combos, newCombo)
		}
	}
	return combos
}

// combinationMatchesOnAxes returns true when, restricted to the original row
// keys, every assignment in comb matches c. Keys in comb that are not in
// rowKeys are treated as additional dimensions and ignored for matching.
func combinationMatchesOnAxes(c map[string]string, comb *actionlint.MatrixCombination, rowKeys map[string]struct{}) bool {
	for k, a := range comb.Assigns {
		if _, isAxis := rowKeys[k]; !isAxis {
			continue
		}
		if a == nil {
			continue
		}
		want := asRaw(a.Value).str
		got, ok := c[k]
		if !ok || got != want {
			return false
		}
	}
	return true
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// resolveMatrixRunsOn handles the common case `runs-on: ${{ matrix.os }}`
// (and similarly `${{ matrix.X }}` with no surrounding text). Returns the
// per-combo resolved value for the first combo, plus ok=true. For combos
// where the key is missing or the expression is not a single matrix.X
// reference, returns ok=false (mapJob then leaves RunsOn.Dynamic=true).
func resolveMatrixRunsOn(expr string, combos []map[string]string) ([]string, bool) {
	key, ok := singleMatrixRef(expr)
	if !ok {
		return nil, false
	}
	if len(combos) == 0 {
		return nil, false
	}
	v, ok := combos[0][key]
	if !ok {
		return nil, false
	}
	return []string{v}, true
}

// singleMatrixRef returns ("os", true) for inputs like "${{ matrix.os }}".
// Anything more complex (multiple expressions, surrounding text, function
// calls) returns ok=false.
func singleMatrixRef(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "${{") || !strings.HasSuffix(t, "}}") {
		return "", false
	}
	inner := strings.TrimSpace(t[3 : len(t)-2])
	if !strings.HasPrefix(inner, "matrix.") {
		return "", false
	}
	key := strings.TrimPrefix(inner, "matrix.")
	if key == "" || strings.ContainsAny(key, " \t().${}") {
		return "", false
	}
	return key, true
}
