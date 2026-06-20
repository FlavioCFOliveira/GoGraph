package lpg

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// edge_property_fused_test.go — equivalence and correctness tests for the fused
// property-carrying append path ([Graph.AddEdgeLabeledWithProperty], sprint 222
// #1646). The contract is that the fused write is OBSERVATIONALLY IDENTICAL to
// the two-step AddEdgeLabeled + SetEdgeProperty it replaces on the bulk-build
// path, while doing it in O(degree) per source rather than O(degree²).

// propValString renders a PropertyValue to a comparable string for equality
// assertions across the fused and two-step paths (no PropertyValue.Equal exists).
func propValString(v PropertyValue) string {
	switch v.Kind() {
	case PropInt64:
		i, _ := v.Int64()
		return fmt.Sprintf("i:%d", i)
	case PropFloat64:
		f, _ := v.Float64()
		return fmt.Sprintf("f:%v", f)
	case PropBool:
		b, _ := v.Bool()
		return fmt.Sprintf("b:%v", b)
	case PropString:
		s, _ := v.String()
		return fmt.Sprintf("s:%q", s)
	default:
		return fmt.Sprintf("?:%v", v)
	}
}

// edgePropsString renders the coalesced per-pair property map deterministically.
func edgePropsString(m map[string]PropertyValue) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion-sort: tiny maps.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	out := "{"
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		out += k + "=" + propValString(m[k])
	}
	return out + "}"
}

// TestFused_EquivalenceSingleEdge asserts the fused path produces exactly the
// same per-pair properties, relationship label, and edge presence as the
// two-step path for a single edge, for every property kind.
func TestFused_EquivalenceSingleEdge(t *testing.T) {
	t.Parallel()
	kinds := []struct {
		name string
		v    PropertyValue
	}{
		{"int", Int64Value(2024)},
		{"float", Float64Value(3.14)},
		{"bool", BoolValue(true)},
		{"str", StringValue("hello")},
		{"date", StringValue("\x012020-06-19")}, // SOH-tagged Date
	}
	for _, k := range kinds {
		k := k
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			// Two-step reference.
			ref := New[string, int64](adjlist.Config{Directed: true})
			if err := ref.AddEdgeLabeled("a", "b", 1, "REL"); err != nil {
				t.Fatalf("AddEdgeLabeled: %v", err)
			}
			ref.mustSet(t, "a", "b", "p", k.v)

			// Fused.
			fused := New[string, int64](adjlist.Config{Directed: true})
			if err := fused.AddEdgeLabeledWithProperty("a", "b", 1, "REL", "p", k.v); err != nil {
				t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
			}

			refProps := edgePropsString(ref.EdgeProperties("a", "b"))
			fusedProps := edgePropsString(fused.EdgeProperties("a", "b"))
			if refProps != fusedProps {
				t.Fatalf("EdgeProperties mismatch:\n two-step: %s\n fused:    %s", refProps, fusedProps)
			}
			// Relationship label identical.
			refLabels := ref.EdgeLabels("a", "b")
			fusedLabels := fused.EdgeLabels("a", "b")
			if len(refLabels) != len(fusedLabels) || (len(refLabels) == 1 && refLabels[0] != fusedLabels[0]) {
				t.Fatalf("EdgeLabels mismatch: two-step %v fused %v", refLabels, fusedLabels)
			}
			if !fused.AdjList().HasEdge("a", "b") {
				t.Fatalf("fused edge not present")
			}
		})
	}
}

// TestFused_EquivalenceHighDegree is the regression's core scenario: a single
// high-degree source receiving one fused property per edge. It asserts the fused
// per-slot state coalesces to exactly the two-step result for EVERY pair (so the
// O(degree²)→O(degree) optimisation changed cost, not behaviour).
func TestFused_EquivalenceHighDegree(t *testing.T) {
	t.Parallel()
	const degree = 600 // large enough to span several geometric grows + a reshape
	ref := New[string, int64](adjlist.Config{Directed: true})
	fused := New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < degree; i++ {
		dst := fmt.Sprintf("d%d", i)
		// A mix of date strings (folded to epoch-day) and plain strings.
		val := fmt.Sprintf("\x012020-01-%02d", (i%28)+1)
		if err := ref.AddEdgeLabeled("hub", dst, 1, "FRIEND"); err != nil {
			t.Fatalf("ref AddEdgeLabeled: %v", err)
		}
		ref.mustSet(t, "hub", dst, "since", StringValue(val))
		if err := fused.AddEdgeLabeledWithProperty("hub", dst, 1, "FRIEND", "since", StringValue(val)); err != nil {
			t.Fatalf("fused AddEdgeLabeledWithProperty: %v", err)
		}
	}
	for i := 0; i < degree; i++ {
		dst := fmt.Sprintf("d%d", i)
		r := edgePropsString(ref.EdgeProperties("hub", dst))
		f := edgePropsString(fused.EdgeProperties("hub", dst))
		if r != f {
			t.Fatalf("pair hub->%s mismatch: two-step %s fused %s", dst, r, f)
		}
	}
}

