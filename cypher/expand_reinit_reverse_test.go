package cypher_test

// expand_reinit_reverse_test.go — the ENGINE-level counterpart to
// TestExpand_ReInitResetsReverseCursor (cypher/exec), pinning the wrong ANSWER
// the operator defect produced rather than the operator state that produced it.
//
// Present at commit 35990293: `MATCH (a:P) WHERE EXISTS { MATCH (a)<-[r]-(x)
// RETURN x } RETURN a.id ORDER BY a.id` over four nodes and two arcs, both into
// b, returned b AND c. Node c has no incoming arc whatsoever.
//
// # Why this shape, and why the untyped form is the one that must be pinned
//
// EXISTS lowers to SemiApply over a correlated Apply, which stops at the first
// match and then re-Inits the Expand for the next outer row — leaving the
// previous source's reverse cursor half-consumed. b's reverse run has TWO slots
// and the search stops after the first, so exactly one slot is left over and is
// attributed to the next node scanned.
//
// The TYPED form of the same query was accidentally CORRECT before rmp #2251, so
// only the untyped form pins the defect on the pre-fix code. The reason is worth
// recording, because it is the whole lesson: the reverse type test used to recover
// each reverse slot's FORWARD position from (dst, src), was handed
// uint64(op.srcID) — 2^64-1, the not-yet-loaded sentinel — as src, could never
// find a forward counterpart for it, and rejected the slot. An accidental cost was
// masking a defect. Removing the cost removed the mask.
//
// Both forms are asserted below so the pairing is visible and the typed form
// cannot silently regress later.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestExpandReInit_ExistsReverseDoesNotLeakPriorSource is the regression gate.
func TestExpandReInit_ExistsReverseDoesNotLeakPriorSource(t *testing.T) {
	ctx := context.Background()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "id", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", k, err)
		}
	}
	// TWO arcs, both INTO b. b's reverse run therefore has two slots, so an EXISTS
	// that stops at the first leaves exactly one behind. a, c and d have none.
	for _, e := range [][2]string{{"a", "b"}, {"c", "b"}} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%v): %v", e, err)
		}
		g.SetEdgeLabel(e[0], e[1], "K")
	}
	eng := cypher.NewEngine(g)

	for _, tc := range []struct{ name, q string }{
		{"untyped", `MATCH (a:P) WHERE EXISTS { MATCH (a)<-[r]-(x) RETURN x } RETURN a.id ORDER BY a.id`},
		{"typed", `MATCH (a:P) WHERE EXISTS { MATCH (a)<-[r:K]-(x) RETURN x } RETURN a.id ORDER BY a.id`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runIDs(ctx, t, eng, tc.q)
			if len(got) != 1 || got[0] != "b" {
				t.Errorf("got %v, want [b] — only b has an incoming relationship.\n"+
					"  query: %s\n"+
					"  An extra node means Expand.Init inherited the previous outer row's "+
					"half-consumed reverse cursor and attributed its leftover slot to the "+
					"next node scanned.", got, tc.q)
			}
		})
	}
}

// runIDs executes q and returns its single string column.
func runIDs(ctx context.Context, t *testing.T, eng *cypher.Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		// Rendered, then unquoted: a StringValue renders with its quotes, and the
		// assertion is about WHICH nodes came back, not how they print.
		out = append(out, strings.Trim(res.ValueAt(0).String(), `"`))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close %q: %v", q, err)
	}
	return out
}
