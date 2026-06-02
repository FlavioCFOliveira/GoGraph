package recovery

import (
	"context"
	"path/filepath"
	"sort"
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

// TestRecovery_SnapshotLabelDoesNotResurrectAcrossWALRelabel locks the
// recovery-ordering consistency contract (#1266): when the snapshot holds a
// node with label L1 / property p=1 and the WAL tail deletes it and
// re-creates it with label L2 / property p=2 (no checkpoint between), the
// recovered node must carry EXACTLY the WAL-tail state ({L2}, p=2), not the
// stale snapshot state. RED before the fix: snapshot labels/properties were
// re-applied AFTER WAL replay, so the deleted-then-recreated node ended up
// carrying L1 (re-added) and p=1 (clobbering the WAL's p=2).
func TestRecovery_SnapshotLabelDoesNotResurrectAcrossWALRelabel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// open 1: CREATE X:L1 {p:1}; checkpoint (snapshot holds X:L1,p=1); close.
	{
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("open1 wal.Open: %v", err)
		}
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())
		tx := store.Begin()
		mustTx(t, tx.AddNode("X"))
		mustTx(t, tx.SetNodeLabel("X", "L1"))
		mustTx(t, tx.SetNodeProperty("X", "p", lpg.Int64Value(1)))
		mustTx(t, tx.Commit())
		checkpointAndClose(t, dir, g, w)
	}

	// open 2: DELETE X, then re-CREATE X:L2 {p:2}; commit; NO checkpoint, so
	// the snapshot stays at X:L1 and the WAL retains the delete+recreate.
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

		del := store.Begin()
		for _, lbl := range g.NodeLabels("X") {
			mustTx(t, del.RemoveNodeLabel("X", lbl))
		}
		for k := range g.NodeProperties("X") {
			mustTx(t, del.DelNodeProperty("X", k))
		}
		mustTx(t, del.RemoveNode("X"))
		mustTx(t, del.Commit())

		create := store.Begin()
		mustTx(t, create.AddNode("X"))
		mustTx(t, create.SetNodeLabel("X", "L2"))
		mustTx(t, create.SetNodeProperty("X", "p", lpg.Int64Value(2)))
		mustTx(t, create.Commit())

		if err := w.Close(); err != nil { // close WITHOUT checkpoint
			t.Fatalf("open2 wal.Close: %v", err)
		}
	}

	// open 3: recover snapshot{X:L1,p=1} + WAL[delete; recreate L2,p=2].
	{
		res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
		if err != nil {
			t.Fatalf("open3 recovery.Open: %v", err)
		}
		g := res.Graph
		if got := g.LiveOrder(); got != 1 {
			t.Fatalf("open3 LiveOrder = %d, want 1", got)
		}
		if !g.HasNodeLabel("X", "L2") {
			t.Error("X must carry the WAL-tail label L2")
		}
		if g.HasNodeLabel("X", "L1") {
			t.Error("X must NOT carry the stale snapshot label L1 after a WAL-tail re-create")
		}
		if labels := g.NodeLabels("X"); len(labels) != 1 {
			t.Errorf("X labels = %v, want exactly [L2]", labels)
		}
		if v, ok := g.GetNodeProperty("X", "p"); !ok {
			t.Error("X lost property p after recovery")
		} else if got, _ := v.Int64(); got != 2 {
			t.Errorf("X.p = %d, want 2 (WAL-tail value, not the stale snapshot 1)", got)
		}
	}
}

// TestRecovery_RemoveEdgeReplay_StripsStaleEdgeState locks the edge analogue
// of the node-deletion durability contract (#1267): when the snapshot holds
// an edge a->b with label EL1 and property old=1, and the WAL tail removes
// that edge and re-adds it with label EL2 and property new=2 (no checkpoint
// between), the recovered edge must carry EXACTLY the WAL-tail state (label
// EL2, property new=2, no EL1, no old), not the resurrected snapshot state.
func TestRecovery_RemoveEdgeReplay_StripsStaleEdgeState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// open 1: CREATE (a)-[:EL1 {old:1}]->(b); checkpoint; close.
	{
		w, err := wal.Open(walPath)
		if err != nil {
			t.Fatalf("open1 wal.Open: %v", err)
		}
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		store := txn.NewStoreWithOptions[string, int64](g, w, tombstoneTxnOpts())
		tx := store.Begin()
		mustTx(t, tx.AddNode("a"))
		mustTx(t, tx.AddNode("b"))
		mustTx(t, tx.AddEdge("a", "b", 0))
		mustTx(t, tx.SetEdgeLabel("a", "b", "EL1"))
		mustTx(t, tx.SetEdgeProperty("a", "b", "old", lpg.Int64Value(1)))
		mustTx(t, tx.Commit())
		checkpointAndClose(t, dir, g, w)
	}

	// open 2: delete the edge, re-add it with a new label/property; commit;
	// NO checkpoint, so the snapshot keeps EL1/old and the WAL retains the
	// remove-then-re-add.
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
		mustTx(t, tx.RemoveEdge("a", "b"))
		mustTx(t, tx.AddEdge("a", "b", 0))
		mustTx(t, tx.SetEdgeLabel("a", "b", "EL2"))
		mustTx(t, tx.SetEdgeProperty("a", "b", "new", lpg.Int64Value(2)))
		mustTx(t, tx.Commit())
		if err := w.Close(); err != nil { // no checkpoint
			t.Fatalf("open2 wal.Close: %v", err)
		}
	}

	// open 3: recover snapshot{a-[:EL1{old:1}]->b} + WAL[remove; re-add EL2{new:2}].
	{
		res, err := Open[string, int64](dir, tombstoneRecoveryOpts())
		if err != nil {
			t.Fatalf("open3 recovery.Open: %v", err)
		}
		g := res.Graph
		if !g.AdjList().HasEdge("a", "b") {
			t.Fatal("edge a->b must exist after re-add")
		}
		labels := g.EdgeLabels("a", "b")
		sort.Strings(labels)
		if len(labels) != 1 || labels[0] != "EL2" {
			t.Fatalf("edge labels = %v, want exactly [EL2] (no stale EL1)", labels)
		}
		if _, ok := g.GetEdgeProperty("a", "b", "old"); ok {
			t.Error("stale snapshot edge property 'old' must not survive the remove+re-add")
		}
		if v, ok := g.GetEdgeProperty("a", "b", "new"); !ok {
			t.Error("WAL-tail edge property 'new' missing")
		} else if got, _ := v.Int64(); got != 2 {
			t.Errorf("edge property new = %d, want 2", got)
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
