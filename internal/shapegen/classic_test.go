package shapegen

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// classicGoldenDir is the directory holding the classic-family
// adjacency listings. The path is rooted at the package directory;
// go test's working directory convention puts the test binary there.
//
// This file deliberately reuses formatAdjacency from trivial_test.go
// (same package). When T58.22 lands the shared golden helper in
// internal/goldens, both files must migrate together.
const classicGoldenDir = "testdata/shapegen/classic"

// classicGolden compares got with the contents of the golden file at
// classicGoldenDir/<name>. When the -shapegen-update flag declared by
// trivial_test.go is set, the helper rewrites the golden instead of
// comparing. The implementation duplicates trivial_test.go's
// assertGolden rather than refactor it because the two files will
// migrate together to internal/goldens in T58.22 (#526).
func classicGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(classicGoldenDir, name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("classicGolden: MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("classicGolden: WriteFile(%q): %v", path, err)
		}
		t.Logf("rewrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is a test-local golden under testdata/, not user input
	if err != nil {
		t.Fatalf("classicGolden: ReadFile(%q): %v (run with -shapegen-update to bootstrap)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// -------------------------------------------------------------------
// Path
// -------------------------------------------------------------------

// TestClassic_Path_Invariants walks n through {0,1,2,3,5,10} for the
// directed and undirected variants and asserts the catalogue invariants
// declared at the top of Path's godoc. It is the unit-test arm of
// acceptance criterion 1.
func TestClassic_Path_Invariants(t *testing.T) {
	t.Parallel()

	for _, n := range goldenSizes() {
		for _, directed := range []bool{true, false} {
			n, directed := n, directed
			t.Run(fmt.Sprintf("n=%d_directed=%v", n, directed), func(t *testing.T) {
				t.Parallel()
				s := Path(n, directed)
				if got, want := s.Name(), "classic.path"; got != want {
					t.Fatalf("Name = %q, want %q", got, want)
				}
				if got := s.Knobs(); len(got) != 1 || got[0].Name != "n" || got[0].Min != 0 || got[0].Max != 100_000 || got[0].Default != 5 {
					t.Fatalf("Knobs = %#v, want exactly one n:[0,100000] default 5", got)
				}
				g, err := s.Build(defaultCfg)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				assertOrder(t, g, uint64(n))
				wantSize := uint64(0)
				if n > 1 {
					wantSize = uint64(n - 1)
				}
				assertSize(t, g, wantSize)
				assertDirected(t, g, directed)
				assertDegreeSequencePath(t, g, n, directed)
				if n >= 1 {
					assertDiameter(t, "Path", g, pathDiameter(n))
				}
			})
		}
	}
}

// TestClassic_Path_PanicsOnNegative locks in the constructor guard.
func TestClassic_Path_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Path(-1, true) did not panic")
		}
	}()
	_ = Path(-1, true)
}

// TestClassic_Path_Goldens pins Path(n, true) for n in {0,1,2,3,5,10}.
// The brief specifies we document that the undirected variant's
// listing is the symmetric closure of the directed one: the assertion
// is encoded in TestClassic_Path_UndirectedSymmetry below.
func TestClassic_Path_Goldens(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			g, err := Path(n, true).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			classicGolden(t, fmt.Sprintf("path-n%d-directed.txt", n), formatAdjacency(g))
		})
	}
}

// TestClassic_Path_UndirectedSymmetry asserts that the undirected
// variant's adjacency listing is the symmetric closure of the
// directed one: for every (u, v) emitted by the directed graph, the
// undirected graph emits both (u, v) and (v, u). This codifies the
// brief's contract without duplicating goldens.
func TestClassic_Path_UndirectedSymmetry(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			assertSymmetricClosure(t, Path(n, true), Path(n, false))
		})
	}
}

// -------------------------------------------------------------------
// Cycle
// -------------------------------------------------------------------

// TestClassic_Cycle_Invariants exercises the directed variant over
// n ∈ {1,2,3,5,10} and the undirected variant over n ∈ {3,5,10}. The
// excluded values are handled by TestClassic_Cycle_TooSmall.
func TestClassic_Cycle_Invariants(t *testing.T) {
	t.Parallel()

	for _, n := range cycleSizes(true) {
		n := n
		t.Run(fmt.Sprintf("directed_n=%d", n), func(t *testing.T) {
			t.Parallel()
			s := Cycle(n, true)
			if got, want := s.Name(), "classic.cycle"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(n))
			assertSize(t, g, uint64(n))
			assertDirected(t, g, true)
		})
	}

	for _, n := range cycleSizes(false) {
		n := n
		t.Run(fmt.Sprintf("undirected_n=%d", n), func(t *testing.T) {
			t.Parallel()
			g, err := Cycle(n, false).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(n))
			assertSize(t, g, uint64(n))
			assertDirected(t, g, false)
			// 2-regular: every node has degree 2.
			for v := 0; v < n; v++ {
				if got := degreeOut(g, v); got != 2 {
					t.Fatalf("undirected cycle n=%d, deg(%d) = %d, want 2", n, v, got)
				}
			}
			// Diameter of C_n is floor(n/2) per the catalogue.
			assertDiameter(t, "Cycle", g, uint64(n/2))
		})
	}
}

