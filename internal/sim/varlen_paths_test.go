package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// vleFixtureStats returns a vleStats with every arm already recorded, so a test
// that only exercises the FIXTURE-side floors of [varlenPathsVacuity] is not
// tripped by the run-side flags.
func vleFixtureStats() *vleStats {
	return &vleStats{
		depth2Seen: 1, zeroLengthSeen: 1, multiTypeBeyond: 1,
		undirectedAbove: 1, unboundedDeep: 1, predSelective: 1,
	}
}

// TestVarlenPaths_MotifFixture_HandEnumerated pins the trail references to
// counts derived BY HAND on [buildMotifFixture], then asserts the engine agrees.
//
// The fixture's KNOWS set is K = {ab, bc, ca, ba, cc} (ages a=0, b=10, c=20,
// d=30) and its FOLLOWS set is F = {ab, da}. Out-adjacency:
// a→{b}, b→{c,a}, c→{a,c}, d→{}.
//
// Directed KNOWS trails (edge-distinct walks) by exact depth:
//
//	depth 1 = 5   — one per relationship.
//	depth 2 = 7   — ab→{bc,ba}=2, bc→{ca,cc}=2, ca→{ab}=1, ba→{ab}=1,
//	                cc→{ca}=1; cc→cc is excluded because it would repeat cc.
//	depth 3 = 8   — from a: (ab,bc,ca),(ab,bc,cc); from b: (bc,ca,ab),
//	                (bc,cc,ca),(ba,ab,bc); from c: (ca,ab,bc),(ca,ab,ba),
//	                (cc,ca,ab).
//	depth 4 = 8   — extend each depth-3 trail by one unused edge:
//	                (ab,bc,cc,ca),(bc,ca,ab,ba),(bc,cc,ca,ab),(ba,ab,bc,ca),
//	                (ba,ab,bc,cc),(ca,ab,bc,cc),(cc,ca,ab,bc),(cc,ca,ab,ba).
//	depth 5 = 2   — (bc,cc,ca,ab,ba) and (ba,ab,bc,cc,ca); nothing longer, since
//	                only five relationships exist.
//
// Undirected KNOWS trails. Each node's incidence list is a:{ab,ca,ba},
// b:{ab,bc,ba}, c:{bc,ca,cc}, d:{} — the self-loop is incident ONCE.
//
//	depth 1 = 9   — 2*|K| - selfLoops = 10 - 1.
//	depth 2 = 18  — per middle node, ordered pairs of distinct incident edges:
//	                3*2 at each of a, b and c.
//	depth 3 = 30  — 10 extensions from each of the three middle-node groups.
//
// Multi-type KNOWS|FOLLOWS trails. The pair (a,b) carries two DISTINCT
// relationships, ab(K) and ab(F):
//
//	depth 1 = 7   — |K| + |F|.
//	depth 2 = 13  — ab(K)→2, ab(F)→2, bc→2, ba→2, ca→2, da→2, cc→1 (cc excluded
//	                from its own continuation).
func TestVarlenPaths_MotifFixture_HandEnumerated(t *testing.T) {
	a, o := buildMotifFixture(t)
	m := buildVLEModel(o)

	if got, want := len(m.persons), 4; got != want {
		t.Fatalf("Person count = %d, want %d", got, want)
	}
	if !m.allAged {
		t.Fatal("every fixture Person carries an age; the predicate arms must not stand down")
	}

	deep := 5
	knows := countTrails(m.knowsOut, m.persons, deep, false)
	undir := countTrails(m.knowsInc, m.persons, deep, false)
	union := countTrails(m.unionOut, m.persons, deep, false)
	for _, w := range []vleWalks{knows, undir, union} {
		if !w.exact {
			t.Fatal("the fixture enumeration must never exhaust the walk budget")
		}
	}

	cases := []struct {
		name string
		got  []int64
		want []int64
	}{
		{"directed KNOWS", knows.perDepth, []int64{4, 5, 7, 8, 8, 2}},
		{"undirected KNOWS", undir.perDepth[:4], []int64{4, 9, 18, 30}},
		{"multi-type KNOWS|FOLLOWS", union.perDepth[:3], []int64{4, 7, 13}},
	}
	for _, c := range cases {
		if len(c.got) != len(c.want) {
			t.Fatalf("%s: %d depths, want %d", c.name, len(c.got), len(c.want))
		}
		for d := range c.want {
			if c.got[d] != c.want[d] {
				t.Errorf("%s depth %d = %d, want %d (hand enumeration)", c.name, d, c.got[d], c.want[d])
			}
		}
	}

	// The length-0 count is one binding per start node, at every view.
	for _, w := range []vleWalks{knows, undir, union} {
		if w.perDepth[0] != int64(len(m.persons)) {
			t.Errorf("length-0 bindings = %d, want one per Person (%d)", w.perDepth[0], len(m.persons))
		}
	}

	// Path predicates at depth 2 with threshold 20 (only c=20 and d=30 qualify).
	//
	// Intermediate-only — the middle node must qualify. The seven depth-2 trails
	// and their middles are (ab,bc):b, (ab,ba):b, (bc,ca):c, (bc,cc):c,
	// (ca,ab):a, (ba,ab):a, (cc,ca):c — so three qualify.
	//
	// Whole path — every node must qualify. Of those three, (bc,ca) has b and a,
	// (bc,cc) has b, and (cc,ca) has a: none qualifies, so the count is zero.
	// The two scopes therefore differ, which is what lets the probe tell them
	// apart.
	aged20 := func(id uint64) bool { return m.ages[id] >= 20 }
	if got, want := m.countDepth2ByPredicate(true, aged20), int64(3); got != want {
		t.Errorf("depth-2 trails with an intermediate of age >= 20 = %d, want %d", got, want)
	}
	if got, want := m.countDepth2ByPredicate(false, aged20), int64(0); got != want {
		t.Errorf("depth-2 trails with EVERY node of age >= 20 = %d, want %d", got, want)
	}
	// The two predicate scopes must genuinely differ, or the probe could not
	// tell an intermediates-only filter from a whole-path one.
	if m.countDepth2ByPredicate(true, aged20) == m.countDepth2ByPredicate(false, aged20) {
		t.Error("the intermediate-only and whole-path predicate references coincide on this fixture")
	}

	// The engine must agree with every one of those references.
	if v := CheckVarlenPaths(0, o, a, &vleStats{}); len(v) > 0 {
		t.Fatalf("engine disagrees with the hand-enumerated references: %v", v)
	}
}

