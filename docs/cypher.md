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

// Multigraph: true is required — openCypher's data model is a multigraph, so a
// CREATE always adds a relationship (including a parallel edge between an
// existing node pair). Constructing the engine over a non-multigraph graph makes
// such a CREATE fail with cypher.ErrParallelEdgeInSimpleGraph.
// Directed: true is required too — openCypher relationships are directed,
// whereas the zero value mirrors every edge back to its source.
// Weightless: true drops the per-node edge-weight column, which carries no
// information for the engine (see "Graph configuration" below).
g   := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true, Weightless: true})
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

### Graph configuration

All three `adjlist.Config` fields in the quick start are deliberate, and the
first two are required for openCypher semantics rather than merely advisable:

- `Multigraph: true` — openCypher's data model is a multigraph, so two `CREATE`
  statements between the same ordered pair must yield two relationships. On a
  simple graph the second one instead fails with
  `cypher.ErrParallelEdgeInSimpleGraph`. This is the field most easily missed:
  `adjlist.Config{}` is *not* a multigraph.
- `Directed: true` — openCypher relationships are directed. On an undirected
  graph `AddEdge` also inserts the reverse edge, so the stored adjacency no
  longer matches the direction the query wrote.
- `Weightless: true` — Cypher has no edge-weight concept: the engine records the
  zero weight for every relationship, and the one path that reads a weight back
  (transaction undo, restoring an edge a rolled-back write had removed) therefore
  only ever reads that same zero. The per-node `[]float64` weight column carries
  no information, and dropping it removes one heap object per node that has
  outgoing relationships — measured at 32 B/node for a degree-4 graph and
  64 B/node for degrees 5–8, the geometric capacity buckets the column is
  allocated in.

The engine's own openCypher TCK harness builds its graph with `Directed: true,
Multigraph: true` for exactly these reasons.

`Weightless` is the only one of the three that is an optimisation rather than a
requirement, and it carries one exclusion: do not set it on a graph you also
intend to query with a weight-consuming `search/` algorithm (Dijkstra, A\*,
Bellman-Ford, and the rest), which would then see every edge as weight 0. See
`adjlist.Config.Weightless` for that contract in full.

---

## Reading data

### MATCH

Finds nodes (and, with an expand pattern, relationships) that satisfy a pattern.

```cypher
MATCH (n) RETURN n

MATCH (n:Person) RETURN n

MATCH (n:Person {name: 'Alice'}) RETURN n

MATCH (a)-[:KNOWS]->(b) RETURN a, b

MATCH (a)-[r:KNOWS]->(b) RETURN r
```

`MATCH` without a relationship pattern performs a node scan. With a
relationship pattern it drives an `Expand` operator from a bound start node.

**Bound destinations use a seek, shown as `ExpandInto`.** When a hop's destination
variable is *already* bound — a pattern that closes a cycle, a triangle, or detects
a mutual relationship — the operator does not enumerate the source's neighbours and
discard the ones that miss. The CSR neighbour run is ordered by destination, so it
binary-searches the bound destination's contiguous run and walks only the matching
edges: `O(log d + r)` for multiplicity `r`, instead of `Θ(d)`.

```cypher
MATCH (a:Person)-[:KNOWS]->(b:Person)-[:KNOWS]->(a) RETURN a, b
```

`EXPLAIN` renders such a hop as `Expand [ExpandInto seek]`, distinguishing it from
an ordinary `Expand`. The name is what openCypher implementations conventionally
call this access path; it appears as the operator's *detail* rather than its name
because a rendered physical name is the concrete operator type and must stay
incapable of disagreeing with the operator that ran.

**Cyclic patterns can fuse two hops into one `ExpandIntersect` (opt-in).** A
directed cycle's last two hops — the open middle expand and the closing seek —
together compute `N_out(b) ∩ N_in(a)`, but by materialising the left operand in full
first. `exec.ExpandIntersect` computes the same set directly with a sorted-set
intersection over the destination-ordered runs, so a candidate that does not close the
cycle is never built into a row. `EXPLAIN` renders it as
`ExpandIntersect [mid=<type> close=<type>]`.

```cypher
// Fuses: the closing hop sits directly on the open middle hop.
MATCH (a)-[:KNOWS]->(b)-[:KNOWS]->(c)-[:KNOWS]->(a) RETURN count(*)
```

It is **opt-in** via `EngineOptions.EnableCyclicIntersect`, which has *positive*
polarity — unlike every `Disable*` option — so the zero value leaves it off. Two
limitations are worth knowing before enabling it:

- **A node-label predicate prevents the fusion.** Labels interpose a `Selection`
  between the two hops, so `(a:Person)-[:KNOWS]->(b:Person)-…` correctly declines and
  runs today's plan. Where the relationship type already constrains the endpoints,
  dropping the labels is often semantically equivalent and lets the operator engage.
- **Only the last hop of a cycle fuses.** A square or a longer cycle gains the same
  single intersection as a triangle, not one per hop, because every simple cycle has
  exactly one vertex with two already-bound neighbours.

Undirected legs, variable-length legs and patterns under `PROFILE` all decline. The
operator is result- and order-identical when it engages: it yields candidates in
ascending order and walks each leg's contiguous handle run in position order, so the
emitted sequence matches the two hops it replaces exactly. Measurements and the
default-flip recommendation are in
[`benchmarks/cyclic-join-2026-07-30.md`](benchmarks/cyclic-join-2026-07-30.md);
the design and its semantic obligations are in
[`design-wcoj-cyclic-patterns.md`](design-wcoj-cyclic-patterns.md).

The seek emits exactly the rows the enumerate-and-filter path did, **in the same
order** — the slots sharing a destination are contiguous, so the block it seeks to
is precisely the subsequence the filter would have emitted. It also preserves
per-relationship-instance identity across parallel edges: one row per relationship,
each with its own identity, never one row per neighbour. Where the bound cell cannot
be resolved to a node — a `NULL` from an `OPTIONAL MATCH`, for instance — the
operator falls back to the full range, so the result is never narrowed by a seek that
could not decide. Variable-length expands, `shortestPath` and `allShortestPaths` take
their own operators and are unaffected.

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

#### Subquery expression bodies

`EXISTS { … }` and `COUNT { … }` accept three body forms:

| Body | Example |
|---|---|
| A bare pattern | `EXISTS { (n)-[:KNOWS]->(m) }` |
| Reading clauses without a `RETURN` | `EXISTS { MATCH (n)-[:KNOWS]->(m) }` |
| A full query with a `RETURN` | `EXISTS { MATCH (n)-[:KNOWS]->(m) RETURN m }` |

The `RETURN` is optional because the openCypher grammar makes the trailing
result statement optional. A body may chain several reading clauses
(`MATCH`, `UNWIND`, `CALL`), and an inner `WHERE` is applied in every form:

```cypher
MATCH (n:Person)
WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) WHERE m.age > 30 }
RETURN n.name
```

That includes the **bare-pattern** body, where the `WHERE` follows the pattern
directly, and it applies to `COUNT { … }` on the same terms as `EXISTS { … }`:

```cypher
MATCH (n:Person)
RETURN n.name, COUNT { (n)-[:KNOWS]->(m) WHERE m.age > 30 } AS olderFriends
```

`EXISTS { … }` is a read-only existence check: an updating clause in the body
is rejected at compile time with `InvalidClauseComposition`.

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
// bare node
CREATE (n:Person)

// node with properties
CREATE (n:Person {name: 'Alice', age: 30})

// relationship between already-matched nodes
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
// set a property
MATCH (n:Person {name: 'Alice'})
SET   n.age = 31

// add a label
MATCH (n {name: 'Alice'})
SET   n:Employee
```

