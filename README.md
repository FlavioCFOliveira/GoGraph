# GoGraph

A Go module for graph persistence, manipulation, and fast search,
designed to scale from in-memory graphs to graphs that exceed RAM.

## Status

**Current release: `v0.14.1`.** This is the project's **eighteenth
release**, published at a pre-1.0 baseline: under Semantic Versioning a
`0.y.z` version signals that the public API is **not yet stable** and may
change without a major bump while the module matures toward `1.0.0`.
`v0.14.1` is a pre-1.0 **PATCH** release of **50 commits** from **one
sprint (357, *clear every open bug and chore, and ship GoGraph v0.14.1*)
and 46 closed tasks**. No change is marked breaking, and `go.mod` and
`go.sum` are **byte-identical** to `v0.14.0` — same pinned toolchain, same
dependency set — so nothing in the supply chain moved. It is a
**correctness release**: 26 of its 50 commits are fixes, and it exists
because `v0.14.0` returns wrong answers on a query that `v0.14.0` itself
made faster.

**Upgrade from `v0.14.0` if you use any index or constraint.** The equality
index seek was blind to writes the statement itself had made, so a `MATCH`
on an indexed property could both **lose rows the transaction had
written** and **return rows that no longer matched** — with no explicit
transaction, on a single autocommit statement (rmp #2814). The fix
declines the index access path for the `(label, property)` coordinates a
transaction has dirtied, turning a wrong answer into a slower correct one.

**Four routes by which a secondary index could disagree with the graph are
closed.** The `CREATE INDEX` backfill read *uncommitted* mutations, so a
seek could return a row that was never committed (#2778); `CREATE
CONSTRAINT` had the same defect (#2792); `FinishBuild`'s replay resolved
against live state, so a concurrent write could fabricate an entry
(#2793); and `rewindConstraintDrop` read live state, where the obvious fix
traded a fabricated entry for a **lost** one (#2799). Because an affected
build can have written fabricated entries to disk, the snapshot manifest
now carries an index-builder epoch and recovery **refuses to hydrate** an
older payload (#2797) — so **a store written by `v0.14.0` or earlier
rebuilds its secondary indexes once, on first open**. That is deliberate,
requires nothing of you, and is what removes the fabricated entries.

**The durability path stopped calling three different failures clean.** The
checkpoint gate verified that the expected snapshot files *existed* and
then discarded the WAL prefix, without ever verifying the snapshot could be
**read** (#2749); the readback that fixed it covered recovery's reader but
not its applier (#2780); and `ReplayWAL` reported corruption inside an
already-durable frame as benign, contradicting its own documented contract
(#2794). Three values that committed durably and then blocked every later
checkpoint — a property between 1 GiB and 4 GiB, a nested property list
that was **silently lost on replay**, and an over-cap edge-handle record
count — are now refused at commit, where the caller can act (#2750,
#2783, #2784).

**The Bolt server's four timeout defaults are now `0`, which means
disabled.** The old defaults armed a read deadline before *every* read of
the message loop, and the reader sits in that read while the loop executes
the client's own statement — so the deadline ran against a **busy server**
rather than an idle client: measured, a 900 ms statement was cut off at
110 ms against a 100 ms `ConnTimeout` (#2806, #2807). PostgreSQL and Neo4j
both ship the equivalent bounds disabled and use TCP keep-alive or NOOP
chunks for liveness, and GoGraph now does the same. The contract is
three-way and explicit — zero disables, a positive value bounds, a
negative one is an error. **This has a security cost, and it is stated
rather than glossed:** an unauthenticated client that completes the
handshake and falls silent before LOGON now holds its connection slot
indefinitely. Set `Options.ConnTimeout` explicitly before exposing the
server to an untrusted network.

**This release was measured against `v0.14.0` first-hand**, three arms interleaved with a
noise floor measured in the same rounds
([docs/benchmarks/v0.14.1.md](docs/benchmarks/v0.14.1.md)). **Of 229 comparable result rows,
11 survive adjudication — eight improvements and three regressions**, the largest regression
**+6.54 %** on one 78 µs benchmark. The **Cypher read path is unchanged at all five published
concurrency levels** and the **durable commit path is unchanged**, still scaling **119×** from
1 to 256 writers. The clearest gain is the `ORDER BY` key hoist, where nine of ten
`BoundedOrder` rows move together (`Top` **−2.75 %**, **−2.56 %**, **−2.10 %**); and
`v0.14.1` **gives back the largest regression `v0.14.0` published**, taking
`AllNodesScan_PerNodeAllocCost` from 11.60 µs back to 10.98 µs (**−5.39 %**). The counting
`v0.14.0` added costs **one allocation** on the count-store leaf, measured. See
[Performance](#performance).

The two compliance invariants remain in force: the module is **100 %
openCypher TCK-compliant at the execution level** (**3 897/3 897
scenarios**, preserved rather than extended — no `.feature` file changed
this cycle) and **100 % ACID-compliant** — and this is the release in
which several of the second one's guarantees stopped being merely
asserted, with named fixes under Isolation (#2814), Consistency (#2778,
#2792, #2793, #2799), Durability (#2749, #2780, #2794, #2530) and
Atomicity (#2750, #2783, #2784). **The soak and nightly layers were not
run for this release**, and it carries **no production certification of
its own** — the most recent was taken at the `v0.11.0` commit, and the
whole-tree soak layer has now gone unrun for an eighth consecutive cycle.
The openCypher divergence `v0.14.0` shipped open (rmp #2675: a subquery
body's final projection was never translated) **is fixed in this
release**, along with #2779 and #2781; the TCK remains structurally blind
to it, since zero of 220 feature files contain `COUNT {`. The module uses
the conventional Go path `github.com/FlavioCFOliveira/GoGraph` and is
fetchable with `go get github.com/FlavioCFOliveira/GoGraph@v0.14.1`. See
[CHANGELOG.md](CHANGELOG.md) and
[release-notes/v0.14.1.md](release-notes/v0.14.1.md) for the full release
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
[docs/benchmarks/v0.14.1.md](docs/benchmarks/v0.14.1.md)** — the arms, the two noise floors,
the adjudication rule, the load conditions, every rejected row, and reproduce commands. This
section is a summary, not a second source.

**`v0.14.1` was measured against `v0.14.0` first-hand, on three arms.** `A` is tag `v0.14.0`
(`7c59a02c`), `B` is the release tree (`efd32fb9`), and **`B2` is a second, independent
compilation of that same `v0.14.1` tree** — byte-identical to `B` by sha256 on all eighteen
packages, so `B` vs `B2` is one binary measured against itself in the same rounds, at the same
concurrency levels, under the same load as the signal it calibrates. All three were compiled
once each by the same `go1.27.1`, then run **interleaved with the arm order rotated every
round**, `-race` off, `n=6`, load-gated per round: **192 invocations, 192 exiting 0**, across
150.9 minutes on 2026-09-08/09. `go.mod` and `go.sum` are byte-identical between the trees.

**The noise floor is scale- and kind-dependent, and that governs every verdict below.** A
byte-identical binary measured against itself drifted **0.49 % at the median and 1.95 % at the
p95** over 243 comparisons — but produced a **statistically significant −26.13 %** on one
1024-goroutine cell. Serial millisecond-scale benchmarks are where this host is trustworthy
(same-code maximum **1.68 %** over 84 comparisons); oversubscribed cells below 10 µs are where
it is not. A delta is a finding only when it is significant **and** exceeds its own
population's same-code envelope. **25 rows reached `p<0.05`; 11 are findings, 5 inconclusive,
9 rejected as noise.** `allocs/op` behaves oppositely — **0.00 % at the median and the p95** —
so every allocation delta below is real.

**What a consumer actually feels: the Cypher read path is unchanged, and that is the result.**

| Goroutines | 1 | 8 | 64 | 256 | 1024 |
|---|---:|---:|---:|---:|---:|
| `ReadTx_LockFree` `v0.14.0` → `v0.14.1` | 3.053 → 3.040 µs | 1.653 → 1.653 µs | 2.149 → 2.236 µs | 2.719 → 2.823 µs | 4.619 → 4.798 µs |
| `ReadTx_WriterLock` `v0.14.0` → `v0.14.1` | 101.0 → 101.6 µs | 24.78 → 24.75 µs | 23.75 → 23.99 µs | 24.84 → 25.10 µs | 28.35 → 29.49 µs |

**Not one cell is significant** (smallest `p` = 0.066), and the block's same-binary floor
geomean of **−4.57 %** is larger than its effect geomean of +1.78 % — the floor exceeds the
signal. The 64/256/1024 cells carry a same-code drift of up to −26.13 % in this very
campaign, so the apparent +3.8 % there is **not** reported as a result. Note `ns/op` under
`RunParallel` is inverse *aggregate* throughput, not per-goroutine latency.

**The `ORDER BY` key hoist is the release's clearest gain, and the whole family moves
together.** #2662 hoists a non-projected sort key into its own hidden column so the projection
stops materialising the whole node per row:

| Benchmark | `v0.14.0` | `v0.14.1` | Δ | p |
|---|---:|---:|---:|---:|
| `BoundedOrder/n=000110/Top` | 1.442 ms | **1.402 ms** | **−2.75 %** | 0.020 |
| `BoundedOrder/n=000010/Top` | 1.410 ms | **1.374 ms** | **−2.56 %** | 0.005 |
| `BoundedOrder/n=010010/Top` | 4.858 ms | **4.756 ms** | **−2.10 %** | 0.005 |
| `BoundedOrder/n=000110/Sort` | 8.487 ms | **8.373 ms** | **−1.34 %** | 0.045 |

Nine of the ten `BoundedOrder` rows move the same way; the four above clear their band's
1.28 % bar. A coherent family is stronger evidence than any single row.

**`v0.14.1` gives back the largest regression `v0.14.0` published.**
`AllNodesScan_PerNodeAllocCost` — named in `v0.14.0`'s own report as **that release's biggest
cost at +7.11 %**, when per-node db-hits counting was added to exactly this path — measures
**11.60 µs → 10.98 µs (−5.39 %, `p=0.005`)**. Across six rounds `B` and `B2` agree to 0.4 %
while `A` sits 5.4 % above both, every time. Separately,
`ExpandDir_InVsOut_Baseline/OUT_deg1_sources`, `v0.14.0`'s **+3.46 %** regression, is back to
parity at **+0.10 %**.

**And the costs, because a README that lists only wins is not a faithful one.**

| Benchmark | `v0.14.0` → `v0.14.1` | Δ | p |
|---|---:|---:|---:|
| `cypher/exec` `ExpandOut_PerEdge_SingleSource/K=4096` | 78.10 → 83.21 µs | **+6.54 %** | 0.005 |
| `cypher/exec` `ExpandDir_InVsOut_Baseline/IN_deg1_sources` | 93.27 → 98.69 µs | **+5.81 %** | 0.005 |
| `cypher/exec` `ExpandOut_PerEdge_SingleSource/K=65536` | 1.271 → 1.327 ms | **+4.37 %** | 0.005 |
| `bolt/server` `MsgObserve_RealBackend` | 68.17 → 69.63 ns | **+2.13 %** | 0.031 |

**They are not systemic.** Eleven of the fourteen `Expand*` rows are flat, including all four
`ExpandIn_PerEdge_*` (−0.53 % to +0.42 %) and all four `ExpandIn_TypeFiltered_*` (−0.57 % to
+0.30 %). The cost is confined to the OUT single-source per-edge path and the IN deg1-sources
baseline. `cypher/exec/expand.go` changed in this window, which is the obvious candidate —
but **attribution is a hypothesis, not established**: no arm was built with the destination-
label admission disabled.

**Allocations moved, and every figure here is real because the floor is exactly zero.**
`CountAllNodes` goes **27 → 28 `allocs/op`** and **3 296 → 3 488 `B/op`** with *every sample
equal in both arms* — #2777's "one integer add per `Init`" showing up exactly where it should
— and the count-pushdown paths take **+16 `B/op`**. `ReadTx_WriterLock` takes **+2
`allocs/op` and +352 `B/op`** identically at all five concurrency levels, plausibly #2814's
plan footprint. **No allocation count fell, and none rose by more than two.**

**Durable write throughput, at the concurrency levels the module publishes**
(`store/txn` `BenchmarkCommitConcurrent`, median of 6, re-measured at both trees):

| Writers | 1 | 8 | 64 | 256 | 1024 |
|---|---:|---:|---:|---:|---:|
| `v0.14.0` | 3.732 ms | 866.4 µs | 121.5 µs | 31.31 µs | *not measured* |
| `v0.14.1` | 3.713 ms | 897.2 µs | 120.3 µs | 31.22 µs | *not measured* |
| scaling vs own level 1 (`v0.14.1`) | 1.00× | 4.14× | 30.87× | **118.93×** | — |

**The durable commit path scales 119× from 1 to 256 concurrent writers and is unchanged.**
Three of four cells are non-significant; `goroutines=8` at +3.56 % is reported
**inconclusive** rather than as a result. **This instrument stops at 256**, so the published
1024 level remains unmeasured for durable commits.

**The controls confirm the harness rather than the code.** No file under `search/` changed in
this window, and all five headline benchmarks are non-significant:

| Operation | `v0.14.1` | vs `v0.14.0` |
|---|---|---|
| `search.Dijkstra` (post-warmup, reusable state) | 8.081 ms, **0 B, 0 allocs** | `~` (p=0.471) |
| `search.Dijkstra` (large) | 8.123 ms | `~` (p=0.471) |
| `search.BFS` direction-optimising (power law) | 28.50 ms, **0 B, 0 allocs** | `~` (p=0.066), the largest — **unresolved, not flat** |
| `search.Yen` k=100 | 14.49 ms | `~` (p=0.471) |
| `centrality.Brandes` (random graph) | 7.776 ms | `~` (p=0.173) |

**The zero-allocation hot-path mandate holds, verified rather than asserted:** `allocs/op` is
identical on `Dijkstra_PostWarmup` and `BFSDirectionOpt_PowerLaw` with **every sample equal**
in both arms.

**The durability fix is priced, and it has no baseline because it is new.** The five
`store/checkpoint` benchmarks do not exist at `v0.14.0`, so nothing is compared and nothing is
carried over. Measured within this release: reading the snapshot back costs **14.73 ms against
a 150.3 ms checkpoint — 9.8 %** — and #2780's decode pass adds **0.46 ms** on top, 0.31 % of a
checkpoint, with `MapperDecodeOnly` at 235.6 µs carrying no filesystem call at all.

**`bolt/server` closes a gap `v0.14.0` named as its own.** That report recorded the whole
`TxBookkeeping_*` family as unmeasured; all 24 `bolt/server` benchmarks were run here,
including its eleven, and **none is significant**.

**What this release does not establish.** **`cypher` is sampled, not covered — 60 of 149
benchmark functions**, the full sweep costing 454 s per arm against 204 s for the targeted
set, which is disproportionate for a patch release. `graph/index{,/btree,/hash}`,
`graph/lpg`, `store/snapshot`, `store/wal` and six further concurrency ladders were sized and
**dropped**, each with its reason recorded. **Latency percentiles at the published
concurrency levels are unmeasured for an eighth consecutive cycle**; 1 024 concurrent writers
on the durable path is unmeasured; on-disk footprint growth is unmeasured; `bench/mtaudit`'s
engine-under-writer ladder remains the highest-value measurement not made, now for three
releases. The soak and nightly layers were not run, no `-race` figures and no profiles were
captured, **attribution is established for nothing**, and everything here is **one host, one
architecture**: Apple M4, 10 cores, `darwin/arm64`, macOS 26.6.2. The floors above are
properties of that host and the adjudication bars derived from them do not transfer.

> **Reproduce:** the exact commands — the three-arm interleaved harness, the load gate, the
> whole-campaign load sampler and the noise-floor procedure — are in
> [docs/benchmarks/v0.14.1.md](docs/benchmarks/v0.14.1.md) §7 and were used verbatim from
> [docs/benchmarks/v0.14.1-raw/](docs/benchmarks/v0.14.1-raw/); the general workflow is
> `make bench BENCH_PATTERN=. BENCH_COUNT=5` and [docs/profiling.md](docs/profiling.md).
> Hardware deltas should be reported in CHANGELOG.md alongside any number that regresses
> beyond the local `benchstat` regression gate (`scripts/bench_gate.sh`), which is run
> locally to compare a candidate against its baseline before the change lands.

**Figures from earlier releases, kept and labelled rather than relabelled.** The following are
**not** `v0.14.1` measurements and were not re-measured by this campaign: the conditional
join-reorder result (`ZZReorderSkewed/reorder=on`, **267.155 ms → 2.723 ms, −98.98 %**) is a
**`v0.13.0` → `v0.14.0`** figure from the `v0.14.0` campaign, and fires only when
`RefreshStatistics` has run on a qualifying disjoint two-component shape; `Mapper.Intern`
(hot key, uncontended) at **8.67 ns/op, 0 B, 0 allocs** is carried forward from **`v0.11.0`**;
and the 1 024-level durable write rate of **111,483 ops/s** (store API) / **115,301 ops/s**
(Cypher engine) at 422.05 commits per fsync is **`v0.11.0`**'s and has not been re-measured
since.


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
