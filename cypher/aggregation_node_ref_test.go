package cypher_test

// aggregation_node_ref_test.go — an aggregation grouped by, or counting
// DISTINCT, a bare node variable must not materialise the whole node once per
// input row, and must return exactly what the eager materialisation returned
// (rmp #2732).
//
// Layer: short.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	nodeRefNodes     = 400
	nodeRefOutDegree = 4
	nodeRefStride    = 97 // spreads each node's targets, so in-degree runs are not consecutive
)

// nodeRefGraph builds nodeRefNodes :P nodes with four properties each and
// nodeRefOutDegree outgoing :K relationships per node, each carrying one
// property. Every node therefore has in-degree and out-degree nodeRefOutDegree.
func nodeRefGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	t.Cleanup(func() { _ = g.Close() })
	for i := 0; i < nodeRefNodes; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		for _, kv := range []struct {
			k string
			v lpg.PropertyValue
		}{
			{"name", lpg.StringValue(fmt.Sprintf("name-%d", i))},
			{"email", lpg.StringValue(fmt.Sprintf("p%d@example.org", i))},
			{"age", lpg.Int64Value(int64(i % 70))},
			{"rank", lpg.Int64Value(int64(i))},
		} {
			if err := g.SetNodeProperty(k, kv.k, kv.v); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
		}
	}
	for i := 0; i < nodeRefNodes; i++ {
		for d := 1; d <= nodeRefOutDegree; d++ {
			src, dst := fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", (i+d*nodeRefStride)%nodeRefNodes)
			if err := g.AddEdgeLabeledWithProperty(src, dst, 1, "K", "w", lpg.Int64Value(int64(d))); err != nil {
				t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
			}
		}
	}
	return g
}

