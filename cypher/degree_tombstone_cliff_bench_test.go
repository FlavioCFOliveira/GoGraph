package cypher_test

// degree_tombstone_cliff_bench_test.go — rmp #2265's permanent benchmark.
//
// # The defect
//
// The untyped degree count is O(1) — one adjacency column length — only while the
// graph holds no tombstones. Deleting ANY node, anywhere, related or not, switches
// every untyped degree count in the graph onto a walk that excludes tombstoned
// endpoints, and that walk used to be driven with maxInt as its limit, discarding
// the caller's cap. A question that needed one edge then read the whole column.
//
// # How to read it
//
// Each shape is measured twice, over graphs that differ ONLY in whether a single
// edgeless, unrelated node has been deleted. The number that matters is the RATIO
// between the two arms, not either arm's absolute time: a shape whose cost depends
// on an unrelated deletion is the defect, whatever the machine.
//
// # Two traps this file is shaped around, both of which caught this benchmark
//
//  1. `WHERE EXISTS { (a)-->() }` NEVER REACHES THE DEGREE PATH. The plan-level
//     SemiApply rewrite claims that shape first, so the first version of this
//     benchmark measured a different operator entirely and reported no cliff at
//     all. The EXISTS shape here is therefore a RETURN projection, which the
//     expression evaluator serves. Which shapes route where is pinned as a test,
//     not left to this comment: TestDegreeRewrite_WhichShapesReachTheDegreePath.
//
//  2. REBUILDING THE FIXTURE PER BENCHMARK SWAMPS THE MEASUREMENT. At 400 000
//     edges, a rebuild per benchmark function per -count repetition is 60 builds
//     in one process; the GC pressure that produces moved the untouched control
//     arms by more than 100% between two runs of identical code. Both graphs are
//     therefore built ONCE per process and shared read-only by every benchmark
//     below.
//
// Run with:
//
//	go test -run=^$ -bench='BenchmarkDegreeCliff' -benchmem -count=10 ./cypher/

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// degreeCliffDegree is the hub's out-degree — the degree rmp #2265 measured the
// cliff at. It has to be large: the defect is an O(d) walk where the caller asked
// for O(1), so a small d hides inside the per-query constant.
const degreeCliffDegree = 400_000

// seedDegreeCliffGraph builds one hub with degreeCliffDegree out-edges of type K,
// plus one EDGELESS, UNRELATED node labelled Z, deleted when tombstone is set.
//
// That deletion is the entire difference between the two arms of every pair
// below. Z shares no edge, no label and no property with the hub, so touching it
// must not change what the hub's degree count costs.
//
// The graph is built through the lpg API rather than through Cypher CREATE
// because 400 000 statements would dominate set-up by orders of magnitude; the
// shape the query sees is identical either way.
func seedDegreeCliffGraph(tb testing.TB, tombstone bool) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	if err := g.AddNode("hub"); err != nil {
		tb.Fatalf("AddNode(hub): %v", err)
	}
	if err := g.SetNodeLabel("hub", "H"); err != nil {
		tb.Fatalf("SetNodeLabel(hub): %v", err)
	}
	for i := 0; i < degreeCliffDegree; i++ {
		dst := fmt.Sprintf("d%d", i)
		if err := g.AddNode(dst); err != nil {
			tb.Fatalf("AddNode(%s): %v", dst, err)
		}
		if err := g.AddEdgeLabeled("hub", dst, 1, "K"); err != nil {
			tb.Fatalf("AddEdgeLabeled(%s): %v", dst, err)
		}
	}

	if err := g.AddNode("z"); err != nil {
		tb.Fatalf("AddNode(z): %v", err)
	}
	if err := g.SetNodeLabel("z", "Z"); err != nil {
		tb.Fatalf("SetNodeLabel(z): %v", err)
	}
	if tombstone {
		g.RemoveNode("z")
	}
	return g
}

// The two fixtures, built once per process. See trap 2 in the file comment: a
// per-benchmark rebuild at this size moves the control arms further than the
// effect being measured.
var (
	cleanEngineOnce sync.Once
	cleanEngine     *cypher.Engine
	tombEngineOnce  sync.Once
	tombEngine      *cypher.Engine
)

func degreeCliffEngine(tb testing.TB, tombstone bool) *cypher.Engine {
	tb.Helper()
	if tombstone {
		tombEngineOnce.Do(func() { tombEngine = cypher.NewEngine(seedDegreeCliffGraph(tb, true)) })
		return tombEngine
	}
	cleanEngineOnce.Do(func() { cleanEngine = cypher.NewEngine(seedDegreeCliffGraph(tb, false)) })
	return cleanEngine
}

// runDegreeCliff drives q b.N times over the shared fixture selected by
// tombstone.
func runDegreeCliff(b *testing.B, tombstone bool, q string) {
	b.Helper()
	eng := degreeCliffEngine(b, tombstone)
	ctx := context.Background()

	// One warm-up so the plan cache and the label registry are populated and the
	// measured loop times the degree count, not the first plan build.
	drainDegreeCliff(ctx, b, eng, q)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainDegreeCliff(ctx, b, eng, q)
	}
}

func drainDegreeCliff(ctx context.Context, b *testing.B, eng *cypher.Engine, q string) {
	b.Helper()
	res, err := eng.Run(ctx, q, nil)
	if err != nil {
		b.Fatal(err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		b.Fatal(err)
	}
	_ = res.Close()
}

// degreeCliffExists is the shape the defect was reported on: EXISTS over an
// UNTYPED single hop, which the degree rewrite answers with a cap of 1. It is a
// RETURN projection, not a WHERE predicate — see trap 1 in the file comment.
const degreeCliffExists = `MATCH (a:H) RETURN EXISTS { (a)-->() } AS e`

// degreeCliffCountCmp asks the same question as a bounded COUNT, which the
// evaluator caps at literal+1 rather than at 1 — a second, independent route into
// the same untyped walk.
const degreeCliffCountCmp = `MATCH (a:H) WHERE COUNT { (a)-->() } > 2 RETURN count(a)`

// degreeCliffTypedExists is the CONTROL. The typed walk has always honoured its
// cap, so its two arms were within noise of each other before the fix and must
// stay that way after it: a change that bought the untyped path's speed by
// weakening the typed one would show up here.
const degreeCliffTypedExists = `MATCH (a:H) RETURN EXISTS { (a)-[:K]->() } AS e`

func BenchmarkDegreeCliffUntypedExists_Clean(b *testing.B) {
	runDegreeCliff(b, false, degreeCliffExists)
}

func BenchmarkDegreeCliffUntypedExists_OneUnrelatedDelete(b *testing.B) {
	runDegreeCliff(b, true, degreeCliffExists)
}

func BenchmarkDegreeCliffUntypedCount_Clean(b *testing.B) {
	runDegreeCliff(b, false, degreeCliffCountCmp)
}

func BenchmarkDegreeCliffUntypedCount_OneUnrelatedDelete(b *testing.B) {
	runDegreeCliff(b, true, degreeCliffCountCmp)
}

func BenchmarkDegreeCliffTypedExists_Clean(b *testing.B) {
	runDegreeCliff(b, false, degreeCliffTypedExists)
}

func BenchmarkDegreeCliffTypedExists_OneUnrelatedDelete(b *testing.B) {
	runDegreeCliff(b, true, degreeCliffTypedExists)
}
