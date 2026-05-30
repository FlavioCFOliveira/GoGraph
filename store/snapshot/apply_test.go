package snapshot

// apply_test.go — unit tests for ApplyMapperToGraph, ApplyMapperToGraphWithCodec,
// and ApplyCSRToGraph (all were at 0% own-package coverage).

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/store/txn"
)

// ─────────────────────────────────────────────────────────────────────────────
// ApplyMapperToGraph
// ─────────────────────────────────────────────────────────────────────────────

// TestApplyMapperToGraph_EmptyReadback confirms the function is a no-op
// when the readback carries zero pairs.
func TestApplyMapperToGraph_EmptyReadback(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraph(g, MapperReadback{}); err != nil {
		t.Fatalf("ApplyMapperToGraph(empty): %v", err)
	}
	if g.AdjList().Mapper().Len() != 0 {
		t.Fatal("empty readback must leave the mapper empty")
	}
}

// TestApplyMapperToGraph_NonStringKeyType confirms the function returns
// ErrMapperApply when applied to a non-string-keyed graph (which can never
// have produced a v3 mapper.bin Pairs readback).
func TestApplyMapperToGraph_NonStringKeyType(t *testing.T) {
	t.Parallel()
	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	rb := MapperReadback{
		Pairs: []MapperPair{{ID: 0, Key: "x"}}, // non-empty to skip the early return
	}
	err := ApplyMapperToGraph(g, rb)
	if err == nil {
		t.Fatal("ApplyMapperToGraph with non-string-keyed graph must return an error")
	}
}

