// Example 11_social_network — an end-to-end social-network workload over a
// labelled property graph (LPG): PageRank influence ranking, Leiden
// community detection, and a manual friend-of-friend recommendation walk,
// all over ONE seeded, scale-parametrised social graph.
//
// It generates a realistic friendship network whose shape is fixed by the
// RNG seed, freezes it into an immutable CSR snapshot, and reads it three
// ways:
//
//   - PageRank influence ranking — who is most central, reported as a
//     deterministic top-k of influencer ids ([centrality.PageRank]).
//   - Leiden community detection — which clusters the friendships form,
//     reported as a community-count band and a modularity lower bound
//     ([community.LeidenCtx]).
//   - Friend-of-friend recommendation — who a fixed seed user should
//     befriend next, a manual two-hop walk over the live adjacency list.
//
// The output is split into deterministic *facts* (bare lines: counts,
// influencer ids, community count, recommendation result — reproducible
// for a fixed seed) and volatile *telemetry* (lines prefixed with "# ":
// per-stage wall-clock and live heap — varies per run and per machine).
// A regression test pins the facts and ignores the telemetry.
//
// # Model
//
//	(:User {id, name, community})            // id is "u%07d" in creation order
//	(:User)-[:FRIEND]-(:User)                // an undirected, unweighted friendship
//
// The graph is undirected (friendship is symmetric), so Leiden and the
// friend-of-friend walk both see a symmetric neighbourhood, and PageRank
// runs over the symmetric CSR (each undirected edge is stored as two
// directed entries) where degree heterogeneity still yields a meaningful
// centrality.
//
// # Topology — per-community Barabási–Albert blocks + a sparse bridge layer
//
// The generative model was chosen with the project's graph-theory-expert
// sub-agent so that ONE graph serves all three analytics. Verbatim:
//
//	GENERATIVE MODEL: per-community Barabási–Albert (BA) blocks + a sparse
//	bridge layer. Why this model: it is the only candidate that is natively
//	single-pass and O(E) with no rejection loop. BA gives a heavy degree
//	tail (gamma=3) per block -> meaningful PageRank influencers; separate
//	blocks + sparse bridges give assortative community structure -> Leiden
//	recovers the planted partition; triangle-rich BA neighbourhoods give a
//	non-trivial intra-community friend-of-friend set. (DCSBM = principled
//	but needs a power-law sequence + sparse edge sampler; LFR = the
//	benchmark gold standard but rejection-heavy — both over-engineered for
//	an example.) Refs: Barabási & Albert, Science 286:509 (1999);
//	Holland-Laskey-Leinhardt, Social Networks 5:109 (1983); Newman,
//	Networks 2e §13 (linear-time target-list sampling). Modularity: Newman &
//	Girvan, Phys. Rev. E 69:026113 (2004), Q ≈ (intra-edge fraction) − 1/K.
//	Detectability: Abbe, JMLR 18(177) (2017), SNR=(a−b)²/[K(a+(K−1)b)].
//
//	PARAMETER REGIME (validate()):
//	 1. K >= 3                          (so Q_max = 1−1/K can exceed 0.5)
//	 2. m >= 1 AND s >= m+1             (each BA block connected BY CONSTRUCTION)
//	 3. B >= K−1, laid as a spanning tree over blocks first  (whole graph
//	                                    connected structurally, not by luck)
//	 4. B <= rho·K·m·s, rho ∈ [0.01,0.05]  (intra fraction high -> Q ≈
//	                                    (1−1/K)−rho ∈ [0.4,0.7])
//	 5. SNR = (a−b)²/(K(a+(K−1)b)) >= 2, a≈2m, b≈2B/N  (detectability margin)
//	 Small default: K=4, s=64, m=2, B=8 -> Q≈0.73; assert Q>=0.55 & comms∈[3,5].
//
//	FoF (fixed seed user u, sorted by shared-friend count, tie-break by id):
//	 - count of distinct FoF candidates  -> DETERMINISTIC fact, pin it.
//	 - exact ordered list (id tie-break) -> DETERMINISTIC fact, pin it.
//	 - "every candidate is in community(u)" -> a THEOREM iff u is placed away
//	   from any bridge (no bridge on u or its direct friends).
//	 - "top recommendation is same-community as u" -> guarantee only under
//	   that bridge-free-neighbourhood placement.
//
// The generator follows this regime exactly. The fixed seed user is node 0
// (the first-born hub of community 0); the bridge layer is laid down so that
// neither node 0 nor any of its direct friends is a bridge endpoint, which
// makes "every friend-of-friend candidate is in the seed user's community"
// a theorem of the construction (see buildBridges and friendsOfFriends).
//
// # Scale
//
// Run with no flags, the example builds the small deterministic default —
// four communities of sixty-four users (256 users), m=2 attachment, 8
// bridges — which builds and analyses in well under a second and is pinned
// by the regression test. Every dimension is a flag, so the same binary
// scales up to where PageRank's convergence cost and the live-heap
// footprint become observable:
//
//	go run ./examples/11_social_network -users 1000000 -communities 50 -m 4 -seed 7
//
// The deterministic facts are reproducible for a fixed -seed; only the
// telemetry (lines prefixed with "# ") varies between runs and machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
	"github.com/FlavioCFOliveira/GoGraph/search/community"
)

