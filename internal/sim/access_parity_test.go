package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// parityFixtureNodes is the fixture population: above the engine's range-seek
// label-population floor (1024) so the btree probes genuinely seek, and small
// enough to keep the tests in the short layer.
const parityFixtureNodes = 1500

// newParityEngine builds an in-memory engine shaped like the index-diversity
// scenario's graph — Person nodes with a unique string name (hash index), a
// cyclic integer age (btree index), and a cyclic string city (btree index) —
// with the given engine options.
func newParityEngine(t *testing.T, opts *cypher.EngineOptions) *EngineAdapter {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := cypher.NewEngineWithOptions(g, *opts)
	ctx := context.Background()
	for i := 0; i < parityFixtureNodes; i++ {
		q := fmt.Sprintf("CREATE (:Person {name:'p%d', age:%d, city:'c%d'})", i, i%500, i%100)
		res, err := eng.RunInTx(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("seed %d: close: %v", i, cerr)
		}
	}
	for _, ddl := range indexDiversityDDL {
		res, err := eng.Run(ctx, ddl, nil)
		if err != nil {
			t.Fatalf("DDL %q: %v", ddl, err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("DDL %q: close: %v", ddl, cerr)
		}
	}
	return NewEngineAdapter(eng)
}

// parityFixtureProbes is a fixed probe set matching the fixture's data
// distribution: every value exists and every range/prefix stays around 1%
// selectivity, so the seek gates admit the seek.
func parityFixtureProbes() []ParityProbe {
	return []ParityProbe{
		{
			Shape:    "equality",
			Literal:  "MATCH (n:Person) WHERE n.name = 'p250' RETURN id(n)",
			Param:    "MATCH (n:Person) WHERE n.name = $p RETURN id(n)",
			Params:   map[string]any{"p": "p250"},
			MustSeek: true,
		},
		{
			Shape:    "range",
			Literal:  "MATCH (n:Person) WHERE n.age >= 100 AND n.age < 105 RETURN id(n)",
			Param:    "MATCH (n:Person) WHERE n.age >= $lo AND n.age < $hi RETURN id(n)",
			Params:   map[string]any{"lo": int64(100), "hi": int64(105)},
			MustSeek: true,
		},
		{
			Shape:    "starts-with",
			Literal:  "MATCH (n:Person) WHERE n.city STARTS WITH 'c17' RETURN id(n)",
			Param:    "MATCH (n:Person) WHERE n.city STARTS WITH $p RETURN id(n)",
			Params:   map[string]any{"p": "c17"},
			MustSeek: true,
		},
		{
			Shape:   "in-list",
			Literal: "MATCH (n:Person) WHERE n.name IN ['p10','p20','p30'] RETURN id(n)",
			Param:   "MATCH (n:Person) WHERE n.name IN $p RETURN id(n)",
			Params:  map[string]any{"p": []any{"p10", "p20", "p30"}},
		},
	}
}

// TestCheckAccessPathParity_CleanOnHealthyEngine is the happy path: on a
// healthy engine every shape's literal and parameter arms agree on both the
// access path and the result multiset, the MustSeek arms really seek, and the
// Profile probe reports non-zero db-hits — zero violations.
func TestCheckAccessPathParity_CleanOnHealthyEngine(t *testing.T) {
	t.Parallel()
	a := newParityEngine(t, &cypher.EngineOptions{})
	if v := CheckAccessPathParity(1, nil, a, parityFixtureProbes()...); len(v) != 0 {
		t.Fatalf("expected a clean parity check, got violations:\n%v", v)
	}
}

// splitPlanEngine routes literal-arm calls (nil params) to lit and
// parameter-arm calls (non-nil params) to par. It reproduces the rmp #2414
// defect class for the sensitivity proof: the two engines hold identical data,
// so every answer is correct, but the param engine has the prefix seek
// disabled, so only the plan collapses — exactly the divergence the checker
// exists to see and a result-only oracle cannot.
type splitPlanEngine struct {
	lit *EngineAdapter
	par *EngineAdapter
}

func (s *splitPlanEngine) route(params map[string]any) *EngineAdapter {
	if params == nil {
		return s.lit
	}
	return s.par
}

func (s *splitPlanEngine) Run(ctx context.Context, query string, params map[string]any) (Result, error) {
	return s.route(params).Run(ctx, query, params)
}

func (s *splitPlanEngine) Explain(query string, params map[string]any) (string, error) {
	return s.route(params).Explain(query, params)
}

func (s *splitPlanEngine) Profile(ctx context.Context, query string, params map[string]any) (string, error) {
	return s.route(params).Profile(ctx, query, params)
}

func (s *splitPlanEngine) NodeCount() (int64, error) { return s.lit.NodeCount() }
func (s *splitPlanEngine) EdgeCount() (int64, error) { return s.lit.EdgeCount() }

// TestCheckAccessPathParity_FiresOnPlanDivergence is the mandatory sensitivity
// proof: when the parameter arm genuinely scans while the identical literal
// seeks (the rmp #2414 shape, forced here via DisablePrefixIndexSeek on the
// param-arm engine), the checker MUST fire — with correct answers on both
// arms, so nothing but the plan comparison can catch it.
func TestCheckAccessPathParity_FiresOnPlanDivergence(t *testing.T) {
	t.Parallel()
	split := &splitPlanEngine{
		lit: newParityEngine(t, &cypher.EngineOptions{}),
		par: newParityEngine(t, &cypher.EngineOptions{DisablePrefixIndexSeek: true}),
	}
	// The two engines assign internal node ids independently, so this probe
	// projects a content-derived integer (n.age) instead of id(n): the multiset
	// of ages is a pure function of the data, identical on both engines, which
	// isolates the PLAN divergence as the only observable difference.
	probe := ParityProbe{
		Shape:    "starts-with",
		Literal:  "MATCH (n:Person) WHERE n.city STARTS WITH 'c17' RETURN n.age",
		Param:    "MATCH (n:Person) WHERE n.city STARTS WITH $p RETURN n.age",
		Params:   map[string]any{"p": "c17"},
		MustSeek: true,
	}

	v := CheckAccessPathParity(7, nil, split, probe)
	if len(v) == 0 {
		t.Fatalf("checker did not fire on a literal-seeks/param-scans divergence")
	}
	got := v[0]
	if got.Kind != ViolationOracleDeviation {
		t.Fatalf("expected %s, got %s: %s", ViolationOracleDeviation, got.Kind, got.Message)
	}
	for _, want := range []string{`"starts-with"`, "NodeByIndexRangeScan", "NodeByLabelScan", "literal plan:", "param plan:", "literal result:", "param result:"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("violation message misses %q:\n%s", want, got.Message)
		}
	}
}

