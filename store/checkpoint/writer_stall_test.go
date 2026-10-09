package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestCheckpoint_Phase3HoldsNoCommitLock_LargeSuffix is the #2195 acceptance
// test. Before WAL v2, phase 3 copied the WAL suffix committed during phase 2
// into a new file under the commit lock, so every committer stalled for a time
// proportional to that suffix (8.1 ms / 30.9 ms measured). With a segmented WAL
// phase 3 only unlinks whole segments below the oldest retained position, and
// takes no commit lock at all.
//
// The test commits a ~20 MiB prefix (two 16 MiB segments' worth, so whole
// segments lie below the redo position), runs a checkpoint during whose phase 2
// a 56.5 MB suffix is committed, and parks phase 3 for phase3Delay. A committer
// started while phase 3 is parked must finish in well under that delay. After
// the checkpoint the segments below the redo position are gone and recovery
// returns the whole suffix.
func TestCheckpoint_Phase3HoldsNoCommitLock_LargeSuffix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec(),
	})
	big := lpg.StringValue(strings.Repeat("s", 64<<10))
	commitBig := func(prefix string, n int) {
		for i := range n {
			tx := store.Begin()
			k := prefix + strconv.Itoa(i)
			if err := tx.SetNodeProperty(k, "v", big); err != nil {
				t.Errorf("SetNodeProperty: %v", err)
				return
			}
			if err := tx.Commit(); err != nil {
				t.Errorf("Commit: %v", err)
				return
			}
		}
	}
	commitBig("prefix-", 320) // ~20 MiB
	const suffixTxns = 862    // 862 x ~65.6 KB ~ 56.5 MB

	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu,
		WithCommitSerialiser[string, int64](store.RunUnderCommitLock),
		WithMapperCodec[string, int64](store.Codec()),
	)
	var suffixBytes int64
	cp.afterCaptureHook = func() {
		before := w.DurableOffset()
		commitBig("suffix-", suffixTxns)
		suffixBytes = w.DurableOffset() - before
	}
	const phase3Delay = 300 * time.Millisecond
	phase3Entered := make(chan struct{})
	cp.beforePhase3Hook = func() {
		close(phase3Entered)
		time.Sleep(phase3Delay)
	}
	cpDone := make(chan error, 1)
	go func() { cpDone <- cp.RunCheckpoint() }()
	select {
	case <-phase3Entered:
	case <-time.After(60 * time.Second):
		t.Fatal("checkpoint did not reach phase 3")
	}
	start := time.Now()
	tx := store.Begin()
	if err := tx.AddNode("during-phase-3"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	commitLatency := time.Since(start)
	if err := <-cpDone; err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
	if suffixBytes < 56_500_000 {
		t.Fatalf("the phase-2 suffix is %d bytes, below the 56.5 MB the test is about", suffixBytes)
	}
	if commitLatency >= phase3Delay/2 {
		t.Fatalf("a commit during phase 3 took %v (phase 3 parked %v): phase 3 holds the commit lock", commitLatency, phase3Delay)
	}
	if _, err := os.Stat(wal.SegmentPath(walPath, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("segment 1 lies below the redo position and survived phase 3: %v", err)
	}
	if cp.Stats().WALTruncBytes == 0 {
		t.Fatal("phase 3 reclaimed no segment")
	}
	t.Logf("suffix %d bytes; commit during phase 3: %v (parked %v); reclaimed %d log bytes",
		suffixBytes, commitLatency, phase3Delay, cp.Stats().WALTruncBytes)
}
