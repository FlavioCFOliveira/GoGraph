package bulkimport

// bulkimport_test.go — rmp #2178.
//
// The decisive test here is the DIFFERENTIAL one: a graph built through the
// importer must be element-for-element identical to the same data inserted
// through the ordinary Go API. That is what licenses the importer to bypass the
// normal write path — it is a faster route to the same state, not a second
// definition of what the state is.
//
// Layer: short.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// fingerprint renders a graph's whole observable content as a deterministic
// string: every node with its sorted labels and sorted properties, and every
// out-edge with its weight, sorted type list and sorted properties. Two
// equivalent graphs produce byte-identical output, so a diff of two
// fingerprints names the divergent record.
//
// Edges are addressed by HANDLE, not by (src, dst): in a multigraph a pair may
// carry several edges with different types and properties, and a pair-addressed
// read would collapse them and hide exactly the case the handle API exists for.
func fingerprint(t *testing.T, g *lpg.Graph[string, int64]) string {
	t.Helper()
	var keys []string
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, k string) bool {
		keys = append(keys, k)
		return true
	})
	sort.Strings(keys)

	var sb []byte
	app := func(format string, a ...any) { sb = append(sb, fmt.Sprintf(format, a...)...) }

	for _, k := range keys {
		app("N %s\n", k)
		labels := append([]string(nil), g.NodeLabels(k)...)
		sort.Strings(labels)
		for _, l := range labels {
			app(" L %s\n", l)
		}
		props := g.NodeProperties(k)
		pk := make([]string, 0, len(props))
		for p := range props {
			pk = append(pk, p)
		}
		sort.Strings(pk)
		for _, p := range pk {
			app(" P %s=%s\n", p, renderValue(props[p]))
		}

		// Out-edges. LoadEntryH returns the aligned (neighbour, weight, handle)
		// slot arrays, which is the only way to see PARALLEL edges separately: a
		// pair-addressed read collapses them and would hide the very case the
		// handle API exists for.
		srcID, ok := g.AdjList().Mapper().Lookup(k)
		if !ok {
			t.Fatalf("node %q is not interned", k)
		}
		nbrs, weights, handles := g.AdjList().LoadEntryH(srcID)
		type outEdge struct {
			dst    string
			weight int64
			handle uint64
		}
		var outs []outEdge
		for i, dstID := range nbrs {
			dstKey, dok := g.AdjList().Mapper().Resolve(dstID)
			if !dok {
				continue
			}
			var h uint64
			if i < len(handles) {
				h = handles[i]
			}
			var w int64
			if i < len(weights) {
				w = weights[i]
			}
			outs = append(outs, outEdge{dst: dstKey, weight: w, handle: h})
		}
		// Sort by (destination, type list, property rendering) rather than by the
		// handle NUMBER: handles are an allocation order, not content, and the two
		// build paths need not mint the same numbers. Sorting on content makes the
		// comparison independent of that while still distinguishing parallel edges.
		render := func(e outEdge) string {
			el := append([]string(nil), g.EdgeLabelsByHandle(k, e.dst, e.handle)...)
			sort.Strings(el)
			ep := g.EdgePropertiesByHandle(k, e.dst, e.handle)
			ek := make([]string, 0, len(ep))
			for p := range ep {
				ek = append(ek, p)
			}
			sort.Strings(ek)
			out := fmt.Sprintf(" E -> %s w=%d\n", e.dst, e.weight)
			for _, l := range el {
				out += "  EL " + l + "\n"
			}
			for _, p := range ek {
				out += "  EP " + p + "=" + renderValue(ep[p]) + "\n"
			}
			return out
		}
		rendered := make([]string, 0, len(outs))
		for _, e := range outs {
			rendered = append(rendered, render(e))
		}
		sort.Strings(rendered)
		for _, r := range rendered {
			app("%s", r)
		}
	}
	return string(sb)
}

// renderValue renders a property kind-tagged, so Int64(7) and Float64(7) are
// distinguishable in the fingerprint.
func renderValue(v lpg.PropertyValue) string {
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		return "s:" + s
	case lpg.PropInt64:
		i, _ := v.Int64()
		return "i:" + strconv.FormatInt(i, 10)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		return "f:" + strconv.FormatFloat(f, 'g', -1, 64)
	case lpg.PropBool:
		b, _ := v.Bool()
		return "b:" + strconv.FormatBool(b)
	default:
		return "?:" + strconv.Itoa(int(v.Kind()))
	}
}

// fixture is the shared shape both build paths consume: nodes with two labels
// and three properties of different kinds, and edges including PARALLEL ones
// between the same pair carrying different types and properties.
type fixture struct {
	nodes []Node
	edges []Edge[int64]
}