// TestFused_EquivalenceMultigraphParallelEdges asserts the fused path keeps the
// per-pair coalesce contract under parallel edges: every dst-matching slot must
// carry the latest value, so a SetEdgeProperty after a fused parallel build still
// fans out to all slots and reads consistently.
func TestFused_EquivalenceMultigraphParallelEdges(t *testing.T) {
	t.Parallel()
	ref := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	fused := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	// Three parallel a->b edges, each fused with the SAME key but a per-edge value;
	// the per-pair coalesce takes the latest dst-matching slot, so both paths must
	// agree on the last value.
	for i, val := range []string{"\x012021-03-01", "\x012021-03-02", "\x012021-03-03"} {
		if err := ref.AddEdgeLabeled("a", "b", int64(i), "LIKE"); err != nil {
			t.Fatalf("ref add: %v", err)
		}
		ref.mustSet(t, "a", "b", "when", StringValue(val)) // fans to all slots
		if err := fused.AddEdgeLabeledWithProperty("a", "b", int64(i), "LIKE", "when", StringValue(val)); err != nil {
			t.Fatalf("fused add: %v", err)
		}
	}
	r := edgePropsString(ref.EdgeProperties("a", "b"))
	f := edgePropsString(fused.EdgeProperties("a", "b"))
	if r != f {
		t.Fatalf("multigraph coalesce mismatch: two-step %s fused %s", r, f)
	}
	// A subsequent SetEdgeProperty on the fused graph must fan out to every
	// parallel slot exactly as on the reference graph.
	ref.mustSet(t, "a", "b", "when", StringValue("\x012021-12-31"))
	fused.mustSet(t, "a", "b", "when", StringValue("\x012021-12-31"))
	if got := edgePropsString(fused.EdgeProperties("a", "b")); got != edgePropsString(ref.EdgeProperties("a", "b")) {
		t.Fatalf("post-fan-out mismatch: fused %s ref %s", got, edgePropsString(ref.EdgeProperties("a", "b")))
	}
}

// TestFused_MixedWithSetEdgeProperty asserts a fused build followed by general-path
// mutations (set a second key, overwrite, delete) behaves exactly like the
// two-step build under the same mutations — exercising the dense↔sparse and the
// grownWithValue-after-dense paths.
func TestFused_MixedWithSetEdgeProperty(t *testing.T) {
	t.Parallel()
	ref := New[string, int64](adjlist.Config{Directed: true})
	fused := New[string, int64](adjlist.Config{Directed: true})
	const degree = 40
	for i := 0; i < degree; i++ {
		dst := fmt.Sprintf("d%d", i)
		if err := ref.AddEdgeLabeled("h", dst, 1, "R"); err != nil {
			t.Fatalf("ref add: %v", err)
		}
		ref.mustSet(t, "h", dst, "a", Int64Value(int64(i)))
		if err := fused.AddEdgeLabeledWithProperty("h", dst, 1, "R", "a", Int64Value(int64(i))); err != nil {
			t.Fatalf("fused add: %v", err)
		}
	}
	// Now drive identical general-path mutations on both.
	for _, g := range []*Graph[string, int64]{ref, fused} {
		g.mustSet(t, "h", "d5", "b", StringValue("x"))    // second key on one pair
		g.mustSet(t, "h", "d5", "a", Int64Value(999))     // overwrite the fused key
		g.mustSet(t, "h", "d10", "c", Float64Value(2.71)) // second key, another pair
		g.DelEdgeProperty("h", "d20", "a")                // delete a fused key
	}
	for i := 0; i < degree; i++ {
		dst := fmt.Sprintf("d%d", i)
		if r, f := edgePropsString(ref.EdgeProperties("h", dst)), edgePropsString(fused.EdgeProperties("h", dst)); r != f {
			t.Fatalf("pair h->%s mismatch after mixed mutation: two-step %s fused %s", dst, r, f)
		}
	}
}

