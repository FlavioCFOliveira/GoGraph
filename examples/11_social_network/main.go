// Example 11_social_network — a small social-network application
// showing how to:
//
//   - Attach labels and typed properties to users.
//   - Build a CSR snapshot for analytics.
//   - Rank users by influence via PageRank.
//   - Detect communities via Leiden.
//   - Recommend friend-of-friend candidates with a 2-hop walk.
//
// Sample output: run `go run ./examples/11_social_network` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sort"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/search/centrality"
	"gograph/search/community"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the labelled social graph, runs the analytics, and writes
// the report to w. All output goes to w so a test can capture and
// assert it; run returns wrapped errors rather than terminating the
// process.
func run(w io.Writer) error {
	g := lpg.New[string, int64](adjlist.Config{Directed: false})

	users := []struct {
		name     string
		verified bool
		age      int64
	}{
		{"alice", true, 30},
		{"bob", false, 28},
		{"carol", true, 34},
		{"dave", false, 25},
		{"erin", true, 31},
		{"frank", false, 40},
		{"grace", false, 27},
	}
	for _, u := range users {
		if err := g.SetNodeLabel(u.name, "User"); err != nil {
			return fmt.Errorf("SetNodeLabel User %s: %w", u.name, err)
		}
		if u.verified {
			if err := g.SetNodeLabel(u.name, "Verified"); err != nil {
				return fmt.Errorf("SetNodeLabel Verified %s: %w", u.name, err)
			}
		}
		if err := g.SetNodeProperty(u.name, "age", lpg.Int64Value(u.age)); err != nil {
			return fmt.Errorf("SetNodeProperty age %s: %w", u.name, err)
		}
	}

	// Friendship edges.
	for _, e := range [][2]string{
		{"alice", "bob"}, {"alice", "carol"}, {"alice", "erin"},
		{"bob", "dave"}, {"carol", "erin"}, {"carol", "frank"},
		{"dave", "grace"}, {"erin", "grace"},
	} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			return fmt.Errorf("AddEdge %s-%s: %w", e[0], e[1], err)
		}
	}

	c := csr.BuildFromAdjList(g.AdjList())
	mapper := g.AdjList().Mapper()

	reportInfluence(w, c, mapper, len(users))
	reportCommunities(w, c, mapper)

	fmt.Fprintln(w, "\nFriend-of-friend recommendations for alice:")
	for _, name := range friendsOfFriends(g, "alice") {
		fmt.Fprintf(w, "  -> %s\n", name)
	}
	return nil
}

// reportInfluence ranks users by PageRank and writes them to w in
// descending rank order, breaking ties by name so the output is
// byte-stable (sort.Slice is not a stable sort). ranks is indexed by
// NodeID and may contain phantom zero entries for ids the mapper cannot
// resolve, so those are skipped. numUsers sizes the result slice.
func reportInfluence(w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[string], numUsers int) {
	fmt.Fprintln(w, "Influence (PageRank):")
	ranks, _, _ := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	type ranked struct {
		name string
		rank float64
	}
	ordered := make([]ranked, 0, numUsers)
	for i, r := range ranks {
		name, ok := mapper.Resolve(graph.NodeID(i))
		if !ok {
			continue
		}
		ordered = append(ordered, ranked{name, r})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].rank != ordered[j].rank {
			return ordered[i].rank > ordered[j].rank
		}
		return ordered[i].name < ordered[j].name
	})
	for _, o := range ordered {
		fmt.Fprintf(w, "  %-8s %.4f\n", o.name, o.rank)
	}
}

// reportCommunities groups users by Leiden cluster id and writes each
// cluster to w in ascending cluster-id order, with member names sorted,
// so the output is deterministic despite the non-deterministic map
// iteration order of the partition.
func reportCommunities(w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[string]) {
	fmt.Fprintln(w, "\nCommunities (Leiden):")
	p := community.Leiden(c, community.DefaultLeidenOptions())
	clusters := map[int][]string{}
	for i, cid := range p.Community {
		name, ok := mapper.Resolve(graph.NodeID(i))
		if !ok {
			continue
		}
		clusters[cid] = append(clusters[cid], name)
	}
	cids := make([]int, 0, len(clusters))
	for cid := range clusters {
		cids = append(cids, cid)
	}
	sort.Ints(cids)
	for _, cid := range cids {
		names := clusters[cid]
		sort.Strings(names)
		fmt.Fprintf(w, "  community %d: %v\n", cid, names)
	}
}

// friendsOfFriends returns users two hops away from src that are not
// already direct friends. The function shows a manual two-hop walk
// over the live adjacency list without building a CSR. The result is
// sorted by suggestion count (descending) then name, so it is stable.
func friendsOfFriends(g *lpg.Graph[string, int64], src string) []string {
	direct := map[string]bool{src: true}
	for v := range g.AdjList().Neighbours(src) {
		direct[v] = true
	}
	suggestions := map[string]int{}
	for v := range g.AdjList().Neighbours(src) {
		for w := range g.AdjList().Neighbours(v) {
			if direct[w] {
				continue
			}
			suggestions[w]++
		}
	}
	out := make([]string, 0, len(suggestions))
	for k := range suggestions {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if suggestions[out[i]] != suggestions[out[j]] {
			return suggestions[out[i]] > suggestions[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
