package lpg

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestEdgePropTier_PerKindRoundTripPublic exercises the public Set/Get/EdgeProperties
// surface for every kind, asserting the columnar tier preserves value identity.
func TestEdgePropTier_PerKindRoundTripPublic(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.mustSet(t, "a", "b", "i", Int64Value(2020))
	g.mustSet(t, "a", "b", "f", Float64Value(1.5))
	g.mustSet(t, "a", "b", "flag", BoolValue(true))
	g.mustSet(t, "a", "b", "name", StringValue("edge"))
	g.mustSet(t, "a", "b", "since", StringValue("\x012020-01-15")) // tagged Date

	props := g.EdgeProperties("a", "b")
	if len(props) != 5 {
		t.Fatalf("EdgeProperties len = %d, want 5: %v", len(props), props)
	}
	if i, _ := props["i"].Int64(); i != 2020 {
		t.Fatalf("i = %d", i)
	}
	if f, _ := props["f"].Float64(); f != 1.5 {
		t.Fatalf("f = %v", f)
	}
	if b, _ := props["flag"].Bool(); !b {
		t.Fatalf("flag = false")
	}
	if s, _ := props["name"].String(); s != "edge" {
		t.Fatalf("name = %q", s)
	}
	if s, _ := props["since"].String(); s != "\x012020-01-15" {
		t.Fatalf("since = %q (date round-trip)", s)
	}
}

// TestEdgePropTier_LastWriteWinsAcrossKinds asserts that re-setting a key to a
// different kind overwrites the previous value (single value per (pair,key)),
// exactly as the old single-bag store did.
func TestEdgePropTier_LastWriteWinsAcrossKinds(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.mustSet(t, "a", "b", "k", Int64Value(7))
	g.mustSet(t, "a", "b", "k", StringValue("seven")) // different kind

	props := g.EdgeProperties("a", "b")
	if len(props) != 1 {
		t.Fatalf("expected 1 property, got %d: %v", len(props), props)
	}
	v, ok := g.GetEdgeProperty("a", "b", "k")
	if !ok {
		t.Fatalf("k missing")
	}
	if v.Kind() != PropString {
		t.Fatalf("k kind = %d, want PropString (last write wins)", v.Kind())
	}
	if s, _ := v.String(); s != "seven" {
		t.Fatalf("k = %q, want seven", s)
	}
}

// TestEdgePropTier_UndirectedBothDirections asserts that on an undirected graph a
// property set on (a,b) is observable from both endpoint orders, since the
// undirected edge is two adjacency slots (forward + mirror) and the value fans
// out to the dst-matching slot in each direction.
func TestEdgePropTier_UndirectedBothDirections(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: false})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// Set under the (a,b) order.
	g.mustSet(t, "a", "b", "since", Int64Value(2020))
	// Set under the (b,a) order — both directions independently carry the value.
	g.mustSet(t, "b", "a", "weight", Int64Value(5))

	ab := g.EdgeProperties("a", "b")
	ba := g.EdgeProperties("b", "a")
	if i, ok := g.GetEdgeProperty("a", "b", "since"); !ok {
		t.Fatalf("a->b since missing")
	} else if v, _ := i.Int64(); v != 2020 {
		t.Fatalf("a->b since = %d", v)
	}
	if i, ok := g.GetEdgeProperty("b", "a", "weight"); !ok {
		t.Fatalf("b->a weight missing")
	} else if v, _ := i.Int64(); v != 5 {
		t.Fatalf("b->a weight = %d", v)
	}
	// Each direction has exactly the property set on it.
	if _, ok := ab["since"]; !ok {
		t.Fatalf("a->b missing since: %v", ab)
	}
	if _, ok := ba["weight"]; !ok {
		t.Fatalf("b->a missing weight: %v", ba)
	}
}

