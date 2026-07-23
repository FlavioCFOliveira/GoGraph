package cypher

// count_durability_test.go — reopen/recovery parity gate for the derived
// relationship count-store (task #2084, design docs/count-store-design.md §6).
//
// The count-store is DERIVED and NON-DURABLE: it has no WAL op, no checkpoint
// component and no fsync, and it starts EMPTY on every open. Correctness after a
// reopen is therefore guaranteed only by recomputeCountStore, an O(V+E) pass
// that rebuilds the store from the recovered graph at engine construction. These
// tests prove that pass:
//
//   - after a plain WAL-replay reopen,
//   - after a checkpoint (snapshot + WAL truncate) reopen,
//   - after a checkpoint followed by a WAL tail (snapshot + replay) reopen,
//
// the reopened store equals a fresh ground-truth O(V+E) recount of the recovered
// graph on EVERY cell (E, D, T exact) with ZERO dirty markings — i.e. any cell
// the prior session left dirty (an X-scoped relabel degradation) self-heals on
// reopen. Because the store is a pure function of the crash-consistent recovered
// graph, it cannot diverge regardless of where a crash fell; that is what
// "recompute from the recovered graph" buys, and it is verified here rather than
// asserted.
//
// These live in package cypher (not cypher_test) to reuse the internal
// ground-truth oracle recount / assertCountsMatch / cs / idOf / mustRun from
// count_maintenance_test.go. Goroutine-leak coverage is the package-level
// goleak.VerifyTestMain in testmain_test.go (every WAL/store here is local and
// closed). Layer: short.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// countDurWorkload is the session-1 write workload shared by the reopen-parity
// tests. It exercises multi-label nodes (:Person:Temp, :Person:Target), several
// relationship types (KNOWS, WORKS_AT, FOUNDED), parallel edges of DIFFERING type
// between one ordered pair (a-[:WORKS_AT]->c and a-[:FOUNDED]->c), and both a SET
// and a REMOVE label that dirty the X-scoped IN cells (design §3.3.1). The known
// live edge multiset it leaves is:
//
//	KNOWS: 4   WORKS_AT: 2   FOUNDED: 1   (total 7)
//
// and the relabelled :Target node (a KNOWS destination) gains :VIP, so after a
// recompute D(VIP,KNOWS,IN) == 1 and T(Person,KNOWS,VIP) == 1 — non-zero cells
// that were DIRTY in session 1 and must be EXACT after reopen.
var countDurWorkload = []string{
	`CREATE (:Person)-[:KNOWS]->(:Person)`,
	`CREATE (:Person)-[:KNOWS]->(:Person)`,
	`CREATE (:Person)-[:WORKS_AT]->(:Company)`,
	`CREATE (:Person:Temp)-[:KNOWS]->(:Person)`,
	`CREATE (:Person)-[:KNOWS]->(:Person:Target)`,
	// Parallel edges of differing type between the same ordered (Person, Company).
	`MATCH (a:Person),(c:Company) WITH a, c LIMIT 1 CREATE (a)-[:WORKS_AT]->(c),(a)-[:FOUNDED]->(c)`,
	// Relabel a KNOWS-destination: dirties D(VIP,*,IN) and T(*,*,VIP) (IN X-scoped).
	`MATCH (t:Target) SET t:VIP`,
	// Remove a label from a KNOWS-source: also dirties D(Temp,*,IN), T(*,*,Temp).
	`MATCH (n:Temp) REMOVE n:Temp`,
}

func countDurRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

func countDurStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// countRunCycle opens a WAL-backed engine over g, runs each query to completion,
// captures the engine's count-store snapshot BEFORE closing (so a caller can
// assert the in-session dirty state), optionally writes a full snapshot and
// truncates the WAL (the checkpoint path), fsyncs, and closes the WAL. The
// returned snapshot is the session's final count-store image.
func countRunCycle(t *testing.T, dir string, g *lpg.Graph[string, float64], checkpoint bool, queries ...string) count.Snapshot {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	store := txn.NewStoreWithOptions[string, float64](g, w, countDurStoreOpts())
	eng := NewEngineWithStore(store)
	for _, q := range queries {
		mustRun(t, eng, q)
	}
	snap := cs(eng).Snapshot()
	if checkpoint {
		csrGraph := csr.BuildFromAdjList(g.AdjList())
		if werr := snapshot.WriteSnapshotFullWithMapperCodec(
			filepath.Join(dir, "snapshot"), csrGraph, g, txn.NewStringCodec(),
		); werr != nil {
			t.Fatalf("WriteSnapshotFullWithMapperCodec: %v", werr)
		}
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("wal.Sync: %v", serr)
	}
	if checkpoint {
		if _, terr := w.Truncate(); terr != nil {
			t.Fatalf("wal.Truncate: %v", terr)
		}
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("wal.Close: %v", cerr)
	}
	return snap
}

// countReopen recovers dir into a fresh WAL-backed engine (the recommended
// schema-aware constructor) and returns it with the recovered graph and a
// cleanup that closes the WAL. The recompute runs during NewEngineWithStore*.
func countReopen(t *testing.T, dir string) (*Engine, *lpg.Graph[string, float64], func()) {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, countDurRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("reopen wal.Open: %v", err)
	}
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, countDurStoreOpts())
	eng := NewEngineWithStoreAndSchema(store, res.Constraints, res.Indexes)
	return eng, res.Graph, func() {
		if cerr := w.Close(); cerr != nil {
			t.Errorf("reopen wal.Close: %v", cerr)
		}
	}
}

// assertSession1WasDirty confirms the workload actually degraded a cell in the
// first session, so the "self-heals on reopen" assertion is not vacuous. The
// :Target node's SET :VIP dirties the X-scoped IN cells for VIP.
func assertSession1WasDirty(t *testing.T, snap1 *count.Snapshot, g *lpg.Graph[string, float64]) {
	t.Helper()
	vip := idOf(t, g, "VIP")
	if !setOf(snap1.DirtyDIn)[vip] {
		t.Fatalf("session 1 should have dirtied D(VIP,*,IN); DirtyDIn=%v", snap1.DirtyDIn)
	}
	if !setOf(snap1.DirtyTB)[vip] {
		t.Fatalf("session 1 should have dirtied T(*,*,VIP); DirtyTB=%v", snap1.DirtyTB)
	}
}

// assertStoreExactAndClean is the generic post-reopen oracle: the store carries
// zero dirty markings, equals a fresh independent O(V+E) recount of the recovered
// graph on EVERY cell (zero dirty ⇒ every cell compared), is non-empty (recovery
// kept the graph and the recompute ran), and its E marginal sums to the recovered
// live edge count. It is the property that must hold at every durability boundary
// — i.e. at every point a crash could have fallen.
func assertStoreExactAndClean(t *testing.T, eng *Engine, g *lpg.Graph[string, float64]) {
	t.Helper()
	store := cs(eng)
	got := store.Snapshot()

	if n := len(got.DirtyDOut) + len(got.DirtyDIn) + len(got.DirtyTA) + len(got.DirtyTB); n != 0 {
		t.Fatalf("reopened count-store has %d dirty markings, want 0 (recompute must heal every cell): %+v", n, got)
	}
	assertCountsMatch(t, store, g)
	if len(got.E) == 0 {
		t.Fatalf("reopened count-store is empty; recovery lost the graph or the recompute did not run")
	}
	var sumE int64
	for _, v := range got.E {
		sumE += v
	}
	if uint64(sumE) != g.AdjList().Size() {
		t.Errorf("sum(E)=%d != recovered AdjList().Size()=%d", sumE, g.AdjList().Size())
	}
}

