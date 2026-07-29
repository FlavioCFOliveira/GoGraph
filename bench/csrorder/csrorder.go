// Package csrorder holds the permanent fixtures and measurement helpers behind
// the destination-ordered CSR neighbour runs delivered in sprint 313 (rmp #2141,
// #2142, #2143) and benchmarked under rmp #2145.
//
// # What is being measured, and why the fixtures look like this
//
// Since #2141 every CSR built by graph/csr has each source's neighbour run
// ordered by the total key (destination, handle), which turns the executor's
// forward-position membership probes from an O(d) scan into an O(log d) binary
// search (#2142). The probes fire on the REVERSE and UNDIRECTED expand path
// ([cypher/exec.Expand.advanceRevEdge]): every reverse slot must locate the
// corresponding forward edge, and that lookup costs O(deg(dst)) in the
// destination's forward run. So a benchmark that only expands OUTWARD would
// measure none of the changed code. The query fixtures here are therefore
// reverse-direction, relationship-type-filtered expands over sources whose
// out-degree is the swept parameter.
//
// # The calibrated figures — and the refuted ones
//
// The authority is docs/design-degree-adaptive-adjacency.md §2.2:
//
//   - the linear/binary crossover is at out-degree ≈ 16;
//   - the win at out-degree 4096 is 6.04x (hit) and 10.86x (miss);
//   - a linear scan costs 0.268 ns/element asymptotically in an L2-resident
//     arena, 0.348 ns/element in a 64 MiB arena.
//
// The figures in docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md §2.4
// (0.659 ns vs 1.865 ns at degree 8; 164 ns vs 5.31 ns at degree 4096, implying
// 0.040 ns/element and a 30.9x win) are REFUTED and are recorded here only as
// superseded values. They are not merely optimistic: 0.040 ns/element is 4.1x
// faster than a branch-free unrolled accumulate can run in Go, so the number is
// physically unattainable. Nothing in this package may quote them as current.
//
// # Why the primary fixture is power-law and not RMAT
//
// §2.4 measured that at a threshold of 64, RMAT (scale 16, edgeFactor 16) puts
// 97.78% of linear-scan cost above threshold against Barabási–Albert's 67.18%.
// A change measured only on RMAT therefore looks like a triumph and then fails
// to reproduce on a real property graph. [PowerLawFixture] is the primary and
// RMAT is carried only as the contrast that makes the trap visible. Every
// benchmark in this package reports the degree distribution of the fixture it
// ran on, via [DegreeProfile] and b.ReportMetric, so a result can never be read
// without the skew that produced it.
//
// # Layers
//
// Nothing here runs as part of the short test layer beyond the fixture
// self-checks and the distribution report, both of which use bounded sizes.
// Fixtures are built lazily and memoised, so `go test ./bench/csrorder/...`
// without -bench pays only for what the tests touch.
//
// Run with:
//
//	go test -bench=. -benchmem -count=10 ./bench/csrorder/...
package csrorder

