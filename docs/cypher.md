# Cypher Reference — GoGraph

GoGraph embeds a Cypher execution engine that is wire-compatible with
`neo4j-go-driver` v5 via the Bolt v5 protocol. The engine parses and executes
an openCypher-compatible dialect; it is not a full Neo4j replacement, but it
covers the core read/write/schema surface that most application workloads
require.

## Quick start

```go
import (
    "context"
    "gograph/cypher"
    "gograph/graph/adjlist"
    "gograph/graph/lpg"
)

g   := lpg.New[string, float64](adjlist.Config{})
eng := cypher.NewEngine(g)

res, err := eng.Run(context.Background(),
    "CREATE (n:Person {name: $name}) RETURN n",
    map[string]expr.Value{"name": expr.StringVal("Alice")},
)
if err != nil {
    // handle
}
defer res.Close()
for res.Next() {
    rec := res.Record() // map[string]expr.Value
    _ = rec
}
```

Write queries must use `RunInTx`; read queries may use `Run` directly.

---

## Reading data

### MATCH

Finds nodes (and, with an expand pattern, relationships) that satisfy a pattern.

```cypher
MATCH (n)
MATCH (n:Label)
MATCH (n:Label {prop: val})
MATCH (a)-[:REL]->(b)
MATCH (a)-[r:REL]->(b)
```

`MATCH` without a relationship pattern performs a node scan. With a
relationship pattern it drives an `Expand` operator from a bound start node.

**Variable-length patterns** (`[:*1..3]`) are supported via the
`VarLengthExpand` operator:

```cypher
MATCH (a)-[:KNOWS*1..3]->(b)
RETURN a, b
```

### WHERE

Filters rows produced by the preceding clause.

```cypher
MATCH (n:Person)
WHERE n.age > 30 AND n.active = true
RETURN n
```

Supported predicates:

| Operator | Example |
|---|---|
| `=` | `n.name = 'Alice'` |
| `<>` | `n.status <> 'inactive'` |
| `<`, `>`, `<=`, `>=` | `n.age >= 18` |
| `IS NULL` | `n.email IS NULL` |
| `IS NOT NULL` | `n.email IS NOT NULL` |
| `AND`, `OR`, `NOT` | `n.a = 1 AND NOT n.b IS NULL` |
| `EXISTS { MATCH … }` | `WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) }` |

The engine pushes predicates through the plan tree; filters on labelled
properties that have an index are converted to `IndexScan` operators
automatically.

### RETURN

Projects columns from the current row set.

```cypher
MATCH (n:Person)
RETURN n, n.name AS name, n.age
```

Aliases rename output columns. `RETURN *` returns all bound variables.

### WITH

Pipes the result of one query segment into the next, optionally with
aggregation or filtering.

```cypher
MATCH (n:Person)
WITH n.name AS name, count(n) AS total
WHERE total > 1
RETURN name, total
```

