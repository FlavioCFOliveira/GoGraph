# Importing and Exporting Graphs

This document describes the data exchange formats GoGraph speaks
and the compatibility matrix with common external tooling.

## v1 type restriction

All graph/io readers and writers (CSV, DOT, GraphML, JSON Lines) and
the store/bulk Loader are concretely typed as
`adjlist.AdjList[string, int64]`. Genericising over `[N comparable, W any]`
is on the v1.x roadmap but is deferred: the parsers must
serialise/deserialise N to bytes, which needs a typed `NodeCodec[N]`
interface plus an implementation per N. Threading that interface
across four file-format packages, the bulk loader, the recovery
path, every test and every example is a sizable refactor.

Callers with non-string node IDs or non-int64 weights should keep an
in-memory mapping layer (e.g. `fmt.Sprintf` the user value into the
string slot the reader expects) until the generic surface lands.


## Supported formats

| Format     | Reader | Writer | Package                  | Notes                                 |
|------------|--------|--------|--------------------------|---------------------------------------|
| CSV        | ✓      | ✓      | `graph/io/csv`           | `src,dst[,weight]` rows; `#` comments |
| GraphML    | ✓      | ✓      | `graph/io/graphml`       | XML; directed / undirected; weights   |
| DOT        |        | ✓      | `graph/io/dot`           | Graphviz text; for visualisation      |
| JSON Lines | ✓      | ✓      | `graph/io/jsonl`         | `{type: node\|edge, …}` per line      |

For bulk ingestion that bypasses the WAL, use
[`store/bulk`](../store/bulk/bulk.go); for the on-disk Tier 2 CSR
binary format, see [`csrfile`](csrfile-v1.md) and [`tier2.md`](tier2.md).

## CSV

Default options match the most common conventions:

- comma delimiter,
- `#` comment lines skipped,
- no header (override with `Options.HasHeader = true`).

```go
g, n, err := csv.ReadInto(file, csv.DefaultOptions())
// ... use g ...
csv.Write(out, g, csv.DefaultOptions())
```

A third optional column is parsed as an `int64` edge weight.

## GraphML

The reader accepts the conventional shape:

```xml
<graphml xmlns="http://graphml.graphdrawing.org/xmlns">
  <key id="w" for="edge" attr.name="weight" attr.type="long"/>
  <graph id="G" edgedefault="directed">
    <node id="alice"/>
    <node id="bob"/>
    <edge source="alice" target="bob"><data key="w">7</data></edge>
  </graph>
</graphml>
```

`edgedefault="undirected"` selects the matching adjacency-list
configuration. Other attributes on `<node>` / `<edge>` /
`<graph>` are accepted and ignored.

## DOT (Graphviz)

DOT is write-only in v1. The exporter emits a `digraph G` (or
`graph G` for undirected), one `<id> -> <id> [label="<w>"]` edge
per row, with the weight label omitted when zero. Identifiers
containing non-alphanumeric characters are double-quoted with
proper escaping.

```go
dot.Write(os.Stdout, g)
```

## JSON Lines (NDJSON)

One JSON object per line:

```
{"type":"node","id":"alice"}
{"type":"node","id":"bob"}
{"type":"edge","src":"alice","dst":"bob","weight":7}
```

The reader/writer accept the `Record` shape and the writer
suppresses HTML escaping so the output matches conventional JSON
tooling.

## Tooling compatibility matrix

| Tool                | CSV | GraphML | DOT | JSON Lines |
|---------------------|-----|---------|-----|------------|
| Gephi               | ✓   | ✓       | ✓   |            |
| Cytoscape           | ✓   | ✓       | ✓   |            |
| NetworkX            | ✓   | ✓       | ✓   |            |
| Graphviz            |     |         | ✓   |            |
| jq / line-oriented  |     |         |     | ✓          |

## Round-trip tests

The per-package tests cover round trips for every read/write pair:

- `graph/io/csv/csv_test.go::TestWrite_Roundtrip` — read, write,
  re-read.
- `graph/io/graphml/writer_test.go::TestWrite_Roundtrip` — Write
  + ReadInto preserve directed edges and weights.
- `graph/io/jsonl/jsonl_test.go::TestWrite_Roundtrip` — same
  pattern over the NDJSON encoder.

DOT is write-only, so its round trip is asserted by feeding the
output through `dot` externally; the unit tests cover the wire
contract (header, edge operator, weight labelling, quoting).
