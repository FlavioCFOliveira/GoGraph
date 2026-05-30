// Example 08_pagerank — runs PageRank on a small directed "authority"
// graph and prints each page's rank, sorted from most to least
// important.
//
// The graph models a tiny web of pages that link to one another. Four
// peripheral pages (B, C, D, E) all link to a single Authority page A,
// giving A a high in-degree. A is the only node every other page
// endorses, so the random-surfer model concentrates the most
// stationary mass on it. A links back to a Hub page H, which in turn
// links to two of the peripheral pages — this asymmetry is what makes
// the four peripheral ranks differ from one another instead of being
// identical.
//
// Unlike a symmetric cycle (where every PageRank score is the same),
// this topology produces clearly distinct ranks: the authority A wins,
// the hub H comes next on the strength of A's single outgoing link, the
// two pages H endorses (B, C) outrank the two it does not (D, E).
//
// Sample output: run `go run ./examples/08_pagerank` and capture the
// stdout — the output is deterministic for the inputs hard-coded above
// and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sort"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/centrality"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the authority graph, runs PageRank, and writes the ranked
// report to w. All output goes to w so a test can capture and assert
// it; run returns wrapped errors rather than terminating the process.
func run(w io.Writer) error {
	// A directed "authority" web. Every peripheral page links to the
	// Authority A; A links to the Hub H; H endorses two of the four
	// peripheral pages, breaking the symmetry between them.
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	for _, edge := range [...]struct{ s, d string }{
		{"B", "A"}, // peripheral pages all endorse the authority
		{"C", "A"},
		{"D", "A"},
		{"E", "A"},
		{"A", "H"}, // the authority's single out-link feeds the hub
		{"H", "B"}, // the hub endorses two peripheral pages...
		{"H", "C"}, // ...leaving D and E with only their teleport share
	} {
		if err := a.AddEdge(edge.s, edge.d, struct{}{}); err != nil {
			return fmt.Errorf("AddEdge %s->%s: %w", edge.s, edge.d, err)
		}
	}

	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	ranks, iters, err := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	if err != nil {
		return fmt.Errorf("PageRank: %w", err)
	}

	// The rank slice is indexed by NodeID and rounds up to MaxNodeID
	// across the 256-shard Mapper space, so it contains ghost slots for
	// NodeIDs that no page occupies. Resolve the live pages through the
	// mapper and read only their ranks.
	pages := []string{"A", "B", "C", "D", "E", "H"}
	type scored struct {
		name string
		rank float64
	}
	scores := make([]scored, 0, len(pages))
	for _, page := range pages {
		id, ok := mapper.Lookup(page)
		if !ok {
			return fmt.Errorf("page %q not found in graph", page)
		}
		scores = append(scores, scored{name: page, rank: ranks[id]})
	}

	// Sort descending by rank. The name is the tiebreaker so the order
	// is fully deterministic even when two ranks are numerically equal.
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].rank != scores[j].rank {
			return scores[i].rank > scores[j].rank
		}
		return scores[i].name < scores[j].name
	})

	fmt.Fprintf(w, "Converged in %d iterations (%d live pages)\n", iters, len(scores))
	for rank, s := range scores {
		fmt.Fprintf(w, "  %d. page %s: %.6f\n", rank+1, s.name, s.rank)
	}
	return nil
}