### REMOVE

Removes a property or a label from a node.

```cypher
// remove a property
MATCH (n:Person {name: 'Alice'})
REMOVE n.age

// remove a label
MATCH (n:Employee {name: 'Alice'})
REMOVE n:Employee
```

### DELETE / DETACH DELETE

`DELETE` removes a node or relationship. A node with existing relationships
cannot be deleted unless `DETACH DELETE` is used.

```cypher
// delete a relationship
MATCH (a)-[r:KNOWS]->(b)
DELETE r

// delete a node and all its relationships
MATCH (n:Person {name: 'Alice'})
DETACH DELETE n
```

### FOREACH

`FOREACH (x IN list | <updating clauses>)` binds `x` to each element of a
list in turn and runs the body's updating clauses as side-effects. It does
**not** change the surrounding query's row cardinality: the outer row is
forwarded unchanged after the body has run once per list element.

The body accepts the updating clauses `CREATE`, `MERGE`, `SET`, `REMOVE`,
`DELETE`, and a nested `FOREACH`. The list may be a literal, a bound list
variable, or any list-valued expression.

```cypher
// create a chain of nodes from a literal list
FOREACH (name IN ['Alice', 'Bob', 'Carol'] |
  CREATE (:Person {name: name})
)
```

```cypher
// update every node bound along a matched path
MATCH p = (start:Person {name: 'Alice'})-[:KNOWS*]->(dest:Person)
FOREACH (n IN nodes(p) |
  SET n.visited = true
)
```

The variable `x` is scoped to the `FOREACH` body and is not visible after
the clause. A `FOREACH` may appear on its own or after a `WITH`, where it
keeps its document order among the surrounding clauses.

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
MERGE  (p:Product {sku: item.sku})
SET    p.price = item.price
```

---

## Schema

### CREATE INDEX

Creates a property index on a label. The index name is optional; when omitted
it is derived as `<label>_<property>_<type>`.

```cypher
// named
CREATE INDEX person_email FOR (n:Person) ON (n.email)

// unnamed (name derived automatically)
CREATE INDEX FOR (n:Person) ON (n.email)

// idempotent
CREATE INDEX IF NOT EXISTS person_email FOR (n:Person) ON (n.email)
```

By default a hash index is created. A BTree index is selected with an `OPTIONS`
clause:

```cypher
CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType: 'btree'}
```

Which predicates each kind accelerates depends on the **type** of the property
value, because the two kinds are keyed differently:

| Predicate | String value | Numeric value (integer or float) |
|---|---|---|
| `=` | `hash` | either kind |
| `<` `>` `<=` `>=` | `btree` | either kind |
| `STARTS WITH` | `btree` | — |
| `ENDS WITH`, `CONTAINS` | never indexed | — |

A hash index is keyed on strings, so of the string predicates it accelerates
equality only; a string range needs a BTree index.

**A prefix is a range.** `n.p STARTS WITH 'abc'` selects exactly the keys in the
half-open interval `['abc', 'abd')`, so on a BTree index it is served by the same
range seek as `>=` / `<`, under the same cost gate. Only the prefix form
qualifies: `ENDS WITH` and `CONTAINS` do not describe an interval of the key
order — with `"ax" < "b" < "bx"` the outer two match a suffix the middle one does
not — so the narrowest sound interval for them is the whole index, which carries
no selectivity and is never worth seeking. Serving those would need a different
structure (a suffix or n-gram index), which GoGraph does not have.

The rewrite applies to `n.p STARTS WITH <literal>` only. A negated prefix
(`NOT n.p STARTS WITH 'abc'`) selects the *complement* of the interval and is
answered by a scan, as is a disjunction of prefixes, a prefix operand supplied as
a parameter, and the mirrored form `'abc' STARTS WITH n.p` — which asks a
different question, whether the literal begins with the property's value.

**Numeric properties are served by either kind.** Every index — hash or BTree —
carries an internal numeric companion keyed on a unified `float64` order, so a
numeric equality and a numeric range both reach an index whichever kind you
created. Equality is served as the degenerate closed range `[v, v]`. It is
matched across the integer/float boundary as openCypher requires (`5` matches a
stored `5.0`), and remains exact above 2<sup>53</sup>, where distinct integers
share a floating-point image.

That means the default `CREATE INDEX` is the right choice for a numeric
property; `OPTIONS {indexType: 'btree'}` is needed only for a **string** range.
Before this was so, a default index on a numeric property could hold no entries
at all while still reporting `state: "ONLINE"`, and the query silently scanned.

Index use is a cost decision, not a guarantee: the engine seeks only when the
label holds at least 1024 nodes and the predicate matches at most 10 % of them,
and it scans otherwise. Use `Engine.Explain` to see which access path a query
gets.

**Which key forms reach the index.** The key may be written inline or bound by a
preceding `WITH`, provided its value is the same on every row:

```cypher
// all three seek
MATCH (a:Person {email: 'a@example.com'}) RETURN a

MATCH (a:Person {email: $email})          RETURN a

