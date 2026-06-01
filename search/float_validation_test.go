package search

// Task T926: NaN/Inf float-weight gate on Dijkstra, A*, BiDijkstra,
// Yen, KShortestPathsLoopless, Prim, Kruskal, Floyd-Warshall,
// Johnson, and DijkstraAPSP.
//
// IEEE-754 makes every `NaN < x` comparison false, so without an
// explicit gate the relaxation/PQ comparators silently drop edges
// and the algorithm returns wrong-but-non-erroring distances. Every
// public weighted entry point must validate the CSR's weights at
// the boundary and return [ErrInvalidInput].
//
// Integer Weight types must NOT pay the scan cost: anyFloatInvalid
// type-switches the zero value to a non-float arm and short-
// circuits before iterating.

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildPoisonedCSR builds a 3-node directed float64 CSR with a single
// poisoned edge (0 -> 1 -> 2 where the second edge carries `poison`).
// The returned src is the mapper-translated NodeID for vertex 0;
// dst is the mapper-translated NodeID for vertex 2.
func buildPoisonedCSR(t *testing.T, poison float64) (c *csr.CSR[float64], src, dst graph.NodeID) {
	t.Helper()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge(0->1): %v", err)
	}
	if err := a.AddEdge(1, 2, poison); err != nil {
		t.Fatalf("AddEdge(1->2): %v", err)
	}
	c = csr.BuildFromAdjList(a)
	src, _ = a.Mapper().Lookup(0)
	dst, _ = a.Mapper().Lookup(2)
	return c, src, dst
}

// buildPoisonedCSRUndirected mirrors buildPoisonedCSR but produces a
// symmetric directed CSR so that Prim and Kruskal (which expect
// undirected graphs encoded as symmetric digraphs) have a well-formed
// input on the happy path; the gate fires before any traversal, so
// the poisoned edge is enough.
func buildPoisonedCSRUndirected(t *testing.T, poison float64) (c *csr.CSR[float64], src graph.NodeID) {
	t.Helper()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge(0-1): %v", err)
	}
	if err := a.AddEdge(1, 2, poison); err != nil {
		t.Fatalf("AddEdge(1-2): %v", err)
	}
	c = csr.BuildFromAdjList(a)
	src, _ = a.Mapper().Lookup(0)
	return c, src
}

// poisonCases enumerates the three IEEE-754 invalid values an edge
// weight can take. Each case is exercised against every gated entry
// point by the table-driven tests below.
var poisonCases = []struct {
	name string
	w    float64
}{
	{"NaN", math.NaN()},
	{"PosInf", math.Inf(+1)},
	{"NegInf", math.Inf(-1)},
}

// TestFloatValidation_Dijkstra covers Dijkstra, DijkstraCtx, and
// DijkstraInto. All three feed dijkstraCore; the gate sits there.
func TestFloatValidation_Dijkstra(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, src, _ := buildPoisonedCSR(t, tc.w)

			if d, err := Dijkstra(c, src); !errors.Is(err, ErrInvalidInput) || d != nil {
				t.Fatalf("Dijkstra: d=%v err=%v, want (nil, ErrInvalidInput)", d, err)
			}
			if d, err := DijkstraCtx(context.Background(), c, src); !errors.Is(err, ErrInvalidInput) || d != nil {
				t.Fatalf("DijkstraCtx: d=%v err=%v, want (nil, ErrInvalidInput)", d, err)
			}
			maxID := uint64(c.MaxNodeID())
			dist := make([]float64, maxID)
			parent := make([]graph.NodeID, maxID)
			found := make([]bool, maxID)
			if err := DijkstraInto(context.Background(), c, src, dist, parent, found); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("DijkstraInto: err=%v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestFloatValidation_AStar covers AStar, AStarCtx, AStarInto. The
// heuristic returns zero (admissible for any non-negative graph).
func TestFloatValidation_AStar(t *testing.T) {
	t.Parallel()
	zeroH := func(graph.NodeID) float64 { return 0 }
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, src, dst := buildPoisonedCSR(t, tc.w)

			if p, cost, err := AStar(c, src, dst, zeroH); !errors.Is(err, ErrInvalidInput) || p != nil || cost != 0 {
				t.Fatalf("AStar: p=%v cost=%v err=%v, want (nil, 0, ErrInvalidInput)", p, cost, err)
			}
			if p, cost, err := AStarCtx(context.Background(), c, src, dst, zeroH); !errors.Is(err, ErrInvalidInput) || p != nil || cost != 0 {
				t.Fatalf("AStarCtx: p=%v cost=%v err=%v, want (nil, 0, ErrInvalidInput)", p, cost, err)
			}
			maxID := uint64(c.MaxNodeID())
			dist := make([]float64, maxID)
			parent := make([]graph.NodeID, maxID)
			found := make([]bool, maxID)
			var path []graph.NodeID
			if cost, err := AStarInto(context.Background(), c, src, dst, zeroH, dist, parent, found, &path); !errors.Is(err, ErrInvalidInput) || cost != 0 {
				t.Fatalf("AStarInto: cost=%v err=%v, want (0, ErrInvalidInput)", cost, err)
			}
		})
	}
}

