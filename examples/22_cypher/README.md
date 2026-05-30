# Example 22 — Cypher engine

## What it demonstrates

The GoGraph Cypher engine — the module's flagship, 100% compliant with
the openCypher TCK at the execution level. It runs four Cypher idioms
against a small social graph: a label scan with a property projection and
`ORDER BY`, a `WHERE` filter, a directed relationship pattern, and a
`CREATE` inside a write transaction. Every value is read back from the
result record and printed in human-readable form (names and ages), never
as a raw node ID.

## Domain / scenario

A tiny social graph of five people. Each `Person` node carries a `name`
and an `age`; directed relationships connect them:

```
(Alice {age:30}) -[:KNOWS]->   (Bob {age:25})
(Bob   {age:25}) -[:KNOWS]->   (Carol {age:35})
(Carol {age:35}) -[:KNOWS]->   (Dave {age:28})
(Dave  {age:28}) -[:KNOWS]->   (Eve {age:22})
(Alice {age:30}) -[:FRIENDS]-> (Carol {age:35})
```

The relationship query asks only for `KNOWS` edges, so the single
`FRIENDS` edge is filtered out by the pattern. The `WHERE` query keeps
only people older than 25. Finally a `CREATE` adds a `Guest` node named
Frank in its own transaction, and the example confirms the new label is
registered in the graph.

## How to run

```sh
go run ./examples/22_cypher
```

## Expected output

```
MATCH (n:Person) RETURN n.name AS name ORDER BY name
  Alice
  Bob
  Carol
  Dave
  Eve

MATCH (n:Person) WHERE n.age > 25 RETURN n.name AS name, n.age AS age ORDER BY name
  Alice age 30
  Carol age 35
  Dave  age 28

MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS from, b.name AS to ORDER BY from, to
  Alice KNOWS Bob
  Bob KNOWS Carol
  Carol KNOWS Dave
  Dave KNOWS Eve

CREATE (n:Guest {name: "Frank"})
  created Guest{name: "Frank"} — label registered in graph
```

Every query that can return more than one row carries an `ORDER BY`, so
the output is byte-stable and pinned by `example_test.go`.

## Cypher clauses exercised

- `MATCH (n:Person)` — a label scan over the `Person` nodes.
- `RETURN n.name AS name` — projection of a node property under an alias.
- `ORDER BY name` — deterministic ascending ordering of the result rows.
- `WHERE n.age > $min` — predicate filtering, with the threshold passed
  as a query parameter.
- `MATCH (a:Person)-[:KNOWS]->(b:Person)` — a directed relationship
  pattern restricted to the `KNOWS` type.
- `CREATE (n:Guest {name: "Frank"})` — a write transaction adding a
  labelled node with an inline property map.

## Key APIs

- `graph/lpg.New` / `Graph.AddNode` / `SetNodeLabel` / `SetNodeProperty` /
  `AddEdge` / `SetEdgeLabel` — build the labelled property graph.
- `cypher.NewEngine` — bind the graph to a query engine.
- `cypher.Engine.Run` — execute a read query (the three `MATCH` queries).
- `cypher.Engine.RunInTx` — execute a write query (`CREATE`) atomically.
- `cypher.Result` — forward-only streaming result set (`Next`, `Record`,
  `Err`, `Close`).
- `cypher/expr.StringValue` / `expr.IntegerValue` — the runtime value
  types the engine returns for projected `string` and integer properties;
  the example unwraps them to bare Go `string` / `int64` for printing.

## Further reading

- [`cypher`](../../cypher) — the Cypher engine package documentation
- [`cypher/expr`](../../cypher/expr) — the runtime value model returned in result records
- [`graph/lpg`](../../graph/lpg) — the labelled property graph used as the query surface
- [Example 24 — social network CLI](../24_social_network_cli) — an interactive CLI running Cypher over a persistent LPG
- [Example 25 — software house API](../25_software_house_api) — a REST API serving Cypher queries
- [docs/cypher.md](../../docs/cypher.md) — the Cypher engine design note
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
