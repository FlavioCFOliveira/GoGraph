package lpg_test

import (
	"slices"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// sortedEdgeLabels returns EdgeLabels(src,dst) sorted for deterministic
// comparison.
func sortedEdgeLabels[N comparable, W any](g *lpg.Graph[N, W], src, dst N) []string {
	got := g.EdgeLabels(src, dst)
	sort.Strings(got)
	return got
}

func eqStrings(a, b []string) bool { return slices.Equal(a, b) }

// TestEdgeLabel_Derived_SingleLabel covers case (a): a simple-graph single
// edge with a single label, stored inline in the adjacency slot with no
// overflow.
func TestEdgeLabel_Derived_SingleLabel(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("a", "b", "KNOWS")

	if !g.HasEdgeLabel("a", "b", "KNOWS") {
		t.Fatal("HasEdgeLabel(KNOWS) = false, want true")
	}
	if g.HasEdgeLabel("a", "b", "LIKES") {
		t.Fatal("HasEdgeLabel(LIKES) = true, want false")
	}
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"KNOWS"}) {
		t.Fatalf("EdgeLabels = %v, want [KNOWS]", got)
	}
	// Idempotent re-set: still exactly one label, no overflow growth.
	g.SetEdgeLabel("a", "b", "KNOWS")
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"KNOWS"}) {
		t.Fatalf("after re-set EdgeLabels = %v, want [KNOWS]", got)
	}
}

// TestEdgeLabel_Derived_MultiLabel covers case (b): a single edge carrying two
// labels — the first inline, the second in overflow — unioned by EdgeLabels.
func TestEdgeLabel_Derived_MultiLabel(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("a", "b", "A")
	g.SetEdgeLabel("a", "b", "B")
	g.SetEdgeLabel("a", "b", "C")

	for _, name := range []string{"A", "B", "C"} {
		if !g.HasEdgeLabel("a", "b", name) {
			t.Errorf("HasEdgeLabel(%q) = false, want true", name)
		}
	}
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"A", "B", "C"}) {
		t.Fatalf("EdgeLabels = %v, want [A B C]", got)
	}
	// Remove the inline (first) label: the remaining overflow labels survive.
	g.RemoveEdgeLabel("a", "b", "A")
	if g.HasEdgeLabel("a", "b", "A") {
		t.Fatal("HasEdgeLabel(A) = true after remove, want false")
	}
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"B", "C"}) {
		t.Fatalf("after remove A: EdgeLabels = %v, want [B C]", got)
	}
	// Remove an overflow label.
	g.RemoveEdgeLabel("a", "b", "C")
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"B"}) {
		t.Fatalf("after remove C: EdgeLabels = %v, want [B]", got)
	}
}

// TestEdgeLabel_Derived_MultigraphSharedType covers case (c): parallel edges
// A->B that share a single relationship type must report that type ONCE
// (dedup across slots), never doubled.
func TestEdgeLabel_Derived_MultigraphSharedType(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge #1: %v", err)
	}
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge #2: %v", err)
	}
	// Both parallel edges carry the same type; SetEdgeLabel is called once per
	// CREATE (mirroring the executor), so it is invoked twice with "T".
	g.SetEdgeLabel("a", "b", "T")
	g.SetEdgeLabel("a", "b", "T")

	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"T"}) {
		t.Fatalf("EdgeLabels = %v, want exactly one [T] (deduped across slots)", got)
	}
}

// TestEdgeLabel_Derived_MultigraphDistinctTypes covers case (c) with distinct
// types on parallel edges: the union is the set of distinct types.
func TestEdgeLabel_Derived_MultigraphDistinctTypes(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge #1: %v", err)
	}
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge #2: %v", err)
	}
	// Two distinct types: first inline on slot 0, second spills to overflow
	// (because the first slot already holds a different label).
	g.SetEdgeLabel("a", "b", "X")
	g.SetEdgeLabel("a", "b", "Y")

	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"X", "Y"}) {
		t.Fatalf("EdgeLabels = %v, want [X Y]", got)
	}
	// Removing one distinct type leaves the other.
	g.RemoveEdgeLabel("a", "b", "X")
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"Y"}) {
		t.Fatalf("after remove X: EdgeLabels = %v, want [Y]", got)
	}
}

// TestEdgeLabel_Orphan_SetThenRemoveAfterEdgeGone replays the executor
// transaction-undo sequence the design's invariant 4 protects: a label set
// while the edge exists must remain removable via RemoveEdgeLabel even after
// the edge is gone (RemoveEdgeLabel does NOT require HasEdge). The label must
// reside in overflow once it can no longer live in a slot.
func TestEdgeLabel_Orphan_SetThenRemoveAfterEdgeGone(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// Two labels so the second is in overflow (an orphan-able representation).
	g.SetEdgeLabel("a", "b", "L1")
	g.SetEdgeLabel("a", "b", "L2")

	// Remove the edge entirely: clearEdgePairState wipes both slot and overflow.
	g.RemoveEdge("a", "b")
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("after RemoveEdge EdgeLabels = %v, want empty", got)
	}
	// RemoveEdgeLabel on the now-gone edge must be a safe no-op (does not
	// require HasEdge, does not panic, does not resurrect anything).
	g.RemoveEdgeLabel("a", "b", "L1")
	g.RemoveEdgeLabel("a", "b", "L2")
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("after RemoveEdgeLabel on gone edge: EdgeLabels = %v, want empty", got)
	}

	// Re-create the edge between the same endpoints: no removed label resurrects.
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("re-AddEdge: %v", err)
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("after re-create EdgeLabels = %v, want empty (no resurrection)", got)
	}
}

