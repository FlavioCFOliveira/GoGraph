package cypher

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file is the regression gate for rmp #2241: the typed degree rewrite
// shipped by #2232 under-counted parallel edges, so
// `COUNT { (a)-[:K]->() }` returned 1 where 3 was correct.
//
// Why the original suite missed it: degreeFixture never creates two edges
// between the same pair, so no case in it exercises a parallel edge at all.
// Every case here uses a multigraph fixture that does.
//
// Root cause, for the reader who finds this test failing again: the adjacency
// label column holds at most ONE label per (src, dst) pair, because
// AdjList.SetEdgeLabelSlot scans for the first dst-matching slot and stops. The
// authoritative per-edge type lives in the handle-keyed store that
// CreateRelationship writes (SetEdgeLabelByHandle), which is what
// Graph.slotCarriesType consults first.

// parallelFixture builds one anchor with the requested parallel out-edges. spec
// lists one relationship type per edge, in creation order, all to the SAME far
// node — so every entry is a parallel edge of the pair.
func parallelFixture(t *testing.T, spec ...string) (*lpg.Graph[string, float64], *Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := NewEngine(g)
	mustRun(t, eng, "CREATE (:P {id: 0})")
	mustRun(t, eng, "CREATE (:P:Q {id: 1})")
	for _, typ := range spec {
		mustRun(t, eng, fmt.Sprintf(
			"MATCH (a {id: 0}), (b {id: 1}) CREATE (a)-[:%s]->(b)", typ))
	}
	return g, eng
}

// TestDegreeRewrite_ParallelEdges_AgreeWithEnumeratingOracle is acceptance
// criteria 1 and 2 of rmp #2241. The oracle is the FULL subquery form, which
// binds a relationship variable and therefore enumerates rather than taking any
// degree shortcut.
func TestDegreeRewrite_ParallelEdges_AgreeWithEnumeratingOracle(t *testing.T) {
	cases := []struct {
		name string
		spec []string
		// wantK is the hand-computed number of :K edges — the absolute oracle,
		// which is what caught this defect in the first place.
		wantK int
	}{
		{"one edge", []string{"K"}, 1},
		{"two parallel edges of the same type", []string{"K", "K"}, 2},
		{"three parallel edges of the same type", []string{"K", "K", "K"}, 3},
		{"mixed types between the same pair", []string{"K", "M", "K", "M"}, 2},
		{"the counted type created last", []string{"M", "M", "K"}, 1},
		{"the counted type absent", []string{"M", "M"}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, eng := parallelFixture(t, tc.spec...)
			wantK := fmt.Sprintf("%d\x1f", tc.wantK)

			// The degree rewrite, typed, unlabelled far node.
			if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() }"); got[0] != wantK {
				t.Errorf("COUNT { (a)-[:K]->() } = %v, want %s (spec %v)", got, wantK, tc.spec)
			}
			// The enumerating oracle.
			oracle := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { MATCH (a)-[r:K]->(b) RETURN r }")
			if oracle[0] != wantK {
				t.Fatalf("the enumerating oracle itself returned %v, want %s — the fixture is "+
					"not what this case assumes (spec %v)", oracle, wantK, tc.spec)
			}
			// The labelled far node, which goes through the #2235 path instead.
			if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->(:Q) }"); got[0] != wantK {
				t.Errorf("COUNT { (a)-[:K]->(:Q) } = %v, want %s (spec %v)", got, wantK, tc.spec)
			}
			// EXISTS and size([pattern]) must agree with the same count.
			wantExists := fmt.Sprintf("%t\x1f", tc.wantK > 0)
			if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN EXISTS { (a)-[:K]->() }"); got[0] != wantExists {
				t.Errorf("EXISTS { (a)-[:K]->() } = %v, want %s (spec %v)", got, wantExists, tc.spec)
			}
			if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN size([(a)-[:K]->() | 1])"); got[0] != wantK {
				t.Errorf("size([(a)-[:K]->() | 1]) = %v, want %s (spec %v)", got, wantK, tc.spec)
			}
		})
	}
}

