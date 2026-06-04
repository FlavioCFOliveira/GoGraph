package extern

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// writeGridReader writes an m×n grid CSR to a temp file and opens a
// Reader over it. The Reader is NOT registered for cleanup: tests
// that race Close own the close themselves.
func writeGridReader(t *testing.T, m, n int) *csrfile.Reader {
	t.Helper()
	g, err := shapegen.Grid(m, n, false).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Grid.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	path := filepath.Join(t.TempDir(), "grid.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		t.Fatalf("csrfile.WriteToFile: %v", err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	return r
}

// TestBFSRacingClose drives a real extern.BFS read loop in one
// goroutine while another closes the shared Reader after a short
// delay. Before the scoped-read fix, Close unmapped the region while
// BFS was still iterating the mmap-aliased edges slice — a data race
// the race detector flags and a use-after-unmap fault that kills the
// whole process. With BFS running inside Reader.Read, Close blocks
// until the in-flight traversal returns, so every BFS call either
// completes cleanly or, once Close has won, returns ErrReaderClosed.
//
// The test must be observed FAILING (race report and/or SIGSEGV/
// SIGBUS) on the pre-fix bind-once-iterate code and PASSING under
// -race after the migration.
func TestBFSRacingClose(t *testing.T) {
	// Not parallel: this test deliberately provokes a process-fatal
	// fault on regressed code; isolating it keeps the failure signal
	// unambiguous.
	r := writeGridReader(t, 60, 60) // 3600 nodes, plenty of edges to iterate

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		loopErrs = map[string]int{}
	)

	// Reader goroutine: a tight BFS loop until Close makes the Reader
	// unusable. Any returned error other than ErrReaderClosed is a
	// real failure.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			err := BFS(r, graph.NodeID(0), func(graph.NodeID, int) bool { return true })
			if err == nil {
				continue
			}
			mu.Lock()
			loopErrs[err.Error()]++
			mu.Unlock()
			if errors.Is(err, csrfile.ErrReaderClosed) {
				return // Reader closed underneath us: expected, stop.
			}
			t.Errorf("BFS returned unexpected error: %v", err)
			return
		}
	}()

	// Let the reader loop spin up and get deep into iterating the
	// mapping, then close concurrently.
	time.Sleep(2 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	wg.Wait()

	// After Close, a fresh BFS must fail cleanly with the typed error,
	// never crash and never read freed memory.
	if err := BFS(r, graph.NodeID(0), func(graph.NodeID, int) bool { return true }); !errors.Is(err, csrfile.ErrReaderClosed) {
		t.Errorf("post-Close BFS: got %v, want ErrReaderClosed", err)
	}
}

// TestBFSAndPageRankRacingCloseManyRounds is a higher-pressure
// variant: it repeats the open / race-Close cycle many times across
// both extern algorithms, maximising the chance of catching the
// unmap precisely while a traversal is mid-iteration. Each round uses
// its own Reader so a closed Reader never leaks into the next round.
func TestBFSAndPageRankRacingCloseManyRounds(t *testing.T) {
	const rounds = 40
	for round := 0; round < rounds; round++ {
		r := writeGridReader(t, 40, 40) // 1600 nodes

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for {
				err := BFS(r, graph.NodeID(0), func(graph.NodeID, int) bool { return true })
				if err == nil {
					continue
				}
				if errors.Is(err, csrfile.ErrReaderClosed) {
					return
				}
				t.Errorf("round %d: BFS unexpected error: %v", round, err)
				return
			}
		}()

		go func() {
			defer wg.Done()
			opts := DefaultPageRankOptions()
			opts.MaxIterations = 5 // short runs => many race attempts
			for {
				_, _, err := PageRank(r, opts)
				if err == nil {
					continue
				}
				if errors.Is(err, csrfile.ErrReaderClosed) {
					return
				}
				t.Errorf("round %d: PageRank unexpected error: %v", round, err)
				return
			}
		}()

		// Stagger the close so the readers are mid-iteration.
		time.Sleep(time.Millisecond)
		if err := r.Close(); err != nil {
			t.Errorf("round %d: Close: %v", round, err)
		}
		wg.Wait()
	}
}
