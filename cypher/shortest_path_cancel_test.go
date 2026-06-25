package cypher_test

// shortest_path_cancel_test.go — regression gate for the 2026-06-25 round-3
// audit finding #1780: allShortestPaths enumerated every shortest path with no
// context check, so on a dense/layered graph (exponentially many shortest
// paths) a deadline could not interrupt it — the call hung / grew memory
// unbounded. The enumeration must honour cancellation promptly.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestAllShortestPaths_HonoursContextDeadline(t *testing.T) {
	t.Parallel()

	// Layered DAG: a -> L1 -> L2 -> ... -> L7 -> b, each intermediate layer
	// fully connected to the next. Shortest a->b paths = width^7 (here 10^7),
	// far more than can be enumerated within the deadline — so a correct
	// implementation must abort on the deadline, not run to completion.
	const width, layers = 10, 7
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("a", "k", lpg.StringValue("src")); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := g.SetNodeProperty("b", "k", lpg.StringValue("dst")); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	node := func(layer, idx int) string { return fmt.Sprintf("L%d_%d", layer, idx) }
	for i := 0; i < width; i++ { // a -> L1
		if err := g.AddEdge("a", node(1, i), 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	for l := 1; l < layers; l++ { // L l -> L l+1, fully connected
		for i := 0; i < width; i++ {
			for j := 0; j < width; j++ {
				if err := g.AddEdge(node(l, i), node(l+1, j), 1); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
		}
	}
	for i := 0; i < width; i++ { // L7 -> b
		if err := g.AddEdge(node(layers, i), "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	eng := cypher.NewEngine(g)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	const q = `MATCH (a {k:'src'}),(b {k:'dst'}) MATCH p = allShortestPaths((a)-[*]->(b)) RETURN count(*) AS c`

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		res, err := eng.RunAny(ctx, q, nil)
		if err == nil {
			for res.Next() {
			}
			err = res.Err()
			_ = res.Close()
		}
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		// The query must fail with the context error, not succeed after
		// enumerating ~10^7 paths.
		if r.err == nil {
			t.Fatal("allShortestPaths completed without honouring the 300ms deadline (enumerated everything)")
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want it to wrap context.DeadlineExceeded", r.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("allShortestPaths hung well past its 300ms deadline (#1780): the enumeration ignores cancellation")
	}
}
