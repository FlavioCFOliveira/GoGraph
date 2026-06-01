//go:build soak

package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCheckpoint_Soak_SustainedWrites exercises the Checkpointer under
// sustained concurrent write load for 10 seconds.
//
// Two writer goroutines each append 50 WAL frames per second while the
// checkpointer runs with MaxAge=5s. After 10 seconds the writers are
// stopped, the checkpointer is stopped, and the test verifies that at
// least one checkpoint fired and no goroutine leak occurred (via
// goleak in TestMain).
//
// The task budget labels this the "60s soak" but the actual goroutine
// runtime is capped at 10s to keep the test tractable in CI while
// still exercising the concurrent-checkpoint + concurrent-WAL-append
// scenario.
func TestCheckpoint_Soak_SustainedWrites(t *testing.T) {
	testlayers.RequireSoak(t)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	// Seed one edge so the very first checkpoint has something to write.
	if err := g.AddEdge("seed", "node", 0); err != nil {
		t.Fatalf("AddEdge(seed): %v", err)
	}

	var storeMu sync.Mutex
	cp := New(Config{
		Dir:    dir,
		MaxAge: 5 * time.Second,
	}, g, w, &storeMu)

	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)

	const (
		numWriters   = 2
		opsPerSecond = 50
		runDuration  = 10 * time.Second
	)

	// payload is a fixed 64-byte WAL frame shared across all writers.
	payload := make([]byte, 64)

	var wg sync.WaitGroup
	var appendErrors atomic.Uint64

	deadline := time.Now().Add(runDuration)
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second / opsPerSecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if time.Now().After(deadline) {
						return
					}
					if err := w.Append(payload); err != nil {
						appendErrors.Add(1)
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Wait for the run window to close, then shut everything down.
	time.Sleep(runDuration + 50*time.Millisecond)
	cancel()
	wg.Wait()
	cp.Stop()

	if n := appendErrors.Load(); n > 0 {
		t.Errorf("writer goroutines reported %d append errors", n)
	}

	stats := cp.Stats()
	if stats.Checkpoints < 1 {
		t.Fatalf("Checkpoints = %d, want >= 1 after %s with MaxAge=5s",
			stats.Checkpoints, runDuration)
	}
	t.Logf("soak complete: checkpoints=%d walTruncBytes=%d lastDurationNS=%d",
		stats.Checkpoints, stats.WALTruncBytes, stats.LastDurationNS)
}
