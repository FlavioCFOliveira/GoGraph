// Example 20_concurrent_reads — run three different read-only graph
// algorithms concurrently over one shared, immutable CSR snapshot.
//
// A single csr.CSR is built once and then read by three goroutines at
// the same time: one runs a batch of Dijkstra single-source shortest
// paths, one runs a BFS reach count, and one runs PageRank to
// convergence. None of them takes a lock on the snapshot — an immutable
// CSR is safe for any number of concurrent readers with zero
// synchronisation on the hot path. The goroutines publish their
// findings into a shared map under a mutex (the map is mutated, the CSR
// is not), and run prints the aggregated results in a fixed key order.
//
// Sample output: run `go run ./examples/20_concurrent_reads` and capture
// the stdout. Goroutine completion order is non-deterministic, but the
// reported aggregates (summed Dijkstra cost, BFS reach count, PageRank
// live-rank count) are deterministic for the hard-coded inputs and serve
// as the regression baseline a future change should preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/centrality"
)

func main() {
	if _, err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// aggregates holds the deterministic results the three concurrent
// readers compute over the shared immutable CSR. The values do not
// depend on goroutine scheduling, so a test can assert them directly
// without parsing the printed report.
type aggregates struct {
	// dijkstraCost is the sum of the shortest-path costs from each of
	// the eight seeds to its antipodal node.
	dijkstraCost int64
	// bfsReached is the number of nodes BFS visits from node 0.
	bfsReached int
	// pagerankLive is the number of nodes carrying non-zero PageRank.
	pagerankLive int
}

// run builds one immutable CSR, runs Dijkstra, BFS, and PageRank over it
// concurrently, and writes the aggregated report to w in a fixed key
// order so any single run's stdout is stable. It returns the computed
// aggregates alongside any error so a test can assert the invariants
// without depending on goroutine timing. All output goes to w; run
// returns wrapped errors rather than terminating the process.
//
//nolint:gocyclo // example walk-through: setup + three concurrent reader goroutines + join + ordered report
func run(w io.Writer) (aggregates, error) {
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	for i := 0; i < 100; i++ {
		if err := a.AddEdge(i, (i+1)%100, int64(i%5+1)); err != nil {
			return aggregates{}, fmt.Errorf("AddEdge ring %d: %w", i, err)
		}
		if err := a.AddEdge(i, (i+7)%100, int64(i%3+1)); err != nil {
			return aggregates{}, fmt.Errorf("AddEdge chord %d: %w", i, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = map[string]string{}
		agg     aggregates
		// errs collects the first error each reader hits; checked after
		// the readers join so run never observes a partially built result.
		errs = make([]error, 3)
	)

	wg.Add(3)

	// Reader 1 — a batch of Dijkstra single-source shortest paths. Each
	// seed s measures the cost to its antipodal node (s+50) on the ring.
	go func() {
		defer wg.Done()
		var totalCost int64
		for s := 0; s < 8; s++ {
			src, ok := mapper.Lookup(s)
			if !ok {
				errs[0] = fmt.Errorf("dijkstra: seed %d not found", s)
				return
			}
			d, err := search.Dijkstra(c, src)
			if err != nil {
				errs[0] = fmt.Errorf("dijkstra from %d: %w", s, err)
				return
			}
			dst, ok := mapper.Lookup((s + 50) % 100)
			if !ok {
				errs[0] = fmt.Errorf("dijkstra: target for seed %d not found", s)
				return
			}
			cost, reachable := d.Distance(dst)
			if !reachable {
				errs[0] = fmt.Errorf("dijkstra: target for seed %d unreachable", s)
				return
			}
			totalCost += cost
		}
		mu.Lock()
		agg.dijkstraCost = totalCost
		results["dijkstra"] = fmt.Sprintf("8 SSSPs, summed cost = %d", totalCost)
		mu.Unlock()
	}()

	// Reader 2 — a BFS reach count from node 0.
	go func() {
		defer wg.Done()
		src, ok := mapper.Lookup(0)
		if !ok {
			errs[1] = fmt.Errorf("bfs: source node 0 not found")
			return
		}
		visited := 0
		search.BFS(c, src, func(_ graph.NodeID, _ int) bool {
			visited++
			return true
		})
		mu.Lock()
		agg.bfsReached = visited
		results["bfs"] = fmt.Sprintf("BFS reached %d nodes", visited)
		mu.Unlock()
	}()

	// Reader 3 — PageRank to convergence. Counts the nodes that end up
	// with a non-zero rank (every live node, here).
	go func() {
		defer wg.Done()
		ranks, iters, err := centrality.PageRank(c, centrality.DefaultPageRankOptions())
		if err != nil {
			errs[2] = fmt.Errorf("pagerank: %w", err)
			return
		}
		var live int
		for _, r := range ranks {
			if r > 0 {
				live++
			}
		}
		mu.Lock()
		agg.pagerankLive = live
		results["pagerank"] = fmt.Sprintf("PageRank %d iters, %d live ranks", iters, live)
		mu.Unlock()
	}()

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return aggregates{}, err
		}
	}

	fmt.Fprintln(w, "Concurrent results over a single immutable CSR:")
	for _, k := range []string{"dijkstra", "bfs", "pagerank"} {
		fmt.Fprintf(w, "  %-9s %s\n", k, results[k])
	}
	return agg, nil
}
