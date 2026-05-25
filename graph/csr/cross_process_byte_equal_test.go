package csr_test

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestCSR_CrossProcess_ByteEqual verifies that two CSR instances built
// from identical inputs produce byte-equal vertex and edge slices.
//
// This is a same-process proxy for the cross-process stability guarantee:
// BuildFromAdjList must be a pure function of its input — same
// AdjList state produces the same vertices[] and edges[] arrays.
// Because the Mapper uses a deterministic FNV-1a hash (not a
// process-local maphash seed), the NodeID assignment is stable across
// processes, so byte-equality here implies byte-equality cross-process
// for the same (shape, seed) pair.
//
// The fixture is BarabasiAlbert(n=1000, m0=3, seed=42): large enough
// to stress the offset computation, small enough for a short-layer test.
func TestCSR_CrossProcess_ByteEqual(t *testing.T) {
	t.Parallel()

	shape := shapegen.BarabasiAlbert(1000, 3, 42)

	buildCSR := func() *csr.CSR[int64] {
		g, err := shape.Build(adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return csr.BuildFromAdjList(g.AdjList())
	}

	c1 := buildCSR()
	c2 := buildCSR()

	v1, v2 := c1.VerticesSlice(), c2.VerticesSlice()
	if len(v1) != len(v2) {
		t.Fatalf("vertices length mismatch: %d vs %d", len(v1), len(v2))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Errorf("vertices[%d] mismatch: %d vs %d", i, v1[i], v2[i])
			break
		}
	}

	e1, e2 := c1.EdgesSlice(), c2.EdgesSlice()
	if len(e1) != len(e2) {
		t.Fatalf("edges length mismatch: %d vs %d", len(e1), len(e2))
	}
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Errorf("edges[%d] mismatch: %d vs %d", i, e1[i], e2[i])
			break
		}
	}

	// BarabasiAlbert carries no weight payload (int64 sentinel 0 for all
	// edges), but WeightsSlice is populated because W=int64 is not struct{}.
	// Verify the weight arrays are also byte-equal.
	w1, w2 := c1.WeightsSlice(), c2.WeightsSlice()
	if (w1 == nil) != (w2 == nil) {
		t.Fatalf("weights nil-ness mismatch: c1=%v c2=%v", w1 == nil, w2 == nil)
	}
	if len(w1) != len(w2) {
		t.Fatalf("weights length mismatch: %d vs %d", len(w1), len(w2))
	}
	for i := range w1 {
		if w1[i] != w2[i] {
			t.Errorf("weights[%d] mismatch: %d vs %d", i, w1[i], w2[i])
			break
		}
	}

	t.Logf("CSR byte-equal: vertices=%d edges=%d weights=%d", len(v1), len(e1), len(w1))
}