// TestVarlenPaths_LowerBoundOnlyIsExhaustive pins the anchored `*2..` reference
// on the fixture: from "a" the trails of length >= 2 are the ones the hand
// enumeration above lists with a as their first node — depth 2: (ab,bc),
// (ab,ba); depth 3: (ab,bc,ca), (ab,bc,cc); depth 4: (ab,bc,cc,ca) — five in
// all. The enumeration must report itself EXHAUSTIVE, since the fixture's
// longest trail (5) is far below [vleAnchoredDepthCap].
func TestVarlenPaths_LowerBoundOnlyIsExhaustive(t *testing.T) {
	_, o := buildMotifFixture(t)
	m := buildVLEModel(o)
	w := countTrails(m.knowsOut, []uint64{o.byName["a"]}, vleAnchoredDepthCap, true)
	if !w.exact {
		t.Fatal("the anchored enumeration declared itself inexact on a five-edge fixture")
	}
	if got, want := w.total(2, vleAnchoredDepthCap), int64(5); got != want {
		t.Errorf("trails of length >= 2 from \"a\" = %d, want %d", got, want)
	}
	if !m.reachesTwoHops(o.byName["a"]) {
		t.Error("\"a\" reaches two hops but the anchor precondition says otherwise")
	}
	if m.reachesTwoHops(o.byName["d"]) {
		t.Error("\"d\" has no outgoing KNOWS edge yet the anchor precondition accepted it")
	}
}

// TestVarlenPaths_ReferenceIsUniquenessSensitive proves the reference really
// enumerates TRAILS and not plain walks — that is, that it encodes the
// openCypher rule forbidding a relationship to repeat within one
// variable-length path. Counting walks instead would give 8 at depth 2 on the
// fixture (the self-loop would continue into itself) against the correct 7, so
// the probe would not survive a Cyphermorphism regression if it did.
func TestVarlenPaths_ReferenceIsUniquenessSensitive(t *testing.T) {
	_, o := buildMotifFixture(t)
	m := buildVLEModel(o)

	trails := countTrails(m.knowsOut, m.persons, 2, false).perDepth[2]

	// The same composition WITHOUT the uniqueness rule.
	var walks int64
	for _, start := range m.persons {
		for _, s1 := range m.knowsOut[start] {
			walks += int64(len(m.knowsOut[s1.to]))
		}
	}
	if trails != 7 {
		t.Fatalf("trail reference at depth 2 = %d, want 7", trails)
	}
	if walks != 8 {
		t.Fatalf("walk (no-uniqueness) count at depth 2 = %d, want 8", walks)
	}
	if trails == walks {
		t.Fatal("the fixture cannot distinguish trails from walks, so the probe is blind to a uniqueness regression")
	}
}

