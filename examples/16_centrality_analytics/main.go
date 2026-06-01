// Example 16_centrality_analytics — analyse a small undirected
// network with two centrality metrics: Brandes betweenness
// (structural importance via shortest paths) and label
// propagation (cluster membership).
//
// Sample output: run `go run ./examples/16_centrality_analytics` and capture the
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

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
	"github.com/FlavioCFOliveira/GoGraph/search/community"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the two-cluster bridged network, computes betweenness
// centrality and label-propagation communities over it, and writes the
// report to w. All output goes to w so a test can capture and assert
// it; run returns wrapped errors rather than terminating the process.
//
// Both rankings are emitted in a fully deterministic order: the
// betweenness ranking sorts by descending score and breaks ties by
// node name, and the cluster listing sorts both the community IDs and
// the member names. This keeps the output byte-stable across runs even
// though the underlying scores contain structural ties.
func run(w io.Writer) error {
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: false})
	for _, e := range [][2]string{
		// Cluster 1 (around "marie")
		{"marie", "pierre"}, {"marie", "anne"}, {"anne", "pierre"},
		// Cluster 2 (around "jose")
		{"jose", "ana"}, {"jose", "luis"}, {"luis", "ana"},
		// The bridge between the two clusters.
		{"marie", "jose"},
	} {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			return fmt.Errorf("AddEdge %s--%s: %w", e[0], e[1], err)
		}
	}
	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	fmt.Fprintln(w, "Betweenness (higher = more critical):")
	bc := centrality.Betweenness(c)
	type entry struct {
		name  string
		score float64
	}
	var entries []entry
	for i, s := range bc {
		name, ok := mapper.Resolve(graph.NodeID(i))
		if !ok || name == "" {
			continue
		}
		entries = append(entries, entry{name, s})
	}
	// Sort by descending score, breaking ties by ascending name so the
	// structurally symmetric nodes (marie/jose) print in a stable order.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		return entries[i].name < entries[j].name
	})
	for _, e := range entries {
		fmt.Fprintf(w, "  %-7s %.2f\n", e.name, e.score)
	}

	fmt.Fprintln(w, "\nLabel propagation clusters:")
	p := community.LabelPropagation(c, community.DefaultLabelPropagationOptions())
	clusters := map[int][]string{}
	for i, cid := range p.Community {
		name, ok := mapper.Resolve(graph.NodeID(i))
		if !ok || name == "" {
			continue
		}
		clusters[cid] = append(clusters[cid], name)
	}
	// Map iteration order is non-deterministic, so collect the community
	// IDs and sort them before printing; member names are sorted too.
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
	return nil
}
