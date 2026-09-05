# GoGraph

A Go module for graph persistence, manipulation, and fast search,
designed to scale from in-memory graphs to graphs that exceed RAM.

## Status

**Current release: `v0.13.0`.** This is the project's **sixteenth
release**, published at a pre-1.0 baseline: under Semantic Versioning a
`0.y.z` version signals that the public API is **not yet stable** and may
change without a major bump while the module matures toward `1.0.0`.
`v0.13.0` is a pre-1.0 **MINOR** release of **100 commits** across **two
sprints (352 and 353) and 86 closed tasks**. Where `v0.12.0` broke nothing,
this one **carries seven breaking changes** — and every one of them turns a
previously *silent* failure into a loud one. `go.mod` and
`go.sum` are **byte-identical** to `v0.12.0` — same pinned toolchain, same
dependency set — so nothing in the supply chain moved. It is a
**performance and correctness-hardening release**.

Three things define it. **It is the module's two deep-profiling and
optimisation cycles** — sprint 352 audited the module for bottlenecks and
then worked the Cypher read path; sprint 353 built a committed contention
observatory and worked the index, `graph/lpg`, `metrics` and Bolt surfaces
under concurrency. **27 of the 100 commits are `perf`**, against 4 in the
whole of `v0.12.0`.

**Contention stopped being inferred and started being measured.** Before
this release **nothing in the repository enabled Go's contention
profilers**, so every prior contention claim rested on the shape of a
throughput curve and never on lock-site attribution. `bench/contention` is
the instrument that closes that gap, and it is committed. Four workloads
that **anti-scaled** — throughput falling as goroutines rise while cores sit
idle — no longer do: the btree index goes **34.6×** at 1024 goroutines with
mutex delay falling from 1.99 hours to 120 s, a mixed read/write Cypher
workload goes **41.1×**, the count store's write path **+468 %**, and
contended metric emission **−96.4 %**. Every one of those figures is that
change's own interleaved A/B against a noise floor measured first.

**A great many published claims were refuted by their own measurement, and
are recorded as refuted.** The write barrier ranked at 72–99 % of write-path
blocked time holds **zero** of it — a cumulative share is not a bottleneck.
The Bolt transport suspected of throttling the wire is not the limiter, and
the published ratios *understate* a real socket. A ceiling table published
as "every arm is a lower bound" had been **filtered**, and three of the
seven omitted arms sit above 1.00. Two silent wrong answers were each
uncovered by removing the cost that hid them: `PROFILE` rendered a different
plan from the one that ran and reported `rows=0` with no error, and reverse
traversal under a correlated `Apply` returned rows for nodes with no
matching edge — accidentally correct in its typed spelling only, because the
per-slot type test was acting as an unintended validity filter.

Alongside them: **eight engine-side ACID fixes**, including three
life-record families where a rolled-back `DELETE` hid a committed node from
older readers and a *refused* removal resurrected a node or an arc on
rollback; two durable formats that now refuse a field they would previously
have truncated or written unreadably, one of which was discarding the WAL
behind a snapshot no reader could parse; a panic in the commit window that
wedged every later committer for ever; and **the vulnerability gate that
never existed** — `govulncheck` appeared in no Makefile target, no `make ci`
path and no script anywhere in the repository, so the check was never
automated and could not fail loudly because it never ran.

Cypher gains `EXPLAIN` and `PROFILE` as **statement prefixes**, so a Bolt
driver can ask for a plan at all; driver-compat rises 28 → 30 of 37.

