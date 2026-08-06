package exec_test

// expand_reverse_cost_bench_test.go — the #2089a reverse-expand cost
// measurement (docs/reordering-design.md §5). It empirically settles the
// question the P-anchor cost model (§2) rests on: is a DirIn expand
// Θ(indegree) — so that re-rooting a single-edge pattern onto its target and
// traversing the relationship in reverse is faithfully costed by
// c_e^IN·D(label,relType,IN) — or does it carry a per-edge cost the aggregate
// count-store statistics cannot see?
//
// The Expand operator is driven over CSR snapshots whose NodeID is the array
// index (the staticCSR helper of expand_test.go), so the edge list maps
// one-to-one onto CSR positions. HandlesSlice is nil, which is exactly the
// production NON-multigraph path: the reverse traversal recovers a canonical
// edge id through Expand.lookupFwdEdgePos, not the by-handle variant.
//
// Findings drive the #2090 anchor-swap policy:
//
//   - BenchmarkExpandDir_InVsOut_Baseline: OUT vs IN over a fan-in star whose
//     sources all have out-degree 1. Both examine the same D edges; each
//     source's out-range is length 1, so the reverse per-edge canonical-id scan
//     is O(1). This is the well-behaved case the cost model assumes.
//   - BenchmarkExpandIn_PerEdge_vs_SourceOutdegree: the decisive one. Expanding
//     IN examines a node's in-edges, but for EACH in-edge (src → cur) the
//     operator scans src's WHOLE forward out-range to recover the canonical edge
//     id (Expand.lookupFwdEdgePos, expand.go). So the cost of examining ONE
//     in-edge is Θ(out-degree of that edge's source), NOT Θ(1). This benchmark
//     fixes the number of in-edges examined at 1 and sweeps the source's
//     out-degree K; linear growth in K means the reverse per-edge cost is
//     unbounded by any count-store aggregate.
//   - BenchmarkExpandIn_TypeFiltered_vs_SourceOutdegree: the same, with an edge
//     type filter set, which adds a SECOND O(K) forward scan per in-edge
//     (Expand.reverseEdgePassesFilter). A directed `(a:A)-[:R]->(b:B)` anchored
//     at b IS this case (EdgeType="R").
//   - BenchmarkBuildReverse_vs_E: the reverse-CSR build itself, O(V+E). Read
//     together with the code fact that api.go's csrPairFromGraph builds BOTH the
//     forward and the reverse CSR unconditionally on every Expand (fwd, rev :=
//     csrPairFromGraph(g); rev = fwd.BuildReverse()), regardless of the expand
//     direction — so the reverse-CSR build cost is identical for an anchor-A
//     (OUT) plan and an anchor-B (IN) plan and cancels in the P-anchor
//     comparison. The hazard, if any, is the per-edge scan overhead above.
//
// Layer: short. Run with:
//
//	go test -run=^$ -bench=BenchmarkExpand -benchmem -count=6 ./cypher/exec/
//	go test -run=^$ -bench=BenchmarkBuildReverse -benchmem -count=6 ./cypher/exec/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildStaticPair builds a forward staticCSR and a reverse staticCSR from an
// explicit directed edge list over nodes 0..maxNode. NodeID equals the array
// index, so an int i in the edge list is NodeID i; the reverse CSR is the same
// edges with endpoints swapped.
func buildStaticPair(maxNode int, edges [][2]int) (fwd, rev *staticCSR) {
	fwd = buildCSR(maxNode, edges)
	swapped := make([][2]int, len(edges))
	for i, e := range edges {
		swapped[i] = [2]int{e[1], e[0]}
	}
	rev = buildCSR(maxNode, swapped)
	return fwd, rev
}

// allEdgesTypeFilter maps every forward-edge position to a single type label, so
// a DirIn Expand configured with that EdgeType exercises the reverse type-filter
// path (reverseEdgePassesFilter) for every edge.
func allEdgesTypeFilter(fwd *staticCSR, label string) map[uint64]string {
	edges := fwd.EdgesSlice()
	m := make(map[uint64]string, len(edges))
	for pos := range edges {
		m[uint64(pos)] = label
	}
	return m
}

// benchSliceOp is a reusable source operator: Init rewinds it so the same
// instance can drive every benchmark iteration without re-allocating the rows.
type benchSliceOp struct {
	rows []exec.Row
	idx  int
	ctx  context.Context //nolint:containedctx // bench source stores ctx intentionally
}

