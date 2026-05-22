// Example 15_task_assignment — assign workers to tasks using the
// Hungarian algorithm to minimise total cost, and compare with the
// unweighted Hopcroft-Karp maximum matching.
package main

import (
	"fmt"
	"log"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

func main() {
	workers := []string{"alice", "bob", "carol", "dave"}
	tasks := []string{"task-A", "task-B", "task-C", "task-D"}

	// cost[i*m + j] = cost of worker i taking task j.
	cost := []float64{
		8, 4, 7, 5,
		6, 9, 5, 6,
		5, 3, 8, 7,
		7, 6, 4, 9,
	}
	n, m := len(workers), len(tasks)

	fmt.Println("=== Minimum-cost assignment (Hungarian) ===")
	a, err := search.Hungarian(cost, n, m)
	if err != nil {
		panic(err)
	}
	for i, j := range a.RowToCol {
		if j < 0 {
			continue
		}
		fmt.Printf("  %-7s -> %-7s (cost %.0f)\n", workers[i], tasks[j], cost[i*m+j])
	}
	fmt.Printf("  total = %.0f\n", a.TotalCost)

	// Convert the cost matrix into a bipartite edge list with edges
	// only where the worker is willing (cost <= 6) — this is the
	// kind of constraint typical of staffing problems.
	fmt.Println("\n=== Maximum cardinality matching (Hopcroft-Karp) ===")
	adj := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		if err := adj.AddNode(i); err != nil {
			log.Fatalf("AddNode: %v", err)
		}
	}
	for j := 0; j < m; j++ {
		if err := adj.AddNode(n + j); err != nil {
			log.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < m; j++ {
			if cost[i*m+j] <= 6 {
				if err := adj.AddEdge(i, n+j, struct{}{}); err != nil {
					log.Fatalf("AddEdge: %v", err)
				}
			}
		}
	}
	c := csr.BuildFromAdjList(adj)
	match := search.HopcroftKarp(c, int(c.MaxNodeID()))
	fmt.Printf("  matched pairs: %d\n", match.Size)
}
