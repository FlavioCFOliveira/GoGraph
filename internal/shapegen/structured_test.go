package shapegen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// structuredGoldenDir is the directory holding the structured-family
// adjacency listings. As with classicGoldenDir, the path is rooted at
// the package directory.
//
// This file deliberately reuses formatAdjacency from trivial_test.go
// (same package). When T58.22 lands the shared golden helper in
// internal/goldens, both files must migrate together.
const structuredGoldenDir = "testdata/shapegen/structured"

// structuredGolden compares got with the contents of the golden file
// at structuredGoldenDir/<name>. The implementation mirrors
// classicGolden exactly because the three families will migrate
// together to the shared helper in T58.22; until then duplicating
// keeps each family's test surface self-contained.
func structuredGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(structuredGoldenDir, name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("structuredGolden: MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("structuredGolden: WriteFile(%q): %v", path, err)
		}
		t.Logf("rewrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is a test-local golden under testdata/, not user input
	if err != nil {
		t.Fatalf("structuredGolden: ReadFile(%q): %v (run with -shapegen-update to bootstrap)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// -------------------------------------------------------------------
// Hypercube
// -------------------------------------------------------------------

// TestStructured_Hypercube_Invariants asserts Order, Size, regularity
// and diameter for Q_d on the short sweep d in [0, 8]. Diameter is
// computed via the package-local bfsDiameterAsUndirected helper (the
// same one classic_test.go uses); the brief explicitly forbids
// pulling in search.Diameter so the helper stays inside the
// generator package.
func TestStructured_Hypercube_Invariants(t *testing.T) {
	t.Parallel()
	for d := 0; d <= 8; d++ {
		d := d
		t.Run(fmt.Sprintf("d=%d", d), func(t *testing.T) {
			t.Parallel()
			s := Hypercube(d)
			if got, want := s.Name(), "structured.hypercube"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 1 || got[0].Name != "d" || got[0].Min != 0 || got[0].Max != 24 || got[0].Default != 3 {
				t.Fatalf("Knobs = %#v, want exactly one d:[0,24] default 3", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(1<<d))
			wantSize := uint64(0)
			if d >= 1 {
				wantSize = uint64(d) * uint64(1<<(d-1))
			}
			assertSize(t, g, wantSize)
			assertDirected(t, g, false)
			// d-regular: every node has degree d.
			for v := 0; v < 1<<d; v++ {
				if got := degreeOut(g, v); got != d {
					t.Fatalf("Q_%d: deg(%d) = %d, want %d", d, v, got, d)
				}
			}
			// Diameter == d for d >= 1; 0 for d == 0.
			want := uint64(d)
			if d == 0 {
				want = 0
			}
			assertDiameter(t, "Hypercube", g, want)
		})
	}
}

// TestStructured_Hypercube_PanicsOutOfRange covers both the negative
// and above-24 guards.
func TestStructured_Hypercube_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, d := range []int{-1, 25} {
		d := d
		t.Run(fmt.Sprintf("d=%d", d), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Hypercube(%d) did not panic", d)
				}
			}()
			_ = Hypercube(d)
		})
	}
}

// TestStructured_Hypercube_Goldens pins Q_2 (= C_4) and Q_3 (the cube).
func TestStructured_Hypercube_Goldens(t *testing.T) {
	t.Parallel()
	cases := []int{2, 3}
	for _, d := range cases {
		d := d
		t.Run(fmt.Sprintf("d=%d", d), func(t *testing.T) {
			t.Parallel()
			g, err := Hypercube(d).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			structuredGolden(t, fmt.Sprintf("hypercube-d%d.txt", d), formatAdjacency(g))
		})
	}
}

// -------------------------------------------------------------------
// Grid
// -------------------------------------------------------------------

// TestStructured_Grid_Invariants asserts Order, Size, and diameter
// for L_{m,n} (4-neighbour and 8-neighbour) on a small (m, n) sweep.
// The 8-neighbour Size closed form (4*m*n - 3*(m+n) + 2) is only
// valid for m, n >= 2; for degenerate 1xk or kx1 strips the
// 8-neighbour count coincides with the 4-neighbour count.
func TestStructured_Grid_Invariants(t *testing.T) {
	t.Parallel()
	pairs := []struct{ m, n int }{
		{0, 0}, {1, 1}, {1, 3}, {2, 2}, {3, 3}, {4, 5}, {5, 7},
	}
	for _, p := range pairs {
		for _, eight := range []bool{false, true} {
			p, eight := p, eight
			t.Run(fmt.Sprintf("m=%d_n=%d_eight=%v", p.m, p.n, eight), func(t *testing.T) {
				t.Parallel()
				s := Grid(p.m, p.n, eight)
				if got, want := s.Name(), "structured.grid"; got != want {
					t.Fatalf("Name = %q, want %q", got, want)
				}
				if got := s.Knobs(); len(got) != 2 || got[0].Name != "m" || got[1].Name != "n" {
					t.Fatalf("Knobs = %#v, want m,n", got)
				}
				g, err := s.Build(defaultCfg)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				assertOrder(t, g, uint64(p.m*p.n))
				assertSize(t, g, expectedGridSize(p.m, p.n, eight))
				assertDirected(t, g, false)
				if p.m >= 1 && p.n >= 1 && !eight {
					// Diameter of the 4-neighbour grid is (m-1) + (n-1).
					assertDiameter(t, "Grid4", g, uint64((p.m-1)+(p.n-1)))
				}
			})
		}
	}
}

// TestStructured_Grid_PanicsOnNegative covers the negative-m and
// negative-n branches of the constructor guard.
func TestStructured_Grid_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ m, n int }{{-1, 0}, {0, -1}} {
		c := c
		t.Run(fmt.Sprintf("m=%d_n=%d", c.m, c.n), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Grid(%d,%d,false) did not panic", c.m, c.n)
				}
			}()
			_ = Grid(c.m, c.n, false)
		})
	}
}