// TestVarlenPaths_SensitivityToWrongReference proves the battery FIRES when the
// oracle's model is perturbed: an edge removed, a phantom edge added, the
// self-loop dropped, and an age moved across the predicate threshold. In each
// case the engine is untouched and correct, so the disagreement must surface.
func TestVarlenPaths_SensitivityToWrongReference(t *testing.T) {
	cases := []struct {
		name    string
		perturb func(o *GraphOracle)
	}{
		{"missing KNOWS edge", func(o *GraphOracle) {
			delete(o.edges, edgeKey{src: o.byName["b"], dst: o.byName["c"], label: "KNOWS"})
		}},
		{"phantom KNOWS edge", func(o *GraphOracle) {
			src, dst := o.byName["d"], o.byName["b"]
			o.edges[edgeKey{src: src, dst: dst, label: "KNOWS"}] =
				&EdgeState{SrcID: src, DstID: dst, Label: "KNOWS", Properties: map[string]any{}}
		}},
		{"dropped self-loop", func(o *GraphOracle) {
			delete(o.edges, edgeKey{src: o.byName["c"], dst: o.byName["c"], label: "KNOWS"})
		}},
		{"phantom FOLLOWS edge", func(o *GraphOracle) {
			src, dst := o.byName["b"], o.byName["c"]
			o.edges[edgeKey{src: src, dst: dst, label: "FOLLOWS"}] =
				&EdgeState{SrcID: src, DstID: dst, Label: "FOLLOWS", Properties: map[string]any{}}
		}},
		{"age moved across the predicate threshold", func(o *GraphOracle) {
			// Every fixture age is below vleAgeThreshold, so the predicate arms
			// reference zero rows. Lifting b above it makes the model claim
			// matches the engine cannot produce.
			o.nodes[o.byName["b"]].Properties["age"] = int64(vleAgeThreshold + 1)
		}},
		{"extra Person the engine does not have", func(o *GraphOracle) {
			// Inflates the length-0 reference without touching adjacency, so only
			// the zero-length arms can catch it.
			o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "ghost", "age": int64(1)})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, o := buildMotifFixture(t)
			c.perturb(o)
			if v := CheckVarlenPaths(0, o, a, &vleStats{}); len(v) == 0 {
				t.Fatal("the variable-length battery FAILED to fire on a perturbed reference")
			}
		})
	}
}

// TestVarlenPaths_ZeroLengthContract pins the zero-length semantics the battery
// depends on, directly against the engine: `*0..0` yields exactly one row per
// node matching the left-hand pattern, that row binds both endpoints to the
// SAME node, it survives a relationship type that does not exist at all, and it
// contributes length 0, one node and zero relationships to the path functions.
func TestVarlenPaths_ZeroLengthContract(t *testing.T) {
	a, o := buildMotifFixture(t)
	ctx := context.Background()
	n := int64(len(buildVLEModel(o).persons))

	for _, c := range []struct {
		name  string
		query string
		want  int64
	}{
		{"one row per Person", tmplVLEZeroOnly, n},
		{"both endpoints are the same node", tmplVLEZeroIdentity, n},
		{"independent of the type existing", tmplVLEZeroAbsentType, n},
		{"the far-side pattern still applies",
			"MATCH (a:Person)-[:KNOWS*0..0]->(b:NoSuchLabel) RETURN count(*)", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := scalarCountWithParams(ctx, a, c.query, nil)
			if err != nil {
				t.Fatalf("%s: %v", c.query, err)
			}
			if got != c.want {
				t.Errorf("%s = %d, want %d", c.query, got, c.want)
			}
		})
	}

	// The zero-length row carries no relationship: over `*0..0` the length and
	// relationship sums are 0 while the node sum is one per row.
	t.Run("path functions over the length-0 binding", func(t *testing.T) {
		if v := checkVLEPathFunctions(ctx, 0, a, "zero-length path functions",
			"MATCH p=(a:Person)-[:KNOWS*0..0]->(b) "+
				"RETURN count(*), sum(length(p)), sum(size(nodes(p))), sum(size(relationships(p)))",
			n, 0); len(v) > 0 {
			t.Fatalf("the length-0 binding is not length 0 / 1 node / 0 relationships: %v", v)
		}
	})
}