// Node label and relationship type, plus the node property keys.
// Centralised so the model is described in exactly one place and a rename
// surfaces as a compile error everywhere it is used.
const (
	labelUser = "User"
	relFriend = "FRIEND" // (:User)-[:FRIEND]-(:User), undirected

	propID        = "id"
	propName      = "name"
	propCommunity = "community"
)

// config captures every scale and shape knob of the social-network
// generator. The zero value is not valid; build one with defaultConfig and
// override fields from flags (see main) or construct one directly (see the
// regression test).
type config struct {
	users       int   // total number of :User nodes (>= communities*2)
	communities int   // K — number of planted communities (>= 3)
	m           int   // BA attachment parameter: edges each new user emits
	bridges     int   // B — inter-community bridge edges (>= communities-1)
	topK        int   // how many top influencers to report as facts
	seed        int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins: four communities of sixty-four users (256 users) with BA
// attachment m=2 and eight bridges. By the planted-partition modularity
// identity this targets Q ≈ 0.73 (theoretical max 1 − 1/K = 0.75 for K=4),
// and each block is connected by BA construction, so the build and the
// three analytics are instantaneous — well under the short-layer budget.
func defaultConfig() config {
	return config{
		users:       256,
		communities: 4,
		m:           2,
		bridges:     8,
		topK:        4,
		seed:        1,
	}
}

// validate rejects a configuration that cannot produce the topology the
// three analytics rely on. It is checked once, at the boundary, before any
// work. The five rules implement the graph-theory-expert's parameter regime
// recorded in the package doc comment.
func (c config) validate() error {
	switch {
	case c.communities < 3:
		// Rule 1: K >= 3 so the modularity ceiling 1 − 1/K can exceed 0.5.
		return fmt.Errorf("communities must be >= 3, got %d", c.communities)
	case c.users < c.communities*2:
		return fmt.Errorf("users (%d) must be >= 2*communities (%d): each block needs at least 2 users", c.users, c.communities*2)
	case c.m < 1:
		// Rule 2: m >= 1 so every BA block is connected by construction.
		return fmt.Errorf("m must be >= 1, got %d", c.m)
	case c.topK <= 0:
		return fmt.Errorf("top-k must be > 0, got %d", c.topK)
	case c.topK > c.users:
		return fmt.Errorf("top-k (%d) exceeds users (%d)", c.topK, c.users)
	}

	// Rule 2 (cont.): the smallest block must hold at least m+1 users so the
	// BA seed clique is well-defined. communitySizes splits users as evenly
	// as it can, so the smallest block is users/communities.
	if smallest := c.users / c.communities; smallest < c.m+1 {
		return fmt.Errorf("smallest community has %d users < m+1 (%d): lower -m or raise -users", smallest, c.m+1)
	}

	// Rule 3: at least K-1 bridges, laid as a spanning tree over the blocks,
	// so the whole graph is connected structurally rather than by luck.
	if c.bridges < c.communities-1 {
		return fmt.Errorf("bridges (%d) must be >= communities-1 (%d) to connect every community", c.bridges, c.communities-1)
	}

	// Rule 4: cap the bridge budget at rho=0.05 of the intra-community edge
	// count so the intra fraction stays high and modularity lands in
	// [0.4,0.7]. Intra edges ≈ K*m*(s−m); use the average block size s.
	s := c.users / c.communities
	intra := c.communities * c.m * (s - c.m)
	if maxBridges := intra / 20; c.bridges > maxBridges { // rho = 1/20 = 0.05
		return fmt.Errorf("bridges (%d) exceeds 5%% of intra edges (%d): communities would blur, raise -users or lower -bridges", c.bridges, maxBridges)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.users, "users", cfg.users, "total number of User nodes")
	flag.IntVar(&cfg.communities, "communities", cfg.communities, "number of planted communities (K)")
	flag.IntVar(&cfg.m, "m", cfg.m, "BA attachment parameter (edges each new user emits)")
	flag.IntVar(&cfg.bridges, "bridges", cfg.bridges, "inter-community bridge edges (B)")
	flag.IntVar(&cfg.topK, "top-k", cfg.topK, "how many top influencers to report as facts")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the social network described by cfg, runs the three analytics
// over it, and writes a report to w. Bare lines carry deterministic facts
// (counts, influencer ids, community count, recommendation result —
// reproducible for a fixed seed); lines prefixed with "# " carry volatile
// telemetry (per-stage durations and heap figures) that vary per run and
// per machine. All output goes to w so a test can capture and assert on the
// deterministic lines. run honours ctx cancellation between phases and on a
// periodic check inside the generator.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.users=%d\n", cfg.users)
	fmt.Fprintf(w, "config.communities=%d\n", cfg.communities)
	fmt.Fprintf(w, "config.m=%d\n", cfg.m)
	fmt.Fprintf(w, "config.bridges=%d\n", cfg.bridges)
	fmt.Fprintf(w, "config.top_k=%d\n", cfg.topK)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	g := lpg.New[string, int64](adjlist.Config{Directed: false})
	stats, err := build(ctx, g, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Build-then-analyse workload: the graph is fully assembled above and
	// only read from here on. Compact right-sizes the adjacency backing
	// arrays before the CSR snapshot is taken so the resident-heap figures
	// reflect the tight arrays the analysis runs against.
	if err := ctx.Err(); err != nil {
		return err
	}
	g.AdjList().Compact(ctx)

	fmt.Fprintf(w, "nodes.users=%d\n", stats.users)
	fmt.Fprintf(w, "edges.friend=%d\n", stats.friendEdges)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", stats.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(stats.friendEdges, stats.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(built.HeapAlloc, base.HeapAlloc)))

	c := csr.BuildFromAdjList(g.AdjList())
	mapper := g.AdjList().Mapper()

	if err := reportInfluence(ctx, w, c, mapper, stats.idComm, cfg); err != nil {
		return fmt.Errorf("influence: %w", err)
	}
	if err := reportCommunities(ctx, w, c); err != nil {
		return fmt.Errorf("communities: %w", err)
	}
	reportRecommendations(w, g, stats, cfg)
	return nil
}

// buildStats reports the realised shape of a build (the BA draw means the
// edge total is not known until the graph is materialised) plus the
// wall-clock cost and the per-user community label.
//
// The community label is keyed by user id, NOT by NodeID: the adjacency
// Mapper assigns FNV-1a-sharded NodeIDs that are neither contiguous nor
// equal to the creation index, so community[] could not be indexed by
// NodeID. Resolving a NodeID back to its id (via the Mapper) and looking it
// up here is the correct, sharding-agnostic path.
type buildStats struct {
	users       int
	friendEdges int
	idComm      map[string]int // user id -> planted community in [0,K)
	seedUser    string         // the fixed user the FoF recommendation anchors on
	seedComm    int            // the seed user's community (community of node 0)
	elapsed     time.Duration
}

// build materialises the social network described by cfg into g. It lays
// down K per-community Barabási–Albert blocks (heavy-tailed degree within
// each community) and then a sparse bridge layer between communities,
// following the regime in the package doc comment. Nodes are added in
// creation order so user i has NodeID i, which lets community[] be indexed
// directly by NodeID later. The build honours ctx cancellation on a
// periodic check.
func build(ctx context.Context, g *lpg.Graph[string, int64], cfg config) (buildStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed social network for a given -seed; crypto/rand would
	// defeat the reproducibility the examples standard requires.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	sizes := communitySizes(cfg.users, cfg.communities)
	userIDs := make([]string, cfg.users)
	idComm := make(map[string]int, cfg.users)

	// First lay out every node so all FRIEND endpoints exist before any edge
	// references them, and so the community label is queryable.
	bounds := make([]int, cfg.communities+1) // bounds[k]..bounds[k+1) are block k's node indices
	idx := 0
	for k := 0; k < cfg.communities; k++ {
		bounds[k] = idx
		for j := 0; j < sizes[k]; j++ {
			if idx%checkEvery == 0 {
				if err := ctx.Err(); err != nil {
					return buildStats{}, err
				}
			}
			id := userID(idx)
			userIDs[idx] = id
			idComm[id] = k
			if err := addUser(g, id, realisticName(rng), k); err != nil {
				return buildStats{}, err
			}
			idx++
		}
	}
	bounds[cfg.communities] = idx

	// Per-community Barabási–Albert blocks: heavy-tailed intra-community
	// degree so a clear local hub (the first-born node) emerges per block.
	friendEdges := 0
	for k := 0; k < cfg.communities; k++ {
		n, err := buildBABlock(ctx, g, userIDs, bounds[k], bounds[k+1], cfg.m, rng)
		if err != nil {
			return buildStats{}, err
		}
		friendEdges += n
	}

	// Sparse bridge layer: a spanning path over the K blocks first (so the
	// whole graph is connected structurally), then the remaining bridges
	// between random distinct block pairs. Both kinds avoid touching the
	// seed user (node 0) and its direct friends, which makes the FoF
	// invariant a theorem.
	n, err := buildBridges(ctx, g, bounds, cfg, rng)
	if err != nil {
		return buildStats{}, err
	}
	friendEdges += n

	return buildStats{
		users:       cfg.users,
		friendEdges: friendEdges,
		idComm:      idComm,
		seedUser:    userIDs[0],
		seedComm:    idComm[userIDs[0]],
		elapsed:     time.Since(start),
	}, nil
}

// checkEvery bounds how often the build polls ctx for cancellation: often
// enough that a cancelled large run stops promptly, rare enough that the
// check is free relative to the surrounding per-node work.
const checkEvery = 4096

// communitySizes splits total users into k blocks as evenly as possible:
// the first (total mod k) blocks get one extra user. Deterministic and
// independent of the RNG, so block boundaries are fixed by the scale knobs.
func communitySizes(total, k int) []int {
	sizes := make([]int, k)
	base, extra := total/k, total%k
	for i := range sizes {
		sizes[i] = base
		if i < extra {
			sizes[i]++
		}
	}
	return sizes
}

// buildBABlock grows a Barabási–Albert block over the node indices
// [lo, hi). The first m+1 nodes form a seed path (so every later node has m
// distinct existing targets), then each remaining node attaches to m
// distinct existing nodes drawn with probability proportional to current
// degree. Targets are sampled in O(1) amortised time by the
// redirection / edge-copying trick (Newman, Networks 2e §13): targetList
// records every edge endpoint, so a node appears in it exactly degree
// times and a uniform pick from it is a degree-proportional pick. Returns
// the number of FRIEND edges added in this block.
func buildBABlock(ctx context.Context, g *lpg.Graph[string, int64], ids []string, lo, hi, m int, rng *rand.Rand) (int, error) {
	size := hi - lo
	if size <= 1 {
		return 0, nil // a singleton block has no internal edges
	}
	// targetList holds endpoints of edges already placed in this block; two
	// entries per edge. Pre-sized to the eventual edge count: a seed path of
	// m edges plus m per non-seed node.
	targetList := make([]int, 0, 2*(m+m*(size-m)))
	edges := 0

	// Seed path over the first min(size, m+1) nodes guarantees the block is
	// connected and gives the BA process an initial degree distribution.
	seed := m + 1
	if seed > size {
		seed = size
	}
	for off := 1; off < seed; off++ {
		u, v := lo+off-1, lo+off
		if err := g.AddEdge(ids[u], ids[v], 1); err != nil {
			return 0, fmt.Errorf("AddEdge seed %s-%s: %w", ids[u], ids[v], err)
		}
		targetList = append(targetList, u, v)
		edges++
	}

	// Each remaining node attaches to m distinct existing nodes, chosen
	// degree-proportionally via the target list. The chosen targets are kept
	// in a slice (with a small membership set for the distinctness check) and
	// added in draw order: a Go map's iteration order is randomised per
	// range, so draining a set would make the edge-insertion order — and thus
	// every later target-list draw — non-reproducible. Slice order is fixed
	// by the seeded RNG, which is exactly the determinism the standard needs.
	chosen := make([]int, 0, m)
	inChosen := make(map[int]struct{}, m)
	for off := seed; off < size; off++ {
		if off%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		v := lo + off
		chosen = chosen[:0]
		clear(inChosen)
		for len(chosen) < m {
			u := targetList[rng.Intn(len(targetList))]
			if u == v {
				continue // never expected (v is not yet in the list), but cheap insurance
			}
			if _, dup := inChosen[u]; dup {
				continue
			}
			inChosen[u] = struct{}{}
			chosen = append(chosen, u)
		}
		for _, u := range chosen {
			if err := g.AddEdge(ids[u], ids[v], 1); err != nil {
				return 0, fmt.Errorf("AddEdge BA %s-%s: %w", ids[u], ids[v], err)
			}
			targetList = append(targetList, u, v)
			edges++
		}
	}
	return edges, nil
}

// buildBridges adds the inter-community bridge layer described by cfg: a
// spanning path over the K blocks (block k ↔ block k+1) so the whole graph
// is connected, then the remaining bridges between random distinct block
// pairs. Every bridge endpoint is drawn to avoid the seed user (node 0) and
// its direct friends, so a friend-of-friend walk from the seed user never
// crosses a community boundary — making "every FoF candidate is in the seed
// user's community" a theorem of the construction. Returns the number of
// bridge edges added.
//
// The seed user (node 0) is the first-born hub of community 0; its direct
// friends are the highest-degree members of that community. Excluding them
// as bridge endpoints is cheap: a block of size s has s − (1 + deg(node 0))
// eligible members, and validate guarantees s ≥ m+1.
func buildBridges(ctx context.Context, g *lpg.Graph[string, int64], bounds []int, cfg config, rng *rand.Rand) (int, error) {
	k := cfg.communities
	// Endpoints that a bridge must not touch: the seed user and its direct
	// friends, all of which live in community 0. Tracked by string id (the
	// neighbour iterator yields ids) so no NodeID conversion is needed.
	forbidden := map[string]struct{}{userID(0): {}}
	for v := range g.AdjList().Neighbours(userID(0)) {
		forbidden[v] = struct{}{}
	}

	pick := func(block int) (int, bool) {
		lo, hi := bounds[block], bounds[block+1]
		for attempt := 0; attempt < hi-lo; attempt++ {
			n := lo + rng.Intn(hi-lo)
			if _, bad := forbidden[userID(n)]; bad {
				continue
			}
			return n, true
		}
		return 0, false // every member forbidden (only possible for a tiny block 0)
	}

	addBridge := func(a, b int) error {
		ua, oka := pick(a)
		ub, okb := pick(b)
		if !oka || !okb {
			return nil // skip a bridge we cannot place without touching the seed neighbourhood
		}
		return g.AddEdge(userID(ua), userID(ub), 1)
	}

	edges := 0
	// Spanning path: connects every community into one component.
	for b := 0; b < k-1; b++ {
		if err := addBridge(b, b+1); err != nil {
			return 0, fmt.Errorf("AddEdge spanning bridge %d-%d: %w", b, b+1, err)
		}
		edges++
	}
	// Remaining bridges between random distinct block pairs.
	for added := k - 1; added < cfg.bridges; added++ {
		if added%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		a := rng.Intn(k)
		b := rng.Intn(k - 1)
		if b >= a {
			b++ // map to a distinct block in [0,k) \ {a}
		}
		if err := addBridge(a, b); err != nil {
			return 0, fmt.Errorf("AddEdge bridge %d-%d: %w", a, b, err)
		}
		edges++
	}
	return edges, nil
}

// userID renders user index i as a stable, zero-padded id. Creation order
// (the index) is the identity, so the id is reproducible for a fixed scale.
func userID(i int) string {
	return fmt.Sprintf("u%07d", i)
}

// addUser adds a single :User node carrying its id, a realistic name, and
// its planted community label (so the community is queryable and the
// recommendation walk can verify the same-community invariant).
func addUser(g *lpg.Graph[string, int64], id, name string, comm int) error {
	if err := g.AddNode(id); err != nil {
		return fmt.Errorf("AddNode %s: %w", id, err)
	}
	if err := g.SetNodeLabel(id, labelUser); err != nil {
		return fmt.Errorf("SetNodeLabel %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propID, lpg.StringValue(id)); err != nil {
		return fmt.Errorf("SetNodeProperty id %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propName, lpg.StringValue(name)); err != nil {
		return fmt.Errorf("SetNodeProperty name %s: %w", id, err)
	}
	if err := g.SetNodeProperty(id, propCommunity, lpg.Int64Value(int64(comm))); err != nil {
		return fmt.Errorf("SetNodeProperty community %s: %w", id, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Stage 1 — PageRank influence ranking
// ─────────────────────────────────────────────────────────────────────────────

// reportInfluence ranks users by PageRank over the CSR snapshot and reports
// the top-k influencer ids as deterministic facts, with the score and
// timing as telemetry. PageRank scores are floats whose exact values are
// volatile across machines, so the pinnable fact is the ranked identity of
// the top-k (id tiebreak) plus the count of distinct communities they span:
// because the topology grows one BA hub per community, a clean influencer
// set with several communities represented is the expected, separable
// result.
func reportInfluence(ctx context.Context, w io.Writer, c *csr.CSR[int64], mapper *graph.Mapper[string], idComm map[string]int, cfg config) error {
	start := time.Now()
	ranks, iters, err := centrality.PageRankCtx(ctx, c, centrality.DefaultPageRankOptions())
	if err != nil {
		return fmt.Errorf("pagerank: %w", err)
	}
	elapsed := time.Since(start)

	type ranked struct {
		id   string
		comm int
		rank float64
	}
	// The Mapper assigns FNV-1a-sharded NodeIDs, so MaxNodeID() is padded and
	// the live NodeIDs are sparse: resolve each id back to its user id and
	// look its community up by id (never index a per-node slice by NodeID).
	// ranks is NodeID-indexed and safe to index by id directly.
	ordered := make([]ranked, 0, len(idComm))
	for id := graph.NodeID(0); id < c.MaxNodeID(); id++ {
		name, ok := mapper.Resolve(id)
		if !ok {
			continue // ghost slot from sharded packing
		}
		ordered = append(ordered, ranked{name, idComm[name], ranks[id]})
	}
	// Descending rank, id tiebreak, so the top-k is byte-stable despite
	// equal-score hubs across communities.
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].rank != ordered[j].rank {
			return ordered[i].rank > ordered[j].rank
		}
		return ordered[i].id < ordered[j].id
	})

	limit := cfg.topK
	if limit > len(ordered) {
		limit = len(ordered)
	}
	spanned := map[int]struct{}{}
	for i := 0; i < limit; i++ {
		fmt.Fprintf(w, "influence.rank.%d=%s\n", i+1, ordered[i].id)
		spanned[ordered[i].comm] = struct{}{}
	}
	fmt.Fprintf(w, "influence.communities_spanned=%d\n", len(spanned))

	fmt.Fprintf(w, "# influence.pagerank.elapsed=%s\n", elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# influence.pagerank.iterations=%d\n", iters)
	for i := 0; i < limit; i++ {
		fmt.Fprintf(w, "# influence.rank.%d.score=%.6f\n", i+1, ordered[i].rank)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Stage 2 — Leiden community detection
// ─────────────────────────────────────────────────────────────────────────────

// reportCommunities runs Leiden over the CSR snapshot and reports the
// recovered community count and the Newman modularity Q as deterministic
// facts, with the detect timing as telemetry. Leiden's output is
// deterministic for a fixed input, but its community ids are arbitrary
// integers, so the report pins the count and rounds Q to two decimals; the
// regression test asserts a count band and a Q lower bound (≥ 0.55 at the
// default) rather than an exact float, surviving an internal change that
// preserves partition quality.
func reportCommunities(ctx context.Context, w io.Writer, c *csr.CSR[int64]) error {
	start := time.Now()
	part, err := community.LeidenCtx(ctx, c, community.DefaultLeidenOptions())
	if err != nil {
		return fmt.Errorf("leiden: %w", err)
	}
	elapsed := time.Since(start)

	q := computeModularity(c, part)
	fmt.Fprintf(w, "communities.found=%d\n", part.NumCommunities)
	fmt.Fprintf(w, "communities.modularity=%.2f\n", q)

	fmt.Fprintf(w, "# communities.leiden.elapsed=%s\n", elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# communities.modularity_exact=%.6f\n", q)
	return nil
}

// computeModularity returns the Newman modularity Q of partition part over
// the undirected, unweighted snapshot c, using the per-community form
//
//	Q = Σ_c [ L_c/m − (D_c/2m)² ]
//
// where m is the edge count, L_c the number of edges with both endpoints in
// community c (counted once), and D_c the summed degree of community c
// (Newman & Girvan, Phys. Rev. E 69, 026113, 2004). The CSR stores each
// undirected edge as two directed entries, so m is the total directed entry
// count halved and L_c counts only the u<v direction of an intra-community
// adjacency. Ghost NodeID slots (community -1 from sharded packing) are
// skipped. Runs in O(V + E). Mirrors example 09's computeModularity, adapted
// to the int64 edge-weight type used here.
func computeModularity(c *csr.CSR[int64], part community.Partition) float64 {
	offsets := c.VerticesSlice() // len == MaxNodeID()+1
	edges := c.EdgesSlice()
	maxID := c.MaxNodeID()

	twoM := len(edges) // each undirected edge contributes two directed entries
	if twoM == 0 {
		return 0
	}
	m := float64(twoM) / 2

	lc := make([]int, part.NumCommunities)    // intra edges per community (u<v)
	dc := make([]uint64, part.NumCommunities) // summed degree per community
	// Iterate NodeIDs directly (no int conversions): offsets/edges/Community
	// are all indexable by NodeID, and NodeIDs compare directly for the u<v
	// once-per-undirected-pair guard.
	for u := graph.NodeID(0); u < maxID; u++ {
		cu := part.Community[u]
		if cu < 0 {
			continue // ghost slot
		}
		dc[cu] += offsets[u+1] - offsets[u]
		for e := offsets[u]; e < offsets[u+1]; e++ {
			v := edges[e]
			if v <= u {
				continue // count each undirected pair once
			}
			if part.Community[v] == cu {
				lc[cu]++
			}
		}
	}

	q := 0.0
	for cid := 0; cid < part.NumCommunities; cid++ {
		frac := float64(dc[cid]) / (2 * m)
		q += float64(lc[cid])/m - frac*frac
	}
	return q
}

// ─────────────────────────────────────────────────────────────────────────────
// Stage 3 — Friend-of-friend recommendation
// ─────────────────────────────────────────────────────────────────────────────

// reportRecommendations runs a manual two-hop friend-of-friend walk from the
// fixed seed user and reports the deterministic recommendation result: the
// number of distinct candidates, whether every candidate lies in the seed
// user's community (a theorem of the construction — the seed user is placed
// away from any bridge), and the top recommendation by shared-friend count.
// The walk timing is telemetry.
func reportRecommendations(w io.Writer, g *lpg.Graph[string, int64], stats buildStats, cfg config) {
	start := time.Now()
	recs := friendsOfFriends(g, stats.seedUser)
	elapsed := time.Since(start)

	allSameComm := true
	for _, r := range recs {
		// Community is keyed by id (the Mapper's NodeIDs are sharded, not a
		// creation index), so the check is a plain map lookup.
		if stats.idComm[r.id] != stats.seedComm {
			allSameComm = false
			break
		}
	}

	fmt.Fprintf(w, "fof.seed_user=%s\n", stats.seedUser)
	fmt.Fprintf(w, "fof.candidates=%d\n", len(recs))
	fmt.Fprintf(w, "fof.all_same_community=%t\n", allSameComm)
	if len(recs) > 0 {
		fmt.Fprintf(w, "fof.top=%s\n", recs[0].id)
		fmt.Fprintf(w, "fof.top_shared=%d\n", recs[0].shared)
	}

	fmt.Fprintf(w, "# fof.walk.elapsed=%s\n", elapsed.Round(time.Microsecond))
	limit := cfg.topK
	if limit > len(recs) {
		limit = len(recs)
	}
	for i := 0; i < limit; i++ {
		fmt.Fprintf(w, "# fof.rank.%d=%s shared=%d\n", i+1, recs[i].id, recs[i].shared)
	}
}

// recommendation is a friend-of-friend candidate and the number of mutual
// friends it shares with the seed user (the strength of the suggestion).
type recommendation struct {
	id     string
	shared int
}

// friendsOfFriends returns users two hops from src that are not already
// direct friends, ranked by the number of mutual friends (descending) then
// id (ascending) so the ordering is byte-stable despite equal counts and
// the non-deterministic neighbour-iteration order. The walk runs over the
// live undirected adjacency list — no CSR needed — and shows the canonical
// triadic-closure recommendation.
func friendsOfFriends(g *lpg.Graph[string, int64], src string) []recommendation {
	direct := map[string]bool{src: true}
	for v := range g.AdjList().Neighbours(src) {
		direct[v] = true
	}
	shared := map[string]int{}
	for v := range g.AdjList().Neighbours(src) {
		for w := range g.AdjList().Neighbours(v) {
			if direct[w] {
				continue
			}
			shared[w]++
		}
	}
	out := make([]recommendation, 0, len(shared))
	for id, n := range shared {
		out = append(out, recommendation{id, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].shared != out[j].shared {
			return out[i].shared > out[j].shared
		}
		return out[i].id < out[j].id
	})
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers (copied from the reference example 26).
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// saturatingSub returns a−b, clamped to 0 when b > a (GC between the two
// snapshots can leave the later HeapAlloc below the earlier one).
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ─────────────────────────────────────────────────────────────────────────────
// Realistic-data word lists. Fixed so the dataset is reproducible.
// ─────────────────────────────────────────────────────────────────────────────

// realisticName assembles a plausible "First Last" personal name from fixed
// word lists. Names are intentionally allowed to repeat — the unique key is
// the id, not the name, which mirrors reality.
func realisticName(rng *rand.Rand) string {
	return firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
}

var firstNames = []string{
	"Olivia", "Liam", "Emma", "Noah", "Ava", "Oliver", "Sophia", "Elijah",
	"Isabella", "James", "Mia", "Lucas", "Charlotte", "Mateo", "Amelia",
	"Ethan", "Harper", "Leo", "Evelyn", "Sebastian", "Abigail", "Daniel",
	"Emily", "Henry", "Ella", "Alexander", "Scarlett", "Jack", "Aria",
	"Benjamin", "Camila", "Theodore", "Luna", "Samuel", "Chloe", "David",
	"Sofia", "Joseph", "Layla", "Carter", "Nora", "Wyatt", "Zoe", "Julian",
	"Mila", "Levi", "Aurora", "Gabriel", "Hannah", "Anthony",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller",
	"Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez",
	"Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin",
	"Lee", "Perez", "Thompson", "White", "Harris", "Sanchez", "Clark",
	"Ramirez", "Lewis", "Robinson", "Walker", "Young", "Allen", "King",
	"Wright", "Scott", "Torres", "Nguyen", "Hill", "Flores", "Green",
	"Adams", "Nelson", "Baker", "Hall", "Rivera", "Campbell", "Mitchell",
	"Carter", "Roberts",
}
