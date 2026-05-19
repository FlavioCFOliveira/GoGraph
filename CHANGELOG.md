# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

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
