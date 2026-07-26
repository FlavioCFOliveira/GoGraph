package server

// entity_struct_testhelpers_test.go — shared decoders for the Bolt entity STRUCTURES
// (#2189).
//
// Before #2189 a node, relationship or path went on the wire as a PackStream map, and
// tests asserted against map keys. They now assert against structure fields, in the
// order the Bolt protocol fixes and the official driver's hydrator asserts. These
// helpers take rapid.TB, like the asStruct they build on, so the same decoder serves an
// ordinary test and a rapid property test. They do the unwrapping once — mirroring the driver's own field order — so a test
// reads the entity by name rather than by index, and a future field-order change fails
// in one place instead of seventeen.

import (
	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// decodedNode is a Bolt 'N' structure unwrapped for assertions.
type decodedNode struct {
	ID        int64
	Labels    []packstream.Value
	Props     map[string]packstream.Value
	ElementID string // empty when the structure carried no element_id (Bolt < 5)
}

// decodedRel is a Bolt 'R' structure unwrapped for assertions.
type decodedRel struct {
	ID, StartID, EndID          int64
	Type                        string
	Props                       map[string]packstream.Value
	ElementID, StartEID, EndEID string
}

// decodedPath is a Bolt 'P' structure unwrapped for assertions.
type decodedPath struct {
	Nodes   []packstream.Value
	Rels    []packstream.Value
	Indices []packstream.Value
}

// decodeNode unwraps a Bolt Node structure. It accepts both the 3-field (Bolt < 5) and
// 4-field (Bolt 5+) forms and rejects any other arity, because the driver asserts the
// exact count.
func decodeNode(t rapid.TB, v packstream.Value) decodedNode {
	t.Helper()
	f := asStruct(t, v, tagNode)
	if len(f) != 3 && len(f) != 4 {
		t.Fatalf("node: %d fields, want 3 (Bolt <5) or 4 (Bolt 5+)", len(f))
	}
	n := decodedNode{}
	n.ID, _ = f[0].(int64)
	n.Labels, _ = f[1].([]packstream.Value)
	n.Props, _ = f[2].(map[string]packstream.Value)
	if len(f) == 4 {
		n.ElementID, _ = f[3].(string)
	}
	return n
}

// decodeRel unwraps a Bolt Relationship structure, accepting the 5-field (Bolt < 5) and
// 8-field (Bolt 5+) forms.
func decodeRel(t rapid.TB, v packstream.Value) decodedRel {
	t.Helper()
	f := asStruct(t, v, tagRelationship)
	if len(f) != 5 && len(f) != 8 {
		t.Fatalf("relationship: %d fields, want 5 (Bolt <5) or 8 (Bolt 5+)", len(f))
	}
	r := decodedRel{}
	r.ID, _ = f[0].(int64)
	r.StartID, _ = f[1].(int64)
	r.EndID, _ = f[2].(int64)
	r.Type, _ = f[3].(string)
	r.Props, _ = f[4].(map[string]packstream.Value)
	if len(f) == 8 {
		r.ElementID, _ = f[5].(string)
		r.StartEID, _ = f[6].(string)
		r.EndEID, _ = f[7].(string)
	}
	return r
}

// decodeUnboundRel unwraps a Bolt UnboundRelationship structure — the form a Path
// carries — accepting the 3-field (Bolt < 5) and 4-field (Bolt 5+) forms.
func decodeUnboundRel(t rapid.TB, v packstream.Value) (id int64, typ string, props map[string]packstream.Value, elementID string) {
	t.Helper()
	f := asStruct(t, v, tagUnboundRelationship)
	if len(f) != 3 && len(f) != 4 {
		t.Fatalf("unbound relationship: %d fields, want 3 (Bolt <5) or 4 (Bolt 5+)", len(f))
	}
	id, _ = f[0].(int64)
	typ, _ = f[1].(string)
	props, _ = f[2].(map[string]packstream.Value)
	if len(f) == 4 {
		elementID, _ = f[3].(string)
	}
	return id, typ, props, elementID
}

// decodePath unwraps a Bolt Path structure. The driver requires an EVEN index count
// (one (relationship, node) pair per hop), so that is checked here.
func decodePath(t rapid.TB, v packstream.Value) decodedPath {
	t.Helper()
	f := asStruct(t, v, tagPath)
	if len(f) != 3 {
		t.Fatalf("path: %d fields, want 3 [nodes, relationships, indices]", len(f))
	}
	p := decodedPath{}
	p.Nodes, _ = f[0].([]packstream.Value)
	p.Rels, _ = f[1].([]packstream.Value)
	p.Indices, _ = f[2].([]packstream.Value)
	if len(p.Indices)%2 != 0 {
		t.Fatalf("path: %d indices, want an even count (the driver rejects an odd one)", len(p.Indices))
	}
	return p
}
