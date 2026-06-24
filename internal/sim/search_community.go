package sim

import (
	"fmt"
	"slices"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/community"
)

// communitySeedSalt is XORed into the tick before deriving this checker's Seed
// so the COMMUNITY-DETECTION battery draws an independent random stream from the
// other per-tick search checks (which salt the same tick with their own
// distinct constants). Sharing the raw tick would couple the fixtures of
// unrelated checks.
const communitySeedSalt uint64 = 0xc0117_de7_ec7_9a11

// communityMinCliques and communityMaxCliques bound how many cliques the
// seed-derived planted fixture stitches together: between 2 and 4 cliques joined
// by single bridge edges, keeping every per-tick fixture small and the check
// cost O(V + E) on a graph whose node count never exceeds a few dozen.
const (
	communityMinCliques = 2
	communityMaxCliques = 4
)

// communityMinCliqueSize and communityMaxCliqueSize bound each planted clique's
// order. A clique must hold at least 3 nodes so that, after a single bridge edge
// is added between consecutive cliques, every intra-clique edge still
// outnumbers the lone inter-clique edge — keeping the planted communities the
// unambiguous modularity optimum that Leiden must recover.
const (
	communityMinCliqueSize = 3
	communityMaxCliqueSize = 6
)

// communityEdge is a single undirected edge {A, B} of a planted fixture,
// expressed in the integer NodeID space the CSR builder consumes. A < B is not
// required; communityBuildCSR symmetrises every edge into both directions.
type communityEdge struct {
	A graph.NodeID
	B graph.NodeID
}

// communityFixture is one deterministic undirected graph the community-detection
// battery probes. It carries the symmetric edge list, the count of distinct
// NodeIDs it spans (dense in [0, order)), a human label for diagnostics, and the
// planted ground-truth community of every node (block[i] is the planted
// community id of NodeID i). Because every clique has >= 3 fully-connected nodes
// and consecutive cliques are joined by a single bridge edge, the planted blocks
// are the unique modularity-maximising partition, so Leiden must recover them
// exactly (up to relabelling).
type communityFixture struct {
	name  string
	order int // number of distinct NodeIDs, 0..order-1, all live
	edges []communityEdge
	block []int // planted community id per NodeID, length order
}

