package sim

// wire_list_encoding_test.go — end-to-end wire encoding of list-valued columns
// (rmp #2513).
//
// Every list-producing Cypher construct reaches a driver through the RECORD
// encoder. Before #2513 that encoder had no expr.ListValue arm, so a list column
// crossed the wire as a packstream String: `RETURN [1,2,3]` arrived as
// "[1, 2, 3]", collect()/labels()/keys() arrived as text, and nodes(p) arrived
// as text with all node structure destroyed.
//
// These tests drive the REAL Bolt wire through [WireClient] — the same encoder,
// framing and decoder a driver uses — and assert the decoded Go type, because
// the type IS the wire encoding. The encoder-level matrix lives in
// bolt/server/list_value_test.go; this file proves the fix survives the whole
// path: engine → RECORD encoder → chunked framing → PackStream decoder.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// wireListClient dials a fresh simulator server and returns a connected client.
func wireListClient(t *testing.T) *WireClient {
	t.Helper()
	srv := newWireRoundTripServer(t)
	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c
}

// wireOneValue runs query and returns its single column of its single row.
func wireOneValue(t *testing.T, c *WireClient, query string, params map[string]any) any {
	t.Helper()
	recs, err := wireQuery(c, query, params)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		t.Fatalf("%s: expected one column in one row, got %d row(s)", query, len(recs))
	}
	return recs[0].Data[0]
}

// wireList asserts a decoded column is a PackStream List and returns it. A
// string here is the #2513 defect: the text may look right while the wire type
// is wrong, which is precisely what breaks every driver.
func wireList(t *testing.T, got any, what string) []any {
	t.Helper()
	if s, isString := got.(string); isString {
		t.Fatalf("%s: arrived as a packstream String %q, want a packstream List (#2513)", what, s)
	}
	l, ok := got.([]any)
	if !ok {
		t.Fatalf("%s: arrived as %T %#v, want a packstream List", what, got, got)
	}
	return l
}

// TestWireList_LiteralsAndNesting covers list literals, the empty list, nesting,
// a list inside a map, and a bound list parameter echoed back — the shapes that
// reach a driver from a plain RETURN.
func TestWireList_LiteralsAndNesting(t *testing.T) {
	t.Parallel()
	c := wireListClient(t)

	t.Run("scalar literal", func(t *testing.T) {
		l := wireList(t, wireOneValue(t, c, "RETURN [1, 2, 3] AS l", nil), "list literal")
		if len(l) != 3 || l[0] != int64(1) || l[2] != int64(3) {
			t.Fatalf("got %#v, want [1 2 3]", l)
		}
	})

	t.Run("mixed literal", func(t *testing.T) {
		l := wireList(t, wireOneValue(t, c, `RETURN ['a', 1, 2.5, true, null] AS l`, nil), "mixed literal")
		want := []any{"a", int64(1), 2.5, true, nil}
		if len(l) != len(want) {
			t.Fatalf("arity: got %d, want %d", len(l), len(want))
		}
		for i := range want {
			if l[i] != want[i] {
				t.Errorf("element %d: got %T %#v, want %T %#v", i, l[i], l[i], want[i], want[i])
			}
		}
	})

	t.Run("empty literal", func(t *testing.T) {
		if l := wireList(t, wireOneValue(t, c, "RETURN [] AS l", nil), "empty literal"); len(l) != 0 {
			t.Fatalf("got %#v, want an empty List", l)
		}
	})

	t.Run("nested literal", func(t *testing.T) {
		l := wireList(t, wireOneValue(t, c, "RETURN [[1, 2], [], [[3]]] AS l", nil), "nested literal")
		if len(l) != 3 {
			t.Fatalf("outer arity: got %d, want 3", len(l))
		}
		if inner := wireList(t, l[0], "inner list"); len(inner) != 2 || inner[1] != int64(2) {
			t.Errorf("inner list: got %#v, want [1 2]", inner)
		}
		if empty := wireList(t, l[1], "nested empty list"); len(empty) != 0 {
			t.Errorf("nested empty: got %#v", empty)
		}
		deep := wireList(t, wireList(t, l[2], "doubly nested")[0], "doubly nested inner")
		if len(deep) != 1 || deep[0] != int64(3) {
			t.Errorf("doubly nested: got %#v, want [3]", deep)
		}
	})

	t.Run("list inside map", func(t *testing.T) {
		got := wireOneValue(t, c, "RETURN {xs: [7, 8]} AS m", nil)
		m, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("map column: got %T %#v, want a packstream Map", got, got)
		}
		if xs := wireList(t, m["xs"], "list inside map"); len(xs) != 2 || xs[0] != int64(7) {
			t.Fatalf("got %#v, want [7 8]", xs)
		}
	})

	t.Run("bound parameter echo", func(t *testing.T) {
		got := wireOneValue(t, c, "RETURN $l AS l", map[string]any{"l": []any{int64(1), "two", nil}})
		l := wireList(t, got, "list parameter echo")
		if len(l) != 3 || l[0] != int64(1) || l[1] != "two" || l[2] != nil {
			t.Fatalf("got %#v, want [1 two <nil>]", l)
		}
	})
}

