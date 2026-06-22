package btree

import (
	"bytes"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestBulkLoadSorted_MatchesBulkLoad verifies that BulkLoadSorted on ascending
// input builds a tree byte-identical to BulkLoad on the same data (BulkLoad
// sorts internally), including duplicate keys whose nodes are unioned.
func TestBulkLoadSorted_MatchesBulkLoad(t *testing.T) {
	t.Parallel()
	const n = 5000
	values := make([]int, n)
	nodes := make([]graph.NodeID, n)
	for i := range values {
		values[i] = i / 2 // each key appears twice -> exercises the union grouping
		nodes[i] = graph.NodeID(uint64(i))
	}

	viaBulk := New[int]()
	if err := viaBulk.BulkLoad(values, nodes); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	viaSorted := New[int]()
	if err := viaSorted.BulkLoadSorted(values, nodes); err != nil {
		t.Fatalf("BulkLoadSorted: %v", err)
	}

	var a, b bytes.Buffer
	if err := viaBulk.Serialize(&a); err != nil {
		t.Fatalf("Serialize(bulk): %v", err)
	}
	if err := viaSorted.Serialize(&b); err != nil {
		t.Fatalf("Serialize(sorted): %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("BulkLoadSorted tree differs from BulkLoad: %d vs %d bytes", a.Len(), b.Len())
	}
}

// TestBulkLoadSorted_Rejects covers the input-validation contract.
func TestBulkLoadSorted_Rejects(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	if err := idx.BulkLoadSorted([]int{1, 2}, []graph.NodeID{1}); !errors.Is(err, ErrMismatchedLengths) {
		t.Fatalf("length mismatch: got %v, want ErrMismatchedLengths", err)
	}
	if err := idx.BulkLoadSorted([]int{3, 1, 2}, []graph.NodeID{1, 2, 3}); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("unsorted: got %v, want ErrNotSorted", err)
	}
}

// benchSortedInputs builds n ascending (value, node) pairs reused across the
// BulkLoad / BulkLoadSorted allocation comparison.
func benchSortedInputs(n int) ([]int, []graph.NodeID) {
	values := make([]int, n)
	nodes := make([]graph.NodeID, n)
	for i := range values {
		values[i] = i
		nodes[i] = graph.NodeID(uint64(i))
	}
	return values, nodes
}

// BenchmarkIndex_BulkLoad_Sorted contrasts the two bulk-load paths on the same
// pre-sorted input: BulkLoad copies into a []pair and sorts, then groups and
// packs; BulkLoadSorted validates order in place and skips straight to group +
// pack. The Sorted sub-benchmark should report markedly lower B/op.
func BenchmarkIndex_BulkLoad_Sorted(b *testing.B) {
	const n = 1_000_000
	values, nodes := benchSortedInputs(n)

	b.Run("BulkLoad", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			idx := New[int]()
			if err := idx.BulkLoad(values, nodes); err != nil {
				b.Fatalf("BulkLoad: %v", err)
			}
		}
	})
	b.Run("BulkLoadSorted", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			idx := New[int]()
			if err := idx.BulkLoadSorted(values, nodes); err != nil {
				b.Fatalf("BulkLoadSorted: %v", err)
			}
		}
	})
}