// communityViolations runs the COMMUNITY-DETECTION correctness battery for one
// simulation tick. Community detection is HEURISTIC — there is no unique
// ground-truth partition for an arbitrary graph — so the battery does not
// compare against one canonical answer. Instead it asserts the properties that
// any correct implementation must satisfy:
//
//   - DETERMINISM: Leiden's godoc promises bit-for-bit reproducible output on
//     the same input; the checker runs it twice on the same graph and requires
//     an identical Partition. LabelPropagation is likewise documented
//     deterministic (ties broken by the smaller label id, nodes visited in
//     NodeID order), so its output is checked the same way.
//   - VALID PARTITION: every live node is assigned exactly one community id in
//     [0, NumCommunities), the labelling is dense (every id in that range is
//     used), and ghost slots (none on these dense fixtures) hold the sentinel
//     -1. Checked for both algorithms.
//   - PLANTED RECOVERY (the real oracle): on a graph with unambiguous community
//     structure — 2..4 cliques joined by a single bridge edge between
//     consecutive cliques — Leiden must never SPLIT a planted clique across
//     communities (a clique is internally maximally dense, so splitting it would
//     lower modularity). It may legitimately MERGE adjacent cliques: that is the
//     modularity resolution limit (Fortunato & Barthélemy, PNAS 2007), not a bug.
//     The check therefore asserts "no clique is split", which holds for every
//     Leiden result, rather than exact recovery, which does not.
//   - MODULARITY SANITY: the recovered partition's modularity must be at least
//     the all-singletons partition's modularity (a partition Leiden must never
//     do worse than). The package exposes no Modularity function, so the
//     checker computes Q itself with the same formula the package uses
//     internally.
//
// All randomness flows from a single Seed derived from tick, no map-iteration
// order influences any output, and node ids are plain integers, so a violation
// at a given tick is reproducible bit-for-bit. A clean run returns nil;
// divergences are returned tagged ViolationSearchDivergence with Op
// "search:Leiden" or "search:LabelPropagation".
func communityViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ communitySeedSalt)

	var vs []Violation
	for _, f := range communityFixtures(seed) {
		c := communityBuildCSR(f)
		n := f.order

		// --- Leiden ---
		p1 := community.Leiden(c, community.DefaultLeidenOptions())
		// DETERMINISM: a second run on the same graph must be bit-identical.
		p2 := community.Leiden(c, community.DefaultLeidenOptions())
		if !slices.Equal(p1.Community, p2.Community) || p1.NumCommunities != p2.NumCommunities {
			vs = append(vs, communityDiverge(tick, "Leiden", fmt.Sprintf(
				"%s: Leiden is documented bit-for-bit deterministic but two runs on the same graph disagreed",
				f.name)))
		}
		// VALID PARTITION.
		vs = appendIfErr(vs, tick, "Leiden", f.name, validatePartition(p1, n))
		// PLANTED RECOVERY (up to relabelling).
		vs = appendIfErr(vs, tick, "Leiden", f.name, checkPlantedRecovery(p1, f))
		// MODULARITY SANITY: at least the all-singletons baseline.
		vs = appendIfErr(vs, tick, "Leiden", f.name, checkModularityFloor(c, p1, n))

		// --- LabelPropagation (documented deterministic) ---
		lp1 := community.LabelPropagation(c, community.DefaultLabelPropagationOptions())
		lp2 := community.LabelPropagation(c, community.DefaultLabelPropagationOptions())
		if !slices.Equal(lp1.Community, lp2.Community) || lp1.NumCommunities != lp2.NumCommunities {
			vs = append(vs, communityDiverge(tick, "LabelPropagation", fmt.Sprintf(
				"%s: LabelPropagation is documented deterministic (smaller-label tie-break, NodeID-order visits) but two runs disagreed",
				f.name)))
		}
		// LabelPropagation is also expected to return a valid partition. It is a
		// heuristic with no recovery guarantee on every graph (its fixed-point
		// can oscillate on symmetric structure), so we do NOT assert planted
		// recovery for it — only partition validity, which any correct
		// implementation must satisfy.
		vs = appendIfErr(vs, tick, "LabelPropagation", f.name, validatePartition(lp1, n))
	}
	return vs
}

// communityFixtures returns the deterministic battery of planted graphs for one
// tick: a fixed two-clique graph (a hand-checkable baseline), a fixed
// three-clique chain, and one seed-derived multi-clique chain whose clique count
// and per-clique sizes the seed varies tick to tick. The order is fixed and
// independent of any map iteration; only the last fixture consumes random draws,
// so the whole battery replays from the seed alone.
func communityFixtures(seed *Seed) []communityFixture {
	return []communityFixture{
		communityCliqueChain("two-cliques-3-3", []int{3, 3}),
		communityCliqueChain("three-cliques-4-3-5", []int{4, 3, 5}),
		communityRandomCliqueChain(seed),
	}
}

