//go:build soak || nightly

package search_test

import (
	"runtime"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// security_shardflood_envelope_soak_test.go is the soak-layer regression
// envelope for the mapper NodeID-space amplification gap (rmp #1474). It
// loads a key set that all collides on mapper shard 0 — so the resulting
// CSR has a tiny live order but a MaxNodeID() inflated 256× — and runs
// two analytics on it:
//
//   - search.TransitiveClosure, which formerly sized its reachability
//     bitset on MaxNodeID() (the AMPLIFIED path, O(MaxNodeID()^2 / 8) =
//     256^2 = 65,536× the necessary memory). After the #1474 fix it
//     compacts through CSR.LiveMask() into a dense [0, live) space, so
//     its bitset is O(order^2 / 8). This test now asserts that TIGHT,
//     compacted envelope.
//   - search.WCC on the same graph, as a second compacted analytic that
//     must complete within the same tight envelope.
//
// SECURITY-FIX #1474: TransitiveClosure and WCC now size their working
// buffers on the live node count (via CSR.LiveMask compaction), not on
// MaxNodeID(). A shard-flooded graph therefore drives only an O(order)
// / O(order^2) allocation, independent of the 256× NodeID inflation.
// The structural inflation factor itself is still pinned exactly by
// graph.TestSec_Core_MapperShardAmplificationFactor; here we prove the
// downstream allocation no longer tracks it.
//
// It lives in package search_test (external) because internal/shapegen
// imports graph and would form an import cycle if used from package
// search internals.
func TestSec_Core_ShardFloodAnalyzeEnvelope(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	// realOrder is the count of live nodes. A compacted TransitiveClosure
	// needs only order^2 / 8 bytes for its bitset, regardless of the 256×
	// NodeID inflation:
	//
	//	compacted TC bitset bytes = order^2 / 8
	//
	// We pick a moderately large order so the compacted footprint is
	// measurable yet tiny, and so an accidental regression back to the
	// MaxNodeID()-sized allocation (order*256 squared / 8) would blow the
	// tight ceiling by orders of magnitude and fail loudly.
	const realOrder = 2048

	keys := shapegen.GenerateShardZeroKeys(realOrder)
	if len(keys) < realOrder {
		t.Fatalf("GenerateShardZeroKeys(%d) returned %d keys", realOrder, len(keys))
	}
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	// A directed chain over the shard-0 keys: live, weakly connected,
	// every node reachable from its predecessor.
	for i := 0; i+1 < realOrder; i++ {
		if err := a.AddEdge(keys[i], keys[i+1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	order := int(c.Order())
	maxID := uint64(c.MaxNodeID())
	wantMax := uint64(realOrder) * uint64(graph.MapperShardCount())
	if maxID != wantMax {
		t.Fatalf("MaxNodeID() = %d, want %d (%dx amplification precondition)",
			maxID, wantMax, graph.MapperShardCount())
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	// TransitiveClosure: the formerly-amplified path, now compacted. It
	// must complete and report correct reachability (every later node
	// reachable from node 0 along the chain) while allocating only the
	// compact O(order^2) bitset — NOT the O(MaxNodeID()^2) one.
	tc := search.TransitiveClosure(c)
	src, ok := a.Mapper().Lookup(keys[0])
	if !ok {
		t.Fatal("source key not interned")
	}
	dst, ok := a.Mapper().Lookup(keys[realOrder-1])
	if !ok {
		t.Fatal("sink key not interned")
	}
	if !tc.Reachable(src, dst) {
		t.Fatal("TransitiveClosure: chain head must reach chain tail")
	}

	// WCC on the same graph: a second compacted analytic that must still
	// report the chain as a single weakly-connected component.
	if _, k, err := search.WCC(c); err != nil || k != 1 {
		t.Fatalf("WCC on shard-flood chain: k=%d err=%v, want k=1 err=nil", k, err)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// The TIGHT compacted envelope. TransitiveClosure's bitset is now
	// order^2 / 8 bytes (the live-set dimension), plus O(order) scratch
	// for the compaction map and O(order) for WCC's Union-Find. We assert
	// the heap growth stays within a small multiple of that COMPACTED
	// figure. Critically, this ceiling is far below the old amplified
	// footprint: a regression that re-sized the bitset on MaxNodeID()
	// would need (order*256)^2 / 8 bytes — 65,536× larger — and would
	// blow this ceiling immediately.
	//
	// The bound keeps generous slack (4x the bitset + 16 MiB) so the test
	// stays robust to GC timing on shared CI runners, per the project's
	// guidance that MemStats deltas are noisy; the exact structural factor
	// is pinned by graph.TestSec_Core_MapperShardAmplificationFactor.
	compactBitsetBytes := (uint64(order) * uint64(order)) / 8
	amplifiedBitsetBytes := (maxID * maxID) / 8
	tightCeilingBytes := compactBitsetBytes*4 + (16 << 20) // 4x compact bitset + 16 MiB slack
	// Sanity: the tight ceiling must be far below what a regression to the
	// amplified allocation would require, otherwise the test is toothless.
	if tightCeilingBytes >= amplifiedBitsetBytes {
		t.Fatalf("tight ceiling %d not below amplified footprint %d; test would not catch a regression",
			tightCeilingBytes, amplifiedBitsetBytes)
	}
	if after.HeapAlloc > before.HeapAlloc {
		delta := after.HeapAlloc - before.HeapAlloc
		if delta > tightCeilingBytes {
			t.Fatalf("heap delta = %d MiB exceeds COMPACTED envelope ceiling %d MiB "+
				"(order=%d, compact bitset ≈ %d KiB) — suspect a regression back to "+
				"MaxNodeID()-sized allocation (amplified bitset would be ≈ %d MiB)",
				delta>>20, tightCeilingBytes>>20, order, compactBitsetBytes>>10,
				amplifiedBitsetBytes>>20)
		}
		t.Logf("shard-flood COMPACTED envelope: order=%d MaxNodeID=%d heap delta=%d KiB "+
			"(compact bitset ≈ %d KiB; the old amplified bitset would have needed ≈ %d MiB) [SECURITY-FIX #1474]",
			order, maxID, delta>>10, compactBitsetBytes>>10, amplifiedBitsetBytes>>20)
	}
}
