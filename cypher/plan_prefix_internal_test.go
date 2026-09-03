package cypher

// plan_prefix_internal_test.go — the depth derivation behind the captured
// LOGICAL plan tree (rmp #2721).
//
// [Engine.logicalPlanNode] rebuilds a tree from the flat line stream the shared
// logical-plan walk emits, using [planLine.depth], which is DERIVED from the
// line's prefix and connector rather than carried as a field. That keeps the
// walk — which the tree and table renderers both drive — untouched, at the cost
// of depending on how the walk builds its prefixes.
//
// This test is what makes that dependency safe: it pins the reconstruction
// against the walk's OWN rendering, line for line. A change to the walk's
// connectors or continuations breaks it here rather than silently mis-nesting a
// plan on the wire.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestPlanLineDepth_ReconstructsTheWalksOwnNesting asserts that rendering the
// reconstructed tree reproduces the logical renderer's indentation exactly.
//
// The logical renderer writes prefix+connector+text+annot; the tree renderer
// writes prefix+connector+name. So for every line, the logical rendering must
// START with the reconstructed one — same nesting, same operator text, with only
// the cardinality annotation left over. Equal line counts rule out a dropped or
// duplicated node.
func TestPlanLineDepth_ReconstructsTheWalksOwnNesting(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	for _, k := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "age", lpg.Int64Value(30)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("a", "b", "KNOWS")
	eng := NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	queries := []string{
		// A deep single-child chain: every level must nest one deeper.
		"MATCH (n:P) WHERE n.age > 20 RETURN n.age AS a ORDER BY a SKIP 1 LIMIT 2",
		// A binary shape: a Cartesian product has two children, which is what
		// exercises the "├─"/"└─" connectors and the "│  " continuation.
		"MATCH (x:P), (y:P) RETURN x, y",
		// Aggregation over an expand.
		"MATCH (x:P)-[:KNOWS]->(y:P) RETURN count(y) AS c",
		// A writing statement — the shape the EXPLAIN prefix actually captures.
		"MATCH (n:P) SET n.age = n.age + 1 RETURN n.age AS a",
		// A trivial single-node plan: the root must still be the root.
		"RETURN 1 AS one",
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			entry, _, err := eng.parseAndAnalyse(q)
			if err != nil {
				t.Fatalf("parseAndAnalyse: %v", err)
			}
			wantLines := strings.Split(strings.TrimRight(eng.explainLogical(entry, nil), "\n"), "\n")
			node := eng.logicalPlanNode(entry, nil)
			gotLines := strings.Split(exec.RenderPlanNode(&node), "\n")

			if len(gotLines) != len(wantLines) {
				t.Fatalf("reconstructed %d lines, the walk emitted %d\ngot:\n%s\nwant:\n%s",
					len(gotLines), len(wantLines),
					strings.Join(gotLines, "\n"), strings.Join(wantLines, "\n"))
			}
			for i := range gotLines {
				if !strings.HasPrefix(wantLines[i], gotLines[i]) {
					t.Errorf("line %d\n reconstructed: %q\n walk emitted:  %q",
						i, gotLines[i], wantLines[i])
				}
			}
			if len(gotLines) < 2 && q != "RETURN 1 AS one" {
				t.Errorf("plan collapsed to %d line(s); the comparison is vacuous", len(gotLines))
			}
		})
	}
}

// TestPlanLineDepth_Formula pins the derivation itself against hand-built lines,
// so a reader can see what the rule is without running a plan through it.
func TestPlanLineDepth_Formula(t *testing.T) {
	t.Parallel()
	cases := []struct {
		prefix, connector string
		want              int
	}{
		{"", "", 0},          // root
		{"", "└─ ", 1},       // only child of the root
		{"", "├─ ", 1},       // first of two children of the root
		{"   ", "└─ ", 2},    // grandchild under a last child
		{"│  ", "├─ ", 2},    // grandchild under a non-last child
		{"│     ", "└─ ", 3}, // two levels of continuation
		{"      ", "└─ ", 3},
	}
	for _, c := range cases {
		l := planLine{prefix: c.prefix, connector: c.connector}
		if got := l.depth(); got != c.want {
			t.Errorf("depth(prefix=%q, connector=%q) = %d, want %d",
				c.prefix, c.connector, got, c.want)
		}
	}
}
