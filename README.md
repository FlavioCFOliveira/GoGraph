# GoGraph

A Go module for graph persistence, manipulation, and fast search,
designed to scale from in-memory graphs to graphs that exceed RAM.

## Status

**Current release: `v0.14.0`.** This is the project's **seventeenth
release**, published at a pre-1.0 baseline: under Semantic Versioning a
`0.y.z` version signals that the public API is **not yet stable** and may
change without a major bump while the module matures toward `1.0.0`.
`v0.14.0` is a pre-1.0 **MINOR** release of **13 commits** from **one
sprint (355, *GoGraph execution reporting*) and 11 closed tasks**. No
change is marked breaking, and `go.mod` and `go.sum` are
**byte-identical** to `v0.13.0` — same pinned toolchain, same dependency
set — so nothing in the supply chain moved. It is an **execution-reporting
release**: it changes what `EXPLAIN` and `PROFILE` tell you, and it makes
the planner act on a measurement for the first time.

**A db-hits figure nobody counted no longer prints as `0`.** The column
became tri-state — a counted figure, a counted zero, and `?` for a figure
that was never counted — and an incomplete total renders `x + ?` instead
of silently summing across the gaps. Four operator families that read
storage and reported nothing now count it: `Expand` reports the adjacency
slots it walked rather than the edges it emitted (a type-filtered hop had
under-reported by 100×); the three morsel-parallel leaves report their
workers' node walk (they reported `0` for a 2 000-node scan); and
`ShortestPath` and `AllShortestPaths` report the relationship slots their
searches read. A new `Removed` column reports how many rows a predicate
rejected, omitted entirely for an operator that has no rejection
mechanism.

**The planner consumes its own statistics for the first time.** The
property statistics have been built since #2097 and, until this release,
were read by nothing but the `EXPLAIN` renderer. They now drive the
disjoint-component join reorder, under the pre-existing trustworthiness
veto and over a certified error interval. `Est.Rows` renders beside the
measured `Rows` with its provenance marked, so an estimate and the
measurement that tested it can be read in one table; and how wrong the
estimates turn out to be is itself observable, as a q-error metric with
`Engine.StatsMisestimatedPairs` reporting how many `(label, property)`
pairs are currently behind it.

**Two defects were found by the sprint's own gate, and fixed.** A declined
MVCC label count was read as an empty label, which made the planner
benefit non-deterministic and let `EXPLAIN` render a plan the engine did
not run; and a labelled count cloned a roaring bitmap and returned it
uncorrected, purely to read its cardinality.

