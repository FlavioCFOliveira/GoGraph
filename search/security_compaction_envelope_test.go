package search_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// security_compaction_envelope_test.go is part of the GoGraph security
// test battery. It is a DEFENSE lock-in for the live-mask compaction that
// shields the dense-matrix algorithms ([search.FloydWarshall],
// [search.JohnsonAPSP]) from the mapper NodeID-space amplification gap
// (rmp #1474).
//
// When natural keys collide on a single mapper shard, csr.MaxNodeID()
// inflates to 256× the real node count (see graph package
// TestSec_Core_MapperShardAmplificationFactor). An O(V^2)/O(V^3)
// algorithm that sized its working matrix on MaxNodeID() would allocate
// 256^2 = 65 536× the necessary memory and compute on mostly-ghost
// vertices — a memory-amplification denial-of-service. FloydWarshall and
// Johnson defend against this by compacting through csr.LiveMask() so
// their matrix dimension is the live node count, not MaxNodeID(). This
// test pins that the compacted dimension tracks the REAL order even when
// MaxNodeID() is amplified 256×.
//
// It lives in package search_test (external) because it depends on
// internal/shapegen, which imports graph and would form a cycle if used
// from package search internals.

// secShardFloodChainCSR builds a directed chain over n keys that all hash
// to mapper shard 0, so the resulting CSR has Order()==n live nodes but
// MaxNodeID()==256*n (a 256× sparse NodeID space). It returns the CSR
// plus its measured (order, maxID) for the caller's amplification check.
func secShardFloodChainCSR(tb testing.TB, n int) (*csr.CSR[int64], int, uint64) {
	tb.Helper()
	keys := shapegen.GenerateShardZeroKeys(n)
	if len(keys) < n {
		tb.Fatalf("GenerateShardZeroKeys(%d) returned %d keys", n, len(keys))
	}
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i+1 < n; i++ {
		if err := a.AddEdge(keys[i], keys[i+1], int64(1)); err != nil {
			tb.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	return c, int(c.Order()), uint64(c.MaxNodeID())
}

// TestSec_Core_FloydWarshallCompactsAmplifiedNodeSpace pins that
// FloydWarshall's APSP matrix dimension (APSP.N()) equals the live node
// count, not the 256×-amplified MaxNodeID(), on a shard-flooded graph.
//
// A regression that dropped the LiveMask compaction and sized the matrix
// on MaxNodeID() would make N() jump to 256× and the dist slice grow by
// 256^2 — caught here.
func TestSec_Core_FloydWarshallCompactsAmplifiedNodeSpace(t *testing.T) {
	t.Parallel()

	const n = 64 // small real order; MaxNodeID() will be 64*256 = 16384.
	c, order, maxID := secShardFloodChainCSR(t, n)

	// Precondition: the node space really is amplified by the shard count.
	if order != n {
		t.Fatalf("live order = %d, want %d", order, n)
	}
	wantMax := uint64(n) * uint64(graph.MapperShardCount())
	if maxID != wantMax {
		t.Fatalf("MaxNodeID() = %d, want %d (%dx amplification precondition)",
			maxID, wantMax, graph.MapperShardCount())
	}

	apsp := search.FloydWarshall(c)
	if apsp.N() != order {
		t.Fatalf("APSP.N() = %d, want live order %d (MaxNodeID was %d). "+
			"The live-mask compaction must keep the matrix dimension at the "+
			"real node count, not the %dx-amplified MaxNodeID.",
			apsp.N(), order, maxID, graph.MapperShardCount())
	}
	t.Logf("FloydWarshall compacted: APSP.N()=%d == Order()=%d (MaxNodeID()=%d, %.0fx amplified)",
		apsp.N(), order, maxID, float64(maxID)/float64(order))
}

// TestSec_Core_JohnsonCompactsAmplifiedNodeSpace pins the same
// compaction guarantee for JohnsonAPSP on the amplified node space.
func TestSec_Core_JohnsonCompactsAmplifiedNodeSpace(t *testing.T) {
	t.Parallel()

	const n = 64
	c, order, maxID := secShardFloodChainCSR(t, n)
	wantMax := uint64(n) * uint64(graph.MapperShardCount())
	if maxID != wantMax {
		t.Fatalf("MaxNodeID() = %d, want %d (amplification precondition)", maxID, wantMax)
	}

	apsp, err := search.JohnsonAPSP(c)
	if err != nil {
		t.Fatalf("JohnsonAPSP: unexpected error %v", err)
	}
	if apsp.N() != order {
		t.Fatalf("JohnsonAPSP APSP.N() = %d, want live order %d (MaxNodeID %d): "+
			"compaction regressed", apsp.N(), order, maxID)
	}
	t.Logf("JohnsonAPSP compacted: APSP.N()=%d == Order()=%d (MaxNodeID()=%d)", apsp.N(), order, maxID)
}

// TestSec_Core_WCCCompactsAmplifiedComponentLabels pins that WCC reports
// exactly one component (the chain is weakly connected) and that its
// component-label space is compacted to the live nodes — k == 1, not a
// count inflated by the 256× ghost slots. Although WCC's internal
// Union-Find universe is sized on MaxNodeID (a separate concern tracked
// by the UnionFindSlice int32 gap #1476), its OUTPUT labelling is
// compacted via LiveMask, which is what this asserts.
func TestSec_Core_WCCCompactsAmplifiedComponentLabels(t *testing.T) {
	t.Parallel()

	const n = 64
	c, order, maxID := secShardFloodChainCSR(t, n)

	component, k, err := search.WCC(c)
	if err != nil {
		t.Fatalf("WCC: unexpected error %v", err)
	}
	if k != 1 {
		t.Fatalf("WCC k = %d, want 1 (a directed chain is weakly connected)", k)
	}
	// Exactly `order` live nodes carry a real component id in [0,k); the
	// rest of the MaxNodeID-length slice are ghost slots labelled -1.
	live := 0
	for _, c := range component {
		if c >= 0 {
			live++
		}
	}
	if live != order {
		t.Fatalf("WCC labelled %d live nodes, want %d (component slice length %d == MaxNodeID %d)",
			live, order, len(component), maxID)
	}
}
