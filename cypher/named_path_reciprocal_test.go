package cypher_test

// named_path_reciprocal_test.go — rmp #2874 and #2875 regressions: a named path
// over a RECIPROCAL pair rendered the wrong member of the pair.
//
// The single-relationship route was fixed in rmp #2504, but a named path is
// rendered by three other sites, and none of them consulted the bound edge's
// handle to recover its stored orientation:
//
//   - buildPathValueFromChainInfo (a path read through an expression such as
//     `relationships(p)`) and buildPathValueFromVLEMeta (a fixed hop leading a
//     variable-length hop) asked the topology alone, which cannot tell the two
//     edges of a reciprocal pair apart and answered "stored forward";
//   - the projection's `RETURN p` fast path read the handle as a forward-CSR
//     POSITION (rmp #2875), which it stopped being in rmp #2317, and so derived
//     the orientation from an unrelated edge.
//
// Edges created through Cypher record their type by handle, and the hop
// materialiser's by-handle swap corrected all three silently, which is why the
// Cypher-built #2504 fixture never exposed them. The fixture here is built
// through the Go API, which stamps a handle on the slot but writes the per-pair
// store only, so nothing downstream can repair a wrong orientation.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newGoAPIReciprocalEngine builds Persons A, B, C and three KNOWS edges through
// the Go API, in this order:
//
//	A->B {tag:'AB'}  (handle 1)
//	B->A {tag:'BA'}  (handle 2)
//	B->C {tag:'BC'}  (handle 3)
//
// A->B / B->A are the reciprocal pair; B->C is the non-reciprocal control and
// the tail a variable-length hop continues along.
func newGoAPIReciprocalEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for _, k := range []string{"A", "B", "C"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", k, err)
		}
	}
	for i, e := range [][2]string{{"A", "B"}, {"B", "A"}, {"B", "C"}} {
		h, err := g.AddEdgeH(e[0], e[1], 1)
		if err != nil {
			t.Fatalf("AddEdgeH(%s,%s): %v", e[0], e[1], err)
		}
		if h != uint64(i+1) {
			t.Fatalf("AddEdgeH(%s,%s) handle = %d, want %d", e[0], e[1], h, i+1)
		}
		if err := g.SetEdgeLabel(e[0], e[1], "KNOWS"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetEdgeProperty(e[0], e[1], "tag", lpg.StringValue(e[0]+e[1])); err != nil {
			t.Fatalf("SetEdgeProperty(%s,%s): %v", e[0], e[1], err)
		}
	}
	return cypher.NewEngine(g)
}

// renderPath renders every relationship of pv as "id tag start end", naming the
// endpoints through the path's own nodes, joined by " | ". It reads the
// relationship's StartID/EndID rather than the pattern variables, so a path
// that renders the wrong member of a pair cannot pass.
func renderPath(t *testing.T, pv expr.PathValue) string {
	t.Helper()
	names := make(map[uint64]string, len(pv.Nodes))
	for _, n := range pv.Nodes {
		sv, ok := n.Properties["name"].(expr.StringValue)
		if !ok {
			t.Fatalf("path node %d carries no string name: %v", n.ID, n.Properties)
		}
		names[n.ID] = string(sv)
	}
	parts := make([]string, 0, len(pv.Relationships))
	for _, r := range pv.Relationships {
		tag, _ := r.Properties["tag"].(expr.StringValue)
		sn, sOK := names[r.StartID]
		en, eOK := names[r.EndID]
		if !sOK || !eOK {
			t.Fatalf("relationship %d endpoints (%d,%d) are not nodes of its path", r.ID, r.StartID, r.EndID)
		}
		parts = append(parts, fmt.Sprintf("%d %s %s %s", r.ID, string(tag), sn, en))
	}
	return strings.Join(parts, " | ")
}

