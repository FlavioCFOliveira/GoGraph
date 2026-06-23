//go:build gograph_crashinject

package recovery

// Durability proof for the per-instance (by-handle) edge-property store
// maintained on a relationship SET / REMOVE (#1686). It drives the
// crashinject-helper to SIGKILL itself AFTER a durable per-instance mutation
// frame, so it compiles only under the gograph_crashinject build tag. Run with:
// go test -tags gograph_crashinject ./store/recovery/...
//
// The scenarios commit two parallel edges between the same ordered (src, dst)
// pair — each with its own stable handle and a distinct per-instance `w`
// property — then make a durable per-instance mutation on the FIRST handle only
// and crash. Recovery over the resulting WAL must land on the post-mutation
// state (durable-iff-acked), with the sibling instance untouched, exactly two
// parallel edges (no doubling, no handle re-mint), proving the
// OpSetEdgePropertyByHandle / OpDelEdgePropertyByHandle WAL records replay
// correctly across a kill -9.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// These mirror the constants in cmd/crashinject-helper/main.go.
const (
	ehSrcKey = "src"
	ehDstKey = "dst"
	ehH1     = uint64(1)
	ehH2     = uint64(2)
)

// recoverEdgeHandleProps opens the crashed WAL and returns, for the
// (src, dst) pair, the per-instance property map keyed by stable handle, plus
// the count of parallel edges (live handles) on that pair.
func recoverEdgeHandleProps(t *testing.T, dir string) (perInstance map[uint64]map[string]lpg.PropertyValue, parallelEdges int) {
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
	// Count parallel adjacency slots between the pair (no doubling check).
	for nb := range g.AdjList().Neighbours(ehSrcKey) {
		if nb == ehDstKey {
			parallelEdges++
		}
	}
	return perInstance, parallelEdges
}

// runEdgeHandleCrash runs the named scenario, asserts the child was SIGKILL'd,
// and returns the recovered per-instance state.
func runEdgeHandleCrash(t *testing.T, scenario string) (map[uint64]map[string]lpg.PropertyValue, int) {
	t.Helper()
	out, err := crashinject.Run(t, scenario, crashinject.Opts{})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s\nstdout: %s\nstderr: %s", scenario, out.Stdout, out.Stderr)
	}
	return recoverEdgeHandleProps(t, out.Dir)
}

// TestEdgeHandleSetPropCrash_PostWALSync proves a durable
// OpSetEdgePropertyByHandle survives a kill -9: after recovery the SET property
// is on the first handle only, the sibling is untouched, and exactly two
// parallel edges exist (no doubling / no handle re-mint).
func TestEdgeHandleSetPropCrash_PostWALSync(t *testing.T) {
	perInstance, parallel := runEdgeHandleCrash(t, "edgehandle.setprop.post-wal-sync")

	if parallel != 2 {
		t.Fatalf("recovered %d parallel edges, want 2 (no doubling)", parallel)
	}
	if len(perInstance) != 2 {
		t.Fatalf("recovered %d by-handle instances, want 2: %+v", len(perInstance), perInstance)
	}
	// h1 carries tag='set' AND its own w=1; h2 carries only w=2.
	h1 := perInstance[ehH1]
	h2 := perInstance[ehH2]
	if h1 == nil || h2 == nil {
		t.Fatalf("missing a handle instance after recovery: h1=%v h2=%v", h1, h2)
	}
	if v, ok := h1["tag"]; !ok {
		t.Fatalf("durable SET lost: h1 has no tag after recovery: %v", h1)
	} else if s, _ := v.String(); s != "set" {
		t.Fatalf("h1 tag = %q, want set", s)
	}
	if w, _ := h1["w"].Int64(); w != 1 {
		t.Fatalf("h1 w = %d, want 1", w)
	}
	if _, ok := h2["tag"]; ok {
		t.Fatalf("sibling h2 corrupted with tag after a SET on h1: %v", h2)
	}
	if w, _ := h2["w"].Int64(); w != 2 {
		t.Fatalf("h2 w = %d, want 2 (sibling untouched)", w)
	}
}

// TestEdgeHandleDelPropCrash_PostWALSync proves a durable
// OpDelEdgePropertyByHandle survives a kill -9: after recovery tag is absent on
// the first handle (it was seeded at CREATE then durably removed), the sibling
// keeps its own state, and exactly two parallel edges exist.
func TestEdgeHandleDelPropCrash_PostWALSync(t *testing.T) {
	perInstance, parallel := runEdgeHandleCrash(t, "edgehandle.delprop.post-wal-sync")

	if parallel != 2 {
		t.Fatalf("recovered %d parallel edges, want 2 (no doubling)", parallel)
	}
	h1 := perInstance[ehH1]
	h2 := perInstance[ehH2]
	if h1 == nil || h2 == nil {
		t.Fatalf("missing a handle instance after recovery: h1=%v h2=%v", h1, h2)
	}
	if _, ok := h1["tag"]; ok {
		t.Fatalf("durable DEL lost: h1 still carries tag after recovery: %v", h1)
	}
	if w, _ := h1["w"].Int64(); w != 1 {
		t.Fatalf("h1 w = %d, want 1 (own state intact after DEL of tag)", w)
	}
	if w, _ := h2["w"].Int64(); w != 2 {
		t.Fatalf("h2 w = %d, want 2 (sibling untouched)", w)
	}
}