// TestWireList_FunctionsAndProperties covers the list-producing functions a real
// workload uses — collect(), labels(), keys() — plus a list-valued property
// stored and read back, all over the wire.
func TestWireList_FunctionsAndProperties(t *testing.T) {
	t.Parallel()
	c := wireListClient(t)

	if _, err := wireQuery(c,
		"CREATE (:WireList:Tagged {id: 1, xs: [10, 20, 30], name: 'a'})", nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := wireQuery(c, "CREATE (:WireList {id: 2, name: 'b'})", nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	t.Run("collect", func(t *testing.T) {
		l := wireList(t, wireOneValue(t, c,
			"MATCH (n:WireList) RETURN collect(n.id) AS ids", nil), "collect()")
		if len(l) != 2 {
			t.Fatalf("collect(): got %#v, want two ids", l)
		}
		for i, e := range l {
			if _, ok := e.(int64); !ok {
				t.Errorf("collect() element %d: got %T, want int64", i, e)
			}
		}
	})

	t.Run("labels", func(t *testing.T) {
		l := wireList(t, wireOneValue(t, c,
			"MATCH (n:WireList {id: 1}) RETURN labels(n) AS ls", nil), "labels()")
		if len(l) != 2 {
			t.Fatalf("labels(): got %#v, want two labels", l)
		}
		for i, e := range l {
			if _, ok := e.(string); !ok {
				t.Errorf("labels() element %d: got %T, want string", i, e)
			}
		}
	})

	t.Run("keys", func(t *testing.T) {
		l := wireList(t, wireOneValue(t, c,
			"MATCH (n:WireList {id: 2}) RETURN keys(n) AS ks", nil), "keys()")
		if len(l) != 2 {
			t.Fatalf("keys(): got %#v, want two keys", l)
		}
	})

	t.Run("list-valued property", func(t *testing.T) {
		l := wireList(t, wireOneValue(t, c,
			"MATCH (n:WireList {id: 1}) RETURN n.xs AS xs", nil), "list property")
		want := []any{int64(10), int64(20), int64(30)}
		if len(l) != len(want) {
			t.Fatalf("arity: got %d, want %d", len(l), len(want))
		}
		for i := range want {
			if l[i] != want[i] {
				t.Errorf("element %d: got %T %#v, want %#v", i, l[i], l[i], want[i])
			}
		}
	})
}

// TestWireList_OfEntities is the case that loses the most: nodes(p) and
// relationships(p) must arrive as Lists of Bolt Node/Relationship STRUCTURES, so
// a driver can materialise them. Stringified, every field of every entity is
// destroyed — this is the strongest signal the encoder is structural.
func TestWireList_OfEntities(t *testing.T) {
	t.Parallel()
	c := wireListClient(t)

	if _, err := wireQuery(c,
		"CREATE (a:WireEnt {id: 1})-[:LINKS {w: 5}]->(b:WireEnt {id: 2})", nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	recs, err := wireQuery(c,
		"MATCH p = (:WireEnt {id: 1})-[:LINKS]->(:WireEnt {id: 2}) "+
			"RETURN nodes(p) AS ns, relationships(p) AS rs, collect(p) AS ps", nil)
	if err != nil {
		t.Fatalf("path query: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Data) != 3 {
		t.Fatalf("expected three columns in one row, got %d row(s)", len(recs))
	}

	ns := wireList(t, recs[0].Data[0], "nodes(p)")
	if len(ns) != 2 {
		t.Fatalf("nodes(p): got %d nodes, want 2", len(ns))
	}
	for i, n := range ns {
		s, ok := n.(packstream.Struct)
		if !ok {
			t.Fatalf("nodes(p)[%d]: got %T %#v, want a Node Struct", i, n, n)
		}
		if s.Tag != 0x4E {
			t.Errorf("nodes(p)[%d]: tag 0x%02X, want 0x4E ('N')", i, s.Tag)
		}
		if len(s.Fields) != 4 { // Bolt 5: id, labels, properties, element_id
			t.Errorf("nodes(p)[%d]: %d fields, want 4", i, len(s.Fields))
		}
	}

	rs := wireList(t, recs[0].Data[1], "relationships(p)")
	if len(rs) != 1 {
		t.Fatalf("relationships(p): got %d, want 1", len(rs))
	}
	rel, ok := rs[0].(packstream.Struct)
	if !ok {
		t.Fatalf("relationships(p)[0]: got %T %#v, want a Relationship Struct", rs[0], rs[0])
	}
	if rel.Tag != 0x52 {
		t.Errorf("relationships(p)[0]: tag 0x%02X, want 0x52 ('R')", rel.Tag)
	}
	if rel.Fields[3] != "LINKS" {
		t.Errorf("relationships(p)[0]: type field %#v, want \"LINKS\"", rel.Fields[3])
	}

	ps := wireList(t, recs[0].Data[2], "collect(p)")
	if len(ps) != 1 {
		t.Fatalf("collect(p): got %d paths, want 1", len(ps))
	}
	path, ok := ps[0].(packstream.Struct)
	if !ok {
		t.Fatalf("collect(p)[0]: got %T %#v, want a Path Struct", ps[0], ps[0])
	}
	if path.Tag != 0x50 {
		t.Errorf("collect(p)[0]: tag 0x%02X, want 0x50 ('P')", path.Tag)
	}
	if len(path.Fields) != 3 {
		t.Fatalf("collect(p)[0]: %d fields, want 3", len(path.Fields))
	}
	if pathNodes := wireList(t, path.Fields[0], "path nodes"); len(pathNodes) != 2 {
		t.Errorf("path nodes: got %d, want 2", len(pathNodes))
	}
}

// TestWireList_OfTemporals proves the negotiated version is threaded into list
// ELEMENTS: a date inside a list must arrive as its temporal Struct, not as
// text. The list markers themselves are version-independent; the elements are
// not, which is why boltMajor has to be propagated into the arm.
func TestWireList_OfTemporals(t *testing.T) {
	t.Parallel()
	c := wireListClient(t)

	l := wireList(t, wireOneValue(t, c,
		"RETURN [date('2024-01-15'), duration({days: 2})] AS l", nil), "temporal list")
	if len(l) != 2 {
		t.Fatalf("arity: got %d, want 2", len(l))
	}
	for i, wantTag := range []byte{0x44, 0x45} {
		s, ok := l[i].(packstream.Struct)
		if !ok {
			t.Fatalf("element %d: got %T %#v, want a temporal Struct", i, l[i], l[i])
		}
		if s.Tag != wantTag {
			t.Errorf("element %d: tag 0x%02X, want 0x%02X", i, s.Tag, wantTag)
		}
	}
}
