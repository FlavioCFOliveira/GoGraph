package lpg

// bulkload_bracket_test.go — rmp #2395. The 2026-08-10 profile sweep found that
// an unbracketed bulk load clones the touched shard's slot array once PER EDGE
// (50.75% of every object allocated by examples/01_basic) and filed it as
// "lpg.Graph exposes no commit window at all, so the public API cannot reach the
// fast path".
//
// That premise was WRONG, and these tests are what keeps it from being believed
// again: ApplyAtomically already opens the adjacency commit window for the whole
// callback, so the fast path is public, scoped, and cannot leak. The tests pin
// the two properties the godoc now promises —
//
//  1. bracketing allocates strictly FEWER objects than the same build unwrapped;
//  2. it changes cost and NOT content, verified by an order-sensitive
//     fingerprint over every out-neighbour and weight rather than by edge count
//     alone.
//
// A DIRECTION-ONLY assertion ("bracketed allocates less") was tried first and
// REJECTED, because it cannot fail for the right reason. Bracketing wins for TWO
// independent reasons, and forcing adjlist.storeEntry's inWindow to false
// separates them on this fixture (6 000 edges, 3 rounds, spread +/-0.001):
//
//	both mechanisms live          70 923 -> 53 784 objects   0.758x
//	slot-array dedup disabled     70 923 -> 65 330 objects   0.921x
//
// So about two thirds of the saving (11 546 objects, ~1.9 clones per edge) is
// the adjacency's clone-once dedup, and the remaining third (5 593 objects) is
// one shared MVCC commit record instead of a fresh one per write. A
// direction-only test therefore still PASSED with the dedup removed — the
// commit-record saving alone kept bracketed under unbracketed. The threshold
// below exists to close that hole, and it is set between the two measured
// regimes rather than near either, so it separates them structurally instead of
// chasing noise.
//
// Layer: short.

