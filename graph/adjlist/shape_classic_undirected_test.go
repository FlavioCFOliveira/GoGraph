package adjlist_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// degree returns the out-degree of node u in a by counting the
// elements yielded by Neighbours. It is allocation-free beyond the
// iterator closure itself.
func degree(a *adjlist.AdjList[int, int64], u int) int {
	d := 0
	for range a.Neighbours(u) {
		d++
	}
	return d
}

// assertSymmetric verifies mirror symmetry for every edge in a: for
// every (u, v) yielded by Neighbours(u) across all nodes 0..order-1,
// HasEdge(v, u) must be true. It uses integer key iteration directly
// because all classic/structured shapes assign contiguous int keys
// 0..order-1.
func assertSymmetric(t *testing.T, a *adjlist.AdjList[int, int64], order int) {
	t.Helper()
	for u := 0; u < order; u++ {
		for v := range a.Neighbours(u) {
			if !a.HasEdge(v, u) {
				t.Errorf("edge %d->%d present but reverse %d->%d absent", u, v, v, u)
			}
		}
	}
}

// TestAdjList_ClassicUndirected validates the undirected adjacency-list
// backend against classic skeleton shapes from internal/shapegen. The
// primary invariant under test is mirror symmetry: for every (u,v) edge
// inserted, HasEdge(v,u) must also be true. Each sub-test additionally
// asserts Order, Size, and the analytic degree sequence where it has a
// closed-form expression.
func TestAdjList_ClassicUndirected(t *testing.T) {
	t.Parallel()

	// -- Cycle -------------------------------------------------------
	// C_n: Order=n, Size=n, every node degree=2 (n >= 3).
	t.Run("Cycle", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{3, 4, 5, 10} {
			n := n
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.Cycle(n, false).Build(adjlist.Config{})
				if err != nil {
					t.Fatalf("Cycle(%d) Build: %v", n, err)
				}
				a := g.AdjList()

				if got := a.Order(); got != uint64(n) {
					t.Errorf("n=%d: Order = %d, want %d", n, got, n)
				}
				if got := a.Size(); got != uint64(n) {
					t.Errorf("n=%d: Size = %d, want %d", n, got, n)
				}
				assertSymmetric(t, a, n)
				for u := 0; u < n; u++ {
					if got := degree(a, u); got != 2 {
						t.Errorf("n=%d: degree(%d) = %d, want 2", n, u, got)
					}
				}
			})
		}
	})

	// -- Complete ----------------------------------------------------
	// K_n: Order=n, Size=n*(n-1)/2, every node degree=n-1.
	t.Run("Complete", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{1, 2, 3, 5} {
			n := n
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.Complete(n, false).Build(adjlist.Config{})
				if err != nil {
					t.Fatalf("Complete(%d) Build: %v", n, err)
				}
				a := g.AdjList()

				wantOrder := uint64(n)
				wantSize := uint64(n) * uint64(n-1) / 2
				if got := a.Order(); got != wantOrder {
					t.Errorf("n=%d: Order = %d, want %d", n, got, wantOrder)
				}
				if got := a.Size(); got != wantSize {
					t.Errorf("n=%d: Size = %d, want %d", n, got, wantSize)
				}
				assertSymmetric(t, a, n)
				wantDeg := n - 1
				for u := 0; u < n; u++ {
					if got := degree(a, u); got != wantDeg {
						t.Errorf("n=%d: degree(%d) = %d, want %d", n, u, got, wantDeg)
					}
				}
			})
		}
	})

	// -- CompleteBipartite -------------------------------------------
	// K_{m,n}: Order=m+n, Size=m*n,
	// left-node (0..m-1) degree=n, right-node (m..m+n-1) degree=m.
	t.Run("CompleteBipartite", func(t *testing.T) {
		t.Parallel()
		cases := [][2]int{
			{0, 0},
			{1, 1},
			{2, 3},
			{5, 5},
			{3, 7},
		}
		for _, tc := range cases {
			m, n := tc[0], tc[1]
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.CompleteBipartite(m, n).Build(adjlist.Config{})
				if err != nil {
					t.Fatalf("CompleteBipartite(%d,%d) Build: %v", m, n, err)
				}
				a := g.AdjList()

				wantOrder := uint64(m + n)
				wantSize := uint64(m) * uint64(n)
				if got := a.Order(); got != wantOrder {
					t.Errorf("m=%d n=%d: Order = %d, want %d", m, n, got, wantOrder)
				}
				if got := a.Size(); got != wantSize {
					t.Errorf("m=%d n=%d: Size = %d, want %d", m, n, got, wantSize)
				}
				assertSymmetric(t, a, m+n)
				// Left nodes: degree = n.
				for u := 0; u < m; u++ {
					if got := degree(a, u); got != n {
						t.Errorf("m=%d n=%d: left-node degree(%d) = %d, want %d", m, n, u, got, n)
					}
				}
				// Right nodes: degree = m.
				for u := m; u < m+n; u++ {
					if got := degree(a, u); got != m {
						t.Errorf("m=%d n=%d: right-node degree(%d) = %d, want %d", m, n, u, got, m)
					}
				}
			})
		}
	})

	// -- Hypercube ---------------------------------------------------
	// Q_d: Order=2^d, Size=d*2^(d-1), every node degree=d.
	t.Run("Hypercube", func(t *testing.T) {
		t.Parallel()
		for _, d := range []int{0, 1, 2, 3, 4} {
			d := d
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.Hypercube(d).Build(adjlist.Config{})
				if err != nil {
					t.Fatalf("Hypercube(%d) Build: %v", d, err)
				}
				a := g.AdjList()

				order := 1 << d
				wantOrder := uint64(order)
				var wantSize uint64
				if d >= 1 {
					wantSize = uint64(d) * uint64(1<<(d-1))
				}
				if got := a.Order(); got != wantOrder {
					t.Errorf("d=%d: Order = %d, want %d", d, got, wantOrder)
				}
				if got := a.Size(); got != wantSize {
					t.Errorf("d=%d: Size = %d, want %d", d, got, wantSize)
				}
				assertSymmetric(t, a, order)
				for u := 0; u < order; u++ {
					if got := degree(a, u); got != d {
						t.Errorf("d=%d: degree(%d) = %d, want %d", d, u, got, d)
					}
				}
			})
		}
	})

	// -- Grid (4-neighbour) ------------------------------------------
	// L_{m,n}: Order=m*n,
	// Size=(m-1)*n + m*(n-1) for m,n >= 1; 0 otherwise.
	// Degrees: corner=2, edge=3, interior=4.
	t.Run("Grid4", func(t *testing.T) {
		t.Parallel()
		cases := [][2]int{
			{0, 0},
			{1, 1},
			{1, 5},
			{5, 1},
			{3, 3},
			{4, 6},
		}
		for _, tc := range cases {
			m, n := tc[0], tc[1]
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.Grid(m, n, false).Build(adjlist.Config{})
				if err != nil {
					t.Fatalf("Grid(%d,%d,false) Build: %v", m, n, err)
				}
				a := g.AdjList()

				wantOrder := uint64(m * n)
				var wantSize uint64
				if m >= 1 && n >= 1 {
					wantSize = uint64((m-1)*n + m*(n-1))
				}
				if got := a.Order(); got != wantOrder {
					t.Errorf("m=%d n=%d: Order = %d, want %d", m, n, got, wantOrder)
				}
				if got := a.Size(); got != wantSize {
					t.Errorf("m=%d n=%d: Size = %d, want %d", m, n, got, wantSize)
				}
				assertSymmetric(t, a, m*n)
				// Verify degree per node using the row/col position formula.
				// Corner (2 neighbours), edge (3 neighbours), interior (4 neighbours).
				for r := 0; r < m; r++ {
					for c := 0; c < n; c++ {
						u := r*n + c
						wantDeg := 0
						if c+1 < n {
							wantDeg++ // right
						}
						if c-1 >= 0 {
							wantDeg++ // left
						}
						if r+1 < m {
							wantDeg++ // down
						}
						if r-1 >= 0 {
							wantDeg++ // up
						}
						if got := degree(a, u); got != wantDeg {
							t.Errorf("m=%d n=%d: degree(%d) [r=%d,c=%d] = %d, want %d",
								m, n, u, r, c, got, wantDeg)
						}
					}
				}
			})
		}
	})

	// -- Grid (8-neighbour) ------------------------------------------
	// Symmetry-only: the degree formula for the 8-neighbour variant is
	// more involved so only mirror symmetry and Order/Size are asserted.
	t.Run("Grid8", func(t *testing.T) {
		t.Parallel()
		cases := [][2]int{
			{0, 0},
			{1, 1},
			{2, 2},
			{3, 4},
			{5, 5},
		}
		for _, tc := range cases {
			m, n := tc[0], tc[1]
			t.Run("", func(t *testing.T) {
				t.Parallel()
				g, err := shapegen.Grid(m, n, true).Build(adjlist.Config{})
				if err != nil {
					t.Fatalf("Grid(%d,%d,true) Build: %v", m, n, err)
				}
				a := g.AdjList()

				wantOrder := uint64(m * n)
				// Size formula from the structured.go docstring:
				// 4*m*n - 3*(m+n) + 2 for m,n >= 2;
				// degenerate 1×k or k×1 strip uses the 4-neighbour formula.
				var wantSize uint64
				switch {
				case m == 0 || n == 0:
					wantSize = 0
				case m == 1 || n == 1:
					// 1×k or k×1: only a single row/column path, no diagonals.
					wantSize = uint64((m-1)*n + m*(n-1))
				default:
					wantSize = uint64(4*m*n-3*(m+n)) + 2
				}
				if got := a.Order(); got != wantOrder {
					t.Errorf("m=%d n=%d: Order = %d, want %d", m, n, got, wantOrder)
				}
				if got := a.Size(); got != wantSize {
					t.Errorf("m=%d n=%d: Size = %d, want %d", m, n, got, wantSize)
				}
				assertSymmetric(t, a, m*n)
			})
		}
	})
}
