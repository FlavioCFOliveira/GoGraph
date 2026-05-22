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
	"log"
	"sort"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/search/centrality"
	"gograph/search/community"
)

func main() {
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
			log.Fatalf("SetNodeLabel: %v", err)
		}
		if u.verified {
			if err := g.SetNodeLabel(u.name, "Verified"); err != nil {
				log.Fatalf("SetNodeLabel: %v", err)
			}
		}
		if err := g.SetNodeProperty(u.name, "age", lpg.Int64Value(u.age)); err != nil {
			log.Fatalf("SetNodeProperty: %v", err)
		}
	}

	// Friendship edges.
	for _, e := range [][2]string{
		{"alice", "bob"}, {"alice", "carol"}, {"alice", "erin"},
		{"bob", "dave"}, {"carol", "erin"}, {"carol", "frank"},
		{"dave", "grace"}, {"erin", "grace"},
	} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}

	c := csr.BuildFromAdjList(g.AdjList())

	fmt.Println("Influence (PageRank):")
	ranks, _, _ := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	type ranked struct {
		name string
		rank float64
	}
	var ordered []ranked
	for i, r := range ranks {
		name, ok := g.AdjList().Mapper().Resolve(graph.NodeID(i))
		if !ok {
			continue
		}
		ordered = append(ordered, ranked{name, r})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].rank > ordered[j].rank })
	for _, o := range ordered {
		fmt.Printf("  %-8s %.4f\n", o.name, o.rank)
	}

	fmt.Println("\nCommunities (Leiden):")
	p := community.Leiden(c, community.DefaultLeidenOptions())
	clusters := map[int][]string{}
	for i, cid := range p.Community {
		name, _ := g.AdjList().Mapper().Resolve(graph.NodeID(i))
		if name == "" {
			continue
		}
		clusters[cid] = append(clusters[cid], name)
	}
	for cid, names := range clusters {
		sort.Strings(names)
		fmt.Printf("  community %d: %v\n", cid, names)
	}

	fmt.Println("\nFriend-of-friend recommendations for alice:")
	for _, name := range friendsOfFriends(g, "alice") {
		fmt.Printf("  -> %s\n", name)
	}
}

// friendsOfFriends returns users two hops away from src that are not
// already direct friends. The function shows a manual two-hop walk
// over the live adjacency list without building a CSR.
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
