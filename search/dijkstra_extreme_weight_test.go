package search

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestDijkstra_ExtremeFloat64Weights verifies Dijkstra's behaviour on
// graphs with edge weights near the float64 extremes.
//
// Case 1 — near-max weight (1e308 per hop, 7 hops):
//
//	7×1e308 overflows IEEE 754 double precision to +Inf.
//	Dijkstra must not return ErrNegativeWeight or panic; it should
//	complete normally with dist[7]=+Inf in the distance map.
//
// Case 2 — near-min positive weight (1e-308 per hop, 7 hops):
//
//	7×1e-308 stays finite (no underflow to zero beyond the first hop
//	since 1e-308 is above the subnormal range). Dijkstra must return
//	a finite, correct distance.
func TestDijkstra_ExtremeFloat64Weights(t *testing.T) {
	t.Parallel()

	t.Run("near-max-weight-overflow-to-inf", func(t *testing.T) {
		t.Parallel()

		const n = 8 // nodes 0..7
		const w = 1e308

		a := adjlist.New[int, float64](adjlist.Config{Directed: true})
		for i := 0; i < n-1; i++ {
			if err := a.AddEdge(i, i+1, w); err != nil {
				t.Fatalf("AddEdge(%d→%d): %v", i, i+1, err)
			}
		}
		c := csr.BuildFromAdjList(a)
		id0, _ := a.Mapper().Lookup(0)
		id7, _ := a.Mapper().Lookup(7)

		d, err := Dijkstra(c, id0)
		// No ErrNegativeWeight — all weights are positive.
		if errors.Is(err, ErrNegativeWeight) {
			t.Fatal("unexpected ErrNegativeWeight: all weights are positive")
		}
		if err != nil {
			t.Fatalf("Dijkstra: %v", err)
		}

		dist7, ok := d.Distance(id7)
		if !ok {
			t.Fatal("node 7 reported unreachable")
		}
		if !math.IsInf(dist7, +1) {
			t.Errorf("dist[7]=%v, want +Inf (7*1e308 overflows float64)", dist7)
		}
	})

	t.Run("near-min-positive-weight", func(t *testing.T) {
		t.Parallel()

		const n = 8 // nodes 0..7
		const w = 1e-308

		a := adjlist.New[int, float64](adjlist.Config{Directed: true})
		for i := 0; i < n-1; i++ {
			if err := a.AddEdge(i, i+1, w); err != nil {
				t.Fatalf("AddEdge(%d→%d): %v", i, i+1, err)
			}
		}
		c := csr.BuildFromAdjList(a)
		id0, _ := a.Mapper().Lookup(0)
		id7, _ := a.Mapper().Lookup(7)

		d, err := Dijkstra(c, id0)
		if errors.Is(err, ErrNegativeWeight) {
			t.Fatal("unexpected ErrNegativeWeight: all weights are positive")
		}
		if err != nil {
			t.Fatalf("Dijkstra: %v", err)
		}

		dist7, ok := d.Distance(id7)
		if !ok {
			t.Fatal("node 7 reported unreachable")
		}
		// 7 * 1e-308 is finite and strictly positive; it must not be zero.
		if math.IsInf(dist7, 0) || math.IsNaN(dist7) || dist7 <= 0 {
			t.Errorf("dist[7]=%v, want a small positive finite value (7*1e-308)", dist7)
		}
		// Allow up to 1 ULP of rounding error from the 7 additions.
		want := 7 * w
		const ulpTolerance = 2
		if math.Abs(dist7-want) > ulpTolerance*math.SmallestNonzeroFloat64 {
			// Relative check: accept if within 1e-12 relative error.
			rel := math.Abs(dist7-want) / want
			if rel > 1e-12 {
				t.Errorf("dist[7]=%v, want %v (rel err %v)", dist7, want, rel)
			}
		}
	})
}