// TestVarlenPaths_IntermediatePredicateDistinguishesScope proves against the
// ENGINE — not merely against the reference — that a predicate over
// `nodes(p)[1..-1]` constrains only the intermediate nodes while the same
// predicate over `nodes(p)` constrains the endpoints too. At threshold 20 on
// the fixture the two counts are 3 and 0 (derived by hand in
// TestVarlenPaths_MotifFixture_HandEnumerated), so an engine that dropped the
// slice — or applied it to the wrong end — could not produce both.
//
// [CheckVarlenPaths] runs these arms at [vleAgeThreshold], which the fixture's
// ages all sit below; this test therefore supplies the threshold that makes the
// fixture discriminate, and the scenario's own population supplies it for the
// live run (the terminal gate fails a run in which the predicate never
// strictly filtered).
func TestVarlenPaths_IntermediatePredicateDistinguishesScope(t *testing.T) {
	a, _ := buildMotifFixture(t)
	ctx := context.Background()
	params := map[string]any{"t": int64(20)}

	mid, err := scalarCountWithParams(ctx, a, tmplVLEMiddlePredParam, params)
	if err != nil {
		t.Fatalf("intermediate-node predicate: %v", err)
	}
	all, err := scalarCountWithParams(ctx, a, tmplVLEAllNodesPredParam, params)
	if err != nil {
		t.Fatalf("all-path-node predicate: %v", err)
	}
	if mid != 3 {
		t.Errorf("intermediate-node predicate = %d, want 3", mid)
	}
	if all != 0 {
		t.Errorf("all-path-node predicate = %d, want 0", all)
	}
	if mid == all {
		t.Fatal("the engine gives the same answer for both predicate scopes, so the slice is not being applied")
	}
}

// TestVarlenPaths_PathFunctionsFireOnAWrongReference proves the four-column
// path-function adjudication is not merely self-consistent: a reference off by
// one row, or off in the length sum, must be rejected.
func TestVarlenPaths_PathFunctionsFireOnAWrongReference(t *testing.T) {
	a, o := buildMotifFixture(t)
	ctx := context.Background()
	m := buildVLEModel(o)
	knows := countTrails(m.knowsOut, m.persons, vleMaxDepth, false)
	rows, length := knows.total(1, 3), knows.weighted(1, 3)

	if v := checkVLEPathFunctions(ctx, 0, a, "correct", tmplVLEPathFunctions, rows, length); len(v) > 0 {
		t.Fatalf("the correct reference was rejected: %v", v)
	}
	for _, c := range []struct {
		name        string
		rows, lenth int64
	}{
		{"row count off by one", rows + 1, length},
		{"length sum off by one", rows, length + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if v := checkVLEPathFunctions(ctx, 0, a, "perturbed", tmplVLEPathFunctions, c.rows, c.lenth); len(v) == 0 {
				t.Fatal("the path-function adjudication FAILED to fire on a wrong reference")
			}
		})
	}
}

// TestVarlenPaths_EnumerationBoundIsHonoured proves the two guards actually
// guard: an enumeration that runs out of walk budget, and one whose depth cap
// would have truncated an unbounded trail, both report themselves INEXACT — so
// the lower-bound-only probe stands down rather than comparing a partial count.
func TestVarlenPaths_EnumerationBoundIsHonoured(t *testing.T) {
	_, o := buildMotifFixture(t)
	m := buildVLEModel(o)
	starts := []uint64{o.byName["a"]}

	t.Run("depth cap truncates an unbounded enumeration", func(t *testing.T) {
		// The fixture holds trails of length 5, so a cap of 2 must be reported as
		// inexact when exhaustiveness is required...
		if countTrails(m.knowsOut, starts, 2, true).exact {
			t.Fatal("a depth cap below the longest trail was reported as exhaustive")
		}
		// ...and accepted when it is the query's own bound.
		if !countTrails(m.knowsOut, starts, 2, false).exact {
			t.Fatal("a bounded enumeration was reported as inexact although only the query's limit applied")
		}
		// A cap above the longest trail is exhaustive either way.
		if !countTrails(m.knowsOut, starts, vleAnchoredDepthCap, true).exact {
			t.Fatal("an enumeration deeper than the longest trail was reported as inexact")
		}
	})

	t.Run("walk budget truncates", func(t *testing.T) {
		var seen int64
		exact := enumerateTrails(m.knowsOut, m.persons, vleMaxDepth, 3, false,
			func(_ []uint64, depth int) {
				if depth > 0 {
					seen++
				}
			})
		if exact {
			t.Fatal("an enumeration that spent its whole budget claimed to be exhaustive")
		}
		if seen > 3 {
			t.Errorf("the enumeration visited %d trails on a budget of 3", seen)
		}
	})
}