// TestEdgePropTier_MultigraphCoalesce asserts the per-pair latest-wins coalesce
// across parallel edges: a property set on the pair is visible regardless of how
// many parallel edges connect them, and removing one parallel edge keeps the
// property while any edge survives.
func TestEdgePropTier_MultigraphCoalesce(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	// Two parallel a->b edges.
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge 1: %v", err)
	}
	if err := g.AddEdge("a", "b", 2); err != nil {
		t.Fatalf("AddEdge 2: %v", err)
	}
	g.mustSet(t, "a", "b", "since", Int64Value(2020))

	if v, ok := g.GetEdgeProperty("a", "b", "since"); !ok {
		t.Fatalf("since missing on multigraph pair")
	} else if i, _ := v.Int64(); i != 2020 {
		t.Fatalf("since = %d", i)
	}

	// Remove one parallel edge: the property survives on the remaining one.
	g.RemoveEdge("a", "b")
	if !g.AdjList().HasEdge("a", "b") {
		t.Fatalf("expected a parallel edge to survive")
	}
	if v, ok := g.GetEdgeProperty("a", "b", "since"); !ok {
		t.Fatalf("since lost after removing one parallel edge")
	} else if i, _ := v.Int64(); i != 2020 {
		t.Fatalf("since = %d after one removal", i)
	}

	// Remove the last edge: per-pair state is dropped.
	g.RemoveEdge("a", "b")
	if g.AdjList().HasEdge("a", "b") {
		t.Fatalf("expected no edge after second removal")
	}
	if _, ok := g.GetEdgeProperty("a", "b", "since"); ok {
		t.Fatalf("since survived last-edge removal (no hygiene)")
	}
	// Re-creating the edge starts clean (no resurrection).
	if err := g.AddEdge("a", "b", 9); err != nil {
		t.Fatalf("re-AddEdge: %v", err)
	}
	if _, ok := g.GetEdgeProperty("a", "b", "since"); ok {
		t.Fatalf("since resurrected after re-create")
	}
}

// TestEdgePropTier_DeletePreservesOtherKeys asserts deleting one key leaves the
// others intact (the edge↔property binding survives a delete).
func TestEdgePropTier_DeletePreservesOtherKeys(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.mustSet(t, "a", "b", "since", Int64Value(2020))
	g.mustSet(t, "a", "b", "weight", Float64Value(0.9))

	g.DelEdgeProperty("a", "b", "weight")
	if _, ok := g.GetEdgeProperty("a", "b", "weight"); ok {
		t.Fatalf("weight not deleted")
	}
	if v, ok := g.GetEdgeProperty("a", "b", "since"); !ok {
		t.Fatalf("since lost after deleting weight")
	} else if i, _ := v.Int64(); i != 2020 {
		t.Fatalf("since = %d", i)
	}
	props := g.EdgeProperties("a", "b")
	if len(props) != 1 {
		t.Fatalf("EdgeProperties len = %d, want 1", len(props))
	}
}

// TestEdgePropTier_BindingSurvivesCompaction is the public-surface analogue of
// the column compaction test: it builds a star of edges out of one source, sets
// a distinct property on each, removes edges in the middle, and asserts every
// surviving edge keeps its OWN property (positional binding preserved by the
// adjlist compaction driving CompactSlot). Run against an oracle of the
// surviving (src,dst)->value map.
func TestEdgePropTier_BindingSurvivesCompaction(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	dsts := []string{"b", "c", "d", "e", "f"}
	oracle := map[string]int64{}
	for i, d := range dsts {
		if err := g.AddEdge("a", d, 0); err != nil {
			t.Fatalf("AddEdge a->%s: %v", d, err)
		}
		val := int64(100 + i)
		g.mustSet(t, "a", d, "rank", Int64Value(val))
		oracle[d] = val
	}
	// Remove the middle edge a->d.
	g.RemoveEdge("a", "d")
	delete(oracle, "d")

	for d, want := range oracle {
		v, ok := g.GetEdgeProperty("a", d, "rank")
		if !ok {
			t.Fatalf("a->%s lost its rank after compaction", d)
		}
		if got, _ := v.Int64(); got != want {
			t.Fatalf("a->%s rank = %d, want %d (binding broke)", d, got, want)
		}
	}
	if _, ok := g.GetEdgeProperty("a", "d", "rank"); ok {
		t.Fatalf("removed a->d still has a property")
	}
}