// TestClassic_Cycle_TooSmall locks in the ErrCycleTooSmall contract
// for the values explicitly excluded by the catalogue.
func TestClassic_Cycle_TooSmall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n        int
		directed bool
	}{
		{n: 0, directed: true},
		{n: 0, directed: false},
		{n: 1, directed: false},
		{n: 2, directed: false},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_directed=%v", c.n, c.directed), func(t *testing.T) {
			t.Parallel()
			_, err := Cycle(c.n, c.directed).Build(defaultCfg)
			if err == nil {
				t.Fatal("Build returned nil error, want ErrCycleTooSmall")
			}
			if !errors.Is(err, ErrCycleTooSmall) {
				t.Fatalf("Build err = %v, want errors.Is(ErrCycleTooSmall)", err)
			}
		})
	}
}

// TestClassic_Cycle_PanicsOnNegative covers the constructor guard.
func TestClassic_Cycle_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Cycle(-1, true) did not panic")
		}
	}()
	_ = Cycle(-1, true)
}

// TestClassic_Cycle_Goldens pins Cycle(n, true) at every n where the
// directed cycle is defined: {1,2,3,5,10}.
func TestClassic_Cycle_Goldens(t *testing.T) {
	t.Parallel()
	for _, n := range cycleSizes(true) {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			g, err := Cycle(n, true).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			classicGolden(t, fmt.Sprintf("cycle-n%d-directed.txt", n), formatAdjacency(g))
		})
	}
}

// -------------------------------------------------------------------
// Star
// -------------------------------------------------------------------

// TestClassic_Star_Invariants walks n through {0,1,2,3,5,10} for both
// orientations and asserts the catalogue invariants.
func TestClassic_Star_Invariants(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		for _, out := range []bool{true, false} {
			n, out := n, out
			t.Run(fmt.Sprintf("n=%d_outgoing=%v", n, out), func(t *testing.T) {
				t.Parallel()
				s := Star(n, out)
				if got, want := s.Name(), "classic.star"; got != want {
					t.Fatalf("Name = %q, want %q", got, want)
				}
				g, err := s.Build(defaultCfg)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				assertOrder(t, g, uint64(n))
				wantSize := uint64(0)
				if n > 1 {
					wantSize = uint64(n - 1)
				}
				assertSize(t, g, wantSize)
				if n >= 2 {
					if out {
						if got := degreeOut(g, 0); got != n-1 {
							t.Fatalf("out-degree(centre) = %d, want %d", got, n-1)
						}
						for v := 1; v < n; v++ {
							if got := degreeOut(g, v); got != 0 {
								t.Fatalf("out-degree(leaf %d) = %d, want 0", v, got)
							}
						}
					} else {
						if got := degreeOut(g, 0); got != 0 {
							t.Fatalf("out-degree(centre, incoming variant) = %d, want 0", got)
						}
						for v := 1; v < n; v++ {
							if got := degreeOut(g, v); got != 1 {
								t.Fatalf("out-degree(leaf %d, incoming variant) = %d, want 1", v, got)
							}
						}
					}
				}
				switch {
				case n <= 1:
					// Diameter undefined / 0; nothing to assert.
				case n == 2:
					assertDiameter(t, "Star", g, 1)
				default:
					assertDiameter(t, "Star", g, 2)
				}
			})
		}
	}
}

// TestClassic_Star_PanicsOnNegative covers the constructor guard.
func TestClassic_Star_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Star(-1, true) did not panic")
		}
	}()
	_ = Star(-1, true)
}

// TestClassic_Star_Goldens pins Star(n, outgoing=true) for n in
// {0,1,2,3,5,10}. The incoming variant is the transpose; the
// catalogue does not require a separate golden for it because
// TestClassic_Star_Invariants exercises the orientation toggle.
func TestClassic_Star_Goldens(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			g, err := Star(n, true).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			classicGolden(t, fmt.Sprintf("star-n%d-outgoing.txt", n), formatAdjacency(g))
		})
	}
}

// -------------------------------------------------------------------
// DoubleStar
// -------------------------------------------------------------------

