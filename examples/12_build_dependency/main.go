// Example 12_build_dependency — model a software build dependency
// graph, derive the build order via topological sort, and detect
// circular dependencies with Tarjan SCC.
//
// The dependency edges are hard-coded, so NodeIDs are assigned by the
// mapper in a fixed first-appearance order. Kahn's algorithm emits
// vertices in ascending NodeID order, which makes the build order
// byte-stable across runs. The names printed inside a detected cycle
// are sorted alphabetically so the output does not depend on Tarjan's
// internal stack-pop order.
//
// Sample output: run `go run ./examples/12_build_dependency` and
// capture the stdout — the output is deterministic for the inputs
// hard-coded above and serves as the regression baseline a future
// change should preserve.
package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// deps is a small Go-style package dependency graph. Each edge (a, b)
// reads "a depends on b", so b must be built before a. The slice order
// fixes the mapper's NodeID assignment and therefore the deterministic
// build order.
var deps = [][2]string{
	{"app", "auth"},
	{"app", "store"},
	{"auth", "crypto"},
	{"store", "db"},
	{"db", "logging"},
	{"auth", "logging"},
}

// run builds the dependency graph, derives the build order, then
// introduces a cycle and detects it. All output goes to w so a test
// can capture and assert it; run returns wrapped errors rather than
// terminating the process.
func run(w io.Writer) error {
	if err := buildOrder(w); err != nil {
		return err
	}
	return detectCycle(w)
}

// buildOrder topologically sorts the acyclic dependency graph and
// prints the order in which the packages must be built (dependencies
// first). The topo order is reversed because TopologicalSort emits
// dependents before their dependencies.
func buildOrder(w io.Writer) error {
	fmt.Fprintln(w, "=== Build order (no cycles) ===")

	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for _, e := range deps {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			return fmt.Errorf("AddEdge %s->%s: %w", e[0], e[1], err)
		}
	}

	c := csr.BuildFromAdjList(a)
	order, err := search.TopologicalSort(c)
	if err != nil {
		return fmt.Errorf("TopologicalSort: %w", err)
	}

	mapper := a.Mapper()
	// Build dependencies first → reverse the topological order.
	for i := len(order) - 1; i >= 0; i-- {
		name, ok := mapper.Resolve(order[i])
		if !ok {
			return fmt.Errorf("unresolved node id %d", order[i])
		}
		fmt.Fprintf(w, "  %d. %s\n", len(order)-i, name)
	}
	return nil
}

// detectCycle adds a back edge that closes a circular dependency, shows
// that TopologicalSort rejects it with ErrCycle, and then reports the
// strongly connected component that contains the cycle via Tarjan SCC.
func detectCycle(w io.Writer) error {
	fmt.Fprintln(w, "\n=== Detecting a cycle ===")

	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for _, e := range deps {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			return fmt.Errorf("AddEdge %s->%s: %w", e[0], e[1], err)
		}
	}
	// Introduce a circular dependency: logging -> app.
	if err := a.AddEdge("logging", "app", struct{}{}); err != nil {
		return fmt.Errorf("AddEdge logging->app: %w", err)
	}

	c := csr.BuildFromAdjList(a)
	if _, err := search.TopologicalSort(c); !errors.Is(err, search.ErrCycle) {
		return fmt.Errorf("expected ErrCycle, got %v", err)
	}
	fmt.Fprintln(w, "topological sort rejects the cycle (ErrCycle).")

	fmt.Fprintln(w, "Strongly connected components (size > 1 are cycles):")
	mapper := a.Mapper()
	for _, comp := range search.TarjanSCC(c) {
		if len(comp) <= 1 {
			continue
		}
		names, err := cycleNames(mapper, comp)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  cycle: %v\n", names)
	}
	return nil
}

// cycleNames resolves a component's NodeIDs back to names and sorts
// them alphabetically. Sorting makes the printed cycle independent of
// Tarjan's internal stack-pop order, keeping the output byte-stable.
func cycleNames(mapper *graph.Mapper[string], comp []graph.NodeID) ([]string, error) {
	names := make([]string, len(comp))
	for i, n := range comp {
		name, ok := mapper.Resolve(n)
		if !ok {
			return nil, fmt.Errorf("unresolved node id %d", n)
		}
		names[i] = name
	}
	slices.Sort(names)
	return names, nil
}
