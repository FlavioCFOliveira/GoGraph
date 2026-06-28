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
    "github.com/FlavioCFOliveira/GoGraph/cypher"
    "github.com/FlavioCFOliveira/GoGraph/cypher/expr"
    "github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
    "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

g   := lpg.New[string, float64](adjlist.Config{})
eng := cypher.NewEngine(g)

res, err := eng.RunInTx(context.Background(),
    "CREATE (n:Person {name: $name}) RETURN n",
    map[string]expr.Value{"name": expr.StringValue("Alice")},
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

A single `Engine` is safe for concurrent use: any number of `Run` readers may
execute alongside concurrent `RunInTx` writers. Both the physical-plan build
and execution run under the graph's visibility barrier, so a writer that grows
the node space can never tear a concurrent reader's plan build, and readers
never observe a partially-applied write transaction. When the engine is backed
by a WAL store, concurrent `RunInTx` calls serialise on the store's single
writer. The returned `Result` is not safe for concurrent use.

To classify a query as read or write without running it (for example, to route
writers to `RunInTx`), call `cypher.QueryHasWritingClause(query)`; this is the
same textual heuristic `RunAny`/`RunInTxAny` use to dispatch.

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

A relationship-type disjunction matches an edge of any listed type, in any
traversal direction:

```cypher
MATCH (a)-[:KNOWS|FOLLOWS*1..3]-(b)
RETURN b
```

**Shortest paths.** `shortestPath(...)` returns one minimum-hop path (an
arbitrary one when several are tied); `allShortestPaths(...)` returns every
minimum-hop path. Both wrap a single relationship pattern between two node
patterns and bind the result to a path variable. The endpoints are normally
already bound by a preceding clause:

```cypher
MATCH (a:Person {id: 1}), (b:Person {id: 2})
MATCH p = shortestPath((a)-[:KNOWS*]-(b))
RETURN length(p), [r IN relationships(p) | type(r)]

MATCH (a:Person {id: 1}), (b:Person {id: 2})
MATCH p = allShortestPaths((a)-[*]-(b))
RETURN p
```

The hop count is the only metric (unweighted). The lower hop bound must be
`0` or `1`. When no path exists, `MATCH` produces no row and `OPTIONAL MATCH`
produces one row with the path variable set to `null`. Over a multigraph,
each relationship in the returned path reports its own type and properties
(parallel edges of different types are not collapsed).

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

`OPTIONAL MATCH` supports both single-hop and multi-hop relationship patterns:
the optional segment is planned as an `OptionalApply` operator that drives the
inner pattern per outer row and NULL-extends the unbound variables when no match
is found.

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

Creates a property index on a label. The index name is optional; when omitted
it is derived as `<label>_<property>_<type>`.

```cypher
-- named
CREATE INDEX person_email FOR (n:Person) ON (n.email)

-- unnamed (name derived automatically)
CREATE INDEX FOR (n:Person) ON (n.email)

-- idempotent
CREATE INDEX IF NOT EXISTS person_email FOR (n:Person) ON (n.email)
```

By default a hash index is created. A BTree index is selected with an `OPTIONS`
clause:

```cypher
CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType: 'btree'}
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

Returns every distinct node label currently in use — that is, attached to at
least one live node. Labels are reported whether or not an index exists for
them, and a label disappears from the list once its last bearing node is
deleted. The order is unspecified.

```cypher
CALL db.labels()
```

Yields: `label STRING`

### db.relationshipTypes()

Returns every distinct relationship type currently in use — that is, attached
to at least one live relationship. The order is unspecified.

```cypher
CALL db.relationshipTypes()
```

Yields: `relationshipType STRING`

### db.propertyKeys()

Returns every distinct property key currently in use across nodes **and**
relationships. The order is unspecified.

> **Divergence from Neo4j.** Neo4j's `db.propertyKeys()` lists property-key
> tokens from the token store, which are never garbage-collected, so it keeps
> reporting a key even after the last node or relationship using it is deleted.
> GoGraph instead reports only the property keys currently in use: a key that
> no live element bears is not listed. This difference is observable but is not
> an openCypher-conformance issue — the `db.*` introspection procedures are not
> covered by the openCypher TCK.

```cypher
CALL db.propertyKeys()
```

Yields: `propertyKey STRING`

### db.schema.visualization()

Intended to return the schema as two lists (node labels and relationship
types) for schema introspection tooling.

> **Not yet implemented.** This procedure is registered but currently returns
> an empty result set. It is documented here for forward compatibility; do not
> rely on its output until it is implemented.

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

`RunAny`/`RunInTxAny` dispatch to `Run` or `RunInTx` automatically based on
whether the query contains a writing clause.

`BindParams` converts native Go types to `expr.Value`. The supported
conversions are: `nil` (→ `expr.Null`), `bool`, every signed and unsigned
integer width (`int`, `int8`…`int64`, `uint`…`uint64`; unsigned values are
truncated to `int64`), `float32`/`float64`, `string`, `[]any` (recursively),
`map[string]any` (recursively), and any `expr.Value` (passed through
unchanged). Other types return an error.

Parameters are type-checked at plan time and a type mismatch returns a
`*sema.ParamTypeError` before execution begins. Inference is index-aware: a
property-vs-parameter equality (`n.prop = $p`) is typed from the index that
backs `n.prop` when one exists — an integer-keyed index proves an `Integer`
parameter, a string-keyed index a `String` parameter. Absent a matching index
the inference defaults to `String`. This means an integer parameter is accepted
on an integer-property index seek, while a string parameter against an
integer-keyed index is rejected.

---

## Explicit transactions

`RunInTx` is autocommit: each call is its own transaction. To span **several
statements** in one all-or-nothing transaction, open an explicit transaction
with `Engine.BeginTx`:

```go
func (e *Engine) BeginTx(ctx context.Context) (*cypher.ExplicitTx, error)
```

`BeginTx` acquires the engine's writer serialisation (the WAL store's
single-writer lock when the engine is WAL-backed, the engine writer mutex when
it is store-less) and holds it for the lifetime of the returned handle. If `ctx`
is already cancelled or its deadline has elapsed, `BeginTx` returns promptly
without acquiring any lock, wrapping the context error.

The returned `*ExplicitTx` exposes:

| Method | Signature | Purpose |
|---|---|---|
| `Exec` | `Exec(query string, params map[string]expr.Value) (*Result, error)` | Run one statement; its writes accumulate in the transaction. |
| `ExecAny` | `ExecAny(query string, params map[string]any) (*Result, error)` | As `Exec`, converting native Go params via `BindParams`. |
| `Commit` | `Commit() error` | Make every accumulated write durable and visible, then release the writer serialisation. |
| `Rollback` | `Rollback() error` | Unwind every accumulated write, then release the writer serialisation. |

The caller MUST finish the handle with exactly one `Commit` or `Rollback`. Until
then the writer serialisation is held and concurrent writers block (write-write
isolation). Each `Exec` applies its writes eagerly to the in-memory graph and
records the inverse into a transaction-wide undo log; `Commit` fsyncs the WAL
**once** for the whole transaction (durable-then-visible) and discards the undo
log, while `Rollback` replays the undo log in reverse to restore the
pre-transaction state.

```go
tx, err := eng.BeginTx(ctx)
if err != nil {
    // handle
}
if _, err := tx.Exec(
    "CREATE (n:Person {name: $name})",
    map[string]expr.Value{"name": expr.StringValue("Alice")},
); err != nil {
    _ = tx.Rollback()
    // handle
}
if _, err := tx.Exec(
    "CREATE (n:Person {name: $name})",
    map[string]expr.Value{"name": expr.StringValue("Bob")},
); err != nil {
    _ = tx.Rollback()
    // handle
}
if err := tx.Commit(); err != nil {
    // both CREATEs were rolled back; handle the durability error
}
```

Notes on behaviour, all enforced by the implementation:

- **DDL is rejected inside a transaction.** A `CREATE`/`DROP INDEX` or
  `CREATE`/`DROP CONSTRAINT` statement returns an error from `Exec`; schema
  changes are not transactional and must be issued outside an explicit
  transaction (autocommit).
- **Read statements are permitted** and observe the transaction's current
  state.
- A statement that raises a runtime error is returned directly from `Exec`,
  wrapped in `*cypher.ErrStatementPipeline`; the partial writes remain in the
  undo log, so the caller decides whether to `Rollback`.
- After `Commit` or `Rollback` the handle is finished; any further `Exec`,
  `Commit`, or `Rollback` returns `cypher.ErrTxFinished`.
- If the `Commit` WAL fsync fails, the transaction is rolled back and the fsync
  error is returned: a transaction whose durability cannot be guaranteed is
  reported as failed, never acknowledged.

**Concurrency contract.** An `ExplicitTx` is **not** safe for concurrent use: it
is owned by a single caller and its methods must be called in sequence. Distinct
`ExplicitTx` handles — and an `ExplicitTx` running alongside autocommit
`RunInTx` calls on the same engine — are safe to use concurrently; they
serialise on the writer mutex. `Closing` a `Result` returned by `Exec` releases
only that result's iterator state; it never commits or rolls the transaction
back.

This API is the engine substrate for the Bolt `BEGIN` / `RUN` / `COMMIT` /
`ROLLBACK` protocol (see [docs/bolt.md](bolt.md)).

### Read-only explicit transactions

For read-only work, prefer `Engine.BeginReadTx`. It opens an explicit
transaction that acquires **neither** the writer serialisation, **nor** the
visibility barrier, **nor** a WAL transaction, so it never serialises behind, or
blocks, a concurrent writer — roughly doubling concurrent read throughput.

```go
func (e *Engine) BeginReadTx(ctx context.Context) (*cypher.ExplicitTx, error)
```

It returns the same `*ExplicitTx` handle, with these differences from `BeginTx`:

- **Writes are rejected before execution.** A statement containing a writing
  clause, or any DDL, is rejected with the exported sentinel
  `cypher.ErrWriteInReadOnlyTx` **before** it runs. This guard is what keeps the
  lock-free read path safe: a write would otherwise execute with no writer lock,
  no barrier, and no WAL.
- **Read-committed isolation across statements.** Each `Exec` takes its own
  per-statement snapshot, so reads observe the latest committed state across the
  statements of the transaction (matching Neo4j's default), rather than a single
  snapshot for the whole transaction.
- **Teardown-only finish.** The caller must still finish the handle with exactly
  one `Commit` or `Rollback`; on a read-only handle both are no-ops, because
  nothing was acquired.

```go
tx, err := eng.BeginReadTx(ctx)
if err != nil {
    // handle
}
defer tx.Rollback() // teardown-only no-op for a read-only handle

res, err := tx.Exec("MATCH (n:Person) RETURN n.name", nil)
if err != nil {
    // handle
}
_ = res

if _, err := tx.Exec("CREATE (:Person)", nil); errors.Is(err, cypher.ErrWriteInReadOnlyTx) {
    // expected: writes are not permitted on a read-only transaction
}
```

Over the Bolt protocol, `BEGIN` with `mode="r"` routes through `BeginReadTx`, and
`ErrWriteInReadOnlyTx` maps to `Neo.ClientError.Request.Invalid`
(see [docs/isolation-design.md](isolation-design.md) and [docs/bolt.md](bolt.md)).

---

## Resource budgets

A single `Run` / `RunInTx` (and each `ExplicitTx.Exec`) materialises its result
under the graph's visibility barrier. To stop an unintentional whole-graph scan
or Cartesian-product query from exhausting memory, the engine applies **finite
default caps** to every result. These caps are configured through
`cypher.EngineOptions` and wired by `cypher.NewEngineWithOptions`.

| Option | Default constant | Default value | Unbounded sentinel |
|---|---|---|---|
| `MaxResultRows` | `DefaultMaxResultRows` | `10_000_000` rows | `MaxResultRowsUnlimited` (`-1`) |
| `MaxResultBytes` | `DefaultMaxResultBytes` | `1 << 30` (1 GiB) | `MaxResultBytesUnlimited` (`-1`) |
| `MaxCollectItems` | `funcs.DefaultMaxCollectItems` | `10_000_000` items | `MaxCollectItemsUnlimited` (`-1`) |

For every option the zero value (the default) selects the corresponding finite
`Default*` cap, a positive value overrides it, and the `-1` sentinel disables
the cap entirely.

- **`MaxResultRows`** limits the number of rows a single call may materialise.
  When exceeded, `Result.Next` returns `false` and `Result.Err` reports
  `cypher.ErrResultRowsExceeded`.
- **`MaxResultBytes`** is a coarse aggregate-byte budget complementing the row
  cap: a handful of rows carrying very large values (a node with megabyte-scale
  string properties) can dwarf a high row count. The estimate is intentionally
  cheap (`O(columns)` per row, no allocation, no serialisation). When the
  cumulative estimated encoded size exceeds the budget, `Result.Err` reports
  `cypher.ErrResultBytesExceeded`.
- **`MaxCollectItems`** bounds the number of values a single buffering
  aggregator — `collect()`, `collect(DISTINCT …)`, `percentileCont()`,
  `percentileDisc()` — retains in one group. When exceeded the aggregator
  returns `funcs.ErrCollectItemsExceeded`, surfaced through `Result.Err`.

```go
eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
    MaxResultRows:   1_000_000,                  // override the default cap
    MaxResultBytes:  cypher.MaxResultBytesUnlimited, // opt out of the byte budget
    MaxCollectItems: cypher.MaxCollectItemsUnlimited, // opt out of the collect cap
})
```

**Behavioural implication.** Because the defaults are *finite*, a query that
previously appeared to stream an unbounded result now stops with a typed error
once it crosses `DefaultMaxResultRows` (or `DefaultMaxResultBytes`, or
`funcs.DefaultMaxCollectItems`). A caller that genuinely needs an unbounded
result must opt out explicitly with the relevant `-1` sentinel, and must then
bound memory by another means (for example, streaming and closing the `Result`
promptly), because an unbounded `MATCH` otherwise materialises every row under
the graph's visibility barrier. All three caps trip **inside** the barrier
during materialisation, before the surplus reaches the caller.

---

## Known limitations

The following constructs are not yet supported:

| Feature | Status |
|---|---|
| `FOREACH` | Not parsed; rejected at parse time |
| `CALL { … }` standalone subquery clause | Not parsed; rejected at parse time |
| `CALL { … } IN TRANSACTIONS` | Not supported |

`EXISTS { … }`, `COUNT { … }`, and `COLLECT { … }` subquery *expressions* are
supported (see [WHERE](#where) and [Aggregation](#aggregation)); only the
standalone `CALL { … }` subquery *clause* is unsupported.

The openCypher TCK execution suite is fully green: all 3897 scenarios pass
(100%), enforced by `tckExecutionBaseline` in `cypher/tck/runner_test.go`. For
the full divergence taxonomy, see [docs/tck/DIVERGENCES.md](tck/DIVERGENCES.md).

### String + number concatenation

The `+` operator concatenates only when **both** operands are strings. A mixed
string + number expression returns `null` rather than implicitly coercing the
number to text:

```cypher
RETURN 'a' + 1                   // → null
RETURN 'count: ' + 5             // → null
RETURN 1 + '2'                   // → null
RETURN 'count: ' + toString(5)   // → 'count: 5'
```

openCypher deliberately leaves implicit `string + number` coercion
underspecified (openCypher issue #189 notes the conversion is internally
inconsistent), and the manual steers users to `toString()`. GoGraph therefore
requires an explicit `toString()` for mixed concatenation, mirroring the
`date()`-returns-`null`-on-invalid-input choice. This is pinned by a regression
test so the behaviour cannot drift silently.

### Element identity: `id()` and `elementId()`

`id()` returns an integer identifier for a node or relationship, with an
important stability asymmetry between the two:

- **Node `id()` is stable across a store reopen** — it is the node's interned
  `NodeID`, persisted via the snapshot/WAL mapper, so a node resolves to the
  same `id()` after recovery.
- **Relationship `id()` is stable only *within* a single graph snapshot** — it
  is the relationship's positional index in the current CSR adjacency (the same
  value the engine uses as the relationship-isomorphism key to reject a repeated
  edge within a query). It is **not** guaranteed to survive a store reopen or a
  CSR rebuild (for example, a relationship delete can renumber positions). Do
  **not** persist a relationship `id()` and expect to resolve the same
  relationship after a restart.
- **`elementId()` is not implemented.**

Both values are valid identifiers *within* a query — `id(r)` is unique per
relationship in a result and consistent whether the edge is traversed forwards
or backwards. openCypher treats the concrete `id()` value, and its cross-reopen
stability, as implementation-defined; the TCK does not constrain it.

---

## See also

- [docs/bolt.md](bolt.md) — Bolt v5 server: connection, authentication, TLS
- [docs/benchmarks/cypher.md](benchmarks/cypher.md) — IC1–IC14 benchmark results
- [docs/metrics.md](metrics.md) — observability metrics exposed by the engine

---

## Release-time documentation checklist

At each release, re-review the four reference documents against the `CHANGELOG`
"Added" and "Changed" sections so that no behaviour-changing feature ships
undocumented:

- [docs/persistence.md](persistence.md)
- [docs/cypher.md](cypher.md) (this document)
- [docs/bolt.md](bolt.md)
- [docs/test-battery.md](test-battery.md)

Pay particular attention to changes that alter *observed behaviour* — new APIs,
new default limits, and new typed errors — since a user who hits such a change
consults the reference first.


---

*Last reviewed: 2026-06-28 against commit `3b51a5b`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*
