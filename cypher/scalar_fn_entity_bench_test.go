package cypher_test

// scalar_fn_entity_bench_test.go — empirical evidence for #1659 (audit H1).
//
// RETURN id(r) / type(r) / labels(n) / startNode(r) / endNode(r) / keys(r) are
// *ast.FunctionInvocation over a bare bound variable, so before #1659 they
// bypassed every bare-variable fast projection path and fell to the general
// path, where analyseNodeScalarUse classified the function's bare-variable
// argument as needsWholeNode. That disabled the pooled RowContext and eagerly
// materialised EVERY in-scope variable per row — unrelated bound nodes/edges as
// full NodeValue/RelationshipValue (+ property maps) — only to extract one
// scalar field. The audit measured RETURN id(r) at ~24 400 allocs/op versus
// ~2 120 for RETURN count(r) and even ~11 000 for RETURN r.
//
// #1659 teaches analyseNodeScalarUse to classify the field-extractor functions
// (id/elementId → needsIDOnly, type → needsType, startNode/endNode →
// needsEndpoint, labels → needsLabelList, keys → needsKeyNames; properties()
// keeps full materialisation) and the projection path to materialise each
// variable partially, so unrelated variables are skipped entirely and the
// touched entity is built without its property map.
//
// These benchmarks reuse the relationship-dense multigraph from
// seedRelGraph (rel_projection_bench_test.go): ~nNodes*fanout*2 rows, each edge
// carrying a `w` property and a stable handle. Layer: short.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkScalarFnEntity -benchmem -count=6 ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// seedScalarFnGraph builds a graph of nNodes nodes connected in a ring with
// `fanout` forward edges per node, each edge created via CREATE (so it carries a
// stable handle) and carrying THREE properties (w, k, ts). The three-property
// edge matches the audit H1 measurement shape: it makes the eliminated per-row
// property-map materialisation (which #1659 skips for the field extractors)
// dominate the projection cost, so the before/after allocation delta is
// attributable to the property-map build rather than the traversal. The query
// `MATCH (a)-[r]->(b) RETURN <fn>` returns roughly nNodes*fanout rows.
func seedScalarFnGraph(b *testing.B, eng *cypher.Engine, nNodes, fanout int) {
	b.Helper()
	mk := func(q string) {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("seed drain %q: %v", q, err)
		}
		_ = res.Close()
	}
	for i := 0; i < nNodes; i++ {
		mk(fmt.Sprintf("CREATE (:N {i:%d})", i))
	}
	for i := 0; i < nNodes; i++ {
		for f := 1; f <= fanout; f++ {
			j := (i + f) % nNodes
			mk(fmt.Sprintf("MATCH (a:N {i:%d}),(b:N {i:%d}) CREATE (a)-[:KNOWS {w:%d, k:'edge%d', ts:%d}]->(b)", i, j, f, f, i*100+f))
		}
	}
}

// benchmarkScalarFnEntity seeds a relationship-dense, three-property-edge graph
// once and repeatedly runs query q, fully draining each result. q is an
// entity-introspection projection whose per-row cost #1659 targets. The shape
// (~800 rows, 3-property edges) mirrors the audit H1 measurement.
func benchmarkScalarFnEntity(b *testing.B, q string) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedScalarFnGraph(b, eng, 400, 2) // ~800 relationships, 3 properties each
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatalf("Exec: %v", err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("drain: %v", err)
		}
		_ = res.Close()
	}
}

// BenchmarkScalarFnEntity_IDRel — RETURN id(r): the audit headline. The integer
// already sits in the row; before #1659 it cost more than RETURN r.
func BenchmarkScalarFnEntity_IDRel(b *testing.B) {
	benchmarkScalarFnEntity(b, "MATCH (a)-[r]->(b) RETURN id(r)")
}

// BenchmarkScalarFnEntity_TypeRel — RETURN type(r): reads only r.Type.
func BenchmarkScalarFnEntity_TypeRel(b *testing.B) {
	benchmarkScalarFnEntity(b, "MATCH (a)-[r]->(b) RETURN type(r)")
}

// BenchmarkScalarFnEntity_LabelsNode — RETURN labels(a): reads only a.Labels.
func BenchmarkScalarFnEntity_LabelsNode(b *testing.B) {
	benchmarkScalarFnEntity(b, "MATCH (a)-[r]->(b) RETURN labels(a)")
}

// BenchmarkScalarFnEntity_StartNodeRel — RETURN startNode(r): reads only
// r.StartID and returns a bare node reference.
func BenchmarkScalarFnEntity_StartNodeRel(b *testing.B) {
	benchmarkScalarFnEntity(b, "MATCH (a)-[r]->(b) RETURN startNode(r)")
}

// BenchmarkScalarFnEntity_KeysRel — RETURN keys(r): reads only the property key
// set, never the values.
func BenchmarkScalarFnEntity_KeysRel(b *testing.B) {
	benchmarkScalarFnEntity(b, "MATCH (a)-[r]->(b) RETURN keys(r)")
}

// BenchmarkScalarFnEntity_PropertiesRel — RETURN properties(r): the control. It
// returns the full property map, so #1659 deliberately keeps it on the full
// materialisation path; its allocations should NOT collapse like the others.
func BenchmarkScalarFnEntity_PropertiesRel(b *testing.B) {
	benchmarkScalarFnEntity(b, "MATCH (a)-[r]->(b) RETURN properties(r)")
}

// BenchmarkScalarFnEntity_WholeRel — RETURN r: the whole-relationship path M2
// (#1662) targets. The relationship escapes whole, so buildEdgeProps takes the
// full-coalesced-map branch #1659 did NOT optimise. Before M2 that branch built
// a transient lpg map[string]PropertyValue per row and then copied every entry
// into a second expr.MapValue; M2 streams the values straight into the expr map,
// removing the first map. On the 3-property edges the per-row property-map build
// dominates, so this benchmark attributes the allocation delta to the eliminated
// intermediate map.
func BenchmarkScalarFnEntity_WholeRel(b *testing.B) {
	benchmarkScalarFnEntity(b, "MATCH (a)-[r]->(b) RETURN r")
}