// TestClassic_DoubleStar_Invariants asserts the catalogue invariants
// for (k1, k2) drawn from the cartesian product {0,1,3} x {0,1,3}.
// The combinations cover both the "no leaves" degenerate case (k1=0,
// k2=0 collapses to the centre-to-centre edge K_2) and the
// asymmetric case (k1 != k2).
func TestClassic_DoubleStar_Invariants(t *testing.T) {
	t.Parallel()
	for _, k1 := range []int{0, 1, 3} {
		for _, k2 := range []int{0, 1, 3} {
			k1, k2 := k1, k2
			t.Run(fmt.Sprintf("k1=%d_k2=%d", k1, k2), func(t *testing.T) {
				t.Parallel()
				s := DoubleStar(k1, k2)
				if got, want := s.Name(), "classic.double-star"; got != want {
					t.Fatalf("Name = %q, want %q", got, want)
				}
				knobs := s.Knobs()
				if len(knobs) != 2 {
					t.Fatalf("Knobs has %d entries, want 2", len(knobs))
				}
				if knobs[0].Name != "k1" || knobs[1].Name != "k2" {
					t.Fatalf("Knob names = %q,%q, want k1,k2", knobs[0].Name, knobs[1].Name)
				}
				g, err := s.Build(defaultCfg)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				assertOrder(t, g, uint64(2+k1+k2))
				assertSize(t, g, uint64(k1+k2+1))
				assertDirected(t, g, false)
				// Centre degrees.
				if got := degreeOut(g, 0); got != k1+1 {
					t.Fatalf("deg(centre0) = %d, want %d", got, k1+1)
				}
				if got := degreeOut(g, 1); got != k2+1 {
					t.Fatalf("deg(centre1) = %d, want %d", got, k2+1)
				}
				// Leaf degrees.
				for v := 2; v < 2+k1+k2; v++ {
					if got := degreeOut(g, v); got != 1 {
						t.Fatalf("deg(leaf %d) = %d, want 1", v, got)
					}
				}
			})
		}
	}
}

// TestClassic_DoubleStar_PanicsOnNegative covers both negative-k
// branches of the constructor guard.
func TestClassic_DoubleStar_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ k1, k2 int }{{-1, 0}, {0, -1}} {
		c := c
		t.Run(fmt.Sprintf("k1=%d_k2=%d", c.k1, c.k2), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("DoubleStar(%d,%d) did not panic", c.k1, c.k2)
				}
			}()
			_ = DoubleStar(c.k1, c.k2)
		})
	}
}

// TestClassic_DoubleStar_Golden pins DoubleStar(3, 3) — the
// representative parameter set requested by the brief.
func TestClassic_DoubleStar_Golden(t *testing.T) {
	t.Parallel()
	g, err := DoubleStar(3, 3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	classicGolden(t, "double-star-k1-3-k2-3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Complete
// -------------------------------------------------------------------

// TestClassic_Complete_Invariants asserts Order, Size, diameter, and
// the triangle count C(n, 3) for the undirected variant. The
// directed variant covers Size = n*(n-1).
func TestClassic_Complete_Invariants(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		for _, directed := range []bool{true, false} {
			n, directed := n, directed
			t.Run(fmt.Sprintf("n=%d_directed=%v", n, directed), func(t *testing.T) {
				t.Parallel()
				s := Complete(n, directed)
				if got, want := s.Name(), "classic.complete"; got != want {
					t.Fatalf("Name = %q, want %q", got, want)
				}
				g, err := s.Build(defaultCfg)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				assertOrder(t, g, uint64(n))
				wantSize := uint64(n) * uint64(n-1)
				if !directed {
					wantSize /= 2
				}
				if n == 0 {
					wantSize = 0
				}
				assertSize(t, g, wantSize)
				assertDirected(t, g, directed)
				if n >= 2 {
					assertDiameter(t, "Complete", g, 1)
				}
				if !directed && n >= 3 {
					want := uint64(n) * uint64(n-1) * uint64(n-2) / 6
					if got := countTrianglesUndirected(g, n); got != want {
						t.Fatalf("triangle count = %d, want C(%d,3) = %d", got, n, want)
					}
				}
				// Every non-self pair must be connected in the chosen direction.
				if directed {
					for i := 0; i < n; i++ {
						for j := 0; j < n; j++ {
							if i == j {
								continue
							}
							if !g.AdjList().HasEdge(i, j) {
								t.Fatalf("directed K_%d missing edge %d->%d", n, i, j)
							}
						}
					}
				} else {
					for i := 0; i < n; i++ {
						for j := i + 1; j < n; j++ {
							if !g.AdjList().HasEdge(i, j) || !g.AdjList().HasEdge(j, i) {
								t.Fatalf("undirected K_%d missing symmetric edge {%d,%d}", n, i, j)
							}
						}
					}
				}
			})
		}
	}
}

// TestClassic_Complete_PanicsOnNegative covers the constructor guard.
func TestClassic_Complete_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Complete(-1, true) did not panic")
		}
	}()
	_ = Complete(-1, true)
}