The two compliance invariants remain in force: the module is **100 %
openCypher TCK-compliant at the execution level** (**3 897/3 897 scenarios**,
16 006/16 006 steps, preserved rather than extended — no `.feature` file
changed this cycle, and the count was re-verified across a grammar change)
and **100 % ACID-compliant**. Every change is gated by the project's local
validation pipeline, run via `make ci` / `make release-preflight` before it
lands. **This release carries no production certification of its own** — the
most recent one was taken at the `v0.11.0` commit, 291 commits behind this
tag, and the whole-tree soak layer has now gone unrun for six consecutive
cycles. One openCypher divergence also ships open and the TCK is
structurally blind to it (rmp #2675: a subquery body's final projection is
never translated, and zero of 220 feature files contain `COUNT {`). The
module uses the conventional Go path
`github.com/FlavioCFOliveira/GoGraph` and is fetchable with
`go get github.com/FlavioCFOliveira/GoGraph@v0.13.0`. See
[CHANGELOG.md](CHANGELOG.md),
[release-notes/v0.13.0.md](release-notes/v0.13.0.md) and
[docs/benchmarks/v0.13.0.md](docs/benchmarks/v0.13.0.md) for the full release
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
[docs/benchmarks/v0.13.0.md](docs/benchmarks/v0.13.0.md)** — run environment,
method, what the figures do *not* establish, and reproduce commands.
This section is a summary, not a second source.

Unlike `v0.12.0`, **`v0.13.0` moved the figures**, and the deltas below were
measured **first-hand at both trees** rather than carried forward: a `v0.12.0`
worktree and this release candidate, compiled once each, run **interleaved**
one repetition at a time with the leading arm alternating, `-race` off,
`-count=6`, on a host gated below a 1-minute load average of 2.5. The two
trees pin an **identical `go.mod` and `go.sum`**, so — for the first time since
`v0.11.0` — no toolchain or dependency change is mixed into any comparison.

The **noise floor was measured before anything was attributed**: both arms
pointed at the same tree, 14 comparisons, **0 significant**, largest median
drift **0.87 % on sec/op and 0.00 % on allocations**. Every figure below clears
it by an order of magnitude. Nothing here is compared against the *published*
`v0.12.0` numbers, which were taken on a different toolchain patch.

**Durable write throughput, at the concurrency levels the module publishes**
(`store/txn`, one durable single-edge transaction per op, median of 6).
**Carried forward from `v0.11.0` and not re-measured this cycle** — the
`store/txn` write ladder was run as an A/B in this window and moved only in
allocated bytes (+2.50 % to +3.37 % B/op at 1/8/64 writers), so the ops/s
figures below are quoted with their original provenance:

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

**Read path under concurrency.** The engine's read-transaction path was
measured as a ladder at both trees and is where this release moved most:
`ReadTx_LockFree` is **−91.40 %** at 1024 goroutines and `ReadTx_WriterLock`
**−52.56 %**, geomean **−73.04 %** across both benchmarks and all five levels.
The spread widens to ±32–44 % at 1024, so treat the median there as indicative
rather than precise. The hot-key intern probe, where every goroutine contends
for one cache line, remains **flat at 75–89 ns/op with zero allocations from
8 to 1024 goroutines** (a 128× over-subscription of 10 cores) — carried forward
from `v0.11.0`. Note `ns/op` under `RunParallel` is inverse *aggregate*
throughput, not per-goroutine latency.

**Primitive operations** and the guard-band algorithm set — **measured at this
release candidate**, median of 6, `-race` off:

| Operation | Result | vs `v0.12.0` |
|---|---|---|
| `search.Dijkstra` (post-warmup, reusable state) | 8.04 ms, **0 B, 0 allocs** | unchanged |
| `search.Dijkstra` (large) | 8.21 ms, 1.13 MB, 4 allocs | unchanged |
| `search.BFS` direction-optimising (power law) | 28.28 ms, **0 B, 0 allocs** | unchanged |
| `search.Yen` k=100 | 14.85 ms, 459 KB, 1,280 allocs | **+2.42 %** |
| `centrality.Brandes` (random graph) | 7.82 ms, 62 KB, 10 allocs | unchanged |
| `Mapper.Intern` (hot key, uncontended) | 8.67 ns/op, 0 B, 0 allocs | not re-measured |