import (
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// bulkLoadFixture is the shape examples/01 and examples/26 build: a directed
// multigraph loaded one labelled, propertied edge at a time. Kept small enough
// for the short layer while still touching every property shard.
const (
	bulkLoadNodes = 400
	bulkLoadEdges = 6000
)

func bulkLoadKey(i int) string {
	const digits = "0123456789"
	return "n" + string([]byte{
		digits[(i/1000)%10], digits[(i/100)%10], digits[(i/10)%10], digits[i%10],
	})
}

// buildBulk loads the fixture, optionally wrapped in one commit window, and
// returns the graph. The two arms differ in NOTHING but the bracket.
func buildBulk(t *testing.T, bracketed bool) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < bulkLoadNodes; i++ {
		if err := g.AddNode(bulkLoadKey(i)); err != nil {
			t.Fatalf("AddNode %d: %v", i, err)
		}
	}
	load := func() error {
		for i := 0; i < bulkLoadEdges; i++ {
			src := bulkLoadKey(i % bulkLoadNodes)
			dst := bulkLoadKey((i*7 + 11) % bulkLoadNodes)
			if src == dst {
				continue
			}
			if err := g.AddEdgeLabeledWithProperty(src, dst, float64(i%17), "FRIEND", "since", Int64Value(int64(2000+i%25))); err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if bracketed {
		err = g.ApplyAtomically(load)
	} else {
		err = load()
	}
	if err != nil {
		t.Fatalf("load(bracketed=%v): %v", bracketed, err)
	}
	return g
}

// bulkFingerprint digests the whole adjacency in node-key order: every
// out-neighbour key and weight, in CSR slot order. Any difference in edges,
// order or weights changes it. It also counts the edges walked, so a build that
// silently lost edges cannot pass on a coincidental digest match.
func bulkFingerprint(g *Graph[string, float64]) (uint64, int) {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	count := 0
	mix := func(v uint64) {
		h ^= v
		h *= prime
	}
	for i := 0; i < bulkLoadNodes; i++ {
		k := bulkLoadKey(i)
		mix(uint64(i))
		for dst, w := range g.AdjList().Neighbours(k) {
			for j := 0; j < len(dst); j++ {
				mix(uint64(dst[j]))
			}
			mix(uint64(int64(w * 1000)))
			count++
		}
	}
	return h, count
}

// objectsFor loads the fixture once and reports how many objects the load
// allocated. The fingerprint is taken AFTER the second MemStats read so its own
// allocation is never charged to the arm.
func objectsFor(t *testing.T, bracketed bool) (objects, fp uint64, edges int) {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	g := buildBulk(t, bracketed)
	runtime.ReadMemStats(&after)
	fp, edges = bulkFingerprint(g)
	runtime.KeepAlive(g)
	return after.Mallocs - before.Mallocs, fp, edges
}

// TestApplyAtomically_BulkLoadAllocatesLess pins that the bracket is reachable
// from the public API and that reaching it removes allocation. Both arms run in
// ONE process, interleaved and never overlapping, so a host-share difference
// cannot explain the gap.
func TestApplyAtomically_BulkLoadAllocatesLess(t *testing.T) {
	// Not parallel: the arms are compared on process-wide allocation counters, so
	// a sibling test allocating concurrently would corrupt the measurement.
	const rounds = 3

	// maxRatio sits between the two regimes measured by injection (0.758x with
	// the slot-array dedup live, 0.921x with only the shared commit record), so
	// losing the dedup fails this test instead of sliding past it.
	const maxRatio = 0.85

	var bracketWins int
	var lastPlain, lastBracket uint64
	var worstRatio float64
	var wantFP uint64
	var wantEdges int

	for r := 0; r < rounds; r++ {
		plainObjs, plainFP, plainEdges := objectsFor(t, false)
		brktObjs, brktFP, brktEdges := objectsFor(t, true)

		if r == 0 {
			wantFP, wantEdges = plainFP, plainEdges
			if wantEdges == 0 {
				t.Fatal("fixture built no edges: the comparison would be vacuous")
			}
		}
		// Cost, not content — checked every round and for both arms.
		for name, got := range map[string]struct {
			fp    uint64
			edges int
		}{"unbracketed": {plainFP, plainEdges}, "bracketed": {brktFP, brktEdges}} {
			if got.fp != wantFP || got.edges != wantEdges {
				t.Fatalf("round %d %s arm changed CONTENT: fingerprint %#x edges %d, want %#x / %d",
					r, name, got.fp, got.edges, wantFP, wantEdges)
			}
		}

		ratio := float64(brktObjs) / float64(plainObjs)
		if ratio < maxRatio {
			bracketWins++
		}
		if ratio > worstRatio {
			worstRatio = ratio
		}
		lastPlain, lastBracket = plainObjs, brktObjs
		t.Logf("round %d: unbracketed %d objects, bracketed %d objects (%.3fx)",
			r, plainObjs, brktObjs, ratio)
	}

	// Every round must favour the bracket. The saving is a removed per-edge
	// clone, so a single round going the other way means the writes inside fn
	// stopped presenting a builder owner to the adjacency — which is the
	// regression this test exists to catch.
	if bracketWins != rounds {
		t.Fatalf("bracketed load stayed under %.2fx of unbracketed in only %d of %d rounds "+
			"(worst %.3fx; last: unbracketed %d, bracketed %d). A ratio near 0.92x means the "+
			"shared commit record is still saving but the SLOT-ARRAY dedup is not: writes "+
			"inside ApplyAtomically are no longer presenting a non-zero builder owner to "+
			"adjlist.storeEntry, so each one clones the shard's slot array again",
			maxRatio, bracketWins, rounds, worstRatio, lastPlain, lastBracket)
	}
}

// TestApplyAtomicallyTx_AlsoOpensTheWindow pins the godoc's claim that the
// transaction-threading variant is not a slower alternative: it goes through the
// same openWriteBracket, so a caller that needs one commit instant does not have
// to give up the allocation win to get it.
func TestApplyAtomicallyTx_AlsoOpensTheWindow(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats

	// Unbracketed reference.
	runtime.ReadMemStats(&before)
	plain := buildBulk(t, false)
	runtime.ReadMemStats(&after)
	plainObjs := after.Mallocs - before.Mallocs
	wantFP, wantEdges := bulkFingerprint(plain)
	runtime.KeepAlive(plain)

	// ApplyAtomicallyTx arm, built identically but through the Tx bracket.
	runtime.GC()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < bulkLoadNodes; i++ {
		if err := g.AddNode(bulkLoadKey(i)); err != nil {
			t.Fatalf("AddNode %d: %v", i, err)
		}
	}
	runtime.ReadMemStats(&before)
	err := g.ApplyAtomicallyTx(func(WriteTx) error {
		for i := 0; i < bulkLoadEdges; i++ {
			src := bulkLoadKey(i % bulkLoadNodes)
			dst := bulkLoadKey((i*7 + 11) % bulkLoadNodes)
			if src == dst {
				continue
			}
			if err := g.AddEdgeLabeledWithProperty(src, dst, float64(i%17), "FRIEND", "since", Int64Value(int64(2000+i%25))); err != nil {
				return err
			}
		}
		return nil
	})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("ApplyAtomicallyTx: %v", err)
	}
	txObjs := after.Mallocs - before.Mallocs
	gotFP, gotEdges := bulkFingerprint(g)
	runtime.KeepAlive(g)

	if gotFP != wantFP || gotEdges != wantEdges {
		t.Fatalf("ApplyAtomicallyTx changed CONTENT: fingerprint %#x edges %d, want %#x / %d",
			gotFP, gotEdges, wantFP, wantEdges)
	}
	// Same threshold and same reason as TestApplyAtomically_BulkLoadAllocatesLess:
	// "fewer than unbracketed" is satisfied by the shared commit record alone, so
	// it cannot detect the loss of the slot-array dedup.
	if ratio := float64(txObjs) / float64(plainObjs); ratio >= 0.85 {
		t.Fatalf("ApplyAtomicallyTx allocated %d objects vs %d unbracketed (%.3fx): it must "+
			"present a builder owner to adjlist.storeEntry exactly as ApplyAtomically does, "+
			"so a caller needing one commit instant does not pay per-edge slot-array clones",
			txObjs, plainObjs, ratio)
	}
	t.Logf("unbracketed %d objects, ApplyAtomicallyTx %d objects (%.3fx)",
		plainObjs, txObjs, float64(txObjs)/float64(plainObjs))
}
