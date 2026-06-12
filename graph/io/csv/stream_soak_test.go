//go:build soak || nightly

package csv_test

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"

	"go.uber.org/goleak"
)

// TestCSVStream_1GB_Bounded writes a large path-graph edge list to a
// temporary file and reads it back, asserting that heap growth stays
// within a generous bound.  It exercises the streaming paths without
// pulling the entire dataset into memory at once.
func TestCSVStream_1GB_Bounded(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	const n = 5_000_000 // 5M edges ≈ 100+ MB on disk

	// ---- build adjacency list and write to temp file ----
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := range n {
		src := fmt.Sprintf("n%d", i)
		dst := fmt.Sprintf("n%d", i+1)
		if err := a.AddEdge(src, dst, int64(i)); err != nil {
			t.Fatalf("AddEdge(%d): %v", i, err)
		}
	}

	f, err := os.CreateTemp(t.TempDir(), "csv_soak_*.csv")
	if err != nil {
		t.Fatalf("TempFile: %v", err)
	}
	defer f.Close()

	if _, err := csv.Write(f, a, csv.DefaultOptions()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// ---- capture heap baseline before read ----
	runtime.GC()
	var msBefore runtime.MemStats
	runtime.ReadMemStats(&msBefore)

	// Seek back to start for reading.
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	// ---- read back ----
	b, count, err := csv.ReadInto(f, csv.DefaultOptions())
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if count != n {
		t.Errorf("edge count = %d, want %d", count, n)
	}
	_ = b

	// ---- assert heap growth ----
	runtime.GC()
	var msAfter runtime.MemStats
	runtime.ReadMemStats(&msAfter)

	const maxHeapDeltaMiB = 64
	if msAfter.HeapAlloc > msBefore.HeapAlloc {
		deltaMiB := (msAfter.HeapAlloc - msBefore.HeapAlloc) / (1 << 20)
		if deltaMiB > maxHeapDeltaMiB {
			t.Errorf("heap delta = %d MiB, want ≤ %d MiB", deltaMiB, maxHeapDeltaMiB)
		}
	}
}
