package cypher_test

// remove_parallel_edge_counters_test.go — regression gate for #2500: on a
// multigraph, REMOVE r.<key> pinned to one parallel-edge instance must report
// PropertiesRemoved per INSTANCE, not per (src, dst) pair. Before the fix the
// -properties gate read the per-pair aggregate store
// (lpgMutatorAdapter.DelEdgeProperty / walMutatorAdapter.DelEdgeProperty), so
// only the FIRST removal on a pair counted 1 and a later removal of a
// genuinely present property on a sibling instance counted 0 — while the
// graph STATE was per-instance and correct throughout. Found by the DST
// counters oracle (rmp #2448/#2449).
//
// Both write paths are covered: the store-less lpgMutatorAdapter and the
// WAL-backed walMutatorAdapter, which carried the same defect.
//
// Layer: short. goleak-clean (engines/graphs are local; the WAL writer is
// closed via t.Cleanup).

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// walMultigraphEngine returns a WAL-backed engine over a fresh directed
// multigraph, exercising the walMutatorAdapter write path.
func walMultigraphEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithStore(store)
}

// removePerInstanceCounters drives the #2500 shape against eng: two parallel
// KNOWS instances between the same ordered pair, each carrying `since`, then
// eid-pinned REMOVEs whose -properties must follow the TARGETED instance.
func removePerInstanceCounters(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	_ = runCounted(t, eng, `CREATE (:P {key:'x'})`)
	_ = runCounted(t, eng, `CREATE (:P {key:'y'})`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:1, since:'2026-01-01'}]->(b)`)
	_ = runCounted(t, eng,
		`MATCH (a:P {key:'x'}),(b:P {key:'y'}) CREATE (a)-[:KNOWS {eid:2, since:'2026-02-02'}]->(b)`)

	remove := func(eid string, want wantCounters) {
		q := `MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.eid = ` + eid + ` REMOVE r.since`
		assertCounters(t, q, runCounted(t, eng, q), want)
	}

	// First instance: `since` present on eid 1 — exactly one -properties.
	remove("1", wantCounters{propsRemoved: 1, containsUpdates: true})

	// Same instance again: `since` now absent on eid 1, so this is a no-op
	// and counts nothing — even though the SIBLING (eid 2) still carries it.
	remove("1", wantCounters{})

	// Sibling instance: `since` genuinely present on eid 2 — one -properties.
	// The defective per-pair gate reported 0 here (#2500).
	remove("2", wantCounters{propsRemoved: 1, containsUpdates: true})

	// State cross-check: both instances survive and both `since` values are
	// gone (the counters above are evidence about attribution, not state).
	if rows := drainOK(t, eng,
		`MATCH (:P {key:'x'})-[r:KNOWS]->(:P {key:'y'}) WHERE r.since IS NULL RETURN r.eid`); rows != 2 {
		t.Fatalf("post-REMOVE state: %d instances with since IS NULL, want 2", rows)
	}
}

// TestRemoveParallelEdge_InMemory_CountersPerInstance is the #2500 regression
// gate on the store-less write path (lpgMutatorAdapter).
func TestRemoveParallelEdge_InMemory_CountersPerInstance(t *testing.T) {
	t.Parallel()
	removePerInstanceCounters(t, counterEngine(t))
}

// TestRemoveParallelEdge_WalStore_CountersPerInstance is the #2500 regression
// gate on the durable write path (walMutatorAdapter), which carried the same
// per-pair gate.
func TestRemoveParallelEdge_WalStore_CountersPerInstance(t *testing.T) {
	t.Parallel()
	removePerInstanceCounters(t, walMultigraphEngine(t))
}
