package recovery

import (
	"strconv"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// TestCrossProc_DisjointDirs verifies that N independent writer
// processes each operating on their own snapshot directory produce
// independent, consistent graphs that survive recovery.
//
// The library's single-writer contract (store/txn, store/wal) requires
// that each process owns an exclusive directory. This test validates
// the positive case: disjoint directories → concurrent writers → all
// directories recover cleanly with the expected edge count.
//
// The "txn-write-loop" subproc handler (registered in
// cross_proc_sigkill_test.go) writes n AddEdge ops via txn and exits
// cleanly; recovery.Open must replay all ops from each directory.
func TestCrossProc_DisjointDirs(t *testing.T) {
	t.Parallel()

	const (
		nProcs = 4
		nEdges = 20
	)

	dirs := make([]string, nProcs)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}

	// Spawn nProcs children concurrently, each writing to its own dir.
	var wg sync.WaitGroup
	errs := make([]error, nProcs)
	stderrs := make([][]byte, nProcs)
	wg.Add(nProcs)
	for i := range dirs {
		i := i
		go func() {
			defer wg.Done()
			_, stderrs[i], errs[i] = subproc.Run(t, "txn-write-loop", dirs[i], strconv.Itoa(nEdges))
		}()
	}
	wg.Wait()

	for i := range dirs {
		if errs[i] != nil {
			t.Errorf("child[%d] failed: %v\nstderr: %s", i, errs[i], stderrs[i])
		}
	}
	if t.Failed() {
		return
	}

	// Recover each directory and assert the expected edge count.
	opts := recoveryIntOpts()
	for i, dir := range dirs {
		res, recErr := Open[int, int64](dir, opts)
		if recErr != nil {
			t.Errorf("dir[%d] recovery.Open: %v", i, recErr)
			continue
		}
		adj := res.Graph.AdjList()
		if got := adj.Size(); got != nEdges {
			t.Errorf("dir[%d] Size = %d, want %d", i, got, nEdges)
		}
	}
}
