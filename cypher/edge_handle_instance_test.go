package cypher

// edge_handle_instance_test.go — the regression gate for rmp #3088.
//
// Since rmp #2317 the relationship id a row carries is the edge's stable HANDLE,
// not a forward-CSR position. The projection's per-ordinal type fallback
// (the former edgeInstanceIdxFor) still read that id as a CSR position, counted
// the dst-matching slots up to it, and took the per-CREATE ordinal store's type
// at that count. A Go-API edge — which has no by-handle type record and so
// reaches that fallback — sharing its pair with Cypher-created parallel edges
// was therefore typed as whichever Cypher sibling the handle-as-position
// arithmetic happened to land on.
//
// Every expectation below is hand-computed and absolute: Cypher-created edges
// report their own CREATE's type and properties (by handle), and the Go-API edge
// reports the pair's derived type union resolved by pickEdgeType's documented
// rule (alphabetically smallest when the pattern names no type), which is "G"
// for the union {G, T, U}.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mixedPairFixture builds, out of node "a":
//
//	a -[:X {w:1}]-> c   Cypher
//	a -[:G]->       b   Go API (AddEdge + SetEdgeLabel), no by-handle record
//	a -[:T {w:2}]-> b   Cypher
//	a -[:U {w:3}]-> b   Cypher
//	a -[:Y {w:4}]-> c   Cypher
//	a -[:Z {w:5}]-> c   Cypher
//
// so the Go-API slot shares its pair with two Cypher instances and several
// handles fall inside a's forward position range.
func mixedPairFixture(t *testing.T) (*lpg.Graph[string, float64], *Engine) {
	t.Helper()
	g, eng := perSlotFixture(t)
	create := func(dst int, typ string, w int) {
		mustRun(t, eng, fmt.Sprintf(
			"MATCH (x:P {id: 0}), (y:P {id: %d}) CREATE (x)-[:%s {w: %d}]->(y)", dst, typ, w))
	}
	create(2, "X", 1)
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetEdgeLabel("a", "b", "G"); err != nil {
		t.Fatalf("SetEdgeLabel: %v", err)
	}
	create(1, "T", 2)
	create(1, "U", 3)
	create(2, "Y", 4)
	create(2, "Z", 5)
	return g, eng
}

// relRows returns "type|w" for every relationship out of "a", sorted.
func relRows(t *testing.T, eng *Engine) []string {
	t.Helper()
	rows := runRows(t, eng, "MATCH (x:P {id: 0})-[r]->(y) RETURN type(r), r.w")
	sort.Strings(rows)
	return rows
}

func TestEdgeHandleInstance_GoAPIEdgeNotTypedAsCypherSibling(t *testing.T) {
	g, eng := mixedPairFixture(t)

	// The Go-API edge's w is the pair's coalesced per-pair value: every Cypher
	// CREATE on (a, b) fans its SetEdgeProperty out to every a->b slot, so the
	// latest one (U's w: 3) is what the Go-API slot carries.
	want := []string{`"G"|3`, `"T"|2`, `"U"|3`, `"X"|1`, `"Y"|4`, `"Z"|5`}
	if got := relRows(t, eng); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("before delete: got %v, want %v", got, want)
	}

	// properties(r) per relationship: by handle for the Cypher edges, the
	// per-pair coalesced map for the Go-API edge.
	props := runRows(t, eng, "MATCH (x:P {id: 0})-[r]->(y) RETURN type(r), properties(r)")
	sort.Strings(props)
	wantProps := []string{`"G"|{w: 3}`, `"T"|{w: 2}`, `"U"|{w: 3}`, `"X"|{w: 1}`, `"Y"|{w: 4}`, `"Z"|{w: 5}`}
	if strings.Join(props, ",") != strings.Join(wantProps, ",") {
		t.Fatalf("properties: got %v, want %v", props, wantProps)
	}

	// Delete the first slot so every later slot's position shifts by one and
	// handles and positions diverge further.
	mustRun(t, eng, "MATCH (:P {id: 0})-[r:X]->() DELETE r")
	want = []string{`"G"|3`, `"T"|2`, `"U"|3`, `"Y"|4`, `"Z"|5`}
	if got := relRows(t, eng); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after deleting X: got %v, want %v", got, want)
	}

	// Delete a Cypher sibling on the Go-API edge's own pair: the union is now
	// {G, U}, still resolving to G.
	mustRun(t, eng, "MATCH (:P {id: 0})-[r:T]->() DELETE r")
	want = []string{`"G"|3`, `"U"|3`, `"Y"|4`, `"Z"|5`}
	if got := relRows(t, eng); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after deleting T: got %v, want %v", got, want)
	}

	// Per-edge lookup by id: each relationship's id resolves to its own type.
	for _, row := range runRows(t, eng, "MATCH (x:P {id: 0})-[r]->(y) RETURN id(r), type(r)") {
		parts := strings.SplitN(row, "|", 2)
		got := runRows(t, eng, "MATCH ()-[r]->() WHERE id(r) = "+parts[0]+" RETURN type(r)")
		if len(got) != 1 || got[0] != parts[1] {
			t.Fatalf("id(r) = %s: got %v, want [%s]", parts[0], got, parts[1])
		}
	}
	_ = g
}
