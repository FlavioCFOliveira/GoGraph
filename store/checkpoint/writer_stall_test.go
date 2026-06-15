package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCheckpoint_WriterStallBoundedByCapture is the #1508 acceptance proof that
// a non-blocking checkpoint stalls writers only for the watermark+CSR capture
// (phase 1), NOT for the snapshot disk I/O (phase 2). It wires the real commit
// serialiser ([txn.Store.RunUnderCommitLock]) and injects a long, deterministic
// delay into phase 2 via the afterCaptureHook seam; a concurrent committer
// started during that delay must complete its commit promptly — far below the
// phase-2 delay — because the commit lock is released before phase 2 runs.
//
// Under the OLD blocking checkpoint the whole snapshot write was held under the
// commit lock, so the concurrent committer would have blocked for the full
// phase-2 delay; here it must not.
func TestCheckpoint_WriterStallBoundedByCapture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// Seed a transaction so the snapshot has content.
	tx := store.Begin()
	if err := tx.AddNode("seed"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(seed): %v", err)
	}

	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu,
		WithCommitSerialiser[string, int64](store.RunUnderCommitLock),
		WithMapperCodec[string, int64](store.Codec()),
	)

	// Phase-2 delay: long enough that a blocking checkpoint would clearly stall
	// the concurrent committer, short enough to keep the test fast.
	const phase2Delay = 300 * time.Millisecond
	phase2Entered := make(chan struct{})
	cp.afterCaptureHook = func() {
		close(phase2Entered)
		time.Sleep(phase2Delay)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	// Run the checkpoint in the background; it will park in phase 2 for
	// phase2Delay.
	cpDone := make(chan error, 1)
	go func() { cpDone <- cp.Trigger() }()

	// Wait until the checkpoint has captured the watermark and entered the
	// lock-free phase 2 (commit lock released).
	select {
	case <-phase2Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint did not reach phase 2")
	}

	// Now commit a transaction concurrently. If the commit lock were held for
	// the whole snapshot write (the old blocking behaviour) this would block
	// for ~phase2Delay; under the non-blocking checkpoint it completes promptly.
	start := time.Now()
	tx2 := store.Begin()
	if err := tx2.AddNode("concurrent"); err != nil {
		t.Fatalf("AddNode(concurrent): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit(concurrent): %v", err)
	}
	commitLatency := time.Since(start)

	// The concurrent commit must complete in well under the phase-2 delay,
	// proving it was not serialised behind the snapshot write. A generous
	// fraction of phase2Delay keeps the assertion robust on a loaded CI box
	// while still failing hard if the whole snapshot window blocks the writer.
	if commitLatency >= phase2Delay/2 {
		t.Fatalf("concurrent commit latency %v >= %v (half the phase-2 delay): "+
			"the commit lock appears held during the lock-free snapshot write",
			commitLatency, phase2Delay/2)
	}
	t.Logf("concurrent commit latency during checkpoint phase 2: %v (phase-2 delay %v)", commitLatency, phase2Delay)

	if err := <-cpDone; err != nil {
		t.Fatalf("checkpoint Trigger: %v", err)
	}
}