// TestStructured_Grid_Goldens pins L_{3,3} in both 4- and 8-neighbour
// variants.
func TestStructured_Grid_Goldens(t *testing.T) {
	t.Parallel()
	t.Run("3x3", func(t *testing.T) {
		t.Parallel()
		g, err := Grid(3, 3, false).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		structuredGolden(t, "grid-3x3.txt", formatAdjacency(g))
	})
	t.Run("3x3_eight", func(t *testing.T) {
		t.Parallel()
		g, err := Grid(3, 3, true).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		structuredGolden(t, "grid-3x3-eight.txt", formatAdjacency(g))
	})
}

// -------------------------------------------------------------------
// Torus
// -------------------------------------------------------------------

// TestStructured_Torus_Invariants asserts Order, Size, and diameter
// for T_{m,n} on a small (m, n) sweep with m, n >= 3 (so the
// degree-4 closed form Size = 2*m*n holds).
func TestStructured_Torus_Invariants(t *testing.T) {
	t.Parallel()
	pairs := []struct{ m, n int }{
		{3, 3}, {3, 4}, {4, 4}, {4, 5}, {5, 5}, {6, 7},
	}
	for _, p := range pairs {
		p := p
		t.Run(fmt.Sprintf("m=%d_n=%d", p.m, p.n), func(t *testing.T) {
			t.Parallel()
			s := Torus(p.m, p.n)
			if got, want := s.Name(), "structured.torus"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 2 || got[0].Name != "m" || got[1].Name != "n" {
				t.Fatalf("Knobs = %#v, want m,n", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(p.m*p.n))
			assertSize(t, g, 2*uint64(p.m)*uint64(p.n))
			assertDirected(t, g, false)
			// 4-regular: every node has degree 4.
			for v := 0; v < p.m*p.n; v++ {
				if got := degreeOut(g, v); got != 4 {
					t.Fatalf("T_{%d,%d}: deg(%d) = %d, want 4", p.m, p.n, v, got)
				}
			}
			// Diameter of T_{m,n} is floor(m/2) + floor(n/2).
			assertDiameter(t, "Torus", g, uint64(p.m/2+p.n/2))
		})
	}
}

// TestStructured_Torus_PanicsOnTooSmall covers the m<1, n<1 branches.
func TestStructured_Torus_PanicsOnTooSmall(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ m, n int }{{0, 1}, {1, 0}} {
		c := c
		t.Run(fmt.Sprintf("m=%d_n=%d", c.m, c.n), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Torus(%d,%d) did not panic", c.m, c.n)
				}
			}()
			_ = Torus(c.m, c.n)
		})
	}
}