WITH $email AS k MATCH (a:Person {email: k}) RETURN a
```

A key **set** written as a list literal also seeks, with one probe per distinct
key merged into a single result:

```cypher
UNWIND ['a@example.com', 'b@example.com'] AS k MATCH (a:Person {email: k}) RETURN a
```

Duplicate keys in the list cost nothing extra — they are deduplicated before
probing — and a `null` or type-incompatible element simply contributes no rows
rather than disabling the seek.

A key bound to something that varies per row does **not** seek, and scans
instead: a key drawn from the graph (`MATCH (q:Q) WITH q.email AS k …`), or a key
list supplied at runtime (`UNWIND $keys AS k …`), whose elements cannot be
enumerated when the plan is built. The distinction is row-invariance, not syntax:
a `WITH`-bound literal or parameter, and the elements of a literal list, are all
known before the first row, whereas a data-derived key would need the engine to
drain its input before probing.

The seek is chosen for the same reasons in all cases, so it can also decline: a
key whose type the index cannot serve (an integer against a string-keyed hash
index) falls back to a scan with the original predicate as the filter, and
returns the rows openCypher requires either way. A key **set** is additionally
cost-gated on its exact merged posting count — a set covering more than 10 % of
the label is answered by a scan, because probing that many keys costs more than
the scan it would replace.

A multi-label pattern combined with a property equality — `MATCH (a:A:B {p: v})` —
does not reach the **property** index, for any key form. The labels are still
served set-at-a-time (see below) and the property equality remains a filter over
the rows the label conjunction admits.

**Conjunctions of indexed properties compose.** `WHERE n.a > 1 AND n.b < 9` uses
**both** indexes: each conjunct's index yields a bitmap and the planner intersects
them, so only the rows satisfying both are materialised. This is what other engines
need a dedicated *composite index* type for; GoGraph composes **any two ordinary
single-property indexes** — including across kinds, a numeric index with a string
one — so nothing has to be declared in advance for a particular pair of properties.

Each conjunct must independently clear the same cost gate a lone range predicate
does, so a conjunct matching most of the label is left as a filter rather than
probed; and because a range index yields a *superset*, the original predicate is
still applied to every surviving row. A conjunct on an unindexed property, a
disjunction, and a negation are never composed.

**Multi-label patterns are answered as set intersections.**
`MATCH (n:A:B)` is a set intersection, and GoGraph answers it as one. Labels are
stored as Roaring bitmaps, so the conjunction is a single k-way AND of those
bitmaps rather than a scan of one label with the rest re-checked on every row.

The plan renders as a `NodeByLabelScan` whose detail names the intersected labels
in the order they are ANDed — `[Small∩Big]` — which is ascending cardinality,
because the smallest bitmap is the cheapest to copy.

Two consequences worth knowing:

- **No residual label filter.** The intersected bitmap already encodes the
  conjunction, and the label index is maintained on delete and on relabel, so the
  rows it yields are exactly the matching live nodes. Any *other* predicate in the
  pattern (a property, a `WHERE`) is of course still applied.
- **The conjunction is decided atomically.** The AND runs under a single read-lock
  over the label index, so the whole conjunction reflects one consistent image
  rather than each label being checked at a different moment.

It is a cost decision like every other access path. The intersection is used when
it scans **strictly fewer** rows than the smallest single label would; when one
label is entirely contained in the others there are no rows left to remove, so the
engine keeps the ordinary plan. A label with no members short-circuits the whole
conjunction to an empty scan. Use `Engine.Explain` to see which applies.

All of this is a transparent optimisation: a query using an index returns
exactly the same rows as the same query with no index (a residual filter refines
any over-returned superset). An index never changes ordering — it is not used to
satisfy `ORDER BY`, which is always evaluated by a separate sort operator.

**Dialect and scope.** GoGraph's index kinds are `hash` (equality) and `btree`
(range), selected via `OPTIONS {indexType: …}`; this differs from Neo4j, whose
index types are `RANGE`/`TEXT`/`POINT`/`LOOKUP`/`FULLTEXT`/`VECTOR`. Indexes
cover a single **node** property (`FOR (n:Label) ON (n.prop)`); composite
(multi-property) and relationship-property indexes are not supported and are
rejected with an error.

**Composite indexes are deliberately absent, not missing.** A conjunction over two
indexed properties is answered by intersecting the two single-property indexes'
bitmaps (above), which covers what a composite index is normally declared for —
without asking the user to predict which pairs of properties will be queried
together, and without a second index type to build, maintain and recover. The
trade is that a composite index can also serve an ordered prefix scan over the
combined key, which an intersection cannot; GoGraph does not use indexes to satisfy
`ORDER BY` at all, so that capability would have nothing to attach to today.

These are deliberate scope boundaries: the openCypher
TCK does not cover index DDL, so the dialect does not affect conformance.

If the index already exists, the engine returns
`Neo.ClientError.Schema.IndexAlreadyExists` (via the Bolt protocol).

### DROP INDEX

```cypher gate:fixture=schema
DROP INDEX person_email

DROP INDEX person_email IF EXISTS
```

`IF EXISTS` suppresses the error when the index does not exist.

### CREATE CONSTRAINT

Two constraint types are supported, both node-scoped on a single property:
`UNIQUE` (at most one node with a given label has a given property value) and
`NOT NULL` (every node with the label has the property present and non-null).

The modern `FOR … REQUIRE` grammar is the primary form; the legacy
`ON … ASSERT` grammar (removed in Neo4j 5) is accepted as an alias.

```cypher
// uniqueness constraint (modern form)
CREATE CONSTRAINT person_email_unique
    FOR (n:Person) REQUIRE n.email IS UNIQUE

// not-null constraint, idempotent create
CREATE CONSTRAINT person_name_notnull IF NOT EXISTS
    FOR (n:Person) REQUIRE n.name IS NOT NULL
```

The legacy spelling declares exactly the same constraint as the first statement
above:

```cypher gate:fixture=empty
CREATE CONSTRAINT person_email_unique
    ON (n:Person) ASSERT n.email IS UNIQUE
```

Both types enforce the constraint on every future write. `CREATE CONSTRAINT`
also validates the **existing** data: it fails (rejecting the constraint, with
nothing registered) if a `UNIQUE` property already has duplicate values, or if a
`NOT NULL` property is already absent on some node carrying the label.

Constraint names must be unique across the database: creating a constraint whose
name is already in use by a different constraint is rejected, as is re-declaring
an already-existing constraint without `IF NOT EXISTS`.

> **Cost note.** `CREATE CONSTRAINT` validates and back-fills a bound index over
> every node carrying the label, so its cost is **O(N)** in the number of such
> nodes (`CREATE INDEX` is likewise O(N); `DROP` of either is cheap). A repeated
> `CREATE`/`DROP` cycle on a heavily-populated label therefore consumes CPU
> proportional to the labelled dataset on each iteration. This work is bounded by
> the graph size and requires an authenticated client holding write (DDL) access,
> and it is proportional to data that client can already scan — so it is an
> operational cost characteristic, not a privilege-escalation vector. If DDL is
> ever exposed to lower-trust clients, add a rate or size consideration around
> schema mutation.

The following forms are **not supported** and are rejected with a specific
error: relationship constraints (`FOR ()-[r:T]-() REQUIRE …`), composite
(multi-property) constraints, `NODE KEY` / relationship key, and property type
constraints (`IS :: <TYPE>`).

### DROP CONSTRAINT

`DROP CONSTRAINT` removes a constraint by its declared name (the name shown in
`db.constraints()`). `IF EXISTS` suppresses the error when no such constraint
exists.

```cypher gate:fixture=schema
DROP CONSTRAINT person_email_unique

