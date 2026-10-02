package lpg

// direct_edge_lockorder_test.go — rmp #2947: an undirected direct edge write
// holds BOTH endpoints' adjacency shard locks, and a bulk removal holds every
// neighbour's. They are taken in ascending shard order; this drives writers in
// OPPOSITE directions on the same pairs, on pairs that share a shard and pairs
// that do not, so an inverted order would deadlock here.

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestDirectEdge_OppositeDirectionsDoNotDeadlock(t *testing.T) {
	const (
		rounds   = 400
		deadline = 2 * time.Minute // sized to catch a hang, not to pace the run
	)
	g := New[string, float64](adjlist.Config{Directed: false, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })

	// 300 nodes: ids 0..299, so ids k and k+256 share a shard and the pairs
	// below cover both the one-lock and the two-lock path.
	keys := make([]string, 300)
	for i := range keys {
		keys[i] = fmt.Sprintf("v%d", i)
		if err := g.AddNode(keys[i]); err != nil {
			t.Fatal(err)
		}
	}
	pairs := [][2]string{
		{keys[0], keys[256]}, // same shard
		{keys[1], keys[2]},   // adjacent shards
		{keys[3], keys[255]}, // far shards
		{keys[257], keys[1]}, // crosses the pairs above
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	work := func(f func(i int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := f(i); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for _, p := range pairs {
		a, b := p[0], p[1]
		work(func(int) error { return g.AddEdge(a, b, 1) })
		work(func(int) error { return g.AddEdge(b, a, 1) })
		work(func(int) error { return g.RemoveEdge(a, b) })
		work(func(int) error { return g.RemoveEdge(b, a) })
		work(func(i int) error {
			if i%8 == 0 {
				return g.RemoveAllEdgesFrom(a)
			}
			return nil
		})
		work(func(i int) error {
			if i%8 == 4 {
				return g.RemoveAllEdgesFrom(b)
			}
			return nil
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(deadline):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("direct edge writers made no progress within %v: a lock-order inversion "+
			"between two-entry writes. Goroutines:\n%s", deadline, buf[:n])
	}
	close(errs)
	for err := range errs {
		// No transaction is open, so nothing is pending and no write may be
		// refused: every error here is a defect.
		t.Errorf("direct edge write failed with no transaction open: %v", err)
	}
	// Symmetry survives the race: every arc has its mirror.
	for _, p := range pairs {
		if g.AdjList().HasEdge(p[0], p[1]) != g.AdjList().HasEdge(p[1], p[0]) {
			t.Errorf("undirected pair %s—%s lost its mirror under concurrent direct writes", p[0], p[1])
		}
	}
}
