package checkpoint

// capture_atomicity_test.go — regression test for rmp #2269.
//
// THE DEFECT: the checkpointer captured only the CSR adjacency under the commit
// serialisation (phase 1) and then handed the LIVE graph to the snapshot writer
// for the deliberately lock-free publish (phase 2). Every other component —
// mapper.bin above all, but also labels.bin, properties.bin, tombstones.bin,
// edgehandles.bin and the index payloads — was therefore walked at a LATER
// instant than the adjacency.
//
// For a workload of "two fresh nodes plus one edge between them" transactions,
// a transaction that committed during phase 2 contributed its two nodes to the
// captured mapper while its edge was absent from the phase-1 CSR. The published
// snapshot then reconstructed a graph with Order > 2*Size: a PARTIAL
// TRANSACTION, a state no serial schedule could produce, made durable in the
// artefact a crash recovery replays.
//
// THE FIX: phase 1 now captures every graph-derived component into an atomic
// in-memory image ([snapshot.Capture]) inside the same Graph.View, and phase 2
// publishes those bytes without touching the graph. Publishing stays lock-free.
//
// These tests assert the invariant on the ARTEFACT with a hand-computed
// ABSOLUTE oracle — each transaction contributes exactly 2 nodes and 1 edge, so
// Order must equal 2*Size exactly — rather than by comparing the artefact
// against itself.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// capAtomicRecOpts is the recovery configuration matching the store the tests
// below drive.
func capAtomicRecOpts() recovery.Options[string, int64] {
	return recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
}

// assertPairInvariant checks the absolute structural oracle for a graph built
// exclusively from "two fresh nodes plus one edge" transactions: every edge has
// both of its unique endpoints, so Order == 2*Size exactly. It also verifies
// every edge's endpoints really are present, so the count identity cannot be
// satisfied by an accidental compensation (one orphan edge plus one orphan
// node).
func assertPairInvariant(t *testing.T, label string, g *lpg.Graph[string, int64]) {
	t.Helper()
	adj := g.AdjList()
	order, size := adj.Order(), adj.Size()
	if order != 2*size {
		t.Fatalf("%s: partial-transaction artefact: Order=%d, Size=%d, want Order == 2*Size (%d)",
			label, order, size, 2*size)
	}
	// Structural cross-check: every edge endpoint must be an interned node.
	// A dropped or duplicated edge, or an edge whose endpoint was not
	// captured, fails here even where the counts happen to balance (one
	// orphan edge plus one orphan node would otherwise cancel out).
	mapper := adj.Mapper()
	ids := make([]graph.NodeID, 0, order)
	mapper.Walk(func(id graph.NodeID, _ string) bool {
		ids = append(ids, id)
		return true
	})
	if uint64(len(ids)) != order {
		t.Fatalf("%s: mapper walked %d nodes but Order()=%d", label, len(ids), order)
	}
	var edges uint64
	for _, id := range ids {
		nbrs, _ := adj.LoadEntry(id)
		for _, dst := range nbrs {
			edges++
			if _, ok := mapper.Resolve(dst); !ok {
				t.Fatalf("%s: edge %d->%d has an endpoint absent from the captured node set",
					label, uint64(id), uint64(dst))
			}
		}
	}
	if edges != size {
		t.Fatalf("%s: walked %d edges but Size()=%d (an edge was dropped or duplicated)", label, edges, size)
	}
}

// newPairStore builds a WAL-backed string store and a checkpointer wired the
// production way: the snapshot+truncate window runs under the store's real
// commit serialisation, and the mapper codec makes the snapshot
// self-sufficient so the WAL is genuinely truncated.
func newPairStore(t *testing.T) (dir string, g *lpg.Graph[string, int64], st *txn.Store[string, int64], w *wal.Writer, cp *Checkpointer[string, int64]) {
	t.Helper()
	dir = t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g = lpg.New[string, int64](adjlist.Config{Directed: true})
	st = txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	var unusedMu sync.Mutex
	cp = New[string, int64](
		Config{Dir: dir}, g, w, &unusedMu,
		WithCommitSerialiser[string, int64](st.RunUnderCommitLock),
		WithMapperCodec[string, int64](st.Codec()),
	)
	return dir, g, st, w, cp
}

