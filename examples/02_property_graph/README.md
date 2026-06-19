# Example 02 — Property graph

## What it demonstrates

Building a labelled property graph (LPG) with an **optional type schema**,
attaching labels and typed properties spanning all four scalar kinds (string,
int64, float64, bool), running **label- and property-indexed `MATCH`-style
queries** through the [`graph/query`](../../graph/query) engine, and — the other
half of the round trip — reading the typed properties back out of a matched
node with `lpg.Graph.GetNodeProperty`.

It is the property-graph counterpart to the scale benchmark in
[example 26](../26_social_scale_bench): a seeded, scale-parametrised dataset
that reports build throughput, index-backed query latency, live heap and bytes
per node, with a deterministic data shape pinned by a regression test.

## Domain / scenario

A realistic **employee directory**. A seeded generator produces:

- `:Person` nodes, each carrying a `name` (string), `age` (int64), `salary`
  (float64), `active` (bool), `dept` (string, one of a fixed set) and a stable
  `id` (`p<NNN>`);
- a configurable fraction of those persons additionally carrying the `:Manager`
  label;
- `:Org` nodes, each carrying a `name` (string), `founded` year (int64) and
  `revenue` (float64), with a stable `id` (`o<NNN>`);
- one `(:Person)-[:WORKS_AT]->(:Org)` edge per person, to a randomly chosen org.

An optional `schema.Schema` declares the labels and the typed property keys and
is installed as the graph's runtime validator, so every property write is
type-checked at the boundary before it lands (disable it with `-schema=false`).

Fixing `-seed` fixes the data shape exactly — and therefore every indexed match
count and every read-back value.

## How to run

```sh
go run ./examples/02_property_graph                      # small deterministic default
go run ./examples/02_property_graph -persons 2000000 -orgs 5000 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large value |
|---|---|---|---|
| `-persons` | number of `:Person` nodes | `2000` | `2000000` |
| `-orgs` | number of `:Org` nodes | `50` | `5000` |
| `-manager-pct` | percentage of persons that are managers (0..100) | `20` | `20` |
| `-active-pct` | percentage of persons that are active (0..100) | `75` | `75` |
| `-seed` | RNG seed (fixes the data shape) | `1` | `7` |
| `-schema` | install the type schema as a runtime validator | `true` | `true` |

The per-person value ranges (age 21–65, salary 35 000–180 000) are fixed
constants rather than flags: they shape the *values* a node carries, not the
*scale* of the dataset.

## Expected output

At the default config the deterministic **fact** lines are:

```
config.persons=2000
config.orgs=50
config.manager_pct=20
config.active_pct=75
config.seed=1
config.schema=true
nodes.persons=2000
nodes.orgs=50
edges.works_at=2000
q.persons=2000
q.managers=417
q.dept_engineering=348
q.active=1496
q.manager_orgs=50
sample.key=p0
sample.name=Charlotte Anderson
sample.age=58
sample.salary=71360.53
sample.active=true
sample.dept=Operations
```

Interleaved with the facts the run also prints `# `-prefixed **telemetry**, for
example:

```
# build.elapsed=10.724ms
# build.node_rate=191156 nodes/s
# mem.heap_alloc=1.79 MiB
# bytes_per_node=815.0
# q.dept_engineering.latency=399µs
```

Telemetry varies per run and per machine and is **never** pinned by the test —
only the bare fact lines above are.

## Evidence it collects

This is a graph-structure (`lpg`) plus indexed-query example, so it reports the
dimensions from that row of the evidence taxonomy:

- **Build throughput** — `# build.node_rate` (nodes/s) and `# build.elapsed`.
- **Live heap and bytes per node** — `# mem.heap_alloc`, `# mem.heap_growth`,
  `# mem.total_alloc`, `# mem.num_gc`, `# bytes_per_node`.
- **Index-backed query latency** — one `# q.<name>.latency` line per query.
- **Count of nodes matching each indexed predicate** — the `q.persons`,
  `q.managers`, `q.dept_engineering`, `q.active`, `q.manager_orgs` fact lines.

When you scale it up, watch how the bytes-per-node figure settles and how the
index-backed point-lookup latencies stay flat as the population grows — that is
the label/property index doing its job instead of a full scan.

## Key APIs

- `graph/lpg.New` — build a labelled property graph over the mutable adjacency-list backend.
- `graph/lpg/schema.New` / `Schema.RegisterLabel` / `Schema.RegisterProperty` and `lpg.Graph.SetValidator` — declare and install the optional type schema enforced on the write path.
- `graph/lpg.Graph.SetNodeLabel` / `SetNodeProperty` / `AddEdgeLabeled` — attach labels, typed property values, and a typed edge.
- `graph/lpg.StringValue` / `Int64Value` / `Float64Value` / `BoolValue` — construct the four scalar typed property values.
- `graph/lpg.Graph.GetNodeProperty` and `PropertyValue.String` / `Int64` / `Float64` / `Bool` — read the typed properties back off each matched node.
- `graph/csr.BuildFromAdjList` — freeze the live graph into the immutable CSR snapshot the query engine reads.
- `graph/query.New` / `Engine.Match` / `Pattern.Vertex` / `Pattern.Out` / `Pattern.Cardinality` — run the index-backed `MATCH`-style queries.
- `graph/query.WithLabel` / `WithProperty` — the label and property predicates that drive the indexed match (Roaring label bitmaps + per-property value index).

## Further reading

- [`graph/lpg`](../../graph/lpg) — the labelled-property-graph package documentation
- [`graph/lpg/schema`](../../graph/lpg/schema) — schema declaration and validation
- [`graph/query`](../../graph/query) — the indexed `MATCH`-style query engine
- [Example 01 — basic shortest paths](../01_basic) — the prior example, building the flow this one extends
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the reference scaled example this one mirrors
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
