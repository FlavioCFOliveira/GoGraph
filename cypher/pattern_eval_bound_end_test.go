package cypher

// pattern_eval_bound_end_test.go — the bound-end guard of the pattern-predicate
// evaluator (rmp #2894).
//
// `WHERE NOT (h)-[:LINK]->(s)` with both h and s bound used to resolve the edge
// labels of EVERY neighbour of h before the end-node check rejected all but s,
// so one evaluation over a hub cost deg(h) versioned label lookups. The
// incoming leg scanned every node of the graph and resolved labels for every
// node with an arc into the anchor. These tests bound the lookups per
// evaluation and pin that the verdicts — parallel edges, self-loops, undirected
// patterns, snapshot visibility — are unchanged.
//
// Layer: short.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// boundEndSpokes is the hub's out-degree. It is large enough that a per-neighbour
// lookup cannot hide under the bound asserted below.
const boundEndSpokes = 256

// boundEndHubGraph builds hub h with boundEndSpokes :LINK arcs to s0..s255, plus
// boundEndSpokes further hubs g0..g255 that each point at s0 with :LINK, and a
// node "lone" nothing points at.
func boundEndHubGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	mustNode := func(k string) {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
	}
	mustEdge := func(src, dst, typ string) {
		if err := g.AddEdgeLabeled(src, dst, 1, typ); err != nil {
			t.Fatalf("AddEdgeLabeled %s-[:%s]->%s: %v", src, typ, dst, err)
		}
	}
	mustNode("h")
	mustNode("lone")
	for i := range boundEndSpokes {
		mustNode(fmt.Sprintf("s%d", i))
		mustNode(fmt.Sprintf("g%d", i))
	}
	for i := range boundEndSpokes {
		mustEdge("h", fmt.Sprintf("s%d", i), "LINK")
		mustEdge(fmt.Sprintf("g%d", i), "s0", "LINK")
	}
	return g
}

// typedHop is the AST for `(a)-[:typ]-(b)` in direction dir, with both
// endpoints named.
func typedHop(a, b, typ string, dir ast.RelDirection) *ast.PathPattern {
	return &ast.PathPattern{Head: &ast.PathElement{
		Node: &ast.NodePattern{Variable: &a},
		Next: &ast.PathElement{
			Relationship: &ast.RelationshipPattern{Direction: dir, Types: []string{typ}},
			Node:         &ast.NodePattern{Variable: &b},
		},
	}}
}

// bindRow binds each var to the interned NodeID of its key.
func bindRow(t *testing.T, g *lpg.Graph[string, float64], pairs ...string) expr.RowContext {
	t.Helper()
	row := expr.RowContext{}
	for i := 0; i+1 < len(pairs); i += 2 {
		id, ok := g.AdjList().Mapper().Lookup(pairs[i+1])
		if !ok {
			t.Fatalf("node %q has no interned NodeID", pairs[i+1])
		}
		row[pairs[i]] = expr.NodeValue{ID: uint64(id)}
	}
	return row
}

// evalCounting evaluates pp over view and returns the verdict together with the
// number of edge-type lookups the evaluation spent.
func evalCounting(t *testing.T, view *lpg.ReadView[string, float64], pp *ast.PathPattern, row expr.RowContext) (found bool, lookups uint64) {
	t.Helper()
	pe := newPatternEvaluator(view, 0)
	v, err := pe.EvalPattern(context.Background(), pp, row, nil)
	if err != nil {
		t.Fatalf("EvalPattern: %v", err)
	}
	b, ok := v.(expr.BoolValue)
	if !ok {
		t.Fatalf("EvalPattern returned %T, want expr.BoolValue", v)
	}
	return bool(b), pe.relTypeChecks
}

// TestPatternPredicate_BoundEndBoundsLabelLookups pins that a predicate with
// BOTH endpoints bound spends at most one edge-type lookup per direction it
// crosses, whatever the degree of the anchor. On the pre-#2894 evaluator the
// outgoing cases spent boundEndSpokes lookups and the incoming cases spent one
// per hub pointing at the anchor.
func TestPatternPredicate_BoundEndBoundsLabelLookups(t *testing.T) {
	t.Parallel()
	g := boundEndHubGraph(t)
	last := fmt.Sprintf("s%d", boundEndSpokes-1)
	lastHub := fmt.Sprintf("g%d", boundEndSpokes-1)

	cases := []struct {
		name       string
		pp         *ast.PathPattern
		row        []string
		want       bool
		maxLookups uint64
	}{
		{"outgoing/neighbour", typedHop("h", "s", "LINK", ast.RelDirectionOutgoing), []string{"h", "h", "s", last}, true, 1},
		{"outgoing/non-neighbour", typedHop("h", "s", "LINK", ast.RelDirectionOutgoing), []string{"h", "h", "s", "lone"}, false, 0},
		{"outgoing/wrong-type", typedHop("h", "s", "OTHER", ast.RelDirectionOutgoing), []string{"h", "h", "s", last}, false, 1},
		{"incoming/neighbour", typedHop("s", "x", "LINK", ast.RelDirectionIncoming), []string{"s", "s0", "x", lastHub}, true, 1},
		{"incoming/non-neighbour", typedHop("s", "x", "LINK", ast.RelDirectionIncoming), []string{"s", "s0", "x", "lone"}, false, 0},
		// The undirected hop crosses both legs: the outgoing leg from s0 has no
		// slot to lastHub (0 lookups), the incoming leg finds lastHub → s0 (1).
		{"undirected/neighbour", typedHop("s", "x", "LINK", ast.RelDirectionNone), []string{"s", "s0", "x", lastHub}, true, 1},
		{"undirected/non-neighbour", typedHop("h", "s", "LINK", ast.RelDirectionNone), []string{"h", "h", "s", "lone"}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			found, lookups := evalCounting(t, g.ReadAt(nil), tc.pp, bindRow(t, g, tc.row...))
			if found != tc.want {
				t.Fatalf("verdict = %v, want %v", found, tc.want)
			}
			if lookups > tc.maxLookups {
				t.Errorf("edge-type lookups = %d, want <= %d: the evaluator resolved labels for "+
					"neighbours the bound end node rejects", lookups, tc.maxLookups)
			}
		})
	}
}