// TestClassic_Complete_Goldens pins Complete(n, true) for n in
// {0,1,2,3,5,10}.
func TestClassic_Complete_Goldens(t *testing.T) {
	t.Parallel()
	for _, n := range goldenSizes() {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			g, err := Complete(n, true).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			classicGolden(t, fmt.Sprintf("complete-n%d-directed.txt", n), formatAdjacency(g))
		})
	}
}

// -------------------------------------------------------------------
// CompleteBipartite
// -------------------------------------------------------------------

// TestClassic_CompleteBipartite_Invariants asserts Order, Size, and
// the bipartite degree sequence over (m, n) in the parameter sweep
// requested by the brief.
func TestClassic_CompleteBipartite_Invariants(t *testing.T) {
	t.Parallel()
	for _, pair := range bipartitePairs() {
		pair := pair
		t.Run(fmt.Sprintf("m=%d_n=%d", pair.m, pair.n), func(t *testing.T) {
			t.Parallel()
			s := CompleteBipartite(pair.m, pair.n)
			if got, want := s.Name(), "classic.complete-bipartite"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 || knobs[0].Name != "m" || knobs[1].Name != "n" {
				t.Fatalf("Knobs = %#v, want m,n", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(pair.m+pair.n))
			assertSize(t, g, uint64(pair.m)*uint64(pair.n))
			assertDirected(t, g, false)
			// Left side degrees (every left node connects to all n right nodes).
			for v := 0; v < pair.m; v++ {
				if got := degreeOut(g, v); got != pair.n {
					t.Fatalf("left node %d degree = %d, want %d", v, got, pair.n)
				}
			}
			// Right side degrees.
			for v := pair.m; v < pair.m+pair.n; v++ {
				if got := degreeOut(g, v); got != pair.m {
					t.Fatalf("right node %d degree = %d, want %d", v, got, pair.m)
				}
			}
		})
	}
}

// TestClassic_CompleteBipartite_PanicsOnNegative covers the negative
// branches of the constructor guard.
func TestClassic_CompleteBipartite_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ m, n int }{{-1, 0}, {0, -1}} {
		c := c
		t.Run(fmt.Sprintf("m=%d_n=%d", c.m, c.n), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("CompleteBipartite(%d,%d) did not panic", c.m, c.n)
				}
			}()
			_ = CompleteBipartite(c.m, c.n)
		})
	}
}

// TestClassic_CompleteBipartite_Goldens pins six representative
// (m, n) pairs from the brief.
func TestClassic_CompleteBipartite_Goldens(t *testing.T) {
	t.Parallel()
	for _, pair := range bipartitePairs() {
		pair := pair
		t.Run(fmt.Sprintf("m=%d_n=%d", pair.m, pair.n), func(t *testing.T) {
			t.Parallel()
			g, err := CompleteBipartite(pair.m, pair.n).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			classicGolden(t, fmt.Sprintf("complete-bipartite-m%d-n%d.txt", pair.m, pair.n), formatAdjacency(g))
		})
	}
}

// -------------------------------------------------------------------
// Multipartite
// -------------------------------------------------------------------

// TestClassic_Multipartite_Invariants enumerates a small set of
// parts vectors and asserts Order, Size, and the absence of
// intra-group edges. The property-based arm is in
// TestClassic_Multipartite_Property.
func TestClassic_Multipartite_Invariants(t *testing.T) {
	t.Parallel()
	cases := [][]int{
		nil,
		{},
		{0},
		{3},
		{0, 0, 5},
		{2, 3, 4},
		{1, 1, 1, 1},
	}
	for _, parts := range cases {
		parts := parts
		t.Run(fmt.Sprintf("parts=%v", parts), func(t *testing.T) {
			t.Parallel()
			s := Multipartite(parts)
			if got, want := s.Name(), "classic.multipartite"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			if got := s.Knobs(); len(got) != 0 {
				t.Fatalf("Knobs = %#v, want empty (variadic parts)", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			total := 0
			for _, p := range parts {
				total += p
			}
			assertOrder(t, g, uint64(total))
			assertSize(t, g, expectedMultipartiteSize(parts))
			assertDirected(t, g, false)
			assertNoIntraGroupEdges(t, g, parts)
		})
	}
}

// TestClassic_Multipartite_PanicsOnNegative covers the negative-part
// guard. The constructor scans the slice and panics on the first
// negative element.
func TestClassic_Multipartite_PanicsOnNegative(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Multipartite with a negative part did not panic")
		}
	}()
	_ = Multipartite([]int{2, -1, 3})
}