**This release adds counting work to several hot paths** — slot bracketing
in `Expand`, one atomic add per morsel in the parallel leaves, slot
accumulation in the shortest-path searches, and reject counting in the
filters. **The aggregate cost has since been measured** in a three-arm
interleaved campaign against the `v0.13.0` tree
([docs/benchmarks/v0.14.0.md](docs/benchmarks/v0.14.0.md)), and **the added
work costs nothing that campaign can resolve**: of 272 comparable result
rows, nine deltas survive adjudication against a same-code noise floor —
six improvements and three regressions, the largest of them **+7.11 %** on
one 11 µs benchmark. The Cypher read path, which the counting was added to,
is **unchanged at all five published concurrency levels**. One gain is
large and **conditional**: with `(label, property)` statistics populated,
the join reorder takes a skewed two-component shape from **267.155 ms to
2.723 ms (−98.98 %)** — a figure that must never be read as a general
speed-up. See [Performance](#performance).

The two compliance invariants remain in force: the module is **100 %
openCypher TCK-compliant at the execution level** (**3 897/3 897
scenarios**, preserved rather than extended — no `.feature` file changed
this cycle) and **100 % ACID-compliant**. `make ci` is green on the
release tree: race-clean, `golangci-lint` 0 issues, aggregate library
coverage 88.6 %. **The soak and nightly layers were not run for this
release**, and it carries **no production certification of its own** — the
most recent was taken at the `v0.11.0` commit, and the whole-tree soak
layer has now gone unrun for a seventh consecutive cycle. One openCypher
divergence also ships open and the TCK is structurally blind to it (rmp
#2675: a subquery body's final projection is never translated, and zero of
220 feature files contain `COUNT {`). The module uses the conventional Go
path `github.com/FlavioCFOliveira/GoGraph` and is fetchable with
`go get github.com/FlavioCFOliveira/GoGraph@v0.14.0`. See
[CHANGELOG.md](CHANGELOG.md) and
[release-notes/v0.14.0.md](release-notes/v0.14.0.md) for the full release
narrative, the behaviour changes a caller must know about, and what the
release does **not** establish.

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
  `cypher/ir` · `cypher/exec` — parser-to-execution
  pipeline with plan-cache, `EXPLAIN` of the **physical** plan, `PROFILE` with
  per-operator rows, time, db-hits, rows-removed-by-filter and the planner's
  estimate beside the measurement — an uncounted db-hits figure renders `?`,
  never `0` — and per-statement write-effect counters (`Result.Counters`).
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
[docs/benchmarks/v0.14.0.md](docs/benchmarks/v0.14.0.md)** — run environment,
method, the two noise floors, what the figures do *not* establish, and
reproduce commands. This section is a summary, not a second source.

**`v0.14.0` was measured against `v0.13.0` first-hand, on three arms.** `A` is a
worktree of tag `v0.13.0` (`b439283e`), `B` is the release tree (`f8a3ac0b`),
and **`B2` is a second, independent build of that same `v0.14.0` tree** —
byte-identical to `B` by sha256 on all twelve packages, so `B` vs `B2` is one
binary measured against itself in the same rounds, at the same concurrency
levels, under the same load as the signal it calibrates. All three arms were
compiled once each by the same `go1.27.1`, then run **interleaved with the arm
order rotated every round**, `-race` off, `-count=6`, load-gated per round:
**252 invocations, 252 exiting 0**, across 189.8 minutes on 2026-09-06.
`go.mod` and `go.sum` are byte-identical between the two trees, so no toolchain
or dependency change is mixed into any comparison.

**The noise floor is scale-dependent, and that governs every verdict below.**
Above 1 ms the cross-arm same-code floor is **0.17 % at the median and 0.87 % at
the maximum**; below 100 ns it reaches **37.80 %**, and a **statistically
significant −6.21 %** was produced by a **byte-identical binary**. A delta is
therefore reported as a finding only when it is significant **and** exceeds its
own magnitude band's same-code envelope — 6.21 % under 100 ns, 4.28 % to 10 µs,
2.89 % to 1 ms, 2.62 % above. **27 rows reached `p<0.05`; nine are findings and
eighteen are rejected as noise**, four of them in `store/wal`, whose test binary
is byte-identical between the releases and therefore cannot have changed.
`allocs/op` behaves in the opposite way: it is **exact across the arms — 0.00 %
at the median, the p95 *and* the maximum** — so every allocation delta below is
real.

**Of 272 comparable result rows, nine survive: six improvements and three
regressions.** The largest regression is **+7.11 %**, on one 11 µs benchmark.
`cypher/exec`, which carries most of sprint 355's 4 478 changed non-test lines,
was swept completely (36 benchmark functions, 62 rows) and has an **effect
geomean of +0.50 % against its own same-binary floor geomean of −0.84 %**: the
package did not move.

**What a consumer actually feels: the Cypher read path is unchanged, and that is
the result.** This is the path the release added db-hit and rows-removed
counting to, so it is the one that matters most.

| Goroutines | 1 | 8 | 64 | 256 | 1024 |
|---|---:|---:|---:|---:|---:|
| `ReadTx_LockFree` `v0.13.0` → `v0.14.0` | 3.102 → 3.058 µs | 1.696 → 1.677 µs | 2.252 → 2.152 µs | 2.910 → 2.632 µs | 5.250 → 4.529 µs |
| `ReadTx_WriterLock` `v0.13.0` → `v0.14.0` | 104.9 → 105.3 µs | 24.73 → 24.84 µs | 24.00 → 24.22 µs | 25.32 → 25.89 µs | 29.91 → 30.94 µs |

**Not one cell is significant** (geomean **−2.43 %** against a block floor of
−0.71 %), so the **−73 % geomean `v0.13.0` won on this path is held, not
eroded**, by a release that added counting work to it. The absolute figures
reproduce `v0.13.0`'s published head arm to within a few percent at **37 and
114 `allocs/op` on both arms** — two independent campaigns, a day apart,
agreeing. Note `ns/op` under `RunParallel` is inverse *aggregate* throughput,
not per-goroutine latency, and the 1024 cells are the weakest data in the
campaign: an identical binary differed by 38 % at 8 goroutines elsewhere in the
same session.

**The release's one large gain — the join reorder — is conditional and must
never be quoted as a general speed-up.** With `(label, property)` statistics
populated, the planner now picks the cheaper driver on a disjoint two-component
match where a label count alone picks the wrong one:

| Benchmark | `v0.13.0` | `v0.14.0` | Δ `sec/op` | speed-up |
|---|---:|---:|---:|---:|
| `ZZReorderSkewed/reorder=on` | 267.155 ms | **2.723 ms** | **−98.98 %** | **98.1×** |
| `ZZReorderLiveHistory/history=quiet` | 27.685 ms | **976.6 µs** | −96.47 % | 28.3× |
| `ZZReorderLiveHistory/history=live` | 27.695 ms | **987.3 µs** | −96.43 % | 28.0× |
| `ZZReorderSkewed/reorder=off` — **control** | 267.4 ms | 268.0 ms | `~` (p=0.589) | — |

All three resource vectors move together — `ZZReorderSkewed/reorder=on` also
falls **47 226.6 KiB → 553.4 KiB** (−98.83 %) and **5 925 706 → 59 968
`allocs/op`** (−98.99 %) — the plan changes shape between the trees, the two
controls are flat, and the result set hashes identically on both arms. **It
fires only** when `RefreshStatistics` has run, the query is a qualifying
disjoint two-component shape, and the trustworthiness veto does not fire.
**A workload of ordinary queries sees none of this 98 %**; it sees what
`reorder=off` measures, which is unchanged.

**The controls confirm the harness rather than the code.** `search/`,
`search/centrality/` and `store/txn`'s commit path have **no changed source file**
in this release, and all seven of their benchmarks are non-significant
(`search` geomean **−0.03 %**, `store/txn` **−0.06 %**) — which is what makes the
−98.98 % above code and not session drift.

| Operation | `v0.14.0` | vs `v0.13.0` |
|---|---|---|
| `search.Dijkstra` (post-warmup, reusable state) | 8.193 ms, **0 B, 0 allocs** | `~` (p=0.485) |
| `search.Dijkstra` (large) | 8.210 ms, 1.13 MB, 4 allocs | `~` (p=0.818) |
| `search.BFS` direction-optimising (power law) | 28.90 ms, **0 B, 0 allocs** | `~` (p=0.485) |
| `search.Yen` k=100 | 14.71 ms, 459 KB, 1,280 allocs | `~` (p=0.093) |
| `centrality.Brandes` (random graph) | 7.897 ms, 62 KB, 10 allocs | `~` (p=0.589) |
| `Mapper.Intern` (hot key, uncontended) | 8.67 ns/op, 0 B, 0 allocs | carried forward from `v0.11.0` |

**The zero-allocation hot-path mandate holds, verified rather than asserted:**
`allocs/op` is byte-identical on all five search benchmarks with **every sample
equal** (0, 4, 0, 1 280, 10); the `B/op` figures are carried forward from the
`v0.13.0` campaign, since no file under `search/` changed. `Yen_K100` is worth
naming — it was `v0.13.0`'s smallest reported regression (+2.42 %) and it did
**not** regress further here.

**Durable write throughput, at the concurrency levels the module publishes**
(`store/txn` `BenchmarkCommitConcurrent`, a hand-rolled `go func` fan-out behind
a `WaitGroup`, median of 6, re-measured at both trees):

| Writers | 1 | 8 | 64 | 256 | 1024 |
|---|---:|---:|---:|---:|---:|
| `v0.13.0` | 3.687 ms | 885.4 µs | 120.2 µs | 30.64 µs | *not measured* |
| `v0.14.0` | 3.672 ms | 920.4 µs | 119.9 µs | 30.82 µs | *not measured* |
| scaling vs own level 1 (`v0.14.0`) | 1.00× | 3.99× | 30.62× | **119.14×** | — |

**The durable commit path scales 119–120× from 1 to 256 concurrent writers,
identically on both arms**; no cell is significant (geomean +0.95 % against a
floor of −0.30 %). The single-writer rate is this device's fsync rate, and the
gain is group-commit fsync amortisation. **This instrument stops at 256**, so
the published 1024 level is measured for reads and metrics but **not for durable
commits**; the last figure there remains `v0.11.0`'s **111,483 ops/s** (store
API) / **115,301 ops/s** (Cypher engine) at 422.05 commits per fsync, carried
forward and not re-measured since.