// TestPatternPredicate_UnboundEndStillScans is the control for the test above:
// with the end node unbound the evaluator must still look at the neighbours, so
// the lookup counter is shown to observe the work it bounds.
func TestPatternPredicate_UnboundEndStillScans(t *testing.T) {
	t.Parallel()
	g := boundEndHubGraph(t)
	pp := typedHop("h", "s", "OTHER", ast.RelDirectionOutgoing)
	found, lookups := evalCounting(t, g.ReadAt(nil), pp, bindRow(t, g, "h", "h"))
	if found {
		t.Fatal("verdict = true, want false: no :OTHER arc exists")
	}
	if lookups != boundEndSpokes {
		t.Fatalf("edge-type lookups = %d, want %d (one per neighbour of the hub)", lookups, boundEndSpokes)
	}
}

// TestPatternPredicate_BoundEndVerdicts pins the verdicts the guard must not
// change: parallel edges of different types, self-loops in every direction, and
// the undirected composition.
func TestPatternPredicate_BoundEndVerdicts(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	for _, k := range []string{"a", "b", "c", "loop"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
	}
	for _, e := range []struct{ src, dst, typ string }{
		{"a", "b", "FIRST"},
		{"a", "b", "SECOND"}, // parallel arc, a different type
		{"a", "c", "FIRST"},
		{"loop", "loop", "SELF"},
	} {
		if err := g.AddEdgeLabeled(e.src, e.dst, 1, e.typ); err != nil {
			t.Fatalf("AddEdgeLabeled: %v", err)
		}
	}
	cases := []struct {
		name string
		pp   *ast.PathPattern
		row  []string
		want bool
	}{
		{"parallel/first", typedHop("x", "y", "FIRST", ast.RelDirectionOutgoing), []string{"x", "a", "y", "b"}, true},
		{"parallel/second", typedHop("x", "y", "SECOND", ast.RelDirectionOutgoing), []string{"x", "a", "y", "b"}, true},
		{"parallel/absent-type", typedHop("x", "y", "SECOND", ast.RelDirectionOutgoing), []string{"x", "a", "y", "c"}, false},
		{"parallel/incoming", typedHop("y", "x", "SECOND", ast.RelDirectionIncoming), []string{"x", "a", "y", "b"}, true},
		{"parallel/reverse-direction", typedHop("y", "x", "FIRST", ast.RelDirectionOutgoing), []string{"x", "a", "y", "b"}, false},
		{"parallel/undirected-reversed", typedHop("y", "x", "SECOND", ast.RelDirectionNone), []string{"x", "a", "y", "b"}, true},
		{"selfloop/outgoing", typedHop("x", "y", "SELF", ast.RelDirectionOutgoing), []string{"x", "loop", "y", "loop"}, true},
		{"selfloop/incoming", typedHop("x", "y", "SELF", ast.RelDirectionIncoming), []string{"x", "loop", "y", "loop"}, true},
		{"selfloop/undirected", typedHop("x", "y", "SELF", ast.RelDirectionNone), []string{"x", "loop", "y", "loop"}, true},
		{"selfloop/other-node", typedHop("x", "y", "SELF", ast.RelDirectionNone), []string{"x", "loop", "y", "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			found, _ := evalCounting(t, g.ReadAt(nil), tc.pp, bindRow(t, g, tc.row...))
			if found != tc.want {
				t.Fatalf("verdict = %v, want %v", found, tc.want)
			}
		})
	}
}

// TestPatternPredicate_BoundEndSnapshotVisibility holds a snapshot across the
// removal of one arc and the addition of another, and pins that the bound-end
// path answers both directions from the snapshot, on both legs.
func TestPatternPredicate_BoundEndSnapshotVisibility(t *testing.T) {
	t.Parallel()
	g := boundEndHubGraph(t)
	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	must(t).E(g.RemoveEdge("h", "s7"))
	if err := g.AddEdgeLabeled("h", "lone", 1, "LINK"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}

	cases := []struct {
		name     string
		pp       *ast.PathPattern
		row      []string
		wantNow  bool
		wantSnap bool
	}{
		{"outgoing/removed", typedHop("h", "s", "LINK", ast.RelDirectionOutgoing), []string{"h", "h", "s", "s7"}, false, true},
		{"outgoing/added", typedHop("h", "s", "LINK", ast.RelDirectionOutgoing), []string{"h", "h", "s", "lone"}, true, false},
		{"incoming/removed", typedHop("s", "x", "LINK", ast.RelDirectionIncoming), []string{"s", "s7", "x", "h"}, false, true},
		{"incoming/added", typedHop("s", "x", "LINK", ast.RelDirectionIncoming), []string{"s", "lone", "x", "h"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := bindRow(t, g, tc.row...)
			if got, _ := evalCounting(t, g.ReadAt(nil), tc.pp, row); got != tc.wantNow {
				t.Fatalf("verdict at the CURRENT instant = %v, want %v: the fixture is wrong", got, tc.wantNow)
			}
			if got, _ := evalCounting(t, g.ReadAt(old), tc.pp, row); got != tc.wantSnap {
				t.Errorf("verdict at the OLD snapshot = %v, want %v: the bound-end path did not "+
					"answer from the snapshot", got, tc.wantSnap)
			}
		})
	}
}