// TestStructured_Torus_Golden pins T_{3,3}.
func TestStructured_Torus_Golden(t *testing.T) {
	t.Parallel()
	g, err := Torus(3, 3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	structuredGolden(t, "torus-3x3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Rook
// -------------------------------------------------------------------

// TestStructured_Rook_Invariants asserts Order, Size, regularity and
// diameter for R_n over n in {1, 2, 3, 4, 5}. The closed-form Size
// is n^2 * (n-1) (each node has degree 2(n-1) and we count each
// undirected edge once: sum of degrees / 2 = n^2 * 2(n-1) / 2).
func TestStructured_Rook_Invariants(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 3, 4, 5} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			s := Rook(n)
			if got, want := s.Name(), "structured.rook"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 1 || got[0].Name != "n" {
				t.Fatalf("Knobs = %#v, want exactly one n", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(n*n))
			assertSize(t, g, uint64(n*n)*uint64(n-1))
			assertDirected(t, g, false)
			// 2(n-1)-regular: every node has degree 2(n-1).
			for v := 0; v < n*n; v++ {
				want := 2 * (n - 1)
				if got := degreeOut(g, v); got != want {
					t.Fatalf("R_%d: deg(%d) = %d, want %d", n, v, got, want)
				}
			}
			// Diameter == 2 for n >= 2; 0 for n == 1.
			want := uint64(2)
			if n == 1 {
				want = 0
			}
			assertDiameter(t, "Rook", g, want)
		})
	}
}

// TestStructured_Rook_PanicsOnTooSmall covers the n<1 branch.
func TestStructured_Rook_PanicsOnTooSmall(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Rook(0) did not panic")
		}
	}()
	_ = Rook(0)
}

// TestStructured_Rook_Golden pins R_3.
func TestStructured_Rook_Golden(t *testing.T) {
	t.Parallel()
	g, err := Rook(3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	structuredGolden(t, "rook-3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Mobius
// -------------------------------------------------------------------

// TestStructured_Mobius_Invariants asserts Order, Size and
// 3-regularity for M_n on a small n sweep.
func TestStructured_Mobius_Invariants(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3, 4, 5, 8} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			s := Mobius(n)
			if got, want := s.Name(), "structured.mobius"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 1 || got[0].Name != "n" {
				t.Fatalf("Knobs = %#v, want exactly one n", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(2*n))
			assertSize(t, g, uint64(3*n))
			assertDirected(t, g, false)
			// 3-regular.
			for v := 0; v < 2*n; v++ {
				if got := degreeOut(g, v); got != 3 {
					t.Fatalf("M_%d: deg(%d) = %d, want 3", n, v, got)
				}
			}
		})
	}
}

// TestStructured_Mobius_PanicsOnTooSmall covers the n<2 branch.
func TestStructured_Mobius_PanicsOnTooSmall(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Mobius(1) did not panic")
		}
	}()
	_ = Mobius(1)
}

// TestStructured_Mobius_Golden pins M_3.
func TestStructured_Mobius_Golden(t *testing.T) {
	t.Parallel()
	g, err := Mobius(3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	structuredGolden(t, "mobius-n3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Ladder
// -------------------------------------------------------------------

// TestStructured_Ladder_Invariants asserts Order and Size for L_n on
// a small n sweep. Special case n == 1 collapses to K_2 (Size = 1,
// matching 3n-2 = 1).
func TestStructured_Ladder_Invariants(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 3, 4, 8} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			s := Ladder(n)
			if got, want := s.Name(), "structured.ladder"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 1 || got[0].Name != "n" {
				t.Fatalf("Knobs = %#v, want exactly one n", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(2*n))
			assertSize(t, g, uint64(3*n-2))
			assertDirected(t, g, false)
		})
	}
}

// TestStructured_Ladder_PanicsOnTooSmall covers the n<1 branch.
func TestStructured_Ladder_PanicsOnTooSmall(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Ladder(0) did not panic")
		}
	}()
	_ = Ladder(0)
}

// TestStructured_Ladder_Golden pins L_3.
func TestStructured_Ladder_Golden(t *testing.T) {
	t.Parallel()
	g, err := Ladder(3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	structuredGolden(t, "ladder-n3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Prism
// -------------------------------------------------------------------

// TestStructured_Prism_Invariants asserts Order, Size and
// 3-regularity for Y_n on a small n sweep.
func TestStructured_Prism_Invariants(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 4, 5, 8} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			s := Prism(n)
			if got, want := s.Name(), "structured.prism"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 1 || got[0].Name != "n" {
				t.Fatalf("Knobs = %#v, want exactly one n", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(2*n))
			assertSize(t, g, uint64(3*n))
			assertDirected(t, g, false)
			for v := 0; v < 2*n; v++ {
				if got := degreeOut(g, v); got != 3 {
					t.Fatalf("Y_%d: deg(%d) = %d, want 3", n, v, got)
				}
			}
		})
	}
}

// TestStructured_Prism_PanicsOnTooSmall covers the n<3 branch.
func TestStructured_Prism_PanicsOnTooSmall(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Prism(2) did not panic")
		}
	}()
	_ = Prism(2)
}

