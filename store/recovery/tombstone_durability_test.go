package recovery

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// Why the pre-existing suite missed the node-deletion durability defect:
// every DELETE test validated a SINGLE in-memory engine, where the Cypher
// AllNodesScan already filters tombstoned ids — so the deleted node never
// shows up regardless of whether the tombstone is durable. The defect lives
// ONLY across the persist→reopen boundary: the tombstone set is dropped both
// by snapshot serialisation and by WAL replay. Therefore every test here
// mutates, persists to disk, DISCARDS the in-memory graph, reopens from
// disk, and only then asserts — a test that stays in one graph instance
// would pass while the bug is live.

// recoveryOpts returns the string/int64 codec pair shared by these tests.
func tombstoneRecoveryOpts() Options[string, int64] {
	return Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
}

func tombstoneTxnOpts() txn.Options[string, int64] {
	return txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
}

// checkpointAndClose writes a self-sufficient snapshot of g, truncates the
// WAL, and closes the WAL writer — the GoGraph equivalent of one Groadmap
// `rmp graph` command's checkpoint-then-exit.
func checkpointAndClose(t *testing.T, dir string, g *lpg.Graph[string, int64], w *wal.Writer) {
	t.Helper()
	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("checkpoint Trigger: %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}

// TestRecovery_RemoveNodeReplay_Tombstones is the direct Gap-2 regression:
// a committed OpRemoveNode must reconstruct the tombstone on WAL replay,
// with NO snapshot involved. RED on the pre-fix code: applyOpCodec's
// OpRemoveNode case only re-strips labels/properties and never calls
// g.RemoveNode, so the node replays back as a live, label-less ghost.
func TestRecovery_RemoveNodeReplay_Tombstones(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())

	tx := store.Begin()
	mustTx(t, tx.AddNode("auth"))
	mustTx(t, tx.SetNodeLabel("auth", "Spec"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("create Commit: %v", err)
	}

	tx = store.Begin()
	mustTx(t, tx.RemoveNodeLabel("auth", "Spec"))
	mustTx(t, tx.RemoveNode("auth"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("delete Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Reopen from the WAL alone (no checkpoint, so no snapshot).
	res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.SnapshotHit {
		t.Fatal("SnapshotHit = true, want false (this test exercises WAL replay only)")
	}
	rg := res.Graph
	id, ok := rg.AdjList().Mapper().Lookup("auth")
	if !ok {
		t.Fatal("node auth was not interned during WAL replay")
	}
	if !rg.IsTombstoned(id) {
		t.Fatal("node auth must be tombstoned after WAL replay of OpRemoveNode (Gap 2)")
	}
	if got := rg.LiveOrder(); got != 0 {
		t.Fatalf("LiveOrder = %d, want 0 after replaying the delete", got)
	}
}

// TestRecovery_DeleteSurvivesCheckpointReopen reproduces the observable
// Groadmap bug end to end, in one process, using three separate
// open→mutate→checkpoint→close cycles (each cycle mimics one `rmp graph`
// invocation, which reopens the store from disk). RED on the pre-fix code:
// the checkpoint snapshot does not persist the tombstone, so the deleted
// node resurrects as a label-stripped ghost and LiveOrder stays at 1.
func TestRecovery_DeleteSurvivesCheckpointReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// --- open 1: CREATE (a:Spec {key:'auth'}); checkpoint; close ---
	{
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("open1 wal.Open: %v", err)
		}
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())
		tx := store.Begin()
		mustTx(t, tx.AddNode("auth"))
		mustTx(t, tx.SetNodeLabel("auth", "Spec"))
		mustTx(t, tx.SetNodeProperty("auth", "key", lpg.StringValue("auth")))
		if err := tx.Commit(); err != nil {
			t.Fatalf("open1 Commit: %v", err)
		}
		checkpointAndClose(t, dir, g, w)
	}

	// --- open 2: MATCH (n) DETACH DELETE n; checkpoint; close ---
	{
		res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
		if err != nil {
			t.Fatalf("open2 recovery.Open: %v", err)
		}
		g := res.Graph
		if got := g.LiveOrder(); got != 1 {
			t.Fatalf("open2 LiveOrder = %d, want 1 (the node created in open1)", got)
		}
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("open2 wal.Open: %v", err)
		}
		store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())
		// Mirror the Cypher DETACH DELETE: strip labels and properties,
		// record the removal on the WAL, and tombstone the live graph
		// eagerly exactly as the Cypher mutator adapter does.
		tx := store.Begin()
		for _, lbl := range g.NodeLabels("auth") {
			mustTx(t, tx.RemoveNodeLabel("auth", lbl))
		}
		for k := range g.NodeProperties("auth") {
			mustTx(t, tx.DelNodeProperty("auth", k))
		}
		mustTx(t, tx.RemoveNode("auth"))
		if err := tx.Commit(); err != nil {
			t.Fatalf("open2 delete Commit: %v", err)
		}
		g.RemoveNode("auth")
		checkpointAndClose(t, dir, g, w)
	}

	// --- open 3: MATCH (n) RETURN count(n) ⇒ 0, no ghost ---
	{
		res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
		if err != nil {
			t.Fatalf("open3 recovery.Open: %v", err)
		}
		g := res.Graph
		if got := g.LiveOrder(); got != 0 {
			t.Fatalf("open3 LiveOrder = %d, want 0 — the deleted node resurrected as a ghost across reopen", got)
		}
		if id, ok := g.AdjList().Mapper().Lookup("auth"); ok && !g.IsTombstoned(id) {
			t.Fatalf("node auth is live after reopen (labels=%v); the tombstone did not survive the snapshot",
				g.NodeLabels("auth"))
		}
	}
}

