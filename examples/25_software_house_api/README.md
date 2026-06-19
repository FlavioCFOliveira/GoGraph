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
# from the repository root — small deterministic fixture
go run ./examples/25_software_house_api -d ./data -addr :8080

# observable-scale run: a seeded synthetic graph (~5.7k nodes, ~19k edges)
go run ./examples/25_software_house_api -d ./big -addr :8081 \
    -scale-components 2000 -scale-tasks 1500 -scale-developers 80 -scale-seed 7
```

`-d` is the data directory (created if missing); `-addr` is the listen address
(default `:8080`). Press Ctrl-C (SIGINT) or send SIGTERM for a graceful
shutdown that writes a final snapshot and closes the WAL.

### Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-d <dir>` | Data directory (WAL + snapshot) | *(required)* | — |
| `-addr <host:port>` | HTTP listen address | `:8080` | — |
| `-scale-components <n>` | Extra synthetic `:Component` nodes | `0` (bare fixture) | `2000` |
| `-scale-tasks <n>` | Extra synthetic `:Task` nodes | `0` | `1500` |
| `-scale-developers <n>` | Extra synthetic `:Developer` nodes | `0` | `80` |
| `-scale-seed <s>` | RNG seed fixing the synthetic shape | `1` | `7` |

With every `-scale-*` flag at `0` the server loads exactly the **46-node /
106-edge** hand-authored fixture below — the small deterministic default the
regression tests pin. Setting any `-scale-*` flag grows the *same* multi-layer
model with a **seeded, reproducible** synthetic layer (keys prefixed `syn:`, so
no hand-authored key is ever touched), up to a size where query latency, live
heap, and bytes-per-element become observable. The same scale can be requested
per call via the `POST /seed` body (see below). The synthetic topology is
described in [`synth.go`](synth.go): a Price-model power-law dependency DAG with
a bounded number of injected cycles, ownership clustered by developer
home-module affinity (so bus-factor risk is realistic), and shallow `BLOCKS`
chains — so every maintenance query stays meaningful at scale.

---

## Endpoints

| Method & path | Purpose |
|---|---|
| `POST /query` | Run an arbitrary Cypher statement (read or write). |
| `POST /seed`  | Idempotently load the fixture, optionally at a synthetic scale. |
| `GET /stats`  | Node/edge counts (facts) **plus** a volatile `telemetry` object. |
| `GET /healthz`| Liveness probe. |

### Seed the graph

```sh
curl -s -XPOST localhost:8080/seed
```
```json
{"seeded":true,"status":"ok","scale_components":0,"scale_tasks":0,"scale_developers":0}
```

Running it again is a no-op (`"seeded":false`). The default fixture is
**46 nodes** and **106 edges** across the three layers.

To grow it with a seeded synthetic layer, post the scale in the body:

```sh
curl -s localhost:8080/seed -d '{"scale_components":2000,"scale_tasks":1500,"scale_developers":80,"scale_seed":7}'
```
```json
{"seeded":true,"status":"ok","scale_components":2000,"scale_tasks":1500,"scale_developers":80}
```

The synthetic layer commits in the **same atomic transaction** as the base
fixture, so it is applied exactly once and survives a `kill -9` (the next reopen
replays the whole seed from the WAL). A given `scale_seed` reproduces the same
graph byte-for-byte.

### Inspect the counts and telemetry

```sh
curl -s localhost:8080/stats
```

The response splits **deterministic facts** (`nodes`, `edges`) from **volatile
telemetry** (`telemetry`). The facts are reproducible for a fixed seed and
scale; every telemetry field varies per run and per machine — the JSON analogue
of the `# ` telemetry convention the non-server examples use.

