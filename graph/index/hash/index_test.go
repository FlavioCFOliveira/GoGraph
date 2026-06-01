package hash

import (
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

func TestIndex_Basic(t *testing.T) {
	t.Parallel()
	idx := New[string]()
	idx.Insert("alice@example.com", graph.NodeID(1))
	idx.Insert("alice@example.com", graph.NodeID(5))
	idx.Insert("bob@example.com", graph.NodeID(2))

	if idx.Cardinality("alice@example.com") != 2 {
		t.Fatalf("Cardinality alice = %d, want 2", idx.Cardinality("alice@example.com"))
	}
	if !idx.Contains("alice@example.com", graph.NodeID(1)) {
		t.Fatalf("Contains alice/1 should be true")
	}
	if idx.Contains("alice@example.com", graph.NodeID(9)) {
		t.Fatalf("Contains alice/9 should be false")
	}
	if idx.DistinctValues() != 2 {
		t.Fatalf("DistinctValues = %d, want 2", idx.DistinctValues())
	}

	bm := idx.Lookup("alice@example.com")
	if bm.GetCardinality() != 2 {
		t.Fatalf("Lookup cardinality = %d", bm.GetCardinality())
	}

	idx.Delete("alice@example.com", graph.NodeID(1))
	if idx.Contains("alice@example.com", graph.NodeID(1)) {
		t.Fatalf("Contains alice/1 should be false after delete")
	}
}

func TestIndex_DeleteEmptiesEntry(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	idx.Insert(42, graph.NodeID(1))
	idx.Delete(42, graph.NodeID(1))
	if idx.DistinctValues() != 0 {
		t.Fatalf("DistinctValues = %d, want 0", idx.DistinctValues())
	}
}

func TestIndex_LookupReturnsClone(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	idx.Insert(7, graph.NodeID(1))
	bm := idx.Lookup(7)
	bm.Add(99) // mutating the returned clone must not affect the index
	if idx.Cardinality(7) != 1 {
		t.Fatalf("Cardinality = %d, want 1 after mutating clone", idx.Cardinality(7))
	}
}

func TestIndex_RandomisedVsBaseline(t *testing.T) {
	t.Parallel()
	const n = 10_000
	r := rand.New(rand.NewPCG(7, 1)) //nolint:gosec // deterministic test RNG
	idx := New[int]()
	baseline := map[int]map[graph.NodeID]struct{}{}
	for i := 0; i < n; i++ {
		v := r.IntN(256)
		node := graph.NodeID(r.IntN(1024))
		idx.Insert(v, node)
		if baseline[v] == nil {
			baseline[v] = map[graph.NodeID]struct{}{}
		}
		baseline[v][node] = struct{}{}
	}
	for v, set := range baseline {
		if idx.Cardinality(v) != uint64(len(set)) {
			t.Fatalf("v=%d Cardinality mismatch: got=%d want=%d",
				v, idx.Cardinality(v), len(set))
		}
		for node := range set {
			if !idx.Contains(v, node) {
				t.Fatalf("v=%d node=%d missing", v, node)
			}
		}
	}
}

func TestIndex_Concurrent(t *testing.T) {
	t.Parallel()
	idx := New[int]()
	const goroutines = 256
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for w := 0; w < goroutines; w++ {
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 11)) //nolint:gosec // deterministic test RNG
			for i := 0; i < 1024; i++ {
				v := r.IntN(64)
				node := graph.NodeID(uint64(w*1024 + i))
				idx.Insert(v, node)
				_ = idx.Lookup(v)
			}
		}(w)
	}
	wg.Wait()
}

func BenchmarkIndex_LookupHot(b *testing.B) {
	idx := New[int]()
	for i := 0; i < 1_000_000; i++ {
		idx.Insert(i%2048, graph.NodeID(uint64(i)))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Cardinality(42)
	}
}
