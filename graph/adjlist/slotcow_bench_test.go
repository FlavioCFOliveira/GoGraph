package adjlist_test

import (
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// slotcow_bench_test.go — the cost of one adjacency write outside a commit
// window, and of the lock-free reads that share its slot structure, at several
// graph sizes (rmp #2882).
//
// A write outside a window publishes a new version of its shard's slot
// structure by copy-on-write. Until #2882 that copy was the WHOLE shard slot
// array — 8·V/256 bytes per write, so the per-write cost grew linearly with the
// graph. The Unbracketed benchmark reports B/op across sizes: a size-independent
// B/op is the property the change delivers, and the Read benchmarks are the
// guard that the read path did not pay for it.

var slotCowSizes = []int{5_000, 50_000, 150_000}

// newSlotCowGraph builds a directed graph of n nodes, each with one outgoing
// edge, inside one commit window so setup cost does not depend on the code
// under measurement's per-write copy.
func newSlotCowGraph(tb testing.TB, n int) *adjlist.AdjList[int, float64] {
	tb.Helper()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	a.BeginCommit()
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, 1); err != nil {
			a.EndCommit()
			tb.Fatalf("AddEdge: %v", err)
		}
	}
	a.EndCommit()
	return a
}

// BenchmarkSlotCOW_Unbracketed measures one unbracketed edge insert plus its
// removal (two copy-on-write publications per op) against a graph of the given
// size. The node's entry stays at degree one or two, so the entry allocations
// are size-independent and any growth of B/op with size is the slot copy.
func BenchmarkSlotCOW_Unbracketed(b *testing.B) {
	for _, n := range slotCowSizes {
		b.Run("nodes="+strconv.Itoa(n), func(b *testing.B) {
			a := newSlotCowGraph(b, n)
			const sentinel = -1
			if err := a.AddNode(sentinel); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src := (i * 7919) % n
				if err := a.AddEdge(src, sentinel, 2); err != nil {
					b.Fatal(err)
				}
				a.RemoveEdge(src, sentinel)
			}
		})
	}
}

// slotCowProbes returns a deterministic pseudo-random sequence of NodeIDs of
// nodes that carry an entry, so reads touch the whole slot structure rather
// than one hot path.
func slotCowProbes(a *adjlist.AdjList[int, float64], n int) []graph.NodeID {
	const probes = 1 << 16
	r := rand.New(rand.NewPCG(2882, 1)) //nolint:gosec // deterministic benchmark RNG
	out := make([]graph.NodeID, probes)
	for i := range out {
		id, ok := a.Mapper().Lookup(r.IntN(n))
		if !ok {
			panic("probe node not interned")
		}
		out[i] = id
	}
	return out
}

// BenchmarkSlotCOW_ReadLoadEntry measures LoadEntry, the raw lock-free slot
// resolution, over random nodes.
func BenchmarkSlotCOW_ReadLoadEntry(b *testing.B) {
	for _, n := range slotCowSizes {
		b.Run("nodes="+strconv.Itoa(n), func(b *testing.B) {
			a := newSlotCowGraph(b, n)
			probes := slotCowProbes(a, n)
			mask := len(probes) - 1
			var sink int
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				nb, _ := a.LoadEntry(probes[i&mask])
				sink += len(nb)
			}
			if sink == 0 {
				b.Fatal("no entry was read")
			}
		})
	}
}

// BenchmarkSlotCOW_ReadNeighbours measures neighbour iteration by node value
// (mapper lookup, slot resolution, iteration) over random nodes.
func BenchmarkSlotCOW_ReadNeighbours(b *testing.B) {
	for _, n := range slotCowSizes {
		b.Run("nodes="+strconv.Itoa(n), func(b *testing.B) {
			a := newSlotCowGraph(b, n)
			r := rand.New(rand.NewPCG(2882, 2)) //nolint:gosec // deterministic benchmark RNG
			keys := make([]int, 1<<16)
			for i := range keys {
				keys[i] = r.IntN(n)
			}
			mask := len(keys) - 1
			var sink int
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for v := range a.Neighbours(keys[i&mask]) {
					sink += v
				}
			}
			if sink == 0 {
				b.Fatal("no neighbour was read")
			}
		})
	}
}

// BenchmarkSlotCOW_PinSnapshot measures one PinSnapshot on a populated graph:
// one load per shard, plus — since rmp #2882 — the claim that freezes each
// captured version. After the first pin every version is already frozen, so
// later pins exercise the frozen fast path; the first write to a shard after a
// pin clones its array once, which this benchmark does not include.
func BenchmarkSlotCOW_PinSnapshot(b *testing.B) {
	a := newSlotCowGraph(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Assigned to a package-level sink so the Snapshot escapes, as it does
		// for any caller that holds it: otherwise an inlined pin is
		// stack-allocated and the arms are not comparable.
		pinSink = a.PinSnapshot()
	}
	if pinSink == nil {
		b.Fatal("nil snapshot")
	}
}

var pinSink *adjlist.Snapshot[int, float64]
