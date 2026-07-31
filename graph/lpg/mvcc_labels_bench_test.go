package lpg

// mvcc_labels_bench_test.go — the measurement rmp #2275 exists to produce.
//
// THE QUESTION: is the per-write cost of a delta chain independent of graph
// size? rmp #2051's whole-graph copy-on-write prototype was O(shard size) per
// write, which is why it cost 5.4× time and 43× memory and was reverted. If a
// delta chain is O(1), the conclusion drawn from that measurement — that MVCC
// requires replacing the LPG core maps with persistent structures — does not
// apply to this design.
//
// The two arms run in ONE process toggled by an option rather than as two
// builds compared back to back, because on this machine a byte-identical
// control has produced 22 of 36 flat-by-construction rows as "significant".
//
// The sizes are an order of magnitude apart on purpose: the claim under test is
// about SCALING, so a single size cannot support or refute it.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// benchLabelGraph builds n nodes, each already carrying label "Base", so a
// measured write is a real second label on a populated bag rather than the
// first insertion into an empty map.
// armAfterSeed decides WHEN the spike is armed relative to seeding, and the
// distinction is the whole point of the read benchmark. Arming BEFORE the seed
// records a delta for every node's first label, so the graph ends up with
// 100 000 live deltas and a "no live delta" arm measures a chain walk — which is
// exactly what the first version of this file did, reporting the fast path as
// 3× slower than it is. Arming AFTER leaves the counter at zero, which is the
// state a read-mostly workload is actually in.
func benchLabelGraph(b *testing.B, n int, deltas bool) (*Graph[string, float64], []string) {
	return benchLabelGraphAt(b, n, deltas, false)
}

func benchLabelGraphAt(b *testing.B, n int, deltas, armAfterSeed bool) (*Graph[string, float64], []string) {
	b.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if deltas && !armAfterSeed {
		g.EnableLabelDeltas()
	}
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = fmt.Sprintf("n%d", i)
		if err := g.AddNode(keys[i]); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(keys[i], "Base"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
	}
	if deltas && armAfterSeed {
		g.EnableLabelDeltas()
	}
	return g, keys
}

// BenchmarkLabelWrite measures one label mutation against graph size, with and
// without delta recording.
//
// Each iteration adds a label and removes it again, so the bag returns to its
// starting state and the benchmark measures a steady stream of real changes
// rather than a growing bag. With deltas armed that is TWO delta records per
// iteration, which is the worst case for the cost model.
func BenchmarkLabelWrite(b *testing.B) {
	for _, size := range []int{10000, 1000000} {
		for _, deltas := range []bool{false, true} {
			name := fmt.Sprintf("nodes=%d/deltas=%v", size, deltas)
			b.Run(name, func(b *testing.B) {
				g, keys := benchLabelGraph(b, size, deltas)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					k := keys[i%len(keys)]
					if err := g.SetNodeLabel(k, "Hot"); err != nil {
						b.Fatalf("SetNodeLabel: %v", err)
					}
					g.RemoveNodeLabel(k, "Hot")
				}
				b.StopTimer()
				if deltas {
					// Reported, not asserted: nothing reclaims deltas in the
					// spike, so this is the memory the design owes a GC phase.
					b.ReportMetric(float64(g.LabelDeltaCount())/float64(b.N), "deltas/op")
				}
			})
		}
	}
}

// BenchmarkLabelRead measures the read fast path — the case that decides
// whether the mechanism is affordable, since a read-mostly workload pays it on
// every access and gets nothing back.
//
// Three arms: the mechanism absent, the mechanism armed but no live delta (the
// lock-free gate short-circuits), and the mechanism armed with a live delta on
// the node being read (the chain walk).
func BenchmarkLabelRead(b *testing.B) {
	const size = 100000
	b.Run("deltas=off", func(b *testing.B) {
		g, keys := benchLabelGraph(b, size, false)
		id, _ := g.adj.Mapper().Lookup(keys[0])
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// labelBagPlain, not the read written inline here: the control must
			// pay the same call and 40-byte return copy the MVCC read pays, or
			// the comparison charges those to the mechanism under test.
			bag := g.labelBagPlain(id)
			if bag.len() == 0 {
				b.Fatal("empty bag")
			}
		}
	})
	b.Run("deltas=on/no-live-delta", func(b *testing.B) {
		g, keys := benchLabelGraphAt(b, size, true, true)
		if g.LabelDeltaCount() != 0 {
			b.Fatalf("fixture left %d live deltas; this arm must measure the "+
				"lock-free gate, not a chain walk", g.LabelDeltaCount())
		}
		id, _ := g.adj.Mapper().Lookup(keys[0])
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			bag := g.labelBagAsOf(id, ^uint64(0)>>1, 0)
			if bag.len() == 0 {
				b.Fatal("empty bag")
			}
		}
	})
	b.Run("deltas=on/one-live-delta", func(b *testing.B) {
		g, keys := benchLabelGraphAt(b, size, true, true)
		if err := g.SetNodeLabel(keys[0], "Hot"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		id, _ := g.adj.Mapper().Lookup(keys[0])
		if g.LabelDeltaCount() != 1 {
			b.Fatalf("expected exactly one live delta, got %d", g.LabelDeltaCount())
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// startTS 0 makes the delta unconditionally newer than the reader,
			// so the chain walk and the bag clone both actually run.
			bag := g.labelBagAsOf(id, 0, 0)
			if bag.len() == 0 {
				b.Fatal("empty bag")
			}
		}
	})
}