func newFixture(nNodes, edgesPerNode int) fixture {
	f := fixture{}
	key := func(i int) string { return "n" + strconv.Itoa(i) }
	for i := 0; i < nNodes; i++ {
		f.nodes = append(f.nodes, Node{
			Key:    key(i),
			Labels: []string{"Person", "L" + strconv.Itoa(i%5)},
			Properties: map[string]lpg.PropertyValue{
				"id":    lpg.Int64Value(int64(i)),
				"name":  lpg.StringValue("name-" + key(i)),
				"ratio": lpg.Float64Value(float64(i) + 0.5),
			},
		})
	}
	for i := 0; i < nNodes; i++ {
		for e := 1; e <= edgesPerNode; e++ {
			dst := key((i*e + e*e) % nNodes)
			f.edges = append(f.edges, Edge[int64]{
				Src: key(i), Dst: dst, Weight: int64(i*100 + e),
				Type: "T" + strconv.Itoa(e%3),
				Properties: map[string]lpg.PropertyValue{
					"since": lpg.Int64Value(int64(e)),
					"tag":   lpg.StringValue("e" + strconv.Itoa(e)),
				},
			})
		}
		// A deliberate PARALLEL edge to the same destination with a DIFFERENT type
		// and property, which a pair-addressed write would clobber.
		dst := key((i + 1) % nNodes)
		f.edges = append(f.edges,
			Edge[int64]{Src: key(i), Dst: dst, Weight: 1, Type: "PAR_A",
				Properties: map[string]lpg.PropertyValue{"which": lpg.StringValue("a")}},
			Edge[int64]{Src: key(i), Dst: dst, Weight: 1, Type: "PAR_B",
				Properties: map[string]lpg.PropertyValue{"which": lpg.StringValue("b")}},
		)
	}
	return f
}

// buildViaImporter builds the fixture through the package under test.
func buildViaImporter(t *testing.T, f fixture) *lpg.Graph[string, int64] {
	t.Helper()
	b := New[int64](Options{Directed: true, Multigraph: true, ExpectNodes: len(f.nodes)})
	if err := b.AddNodes(f.nodes); err != nil {
		t.Fatalf("AddNodes: %v", err)
	}
	if err := b.AddEdges(f.edges); err != nil {
		t.Fatalf("AddEdges: %v", err)
	}
	st, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if st.Nodes != len(f.nodes) {
		t.Fatalf("Stats.Nodes = %d, want %d", st.Nodes, len(f.nodes))
	}
	if st.Edges != len(f.edges) {
		t.Fatalf("Stats.Edges = %d, want %d", st.Edges, len(f.edges))
	}
	g := b.Graph()
	if g == nil {
		t.Fatal("Graph() returned nil after Finish")
	}
	return g
}

// buildViaGoAPI builds the SAME fixture through the ordinary public Go API, with
// no commit window and no importer involved — the reference the differential is
// taken against.
func buildViaGoAPI(t *testing.T, f fixture) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range f.nodes {
		if err := g.AddNode(n.Key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		for _, l := range n.Labels {
			if err := g.SetNodeLabel(n.Key, l); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
		for k, v := range n.Properties {
			if err := g.SetNodeProperty(n.Key, k, v); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
		}
	}
	for _, e := range f.edges {
		h, err := g.AddEdgeH(e.Src, e.Dst, e.Weight)
		if err != nil {
			t.Fatalf("AddEdgeH: %v", err)
		}
		if e.Type != "" {
			g.SetEdgeLabelByHandle(e.Src, e.Dst, h, e.Type)
		}
		for k, v := range e.Properties {
			if perr := g.SetEdgePropertyByHandle(e.Src, e.Dst, h, k, v); perr != nil {
				t.Fatalf("SetEdgePropertyByHandle: %v", perr)
			}
		}
	}
	return g
}

// TestDifferential_ImporterMatchesGoAPI is the acceptance test: identical content
// from both paths, compared element by element.
func TestDifferential_ImporterMatchesGoAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(256, 4)

	want := fingerprint(t, buildViaGoAPI(t, f))
	got := fingerprint(t, buildViaImporter(t, f))

	if want == "" {
		t.Fatal("the reference fingerprint is empty; the fixture built nothing")
	}
	if got != want {
		t.Fatalf("the importer produced different content from the Go API.\n%s",
			firstDiff(want, got))
	}
}