func nodeRefRows(t *testing.T, eng *cypher.Engine, q string) []map[string]expr.Value {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []map[string]expr.Value
	for res.Next() {
		rec := res.Record()
		row := make(map[string]expr.Value, len(rec))
		for k, v := range rec {
			ev, ok := v.(expr.Value)
			if !ok {
				t.Fatalf("%q: column %s holds %T, want an expr.Value", q, k, v)
			}
			row[k] = ev
		}
		out = append(out, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err %q: %v", q, err)
	}
	return out
}

// assertWholeNode fails unless v is a NodeValue carrying the :P label and all
// four properties of the node named by its rank — the full materialisation the
// eager path produced.
func assertWholeNode(t *testing.T, v expr.Value) {
	t.Helper()
	nv, ok := v.(expr.NodeValue)
	if !ok {
		t.Fatalf("grouping key: want expr.NodeValue, got %T (%v)", v, v)
	}
	if len(nv.Labels) != 1 || nv.Labels[0] != "P" {
		t.Fatalf("node %d labels = %v, want [P]", nv.ID, nv.Labels)
	}
	rank, ok := nv.Properties["rank"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("node %d: rank = %v, want an integer", nv.ID, nv.Properties["rank"])
	}
	want := expr.MapValue{
		"name":  expr.StringValue(fmt.Sprintf("name-%d", rank)),
		"email": expr.StringValue(fmt.Sprintf("p%d@example.org", rank)),
		"age":   expr.IntegerValue(int64(rank) % 70),
		"rank":  rank,
	}
	if len(nv.Properties) != len(want) {
		t.Fatalf("node %d properties = %v, want %v", nv.ID, nv.Properties, want)
	}
	for k, w := range want {
		if got := nv.Properties[k]; !expr.Equivalent(got, w) {
			t.Fatalf("node %d property %s = %v, want %v", nv.ID, k, got, w)
		}
	}
}

// TestAggregationNodeKey_ReturnsWholeNodes pins result identity: a node grouping
// key that is emitted downstream is still the WHOLE node, with the right count,
// whether the input rows of a group arrive consecutively (grouped by the
// expansion source) or interleaved (grouped by the expansion target).
func TestAggregationNodeKey_ReturnsWholeNodes(t *testing.T) {
	eng := cypher.NewEngine(nodeRefGraph(t))
	for _, q := range []string{
		`MATCH (u:P)-[:K]->(:P) WITH u, count(*) AS deg RETURN u, deg`,
		`MATCH (:P)-[:K]->(v:P) WITH v AS u, count(*) AS deg RETURN u, deg`,
		`MATCH (u:P)-[:K]->(:P) RETURN u, count(*) AS deg`,
	} {
		rows := nodeRefRows(t, eng, q)
		if len(rows) != nodeRefNodes {
			t.Fatalf("%q: %d groups, want %d", q, len(rows), nodeRefNodes)
		}
		seen := make(map[uint64]bool, nodeRefNodes)
		for _, r := range rows {
			assertWholeNode(t, r["u"])
			id := r["u"].(expr.NodeValue).ID
			if seen[id] {
				t.Fatalf("%q: node %d grouped twice", q, id)
			}
			seen[id] = true
			if !expr.Equivalent(r["deg"], expr.IntegerValue(nodeRefOutDegree)) {
				t.Fatalf("%q: deg = %v, want %d", q, r["deg"], nodeRefOutDegree)
			}
		}
	}
	// A property read through the key after the aggregation sees the real value.
	rows := nodeRefRows(t, eng, `MATCH (u:P)-[:K]->(:P) WITH u, count(*) AS deg WHERE u.rank < 3 RETURN u.name AS n, deg ORDER BY n`)
	if len(rows) != 3 {
		t.Fatalf("filtered groups = %d, want 3", len(rows))
	}
	for i, r := range rows {
		if want := expr.StringValue(fmt.Sprintf("name-%d", i)); !expr.Equivalent(r["n"], want) {
			t.Fatalf("row %d: n = %v, want %v", i, r["n"], want)
		}
	}
}

// TestAggregationNodeRef_NonNodeBindingsUnchanged pins the guard that limits the
// raw-id binding to a PLAIN node: an integer column whose values coincide with
// real node ids stays an integer, and a relationship is still built whole.
func TestAggregationNodeRef_NonNodeBindingsUnchanged(t *testing.T) {
	eng := cypher.NewEngine(nodeRefGraph(t))

	rows := nodeRefRows(t, eng, `UNWIND [0, 1, 1, 2, 2, 2] AS x WITH x, count(*) AS c RETURN x, c ORDER BY x`)
	if len(rows) != 3 {
		t.Fatalf("integer groups = %d, want 3", len(rows))
	}
	for i, r := range rows {
		if _, isInt := r["x"].(expr.IntegerValue); !isInt {
			t.Fatalf("row %d: x = %T (%v), want expr.IntegerValue", i, r["x"], r["x"])
		}
		if !expr.Equivalent(r["c"], expr.IntegerValue(int64(i+1))) {
			t.Fatalf("row %d: c = %v, want %d", i, r["c"], i+1)
		}
	}
	rows = nodeRefRows(t, eng, `UNWIND [0, 1, 1, 2, 2, 2] AS x RETURN count(DISTINCT x) AS c`)
	if len(rows) != 1 || !expr.Equivalent(rows[0]["c"], expr.IntegerValue(3)) {
		t.Fatalf("count(DISTINCT x) over integers = %v, want 3", rows)
	}

	const edges = nodeRefNodes * nodeRefOutDegree
	rows = nodeRefRows(t, eng, `MATCH (:P)-[r:K]->(:P) WITH r, count(*) AS c RETURN r, c`)
	if len(rows) != edges {
		t.Fatalf("relationship groups = %d, want %d", len(rows), edges)
	}
	for _, r := range rows {
		rv, ok := r["r"].(expr.RelationshipValue)
		if !ok {
			t.Fatalf("r = %T, want expr.RelationshipValue", r["r"])
		}
		if rv.Type != "K" || rv.Properties["w"] == nil {
			t.Fatalf("relationship %d not whole: type %q, properties %v", rv.ID, rv.Type, rv.Properties)
		}
	}
	rows = nodeRefRows(t, eng, `MATCH (:P)-[r:K]->(:P) RETURN count(DISTINCT r) AS c`)
	if len(rows) != 1 || !expr.Equivalent(rows[0]["c"], expr.IntegerValue(edges)) {
		t.Fatalf("count(DISTINCT r) = %v, want %d", rows, edges)
	}
}

// TestAggregationCountDistinctNode_Result pins count(DISTINCT n) over a node
// reached many times: every node is the target of nodeRefOutDegree
// relationships, so the distinct count is the node count, and a grouped form
// counts each source's distinct targets.
func TestAggregationCountDistinctNode_Result(t *testing.T) {
	eng := cypher.NewEngine(nodeRefGraph(t))
	rows := nodeRefRows(t, eng, `MATCH (:P)-[:K]->(b:P) RETURN count(DISTINCT b) AS c, count(b) AS total`)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !expr.Equivalent(rows[0]["c"], expr.IntegerValue(nodeRefNodes)) {
		t.Fatalf("count(DISTINCT b) = %v, want %d", rows[0]["c"], nodeRefNodes)
	}
	if !expr.Equivalent(rows[0]["total"], expr.IntegerValue(nodeRefNodes*nodeRefOutDegree)) {
		t.Fatalf("count(b) = %v, want %d", rows[0]["total"], nodeRefNodes*nodeRefOutDegree)
	}
	rows = nodeRefRows(t, eng, `MATCH (a:P)-[:K]->(:P)-[:K]->(b:P) RETURN a.rank AS r, count(DISTINCT b) AS c`)
	if len(rows) != nodeRefNodes {
		t.Fatalf("grouped rows = %d, want %d", len(rows), nodeRefNodes)
	}
	for _, r := range rows {
		rank := int(r["r"].(expr.IntegerValue))
		want := make(map[int]bool)
		for d1 := 1; d1 <= nodeRefOutDegree; d1++ {
			mid := (rank + d1*nodeRefStride) % nodeRefNodes
			for d2 := 1; d2 <= nodeRefOutDegree; d2++ {
				want[(mid+d2*nodeRefStride)%nodeRefNodes] = true
			}
		}
		if !expr.Equivalent(r["c"], expr.IntegerValue(int64(len(want)))) {
			t.Fatalf("a.rank=%d: count(DISTINCT b) = %v, want %d", rank, r["c"], len(want))
		}
	}
}

// TestAggregationNodeRef_AllocationBound pins the WORK the change removes. Both
// rewrites are result-identical, so only allocation can tell them from the eager
// path. Bounds and their measurements are in the table.
func TestAggregationNodeRef_AllocationBound(t *testing.T) {
	eng := cypher.NewEngine(nodeRefGraph(t))
	ctx := context.Background()
	const rows = nodeRefNodes * nodeRefOutDegree
	for _, tc := range []struct {
		q        string
		maxPerRw float64
	}{
		// Measured on this fixture (darwin/arm64), allocations per input row, the
		// eager arm by disabling both rewrites: eager 11.34, one materialisation
		// per run of equal keys 4.91. The bound sits between them.
		{`MATCH (u:P)-[:K]->(:P) WITH u, count(*) AS deg RETURN max(deg) AS m`, 8},
		// Eager 10.82, raw id 2.53.
		{`MATCH (:P)-[:K]->(b:P) RETURN count(DISTINCT b) AS c`, 6},
	} {
		run := func() {
			res, err := eng.Run(ctx, tc.q, nil)
			if err != nil {
				t.Fatalf("Run %q: %v", tc.q, err)
			}
			for res.Next() {
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Err %q: %v", tc.q, err)
			}
			_ = res.Close()
		}
		run()
		perRow := testing.AllocsPerRun(5, run) / rows
		if perRow > tc.maxPerRw {
			t.Errorf("%q: %.2f allocs per input row, want <= %.0f (whole-node materialisation per row is back)", tc.q, perRow, tc.maxPerRw)
		}
	}
}