// assertReopenedStoreCleanAndExact layers the countDurWorkload-specific anchors
// on top of the generic oracle: the exact live-edge multiset (including the two
// parallel edges of differing type) and the two cells (D(VIP,KNOWS,IN),
// T(Person,KNOWS,VIP)) that were DIRTY in session 1 and are now healed to their
// true non-zero value.
func assertReopenedStoreCleanAndExact(t *testing.T, eng *Engine, g *lpg.Graph[string, float64]) {
	t.Helper()
	assertStoreExactAndClean(t, eng, g)
	store := cs(eng)

	// Concrete E anchors — the exact live-edge multiset the workload leaves,
	// including the two parallel edges of differing type (WORKS_AT + FOUNDED).
	knows := idOf(t, g, "KNOWS")
	worksAt := idOf(t, g, "WORKS_AT")
	founded := idOf(t, g, "FOUNDED")
	if v := store.CountE(knows); v != 4 {
		t.Errorf("E(KNOWS)=%d after reopen, want 4", v)
	}
	if v := store.CountE(worksAt); v != 2 {
		t.Errorf("E(WORKS_AT)=%d after reopen, want 2 (parallel edge lost?)", v)
	}
	if v := store.CountE(founded); v != 1 {
		t.Errorf("E(FOUNDED)=%d after reopen, want 1 (parallel edge lost?)", v)
	}

	// Healing anchors — dirty in session 1, exact after reopen. :Target (now also
	// :VIP) is the destination of exactly one KNOWS edge from a :Person.
	vip := idOf(t, g, "VIP")
	person := idOf(t, g, "Person")
	if v := store.CountD(vip, knows, count.In); v != 1 {
		t.Errorf("D(VIP,KNOWS,IN)=%d after reopen, want 1 (dirty IN cell not healed exactly)", v)
	}
	if store.DDirty(vip, count.In) {
		t.Errorf("D(VIP,*,IN) still dirty after reopen")
	}
	if v := store.CountT(person, knows, vip); v != 1 {
		t.Errorf("T(Person,KNOWS,VIP)=%d after reopen, want 1 (dirty T cell not healed exactly)", v)
	}
	if store.TDirty(person, vip) {
		t.Errorf("T(*,*,VIP) still dirty after reopen")
	}
}

// TestCountStore_ReopenParity_WALReplay is the primary acceptance criterion:
// after a plain WAL-replay reopen (no checkpoint) the recomputed count-store
// equals a ground-truth recount of the recovered graph, with zero dirty cells,
// and the cells dirtied in session 1 are healed exactly.
func TestCountStore_ReopenParity_WALReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	snap1 := countRunCycle(t, dir, g, false, countDurWorkload...)
	assertSession1WasDirty(t, &snap1, g)

	eng, rg, cleanup := countReopen(t, dir)
	defer cleanup()
	assertReopenedStoreCleanAndExact(t, eng, rg)
}

// TestCountStore_ReopenParity_AfterCheckpoint forces a checkpoint (full snapshot
// + WAL truncate) before close, so recovery rebuilds the graph from the snapshot
// rather than the WAL. The recomputed store must again match the recount exactly
// with zero dirty cells (design §6.3 — the count-store does not participate in
// the checkpoint at all).
func TestCountStore_ReopenParity_AfterCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	snap1 := countRunCycle(t, dir, g, true, countDurWorkload...)
	assertSession1WasDirty(t, &snap1, g)

	eng, rg, cleanup := countReopen(t, dir)
	defer cleanup()
	assertReopenedStoreCleanAndExact(t, eng, rg)
}