**Concurrency scaling elsewhere.** `graph`, `graph/mvcc` and
`internal/metrics/prometheus` compile to **byte-identical binaries** between the
releases, so their full five-level ladders are floors measured at every level —
including 1024. `graph/mvcc` shows **no significant cell** across twenty
(geomean −0.07 %), and its lock-free read gate `Gate_WeakParallel` scales
**8.0×** from 1 to 1024 goroutines, agreeing to within 2 % with the 8.19×/8.12×
`v0.13.0` measured in a different session. The hot-key intern probe
(`Mapper_Intern_HotKey_Parallel`) has no significant cell either, the arms
agreeing to **0.85 % at level 1 and 0.03–0.35 % above it**.

**And the costs, because a README that lists only wins is not a faithful one.**
Three regressions survive adjudication, and **all three have byte-identical
`B/op` and `allocs/op` with every sample equal** — more work per operation, not
more garbage:

| Benchmark | `v0.13.0` → `v0.14.0` | Δ | p |
|---|---:|---:|---:|
| `cypher/exec` `AllNodesScan_PerNodeAllocCost` | 10.97 → 11.75 µs | **+7.11 %** | 0.002 |
| `cypher/exec` `ExpandDir_InVsOut_Baseline/OUT_deg1_sources` | 142.2 → 147.1 µs | **+3.46 %** | 0.009 |
| `cypher/exec` `RelTypeProbe_Column` | 1.490 → 1.763 ns | **+18.32 %** | 0.002 |

