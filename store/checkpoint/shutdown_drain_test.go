package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCheckpoint_ShutdownDrain verifies that Stop blocks until the
// background goroutine exits, that no goroutine leak occurs, and that
// after a Trigger+Stop cycle the snapshot directory is recognised by
// recovery.Open.
func TestCheckpoint_ShutdownDrain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	edges := [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}}
	for _, e := range edges {
		if err := g.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
	}

	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// Stop must drain the goroutine; goleak in TestMain will catch any leak.
	cp.Stop()

	if got := cp.Stats().Checkpoints; got < 1 {
		t.Fatalf("Checkpoints = %d, want >= 1", got)
	}

	// Snapshot must be visible after Stop.
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatalf("SnapshotHit = false after shutdown drain")
	}
}

// TestCheckpoint_ShutdownCtxCancel verifies that the background
// goroutine exits promptly when its governing context is cancelled
// before Start even gets to execute meaningful work.
func TestCheckpoint_ShutdownCtxCancel(t *testing.T) {
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

	// Cancel before Start: the goroutine should observe ctx.Done() on its
	// first select iteration and close doneCh promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cp.Start(ctx)

	// Stop blocks until doneCh is closed. This must return quickly.
	cp.Stop()
}
