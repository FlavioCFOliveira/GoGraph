# Does a later clause observe an earlier edge write in the same statement?

**Date:** 2026-08-02
**Head:** `b8b9cf38`
**Task:** rmp #2316, sprint 334
**Found during:** rmp #2299 (writer transaction identity), which neither caused nor fixed it

---

## 0. The answer, up front

**Nodes: yes. Relationships: no.** And the difference is an artefact of *when* a
structure is built, not a semantic choice.

| earlier in the SAME statement | observed by a later clause? | committed correctly? |
|---|---|---|
| node `CREATE` | **yes** | yes |
| node `DELETE` | **yes** | yes |
| node `DETACH DELETE` | **yes** | yes |
| label `SET` | **yes** | yes |
| label `REMOVE` | **yes** | yes |
| property `SET` | **yes** | yes |
| **edge `CREATE`** | **no** | yes |
| **edge `DELETE`** | **no** | yes |

Both reference engines resolve relationships **per row at execution time**.
GoGraph materialises the whole adjacency into a CSR at **plan-build time**, which
is before any row runs — so a later `MATCH` traverses topology as it was before
the statement's own edge writes.

It is **not** a TCK-conformance failure: the suite is silent on relationships in
this shape. It **is** an inconsistency with no defence — the same query shape
gives a different answer depending on whether the entity is a node or a
relationship — and neither Neo4j nor Memgraph has it.

---

## 1. What the openCypher TCK actually requires

Only **two** scenarios in the whole suite compose an updating clause with a
subsequent *reading* clause inside one query. Found by scanning every
`executing query:` block in `cypher/tck/features` for an updating clause followed
by a `MATCH`/`OPTIONAL MATCH`:

| scenario | query |
|---|---|
| `clauses/create/Create3.feature` [3] | `MATCH () CREATE () WITH * MATCH () CREATE ()` |
| `clauses/match/Match8.feature` [2] | `MATCH (a) MERGE (b) WITH * OPTIONAL MATCH (a)--(b) RETURN count(*)` |

**`Create3` [3] is normative, and it settles the node case.** Its expected side
effect is `+nodes 10` from a graph seeded with two nodes:

```
MATCH ()      -- 2 rows
CREATE ()     -- +2 nodes (now 4), still 2 rows
WITH *
MATCH ()      -- the question: does this see 2 or 4?
CREATE ()     -- +1 node per row
```

Ten new nodes is reachable **only** if the second `MATCH` observes the two nodes
the first `CREATE` made: `2 + (2 × 4) = 10`. Had it seen only the original two,
the answer would be `2 + (2 × 2) = 6`. GoGraph passes this scenario, and the
suite is at its full baseline of 3897/3897.

`Match8` [2] does not bear on the question: its relationships pre-exist the
query, so it tests row counting, not write visibility.

**The TCK does not cover an edge `CREATE` or `DELETE` followed by a traversal in
the same query.** That gap is how this behaviour survived a green suite, and it
is why this is not reportable as a conformance failure — but the *principle*
`Create3` establishes for nodes has no clause exempting relationships.

## 2. The mechanism, with file:line

`cypher/api.go:8190`, in the `*ir.Expand` physical builder:

```go
fwd, rev, pairAt := csrPairCachedForAt(bopts, g)
```

The physical operator tree is assembled **in full** by
`buildPlanWithMutatorFull` before a single row executes. The second `MATCH`'s
`Expand` is therefore handed a CSR built from the graph as it stood at *plan
build time* — before the statement's own `CREATE` ran.

**This is materialisation, not caching.** The CSR pair cache
(`cypher/csr_pair_cache.go`) is keyed on `lpg.Graph.TopoGeneration` plus the
snapshot's `startTS`, and that key is sound. Disabling it changes nothing:

| `EngineOptions.DisableCSRPairCache` | in-statement traversal sees |
|---|---|
| `false` (cache on) | 0 edges |
| `true` (cache off) | 0 edges |

With the cache off, `csrPairFromGraphAt(g)` still builds *at build time*. The
edge is invisible because the structure was frozen too early, not because a
stale entry was reused.

**Why nodes behave differently.** Node existence, labels and properties are
resolved lazily, per row, against the live stores and the label index. Only edge
*topology* is precomputed into a CSR. So exactly the structures that are read
lazily are the ones that see the statement's own writes.

**It is confined to one statement.** An edge created in statement 1 of an
explicit transaction *is* visible to statement 2 of the same transaction
(measured: `LINK edges = 1`), because statement 2 gets a fresh plan build. The
window is precisely one statement wide.

## 3. What the reference engines do

Both resolve relationships **per row, at execution time, against the
transaction's live view**. Neither precomputes adjacency when the plan is built.

**Neo4j** — `ExpandAllPipe.internalCreateResults`
(neo4j/neo4j, branch `5.26`, read 2026-08-02;
`community/cypher/interpreted-runtime/.../pipes/ExpandAllPipe.scala`):

```scala
input.flatMap {
  row =>
    ...
    val relationships = state.query.getRelationshipsForIds(n.id(), dir, types.types(state.query))
```

The lookup is inside the per-row `flatMap`, through `state.query` — the
transaction's query context — not a structure captured when the pipe was built.

**Memgraph** — `Expand::ExpandCursor::InitEdges`
(memgraph/memgraph, branch `master`, read 2026-08-02;
`src/query/plan/operator.cpp`):

