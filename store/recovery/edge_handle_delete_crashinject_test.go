//go:build gograph_crashinject

package recovery

// Durability proof for an instance-precise parallel-edge DELETE (rmp #2018). It
// drives the crashinject-helper to SIGKILL itself AFTER a durable
// OpRemoveEdgeByHandle frame, so it compiles only under the gograph_crashinject
// build tag. Run with:
// go test -tags gograph_crashinject ./store/recovery/...
//
// The scenario commits two parallel edges between the same ordered (src, dst)
// pair — each with its own stable handle and a distinct per-instance `w`
// property — then durably removes the SECOND handle (h2) by handle and crashes.
// Recovery over the resulting WAL must land on exactly ONE parallel edge — the
// FIRST handle (h1) with its own w=1 intact — proving the durable removal
// retired the EXACT bound instance (not the first-match slot) across a kill -9.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// recoverEdgeHandleDelete opens the crashed WAL and returns, for the (src, dst)
// pair, the per-instance property map keyed by stable handle plus the count of
// surviving parallel adjacency slots between the pair.
func recoverEdgeHandleDelete(t *testing.T, dir string) (perInstance map[uint64]map[string]lpg.PropertyValue, parallelEdges int) {
	t.Helper()
	res, oerr := Open[string, float64](dir, Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if oerr != nil {
		t.Fatalf("recovery.Open: %v", oerr)
	}
	g := res.Graph
	srcID, ok := g.AdjList().Mapper().Lookup(ehSrcKey)
	if !ok {
		t.Fatalf("src key %q not recovered", ehSrcKey)
	}
	dstID, ok := g.AdjList().Mapper().Lookup(ehDstKey)
	if !ok {
		t.Fatalf("dst key %q not recovered", ehDstKey)
	}
	perInstance = make(map[uint64]map[string]lpg.PropertyValue)
	g.WalkEdgeHandles(func(tr lpg.EdgeHandleTriple) bool {
		if tr.Src == srcID && tr.Dst == dstID {
			perInstance[tr.Handle] = g.EdgePropertiesByHandleID(srcID, dstID, tr.Handle)
		}
		return true
	})
	for nb := range g.AdjList().Neighbours(ehSrcKey) {
		if nb == ehDstKey {
			parallelEdges++
		}
	}
	return perInstance, parallelEdges
}

// TestEdgeHandleDeleteCrash_PostWALSync proves a durable OpRemoveEdgeByHandle
// survives a kill -9: after recovery exactly ONE parallel edge remains — the
// FIRST handle (h1) with its own w=1 — and the removed SECOND handle (h2) is
// gone. Pre-fix this op kind did not exist; the removal path removed the
// first-match slot, so recovery would have retired h1 and left h2.
func TestEdgeHandleDeleteCrash_PostWALSync(t *testing.T) {
	perInstance, parallel := runEdgeHandleCrash(t, "edgehandle.delete.post-wal-sync")

	if parallel != 1 {
		t.Fatalf("recovered %d parallel edges, want 1 (exactly the bound instance removed)", parallel)
	}
	if len(perInstance) != 1 {
		t.Fatalf("recovered %d by-handle instances, want 1: %+v", len(perInstance), perInstance)
	}
	h1 := perInstance[ehH1]
	if h1 == nil {
		t.Fatalf("surviving instance is not h1 (the first handle): %+v", perInstance)
	}
	if w, _ := h1["w"].Int64(); w != 1 {
		t.Fatalf("surviving h1 w = %d, want 1 (own state intact)", w)
	}
	if _, ok := perInstance[ehH2]; ok {
		t.Fatalf("removed instance h2 survived recovery: %+v", perInstance)
	}
}
