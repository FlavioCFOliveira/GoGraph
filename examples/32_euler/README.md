# Example 32 — Eulerian circuits (route inspection)

## What it demonstrates

Finding an **Eulerian circuit** — a tour that traverses every edge exactly
once and returns to its start — with Hierholzer's algorithm, over both an
**undirected** graph (`search.HierholzerUndirected`) and a **directed** one
(`search.Hierholzer`). It verifies the returned tour uses every street exactly
once and is a closed circuit, and shows the module correctly reporting
`search.ErrNoEulerian` when the preconditions fail. The scenario is *route
inspection* (the "Chinese postman" setting): a fleet must cover every street
of a network once and come back to the depot.

A subtle, faithful lesson is built in: **closing a single street does not
destroy the route.** Removing one undirected edge leaves exactly two
odd-degree vertices, so an Eulerian *path* still exists — a valid inspection
run that simply no longer returns to the depot (the directed analogue leaves
one surplus source and one surplus sink). Hierholzer finds that path. Only
when **two vertex-disjoint** streets are closed (four odd-degree vertices) does
every Eulerian trail disappear and the module report `ErrNoEulerian`. The
`-broken` flag closes two disjoint streets for exactly this reason.

## Domain / scenario

A seeded route network assembled from **edge-disjoint cycles** ("patrol
loops"). A base ring through a random permutation of all `-nodes`
intersections guarantees the network is connected and every node starts at
even degree. Each of `-loops` extra loops is a simple cycle over a random
subset of intersections, added only if all of its streets are new
(edge-disjoint). Because every added cycle raises each of its nodes' degree by
two (undirected) — or its in- and out-degree by one each (directed) — the
Eulerian precondition is preserved by construction no matter how many loops
are layered on. The streets far outnumber the intersections, so Hierholzer
genuinely stitches many loops together rather than walking a single cycle.

## How to run

```sh
go run ./examples/32_euler                              # small deterministic default
go run ./examples/32_euler -nodes 200000 -loops 40000   # observable-scale run
go run ./examples/32_euler -broken                      # close two streets → no Eulerian tour
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-nodes` | number of intersections (≥ 4) | `200` | `200000` |
| `-loops` | extra edge-disjoint patrol loops | `40` | `40000` |
| `-loop-min` | minimum loop length (≥ 3) | `3` | `3` |
| `-loop-max` | maximum loop length (≤ nodes) | `8` | `8` |
| `-broken` | close two disjoint streets to force `ErrNoEulerian` | `false` | — |
| `-seed` | RNG seed (fixes the network shape) | `1` | any |

## Expected output

At the default config the deterministic **fact** lines are:

```
config.nodes=200
config.loops=40
config.broken=false
config.seed=1
undirected.streets=404
undirected.trail_len=405
undirected.trail_len_is_streets_plus_1=true
undirected.each_street_once=true
undirected.is_circuit=true
directed.streets=404
directed.trail_len=405
directed.trail_len_is_streets_plus_1=true
directed.each_street_once=true
directed.is_circuit=true
```

With `-broken` the two street closures leave four odd-degree vertices, so:

```
undirected.streets=402
undirected.no_eulerian=true
directed.streets=402
directed.no_eulerian=true
```

Interleaved with the facts are volatile **telemetry** lines, prefixed with
`# `, that vary per run and per machine:

```
# undirected.elapsed=53µs
# directed.elapsed=16µs
# mem.heap_alloc=208.70 KiB
```

The regression test pins the fact lines and ignores every `# ` line.

## Evidence it collects

For the traversal subject (per `docs/examples-standard.md`): the **per-tour
wall-clock** (`# undirected.elapsed`, `# directed.elapsed`) for the O(E)
Hierholzer pass, and **live heap** (`# mem.heap_alloc`, `# mem.heap_growth`).
The correctness evidence is the tour verification itself — every street used
exactly once, trail length `E + 1`, and the circuit closing back to its start —
asserted as facts rather than telemetry. Scale it up with `-nodes` / `-loops`
and watch the elapsed grow linearly with the street count.

## Key APIs

- `graph/adjlist.New` (`Directed: false` / `true`) / `AdjList.AddEdge` — build the mutable route network; the undirected form mirrors each street automatically.
- `graph/csr.BuildFromAdjList` — freeze the builder into the immutable CSR snapshot Hierholzer reads.
- `search.HierholzerUndirectedCtx` — Eulerian circuit/path over an undirected (symmetric) CSR; returns the trail as `[]graph.NodeID` of length `E + 1`, or `search.ErrNoEulerian`.
- `search.HierholzerCtx` — the directed counterpart, requiring equal in- and out-degree for a circuit.
- `search.ErrNoEulerian` — the sentinel returned when no Eulerian trail exists.

## Further reading

- [`search`](../../search) — traversal and path-finding package documentation
- [Example 12 — Build dependency](../12_build_dependency) — topological sort and cycle detection over a directed graph
- [Example 13 — Network reliability](../13_network_reliability) — connectivity, articulation points and max-flow over a network
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
