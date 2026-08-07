package snapshot

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// determinism_order_test.go — the durable components must be a deterministic
// function of the graph's CONTENT, never of its mutation history or of Go's
// randomised map iteration order (rmp #2141).
//
// Two distinct defects are pinned here, both found while making the CSR build
// order its neighbour runs:
//
//  1. collectEdgePropertyRecords coalesced an edge's properties into a map and
//     then ranged over it, so an edge carrying two or more properties emitted
//     its records in a RANDOM order and two snapshots of the identical graph
//     disagreed byte-for-byte. This was pre-existing and independent of the CSR
//     change; no test covered it because the byte-stability fixtures carried no
//     multi-property edge.
//
//  2. The collectors walked the adjacency directly, so the emitted record order
//     followed edge INSERTION order. Since csr.BuildFromAdjList now orders each
//     source's run by destination, ApplyCSRToGraph replays edges in a different
//     order than they were originally inserted, so a recovered graph's insertion
//     order legitimately differs and the components drifted.

// buildMultiPropGraph returns a graph whose edges carry several properties each
// and whose neighbours are inserted in DESCENDING destination order, so both
// defects above are in play.
func buildMultiPropGraph(t *testing.T) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	srcs := []string{"s1", "s2", "s3"}
	dsts := []string{"d9", "d7", "d5", "d3", "d1"} // descending by name and by intern order
	for _, s := range srcs {
		for _, d := range dsts {
			if err := g.AddEdge(s, d, 1); err != nil {
				t.Fatal(err)
			}
			// Several properties per edge: this is what exposes the map-iteration
			// order defect. A single property could never reveal it.
			for _, kv := range []struct {
				k string
				v lpg.PropertyValue
			}{
				{"alpha", lpg.Int64Value(1)},
				{"beta", lpg.StringValue("two")},
				{"gamma", lpg.Float64Value(3.5)},
				{"delta", lpg.BoolValue(true)},
				{"epsilon", lpg.Int64Value(5)},
			} {
				if err := g.SetEdgeProperty(s, d, kv.k, kv.v); err != nil {
					t.Fatal(err)
				}
			}
			g.SetEdgeLabel(s, d, "E")
		}
	}
	return g
}

// TestWriteProperties_ByteStableAcrossRepeatedWrites fails on the pre-fix code:
// ranging over the coalesce map gave each multi-property edge a random record
// order, so repeated serialisations of ONE unchanged graph differed. Twenty
// attempts make a chance agreement effectively impossible for 5 keys.
func TestWriteProperties_ByteStableAcrossRepeatedWrites(t *testing.T) {
	t.Parallel()
	g := buildMultiPropGraph(t)

	var first []byte
	for i := 0; i < 20; i++ {
		var buf bytes.Buffer
		if _, _, err := WriteProperties(&buf, g, nil); err != nil {
			t.Fatalf("WriteProperties: %v", err)
		}
		if i == 0 {
			first = bytes.Clone(buf.Bytes())
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("properties.bin differed on write %d of the SAME graph "+
				"(%d vs %d bytes): the record order is not deterministic",
				i+1, len(first), buf.Len())
		}
	}
}

// TestWriteLabels_ByteStableAcrossRepeatedWrites is the labels-side companion.
func TestWriteLabels_ByteStableAcrossRepeatedWrites(t *testing.T) {
	t.Parallel()
	g := buildMultiPropGraph(t)

	var first []byte
	for i := 0; i < 20; i++ {
		var buf bytes.Buffer
		if _, _, err := WriteLabels(&buf, g, nil); err != nil {
			t.Fatalf("WriteLabels: %v", err)
		}
		if i == 0 {
			first = bytes.Clone(buf.Bytes())
			continue
		}
		if !bytes.Equal(first, buf.Bytes()) {
			t.Fatalf("labels.bin differed on write %d of the SAME graph", i+1)
		}
	}
}

// TestCollectors_IndependentOfInsertionOrder is the direct assertion for defect
// 2: two graphs with IDENTICAL content but opposite edge-insertion order must
// serialise to identical component bytes. This is the property that lets a
// recovered graph — whose edges are replayed in destination order rather than
// original insertion order — re-snapshot byte-for-byte.
//
// Node IDs are assigned in first-seen intern order, so both graphs intern the
// endpoints in the same order before any edge is added; only the ADJACENCY
// insertion order differs.
func TestCollectors_IndependentOfInsertionOrder(t *testing.T) {
	t.Parallel()
	dsts := []string{"d1", "d3", "d5", "d7", "d9"}

	build := func(descending bool) *lpg.Graph[string, int64] {
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		// Intern every endpoint up front, in the SAME order for both graphs, so
		// the mapper and therefore the NodeIDs are identical.
		if err := g.AddNode("s1"); err != nil {
			t.Fatal(err)
		}
		for _, d := range dsts {
			if err := g.AddNode(d); err != nil {
				t.Fatal(err)
			}
		}
		order := make([]string, len(dsts))
		copy(order, dsts)
		if descending {
			for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}
		}
		for _, d := range order {
			if err := g.AddEdge("s1", d, 1); err != nil {
				t.Fatal(err)
			}
			if err := g.SetEdgeProperty("s1", d, "k1", lpg.Int64Value(1)); err != nil {
				t.Fatal(err)
			}
			if err := g.SetEdgeProperty("s1", d, "k2", lpg.Int64Value(2)); err != nil {
				t.Fatal(err)
			}
			g.SetEdgeLabel("s1", d, "E")
		}
		return g
	}

	asc, desc := build(false), build(true)

	var ascProps, descProps, ascLabels, descLabels bytes.Buffer
	if _, _, err := WriteProperties(&ascProps, asc, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteProperties(&descProps, desc, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteLabels(&ascLabels, asc, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteLabels(&descLabels, desc, nil); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(ascProps.Bytes(), descProps.Bytes()) {
		t.Errorf("properties.bin depends on edge insertion order (%d vs %d bytes)",
			ascProps.Len(), descProps.Len())
	}
	if !bytes.Equal(ascLabels.Bytes(), descLabels.Bytes()) {
		t.Errorf("labels.bin depends on edge insertion order (%d vs %d bytes)",
			ascLabels.Len(), descLabels.Len())
	}
}
