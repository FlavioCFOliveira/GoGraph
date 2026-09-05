//go:build soak || nightly

package audit352_test

// gctax_soak_test.go — the resident-graph garbage-collector tax sweep (rmp #2667).
//
// # Why this file is soak-layer
//
// TestGCTax_ResidentGraph builds three graphs of 50 000, 200 000 and 800 000
// nodes — the largest carrying 3.2 million directed :KNOWS edges — and forces
// five full collections over each of them. That is the whole point: the mark
// phase's cost is what is being measured, and it only separates from the noise
// on a heap large enough to take milliseconds to trace. It is also why the test
// does not belong in the short layer. Measured on the reference host (Apple M4,
// 10 cores, darwin/arm64, go1.27.0), in-package under -race, load average 1.79
// before / 2.55 after:
//
//	TestGCTax_ResidentGraph   138.37 s   (34.6 % of the whole package)
//
// Left in the short layer it was the single largest cost in bench/audit352, which
// measured 399.77 s under -race against the 240 s hard ceiling that
// scripts/pkg_time_budget.sh fails `make ci` on. See docs/test-layers.md.
//
// # What it does NOT assert
//
// The sweep carries no assertion on the quantity it measures: the GC times, heap
// object counts and per-node ratios are reported with t.Logf and compared against
// nothing. Its only failure paths are the fixture-construction errors that must()
// reports. Moving it therefore removes no regression protection from the short
// layer — it removes a measurement.
//
// Run it with:
//
//	go test -tags=soak -race ./bench/audit352/ -run '^TestGCTax_ResidentGraph$' -v

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestGCTax_ResidentGraph measures what it costs the Go garbage collector to
// TRACE a graph that is merely resident and completely unchanging.
//
// Why this matters: a CPU profile of the search benchmarks in ./search showed
// ~46% of cumulative CPU inside runtime.scanObject, 99.93% of it reached from
// runtime.gcDrain — while the algorithms themselves reported 0 to 5
// allocations per operation. Allocation-free code cannot be blamed for GC
// work, so the cost must be the MARK phase walking a pointer-dense resident
// heap. This test isolates exactly that: build a graph, stop changing it,
// then force collections and time them.
//
// A pointer-free representation (index arrays) is invisible to the mark
// phase; a pointer-dense one is re-traced on every cycle for as long as the
// process holds it. The numbers below are the tax the current representation
// charges per resident node.
//
//	go test -tags=soak -run '^TestGCTax_ResidentGraph$' -v -timeout 30m ./bench/audit352/
func TestGCTax_ResidentGraph(t *testing.T) {
	// An empty-process baseline, so the per-graph figures are differences
	// against a real floor rather than against zero.
	base := forcedGCCost(t, 5)
	t.Logf("empty process: forced GC = %.3f ms (heap objects %d)", base.ms, base.objects)

	t.Logf("%-10s %10s %12s %14s %14s %14s %14s", "nodes", "degree", "GC ms", "GC ms/1k node", "heap objects", "obj/node", "heapAlloc MB")
	for _, n := range []int{50_000, 200_000, 800_000} {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("n%d", i)
			must(g.AddNode(k))
			must(g.SetNodeLabel(k, "Person"))
			must(g.SetNodeProperty(k, "age", lpg.Int64Value(int64(1000+i%60000))))
		}
		for i := 0; i < n; i++ {
			src := fmt.Sprintf("n%d", i)
			for d := 1; d <= 4; d++ {
				must(g.AddEdge(src, fmt.Sprintf("n%d", (i+d*104729)%n), 0))
				g.SetEdgeLabel(src, fmt.Sprintf("n%d", (i+d*104729)%n), "KNOWS")
			}
		}
		c := forcedGCCost(t, 5)
		t.Logf("%-10d %10d %12.3f %14.4f %14d %14.2f %14.1f",
			n, 4, c.ms, c.ms/float64(n)*1000, c.objects, float64(c.objects)/float64(n),
			float64(c.heapAlloc)/(1<<20))
		runtime.KeepAlive(g)
		g = nil
		runtime.GC()
		debug.FreeOSMemory()
	}
}

type gcCost struct {
	ms        float64
	objects   uint64
	heapAlloc uint64
}

// forcedGCCost runs reps full collections on a quiesced heap and returns the
// MEDIAN wall time of one, plus the live-heap statistics that explain it.
// The first collection is discarded: it also sweeps whatever the build left
// behind, which is not what this measures.
func forcedGCCost(t *testing.T, reps int) gcCost {
	t.Helper()
	runtime.GC()
	runtime.GC()
	var samples []float64
	for i := 0; i < reps; i++ {
		start := time.Now()
		runtime.GC()
		samples = append(samples, time.Since(start).Seconds()*1e3)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return gcCost{ms: medianOf(samples), objects: ms.HeapObjects, heapAlloc: ms.HeapAlloc}
}
