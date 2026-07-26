# Stream 7 — Graph algorithms

Baseline: `6f31f61`, v0.10.0. Measurements: Apple M4, `darwin/arm64`, GOMAXPROCS=10, Go toolchain as pinned by the repo.
External baselines verified 2026-07-25 against **Neo4j GDS 2026.06** (current) and **Memgraph MAGE** (current docs).

## Verdict summary

GoGraph's algorithm set is **exactness-first and narrow**; GDS is **breadth-first and approximation-tolerant**; MAGE is **breadth-first and language-mixed** (a third of its catalogue is Python, including `max_flow`). On the classical exact core — flow, matching, APSP, Euler, connectivity, exact betweenness — GoGraph is at parity or ahead, and it is the only one of the three that can run an algorithm over a graph larger than RAM. But **round 1's headline algorithm finding is wrong**: GDS shipped `gds.maxFlow` in 2.23.0 (2025-11-14) and `gds.maxFlow.minCost` in 2.25.0 (2026-01-19), so "GDS has NO max-flow" was already ~8 months stale when round 1 asserted it. The residual flow advantage is real but much narrower than claimed (global min-cut, assignment, Eulerian circuits).

The single most valuable lever is **not** anything Neo4j or Memgraph does — it is something **neither** does: a **preprocessing-based point-to-point shortest-path index** (Contraction Hierarchies / hub labelling). Measured today, one point-to-point query on a 490k-node road-like graph costs **27.9 ms** with bidirectional Dijkstra (already tight: 3 allocs/op). The literature's exact preprocessing methods answer the same query in hundreds of nanoseconds to hundreds of microseconds on continent-scale road networks. GoGraph is an *embeddable library over an immutable CSR snapshot with an existing epoch-based invalidation signal* — the single best-suited of the three for build-once-query-many, and the only one whose architecture makes it cheap.

On round-1 lever **T2.5 (delta-stepping): reject, with evidence.** The SPAA'21 authors measured on **96 cores / 192 hyperthreads**, not 10; the best Δ varies by up to 2^8 across graphs *with identical weight distributions* and a wrong Δ costs >4×; and GDS's own delta-stepping documentation concedes it "is not guaranteed to return the same path in each computation" — which collides head-on with GoGraph's determinism contract.

## Feature-by-feature comparison

