# Importing and Exporting Graphs

This document describes the data exchange formats GoGraph speaks
and the compatibility matrix with common external tooling.

## Type restriction

All graph/io readers and writers (CSV, DOT, GraphML, JSON Lines) and
the store/bulk Loader are concretely typed as
`adjlist.AdjList[string, int64]`. Genericising over `[N comparable, W any]`
is on the roadmap but is deferred: the parsers must
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

GraphML declares `attr.type` per `<key>`, not per value. When a property
name carries the *same* kind on every node, the writer emits a single
`<key id="p_<name>">` (the legacy, byte-stable form). When the same name
appears with *different* kinds across nodes (e.g. `v=42` on one node,
`v="hello"` on another), the writer emits one `<key>` per kind — each with
its own id `p_<name>_<attr.type>` and the shared `attr.name` — and every
`<data>` references the id matching that value's kind, so each value
round-trips with its own type rather than failing the import or silently
degrading (#1791). The reader resolves each `<data>` purely by its key id,
so this is transparent and back-compatible with single-key files.

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

## Non-finite float properties

Float (`PropFloat64`) property values may be `NaN`, `+Inf`, or `-Inf`.
The two encoders handle them as follows:

- **GraphML** emits the XML-Schema `xs:double` lexical forms `NaN`,
  `INF`, and `-INF` inside the `attr.type="double"` `<data>` element, so
  conformant external parsers (NetworkX, the Java GraphML stack) accept
  them. The reader parses all three back via `strconv.ParseFloat`.
- **JSON Lines** carries every float as a JSON *string* (never a bare
  JSON number), so it is not bound by JSON's prohibition on non-finite
  numerics. Non-finite values are written with Go's `strconv` form —
  `"NaN"`, `"+Inf"`, `"-Inf"` — and round-trip losslessly within GoGraph.
  External consumers reading the value as a numeric float must treat
  these three tokens specially.

Both encoders round-trip every non-finite value exactly within GoGraph.

## List property value limits

Both encoders serialise a list (`PropList`) property as a JSON array of
`[kind, value]` pairs, embedding each nested level as a re-escaped JSON
string. The escaping makes the serialised size grow by roughly a factor of
four per nesting level, so to keep the writer's memory bounded each encoder
fails fast — rather than exhausting memory — when a single property value
either nests deeper than **128 levels** (`ErrPropertyNestingTooDeep`) or
serialises to more than **64 MiB** (`ErrPropertyValueTooLarge`). Realistic
list properties are orders of magnitude below both limits; the guards exist
solely to reject pathological values that would otherwise OOM or hang the
writer (the `WriteWithProps` call returns the typed error and writes
nothing further for that graph).

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
