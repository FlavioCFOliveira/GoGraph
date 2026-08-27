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

	"github.com/FlavioCFOliveira/GoGraph/graph"
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

// # Why this disarms rather than calling EnableLabelDeltas (rmp #2623)
//
// [Graph.EnableLabelDeltas] is a NO-OP on a graph from [New]. It was the #2275
// spike's opt-in switch, and the substrate moved underneath it: [Graph.armMVCC]
// now sets labelDeltas = true and runs by default, so the flag it sets is
// already set. Its godoc still says "Nothing in the module calls it".
//
// The consequences were both invisible and total. The `deltas=off` arm was NOT
// A CONTROL — MEASURED, a graph built by benchLabelGraph(size, false) reports
// labelDeltas=true and 38 live deltas — so both arms measured the same thing.
// And the `deltas=on` arms seeded 100 000 labelled nodes WITH deltas recording,
// then asserted the count was zero; what they actually observed was whatever the
// reclaimer had not yet swept, which is why the number was 13 at -benchtime=1x,
// 153 at 10x, and 4965 on another run. It never accumulated across the loop:
// the fixture is rebuilt for every b.N, and the count is a race, not a total.
//
// The real seam is [Graph.disarmMVCCForTest], which is what this uses.
// Disarming before the seed and re-arming after gives the "armed, no live delta"
// state DETERMINISTICALLY: measured 0, not 3 or 13 or 4965.
func benchLabelGraphAt(b *testing.B, n int, deltas, armAfterSeed bool) (*Graph[string, float64], []string) {
	b.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	// Seed with the substrate DISARMED in every arm, so the seeding writes never
	// record a delta and the post-seed state is a function of the arm rather than
	// of how fast the reclaimer ran.
	g.disarmMVCCForTest()
	if deltas && !armAfterSeed {
		g.armMVCC()
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
		g.armMVCC()
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

// BenchmarkPropWrite re-measures the cost model for node PROPERTIES, whose undo
// record carries a value (56 bytes) rather than an identifier (32).
//
// P0 confirmed the model for the cheapest structure. The claim under test is
// again scaling, not the constant: the per-modification cost must not depend on
// how many nodes the graph holds.
func BenchmarkPropWrite(b *testing.B) {
	for _, size := range []int{10000, 1000000} {
		for _, deltas := range []bool{false, true} {
			b.Run(fmt.Sprintf("nodes=%d/deltas=%v", size, deltas), func(b *testing.B) {
				g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
				keys := make([]string, size)
				for i := 0; i < size; i++ {
					keys[i] = fmt.Sprintf("n%d", i)
					if err := g.AddNode(keys[i]); err != nil {
						b.Fatalf("AddNode: %v", err)
					}
					if err := g.SetNodeProperty(keys[i], "base", Int64Value(int64(i))); err != nil {
						b.Fatalf("SetNodeProperty: %v", err)
					}
				}
				if deltas {
					g.EnablePropDeltas()
				}
				// The WORKING SET is bounded independently of the graph size.
				// Without this the 1M arm touches a million distinct nodes and
				// the sparse delta side map grows to a million entries, whose
				// amortised rehashing lands in B/op and looks like the delta
				// cost scaling with the graph. Holding the working set fixed
				// separates the two, which is the whole question.
				work := len(keys)
				if work > 1000 {
					work = 1000
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					k := keys[i%work]
					// A real overwrite each time. A first version used
					// Int64Value(i&1), which revisits each key with the SAME
					// value once the loop wraps, so the redundant-write guard
					// suppressed every delta and the armed arm measured as
					// FASTER than the control with zero allocations — a
					// benchmark of nothing. The iteration counter always
					// changes, so every write is a real one.
					if err := g.SetNodeProperty(k, "hot", Int64Value(int64(i))); err != nil {
						b.Fatalf("SetNodeProperty: %v", err)
					}
				}
				b.StopTimer()
				if deltas {
					b.ReportMetric(float64(g.PropDeltaCount())/float64(b.N), "deltas/op")
				}
			})
		}
	}
}

// BenchmarkPropRead measures the property read fast path, the same three arms
// as BenchmarkLabelRead.
func BenchmarkPropRead(b *testing.B) {
	const size = 100000
	build := func(arm bool) (*Graph[string, float64], graph.NodeID) {
		g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		// Seed DISARMED, then arm — the same correction as the label fixture and
		// for the same reason: EnablePropDeltas is a no-op on a graph from New,
		// because armMVCC already set propDeltas and runs by default. Seeding
		// with it on recorded a delta per node, so this arm's "no live delta"
		// precondition observed whatever the reclaimer had not yet swept (rmp
		// #2623). See benchLabelGraphAt for the measurements.
		g.disarmMVCCForTest()
		for i := 0; i < size; i++ {
			k := fmt.Sprintf("n%d", i)
			if err := g.AddNode(k); err != nil {
				b.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeProperty(k, "w", Int64Value(int64(i))); err != nil {
				b.Fatalf("SetNodeProperty: %v", err)
			}
		}
		if arm {
			g.armMVCC()
		}
		id, ok := g.adj.Mapper().Lookup("n0")
		if !ok {
			b.Fatal("n0 not interned")
		}
		return g, id
	}
	b.Run("deltas=off", func(b *testing.B) {
		g, id := build(false)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			bag := g.propBagPlain(id)
			if bag.len() == 0 {
				b.Fatal("empty bag")
			}
		}
	})
	b.Run("deltas=on/no-live-delta", func(b *testing.B) {
		g, id := build(true)
		if g.PropDeltaCount() != 0 {
			b.Fatalf("fixture left %d live deltas", g.PropDeltaCount())
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			bag := g.propBagAsOf(id, ^uint64(0)>>1, 0)
			if bag.len() == 0 {
				b.Fatal("empty bag")
			}
		}
	})
	b.Run("deltas=on/one-live-delta", func(b *testing.B) {
		g, id := build(true)
		if err := g.SetNodeProperty("n0", "w", Int64Value(999)); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
		if g.PropDeltaCount() != 1 {
			b.Fatalf("expected one live delta, got %d", g.PropDeltaCount())
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			bag := g.propBagAsOf(id, 0, 0)
			if bag.len() == 0 {
				b.Fatal("empty bag")
			}
		}
	})
}