func (s *benchSliceOp) Init(ctx context.Context) error { s.ctx = ctx; s.idx = 0; return nil }
func (s *benchSliceOp) Next(out *exec.Row) (bool, error) {
	if s.idx >= len(s.rows) {
		return false, nil
	}
	*out = s.rows[s.idx]
	s.idx++
	return true, nil
}
func (s *benchSliceOp) Close() error { return nil }

// intRows builds one single-column input row per node id in ids.
func intRows(ids ...int) []exec.Row {
	rows := make([]exec.Row, len(ids))
	for i, id := range ids {
		rows[i] = exec.Row{expr.IntegerValue(id)}
	}
	return rows
}

// drainExpand runs a single Expand over the given source rows to exhaustion,
// returning the number of emitted rows so the compiler cannot elide the work.
func drainExpand(b *testing.B, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig, srcRows []exec.Row) int {
	b.Helper()
	ctx := context.Background()
	src := &benchSliceOp{rows: srcRows}
	op := exec.NewExpand(src, exec.StaticAdjacency(fwd, rev, filter), cfg)
	if err := op.Init(ctx); err != nil {
		b.Fatalf("init: %v", err)
	}
	var row exec.Row
	n := 0
	for {
		ok, err := op.Next(&row)
		if err != nil {
			b.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		n++
	}
	if err := op.Close(); err != nil {
		b.Fatalf("close: %v", err)
	}
	return n
}

// BenchmarkExpandDir_InVsOut_Baseline compares OUT and IN over a fan-in star
// whose sources all have out-degree 1 (leaf_i → centre). Both traversals
// examine the same D edges; each source's out-range is length 1, so the reverse
// per-edge canonical-id scan is O(1). This is the well-behaved case the cost
// model assumes; the IN/OUT ratio here is the honest constant-factor gap.
func BenchmarkExpandDir_InVsOut_Baseline(b *testing.B) {
	const degree = 4096
	// centre = node 0; leaves 1..degree each with one edge leaf → centre.
	edges := make([][2]int, 0, degree)
	for leaf := 1; leaf <= degree; leaf++ {
		edges = append(edges, [2]int{leaf, 0})
	}
	fwd, rev := buildStaticPair(degree+1, edges) // node ids 0..degree

	leafIDs := make([]int, degree)
	for i := range leafIDs {
		leafIDs[i] = i + 1
	}
	outRows := intRows(leafIDs...) // drive OUT from every leaf (each deg-1)
	inRows := intRows(0)           // drive IN from the centre (in-deg = degree)

	b.Run("OUT_deg1_sources", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if n := drainExpand(b, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}, outRows); n != degree {
				b.Fatalf("OUT emitted %d, want %d", n, degree)
			}
		}
	})
	b.Run("IN_deg1_sources", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if n := drainExpand(b, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirIn, InputCol: 0}, inRows); n != degree {
				b.Fatalf("IN emitted %d, want %d", n, degree)
			}
		}
	})
}

// BenchmarkExpandIn_PerEdge_vs_SourceOutdegree is the decisive measurement.
// Graph: one hub h=0 with K out-edges (0→1 … 0→K). Node K therefore has exactly
// ONE in-edge, from the hub, and it is the hub's LAST out-neighbour. Expanding
// IN from node K examines that single in-edge, but recovering its canonical id
// scans the hub's whole out-range to the final slot — O(K)
// (Expand.lookupFwdEdgePos). The single-in-edge cost is swept against K: linear
// growth means the reverse per-edge cost is Θ(source out-degree), invisible to
// the count-store's aggregate D(label,relType,IN).
func BenchmarkExpandIn_PerEdge_vs_SourceOutdegree(b *testing.B) {
	for _, K := range []int{16, 256, 4096, 65536} {
		edges := make([][2]int, 0, K)
		for dst := 1; dst <= K; dst++ {
			edges = append(edges, [2]int{0, dst})
		}
		fwd, rev := buildStaticPair(K+1, edges) // node ids 0..K
		inRows := intRows(K)                    // hub's LAST out-neighbour: lookupFwdEdgePos scans all K
		b.Run(fmt.Sprintf("K=%d/one_in_edge", K), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if n := drainExpand(b, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirIn, InputCol: 0}, inRows); n != 1 {
					b.Fatalf("emitted %d, want 1", n)
				}
			}
		})
	}
}