The first is the most coherent: the benchmark exists to price the per-node cost
of the all-nodes scan, and this release added per-node db-hits counting to
exactly that path — but **attribution is a hypothesis, not established**, since
no third arm with the counting disabled was built. The third is a **0.27
nanosecond** difference in the band where a byte-identical binary produced a
significant −6.21 %, and is the weakest of the three. The two surviving
improvements outside the reorder are
`RelTypeProbe_ReverseRecovery/map+recovery` (**−25.87 %**, 7.046 → 5.224 ns) and
`PlanReusePhasesParallel/4full` (**−4.90 %** at 8 goroutines, **−8.50 %** at 64).

**Allocations fell and nothing regressed one.** Beyond the reorder rows, the
shortest-path operators that #2763 *added* per-run slot accumulation to allocate
**less**: `ShortestPath_Layered` and `_LayeredTyped` both **126 → 108
(−14.29 %)**, `_HighDegreeFan` 317 → 314. That reduction is measured, not
explained. Nothing else in the campaign moved an allocation count.

**Footprint** closes a gap `v0.13.0` named as unmeasured. The `graph/index/count`
test binary is byte-identical across all three arms, and `BenchmarkStoreFootprint`
reports **123 712 `B/store` on every one of 18 samples, ±0.00 %**. **On-disk
footprint remains unmeasured** — no benchmark reporting bytes written to disk
exists in both trees.

**What this release does not establish.** **Latency percentiles at the published
concurrency levels are unmeasured for a seventh consecutive cycle** — the
figures above are per-op cost and aggregate throughput, not a distribution.
**1024 concurrent writers on the durable path** is unmeasured. **43 of the 57
benchmarks that are genuinely concurrent in both trees were not exercised**,
including the whole `bolt/server` `TxBookkeeping_*` family (on a release that
changed `bolt/server/plan_meta.go` by 103 lines) and `bench/mtaudit`'s
engine-under-writer ladder, which remains **the single highest-value measurement
not made, now for two consecutive releases**. `cypher` is **sampled, not
covered** — 62 of its 144 benchmark functions, 43 %. The soak and nightly layers
were not run, no `-race` figures and no profiles were captured, attribution to
individual commits is established only for the reorder, and everything here is
**one host, one architecture**: Apple M4, 10 cores, `darwin/arm64`. The floors
above are properties of that host and the adjudication bars derived from them do
not transfer.

> **Reproduce:** the exact commands, including the three-arm interleaved
> harness, the load gate and the noise-floor procedure, are in
> [docs/benchmarks/v0.14.0.md](docs/benchmarks/v0.14.0.md) §9 and were used
> verbatim from [docs/benchmarks/v0.14.0-raw/](docs/benchmarks/v0.14.0-raw/);
> the general workflow is `make bench BENCH_PATTERN=. BENCH_COUNT=5` and
> [docs/profiling.md](docs/profiling.md).
> Hardware deltas should be reported in CHANGELOG.md alongside
> any number that regresses beyond the local `benchstat` regression
> gate (`scripts/bench_gate.sh`), which is run locally to compare a
> candidate against its baseline before the change lands.

Per-release reports live in [docs/benchmarks/](docs/benchmarks/), one per tag;
per-change tracking is in [docs/benchmarks/history/](docs/benchmarks/history/)
with the narrative ledger at
[history/LEDGER.md](docs/benchmarks/history/LEDGER.md). The allocation-only
comparison of these same two trees, taken on a host that was **not** quiet and
which therefore makes no timing claim, is
[v0130-vs-v0140-2026-09-06.md](docs/benchmarks/v0130-vs-v0140-2026-09-06.md).
The end-to-end comparison of `v0.10.0` against the tree as it stood on
2026-08-10 — five sprints before the `v0.11.0` tag, and including the
regressions — is
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
cypher/parser · ast · sema · ir · exec
                          — parser-to-execution pipeline: plan cache, physical-plan EXPLAIN,
                            PROFILE with per-operator rows/time/db-hits/removed/estimate,
                            write counters
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
