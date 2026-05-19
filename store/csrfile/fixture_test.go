package csrfile

import "testing"

func TestBuildFixture_Deterministic(t *testing.T) {
	t.Parallel()
	spec := FixtureSpec{Vertices: 256, Edges: 4096, Seed: 7, Multigraph: true}
	a := BuildFixture(spec)
	b := BuildFixture(spec)
	if a.Size() != b.Size() || a.Order() != b.Order() {
		t.Fatalf("non-deterministic fixture: a=(%d,%d) b=(%d,%d)",
			a.Order(), a.Size(), b.Order(), b.Size())
	}
	ae := a.EdgesSlice()
	be := b.EdgesSlice()
	if len(ae) != len(be) {
		t.Fatalf("edges length mismatch")
	}
	for i := range ae {
		if ae[i] != be[i] {
			t.Fatalf("edges differ at %d", i)
		}
	}
}

func TestBuildFixture_SeedVariation(t *testing.T) {
	t.Parallel()
	a := BuildFixture(FixtureSpec{Vertices: 100, Edges: 1024, Seed: 1, Multigraph: true})
	b := BuildFixture(FixtureSpec{Vertices: 100, Edges: 1024, Seed: 2, Multigraph: true})
	// Same size, different content
	if a.Size() != b.Size() {
		t.Fatalf("sizes differ unexpectedly")
	}
	ae := a.EdgesSlice()
	be := b.EdgesSlice()
	same := 0
	for i := range ae {
		if ae[i] == be[i] {
			same++
		}
	}
	if same == len(ae) {
		t.Fatalf("different seeds produced identical edge sequences")
	}
}