// TestCheckpoint_CaptureIsAtomic_SnapshotOnlyArtefact drives checkpoints
// against concurrent "2 nodes + 1 edge" transactions and asserts the
// SELF-SUFFICIENT SNAPSHOT path: the snapshot directory alone, with no WAL to
// repair it, must reconstruct a graph in which every edge has both endpoints.
//
// It additionally asserts that the manifest's own Order/Size — recorded from
// the captured CSR — agree with what the reconstructed graph reports. That is
// the direct cross-component check: before the fix the manifest said
// Order == 2*Size (the CSR was consistent) while the reconstructed graph did
// not, because the node set came from a later mapper walk.
func TestCheckpoint_CaptureIsAtomic_SnapshotOnlyArtefact(t *testing.T) {
	t.Parallel()
	dir, _, st, w, cp := newPairStore(t)
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	var (
		stop      atomic.Bool
		committed atomic.Int64
		writerErr atomic.Pointer[error]
	)
	const writers = 4
	var wg sync.WaitGroup
	for wi := 0; wi < writers; wi++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; !stop.Load(); n++ {
				// Two FRESH keys per transaction, so each commit contributes
				// exactly two nodes and one edge to the absolute oracle.
				src := fmt.Sprintf("w%d-a%d", id, n)
				dst := fmt.Sprintf("w%d-b%d", id, n)
				tx := st.Begin()
				if err := tx.AddEdge(src, dst, 0); err != nil {
					e := err
					writerErr.Store(&e)
					_ = tx.Rollback()
					return
				}
				if err := tx.Commit(); err != nil {
					e := err
					writerErr.Store(&e)
					return
				}
				committed.Add(1)
			}
		}(wi)
	}

	const checks = 60
	for c := 0; c < checks; c++ {
		if err := cp.Trigger(); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("checkpoint %d: %v", c, err)
		}
		// Reconstruct from the snapshot ALONE: copy it into a WAL-free
		// directory so recovery has nothing to repair the artefact with.
		scratch := t.TempDir()
		copySnapshotTree(t, filepath.Join(dir, "snapshot"), filepath.Join(scratch, "snapshot"))

		man, err := snapshot.ReadManifestFile(filepath.Join(scratch, "snapshot", "manifest.json"))
		if err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("checkpoint %d: read manifest: %v", c, err)
		}
		res, err := recovery.Open[string, int64](scratch, capAtomicRecOpts())
		if err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("checkpoint %d: snapshot-only recovery: %v", c, err)
		}
		if !res.SnapshotHit {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("checkpoint %d: SnapshotHit = false", c)
		}
		if res.WALOps != 0 {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("checkpoint %d: snapshot-only recovery consulted the WAL (WALOps=%d)", c, res.WALOps)
		}
		func() {
			defer func() {
				if t.Failed() {
					stop.Store(true)
				}
			}()
			assertPairInvariant(t, fmt.Sprintf("snapshot-only checkpoint %d", c), res.Graph)
		}()
		// The SAME absolute oracle, applied to the manifest itself. Every
		// transaction contributes exactly two nodes and one edge, so a manifest
		// describing a transactional instant must satisfy Order == 2*Size just as
		// the reconstructed graph must. Asserting it here rather than only on the
		// reconstruction is what distinguishes the two ways this can fail: a
		// manifest that is internally consistent but disagrees with the image means
		// the components were read at different instants, whereas one that is
		// internally INCONSISTENT means the manifest's own counts do not come from
		// the same instant as each other (rmp #2310 — Order was taken from the CSR's
		// vertex-array length, which is sized from the present id space and so
		// counts slots for ids interned after the captured instant).
		if man.Order != 2*man.Size {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("checkpoint %d: the MANIFEST is internally inconsistent — Order=%d Size=%d, "+
				"want Order == 2*Size (%d). Every transaction contributes exactly two nodes and "+
				"one edge, so these two numbers were not derived from the same instant",
				c, man.Order, man.Size, 2*man.Size)
		}
		// Cross-component identity: the manifest records the captured image's
		// Order/Size. Every other component must describe that same instant,
		// so the reconstructed graph must match it exactly. This is the
		// assertion that pinned the defect: the manifest was right and the
		// graph was not.
		gotOrder := res.Graph.AdjList().Order()
		gotSize := res.Graph.AdjList().Size()
		if gotOrder != man.Order || gotSize != man.Size {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("checkpoint %d: components disagree — manifest Order=%d Size=%d, reconstructed Order=%d Size=%d",
				c, man.Order, man.Size, gotOrder, gotSize)
		}
	}

	stop.Store(true)
	wg.Wait()
	if p := writerErr.Load(); p != nil {
		t.Fatalf("writer failed: %v", *p)
	}
	if committed.Load() == 0 {
		t.Fatal("no transaction committed during the race; the path was not exercised")
	}
}