DROP CONSTRAINT person_email_unique IF EXISTS
```

### SHOW CONSTRAINTS / SHOW INDEXES

`SHOW CONSTRAINTS` and `SHOW INDEXES` list the registered schema. They are the
modern replacements for the `db.constraints()` / `db.indexes()` procedures
(deprecated in Neo4j 4.3, removed in 5.0) and are what a modern Neo4j client
issues to introspect the schema. Both are **pure reads**: they emit a result
set (named columns and rows), mutate nothing, and may be run standalone, on a
read-only transaction (`BeginReadTx`), or inside an explicit write transaction.

```cypher
SHOW CONSTRAINTS

SHOW INDEXES
```

The singular aliases `SHOW CONSTRAINT` and `SHOW INDEX` are accepted and behave
identically. A single trailing `;` is tolerated. A trailing `YIELD` / `WHERE` /
`RETURN` projection is also supported — see [Projecting and filtering
(YIELD / WHERE / RETURN)](#projecting-and-filtering-yield--where--return).

**Rows are drawn from the same enumeration as `db.constraints()` /
`db.indexes()`**, so the two views can never disagree on which constraints or
indexes exist. Output is ordered deterministically by `name`.

`SHOW CONSTRAINTS` yields, in order:

| Column | Type | Value |
|---|---|---|
| `name` | `STRING` | the declared (or auto-generated) constraint name — the name `DROP CONSTRAINT` takes |
| `type` | `STRING` | `UNIQUE` or `NOT_NULL` |
| `entityType` | `STRING` | always `NODE` |
| `labelsOrTypes` | `LIST<STRING>` | the single label, e.g. `["Person"]` |
| `properties` | `LIST<STRING>` | the single property, e.g. `["email"]` |

`SHOW INDEXES` yields, in order:

| Column | Type | Value |
|---|---|---|
| `name` | `STRING` | the index name |
| `state` | `STRING` | always `ONLINE` (indexes are built synchronously) |
| `type` | `STRING` | `hash` or `btree` |
| `entityType` | `STRING` | always `NODE` |
| `labelsOrTypes` | `LIST<STRING>` | the single label |
| `properties` | `LIST<STRING>` | the single property |

The backing index of a `UNIQUE` constraint (named `__uniq__<Label>.<prop>`) is
listed by `SHOW INDEXES`, matching `db.indexes()`; because it is not a
user-declared index its `labelsOrTypes` and `properties` are reported as empty
lists.

**Dialect alignment and deviations.** The *column names* follow modern Neo4j
(`entityType`, `labelsOrTypes`, `properties`), but the *column values* stay in
GoGraph's own vocabulary rather than being remapped to Neo4j's — this is both
faithful to what GoGraph is and consistent with `db.constraints()` /
`db.indexes()`:

- Constraint `type` is `UNIQUE` / `NOT_NULL`, not Neo4j's `UNIQUENESS` /
  `NODE_PROPERTY_EXISTENCE`.
- Index `type` is `hash` / `btree` (GoGraph's real index kinds), not Neo4j's
  `RANGE` — a hash index is equality-only and reporting it as `RANGE` would be
  incorrect.
- `state` is always `ONLINE` and `entityType` always `NODE`, because GoGraph
  builds every index synchronously and supports node-scoped schema only.

Columns that Neo4j reports for data GoGraph does not track — `id`,
`populationPercent`, `indexProvider`, `owningConstraint`, `ownedIndex`,
`lastRead`, `readCount`, `propertyType` — are **omitted** rather than filled with
fabricated values.

#### Projecting and filtering (YIELD / WHERE / RETURN)

Modern clients issue the SHOW commands with a trailing `YIELD` / `WHERE` /
`RETURN` clause. The supported grammar is:

```
SHOW CONSTRAINTS | SHOW INDEXES
  [ YIELD { * | column [AS alias] [, …] } ]
  [ WHERE <predicate> ]
  [ RETURN { * | item [AS alias] [, …] } ]
```

- **`YIELD *`** projects every default column, in the default order — the form a
  modern client (Browser `:schema`, driver tooling) emits by default. It is
  equivalent to the plain form.

  ```cypher
  SHOW CONSTRAINTS YIELD *
  ```

- **`YIELD column [AS alias] [, …]`** selects and optionally renames columns, and
  is a scope barrier: only the yielded (aliased) names are visible to the
  following `WHERE` and `RETURN`. Referencing a non-yielded column afterwards is a
  compile-time error.

  ```cypher
  SHOW INDEXES YIELD name AS index, type
  ```

- **`WHERE <predicate>`** filters rows. It may follow a `YIELD` (scope: the
  yielded columns) or stand alone without a `YIELD` (scope: every default
  column). The predicate is an ordinary Cypher expression evaluated per row with
  three-valued logic — `NULL` and `false` both drop the row — and may reference
  query parameters and list columns.

  ```cypher
  SHOW CONSTRAINTS WHERE type = 'UNIQUE'
  SHOW INDEXES YIELD name, type WHERE type = $kind
  SHOW CONSTRAINTS YIELD name, labelsOrTypes WHERE 'Person' IN labelsOrTypes
  ```

- **`RETURN item [AS alias] [, …]`** (or `RETURN *`) is a final scalar projection
  over the yielded scope. `RETURN` requires an explicit `YIELD` (Neo4j: the
  `YIELD` clause is mandatory before `RETURN`, and `YIELD *` may not be combined
  with `RETURN`).

  ```cypher
  SHOW CONSTRAINTS YIELD name, type WHERE type = 'UNIQUE' RETURN name AS cname
  ```

Output order is preserved: rows stay sorted deterministically by `name`.

**Unsupported forms.** The following are rejected with a specific error rather
than silently ignored:

- The legacy `BRIEF` / `VERBOSE` suffixes (removed in Neo4j 5.0).
- `ORDER BY`, `SKIP`, and `LIMIT` (on either `YIELD` or `RETURN`), `RETURN
  DISTINCT`, and aggregations in `RETURN` — the SHOW result set is small and
  already ordered by `name`.
- Expressions inside `YIELD` (a `YIELD` item is a column name with an optional
  `AS` alias; put any computation in `RETURN`).
- Combining SHOW with arbitrary general clauses (`SHOW … MATCH …`, `… WITH …`).

```cypher
SHOW CONSTRAINTS VERBOSE
// BRIEF/VERBOSE not supported    gate:error=unsupported clause "VERBOSE"