```cpp
auto edges_result = UnwrapEdgesResult(
    vertex.OutEdges(self_.view_, self_.common_.edge_types, &context->hops_limit));
```

Also per row, on the vertex accessor. Memgraph goes further and makes the
question **explicit**: `self_.view_` is a `storage::View`, either `OLD` or `NEW`,
and it is a plan-level decision which one an expansion uses. Same-transaction
edge writes are visible under `NEW` and not under `OLD`.

That distinction is the point. Memgraph *decides*; GoGraph's answer falls out of
when a data structure happened to be built, and lands on "not visible" for
relationships while landing on "visible" for everything else.

## 4. Verdict

**A defect, but of consistency and mechanism — not of TCK conformance.**

- Not a conformance failure: the TCK does not specify the relationship case, and
  GoGraph passes the one scenario (`Create3` [3]) that does specify the node
  case. The 100% mandate is intact.
- A real defect nonetheless: one query shape answers differently for nodes and
  for relationships, for no reason a user could predict or a specification
  states, and the two engines GoGraph takes as references have neither the
  inconsistency nor the build-time materialisation that causes it.

**Blast radius on the rest of sprint 334.** The CSR is materialised from the
graph the writer reads through, so rmp #2299's writer snapshot is already
threaded into it (`csrPairKeyFor` reads `g.Snapshot().StartTS()`). Making the
resolution lazy or refreshable does not conflict with #2303 (ordering the
structures whose publication depends on the barrier) or #2304 (removing the
barrier) — but it touches the same adjacency-publication machinery, which is why
settling it before those tasks was the right call.

**The trade the fix must respect.** The CSR pair exists because building it per
row was `O(R·(V+E))` and dominated allocations (`cypher/api.go:366-387`,
rmp #1574; the cache itself is rmp #2143). Any fix that reaches live topology per
row must not put that cost back on the read path, which is the overwhelming
majority of queries and the one this project optimises hardest.

## 5. The decided fix, and what it actually costs

**DECIDED, 2026-08-02: make the traversal resolve adjacency live, per row, at
execution time — the Neo4j/Memgraph shape — on every path, read and write.**
Rejected alternatives: freezing nodes to match edges (ruled out by TCK
`Create3` [3]); accepting the inconsistency and documenting it; and a
write-path-only lazy refresh, which would have fixed the symptom while leaving
GoGraph structurally unlike both references.

**The coupling this has to break is larger than the performance question.**
Measuring it before starting, because "abandon the CSR on the traversal path" is
not a local edit:

- **Edge identity IS the forward-CSR position.** `Expand` emits
  `(srcID, edgeID, dstID)` where `edgeID` is an absolute index into
  `fwd.EdgesSlice()`. Every downstream consumer decodes it that way.
- **`csrAdjacency` is a flat-array contract**, not a per-node lookup one:
  `VerticesSlice() []uint64`, `EdgesSlice() []graph.NodeID`,
  `HandlesSlice() []uint64` (`cypher/exec/expand.go:77-89`). A live provider
  cannot satisfy it lazily; the arrays *are* the materialised structure.
- **The edge-type filter is keyed on absolute positions** —
  `edgeTypeFilter map[uint64]string`, probed at `cypher/exec/expand.go:711,817`
  and `cypher/exec/shortest_path.go:1201,2347`. Rebuilding the CSR renumbers
  every key.
- **84 call sites across six files** consume the pair or its slices:
  `cypher/api.go`, `cypher/csr_pair_cache.go`, `cypher/exec/expand.go`,
  `cypher/exec/varlen_expand.go`, `cypher/exec/expand_intersect.go`,
  `cypher/exec/shortest_path.go`.

So the change is not "swap the adjacency source". It is **replacing the engine's
relationship identity encoding**, which is what Neo4j and Memgraph avoid needing
because their relationships carry their own ids (`VirtualValues.relationship(relId, …)`,
`EdgeAccessor`) rather than being addressed by position in a rebuilt array.

That makes it a task of its own rather than a rider on this investigation, and it
must be sequenced against the rest of sprint 334 deliberately: it touches the
same adjacency machinery as rmp #2303 and #2304.

## 5. Reproducing

Collapse rows with an aggregation *before* the second reading clause, or the
count measures row multiplication rather than visibility — a confound that made
an earlier pass of this investigation report node `DELETE` as broken when it is
not.

```cypher
-- edge CREATE: returns 0 in-statement, 1 in a later statement
CREATE (:R1 {id:1}), (:R2 {id:2});
MATCH (a:R1), (b:R2) CREATE (a)-[:LINK]->(b)
WITH count(*) AS c
MATCH (:R1)-[:LINK]->(x:R2) RETURN count(x);

-- edge DELETE: returns 1 in-statement, 0 in a later statement
CREATE (a:K), (b:K) CREATE (a)-[:T]->(b);
MATCH (:K)-[r:T]->(:K) DELETE r
WITH count(*) AS c
MATCH (:K)-[q:T]->(:K) RETURN count(q);

-- the contrast: node CREATE returns 2 in-statement
CREATE (:C), (:C) WITH count(*) AS c MATCH (n:C) RETURN count(n);
```

The behaviours are pinned in
`cypher/writer_rows_test.go` (`TestWriteRows_StructuralChangesAreNotVisibleToALaterClause`),
asserted as **observed** rather than as correct, so a fix can change them without
fighting a test that pre-judged the answer.
