// Example 15_task_assignment — staff four workers onto four tasks two
// ways and compare the results: the Hungarian algorithm computes the
// globally cheapest one-to-one assignment, while Hopcroft-Karp computes
// the largest matching that respects a "willing to take" business rule.
//
// The two algorithms answer different questions. Hungarian minimises
// total cost over the full cost matrix; Hopcroft-Karp ignores cost and
// instead maximises how many workers can be staffed once each worker is
// allowed to refuse tasks they are not willing to take. Printing both
// and relating them is the point of the example.
//
// Sample output: run `go run ./examples/15_task_assignment` and capture
// the stdout — the output is deterministic for the inputs hard-coded
// below and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

// willingCostThreshold is the business rule that connects the two
// algorithms. A worker only accepts ("is willing to take") a task whose
// cost to them is at most this value; a task costing more is refused,
// however cheap it might be for the employer. This models a staffing
// constraint — fatigue, skill comfort, contractual limits — that the
// pure min-cost assignment is free to ignore but a real roster is not.
//
// The Hungarian assignment is computed over the *full* cost matrix and
// may legally use a pair the worker would refuse; the Hopcroft-Karp
// matching is computed over *only* the willing pairs, so it shows how
// many workers can still be staffed once refusals are honoured.
const willingCostThreshold = 6.0

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run computes the minimum-cost assignment and the maximum willing
// matching for the hard-coded roster, then writes a report — including a
// comparison that relates the two — to w. All output goes to w so a test
// can capture and assert it; run returns wrapped errors rather than
// terminating the process.
func run(w io.Writer) error {
	workers := []string{"alice", "bob", "carol", "dave"}
	tasks := []string{"task-A", "task-B", "task-C", "task-D"}

	// cost[i*m + j] is the cost of worker i taking task j. Lower is
	// better. The matrix is intentionally asymmetric so the two
	// algorithms disagree on who does what.
	cost := []float64{
		8, 4, 7, 5,
		6, 9, 5, 6,
		5, 3, 8, 7,
		7, 6, 4, 9,
	}
	n, m := len(workers), len(tasks)

	a, withinWilling, err := reportHungarian(w, workers, tasks, cost, n, m)
	if err != nil {
		return err
	}

	reportWillingSet(w, workers, tasks, cost, n, m)

	matchSize, matchedCost, err := reportMatching(w, workers, tasks, cost, n, m)
	if err != nil {
		return err
	}

	reportComparison(w, a, withinWilling, matchSize, matchedCost, n)
	return nil
}

// reportHungarian computes and prints the globally cheapest one-to-one
// assignment over the full cost matrix. Hungarian does not know about the
// willingness rule, so a pair it picks may be one the worker would
// refuse; each printed pair is annotated accordingly. It returns the
// assignment and how many of its pairs fall within the willing set, for
// the later comparison.
func reportHungarian(w io.Writer, workers, tasks []string, cost []float64, n, m int) (search.Assignment, int, error) {
	fmt.Fprintln(w, "=== Minimum-cost assignment (Hungarian) ===")
	a, err := search.Hungarian(cost, n, m)
	if err != nil {
		return search.Assignment{}, 0, fmt.Errorf("hungarian: %w", err)
	}
	withinWilling := 0
	for i, j := range a.RowToCol {
		if j < 0 {
			continue
		}
		c := cost[i*m+j]
		note := "willing"
		if c <= willingCostThreshold {
			withinWilling++
		} else {
			note = "OVER threshold — would be refused"
		}
		fmt.Fprintf(w, "  %-7s -> %-7s (cost %.0f, %s)\n", workers[i], tasks[j], c, note)
	}
	fmt.Fprintf(w, "  total = %.0f\n", a.TotalCost)
	return a, withinWilling, nil
}

// reportWillingSet makes the business rule explicit by listing, per
// worker, which tasks they accept and which they refuse. This is exactly
// the edge set fed to Hopcroft-Karp.
func reportWillingSet(w io.Writer, workers, tasks []string, cost []float64, n, m int) {
	fmt.Fprintf(w, "\n=== Willing set (worker accepts task when cost <= %.0f) ===\n", willingCostThreshold)
	for i := 0; i < n; i++ {
		fmt.Fprintf(w, "  %-7s willing:", workers[i])
		writeTaskList(w, tasks, cost, n, m, i, true)
		fmt.Fprintf(w, "          refuses:")
		writeTaskList(w, tasks, cost, n, m, i, false)
	}
}

