# GoGraph

A Go module for graph persistence, manipulation, and fast search,
designed to scale from in-memory graphs to graphs that exceed RAM.

## Status

**Current release: `v0.5.0`.** This is the project's **seventh
release**, published at a pre-1.0 baseline: under Semantic Versioning a
`0.y.z` version signals that the public API is **not yet stable** and may
change without a major bump while the module matures toward `1.0.0`.
`v0.5.0` is a pre-1.0 **MINOR** release — it makes the built-in `db.*`
schema-introspection procedures (`db.labels()`,
`db.relationshipTypes()`, `db.propertyKeys()`) report only the labels,
relationship types, and property keys **currently in use** on live
elements, and adds three exported `lpg.Graph` methods
(`NodeLabelsInUse`, `RelationshipTypesInUse`, `PropertyKeysInUse`); it is
additive over `v0.4.0`. The five major
subsystems below are functional and tested under race, lint, and soak gates.
The two compliance invariants are already in force at this version: the
module is **100 % openCypher TCK-compliant at the execution level**
(**3 897/3 897 scenarios, 16 006/16 006 steps**) and **100 % ACID-compliant**;
every change is gated by the project's CI suite (build, vet, race, lint,
`govulncheck`, TCK conformance, and the deterministic crash-injection
battery). The module uses
the conventional Go path `github.com/FlavioCFOliveira/GoGraph` and is
fetchable with `go get github.com/FlavioCFOliveira/GoGraph@v0.5.0`. See
[CHANGELOG.md](CHANGELOG.md) and
[release-notes/v0.5.0.md](release-notes/v0.5.0.md) for the full release
narrative.

### Core graph (`graph/`)

- `github.com/FlavioCFOliveira/GoGraph/graph` — generic node identifiers and the `Graph[N, W]`
  contract.
- `github.com/FlavioCFOliveira/GoGraph/graph/adjlist` — mutable, sharded adjacency-list backend
  with copy-on-write snapshots and lock-free reads.
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
- `github.com/FlavioCFOliveira/GoGraph/store/txn` — single-writer transactional API
  (Begin/Commit/Rollback).
- `github.com/FlavioCFOliveira/GoGraph/store/checkpoint` — background WAL → snapshot folder.
- `github.com/FlavioCFOliveira/GoGraph/store/recovery` — snapshot + WAL replay on open.
- `github.com/FlavioCFOliveira/GoGraph/store/csrfile` — mmap-backed Tier 2 CSR file format,
  writer, reader, `Reinterpret` zero-copy helper, deterministic
  fixture generator.
- `github.com/FlavioCFOliveira/GoGraph/store/bulk` — high-throughput bulk loader bypassing
  the WAL.

### Cypher engine (`cypher/`)

- `github.com/FlavioCFOliveira/GoGraph/cypher` — openCypher-compatible parser, planner, and
  execution engine; WAL-durable writes via `NewEngineWithStore`.
- `github.com/FlavioCFOliveira/GoGraph/cypher/parser` · `cypher/ast` · `cypher/sema` ·
  `cypher/ir` · `cypher/plan` · `cypher/exec` — parser-to-execution
  pipeline with plan-cache, EXPLAIN/PROFILE, and dbhits accounting.
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
  hot-reload and graceful shutdown.

Subsystem references: [docs/persistence.md](docs/persistence.md)
(WAL, snapshots, recovery) · [docs/tier2.md](docs/tier2.md) (csrfile)
· [docs/io.md](docs/io.md) (interchange formats)
· [docs/algorithms.md](docs/algorithms.md) (algorithms catalogue)
· [docs/cypher.md](docs/cypher.md) (Cypher engine)
· [docs/bolt.md](docs/bolt.md) (Bolt server).

## Examples

The `examples/` directory contains 25 runnable demonstrations. See
[examples/README.md](examples/README.md) for the full categorized index
with per-example links and run commands.

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

Run any example with `go run ./examples/<NAME>/`.

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

Benchmarks (Apple M4, Go 1.26.3):