// TestCheckpoint_CaptureIsAtomic_SnapshotPlusWALArtefact asserts the SECOND
// recovery path: snapshot plus the surviving WAL suffix. After a checkpoint the
// WAL prefix up to the captured watermark is discarded and the suffix — every
// transaction committed during the lock-free publish — is retained, so a reopen
// must reconstruct a state satisfying the same absolute oracle, and must
// account for every acknowledged commit.
//
// This is the path production actually recovers through, and it is asserted
// here rather than assumed: a capture skew that the WAL replay happens to heal
// today would still be a latent defect, and a skew it does NOT heal — such as
// capturing the eagerly-applied writes of a transaction whose commit later
// fails — would be unrecoverable.
func TestCheckpoint_CaptureIsAtomic_SnapshotPlusWALArtefact(t *testing.T) {
	t.Parallel()
	dir, _, st, w, cp := newPairStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	var (
		stop      atomic.Bool
		committed atomic.Int64
		writerErr atomic.Pointer[error]
	)
	const writers = 4
	var wg sync.WaitGroup
	for wi := 0; wi < writers; wi++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; !stop.Load(); n++ {
				src := fmt.Sprintf("w%d-a%d", id, n)
				dst := fmt.Sprintf("w%d-b%d", id, n)
				tx := st.Begin()
				if err := tx.AddEdge(src, dst, 0); err != nil {
					e := err
					writerErr.Store(&e)
					_ = tx.Rollback()
					return
				}
				if err := tx.Commit(); err != nil {
					e := err
					writerErr.Store(&e)
					return
				}
				committed.Add(1)
			}
		}(wi)
	}

	// Fire checkpoints while the writers run, so the retained WAL suffix is
	// non-empty and the snapshot is genuinely mid-workload.
	for c := 0; c < 40; c++ {
		if err := cp.Trigger(); err != nil {
			stop.Store(true)
			wg.Wait()
			cp.Stop()
			t.Fatalf("checkpoint %d: %v", c, err)
		}
	}

	stop.Store(true)
	wg.Wait()
	if p := writerErr.Load(); p != nil {
		cp.Stop()
		t.Fatalf("writer failed: %v", *p)
	}
	want := uint64(committed.Load())
	if want == 0 {
		cp.Stop()
		t.Fatal("no transaction committed during the race; the path was not exercised")
	}
	// One final checkpoint, then stop, so the artefact on disk is the pair
	// (snapshot, surviving WAL) that a restart would recover from.
	if err := cp.Trigger(); err != nil {
		cp.Stop()
		t.Fatalf("final checkpoint: %v", err)
	}
	cp.Stop()
	if err := w.Sync(); err != nil {
		t.Fatalf("WAL Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("WAL Close: %v", err)
	}

	res, err := recovery.Open[string, int64](dir, capAtomicRecOpts())
	if err != nil {
		t.Fatalf("snapshot+WAL recovery: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("snapshot+WAL recovery: SnapshotHit = false")
	}
	assertPairInvariant(t, "snapshot+WAL", res.Graph)
	// Durability oracle, hand-computed: every acknowledged commit contributed
	// exactly one edge and two nodes.
	if got := res.Graph.AdjList().Size(); got != want {
		t.Fatalf("snapshot+WAL: recovered %d edges, want %d acknowledged commits", got, want)
	}
	if got := res.Graph.AdjList().Order(); got != 2*want {
		t.Fatalf("snapshot+WAL: recovered %d nodes, want %d (2 per acknowledged commit)", got, 2*want)
	}
}

// copySnapshotTree copies a published snapshot directory into dst, producing a
// store directory that contains a snapshot and NO WAL — so recovery over its
// parent must reconstruct from the snapshot alone.
func copySnapshotTree(t *testing.T, src, dst string) {
	t.Helper()
	var walk func(from, to string) error
	walk = func(from, to string) error {
		entries, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(to, 0o750); err != nil {
			return err
		}
		for _, e := range entries {
			sp, dp := filepath.Join(from, e.Name()), filepath.Join(to, e.Name())
			if e.IsDir() {
				if err := walk(sp, dp); err != nil {
					return err
				}
				continue
			}
			buf, rerr := os.ReadFile(sp) //nolint:gosec // path under t.TempDir
			if rerr != nil {
				return rerr
			}
			if werr := os.WriteFile(dp, buf, 0o600); werr != nil {
				return werr
			}
		}
		return nil
	}
	if err := walk(src, dst); err != nil {
		t.Fatalf("copy snapshot tree: %v", err)
	}
}