// TestDegreeRewrite_ParallelEdges_UntypedIsUnaffected pins the half that was
// always correct, so a future change to the typed path cannot quietly break it.
// The untyped degree reads the adjacency column length, which counts slots and
// so never had the collision.
func TestDegreeRewrite_ParallelEdges_UntypedIsUnaffected(t *testing.T) {
	_, eng := parallelFixture(t, "K", "M", "K", "M")
	if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { (a)-->() }"); got[0] != "4\x1f" {
		t.Errorf("COUNT { (a)-->() } = %v, want 4", got)
	}
}

// TestDegreeRewrite_ParallelEdges_BoundedAndUnboundedAgree pins the contract the
// bounded and unbounded walkers document: they may differ about WHEN they stop
// counting, never about WHICH edges count. A cap above the true count must give
// the true count.
func TestDegreeRewrite_ParallelEdges_BoundedAndUnboundedAgree(t *testing.T) {
	_, eng := parallelFixture(t, "K", "K", "K", "M")

	// The bounded walk is reached through a comparison against a literal; the
	// unbounded one through a bare projection.
	if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() }"); got[0] != "3\x1f" {
		t.Fatalf("unbounded = %v, want 3", got)
	}
	for _, tc := range []struct{ query, want string }{
		{"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() } > 0", "true\x1f"},
		{"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() } > 2", "true\x1f"},
		{"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() } > 3", "false\x1f"},
		{"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() } = 3", "true\x1f"},
		{"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() } = 1", "false\x1f"},
		{"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() } <= 3", "true\x1f"},
	} {
		if got := degreeRun(t, eng, tc.query); got[0] != tc.want {
			t.Errorf("%s = %v, want %s", tc.query, got, tc.want)
		}
	}
}

// TestExistsSubquery_HonoursInlineWhere is the regression gate for the EXISTS
// half of rmp #2242. The pattern form of EXISTS, evaluated as an EXPRESSION,
// discarded its inline WHERE: existsToSingleQuery built the inner ast.Match
// without threading sub.Where, so any predicate was dropped and the bare
// pattern's verdict was returned.
//
// The WHERE-position spelling was never affected — the planner lowers it to a
// SemiApply carrying its own Selection — which is why only an expression-position
// case can catch this.
func TestExistsSubquery_HonoursInlineWhere(t *testing.T) {
	eng := NewEngine(degreeFixture(t, 60))

	// n1's only :K edge lands on n2, and n2 does not carry :Q. Established
	// independently by the pattern comprehension, which enumerates.
	if got := degreeRun(t, eng, "MATCH (a:P {id: 1}) RETURN [(a)-[:K]->(b) | b.id]"); got[0] != "[2]\x1f" {
		t.Fatalf("fixture assumption broken: n1's :K targets are %v, want [2]", got)
	}
	if got := degreeRun(t, eng, "MATCH (a:P {id: 1}) RETURN [(a)-[:K]->(b:Q) | b.id]"); got[0] != "[]\x1f" {
		t.Fatalf("fixture assumption broken: n1's :Q-labelled :K targets are %v, want []", got)
	}

	for _, tc := range []struct{ name, query, want string }{
		{
			// The unambiguous case: a predicate that can never hold. Before the
			// fix this returned true.
			name:  "a predicate that can never hold",
			query: "MATCH (a:P {id: 1}) RETURN EXISTS { (a)-[:K]->(b) WHERE b.id = 999 }",
			want:  "false\x1f",
		},
		{
			name:  "label predicate the far node does not satisfy",
			query: "MATCH (a:P {id: 1}) RETURN EXISTS { (a)-[:K]->(b) WHERE b:Q }",
			want:  "false\x1f",
		},
		{
			name:  "label predicate the far node does satisfy",
			query: "MATCH (a:P {id: 1}) RETURN EXISTS { (a)-[:K]->(b) WHERE b.id = 2 }",
			want:  "true\x1f",
		},
		{
			name:  "agrees with the full subquery form",
			query: "MATCH (a:P {id: 1}) RETURN EXISTS { MATCH (a)-[:K]->(b) WHERE b:Q RETURN b }",
			want:  "false\x1f",
		},
		{
			name:  "no predicate still matches the bare pattern",
			query: "MATCH (a:P {id: 1}) RETURN EXISTS { (a)-[:K]->(b) }",
			want:  "true\x1f",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := degreeRun(t, eng, tc.query); got[0] != tc.want {
				t.Errorf("%s = %v, want %s", tc.query, got, tc.want)
			}
		})
	}
}