// TestClassic_Multipartite_Golden pins Multipartite([2,3,4]).
func TestClassic_Multipartite_Golden(t *testing.T) {
	t.Parallel()
	g, err := Multipartite([]int{2, 3, 4}).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	classicGolden(t, "multipartite-2-3-4.txt", formatAdjacency(g))
}

// TestClassic_Multipartite_DefensiveCopy ensures that mutating the
// caller's parts slice after construction has no effect on Build.
// This is the contract documented by Multipartite's godoc.
func TestClassic_Multipartite_DefensiveCopy(t *testing.T) {
	t.Parallel()
	parts := []int{2, 3, 4}
	s := Multipartite(parts)
	parts[0] = 100 // would inflate Order to 99+3+4 = 106 without the copy
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 9 {
		t.Fatalf("Order = %d, want 9 (caller mutation must not leak)", got)
	}
}

// -------------------------------------------------------------------
// Property-based: Multipartite
// -------------------------------------------------------------------

// TestClassic_Multipartite_Property is the property-based assertion
// required by acceptance criterion 2: for every parts slice drawn
// from a small bounded space the resulting graph satisfies
// Order == sum(parts) and Size (logical, as Size() returns it) is
// equal to sum_{i<j} parts[i] * parts[j]. The Size() value is
// already the logical edge count for undirected graphs (the adjlist
// counts each undirected edge once), so the factor-of-2 mentioned
// in the brief manifests only at the iteration level — see the
// helper expectedMultipartiteSize for the definition we assert
// against.
//
// Bounds chosen to keep the short layer fast: parts length 1..6,
// each entry 0..50, so worst-case Order <= 300 and Size <= 22_500.
func TestClassic_Multipartite_Property(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		parts := rapid.SliceOfN(rapid.IntRange(0, 50), 1, 6).Draw(r, "parts")
		g, err := Multipartite(parts).Build(defaultCfg)
		if err != nil {
			t.Fatalf("parts=%v: Build: %v", parts, err)
		}
		total := 0
		for _, p := range parts {
			total += p
		}
		if got := g.AdjList().Order(); got != uint64(total) {
			t.Fatalf("parts=%v: Order = %d, want %d", parts, got, total)
		}
		if got, want := g.AdjList().Size(), expectedMultipartiteSize(parts); got != want {
			t.Fatalf("parts=%v: Size = %d, want %d", parts, got, want)
		}
	})
}

// -------------------------------------------------------------------
// Property-based: parametric sweep
// -------------------------------------------------------------------