SHOW INDEXES YIELD name ORDER BY name
// ORDER BY not supported         gate:error=unsupported YIELD form

SHOW CONSTRAINTS YIELD name, type RETURN count(*)
// aggregation not supported      gate:error=aggregation in RETURN is not supported

SHOW CONSTRAINTS YIELD toUpper(name)
// expressions not allowed in YIELD  gate:error=unsupported YIELD form
```

---

## Built-in procedures (CALL)

Procedures are invoked with `CALL proc()` and yield one or more columns.

A procedure resolves identically on every entry point — `Run`, `RunAny`,
`RunInTx` and `RunInTxAny` — so a statement that mixes a write clause with a
`CALL db.*`, such as `CALL db.labels() YIELD label CREATE (:Marker {l: label})`,
is planned and executed like any other. This also means the routing decision
`RunAny` makes on your behalf can never change whether a procedure is found.

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

Yields: `name STRING`, `type STRING`, `label STRING`, `property STRING`. The
`name` column is the constraint's declared (or auto-generated) name — the same
name `DROP CONSTRAINT` takes. `type` is `UNIQUE` or `NOT_NULL`.

> Note: `db.constraints()` / `db.indexes()` are the legacy schema-introspection
> procedures. The modern [`SHOW CONSTRAINTS` / `SHOW INDEXES`](#show-constraints--show-indexes)
> statements list the same schema and are preferred for new code; the two views
> share a single enumeration, so they never diverge.

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

`BeginTx` acquires **no lock at all**. Since rmp #2305 and rmp #2306 concurrency
control is MVCC alone: an open explicit transaction blocks neither readers nor
writers, and two clients may hold open write transactions simultaneously and both
make progress. What the transaction holds is its own unpublished **commit record**,
published exactly once at `COMMIT` — which is what makes a multi-statement
transaction become visible at a single instant and a rolled-back one leave no trace.
Each statement takes the graph's schema barrier *shared* for its own duration and
releases it before returning, so nothing is held across client think-time.

A write-write collision between two open transactions is **refused at the
conflicting statement** with a retriable serialization error (`Exec` fails), not
deferred to `COMMIT`: detection is first-updater-wins on the version chain, so the
loser is known the moment it tries to install its version. Over Bolt the error maps
to a `TransientError`, so the official driver's managed transactions retry it.

An abandoned transaction no longer causes an outage, but it still pins the
reclamation horizon — no version it could read is freed while it lives — which is
why `server.Options.MaxTxIdleTime` keeps a finite bound.

If `ctx` is already cancelled or its deadline has elapsed, `BeginTx` returns
promptly, wrapping the context error.

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
blocks, a concurrent writer — roughly doubling concurrent read throughput. What
it does acquire is one MVCC read snapshot, held for the transaction's lifetime.

```go
func (e *Engine) BeginReadTx(ctx context.Context) (*cypher.ExplicitTx, error)
```

It returns the same `*ExplicitTx` handle, with these differences from `BeginTx`:

- **Writes are rejected before execution.** A statement containing a writing
  clause, or any DDL, is rejected with the exported sentinel
  `cypher.ErrWriteInReadOnlyTx` **before** it runs. This guard is what keeps the
  lock-free read path safe: a write would otherwise execute with no writer lock,
  no barrier, and no WAL.
- **Snapshot isolation across the whole transaction.** `BeginReadTx` pins ONE
  read instant and every `Exec` on the handle executes at it, so a commit made by
  anyone else between two statements is invisible to the second. This is
  stronger than Neo4j's documented multi-statement read-transaction behaviour
  and matches Memgraph's default. Until rmp #2307 each `Exec` took its own
  per-statement snapshot — read-committed across statements — which made an
  explicit read transaction weaker than a single autocommit statement.
- **It pins the reclamation horizon while it is open.** No version the
  transaction could still reach is freed until it finishes, so a long-lived read
  transaction holds version memory. `lpg.MVCCStats.ActiveSnapshots` and
  `OldestSnapshotAge()` report it — renamed in sprint 334, because the horizon
  holds a writer's snapshot as well as a reader's and the old names
  (`ActiveReaders`, `OldestReaderAge()`) under-reported what was pinning it.
  Finish the handle promptly; over Bolt, the idle and total transaction timeouts
  bound an abandoned one.
- **Read-your-own-writes across transactions needs a `Session`.** The commit
  frontier is contiguous, so a commit is acknowledged at an instant that may not
  have published yet, and the same caller's next transaction can begin *below its
  own commit*. A write followed by a separate read may therefore miss the write,
  and a caller writing repeatedly to one key may see a retriable serialization
  error with nothing else contending for it.

  `Engine.NewSession()` closes it (rmp #2329). A `cypher.Session` carries the
  instant it committed at and waits for the frontier to reach it before its next
  operation takes a snapshot, so `Session.Run`, `RunInTx`, `RunAny`, `BeginTx` and
  `BeginReadTx` all observe every commit that session has made. Over Bolt this is
  automatic: the server binds one per connection.

  The `Engine`'s own methods deliberately keep the weaker contract, so an unrelated
  reader pays no wait — the guarantee is asked for by name. It costs nothing when it
  is not needed (32.06 µs against 32.13 µs sessionless on a read-after-write loop;
  see [`benchmarks/session-ryow-2026-08-06.md`](benchmarks/session-ryow-2026-08-06.md)),
  because the wait returns after one atomic load whenever the frontier has already
  passed the session's floor.

  ```go
  s := eng.NewSession()
  if _, err := s.RunInTx(ctx, "CREATE (:Person {name: 'ada'})", nil); err != nil {
      // handle
  }
  // Guaranteed to see the write above, even under concurrent unrelated commits.
  res, err := s.Run(ctx, "MATCH (p:Person) RETURN count(p)", nil)
  ```
- **Teardown-only finish.** The caller must still finish the handle with exactly
  one `Commit` or `Rollback`. On a read-only handle neither makes anything
  durable — but both release the transaction's read snapshot, so skipping them
  leaks a horizon slot for the life of the process.

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

## Inspecting a query plan

Three questions — what runs, what the planner thought, what it cost — across five
Go APIs: three that render an indented tree and two that render the same
information as a fixed-width table. Two of the three questions are also reachable
from Cypher itself, as the `EXPLAIN` and `PROFILE` statement prefixes; see
[The `EXPLAIN` and `PROFILE` statement prefixes](#the-explain-and-profile-statement-prefixes)
below.

### `Engine.Explain` — what runs

Returns the **physical** plan: the operator tree the builder actually produced,
each node named after its concrete operator type. Nothing is executed and the
graph is not touched.

```
Project
└─ GlobalAggregateAdapter
   └─ EagerAggregation
      └─ ColumnarProject
         └─ ColumnarHashJoin [build=right]
            ├─ NodeByLabelScan [P]
            └─ NodeByLabelScan [P]