| Feature | GoGraph (`file:line`) | Neo4j GDS 2026.06 | Memgraph MAGE | Verdict | Label |
|---|---|---|---|---|---|
| Max-flow | Dinic `flow/dinic.go:59` O(V²E); Edmonds-Karp `flow/edmonds_karp.go:20`; Push-relabel `flow/push_relabel.go:22` | `gds.maxFlow` — parallel push-relabel, multi-source/sink, node caps (2.23.0) | `max_flow` — **Python**, Ford-Fulkerson + capacity scaling | **PARITY** (GDS parallel + multi-source is ahead; GoGraph has 3 exact variants natively) | **STALE-R1** |
| Min-cost max-flow | `flow/min_cost.go:68` SSP | `gds.maxFlow.minCost` cost-scaling push-relabel (2.25.0) | absent | PARITY | STALE-R1 |
| Global min-cut | Stoer-Wagner `flow/stoer_wagner.go:22` O(V³) | absent (only s-t cut via max-flow; *approximate* max k-cut) | absent | **BETTER** | NEW |
| Bipartite matching | Hopcroft-Karp `hopcroft_karp.go:32` O(E√V) | **absent** | `bipartite_matching` (C++) | BETTER vs Neo4j, PARITY vs Memgraph | NEW |
| Assignment problem | Hungarian `hungarian.go:71` O(V³), rectangular | absent | absent | **BETTER** | NEW |
| Eulerian circuit | Hierholzer `hierholzer.go:26` / `hierholzer_undirected.go:22` O(E) | absent | absent | **BETTER** | NEW |
| Exact betweenness | Brandes `centrality/brandes.go:28` — exact by default | **approximate by default** (Brandes sampling; exact only if `samplingSize`=all) | `betweenness_centrality` (C++) exact | **BETTER** vs Neo4j | NEW |
| Dynamic betweenness | absent | absent | `betweenness_centrality_online` — **iCentral**, exact, BCC-localised | WORSE vs Memgraph | CONFIRMED-R1 |
| Point-to-point SSSP (repeated) | BiDijkstra `bidijkstra.go:33` — **27.9 ms @ V=490k** | Dijkstra source-target, A*, delta-stepping — all per-query | `weighted shortest path` (C++) per-query | **PARITY (all three weak)** | **NEW** |
| Parallel SSSP | absent (parallel *across* sources only) | `gds.betaDeltaStepping` / Delta-Stepping SSSP | absent | WORSE vs Neo4j — **but reject the lever** | CONFIRMED-R1 |
| APSP exact | Floyd-Warshall `floyd_warshall.go:96` + **bit-identical parallel** `floyd_warshall_parallel.go:47`; Johnson `johnson.go:162` + parallel | All Pairs Shortest Path | `all shortest paths` (C++) | **BETTER** (bit-identical parallel is unique) | CONFIRMED-R1 |
| Transitive closure | `transitive_closure.go:85` Warshall bitset O(V³/64) | absent | absent | BETTER | NEW |
| k-shortest paths | Yen `yen.go:62`; loopless w/ budget `kshortest_loopless.go:97`; Eppstein alias `eppstein.go:23` | Yen only | `k shortest paths` (C++) | PARITY+ | — |
| BCC / bridges / articulation | `bcc.go:38` — all three in one O(V+E) pass, multigraph-correct | Articulation Points + Bridges (separate) | `biconnected_components`, `bridges` | PARITY | — |
| k-core | `kcore.go:22` O(V+E) bucket peel (optimal) | K-Core Decomposition | absent | PARITY | — |
| Community | Leiden `community/leiden.go:104` **documented bit-reproducible**; LabelPropagation | Louvain, Leiden, SLLPA, K-1 Coloring, Modularity Opt., Conductance, HDBSCAN, K-Means, Clique Counting | `community_detection` (C++), `leiden_community_detection`, `community_detection_online` (LabelRankT) | WORSE on breadth, **BETTER on determinism** | CONFIRMED-R1 |
| PageRank | `centrality/pagerank.go:105` parallel pull-SpMV, bit-identical; `PageRanker` reusable :533; PPR push `ppr_push.go:43` | PageRank, Article Rank, Eigenvector, CELF, HITS, Degree, Closeness, Harmonic, Betweenness | `pagerank` (C++), `pagerank_online` | PARITY on core; WORSE on breadth (no HITS/ArticleRank/CELF) | — |
| Katz | `centrality/katz.go:64` | **absent** | `katz_centrality` + `katz_centrality_online` | BETTER vs Neo4j | NEW |
| Out-of-core | `extern/bfs.go:26`, `extern/pagerank.go:65` — semi-external over mmap `csrfile` | **absent** (projection is fully in-RAM) | **absent** (in-memory DB) | **BETTER — unique** | CONFIRMED-R1 |
| Embeddings / ML | **absent** | FastRP, GraphSAGE, node2vec, HashGNN + LP/NC pipelines | `node2vec`, `node2vec_online`, `gnn`, `temporal_graph_networks` (all Python) | WORSE | CONFIRMED-R1 |
| Similarity | absent | Node Similarity, kNN, filtered variants, similarity functions | `node_similarity` (C++) | WORSE | NEW |
| User-defined algorithms | Go API (native) | Pregel API | C/C++/Python/Rust query modules | PARITY (different model) | NEW |

---

## Findings

### F1. Round 1's "GDS has NO max-flow" is false — GDS shipped max-flow and min-cost max-flow  [STALE-R1]  (severity: HIGH)

- **What they do:** GDS 2.23.0 (released **2025-11-14**) added `gds.maxFlow.{stream,mutate,write}` and `.estimate`; GDS 2.25.0 (**2026-01-19**) added `gds.maxFlow.minCost.*`; 2.25.0 also added `nodeCapacityProperty` for per-node capacity limits. The max-flow implementation is documented as "based on a parallel push-relabel algorithm"; min-cost max-flow is a cost-scaling push-relabel. Both support multi-source and multi-sink with per-source supply and per-sink demand scalars.
- **Evidence:** GitHub release bodies for `neo4j/graph-data-science` tags 2.23.0, 2.24.0 ("Fix a bug where gds.maxFlow would return an invalid flow"), 2.25.0, 2026.03.0 ("Fix a bug where `gds.maxFlow` or `gds.maxFlow.minCost` might fail…") — retrieved via the GitHub releases API. Doc pages: `neo4j.com/docs/graph-data-science/current/algorithms/max-flow/` and `.../min-cost-max-flow/` (both HTTP 200, version 2026.06).
- **What GoGraph does:** three exact max-flow variants (`search/flow/dinic.go:59`, `edmonds_karp.go:20`, `push_relabel.go:22`) plus `min_cost.go:68`, all serial, single-source/single-sink, integer capacities.
- **Lever:** Two concrete items GDS has that GoGraph does not: (a) **multi-source / multi-sink with per-terminal supply/demand caps** — today a caller must hand-build a super-source/super-sink, which is boilerplate that belongs in the library; (b) **node capacities** — currently requires manual node-splitting. Both are pure API-level reductions to the existing Dinic core, no new algorithm. Do **not** copy parallel push-relabel: it is non-deterministic in the flow assignment it produces (the flow *value* is unique, the *assignment* is not), which would break GoGraph's determinism posture for no measured gain at these scales.
- **TCK/ACID impact:** None. `search/flow` is outside the Cypher surface entirely; these are pure functions over an immutable `Network` value.
- **Effort:** S (super-source/sink + node-splitting helpers over existing Dinic).
- **Correction to record:** the residual GoGraph flow advantage is **global min-cut (Stoer-Wagner), assignment (Hungarian), and Eulerian circuits** — all three genuinely absent from both incumbents — not max-flow itself.