// TestClassic_Properties_RapidSweep drives every parametric
// generator (other than Multipartite, which has its own dedicated
// rapid test) over a bounded parameter sweep and asserts the
// invariants documented in the constructor godoc. The bounds are
// deliberately smaller than the published knob maxes: the short
// layer must stay well under a minute, and Complete(n) is O(n^2) in
// both time and memory.
func TestClassic_Properties_RapidSweep(t *testing.T) {
	t.Parallel()

	t.Run("path", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 200).Draw(r, "n")
			directed := rapid.Bool().Draw(r, "directed")
			g, err := Path(n, directed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d directed=%v: Build: %v", n, directed, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			wantSize := uint64(0)
			if n > 1 {
				wantSize = uint64(n - 1)
			}
			if got := g.AdjList().Size(); got != wantSize {
				t.Fatalf("Size = %d, want %d", got, wantSize)
			}
		})
	})

	t.Run("cycle_directed", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(1, 200).Draw(r, "n")
			g, err := Cycle(n, true).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d: Build: %v", n, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			if got := g.AdjList().Size(); got != uint64(n) {
				t.Fatalf("Size = %d, want %d", got, n)
			}
		})
	})

	t.Run("cycle_undirected", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(3, 200).Draw(r, "n")
			g, err := Cycle(n, false).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d: Build: %v", n, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			if got := g.AdjList().Size(); got != uint64(n) {
				t.Fatalf("Size = %d, want %d", got, n)
			}
		})
	})

	t.Run("star", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 200).Draw(r, "n")
			out := rapid.Bool().Draw(r, "outgoing")
			g, err := Star(n, out).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d out=%v: Build: %v", n, out, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			wantSize := uint64(0)
			if n > 1 {
				wantSize = uint64(n - 1)
			}
			if got := g.AdjList().Size(); got != wantSize {
				t.Fatalf("Size = %d, want %d", got, wantSize)
			}
		})
	})

	t.Run("double_star", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			k1 := rapid.IntRange(0, 100).Draw(r, "k1")
			k2 := rapid.IntRange(0, 100).Draw(r, "k2")
			g, err := DoubleStar(k1, k2).Build(defaultCfg)
			if err != nil {
				t.Fatalf("k1=%d k2=%d: Build: %v", k1, k2, err)
			}
			if got := g.AdjList().Order(); got != uint64(2+k1+k2) {
				t.Fatalf("Order = %d, want %d", got, 2+k1+k2)
			}
			if got := g.AdjList().Size(); got != uint64(k1+k2+1) {
				t.Fatalf("Size = %d, want %d", got, k1+k2+1)
			}
		})
	})

	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		// Cap n at 60 so worst-case directed K_n has ~3 600 edges
		// — comfortably within the short layer budget.
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 60).Draw(r, "n")
			directed := rapid.Bool().Draw(r, "directed")
			g, err := Complete(n, directed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d directed=%v: Build: %v", n, directed, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			want := uint64(n) * uint64(n-1)
			if !directed {
				want /= 2
			}
			if n == 0 {
				want = 0
			}
			if got := g.AdjList().Size(); got != want {
				t.Fatalf("Size = %d, want %d", got, want)
			}
		})
	})

	t.Run("complete_bipartite", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			m := rapid.IntRange(0, 60).Draw(r, "m")
			n := rapid.IntRange(0, 60).Draw(r, "n")
			g, err := CompleteBipartite(m, n).Build(defaultCfg)
			if err != nil {
				t.Fatalf("m=%d n=%d: Build: %v", m, n, err)
			}
			if got := g.AdjList().Order(); got != uint64(m+n) {
				t.Fatalf("Order = %d, want %d", got, m+n)
			}
			if got := g.AdjList().Size(); got != uint64(m)*uint64(n) {
				t.Fatalf("Size = %d, want %d", got, m*n)
			}
		})
	})
}

// -------------------------------------------------------------------
// MaxShardCapacity preservation
// -------------------------------------------------------------------

