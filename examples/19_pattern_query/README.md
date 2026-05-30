# Example 19 — Pattern query

## What it demonstrates

The fluent `graph/query` API: build a labelled property graph with a
declared schema, freeze it into a CSR snapshot, then run MATCH-style
pattern queries that combine **label** and **property** predicates and a
**one-hop expansion**. For every matched node the example reads back the
`age` and `dept` properties with `lpg.Graph.GetNodeProperty`, so the
output shows not just *which* nodes matched but *why* they matched.

## Domain / scenario

A tiny org graph of five people. Each `Person` node carries an `age`
(int64) and a `dept` (string); some are also tagged `Admin`. Three
directed edges model a "reports-to / follows" relation:

```
alice  Person Admin  age=30  dept=eng   -> bob, carol
bob    Person        age=25  dept=eng
carol  Person        age=30  dept=ops
dave   Person Admin  age=42  dept=eng   -> erin
erin   Person        age=28  dept=ops
```

The four queries each exercise a different combination of predicates:

- `(n:Person:Admin)` — two label predicates intersected (`alice`, `dave`).
- `(n:Person) WHERE n.age = 30` — label plus an int64 property predicate
  (`alice`, `carol`).
- `(n:Admin)-->(b)` — one-hop expansion: the targets reachable in a
  single step from an `Admin` node (`bob`, `carol`, `erin`).
- `(n:Person {dept: 'ops'})` — label plus a string property predicate
  (`carol`, `erin`).

Every result is sorted by node key, so the output is byte-stable
regardless of the engine's internal iteration order.

## How to run

```sh
go run ./examples/19_pattern_query
```

## Expected output

```
MATCH (n:Person:Admin) RETURN n.name, n.age, n.dept
  - alice  age=30  dept=eng
  - dave   age=42  dept=eng

MATCH (n:Person) WHERE n.age = 30 RETURN n.name, n.age, n.dept
  - alice  age=30  dept=eng
  - carol  age=30  dept=ops

MATCH (n:Admin)-->(b) RETURN b.name, b.age, b.dept  (one hop out)
  - bob    age=25  dept=eng
  - carol  age=30  dept=ops
  - erin   age=28  dept=ops

MATCH (n:Person {dept: 'ops'}) RETURN n.name, n.age, n.dept
  - carol  age=30  dept=ops
  - erin   age=28  dept=ops
```

## Key APIs

- `graph/lpg.New` — build the mutable labelled property graph.
- `graph/lpg/schema.New` / `Schema.RegisterLabel` / `Schema.RegisterProperty` — declare the `Person`/`Admin` labels and the `age`/`dept` property keys with their kinds.
- `graph/lpg.Graph.SetNodeLabel` / `SetNodeProperty` — populate labels and typed property values.
- `graph/lpg.Graph.GetNodeProperty` — read a matched node's `age`/`dept` back, with `PropertyValue.Int64` / `PropertyValue.String` to unwrap the typed value.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable CSR snapshot used as the query surface.
- `graph/query.New` / `Engine.Match` / `Pattern.Vertex` / `Pattern.Out` / `Pattern.Collect` — express and run the pattern queries.
- `graph/query.WithLabel` / `WithProperty` — the label and property predicates that seed each pattern.

## Further reading

- [`graph/query`](../../graph/query) — the fluent pattern-query package
- [`graph/lpg`](../../graph/lpg) — the labelled property graph and its property model
- [`graph/lpg/schema`](../../graph/lpg/schema) — schema declaration for labels and property keys
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the query surface
- [Example 02 — property graph](../02_property_graph) — introduces the labelled property graph model
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