// TestFloatValidation_BiDijkstra covers BidirectionalDijkstra,
// BidirectionalDijkstraOn, and BidirectionalDijkstraOnCtx.
func TestFloatValidation_BiDijkstra(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, src, dst := buildPoisonedCSR(t, tc.w)
			rev := c.BuildReverse()

			if p, cost, err := BidirectionalDijkstra(c, src, dst); !errors.Is(err, ErrInvalidInput) || p != nil || cost != 0 {
				t.Fatalf("BidirectionalDijkstra: p=%v cost=%v err=%v, want (nil, 0, ErrInvalidInput)", p, cost, err)
			}
			if p, cost, err := BidirectionalDijkstraOn(c, rev, src, dst); !errors.Is(err, ErrInvalidInput) || p != nil || cost != 0 {
				t.Fatalf("BidirectionalDijkstraOn: p=%v cost=%v err=%v, want (nil, 0, ErrInvalidInput)", p, cost, err)
			}
			if p, cost, err := BidirectionalDijkstraOnCtx(context.Background(), c, rev, src, dst); !errors.Is(err, ErrInvalidInput) || p != nil || cost != 0 {
				t.Fatalf("BidirectionalDijkstraOnCtx: p=%v cost=%v err=%v, want (nil, 0, ErrInvalidInput)", p, cost, err)
			}
		})
	}
}

// TestFloatValidation_Yen covers YenKShortest and YenKShortestCtx.
// The simple entry discards the error, so we only assert that no
// paths are returned — the Ctx entry asserts ErrInvalidInput.
func TestFloatValidation_Yen(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, src, dst := buildPoisonedCSR(t, tc.w)

			if paths := YenKShortest(c, src, dst, 3); len(paths) != 0 {
				t.Fatalf("YenKShortest: len(paths)=%d, want 0 on invalid input", len(paths))
			}
			if paths, err := YenKShortestCtx(context.Background(), c, src, dst, 3); !errors.Is(err, ErrInvalidInput) || paths != nil {
				t.Fatalf("YenKShortestCtx: paths=%v err=%v, want (nil, ErrInvalidInput)", paths, err)
			}
		})
	}
}

// TestFloatValidation_KShortestPathsLoopless covers the loopless
// best-first k-shortest variant.
func TestFloatValidation_KShortestPathsLoopless(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, src, dst := buildPoisonedCSR(t, tc.w)

			if paths := KShortestPathsLoopless(c, src, dst, 3); len(paths) != 0 {
				t.Fatalf("KShortestPathsLoopless: len(paths)=%d, want 0 on invalid input", len(paths))
			}
			if paths, err := KShortestPathsLooplessCtx(context.Background(), c, src, dst, 3); !errors.Is(err, ErrInvalidInput) || paths != nil {
				t.Fatalf("KShortestPathsLooplessCtx: paths=%v err=%v, want (nil, ErrInvalidInput)", paths, err)
			}
		})
	}
}

// TestFloatValidation_Prim covers PrimMST and PrimMSTCtx on an
// undirected CSR.
func TestFloatValidation_Prim(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, src := buildPoisonedCSRUndirected(t, tc.w)

			if parent, found, tw, err := PrimMST(c, src); !errors.Is(err, ErrInvalidInput) || parent != nil || found != nil || tw != 0 {
				t.Fatalf("PrimMST: parent=%v found=%v tw=%v err=%v, want (nil, nil, 0, ErrInvalidInput)", parent, found, tw, err)
			}
			if parent, found, tw, err := PrimMSTCtx(context.Background(), c, src); !errors.Is(err, ErrInvalidInput) || parent != nil || found != nil || tw != 0 {
				t.Fatalf("PrimMSTCtx: parent=%v found=%v tw=%v err=%v, want (nil, nil, 0, ErrInvalidInput)", parent, found, tw, err)
			}
		})
	}
}