// collectPaths runs query, renders column p of every row with [renderPath] and
// returns the renderings sorted, so the comparison is independent of row order.
func collectPaths(t *testing.T, eng *cypher.Engine, query string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var out []string
	for res.Next() {
		pv, ok := res.Record()["p"].(expr.PathValue)
		if !ok {
			t.Fatalf("query %q: column p = %T, want expr.PathValue", query, res.Record()["p"])
		}
		out = append(out, renderPath(t, pv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	sort.Strings(out)
	return out
}

// Each pattern below is paired with the relationships its rows must render, in
// "id tag start end" form. The reverse and undirected hops are the defect's
// surface; the forward hop is the control for the expression routes and, on
// the RETURN p route, a failing case of its own (rmp #2875: handle 1 read as
// CSR position 1 is the edge B->A).
//
// varlen marks the cases whose path ends in a variable-length hop. An
// expression reading such a path straight after its MATCH currently yields null
// (a separate defect), so the expression test reads it across a `WITH p`
// barrier, which still reconstructs it through buildPathValueFromVLEMeta.
var namedPathReciprocalCases = []struct {
	name, match string
	want        []string
	varlen      bool
}{
	{
		"forward",
		`MATCH p=(a:Person {name:'A'})-[:KNOWS]->(b:Person {name:'B'})`,
		[]string{"1 AB A B"},
		false,
	},
	{
		"reverse",
		`MATCH p=(a:Person {name:'A'})<-[:KNOWS]-(b:Person {name:'B'})`,
		[]string{"2 BA B A"},
		false,
	},
	{
		"undirected",
		`MATCH p=(a:Person {name:'A'})-[:KNOWS]-(b:Person {name:'B'})`,
		[]string{"1 AB A B", "2 BA B A"},
		false,
	},
	{
		"leading-forward-then-varlen",
		`MATCH p=(a:Person {name:'A'})-[:KNOWS]->(b:Person {name:'B'})-[:KNOWS*1..1]->(c:Person {name:'C'})`,
		[]string{"1 AB A B | 3 BC B C"},
		true,
	},
	{
		"leading-reverse-then-varlen",
		`MATCH p=(a:Person {name:'A'})<-[:KNOWS]-(b:Person {name:'B'})-[:KNOWS*1..1]->(c:Person {name:'C'})`,
		[]string{"2 BA B A | 3 BC B C"},
		true,
	},
	{
		"leading-undirected-then-varlen",
		`MATCH p=(a:Person {name:'A'})-[:KNOWS]-(b:Person {name:'B'})-[:KNOWS*1..1]->(c:Person {name:'C'})`,
		[]string{"1 AB A B | 3 BC B C", "2 BA B A | 3 BC B C"},
		true,
	},
}

// TestNamedPathReciprocal_ReturnPath pins the projection's `RETURN p` routes:
// the fixed-length chain fast path (rmp #2875) and the variable-length path
// builder's leading hop (rmp #2874).
func TestNamedPathReciprocal_ReturnPath(t *testing.T) {
	eng := newGoAPIReciprocalEngine(t)
	for _, tc := range namedPathReciprocalCases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectPaths(t, eng, tc.match+` RETURN p`)
			assertRows(t, tc.name, got, tc.want)
		})
	}
}

// TestNamedPathReciprocal_PathExpression pins the route a path takes when an
// expression reads it (`relationships(p)` in a projection), which reconstructs
// it through buildPathValueFromChainInfo or buildPathValueFromVLEMeta (rmp
// #2874). The rendering is read from the relationships themselves, not from
// the pattern variables.
func TestNamedPathReciprocal_PathExpression(t *testing.T) {
	eng := newGoAPIReciprocalEngine(t)
	for _, tc := range namedPathReciprocalCases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.match
			if tc.varlen {
				q += ` WITH p`
			}
			q += ` UNWIND relationships(p) AS r ` +
				`RETURN id(r) AS rid, r.tag AS tag, startNode(r).name AS sn, endNode(r).name AS en`
			got := collectRowStrings(t, eng, q, "rid", "tag", "sn", "en")
			sort.Strings(got)
			var want []string
			for _, w := range tc.want {
				want = append(want, strings.Split(w, " | ")...)
			}
			sort.Strings(want)
			assertRows(t, tc.name, got, want)
		})
	}
}
