//go:build soak

package jsonl_test

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"go.uber.org/goleak"

	"gograph/graph/adjlist"
	"gograph/graph/io/jsonl"
	"gograph/internal/testlayers"
)

// TestJSONL_Stream100MB_Bounded writes a 500 000-node path graph to a
// temp file and reads it back, verifying edge count and that the heap
// growth stays within a 64 MiB budget.
func TestJSONL_Stream100MB_Bounded(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	const n = 500_000

	// Build a directed path graph: n0→n1→n2→…→n(N-1).
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := range n {
		if err := a.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("AddNode n%d: %v", i, err)
		}
	}
	for i := range n - 1 {
		if err := a.AddEdge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1), int64(i)); err != nil {
			t.Fatalf("AddEdge n%d→n%d: %v", i, i+1, err)
		}
	}

	// Measure heap before write.
	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	// Write to a temp file.
	f, err := os.CreateTemp(t.TempDir(), "jsonl_soak_*.jsonl")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	wName := f.Name()

	written, err := jsonl.Write(f, a)
	if err != nil {
		f.Close()
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close write: %v", err)
	}
	// n nodes + (n-1) edges.
	wantRecords := n + (n - 1)
	if written != wantRecords {
		t.Errorf("Write returned %d records, want %d", written, wantRecords)
	}

	// Re-open for reading.
	rf, err := os.Open(wName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rf.Close()

	b, rows, err := jsonl.ReadInto(rf, adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	if rows != wantRecords {
		t.Errorf("ReadInto consumed %d rows, want %d", rows, wantRecords)
	}
	if b.Size() != uint64(n-1) {
		t.Errorf("edge count = %d, want %d", b.Size(), n-1)
	}

	// Heap delta check.
	var memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memAfter)

	const maxHeapGrowth = 64 << 20 // 64 MiB
	if memAfter.HeapAlloc > memBefore.HeapAlloc+maxHeapGrowth {
		t.Errorf("heap grew by %d bytes (>%d); before=%d after=%d",
			memAfter.HeapAlloc-memBefore.HeapAlloc, maxHeapGrowth,
			memBefore.HeapAlloc, memAfter.HeapAlloc)
	}
}
