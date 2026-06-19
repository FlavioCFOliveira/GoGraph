# Example 07 — GraphML round-trip

## What it demonstrates

GoGraph's graph interchange I/O: serialise an in-memory graph to GraphML,
parse that GraphML back with `graphml.ReadInto` so the reader is genuinely
exercised, then re-serialise the parsed graph to two formats — GraphML
(`graphml.Write`) and Graphviz DOT (`dot.Write`). It shows that a graph
survives a write/parse/write round-trip with its nodes, edges, and edge
weights intact, and reports the interchange evidence (bytes in/out per
format, parse and serialise throughput) that matters for an I/O subject.

## Domain / scenario

A directed, weighted **link graph** — a miniature "web" of documents that
cite one another. Pages are 12-character hex ids; the graph is grown by
**preferential attachment**, so each new page links to a small number of
distinct earlier pages with probability proportional to the in-degree they
have already accumulated. That yields a heavy-tailed in-degree
distribution (a handful of "authority" pages collect most links) — the
realistic shape of a citation or hyperlink web and a more interesting
interchange payload than a uniform-random graph. Every link carries a
positive integer weight in `[1, max-weight]`, stored as the GraphML
`<data key="w">` long and rendered by DOT as a `label="..."` edge
attribute.

The generated graph is a **simple directed graph** (no self-loops, no
parallel edges), so the GraphML reader — which is directed when
`edgedefault` is not `undirected` and collapses parallel edges —
re-materialises it edge-for-edge. That makes the round-trip exact:
re-reading the written GraphML yields the same node count, the same edge
count, and the same weight sum.

## How to run

```sh
go run ./examples/07_graphml_roundtrip                          # small deterministic default
go run ./examples/07_graphml_roundtrip -nodes 1000000 -edges 12 # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-nodes` | number of pages (nodes) to generate | `300` | `1000000` |
| `-edges` | out-degree target per page (capped at the earlier-page count) | `4` | `12` |
| `-max-weight` | link weights are drawn uniformly from `[1, max-weight]` | `100` | `1000` |
| `-seed` | RNG seed (fixes the deterministic data shape) | `1` | any |
| `-sample` | print this many leading lines of each serialised format (0: none) | `0` | `12` |

The default stays well under the 60 s per-package short-test budget and
is deterministic; the large run is where parse/serialise throughput and
the interchange byte footprint become observable. At the default scale the
example does **not** dump the GraphML or DOT documents to stdout — it
serialises to in-memory buffers and reports their byte sizes; pass
`-sample N` for a quick visual check of the first `N` lines of each format.

## Expected output

```
config.nodes=300
config.edges_per_node=4
config.max_weight=100
config.seed=1
nodes=300
edges=1190
weight.sum=58041
graphml.parsed_nodes=300
graphml.parsed_edges=1190
graphml.written_bytes=126356
dot.written_edges=1190
roundtrip.weight_sum=58041
roundtrip.ok=1
# graphml.in_bytes=123.39 KiB
# graphml.parse.throughput=17.72 MiB/s
# mem.heap_alloc=673.68 KiB
```

The bare lines are deterministic **facts** pinned by the regression test
and reproducible for a fixed `-seed`. The `# `-prefixed lines are volatile
**telemetry** (byte sizes, throughput, heap) that vary per run and per
machine — only one representative telemetry line is shown above; the
binary prints the full set.

## Evidence it collects

From the interchange row of the evidence taxonomy: **parse and serialise
throughput** (nodes/s and MiB/s), **bytes in/out** for each format
(GraphML in, GraphML out, DOT out), live **heap**, and the **round-trip
invariant** (`roundtrip.ok=1`, the conservation of node count, edge count,
and weight checksum across the trip). When scaling up, watch how the
GraphML payload size grows with edge count, how parse throughput compares
to serialise throughput, and how DOT (a more compact text format) compares
to GraphML in bytes per edge.

## Key APIs

- `graph/io/graphml.ReadInto` / `graphml.ReadIntoCtx` — parse a GraphML document into an `adjlist.AdjList[string, int64]`, returning the edge count.
- `graph/io/graphml.Write` / `graphml.WriteCtx` — serialise an adjacency list to a GraphML document.
- `graph/io/dot.Write` / `dot.WriteCtx` — serialise the same graph to Graphviz DOT (`digraph` for directed graphs, with `label="..."` weights).
- `graph/adjlist.AdjList` — the mutable adjacency-list the round-trip generates into and reads back.

## Further reading

- [`graph/io/graphml`](../../graph/io/graphml) — GraphML reader/writer package documentation
- [`graph/io/dot`](../../graph/io/dot) — Graphviz DOT writer package documentation
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list the round-trip loads into
- [Example 06 — CSV import](../06_csv_import) — the sibling graph-I/O example
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