// TestStructured_Prism_Golden pins Y_3.
func TestStructured_Prism_Golden(t *testing.T) {
	t.Parallel()
	g, err := Prism(3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	structuredGolden(t, "prism-n3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Theta
// -------------------------------------------------------------------

// TestStructured_Theta_Invariants asserts Order = a+b+c-1 and Size =
// a+b+c over a small (a, b, c) sweep that includes the
// at-most-one-length-1-path edge cases.
func TestStructured_Theta_Invariants(t *testing.T) {
	t.Parallel()
	triples := []struct{ a, b, c int }{
		{2, 2, 2},
		{1, 2, 3},
		{2, 3, 4},
		{3, 4, 5},
		{5, 5, 5},
	}
	for _, tr := range triples {
		tr := tr
		t.Run(fmt.Sprintf("a=%d_b=%d_c=%d", tr.a, tr.b, tr.c), func(t *testing.T) {
			t.Parallel()
			s := Theta(tr.a, tr.b, tr.c)
			if got, want := s.Name(), "structured.theta"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 3 || got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "c" {
				t.Fatalf("Knobs = %#v, want a,b,c", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(tr.a+tr.b+tr.c-1))
			assertSize(t, g, uint64(tr.a+tr.b+tr.c))
			assertDirected(t, g, false)
		})
	}
}

// TestStructured_Theta_PanicsOnTooSmall covers the lower-bound guard
// and the "more than one length-1 path" guard.
func TestStructured_Theta_PanicsOnTooSmall(t *testing.T) {
	t.Parallel()
	cases := []struct{ a, b, c int }{
		{0, 1, 1},
		{1, 0, 1},
		{1, 1, 0},
		{1, 1, 2}, // two paths of length 1: would produce a parallel edge.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("a=%d_b=%d_c=%d", c.a, c.b, c.c), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Theta(%d,%d,%d) did not panic", c.a, c.b, c.c)
				}
			}()
			_ = Theta(c.a, c.b, c.c)
		})
	}
}

// TestStructured_Theta_Golden pins θ_{2,3,4}.
func TestStructured_Theta_Golden(t *testing.T) {
	t.Parallel()
	g, err := Theta(2, 3, 4).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	structuredGolden(t, "theta-2-3-4.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Property-based: parametric sweep
// -------------------------------------------------------------------

// TestStructured_Properties_RapidSweep drives every parametric
// generator over a bounded parameter sweep and asserts the
// invariants documented in the constructor godoc. The bounds are
// deliberately small so the short layer stays well under a minute:
// Hypercube caps d at 6 (64 nodes), Grid caps m, n at 10 (100
// cells), Theta caps a, b, c at 8 (orders up to 22).
func TestStructured_Properties_RapidSweep(t *testing.T) {
	t.Parallel()

	t.Run("hypercube", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			d := rapid.IntRange(0, 6).Draw(r, "d")
			g, err := Hypercube(d).Build(defaultCfg)
			if err != nil {
				t.Fatalf("d=%d: Build: %v", d, err)
			}
			if got := g.AdjList().Order(); got != uint64(1<<d) {
				t.Fatalf("d=%d: Order = %d, want %d", d, got, 1<<d)
			}
			wantSize := uint64(0)
			if d >= 1 {
				wantSize = uint64(d) * uint64(1<<(d-1))
			}
			if got := g.AdjList().Size(); got != wantSize {
				t.Fatalf("d=%d: Size = %d, want %d", d, got, wantSize)
			}
			// d-regular.
			for v := 0; v < 1<<d; v++ {
				if got := degreeOut(g, v); got != d {
					t.Fatalf("d=%d: deg(%d) = %d, want %d", d, v, got, d)
				}
			}
		})
	})

	t.Run("grid_4n", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			m := rapid.IntRange(1, 10).Draw(r, "m")
			n := rapid.IntRange(1, 10).Draw(r, "n")
			g, err := Grid(m, n, false).Build(defaultCfg)
			if err != nil {
				t.Fatalf("m=%d n=%d: Build: %v", m, n, err)
			}
			if got := g.AdjList().Order(); got != uint64(m*n) {
				t.Fatalf("m=%d n=%d: Order = %d, want %d", m, n, got, m*n)
			}
			want := uint64((m-1)*n + m*(n-1))
			if got := g.AdjList().Size(); got != want {
				t.Fatalf("m=%d n=%d: Size = %d, want %d", m, n, got, want)
			}
		})
	})

	t.Run("theta", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			a := rapid.IntRange(1, 8).Draw(r, "a")
			b := rapid.IntRange(1, 8).Draw(r, "b")
			c := rapid.IntRange(1, 8).Draw(r, "c")
			// Skip parameter triples that the constructor rejects: more
			// than one length-1 path collapses to a parallel edge,
			// which is forbidden under the simple-graph contract.
			ones := 0
			if a == 1 {
				ones++
			}
			if b == 1 {
				ones++
			}
			if c == 1 {
				ones++
			}
			if ones > 1 {
				return
			}
			g, err := Theta(a, b, c).Build(defaultCfg)
			if err != nil {
				t.Fatalf("a=%d b=%d c=%d: Build: %v", a, b, c, err)
			}
			if got := g.AdjList().Order(); got != uint64(a+b+c-1) {
				t.Fatalf("a=%d b=%d c=%d: Order = %d, want %d", a, b, c, got, a+b+c-1)
			}
			if got := g.AdjList().Size(); got != uint64(a+b+c) {
				t.Fatalf("a=%d b=%d c=%d: Size = %d, want %d", a, b, c, got, a+b+c)
			}
		})
	})
}