// TestCountStore_ReopenParity_CheckpointThenWALTail exercises the crash-consistent
// combination of a snapshot AND a WAL tail: a first batch is checkpointed (its
// WAL prefix truncated), a second batch is written to the WAL only, then the store
// is reopened. Recovery replays the WAL tail on top of the snapshot; the recompute
// runs over that combined state and must still yield an exact, clean store. This
// is the "writes only in the WAL, not checkpointed" crash-parity case: whatever
// crash-consistent state recovery produces, the count-store matches it.
func TestCountStore_ReopenParity_CheckpointThenWALTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Split the workload: everything up to (but excluding) the two relabels is the
	// checkpointed prefix; the SET/REMOVE relabels are the WAL-only tail.
	prefix := countDurWorkload[:6]
	tail := countDurWorkload[6:]

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	// Cycle 1: create the graph and checkpoint it (WAL prefix truncated).
	countRunCycle(t, dir, g, true, prefix...)

	// Cycle 2: recover from the snapshot, then apply the relabels to the WAL tail
	// (no new checkpoint), so the final state is snapshot + un-truncated WAL tail.
	res, err := recovery.Open[string, float64](dir, countDurRecOpts())
	if err != nil {
		t.Fatalf("cycle 2 recovery.Open: %v", err)
	}
	snap2 := countRunCycle(t, dir, res.Graph, false, tail...)
	assertSession1WasDirty(t, &snap2, res.Graph)

	// Final reopen: snapshot + WAL tail replayed together.
	eng, rg, cleanup := countReopen(t, dir)
	defer cleanup()
	assertReopenedStoreCleanAndExact(t, eng, rg)
}

// TestCountStore_ReopenParity_InterleavedBoundariesStayExact verifies design
// §6.2 "cannot diverge regardless of where the crash fell" empirically rather
// than by assertion: it drives a graph through a sequence of write cycles that
// alternate the durability boundary — WAL-only, then a checkpoint (snapshot +
// WAL truncate), then a WAL tail on top of that snapshot, then another
// checkpoint — and REOPENS at every boundary. Each boundary is a distinct point
// a crash could have fallen, and at each the recomputed count-store must equal a
// fresh independent recount of whatever crash-consistent graph recovery produced,
// with zero dirty cells. Because the store is recomputed from the recovered
// graph, this holds for every shape of recovered state.
func TestCountStore_ReopenParity_InterleavedBoundariesStayExact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// reopenAssert recovers the store, asserts recompute==recount + zero dirty,
	// then closes the WAL — one crash-boundary check.
	reopenAssert := func() {
		eng, rg, cleanup := countReopen(t, dir)
		defer cleanup()
		assertStoreExactAndClean(t, eng, rg)
	}
	// nextGraph recovers just the graph (no engine) so the next cycle can append.
	nextGraph := func() *lpg.Graph[string, float64] {
		res, err := recovery.Open[string, float64](dir, countDurRecOpts())
		if err != nil {
			t.Fatalf("recovery.Open: %v", err)
		}
		return res.Graph
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	// Cycle 1 — WAL only: seed a small typed graph.
	countRunCycle(t, dir, g, false,
		`CREATE (:A)-[:R]->(:B)`,
		`CREATE (:A)-[:R]->(:B:C)`,
	)
	reopenAssert()

	// Cycle 2 — checkpoint: add an edge + a relabel that dirties an IN cell, then
	// snapshot and truncate the WAL. The in-session dirty degradation must NOT
	// survive into the reopen (the recompute clears it).
	countRunCycle(t, dir, nextGraph(), true,
		`CREATE (:A)-[:R]->(:D)`,
		`MATCH (n:C) WITH n LIMIT 1 SET n:VIP`,
	)
	reopenAssert()

	// Cycle 3 — WAL tail on top of the checkpoint: another edge + a REMOVE label.
	countRunCycle(t, dir, nextGraph(), false,
		`CREATE (:A)-[:S]->(:B)`,
		`MATCH (n:VIP) WITH n LIMIT 1 REMOVE n:VIP`,
	)
	reopenAssert()

	// Cycle 4 — final checkpoint over the combined state.
	countRunCycle(t, dir, nextGraph(), true,
		`CREATE (:A)-[:R]->(:B)`,
	)
	reopenAssert()
}
