// Example 16_centrality_analytics — analyse a small undirected
// network with two centrality metrics: Brandes betweenness
// (structural importance via shortest paths) and label
// propagation (cluster membership).
package main

import (
	"fmt"
	"sort"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/centrality"
	"gograph/search/community"
)

func main() {
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: false})
	for _, e := range [][2]string{
		// Cluster 1 (around "marie")
		{"marie", "pierre"}, {"marie", "anne"}, {"anne", "pierre"},
		// Cluster 2 (around "jose")
		{"jose", "ana"}, {"jose", "luis"}, {"luis", "ana"},
		// The bridge between the two clusters.
		{"marie", "jose"},
	} {
		a.AddEdge(e[0], e[1], struct{}{})
	}
	c := csr.BuildFromAdjList(a)

	fmt.Println("Betweenness (higher = more critical):")
	bc := centrality.Betweenness(c)
	type entry struct {
		name  string
		score float64
	}
	var entries []entry
	for i, s := range bc {
		name, _ := a.Mapper().Resolve(graph.NodeID(i))
		if name == "" {
			continue
		}
		entries = append(entries, entry{name, s})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].score > entries[j].score })
	for _, e := range entries {
		fmt.Printf("  %-7s %.2f\n", e.name, e.score)
	}

	fmt.Println("\nLabel propagation clusters:")
	p := community.LabelPropagation(c, community.DefaultLabelPropagationOptions())
	clusters := map[int][]string{}
	for i, cid := range p.Community {
		name, _ := a.Mapper().Resolve(graph.NodeID(i))
		if name == "" {
			continue
		}
		clusters[cid] = append(clusters[cid], name)
	}
	for cid, names := range clusters {
		sort.Strings(names)
		fmt.Printf("  community %d: %v\n", cid, names)
	}
}
