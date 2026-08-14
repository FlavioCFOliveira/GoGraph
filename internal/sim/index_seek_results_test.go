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

// newSeekResultsEngine builds an in-memory engine shaped like the
// index-diversity scenario's graph (names "p<i>", ages i%500, cities "c<i%100>",
// plus the scenario's three indexes) with nodes Person nodes. The sensitivity
// tests use a small population — the comparison logic under test does not need
// the seek gates engaged; the happy path uses the full parity fixture instead,
// so the verified answers really are index-served.
func newSeekResultsEngine(t *testing.T, nodes int) *EngineAdapter {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for i := 0; i < nodes; i++ {
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

// fixedSeekResults returns a checker with hand-picked windows that all hit the
// fixture's data distribution, so sensitivity tests can plant divergences at
// known values instead of chasing seed draws.
func fixedSeekResults() *IndexSeekResults {
	return &IndexSeekResults{
		lo: 100, hi: 105,
		fromLo:  460,
		prefix:  "c17",
		inAges:  [3]int64{5, 8, 14},
		inNames: [3]string{"p10", "p17", "p23"},
	}
}

// TestIndexSeekResults_CleanOnHealthyEngine is the happy path on a fixed
// fixture: on a healthy engine sized so the range and prefix seek gates engage
// (the parity fixture population), every arm — bounded range, half-open range,
// prefix, IN-list and its UNWIND twin, in literal and parameterised spellings,
// plus the three count arms — agrees with the independent full-scan reference,
// and the terminal Finish is satisfied (rows were seen).
func TestIndexSeekResults_CleanOnHealthyEngine(t *testing.T) {
	t.Parallel()
	a := newParityEngine(t, &cypher.EngineOptions{})
	k := NewIndexSeekResults(NewSeed(0x2450^seekResultsSeedMix), parityFixtureNodes)
	if v := k.Check(1, a); len(v) != 0 {
		t.Fatalf("expected a clean seek-result check, got violations:\n%v", v)
	}
	if v := k.Finish(2); len(v) != 0 {
		t.Fatalf("non-vacuity must be satisfied on the populated fixture, got:\n%v", v)
	}
}

// TestIndexSeekResults_FiresOnParamDivergence is a mandatory sensitivity
// proof: the literal and parameterised spellings genuinely returning different
// rows (forced by routing the param arms to an engine holding two extra
// matching nodes) must fire ACID_CONSISTENCY violations naming the diverging
// shapes — here the bounded range (extra age 101) and the numeric IN-list
// (extra age 8), including its UNWIND twin and the range count arm.
func TestIndexSeekResults_FiresOnParamDivergence(t *testing.T) {
	t.Parallel()
	split := &splitPlanEngine{
		lit: newSeekResultsEngine(t, 300),
		par: newSeekResultsEngine(t, 300),
	}
	for _, q := range []string{
		"CREATE (:Person {name:'x-range', age:101, city:'x'})",
		"CREATE (:Person {name:'x-in', age:8, city:'x'})",
	} {
		res, err := split.par.RunWrite(context.Background(), q, map[string]any{})
		if err != nil {
			t.Fatalf("extra node %q: %v", q, err)
		}
		_ = res.Close()
	}

	v := fixedSeekResults().Check(7, split)
	if len(v) == 0 {
		t.Fatalf("checker did not fire on a literal/param result divergence")
	}
	fired := map[string]bool{}
	for _, got := range v {
		if got.Kind != ViolationACIDConsistency {
			t.Fatalf("expected %s, got %s: %s", ViolationACIDConsistency, got.Kind, got.Message)
		}
		for _, shape := range []string{`"range"`, `"in-age"`, `"in-age-unwind"`, `"range-count"`} {
			if strings.Contains(got.Message, shape) {
				fired[shape] = true
			}
		}
	}
	for _, shape := range []string{`"range"`, `"in-age"`, `"in-age-unwind"`, `"range-count"`} {
		if !fired[shape] {
			t.Errorf("no violation fired for shape %s; got:\n%v", shape, v)
		}
	}
}

// refSplitEngine serves the checker's reference scan from ref and every probe
// arm from rest. It is the test seam that forces the probe-vs-reference
// comparison to diverge honestly: both engines answer their own queries
// correctly, but they hold different data, so the probe arms genuinely
// disagree with the reference — the exact signature of an index serving rows
// its base data does not carry.
type refSplitEngine struct {
	ref  *EngineAdapter
	rest *EngineAdapter
}

func (s *refSplitEngine) Run(ctx context.Context, query string, params map[string]any) (Result, error) {
	if query == seekReferenceQuery {
		return s.ref.Run(ctx, query, params)
	}
	return s.rest.Run(ctx, query, params)
}

func (s *refSplitEngine) NodeCount() (int64, error) { return s.ref.NodeCount() }
func (s *refSplitEngine) EdgeCount() (int64, error) { return s.ref.EdgeCount() }

// TestIndexSeekResults_FiresOnReferenceDivergence proves the primary
// comparison — probe arm vs the independent full-scan reference — fires: with
// the probe arms served by an engine holding one extra in-window node, both
// spellings agree with each other (same engine) but disagree with the
// reference, so only the reference comparison can catch it, for the id arm and
// the count arm alike.
func TestIndexSeekResults_FiresOnReferenceDivergence(t *testing.T) {
	t.Parallel()
	split := &refSplitEngine{
		ref:  newSeekResultsEngine(t, 300),
		rest: newSeekResultsEngine(t, 300),
	}
	res, err := split.rest.RunWrite(context.Background(), "CREATE (:Person {name:'x-extra', age:102, city:'x'})", map[string]any{})
	if err != nil {
		t.Fatalf("extra node: %v", err)
	}
	_ = res.Close()

	v := fixedSeekResults().Check(9, split)
	if len(v) == 0 {
		t.Fatalf("checker did not fire on a probe-vs-reference divergence")
	}
	sawIDArm, sawCountArm := false, false
	for _, got := range v {
		if got.Kind != ViolationACIDConsistency {
			t.Fatalf("expected %s, got %s: %s", ViolationACIDConsistency, got.Kind, got.Message)
		}
		if strings.Contains(got.Message, "disagrees with the independent full-scan reference") {
			if strings.Contains(got.Message, `shape "range":`) && strings.Contains(got.Message, "scan reference:") {
				sawIDArm = true
			}
			if strings.Contains(got.Message, `shape "range-count":`) {
				sawCountArm = true
			}
		}
	}
	if !sawIDArm {
		t.Errorf("no id-multiset reference violation for the range arm; got:\n%v", v)
	}
	if !sawCountArm {
		t.Errorf("no count reference violation for the range-count arm; got:\n%v", v)
	}
}

// TestIndexSeekResults_FinishFiresOnVacuousRun proves the
// assert-something-was-seen guard: on an empty graph every arm agrees with the
// (empty) reference, so Check stays clean — and precisely because it compared
// nothing, Finish must report the run as vacuous.
func TestIndexSeekResults_FinishFiresOnVacuousRun(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	empty := NewEngineAdapter(cypher.NewEngine(g))
	k := fixedSeekResults()
	if v := k.Check(1, empty); len(v) != 0 {
		t.Fatalf("empty-graph check must be clean (all sets empty), got:\n%v", v)
	}
	v := k.Finish(2)
	if len(v) != 1 {
		t.Fatalf("Finish did not fire on a vacuous run, got %d violations: %v", len(v), v)
	}
	if v[0].Kind != ViolationOracleDeviation || !strings.Contains(v[0].Message, "vacuous run") {
		t.Fatalf("unexpected vacuity violation: %+v", v[0])
	}
}

// TestNewIndexSeekResults_Deterministic pins the probe-window builder: the
// same seed yields the same windows, and every draw lands inside the scenario
// data's guarantees — a sub-1%-selectivity bounded window, a half-open floor
// under the range-seek ceiling, a two-digit city prefix, distinct in-range
// ages, and distinct bulk names below the bulk bound.
func TestNewIndexSeekResults_Deterministic(t *testing.T) {
	t.Parallel()
	a := NewIndexSeekResults(NewSeed(42^seekResultsSeedMix), indexDiversityBulk)
	b := NewIndexSeekResults(NewSeed(42^seekResultsSeedMix), indexDiversityBulk)
	if *a != *b {
		t.Fatalf("probe windows not deterministic:\n%+v\nvs\n%+v", a, b)
	}
	if a.lo < 0 || a.lo >= 495 || a.hi != a.lo+5 {
		t.Errorf("bounded window out of range: lo=%d hi=%d", a.lo, a.hi)
	}
	if a.fromLo < 455 || a.fromLo > 494 {
		t.Errorf("half-open floor %d outside [455,494] (would trip the seek gate or miss data)", a.fromLo)
	}
	if !strings.HasPrefix(a.prefix, "c") || len(a.prefix) != 3 {
		t.Errorf("prefix %q is not a two-digit city prefix", a.prefix)
	}
	if a.inAges[0] == a.inAges[1] || a.inAges[1] == a.inAges[2] || a.inAges[2] >= 500 {
		t.Errorf("in-ages not distinct in-range values: %v", a.inAges)
	}
	if a.inNames[0] == a.inNames[1] || a.inNames[1] == a.inNames[2] {
		t.Errorf("in-names not distinct: %v", a.inNames)
	}
	for _, n := range a.inNames {
		var i int
		if _, err := fmt.Sscanf(n, "p%d", &i); err != nil || i < 0 || i >= indexDiversityBulk {
			t.Errorf("in-name %q outside the bulk name space", n)
		}
	}
}