// TestAddEdgeLabeled_EquivalentToAddThenSet proves the fused build fast path is
// observationally identical to the two-step AddEdge + SetEdgeLabel for the
// single-label case across every read surface: EdgeLabels, HasEdgeLabel, the
// per-slot scan (via EdgeLabels dedup), and RelationshipTypesInUse. Two graphs
// are built the two ways and compared.
func TestAddEdgeLabeled_EquivalentToAddThenSet(t *testing.T) {
	t.Parallel()
	type edge struct {
		src, dst, typ string
	}
	edges := []edge{
		{"a", "b", "KNOWS"},
		{"a", "c", "LIKES"},
		{"b", "c", "KNOWS"},
		{"c", "a", "FOLLOWS"},
	}

	twoStep := lpg.New[string, int64](adjlist.Config{Directed: true})
	fused := lpg.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range edges {
		if err := twoStep.AddEdge(e.src, e.dst, 0); err != nil {
			t.Fatalf("two-step AddEdge: %v", err)
		}
		twoStep.SetEdgeLabel(e.src, e.dst, e.typ)
		if err := fused.AddEdgeLabeled(e.src, e.dst, 0, e.typ); err != nil {
			t.Fatalf("fused AddEdgeLabeled: %v", err)
		}
	}

	for _, e := range edges {
		gotTwo := sortedEdgeLabels(twoStep, e.src, e.dst)
		gotFused := sortedEdgeLabels(fused, e.src, e.dst)
		if !eqStrings(gotTwo, gotFused) {
			t.Fatalf("EdgeLabels(%s,%s): two-step %v != fused %v", e.src, e.dst, gotTwo, gotFused)
		}
		if !fused.HasEdgeLabel(e.src, e.dst, e.typ) {
			t.Errorf("fused HasEdgeLabel(%s,%s,%s) = false, want true", e.src, e.dst, e.typ)
		}
	}

	twoTypes := twoStep.RelationshipTypesInUse()
	fusedTypes := fused.RelationshipTypesInUse()
	sort.Strings(twoTypes)
	sort.Strings(fusedTypes)
	if !eqStrings(twoTypes, fusedTypes) {
		t.Fatalf("RelationshipTypesInUse: two-step %v != fused %v", twoTypes, fusedTypes)
	}
}

// TestAddEdgeLabeled_ThenAddSecondLabel verifies that after a fused single-label
// insertion, adding a SECOND distinct type via SetEdgeLabel still spills to
// overflow exactly as it would for a two-step-built edge — the fused path does
// not disturb the multi-label machinery.
func TestAddEdgeLabeled_ThenAddSecondLabel(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdgeLabeled("a", "b", 0, "X"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	g.SetEdgeLabel("a", "b", "Y") // distinct type -> overflow
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"X", "Y"}) {
		t.Fatalf("EdgeLabels = %v, want [X Y]", got)
	}
	// Removing the inline (fused) label leaves the overflow label.
	g.RemoveEdgeLabel("a", "b", "X")
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"Y"}) {
		t.Fatalf("after remove X: EdgeLabels = %v, want [Y]", got)
	}
}

// TestAddEdgeLabeled_Undirected verifies the fused labelled insertion on an
// undirected graph reports the type from both endpoints' perspective.
func TestAddEdgeLabeled_Undirected(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: false})
	if err := g.AddEdgeLabeled("a", "b", 0, "PEER"); err != nil {
		t.Fatalf("AddEdgeLabeled: %v", err)
	}
	if got := sortedEdgeLabels(g, "a", "b"); !eqStrings(got, []string{"PEER"}) {
		t.Fatalf("EdgeLabels(a,b) = %v, want [PEER]", got)
	}
	if got := sortedEdgeLabels(g, "b", "a"); !eqStrings(got, []string{"PEER"}) {
		t.Fatalf("EdgeLabels(b,a) = %v, want [PEER]", got)
	}
}

// TestEdgeLabel_RelationshipTypesInUse covers the introspection enumerator over
// the new representation: it must dedup across inline slots and overflow and
// drop types whose only bearing edge was removed.
func TestEdgeLabel_RelationshipTypesInUse(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge a->b #1: %v", err)
	}
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge a->b #2: %v", err)
	}
	if err := g.AddEdge("c", "d", 0); err != nil {
		t.Fatalf("AddEdge c->d: %v", err)
	}
	g.SetEdgeLabel("a", "b", "T")  // inline on first slot
	g.SetEdgeLabel("a", "b", "T2") // overflow (different type, slot taken)
	g.SetEdgeLabel("c", "d", "T")  // shared type on a different pair

	got := g.RelationshipTypesInUse()
	sort.Strings(got)
	if !eqStrings(got, []string{"T", "T2"}) {
		t.Fatalf("RelationshipTypesInUse = %v, want [T T2] (deduped)", got)
	}

	// Removing the c->d edge drops "T" only if no other live edge bears it; but
	// a->b still bears "T", so it must remain.
	g.RemoveEdge("c", "d")
	got = g.RelationshipTypesInUse()
	sort.Strings(got)
	if !eqStrings(got, []string{"T", "T2"}) {
		t.Fatalf("after remove c->d: RelationshipTypesInUse = %v, want [T T2]", got)
	}
}