```

Because each name is read from the operator itself, the rendering cannot
disagree with what executes: a `HashJoin` appears only when a hash join was
built, and the columnar and parallel tiers are visible as distinct operator
types (`ColumnarHashJoin`, `ParallelScanProject`, …) rather than being hidden
behind their row-mode equivalents. This is the surface to reach for when a query
is slow.

A **writing** statement renders the logical plan and says so on its first line: a
write's operators bind to an open transaction, so there is no physical tree to
walk outside one.

### `Engine.ExplainLogical` — what the planner thought

Returns the **logical** plan with each operator's cardinality **estimate** and its
provenance (`exact` / `stats` / `heuristic`):

```
ProduceResults
└─ Projection
   └─ NodeByLabelScan [n:Person] (est. rows=0, exact)
```

Estimates belong to logical nodes and have no counterpart on a built operator, so
they are visible only here. Reach for this when a plan looks wrong and you suspect
the estimate that drove it.

### `Engine.Profile` — what it cost

Executes the query and returns the physical plan annotated with each operator's
emitted rows, its logical storage accesses (`dbhits`), and the time attributed to
it:

```
ColumnarProject (rows=1, dbhits=0, time=17µs)
└─ NodeByIndexSeek [seek="n7"] (rows=1, dbhits=1, time=0s)

ColumnarProject (rows=8, dbhits=0, time=25µs)
└─ ColumnarFilter (rows=8, dbhits=0, time=24µs)
   └─ NodeByLabelScan [P] (rows=300, dbhits=300, time=2µs)
