// Example 20_concurrent_reads — concurrent algorithm execution
// over a shared immutable CSR. Multiple goroutines run Dijkstra,
// BFS, and PageRank simultaneously on the same snapshot to
// illustrate the lock-free read contract of csr.CSR.
package main

import (
	"fmt"
	"sync"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/centrality"
)

func main() {
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	for i := 0; i < 100; i++ {
		a.AddEdge(i, (i+1)%100, int64(i%5+1))
		a.AddEdge(i, (i+7)%100, int64(i%3+1))
	}
	c := csr.BuildFromAdjList(a)

	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[string]string{}

	wg.Add(3)

	// Goroutine 1 — Dijkstra from a few seeds.
	go func() {
		defer wg.Done()
		var totalCost int64
		for s := 0; s < 8; s++ {
			src, _ := a.Mapper().Lookup(s)
			d, _ := search.Dijkstra(c, src)
			dst, _ := a.Mapper().Lookup((s + 50) % 100)
			cost, _ := d.Distance(dst)
			totalCost += cost
		}
		mu.Lock()
		results["dijkstra"] = fmt.Sprintf("8 SSSPs, summed cost = %d", totalCost)
		mu.Unlock()
	}()

	// Goroutine 2 — BFS reach counts.
	go func() {
		defer wg.Done()
		visited := 0
		src, _ := a.Mapper().Lookup(0)
		search.BFS(c, src, func(_ graph.NodeID, _ int) bool {
			visited++
			return true
		})
		mu.Lock()
		results["bfs"] = fmt.Sprintf("BFS reached %d nodes", visited)
		mu.Unlock()
	}()

	// Goroutine 3 — PageRank to convergence.
	go func() {
		defer wg.Done()
		ranks, iters := centrality.PageRank(c, centrality.DefaultPageRankOptions())
		mu.Lock()
		results["pagerank"] = fmt.Sprintf("PageRank %d iters, %d ranks", iters, len(ranks))
		mu.Unlock()
	}()

	wg.Wait()
	fmt.Println("Concurrent results over a single immutable CSR:")
	for _, k := range []string{"dijkstra", "bfs", "pagerank"} {
		fmt.Printf("  %-9s %s\n", k, results[k])
	}
}
