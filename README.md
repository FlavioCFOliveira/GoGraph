# GoGraph

A Go module for graph persistence, manipulation, and fast search,
designed to scale from in-memory graphs to graphs that exceed RAM.

## Status

**Current release: `v0.12.0`.** This is the project's **fifteenth
release**, published at a pre-1.0 baseline: under Semantic Versioning a
`0.y.z` version signals that the public API is **not yet stable** and may
change without a major bump while the module matures toward `1.0.0`.
`v0.12.0` is a pre-1.0 **MINOR** release of **192 commits** across **six
sprints (345–350) and 176 closed tasks**, and — unlike `v0.11.0` — it
**breaks nothing**: no exported identifier was removed, no exported
signature changed, and no dependency moved — `go.sum` is byte-identical
and the only `go.mod` change is the pinned toolchain, `go1.26.5` →
`go1.27.0` — so existing code compiles and upgrades unchanged. It adds
no engine capability. It is a **testing-and-correctness release**.

Two things define it. **A deterministic-simulation-testing campaign drove
every published surface** — MVCC under true concurrency, the full Cypher
language surface, the storage/durability/MVCC substrate, the Bolt wire, and
the graph API, search and bulk-load surfaces. That is test infrastructure
rather than module functionality, and the split is worth stating plainly:
`internal/sim` accounts for **75.7 %** of the release's changed lines while
the module's own production code accounts for **5.1 %**. **And the bug
backlog was emptied** — every known bug resolved, each premise re-validated
at HEAD first, with seven premises that no longer held recorded as refuted
rather than reported as fixes.

What a user consumes is the **50 defects found and fixed in the shipped
module**, five of them silent — wrong or lost data with no error anywhere.
Every edge weight of a non-primitive type was **permanently lost at the
first checkpoint** (95 of 95 weights present at the WAL boundary, 0 of 191
after one checkpoint), and bulk import was worse because it bypasses the
WAL. `MATCH (:Person)-[:KNOWS]->(:Vip) RETURN count(*)` returned **0**
where the named-source spelling returned 1, on the shipped default.
Reverse and undirected traversal over a reciprocal edge pair returned **the
other edge's** properties and endpoints. **Every** Cypher list crossing the
Bolt wire reached clients as a string. And a write transaction's own
uncommitted view leaked through the CSR cache, so **committed edges lost
their types for every reader**. Alongside them: seven ACID fixes spanning
Consistency, Isolation, Atomicity and Durability, a CWE-59 arbitrary-file
overwrite in `store/csrfile`, four Bolt security fixes, a data race that
could produce a wrong answer, six `search` entry points that ignored a
cancelled context, and a panic reachable from the public API in three calls.

**The most consequential result is a control, not a scenario.** Until this
release the crash model retained unsynced data across a simulated host
crash, so the battery **could not fail an engine that stopped calling
`fsync` at all**. With a durable-length watermark in place, deleting the
WAL commit fsync now fails loudly — 192 violations, exactly half of 384
acknowledged commits lost, 33 tests red — where before it failed nothing.