// BenchmarkExpandIn_TypeFiltered_vs_SourceOutdegree repeats the sweep with an
// edge-type filter set, which forces a SECOND O(K) forward scan per in-edge
// (reverseEdgePassesFilter, in addition to lookupFwdEdgePos).
func BenchmarkExpandIn_TypeFiltered_vs_SourceOutdegree(b *testing.B) {
	for _, K := range []int{16, 256, 4096, 65536} {
		edges := make([][2]int, 0, K)
		for dst := 1; dst <= K; dst++ {
			edges = append(edges, [2]int{0, dst})
		}
		fwd, rev := buildStaticPair(K+1, edges) // node ids 0..K
		filter := allEdgesTypeFilter(fwd, "R")
		inRows := intRows(K) // last out-neighbour: worst-case O(K) forward scan
		b.Run(fmt.Sprintf("K=%d/one_in_edge_typed", K), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if n := drainExpand(b, fwd, rev, filter, exec.ExpandConfig{
					Direction: exec.DirIn,
					InputCol:  0,
					EdgeType:  "R",
				}, inRows); n != 1 {
					b.Fatalf("emitted %d, want 1", n)
				}
			}
		})
	}
}

// BenchmarkBuildReverse_vs_E measures the reverse-CSR build (CSR.BuildReverse),
// which the query path pays unconditionally on every Expand via
// csrPairFromGraph — for an OUT plan just as for an IN plan. It is O(V+E); this
// confirms the build cost is a per-query constant independent of the anchor
// choice, so it cancels in the P-anchor comparison. A real csr.CSR is used here
// (this benchmark measures the production BuildReverse, not the traversal).
func BenchmarkBuildReverse_vs_E(b *testing.B) {
	for _, E := range []int{10000, 100000, 1000000} {
		n := E
		adj := adjlist.New[int, float64](adjlist.Config{Directed: true})
		for i := 0; i < n; i++ {
			_ = adj.AddNode(i)
		}
		for i := 0; i < E; i++ {
			_ = adj.AddEdge(i%n, (i+1)%n, 0)
		}
		fwd := csr.BuildFromAdjList(adj)
		b.Run(fmt.Sprintf("E=%d", E), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = fwd.BuildReverse()
			}
		})
	}
}

// BenchmarkExpandOut_PerEdge_SingleSource measures the OUT per-edge cost in a
// tight single-source loop (hub 0 with K out-edges, expand OUT → K rows), free
// of the per-source-row advance overhead. Divided by K it yields β_out, the
// honest per-OUT-edge time; paired with BenchmarkScan_PerNode's α it calibrates
// the P-anchor cost-model constants (c_s ≈ α, c_e ≈ β_out) and confirms scan and
// OUT-edge costs are the same order of magnitude.
func BenchmarkExpandOut_PerEdge_SingleSource(b *testing.B) {
	for _, K := range []int{4096, 65536} {
		edges := make([][2]int, 0, K)
		for dst := 1; dst <= K; dst++ {
			edges = append(edges, [2]int{0, dst})
		}
		fwd, rev := buildStaticPair(K+1, edges)
		outRows := intRows(0) // hub: one source, K out-edges
		b.Run(fmt.Sprintf("K=%d", K), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if n := drainExpand(b, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}, outRows); n != K {
					b.Fatalf("emitted %d, want %d", n, K)
				}
			}
		})
	}
}

// BenchmarkScan_PerNode measures a NodeByLabelScan-equivalent enumeration cost
// per node so the P-anchor scan constant c_s can be calibrated against the
// per-edge constant c_e. It drains a bare source of N single-column rows through
// the same Expand-less driver, so it isolates row production. Divided by N it
// yields α.
func BenchmarkScan_PerNode(b *testing.B) {
	for _, N := range []int{4096, 65536} {
		ids := make([]int, N)
		for i := range ids {
			ids[i] = i
		}
		rows := intRows(ids...)
		b.Run(fmt.Sprintf("N=%d", N), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src := &benchSliceOp{rows: rows}
				_ = src.Init(context.Background())
				var row exec.Row
				cnt := 0
				for {
					ok, _ := src.Next(&row)
					if !ok {
						break
					}
					cnt++
				}
				if cnt != N {
					b.Fatalf("scanned %d, want %d", cnt, N)
				}
			}
		})
	}
}
