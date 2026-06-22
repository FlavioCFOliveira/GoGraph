package snapshot

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestWriteReadCSR_Weightless_RoundTrip proves a weightless graph persists with
// hasWeights=0 and recovers structurally identical with weights legitimately
// absent. A weightless adjacency over a non-empty W (int64) yields a nil-weights
// CSR; WriteCSR persists no weights section (hasWeights byte = 0); ReadCSR
// reports HasWeights=false / WeightSize=0 / WeightBytes=nil; and the topology
// (vertices offsets + edges) round-trips byte-for-byte.
func TestWriteReadCSR_Weightless_RoundTrip(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true, Weightless: true})
	// Non-zero weights, all ignored by the weightless graph.
	if err := a.AddEdge("a", "b", 11); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("a", "c", 22); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("b", "c", 33); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	if c.WeightsSlice() != nil {
		t.Fatalf("precondition: weightless CSR WeightsSlice() = %v, want nil", c.WeightsSlice())
	}

	var buf bytes.Buffer
	size, csum, err := WriteCSR(&buf, c)
	if err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	if size <= 0 || csum == 0 {
		t.Fatalf("size=%d csum=%d", size, csum)
	}

	back, err := ReadCSR(&buf)
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	// The durability claim: weights are absent on disk.
	if back.HasWeights {
		t.Fatal("weightless snapshot: HasWeights = true, want false")
	}
	if back.WeightSize != 0 {
		t.Fatalf("weightless snapshot: WeightSize = %d, want 0", back.WeightSize)
	}
	if back.WeightBytes != nil {
		t.Fatalf("weightless snapshot: WeightBytes = %v, want nil", back.WeightBytes)
	}
	// Topology round-trips exactly.
	if uint64(len(back.Vertices)) != uint64(len(c.VerticesSlice())) {
		t.Fatalf("vertices length mismatch: got %d want %d", len(back.Vertices), len(c.VerticesSlice()))
	}
	for i := range back.Vertices {
		if back.Vertices[i] != c.VerticesSlice()[i] {
			t.Fatalf("vertices[%d] mismatch: got %d want %d", i, back.Vertices[i], c.VerticesSlice()[i])
		}
	}
	if len(back.Edges) != len(c.EdgesSlice()) {
		t.Fatalf("edges length mismatch: got %d want %d", len(back.Edges), len(c.EdgesSlice()))
	}
	for i := range back.Edges {
		if back.Edges[i] != c.EdgesSlice()[i] {
			t.Fatalf("edges[%d] mismatch: got %d want %d", i, back.Edges[i], c.EdgesSlice()[i])
		}
	}
}

// TestWriteCSR_Weighted_PersistsWeights is the regression guard: a NON-weightless
// graph over the same W=int64 still persists its weights section (hasWeights=1,
// weight size 8, weight bytes present), so the weightless feature does not
// perturb the weighted persistence path.
func TestWriteCSR_Weighted_PersistsWeights(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 11); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("a", "c", 22); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	if c.WeightsSlice() == nil {
		t.Fatal("precondition: weighted CSR WeightsSlice() is nil, want populated")
	}

	var buf bytes.Buffer
	if _, _, err := WriteCSR(&buf, c); err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	back, err := ReadCSR(&buf)
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	if !back.HasWeights {
		t.Fatal("weighted snapshot: HasWeights = false, want true")
	}
	if back.WeightSize != 8 {
		t.Fatalf("weighted snapshot: WeightSize = %d, want 8 (int64)", back.WeightSize)
	}
	if len(back.WeightBytes) != 8*len(back.Edges) {
		t.Fatalf("weighted snapshot: WeightBytes len = %d, want %d", len(back.WeightBytes), 8*len(back.Edges))
	}
}

var _ = graph.NodeID(0)
