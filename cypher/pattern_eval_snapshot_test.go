package cypher

// pattern_eval_snapshot_test.go — snapshot visibility for pattern predicates and
// pattern comprehensions (rmp #2294, MVCC P4e).
//
// The pattern evaluator holds a [lpg.ReadView] bound to its query's snapshot,
// and then read topology through [lpg.ReadView.AdjList] — which is documented as
// returning the adjacency UNBOUND from the view's instant. So every
// `WHERE (a)-[:T]->(b)` existential predicate and every pattern comprehension
// answered from the PRESENT while the rest of the same query answered from the
// snapshot, and a query could return a row set satisfying no single instant.
//
// This is the same defect class as rmp #2293, which fixed it for the CSR pair
// the expand operators traverse. It is a second, independent code path, so it
// needed its own tests and its own fix.
//
// Every test below holds a snapshot ACROSS a committed write rather than racing
// for the window, so each is deterministic. Both directions are covered, because
// they fail on opposite inputs and neither substitutes for the other: an arc
// added after the snapshot must be invisible, and an arc removed after it must
// remain visible.
//
// Layer: short.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// readSourceFile reads a file from this package's directory. A test binary runs
// with its package directory as the working directory, so the bare name resolves.
func readSourceFile(name string) (string, error) {
	b, err := os.ReadFile(name) //nolint:gosec // G304: name is a source file in this package's own directory — a test binary runs with that directory as its working directory (see the readSourceFile doc).
	return string(b), err
}

// countOccurrences counts non-overlapping occurrences of sub in s.
func countOccurrences(s, sub string) int { return strings.Count(s, sub) }

// snapPatternGraph builds a two-node graph with NO edge between them, armed for
// MVCC. The edge is added later, after a snapshot has been taken, which is what
// makes the visibility question meaningful.
//
// It uses the lpg API directly rather than the Cypher engine so the node keys are
// the interned strings the mapper resolves, which keeps the test about the
// evaluator rather than about key resolution.
func snapPatternGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
	}
	return g
}

// outgoingHop is the AST for `(x)-->(y)`, the shape a `WHERE (x)-->(y)`
// existential predicate parses to.
func outgoingHop(anchorVar, endVar string) *ast.PathPattern {
	anchor, end := anchorVar, endVar
	return &ast.PathPattern{Head: &ast.PathElement{
		Node: &ast.NodePattern{Variable: &anchor},
		Next: &ast.PathElement{
			Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionOutgoing},
			Node:         &ast.NodePattern{Variable: &end},
		},
	}}
}

// anchorRow binds anchorVar to key's interned NodeID.
func anchorRow(t *testing.T, g *lpg.Graph[string, float64], anchorVar, key string) expr.RowContext {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(key)
	if !ok {
		t.Fatalf("node %q has no interned NodeID", key)
	}
	return expr.RowContext{anchorVar: expr.NodeValue{ID: uint64(id)}}
}

// evalPredicateAt evaluates `(x)-->(y)` as an existential predicate over the
// view bound to snap.
func evalPredicateAt(t *testing.T, g *lpg.Graph[string, float64], snap *lpg.Snapshot) bool {
	t.Helper()
	pe := newPatternEvaluator(g.ReadAt(snap), 0)
	v, err := pe.EvalPattern(context.Background(), outgoingHop("x", "y"), anchorRow(t, g, "x", "a"), nil)
	if err != nil {
		t.Fatalf("EvalPattern: %v", err)
	}
	b, ok := v.(expr.BoolValue)
	if !ok {
		t.Fatalf("EvalPattern returned %T, want expr.BoolValue", v)
	}
	return bool(b)
}

// evalComprehensionAt evaluates `[(x)-->(y) | y]` over the view bound to snap and
// returns how many matches it produced.
func evalComprehensionAt(t *testing.T, g *lpg.Graph[string, float64], snap *lpg.Snapshot) int {
	t.Helper()
	pe := newPatternEvaluator(g.ReadAt(snap), 0)
	end := "y"
	pc := &ast.PatternComprehension{
		Pattern:    outgoingHop("x", end),
		Projection: &ast.Variable{Name: end},
	}
	v, err := pe.EvalPatternComp(context.Background(), pc, anchorRow(t, g, "x", "a"), nil, nil)
	if err != nil {
		t.Fatalf("EvalPatternComp: %v", err)
	}
	l, ok := v.(expr.ListValue)
	if !ok {
		t.Fatalf("EvalPatternComp returned %T, want expr.ListValue", v)
	}
	return len(l)
}

