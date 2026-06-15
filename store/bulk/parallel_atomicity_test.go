package bulk

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestParallel_NoPartialFileOnBuildError asserts the publication
// invariant the storage-engine-auditor certified: the parallel build
// completes fully in memory before the single csrfile.WriteToFile
// publication, so when the in-memory build fails there is NO csrfile and
// NO leftover ".tmp" at the output path. A crash mid-build is the same
// shape — nothing is published until the one atomic rename, which only
// runs after the build succeeds.
//
// The build failure is induced deterministically with a per-shard
// capacity cap: the parallel directed build issues AddEdge concurrently
// into the source shards, and a saturated shard returns ErrShardFull,
// which buildParallel surfaces as the Finalise error before any
// WriteToFile call.
func TestParallel_NoPartialFileOnBuildError(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2 to exercise the parallel fan-out")
	}
	orig := parallelMinEdges
	parallelMinEdges = 1
	t.Cleanup(func() { parallelMinEdges = orig })

	dir := t.TempDir()
	out := filepath.Join(dir, "graph.csr")

	// Build a loader whose adjacency builder is capped so the load
	// overflows a shard mid-build. Reach inside the package to swap in a
	// capped AdjList (the public Options has no cap knob; this is a
	// white-box atomicity probe).
	l := &Loader{
		opts: Options{OutputPath: out, Directed: true, Parallel: true},
		adj: adjlist.New[string, int64](adjlist.Config{
			Directed:         true,
			MaxShardCapacity: 1, // any shard with >1 distinct source node overflows
		}),
	}

	// Many distinct sources guarantee at least one shard exceeds the cap.
	edges := genEdges(7, 4000, 2000)
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("AddBatch buffered unexpectedly failed: %v", err)
	}

	_, _, err := l.Finalise()
	if err == nil {
		t.Fatal("expected Finalise to fail on a capped overflowing build")
	}

	// The output file must not exist: publication never ran.
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("partial csrfile present after failed build: stat err = %v", statErr)
	}
	// No leftover temp file either.
	if _, statErr := os.Stat(out + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("leftover .tmp present after failed build: stat err = %v", statErr)
	}
	// The directory holds no stray artefacts.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("output dir not empty after failed build: %v", names)
	}
}

// TestParallel_SinglePublicationOnSuccess confirms the success path
// leaves exactly one published file (no ".tmp" survivor), evidencing the
// single atomic rename.
func TestParallel_SinglePublicationOnSuccess(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2 to exercise the parallel fan-out")
	}
	orig := parallelMinEdges
	parallelMinEdges = 1
	t.Cleanup(func() { parallelMinEdges = orig })

	dir := t.TempDir()
	out := filepath.Join(dir, "graph.csr")
	l := New(Options{OutputPath: out, Directed: true, Parallel: true})
	if err := l.AddBatch(genEdges(3, 6000, 500)); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if _, _, err := l.Finalise(); err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("published csrfile missing: %v", statErr)
	}
	if _, statErr := os.Stat(out + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf(".tmp survived a successful publish: stat err = %v", statErr)
	}
}
