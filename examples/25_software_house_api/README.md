# Example 25 — Software-House Task Graph (persistent REST API)

A small, production-shaped REST service built on **GoGraph**. It models task
management inside a software-house as a single **multi-layer Labeled Property
Graph (LPG)** and answers the questions a maintenance team actually asks —
change impact, code ownership, bus-factor, workload, blocked work, dependency
cycles — in Cypher, over one graph that spans code, work and people.

It is built on the Go standard library only (`net/http`, zero new
dependencies) and is **persistent across restarts** and **`kill -9` safe**
(write-ahead log + snapshot + recovery).

- Data model, REST contract and query catalogue: [`SPEC.md`](SPEC.md)
- Runtime reference: the package doc in [`doc.go`](doc.go)

---

## The multi-layer model

Three layers live in one LPG. Every node carries a *layer* label
(`Code` / `Work` / `People`) and a *type* label. Two inter-layer edges,
`ASSIGNED_TO` and `TOUCHES`, form the spine that makes cross-layer questions a
single Cypher pattern.

```
   People layer            Work layer                     Code layer
  ┌────────────┐  ASSIGNED_TO  ┌──────┐     TOUCHES     ┌───────────┐
  │ Developer  │──────────────▶│ Task │────────────────▶│ Component │
  │ Team       │               │ Sprint                 │ Module    │
  └────────────┘               │ WorkflowState          │ Repository│
     MEMBER_OF                  └──────┘                 └───────────┘
                         SUBTASK_OF / NEXT          CONTAINS / DEPENDS_ON
                         BLOCKS / HAS_STATE
                         IN_SPRINT
```

`DEPENDS_ON` (dependent → dependency) is the dependency graph; composed with the
spine it answers "if I change X, who is affected?". Completed work carries
`TOUCHES` edges (realised history); planned work carries only
`ASSIGNED_TO {state:'planned'}`. See [`SPEC.md`](SPEC.md) for the full schema.

---

## Build and run

```sh
# from the repository root
go run ./examples/25_software_house_api -d ./data -addr :8080
```

`-d` is the data directory (created if missing); `-addr` is the listen address
(default `:8080`). Press Ctrl-C (SIGINT) or send SIGTERM for a graceful
shutdown that writes a final snapshot and closes the WAL.

---

## Endpoints

| Method & path | Purpose |
|---|---|
| `POST /query` | Run an arbitrary Cypher statement (read or write). |
| `POST /seed`  | Idempotently load the deterministic fixture. |
| `GET /stats`  | Node counts by type label, edge counts by relationship type. |
| `GET /healthz`| Liveness probe. |

### Seed the graph

```sh
curl -s -XPOST localhost:8080/seed
```
```json
{"seeded":true,"status":"ok"}
```

Running it again is a no-op (`"seeded":false`). The fixture is **46 nodes** and
**106 edges** across the three layers.

### Inspect the counts

```sh
curl -s localhost:8080/stats
```
```json
{
  "nodes": {"Repository":1,"Module":5,"Component":12,"Task":14,
            "Sprint":2,"WorkflowState":4,"Developer":6,"Team":2},
  "edges": {"CONTAINS":17,"DEPENDS_ON":17,"SUBTASK_OF":2,"NEXT":4,
            "BLOCKS":3,"HAS_STATE":14,"IN_SPRINT":14,"MEMBER_OF":6,
            "ASSIGNED_TO":16,"TOUCHES":13}
}
```

### Run a query

`POST /query` takes `{"query": "<cypher>", "params": {<optional>}}` and returns
`{"columns": [...], "rows": [{...}]}`. Because some queries below contain Cypher
string literals (`'done'`), the examples post the body via a quoted heredoc so
the shell leaves it untouched:

```sh
curl -s localhost:8080/query -d @- <<'JSON'
{"query":"MATCH (u:Developer) RETURN u.key AS dev ORDER BY dev"}
JSON
```

A write is durable the moment the response returns:

