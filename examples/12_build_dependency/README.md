# Example 12 — Build-dependency order and cycle detection

## What it demonstrates

Modelling a software build-dependency graph as a directed graph, deriving
a valid build order with `search.TopologicalSort` (Kahn's algorithm) and
verifying it against the topological-order validity invariant, then
detecting a circular dependency with `search.TarjanSCC` after a back-edge
is injected.

## Domain / scenario

A realistic, seeded, scale-parametrised module dependency graph. Each
directed edge `(a, b)` reads "`a` depends on `b`", so `b` must be built
before `a`. Modules are partitioned into `-layers` layers and a module in
layer *k* draws a few distinct dependencies from layers strictly below it
(`[0, k)`). Layer 0 modules are leaves — the foundation libraries with no
dependencies. The strict layering is what guarantees acyclicity: along any
dependency path the layer index strictly decreases, so no directed cycle
can form and `TopologicalSort` always succeeds.

The layer widths follow a pyramid (widest at the leaves, controlled by
`-pyramid-base`), apportioned with a floor pass so every layer is
non-empty and the per-layer counts sum exactly to `-modules`. A single
dependency chain is planted from the top layer down to layer 0, so the
graph always has a known longest chain and a known forward path to close
into a cycle.

The first stage derives the build order for this acyclic graph and
verifies it. The second stage injects a back-edge from the bottom of the
planted chain to its top, closing a circular dependency; `TopologicalSort`
then fails with `ErrCycle`, and `TarjanSCC` reports the single strongly
connected component that contains the cycle.

## How to run

```sh
go run ./examples/12_build_dependency                       # small deterministic default
go run ./examples/12_build_dependency -modules 1000000 -layers 40 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large example |
|---|---|---|---|
| `-modules` | number of module nodes | `5000` | `1000000` |
| `-layers` | number of dependency layers (`>= 2`) | `12` | `40` |
| `-deps-min` | minimum dependencies a non-leaf module requests | `1` | `1` |
| `-deps-max` | maximum dependencies a non-leaf module requests | `5` | `8` |
| `-pyramid-base` | layer-width growth toward the leaves (`>= 1`) | `1.6` | `2.0` |
| `-seed` | RNG seed (fixes the deterministic data shape) | `1` | `7` |

`-modules` must be at least `-layers` so every layer holds at least one
module; the configuration is rejected otherwise, once, at the boundary.

## Expected output

```
config.modules=5000
config.layers=12
config.deps=[1,5]
config.seed=1
nodes.modules=5000
edges.dependencies=9265
dag.layers=12
topo.order_valid=true
topo.modules_ordered=4927
dag.longest_chain=12
cycle.detected=true
cycle.scc_count=1
cycle.scc_size=14
```

The bare lines above are the deterministic **facts** the regression test
pins for the default seed. Interleaved with them, the program also prints
volatile **telemetry** prefixed with `# `, which varies per run and per
machine:

```
# build.elapsed=2.69ms
# build.node_rate=1858592 nodes/s
# topo.elapsed=108µs
# tarjan.elapsed=215µs
# mem.heap_alloc=1.25 MiB
```

`topo.modules_ordered` is below `nodes.modules` because `TopologicalSort`
omits modules that have no edges (leaves that nothing depends on).
`cycle.scc_size` is reported empirically rather than predicted: the
injected back-edge always closes the planted chain (`>= layers` modules),
but the resulting strongly connected component may absorb a few extra
modules whenever a random edge created a second forward path between two
chain modules. It is reproducible for a fixed seed.

## Evidence it collects

From the *graph structures / search* dimension of the evidence taxonomy:

- **Wall-clock per algorithm** — `# topo.elapsed` and `# tarjan.elapsed`
  isolate `TopologicalSort` and `TarjanSCC` from generation cost.
- **DAG statistics** — nodes, edges, layers, and the longest dependency
  chain (`dag.longest_chain`), which is the build's critical-path depth.
- **Build throughput and live heap** — `# build.*_rate` and `# mem.*`.

When scaling up, watch the two algorithm latencies grow roughly linearly
in `V + E` while generation stays the dominant cost, and watch
`dag.longest_chain` track `-layers`.

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` / `AdjList.AddNode` — build the mutable directed dependency graph.
- `graph/adjlist.AdjList.Mapper` / `graph.Mapper.Lookup` / `graph.Mapper.Resolve` — translate between module names and compact `NodeID`s.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for analytics.
- `search.TopologicalSortCtx` / `search.ErrCycle` — derive a build order, or fail when the graph has a cycle.
- `search.TarjanSCCCtx` — find the strongly connected components; a component of size > 1 is a cycle.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 01 — Basic shortest paths](../01_basic) — the minimal build → snapshot → query flow
- [Example 26 — Social-scale benchmark](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