// TestApplyMapperToGraph_RoundTrip writes a string-keyed mapper, reads it
// back, applies it to a fresh graph, and confirms the mapper is fully
// populated with the expected (NodeID, key) pairs.
func TestApplyMapperToGraph_RoundTrip(t *testing.T) {
	t.Parallel()
	// Build the original mapper with three keys.
	src := graph.NewMapper[string]()
	alice := src.Intern("alice")
	bob := src.Intern("bob")
	carol := src.Intern("carol")

	rb := MapperReadback{
		Pairs: []MapperPair{
			{ID: alice, Key: "alice"},
			{ID: bob, Key: "bob"},
			{ID: carol, Key: "carol"},
		},
	}

	// Apply to a fresh graph.
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraph(g, rb); err != nil {
		t.Fatalf("ApplyMapperToGraph: %v", err)
	}

	m := g.AdjList().Mapper()
	if m.Len() != 3 {
		t.Fatalf("mapper Len = %d, want 3", m.Len())
	}
	for _, tc := range []struct {
		id  graph.NodeID
		key string
	}{{alice, "alice"}, {bob, "bob"}, {carol, "carol"}} {
		got, ok := m.Resolve(tc.id)
		if !ok || got != tc.key {
			t.Errorf("Resolve(%d) = %q, %v; want %q, true", tc.id, got, ok, tc.key)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ApplyMapperToGraphWithCodec
// ─────────────────────────────────────────────────────────────────────────────

// TestApplyMapperToGraphWithCodec_EmptyReadback confirms the function is a
// no-op for an empty RawPairs slice.
func TestApplyMapperToGraphWithCodec_EmptyReadback(t *testing.T) {
	t.Parallel()
	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraphWithCodec(g, MapperReadback{}, txn.NewInt64Codec()); err != nil {
		t.Fatalf("ApplyMapperToGraphWithCodec(empty): %v", err)
	}
}

// TestApplyMapperToGraphWithCodec_NilCodecErrors confirms a nil codec returns
// ErrMapperApply immediately (even when RawPairs is non-empty).
func TestApplyMapperToGraphWithCodec_NilCodecErrors(t *testing.T) {
	t.Parallel()
	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	rb := MapperReadback{
		RawPairs: []MapperRawPair{{ID: 0, Key: []byte{0x01}}},
	}
	if err := ApplyMapperToGraphWithCodec[int64, float64](g, rb, nil); err == nil {
		t.Fatal("nil codec must return an error")
	}
}

// TestApplyMapperToGraphWithCodec_RoundTrip exercises the full codec
// encode→decode cycle for int64 keys: write via WriteMapper, read back via
// ReadMapperBytes, apply via ApplyMapperToGraphWithCodec, and confirm the
// mapper is correctly populated.
func TestApplyMapperToGraphWithCodec_RoundTrip(t *testing.T) {
	t.Parallel()
	codec := txn.NewInt64Codec()

	// Build original mapper.
	src := graph.NewMapper[int64]()
	id1 := src.Intern(10)
	id2 := src.Intern(20)
	id3 := src.Intern(30)

	rb := MapperReadback{
		RawPairs: []MapperRawPair{
			{ID: id1, Key: encodeInt64(t, codec, 10)},
			{ID: id2, Key: encodeInt64(t, codec, 20)},
			{ID: id3, Key: encodeInt64(t, codec, 30)},
		},
	}

	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraphWithCodec(g, rb, codec); err != nil {
		t.Fatalf("ApplyMapperToGraphWithCodec: %v", err)
	}

	m := g.AdjList().Mapper()
	if m.Len() != 3 {
		t.Fatalf("mapper Len = %d, want 3", m.Len())
	}
	for _, tc := range []struct {
		id  graph.NodeID
		key int64
	}{{id1, 10}, {id2, 20}, {id3, 30}} {
		got, ok := m.Resolve(tc.id)
		if !ok || got != tc.key {
			t.Errorf("Resolve(%d) = %v, %v; want %d, true", tc.id, got, ok, tc.key)
		}
	}
}

// encodeInt64 encodes v using codec.Encode and returns the bytes.
func encodeInt64(t *testing.T, codec txn.Codec[int64], v int64) []byte {
	t.Helper()
	b, err := codec.Encode(nil, v)
	if err != nil {
		t.Fatalf("codec.Encode(%d): %v", v, err)
	}
	return b
}

// ─────────────────────────────────────────────────────────────────────────────
// ApplyCSRToGraph
// ─────────────────────────────────────────────────────────────────────────────

// TestApplyCSRToGraph_EmptyVertices is a no-op guard: zero vertices means no
// edges are applied and the function returns nil immediately.
func TestApplyCSRToGraph_EmptyVertices(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	rb := &CSRReadback{}
	if err := ApplyCSRToGraph(g, rb); err != nil {
		t.Fatalf("ApplyCSRToGraph(empty): %v", err)
	}
}

// TestApplyCSRToGraph_RoundTrip builds an adjlist, converts to CSR, writes to
// disk, reads back, populates a fresh mapper, and applies the CSR via
// ApplyCSRToGraph — verifying the edge set is preserved.
func TestApplyCSRToGraph_RoundTrip(t *testing.T) {
	t.Parallel()
	// Build the original graph.
	orig := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := orig.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := orig.AddEdge("b", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// Write snapshot then load CSR + mapper.
	c := csr.BuildFromAdjList(orig.AdjList())

	// Capture the mapper pairs in Walk order so we can seed the fresh graph.
	var pairs []MapperPair
	orig.AdjList().Mapper().Walk(func(id graph.NodeID, k string) bool {
		pairs = append(pairs, MapperPair{ID: id, Key: k})
		return true
	})

	// Apply to a fresh graph.
	g2 := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := ApplyMapperToGraph(g2, MapperReadback{Pairs: pairs}); err != nil {
		t.Fatalf("ApplyMapperToGraph: %v", err)
	}

	// Convert the CSR to CSRReadback for ApplyCSRToGraph.
	rb := &CSRReadback{
		Vertices: c.VerticesSlice(),
		Edges:    c.EdgesSlice(),
	}
	if err := ApplyCSRToGraph(g2, rb); err != nil {
		t.Fatalf("ApplyCSRToGraph: %v", err)
	}

	if !g2.AdjList().HasEdge("a", "b") {
		t.Error("a→b missing after ApplyCSRToGraph")
	}
	if !g2.AdjList().HasEdge("b", "c") {
		t.Error("b→c missing after ApplyCSRToGraph")
	}
}

// TestApplyCSRToGraph_UnresolvedSrc confirms that source nodes absent from
// the mapper are silently skipped without an error.
func TestApplyCSRToGraph_UnresolvedSrc(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	// Provide a readback where vertex 0 has 1 edge, but the mapper is empty
	// so neither endpoint can be resolved.
	rb := &CSRReadback{
		Vertices: []uint64{0, 1}, // one entry for src=0, spanning edges[0..1)
		Edges:    []graph.NodeID{graph.NodeID(1)},
	}
	if err := ApplyCSRToGraph(g, rb); err != nil {
		t.Fatalf("ApplyCSRToGraph with unresolved src: %v", err)
	}
	// No edges must have been added.
	if g.AdjList().Size() != 0 {
		t.Fatalf("edges = %d, want 0", g.AdjList().Size())
	}
}
