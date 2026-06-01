package dimacs9

// ny_routing_test.go — T744: DIMACS9 NY routing via the public Go API.
//
// Builds a synthetic 1k-vertex / 3k-edge road-network graph, converts it
// to a CSR snapshot, and verifies that Dijkstra and A* (with a zero
// heuristic) agree on the shortest-path distance for 10 canonical
// (src, dst) pairs chosen with a fixed PRNG seed.
//
// Admissibility check: a zero heuristic degrades A* to Dijkstra, so
// the two distances must be identical on every reachable pair. Pairs for
// which Dijkstra returns ErrNoPath (no path in a sparse synthetic graph)
// are skipped; the test requires at least 5 reachable pairs to be
// meaningful.

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestNYRouting_DijkstraAStar verifies admissibility of the A* zero-heuristic
// variant against Dijkstra on a synthetic road-network graph.
func TestNYRouting_DijkstraAStar(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := context.Background()

	// Build a 1 000-vertex / 3 000-edge synthetic road-network graph.
	a, err := Synthetic(ctx, 1000, 3000)
	if err != nil {
		t.Fatalf("Synthetic: %v", err)
	}

	// Convert to CSR for search algorithms.
	c := csr.BuildFromAdjList(a)

	maxID := c.MaxNodeID()
	if maxID == 0 {
		t.Fatal("CSR has zero MaxNodeID; graph is empty")
	}

	// Pick 10 canonical (src, dst) pairs from a fixed PRNG seed.
	r := rand.New(rand.NewPCG(42, 0))
	pairs := make([][2]graph.NodeID, 10)
	for i := range pairs {
		pairs[i] = [2]graph.NodeID{
			graph.NodeID(r.Uint64N(uint64(maxID))),
			graph.NodeID(r.Uint64N(uint64(maxID))),
		}
	}

	// Zero heuristic: makes A* behave identically to Dijkstra.
	zeroH := func(_ graph.NodeID) int64 { return 0 }

	var (
		reachable int
		minLat    time.Duration = 1<<62 - 1
		maxLat    time.Duration
	)

	for i, pair := range pairs {
		src, dst := pair[0], pair[1]

		// --- Dijkstra ---
		t0 := time.Now()
		dists, dijErr := search.Dijkstra(c, src)
		dijLat := time.Since(t0)

		if dijErr != nil {
			// Dijkstra only errors on negative weights; our synthetic graph
			// has weights in [1, 97], so this would be unexpected.
			t.Fatalf("pair[%d] Dijkstra(%d): unexpected error: %v", i, src, dijErr)
		}
		dijDist, dijReachable := dists.Distance(dst)

		if !dijReachable {
			// Unreachable pair in a sparse directed graph — skip.
			continue
		}

		// --- A* with zero heuristic ---
		t1 := time.Now()
		_, astarDist, astarErr := search.AStarCtx(ctx, c, src, dst, zeroH)
		astarLat := time.Since(t1)

		if astarErr != nil {
			if errors.Is(astarErr, search.ErrNoPath) {
				// Dijkstra found a path but A* did not: this is a bug.
				t.Errorf("pair[%d] src=%d dst=%d: Dijkstra found path (dist=%d) but A* returned ErrNoPath",
					i, src, dst, dijDist)
				continue
			}
			t.Fatalf("pair[%d] AStarCtx(%d->%d): unexpected error: %v", i, src, dst, astarErr)
		}

		// Admissibility check: distances must agree.
		if dijDist != astarDist {
			t.Errorf("pair[%d] src=%d dst=%d: Dijkstra dist=%d, A* dist=%d (mismatch)",
				i, src, dst, dijDist, astarDist)
		}

		reachable++
		total := dijLat + astarLat
		if total < minLat {
			minLat = total
		}
		if total > maxLat {
			maxLat = total
		}
	}

	if reachable < 5 {
		t.Errorf("only %d reachable pairs (need >= 5 for a meaningful admissibility check)", reachable)
	}

	t.Logf("reachable pairs: %d/10  min-latency(dijk+astar): %v  max-latency: %v",
		reachable, minLat, maxLat)
}
