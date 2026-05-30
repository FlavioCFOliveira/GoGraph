# Example 12 — Build-dependency order and cycle detection

## What it demonstrates

Modelling a software build-dependency graph as a directed graph,
deriving a valid build order with `search.TopologicalSort` (Kahn's
algorithm), and detecting a circular dependency with `search.TarjanSCC`
when one is introduced.

## Domain / scenario

A small Go-style package dependency graph. Each directed edge `(a, b)`
reads "`a` depends on `b`", so `b` must be built before `a`:

```
app   -> auth
app   -> store
auth  -> crypto
store -> db
db    -> logging
auth  -> logging
```

The first half derives the build order for this acyclic graph
(dependencies first). The second half adds a back edge `logging -> app`,
which closes a cycle through `app -> auth -> logging -> app` (and
`app -> store -> db -> logging -> app`). `TopologicalSort` then fails
with `ErrCycle`, and `TarjanSCC` reports the strongly connected
component that contains the cycle.

## Determinism

The output is byte-stable across runs:

- NodeIDs are assigned by the mapper in the order names first appear in
  the hard-coded edge slice, and Kahn's algorithm emits vertices in
  ascending NodeID order — so the topological (and reversed build)
  order is fixed for this input.
- The names printed inside a detected cycle are sorted alphabetically
  before printing, so the cycle line does not depend on Tarjan's
  internal stack-pop order.

## How to run

```sh
go run ./examples/12_build_dependency
```

## Expected output

```
=== Build order (no cycles) ===
  1. logging
  2. db
  3. crypto
  4. store
  5. auth
  6. app

=== Detecting a cycle ===
topological sort rejects the cycle (ErrCycle).
Strongly connected components (size > 1 are cycles):
  cycle: [app auth db logging store]
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable directed dependency graph.
- `graph/adjlist.AdjList.Mapper` / `graph.Mapper.Resolve` — translate compact `NodeID`s back to package names.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot for analytics.
- `search.TopologicalSort` / `search.ErrCycle` — derive a build order, or fail when the graph has a cycle.
- `search.TarjanSCC` — find the strongly connected components; a component of size > 1 is a cycle.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 01 — Basic shortest paths](../01_basic) — the minimal build → snapshot → query flow
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