This set doubles as the release's **control**: `search` changed by 78 lines in
this window, so it should not move, and it does not — four of the five are
statistically indistinguishable and the geomean is **+0.83 %**, inside the
noise. A large delta here would have been evidence that the *measurement* was
wrong, not that traversal had improved. `Yen`'s +2.42 % is real but small and
its cause was not investigated.

The two post-warmup traversal paths allocate **zero** bytes per call, and every
allocation count above is **byte-identical** to the `v0.12.0` arm measured
alongside it — the zero-allocation hot-path mandate holds, verified rather than
asserted.

**What `v0.13.0` changed, measured at both trees** (geomean per block,
p=0.002 unless stated, `-race` off):

| Block | Delta | Largest single move |
|---|---:|---|
| Cypher count pushdown | **−87.65 %** | `Count_LabelStarBig_Serial` −99.97 % (≈3 ms → 880 ns) |
| Read transaction under concurrency | **−73.04 %** | `ReadTx_LockFree` −91.40 % at 1024 goroutines |
| `cypher/exec` read-path operators | **−18.93 %** | `Scan_PerNode` −69.97 %; `Sort_10k` −43.59 % |
| Contended metric emission | **−92.79 %** | `IncCounterParallel` −96.47 % at 1024 |
| Columnar query shapes | **−4.28 %** | five hop shapes −7.69 % to −9.42 % |

The read-transaction ladder is the one that changes character rather than
degree: at 1024 goroutines the spread widens to ±32–44 %, so read the median
with that in mind. Contended metric emission changes **sign** — `v0.12.0`
anti-scaled, with cost rising 5.8× from 1 to 1024 goroutines; `v0.13.0` scales,
with cost falling 5.3×.

**Allocations moved as much as time did, on two of the four blocks.** The
read-transaction ladder falls **−76.29 % allocs/op** geomean (`ReadTx_LockFree`
249 → **37**, `ReadTx_WriterLock` 306 → **114**) and the count block
**−90.12 %** (`Count_LabelStarBig_Serial` 199,796 → **19**). Those counts are
now **constant across the whole 1 → 1024 ladder**, where `v0.12.0`'s drifted
with concurrency — a structural change, not a tuning one. The `cypher/exec`
operator block and the columnar shapes are by contrast **flat** (−0.12 % and
−0.19 %), so their time wins are CPU-side. `-race` is off throughout;
`allocs/op` is not build-invariant.

**And the costs, because a README that lists only wins is not a faithful one.**
Four regressions were measured in this release and none is omitted:
`graph/index/btree`'s `Index_LookupHot` is **+26.93 %** (geomean, every
GOMAXPROCS level, p=0.002) where the commit responsible reports its own
uncontended cost as 6.66 %; `cypher/exec`'s `Drain_Throughput` is **+12.87 %**;
`IncCounterParallel` pays **+8.32 %** at a single goroutine for its contended
win; and `CountAllNodes` is **+3.09 %**. Carried forward from `v0.11.0` and not
re-measured here: graph iteration and bulk write **+7–16 %**, WAL recovery
**1.51× slower**, on-disk bytes per node **+2.9 %**.

**Three things this release does not establish.** Sprint 353's headline index
contention claims — 34.6× on the btree spine, 1.91× on the hash index —
**cannot be verified at release level**: no benchmark common to both trees
exercises those indexes concurrently, because the instrument that measures them
(`bench/contention`) is new in `v0.13.0`. They rest on each commit's own local
A/B. And storage footprint, latency percentiles at the published concurrency
levels, `bench/mtaudit`'s engine-under-writer ladder, and any non-`darwin/arm64`
host remain unmeasured.

> **Reproduce:** the exact commands, including the interleaved A/B harness and
> the noise-floor procedure, are in
> [docs/benchmarks/v0.13.0.md](docs/benchmarks/v0.13.0.md); the
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
