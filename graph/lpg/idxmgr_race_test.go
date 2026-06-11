package lpg_test

// Gate: go test -race ./graph/lpg/...
//
// Before the fix (idxMgr was a plain *index.Manager pointer), the Go race
// detector would report a DATA RACE on g.idxMgr because SetIndexManager
// wrote the field without synchronisation while IndexManager read it on a
// different goroutine. After the fix the field is backed by an
// [atomic.Pointer] (atomicIndexManager), so the race detector sees only
// sequentially-consistent atomic accesses and reports no race.

import (
	"runtime"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSetIndexManagerConcurrentRace verifies that concurrent calls to
// SetIndexManager (writer) and IndexManager (reader) do not race.
// Run with: go test -race ./graph/lpg/...
func TestSetIndexManagerConcurrentRace(t *testing.T) {
	// Maximise parallelism to increase the chance of detecting a race on an
	// unfixed binary.
	runtime.GOMAXPROCS(runtime.NumCPU())

	g := lpg.New[int, float64](adjlist.Config{Directed: true})

	// Two managers to alternate between (nil is also valid).
	mgr1 := index.NewManager()
	mgr2 := index.NewManager()

	const iterations = 10_000

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: alternate between mgr1, nil, and mgr2 in a tight loop.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			switch i % 3 {
			case 0:
				g.SetIndexManager(mgr1)
			case 1:
				g.SetIndexManager(nil)
			case 2:
				g.SetIndexManager(mgr2)
			}
			runtime.Gosched()
		}
	}()

	// Reader: call IndexManager in a tight loop; use the result to prevent
	// the compiler from optimising away the read.
	go func() {
		defer wg.Done()
		var sink *index.Manager
		for i := 0; i < iterations; i++ {
			sink = g.IndexManager()
			runtime.Gosched()
		}
		// Prevent sink from being eliminated by the compiler.
		_ = sink
	}()

	wg.Wait()
}
