package exec

// shortest_path_bench_test.go — the cost instrument for rmp #2763.
//
// #2763 put a storage-access counter on both shortest-path operators. The charge
// is one add per adjacency RUN rather than one per slot ([ShortestPath.scanRun]),
// and this file is what that claim is measured against.
//
// The three shapes are chosen to BRACKET the added cost rather than to flatter
// it:
//
//   - unit-degree chain — every adjacency run holds ONE slot, so the per-run
//     charge degenerates to a per-slot charge. This is the worst case the design
//     can be put in, and it is measured first;
//   - layered — a realistic BFS with a branching frontier and average out-degree
//     8, where the charge is one add per 8 slots;
//   - high-degree fan — one run of 100 000 slots, where the charge is one add for
//     the whole walk. This is the best case, and it is here to show the range,
//     not to stand in for the answer.
//
// Each shape is driven for both operators, and the layered shape additionally
// with a relationship-type filter, because a typed search takes a different
// admission branch inside the same loop.

import (
	"context"
	"testing"
)

// spBenchLayeredGraph builds `layers` layers of `width` nodes, each node linked to
// `deg` nodes of the next layer by a deterministic stride. Node 0 is in layer 0
// and the returned dst sits in the last layer, so a path of exactly layers-1 hops
// exists and the BFS frontier branches at every level.
func spBenchLayeredGraph(layers, width, deg int) (g biTestGraph, src, dst uint64) {
	n := layers * width
	edges := make([][2]int, 0, (layers-1)*width*deg)
	for l := 0; l < layers-1; l++ {
		for w := 0; w < width; w++ {
			from := l*width + w
			for k := 0; k < deg; k++ {
				edges = append(edges, [2]int{from, (l+1)*width + (w*deg+k)%width})
			}
		}
	}
	return biTestGraph{n: n, edges: edges}, 0, uint64((layers-1)*width + 0)
}

// spBenchChainGraph builds a single path 0→1→…→(n-1): every adjacency run holds exactly
// one slot, which is the worst case for a charge taken once per run.
func spBenchChainGraph(n int) (g biTestGraph, src, dst uint64) {
	edges := make([][2]int, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	return biTestGraph{n: n, edges: edges}, 0, uint64(n - 1)
}

// spBenchFanGraph builds one node with `fan` out-edges, of which the first continues to
// the destination: a single run of `fan` slots, the best case for a per-run
// charge.
func spBenchFanGraph(fan int) (g biTestGraph, src, dst uint64) {
	edges := make([][2]int, 0, fan+1)
	for i := 1; i <= fan; i++ {
		edges = append(edges, [2]int{0, i})
	}
	edges = append(edges, [2]int{1, fan + 1})
	return biTestGraph{n: fan + 2, edges: edges}, 0, uint64(fan + 1)
}

func benchShortestOperator(b *testing.B, g biTestGraph, filter map[uint64]string) *ShortestPath {
	b.Helper()
	fwd, rev := g.csrPair()
	op := NewShortestPath(biNoInput{}, StaticAdjacency(fwd, rev, filter), DirOut, 0, 1)
	if filter != nil {
		op.WithTypeFilter("K")
	}
	if err := op.Init(context.Background()); err != nil {
		b.Fatalf("Init: %v", err)
	}
	return op
}

func benchAllShortestOperator(b *testing.B, g biTestGraph, filter map[uint64]string) *AllShortestPaths {
	b.Helper()
	fwd, rev := g.csrPair()
	op := NewAllShortestPaths(biNoInput{}, StaticAdjacency(fwd, rev, filter), DirOut, 0, 1)
	if filter != nil {
		op.WithTypeFilter("K")
	}
	if err := op.Init(context.Background()); err != nil {
		b.Fatalf("Init: %v", err)
	}
	return op
}

func BenchmarkShortestPath_UnitDegreeChain(b *testing.B) {
	g, src, dst := spBenchChainGraph(4096)
	op := benchShortestOperator(b, g, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found, err := op.bfsShortestPath(src, dst); err != nil || !found {
			b.Fatalf("found=%v err=%v", found, err)
		}
	}
}

func BenchmarkShortestPath_Layered(b *testing.B) {
	g, src, dst := spBenchLayeredGraph(7, 1024, 8)
	op := benchShortestOperator(b, g, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found, err := op.bfsShortestPath(src, dst); err != nil || !found {
			b.Fatalf("found=%v err=%v", found, err)
		}
	}
}

func BenchmarkShortestPath_LayeredTyped(b *testing.B) {
	g, src, dst := spBenchLayeredGraph(7, 1024, 8)
	// Admit every forward position, so the search reaches dst while still taking
	// the typed admission branch on every slot.
	filter := make(map[uint64]string, len(g.edges))
	for i := range g.edges {
		filter[uint64(i)] = "K"
	}
	op := benchShortestOperator(b, g, filter)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found, err := op.bfsShortestPath(src, dst); err != nil || !found {
			b.Fatalf("found=%v err=%v", found, err)
		}
	}
}

func BenchmarkShortestPath_HighDegreeFan(b *testing.B) {
	g, src, dst := spBenchFanGraph(20000)
	op := benchShortestOperator(b, g, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found, err := op.bfsShortestPath(src, dst); err != nil || !found {
			b.Fatalf("found=%v err=%v", found, err)
		}
	}
}

func BenchmarkAllShortestPaths_UnitDegreeChain(b *testing.B) {
	// 1024 rather than the single-path benchmark's 4096: this operator reconstructs
	// every shortest path, so a longer chain measures reconstruction rather than
	// the scan the counter sits in.
	g, src, dst := spBenchChainGraph(1024)
	op := benchAllShortestOperator(b, g, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		paths, err := op.bfsAllShortest(src, dst)
		if err != nil || len(paths) == 0 {
			b.Fatalf("paths=%d err=%v", len(paths), err)
		}
	}
}

func BenchmarkAllShortestPaths_Layered(b *testing.B) {
	g, src, dst := spBenchLayeredGraph(5, 512, 8)
	op := benchAllShortestOperator(b, g, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		paths, err := op.bfsAllShortest(src, dst)
		if err != nil || len(paths) == 0 {
			b.Fatalf("paths=%d err=%v", len(paths), err)
		}
	}
}

func BenchmarkAllShortestPaths_HighDegreeFan(b *testing.B) {
	g, src, dst := spBenchFanGraph(20000)
	op := benchAllShortestOperator(b, g, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		paths, err := op.bfsAllShortest(src, dst)
		if err != nil || len(paths) == 0 {
			b.Fatalf("paths=%d err=%v", len(paths), err)
		}
	}
}
