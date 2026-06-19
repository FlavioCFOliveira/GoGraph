package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// link is one undirected backbone connection between two sites, carrying
// a capacity in Gb/s. The whole network — both the structural reliability
// analysis and the max-flow throughput analysis — is derived from this
// single list, so the two views can never drift apart. The endpoints a
// and b are dense site indices in [0, sites): the same index space the
// flow solver uses, so no NodeID narrowing is ever needed.
type link struct {
	a, b int
	cap  int
}

// network is the materialised backbone: the link list both analyses
// share, the mutable adjacency the structural analysis snapshots, the
// mapper that resolves a site index back to its name, and the source/sink
// indices the flow analysis runs between.
type network struct {
	sites   int    // number of site indices, [0, sites)
	links   []link // every undirected link, in build order
	adj     *adjlist.AdjList[string, int64]
	mapper  *graph.Mapper[string] // site name -> NodeID, for SPOF resolution
	idOf    []graph.NodeID        // site index -> NodeID, for cut resolution
	source  int                   // an interior site of cluster 0
	sink    int                   // an interior site of cluster K-1
	elapsed time.Duration
}

// checkEvery bounds how often generation polls ctx for cancellation:
// often enough that a cancelled large build stops promptly, rare enough
// that the check is free relative to the surrounding work.
const checkEvery = 4096

// generate materialises the deterministic transit-stub backbone described
// by cfg. It honours ctx cancellation on a periodic check. See the
// package doc comment for the topology and the graph-theory rationale.
//
// Layout (cluster c occupies site indices [c*clusterSize, (c+1)*clusterSize)):
//
//	clusters 0 .. K-1 : the spine, a path of dense 2-connected clusters
//	cluster  K        : the stub, joined to spine cluster 0 by ONE bridge link
//
// The source is the first site of cluster 0 and the sink the first site
// of cluster K-1; both end up with intra-degree >= 2 from the Hamiltonian
// cycle alone (>= 3 once chords are added at the default scale), so their
// incident capacity dwarfs any inter-cluster boundary.
func generate(ctx context.Context, cfg config) (*network, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional — the example must
	// reproduce a fixed topology for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	spineSites := cfg.clusters * cfg.clusterSize
	stubBase := spineSites
	totalSites := spineSites + cfg.clusterSize

	net := &network{
		sites:  totalSites,
		adj:    adjlist.New[string, int64](adjlist.Config{Directed: false}),
		mapper: graph.NewMapper[string](),
		idOf:   make([]graph.NodeID, totalSites),
		source: 0,                                    // first site of cluster 0
		sink:   (cfg.clusters - 1) * cfg.clusterSize, // first site of cluster K-1
	}
	net.mapper.Reserve(totalSites)
	net.idOf = net.idOf[:0]

	// Pre-size the link slice from the known upper bound so the append loop
	// never reallocates: per cluster a Hamiltonian cycle (clusterSize) plus
	// chords, the spine boundaries, and one bridge link.
	maxLinks := (cfg.clusters+1)*(cfg.clusterSize+cfg.chords) +
		cfg.clusters*spineLinksWide + 1
	net.links = make([]link, 0, maxLinks)

	// Intern every site name first so the structural CSR snapshot and the
	// flow solver agree on the index space.
	for s := 0; s < totalSites; s++ {
		net.idOf = append(net.idOf, net.mapper.Intern(siteName(s, cfg, stubBase)))
	}

	// Dense clusters: spine clusters 0..K-1 plus the stub cluster at index K.
	for c := 0; c <= cfg.clusters; c++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		baseIdx := c * cfg.clusterSize
		if c == cfg.clusters {
			baseIdx = stubBase
		}
		if err := net.addCluster(ctx, rng, cfg, baseIdx); err != nil {
			return nil, err
		}
	}

	// Spine boundaries: join consecutive spine clusters by a parallel set
	// of inter-cluster links. Exactly one interior boundary is the
	// narrowest (two links) and becomes the global min-cut; the rest are
	// wider (three links).
	if err := net.addSpineBoundaries(cfg); err != nil {
		return nil, err
	}

	// The single off-spine bridge: one link from spine cluster 0 to the
	// stub cluster. Because the cluster graph stays a tree, this link is a
	// bridge and both its endpoints are articulation points. The spine
	// endpoint is deliberately NOT the source site (index 0) — keeping the
	// source a plain interior site rather than itself a single point of
	// failure.
	bridgeSpineSite := cfg.clusterSize / 2 // an interior site of cluster 0, != source
	if err := net.addLink(bridgeSpineSite, stubBase+cfg.clusterSize/2, capBridge); err != nil {
		return nil, err
	}

	net.elapsed = time.Since(start)
	return net, nil
}