// TestFused_EquivalenceAfterRemoval asserts the per-slot columns stay aligned
// through edge removal (compaction) after a fused build, matching the two-step
// path. This exercises CompactSlot on a fused-built sparse block.
func TestFused_EquivalenceAfterRemoval(t *testing.T) {
	t.Parallel()
	ref := New[string, int64](adjlist.Config{Directed: true})
	fused := New[string, int64](adjlist.Config{Directed: true})
	const degree = 30
	for i := 0; i < degree; i++ {
		dst := fmt.Sprintf("d%d", i)
		if err := ref.AddEdgeLabeled("h", dst, 1, "R"); err != nil {
			t.Fatalf("ref add: %v", err)
		}
		ref.mustSet(t, "h", dst, "k", Int64Value(int64(100+i)))
		if err := fused.AddEdgeLabeledWithProperty("h", dst, 1, "R", "k", Int64Value(int64(100+i))); err != nil {
			t.Fatalf("fused add: %v", err)
		}
	}
	// Remove a handful of edges from both (middle, first, last).
	for _, d := range []string{"d0", "d15", fmt.Sprintf("d%d", degree-1), "d7"} {
		ref.RemoveEdge("h", d)
		fused.RemoveEdge("h", d)
	}
	for i := 0; i < degree; i++ {
		dst := fmt.Sprintf("d%d", i)
		if r, f := edgePropsString(ref.EdgeProperties("h", dst)), edgePropsString(fused.EdgeProperties("h", dst)); r != f {
			t.Fatalf("pair h->%s mismatch after removal: two-step %s fused %s", dst, r, f)
		}
		if ref.AdjList().HasEdge("h", dst) != fused.AdjList().HasEdge("h", dst) {
			t.Fatalf("HasEdge h->%s differs after removal", dst)
		}
	}
}

// TestFused_EquivalenceUndirected asserts the undirected mirror carries the same
// fused property on both directions, matching the two-step path.
func TestFused_EquivalenceUndirected(t *testing.T) {
	t.Parallel()
	ref := New[string, int64](adjlist.Config{Directed: false})
	fused := New[string, int64](adjlist.Config{Directed: false})
	if err := ref.AddEdgeLabeled("x", "y", 1, "KNOWS"); err != nil {
		t.Fatalf("ref add: %v", err)
	}
	ref.mustSet(t, "x", "y", "since", StringValue("\x012019-05-05"))
	if err := fused.AddEdgeLabeledWithProperty("x", "y", 1, "KNOWS", "since", StringValue("\x012019-05-05")); err != nil {
		t.Fatalf("fused add: %v", err)
	}
	for _, pair := range [][2]string{{"x", "y"}, {"y", "x"}} {
		r := edgePropsString(ref.EdgeProperties(pair[0], pair[1]))
		f := edgePropsString(fused.EdgeProperties(pair[0], pair[1]))
		if r != f {
			t.Fatalf("undirected %s->%s mismatch: two-step %s fused %s", pair[0], pair[1], r, f)
		}
	}
}

// TestFused_RandomisedEquivalence builds two graphs — one fused, one two-step —
// from the same randomised script of labelled property-carrying edges across many
// sources and degrees, and asserts every pair coalesces identically. This is the
// broad differential check that the fused path never diverges from the contract.
func TestFused_RandomisedEquivalence(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(0x1646))
	ref := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	fused := New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	const sources = 50
	type pair struct{ src, dst string }
	seen := map[pair]struct{}{}
	for op := 0; op < 4000; op++ {
		src := fmt.Sprintf("s%d", rng.Intn(sources))
		dst := fmt.Sprintf("t%d", rng.Intn(sources*3))
		val := fmt.Sprintf("\x012020-%02d-%02d", (rng.Intn(12))+1, (rng.Intn(28))+1)
		if err := ref.AddEdgeLabeled(src, dst, 1, "E"); err != nil {
			t.Fatalf("ref add: %v", err)
		}
		ref.mustSet(t, src, dst, "d", StringValue(val))
		if err := fused.AddEdgeLabeledWithProperty(src, dst, 1, "E", "d", StringValue(val)); err != nil {
			t.Fatalf("fused add: %v", err)
		}
		seen[pair{src, dst}] = struct{}{}
	}
	for p := range seen {
		r := edgePropsString(ref.EdgeProperties(p.src, p.dst))
		f := edgePropsString(fused.EdgeProperties(p.src, p.dst))
		if r != f {
			t.Fatalf("pair %s->%s mismatch: two-step %s fused %s", p.src, p.dst, r, f)
		}
	}
}
