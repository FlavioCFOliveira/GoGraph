package btree

import (
	"errors"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

func TestIndex_InsertAndRange(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	for i := 1; i <= 10; i++ {
		idx.Insert(i, graph.NodeID(uint64(i*10)))
	}
	bm := idx.Range(3, 7)
	if bm.GetCardinality() != 5 {
		t.Fatalf("range cardinality = %d, want 5", bm.GetCardinality())
	}
	for v := 3; v <= 7; v++ {
		if !bm.Contains(uint64(v * 10)) {
			t.Fatalf("missing node %d in range", v*10)
		}
	}
}

func TestIndex_Lookup(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	idx.Insert(5, graph.NodeID(50))
	idx.Insert(5, graph.NodeID(55))
	idx.Insert(7, graph.NodeID(70))
	bm := idx.Lookup(5)
	if bm.GetCardinality() != 2 {
		t.Fatalf("Lookup(5) = %d, want 2", bm.GetCardinality())
	}
	if idx.Lookup(99).GetCardinality() != 0 {
		t.Fatalf("Lookup(unknown) must be empty")
	}
}

func TestIndex_Delete(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	idx.Insert(1, graph.NodeID(10))
	idx.Insert(1, graph.NodeID(11))
	idx.Delete(1, graph.NodeID(10))
	if idx.Cardinality(1) != 1 {
		t.Fatalf("Cardinality = %d, want 1", idx.Cardinality(1))
	}
	idx.Delete(1, graph.NodeID(11))
	if idx.DistinctValues() != 0 {
		t.Fatalf("DistinctValues = %d, want 0 after last delete", idx.DistinctValues())
	}
}

func TestIndex_BulkLoad(t *testing.T) {
	t.Parallel()
	const n = 10000
	values := make([]int, n)
	nodes := make([]graph.NodeID, n)
	r := rand.New(rand.NewPCG(99, 1)) //nolint:gosec // deterministic test RNG
	for i := 0; i < n; i++ {
		values[i] = r.IntN(1024)
		nodes[i] = graph.NodeID(uint64(i))
	}
	idx := New[int]()
	if err := idx.BulkLoad(values, nodes); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	// Compare against an ad-hoc inverted map.
	want := map[int]uint64{}
	for i, v := range values {
		_ = i
		want[v]++
	}
	for v, n := range want {
		if idx.Cardinality(v) != n {
			t.Fatalf("v=%d Cardinality = %d, want %d", v, idx.Cardinality(v), n)
		}
	}
}

func TestIndex_RangeProperty(t *testing.T) {
	t.Parallel()
	// Random insertion order; range query must still respect ordering.
	idx := New[int]()
	r := rand.New(rand.NewPCG(7, 1)) //nolint:gosec // deterministic test RNG
	values := r.Perm(100)
	for _, v := range values {
		idx.Insert(v, graph.NodeID(uint64(v+1000)))
	}
	bm := idx.Range(25, 75)
	if bm.GetCardinality() != 51 {
		t.Fatalf("range cardinality = %d, want 51", bm.GetCardinality())
	}
	for v := 25; v <= 75; v++ {
		if !bm.Contains(uint64(v + 1000)) {
			t.Fatalf("missing v=%d", v)
		}
	}
}

func TestIndex_Concurrent(t *testing.T) {
	testlayers.RequireSoak(t) // 64-worker race stress → soak layer (short-layer per-package budget, #1460)
	t.Parallel()
	idx := New[int]()
	const workers = 64
	const per = 256
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 13)) //nolint:gosec // deterministic test RNG
			for i := 0; i < per; i++ {
				v := r.IntN(512)
				idx.Insert(v, graph.NodeID(uint64(w*per+i)))
				_ = idx.Range(0, 256)
			}
		}(w)
	}
	wg.Wait()
}

func BenchmarkIndex_RangeFirst(b *testing.B) {
	values := make([]int, 10_000_000)
	nodes := make([]graph.NodeID, 10_000_000)
	for i := range values {
		values[i] = i
		nodes[i] = graph.NodeID(uint64(i))
	}
	idx := New[int]()
	if err := idx.BulkLoad(values, nodes); err != nil {
		b.Fatalf("BulkLoad: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = idx.RangeFirst(5_000_000, 9_999_999)
	}
}

func BenchmarkIndex_BulkLoad_10M(b *testing.B) {
	for n := 0; n < b.N; n++ {
		values := make([]int, 10_000_000)
		nodes := make([]graph.NodeID, 10_000_000)
		for i := range values {
			values[i] = i
			nodes[i] = graph.NodeID(uint64(i))
		}
		idx := New[int]()
		b.StartTimer()
		if err := idx.BulkLoad(values, nodes); err != nil {
			b.Fatalf("BulkLoad: %v", err)
		}
		b.StopTimer()
	}
}

// TestIndex_BulkLoad_LengthMismatch covers the new error return on
// mismatched values/nodes slices. Before sprint 21 this was a
// panic("btree: values and nodes must have the same length").
func TestIndex_BulkLoad_LengthMismatch(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	values := []int{1, 2, 3}
	nodes := []graph.NodeID{10, 20} // shorter
	err := idx.BulkLoad(values, nodes)
	if !errors.Is(err, ErrMismatchedLengths) {
		t.Fatalf("err=%v, want ErrMismatchedLengths", err)
	}

	// Symmetric: values shorter than nodes.
	err = idx.BulkLoad([]int{1}, []graph.NodeID{10, 20, 30})
	if !errors.Is(err, ErrMismatchedLengths) {
		t.Fatalf("err=%v, want ErrMismatchedLengths", err)
	}

	// Empty/empty must be a valid no-op (no error).
	if err := idx.BulkLoad(nil, nil); err != nil {
		t.Fatalf("nil/nil err=%v, want nil", err)
	}
}