// TestCheckAccessPathParity_FiresOnResultDivergence proves the answer half
// fires: when the two arms return different multisets (forced by a param-arm
// engine holding one extra matching node), the checker reports an
// ACID_CONSISTENCY violation naming both result summaries.
func TestCheckAccessPathParity_FiresOnResultDivergence(t *testing.T) {
	t.Parallel()
	split := &splitPlanEngine{
		lit: newParityEngine(t, &cypher.EngineOptions{}),
		par: newParityEngine(t, &cypher.EngineOptions{}),
	}
	// One extra node visible only to the param arm.
	res, err := split.par.RunWrite(context.Background(), "CREATE (:Person {name:'p250', age:1, city:'c1'})", map[string]any{})
	if err != nil {
		t.Fatalf("extra node: %v", err)
	}
	_ = res.Close()

	v := CheckAccessPathParity(9, nil, split, parityFixtureProbes()[0]) // equality on name p250
	if len(v) == 0 {
		t.Fatalf("checker did not fire on a result-multiset divergence")
	}
	got := v[0]
	if got.Kind != ViolationACIDConsistency {
		t.Fatalf("expected %s, got %s: %s", ViolationACIDConsistency, got.Kind, got.Message)
	}
	if !strings.Contains(got.Message, "different result multisets") {
		t.Errorf("violation message misses the multiset diagnosis:\n%s", got.Message)
	}
}

// TestCheckAccessPathParity_FiresOnVacuousPair proves the vacuity guard fires:
// with the range seek disabled on the ONE engine both arms agree on a scan —
// parity holds, but a MustSeek pair that compares two scans proves nothing and
// must be reported instead of passing silently.
func TestCheckAccessPathParity_FiresOnVacuousPair(t *testing.T) {
	t.Parallel()
	a := newParityEngine(t, &cypher.EngineOptions{DisableRangeIndexSeek: true})
	probe := parityFixtureProbes()[1] // numeric range, MustSeek

	v := CheckAccessPathParity(3, nil, a, probe)
	if len(v) == 0 {
		t.Fatalf("checker did not fire on a MustSeek pair that agrees on two scans")
	}
	if !strings.Contains(v[0].Message, "proves nothing") {
		t.Errorf("violation message misses the vacuity diagnosis:\n%s", v[0].Message)
	}
}