// communityCliqueChain builds a chain of fully-connected cliques whose sizes are
// given by sizes (sizes[i] is the order of clique i), with a single undirected
// bridge edge joining the last node of clique i to the first node of clique
// i+1. NodeIDs are dense in [0, sum(sizes)); clique i owns the contiguous block
// of NodeIDs that follows the cliques before it, and block[node] is i for every
// node of clique i. Each clique must have >= 2 nodes so every node is live, and
// the chain uses single bridges so the cliques stay the unambiguous community
// structure.
func communityCliqueChain(name string, sizes []int) communityFixture {
	var edges []communityEdge
	var block []int
	base := 0
	bases := make([]int, len(sizes))
	for ci, sz := range sizes {
		bases[ci] = base
		// Intra-clique edges: complete graph within [base, base+sz).
		for i := base; i < base+sz; i++ {
			block = append(block, ci)
			for j := i + 1; j < base+sz; j++ {
				edges = append(edges, communityEdge{A: graph.NodeID(i), B: graph.NodeID(j)})
			}
		}
		base += sz
	}
	// Inter-clique bridge edges: last node of clique i -> first node of i+1.
	for ci := 0; ci+1 < len(sizes); ci++ {
		last := bases[ci] + sizes[ci] - 1
		first := bases[ci+1]
		edges = append(edges, communityEdge{A: graph.NodeID(last), B: graph.NodeID(first)})
	}
	return communityFixture{name: name, order: base, edges: edges, block: block}
}

// communityRandomCliqueChain builds a seed-derived clique chain: a seed-chosen
// number of cliques in [communityMinCliques, communityMaxCliques], each of a
// seed-chosen order in [communityMinCliqueSize, communityMaxCliqueSize], joined
// by single bridges. The structure is well-separated by construction (dense
// cliques, lone bridges), so it carries the same no-clique-split guarantee as
// the fixed fixtures while varying the shape tick to tick.
func communityRandomCliqueChain(seed *Seed) communityFixture {
	k := communityMinCliques + seed.IntN(communityMaxCliques-communityMinCliques+1)
	sizes := make([]int, k)
	for i := range sizes {
		sizes[i] = communityMinCliqueSize + seed.IntN(communityMaxCliqueSize-communityMinCliqueSize+1)
	}
	total := 0
	for _, s := range sizes {
		total += s
	}
	return communityCliqueChain(fmt.Sprintf("random-chain-%dcliques-%dnodes", k, total), sizes)
}

// --- CSR construction ----------------------------------------------------------

// communityBuildCSR materialises f as an immutable, SYMMETRIC (undirected)
// CSR[float64]. Community detection operates on undirected graphs, so every
// undirected edge {A, B} of the fixture is emitted as two directed CSR entries
// (A -> B and B -> A); the resulting CSR satisfies IsSymmetric, which is the
// canonical undirected representation the algorithms expect. Node ids are dense
// in [0, order) and every node of a >= 2 clique has at least one incident edge,
// so LiveMask is all-true and the partition carries no ghost (-1) slots.
//
// The offsets array is built programmatically via a counting pass over the
// symmetrised out-degrees followed by a scatter, exactly as csr.BuildFromAdjList
// would, which sidesteps the off-by-one offset bugs that hand-written CSR arrays
// invite. order must be strictly greater than every NodeID that appears in any
// edge.
func communityBuildCSR(f communityFixture) *csr.CSR[float64] {
	order := f.order
	// Symmetrise: one undirected edge contributes one out-edge to each endpoint.
	vertices := make([]uint64, order+1)
	for _, e := range f.edges {
		vertices[int(e.A)+1]++
		vertices[int(e.B)+1]++
	}
	for i := 1; i <= order; i++ {
		vertices[i] += vertices[i-1] // prefix sum -> offsets
	}
	size := uint64(len(f.edges)) * 2
	edges := make([]graph.NodeID, size)
	cursor := make([]uint64, order)
	emit := func(src, dst graph.NodeID) {
		s := int(src)
		pos := vertices[s] + cursor[s]
		edges[pos] = dst
		cursor[s]++
	}
	for _, e := range f.edges {
		emit(e.A, e.B)
		emit(e.B, e.A)
	}
	// Unit weights (float64 1.0) on every directed slot: the modularity floor
	// check reads weights through c.Size() as the doubled total edge weight, so
	// a non-nil weights array keeps the CSR self-describing. The community
	// algorithms ignore weights (they treat the graph as unweighted), so the
	// values only matter to this checker's own modularity computation.
	weights := make([]float64, size)
	for i := range weights {
		weights[i] = 1.0
	}
	return csr.FromArrays[float64](vertices, edges, weights, uint64(order), size)
}