// TestEdgePropTier_DensePathNoValidityBitmap asserts the dense-path guard
// (design risk #4): a simple graph with one property per edge keeps each column
// dense (length 1, validity bitmap nil) so the dense workload pays zero validity
// overhead. We reach into the columnar block to assert the bitmap is omitted.
func TestEdgePropTier_DensePathNoValidityBitmap(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.mustSet(t, "a", "b", "since", Int64Value(2020))

	srcID, _ := g.AdjList().Mapper().Lookup("a")
	block := asEdgePropCols(g.AdjList().LoadEntryAux(srcID))
	if block == nil {
		t.Fatalf("no aux block after set")
	}
	if len(block.cols) != 1 {
		t.Fatalf("expected 1 column, got %d", len(block.cols))
	}
	col := block.cols[0]
	if col.length != 1 {
		t.Fatalf("dense column length = %d, want 1", col.length)
	}
	if col.valid != nil {
		t.Fatalf("dense single-slot column unexpectedly allocated a validity bitmap")
	}
}

// TestEdgePropTier_FullScanPropertyBased drives the public surface with random
// add-edge / set / del / remove-edge operations against an oracle of the
// per-pair coalesced property map, asserting EdgeProperties matches after every
// step. This exercises the full lockstep write/read reconciliation (fan-out on
// write across parallel slots, coalesce on read) end-to-end.
func TestEdgePropTier_FullScanPropertyBased(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= 8; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			runPublicOracle(t, seed)
		})
	}
}