// TestVarlenPaths_VacuityFiresOnAMissingArm proves the terminal gate refuses a
// run in which any arm did nothing, and passes only when every one of them did.
func TestVarlenPaths_VacuityFiresOnAMissingArm(t *testing.T) {
	// A model rich enough to clear the fixture-side floors: a directed KNOWS
	// chain of 30 Persons (29 two-hop and 28 three-hop trails on its own), plus
	// a back edge and a FOLLOWS edge.
	rich := func() *GraphOracle {
		o := NewGraphOracle()
		const n = 30
		name := func(i int) string { return fmt.Sprintf("n%02d", i) }
		for i := range n {
			o.ApplyCreate(tmplCreatePerson, map[string]any{"name": name(i), "age": int64(3 * i)})
		}
		for i := range n - 1 {
			o.ApplyCreate(tmplCreateKnows, map[string]any{"a": name(i), "b": name(i + 1)})
		}
		o.ApplyCreate(tmplCreateKnows, map[string]any{"a": name(2), "b": name(0)})
		o.ApplyCreate(tmplCreateFollows, map[string]any{"a": name(0), "b": name(1)})
		return o
	}

	t.Run("every arm fired", func(t *testing.T) {
		if v := varlenPathsVacuity(0, rich(), vleFixtureStats()); len(v) > 0 {
			t.Fatalf("the vacuity gate fired although every arm ran: %v", v)
		}
	})

	arms := map[string]func(*vleStats){
		"depth2Seen":      func(s *vleStats) { s.depth2Seen = 0 },
		"zeroLengthSeen":  func(s *vleStats) { s.zeroLengthSeen = 0 },
		"multiTypeBeyond": func(s *vleStats) { s.multiTypeBeyond = 0 },
		"undirectedAbove": func(s *vleStats) { s.undirectedAbove = 0 },
		"unboundedDeep":   func(s *vleStats) { s.unboundedDeep = 0 },
		"predSelective":   func(s *vleStats) { s.predSelective = 0 },
	}
	for name, clear := range arms {
		t.Run("missing "+name, func(t *testing.T) {
			st := vleFixtureStats()
			clear(st)
			if v := varlenPathsVacuity(0, rich(), st); len(v) == 0 {
				t.Fatalf("the vacuity gate FAILED to fire with %s never recorded", name)
			}
		})
	}

	t.Run("graph too small", func(t *testing.T) {
		// Every arm recorded, but a two-node model can never have supported them:
		// the fixture-side floors must catch it.
		o := NewGraphOracle()
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "x", "age": int64(1)})
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": "y", "age": int64(2)})
		o.ApplyCreate(tmplCreateKnows, map[string]any{"a": "x", "b": "y"})
		v := varlenPathsVacuity(0, o, vleFixtureStats())
		if len(v) == 0 {
			t.Fatal("the vacuity gate FAILED to fire on a model too small to exercise multi-hop references")
		}
		if !strings.Contains(v[0].Message, "two-hop trail") {
			t.Errorf("unexpected vacuity message: %s", v[0].Message)
		}
	})
}

// TestVarlenPaths_ScenarioRecordsEveryArm runs the registered pattern-shapes
// scenario, which now carries the variable-length battery, and proves it passes
// end to end — including the terminal non-vacuity gate, so every VLE arm
// demonstrably fired against the real workload rather than merely compiling.
func TestVarlenPaths_ScenarioRecordsEveryArm(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioPatternShapes)
	if !ok {
		t.Fatal("pattern-shapes scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("pattern-shapes run: %v", err)
	}
	if report != nil {
		t.Fatalf("the scenario reported a violation:\n%s", report)
	}
}