// TestPatternPredicate_EdgeAddedAfterSnapshotIsInvisible pins that an
// existential pattern predicate does not observe an arc committed after its
// read began.
//
// Both endpoints exist before the snapshot, so node liveness cannot mask the
// arc: if the predicate answers true, it read the present.
func TestPatternPredicate_EdgeAddedAfterSnapshotIsInvisible(t *testing.T) {
	t.Parallel()
	g := snapPatternGraph(t)

	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	if got := evalPredicateAt(t, g, nil); !got {
		t.Fatalf("predicate at the CURRENT instant = false, want true: the fixture is wrong")
	}
	if got := evalPredicateAt(t, g, old); got {
		t.Error("pattern predicate at the OLD snapshot = true, want false: it observed an " +
			"edge committed after its snapshot started, so it read the PRESENT topology")
	}
}

// TestPatternPredicate_EdgeRemovedAfterSnapshotStaysVisible is the opposite
// input: an arc that existed at this read's instant must stay visible to it.
func TestPatternPredicate_EdgeRemovedAfterSnapshotStaysVisible(t *testing.T) {
	t.Parallel()
	g := snapPatternGraph(t)
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	g.RemoveEdge("a", "b")

	if got := evalPredicateAt(t, g, nil); got {
		t.Fatalf("predicate at the CURRENT instant = true, want false: the fixture is wrong")
	}
	if got := evalPredicateAt(t, g, old); !got {
		t.Error("pattern predicate at the OLD snapshot = false, want true: an edge that " +
			"existed at this read's instant was removed from under it")
	}
}

// TestPatternComprehension_EdgeAddedAfterSnapshotIsInvisible covers the
// list-producing entry point. It shares the enumeration with the predicate but
// not the entry point, and a fix applied to only one of the two would leave this
// failing.
func TestPatternComprehension_EdgeAddedAfterSnapshotIsInvisible(t *testing.T) {
	t.Parallel()
	g := snapPatternGraph(t)

	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	if got := evalComprehensionAt(t, g, nil); got != 1 {
		t.Fatalf("comprehension at the CURRENT instant produced %d matches, want 1: "+
			"the fixture is wrong", got)
	}
	if got := evalComprehensionAt(t, g, old); got != 0 {
		t.Errorf("pattern comprehension at the OLD snapshot produced %d matches, want 0: "+
			"it enumerated an edge committed after its snapshot started", got)
	}
}

// TestPatternComprehension_EdgeRemovedAfterSnapshotStaysVisible is the removal
// direction for the comprehension.
func TestPatternComprehension_EdgeRemovedAfterSnapshotStaysVisible(t *testing.T) {
	t.Parallel()
	g := snapPatternGraph(t)
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	old := g.BeginRead()
	if old == nil {
		t.Fatal("BeginRead returned nil: MVCC is disarmed, so this test proves nothing")
	}
	defer g.EndRead(old)

	g.RemoveEdge("a", "b")

	if got := evalComprehensionAt(t, g, nil); got != 0 {
		t.Fatalf("comprehension at the CURRENT instant produced %d matches, want 0: "+
			"the fixture is wrong", got)
	}
	if got := evalComprehensionAt(t, g, old); got != 1 {
		t.Errorf("pattern comprehension at the OLD snapshot produced %d matches, want 1: "+
			"an edge that existed at this read's instant was removed from under it", got)
	}
}

// TestPatternEval_NoRawTopologyReadsRemain is a structural guard, and it is the
// one that keeps this fix from silently regressing.
//
// The tests above pin BEHAVIOUR at the two entry points that exist today. A new
// enumeration path added later — another match* helper, a new comprehension
// shape — could reintroduce a raw read without failing any of them, because
// nothing routes through it yet. This asserts the PROPERTY instead: no topology
// read in pattern_eval.go reaches the unversioned adjacency.
//
// Mapper, Directed and Multigraph are deliberately permitted: the mapper is the
// candidate-set class (it answers which objects to consider, not what they
// contain, and each candidate is verified against the snapshot afterwards), and
// the other two are configuration flags that no instant can change.
func TestPatternEval_NoRawTopologyReadsRemain(t *testing.T) {
	t.Parallel()
	src, err := readSourceFile("pattern_eval.go")
	if err != nil {
		t.Fatalf("read pattern_eval.go: %v", err)
	}
	for _, banned := range []string{
		".AdjList().LoadEntryH(",
		".AdjList().LoadEntry(",
		".AdjList().LoadEntryLabels(",
		".AdjList().LoadEntryAux(",
		".AdjList().HasEdge(",
		".AdjList().Neighbours(",
		".AdjList().OutDegree(",
	} {
		if countOccurrences(src, banned) != 0 {
			t.Errorf("pattern_eval.go still reads topology through the UNVERSIONED adjacency "+
				"via %q — that answers from the present, not from this read's instant "+
				"(rmp #2294). Use the ReadView's as-of methods instead.", banned)
		}
	}
}
