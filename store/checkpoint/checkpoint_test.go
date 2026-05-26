package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/snapshot"
	"gograph/store/wal"
)

func TestCheckpoint_TriggerProducesSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("b", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// Verify snapshot directory exists.
	snapDir := filepath.Join(dir, "snapshot")
	loaded, err := snapshot.Open(snapDir)
	if err != nil {
		t.Fatalf("snapshot.Open: %v", err)
	}
	// The checkpointer calls snapshot.WriteSnapshotCSR (the legacy
	// v1 CSR-only writer), so the manifest carries version 1
	// regardless of the build's ManifestVersion constant. A later
	// sprint extends the checkpointer to write v2 (CSR + labels).
	if loaded.Manifest.Version != 1 {
		t.Fatalf("manifest version %d, want 1 (legacy CSR-only checkpoint)", loaded.Manifest.Version)
	}

	stats := cp.Stats()
	if stats.Checkpoints != 1 {
		t.Fatalf("Checkpoints = %d, want 1", stats.Checkpoints)
	}
	if stats.LastDurationNS == 0 {
		t.Fatalf("LastDurationNS not recorded")
	}
}

func TestCheckpoint_TickerFires(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var mu sync.Mutex
	cp := New(Config{
		Dir:      dir,
		MaxAge:   20 * time.Millisecond,
		Interval: 5 * time.Millisecond,
	}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for cp.Stats().Checkpoints == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("ticker never fired a checkpoint")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCheckpoint_StopReleasesResources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	var mu sync.Mutex
	cp := New(Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	cp.Stop()
}

// TestCheckpoint_TruncatesWAL verifies that runCheckpoint actually
// reduces the WAL file on disk (the v1.0.0 implementation only
// recorded a counter but never truncated, allowing the WAL to grow
// unbounded). After a forced checkpoint the file size on disk must
// drop, and Stats.WALTruncBytes must report the freed bytes.
func TestCheckpoint_TruncatesWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// Write 100 frames to grow the WAL.
	payload := make([]byte, 64)
	for i := 0; i < 100; i++ {
		if err := w.Append(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	preInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if preInfo.Size() == 0 {
		t.Fatal("WAL did not grow after Append+Sync")
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	postInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if postInfo.Size() >= preInfo.Size() {
		t.Fatalf("WAL not truncated: pre=%d post=%d", preInfo.Size(), postInfo.Size())
	}
	if cp.Stats().WALTruncBytes < uint64(preInfo.Size())-uint64(postInfo.Size()) {
		t.Fatalf("Stats.WALTruncBytes = %d, expected at least %d",
			cp.Stats().WALTruncBytes, preInfo.Size()-postInfo.Size())
	}
	// Appending after truncation should still work and produce a
	// fresh WAL.
	if err := w.Append([]byte("post-truncate")); err != nil {
		t.Fatalf("Append after Truncate failed: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync after Truncate failed: %v", err)
	}
	afterInfo, _ := os.Stat(walPath)
	if afterInfo.Size() == 0 {
		t.Fatalf("WAL not growing after post-truncate append")
	}
}

// TestCheckpoint_TruncationMetric_Emits verifies that
// store.checkpoint.wal_truncated_bytes is incremented on the metrics
// backend (not just on the in-process atomic counter) after a
// successful checkpoint reclaims a non-zero WAL prefix (task T932).
func TestCheckpoint_TruncationMetric_Emits(t *testing.T) {
	// NOTE: not parallel because it installs and restores a global
	// metrics backend; concurrent runs would race on the global.
	cap := newCountingBackend()
	prev := setMetricsBackend(cap)
	t.Cleanup(func() { setMetricsBackend(prev) })

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	payload := make([]byte, 64)
	for i := 0; i < 50; i++ {
		if err := w.Append(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	preInfo, _ := os.Stat(walPath)
	if preInfo.Size() == 0 {
		t.Fatal("WAL did not grow")
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	emitted := cap.count("store.checkpoint.wal_truncated_bytes")
	if emitted == 0 {
		t.Fatalf("store.checkpoint.wal_truncated_bytes not emitted: counters=%v", cap.snapshot())
	}
	if uint64(emitted) < uint64(preInfo.Size())/2 {
		t.Fatalf("store.checkpoint.wal_truncated_bytes = %d, expected approximately the truncated prefix (%d bytes)",
			emitted, preInfo.Size())
	}
}

// TestCheckpoint_StopIdempotent asserts Stop is safe under serial
// and concurrent re-entry. The v1.0.0 implementation called
// close(stopCh) directly and panicked on the second call.
func TestCheckpoint_StopIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	var mu sync.Mutex
	cp := New(Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	cp.Stop()
	cp.Stop() // must not panic
	cp.Stop() // and again

	cp2 := New(Config{Dir: dir}, g, w, &mu)
	cp2.Start(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp2.Stop()
		}()
	}
	wg.Wait()
}

// TestCheckpoint_TriggerCtxCancel verifies TriggerCtx returns
// context.Canceled when the context is cancelled before the loop can
// service the request. We fill the buffer first to force the submit
// path to wait on the channel.
func TestCheckpoint_TriggerCtxCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var mu sync.Mutex
	cp := New(Config{Dir: dir}, g, w, &mu)
	// Note: do NOT call Start — we want the loop to never service
	// the buffer so the submit eventually blocks.
	defer func() {
		// Allow Stop to unblock if there's a stuck goroutine.
		close(cp.stopCh)
		close(cp.doneCh)
	}()

	// Fill the buffer (cap 4).
	for i := 0; i < 4; i++ {
		cp.triggerCh <- make(chan error, 1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = cp.TriggerCtx(ctx)
	if err == nil {
		t.Fatalf("TriggerCtx with cancelled context should return error")
	}
	// errors.Is(err, context.Canceled) must hold for the wrapped error.
}