// addCluster lays a Hamiltonian cycle over the clusterSize sites starting
// at baseIdx (which GUARANTEES the cluster is 2-vertex-connected: removing
// any one site leaves a path on the rest), then adds cfg.chords random
// chords. Every intra-cluster link gets the high capacity capIntra so no
// minimum cut ever falls inside a cluster. Adding chords keeps the cluster
// 2-connected (open-ear decomposition theorem), so it stays free of
// internal articulation points and bridges for every seed.
func (net *network) addCluster(ctx context.Context, rng *rand.Rand, cfg config, baseIdx int) error {
	s := cfg.clusterSize
	for i := 0; i < s; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		j := (i + 1) % s
		if err := net.addLink(baseIdx+i, baseIdx+j, capIntra); err != nil {
			return err
		}
	}
	// Random chords. A chord is rejected only when it is a self-loop or
	// duplicates the cycle/another chord; any accepted chord set keeps the
	// cluster 2-connected.
	added := 0
	for attempts := 0; added < cfg.chords && attempts < cfg.chords*8; attempts++ {
		u := baseIdx + rng.Intn(s)
		v := baseIdx + rng.Intn(s)
		if u == v || net.hasLink(u, v) {
			continue
		}
		if err := net.addLink(u, v, capIntra); err != nil {
			return err
		}
		added++
	}
	return nil
}

// addSpineBoundaries joins consecutive spine clusters with parallel
// inter-cluster links. One interior boundary (the middle one) is given
// exactly spineLinksNarrow links and becomes the global source-to-sink
// min-cut; every other boundary gets spineLinksWide links. Because the
// cluster graph is a path, each boundary is a genuine source-to-sink cut,
// so the narrowest one is the bottleneck.
func (net *network) addSpineBoundaries(cfg config) error {
	// Pick one boundary to be the narrowest. Boundaries are indexed c in
	// [0, clusters-1); clamp the midpoint into that range so a narrow
	// boundary always exists (at clusters == 2 the only boundary, c == 0,
	// is the narrow one). This makes the min-cut == spineLinksNarrow*capSpine
	// guarantee hold for every valid scale.
	narrow := cfg.clusters / 2
	if narrow > cfg.clusters-2 {
		narrow = cfg.clusters - 2
	}
	for c := 0; c+1 < cfg.clusters; c++ {
		links := spineLinksWide
		if c == narrow {
			links = spineLinksNarrow
		}
		leftBase := c * cfg.clusterSize
		rightBase := (c + 1) * cfg.clusterSize
		// Place exactly `links` DISTINCT inter-cluster links on distinct
		// interior endpoints, chosen deterministically (site k+1 on each
		// side, skipping the cluster's first site so the source and sink
		// stay plain interior nodes). Deterministic placement — rather than
		// a random draw with rejection — guarantees the boundary really has
		// `links` parallel links, so the narrowest boundary is exactly
		// spineLinksNarrow links and never collapses to a bridge.
		// config.validate guarantees clusterSize > spineLinksWide, so the
		// indices 1..links never collide or run off the cluster.
		for k := 0; k < links; k++ {
			u := leftBase + 1 + k
			v := rightBase + 1 + k
			if err := net.addLink(u, v, capSpine); err != nil {
				return err
			}
		}
	}
	return nil
}

// addLink records an undirected capacitated link between two site indices
// and mirrors it into the adjacency the structural analysis snapshots. The
// adjacency edge weight is the capacity, purely so the CSR snapshot is a
// faithful copy of the link list; the structural analysis ignores weights.
func (net *network) addLink(a, b, capacity int) error {
	na, _ := net.mapper.Resolve(net.idOf[a])
	nb, _ := net.mapper.Resolve(net.idOf[b])
	if err := net.adj.AddEdge(na, nb, int64(capacity)); err != nil {
		return fmt.Errorf("AddEdge %s-%s: %w", na, nb, err)
	}
	net.links = append(net.links, link{a: a, b: b, cap: capacity})
	return nil
}

// hasLink reports whether an undirected link between a and b is already in
// the link list. The scan is linear, which is acceptable because it is
// only used during chord and boundary placement (a small per-cluster and
// per-boundary cost), not on any hot path.
func (net *network) hasLink(a, b int) bool {
	for _, l := range net.links {
		if (l.a == a && l.b == b) || (l.a == b && l.b == a) {
			return true
		}
	}
	return false
}

// siteName returns a stable, human-readable name for a site index. Spine
// sites are "c<cluster>s<offset>"; stub sites are "stub<offset>". The name
// is deterministic for a fixed config, so resolving cut/SPOF links back to
// names yields stable telemetry.
func siteName(idx int, cfg config, stubBase int) string {
	if idx >= stubBase {
		return fmt.Sprintf("stub%d", idx-stubBase)
	}
	return fmt.Sprintf("c%ds%d", idx/cfg.clusterSize, idx%cfg.clusterSize)
}
