package cypher_test

// checkpoint_barrier_race_test.go — regression test for task #1324
// (docs/acid-audit.md F3.5). It proves the background checkpointer captures a
// CONSISTENT, transaction-boundary snapshot — and truncates the WAL safely —
// against a cypher.Engine that applies its writes eagerly under the graph
// visibility barrier (visMu) while holding the txn.Store's PRIVATE commit
// mutex from Begin to Result.Close.
//
// THE BUG (pre-fix): the checkpointer held an externally-supplied *sync.Mutex
// that is NOT the store's commit mutex (the latter is private, with no
// accessor). So for the engine wiring the checkpointer excluded nothing:
//
//   (i)  it could call csr.BuildFromAdjList mid-ApplyAtomically and snapshot a
//        half-applied transaction (an edge with a missing endpoint); and
//   (ii) a transaction could fsync a WAL frame AFTER the snapshot was taken but
//        BEFORE wal.Truncate() — and Truncate() discards the WHOLE WAL prefix —
//        so that committed transaction was lost.
//
// THE FIX: txn.Store.RunUnderCommitLock runs a closure under the same commit
// mutex Begin holds, and the checkpointer runs its snapshot+truncate window
// under it via checkpoint.WithCommitSerialiser. The snapshot is additionally
// taken inside Graph.View (defence in depth). Both windows are then closed.
//
// The test wires the CORRECT (fixed) configuration and asserts both
// invariants hold under -race. Reverting either half of the fix
// (WithCommitSerialiser, or the View capture) reintroduces a failure — see the
// task notes for the manual-revert verification.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// cpRaceStoreOpts is the float64-weighted typed store config the Cypher engine
// requires (NewEngineWithStore is fixed to [string, float64]).
func cpRaceStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

func cpRaceRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// copySnapshotDir copies dir/snapshot into a fresh scratch directory that
// contains NO WAL, so recovery.Open on the scratch dir reconstructs the graph
// from the snapshot ALONE. The copy is shallow-recursive, matching the known
// snapshot shape (manifest.json, csr.bin, labels.bin, properties.bin,
// mapper.bin, indexes/*).
func copySnapshotDir(t *testing.T, srcSnap, dstRoot string) {
	t.Helper()
	dstSnap := filepath.Join(dstRoot, "snapshot")
	var walk func(src, dst string) error
	walk = func(src, dst string) error {
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o750); err != nil {
			return err
		}
		for _, e := range entries {
			sp := filepath.Join(src, e.Name())
			dp := filepath.Join(dst, e.Name())
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
			if werr := os.WriteFile(dp, buf, 0o600); werr != nil { //nolint:gosec // path under t.TempDir
				return werr
			}
		}
		return nil
	}
	if err := walk(srcSnap, dstSnap); err != nil {
		t.Fatalf("copy snapshot dir: %v", err)
	}
}

