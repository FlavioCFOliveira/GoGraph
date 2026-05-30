# Example 02 — Property graph

## What it demonstrates

Building a labelled property graph (LPG): declaring an optional schema,
attaching labels and typed properties to nodes, running label- and
property-indexed `MATCH`-style queries, and — the other half of the
round trip — reading the typed properties back out of each matched node
with `lpg.Graph.GetNodeProperty`.

## Domain / scenario

A tiny social graph of five people. Each person node carries:

- the `Person` label, and the `Admin` label as well for administrators;
- a `name` string property and an `age` int64 property.

```
alice   (Person, Admin)  name=alice    age=30   -> bob, charlie
bob     (Person)         name=bob      age=25   -> dave
charlie (Person)         name=charlie  age=30
dave    (Person, Admin)  name=dave     age=42
erin    (Person)         name=erin     age=28
```

The example runs three queries: all `Admin` nodes, all `Person` nodes
aged exactly 30, and the one-hop out-neighbours of the admins. For every
matched node it then fetches the `name` and `age` properties and prints
them alongside the node key, so the report shows both *which* nodes
matched and *what* they hold. Each result group is sorted, so the output
is fully deterministic.

## How to run

```sh
go run ./examples/02_property_graph
```

## Expected output

```
All Admins:
  alice    name=alice    age=30
  dave     name=dave     age=42
Persons aged 30:
  alice    name=alice    age=30
  charlie  name=charlie  age=30
One-hop out from Admins:
  bob      name=bob      age=25
  charlie  name=charlie  age=30
```

## Key APIs

- `graph/lpg.New` — build a labelled property graph over the mutable adjacency-list backend.
- `graph/lpg/schema.New` / `Schema.RegisterLabel` / `Schema.RegisterProperty` — declare the labels and typed property keys the graph expects.
- `graph/lpg.Graph.SetNodeLabel` / `SetNodeProperty` — attach labels and typed property values to nodes.
- `graph/lpg.Int64Value` / `StringValue` — construct the typed property values written to the graph.
- `graph/lpg.Graph.GetNodeProperty` and `PropertyValue.String` / `PropertyValue.Int64` — read the typed properties back off each matched node.
- `graph/query.New` / `Engine.Match` / `Pattern.Vertex` / `Pattern.Out` / `Pattern.Collect` — run the label- and property-indexed `MATCH`-style query.
- `graph/query.WithLabel` / `WithProperty` — the label and property predicates that drive the indexed match.
- `graph/csr.BuildFromAdjList` — freeze the live graph into the immutable CSR snapshot the query engine reads.

## Further reading

- [`graph/lpg`](../../graph/lpg) — the labelled-property-graph package documentation
- [`graph/lpg/schema`](../../graph/lpg/schema) — schema declaration and validation
- [`graph/query`](../../graph/query) — the indexed `MATCH`-style query engine
- [Example 01 — basic shortest paths](../01_basic) — the prior example, building the flow this one extends
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