import (
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// SweptDegrees is the out-degree sweep required by rmp #2145: it straddles the
// calibrated crossover of 16 (8 below, 16 at parity, 32 above) and then reaches
// the two high degrees the design document quotes ratios for.
//
// 4096 is deliberately included even though no real property graph reaches it —
// it is the degree §2.2 reports 6.04x/10.86x at, so it is the point where this
// benchmark can be checked against the calibration. The realistic regime is the
// 8-64 band, which is exactly why results must be read per degree.
var SweptDegrees = []int{8, 16, 32, 64, 512, 4096}

// TargetSpace is the number of :Target nodes every hub fixture draws its
// destinations from. It bounds the swept degree (a source needs `degree`
// DISTINCT destinations) and is a power of two so any odd stride is coprime with
// it, which is what makes the destination picker below produce distinct
// destinations without a rejection loop.
const TargetSpace = 1 << 16

// HubFixtureArcs is the total arc count every hub fixture is built to, held
// CONSTANT across the degree sweep so that per-degree numbers are comparable.
//
// This is the load-bearing design choice of the sweep. The reverse expand
// performs one forward-position probe per reverse slot, so total probe COUNT is
// the arc count; holding it fixed means the only thing varying across the sweep
// is the cost of an individual probe, which is precisely the quantity #2141
// changed. Letting the arc count grow with degree instead would confound the
// per-probe cost with the number of probes and make the low-degree
// no-regression claim unreadable.
const HubFixtureArcs = 512 << 10

// destStride is the odd stride used to scatter each hub's destinations across
// [TargetSpace]. Insertion order matters: the ordering pass only does real work
// when the arriving destinations are NOT already ascending, so a sequential
// destination run would make the ordered build look free and the probe look
// perfectly predictable. An odd stride over a power-of-two space is a full-cycle
// permutation, so `degree` successive steps are distinct for any degree up to
// TargetSpace.
const destStride = 40_507 // odd, and not a small factor of TargetSpace

// Fixture is one built graph plus the degree profile that describes it. The
// profile travels WITH the graph so no benchmark can report a timing without the
// skew that produced it.
type Fixture struct {
	// Name identifies the fixture in benchmark names and reports.
	Name string
	// Graph is the string/float64 LPG the Cypher engine runs against.
	Graph *lpg.Graph[string, float64]
	// Profile is the measured out-degree distribution of Graph.
	Profile DegreeProfile
}

// DegreeProfile is the measured out-degree distribution of a fixture.
//
// The three fractions are the quantities docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md
// §2.4 compared RMAT against Barabási–Albert with, recomputed here so the
// comparison is permanent rather than a throwaway test:
//
//   - VertexFrac — share of arc-bearing sources whose out-degree exceeds the
//     threshold;
//   - EdgeFrac — share of arcs leaving those sources;
//   - CostFrac — share of Σd² contributed by those sources. Σd² is the
//     linear-scan cost model (probing a source once per out-edge costs d each
//     time), so CostFrac is the share of probe cost a threshold would capture.
//     It is the only one of the three that predicts a speed-up, and it is always
//     the largest, which is why quoting VertexFrac alone understates the win and
//     quoting CostFrac alone overstates how much of the graph is involved.
type DegreeProfile struct {
	// Threshold is the out-degree the three fractions are computed above.
	Threshold int
	// Sources is the number of nodes with at least one out-arc.
	Sources uint64
	// Nodes is the number of interned nodes, INCLUDING those with no out-arcs.
	Nodes uint64
	// Arcs is the total out-arc count.
	Arcs uint64
	// MaxDegree is the largest out-degree observed.
	MaxDegree int
	// MeanDegree is Arcs/Sources — the mean over ARC-BEARING sources, 0 when
	// there are none.
	MeanDegree float64
	// MeanDegreeAllNodes is Arcs/Nodes — the mean over EVERY interned node,
	// including those with no out-arcs.
	//
	// Both means are carried because the two definitions diverge sharply on a
	// directed skewed graph and agree exactly on an undirected one, which makes
	// the difference easy to miss and then to misattribute. Measured here:
	// docs/design-degree-adaptive-adjacency.md §2.4's "avg out" column is this
	// all-nodes mean. On its Barabási–Albert rows the two agree to 16.00, because
	// the generator is undirected so every node carries at least m0 arcs; on its
	// RMAT scale=16 ef=16 row they differ by 1.62x (14.58 all-nodes against 23.66
	// over sources) because 25 149 of the 65 536 nodes end up with no out-arc at
	// all. Reporting only one of them would make a reproduction of §2.4 look
	// broken when it is not.
	MeanDegreeAllNodes float64
	// P50, P90, P99 are out-degree percentiles over arc-bearing sources.
	P50, P90, P99 int
	// SumSqDegree is Σd² over all sources — the linear-scan cost model.
	SumSqDegree uint64
	// VertexFrac, EdgeFrac, CostFrac are the shares above Threshold, in [0,1].
	VertexFrac, EdgeFrac, CostFrac float64
}

// String renders the profile as one compact line suitable for a report table.
func (p DegreeProfile) String() string {
	return fmt.Sprintf(
		"T=%-5d nodes=%-7d sources=%-7d arcs=%-8d max=%-6d meanSrc=%6.2f meanAll=%6.2f "+
			"p50=%-5d p90=%-5d p99=%-6d vertexFrac=%6.2f%% edgeFrac=%6.2f%% costFrac=%6.2f%%",
		p.Threshold, p.Nodes, p.Sources, p.Arcs, p.MaxDegree,
		p.MeanDegree, p.MeanDegreeAllNodes,
		p.P50, p.P90, p.P99,
		100*p.VertexFrac, 100*p.EdgeFrac, 100*p.CostFrac,
	)
}

// ProfileDegrees computes the out-degree distribution of adj above threshold.
//
// It reads the adjacency directly rather than the CSR so the profile describes
// the graph as stored, independent of any CSR build, and so it can be taken on a
// fixture before deciding what to benchmark.
func ProfileDegrees[N comparable, W any](adj *adjlist.AdjList[N, W], threshold int) DegreeProfile {
	maxID := uint64(adj.MaxNodeID())
	degrees := make([]int, 0, maxID)

	var arcs, sumSq uint64
	var aboveVerts, aboveArcs, aboveSumSq uint64
	maxDeg := 0
	for id := uint64(0); id < maxID; id++ {
		nb, _, _ := adj.LoadEntryH(graph.NodeID(id))
		d := len(nb)
		if d == 0 {
			continue
		}
		degrees = append(degrees, d)
		d64 := uint64(d)
		arcs += d64
		sumSq += d64 * d64
		if d > maxDeg {
			maxDeg = d
		}
		if d > threshold {
			aboveVerts++
			aboveArcs += d64
			aboveSumSq += d64 * d64
		}
	}

	p := DegreeProfile{
		Threshold:   threshold,
		Sources:     uint64(len(degrees)),
		Nodes:       adj.Order(),
		Arcs:        arcs,
		MaxDegree:   maxDeg,
		SumSqDegree: sumSq,
	}
	if p.Nodes > 0 {
		p.MeanDegreeAllNodes = float64(arcs) / float64(p.Nodes)
	}
	if p.Sources > 0 {
		p.MeanDegree = float64(arcs) / float64(p.Sources)
		sort.Ints(degrees)
		p.P50 = percentile(degrees, 50)
		p.P90 = percentile(degrees, 90)
		p.P99 = percentile(degrees, 99)
		p.VertexFrac = float64(aboveVerts) / float64(p.Sources)
	}
	if arcs > 0 {
		p.EdgeFrac = float64(aboveArcs) / float64(arcs)
	}
	if sumSq > 0 {
		p.CostFrac = float64(aboveSumSq) / float64(sumSq)
	}
	return p
}

// percentile returns the q-th percentile of an ASCENDING slice using the
// nearest-rank convention. sorted must be non-empty.
func percentile(sorted []int, q int) int {
	idx := (q * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// hubKey and targetKey name the two node sets of a hub fixture. They are kept in
// DISJOINT namespaces so a node is never both a probe source and a probe target,
// which would make the measured out-degree differ from the swept parameter.
func hubKey(i int) string    { return "h" + itoa(i) }
func targetKey(i int) string { return "t" + itoa(i) }

// itoa is strconv.Itoa under a short local name. It is NOT fmt.Sprintf: the
// projection below calls it once per node and once per arc, so on the power-law
// fixture that is several hundred thousand calls of pure fixture setup, and
// Sprintf's reflection-based path measurably lengthens every benchmark's startup
// without contributing to what is being measured.
func itoa(i int) string { return strconv.Itoa(i) }

// BuildHubGraph builds a fixture of hubs with a CONTROLLED out-degree: exactly
// HubFixtureArcs/degree :Hub nodes, each with `degree` distinct :LINK arcs into
// a shared space of [TargetSpace] :Target nodes.
//
// The controlled fixture is what makes the per-degree sweep possible at all: a
// power-law fixture cannot be asked for a specific out-degree, and no realistic
// generator reaches 4096. [PowerLawFixture] carries the realism; this one
// carries the sweep.
//
// Destinations are scattered by an odd stride rather than laid down
// sequentially, for two reasons: an already-ascending run makes the ordering
// pass a no-op (so the ordered build would look free), and a sequential probe
// key stream is perfectly branch-predicted (so the probe would look free too).
// Both were traps the #2139 calibration hit and had to correct for.
func BuildHubGraph(degree int) (*lpg.Graph[string, float64], error) {
	if degree < 1 || degree > TargetSpace {
		return nil, fmt.Errorf("csrorder: degree must be in [1, %d], got %d", TargetSpace, degree)
	}
	hubs := HubFixtureArcs / degree
	if hubs < 1 {
		hubs = 1
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < TargetSpace; i++ {
		k := targetKey(i)
		if err := g.AddNode(k); err != nil {
			return nil, fmt.Errorf("csrorder: AddNode(%s): %w", k, err)
		}
		if err := g.SetNodeLabel(k, "Target"); err != nil {
			return nil, fmt.Errorf("csrorder: SetNodeLabel(%s): %w", k, err)
		}
	}
	for i := 0; i < hubs; i++ {
		src := hubKey(i)
		if err := g.AddNode(src); err != nil {
			return nil, fmt.Errorf("csrorder: AddNode(%s): %w", src, err)
		}
		if err := g.SetNodeLabel(src, "Hub"); err != nil {
			return nil, fmt.Errorf("csrorder: SetNodeLabel(%s): %w", src, err)
		}
		// Each hub starts at a different base so the fixture does not degenerate
		// into every hub pointing at the same destination sequence.
		base := (i * 7919) % TargetSpace
		for k := 0; k < degree; k++ {
			dst := targetKey((base + k*destStride) % TargetSpace)
			if err := g.AddEdgeLabeled(src, dst, 1, "LINK"); err != nil {
				return nil, fmt.Errorf("csrorder: AddEdgeLabeled(%s,%s): %w", src, dst, err)
			}
		}
	}
	return g, nil
}

// BuildPowerLawGraph projects a Barabási–Albert topology onto the string/float64
// graph the Cypher engine requires.
//
// The topology comes from internal/shapegen's generator rather than a local
// preferential-attachment loop, so the fixture inherits a generator that is
// already property-tested (including a power-law exponent test) instead of
// asserting its own realism. shapegen builds it UNDIRECTED and non-multigraph
// over int keys; the projection walks each node's neighbours and emits a
// directed :LINK arc, so the projected out-degree equals the undirected degree
// and the arc count is twice the generator's edge count.
//
// n is bounded by the generator's own O(n²) construction cost, so this is a
// fixture in the tens of thousands of nodes, not the hundreds of thousands. That
// is adequate and honest: the point of this fixture is the SHAPE of the degree
// distribution, and the shape is what determines CostFrac.
func BuildPowerLawGraph(n, m0 int, seed uint64) (*lpg.Graph[string, float64], error) {
	src, err := shapegen.BarabasiAlbert(n, m0, seed).Build(adjlist.Config{})
	if err != nil {
		return nil, fmt.Errorf("csrorder: BarabasiAlbert(%d,%d): %w", n, m0, err)
	}
	return projectIntGraph(src, n, "Person", "KNOWS")
}

// BuildRMATGraph projects an RMAT topology onto a string/float64 graph.
//
// This fixture exists as the CONTRAST that makes the RMAT trap visible, not as a
// target to optimise for. §2.4 measured RMAT putting 97.78% of scan cost above a
// threshold of 64 against Barabási–Albert's 67.18%; the distribution report
// reproduces that gap so a future reader can see why an RMAT-only result would
// have been misleading.
func BuildRMATGraph(scale, edgeFactor int, seed uint64) (*lpg.Graph[string, float64], error) {
	// The a/b/c/d quadrant weights are the canonical Graph500 tuple
	// (0.57/0.19/0.19/0.05). The generator takes them as integer PERCENT and
	// requires a+b+c+d == 100 exactly, panicking otherwise.
	src, err := shapegen.RMAT(scale, edgeFactor, 57, 19, 19, 5, seed).Build(adjlist.Config{})
	if err != nil {
		return nil, fmt.Errorf("csrorder: RMAT(%d,%d): %w", scale, edgeFactor, err)
	}
	return projectIntGraph(src, 1<<scale, "Person", "KNOWS")
}

// projectIntGraph copies the topology of an int-keyed generator graph into the
// string/float64 graph the Cypher engine requires, labelling every node with
// nodeLabel and every arc with relType.
//
// Neighbours are read through the adjacency's key-based iterator, so the
// projection never has to resolve a NodeID back to a key. Keys are the
// generator's contiguous 0..n-1 range, which is why n is passed in rather than
// discovered.
func projectIntGraph(src *lpg.Graph[int, int64], n int, nodeLabel, relType string) (*lpg.Graph[string, float64], error) {
	dstGraph := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := itoa(i)
		if err := dstGraph.AddNode(k); err != nil {
			return nil, fmt.Errorf("csrorder: project AddNode(%s): %w", k, err)
		}
		if err := dstGraph.SetNodeLabel(k, nodeLabel); err != nil {
			return nil, fmt.Errorf("csrorder: project SetNodeLabel(%s): %w", k, err)
		}
	}
	adj := src.AdjList()
	for i := 0; i < n; i++ {
		from := itoa(i)
		for dst := range adj.Neighbours(i) {
			if err := dstGraph.AddEdgeLabeled(from, itoa(dst), 1, relType); err != nil {
				return nil, fmt.Errorf("csrorder: project AddEdgeLabeled(%s,%d): %w", from, dst, err)
			}
		}
	}
	return dstGraph, nil
}

// UnorderedArrays builds the CSR arrays for adj WITHOUT the ordering pass,
// reproducing exactly passes 1 and 2 of csr.BuildFromAdjList and stopping short
// of its pass 3.
//
// This is the "unordered CSR build" arm of the benchmark. It has to live here
// rather than in graph/csr because ordering is an INVARIANT of every CSR that
// package builds (order.go states it as such), and adding a production switch to
// disable it would create a code path that can hand the executor a snapshot its
// O(log d) probes silently mis-read. The cost of that choice is that this
// function must track csr.buildFromAdjListRaw; TestUnorderedArrays_MatchesOrderedAfterOrdering
// pins the two together by ordering this output and requiring byte equality with
// csr.BuildFromAdjList, so a drift fails a test rather than skewing a number.
//
// The weights column follows the same rule the real build uses for a non-empty
// W: allocate unless the adjacency is in weightless mode.
func UnorderedArrays[N comparable, W any](adj *adjlist.AdjList[N, W]) (
	vertices []uint64, edges []graph.NodeID, weights []W, handles []uint64,
) {
	maxID := uint64(adj.MaxNodeID())
	if maxID == 0 {
		return []uint64{0}, nil, nil, nil
	}
	vertices = make([]uint64, maxID+1)

	var total uint64
	var anyHandles bool
	for id := uint64(0); id < maxID; id++ {
		nb, _, h := adj.LoadEntryH(graph.NodeID(id))
		vertices[id] = total
		total += uint64(len(nb))
		if h != nil {
			anyHandles = true
		}
	}
	vertices[maxID] = total

	edges = make([]graph.NodeID, total)
	if !adj.Weightless() {
		weights = make([]W, total)
	}
	if anyHandles {
		handles = make([]uint64, total)
	}
	for id := uint64(0); id < maxID; id++ {
		nb, ws, h := adj.LoadEntryH(graph.NodeID(id))
		if len(nb) == 0 {
			continue
		}
		start := vertices[id]
		copy(edges[start:], nb)
		if weights != nil {
			copy(weights[start:], ws)
		}
		if handles != nil {
			copy(handles[start:], h)
		}
	}
	return vertices, edges, weights, handles
}

// ScanFirstDst is the PRE-#2142 linear probe, carried here verbatim as the
// unordered arm's cost model: it returns the position of the first slot in
// [start, end) whose destination is dst.
//
// It is a superseded implementation, kept only so the ordered and unordered
// probe costs can be measured in one binary at one degree sweep. It is NOT a
// second implementation of shipped behaviour: correctness of the shipped probe
// is gated by cypher/exec/csrprobe_test.go, which reproduces this same scan as
// its oracle and differentially compares every probe against it. This copy
// measures cost only.
func ScanFirstDst(edges []graph.NodeID, start, end, dst uint64) (uint64, bool) {
	for i := start; i < end; i++ {
		if uint64(edges[i]) == dst {
			return i, true
		}
	}
	return 0, false
}

// SearchFirstDst is the ordered arm's probe: the same lower-bound binary search
// the executor uses (cypher/exec.lowerBoundDst followed by the equality check of
// firstDstPos). It returns the FIRST slot whose destination is dst, which is the
// property that makes it substitutable for [ScanFirstDst] — several callers
// depend on getting the first of a parallel-edge group.
//
// The branchy form is deliberate. A conditional-move variant was measured during
// the #2139 calibration and is slower in this regime: on a memory-bound search
// the branch predictor acts as a prefetcher, so removing the branch removes the
// speculation and lengthens the chain of dependent loads.
func SearchFirstDst(edges []graph.NodeID, start, end, dst uint64) (uint64, bool) {
	lo, hi := start, end
	for lo < hi {
		mid := lo + (hi-lo)/2
		if uint64(edges[mid]) < dst {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < end && uint64(edges[lo]) == dst {
		return lo, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Lazy, memoised fixtures
// ---------------------------------------------------------------------------

// PowerLaw* pins the Barabási–Albert fixture's parameters.
//
// n is held at 20 000 because shapegen's generator is O(n²) by construction (it
// rebuilds a cumulative-degree array at every step), so a larger n would spend
// the benchmark's startup budget without changing the SHAPE of the degree
// distribution, which is the only property this fixture is chosen for. m0=8 gives
// a mean projected out-degree of ~16 — the calibrated crossover itself, which is
// the most informative place for a realistic fixture to sit.
//
// §2.4's own rows are reproduced separately, at their original parameters, by the
// soak-layer distribution test; this constant is the benchmark fixture, not the
// reproduction.
const (
	PowerLawN    = 20_000
	PowerLawM0   = 8
	PowerLawSeed = 0x5133_2141
)

// RMAT* pins the contrast fixture at exactly the configuration
// docs/design-degree-adaptive-adjacency.md §2.4 measured (scale 16, edgeFactor
// 16, costFrac 99.68% at T=16 and 97.78% at T=64), so the RMAT trap is
// reproduced at its published parameters rather than approximated.
const (
	RMATScale      = 16
	RMATEdgeFactor = 16
	RMATSeed       = 0x5133_2145
)

// graphCache memoises built fixture GRAPHS, keyed by a fixture name. Only the
// graph is cached, never the profile: a profile is cheap (one pass over the
// offsets and neighbour lengths) and is a function of the threshold, so caching
// the graph alone lets the same fixture be reported at several thresholds
// without rebuilding, and keeps the cached value immutable and therefore safe to
// share.
//
// Fixtures are built on first use rather than in a TestMain so that
// `go test ./bench/csrorder/...` with no -bench pays only for what its tests
// actually touch, keeping the package inside the short layer's per-package
// budget.
var (
	graphMu    sync.Mutex
	graphCache = map[string]*lpg.Graph[string, float64]{}
)

// cachedGraph returns the memoised graph for name, calling build on first use.
// The build runs while the lock is held, which serialises concurrent first-use
// callers rather than letting them both build — correct, and irrelevant to cost
// because benchmarks are sequential.
func cachedGraph(name string, build func() (*lpg.Graph[string, float64], error)) (*lpg.Graph[string, float64], error) {
	graphMu.Lock()
	defer graphMu.Unlock()
	if g, ok := graphCache[name]; ok {
		return g, nil
	}
	g, err := build()
	if err != nil {
		return nil, err
	}
	graphCache[name] = g
	return g, nil
}

// HubFixture returns the controlled-degree fixture for degree, building it on
// first use and memoising the graph. profileThreshold selects the threshold the
// returned profile's fractions are computed above.
func HubFixture(degree, profileThreshold int) (*Fixture, error) {
	name := fmt.Sprintf("hub-d%d", degree)
	g, err := cachedGraph(name, func() (*lpg.Graph[string, float64], error) {
		return BuildHubGraph(degree)
	})
	if err != nil {
		return nil, err
	}
	return &Fixture{Name: name, Graph: g, Profile: ProfileDegrees(g.AdjList(), profileThreshold)}, nil
}

// PowerLawFixture returns the Barabási–Albert fixture — the PRIMARY fixture for
// every end-to-end claim in this package.
func PowerLawFixture(profileThreshold int) (*Fixture, error) {
	const name = "barabasi-albert"
	g, err := cachedGraph(name, func() (*lpg.Graph[string, float64], error) {
		return BuildPowerLawGraph(PowerLawN, PowerLawM0, PowerLawSeed)
	})
	if err != nil {
		return nil, err
	}
	return &Fixture{Name: name, Graph: g, Profile: ProfileDegrees(g.AdjList(), profileThreshold)}, nil
}

// RMATFixture returns the RMAT contrast fixture. It exists to make the
// RMAT-only trap visible, not as a shape to optimise for.
func RMATFixture(profileThreshold int) (*Fixture, error) {
	const name = "rmat"
	g, err := cachedGraph(name, func() (*lpg.Graph[string, float64], error) {
		return BuildRMATGraph(RMATScale, RMATEdgeFactor, RMATSeed)
	})
	if err != nil {
		return nil, err
	}
	return &Fixture{Name: name, Graph: g, Profile: ProfileDegrees(g.AdjList(), profileThreshold)}, nil
}

// OrderedCSR builds the ordered forward CSR of f exactly as the Cypher engine
// does (live-filtered, so the arc set matches what a query would traverse).
func OrderedCSR(g *lpg.Graph[string, float64]) *csr.CSR[float64] {
	return csr.BuildFromAdjListLive(g.AdjList(), g.LiveNodeFilter())
}
