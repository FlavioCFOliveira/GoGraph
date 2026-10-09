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
// THE FIX: every graph-derived component is captured into an atomic in-memory
// image ([snapshot.Capture]) at ONE instant, and phase 2 publishes those bytes
// without touching the graph. Publishing stays lock-free. The instant was first
// held by the commit lock plus Graph.View; it is now an MVCC snapshot opened
// under the commit lock (rmp #2310), and rmp #2344 removed Graph.View.
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
	if err := pairInvariant(g); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

// pairInvariant is the check [assertPairInvariant] makes, returned as an error so
// a caller that must stop its writers first can report it.
func pairInvariant(g *lpg.Graph[string, int64]) error {
	adj := g.AdjList()
	order, size := adj.Order(), adj.Size()
	if order != 2*size {
		return fmt.Errorf("partial-transaction artefact: Order=%d, Size=%d, want Order == 2*Size (%d)",
			order, size, 2*size)
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
		return fmt.Errorf("mapper walked %d nodes but Order()=%d", len(ids), order)
	}
	var edges uint64
	for _, id := range ids {
		nbrs, _ := adj.LoadEntry(id)
		for _, dst := range nbrs {
			edges++
			if _, ok := mapper.Resolve(dst); !ok {
				return fmt.Errorf("edge %d->%d has an endpoint absent from the captured node set",
					uint64(id), uint64(dst))
			}
		}
	}
	if edges != size {
		return fmt.Errorf("walked %d edges but Size()=%d (an edge was dropped or duplicated)", edges, size)
	}
	return nil
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

	// acked counts each writer's acknowledged commits, and atCapture is acked as it
	// stood at the capture point: read inside the phase-1 commit lock, where no
	// commit can publish, so every one of those commits must be in the image
	// (rmp #2980).
	var (
		committed atomic.Int64
		acked     ackedCounts
	)
	cp.afterWatermarkHook = acked.capture

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	// The workload is a FIXED number of commits, independent of time and of the
	// storage medium (rmp #2979). It used to loop until a stop flag, so it grew
	// with the medium's speed: about 11M nodes on a RAM drive. Now every checkpoint
	// round releases each writer for exactly roundCommits transactions and fires
	// the checkpoint once every writer has committed preTrigger of them, so the
	// capture is taken while the rest of the round is still committing.
	const (
		writers      = captureWriters
		checks       = 60
		roundCommits = 50
		preTrigger   = 10
	)
	var (
		writerErr atomic.Pointer[error]
		wg        sync.WaitGroup
		round     sync.WaitGroup
		stopOnce  sync.Once
	)
	starts := make([]chan struct{}, writers)
	for i := range starts {
		starts[i] = make(chan struct{}, 1)
	}
	// One send per writer per round, drained by the round's trigger.
	triggerReady := make(chan struct{}, writers)
	// stopWriters ends every writer and joins it. Idempotent; deferred, so a
	// t.Fatalf below — which runs deferred calls — leaves no writer behind.
	stopWriters := func() {
		stopOnce.Do(func() {
			for _, s := range starts {
				close(s)
			}
			wg.Wait()
		})
	}
	defer stopWriters()
	for wi := 0; wi < writers; wi++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for c := 0; ; c++ {
				if _, ok := <-starts[id]; !ok {
					return
				}
				for n := 0; n < roundCommits; n++ {
					if n == preTrigger {
						triggerReady <- struct{}{}
					}
					if writerErr.Load() != nil {
						continue
					}
					// Two FRESH keys per transaction, so each commit contributes
					// exactly two nodes and one edge to the absolute oracle.
					src := fmt.Sprintf("w%d-c%d-a%d", id, c, n)
					dst := fmt.Sprintf("w%d-c%d-b%d", id, c, n)
					tx := st.Begin()
					if err := tx.AddEdge(src, dst, 0); err != nil {
						e := err
						writerErr.Store(&e)
						_ = tx.Rollback()
						continue
					}
					if err := tx.Commit(); err != nil {
						e := err
						writerErr.Store(&e)
						continue
					}
					committed.Add(1)
					acked.add(id)
				}
				round.Done()
			}
		}(wi)
	}

	for c := 0; c < checks; c++ {
		round.Add(writers)
		for _, s := range starts {
			s <- struct{}{}
		}
		for i := 0; i < writers; i++ {
			<-triggerReady
		}
		if err := cp.Trigger(); err != nil {
			t.Fatalf("checkpoint %d: %v", c, err)
		}
		round.Wait()
		// Reconstruct from the snapshot ALONE: copy it into a WAL-free
		// directory so recovery has nothing to repair the artefact with.
		scratch := t.TempDir()
		copySnapshotTree(t, filepath.Join(dir, "snapshot"), filepath.Join(scratch, "snapshot"))

		man, err := snapshot.ReadManifestFile(filepath.Join(scratch, "snapshot", "manifest.json"))
		if err != nil {
			t.Fatalf("checkpoint %d: read manifest: %v", c, err)
		}
		res, err := recovery.Open[string, int64](scratch, capAtomicRecOpts())
		if err != nil {
			t.Fatalf("checkpoint %d: snapshot-only recovery: %v", c, err)
		}
		if !res.SnapshotHit {
			t.Fatalf("checkpoint %d: SnapshotHit = false", c)
		}
		if res.WALOps != 0 {
			t.Fatalf("checkpoint %d: snapshot-only recovery consulted the WAL (WALOps=%d)", c, res.WALOps)
		}
		assertPairInvariant(t, fmt.Sprintf("snapshot-only checkpoint %d", c), res.Graph)
		// Durability of the capture point (rmp #2980): the snapshot must hold every
		// commit acknowledged before its capture point. An image read at an instant
		// opened before the commit lock misses the commits that published in
		// between, and its pair invariant still holds.
		if err := acked.presentAtCapture(res.Graph, roundCommits); err != nil {
			t.Fatalf("checkpoint %d: snapshot: %v", c, err)
		}
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
			t.Fatalf("checkpoint %d: components disagree — manifest Order=%d Size=%d, reconstructed Order=%d Size=%d",
				c, man.Order, man.Size, gotOrder, gotSize)
		}
	}

	stopWriters()
	if p := writerErr.Load(); p != nil {
		t.Fatalf("writer failed: %v", *p)
	}
	if got, want := committed.Load(), int64(writers*checks*roundCommits); got != want {
		t.Fatalf("%d transactions committed, want exactly %d", got, want)
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

	// The bracket of the mid-run crash image (rmp #2980). acked's capture-point
	// reading is taken inside the phase-1 commit lock, where no commit can publish,
	// so it names the commits the checkpoint must preserve; begun, read after the
	// copy, is a ceiling on the commits the copy can hold.
	var (
		committed atomic.Int64
		begun     atomic.Int64
		acked     ackedCounts
	)
	cp.afterWatermarkHook = acked.capture

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	// The workload is a FIXED number of commits, independent of time and of the
	// storage medium (rmp #2980, the pattern of #2979 above). It used to loop
	// until a stop flag, so it grew with the medium's speed. Now every checkpoint
	// round releases each writer for exactly roundCommits transactions and fires
	// the checkpoint once every writer has committed preTrigger of them, so the
	// capture is taken while the rest of the round is still committing and the
	// retained WAL suffix is non-empty.
	const (
		writers      = captureWriters
		checks       = 40
		roundCommits = 50
		preTrigger   = 10
	)
	var (
		writerErr atomic.Pointer[error]
		wg        sync.WaitGroup
		round     sync.WaitGroup
		stopOnce  sync.Once
	)
	starts := make([]chan struct{}, writers)
	for i := range starts {
		starts[i] = make(chan struct{}, 1)
	}
	// One send per writer per round, drained by the round's trigger.
	triggerReady := make(chan struct{}, writers)
	// stopWriters ends every writer and joins it. Idempotent; deferred, so a
	// t.Fatalf below — which runs deferred calls — leaves no writer behind.
	stopWriters := func() {
		stopOnce.Do(func() {
			for _, s := range starts {
				close(s)
			}
			wg.Wait()
		})
	}
	defer stopWriters()
	for wi := 0; wi < writers; wi++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for c := 0; ; c++ {
				if _, ok := <-starts[id]; !ok {
					return
				}
				for n := 0; n < roundCommits; n++ {
					if n == preTrigger {
						triggerReady <- struct{}{}
					}
					if writerErr.Load() != nil {
						continue
					}
					src := fmt.Sprintf("w%d-c%d-a%d", id, c, n)
					dst := fmt.Sprintf("w%d-c%d-b%d", id, c, n)
					begun.Add(1)
					tx := st.Begin()
					if err := tx.AddEdge(src, dst, 0); err != nil {
						e := err
						writerErr.Store(&e)
						_ = tx.Rollback()
						continue
					}
					if err := tx.Commit(); err != nil {
						e := err
						writerErr.Store(&e)
						continue
					}
					committed.Add(1)
					acked.add(id)
				}
				round.Done()
			}
		}(wi)
	}

	// Fire checkpoints while the writers run, so the retained WAL suffix is
	// non-empty and the snapshot is genuinely mid-workload.
	for c := 0; c < checks; c++ {
		round.Add(writers)
		for _, s := range starts {
			s <- struct{}{}
		}
		for i := 0; i < writers; i++ {
			<-triggerReady
		}
		if err := cp.Trigger(); err != nil {
			stopWriters()
			cp.Stop()
			t.Fatalf("checkpoint %d: %v", c, err)
		}
		// THE MID-RUN CRASH IMAGE (rmp #2980), copied while the round's writers are
		// still committing. The quiescent final checkpoint below re-captures the
		// whole graph and truncates the WAL again, so on its own it overwrites
		// whatever a mid-run checkpoint lost or tore.
		if err := checkMidRunImage(t, dir, &acked, roundCommits, &begun); err != nil {
			stopWriters()
			cp.Stop()
			t.Fatalf("checkpoint %d: %v", c, err)
		}
		round.Wait()
	}

	stopWriters()
	if p := writerErr.Load(); p != nil {
		cp.Stop()
		t.Fatalf("writer failed: %v", *p)
	}
	want := uint64(committed.Load())
	if exact := uint64(writers * checks * roundCommits); want != exact {
		cp.Stop()
		t.Fatalf("%d transactions committed, want exactly %d", want, exact)
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

// captureWriters is the writer count of both capture tests: [ackedCounts] keeps
// one counter per writer.
const captureWriters = 4

// ackedCounts records, per writer, how many commits have been acknowledged, and
// the same counts as they stood at the checkpoint's capture point.
//
// A writer's k-th commit has a key fixed by k (see [pairKeys]) and its commits are
// acknowledged in order, so a count names exactly which commits were acknowledged.
// That is what lets a recovery be checked by IDENTITY: a check by count lets a
// commit lost from the image be hidden by a later commit the WAL suffix holds.
//
// Safe for concurrent use: add is called by the writers, capture by the
// checkpointer goroutine, presentAtCapture by the test goroutine after the
// checkpoint returned.
type ackedCounts struct {
	now       [captureWriters]atomic.Int64
	atCapture [captureWriters]atomic.Int64
}

func (a *ackedCounts) add(writer int) { a.now[writer].Add(1) }

// capture is the afterWatermarkHook: it runs inside the phase-1 commit lock.
func (a *ackedCounts) capture() {
	for i := range a.now {
		a.atCapture[i].Store(a.now[i].Load())
	}
}

// presentAtCapture reports the first commit acknowledged before the capture
// point whose edge g does not hold.
func (a *ackedCounts) presentAtCapture(g *lpg.Graph[string, int64], roundCommits int) error {
	adj := g.AdjList()
	for w := range a.atCapture {
		n := int(a.atCapture[w].Load())
		for k := 0; k < n; k++ {
			src, dst := pairKeys(w, k, roundCommits)
			if !adj.HasEdge(src, dst) {
				return fmt.Errorf("commit %s->%s (writer %d, commit %d of the %d it had acknowledged "+
					"before the capture point) is missing", src, dst, w, k, n)
			}
		}
	}
	return nil
}

// pairKeys returns the keys of writer w's k-th transaction, as the writers of the
// capture tests name them.
func pairKeys(w, k, roundCommits int) (src, dst string) {
	c, n := k/roundCommits, k%roundCommits
	return fmt.Sprintf("w%d-c%d-a%d", w, c, n), fmt.Sprintf("w%d-c%d-b%d", w, c, n)
}

// checkMidRunImage copies the store directory as it stands right after a
// checkpoint, with writers still committing, and recovers the copy twice: from
// the snapshot plus the WAL suffix, and from the snapshot alone. Neither
// recovery may hold a partial transaction (the pair invariant), and each must
// hold every commit acknowledged before the checkpoint's capture point. The
// snapshot-plus-WAL recovery may hold no more commits than had begun when the
// copy ended.
//
// A commit acknowledged before the capture point is either in the image or, if
// the image missed it, only in the WAL prefix the checkpoint truncated. The
// capture-point reading is taken under the commit lock and the ceiling after the
// copy, so every commit the copy can hold lies between them.
func checkMidRunImage(t *testing.T, dir string, acked *ackedCounts, roundCommits int, begun *atomic.Int64) error {
	t.Helper()
	img := t.TempDir()
	copySnapshotTree(t, dir, img)
	ceiling := uint64(begun.Load())

	full, err := recovery.Open[string, int64](img, capAtomicRecOpts())
	if err != nil {
		return fmt.Errorf("mid-run snapshot+WAL recovery: %w", err)
	}
	if err := pairInvariant(full.Graph); err != nil {
		return fmt.Errorf("mid-run snapshot+WAL recovery: %w", err)
	}
	if err := acked.presentAtCapture(full.Graph, roundCommits); err != nil {
		return fmt.Errorf("mid-run snapshot+WAL recovery: %w", err)
	}
	if got := full.Graph.AdjList().Size(); got > ceiling {
		return fmt.Errorf("mid-run snapshot+WAL recovery holds %d commits, but only %d had begun when the copy ended",
			got, ceiling)
	}

	snapOnly := t.TempDir()
	copySnapshotTree(t, filepath.Join(img, "snapshot"), filepath.Join(snapOnly, "snapshot"))
	part, err := recovery.Open[string, int64](snapOnly, capAtomicRecOpts())
	if err != nil {
		return fmt.Errorf("mid-run snapshot-only recovery: %w", err)
	}
	if err := pairInvariant(part.Graph); err != nil {
		return fmt.Errorf("mid-run snapshot-only recovery: %w", err)
	}
	if err := acked.presentAtCapture(part.Graph, roundCommits); err != nil {
		return fmt.Errorf("mid-run snapshot-only recovery: %w", err)
	}
	return nil
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
			if werr := os.WriteFile(dp, buf, 0o600); werr != nil { //nolint:gosec // G703: the directory component is a path this test created and the leaf name is a literal, so no traversal segment can enter.
				return werr
			}
		}
		return nil
	}
	if err := walk(src, dst); err != nil {
		t.Fatalf("copy snapshot tree: %v", err)
	}
}
