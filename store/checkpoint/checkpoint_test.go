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
	g.AddEdge("a", "b", 1)
	g.AddEdge("b", "c", 2)

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
	if loaded.Manifest.Version != snapshot.ManifestVersion {
		t.Fatalf("manifest version %d, want %d", loaded.Manifest.Version, snapshot.ManifestVersion)
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
	g.AddEdge("a", "b", 1)

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
	g.AddEdge("a", "b", 1)
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