// TestImporter_DuplicateNodeKeyMerges pins the documented behaviour a CSV split
// across files needs: a repeated key adds to the existing node rather than
// failing or duplicating it.
func TestImporter_DuplicateNodeKeyMerges(t *testing.T) {
	t.Parallel()
	b := New[int64](Options{Directed: true, Multigraph: true})
	if err := b.AddNode(Node{Key: "a", Labels: []string{"L1"},
		Properties: map[string]lpg.PropertyValue{"x": lpg.Int64Value(1)}}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddNode(Node{Key: "a", Labels: []string{"L2"},
		Properties: map[string]lpg.PropertyValue{"y": lpg.Int64Value(2)}}); err != nil {
		t.Fatal(err)
	}
	st, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes != 1 {
		t.Fatalf("Stats.Nodes = %d, want 1 (the key repeats)", st.Nodes)
	}
	if st.NodeRecords != 2 {
		t.Fatalf("Stats.NodeRecords = %d, want 2", st.NodeRecords)
	}
	g := b.Graph()
	labels := append([]string(nil), g.NodeLabels("a")...)
	sort.Strings(labels)
	if len(labels) != 2 || labels[0] != "L1" || labels[1] != "L2" {
		t.Fatalf("labels = %v, want both L1 and L2 merged onto one node", labels)
	}
	for _, k := range []string{"x", "y"} {
		if _, ok := g.GetNodeProperty("a", k); !ok {
			t.Fatalf("property %q lost by the merge", k)
		}
	}
}

// TestImporter_UnknownEndpointIsAnError pins the deliberate choice NOT to create
// endpoints implicitly: a mistyped key in an edge file must fail loudly rather
// than silently produce a labelless, propertyless node.
func TestImporter_UnknownEndpointIsAnError(t *testing.T) {
	t.Parallel()
	b := New[int64](Options{Directed: true, Multigraph: true})
	if err := b.AddNode(Node{Key: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddEdge(Edge[int64]{Src: "a", Dst: "typo"}); err == nil {
		t.Fatal("an edge to an unknown target was accepted; a mistyped key must not create a node")
	}
	if err := b.AddEdge(Edge[int64]{Src: "typo", Dst: "a"}); err == nil {
		t.Fatal("an edge from an unknown source was accepted")
	}
	st, _ := b.Finish()
	if st.Edges != 0 {
		t.Fatalf("Stats.Edges = %d after two rejected edges, want 0", st.Edges)
	}
}

// TestImporter_GraphUnavailableBeforeFinish pins the window contract: the graph
// must not escape while the adjacency commit window is still open, because the
// touched shards' builders are still mutable in place.
func TestImporter_GraphUnavailableBeforeFinish(t *testing.T) {
	t.Parallel()
	b := New[int64](Options{Directed: true})
	if err := b.AddNode(Node{Key: "a"}); err != nil {
		t.Fatal(err)
	}
	if g := b.Graph(); g != nil {
		t.Fatal("Graph() returned a graph before Finish; the commit window is still open")
	}
	if _, err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	if g := b.Graph(); g == nil {
		t.Fatal("Graph() returned nil after Finish")
	}
}

// TestImporter_IngestAfterFinishIsRefused pins that a finished builder cannot be
// written to, which would mutate a frozen graph.
func TestImporter_IngestAfterFinishIsRefused(t *testing.T) {
	t.Parallel()
	b := New[int64](Options{Directed: true})
	if _, err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := b.AddNode(Node{Key: "a"}); !errors.Is(err, ErrFinished) {
		t.Fatalf("AddNode after Finish = %v, want ErrFinished", err)
	}
	if err := b.AddEdge(Edge[int64]{Src: "a", Dst: "a"}); !errors.Is(err, ErrFinished) {
		t.Fatalf("AddEdge after Finish = %v, want ErrFinished", err)
	}
	if _, err := b.Finish(); !errors.Is(err, ErrFinished) {
		t.Fatalf("second Finish = %v, want ErrFinished", err)
	}
}

// TestImporter_EmptyNodeKeyIsAnError guards the one input that cannot be
// meaningful.
func TestImporter_EmptyNodeKeyIsAnError(t *testing.T) {
	t.Parallel()
	b := New[int64](Options{Directed: true})
	if err := b.AddNode(Node{Key: ""}); err == nil {
		t.Fatal("an empty node key was accepted")
	}
}

// firstDiff returns a short excerpt around the first differing line of two
// fingerprints, so a failure names the divergent record rather than dumping two
// large strings.
func firstDiff(want, got string) string {
	wl, gl := splitLines(want), splitLines(got)
	n := len(wl)
	if len(gl) < n {
		n = len(gl)
	}
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			return fmt.Sprintf("first divergence at line %d\n  want: %s\n  got:  %s", i+1, wl[i], gl[i])
		}
	}
	if len(wl) != len(gl) {
		return fmt.Sprintf("line counts differ: want %d, got %d", len(wl), len(gl))
	}
	return "(no line-level difference found)"
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