| Operation | Throughput |
|---|---|
| `Mapper.Intern` (hot key) | 17 ns/op, 0 allocs |
| `adjlist.HasEdge` (hot cache) | 49 ns/op, 0 allocs |
| `csr.NeighboursByID` | 10.6 ns/op, 0 allocs |
| `csr.BuildFromAdjList` of 10^7 edges | 51 ms |
| `search.BFS` on 10^7-node chain | 38 ms, 1.25 MB peak, 0 allocs/call after warmup |
| `search.Dijkstra` on 1M-node / 4M-edge random graph | 320 ms |
| `search.BellmanFord` on 16K-vertex / 64K-edge | 1.8 ms |

> **Measured on:** 2026-05-22 against commit `1a2f00e`, Apple M4
> (10-core), Go 1.26.3, macOS 25.4.0 (darwin/arm64).
> **Reproduce:** `make bench BENCH_PATTERN=. BENCH_COUNT=5`
> (see [docs/profiling.md](docs/profiling.md) for the sample
> workflow). Per-run variance is captured by `benchstat` and the
> headline numbers above are the median of five runs at `-count=5`.
> Hardware deltas should be reported in CHANGELOG.md alongside
> any number that changes by more than the 5 % CI gate
> (`benchstat regression gate` in `.github/workflows/ci.yml`).

The `v0.3.1` performance cycle lifts the write, analytics, and query
paths without regressing the single-threaded figures above: **group
commit** raises concurrent write throughput ≈ 118× at 256 goroutines
(with zero single-thread regression), **parallel PageRank** runs 1.7–2.4×
on large graphs (bit-identical results), and a **range-predicate B+tree
index seek** is ≈ 114× on selective indexed ranges. See
[docs/benchmarks/v0.3.1.md](docs/benchmarks/v0.3.1.md) for the full
per-release report and concurrency sweeps.

## Module Layout

```
graph/                    — core types: NodeID, Graph[N,W] contract, sharded Mapper
graph/adjlist             — mutable copy-on-write adjacency list (writer-side)
graph/csr                 — immutable Compressed Sparse Row snapshot (reader-side)
graph/generation          — refcount-protected Publisher for atomic snapshot rotation
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
store/txn                 — single-writer transactions (Begin/Commit/Rollback)
store/checkpoint          — background WAL → snapshot folder goroutine
store/recovery            — snapshot + WAL replay on open
store/csrfile             — mmap'd Tier 2 CSR file format (versioned, 64-byte aligned)
store/bulk                — high-throughput bulk ingestion bypassing the WAL

ds/                       — supporting data structures (Union-Find, ...)

bench/ldbc                — LDBC SNB SF1 / SF10 benchmark harness
bench/dimacs9             — DIMACS 9 USA-road SSSP benchmark
bench/rmat                — RMAT power-law graph generator
bench/soak                — 4-hour mixed-workload reliability soak harness
bench/comparison          — cross-library performance comparison vs NetworkX

internal/metrics          — observability API hook (Backend, IncCounter, ObserveLatency, Time)
internal/stress           — concurrency stress test suite (CI under -race)
internal/shapegen         — graph shape generators (trivial, classic, random models, adversarial)
internal/invariants       — graph invariant checkers (connected, DAG, bipartite, distance bound)
internal/testfs           — FS fault-injection wrapper (ENOSPC, partial write, fsync delay)
internal/crashinject      — subprocess crash-injection harness (SIGKILL breakpoints)
internal/subproc          — cross-process test helper (re-exec, mode dispatch)
internal/goldens          — golden-file assertion helper with -update and atomic write

See [docs/test-battery.md](docs/test-battery.md) for the production-readiness
test battery guide and the add-new-shape recipe.

examples/                 — 25 runnable example programs (see "Examples" section)
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
[SECURITY.md](SECURITY.md). Use GitHub Security Advisories or the
private email listed there — please do not open a public issue for a
suspected vulnerability.

## License

GoGraph is distributed under the [MIT License](LICENSE).