// TestFloatValidation_Kruskal covers KruskalMST and KruskalMSTCtx.
func TestFloatValidation_Kruskal(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := buildPoisonedCSRUndirected(t, tc.w)

			if edges, tw, err := KruskalMST(c); !errors.Is(err, ErrInvalidInput) || edges != nil || tw != 0 {
				t.Fatalf("KruskalMST: edges=%v tw=%v err=%v, want (nil, 0, ErrInvalidInput)", edges, tw, err)
			}
			if edges, tw, err := KruskalMSTCtx(context.Background(), c); !errors.Is(err, ErrInvalidInput) || edges != nil || tw != 0 {
				t.Fatalf("KruskalMSTCtx: edges=%v tw=%v err=%v, want (nil, 0, ErrInvalidInput)", edges, tw, err)
			}
		})
	}
}

// TestFloatValidation_FloydWarshall covers FloydWarshall and
// FloydWarshallCtx. The simple entry discards the error so we only
// assert nil; the Ctx entry asserts ErrInvalidInput.
func TestFloatValidation_FloydWarshall(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _, _ := buildPoisonedCSR(t, tc.w)

			if a := FloydWarshall(c); a != nil {
				t.Fatalf("FloydWarshall: a=%v, want nil on invalid input", a)
			}
			if a, err := FloydWarshallCtx(context.Background(), c); !errors.Is(err, ErrInvalidInput) || a != nil {
				t.Fatalf("FloydWarshallCtx: a=%v err=%v, want (nil, ErrInvalidInput)", a, err)
			}
		})
	}
}

// TestFloatValidation_Johnson covers JohnsonAPSP, JohnsonAPSPCtx,
// DijkstraAPSP, and DijkstraAPSPCtx. The gate sits at each public
// boundary so Johnson surfaces a single failure rather than relaying
// the inner Bellman-Ford or Dijkstra error.
func TestFloatValidation_Johnson(t *testing.T) {
	t.Parallel()
	for _, tc := range poisonCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _, _ := buildPoisonedCSR(t, tc.w)

			if a, err := JohnsonAPSP(c); !errors.Is(err, ErrInvalidInput) || a != nil {
				t.Fatalf("JohnsonAPSP: a=%v err=%v, want (nil, ErrInvalidInput)", a, err)
			}
			if a, err := JohnsonAPSPCtx(context.Background(), c); !errors.Is(err, ErrInvalidInput) || a != nil {
				t.Fatalf("JohnsonAPSPCtx: a=%v err=%v, want (nil, ErrInvalidInput)", a, err)
			}
			if a, err := DijkstraAPSP(c); !errors.Is(err, ErrInvalidInput) || a != nil {
				t.Fatalf("DijkstraAPSP: a=%v err=%v, want (nil, ErrInvalidInput)", a, err)
			}
			if a, err := DijkstraAPSPCtx(context.Background(), c); !errors.Is(err, ErrInvalidInput) || a != nil {
				t.Fatalf("DijkstraAPSPCtx: a=%v err=%v, want (nil, ErrInvalidInput)", a, err)
			}
		})
	}
}

// TestFloatValidation_IntegerSkipsGate is a smoke test: with an
// integer Weight type the gate must compile and short-circuit (the
// happy-path graph has no invalid weights, so every algorithm runs
// to completion). The key behavioural point — that integer W skips
// the per-element scan — is enforced by [anyFloatInvalid]'s
// type-switch on the zero value and is exercised by every existing
// integer-weighted test in the package; this test guards against a
// regression that broke compilation for non-float W.
func TestFloatValidation_IntegerSkipsGate(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 5); err != nil {
		t.Fatalf("AddEdge(0->1): %v", err)
	}
	if err := a.AddEdge(1, 2, 7); err != nil {
		t.Fatalf("AddEdge(1->2): %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)

	d, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("Dijkstra(int64): %v", err)
	}
	dstID, _ := a.Mapper().Lookup(2)
	if got, ok := d.Distance(dstID); !ok || got != 12 {
		t.Fatalf("Dijkstra(int64) Distance(2) = %d (ok=%v), want 12", got, ok)
	}
}
