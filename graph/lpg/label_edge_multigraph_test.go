package lpg_test

// TestLPG_EdgeLabel_Multigraph verifies that edge labels operate on
// the per-(src,dst)-pair model under multigraph topologies produced
// by shapegen.ParallelDigon.
//
// Model contract (from edge_labels.go / lpg.go):
//
//   - edgeKey is keyed by (src NodeID, dst NodeID); all parallel edges
//     between the same endpoints share a single label bag.
//   - SetEdgeLabel(src, dst, name) adds name to that shared bag and is a
//     no-op when no edge exists between src and dst.
//   - HasEdgeLabel(src, dst, name) queries the shared bag.
//   - EdgeLabels(src, dst) returns the full bag contents in unspecified
//     order; callers must sort for deterministic assertions.
//
// Because parallel edges share one bag, this file tests:
//  1. Multiple labels accumulate correctly in the shared bag.
//  2. HasEdgeLabel is true for every label added and false for labels
//     never added.
//  3. EdgeLabels (sorted) equals exactly the set of labels added.
//  4. Labels assigned to pair (0,1) are invisible on the reverse pair
//     (1,0), which has no edges in a directed ParallelDigon.
//  5. Behaviour is consistent across ParallelDigon(2) and
//     ParallelDigon(8) — the multiplicity of parallel edges does not
//     alter label semantics.

import (
	"sort"
	"testing"

	"gograph/graph/adjlist"
	"gograph/internal/shapegen"
)

// TestLPG_EdgeLabel_ParallelDigon2 exercises the two-parallel-edge
// digon. Both edges 0→1 share one label bag; this test assigns two
// distinct labels and verifies accumulation and isolation semantics.
func TestLPG_EdgeLabel_ParallelDigon2(t *testing.T) {
	t.Parallel()

	g, err := shapegen.ParallelDigon(2).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// No labels set yet — EdgeLabels must return nil and HasEdgeLabel false.
	if got := g.EdgeLabels(0, 1); got != nil {
		t.Errorf("EdgeLabels before any Set: want nil, got %v", got)
	}
	if g.HasEdgeLabel(0, 1, "A") {
		t.Error("HasEdgeLabel(0,1,'A') before Set: want false, got true")
	}

	// Assign two labels to the shared pair bag.
	g.SetEdgeLabel(0, 1, "A")
	g.SetEdgeLabel(0, 1, "B")

	// HasEdgeLabel must be true for both assigned labels.
	for _, name := range []string{"A", "B"} {
		if !g.HasEdgeLabel(0, 1, name) {
			t.Errorf("HasEdgeLabel(0,1,%q): want true, got false", name)
		}
	}

	// A label never assigned must report false.
	if g.HasEdgeLabel(0, 1, "C") {
		t.Error("HasEdgeLabel(0,1,'C'): want false, got true")
	}

	// EdgeLabels (sorted) must equal exactly {"A", "B"}.
	got := g.EdgeLabels(0, 1)
	sort.Strings(got)
	want := []string{"A", "B"}
	if len(got) != len(want) {
		t.Fatalf("EdgeLabels(0,1): got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("EdgeLabels(0,1)[%d]: got %q, want %q", i, got[i], w)
		}
	}

	// Label isolation: the reverse pair (1,0) has no edges and must
	// not carry the labels assigned to (0,1).
	if g.HasEdgeLabel(1, 0, "A") {
		t.Error("HasEdgeLabel(1,0,'A'): reverse pair must be isolated, got true")
	}
	if got := g.EdgeLabels(1, 0); got != nil {
		t.Errorf("EdgeLabels(1,0): want nil for non-existent edge, got %v", got)
	}

	// SetEdgeLabel on a non-existent edge must be a no-op.
	g.SetEdgeLabel(1, 0, "X")
	if g.HasEdgeLabel(1, 0, "X") {
		t.Error("SetEdgeLabel on missing edge must not persist label")
	}
}

