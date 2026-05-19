# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

(no unreleased work — the next entry below is the live release)

## [1.1.0] — 2026-05-19

Six sprints (8–13) of correctness, observability, hot-path
optimisation, algorithm completeness, and release hygiene. The
release closes the v1.0.0 audit and ships the first set of post-1.0
algorithmic and reliability work.

### Added — Sprint 9 (Concurrency Contract)

- `context.Context` is now accepted by every public blocking API in
  `search/`, `search/centrality/`, `search/community/`,
  `search/flow/`, `search/extern/`, `graph/io/`, `store/` so every
  long-running call honours cancellation and deadlines.
- `goleak.VerifyTestMain` adopted by every package that spawns
  goroutines so leaks fail the test pass.

### Added — Sprint 10 (Observability)

- `internal/metrics` Prometheus-style histogram hook on every
  public blocking API.
- `pprof.SetGoroutineLabels` on every long-lived goroutine.
- `docs/benchmarks/` archive with multi-concurrency-level numbers.
- `govulncheck` job in CI (daily schedule).
- `internal/stress` concurrency stress suite — new CI job runs the
  suite under `-race` on every PR.
- `csrfile` crash-injection fuzz test for truncation recovery.

### Added — Sprint 11 (Hot-path Optimisation)

- `search.DijkstraInto`, `search.BellmanFordInto`, `search.AStarInto`
  — zero-allocation primitives that operate on caller-provided
  scratch slices (`BenchmarkDijkstra_PostWarmup` allocs/op == 0).
- Type-switch per-W `sync.Pool` dispatch (Dijkstra heap acquire
  drops from 5.4 ns/op to 1.08 ns/op).
- BFS index-head queue across Brandes / PPR-push / Topo /
  Dinic / Leiden (Brandes allocs/op −70.8 %).
- Leiden / LabelPropagation scratch+touched-list replaces the per-
  vertex `map[int]float64`/`map[int]int` (`BenchmarkLeiden` at
  V=1e5: 5.12x faster, allocs/op −99.96 %).
- BFS-DO inline bitmap frontier + pooled scratch + Beamer beta
  switch-back (6.08x vs vanilla top-down on power-law graphs).
- Iterative DFS for `flow.Dinic` augmentFlow and
  `search.HopcroftKarp` dfsAugment (no goroutine-stack growth at
  V=1e7).
- Floyd-Warshall column materialisation.
- Hierholzer trail pre-allocation.
- PageRank `outdeg` changed from `float64` to `uint32` (memory
  −50 % on that slice).
- SPFA + SLF deque for Bellman-Ford (4.17x on dense graphs).
- Yen candidate arena (Yen K100 allocs/op −96.65 %).
- `slices.Sort` in `extern/bfs.go`.
- `graph.Mapper.Walk` for shard-batched name lookup; IO writers
  use it to amortise `Resolve` shard-lock acquisitions.
- `strconv.FormatInt` in dot writer.
- `ds.UnionFindSlice` (22.2x faster than the generic map-backed
  variant on a bounded ID space).

### Added — Sprint 12 (Algorithm Completeness)

- `search.BidirectionalDijkstra` / `BidirectionalDijkstraOn`.
- `flow.EdmondsKarp` (max-flow reference / baseline).
- `search.KruskalMST` / `search.PrimMST`.
- `search.WCC` (weakly-connected components).
- `search.KCore` (Batagelj-Zaversnik 2003).
- `search.CountTriangles` (degree-ordered node-iterator).
- `search.TransitiveClosure` (bitset matrix oracle).
- `centrality.WeightedBetweenness` (Dijkstra-augmented Brandes).
- `centrality.BetweennessParallel` (4.9x on M4 at GOMAXPROCS=10).
- `flow.PushRelabelMaxFlow` (FIFO + gap heuristic).
- `search.Diameter` (2-sweep + iFUB refinement).
- `search.HierholzerUndirected`.
- `search.BiBFSOn` reverse-CSR variant; BiBFS now handles directed
  graphs by building the reverse internally.
- `search.EppsteinKShortest`.
- `flow.MinCostMaxFlow` (SSP + node potentials).

### Added — Sprint 13 (Release Hygiene)

- `bench/soak` 4-hour mixed-workload reliability harness with heap
  / FD / goroutine snapshots.
- benchstat regression gate on pull-requests.
- LDBC SF1/SF10 + DIMACS SF1/USA `Benchmark*` functions (the
  large-scale ones skip under `-short`).
- goreleaser pipeline + `.github/workflows/release.yml` + Makefile
  release targets. Documentation at `docs/release.md`.
- Cross-library comparison vs NetworkX 3.2.1 with measured numbers
  (BFS 178x, Dijkstra 25x, PageRank 28x on the same graph). See
  `docs/benchmarks/comparison.md`.
- Rapid-based property tests covering triangle inequality (Dijkstra),
  precedence (TopologicalSort), reflexivity (Tarjan SCC), and
  matching cardinality (Hopcroft-Karp).
