package snapshot

// edgehandles_test.go — durability coverage for the edgehandles.bin component
// and the optional CSR handle column. Each test builds a graph with stable
// edge handles and per-handle metadata, persists it, DISCARDS the source, and
// asserts the handle column and per-handle type/properties survive on a fresh
// graph.
//
// Layer: short.

import (
	"bytes"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newHandleGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	return lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
}

// TestEdgeHandles_RoundTrip writes the per-handle metadata of two distinctly
// typed parallel edges, reads it back into a fresh graph (after restoring the
// mapper + CSR handle column), and asserts each parallel edge keeps its own
// per-CREATE type and properties keyed to its handle.
func TestEdgeHandles_RoundTrip(t *testing.T) {
	t.Parallel()
	g := newHandleGraph(t)
	if err := g.AddNode("x"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddNode("y"); err != nil {
		t.Fatal(err)
	}
	h1, _ := g.AddEdgeH("x", "y", 1)
	g.SetEdgeLabelByHandle("x", "y", h1, "USES")
	if err := g.SetEdgePropertyByHandle("x", "y", h1, "w", lpg.Int64Value(7)); err != nil {
		t.Fatalf("SetEdgePropertyByHandle: %v", err)
	}
	h2, _ := g.AddEdgeH("x", "y", 1)
	g.SetEdgeLabelByHandle("x", "y", h2, "CALLS")

	var buf bytes.Buffer
	_, _, emitted, err := WriteEdgeHandles(&buf, g)
	if err != nil {
		t.Fatalf("WriteEdgeHandles: %v", err)
	}
	if !emitted {
		t.Fatal("WriteEdgeHandles emitted=false, want true (graph has per-handle metadata)")
	}

	rb, err := ReadEdgeHandles(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadEdgeHandles: %v", err)
	}
	if len(rb.Records) != 2 {
		t.Fatalf("readback records = %d, want 2", len(rb.Records))
	}

	// Rebuild a fresh graph with the same node interning and CSR handle
	// column, then apply the readback.
	fresh := newHandleGraph(t)
	cs := csr.BuildFromAdjList(g.AdjList())
	rbCSR := csrReadbackFrom(cs)
	if err := snapshotApplyMapperFrom(fresh, g); err != nil {
		t.Fatalf("mapper restore: %v", err)
	}
	if err := ApplyCSRToGraph(fresh, &rbCSR); err != nil {
		t.Fatalf("ApplyCSRToGraph: %v", err)
	}
	ApplyEdgeHandlesToGraph(fresh, rb)

	// Both handles must resolve their own type on the fresh graph.
	if got := fresh.EdgeLabelsByHandle("x", "y", h1); !hasOnly(got, "USES") {
		t.Fatalf("handle h1 type = %v, want [USES]", got)
	}
	if got := fresh.EdgeLabelsByHandle("x", "y", h2); !hasOnly(got, "CALLS") {
		t.Fatalf("handle h2 type = %v, want [CALLS]", got)
	}
	props := fresh.EdgePropertiesByHandle("x", "y", h1)
	if v, ok := props["w"]; !ok {
		t.Fatalf("handle h1 missing property 'w': %v", props)
	} else if i, _ := v.Int64(); i != 7 {
		t.Fatalf("handle h1 'w' = %v, want 7", v)
	}
}

// TestEdgeHandles_EmptyGraphOmitsComponent confirms a graph with no
// per-handle metadata produces no component (emitted=false), the
// absent-component backward-compat contract.
func TestEdgeHandles_EmptyGraphOmitsComponent(t *testing.T) {
	t.Parallel()
	g := newHandleGraph(t)
	// Plain handle-less edge: no per-handle metadata.
	if err := g.AddNode("x"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddEdge("x", "x", 1); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _, emitted, err := WriteEdgeHandles(&buf, g)
	if err != nil {
		t.Fatalf("WriteEdgeHandles: %v", err)
	}
	if emitted {
		t.Fatalf("emitted=true for a graph with no per-handle metadata (%d bytes)", buf.Len())
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes for an empty component, want 0", buf.Len())
	}
}

// TestEdgeHandles_BadMagic confirms ReadEdgeHandles rejects a corrupt header.
func TestEdgeHandles_BadMagic(t *testing.T) {
	t.Parallel()
	_, err := ReadEdgeHandles(bytes.NewReader([]byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 0, 0, 0}))
	if err == nil {
		t.Fatal("ReadEdgeHandles accepted a bad magic")
	}
}

// TestCSR_HandleColumn_RoundTrip confirms the optional CSR handle column is
// written and read back aligned slot-for-slot with the edges.
func TestCSR_HandleColumn_RoundTrip(t *testing.T) {
	t.Parallel()
	g := newHandleGraph(t)
	if err := g.AddNode("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatal(err)
	}
	h1, _ := g.AddEdgeH("a", "b", 1)
	h2, _ := g.AddEdgeH("a", "b", 1)
	cs := csr.BuildFromAdjList(g.AdjList())

	var buf bytes.Buffer
	if _, _, err := WriteCSR(&buf, cs); err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	if rb.Handles == nil {
		t.Fatal("ReadCSR returned nil Handles for a handle-bearing CSR")
	}
	if len(rb.Handles) != len(rb.Edges) {
		t.Fatalf("Handles len %d != Edges len %d", len(rb.Handles), len(rb.Edges))
	}
	gotHandles := append([]uint64(nil), rb.Handles...)
	sort.Slice(gotHandles, func(i, j int) bool { return gotHandles[i] < gotHandles[j] })
	want := []uint64{h1, h2}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if gotHandles[0] != want[0] || gotHandles[1] != want[1] {
		t.Fatalf("CSR handles = %v, want %v", gotHandles, want)
	}
}

// TestCSR_NoHandleColumn_ByteCompatible confirms a handle-less CSR writes no
// trailing handle block — the byte layout matches the pre-Stage-2 format, so
// the v1 golden and cross-process byte-equality fixtures are unaffected.
func TestCSR_NoHandleColumn_ByteCompatible(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatal(err)
	}
	cs := csr.BuildFromAdjList(a)
	if cs.HandlesSlice() != nil {
		t.Fatal("handle-less graph produced a CSR handle column")
	}
	var buf bytes.Buffer
	size, _, err := WriteCSR(&buf, cs)
	if err != nil {
		t.Fatalf("WriteCSR: %v", err)
	}
	// Header (18) + 1 vertex offset slot (8 per vertex; one src 'a') + edges +
	// weights. The exact value is not the point; the point is that the
	// reported size equals the buffer length AND no trailing handle flag byte
	// was written (the reader returns nil handles).
	if int64(buf.Len()) != size {
		t.Fatalf("WriteCSR size %d != buffer len %d", size, buf.Len())
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadCSR: %v", err)
	}
	if rb.Handles != nil {
		t.Fatalf("ReadCSR returned non-nil Handles for a handle-less CSR: %v", rb.Handles)
	}
}

// hasOnly reports whether got contains exactly the single expected element.
func hasOnly(got []string, want string) bool {
	return len(got) == 1 && got[0] == want
}

// csrReadbackFrom builds a CSRReadback from a live CSR by round-tripping it
// through WriteCSR/ReadCSR, so apply tests exercise the real on-disk decode.
func csrReadbackFrom[W any](cs *csr.CSR[W]) CSRReadback {
	var buf bytes.Buffer
	if _, _, err := WriteCSR(&buf, cs); err != nil {
		panic(err)
	}
	rb, err := ReadCSR(bytes.NewReader(buf.Bytes()))
	if err != nil {
		panic(err)
	}
	return rb
}

// snapshotApplyMapperFrom restores fresh's mapper from src's interned keys via
// the string mapper readback path, mirroring the recovery wiring.
func snapshotApplyMapperFrom(fresh, src *lpg.Graph[string, float64]) error {
	var pairs []MapperPair
	src.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		pairs = append(pairs, MapperPair{ID: id, Key: key})
		return true
	})
	// Sort by ID so LoadFrom sees a dense, ascending sequence per shard.
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].ID < pairs[j].ID })
	return ApplyMapperToGraph(fresh, MapperReadback{Pairs: pairs})
}
