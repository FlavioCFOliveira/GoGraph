package sim

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// The search oracle brings the search package under the DST. Today the DST
// exercises only CRUD; the algorithms in search/ (and its centrality, community
// and flow sub-packages) — the module's headline capability — were never
// invoked by a simulation. This file is the correct-by-construction reference
// machinery that lets the DST run those algorithms over the graph the engine
// actually holds and validate their answers.
//
// # Comparison design
//
// Two independent properties are checked (see [CheckSearch]):
//
//   - Structural parity: the engine's full node-set and (src,dst) edge-set are
//     extracted via the same Cypher read path the workload uses and compared
//     EXACTLY to the oracle's shadow model. This is strictly stronger than the
//     base checker's count-plus-sample probes and needs no new engine-internals
//     API. Because it proves the engine graph is identical to the model, the
//     algorithms can then run on the model (the ground truth) and still be
//     validated against the engine's real contents.
//
//   - Algorithm correctness: each search/ algorithm is run on the oracle graph
//     and its answer is compared to an INDEPENDENT naive reference computed
//     directly from the oracle's edge set (never from the CSR handed to search/,
//     so a CSR-builder bug cannot hide). The reference and the algorithm are
//     compared on an INVARIANT of the answer (e.g. the reachable set, the
//     partition up to relabelling) — never a non-unique witness.
//
// All comparisons are bit-deterministic: the dense node labelling is the sorted
// name order, so the same seed always yields the same graphs, CSRs and answers.

// nameGraph is a directed graph keyed by the canonical Person name of each node.
// The dense index of a node is its position in the sorted, de-duplicated name
// list, so the labelling is a pure function of the node-name set — identical for
// the oracle graph and the engine graph whenever their node-sets match. This is
// the shared bijection every search check builds on.
//
// # Concurrency contract
//
// nameGraph is not safe for concurrent use; it is built and read from the single
// simulation goroutine.
type nameGraph struct {
	// names are the node names in ascending sorted order; the slice index is the
	// node's dense id used for the CSR and every algorithm.
	names []string
	// idx maps a name to its dense id (its position in names).
	idx map[string]int
	// out[i] holds the sorted, de-duplicated dense ids of the out-neighbours of
	// node i along KNOWS edges. Parallel KNOWS edges collapse to one entry,
	// matching the oracle's (src,dst,label)-keyed edge model.
	out [][]int
	// sawUnknownEndpoint records whether an extracted edge referenced an endpoint
	// absent from the node set (a structural anomaly the engine extraction must
	// never produce); [CheckSearch] surfaces it as a violation.
	sawUnknownEndpoint bool
}

// newNameGraph builds an empty graph over the given names. The names are sorted
// and de-duplicated so the dense labelling is canonical regardless of the order
// the caller supplied them in.
func newNameGraph(names []string) *nameGraph {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	idx := make(map[string]int, len(sorted))
	for i, n := range sorted {
		idx[n] = i
	}
	return &nameGraph{
		names: sorted,
		idx:   idx,
		out:   make([][]int, len(sorted)),
	}
}

// addEdgeByName records a directed KNOWS edge between two named nodes. An edge
// whose source or destination is not in the node set is dropped and flagged via
// sawUnknownEndpoint, since a well-formed graph never references an absent node.
func (g *nameGraph) addEdgeByName(src, dst string) {
	i, okI := g.idx[src]
	j, okJ := g.idx[dst]
	if !okI || !okJ {
		g.sawUnknownEndpoint = true
		return
	}
	g.out[i] = append(g.out[i], j)
}

// finalize sorts and de-duplicates every adjacency list so the structure is a
// canonical function of the edge set (independent of insertion order) and
// parallel edges collapse to one, matching the oracle's edge model.
func (g *nameGraph) finalize() {
	for i := range g.out {
		if len(g.out[i]) <= 1 {
			continue
		}
		sort.Ints(g.out[i])
		g.out[i] = slices.Compact(g.out[i])
	}
}

// edgeKeys returns every directed edge as a sorted slice of "src\x00dst" keys.
// It is the canonical edge-set form the structural-parity check compares; using
// names (not dense ids) keeps it well-defined even when two graphs disagree on
// their node-sets.
func (g *nameGraph) edgeKeys() []string {
	var keys []string
	for i, nbrs := range g.out {
		for _, j := range nbrs {
			keys = append(keys, g.names[i]+"\x00"+g.names[j])
		}
	}
	sort.Strings(keys)
	return keys
}

// toCSR materialises the graph as an immutable CSR over float64 weights (nil
// weights for the unweighted connectivity checks; the weighted scenarios fill
// the parallel column in a later task). The dense labelling is preserved so a
// NodeID in the CSR is the node's index in names.
func (g *nameGraph) toCSR() *csr.CSR[float64] {
	n := len(g.names)
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	vertices := make([]uint64, n+1)
	var total uint64
	for i := 0; i < n; i++ {
		vertices[i] = total
		total += uint64(len(g.out[i]))
	}
	vertices[n] = total
	edges := make([]graph.NodeID, 0, total)
	for i := 0; i < n; i++ {
		for _, j := range g.out[i] {
			edges = append(edges, graph.NodeID(j))
		}
	}
	return csr.FromArrays[float64](vertices, edges, nil, uint64(n), total)
}