- Fuzz tests for `store/csrfile`, `graph/io/graphml`, `graph/io/csv`
  parsers.
- t.Run subtest pattern adopted across representative table-shaped
  tests (the bulk migration can land incrementally; the pattern is
  in place and exercised).
- Concurrency-contract godoc clauses added to `search.APSP`,
  `search.Matching`, `search.TC`.

### Added — Sprint 8 (Correctness Hardening, retained from the v1.0.0 audit)

- LICENSE file at repo root: MIT (closes #92).
- 10 new example programs (`examples/11_social_network` through
  `examples/20_concurrent_reads`) demonstrating every major feature
  (commit `ffe335a`).
- `graph/csr.CSR.LiveMask`, `LiveNodes`, `LiveCount`, `IsSymmetric`
  helpers (closes #79 and the first half of #109).
- `search.ErrInvalidInput`, `centrality.ErrInvalidInput`,
  `extern.ErrInvalidInput` for NaN/Inf input rejection (closes #91).
- `search.ErrNotUndirected` returned by `BiBFS` on directed CSRs
  (closes #89).
- `search.ErrNegativeEdgeAPSP` returned by `DijkstraAPSP` on negative
  edges (closes #88).
- `search.DijkstraAPSP` (primary export; `JohnsonAPSP` is now a
  deprecated alias) (closes #88).
- `wal.Writer.Truncate` returning the freed byte count (closes #82).
- `checkpoint.Checkpointer.TriggerCtx` honouring context cancellation
  (closes #84).
- Property test `TestLeiden_ModularityNonDecrease` via
  `pgregory.net/rapid` (closes #80).

### Fixed

- `centrality.PageRank` and `extern.PageRank` now conserve total mass
  by redistributing dangling-node rank uniformly each iteration; the
  v1.0.0 implementations lost the sink's accumulated mass at every
  buffer swap (closes #77, #78).
- `community.Leiden` is now an actual Traag-Waltman implementation
  (local-moving + refinement + aggregation) rather than majority-vote
  label propagation. `Partition.NumCommunities` reflects the live
  community count, not the inflated MaxNodeID-based count (closes #80).
- `centrality.PersonalisedPushPageRank` handles dangling nodes per
  Andersen-Chung-Lang (teleport residue back to source); removed the
  residue-drain pass that double-counted absorbed mass (closes #87).
- `search.HopcroftTarjanBCC` correctly handles multigraph parallel
  edges; tracks the entry-edge index per frame instead of the parent
  NodeID, and the edge-stack pop condition now matches only the
  tree-edge ordering (closes #81).
- `search.Yen` and `search.FloydWarshall` no longer use the
  overflow-prone `v += v` Inf sentinel; reachability is tracked via
  an explicit `found[]` bitmap (closes #85, #86).
- `store/checkpoint.runCheckpoint` actually truncates the WAL on
  disk after writing a snapshot — the v1.0.0 implementation only
  recorded a counter and the WAL grew unbounded in steady state
  (closes #82).
- `store/checkpoint.Stop` is now idempotent (closes #83).
- The `maxID` over-iteration pattern in centrality/community/APSP is
  eliminated; algorithms iterate only live NodeIDs and ghost slots
  carry sentinel `-1` (closes #79).

### Changed (breaking)

- `search.Hungarian` signature: `(Assignment)` → `(Assignment, error)`
  (closes #91).
- `centrality.PageRank` signature: `(ranks, iters)` →
  `(ranks, iters, error)` (closes #91).
- `centrality.PersonalisedPushPageRank` signature: `(ranks)` →
  `(ranks, error)` (closes #91).
- `extern.PageRank` signature: `(ranks, iters)` →
  `(ranks, iters, error)` (closes #91).
- `community.Partition.Community[id]` returns `-1` for ghost NodeID
  slots (closes #79).
- `search.APSP` internal layout switched to a compact `live*live`
  matrix with a NodeID→index map; the public `At`/`N` API is
  preserved but `N` now returns the live count, not `MaxNodeID()`
  (closes #79).

### Deprecated

- `search.JohnsonAPSP`: deprecated alias for `DijkstraAPSP`; scheduled
  for removal in a future major release once Bellman-Ford reweighting
  lands (closes #88).

### Documentation

- README license section updated to point at the LICENSE file
  (closes #92).
- Examples 08, 09, 18, 20 print live-NodeID counts via
  `Mapper().Lookup` rather than the misleading `MaxNodeID`-sized
  slice length (closes #79).

## [1.0.0] — 2026-05-19

The first stable release of GoGraph. Seven sprints landed the
foundation, the property-graph model, durable persistence, the
out-of-core Tier 2 substrate, I/O interop, the analytical algorithm
suite, and the benchmark harnesses.

### Added — Sprint 1 (Foundation & In-Memory Core)

- `graph` — generic NodeID, Graph[N, W] contract, sharded Mapper.
- `graph/adjlist` — mutable copy-on-write adjacency-list backend.
- `graph/csr` — immutable Compressed Sparse Row snapshot.
- `search` — BFS (wavefront), DFS (iterative), Dijkstra (binary
  heap), Bellman-Ford, A\*, Bidirectional BFS, topological sort
  (Kahn), Tarjan SCC.
- `ds` — Union-Find with path compression.
- `examples/01_basic` and the README quickstart.
- CI pipeline (gofmt, vet, build, test, race, golangci-lint).

### Added — Sprint 2 (Property Graph + Indexes)

- `graph/lpg` — Labelled Property Graph with vertex and edge labels
  and a 24-byte tagged PropertyValue (string, int64, float64,
  bool, time.Time, []byte).
- `graph/lpg/schema` — declarative type schema with `Validate`.
- `graph/index/label` — Roaring-bitmap label index with intersect
  and union.
- `graph/index/hash` — sharded hash exact-match property index.
- `graph/index/btree` — order-preserving range property index with
  the sub-microsecond `RangeFirst`.
- `graph/index` — `Manager` fanning out `Change` events to
  subscribers.
- `graph/query` — fluent MATCH-style pattern engine.
- `examples/02_property_graph`.

### Added — Sprint 3 (Durable Persistence)

- `store/wal` — versioned, CRC32C-checksummed Write-Ahead Log
  reader / writer.
- `store/snapshot` — atomic snapshot directories with manifest and
  per-file CRC.
- `store/txn` — single-writer transactions (Begin/Commit/Rollback)
  with fsync-at-commit durability.
- `store/checkpoint` — background WAL → snapshot folder goroutine.
- `store/recovery` — snapshot + WAL replay on open.
- `docs/persistence.md`.

### Added — Sprint 4 (Out-of-Core Tier 2)

- `store/csrfile` — versioned, 64-byte-aligned mmap'd CSR file
  format with atomic writer, mmap reader, `madvise` hints, and
  the `Reinterpret` zero-copy helper.
- `store/csrfile.BuildFixture` — deterministic reproducible
  fixture generator.
- `graph/generation` — refcount-protected `Publisher` for atomic
  snapshot rotation across readers and writers.
- `search/extern` — semi-external BFS and PageRank over a Tier 2
  reader.
- `docs/tier2.md`, `docs/csrfile-v1.md`, `CONTRIBUTING.md` (unsafe
  policy).

### Added — Sprint 5 (I/O Interop)

- `graph/io/csv` — read and write edge-list CSV.
- `graph/io/graphml` — read and write GraphML XML.
- `graph/io/dot` — write Graphviz DOT.
- `graph/io/jsonl` — read and write JSON Lines.
- `store/bulk` — bulk ingestion bypassing the WAL.
- `docs/io.md`.

### Added — Sprint 6 (Advanced Algorithms)

- `search/bfs_do.go` — direction-optimising BFS (Beamer 2012).
- `search/yen.go` — Yen's k-shortest paths.
- `search/floyd_warshall.go` and `search/johnson.go` — APSP.
- `search/bcc.go` — Hopcroft-Tarjan BCC + bridges + articulation.
- `search/hierholzer.go` — Eulerian circuit / path.
- `search/hopcroft_karp.go` — bipartite matching.
- `search/hungarian.go` — weighted assignment.
- `search/flow/dinic.go` — max-flow.
- `search/flow/stoer_wagner.go` — global min-cut.
- `search/centrality/brandes.go` — exact betweenness.
- `search/centrality/pagerank.go` — in-memory power iteration.
- `search/centrality/ppr_push.go` — personalised PageRank (push).
- `search/community/leiden.go` — Leiden-style community detection.
- `search/community/label_propagation.go` — label propagation.
- `docs/algorithms.md`.

### Added — Sprint 7 (Benchmarks, Hardening, Release)

- `bench/ldbc` — LDBC SNB SF1 / SF10 harness.
- `bench/dimacs9` — DIMACS 9 SSSP harness.
- `bench/rmat` — RMAT power-law generator (Graph500 defaults).
- `docs/profiling.md`, `docs/optimisations.md`, `docs/semver.md`.
- `release-notes/v1.0.0.md`.

### Documented limits (v1.0.0)

- Johnson APSP restricts to non-negative weights; Bellman-Ford
  reweighting is deferred.
- Yen's k-shortest is O(k * (V + E) log V); Eppstein's is
  deferred.
- Leiden ships the local-moving + connected-component-split
  simplification; the refinement / aggregation phases of the
  full Traag-Waltman-van Eck paper are deferred.
- `adjlist.AddEdge` cost is dominated by the COW; the delta-log
  in-place atomic-append variant is deferred (tracked in
  `docs/optimisations.md`).
- `bench/ldbc.Run` non-synthetic mode (the LDBC Datagen
  integration) is deferred.