// --- Partition validity --------------------------------------------------------

// validatePartition asserts that p is a well-formed partition of a dense
// n-node graph: the Community slice has length exactly MaxNodeID() (== n here),
// every live slot holds a community id in [0, NumCommunities), the labelling is
// dense (every id in [0, NumCommunities) is used by at least one node), and —
// since these fixtures are dense with no ghost slots — no slot holds the -1
// sentinel. Returns nil when the partition is valid.
func validatePartition(p community.Partition, n int) error {
	if len(p.Community) != n {
		return fmt.Errorf("partition has %d slots, want MaxNodeID()=%d", len(p.Community), n)
	}
	if p.NumCommunities <= 0 {
		return fmt.Errorf("NumCommunities = %d, want >= 1 for a non-empty graph", p.NumCommunities)
	}
	used := make([]bool, p.NumCommunities)
	for id, cid := range p.Community {
		switch {
		case cid < 0:
			return fmt.Errorf("node %d carries the ghost sentinel %d but every node of a dense clique chain is live", id, cid)
		case cid >= p.NumCommunities:
			return fmt.Errorf("node %d carries community id %d out of range [0, %d)", id, cid, p.NumCommunities)
		default:
			used[cid] = true
		}
	}
	for cid, ok := range used {
		if !ok {
			return fmt.Errorf("community id %d in [0, %d) is unused: labelling is not dense", cid, p.NumCommunities)
		}
	}
	return nil
}

// --- Planted recovery (up to relabelling) --------------------------------------

// checkPlantedRecovery asserts the SOUND planted-recovery invariant: Leiden never
// SPLITS a planted clique across communities. A clique is internally maximally
// dense, so any partition that split it would have strictly lower modularity;
// Leiden therefore keeps every clique whole. It may, however, MERGE adjacent
// cliques — that is not a bug but the modularity resolution limit (Fortunato &
// Barthélemy, PNAS 2007): when a clique is small relative to the total edge
// count, folding it into a neighbour raises modularity. Asserting "exact
// recovery" (recovered community count == planted clique count) is therefore
// unsound and produces false positives on chains of small cliques; "no clique is
// split" is the invariant that holds for every Leiden result. Returns nil when
// no planted clique is split.
func checkPlantedRecovery(p community.Partition, f communityFixture) error {
	blockComm := make(map[int]int) // planted clique id -> the recovered community its nodes share
	for i, b := range f.block {
		if b < 0 || i >= len(p.Community) {
			continue
		}
		comm := p.Community[i]
		if prev, ok := blockComm[b]; ok {
			if prev != comm {
				return fmt.Errorf("planted clique %d was split across recovered communities %d and %d", b, prev, comm)
			}
		} else {
			blockComm[b] = comm
		}
	}
	return nil
}

// canonicalPartitionSig maps a label slice to a canonical, relabelling-invariant
// signature: each node is assigned the MINIMUM node index that shares its label.
// Two label slices induce the same partition (the same grouping of nodes) if and
// only if their canonical signatures are element-wise equal, regardless of how
// the labels themselves are numbered. Negative (ghost) labels map to -1 and so
// only match other ghosts at the same position. The result depends only on the
// input order — no map iteration influences it.
func canonicalPartitionSig(labels []int) []int {
	rep := make(map[int]int, len(labels)) // label -> representative (min index seen)
	for i, l := range labels {
		if l < 0 {
			continue
		}
		if _, ok := rep[l]; !ok {
			rep[l] = i // first index for this label is its minimum index
		}
	}
	sig := make([]int, len(labels))
	for i, l := range labels {
		if l < 0 {
			sig[i] = -1
			continue
		}
		sig[i] = rep[l]
	}
	return sig
}

// --- Modularity sanity ---------------------------------------------------------