### F2. Preprocessing-based point-to-point shortest paths — the one place GoGraph can beat both by orders of magnitude  [NEW]  (severity: HIGH)

- **What they do:** *Nothing.* Neither GDS nor MAGE has Contraction Hierarchies, hub labelling, transit-node routing, or any other preprocessing-based distance index. Verified against the complete GDS pathfinding catalogue (Delta-Stepping, Dijkstra source-target, Dijkstra single-source, A*, Yen's, Bellman-Ford, MST, k-MST, Steiner, prize-collecting Steiner, APSP, BFS, DFS, Random Walk, max-flow, min-cost max-flow, DAG longest path, topological sort) and the MAGE module list (78 modules). Every point-to-point query in both products pays full search cost, every time.
- **What GoGraph does:** `search/bidijkstra.go:33` (`BidirectionalDijkstraOn`) — a well-tuned bidirectional Dijkstra over the immutable CSR. It is *not* slow code; it is the wrong algorithm class for repeated queries.
- **Evidence (measured, Apple M4, `go test -bench`, road-like 4-neighbour grid, integer weights 1..100):**

  | V | BiDijkstra | allocs | Full Dijkstra | allocs |
  |---:|---:|---:|---:|---:|
  | 10,000 | 302 µs/op | 3 | 953 µs/op | 4 |
  | 90,000 | 3.11 ms/op | 3 | 9.93 ms/op | 4 |
  | 490,000 | **27.9 ms/op** | 3 | 54.6 ms/op | 5 |

  3 allocs/op means there is no constant-factor headroom left — the 27.9 ms is algorithmic. For contrast, Bast, Delling, Goldberg, Müller-Hannemann, Pajor, Sanders, Wagner & Werneck, *Route Planning in Transportation Networks* (arXiv:1504.05140; Algorithm Engineering, LNCS 9220, 2016) states: "for continent-sized road networks, newly-developed algorithms can answer queries in a few hundred nanoseconds", and — critical for this project — "all current state-of-the-art algorithms find **provably exact** solutions."
- **Lever:** Add a `search/hierarchy` package: a **Contraction Hierarchy** built once over a CSR snapshot, with an exact bidirectional upward search at query time. CH is the right first choice over hub labelling because its index is O(V+E+shortcuts) rather than hub labelling's much larger label set, and its preprocessing is a simple node-ordering + witness-search loop. Expose it as the stateful counterpart to the existing one-shot function, exactly as `search/sssp.go:37` (`NewSSSP`) and `centrality/pagerank.go:533` (`NewPageRanker`) already do — the API precedent is established.
- **Why GoGraph specifically:** the invalidation problem that would make this painful in a mutable server is already solved here. `graph/lpg/lpg.go:1903` exposes `TopoGeneration()` — a monotonic edge-topology counter documented as "exactly the invalidation signal a CSR-position-keyed cache needs" — and `cypher/edge_type_filter_cache.go:27` already implements the epoch-keyed cache pattern against it. A CH index is just another epoch-keyed artefact over the same snapshot.
- **TCK/ACID impact:** **Zero TCK risk.** openCypher's `shortestPath()` is hop-count (unweighted) and is served by BiBFS; a CH index is a weighted-distance structure reached only through the Go API, so no TCK-covered semantics change. ACID: the index is derived, read-only, and epoch-gated — a stale epoch means rebuild, never a wrong answer. It participates in no transaction and needs no WAL record (it is reconstructible from the graph, like the CSR snapshot itself). Bound the memory explicitly in the constructor per the no-unbounded-resources rule.
- **Effort:** L (CH preprocessing with witness search and a node-ordering heuristic is the substantial part; the query side is ~100 lines).
- **Honest caveat:** CH's dramatic wins are on road-like graphs with strong hierarchy. On scale-free/social graphs CH degrades badly (shortcut explosion). Gate it: build only when the graph's contraction produces a bounded shortcut count, and fail over to BiDijkstra otherwise. Measure before shipping.

### F3. Delta-stepping (round-1 lever T2.5) — REJECT  [CONFIRMED-R1 finding, REVERSED recommendation]  (severity: HIGH)

- **What they do:** GDS ships Delta-Stepping SSSP, documented as based on Meyer & Sanders' δ-stepping plus the bucket-fusion optimisation from Zhang, Brahmakshatriya, Chen, Dhulipala, Kamil, Amarasinghe & Shun (GraphIt). Memgraph has no parallel SSSP.
- **Three independent reasons to reject it for GoGraph:**
  1. **Δ cannot be tuned by a library.** Dong, Gu, Sun & Zhang, *Efficient Stepping Algorithms and Implementations for Parallel Shortest Paths*, SPAA 2021 (arXiv:2105.06145), report verbatim: "(1). On the same graph, the best delta can be very different for different implementations (e.g., on Twitter, Julienne's best Δ is 2^12 times larger than Galois's). The best value of delta for one algorithm can make another implementation much slower (e.g., Galois's best Δ on Friendster makes all other implementations more than 4× slower). … (2) For each implementation, the best choices of Δ vary a lot on different graphs (2^8 for GAPBS), **although they have similar edge weight range and distribution**. (3). On the same graph, the performance is very sensitive to the value of Δ." A library that cannot see the user's graph in advance cannot pick Δ, and picking wrong costs more than the parallelism buys.
  2. **The reported wins are at 96 cores, not 10.** The same paper states verbatim: "We use 96 cores (192 hyperthreads)." Its headline results — ρ-stepping 1.3–2.5× faster than existing implementations on social/web graphs, Δ*-stepping ≥14% faster on road graphs — are *relative to other parallel implementations at 96 cores*, not to a tuned sequential Dijkstra at 10. GoGraph's own comparison baseline hardware is Apple M4 / GOMAXPROCS=10 (`docs/benchmarks/comparison.md`).
  3. **It breaks the determinism contract.** GDS's own delta-stepping page concedes: "if multiple shortest paths exist between two nodes, the algorithm is not guaranteed to return the same path in each computation." GoGraph documents the opposite guarantee across its parallel algorithms (`floyd_warshall_parallel.go:27`, `johnson_parallel.go:21`, `triangles_parallel.go:24` all say *bit-identical*).
- **Also reject the 2025 theory result.** Duan, Mao, Mao, Shu & Yin, *Breaking the Sorting Barrier for Directed Single-Source Shortest Paths*, STOC 2025 (arXiv:2504.17033), gives deterministic O(m log^{2/3} n) — genuinely the first algorithm to beat Dijkstra's O(m + n log n) bound. But the independent implementation study by Castro de Souza, Clementino de Andrade & de Freitas Rodrigues (arXiv:2511.03007) concludes verbatim: "our results show that, despite its superior asymptotic complexity, the new algorithm presents significantly larger constant factors, **making Dijkstra's algorithm faster for all tested sparse graph sizes, including instances with tens of millions of vertices**."
- **What to do instead:** GoGraph's existing strategy is already the right one for 10 cores — parallelise **across** independent sources, not within one query. `johnson_parallel.go:44` and `centrality/brandes_parallel.go:31` do exactly this, are embarrassingly parallel, and Johnson's is bit-identical to serial. The remaining single-query lever is F2 (preprocessing), not parallel SSSP.
- **TCK/ACID impact:** N/A (rejection).
- **Effort:** none — this saves an L-sized effort.

### F4. Dynamic/incremental algorithms — adopt the *shape* narrowly, reject the *mechanism*  [NEW analysis of a CONFIRMED-R1 gap]  (severity: MEDIUM)

- **What they do:** MAGE's differentiator is five `*_online` modules: `pagerank_online`, `community_detection_online`, `betweenness_centrality_online`, `katz_centrality_online` (all C++) and `node2vec_online` (Python). The usage pattern is documented explicitly — you install a Memgraph **trigger**:
  ```
  CREATE TRIGGER pagerank_trigger (BEFORE | AFTER) COMMIT
  EXECUTE CALL pagerank_online.update(createdVertices, createdEdges, deletedVertices, deletedEdges)
  YIELD node, rank SET node.rank = rank;
  ```
  Each module holds mutable state between calls, with `set()` / `get()` / `reset()` lifecycle procedures.
- **The two decisive details round 1 missed:**
  1. **`pagerank_online` is an approximation, by construction.** The docs state: "To make it as fast as possible, the online algorithm is only the approximation of PageRank" — it is a Monte-Carlo random-walk estimator, `RankApprox(v) = X_v / (n * R / eps)`, based on Bahmani, Chowdhury & Goel, *Fast Incremental and Personalized PageRank* (VLDB 2010). Its accuracy is a concentration bound, not an equality. `community_detection_online` (LabelRankT) carries its own tuning warning: the default `similarity_threshold`, `exponent` and `min_value` "are not universally applicable, and the actual values should be determined experimentally."
  2. **`betweenness_centrality_online` is the exception — it is exact.** It implements **iCentral** (Jamour, Skiadopoulos & Kalnis, *Parallel Algorithm for Incremental Betweenness Centrality on Large Graphs*, IEEE TPDS 29(3):659–672, 2018), whose structural insight is that an edge update only perturbs betweenness **within the biconnected component containing that edge**, giving O(|V'||E'|) per update in linear space.
- **What GoGraph does:** nothing incremental. Note precisely: `centrality/pagerank.go:533` `NewPageRanker` *looks* like a warm-start hook but is not — its doc at :542 states the result "is bit-for-bit identical to the equivalent one-shot `PageRankCtx` call: Run re-seeds the rank vectors from scratch on every invocation". It caches topology and the reverse transpose only.
- **Is the trigger mechanism compatible with GoGraph's ACID model? No — and that is the right answer.** Memgraph's design places mutable, non-transactional algorithm state beside the transactional store, updated by a commit-time trigger. Under GoGraph's mandate that state would have to be atomic with the commit, roll back with the transaction, survive `kill -9`, and be crash-recovered — i.e. it would become WAL-backed transactional state. That converts an "algorithm cache" into a first-class durable subsystem, and it would put approximate, drifting values inside the durability envelope. **Reject the trigger/`*_online` mechanism outright.**
- **Lever (narrow, and it fits):** keep incremental state **derived, in-memory, epoch-gated, and outside the transaction** — the same contract as the CSR snapshot and F2's CH index. Two candidates, in order:
  1. **Warm-start PageRank.** Add `PageRanker.RunFrom(prev []float64, opts)` that seeds power iteration from the previous rank vector instead of uniform. This is exact (power iteration converges to the same fixed point from any valid start), deterministic, and typically cuts iteration count sharply when topology changed little. It reuses the `PageRanker` state that already exists at `pagerank.go:533`. **Effort: S.**
  2. **iCentral-style localised betweenness.** GoGraph already has both building blocks: exact Brandes (`centrality/brandes.go:28`) and an O(V+E) Hopcroft-Tarjan pass that returns components, bridges and articulation points together (`bcc.go:38`, `BCCResult` at :18-22). This would be **exact**, unlike three of Memgraph's four. **Effort: L** — and see F5 for why the payoff is graph-dependent; measure before committing.
- **TCK/ACID impact:** Both stay outside the transaction and outside Cypher. Derived state that is wrong is never *observable* as wrong because it is epoch-gated: a topology change bumps `TopoGeneration()` and the cached artefact is discarded, not repaired. No WAL record, no recovery path, no new durability surface.

### F5. Exact-betweenness shattering (BADIOS) — real but conditional; I measured it and it is weaker than it looks  [NEW]  (severity: LOW–MEDIUM)

- **What they do:** neither incumbent applies decomposition to *static exact* betweenness. GDS pushes you to sampling instead: its betweenness page says the GDS implementation "is based on Brandes' approximate algorithm", exact "when all nodes are selected as source nodes", and recommends sampling because "for large graphs this can potentially lead to very long runtimes". Memgraph's BCC-localisation exists only inside the *dynamic* iCentral module, never for the static computation.
- **The idea:** Sarıyüce, Saule, Kaya & Çatalyürek, *Shattering and Compressing Networks for Centrality Analysis* (arXiv:1209.6007, 2012) / *Shattering and Compressing Networks for Betweenness Centrality* (SDM 2013) — BADIOS shatters at articulation points and bridges and compresses degree-1 / identical / side vertices, then runs Brandes on the pieces with correction terms. Reported: a 4.6M-edge graph's betweenness dropped from >5 days to <16 hours.
- **Evidence — I measured the two dominant rules on GoGraph's own `HopcroftTarjanBCC`, and the result is mostly negative:**

  *Articulation-point shattering* (cost model Σnᵢ·mᵢ vs n·m):

  | Graph | BCCs | Largest BCC | Predicted reduction |
  |---|---:|---|---:|
  | BA n=20k, m=1 (tree) | 19,999 | V=2 (0.01%) | 10,000× |
  | BA n=20k, m=2 | 8 | V=19,993 (**99.97%**) | **1.0×** |
  | BA n=20k, m=3 | 1 | V=20,000 (**100%**) | **1.0×** |
  | BA n=50k, m=2 | 10 | V=49,991 (**99.98%**) | **1.0×** |

  Scale-free graphs with m≥2 are 2-connected almost everywhere — shattering buys **nothing**. Only the degenerate tree case wins.

  *Iterative degree-1 peeling* (the rule that actually fires on real graphs):

  | Graph | Peeled | Reduction |
  |---|---:|---:|
  | 20k core (m=2), 0% leaves | 0.0% | 1.00× |
  | 20k core (m=2), 33% leaves | 33.4% | 1.88× |
  | 20k core (m=2), 50% leaves | 50.0% | 3.00× |
  | 20k core (m=3), 50% leaves | 50.0% | 2.67× |

  So the honest number is **~2–3× on graphs with a heavy leaf tail, 1.0× otherwise** — not orders of magnitude. The BCC pass itself is cheap (2.2 ms at V=20k, 6.9 ms at V=50k), so the *detection* is free.
- **Lever:** if adopted, make it **adaptive, not unconditional**: run the (already-free) BCC pass, and shatter/peel only when the measured largest-component fraction and degree-1 fraction predict a worthwhile reduction; otherwise go straight to plain Brandes. Do not build it speculatively — it is a 2–3× win on a subset of graphs, well below F2's ranking.
- **TCK/ACID impact:** none; pure function over a CSR snapshot, exact result by construction.
- **Effort:** M. **Recommendation: defer** behind F2 and F4(1).

### F6. Out-of-core is genuinely unique — but the capability is two algorithms wide  [CONFIRMED-R1, now precisely characterised]  (severity: MEDIUM)

- **Verified unique.** GDS operates on a *projected in-memory graph* held in the graph catalogue — a separate materialised copy, sized by `gds.graph.project.estimate`, entirely in RAM. Memgraph is an in-memory database. Neither can run an algorithm over a graph that does not fit in RAM. GoGraph can.
- **What GoGraph actually has — the precise characterisation round 1 did not give:**
  - **Model:** *semi-external memory*, stated explicitly at `search/extern/bfs.go:1-5`: "vertex-sized auxiliary structures (visited bitsets, level frontiers) live in RAM while edge data is streamed from the mapped file." So it is **not** fully external — it needs Θ(V) RAM. For `extern.PageRank` that is two `float64` rank arrays + `outdeg` + `isLive` ≈ **21 bytes/vertex**; for `extern.BFS` a 1-bit/vertex bitset plus frontiers.
  - **Mechanism:** `mmap` over the Tier-2 `csrfile` (`store/csrfile/reader.go:74`, `github.com/edsrzf/mmap-go`), with zero-copy reinterpretation of the mapped region as typed slices and an `madvise` hint path (`madvise_unix.go`). Eviction is the OS page cache, not an explicit buffer pool.
  - **Locality optimisation:** `extern/bfs.go:61` sorts each frontier before expansion "so that edge reads stay sequential, maximising the benefit of any MADV_SEQUENTIAL hint" — the classic external-memory BFS locality trick, correctly applied.
  - **Safety:** the whole traversal runs inside `Reader.Read`, so a concurrent `Close` blocks rather than unmapping mid-iteration (`extern/bfs.go:38-43`). Reader is safe for concurrent use.
  - **Validated at scale:** `bfs_dimacs9_full_test.go` (24M vertices / 60M edges, nightly), `bfs_rmat26_test.go` (RMAT-20, ~1M/16M, nightly), `pagerank_rmat24_test.go` (soak), `bfs_ldbc_sf10_test.go`, plus `tier1_vs_tier2_dimacs9_test.go` cross-checking in-memory against out-of-core. `go test ./search/extern/...` passes.
  - **Limits:** exactly **two** algorithms (BFS, PageRank), read-only, no weighted traversal, no explicit I/O scheduling or prefetch control.
- **Lever:** the differentiator is real but thin, and the marginal cost of widening it is low because the hard parts (mmap safety, `Reader.Read` lifetime discipline, frontier-sorted locality, the Tier-1/Tier-2 equivalence test harness) are already built and tested. The highest-value additions, in order: **(1) WCC** — Afforest already exists at `wcc_parallel.go:49` and is Θ(V) state + edge streaming, a near-direct port; **(2) k-core** — `kcore.go:22` bucket peeling is Θ(V) state and streams edges naturally; **(3) triangle counting** — needs sorted adjacency, which CSR already gives. Each follows the existing `extern` template and gets a Tier-1-vs-Tier-2 equivalence test for free.
- **TCK/ACID impact:** none — `search/extern` is read-only over an immutable, CRC-validated file and touches no transactional path.
- **Effort:** M per algorithm.
- **Nothing to take from them here:** GDS's projection model is strictly worse for this purpose *and* carries a consistency hazard GoGraph does not have — a GDS projection is a snapshot copy that does not track subsequent database writes, so results can silently reflect a stale graph until you re-project.

### F7. Exactness and determinism are documented contracts in GoGraph and are not in GDS  [NEW]  (severity: MEDIUM — this is a strength to protect, not a gap)

- **Evidence, GDS side:** its betweenness is *approximate by default* (Brandes sampling). Its delta-stepping "is not guaranteed to return the same path in each computation." Its Leiden page mentions determinism **zero times** — it offers only a `randomSeed` parameter described as "the seed value to control the randomness of the algorithm." Several catalogue entries are approximate by name: Approximate Maximum k-cut, HashGNN, kNN. Memgraph's `pagerank_online` is explicitly "only the approximation of PageRank."
- **Evidence, GoGraph side:** `floyd_warshall_parallel.go:27` ("bit-identical to the serial FloydWarshall for every…"), `johnson_parallel.go:21-22`, `triangles_parallel.go:24`, `centrality/pagerank.go:542` (`PageRanker.Run` "bit-for-bit identical to the equivalent one-shot" call), `community/leiden.go:84-91` (an explicit `# Determinism` section: reproducible via lower-NodeID anchor tie-breaking), `wcc.go:20` and `wcc_parallel.go:27` (deterministic ascending-NodeID relabel), `diameter.go:123`.
- **And the honesty is calibrated, not blanket:** `centrality/brandes_parallel.go:22-26` and `brandes_weighted_parallel.go:23-29` state plainly that parallel Brandes is "deterministic for a fixed numWorkers" but **not** bit-identical to serial — differing by up to ~1e-12 because parallelising across sources re-associates a float sum — and direct callers who need serial-identical output to `Betweenness`. That is a better-specified contract than anything in either incumbent's documentation.
- **Lever:** none to take. **Protect it.** Every lever in this report is scoped so the contract survives: F2's CH is exact, F4(1)'s warm-start PageRank converges to the same fixed point, F5's shattering is exact by construction, F6's out-of-core is validated against the in-memory tier. The one thing to *add* is visibility — these guarantees are in godoc but not in `docs/benchmarks/` or the README, where a prospective user comparing against GDS would look.
- **Effort:** S (documentation).

### F8. Breadth gaps — real, and mostly *not* worth closing for an embeddable Go library  [CONFIRMED-R1, with an applicability judgement]  (severity: LOW–MEDIUM)

- **The gaps are genuine:** no node embeddings (FastRP, GraphSAGE, node2vec, HashGNN), no ML pipelines, no similarity family (Node Similarity, kNN), no HITS / Article Rank / CELF / Degree Centrality, no Louvain, no K-1 Coloring / K-Means / HDBSCAN / Clique Counting / Conductance / Modularity Optimization, no Steiner trees, no Random Walk. Verified absent by grep across `search/` and `graph/`.
- **Applicability judgement, per the brief's instruction to judge by what GoGraph is for:**
  - **Not worth it:** GraphSAGE, GNNs, ML pipelines, temporal graph networks. These need a tensor/autodiff stack. Memgraph implements them in **Python**, which tells you the honest cost of doing them natively. An embeddable Go library should not grow a training framework; a user who needs this exports to PyTorch Geometric or DGL.
  - **Cheap and worth it:** **Degree Centrality** (trivial over CSR), **HITS** (a two-vector power iteration — the parallel pull-SpMV machinery at `pagerank.go` is already there and already bit-identical), **Article Rank** (a PageRank variant), **Random Walk** (needed anyway as the substrate for anything sampling-based). These are S-sized and close the most visible catalogue-comparison gaps.
  - **Medium and defensible:** **Node Similarity / kNN** over node properties or neighbourhoods. This is the one ML-adjacent family with a clean exact formulation (Jaccard/overlap/cosine over adjacency sets), it parallelises across nodes deterministically, and it is the standard building block users reach for.
  - **Deliberately skip:** Louvain — already on the round-1 consensus-rejection list, and correctly so: GoGraph's Leiden is strictly better (Leiden fixes Louvain's disconnected-community defect) and is bit-reproducible, which GDS's Louvain is not.