`WITH` is the only way to introduce aggregation in a multi-step query (see
[Aggregation](#aggregation)).

### ORDER BY / LIMIT / SKIP

```cypher
MATCH (n:Person)
RETURN n.name
ORDER BY n.name ASC
SKIP  10
LIMIT 5
```

`ORDER BY` accepts multiple expressions. `ASC` is the default; `DESC` reverses
the order. `NULL` values sort last in ascending order and first in descending
order.

`LIMIT` is fused with `ORDER BY` into a `Top` operator (O(M log N) heap)
when both appear on the same projection, which avoids materialising all M rows.

`SKIP` discards the first N rows from the child operator's output.

### DISTINCT

Eliminates duplicate rows from the result.

```cypher
MATCH (n:Person)
RETURN DISTINCT n.name
```

`DISTINCT` may appear on `RETURN` or `WITH`.

### OPTIONAL MATCH

Performs a left outer join: rows without a matching relationship pattern are
emitted with `NULL` in the unbound variables.

```cypher
MATCH    (n:Person)
OPTIONAL MATCH (n)-[:LIVES_IN]->(c:City)
RETURN   n.name, c.name
```

Currently, `OPTIONAL MATCH` supports single-hop relationship patterns via the
`OptionalExpand` operator. Multi-hop optional patterns are not yet planned.

### Aggregation

The engine supports all standard Cypher aggregate functions inside `RETURN`
and `WITH`:

| Function | Description |
|---|---|
| `count(expr)` | Number of non-null values; `count(*)` counts all rows |
| `sum(expr)` | Sum of numeric values |
| `avg(expr)` | Average of numeric values |
| `min(expr)` | Minimum value |
| `max(expr)` | Maximum value |
| `collect(expr)` | List of all non-null values |

```cypher
MATCH (n:Person)
RETURN n.city AS city, count(n) AS residents
ORDER BY residents DESC
LIMIT 10
```

The `EagerAggregation` operator is a pipeline breaker: it consumes all upstream
rows before emitting any output. The number of distinct groups is bounded by
`DefaultMaxGroups` (1 000 000). Exceeding this limit returns
`ErrAggMemoryExceeded`.

---

## Writing data

Write queries must be executed through `Engine.RunInTx` (or via an explicit
Bolt transaction when using the server). Writes inside `Run` return an error.

### CREATE

Creates one or more nodes or relationships.

```cypher
-- bare node
CREATE (n:Person)

-- node with properties
CREATE (n:Person {name: 'Alice', age: 30})

-- relationship between already-matched nodes
MATCH  (a:Person {name: 'Alice'}), (b:Person {name: 'Bob'})
CREATE (a)-[:KNOWS]->(b)
```

`CREATE` always produces a new element; it never reuses an existing one.

### MERGE

Finds an element matching the pattern; creates it if it does not exist.

```cypher
MERGE (n:Person {email: 'alice@example.com'})
```

`MERGE` is atomic with respect to the current transaction. It is equivalent to
"find or create" and is safe to retry.

### SET

Sets node properties or adds a label.

```cypher
-- set a property
MATCH (n:Person {name: 'Alice'})
SET   n.age = 31

-- add a label
MATCH (n {name: 'Alice'})
SET   n:Employee
```

### REMOVE

Removes a property or a label from a node.

```cypher
-- remove a property
MATCH (n:Person {name: 'Alice'})
REMOVE n.age

-- remove a label
MATCH (n:Employee {name: 'Alice'})
REMOVE n:Employee
```

### DELETE / DETACH DELETE

`DELETE` removes a node or relationship. A node with existing relationships
cannot be deleted unless `DETACH DELETE` is used.

```cypher
-- delete a relationship
MATCH (a)-[r:KNOWS]->(b)
DELETE r

-- delete a node and all its relationships
MATCH (n:Person {name: 'Alice'})
DETACH DELETE n
```

---

## Bulk operations

### UNWIND

Expands a list into individual rows. Used to batch-insert or iterate over
values.

```cypher
UNWIND ['Alice', 'Bob', 'Carol'] AS name
CREATE (:Person {name: name})
```

```cypher
UNWIND $items AS item
MERGE  (:Product {sku: item.sku})
SET    item.price = item.price
```

---

## Schema

### CREATE INDEX

Two syntactic forms are accepted; both create a property index on a label.

```cypher
-- legacy form
CREATE INDEX ON :Person(email)

-- named form (preferred)
CREATE INDEX person_email FOR (n:Person) ON (n.email)
```

By default, a hash index is created. The named form supports `USING BTREE`:

```cypher
CREATE INDEX person_age USING BTREE FOR (n:Person) ON (n.age)
```

A BTree index supports range queries (`<`, `>`, `<=`, `>=`, `ORDER BY`). A
hash index only supports equality lookups.

If the index already exists, the engine returns
`Neo.ClientError.Schema.IndexAlreadyExists` (via the Bolt protocol).

### DROP INDEX

```cypher
DROP INDEX person_email
DROP INDEX person_email IF EXISTS
```

`IF EXISTS` suppresses the error when the index does not exist.

### CREATE CONSTRAINT

Two constraint types are supported:

```cypher
-- uniqueness constraint
CREATE CONSTRAINT person_email_unique
    ON (n:Person) ASSERT n.email IS UNIQUE

-- not-null constraint
CREATE CONSTRAINT person_name_notnull
    ON (n:Person) ASSERT n.name IS NOT NULL
```

Both forms enforce the constraint on future writes. Existing data that violates
the constraint is not checked retroactively.

### DROP CONSTRAINT

```cypher
DROP CONSTRAINT person_email_unique
DROP CONSTRAINT person_email_unique IF EXISTS
```

---

## Built-in procedures (CALL)

Procedures are invoked with `CALL proc()` and yield one or more columns.

### db.indexes()

Returns all registered indexes.

```cypher
CALL db.indexes()
```

Yields: `name STRING`, `type STRING`

### db.constraints()

Returns all registered constraints.

```cypher
CALL db.constraints()
```

Yields: `name STRING`, `type STRING`, `label STRING`, `property STRING`

### db.labels()

Returns all distinct node labels present in the graph.

```cypher
CALL db.labels()
```

Yields: `label STRING`

### db.relationshipTypes()

Returns all distinct relationship type names present in the graph.

```cypher
CALL db.relationshipTypes()
```

Yields: `relationshipType STRING`

### db.propertyKeys()

Returns all distinct property key names present across all nodes.

```cypher
CALL db.propertyKeys()
```

Yields: `propertyKey STRING`

### db.schema.visualization()

Returns the schema as two lists: node labels and relationship types. Intended
for schema introspection tooling.

```cypher
CALL db.schema.visualization()
```

Yields: `nodes LIST`, `relationships LIST`

---

## Parameters

Use `$paramName` in a query and pass a `map[string]expr.Value` (or
`map[string]any` via `RunAny`/`RunInTxAny`) at call time.

```go
res, err := eng.Run(ctx,
    "MATCH (n:Person {name: $name}) RETURN n",
    map[string]expr.Value{"name": expr.StringVal("Alice")},
)
```

Alternatively, use the convenience wrapper:

```go
res, err := eng.RunAny(ctx,
    "CREATE (n:Person {name: $name, age: $age})",
    map[string]any{"name": "Alice", "age": 30},
)
```

`BindParams` converts native Go types to `expr.Value`; the supported
conversions are: `bool`, `int`/`int64` (and other integer widths), `float64`,
`string`, `nil`.

Parameters are type-checked at plan time against inferred parameter types. A
type mismatch returns a semantic error before execution begins.

---

## Known limitations

The following constructs parse correctly but are not yet executed, or are
not yet parsed:

| Feature | Status |
|---|---|
| `FOREACH` | Parser-level; no exec operator |
| `CALL { … }` (correlated subquery) | Not yet supported |
| Chained `WITH` (multiple `WITH … MATCH …` in one query) | Grammar gap |
| Zero-prefix float literals (`0.5`) | Lexer gap |
| Leading-dot float literals (`.5`) | Lexer gap |
| Variable-length patterns without an explicit bound (`[:*]`) | Exec gap |

For the full divergence taxonomy and pass-rate breakdown, see
[docs/tck/DIVERGENCES.md](tck/DIVERGENCES.md).

---

## See also

- [docs/bolt.md](bolt.md) — Bolt v5 server: connection, authentication, TLS
- [docs/benchmarks/cypher.md](benchmarks/cypher.md) — IC1–IC14 benchmark results
- [docs/metrics.md](metrics.md) — observability metrics exposed by the engine


---

*Last reviewed: 2026-05-22 against commit `cd97f07`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*