// TestRecovery_ReplayOrdering_AddRemoveAddLeavesLive proves the tombstone
// lifecycle honours WAL order: create → delete → re-create across three
// transactions replays to a single LIVE node. The re-create must un-
// tombstone (g.AddNode revives), not leave a ghost.
func TestRecovery_ReplayOrdering_AddRemoveAddLeavesLive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())

	tx := store.Begin()
	mustTx(t, tx.AddNode("auth"))
	mustTx(t, tx.SetNodeLabel("auth", "Spec"))
	mustTx(t, tx.Commit())

	tx = store.Begin()
	mustTx(t, tx.RemoveNodeLabel("auth", "Spec"))
	mustTx(t, tx.RemoveNode("auth"))
	mustTx(t, tx.Commit())

	tx = store.Begin()
	mustTx(t, tx.AddNode("auth"))
	mustTx(t, tx.SetNodeLabel("auth", "Spec"))
	mustTx(t, tx.Commit())
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	rg := res.Graph
	id, ok := rg.AdjList().Mapper().Lookup("auth")
	if !ok {
		t.Fatal("auth not interned after replay")
	}
	if rg.IsTombstoned(id) {
		t.Fatal("auth must be LIVE after add→remove→add replay (re-create did not revive)")
	}
	if got := rg.LiveOrder(); got != 1 {
		t.Fatalf("LiveOrder = %d, want 1 after add→remove→add", got)
	}
	if !rg.HasNodeLabel("auth", "Spec") {
		t.Error("re-created auth lost its label after replay")
	}
}