```

Those two plans are the reason `dbhits` is reported. They return a comparable
handful of rows over the same 300-node graph, and only the access counts show that
the second read every record in the label.

Times are **inclusive** of an operator's children, because a pipelined operator's
`Next` pulls from them — subtract a node's children for its exclusive cost, as
with Neo4j's `PROFILE`.

Two caveats, both explicit in the output or the API:

- `Profile` refuses a writing statement rather than performing its writes as the
  side effect of a diagnostic.
- A node shown as `(not measured)` was not instrumented, and did **not** cost
  nothing. Every operator is instrumented today; the label remains because a future
  composite lowering could reopen the gap.

Profiling is off unless `Profile` is called: the instrumentation is a wrapper the
builder installs only when asked, so an ordinary `Run` executes the same code as a
build in which profiling does not exist.

> **What `dbhits` is, exactly.** Unlike `rows` and `time`, which are measured for
> every operator, `dbhits` comes from one of three places, and the rendered figure
> does not say which:
>
> - **Derived** — for a scan, an index seek or a single-hop expand, the figure IS
>   the `rows` figure. Those operators are marked internally as reading one record
>   per row they emit, so the count is taken at the operator boundary and needs no
>   counter threaded through any accessor. That is why `rows` and `dbhits` are
>   equal on every such line.
> - **Measured** — a variable-length expansion (`-[*m..n]->`) reports the
>   relationship slots its BFS actually read, from the counter its traversal budget
>   already maintains. That number is not its row count and is usually far larger.
> - **Zero** — every other operator. For a pure row transformer that is the honest
>   answer: it read no storage. For `shortestPath`, `allShortestPaths` and the
>   morsel-parallel leaves it is an **under-report** — they read storage and count
>   none of it. The parallel leaves say so in their own plan line
>   (`[parallel tier; db-hits not counted]`).
>
> Two further gaps are worth knowing before you compare two plans:
>
> - A single-hop expand with a **relationship-type filter** walks every slot of the
>   source node's adjacency and counts only the slots it emitted. On a node with
>   100 out-edges of which one is `:KNOWS`, `-->` reports 100 db-hits and
>   `-[:KNOWS]->` reports 1, for the same 100-slot walk.
> - **Property reads are never counted.** Neo4j charges a db-hit per property
>   access, so its figures for a filter-heavy or projection-heavy plan are larger
>   than GoGraph's, and the two are not comparable in absolute terms. The ratio
>   between two GoGraph plans is the intended use — and only between plans whose
>   access paths are of the same kind.
>
> The reason property reads are not counted is the cost of the alternative:
> threading a counter into the property accessors — the hottest path in the engine —
> so every ordinary query would carry a branch that exists only for a diagnostic.
> Counting at the access-path boundary instead needs no threading at all, which is
> why an ordinary `Run` executes no counting code rather than merely skipping it.
> The full classification of every figure `EXPLAIN` and `PROFILE` print, with the
> measurements behind each claim above, is in
> [`explain-profile-honesty-audit-2026-09-03.md`](explain-profile-honesty-audit-2026-09-03.md).

### `Engine.ExplainTable` and `Engine.ProfileTable` — the same, as a table

`ExplainTable` and `ProfileTable` return what `ExplainLogical` and `Profile`
return, rendered as a Neo4j-style fixed-width table instead of an indented tree.
The point of the table is comparison: a column of right-aligned numbers reads
across operators, where numbers scattered along lines of varying indentation do
not.

```
+-----------------------+----------+------+
| Operator              | Est.Rows | Vars |
+-----------------------+----------+------+
| ProduceResults        |        - | n    |
| └─ Projection         |        - | n    |
|    └─ NodeByIndexSeek |        - | n    |
+-----------------------+----------+------+
```

```
+--------------------------------+------+--------+-----------+
| Operator                       | Rows | DbHits | Time (ms) |
+--------------------------------+------+--------+-----------+
| Project                        |    1 |      0 |     0.000 |
| └─ NodeByIndexSeek [seek="p3"] |    1 |      1 |     0.000 |
+--------------------------------+------+--------+-----------+
| Total                          |    2 |      1 |     0.000 |
+--------------------------------+------+--------+-----------+
```

Each is the **same walk** as its tree counterpart, not a second derivation of the
plan: `ExplainTable` and `ExplainLogical` share one traversal that performs the
index-seek substitutions, applies the count-store-gated reorderings and computes
the estimates, and `ProfileTable` and `Profile` render one captured measurement
tree from one execution. Neither pair can disagree about which access path runs.

Two things the table shows that the tree does not, and two it does not show:

- The **`Vars` column** lists the variables each operator exposes; no tree
  rendering prints them.
- Alignment makes two operators' figures directly comparable.
- The table has no room for the estimate's **provenance tag** or its certified
  error term. `Est.Rows` carries the number and one marker: a bare `40` is an
  exact maintained count, `~40` is a derived (statistics or heuristic) figure, and
  `-` means no estimate is available — either none is derivable for that operator
  shape, or the statistic behind it is absent or stale. Reach for
  `ExplainLogical` when the provenance is what you need.
- `Est.Rows` is an **estimate throughout**: `ExplainTable` executes nothing, so
  even an "exact" cell states what the operator *would* read, never what it did.
  `ProfileTable`'s `Rows` is the measured figure.

`ProfileTable`'s `Total` line needs reading with care, because two of its three
cells are easy to mistake:

- **`Rows`** is every operator's emitted rows added together — a cost measure, not
  the result's row count. The result's row count is the **root** operator's
  `Rows`, on the table's first data line.
- **`DbHits`** is the sum of every operator's `DbHits` cell, so it inherits every
  qualification above: it is a **lower bound** on the query's storage-record reads,
  not a total, whenever the plan contains a type-filtered expand, a
  `shortestPath`, or a morsel-parallel leaf.
- **`Time (ms)`** is the whole query's elapsed time, because the root operator's
  time already includes every child's.

`ProfileTable` carries every caveat `Profile` carries: the query really runs and
its rows are discarded, a writing statement is refused, times are inclusive of
children, and an operator the instrumentation did not reach is marked
`(not measured)` rather than left to read as one that cost nothing.

### The `EXPLAIN` and `PROFILE` statement prefixes

A statement may be written with an `EXPLAIN` or a `PROFILE` prefix, which the
Cypher grammar accepts ahead of any query the grammar itself parses:

```cypher
EXPLAIN MATCH (n:Person) WHERE n.age > 30 RETURN n
PROFILE MATCH (n:Person) WHERE n.age > 30 RETURN n
```

The two are syntactically identical and differ in **execution**:

- **`EXPLAIN` executes nothing.** It plans the statement and returns the
  statement's own column signature with **zero rows**. A side-effecting statement
  prefixed with `EXPLAIN` — `EXPLAIN MATCH (n) DETACH DELETE n` — leaves the
  graph untouched; the prefix diverts before any transaction is opened.
- **`PROFILE` executes the statement.** It returns the query's real rows, plus
  each operator's measured rows, db-hits and time.

#### Where the plan comes back

The plan is **not** returned as result rows. It travels beside the result:

| Surface | `EXPLAIN` | `PROFILE` |
|---|---|---|
| Go | `Result.Plan()` — an `*exec.PlanNode` | `Result.Profile()` — an `*exec.PlanNode` with measurements |
| Bolt | the `plan` field of the terminal SUCCESS, which the drivers surface as `ResultSummary.Plan()` | the `profile` field, surfaced as `ResultSummary.Profile()` |

At most one of the two is ever populated, which is what lets a reader tell the
planner's **estimates** apart from measurements of a run that happened. This is
the shape Neo4j returns, and it is the reason for it: a driver consuming
`EXPLAIN MATCH (n) RETURN n` expects the query's own column signature, and the
plan where its `ResultSummary` looks for one. Returning the rendered plan as a
one-column result set would have made it invisible to every driver.

Render a captured tree with `exec.RenderPlanNode`, which prints exactly what
`Engine.Explain` prints for the same statement — the prefix and the Go APIs share
one captured tree and one set of renderers.

```go
r, err := eng.Run(ctx, "EXPLAIN MATCH (n:Person) RETURN n", nil)
// ...
fmt.Println(exec.RenderPlanNode(r.Plan()))
```

`EXPLAIN` renders the **physical** plan for a reading statement and the
**logical** plan for a writing one, exactly as `Engine.Explain` does and for the
same reason: a write's operators bind to an open transaction, and opening one is
precisely what `EXPLAIN` must not do.

Both prefixes carry the statement's **plan-time notifications** — the
Cartesian-product warning among them — so `EXPLAIN` surfaces the planner's
advisories without running the query, which is one of the things it is for.

`EXPLAIN` does **not** require parameters to be supplied: planning reads a
parameter's value only where an access-path gate needs it, and a plan is a useful
answer before anything is bound. `PROFILE` executes, so it requires them like any
other execution and reports `ParameterMissing` when one is absent.

#### Two limitations

- **`PROFILE` refuses a writing statement**, returning an error rather than
  executing it, because the profiling instrumentation is installed by the read
  builder. This is the same refusal `Engine.Profile` applies. Use `EXPLAIN` for a
  writing statement's plan, or run it without a prefix to execute it.
- **Neither prefix may precede a schema statement** (`CREATE`/`DROP`
  `INDEX`/`CONSTRAINT`, `SHOW …`). Those are parsed by a separate, hand-written
  DDL parser that the Cypher grammar does not cover, so a prefixed schema
  statement is a **syntax error** — and therefore executes nothing, which is the
  property that matters.

`EXPLAIN` and `PROFILE` are **not reserved words**. They are recognised as a
prefix only at the very start of a statement, and remain usable as ordinary
identifiers everywhere else, as they are in Neo4j:

```cypher
MATCH (explain:Explain) RETURN explain.profile AS profile
```

---

## Declared divergence: string ordering is by code point

`ORDER BY` on a string, and every string comparison with `<`, `<=`, `>`, `>=`,
orders by **Unicode code point**. This is deliberate, and it differs from Neo4j.
It is recorded here because nothing in the query result reveals it.

**The rule you can apply without reading the source.** Compare two strings
character by character; at the first position where they differ, the string whose
character has the smaller Unicode code-point value sorts first. If one string is a
prefix of the other, the shorter sorts first. (The implementation compares the
UTF-8 bytes, which is the same thing: UTF-8 is designed so that byte order equals
code-point order.)

**Where it differs from Neo4j.** Neo4j compares UTF-16 **code units**, because
that is what Java's `String.compareTo` does. The two rules agree for all of ASCII,
all of Latin-1, and in fact everything in the Basic Multilingual Plane below
U+E000. They disagree in exactly one situation:

> a **supplementary-plane** character (U+10000 and above) compared against a BMP
> character in the range **U+E000–U+FFFF**.

A supplementary character is stored in UTF-16 as a *surrogate pair* whose leading
unit lies in U+D800–U+DBFF. Because D800–DBFF is numerically **below** E000–FFFF,
Neo4j sorts every supplementary character before that BMP range, while GoGraph —
comparing the actual code points — sorts it after.

**Worked example.** Ordering `Z`, `a`, `e`, `z`, `é` (U+00E9), `ﬁ` (U+FB01, the
Latin small ligature fi), and `😀` (U+1F600, the grinning face):

| | result |
|---|---|
| **GoGraph** | `Z`, `a`, `e`, `z`, `é`, `ﬁ`, `😀` |
| **Neo4j** | `Z`, `a`, `e`, `z`, `é`, `😀`, `ﬁ` |

The single difference is the last two: GoGraph puts `😀` (U+1F600) after `ﬁ`
(U+FB01) because 1F600 > FB01, while Neo4j puts it first because its leading
surrogate U+D83D is below U+FB01. So **any `ORDER BY` over user text containing
emoji or other supplementary characters will differ between the two engines** —
and no query over ASCII or Latin-1 text ever will.

**Why it is deliberate, not an oversight.**

- The openCypher 2024.3 grammar **specifies no collation**, so neither order is
  non-conformant, and the TCK contains no scenario that discriminates them.
- UTF-16 code-unit order is an artefact of Java's internal string
  representation rather than a principled ordering: it places some
  supplementary characters *before* BMP characters that are numerically lower.
- Code-point order is load-bearing elsewhere in the engine. The string B-tree
  index is ordered by the same rule, which is what makes an equality on a
  string btree index answerable as the degenerate closed range `[v, v]` —
  no two distinct strings compare equal under a code-point order, so the range
  is exact. The same rule is what lets `STARTS WITH` be served as an interval:
  `STARTS WITH` is a code-point (UTF-8 byte) prefix test, and the index is laid
  out in that very order, so the matching keys are contiguous. Changing the
  collation would reach into the index layer, not just `ORDER BY`.

If your application needs locale-aware ordering (dictionary order, case-insensitive
order, language-specific rules), neither engine provides it: sort in the
application, or store a pre-computed sort key alongside the text and order by that.

The ordering is pinned by `TestStringOrdering_IsCodePointOrder`, including the
supplementary-plane boundary case, so it cannot change silently.

---

## Known limitations

The following constructs are not yet supported:

| Feature | Status |
|---|---|
| `CALL { … }` standalone subquery clause | Not parsed; rejected at parse time |
| `CALL { … } IN TRANSACTIONS` | Not supported |
| `COLLECT { … }` subquery expression | Not parsed; rejected at parse time |

`EXISTS { … }` and `COUNT { … }` subquery *expressions* are supported (see
[WHERE](#where) and [Aggregation](#aggregation)). `COLLECT { … }` is **not**:
`COLLECT` has no subquery token, so the parser reads it as a variable followed by
a map literal and the query is rejected — `RETURN COLLECT { MATCH (n) RETURN n }`
fails with a syntax error, and `RETURN COLLECT { k: 1 }` with an
undefined-variable error. To build a list, use the `collect()` aggregate or a
pattern comprehension:

```cypher
MATCH (a:Person) RETURN collect(a.name) AS names