// TestCheckPlanStability_CleanAndFires covers both halves of the plan-stability
// oracle: a baseline re-checked against the same engine is byte-identical
// (clean), and re-checked against an engine whose plans genuinely differ (the
// prefix seek disabled) it fires, naming the shape and both renderings.
func TestCheckPlanStability_CleanAndFires(t *testing.T) {
	t.Parallel()
	healthy := newParityEngine(t, &cypher.EngineOptions{})
	probes := parityFixtureProbes()

	base, err := CapturePlanBaseline(healthy, probes...)
	if err != nil {
		t.Fatalf("CapturePlanBaseline: %v", err)
	}
	if v := CheckPlanStability(2, base, healthy); len(v) != 0 {
		t.Fatalf("stability check against the same engine must be clean, got:\n%v", v)
	}

	drifted := newParityEngine(t, &cypher.EngineOptions{DisablePrefixIndexSeek: true})
	v := CheckPlanStability(4, base, drifted)
	if len(v) == 0 {
		t.Fatalf("stability check did not fire on a genuinely drifted plan")
	}
	for _, want := range []string{`"starts-with"`, "baseline plan:", "current plan:"} {
		if !strings.Contains(v[0].Message, want) {
			t.Errorf("violation message misses %q:\n%s", want, v[0].Message)
		}
	}
}

// fakePlanEngine returns canned Explain/Profile renderings so the db-hits arm
// can be driven to zero, which a real engine with data cannot produce.
type fakePlanEngine struct {
	*EngineAdapter
	profile string
}

func (f *fakePlanEngine) Profile(_ context.Context, _ string, _ map[string]any) (string, error) {
	return f.profile, nil
}

// TestCheckAccessPathParity_FiresOnZeroDbHits proves the Profile probe fires
// when a data-touching query reports a zero db-hit total.
func TestCheckAccessPathParity_FiresOnZeroDbHits(t *testing.T) {
	t.Parallel()
	f := &fakePlanEngine{
		EngineAdapter: newParityEngine(t, &cypher.EngineOptions{}),
		profile: "Project (rows=1, dbhits=0, time=23µs)\n" +
			"└─ NodeByIndexSeek [seek=\"p250\"] (rows=1, dbhits=0, time=0s)",
	}
	v := CheckAccessPathParity(5, nil, f, parityFixtureProbes()[0])
	if len(v) == 0 {
		t.Fatalf("checker did not fire on a zero db-hit profile")
	}
	if !strings.Contains(v[0].Message, "ZERO db-hits") {
		t.Errorf("violation message misses the db-hits diagnosis:\n%s", v[0].Message)
	}
}

// TestTotalDbHits pins the profile-annotation parser on the exact rendering
// shape RenderPlanNode emits.
func TestTotalDbHits(t *testing.T) {
	t.Parallel()
	prof := "Project (rows=1, dbhits=0, time=23µs)\n└─ NodeByIndexSeek [seek=\"p250\"] (rows=1, dbhits=17, time=0s)"
	if got := totalDbHits(prof); got != 17 {
		t.Fatalf("totalDbHits = %d, want 17", got)
	}
	if got := totalDbHits("no annotations at all"); got != 0 {
		t.Fatalf("totalDbHits on plain text = %d, want 0", got)
	}
}

// TestIndexDiversityParityProbes_Deterministic pins the probe builder: the
// same seed yields the same probe set, and a different seed draws its values
// from the checker's own stream without touching the workload conventions
// (bulk "p" names, in-range ages, two-digit city prefixes).
func TestIndexDiversityParityProbes_Deterministic(t *testing.T) {
	t.Parallel()
	a := indexDiversityParityProbes(NewSeed(42 ^ paritySeedMix))
	b := indexDiversityParityProbes(NewSeed(42 ^ paritySeedMix))
	if len(a) != 4 || len(b) != 4 {
		t.Fatalf("expected 4 probes, got %d and %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Literal != b[i].Literal || a[i].Param != b[i].Param {
			t.Fatalf("probe %d not deterministic:\n%q\nvs\n%q", i, a[i].Literal, b[i].Literal)
		}
	}
	shapes := []string{"equality", "range", "starts-with", "in-list"}
	for i, p := range a {
		if p.Shape != shapes[i] {
			t.Fatalf("probe %d shape = %q, want %q", i, p.Shape, shapes[i])
		}
	}
	if !a[0].MustSeek || !a[1].MustSeek || !a[2].MustSeek || a[3].MustSeek {
		t.Fatalf("MustSeek flags wrong: %+v", a)
	}
}
