# Example 09 — Leiden community detection

## What it demonstrates

Modularity-optimising community detection with `community.Leiden`: build a
graph that has a genuine, tunable community structure, freeze it into an
immutable CSR snapshot, run Leiden, read the resulting `Partition` back,
and measure how well Leiden recovered the planted structure by computing
the Newman modularity `Q` of its output.

## Domain / scenario

A seeded **planted-partition graph** (symmetric stochastic block model):
`-communities` (`K`) equal-sized blocks of `-community-size` (`s`) nodes
each. Every unordered node pair is offered an undirected edge
independently — with probability `-p-in` when the two nodes share a block
and `-p-out` when they do not. With `p-in ≫ p-out` the blocks are dense
inside and sparse between, so a modularity-optimising method recovers them.

The default parameters place the partition about three times above the
SBM detectability (Kesten–Stigum) threshold, deep in the regime where
Leiden reliably recovers the planting. The generator guidance — the
detectability threshold, the per-block Erdős–Rényi connectivity floor that
`validate()` enforces, and the planted-partition modularity expectation —
was supplied by the `graph-theory-expert` sub-agent and is recorded in the
leading doc comment and the `validate` / `computeModularity` comments.

## How to run

```sh
go run ./examples/09_leiden                                                       # small deterministic default
go run ./examples/09_leiden -communities 8 -community-size 500 -p-in 0.06 -p-out 0.0008 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-communities` | number of planted communities `K` | `4` | `8` |
| `-community-size` | nodes per community `s` | `25` | `500` |
| `-p-in` | intra-community edge probability | `0.55` | `0.06` |
| `-p-out` | inter-community edge probability | `0.01` | `0.0008` |
| `-seed` | RNG seed (fixes the data shape exactly) | `1` | any `int64` |

The default builds 100 nodes and ~700 edges and finishes in well under a
second; it is the shape pinned by the regression test. The large
invocation builds 4 000 nodes and ~130 000 edges, where the detection cost
becomes observable. `validate()` rejects configurations that cannot
produce a recoverable partition: `p-in` must exceed `p-out`, and `p-in`
must clear the connectivity floor `2·ln(s)/(s−1)` so a block cannot
fragment into singletons and inflate the recovered community count.

## Expected output

Bare lines are deterministic **facts** (reproducible for a fixed `-seed`);
the rounded `modularity` is pinned to two decimals as a fact, with the full
value reported as telemetry. Lines prefixed with `# ` are volatile
**telemetry** and vary per run and per machine.

```
config.communities=4
config.community_size=25
config.p_in=0.55
config.p_out=0.01
config.seed=1
nodes=100
edges=701
communities_found=4
modularity=0.69
# build.elapsed=352µs              # telemetry — varies per run/machine
# build.edge_rate=1991947 edges/s  # telemetry
# mem.heap_growth=14.36 KiB        # telemetry
# detect.elapsed=170µs             # telemetry
# detect.node_rate=587085 nodes/s  # telemetry
# modularity.exact=0.694232        # telemetry
```

Leiden's output is deterministic for a fixed input graph, so for the
default seed the modularity is exactly `0.694232` run to run. Because
Leiden is randomised internally by contract, the regression test asserts a
**lower bound** (`Q ≥ 0.55`) and a **community-count band** (`K ± 1`,
i.e. `4` here) rather than an exact float, so it survives an internal
change that preserves partition quality.

## Evidence it collects

For a community-detection subject the example reports (per the evidence
taxonomy in [`docs/examples-standard.md`](../../docs/examples-standard.md)):

- **Number of communities recovered** vs. the planted `K` — the headline
  correctness signal.
- **Newman modularity `Q` of the returned partition** — the objective
  Leiden maximises, computed directly over the CSR snapshot.
- **Build and detection wall-clock and throughput** (`# ` telemetry).
- **Live-heap growth** of the graph snapshot (`# ` telemetry).

When you scale it up (`-communities 8 -community-size 500`), watch the
detection wall-clock and the recovered count: Leiden should still recover
exactly `K` communities with `Q ≈ 0.79`, while `# detect.elapsed` grows
with the graph. `go test -bench=BenchmarkRun -benchmem ./examples/09_leiden`
runs the large configuration mechanically.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddNode` / `AddEdge` — build the mutable, undirected planted-partition graph.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for analytics.
- `graph/csr.CSR.VerticesSlice` / `EdgesSlice` / `MaxNodeID` — the offsets/edges arrays the modularity computation walks in `O(V+E)`.
- `search/community.LeidenCtx` / `DefaultLeidenOptions` — run context-aware Leiden community detection.
- `search/community.Partition` — the result: `NumCommunities` and a NodeID-indexed `Community` slice whose ghost slots carry the sentinel `-1`.

## Further reading

- [`search/community`](../../search/community) — community-detection package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 08 — PageRank](../08_pagerank) — another analytics algorithm over a CSR snapshot
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