// checkModularityFloor asserts that the partition's modularity Q is at least the
// all-singletons partition's modularity (each live node its own community) on
// the same graph, within a small floating-point tolerance. A correct
// modularity-optimising algorithm must never return a partition worse than the
// trivial singleton baseline. The package exposes no Modularity function, so the
// checker computes Q itself with the same formula the algorithm uses internally
// (unit edge weights; m2 = c.Size(), the CSR-doubled undirected edge count).
// Returns nil when the floor holds.
func checkModularityFloor[W any](c *csr.CSR[W], p community.Partition, n int) error {
	const eps = 1e-9
	qGot := communityModularity(c, p.Community)

	// All-singletons baseline over live nodes; ghost slots stay -1.
	mask := c.LiveMask()
	singleton := make([]int, n)
	next := 0
	for id := 0; id < n; id++ {
		if len(mask) == 0 || (id < len(mask) && mask[id]) {
			singleton[id] = next
			next++
		} else {
			singleton[id] = -1
		}
	}
	qSingle := communityModularity(c, singleton)

	if qGot < qSingle-eps {
		return fmt.Errorf("partition modularity Q=%.6f is below the all-singletons baseline Q=%.6f", qGot, qSingle)
	}
	return nil
}

// communityModularity computes Newman's modularity Q for an undirected CSR and a
// NodeID-indexed partition, skipping slots flagged -1. It mirrors the formula the
// community package uses internally: Q = Σ_c [ σ_in(c)/m2 - (σ_tot(c)/m2)² ],
// where m2 = c.Size() is the CSR-doubled undirected edge count, σ_tot(c) is the
// summed degree of community c, and σ_in(c) is the count of directed edge slots
// whose endpoints both lie in c (which, over the symmetric CSR, double-counts
// each internal undirected edge — exactly as the package's own modularity does).
// All weights are treated as unit (1.0), matching the unweighted algorithms.
func communityModularity[W any](c *csr.CSR[W], comm []int) float64 {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return 0
	}
	m2 := float64(c.Size()) // CSR doubles undirected edges
	if m2 == 0 {
		return 0
	}
	cMax := 0
	for _, x := range comm {
		if x+1 > cMax {
			cMax = x + 1
		}
	}
	sigmaIn := make([]float64, cMax)
	sigmaTot := make([]float64, cMax)
	for v := 0; v < maxID; v++ {
		cv := comm[v]
		if cv < 0 {
			continue
		}
		sigmaTot[cv] += float64(verts[v+1] - verts[v])
		for k := verts[v]; k < verts[v+1]; k++ {
			u := int(edges[k])
			if u >= 0 && u < len(comm) && comm[u] == cv {
				sigmaIn[cv]++
			}
		}
	}
	var q float64
	for ci := 0; ci < cMax; ci++ {
		q += sigmaIn[ci]/m2 - (sigmaTot[ci]/m2)*(sigmaTot[ci]/m2)
	}
	return q
}

// --- Violation plumbing --------------------------------------------------------

// appendIfErr appends a single divergence violation for algo when err is
// non-nil, otherwise returns vs unchanged.
func appendIfErr(vs []Violation, tick int64, algo, fixture string, err error) []Violation {
	if err == nil {
		return vs
	}
	return append(vs, communityDiverge(tick, algo, fmt.Sprintf("%s: %v", fixture, err)))
}

// communityDiverge builds a single community-detection divergence violation
// tagged with the algorithm name in its Op ("search:Leiden" or
// "search:LabelPropagation").
func communityDiverge(tick int64, algo, msg string) Violation {
	return Violation{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}
}

// communitySortedDistinct returns the sorted, de-duplicated non-negative labels
// of a slice. It is used only by tests to render a partition's block ids in a
// stable order for diagnostics; keeping it here (unexported, with the rest of the
// community plumbing) avoids leaking a helper into the broader sim package.
func communitySortedDistinct(labels []int) []int {
	seen := make(map[int]struct{})
	for _, l := range labels {
		if l >= 0 {
			seen[l] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Ints(out)
	return out
}