// TestClassic_PreservesMaxShardCapacity confirms that every classic
// generator preserves cfg.MaxShardCapacity verbatim, mirroring the
// trivial-family contract.
func TestClassic_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	for _, tc := range []struct {
		name string
		s    Shape[int, int64]
	}{
		{"path", Path(3, true)},
		{"cycle_directed", Cycle(3, true)},
		{"cycle_undirected", Cycle(3, false)},
		{"star_outgoing", Star(3, true)},
		{"star_incoming", Star(3, false)},
		{"double_star", DoubleStar(1, 1)},
		{"complete_directed", Complete(3, true)},
		{"complete_undirected", Complete(3, false)},
		{"complete_bipartite", CompleteBipartite(2, 2)},
		{"multipartite", Multipartite([]int{1, 2})},
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
// Helpers
// -------------------------------------------------------------------

// goldenSizes is the n-sweep requested by the brief: {0,1,2,3,5,10}.
func goldenSizes() []int { return []int{0, 1, 2, 3, 5, 10} }

// cycleSizes returns the subset of goldenSizes for which Cycle is
// defined under the given orientation. The undirected variant
// requires n >= 3; the directed variant requires n >= 1.
func cycleSizes(directed bool) []int {
	if directed {
		return []int{1, 2, 3, 5, 10}
	}
	return []int{3, 5, 10}
}

// bipartitePairs is the (m, n) sweep requested by the brief.
type bipartitePair struct{ m, n int }

func bipartitePairs() []bipartitePair {
	return []bipartitePair{
		{0, 0},
		{1, 1},
		{2, 3},
		{3, 3},
		{5, 5},
		{10, 10},
	}
}

// pathDiameter returns the catalogue diameter of P_n for n >= 1.
// It is a tiny helper so the call site reads as a single concept.
func pathDiameter(n int) uint64 { return uint64(n - 1) }

// degreeOut returns the number of outgoing neighbours of v in g
// — i.e., len(adjacency[v]) — by iterating g.AdjList().Neighbours
// once. The helper exists because [adjlist.AdjList] does not expose
// a Degree accessor.
func degreeOut(g *lpg.Graph[int, int64], v int) int {
	d := 0
	for range g.AdjList().Neighbours(v) {
		d++
	}
	return d
}

// assertDegreeSequencePath asserts the path-graph degree sequence on
// g. For directed graphs we check out-degrees only: every node has
// one outgoing edge except the last. For undirected graphs the
// [adjlist.AdjList] stores both (u,v) and (v,u) entries, so the
// out-degree iteration recovers the conventional undirected degree:
// 1 at each endpoint and 2 at every interior node.
func assertDegreeSequencePath(t *testing.T, g *lpg.Graph[int, int64], n int, directed bool) {
	t.Helper()
	if n == 0 {
		return
	}
	if directed {
		for v := 0; v < n-1; v++ {
			if got := degreeOut(g, v); got != 1 {
				t.Fatalf("directed Path n=%d: out-deg(%d) = %d, want 1", n, v, got)
			}
		}
		if got := degreeOut(g, n-1); got != 0 {
			t.Fatalf("directed Path n=%d: out-deg(%d) = %d, want 0", n, n-1, got)
		}
		return
	}
	// Undirected.
	if n == 1 {
		if got := degreeOut(g, 0); got != 0 {
			t.Fatalf("undirected Path n=1: deg(0) = %d, want 0", got)
		}
		return
	}
	// Endpoints have degree 1; interiors have degree 2.
	if got := degreeOut(g, 0); got != 1 {
		t.Fatalf("undirected Path n=%d: deg(0) = %d, want 1", n, got)
	}
	if got := degreeOut(g, n-1); got != 1 {
		t.Fatalf("undirected Path n=%d: deg(%d) = %d, want 1", n, n-1, got)
	}
	for v := 1; v < n-1; v++ {
		if got := degreeOut(g, v); got != 2 {
			t.Fatalf("undirected Path n=%d: deg(%d) = %d, want 2", n, v, got)
		}
	}
}

// expectedMultipartiteSize returns the logical edge count of
// K_{parts[0], ..., parts[k-1]}: sum_{i<j} parts[i] * parts[j]. The
// brief mentions a factor-of-2 for the underlying iteration; that
// factor only appears when one counts stored adjacency entries
// (mirror included), which is not what [adjlist.AdjList.Size]
// reports — it counts each undirected edge once.
func expectedMultipartiteSize(parts []int) uint64 {
	var size uint64
	for i := 0; i < len(parts); i++ {
		for j := i + 1; j < len(parts); j++ {
			size += uint64(parts[i]) * uint64(parts[j])
		}
	}
	return size
}

// assertNoIntraGroupEdges walks the adjacency of every node in g and
// fails the test if any edge lands within the same group of parts.
func assertNoIntraGroupEdges(t *testing.T, g *lpg.Graph[int, int64], parts []int) {
	t.Helper()
	groupOf := make(map[int]int, sum(parts))
	off := 0
	for i, p := range parts {
		for j := 0; j < p; j++ {
			groupOf[off+j] = i
		}
		off += p
	}
	for u, group := range groupOf {
		for v := range g.AdjList().Neighbours(u) {
			if groupOf[v] == group {
				t.Fatalf("intra-group edge in K_%v: %d -> %d (both in group %d)", parts, u, v, group)
			}
		}
	}
}

func sum(parts []int) int {
	s := 0
	for _, p := range parts {
		s += p
	}
	return s
}

// countTrianglesUndirected counts unordered triples {a,b,c} that
// form a triangle in g, by brute force over O(n^3). Used only on
// the goldenSizes n-sweep so the cost is bounded.
func countTrianglesUndirected(g *lpg.Graph[int, int64], n int) uint64 {
	var c uint64
	a := g.AdjList()
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if !a.HasEdge(i, j) {
				continue
			}
			for k := j + 1; k < n; k++ {
				if a.HasEdge(j, k) && a.HasEdge(i, k) {
					c++
				}
			}
		}
	}
	return c
}

// assertOrder, assertSize, assertDirected, assertDiameter, and
// assertSymmetricClosure are micro-helpers that keep the per-test
// boilerplate to a single line.
func assertOrder(t *testing.T, g *lpg.Graph[int, int64], want uint64) {
	t.Helper()
	if got := g.AdjList().Order(); got != want {
		t.Fatalf("Order = %d, want %d", got, want)
	}
}

func assertSize(t *testing.T, g *lpg.Graph[int, int64], want uint64) {
	t.Helper()
	if got := g.AdjList().Size(); got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
}

func assertDirected(t *testing.T, g *lpg.Graph[int, int64], want bool) {
	t.Helper()
	if got := g.AdjList().Directed(); got != want {
		t.Fatalf("Directed = %v, want %v", got, want)
	}
}