```sh
curl -s localhost:8080/query -d @- <<'JSON'
{"query":"CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'}) RETURN d.key AS key"}
JSON
```
```json
{"columns":["key"],"rows":[{"key":"dev:zoe"}]}
```

---

## The maintenance-query catalogue

These eight queries are the point of the example: each is a real software-
maintenance question answered over the multi-layer graph. Outputs below are the
actual responses against the seeded fixture.

### Q1 — Change impact / blast radius

*If `comp:platform/config.go` changes, which components are affected, and which
tasks and developers must be told?*

```cypher
MATCH (x:Component {key:$k})
MATCH (affected:Component)-[:DEPENDS_ON*0..]->(x)
OPTIONAL MATCH (dev:Developer)-[:ASSIGNED_TO]->(t:Task)-[:TOUCHES]->(affected)
RETURN affected.key AS component,
       collect(DISTINCT t.key)   AS impactedTasks,
       collect(DISTINCT dev.key) AS impactedDevelopers
ORDER BY component
```
```sh
curl -s localhost:8080/query -d @- <<'JSON'
{"query":"MATCH (x:Component {key:$k}) MATCH (affected:Component)-[:DEPENDS_ON*0..]->(x) OPTIONAL MATCH (dev:Developer)-[:ASSIGNED_TO]->(t:Task)-[:TOUCHES]->(affected) RETURN affected.key AS component, collect(DISTINCT t.key) AS impactedTasks, collect(DISTINCT dev.key) AS impactedDevelopers ORDER BY component","params":{"k":"comp:platform/config.go"}}
JSON
```
`config.go` is the most foundational file, so **all 12 components** come back.
First rows:
```json
{"component":"comp:api/handlers.go","impactedTasks":["task:WS-8"],"impactedDevelopers":["dev:alice"]}
{"component":"comp:catalog/repository.go","impactedTasks":["task:WS-4"],"impactedDevelopers":["dev:bob","dev:alice"]}
```

### Q2 — Code ownership ("who knows this code")

```cypher
MATCH (dev:Developer)-[:ASSIGNED_TO]->(:Task)-[r:TOUCHES]->(c:Component {key:$k})
RETURN dev.key AS developer, count(r) AS touches, sum(r.churn) AS totalChurn
ORDER BY totalChurn DESC
```
```json
{"columns":["developer","touches","totalChurn"],
 "rows":[{"developer":"dev:erin","touches":1,"totalChurn":120},
         {"developer":"dev:carol","touches":1,"totalChurn":25}]}
```

### Q3 — Bus-factor risk sweep

*Which components have been touched by exactly one developer?*

```cypher
MATCH (dev:Developer)-[:ASSIGNED_TO]->(:Task)-[:TOUCHES]->(c:Component)
WITH c, count(DISTINCT dev) AS busFactor, collect(DISTINCT dev.key) AS owners
WHERE busFactor = 1
RETURN c.key AS component, busFactor, owners
ORDER BY component
```
Nine components are owned by a single developer, e.g.:
```json
{"component":"comp:orders/service.go","busFactor":1,"owners":["dev:bob"]}
{"component":"comp:payments/gateway.go","busFactor":1,"owners":["dev:frank"]}
{"component":"comp:platform/auth.go","busFactor":1,"owners":["dev:dave"]}
```

### Q4 — A developer's workload

*What is assigned to Alice and not yet done?*

```cypher
MATCH (d:Developer {key:$dev})-[a:ASSIGNED_TO]->(t:Task)
WHERE t.status <> 'done' AND a.state <> 'done'
RETURN t.key AS task, t.title AS title, t.status AS status, t.priority AS priority, a.role AS role
ORDER BY priority DESC
```
```json
{"rows":[
  {"task":"task:WS-9","title":"Add product search to catalog","status":"in_progress","priority":7,"role":"author"},
  {"task":"task:WS-13","title":"Add catalog caching layer","status":"todo","priority":6,"role":"author"}]}
```