MATCH (a:Person) RETURN [(a)-[:KNOWS]->(b) | b.name] AS friends
```

The openCypher TCK execution suite is fully green: all 3897 scenarios pass
(100%), enforced by `tckExecutionBaseline` in `cypher/tck/runner_test.go`. For
the full divergence taxonomy, see [docs/tck/DIVERGENCES.md](tck/DIVERGENCES.md).

### Unrecognised characters are rejected

Any character outside the openCypher grammar is a syntax error. This matters
most for two spellings that a Neo4j user may reach for, because both used to be
accepted and answered *incorrectly*:

| Written | Result | Use instead |
|---|---|---|
| `WHERE n.v != 2` | Syntax error | `WHERE n.v <> 2` |
| `MATCH (n:!A)` | Syntax error | `MATCH (n) WHERE NOT n:A` |

`!=` is not an openCypher operator (`<>` is), and the label-negation syntax
`:!A` is a Neo4j 5 extension. Until GoGraph v0.10.0 the lexer discarded the `!`
and executed the remainder, so `!= 2` silently evaluated as `= 2` and `:!A`
silently matched exactly the `:A` nodes — the precise opposite of the intent, in
both cases with no error. The same applies to `#`, `&`, `?`, `@`, `~` and `\`
outside a string literal, and to an unterminated string literal.

An unrecognised character inside a string literal, a backtick-quoted identifier
or a comment is ordinary content and is unaffected:

```cypher
RETURN "a != b" AS s             // fine — inside a literal

MATCH (`we!rd`) RETURN 1         // fine — inside a quoted identifier

MATCH (n) RETURN n // uses != !  // fine — inside a comment

RETURN 'abc' =~ '[a-z]+'         // fine — =~ is openCypher
```

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
- **`elementId()` returns that identifier as a decimal string** — the
  openCypher/Neo4j-recommended replacement for the deprecated `id()`. It is
  backed by the same underlying integer `id()` returns (a node's interned
  `NodeID`, a relationship's CSR positional index), so it inherits the same
  stability asymmetry: a node's `elementId()` survives a store reopen, a
  relationship's does not. `null` in yields `null` out, and a non-node,
  non-relationship argument raises a typed error.

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

### Every Cypher example here is executed

The ` ```cypher ` blocks in this document are **executable documentation**.
`internal/cypherdocgate` extracts every statement and runs it against a seeded
engine on each `make ci`, so a published example that stops working fails the
build. When editing this file, keep to these rules:

- **Statements are separated by a blank line.** Two statements on consecutive
  lines are read as one, which will not parse.
- **Comments use `//`.** `--` is not a Cypher comment; a line starting with it
  is a parse error, and a separate check rejects it.
- **Declare a fixture when the default does not fit.** The default seeds a
  small `:Person`/`:City` graph. Use ` ```cypher gate:fixture=schema ` for an
  example that needs pre-existing indexes or constraints, and
  ` ```cypher gate:fixture=empty ` for one that must start from nothing. The
  fixtures are defined in `internal/cypherdocgate/examples_test.go`.
- **Mark an example that is meant to be rejected.** Add
  `// gate:error=<substring of the expected error>` to it, as the unsupported
  `SHOW` forms above do. The gate then requires it to fail, and to fail for
  that reason.
- **Use `gate:skip=<reason>` only for a block that is genuinely not runnable**,
  and say why. There are currently none.


---

*Last reviewed: 2026-07-29 against commit `b2cb4fe5`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*
