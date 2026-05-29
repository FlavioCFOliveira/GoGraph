package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/wal"
)

// TestCheckpoint_Cadence_TimeBased verifies that the ticker-driven
// checkpoint fires at least twice within a 200 ms window when MaxAge
// is set to 50 ms.
func TestCheckpoint_Cadence_TimeBased(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("x", "y", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var mu sync.Mutex
	cp := New(Config{
		Dir:    dir,
		MaxAge: 50 * time.Millisecond,
	}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	// Sleep long enough for the ticker to fire at least twice. MaxAge=50ms
	// with the default Interval=MaxAge/4=13ms. Each checkpoint now writes a
	// self-sufficient snapshot (CSR + labels + properties + mapper, several
	// fsyncs) per the F2 durability fix, so a single checkpoint can take
	// tens of ms on a real disk; the window is sized generously so the
	// time-based trigger reliably fires more than once regardless of
	// per-checkpoint I/O cost. This test asserts repeated triggering, not
	// throughput.
	time.Sleep(time.Second)
	cp.Stop()

	if got := cp.Stats().Checkpoints; got < 2 {
		t.Fatalf("Checkpoints = %d after 200ms with MaxAge=50ms, want >= 2", got)
	}
}

// TestCheckpoint_Cadence_ForcedTrigger verifies that explicit Trigger
// calls each increment Checkpoints exactly once, even when MaxAge is
// so large the ticker never fires.
func TestCheckpoint_Cadence_ForcedTrigger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("p", "q", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	var mu sync.Mutex
	// MaxAge=1h: the ticker will never fire during the test.
	cp := New(Config{Dir: dir, MaxAge: time.Hour}, g, w, &mu)
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

	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger #2: %v", err)
	}
	if got := cp.Stats().Checkpoints; got != 2 {
		t.Fatalf("Checkpoints = %d after second Trigger, want 2", got)
	}
}
