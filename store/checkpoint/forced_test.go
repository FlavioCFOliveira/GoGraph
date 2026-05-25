package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/wal"
)

// TestCheckpoint_ForcedCheckpoint verifies that explicit Trigger calls
// each succeed and increment Stats.Checkpoints, even when MaxAge is
// set to 1 hour so the ticker never fires.
func TestCheckpoint_ForcedCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range [][2]string{{"u", "v"}, {"v", "w"}, {"w", "u"}} {
		if err := g.AddEdge(e[0], e[1], 0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e[0], e[1], err)
		}
	}

	var mu sync.Mutex
	// MaxAge=1h ensures the ticker never fires; all checkpoints are
	// explicitly driven by Trigger.
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger #1: %v", err)
	}
	if got := cp.Stats().Checkpoints; got != 1 {
		t.Fatalf("Checkpoints = %d after first Trigger, want 1", got)
	}

	// Second Trigger: idempotent in the sense that it must succeed and
	// produce another checkpoint.
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger #2: %v", err)
	}
	if got := cp.Stats().Checkpoints; got != 2 {
		t.Fatalf("Checkpoints = %d after second Trigger, want 2", got)
	}
}