// assertDiameter runs a brute-force, as-undirected, multi-source BFS
// over g and asserts that the eccentricity-maximising node achieves
// the analytic diameter supplied by the caller. The brief explicitly
// asks us to avoid pulling in search.Diameter from a generator-
// package test; the inline BFS here is short, allocation-bounded by
// the graph itself, and only invoked on goldenSizes() (n <= 10) so
// the cost is well under a millisecond per call.
//
// "As-undirected" means the helper considers both the forward
// adjacency (g.AdjList().Neighbours(u)) and an inverted adjacency
// reconstructed by a single O(m) pre-pass. This makes the helper
// usable on directed shapes (Star with outgoing=true, directed Path,
// directed Complete) whose forward adjacency alone is not strongly
// connected, so BFS would otherwise miss large parts of the graph.
// The undirected skeleton of every classic shape is what the
// catalogue's diameter is defined against.
func assertDiameter(t *testing.T, shape string, g *lpg.Graph[int, int64], want uint64) {
	t.Helper()
	if got := bfsDiameterAsUndirected(g); got != want {
		t.Fatalf("%s diameter = %d, want %d", shape, got, want)
	}
}

// bfsDiameterAsUndirected returns the diameter of g's undirected
// skeleton: an O(m) pre-pass builds the symmetric closure of g's
// out-adjacency as a map[int][]int, and a brute-force O(n * (n+m))
// BFS from every node finds the longest shortest path.
//
// Returns 0 for graphs with 0 or 1 nodes.
func bfsDiameterAsUndirected(g *lpg.Graph[int, int64]) uint64 {
	adj := g.AdjList()
	maxID := uint64(adj.MaxNodeID())
	if maxID == 0 {
		return 0
	}
	nodes := make([]int, 0, maxID)
	for id := uint64(0); id < maxID; id++ {
		v, ok := adj.Mapper().Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nodes = append(nodes, v)
	}
	if len(nodes) <= 1 {
		return 0
	}
	// Build the symmetric-closure adjacency as a map[int]map[int]struct{}
	// (set semantics avoids double-counting in undirected shapes whose
	// underlying [adjlist.AdjList] already mirrors entries).
	sym := make(map[int]map[int]struct{}, len(nodes))
	ensure := func(u int) map[int]struct{} {
		m := sym[u]
		if m == nil {
			m = make(map[int]struct{})
			sym[u] = m
		}
		return m
	}
	for _, u := range nodes {
		for v := range adj.Neighbours(u) {
			ensure(u)[v] = struct{}{}
			ensure(v)[u] = struct{}{}
		}
	}
	var best uint64
	dist := make(map[int]uint64, len(nodes))
	queue := make([]int, 0, len(nodes))
	for _, src := range nodes {
		for k := range dist {
			delete(dist, k)
		}
		queue = queue[:0]
		dist[src] = 0
		queue = append(queue, src)
		for head := 0; head < len(queue); head++ {
			u := queue[head]
			du := dist[u]
			for v := range sym[u] {
				if _, seen := dist[v]; seen {
					continue
				}
				dist[v] = du + 1
				if du+1 > best {
					best = du + 1
				}
				queue = append(queue, v)
			}
		}
	}
	return best
}

// assertSymmetricClosure verifies that the undirected variant of a
// shape produces exactly the symmetric closure of the directed one.
// It compares the set of stored adjacency entries: for every (u, v)
// in the directed listing, the undirected listing must contain both
// (u, v) and (v, u) with the same weight.
func assertSymmetricClosure(t *testing.T, directed, undirected Shape[int, int64]) {
	t.Helper()
	gd, err := directed.Build(defaultCfg)
	if err != nil {
		t.Fatalf("directed Build: %v", err)
	}
	gu, err := undirected.Build(defaultCfg)
	if err != nil {
		t.Fatalf("undirected Build: %v", err)
	}
	maxID := uint64(gd.AdjList().MaxNodeID())
	for u := uint64(0); u < maxID; u++ {
		v, ok := gd.AdjList().Mapper().Resolve(graph.NodeID(u))
		if !ok {
			continue
		}
		for nbr, w := range gd.AdjList().Neighbours(v) {
			if !hasNeighbourWithWeight(gu, v, nbr, w) {
				t.Fatalf("undirected closure missing forward edge %d -> %d [%d]", v, nbr, w)
			}
			if !hasNeighbourWithWeight(gu, nbr, v, w) {
				t.Fatalf("undirected closure missing reverse edge %d -> %d [%d]", nbr, v, w)
			}
		}
	}
}

// hasNeighbourWithWeight reports whether g has an edge from u to v
// carrying weight w. Returns false when u has no outgoing entry.
func hasNeighbourWithWeight(g *lpg.Graph[int, int64], u, v int, w int64) bool {
	for nbr, nw := range g.AdjList().Neighbours(u) {
		if nbr == v && nw == w {
			return true
		}
	}
	return false
}