### Q5 — What is blocked, and why (transitive chain)

```cypher
MATCH path = (root:Task)-[:BLOCKS*1..]->(t:Task {key:$task})
WHERE root.status <> 'done'
RETURN [n IN nodes(path) | n.key] AS chain
ORDER BY length(path) DESC
```
```json
{"rows":[
  {"chain":["task:WS-14","task:WS-10","task:WS-12"]},
  {"chain":["task:WS-10","task:WS-12"]}]}
```

### Q6 — Most-depended-upon component

```cypher
MATCH (c:Component)<-[:DEPENDS_ON]-(dependent:Component)
RETURN c.key AS component, count(dependent) AS inDegree
ORDER BY inDegree DESC
LIMIT 10
```
```json
{"rows":[
  {"component":"comp:platform/config.go","inDegree":3},
  {"component":"comp:catalog/service.go","inDegree":2},
  {"component":"comp:platform/logging.go","inDegree":2}]}
```

### Q7 — Dependency cycles

```cypher
MATCH (c:Component)-[:DEPENDS_ON*1..]->(c)
RETURN DISTINCT c.key AS componentOnCycle
ORDER BY componentOnCycle
```
```json
{"rows":[
  {"componentOnCycle":"comp:orders/service.go"},
  {"componentOnCycle":"comp:payments/service.go"}]}
```

### Q8 — Component history

```cypher
MATCH (dev:Developer)-[:ASSIGNED_TO]->(t:Task)-[r:TOUCHES]->(c:Component {key:$k})
RETURN dev.key AS developer, t.key AS task,
       r.change_type AS change, r.churn AS churn, r.at AS at
ORDER BY at DESC
```
```json
{"rows":[
  {"developer":"dev:carol","task":"task:WS-2","change":"modify","churn":25,"at":"2026-01-15T16:30:00Z"},
  {"developer":"dev:erin","task":"task:WS-1","change":"add","churn":120,"at":"2026-01-08T16:00:00Z"}]}
```

---

## Persistence — survives a crash

Every committed write is fsynced to the WAL before the response returns, so the
data survives even a hard kill:

```sh
go run ./examples/25_software_house_api -d ./data -addr :8080 &
curl -s -XPOST localhost:8080/seed
curl -s localhost:8080/query -d @- <<'JSON'
{"query":"CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'})"}
JSON

kill -9 %1                       # crash: no graceful shutdown, no final snapshot

go run ./examples/25_software_house_api -d ./data -addr :8080 &
curl -s localhost:8080/stats     # "Developer": 7 — the seed AND dev:zoe survived
```

On restart, recovery replays the WAL on top of the last snapshot. A graceful
SIGTERM additionally writes a fresh snapshot to shorten the next replay. Both
paths are covered by `cross_process_test.go`.

The data directory holds `wal` (the log) and `snapshot/` (manifest plus the
CSR, labels, properties and mapper images).

---

## Concurrency

The store is opened once and shared by all handlers. Read queries run in
parallel; write queries and the seed are serialised. The server takes a
`sync.RWMutex` (read lock for readers, write lock for writers) as the outermost
lock because the engine's plan-building phase reads live graph structures that a
concurrent write mutates — see the note in `server.go`. The result is
snapshot-isolation reads with serialised writes; `server_concurrency_test.go`
exercises it under `-race`.

---

## Tests

```sh
go test ./examples/25_software_house_api/...            # short layer (~3s)
go test -race ./examples/25_software_house_api/...      # race detector
```

Coverage: deterministic seed and the full query catalogue (`seed_test.go`),
every endpoint and error path (`server_test.go`), concurrent readers/writers
(`server_concurrency_test.go`), in-process reopen (`lifecycle_test.go`), and a
real process restart under both SIGTERM and `kill -9` (`cross_process_test.go`).
`go.uber.org/goleak` guards against goroutine leaks via `main_test.go`.