// checkSources returns a small, deterministic set of source node ids the
// reachability checks start from: the first, middle and last nodes by dense id.
// A handful of well-spread sources exercises the traversal code without paying
// the O(V*(V+E)) cost of starting from every node.
func (g *nameGraph) checkSources() []int {
	n := len(g.names)
	switch n {
	case 0:
		return nil
	case 1:
		return []int{0}
	case 2:
		return []int{0, 1}
	default:
		return []int{0, n / 2, n - 1}
	}
}

// naiveReachable returns the sorted dense ids reachable from src along directed
// edges, computed by a textbook BFS over the adjacency lists. It is the
// independent reference the search-package BFS/DFS reachability is compared to.
func (g *nameGraph) naiveReachable(src int) []int {
	n := len(g.names)
	if src < 0 || src >= n {
		return nil
	}
	seen := make([]bool, n)
	seen[src] = true
	queue := []int{src}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, v := range g.out[u] {
			if !seen[v] {
				seen[v] = true
				queue = append(queue, v)
			}
		}
	}
	return boolsToSortedIDs(seen)
}

// naiveWCC returns a component label per node computed by a textbook union-find
// over the SYMMETRIC closure of the edge set (weak connectivity). Labels are the
// final union-find roots; their absolute values are irrelevant — only the
// induced partition is compared (see [componentPartitionSig]). Isolated nodes
// remain their own root, i.e. singleton blocks.
func (g *nameGraph) naiveWCC() []int {
	n := len(g.names)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for i := 0; i < n; i++ {
		for _, j := range g.out[i] {
			union(i, j)
		}
	}
	labels := make([]int, n)
	for i := range labels {
		labels[i] = find(i)
	}
	return labels
}

// boolsToSortedIDs converts a visited bitmap to the sorted slice of set ids.
func boolsToSortedIDs(seen []bool) []int {
	out := make([]int, 0)
	for i, ok := range seen {
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// componentPartitionSig renders a component labelling as a canonical signature
// of the partition it induces, independent of the label values. Two labellings
// produce the same signature iff they group the nodes into the same blocks. A
// label < 0 (the search package's "isolated/ghost" marker) is treated as a
// unique singleton, so it compares equal to a textbook reference that gives an
// isolated node its own ordinary label.
func componentPartitionSig(labels []int) string {
	blocks := make(map[int][]int, len(labels))
	nextSingleton := -1
	for i, l := range labels {
		key := l
		if l < 0 {
			key = nextSingleton
			nextSingleton--
		}
		blocks[key] = append(blocks[key], i)
	}
	ordered := make([][]int, 0, len(blocks))
	for _, members := range blocks {
		sort.Ints(members)
		ordered = append(ordered, members)
	}
	sort.Slice(ordered, func(a, b int) bool { return ordered[a][0] < ordered[b][0] })
	var sb strings.Builder
	for bi, members := range ordered {
		if bi > 0 {
			sb.WriteByte(';')
		}
		for mi, m := range members {
			if mi > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(strconv.Itoa(m))
		}
	}
	return sb.String()
}

// oracleNameGraph builds the name-keyed graph from the oracle's shadow model.
// It is the ground-truth graph: correct by construction, since the oracle only
// ever advances on a committed write.
func oracleNameGraph(o *GraphOracle) *nameGraph {
	g := newNameGraph(o.NodeNames())
	for _, e := range o.edgeStates() {
		if e.Label != knowsLabel {
			continue
		}
		src := o.nameOf(e.SrcID)
		dst := o.nameOf(e.DstID)
		if src == "" || dst == "" {
			continue
		}
		g.addEdgeByName(src, dst)
	}
	g.finalize()
	return g
}

// engineNameGraph extracts the name-keyed graph from the live engine using only
// the public Cypher read path — the same path the workload uses — so it adds no
// engine-internals surface and carries no isolation risk in the single-goroutine
// loop. It reads every Person name, then every KNOWS edge by endpoint name.
func engineNameGraph(engine Engine) (*nameGraph, error) {
	names, err := engineNodeNames(engine)
	if err != nil {
		return nil, err
	}
	g := newNameGraph(names)

	res, err := engine.Run(context.Background(), queryExtractKnowsEdges, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		src, okS := res.StringAt(0)
		dst, okD := res.StringAt(1)
		if !okS || !okD {
			continue
		}
		g.addEdgeByName(src, dst)
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	g.finalize()
	return g, nil
}

// engineNodeNames reads every Person name from the engine.
func engineNodeNames(engine Engine) ([]string, error) {
	res, err := engine.Run(context.Background(), queryExtractPersonNames, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	var names []string
	for res.Next() {
		if name, ok := res.StringAt(0); ok {
			names = append(names, name)
		}
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// knowsLabel is the relationship type the search workload and oracle model.
const knowsLabel = "KNOWS"

// The two extraction queries read the whole Person/KNOWS graph through the public
// engine read path. They are package constants so the extractor and any test
// fake agree on the exact text.
const (
	queryExtractPersonNames = "MATCH (n:Person) RETURN n.name"
	queryExtractKnowsEdges  = "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name, b.name"
)
