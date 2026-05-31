# Example 07 — GraphML round-trip

## What it demonstrates

GoGraph's graph interchange I/O: parse a GraphML document into the
mutable `adjlist` builder with `graphml.ReadInto`, then serialise the
same graph back out to two formats — GraphML (`graphml.Write`) and
Graphviz DOT (`dot.Write`). It shows that a graph survives a read/write
round-trip, edges and weights intact.

## Domain / scenario

A three-node directed chain with integer edge weights, supplied inline
as a GraphML literal:

```
alice -> bob   (7)
bob   -> carol (9)
```

The `<key>` declaration types the `weight` attribute as `long`, and the
graph's `edgedefault` is `directed`, so the DOT export renders as a
`digraph` with `label="..."` edge weights.

## How to run

```sh
go run ./examples/07_graphml_roundtrip
```

## Expected output

```
Ingested 2 edges from GraphML

GraphML out:
<?xml version="1.0" encoding="UTF-8"?>
<graphml xmlns="http://graphml.graphdrawing.org/xmlns">
  <key id="w" for="edge" attr.name="weight" attr.type="long"></key>
  <graph id="G" edgedefault="directed">
    <node id="alice"></node>
    <node id="bob"></node>
    <node id="carol"></node>
    <edge source="alice" target="bob">
      <data key="w">7</data>
    </edge>
    <edge source="bob" target="carol">
      <data key="w">9</data>
    </edge>
  </graph>
</graphml>
DOT out:
digraph G {
  alice -> bob [label="7"];
  bob -> carol [label="9"];
}
```

The output is byte-for-byte deterministic: both writers iterate nodes
in ascending `NodeID` order, so attribute and edge ordering is stable
across runs.

## Key APIs

- `graph/io/graphml.ReadInto` — parse a GraphML document into an `adjlist.AdjList[string, int64]`, returning the edge count.
- `graph/io/graphml.Write` — serialise an adjacency list back to a GraphML document.
- `graph/io/dot.Write` — serialise the same graph to Graphviz DOT (`digraph` for directed graphs, with `label="..."` weights).

## Further reading

- [`graph/io/graphml`](../../graph/io/graphml) — GraphML reader/writer package documentation
- [`graph/io/dot`](../../graph/io/dot) — Graphviz DOT writer package documentation
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list the round-trip loads into
- [Example 06 — CSV import](../06_csv_import) — the sibling graph-I/O example
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
