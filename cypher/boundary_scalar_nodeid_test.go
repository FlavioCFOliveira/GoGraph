package cypher_test

// boundary_scalar_nodeid_test.go — a scalar that crosses a WITH or an
// aggregation must never be read back as the node whose NodeID it equals.
//
// A row reader that holds no scalar fact for a variable upgrades an integer
// cell to the node with that NodeID, when one exists. The projection and the
// aggregation registered that fact for the columns they write, except for an
// output name that shadows an input variable (`count(*) AS r` over a bound r,
// `1 + 1 AS x` over a bound x) and for a variable item that renames a scalar
// (`WITH y AS x`). A computed grouping key was registered only for the
// projection fast path, so an expression reading it (`RETURN k + 0`) upgraded
// it too.
//
// A NodeID packs the mapper shard of the node key into its low bits. Node keys
// come from a process-wide counter, so whether a small integer is a live
// NodeID depended on how many nodes the process had created before. That made
// the same query over the same data answer `1` or `(node#1)` from one run to
// the next: TestRebindingBoundary_*RebindsTheName, TestProjectionAlias_ShadowsBoundName
// and TestRowBindPlan_NameCollisionShapes failed in 1 to 7 % of `-count`
// iterations. This test occupies the mapper shards until every integer it
// asserts on is a live NodeID, so the old behaviour fails on every run.
//
// Layer: short. goleak-clean (engines and graphs are local).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// bsLiveIDs are the integers the queries below produce; each must be a live
// NodeID for the test to observe anything.
var bsLiveIDs = []graph.NodeID{1, 2, 3, 5, 9}

// bsEngine is (:A {n: 1})-[:R {w: 1}]->(:B {n: 2}), (:C), plus enough :Pad
// nodes that every NodeID in bsLiveIDs resolves to a node.
func bsEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ipExec(t, eng, `CREATE (:A {n: 1})-[:R {w: 1}]->(:B {n: 2}), (:C)`)
	m := g.AdjList().Mapper()
	live := func() bool {
		for _, id := range bsLiveIDs {
			if _, ok := m.Resolve(id); !ok {
				return false
			}
		}
		return true
	}
	for batch := 0; !live(); batch++ {
		if batch == 64 {
			t.Fatalf("after %d batches of 1024 nodes, some NodeID of %v is still not live", batch, bsLiveIDs)
		}
		ipExec(t, eng, `UNWIND range(1, 1024) AS i CREATE (:Pad)`)
	}
	return eng
}

func bsRows(t *testing.T, eng *cypher.Engine, q string) string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	var out []string
	for res.Next() {
		out = append(out, fmt.Sprintf("%v", res.Record()["v"]))
	}
	rerr := res.Err()
	if cerr := res.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		t.Fatalf("Run(%q): %v", q, rerr)
	}
	sort.Strings(out)
	return strings.Join(out, ";")
}

func TestBoundaryScalar_NotDecodedAsNode(t *testing.T) {
	t.Parallel()
	eng := bsEngine(t)
	for _, tc := range []struct{ query, want string }{
		// An aggregate output that shadows an input variable.
		{`MATCH (a:A)-[x]->(b) WITH count(*) AS x RETURN x AS v`, `1`},
		{`MATCH x = (a:A)-->(b) WITH count(*) AS x RETURN x AS v`, `1`},
		{`MATCH (x:A) WITH sum(x.n) AS x RETURN x + 0 AS v`, `1`},
		{`MATCH (x:A) WITH x, count(*) AS c WITH c AS x RETURN x AS v`, `1`},
		// A computed projection item that shadows an input variable.
		{`MATCH (x:A) WITH 1 + 1 AS x RETURN x AS v`, `2`},
		{`MATCH (x:A) WITH x.n AS x RETURN x AS v`, `1`},
		{`MATCH (x:A) WITH x.n + 2 AS x WHERE x = 3 RETURN x AS v`, `3`},
		{`MATCH (x:A) WITH x.n + 2 AS x ORDER BY x DESC RETURN x AS v`, `3`},
		{`MATCH (x:A) WITH x.n + 2 AS x MATCH (m:B) RETURN x AS v`, `3`},
		{`MATCH (x:A) WITH x.n + 2 AS x UNWIND [x] AS v RETURN v`, `3`},
		{`MATCH (x:A) WITH 5 AS x WITH x RETURN x AS v`, `5`},
		{`MATCH (a:A)-[x]->(b) WITH b, 9 AS x RETURN x AS v`, `9`},
		// A variable item that renames a scalar.
		{`WITH 3 AS y WITH y AS x RETURN x AS v`, `3`},
		{`UNWIND [3] AS y WITH y AS x RETURN x AS v`, `3`},
		{`MATCH (x:A) WITH x, 3 AS y WITH y AS x RETURN x AS v`, `3`},
		{`MATCH (n:A) WITH n.n + 2 AS k, n WITH k AS x RETURN x AS v`, `3`},
		{`UNWIND [3] AS y WITH y AS x, count(*) AS c RETURN x AS v`, `3`},
		// A computed grouping key read through an expression.
		{`MATCH (a:A) WITH a.n + 2 AS k, count(*) AS c RETURN k + 0 AS v`, `3`},
		{`MATCH (a:A) WITH a.n + 2 AS k, count(*) AS c RETURN [k] AS v`, `[3]`},
		// A node forwarded under a name that was a scalar stays a node.
		{`UNWIND [1] AS x MATCH (n:A) WITH n AS x RETURN x.n AS v`, `1`},
		{`UNWIND [1] AS x MATCH (n:A) WITH n AS x RETURN labels(x) AS v`, `["A"]`},
		{`MATCH (n:A) WITH count(*) AS c, 1 AS x MATCH (m:B) WITH m AS x RETURN x.n AS v`, `2`},
	} {
		if got := bsRows(t, eng, tc.query); got != tc.want {
			t.Errorf("%s\n  got  [%s]\n  want [%s]", tc.query, got, tc.want)
		}
	}
}

// TestBoundaryScalar_WrittenAsScalar pins the write side: a shadowing alias
// stored as a property is the integer, not a node, which a property cannot hold.
func TestBoundaryScalar_WrittenAsScalar(t *testing.T) {
	t.Parallel()
	eng := bsEngine(t)
	ipExec(t, eng, `MATCH (x:A) WITH x.n + 2 AS x CREATE (:T {p: x})`)
	ipExec(t, eng, `MATCH (x:A) WITH x.n AS x MERGE (:U {p: x})`)
	for _, tc := range []struct{ query, want string }{
		{`MATCH (t:T) RETURN t.p AS v`, `3`},
		{`MATCH (u:U) RETURN u.p AS v`, `1`},
	} {
		if got := bsRows(t, eng, tc.query); got != tc.want {
			t.Errorf("%s\n  got  [%s]\n  want [%s]", tc.query, got, tc.want)
		}
	}
}