- **TCK/ACID impact:** none — all are pure functions over a CSR snapshot.
- **Effort:** S for degree/HITS/ArticleRank/RandomWalk; M for similarity/kNN.

---

## Nothing-to-take list

1. **GDS's projected in-memory graph catalogue.** A separate materialised RAM copy of the graph, sized and estimated ahead of time, that does **not** track writes to the underlying database — results silently reflect a stale snapshot until re-projected. GoGraph's CSR snapshot + `TopoGeneration()` epoch (`graph/lpg/lpg.go:1903`) solves the same problem with an explicit staleness signal and no duplicated storage. Reject.
2. **Memgraph's trigger-driven `*_online` state.** Mutable, non-transactional algorithm state mutated by a commit-time trigger. Under GoGraph's ACID mandate this state would have to be atomic with the commit, roll back with the transaction, and survive `kill -9` — turning an algorithm cache into a durable subsystem holding *approximate* values. Take the **iCentral idea** (localise to the affected biconnected component); reject the **mechanism**. See F4.
3. **Delta-stepping and the whole parallel-SSSP family, at ~10 cores.** Three independent disqualifiers: Δ is untunable by a library (>4× penalty for a wrong value, 2^8 variation across graphs with identical weight distributions), the published wins are measured at 96 cores/192 hyperthreads, and GDS's own docs admit path non-determinism. See F3.
4. **The 2025 O(m log^{2/3} n) sorting-barrier SSSP.** A landmark theory result, but the independent implementation study (arXiv:2511.03007) finds Dijkstra faster "for all tested sparse graph sizes, including instances with tens of millions of vertices."
5. **Parallel push-relabel max-flow (GDS's choice).** The flow *value* is unique but the flow *assignment* is not, so a parallel push-relabel returns schedule-dependent per-edge flows. Take GDS's multi-source/multi-sink and node-capacity **API surface** (F1); keep the deterministic serial Dinic core.
6. **Sampling-by-default betweenness (GDS).** GoGraph's exact Brandes is the better default for an embeddable library where the caller controls graph size. If an approximation is ever wanted, prefer an adaptive-stopping estimator with a stated error bound over fixed-size sampling — but only on explicit opt-in.
7. **GNN / embedding training (both).** Memgraph implements these in Python; that is the honest signal of what native implementation costs. Out of scope for a zero-dependency Go library.
8. **Louvain.** Already rejected in round 1 and still correct: GoGraph's Leiden is strictly stronger and is bit-reproducible, which GDS's Louvain is not.

---

## Recommended order

| # | Lever | Finding | Effort | Why here |
|---|---|---|---|---|
| 1 | Contraction Hierarchies point-to-point index | F2 | L | Only place GoGraph can beat **both** by orders of magnitude; 27.9 ms/query measured today; invalidation infrastructure already exists; zero TCK risk |
| 2 | Warm-start `PageRanker.RunFrom` | F4 | S | Captures the useful half of Memgraph's dynamic story, exactly, with no ACID surface; hooks into state that already exists |
| 3 | Multi-source/sink + node capacities for max-flow | F1 | S | Closes the real (narrow) gap GDS opened; pure API over existing Dinic |
| 4 | Widen `search/extern` to WCC, k-core, triangles | F6 | M each | Deepens the one genuinely unique capability; template and test harness already built |
| 5 | Degree / HITS / Article Rank / Random Walk | F8 | S | Cheap catalogue-parity wins on existing SpMV machinery |
| 6 | Document the exactness/determinism contract outside godoc | F7 | S | The strongest differentiator is currently invisible where users compare |
| — | Adaptive BADIOS shattering | F5 | M | **Defer** — measured 1.0× on 2-connected graphs, 2–3× only with heavy leaf tails |
| — | Delta-stepping | F3 | — | **Reject** |

## Method notes

- GoGraph inventory: `grep -rn '^func [A-Z]'` across `search/`, `search/{centrality,community,flow,extern}` → ~55 exported entry points across 34 + 21 non-test files; complexity claims read from the godoc of each entry point.
- Measurements: purpose-written benchmarks/tests placed temporarily in `search/`, run with `go test -bench`/`-run`, then removed (`git status` clean of my files; other streams' scratch files remain). Grid graphs are 4-neighbour lattices with integer weights 1..100 (road-like: high diameter, low degree); BA graphs are preferential-attachment with a seeded PCG.
- GDS facts: `neo4j.com/docs/graph-data-science/current/...` fetched with `curl` (WebFetch is 403-blocked by neo4j.com), version banner **2026.06**; release history via the GitHub releases API for `neo4j/graph-data-science`.
- Memgraph facts: `memgraph.com/docs/advanced-algorithms/available-algorithms[/...]`, 78 modules enumerated with their implementation language from the docs' own table.
- Papers: arXiv HTML renderings where available (2105.06145 SPAA'21, 2511.03007, 1504.05140); citations given with venue and identifier.
- WebSearch budget was exhausted session-wide partway through; remaining external verification used `curl` + WebFetch directly against primary sources.