func runPublicOracle(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	g := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	nodes := []string{"a", "b", "c"}
	keys := []string{"k1", "k2"}
	// oracle[pair] = the coalesced per-pair property map; present iff >=1 edge.
	type pair struct{ s, d string }
	oracle := map[pair]map[string]int64{}
	edges := map[pair]int{} // parallel-edge count per pair

	val := func() int64 { return int64(rng.Intn(5)) }

	for step := 0; step < 300; step++ {
		s := nodes[rng.Intn(len(nodes))]
		d := nodes[rng.Intn(len(nodes))]
		p := pair{s, d}
		switch rng.Intn(4) {
		case 0: // add edge
			if err := g.AddEdge(s, d, 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			edges[p]++
		case 1: // set property (only meaningful when the edge exists)
			if edges[p] == 0 {
				continue
			}
			k := keys[rng.Intn(len(keys))]
			v := val()
			g.mustSet(t, s, d, k, Int64Value(v))
			if oracle[p] == nil {
				oracle[p] = map[string]int64{}
			}
			oracle[p][k] = v
		case 2: // del property
			if edges[p] == 0 {
				continue
			}
			k := keys[rng.Intn(len(keys))]
			g.DelEdgeProperty(s, d, k)
			if oracle[p] != nil {
				delete(oracle[p], k)
			}
		case 3: // remove one edge
			if edges[p] == 0 {
				continue
			}
			g.RemoveEdge(s, d)
			edges[p]--
			if edges[p] == 0 {
				delete(oracle, p) // last-edge removal drops the pair's properties
			}
		}
		// Assert EdgeProperties matches the oracle for every node pair.
		for _, sn := range nodes {
			for _, dn := range nodes {
				pp := pair{sn, dn}
				got := g.EdgeProperties(sn, dn)
				want := oracle[pp]
				if len(got) != len(want) {
					t.Fatalf("seed=%d step=%d pair=%s->%s: len got=%d want=%d (got=%v want=%v)",
						seed, step, sn, dn, len(got), len(want), got, want)
				}
				for k, wv := range want {
					gv, ok := got[k]
					if !ok {
						t.Fatalf("seed=%d step=%d pair=%s->%s key=%s missing", seed, step, sn, dn, k)
					}
					if i, _ := gv.Int64(); i != wv {
						t.Fatalf("seed=%d step=%d pair=%s->%s key=%s: got=%d want=%d", seed, step, sn, dn, k, i, wv)
					}
				}
			}
		}
	}
}

// TestEdgePropTier_HeterogeneousKindAcrossEdges asserts that the SAME property
// key carrying DIFFERENT kinds on DIFFERENT edges of one source node round-trips,
// each edge reading back its own kind and value. openCypher allows a key to carry
// different types across edges; the columnar tier handles this with one column
// per (key, kind), so a key set as int64 on one edge and string on another lands
// in two separate columns. This proves the "boxed overflow tier for same-key type
// collisions" the design sketched is UNNECESSARY: per-(key,kind) columns subsume
// it, and no such tier exists in the implementation.
func TestEdgePropTier_HeterogeneousKindAcrossEdges(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	dsts := []string{"b", "c", "d", "e", "f"}
	for _, d := range dsts {
		if err := g.AddEdge("a", d, 0); err != nil {
			t.Fatalf("AddEdge a->%s: %v", d, err)
		}
	}
	// One key "v", a DIFFERENT kind per edge.
	g.mustSet(t, "a", "b", "v", Int64Value(42))
	g.mustSet(t, "a", "c", "v", StringValue("forty-two"))
	g.mustSet(t, "a", "d", "v", Float64Value(4.2))
	g.mustSet(t, "a", "e", "v", BoolValue(true))
	g.mustSet(t, "a", "f", "v", StringValue("\x012020-01-15")) // tagged Date

	// Each edge reads back exactly its own kind and value.
	check := func(dst string, kind PropertyKind, verify func(PropertyValue) bool) {
		v, ok := g.GetEdgeProperty("a", dst, "v")
		if !ok {
			t.Fatalf("a->%s missing key v", dst)
		}
		if v.Kind() != kind {
			t.Fatalf("a->%s v kind = %d, want %d", dst, v.Kind(), kind)
		}
		if !verify(v) {
			t.Fatalf("a->%s v value mismatch: %v", dst, v)
		}
	}
	check("b", PropInt64, func(v PropertyValue) bool { i, _ := v.Int64(); return i == 42 })
	check("c", PropString, func(v PropertyValue) bool { s, _ := v.String(); return s == "forty-two" })
	check("d", PropFloat64, func(v PropertyValue) bool { f, _ := v.Float64(); return f == 4.2 })
	check("e", PropBool, func(v PropertyValue) bool { b, _ := v.Bool(); return b })
	check("f", PropString, func(v PropertyValue) bool { s, _ := v.String(); return s == "\x012020-01-15" })

	// The block holds one column per (key, kind): v as int64, string, float64,
	// bool, and date (5 distinct columns), and no boxed-overflow spill structure.
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	block := asEdgePropCols(g.AdjList().LoadEntryAux(srcID))
	if block == nil {
		t.Fatalf("no aux block")
	}
	kinds := map[PropertyKind]bool{}
	for i := range block.cols {
		if block.cols[i].key != g.pkeys.Intern("v") {
			continue
		}
		if kinds[block.cols[i].kind] {
			t.Fatalf("duplicate column for kind %d", block.cols[i].kind)
		}
		kinds[block.cols[i].kind] = true
	}
	for _, k := range []PropertyKind{PropInt64, PropString, PropFloat64, PropBool, dateKind} {
		if !kinds[k] {
			t.Fatalf("missing per-(key,kind) column for kind %d", k)
		}
	}
}

// mustSet is a test helper that sets an edge property and fails on error.
func (g *Graph[N, W]) mustSet(t *testing.T, src, dst N, key string, v PropertyValue) {
	t.Helper()
	if err := g.SetEdgeProperty(src, dst, key, v); err != nil {
		t.Fatalf("SetEdgeProperty(%v,%v,%q): %v", src, dst, key, err)
	}
}
