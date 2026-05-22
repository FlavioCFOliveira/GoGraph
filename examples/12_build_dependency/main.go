// Example 12_build_dependency — model a software build dependency
// graph, derive the build order via topological sort, and detect
// circular dependencies with Tarjan SCC.
//
// Sample output: run `go run ./examples/12_build_dependency` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"errors"
	"fmt"
	"log"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

func main() {
	// A small Go-style package dependency graph.
	// Edge (a, b) reads "a depends on b" -> b must be built first.
	deps := [][2]string{
		{"app", "auth"},
		{"app", "store"},
		{"auth", "crypto"},
		{"store", "db"},
		{"db", "logging"},
		{"auth", "logging"},
	}

	fmt.Println("=== Build order (no cycles) ===")
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for _, e := range deps {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	order, err := search.TopologicalSort(c)
	if err != nil {
		fmt.Printf("topo failed: %v\n", err)
		return
	}
	// Build dependencies last → reverse the topo order.
	for i := len(order) - 1; i >= 0; i-- {
		name, _ := a.Mapper().Resolve(order[i])
		fmt.Printf("  %d. %s\n", len(order)-i, name)
	}

	fmt.Println("\n=== Detecting a cycle ===")
	a2 := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for _, e := range deps {
		if err := a2.AddEdge(e[0], e[1], struct{}{}); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}
	// Introduce a circular dependency: logging -> app.
	if err := a2.AddEdge("logging", "app", struct{}{}); err != nil {
		log.Fatalf("AddEdge: %v", err)
	}
	c2 := csr.BuildFromAdjList(a2)
	if _, err := search.TopologicalSort(c2); errors.Is(err, search.ErrCycle) {
		fmt.Println("topological sort rejects the cycle (ErrCycle).")
	}

	sccs := search.TarjanSCC(c2)
	fmt.Println("Strongly connected components (size > 1 are cycles):")
	for _, comp := range sccs {
		if len(comp) <= 1 {
			continue
		}
		names := make([]string, 0, len(comp))
		for _, n := range comp {
			name, _ := a2.Mapper().Resolve(n)
			names = append(names, name)
		}
		fmt.Printf("  cycle: %v\n", names)
	}
}
