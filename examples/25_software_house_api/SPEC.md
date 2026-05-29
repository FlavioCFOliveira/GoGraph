# Example 25 — Software-House Task Graph

**A persistent, multi-layer Labeled Property Graph (LPG) exposed as a small REST
WebAPI, modelling task management inside a software-house.**

This document is the functional and technical specification (the *Specify* step
of the project's `Specify → Implement → Test → Document` workflow). It is the
contract the implementation, tests and user-facing documentation follow.

---

## 1. Purpose & scope

The example demonstrates, end to end and against a real persistent store, how
GoGraph is used in production-shaped code:

- **Creation** — building a multi-layer graph from a deterministic fixture.
- **Query** — answering the questions a maintenance team actually asks, in
  Cypher, over a single graph that spans code, work and people.
- **Manipulation** — mutating the graph (new tasks, re-assignments, status
  changes) durably, through the same query endpoint.

The surface is intentionally minimal: a long-running HTTP server exposing three
endpoints (`POST /query`, `POST /seed`, `GET /stats`). All richness lives in the
**graph model** and in the **curated Cypher catalogue** (§9), not in a sprawl of
bespoke endpoints. The server is built on the Go standard library only
(`net/http`); it adds **zero** new module dependencies.

State is **persistent across process restarts** and survives `kill -9`, using the
module's write-ahead log + snapshot + recovery stack.

---

## 2. The multilayer model (formalism)

The domain has three conceptual **layers**:

1. **Code** — how the project is structured and how its components depend on
   each other.
2. **Work** — tasks and the workflow that coordinates development.
3. **People** — developers, who did past work, and who is planned to do pending
   work.

These layers do **not** share a node set (a `Developer` is not a `Task` is not a
`Component`), and the edges that join them connect *different* node types across
layers. In the vocabulary of network science this is therefore **not** a
node-aligned multiplex; it is a **general multilayer network with non-diagonal
inter-layer coupling**, equivalently a **heterogeneous information network (HIN)**
with a typed schema. The practical consequence is the one that matters: every
cross-layer maintenance question becomes a **meta-path** — a composite of typed
relations — and the schema *is* the query language.

GoGraph stores a single LPG; there is no "graph per layer" primitive, and we do
not want one, because ACID and cross-layer Cypher must span all layers in one
transactional store. The faithful encoding is the standard one:

- **A layer is a label-namespace.** Every node carries **two labels**: a *layer
  label* (`Code`, `Work`, `People`) and a *type label* (`Component`, `Task`,
  `Developer`, …). The layer label materialises the layer index; the type label
  is the HIN object type.
- **Intra-layer edges** have both endpoints in the same layer.
- **Inter-layer (coupling) edges** have endpoints in different layers. There are
  exactly two: `ASSIGNED_TO` (People → Work) and `TOUCHES` (Work → Code).

The invariant *"an edge is inter-layer iff its two endpoints' layer labels
differ"* makes the multilayer structure machine-checkable and lets any query
project onto a single layer by filtering on the layer label.

---

## 3. Node types (schema)

Node keys are strings with a type prefix. Every node has its **layer label** and
its **type label**. Property kinds map to `lpg.PropertyValue`:
`string`, `int` (`Int64Value`), `float` (`Float64Value`), `bool`, `timestamp`
(`TimeValue`). Timestamps are also surfaced as ISO-8601 UTC strings in `*_at`
properties so that text ordering and equality work directly in Cypher.

### Layer `Code`

| Type label   | Key            | Properties |
|--------------|----------------|------------|
| `Repository` | `repo:<name>`  | `name` string, `url` string, `created_at` string |
| `Module`     | `mod:<name>`   | `name` string, `path` string |
| `Component`  | `comp:<path>`  | `name` string, `path` string, `kind` string (`package`/`file`/`function`), `language` string, `loc` int |

`Component.kind` is a discriminator kept under one label so that `DEPENDS_ON` and
`TOUCHES` stay uniform across granularities (a split into `Package`/`File`/…
labels would fork every Code-layer query).

### Layer `Work`

| Type label      | Key             | Properties |
|-----------------|-----------------|------------|
| `Task`          | `task:<id>`     | `title` string, `status` string (`todo`/`in_progress`/`in_review`/`done`), `priority` int, `kind` string (`feature`/`bug`/`chore`/`refactor`), `created_at` string |
| `Sprint`        | `sprint:<id>`   | `name` string, `starts_at` string, `ends_at` string |
| `WorkflowState` | `state:<name>`  | `name` string, `order` int, `is_terminal` bool |

`Task.status` is the **authoritative, filterable** state. `WorkflowState` nodes
model the workflow itself (ordering, terminality); `Task.status` and the
`HAS_STATE` edge are kept in sync at write time.

### Layer `People`

| Type label  | Key            | Properties |
|-------------|----------------|------------|
| `Developer` | `dev:<handle>` | `name` string, `handle` string, `email` string, `seniority` string (`junior`/`mid`/`senior`/`staff`), `active` bool |
| `Team`      | `team:<name>`  | `name` string |

---

## 4. Relationship types

A relationship has exactly **one** type. Direction is `src → dst` and is fixed
and asserted by the seed loader (a flipped edge silently returns the wrong set,
with no error). The LPG `float64` weight defaults to `1.0` unless a numeric
property is more natural.

### Intra-layer — `Code`

| Type         | Direction                                                       | Properties | Notes |
|--------------|-----------------------------------------------------------------|------------|-------|
| `CONTAINS`   | `Repository→Module`, `Module→Component`                         | —          | Containment hierarchy (a tree). |
| `DEPENDS_ON` | `Component→Component`                                            | `kind` string (`import`/`call`), `strength` int | The dependency DAG; *dependent → dependency*. May contain a cycle (a finding, see Q7). |

### Intra-layer — `Work`

| Type         | Direction              | Properties | Notes |
|--------------|------------------------|------------|-------|
| `SUBTASK_OF` | `Task→Task`            | —          | Decomposition. |
| `NEXT`       | `Task→Task`            | —          | Sequencing ("do A before B"). |
| `BLOCKS`     | `Task→Task`            | `reason` string | Hard blocking; *blocker → blocked*. |
| `HAS_STATE`  | `Task→WorkflowState`   | —          | Current workflow state as a node. |
| `IN_SPRINT`  | `Task→Sprint`          | —          | Planning membership. |

### Intra-layer — `People`

| Type        | Direction          | Properties | Notes |
|-------------|--------------------|------------|-------|
| `MEMBER_OF` | `Developer→Team`   | —          | Org structure. |

### Inter-layer (coupling) — the multilayer spine

| Type          | Direction            | Properties | Notes |
|---------------|----------------------|------------|-------|
| `ASSIGNED_TO` | `Developer→Task`     | `role` string (`author`/`reviewer`), `state` string (`planned`/`active`/`done`) | People → Work. Carries the past-vs-planned signal. |
| `TOUCHES`     | `Task→Component`     | `change_type` string (`add`/`modify`/`delete`/`test`), `churn` int, `at` string | Work → Code. **Existence means realised (past) work.** |

---

## 5. The cross-layer spine & past-vs-planned

The canonical meta-path threading all three layers:

```
(d:Developer) -[:ASSIGNED_TO]-> (t:Task) -[:TOUCHES]-> (c:Component)
   People                Work               Code
```

Composed on the Code end with the dependency DAG, this is the engine of impact
analysis: from a changed component, fan out across `DEPENDS_ON*`, then climb the
spine to the tasks and developers that own the affected code (§9, Q1).

**Past vs planned** is queryable from three consistent signals, never from prose:

1. `Task.status` — `done` vs the rest (authoritative task-level state).
2. `ASSIGNED_TO.state` — `planned` / `active` / `done` (developer-level state).
3. **Existence of a `TOUCHES` edge** — planning produces `ASSIGNED_TO {state:'planned'}`
   with **no** `TOUCHES`; completed work produces `TOUCHES` edges. So *"what did
   Alice change"* traverses `TOUCHES`; *"what is Alice planned to do"* is
   `ASSIGNED_TO {state:'planned'}`.

---

## 6. REST API contract

The server holds the store open for the process lifetime and serves concurrent
requests. All bodies are bounded (`http.MaxBytesReader`); all handlers honour the
request context's cancellation/deadline.

### `POST /query`

Run an arbitrary Cypher statement (read **or** write). The engine classifies the
statement and routes reads through the lock-free read path and writes through a
durable, serialised transaction.

Request body (`application/json`):

```json
{ "query": "MATCH (c:Component) RETURN c.key AS key LIMIT 3",
  "params": { "k": "comp:search/dijkstra.go" } }
```

`params` is optional. Response (`200`):

```json
{ "columns": ["key"],
  "rows": [ {"key": "comp:graph/lpg/lpg.go"},
            {"key": "comp:search/dijkstra.go"} ] }
```

Each row is an object keyed by the statement's output columns; values follow the
`expr.Value → JSON` mapping (§ below). A write-only statement (no `RETURN`)
returns `{"columns":[],"rows":[]}`; the write is durably committed before the
response is sent.

### `POST /seed`

Idempotently load the deterministic fixture. Response (`200`):

```json
{ "seeded": true, "status": "ok" }
```

`seeded` is `false` when the graph was already populated (a no-op).

### `GET /stats`

Counts of nodes by type label and edges by relationship type. Response (`200`):

```json
{ "nodes": { "Repository": 1, "Module": 4, "Component": 12, "Task": 14,
             "Sprint": 2, "WorkflowState": 4, "Developer": 6, "Team": 2 },
  "edges": { "CONTAINS": 16, "DEPENDS_ON": 15, "SUBTASK_OF": 3, "NEXT": 4,
             "BLOCKS": 3, "HAS_STATE": 14, "IN_SPRINT": 11, "MEMBER_OF": 6,
             "ASSIGNED_TO": 16, "TOUCHES": 18 } }
```

(Exact counts are fixed by the seed; see the README once implemented.)

### Errors

Every error is a JSON document `{"error": "<message>", "kind": "<kind>"}` with the
appropriate status:

| Status | `kind`        | Cause |
|-------:|---------------|-------|
| `400`  | `bad_request` | Malformed JSON, or a Cypher **parse** error. |
| `405`  | `method`      | Wrong HTTP method for the route. |
| `413`  | `too_large`   | Request body exceeds the limit. |
| `422`  | `semantic`    | Cypher **semantic** error (unknown function, type error, …). |
| `500`  | `runtime`     | Engine/runtime failure. |
| `503`  | `unavailable` | Server is shutting down. |

### Value mapping (`expr.Value → JSON`)

`integer → number`, `float → number`, `string → string`, `boolean → bool`,
`list → array`, `map → object`, `null → null`. A node →
`{"_id", "_labels", "_properties"}`; a relationship →
`{"_id", "_type", "_start", "_end", "_properties"}` (the neo4j-driver convention,
so clients can tell graph metadata from plain maps).

---

## 7. Persistence & recovery contract

The data directory `<dir>` holds:

- `<dir>/wal` — the append-only, CRC-framed write-ahead log. Every committed
  write is appended and **fsynced before the commit is acknowledged**.
- `<dir>/snapshot/*` — a manifest plus the CSR adjacency, labels, properties and
  mapper images, written at startup (empty graph) and at graceful shutdown.

On startup the server calls `recovery.OpenCtx`, which loads the snapshot and
replays any WAL tail on top, reconstructing the exact in-memory graph. Because
each write is durable at commit time, the store is **`kill -9`-safe**: a crash
with no clean shutdown still recovers every acknowledged write from the WAL on
the next boot. The shutdown snapshot is an optimisation (it shortens WAL replay),
not a correctness requirement.

---

## 8. Concurrency & isolation contract

- **Reads** (`MATCH … RETURN`) execute under a read lock and are fully
  parallel; a reader materialises its rows before the lock is released, so it
  never observes a partially-applied transaction.
- **Writes** (`CREATE`/`MERGE`/`SET`/`DELETE`) serialise on the store's
  single-writer mutex and become visible atomically (all-or-nothing).
- The HTTP handler **must fully drain and `Close()`** the result inside the
  handler, because a write holds the store mutex until the result is closed.

The guarantee delivered is **snapshot-isolation reads + serialised writes** —
the module's ACID isolation model — preserved unchanged by the server, which
adds no lock of its own.

---

## 9. Maintenance-question Cypher catalogue

Each query is posted to `POST /query`. `$param` placeholders are supplied via the
request's `params`. These eight queries are the didactic core of the example.

**Q1 — Change-impact / blast-radius.** *If component `$k` changes, which
components are affected, and which tasks and developers must be told?*

```cypher
MATCH (x:Component {key:$k})
MATCH (affected:Component)-[:DEPENDS_ON*0..]->(x)
OPTIONAL MATCH (dev:Developer)-[:ASSIGNED_TO]->(t:Task)-[:TOUCHES]->(affected)
RETURN affected.key AS component,
       collect(DISTINCT t.key)   AS impactedTasks,
       collect(DISTINCT dev.key) AS impactedDevelopers
ORDER BY component
```

**Q2 — Code ownership ("who knows this code").** *Who has actually worked on
component `$k`, and how much?*

```cypher
MATCH (dev:Developer)-[:ASSIGNED_TO]->(:Task)-[r:TOUCHES]->(c:Component {key:$k})
RETURN dev.key AS developer, count(r) AS touches, sum(r.churn) AS totalChurn
ORDER BY totalChurn DESC
```

**Q3 — Bus-factor risk sweep.** *Which components have been touched by exactly one
developer (a knowledge-concentration risk)?*

```cypher
MATCH (dev:Developer)-[:ASSIGNED_TO]->(:Task)-[:TOUCHES]->(c:Component)
WITH c, count(DISTINCT dev) AS busFactor, collect(DISTINCT dev.key) AS owners
WHERE busFactor = 1
RETURN c.key AS component, busFactor, owners
ORDER BY component
```

**Q4 — Developer workload.** *What is assigned to `$dev` and not yet done?*

```cypher
MATCH (d:Developer {key:$dev})-[a:ASSIGNED_TO]->(t:Task)
WHERE t.status <> 'done' AND a.state <> 'done'
RETURN t.key AS task, t.title AS title, t.status AS status, t.priority AS priority, a.role AS role
ORDER BY priority DESC
```

**Q5 — What is blocked, and why (transitive chain).** *Which open tasks are
blocked, and what is the chain of open blockers reaching `$task`?*

```cypher
MATCH path = (root:Task)-[:BLOCKS*1..]->(t:Task {key:$task})
WHERE root.status <> 'done'
RETURN [n IN nodes(path) | n.key] AS chain
ORDER BY length(path) DESC
```

**Q6 — Most-depended-upon component.** *Which components have the highest
dependency in-degree (changing them is riskiest)?*

```cypher
MATCH (c:Component)<-[:DEPENDS_ON]-(dependent:Component)
RETURN c.key AS component, count(dependent) AS inDegree
ORDER BY inDegree DESC
LIMIT 10
```

**Q7 — Dependency cycles.** *Which components lie on a dependency cycle (an
architectural smell)?*

```cypher
MATCH (c:Component)-[:DEPENDS_ON*1..]->(c)
RETURN DISTINCT c.key AS componentOnCycle
ORDER BY componentOnCycle
```

**Q8 — Component history.** *Who touched component `$k`, and when?*

```cypher
MATCH (dev:Developer)-[:ASSIGNED_TO]->(t:Task)-[r:TOUCHES]->(c:Component {key:$k})
RETURN dev.key AS developer, t.key AS task,
       r.change_type AS change, r.churn AS churn, r.at AS at
ORDER BY at DESC
```

Queries Q1, Q5 and Q7 rely on variable-length paths (`DEPENDS_ON*` / `BLOCKS*`).
Q3 uses `count(DISTINCT dev)` — counting *people*, not assignment roles. Q1 uses
`OPTIONAL MATCH` so an affected-but-unowned component is reported rather than
silently dropped.

---

## 10. Seed dataset (shape)

The fixture represents a small software-house working on a graph library. It is
deterministic (fixed keys and timestamps) and idempotent. It is sized so that
**every** query in §9 returns a meaningful, non-empty answer — in particular it
contains at least one bus-factor-1 component (Q3), at least one transitive
blocking chain (Q5), at least one dependency cycle (Q7), and at least one
developer holding both completed and planned work (Q4/Q8). The exact node and
edge counts are those reported by `GET /stats` and are pinned by the tests.
