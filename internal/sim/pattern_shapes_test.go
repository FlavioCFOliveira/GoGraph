package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildMotifFixture creates a hand-analysable motif graph in BOTH the engine
// and the oracle: Persons a, b, c, d; KNOWS edges a→b, b→c, c→a (triangle),
// b→a (mutual pair a⇄b), c→c (self-loop); FOLLOWS edges a→b (both-types pair
// over the KNOWS a→b) and d→a. The engine is a directed multigraph, exactly as
// the pattern-shapes scenario opens it — a simple graph would reject the
// FOLLOWS a→b over the existing KNOWS a→b.
func buildMotifFixture(t *testing.T) (*EngineAdapter, *GraphOracle) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	a := NewEngineAdapter(cypher.NewEngine(g))
	o := NewGraphOracle()
	ctx := context.Background()
	apply := func(cypher string, params map[string]any) {
		t.Helper()
		if _, err := a.RunWrite(ctx, cypher, params); err != nil {
			t.Fatalf("fixture write %q: %v", cypher, err)
		}
		o.ApplyCreate(cypher, params)
	}
	for i, name := range []string{"a", "b", "c", "d"} {
		apply(tmplCreatePerson, map[string]any{"name": name, "age": int64(10 * i)})
	}
	for _, e := range [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}, {"b", "a"}, {"c", "c"}} {
		apply(tmplCreateKnows, map[string]any{"a": e[0], "b": e[1]})
	}
	for _, e := range [][2]string{{"a", "b"}, {"d", "a"}} {
		apply(tmplCreateFollows, map[string]any{"a": e[0], "b": e[1]})
	}
	return a, o
}

// TestPatternShapes_MotifFixture_HandVerified pins the adjacency-composition
// references to counts derived BY HAND on the fixture, then asserts the
// engine agrees (the battery reports no violation). Hand derivation over
// K = {ab, bc, ca, ba, cc}, F = {ab, da}:
//
//   - knows = 5, follows = 2, selfLoops = 1 (cc).
//   - twoHop (ordered pairs e1→e2, e1≠e2):
//     ab→{bc,ba}=2, bc→{ca,cc}=2, ca→{ab}=1, ba→{ab}=1, cc→{ca}=1
//     (cc→cc is e1=e2, excluded) — total 7.
//   - triangles (ordered closed triples, pairwise-distinct edges):
//     (ab,bc,ca), (bc,ca,ab), (ca,ab,bc) — the 3 rotations; no degenerate
//     walk closes (e.g. ab,ba needs aa; cc,ca needs ac) — total 3.
//   - undirected = 2*5 - 1 = 9 (the self-loop matches once).
//   - reverse = knows = 5.
//   - multiType = 5 + 2 = 7.
//   - uniqueness (reverse exists, r1≠r2): ab (ba present) + ba (ab present)
//     = 2; cc's reverse IS itself, excluded by relationship isomorphism.
//   - bothTypePairs = 1 (a,b carries KNOWS and FOLLOWS).
func TestPatternShapes_MotifFixture_HandVerified(t *testing.T) {
	a, o := buildMotifFixture(t)

	refs := computePatternShapeRefs(o)
	want := patternShapeRefs{
		knows: 5, follows: 2, twoHop: 7, triangles: 3,
		undirected: 9, uniqueness: 2, selfLoops: 1, bothTypePairs: 1,
	}
	if refs != want {
		t.Fatalf("reference computation disagrees with the hand derivation:\ngot  %+v\nwant %+v", refs, want)
	}

	if v := CheckPatternShapes(0, o, a); len(v) > 0 {
		t.Fatalf("engine disagrees with the hand-verified references: %v", v)
	}

	if v := patternShapesVacuity(0, o); len(v) > 0 {
		t.Fatalf("fixture contains every motif, vacuity must not fire: %v", v)
	}
}

// TestPatternShapes_SensitivityToWrongReference proves the battery FIRES when
// the reference is wrong: the oracle adjacency is perturbed directly (the
// in-package test seam) and the checks must flag the disagreement with the
// untouched engine.
func TestPatternShapes_SensitivityToWrongReference(t *testing.T) {
	t.Run("missing KNOWS edge", func(t *testing.T) {
		a, o := buildMotifFixture(t)
		// Drop c→a from the model: knows, twoHop, triangles, undirected,
		// reverse and multi-type references all shift while the engine keeps
		// the edge.
		delete(o.edges, edgeKey{src: o.byName["c"], dst: o.byName["a"], label: "KNOWS"})
		if v := CheckPatternShapes(0, o, a); len(v) == 0 {
			t.Fatal("battery FAILED to fire on a perturbed (missing-edge) reference")
		}
	})
	t.Run("phantom FOLLOWS edge", func(t *testing.T) {
		a, o := buildMotifFixture(t)
		// Add a FOLLOWS b→c the engine does not have: the multi-type union
		// reference inflates.
		bid, cid := o.byName["b"], o.byName["c"]
		o.edges[edgeKey{src: bid, dst: cid, label: "FOLLOWS"}] =
			&EdgeState{SrcID: bid, DstID: cid, Label: "FOLLOWS", Properties: map[string]any{}}
		if v := CheckPatternShapes(0, o, a); len(v) == 0 {
			t.Fatal("battery FAILED to fire on a perturbed (phantom-edge) reference")
		}
	})
	t.Run("vacuity fires on a motif-free model", func(t *testing.T) {
		o := NewGraphOracle()
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "x", "age": int64(1)})
		if v := patternShapesVacuity(0, o); len(v) == 0 {
			t.Fatal("non-vacuity assertion FAILED to fire on a model with no motifs")
		}
	})
}

// TestPatternShapes_Scenario_Passes runs the registered pattern-shapes
// scenario end to end (periodic, post-crash-recovery, and terminal battery
// plus the terminal non-vacuity assertion) at its default seed.
func TestPatternShapes_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioPatternShapes)
	if !ok {
		t.Fatalf("pattern-shapes scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("pattern-shapes run: %v", err)
	}
	if report != nil {
		t.Fatalf("pattern-shapes reported a violation (a pattern shape disagreed with its adjacency reference):\n%s", report)
	}
}