The `v0.11.0` **open plan-choice regression is closed**: the selective
multi-label query that was ≈21.6× slower in the default configuration is
**−96.08 %** and no shape regresses. One cost is large and is stated rather
than omitted: a correctness guard on anonymous pattern heads costs the
declined shape **62× to 1705×** (rmp #2604 is filed to recover it).

The two compliance invariants remain in force: the module is **100 %
openCypher TCK-compliant at the execution level** (**3 897/3 897 scenarios**,
16 006/16 006 steps, preserved rather than extended — no `.feature` file
changed this cycle) and **100 % ACID-compliant**. Every change is gated by
the project's local validation pipeline, run via `make ci` /
`make release-preflight` before it lands; at the release tree that gate
reports **127 packages ok with zero failures** under `go test -race ./...`,
**0 lint issues**, and **88.3 %** aggregate coverage with every package
above its 75 % floor. **This release carries no production certification of
its own** — the most recent one was taken at the `v0.11.0` commit, 192
commits behind this tag, and the whole-tree soak layer has now gone unrun
for five consecutive cycles. The module uses the conventional Go path
`github.com/FlavioCFOliveira/GoGraph` and is fetchable with
`go get github.com/FlavioCFOliveira/GoGraph@v0.12.0`. See
[CHANGELOG.md](CHANGELOG.md),
[release-notes/v0.12.0.md](release-notes/v0.12.0.md) and
[docs/benchmarks/v0.12.0.md](docs/benchmarks/v0.12.0.md) for the full release
narrative, the measured performance delta, and what the release does **not**
establish.

### Core graph (`graph/`)

- `github.com/FlavioCFOliveira/GoGraph/graph` — generic node identifiers and the `Graph[N, W]`
  contract.
- `github.com/FlavioCFOliveira/GoGraph/graph/adjlist` — mutable, sharded adjacency-list backend
  with copy-on-write snapshots and lock-free reads. Every edge slot carries a
  stable identity, and adjacency is versioned inside the immutable entry so a
  snapshot resolves it at one instant.
- `github.com/FlavioCFOliveira/GoGraph/graph/mvcc` — the concurrency-control substrate: a
  transaction clock and shared commit records, a contiguous commit frontier,
  the reclamation horizon and watermark, `Gate` (a weak/strong admission gate),
  and `ErrSerializationConflict`. New in `v0.11.0`; MVCC is the module's only
  concurrency-control mechanism and is armed by `lpg.New`.
- `github.com/FlavioCFOliveira/GoGraph/graph/csr` — immutable Compressed Sparse Row view for
  read-mostly analytics.
- `github.com/FlavioCFOliveira/GoGraph/graph/generation` — atomic pointer swap for snapshot
  rotation across readers/writers.
- `github.com/FlavioCFOliveira/GoGraph/graph/lpg` — Labelled Property Graph model (vertex and
  edge labels, typed properties; `PropertyValue` covers string,
  int64, float64, bool, time.Time, []byte, and list ([]PropertyValue)).
- `github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema` — optional type schema with `Validate`.
- `github.com/FlavioCFOliveira/GoGraph/graph/index` — `Manager` coordinating named indexes and
  fanning out `Change` events to subscribers.
- `github.com/FlavioCFOliveira/GoGraph/graph/index/label` — Roaring-bitmap inverted label index.
- `github.com/FlavioCFOliveira/GoGraph/graph/index/hash` — sharded hash exact-match property
  index.
- `github.com/FlavioCFOliveira/GoGraph/graph/index/btree` — order-preserving B+ tree range
  property index (backs the Cypher range-predicate index seek).
- `github.com/FlavioCFOliveira/GoGraph/graph/query` — fluent `MATCH`-style pattern engine.
- `github.com/FlavioCFOliveira/GoGraph/graph/io/csv` · `graph/io/graphml` · `graph/io/dot` ·
  `graph/io/jsonl` — interchange formats for CSV, GraphML, DOT,
  and JSON Lines.
- `github.com/FlavioCFOliveira/GoGraph/ds` — disjoint-set (union-find) primitive.
- `github.com/FlavioCFOliveira/GoGraph/metrics` — the observability seam: `SetBackend` plus
  counters, gauges, latency histograms and `Time`. Every cache, pool and bounded
  queue publishes utilisation through it, as does the MVCC substrate (writers in
  flight, outcomes, conflicts by store, version-chain depth, vacuum latency,
  horizon utilisation). Wire-up in [docs/metrics.md](docs/metrics.md).

### Search and analytics (`search/`)

- `github.com/FlavioCFOliveira/GoGraph/search` — traversal and path-finding algorithms (BFS,
  iterative DFS, Dijkstra, Bellman-Ford, A\*, bidirectional BFS,
  Yen k-shortest, topological sort (Kahn), Tarjan SCC, biconnected
  components, Eulerian path, APSP).
- `github.com/FlavioCFOliveira/GoGraph/search/centrality` — Brandes betweenness, PageRank
  (parallel pull-formulation over a reverse-CSR on large graphs,
  bit-identical to the serial path), personalised PageRank.
- `github.com/FlavioCFOliveira/GoGraph/search/community` — Leiden, label propagation.
- `github.com/FlavioCFOliveira/GoGraph/search/flow` — Dinic, Edmonds-Karp, push-relabel,
  Stoer-Wagner, min-cost max-flow.
- `github.com/FlavioCFOliveira/GoGraph/search/extern` — semi-external BFS and PageRank over
  Tier 2 csrfile readers.

### Storage and persistence (`store/`)

- `github.com/FlavioCFOliveira/GoGraph/store/wal` — Write-Ahead Log with CRC32C framing.
- `github.com/FlavioCFOliveira/GoGraph/store/snapshot` — atomic on-disk snapshot directories.
- `github.com/FlavioCFOliveira/GoGraph/store/txn` — transactional API
  (Begin/Commit/Rollback). **Independent write transactions run concurrently**:
  the single-writer semaphore was retired in `v0.11.0`, so a write-write
  collision is *detected* by MVCC first-updater-wins and returned as a retriable
  error wrapping `mvcc.ErrSerializationConflict`, rather than prevented by
  exclusion.
- `github.com/FlavioCFOliveira/GoGraph/store/checkpoint` — background WAL → snapshot folder.
- `github.com/FlavioCFOliveira/GoGraph/store/recovery` — snapshot + WAL replay on open.
- `github.com/FlavioCFOliveira/GoGraph/store/csrfile` — mmap-backed Tier 2 CSR file format,
  writer, reader, `Reinterpret` zero-copy helper, deterministic
  fixture generator.
- `github.com/FlavioCFOliveira/GoGraph/store/bulk` — high-throughput bulk loader bypassing
  the WAL. Adjacency only: no labels, no properties, and its output is a Tier 2
  csrfile rather than a store.
- `github.com/FlavioCFOliveira/GoGraph/store/bulkimport` — offline bulk **import**: builds a
  labelled property graph and publishes it as a store snapshot, so
  `recovery.Open` reads it back with no WAL. Loads 20 000 nodes and 200 000 edges
  in **0.28 s** of process wall clock (a 233 ms import phase, 0.86 M edges/s) —
  see
  [docs/benchmarks/bulk-import-2026-07-26.md](docs/benchmarks/bulk-import-2026-07-26.md)
  and [docs/design-bulk-import.md](docs/design-bulk-import.md). For scale, the
  Cypher write path loads comparable data (20 000 nodes / 199 941 edges in
  `UNWIND` batches of 5 000) in **2.056 s**, measured in
  [docs/benchmarks/threeway-durability-2026-07-27.md](docs/benchmarks/threeway-durability-2026-07-27.md)
  — down from 35 m 10 s before `#2228` admitted the hash join for writing
  statements. Those are two different harnesses, so treat them as two figures
  rather than one ratio. The whole import is atomic; it is not a transaction, it
  cannot be rolled back, and it requires an empty target directory.

### Cypher engine (`cypher/`)

- `github.com/FlavioCFOliveira/GoGraph/cypher` — openCypher-compatible parser, planner, and
  execution engine; WAL-durable writes via `NewEngineWithStore`. An explicit read
  transaction (`BeginReadTx`) is **snapshot isolated across all of its
  statements**; an explicit write transaction (`BeginTx`) holds no lock, so two
  clients can hold open write transactions and both make progress.
  `Engine.NewSession` returns a `Session` giving **read-your-own-writes** across
  transactions.
- `github.com/FlavioCFOliveira/GoGraph/cypher/parser` · `cypher/ast` · `cypher/sema` ·
  `cypher/ir` · `cypher/plan` · `cypher/exec` — parser-to-execution
  pipeline with plan-cache, `EXPLAIN` of the **physical** plan, `PROFILE` with
  per-operator rows/time/dbhits, and per-statement write-effect counters
  (`Result.Counters`).
- `github.com/FlavioCFOliveira/GoGraph/cypher/funcs` · `cypher/procs` — built-in functions and
  procedures.
- `github.com/FlavioCFOliveira/GoGraph/cypher/tck` — openCypher TCK harness (parser 100 %,
  execution 100 % — 3 897/3 897 scenarios; see
  [docs/tck/DIVERGENCES.md](docs/tck/DIVERGENCES.md)).

### Bolt server (`bolt/`)

- `github.com/FlavioCFOliveira/GoGraph/bolt/proto` · `bolt/packstream` — Bolt v5 protocol and
  PackStream encoding (v5.0–v5.6 preferred; v4.4 fallback).
- `github.com/FlavioCFOliveira/GoGraph/bolt/server` — TCP server compatible with
  `neo4j-go-driver` v5 and `cypher-shell`, with TLS certificate
  hot-reload and graceful shutdown. Nodes, relationships and paths are sent as
  Bolt **structures** (so the official driver materialises them as
  `dbtype.Node`/`Relationship`/`Path`), each connection owns a `cypher.Session`
  for read-your-own-writes, and open transactions are **operable** — bounded by
  idle time and per-principal count, and listable and terminable through an
  operator API. Engine-wide memory ceilings are bounded by default and derived
  from a container's cap where one exists.

Subsystem references: [docs/persistence.md](docs/persistence.md)
(WAL, snapshots, recovery) · [docs/tier2.md](docs/tier2.md) (csrfile)
· [docs/io.md](docs/io.md) (interchange formats)
· [docs/algorithms.md](docs/algorithms.md) (algorithms catalogue)
· [docs/cypher.md](docs/cypher.md) (Cypher engine)
· [docs/bolt.md](docs/bolt.md) (Bolt server).

## Examples

The `examples/` directory contains **37 runnable demonstrations**, numbered
`01`–`37`. They are not part of the module — nothing in GoGraph imports them —
they are exercise harnesses and usage simulators: each drives real features
under realistic conditions and emits telemetry, and **every one can produce a
pprof profile**. See [examples/README.md](examples/README.md) for the full
categorized index with per-example links and run commands.

### Basics

- **01_basic** — Dijkstra on a small European routing graph.
- **02_property_graph** — labels + typed properties + indexed query.
- **03_advanced_algorithms** — BFS, Dijkstra, Brandes betweenness, and PageRank composed over one CSR snapshot.

### Persistence and out-of-core

- **04_persistence** — WAL transactions + recovery.
- **05_out_of_core** — Tier 2 csrfile + mmap + semi-external PageRank.
- **17_transactional_log** — WAL + background checkpointer + crash-recovery walk-through.
- **18_oocore_pipeline** — CSV → CSR → csrfile → mmap → semi-external BFS + PageRank.
- **21_typed_recovery** — generic `recovery.Open[N, W]` over an `(int64, float64)` graph with typed properties; round-trips through a v2 snapshot.

### Cypher and Bolt

- **22_cypher** — Cypher execution engine social-graph demo: label scan with `ORDER BY`, `WHERE` filter, relationship pattern, and `CREATE` — values printed in human-readable form.
- **23_bolt_server** — Bolt v5 server round-trip: a real `neo4j-go-driver` v5 client runs a Cypher query over the wire, then the server shuts down cleanly with no goroutine leak.
- **24_social_network_cli** — interactive CLI over a persistent LPG social network (WAL + recovery + Cypher queries).
- **25_software_house_api** — multi-layer LPG REST API over a software-house domain (Code/Work/People entities).

### Interchange

- **06_csv_import** — CSV read / write + JSON Lines.
- **07_graphml_roundtrip** — GraphML read / write + DOT.

### Algorithms

- **08_pagerank** — PageRank on a directed authority web, ranking pages from most to least important with distinct ranks.
- **09_leiden** — community detection on two cliques + bridge.
- **10_dimacs9_routing** — DIMACS 9 synthetic road network + a concrete Dijkstra SSSP query with a reconstructed shortest path.
- **14_routing_alternatives** — Dijkstra, Yen k-shortest, and A\* with a coordinate-based Euclidean heuristic that expands fewer nodes for the same optimal cost.
- **15_task_assignment** — Hungarian (cost-minimising) + Hopcroft-Karp (cardinality).
- **16_centrality_analytics** — Brandes betweenness + label propagation.

### Real-world recipes

- **11_social_network** — labels + PageRank + Leiden + friend-of-friend recommendations.
- **12_build_dependency** — topological sort + Tarjan SCC for circular-dependency detection.
- **13_network_reliability** — Hopcroft-Tarjan SPOF analysis + max-flow with the limiting min-cut bottleneck, both over the same network.
- **19_pattern_query** — multi-hop MATCH-style queries combining labels and property predicates.
- **20_concurrent_reads** — multiple algorithms run concurrently over a shared immutable CSR.
- **28_negative_weights** — negative-weight routing.
- **29_all_pairs** — all-pairs shortest paths.
- **30_min_spanning_tree** — minimum spanning tree as a least-cost backbone.
- **32_euler** — Eulerian circuits for route inspection.

### Concurrency, MVCC and isolation

- **27_concurrent_txn** — concurrent transaction isolation, with a conserved-total oracle.
- **33_generation_swap** — generation snapshot-swap on a read-mostly workload.
- **35_mvcc_mixed_workload** — reader latency under a mixed OLTP-and-analytics workload.
- **36_mvcc_snapshot_topology** — snapshot isolation on the topology dimension.
- **37_mvcc_write_contention** — MVCC under concurrent **writers**.

### Scale, observability and operations

- **26_social_scale_bench** — social-network scale benchmark; the reference harness for planner and execution work.
- **31_metrics_observability** — the `metrics` seam exported in Prometheus format.
- **34_bolt_transactions** — Bolt transactions, writes, auth and TLS.

Run any example with `go run ./examples/<NAME>/`, and `-h` to see its flags.
Every example binds one identical profiling contract from
`examples/internal/exprof`: **`-profile-dir`** writes `cpu.pprof` and
`heap.pprof`, and **`-trace`** writes a `runtime/trace`. Both are **inert by
default** — with neither flag set no profiler runs and not one byte reaches the
example's output, which is what lets each example's regression test pin its
deterministic output unedited. Flags beyond those two vary per example.

## Getting Started

```go
package main

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

func main() {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	a.AddEdge("Lisbon", "Madrid", 624)
	a.AddEdge("Lisbon", "Paris", 1737)
	a.AddEdge("Madrid", "Paris", 1274)
	a.AddEdge("Madrid", "Rome", 1969)
	a.AddEdge("Paris", "Rome", 1422)

	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup("Lisbon")

	d, err := search.Dijkstra(c, src)
	if err != nil {
		panic(err)
	}
	for _, city := range []string{"Madrid", "Paris", "Rome"} {
		id, _ := a.Mapper().Lookup(city)
		dist, _ := d.Distance(id)
		fmt.Printf("Lisbon -> %s : %d km\n", city, dist)
	}
}
```

## Workflow

The project follows a strict `Specify -> Implement -> Test -> Document`
workflow. Sprint planning lives in the local `rmp` CLI roadmap. The
`Makefile` `ci` target runs the full validation pipeline:

```
make ci
```

The pipeline runs `go mod tidy`, `gofmt`, `go vet`, `go build`, the
short test layer under the race detector (`go test -race`),
`golangci-lint run`, and the coverage gate (`cover-gate`), which
enforces **≥ 85 % aggregate** and **≥ 75 % per-package** statement
coverage. Every change must pass it before being committed.

## Performance

**The authoritative, per-release record for this release is
[docs/benchmarks/v0.12.0.md](docs/benchmarks/v0.12.0.md)** — run environment,
method, what the figures do *not* establish, and reproduce commands.
This section is a summary, not a second source.

`v0.12.0` adds no engine capability, so the throughput and primitive figures
below are unchanged and are quoted with their original provenance: **each was
measured first-hand at the `v0.11.0` commit (`ba436a5b`)** on Apple M4
(10-core), 32 GB, `darwin/arm64`, go1.26.5, on a host gated below a 1-minute
load average of 2.5, and is recorded in
[docs/benchmarks/v0.11.0.md](docs/benchmarks/v0.11.0.md). What `v0.12.0`
changed is set out in
[its own record](docs/benchmarks/v0.12.0.md) and in
[release-notes/v0.12.0.md](release-notes/v0.12.0.md#performance--the-honest-account).

**Durable write throughput, at the concurrency levels the module publishes**
(`store/txn`, one durable single-edge transaction per op, median of 6):

| Writers | 1 | 8 | 64 | 256 | 1024 |
|---|---:|---:|---:|---:|---:|
| Store API | 268 | 1,099 | 8,322 | 32,234 | **111,483 ops/s** |
| Cypher engine | 271 | 1,060 | 8,254 | 30,804 | **115,301 ops/s** |
| commits/fsync | 1.00 | 4.08 | 31.50 | 121.20 | **422.05** |

Throughput is **monotonic in writer count** and scales **415×** (store API) /
**425×** (Cypher) from 1 to 1024 writers. The scaling factor equals
`commits/fsync` at every level, so the gain is group-commit fsync amortisation
and nothing else; the single-writer rate is this device's fsync rate and is
unchanged from `v0.10.0`, so nothing was traded for it. At `v0.10.0` this curve
was flat — the module had no write concurrency.

**Read path under concurrency** — the hot-key intern probe, where every goroutine
contends for one cache line, is **flat at 75–89 ns/op with zero allocations from
8 to 1024 goroutines** (a 128× over-subscription of 10 cores). Note `ns/op` under
`RunParallel` is inverse *aggregate* throughput, not per-goroutine latency.

**Primitive operations** and the guard-band algorithm set (median of 6):

| Operation | Result |
|---|---|
| `Mapper.Intern` (hot key, uncontended) | 8.67 ns/op, 0 B, 0 allocs |
| `search.Dijkstra` (post-warmup, reusable state) | 8.27 ms, **0 B, 0 allocs** |
| `search.BFS` direction-optimising (power law) | 29.87 ms, **0 B, 0 allocs** |
| `search.Yen` k=100 | 14.47 ms, 459 KB, 1,280 allocs |
| `centrality.Brandes` (random graph) | 8.09 ms, 62 KB, 10 allocs |

The two post-warmup traversal paths allocate **zero** bytes per call, and every
allocation count above is identical to `v0.10.0` — the zero-allocation hot-path
mandate holds.

**And the costs, because a README that lists only wins is not a faithful one.**
Graph iteration and bulk write pay **+7–16 %** across seven independent
benchmarks, WAL recovery is **1.51× slower** (the price of deriving the MVCC clock
from the WAL), on-disk bytes per node rise **2.9 %**, the uncontended intern path
is **+14 %**. The `v0.11.0` **open defect** that made a selective multi-label
query ≈21.6× slower in the default configuration (rmp #2431) is **closed in
`v0.12.0`** at **−96.08 % sec/op with no shape regressing**
([docs/benchmarks/min-label-anchor-vs-parallel-scan-2026-08-26.md](docs/benchmarks/min-label-anchor-vs-parallel-scan-2026-08-26.md)).
The cost `v0.12.0` adds in its place is a correctness guard on anonymous
pattern heads, which costs the declined shape **62× to 1705×** — stated in full
in [the release notes](release-notes/v0.12.0.md#what-got-slower--measured-not-minimised),
with rmp #2604 filed to recover the optimisation.

> **Reproduce:** the exact commands are in
> [docs/benchmarks/v0.11.0.md](docs/benchmarks/v0.11.0.md#7-reproduce); the
> general workflow is `make bench BENCH_PATTERN=. BENCH_COUNT=5` and
> [docs/profiling.md](docs/profiling.md).
> Hardware deltas should be reported in CHANGELOG.md alongside
> any number that regresses beyond the local `benchstat` regression
> gate (`scripts/bench_gate.sh`), which is run locally to compare a
> candidate against its baseline before the change lands.

Per-release reports live in [docs/benchmarks/](docs/benchmarks/), one per tag;
per-change tracking is in [docs/benchmarks/history/](docs/benchmarks/history/)
with the narrative ledger at
[history/LEDGER.md](docs/benchmarks/history/LEDGER.md). The end-to-end comparison
of `v0.10.0` against this release — including the regressions — is
[release-delta-v0.10.0-to-head-2026-08-10.md](docs/benchmarks/release-delta-v0.10.0-to-head-2026-08-10.md).

## Module Layout

```
graph/                    — core types: NodeID, Graph[N,W] contract, sharded Mapper
graph/adjlist             — mutable copy-on-write adjacency list, version-chained per slot
graph/csr                 — immutable Compressed Sparse Row snapshot (reader-side)
graph/generation          — refcount-protected Publisher for atomic snapshot rotation
graph/mvcc                — transaction clock, commit records, commit frontier, reclamation
                            horizon/watermark, Gate, ErrSerializationConflict
graph/lpg                 — labelled property graph (labels + typed properties)
graph/lpg/schema          — declarative type schema with Validate
graph/index               — Manager fanning out Change events to subscribers
graph/index/label         — Roaring-bitmap inverted label index
graph/index/hash          — sharded hash exact-match property index
graph/index/btree         — order-preserving range property index
graph/query               — fluent MATCH-style pattern engine
graph/io/csv              — edge-list CSV reader and writer
graph/io/graphml          — GraphML XML reader and writer
graph/io/dot              — Graphviz DOT writer
graph/io/jsonl            — JSON Lines reader and writer

search/                   — traversal and path-finding over CSR (BFS, DFS, Dijkstra,
                            Bellman-Ford, A*, BiBFS, Yen, APSP, BCC, Eulerian, ...)
search/centrality         — Brandes betweenness, PageRank, personalised PageRank
search/community          — Leiden, label propagation
search/extern             — semi-external BFS/PageRank over a Tier 2 reader
search/flow               — Dinic, Edmonds-Karp, push-relabel, Stoer-Wagner, MCMF

store/wal                 — versioned, CRC32C-checksummed Write-Ahead Log
store/snapshot            — atomic snapshot directories with manifest and per-file CRC
store/txn                 — transactions (Begin/Commit/Rollback); writers run CONCURRENTLY,
                            collisions detected by MVCC rather than prevented
store/checkpoint          — background WAL → snapshot folder goroutine
store/recovery            — snapshot + WAL replay on open
store/csrfile             — mmap'd Tier 2 CSR file format (versioned, 64-byte aligned)
store/bulk                — high-throughput bulk ingestion bypassing the WAL (adjacency only)
store/bulkimport          — offline import: labelled property graph → published store snapshot

cypher/                   — openCypher parser, planner and execution engine; snapshot-isolated
                            read transactions, lock-free write transactions, Session (RYOW)
cypher/parser · ast · sema · ir · plan · exec
                          — parser-to-execution pipeline: plan cache, physical-plan EXPLAIN,
                            PROFILE with per-operator rows/time/dbhits, write counters
cypher/funcs · procs      — built-in functions and procedures
cypher/tck                — openCypher TCK harness (execution 100 %, 3 897/3 897)

bolt/proto · packstream   — Bolt v5 protocol and PackStream encoding
bolt/server               — TCP server for neo4j-go-driver v5 / cypher-shell; TLS hot-reload,
                            per-connection Session, operable + bounded transactions

ds/                       — supporting data structures (Union-Find, ...)
metrics/                  — public observability seam (SetBackend, counters, gauges, latency)

cmd/gograph-import        — offline CSV → store importer (see store/bulkimport)

bench/ldbc                — LDBC SNB SF1 / SF10 benchmark harness
bench/dimacs9             — DIMACS 9 USA-road SSSP benchmark
bench/rmat                — RMAT power-law graph generator
bench/soak                — 4-hour mixed-workload reliability soak harness
bench/comparison          — head-to-head harnesses: three-way vs Neo4j and Memgraph over Bolt
                            (throughput, CPU via cgroup counters, memory, concurrency), plus
                            NetworkX and SuiteSparse:GraphBLAS/LAGraph baselines

internal/metrics          — observability implementation behind the public metrics/ facade;
                            external consumers import metrics/, not this
internal/stress           — concurrency stress test suite (CI under -race)
internal/shapegen         — graph shape generators (trivial, classic, random models, adversarial)
internal/invariants       — graph invariant checkers (connected, DAG, bipartite, distance bound)
internal/testfs           — FS fault-injection wrapper (ENOSPC, partial write, fsync delay)
internal/crashinject      — subprocess crash-injection harness (SIGKILL breakpoints)
internal/subproc          — cross-process test helper (re-exec, mode dispatch)
internal/goldens          — golden-file assertion helper with -update and atomic write

See [docs/test-battery.md](docs/test-battery.md) for the production-readiness
test battery guide and the add-new-shape recipe.

examples/                 — 37 runnable example programs, each pprof-able (see "Examples")
```

## Labelled Property Graph + Query Example

```go
g := lpg.New[string, int64](adjlist.Config{Directed: true})
g.SetNodeLabel("alice", "Person")
g.SetNodeLabel("alice", "Admin")
g.SetNodeProperty("alice", "age", lpg.Int64Value(30))
g.AddEdge("alice", "bob", 1)

c := csr.BuildFromAdjList(g.AdjList())
e := query.New(g, c)

for _, n := range e.Match().Vertex(
    query.WithLabel[string, int64]("Admin"),
    query.WithProperty[string, int64]("age", lpg.Int64Value(30)),
).Collect() {
    fmt.Println(n)
}
```

## Security

Vulnerability reports follow the process documented in
[SECURITY.md](SECURITY.md). **Report privately through GitHub Security
Advisories** —
<https://github.com/FlavioCFOliveira/GoGraph/security/advisories/new>. Please do
not open a public issue for a suspected vulnerability; if you cannot use
Security Advisories, open an issue containing **no vulnerability details**, only
a request for a maintainer to open a private advisory. SECURITY.md states the
response targets (48 h acknowledgement, 5 business days to triage, 30 days to a
fix under embargo, 90-day coordinated disclosure) and the scope.

## License

GoGraph is distributed under the [MIT License](LICENSE).
