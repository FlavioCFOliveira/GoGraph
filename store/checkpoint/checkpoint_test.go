package checkpoint

import (
	"context"
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