// reportMatching builds a bipartite graph whose only edges are willing
// pairs and finds the largest matching with Hopcroft-Karp, which ignores
// cost and answers "how many workers can be staffed at all once refusals
// are honoured?". It prints each worker's assignment and returns the
// matching size and its total cost for the comparison.
func reportMatching(w io.Writer, workers, tasks []string, cost []float64, n, m int) (int, float64, error) {
	fmt.Fprintln(w, "\n=== Maximum willing matching (Hopcroft-Karp) ===")
	adj := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		if err := adj.AddNode(i); err != nil {
			return 0, 0, fmt.Errorf("AddNode worker %d: %w", i, err)
		}
	}
	for j := 0; j < m; j++ {
		if err := adj.AddNode(n + j); err != nil {
			return 0, 0, fmt.Errorf("AddNode task %d: %w", j, err)
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < m; j++ {
			if cost[i*m+j] <= willingCostThreshold {
				if err := adj.AddEdge(i, n+j, struct{}{}); err != nil {
					return 0, 0, fmt.Errorf("AddEdge %d->%d: %w", i, n+j, err)
				}
			}
		}
	}
	c := csr.BuildFromAdjList(adj)
	mapper := adj.Mapper()
	// The adjlist mapper assigns sparse, hash-derived NodeIDs, so the
	// left partition is not the contiguous range [0, n). Passing
	// nLeft = MaxNodeID lets every vertex act as a potential left
	// vertex; right (task) vertices have no out-edges and so never
	// match anything, leaving the cardinality correct. This mirrors the
	// convention in search/hopcroft_karp_test.go.
	match := search.HopcroftKarp(c, int(c.MaxNodeID())) //nolint:gosec // G115: bounded example graph size, no realistic overflow

	matchedCost := 0.0
	for i := 0; i < n; i++ {
		j, ok, err := matchedTask(mapper, match, i, n)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve match for %s: %w", workers[i], err)
		}
		if !ok {
			fmt.Fprintf(w, "  %-7s -> (unmatched)\n", workers[i])
			continue
		}
		matchedCost += cost[i*m+j]
		fmt.Fprintf(w, "  %-7s -> %-7s (cost %.0f)\n", workers[i], tasks[j], cost[i*m+j])
	}
	fmt.Fprintf(w, "  matched pairs: %d of %d workers\n", match.Size, n)
	return match.Size, matchedCost, nil
}

// reportComparison relates the two answers: whether the cheapest
// assignment survives the willingness rule, and whether honouring that
// rule costs coverage.
func reportComparison(w io.Writer, a search.Assignment, withinWilling, matchSize int, matchedCost float64, n int) {
	fmt.Fprintln(w, "\n=== Comparison ===")
	if withinWilling == n {
		fmt.Fprintf(w, "  Hungarian: all %d pairs are within the willing set (total cost %.0f).\n", n, a.TotalCost)
	} else {
		fmt.Fprintf(w, "  Hungarian: %d of %d pairs are within the willing set; %d would be refused.\n",
			withinWilling, n, n-withinWilling)
	}
	fmt.Fprintf(w, "  Hopcroft-Karp: %d of %d workers can be staffed using willing pairs only (total cost %.0f).\n",
		matchSize, n, matchedCost)
	switch {
	case matchSize == n && withinWilling == n:
		fmt.Fprintln(w, "  Verdict: the willingness rule is not binding here — the cheapest")
		fmt.Fprintln(w, "  assignment is already fully willing and every worker stays staffed,")
		fmt.Fprintf(w, "  so cost-optimality (%.0f) and full coverage (%d/%d) are achievable together.\n",
			a.TotalCost, matchSize, n)
	case matchSize == n:
		fmt.Fprintln(w, "  Verdict: full coverage is still possible under the willingness rule,")
		fmt.Fprintln(w, "  but the cheapest assignment uses pairs a worker would refuse, so the")
		fmt.Fprintln(w, "  willing roster must trade extra cost for staying within the rule.")
	default:
		fmt.Fprintf(w, "  Verdict: the willingness rule is binding — only %d of %d workers can be\n", matchSize, n)
		fmt.Fprintln(w, "  staffed at all, so some task must go unstaffed or the rule relaxed.")
	}
}

// writeTaskList prints, on one line, the tasks worker i is willing to
// take (willing == true) or refuses (willing == false), each with its
// cost, ordered by task index for deterministic output. It prints
// "(none)" when the list is empty.
func writeTaskList(w io.Writer, tasks []string, cost []float64, n, m, i int, willing bool) {
	printed := false
	for j := 0; j < m; j++ {
		isWilling := cost[i*m+j] <= willingCostThreshold
		if isWilling != willing {
			continue
		}
		fmt.Fprintf(w, " %s(%.0f)", tasks[j], cost[i*m+j])
		printed = true
	}
	if !printed {
		fmt.Fprint(w, " (none)")
	}
	fmt.Fprintln(w)
	_ = n
}

// matchedTask resolves the task index that worker i was matched to in
// the Hopcroft-Karp result, going through the adjlist mapper because the
// match arrays are indexed by sparse, hash-derived NodeIDs rather than
// by the contiguous worker index. It reports ok == false when the worker
// is unmatched, and an error only if a matched NodeID fails to resolve,
// which would indicate a corrupted result.
func matchedTask(mapper *graph.Mapper[int], match search.Matching, i, n int) (int, bool, error) {
	wid, ok := mapper.Lookup(i)
	if !ok {
		return 0, false, fmt.Errorf("worker key %d not found in mapper", i)
	}
	right := match.MatchL[uint64(wid)]
	if right == ^graph.NodeID(0) {
		return 0, false, nil
	}
	taskKey, ok := mapper.Resolve(right)
	if !ok {
		return 0, false, fmt.Errorf("unresolved node id %d", uint64(right))
	}
	return taskKey - n, true, nil
}