// TestCheckpoint_SnapshotUnderBarrier_NoPartialTransaction is the task #1324
// race-invariant test. A cypher.Engine over a txn.Store runs concurrent
// `RunInTx CREATE (a)-[:R]->(b)` writes while a checker goroutine fires
// checkpoints aggressively and, after each one, loads the snapshot ALONE
// (WAL truncated) and asserts the structural invariant that every edge R has
// both of its endpoints — exact for anonymous CREATEs as Order == 2*Size,
// since each CREATE contributes two unique anonymous nodes and one edge.
//
// It also proves no committed transaction is lost: it counts successfully
// committed CREATEs, then after the race takes one final checkpoint, reopens
// from the snapshot alone, and asserts Size == committed (Durability).
func TestCheckpoint_SnapshotUnderBarrier_NoPartialTransaction(t *testing.T) {
	testlayers.RequireSoak(t) // concurrency stress → soak layer (short-layer per-package budget, #1460)
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, cpRaceStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	// CORRECT wiring: the checkpointer runs its snapshot+truncate window under
	// the store's real commit serialisation. The storeMu argument is a
	// throwaway — WithCommitSerialiser supersedes it. WithMapperCodec makes the
	// string-keyed snapshot self-sufficient so the WAL is actually truncated
	// (exercising window (ii)).
	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](
		checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
	)
	cpCtx, cpCancel := context.WithCancel(context.Background())
	defer cpCancel()
	cp.Start(cpCtx)

	var (
		committed atomic.Int64
		writerErr atomic.Pointer[error]
		stopWrite atomic.Bool
	)

	// Writer goroutines: loop RunInTx CREATE (a)-[:R]->(b). Each successful
	// Close() is a durable commit; count it. RunInTx applies the two nodes and
	// the edge eagerly under ApplyAtomically (visMu) and appends the WAL frames
	// in Result.Close, all while holding the store commit mutex.
	const writers = 4
	var wg sync.WaitGroup
	ctx := context.Background()
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stopWrite.Load() {
				res, rerr := eng.RunInTxAny(ctx, `CREATE (a)-[:R]->(b)`, nil)
				if rerr != nil {
					e := rerr
					writerErr.Store(&e)
					return
				}
				for res.Next() { //nolint:revive // drain to run the write to completion
				}
				if cerr := res.Close(); cerr != nil { // Close commits the WAL tx
					e := cerr
					writerErr.Store(&e)
					return
				}
				committed.Add(1)
			}
		}()
	}

	// Checker goroutine: own ALL checkpoint triggers (so no checkpoint runs
	// during its snapshot copy), and after each trigger validate the snapshot
	// in isolation. Writers run concurrently throughout — that is the race.
	const checks = 80
	var checkErr atomic.Pointer[error]
	checkDone := make(chan struct{})
	go func() {
		defer close(checkDone)
		for c := 0; c < checks; c++ {
			if err := cp.Trigger(); err != nil {
				e := err
				checkErr.Store(&e)
				return
			}
			// The snapshot on disk is now complete and the WAL is truncated.
			// Copy the snapshot to a WAL-free scratch dir and reconstruct from
			// it alone.
			scratch := filepath.Join(t.TempDir(), "snaponly")
			copySnapshotDir(t, filepath.Join(dir, "snapshot"), scratch)
			res, err := recovery.Open[string, float64](scratch, cpRaceRecOpts())
			if err != nil {
				e := err
				checkErr.Store(&e)
				return
			}
			if !res.SnapshotHit {
				e := errors.New("snapshot-only recovery: SnapshotHit = false")
				checkErr.Store(&e)
				return
			}
			// Snapshot-only recovery must consult NO WAL.
			if res.WALOps != 0 {
				e := errors.New("snapshot-only recovery consulted the WAL (WALOps != 0)")
				checkErr.Store(&e)
				return
			}
			order := res.Graph.AdjList().Order()
			size := res.Graph.AdjList().Size()
			// Invariant (i): every edge has both of its (unique, anonymous)
			// endpoints ⇒ Order == 2*Size. A torn apply captured by the
			// snapshot breaks this.
			if order != 2*size {
				e := errors.New("partial-transaction snapshot: Order != 2*Size " +
					"(an edge R was captured without both endpoints)")
				checkErr.Store(&e)
				return
			}
			time.Sleep(time.Millisecond) // let writers make progress between checks
		}
	}()

	<-checkDone
	stopWrite.Store(true)
	wg.Wait()

	if p := writerErr.Load(); p != nil {
		t.Fatalf("writer goroutine failed: %v", *p)
	}
	if p := checkErr.Load(); p != nil {
		t.Fatalf("snapshot invariant violated during race: %v", *p)
	}

	// Invariant (ii) — Durability: no committed transaction was lost. Take one
	// final checkpoint (folds every committed CREATE into the snapshot and
	// truncates the WAL), then reopen from the snapshot ALONE and assert every
	// committed CREATE survives. Pre-fix, a CREATE committed inside a
	// checkpoint's snapshot→truncate window was fsynced to the WAL and then
	// truncated away, so the snapshot-only count would fall short of the
	// committed count.
	if err := cp.Trigger(); err != nil {
		t.Fatalf("final checkpoint Trigger: %v", err)
	}
	cp.Stop()

	wantEdges := uint64(committed.Load())
	if wantEdges == 0 {
		t.Fatal("no CREATE committed during the race; test did not exercise the path")
	}
	finalScratch := filepath.Join(t.TempDir(), "final-snaponly")
	copySnapshotDir(t, filepath.Join(dir, "snapshot"), finalScratch)
	res, err := recovery.Open[string, float64](finalScratch, cpRaceRecOpts())
	if err != nil {
		t.Fatalf("final snapshot-only recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("final snapshot-only recovery: SnapshotHit = false")
	}
	if res.WALOps != 0 {
		t.Fatalf("final snapshot-only recovery consulted the WAL: WALOps = %d", res.WALOps)
	}
	gotEdges := res.Graph.AdjList().Size()
	gotNodes := res.Graph.AdjList().Order()
	if gotEdges != wantEdges {
		t.Fatalf("durability: snapshot-only edge count = %d, want %d committed CREATEs "+
			"(a committed transaction was lost to WAL truncation)", gotEdges, wantEdges)
	}
	if gotNodes != 2*wantEdges {
		t.Fatalf("durability: snapshot-only node count = %d, want %d (2 per committed CREATE)",
			gotNodes, 2*wantEdges)
	}
}