// TestLPG_EdgeLabel_ParallelDigon8 exercises an eight-parallel-edge
// digon. Assigns a distinct label per "slot" using a label alphabet
// A–H, verifies full accumulation and per-label HasEdgeLabel, then
// verifies that an oracle built from the label set matches the scan.
func TestLPG_EdgeLabel_ParallelDigon8(t *testing.T) {
	t.Parallel()

	const k = 8
	g, err := shapegen.ParallelDigon(k).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Assign one label per "slot" — all land in the same (0,1) bag.
	labels := make([]string, k)
	for i := 0; i < k; i++ {
		labels[i] = string(rune('A' + i)) // "A" … "H"
		g.SetEdgeLabel(0, 1, labels[i])
	}

	// HasEdgeLabel must report true for every assigned label.
	for _, name := range labels {
		if !g.HasEdgeLabel(0, 1, name) {
			t.Errorf("HasEdgeLabel(0,1,%q): want true, got false", name)
		}
	}

	// Labels not in the alphabet must be absent.
	for _, name := range []string{"I", "Z", "", "a"} {
		if g.HasEdgeLabel(0, 1, name) {
			t.Errorf("HasEdgeLabel(0,1,%q): unexpected label present", name)
		}
	}

	// EdgeLabels (sorted) must equal exactly the assigned label set.
	got := g.EdgeLabels(0, 1)
	sort.Strings(got)
	want := append([]string(nil), labels...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("EdgeLabels(0,1): len %d, want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("EdgeLabels(0,1)[%d]: got %q, want %q", i, got[i], w)
		}
	}

	// Oracle scan: iterate over the label alphabet and count how many
	// labels HasEdgeLabel reports true for. The model is per-pair, so
	// the count must equal k (every label is in the shared bag).
	present := 0
	for _, name := range labels {
		if g.HasEdgeLabel(0, 1, name) {
			present++
		}
	}
	if present != k {
		t.Errorf("oracle scan: %d labels present, want %d", present, k)
	}

	// Partial label assignment on a second pair doesn't exist in a
	// ParallelDigon (only pair 0→1 has edges), so verify isolation
	// from an arbitrary non-existent pair.
	if g.HasEdgeLabel(1, 0, "A") {
		t.Error("HasEdgeLabel(1,0,'A'): reverse pair must remain unlabelled")
	}
}

// TestLPG_EdgeLabel_SharedBag confirms that the per-pair sharing
// contract holds: assigning a subset of labels via repeated
// SetEdgeLabel calls is idempotent and does not duplicate entries.
func TestLPG_EdgeLabel_SharedBag(t *testing.T) {
	t.Parallel()

	g, err := shapegen.ParallelDigon(4).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Assign the same label multiple times — EdgeLabels must not
	// accumulate duplicates because the bag uses set semantics.
	for i := 0; i < 5; i++ {
		g.SetEdgeLabel(0, 1, "REPEAT")
	}
	got := g.EdgeLabels(0, 1)
	if len(got) != 1 {
		t.Errorf("repeated SetEdgeLabel: EdgeLabels len %d, want 1; got %v", len(got), got)
	}
	if len(got) == 1 && got[0] != "REPEAT" {
		t.Errorf("EdgeLabels[0] = %q, want REPEAT", got[0])
	}

	// Assign two distinct labels and verify sorted round-trip.
	g.SetEdgeLabel(0, 1, "X")
	g.SetEdgeLabel(0, 1, "Y")
	got2 := g.EdgeLabels(0, 1)
	sort.Strings(got2)
	want := []string{"REPEAT", "X", "Y"}
	if len(got2) != len(want) {
		t.Fatalf("after adding X+Y: EdgeLabels %v, want %v", got2, want)
	}
	for i, w := range want {
		if got2[i] != w {
			t.Errorf("EdgeLabels[%d]: got %q, want %q", i, got2[i], w)
		}
	}
}