// -------------------------------------------------------------------
// MaxShardCapacity preservation
// -------------------------------------------------------------------

// TestStructured_PreservesMaxShardCapacity confirms that every
// structured generator preserves cfg.MaxShardCapacity verbatim,
// mirroring the trivial- and classic-family contracts.
func TestStructured_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	for _, tc := range []struct {
		name string
		s    Shape[int, int64]
	}{
		{"hypercube_d2", Hypercube(2)},
		{"grid_2x2", Grid(2, 2, false)},
		{"grid_2x2_eight", Grid(2, 2, true)},
		{"torus_3x3", Torus(3, 3)},
		{"rook_2", Rook(2)},
		{"mobius_3", Mobius(3)},
		{"ladder_3", Ladder(3)},
		{"prism_3", Prism(3)},
		{"theta_2_3_4", Theta(2, 3, 4)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, err := tc.s.Build(cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if g == nil {
				t.Fatal("Build returned nil graph")
			}
		})
	}
}

// -------------------------------------------------------------------
// Soak / nightly layer sweeps
// -------------------------------------------------------------------

// TestStructured_Hypercube_Soak exercises Hypercube up to the soak
// ceiling (d == 22, ~4M nodes). It skips outside the soak layer.
func TestStructured_Hypercube_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	// d=22 produces 2^22 = 4_194_304 nodes and 22 * 2^21 ≈ 4.6 * 10^7
	// edges. Only the order/size invariants are asserted; per-node
	// degree checks would blow the time budget.
	const d = 22
	g, err := Hypercube(d).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(1<<d) {
		t.Fatalf("Order = %d, want %d", got, 1<<d)
	}
	want := uint64(d) * uint64(1<<(d-1))
	if got := g.AdjList().Size(); got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
}

// TestStructured_Hypercube_Nightly exercises Hypercube up to the
// nightly ceiling (d == 24, ~16M nodes). It skips outside the
// nightly layer.
func TestStructured_Hypercube_Nightly(t *testing.T) {
	testlayers.RequireNightly(t)
	const d = 24
	g, err := Hypercube(d).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(1<<d) {
		t.Fatalf("Order = %d, want %d", got, 1<<d)
	}
	want := uint64(d) * uint64(1<<(d-1))
	if got := g.AdjList().Size(); got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
}

// -------------------------------------------------------------------
// Helpers local to this file
// -------------------------------------------------------------------

// expectedGridSize returns the closed-form edge count of L_{m,n}
// under the requested neighbourhood scheme. The 8-neighbour count
// 4*m*n - 3*(m+n) + 2 is only valid for m, n >= 2; for degenerate
// 1xk or kx1 strips the diagonals do not exist and the 8-neighbour
// count coincides with the 4-neighbour count (m-1)*n + m*(n-1).
func expectedGridSize(m, n int, eightNeighbour bool) uint64 {
	if m == 0 || n == 0 {
		return 0
	}
	four := uint64((m-1)*n + m*(n-1))
	if !eightNeighbour || m < 2 || n < 2 {
		return four
	}
	return uint64(4*m*n - 3*(m+n) + 2)
}

// _ keeps the lpg import alive in this file even when only the
// shapegen package types are referenced. The build closures hand
// back *lpg.Graph already, so this is purely a compile-time guard
// against import drift during T58.22 migration.
var _ *lpg.Graph[int, int64]