// TestRecovery_DetachDeleteWithEdges proves a DETACH DELETE of a connected
// node replays correctly: the node is tombstoned, its edge is gone, and the
// other endpoint survives intact.
func TestRecovery_DetachDeleteWithEdges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())

	tx := store.Begin()
	mustTx(t, tx.AddNode("x"))
	mustTx(t, tx.SetNodeLabel("x", "Spec"))
	mustTx(t, tx.AddNode("y"))
	mustTx(t, tx.SetNodeLabel("y", "Spec"))
	mustTx(t, tx.AddEdge("x", "y", 1))
	mustTx(t, tx.Commit())

	// DETACH DELETE x: drop the incident edge, strip x, remove x.
	tx = store.Begin()
	mustTx(t, tx.RemoveEdge("x", "y"))
	mustTx(t, tx.RemoveNodeLabel("x", "Spec"))
	mustTx(t, tx.RemoveNode("x"))
	mustTx(t, tx.Commit())
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	rg := res.Graph
	idX, _ := rg.AdjList().Mapper().Lookup("x")
	if !rg.IsTombstoned(idX) {
		t.Error("x must be tombstoned after DETACH DELETE replay")
	}
	if rg.AdjList().HasEdge("x", "y") {
		t.Error("edge x->y must be gone after DETACH DELETE replay")
	}
	idY, _ := rg.AdjList().Mapper().Lookup("y")
	if rg.IsTombstoned(idY) {
		t.Error("y must survive the DETACH DELETE of x")
	}
	if !rg.HasNodeLabel("y", "Spec") {
		t.Error("y lost its label after the DETACH DELETE of x")
	}
	if got := rg.LiveOrder(); got != 1 {
		t.Fatalf("LiveOrder = %d, want 1 (only y is live)", got)
	}
}

// TestRecovery_DeleteThenRecreateInLaterOpen covers the cross-open
// resurrection: a node deleted in one open and re-created (same key) in a
// LATER open must come back as exactly one live node, with the snapshot
// tombstone from the delete cleared by the re-create.
func TestRecovery_DeleteThenRecreateInLaterOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	openCreate := func() {
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())
		tx := store.Begin()
		mustTx(t, tx.AddNode("auth"))
		mustTx(t, tx.SetNodeLabel("auth", "Spec"))
		mustTx(t, tx.SetNodeProperty("auth", "key", lpg.StringValue("auth")))
		mustTx(t, tx.Commit())
		checkpointAndClose(t, dir, g, w)
	}

	// open 1: create.
	openCreate()

	// open 2: delete + checkpoint.
	{
		res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
		if err != nil {
			t.Fatalf("open2 recovery.Open: %v", err)
		}
		g := res.Graph
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("open2 wal.Open: %v", err)
		}
		store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())
		tx := store.Begin()
		mustTx(t, tx.RemoveNodeLabel("auth", "Spec"))
		mustTx(t, tx.DelNodeProperty("auth", "key"))
		mustTx(t, tx.RemoveNode("auth"))
		mustTx(t, tx.Commit())
		checkpointAndClose(t, dir, g, w)
	}

	// open 3: re-create the SAME key + checkpoint.
	{
		res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
		if err != nil {
			t.Fatalf("open3 recovery.Open: %v", err)
		}
		g := res.Graph
		if got := g.LiveOrder(); got != 0 {
			t.Fatalf("open3 starting LiveOrder = %d, want 0 (auth was deleted in open2)", got)
		}
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("open3 wal.Open: %v", err)
		}
		store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())
		tx := store.Begin()
		mustTx(t, tx.AddNode("auth"))
		mustTx(t, tx.SetNodeLabel("auth", "Spec"))
		mustTx(t, tx.Commit())
		checkpointAndClose(t, dir, g, w)
	}

	// open 4: exactly one live node, no ghost, no duplicate.
	{
		res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
		if err != nil {
			t.Fatalf("open4 recovery.Open: %v", err)
		}
		g := res.Graph
		if got := g.LiveOrder(); got != 1 {
			t.Fatalf("open4 LiveOrder = %d, want 1 (auth re-created exactly once)", got)
		}
		id, ok := g.AdjList().Mapper().Lookup("auth")
		if !ok {
			t.Fatal("auth not interned after re-create")
		}
		if g.IsTombstoned(id) {
			t.Fatal("auth must be live after re-create in a later open")
		}
		if !g.HasNodeLabel("auth", "Spec") {
			t.Error("re-created auth lost its label across reopen")
		}
	}
}