```json
{
  "nodes": {"Repository":1,"Module":5,"Component":12,"Task":14,
            "Sprint":2,"WorkflowState":4,"Developer":6,"Team":2},
  "edges": {"CONTAINS":17,"DEPENDS_ON":17,"SUBTASK_OF":2,"NEXT":4,
            "BLOCKS":3,"HAS_STATE":14,"IN_SPRINT":14,"MEMBER_OF":6,
            "ASSIGNED_TO":16,"TOUCHES":13},
  "telemetry": {
    "heap_alloc_bytes": 10590968, "heap_alloc_human": "10.10 MiB",
    "heap_sys_bytes": 45449216, "num_gc": 19,
    "bytes_per_element": 461.0,
    "query_count": 0, "write_count": 0, "stats_count": 1,
    "last_query_ms": 0, "max_query_ms": 0,
    "stats_sweep_ms": 80.72, "seed_ms": 62.83
  }
}
```

(The `telemetry` values above are illustrative — they change every run.)

The server's own startup log on stderr follows the same split for a scaled run:

```
seed.scale_components=2000           # fact — reproducible for a fixed seed
seed.scale_tasks=1500                # fact
# seed.elapsed=63ms                  # telemetry — varies, never pinned
# seed.node_rate=59896 nodes/s       # telemetry
# mem.heap_alloc=9.46 MiB            # telemetry
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
parallel; write queries and the seed are serialised. The store owns a
`sync.RWMutex` (a shared hold for readers, an exclusive hold for writers) as the
outermost lock because the engine's plan-building phase reads live graph
structures that a concurrent write mutates — see the note in `store.go`. The
hold is kept across the engine call, the row drain, and `Result.Close`. The
result is snapshot-isolation reads with serialised writes;
`server_concurrency_test.go` exercises it under `-race`.

`Close` takes the same exclusive hold before releasing the WAL, so it quiesces
any in-flight write rather than closing the WAL underneath a commit that is
about to fsync — which would otherwise leave that write applied in memory but
lost from the WAL. After `Close` takes the hold it marks the store closed, so a
request that arrives during shutdown is cleanly rejected with `503` instead of
being admitted onto the closing WAL. `close_quiesce_test.go` proves the
quiesce-and-reject contract under `-race`.

---

## Evidence it collects

Following the [examples standard](../../docs/examples-standard.md), this example
is both a demonstration and a source of evidence. It reports the dimensions that
matter for a persistent Cypher service over a graph structure:

- **Live heap and bytes-per-element** — `GET /stats` forces a GC and reports
  `telemetry.heap_alloc_bytes` and `telemetry.bytes_per_element` (live heap
  divided by node+edge count), the structures evidence for the in-memory LPG.
- **Per-endpoint latency** — `telemetry.last_query_ms`, `max_query_ms`, and
  `stats_sweep_ms` (the sweep the response itself measured), plus request
  counters (`query_count`, `write_count`, `stats_count`).
- **Seed/build cost** — `telemetry.seed_ms` and the startup `# seed.elapsed` /
  `# seed.node_rate` telemetry lines.

When you scale the graph up (`-scale-*`), watch `bytes_per_element` settle
(~460 B per element at the per-edge-property scale this example uses) and the
catalogue queries' latency grow — in particular the **unbounded** variable-length
queries (Q1's `DEPENDS_ON*0..`, Q7's `DEPENDS_ON*1..`) become expensive on a
dense dependency graph, which is itself evidence worth observing.

## Tests

```sh
go test ./examples/25_software_house_api/...            # short layer (~5s)
go test -race ./examples/25_software_house_api/...      # race detector
```

Coverage: deterministic seed and the full query catalogue (`seed_test.go`); the
opt-in synthetic scale — zero-scale equals the bare fixture, pinned
deterministic counts at a fixed seed, structural invariants, and the realism
properties the queries depend on (`synth_test.go`); every endpoint and error
path including the scaled `POST /seed` and the `GET /stats` telemetry block
(`server_test.go`, `synth_test.go`); concurrent readers/writers
(`server_concurrency_test.go`); the Close quiesce-and-reject contract against a
concurrent write (`close_quiesce_test.go`); in-process reopen
(`lifecycle_test.go`); and a real process restart under both SIGTERM and
`kill -9` (`cross_process_test.go`). The synthetic tests assert only
deterministic facts (counts, structure, presence of telemetry fields) — never
volatile latency or heap values. `go.uber.org/goleak` guards against goroutine
leaks via `main_test.go`.
