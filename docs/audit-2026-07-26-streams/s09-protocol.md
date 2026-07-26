# Stream 9 — Protocol, drivers, public API, import/export and interoperability

Baseline: `6f31f61` (v0.10.0). All measurements on **Apple M4, 10 cores, darwin/arm64, go1.26.5**.
Driver under test: **`github.com/neo4j/neo4j-go-driver/v5 v5.28.4`** (already a repo dependency, `go.mod:12`).
Probe sources: `/private/tmp/.../scratchpad/audit3/probe/` (`main.go`, `probe2/`, `probe3/`, `probe4/`, `wire/`, `enc/`, `io/`).

## Verdict summary

GoGraph's Bolt **framing, state machine, security envelope and version reach** are genuinely good — it negotiates
Bolt 5.0–5.6 (`bolt/proto/handshake.go:32`) where **Memgraph tops out at 5.2**, its chunker handles NOOP, budgets,
and oversize messages, and its TLS baseline is hardened by default. But its **value layer is not Bolt**. I confirmed
round 1's T1.1 finding at the byte level with a raw wire capture, and then went further: I ran the official Neo4j Go
driver against the server and got **13 hard failures out of 37 checks**. The wire-format break is only the first of
five *independent* value-layer gaps, and the second one — **the server never sends `stats`, so every write counter
the driver reports is zero** — is arguably worse in practice than the first, because it silently returns wrong data
rather than a wrong type. A third, `db` metadata being absent, makes `summary.Database().Name()` **panic with a nil
pointer dereference** inside the driver.

The single most valuable lever is unchanged from round 1 — **emit Node(0x4E)/Relationship(0x52)/UnboundRelationship(0x72)/Path(0x50)
structures with `element_id`** — and I can now *strengthen* round 1's "zero risk" claim with three new pieces of
evidence: (a) the change touches exactly **one** production file (`bolt/server/session.go`); (b) `cypher/tck/` has
**zero** dependency on `bolt/` (`go list -deps ./cypher/tck/... | grep -c bolt` → `0`), so TCK impact is structurally
impossible; and (c) the struct encoding is **19.8 % smaller on the wire and 28.8 % faster to encode** than the map it
replaces — it is a compatibility fix that is also a performance win. Round 1's risk assessment is **confirmed and
upgraded**.

Where GoGraph is genuinely ahead: Bolt version reach vs Memgraph, un-gated Prometheus metrics (Neo4j's metrics
subsystem is `[role=enterprise-edition]`; Memgraph's `:9091` metrics server is Enterprise-only), fail-stop import
error handling with row numbers, and a measured **4.85 M edge-rows/s** CSV ingest.

**But one finding outranks everything above.** Loading 20 000 nodes + 200 000 edges through the Cypher write path
takes **35 min 33 s** (**103 entities/s**) against Memgraph's **977 ms** — a **≈ 2 180×** disadvantage. GoGraph
*already has* a 4.18 M edges/s parallel bulk loader that would do the same edge volume in ~48 ms, and it is
**completely unreachable from Cypher**: `grep -rn "bulk\." cypher/ bolt/` returns nothing, there is no import
procedure, and `LOAD CSV` does not parse. The fast path exists and is walled off from the query language. That is
**F17**, and it is the finding that determines whether anyone gets far enough to care about the wire format.

---

## Feature-by-feature comparison

| Feature | GoGraph (`file:line`) | Neo4j | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| Bolt versions offered | 5.6→5.0, 4.4 (`bolt/proto/handshake.go:32-35`) | 3→6.0 (`docs-bolt` compat matrix) | 1, 4.0, 4.1, 4.3, **5.2** max | WORSE than Neo4j, **BETTER than Memgraph** | NEW |
| Node/Rel/Path on the wire | **PackStream maps** (`bolt/server/session.go:1732,1742,1758`) | Structs `0x4E/0x52/0x72/0x50` | Structs (Neo4j-compatible) | **WORSE than both** | CONFIRMED-R1 |
| `element_id` on the wire | absent (no struct to carry it); `elementId()` *function* exists (`cypher/funcs/essentials.go:120`) | 4th Node field since Bolt 5.0 | n/a (Bolt 5.2 = has it) | WORSE | R1 partly **STALE** (function exists) |
| Temporal values **outbound** | correct Structs `0x44/0x74/0x54/0x64/0x49/0x69/0x46/0x66/0x45` (`bolt/server/session.go:1787-1846`) | same | same | **PARITY** | NEW |
| Temporal/Point/Bytes **inbound** (params) | **rejected**, `Neo.ClientError.Statement.TypeError` (`cypher/api.go:3372`) | accepted | accepted | **WORSE than both** | NEW |
| Write counters (`stats`) | never sent (`bolt/server/session.go:1060-1068`); no `Result.Counters()` either | full counter set | full counter set | **WORSE than both** | NEW |
| `db` in SUCCESS metadata | absent → driver `Database()` returns nil → **panic** | sent since Bolt 4.0 | sent | **WORSE than both** | NEW |
| `type` (`r`/`w`/`rw`/`s`) | absent → `StatementTypeUnknown` | sent | sent | WORSE | NEW |
| `t_first` / `t_last` | absent → driver reports `-1ms` | sent | sent | WORSE | NEW |
| `plan` / `profile` (EXPLAIN/PROFILE) | query prefixes **rejected as syntax errors** | full | full | WORSE | NEW |
| `notifications` vs `statuses` at 5.6 | sends `notifications` (`session.go:1066`) — 5.5-era key | `statuses` from 5.6 | n/a (max 5.2) | WORSE (spec deviation) | NEW |
| `qid` / multiple streams per tx | single stream, `qid` always `-1` (`session.go:899,912`) | multi-stream | multi-stream | WORSE (driver masks it) | CONFIRMED-R1 |
| `ROUTE` | single-host table (`bolt/server/route.go`) — `neo4j://` works | cluster routing (Enterprise) | replication-aware | **appropriate** for an embeddable | — |
| NOOP keep-alive | received & skipped (`bolt/proto/chunking.go:184`), **never sent** | sends when busy (Bolt 4.3+) | — | WORSE | NEW |
| `connection.recv_timeout_seconds` hint | `hints: {}` (`session.go:622`) → pooled-conn EOF | sends it | — | WORSE | NEW |
| `imp_user` | **silently ignored** | honoured (Enterprise) or rejected | — | WORSE (security-relevant) | NEW |
| `TELEMETRY` (0x54) | not implemented → `Request.Invalid` | implemented (5.4+) | n/a | LOW risk (driver gates on hint) | NEW |
| Handshake manifest (5.7) | not implemented; legacy slots parsed correctly | implemented | n/a | LOW risk today | NEW |
| Chunking / message caps | 64 KiB chunk, 16 MiB msg default (`bolt/proto/chunking.go:15,24`) | comparable | comparable | PARITY | — |
| TLS | hardened `DefaultTLSConfig()` + hot cert reload (`bolt/server/tls.go`, `tls_reload.go`) | SSL policies | SSL config | PARITY | — |
| CSV import expressiveness | **edge list only**, no labels/props, returns `*adjlist.AdjList` | header files with `:ID/:LABEL/:START_ID/:TYPE` | LOAD CSV → full Cypher | **WORSE than both** | NEW |
| `LOAD CSV` clause | **not parseable** (measured) | yes | yes (primary import path) | WORSE | CONFIRMED-R1 (T3) |
| Bulk ingest throughput (raw engine) | **4.18 M edges/s** parallel (measured) | `neo4j-admin` offline, empty-DB only | ~1 M objects/s documented | **BETTER on raw rate**, worse on richness | NEW |
| **Ingest rate a user can actually reach** | **103 entities/s** via Cypher; fast path unreachable (`grep -rn "bulk\." cypher/ bolt/` → ∅) | `LOAD CSV` + `CALL {} IN TRANSACTIONS` + admin importer | `LOAD CSV`, **977 ms** for the same 220 k entities | **WORSE than both — ≈ 2 180× vs Memgraph** | **NEW (F17)** |
| `CALL { } IN TRANSACTIONS` | `SyntaxError` (measured) | yes (replaced `USING PERIODIC COMMIT`) | yes | WORSE than both | NEW |
| Typed Go slices/maps as params | **all rejected** — only `[]any`/`map[string]any` (`cypher/api.go:3389`) | driver accepts typed | driver accepts typed | WORSE (embedded API only) | NEW |
| Bolt round-trip tax (in-process) | +46 µs write / +88 µs lookup vs embedded | n/a (no embedded mode) | n/a (no embedded mode) | informational — embedded is the fast path | NEW |
| Import error policy | fail-stop with row number (`graph/io/csv/reader.go:193,208,214`) | `--bad-tolerance` (default tolerant) | aborts tx | **BETTER on correctness**, worse on ergonomics | NEW |
| Prometheus metrics | `metrics.NewPrometheusRegistry().Handler()`, zero-dep, **no licence gate** | metrics = Enterprise | `:9091` = **Enterprise** | **BETTER than both Community editions** | CONFIRMED-R1 |
| HTTP / Query API | none | Query API `/db/{db}/query/v2` | none (WS logs + Enterprise metrics) | **appropriate** (see F16) | NEW |

---

## The empirical driver-compatibility matrix (the headline evidence)

`go run .` in `scratchpad/audit3/probe/` — 37 checks, **PASS=20 FAIL=13 DEGRADED=2 INFO=2**:

```
PASS       driver.VerifyConnectivity                       ok
PASS       driver.GetServerInfo (agent/protocol)           agent="GoGraph/1.0" bolt=5.6
FAIL       RETURN n  -> neo4j.Node                         got map[string]interface {}: map[id:140 labels:[Person Admin] properties:map[age:42 name:alice]]
FAIL       GetRecordValue[neo4j.Node]                      expected dbtype.Node but found map[string]interface {}
FAIL       RETURN r  -> neo4j.Relationship                 got map[string]interface {}
FAIL       RETURN p  -> neo4j.Path                         got map[string]interface {}
INFO       elementId(n) string form                        string = 140
FAIL       ResultSummary.Counters (write stats)            ContainsUpdates=false NodesCreated=0 PropertiesSet=0 LabelsAdded=0
FAIL       ResultSummary.StatementType                     StatementTypeUnknown
DEGRADED   ResultSummary timing (t_first/t_last)           available=-1ms consumed=-1ms
FAIL       ResultSummary.Database().Name()                 panic: runtime error: invalid memory address or nil pointer dereference
PASS       ResultSummary.Query().Text()                    "RETURN 1 AS x"
FAIL       ResultSummary.Plan() (EXPLAIN)                  Neo.ClientError.Statement.SyntaxError
FAIL       ResultSummary.Profile() (PROFILE)               Neo.ClientError.Statement.SyntaxError
PASS       ResultSummary.Notifications (cartesian)         1: Neo.ClientNotification.Statement.CartesianProductWarning
PASS       ResultSummary.GqlStatusObjects                  00000        (driver polyfill)
PASS       neo4j.ExecuteQuery + EagerResult                keys=[name] rows=2
PASS       neo4j.ExecuteQuery with database name           accepted db=neo4j
PASS       SessionConfig{DatabaseName:"neo4j"}             accepted
DEGRADED   SessionConfig{ImpersonatedUser}                 SILENTLY ACCEPTED — server ignores imp_user
PASS       ExecuteWrite managed tx                         ok
PASS       two open result streams in one tx (qid)         both streams delivered 3 rows   (driver buffers; server never sees 2 streams)
PASS       tx metadata / tx timeout / bookmarks / chaining ok
PASS       FetchSize=2 over 10 rows (PULL n batching)      10 rows across 5 PULLs
FAIL       temporal round-trip (Date/DateTime/Duration)    TypeError: unsupported parameter type packstream.Struct
FAIL       neo4j.Point2D round-trip                        TypeError: unsupported parameter type packstream.Struct
FAIL       []byte (PackStream Bytes) round-trip            TypeError: unsupported parameter type []uint8
PASS       nested list/map round-trip                      ok
PASS       syntax error -> Neo4jError code                 Neo.ClientError.Statement.SyntaxError
PASS       UNIQUE violation -> Neo4jError                  Neo.ClientError.Schema.ConstraintValidationFailed
PASS       FAILED-state recovery (RESET) after error       session reusable
PASS       neo4j:// routing driver (ROUTE msg)             routed query ok
```

`probe2` (write feedback + tooling):

```
FAIL  MERGE created-vs-matched observable?   NodesCreated first=0 second=0
FAIL  DELETE counters                        NodesDeleted=0
FAIL  index/constraint counters              IndexesAdded=0
FAIL  SHOW DATABASES / SHOW FUNCTIONS / SHOW PROCEDURES / SHOW TRANSACTIONS   SyntaxError
FAIL  CALL dbms.components() / dbms.showCurrentUser() / db.ping()             ProcedureNotFound
FAIL  USE neo4j RETURN 1  /  CYPHER 5 RETURN 1  /  CALL { } IN TRANSACTIONS   SyntaxError
PASS  SHOW INDEXES / SHOW CONSTRAINTS / db.labels() / db.relationshipTypes() / db.propertyKeys() / db.schema.visualization()
PASS  10k rows @ default FetchSize=1000      32.6 ms
PASS  8 goroutines x 50 queries              400 queries in 9 ms
```

### Raw wire capture (`go run ./wire`, no driver in the loop)

```
negotiated bolt 5.6  (raw 00000605)

MATCH (n:Person {name:'ann'}) RETURN n
  RECORD  b17191 a3 826964 c9008c 866c6162656c73 9186506572736f6e 8a70726f70657274696573 a1846e616d6583616e6e
                 ^^ TinyMap(3)      — Neo4j sends  B4 4E  (Structure, 4 fields, tag 'N')

MATCH ()-[r:KNOWS]->() RETURN r
  RECORD  b17191 a5 ...
                 ^^ TinyMap(5)      — Neo4j sends  B8 52  (Structure, 8 fields, tag 'R')

MATCH p=(:Person)-[:KNOWS]->() RETURN p
  RECORD  b17191 a2 856e6f646573 ... 8d72656c6174696f6e7368697073 ...
                 ^^ TinyMap(2)      — Neo4j sends  B3 50  (nodes, rels[0x72], indices)

RETURN date('2026-07-25') AS d
  RECORD  b17191 b144 c950b3
                 ^^^^ Structure(1), tag 0x44 'D', epochDay=20659   ← CORRECT, spec-conformant
```

The Date line is the decisive detail: **the PackStream Structure machinery is present, exercised and correct**
(`bolt/packstream/value.go:112-121`, `bolt/server/session.go:1787-1846`). Only the four graph-entity kinds bypass it.

---

## Findings

*Order note: F1–F13 are the Bolt/wire and import findings in the order they were investigated. **F17 (the
headline) and F18 sit immediately after F13** because they belong with the ingest discussion; F14–F16 (import error
handling, public API, HTTP) follow. Numbers are stable identifiers, not a priority order — for priority see the
[Prioritised lever list](#prioritised-lever-list), where **F17 is item 0**.*

### F1. Graph entities are PackStream maps, not Node/Relationship/UnboundRelationship/Path structures — and the struct form is *cheaper* [CONFIRMED-R1, with NEW quantification] (severity: **HIGH**)

- **What they do:** Bolt specifies `Node` = tag `4E`, 4 fields `(id, labels, properties, element_id)` (3 before 5.0);
  `Relationship` = tag `52`, 8 fields `(id, startNodeId, endNodeId, type, properties, element_id, start_node_element_id,
  end_node_element_id)` (5 before 5.0); `UnboundRelationship` = tag `72`, 4 fields; `Path` = tag `50`, 3 fields
  `(nodes, rels, indices)` where `indices` is a signed, 1-indexed rel / 0-indexed node interleaving that also encodes
  traversal direction. Source: `neo4j/docs-bolt`, `modules/ROOT/pages/bolt/structure-semantics.adoc`
  (fetched via `gh api`; lines 88–235). Memgraph implements the same structures up to Bolt 5.2.
- **What GoGraph does:** `bolt/server/session.go:1723-1736` (Node → `map{"id","labels","properties"}`),
  `:1737-1748` (Relationship → `map{"id","start","end","type","properties"}`), `:1749-1761`
  (Path → `map{"nodes","relationships"}`). No `element_id`, no `indices`, no `UnboundRelationship`.
- **Evidence:**
  1. Raw wire capture above: `a3` / `a5` / `a2` TinyMap markers where `b4 4e` / `b8 52` / `b3 50` belong.
  2. Driver matrix: 4 hard failures — `neo4j.Node`, `neo4j.Relationship`, `neo4j.Path` and the typed getter
     `neo4j.GetRecordValue[dbtype.Node]` all fail. Every `.Labels`, `.Props`, `.ElementId`, `.StartElementId`,
     `.GetId()` accessor is unreachable; callers get `map[string]any`.
  3. **NEW measurement** — encoding a representative node (id + 2 labels + 3 properties),
     `-count=10`, median of `BenchmarkEncodeNode` (`probe/enc`):

     | form | wire bytes | ns/op |
     |---|---|---|
     | map (current) | **81** | **140.30** |
     | `Struct 0x4E`, 3 fields (Bolt 4.x) | 61 (**−24.7 %**) | 92.64 (**−34.0 %**) |
     | `Struct 0x4E`, 4 fields + `element_id` (Bolt 5.x) | 65 (**−19.8 %**) | 99.84 (**−28.8 %**) |

     Both forms are 0 B/op, 0 allocs/op. The map pays 21 bytes of repeated key strings per node
     (`"id"`+`"labels"`+`"properties"`) and 29 per relationship; the struct pays a positional header instead.
  4. Path loses information today: without `indices` a client cannot recover traversal direction for a
     relationship traversed against its stored direction except by comparing endpoint ids itself.
- **Lever:** in `exprValueToPackstream`, replace the three map literals with
  `packstream.Struct{Tag: 0x4E|0x52|0x50, Fields: …}`, gated on `boltMajor` for the `element_id` fields
  (exactly the pattern `dateTimeToPackstream` already uses at `session.go:1825`). Emit `UnboundRelationship`
  (`0x72`) inside `Path` and compute `indices`. `element_id` can be `strconv.FormatInt(id, 10)` — Bolt treats it as
  opaque, and GoGraph's `elementId()` function already returns exactly that string
  (`cypher/funcs/essentials.go:120`, measured: `elementId(n)` → `"140"`), so the wire and the language agree.
- **TCK/ACID impact:** **structurally none.** `go list -deps ./cypher/tck/... | grep -c bolt` → `0`: the TCK runner
  never touches the Bolt layer. No storage, WAL, snapshot or transaction code is involved. The change surface in
  production code is **one file** (`grep -rln 'exprValueToPackstream' --include='*.go' . | grep -v _test` →
  `bolt/server/session.go`). **Round 1's "zero risk" is confirmed and strengthened.** The honest caveat round 1
  omitted: it *is* a breaking change to the current (undocumented) Bolt contract, and **13 test files** are coupled
  to the map shape (measured: `grep -rln '"labels"\|"relationships"\|"properties"\|asNodeMap' bolt/server/*_test.go`
  → `e2e_helpers_test.go`, `e2e_return_node_test.go`, `e2e_return_path_test.go`, `e2e_return_relationship_test.go`,
  `e2e_create_test.go`, `e2e_create_relationship_test.go`, `e2e_match_return_test.go`, `e2e_set_remove_test.go`,
  `node_value_rapid_test.go`, `path_value_rapid_test.go`, `rel_value_rapid_test.go`, `session_test.go`,
  `smoke_test.go`) — the e2e ones get *simpler*, since they can then assert on `neo4j.Node` directly; the three
  `*_rapid_test.go` property tests need their generators re-pointed at the struct shape.
- **Effort:** **M** (≈300 LOC production + test rewrite; `indices` is the only non-trivial part).

### F2. The server never sends `stats`, so every driver-reported write counter is zero [NEW] (severity: **HIGH**)

- **What they do:** Bolt SUCCESS on PULL/DISCARD may carry `stats::Dictionary` — "counter information"
  (`docs-bolt`, `bolt/message.adoc`, introduced in Bolt 3). Neo4j's Query API exposes the same set over HTTP
  (`docs-query-api/query-counters.adoc`, `includeCounters: true`). Memgraph likewise populates the driver's counters.
- **What GoGraph does:** PULL SUCCESS metadata is `{has_more}` plus, at end of stream, `{bookmark, notifications}`
  — `bolt/server/session.go:1060-1068`. DISCARD is `{has_more, bookmark}` — `:1148-1152`. There is no `stats` key
  anywhere in `bolt/server`. The gap is not confined to Bolt: `cypher.Result` exposes
  `Close/Columns/Err/IsClosed/Next/Notifications/Record/RowAt/ValueAt` and **no counters accessor** (`go doc -all
  github.com/FlavioCFOliveira/GoGraph/cypher.Result`).
- **Evidence:** measured, `probe2`:
  ```
  MERGE (n:M {k:1})  twice  → NodesCreated first=0 second=0   (an app CANNOT tell create from match)
  MATCH (n:Del) DELETE n    → NodesDeleted=0
  CREATE INDEX …            → IndexesAdded=0
  CREATE (:Tmp {k:1})       → ContainsUpdates=false, NodesCreated=0, PropertiesSet=0, LabelsAdded=0
  ```
  This is worse than a type error: the driver returns *plausible-looking wrong numbers*. `ContainsUpdates()` — the
  standard "did my write do anything?" predicate — reports `false` for a successful `CREATE`. The repo's own
  `bolt/server/e2e_helpers_test.go:20-22` already documents this ("All `Counters()` fields therefore return 0").
- **Lever:** thread a per-statement side-effect counter set through the write operators and surface it as
  (a) `stats` in the terminal PULL/DISCARD SUCCESS and (b) a `Result.Counters()` accessor. **Do not** reuse
  `lpg.Graph.SideEffectCounters()` (`graph/lpg/lpg.go:1924`): those are graph-*global* monotone `atomic.Uint64`
  counters that the TCK reads as before/after deltas — correct for a single-threaded TCK scenario, wrong for a
  per-statement figure under concurrency. Counters must be per-statement, and they must be **rolled back with the
  undo log** so an aborted statement reports nothing (the same invariant the TCK already depends on).
  Minimum useful set: `nodes-created`, `nodes-deleted`, `relationships-created`, `relationships-deleted`,
  `properties-set`, `labels-added`, `labels-removed`, `indexes-added/removed`, `constraints-added/removed`.
- **TCK/ACID impact:** TCK-neutral (the TCK asserts side effects from its own graph-level snapshot, and this adds a
  parallel per-statement channel rather than changing the existing one). **ACID-sensitive**: the counters must be
  part of the statement's atomic unit — increment inside the same critical section as the mutation and decrement on
  undo — otherwise a rolled-back statement reports phantom writes. That is a real constraint but a well-understood
  one; the undo-log discipline already exists.
- **Effort:** **M–L** (plumbing crosses `cypher/exec` write operators, the undo log, and `bolt/server`).

### F3. Missing `db` metadata makes the official driver panic [NEW] (severity: **HIGH**, trivial fix)

- **What they do:** `db::String` — "the database name where the query was executed" — has been part of the PULL
  SUCCESS summary since Bolt 4.0 (`docs-bolt`, `bolt/message.adoc`). Bolt 5.8 additionally requires it on
  BEGIN/RUN SUCCESS.
- **What GoGraph does:** never sends it (`bolt/server/session.go:1060-1068`).
- **Evidence:** measured. `neo4j-go-driver` returns a **nil interface** when `db` is empty
  (`resultsummary.go:499-505`: `if database == "" { return nil }`), so the documented, ordinary call
  `summary.Database().Name()` faults:
  ```
  FAIL  ResultSummary.Database().Name()  panic: runtime error: invalid memory address or nil pointer dereference
  ```
  An application crash caused by a **missing map key**. GoGraph already *accepts* `db` in HELLO/RUN/BEGIN extras
  (both `SessionConfig{DatabaseName:"neo4j"}` and `ExecuteQueryWithDatabase("neo4j")` PASS) — it just never echoes it.
- **Lever:** add `meta["db"] = <configured database name>` (a new `Options.DatabaseName`, default `"neo4j"`) to the
  terminal PULL/DISCARD SUCCESS. Echoing the `db` the client asked for is the minimum; a real name is better.
- **TCK/ACID impact:** none whatsoever — one metadata key on one message.
- **Effort:** **S** (under 20 lines). This is the single highest value-per-line item in the whole stream.

### F4. Missing `type`, `t_first`, `t_last` summary metadata [NEW] (severity: MEDIUM)

- **What they do:** `type::String` (`"r"`/`"w"`/`"rw"`/`"s"`), `t_first::Integer`, `t_last::Integer` — all Bolt 3+
  (`docs-bolt`, `bolt/message.adoc`).
- **What GoGraph does:** RUN SUCCESS is `{fields, qid}` only (`bolt/server/session.go:896-901`); PULL SUCCESS carries
  no timings or type.
- **Evidence:** measured — `StatementTypeUnknown`; `ResultAvailableAfter()` and `ResultConsumedAfter()` both `-1ms`.
  Every Neo4j-ecosystem dashboard, APM integration and the `neo4j-go-driver`'s own metrics surface read these.
  Round 1's T2 item "Bolt-path latency histogram" is the *server-side* half of the same gap; `t_first`/`t_last` is
  the client-side half and is far cheaper.
- **Lever:** stamp `time.Now()` at RUN entry and at cursor drain; emit `t_first` in RUN SUCCESS and `t_last` + `type`
  in the terminal PULL SUCCESS. `type` is derivable from the IR the planner already builds (read-only vs has-writes
  vs DDL).
- **TCK/ACID impact:** none.
- **Effort:** **S**.

### F5. Temporal, spatial and byte-array *parameters* are rejected — the outbound/inbound asymmetry [NEW] (severity: **HIGH**)

- **What they do:** every Bolt server accepts the same structures inbound that it emits outbound. Drivers send
  `dbtype.Date` → `Structure(0x44)`, `time.Time` → `0x49/0x69`, `dbtype.Duration` → `0x45`, `[]byte` → PackStream
  Bytes, `dbtype.Point2D` → `0x58`.
- **What GoGraph does:** `cypher.BindParams` (`cypher/api.go:3372`, worker `bindAny` at `:3389`) handles only
  `nil, bool, string, []any, map[string]any, expr.Value` and the numeric primitives. A decoded
  `packstream.Struct` or a `[]byte` falls to the default arm and returns `ErrUnsupportedParamType`
  (`cypher/api.go:3355`).
- **Evidence:** measured, three separate failures:
  ```
  RETURN $d  (dbtype.Date)      → Neo.ClientError.Statement.TypeError: unsupported parameter type packstream.Struct
  RETURN $p  (dbtype.Point2D)   → Neo.ClientError.Statement.TypeError: unsupported parameter type packstream.Struct
  RETURN $b  ([]byte{1,2,3})    → Neo.ClientError.Statement.TypeError: unsupported parameter type []uint8
  ```
  Meanwhile the *outbound* direction is fully correct (`RETURN date('2026-07-25')` → `b1 44 c9 50b3`, verified
  byte-for-byte). So a client can read a date out of GoGraph but **cannot write one back in as a parameter** — the
  most basic round-trip an application performs. `Point2D` is a genuine type gap (no spatial type exists anywhere in
  the engine — `grep -rn "PointValue\|Point2D" cypher/ graph/` → nothing); `Date`/`DateTime`/`Duration`/`Bytes` are
  *pure plumbing omissions*, because the corresponding `expr.Value` kinds already exist.
- **Lever:** add a Bolt-layer parameter adapter (in `bolt/server`, **not** in `cypher`, to keep the engine free of a
  protocol dependency) that maps the temporal tags `0x44/0x54/0x74/0x64/0x49/0x69/0x46/0x66/0x45` back to the
  matching `expr.*Value` — the exact inverse of `exprValueToPackstream:1787-1846`. Reject unknown tags with a clear
  `TypeError` naming the tag. `[]byte` needs an engine decision (openCypher 9 has no BYTES type), so treat it
  separately.
- **TCK/ACID impact:** none — the openCypher TCK drives the engine directly and never binds a PackStream struct.
  Adding accepted parameter types cannot remove a passing scenario.
- **Effort:** **S–M** for temporals + a decision on Bytes; **L** for spatial (new type, new functions, new index
  support).

#### F5b. `BindParams` rejects *every* typed Go slice and map — the embedded-API ergonomics cliff [NEW] (severity: **HIGH** for the Go API)

The parameter gap is far wider than the Bolt-struct case. Measured exhaustively (`probe/bindparams`):

```
accepted  []any{...}
accepted  map[string]any
REJECTED  []map[string]any    unsupported parameter type []map[string]interface {}
REJECTED  []string            unsupported parameter type []string
REJECTED  []int64             unsupported parameter type []int64
REJECTED  []int               unsupported parameter type []int
REJECTED  []float64           unsupported parameter type []float64
REJECTED  []byte              unsupported parameter type []uint8
REJECTED  map[string]string   unsupported parameter type map[string]string
REJECTED  map[string]int64    unsupported parameter type map[string]int64
REJECTED  [][]any             unsupported parameter type [][]interface {}
```

Only the two **fully type-erased** containers are accepted (`cypher/api.go:3389` `bindAny` matches `[]any` and
`map[string]any` as exact types; everything else falls to the default arm). A Go caller holding the most natural
possible value — `[]string{"alice","bob"}` for `WHERE n.name IN $names` — must first hand-copy it into `[]any`.

**Crucial scoping distinction:** this does **not** bite over Bolt, because the PackStream decoder already produces
`[]Value`/`map[string]Value` (= `[]any`/`map[string]any`) before `BindParams` sees them
(`bolt/packstream/value.go:181,198`). It bites the **embedded Go API** — GoGraph's primary audience — and only
there. The official Neo4j driver accepts typed slices without ceremony, so an embedder migrating *from* the driver
*to* embedded GoGraph hits an immediate, silent regression in ergonomics.

**Lever:** add a `reflect.Kind`-based fallback in `bindAny`'s default arm for `reflect.Slice`, `reflect.Array` and
`reflect.Map` (with `string`-convertible keys), recursing through `bindAny` per element. Keep the existing exact-type
fast paths first so the hot path stays reflection-free and zero-cost for `[]any`/`map[string]any`; reflection is
paid only by callers who would otherwise get an error. Guard the recursion with the same depth bound the encoder
uses.
**TCK/ACID impact:** none — the TCK binds only `[]any`/`map[string]any`; this strictly widens the accepted set.
**Effort:** **S**.

### F6. `EXPLAIN` / `PROFILE` are not query prefixes, so no plan is reachable over Bolt [NEW] (severity: MEDIUM)

- **What they do:** `EXPLAIN`/`PROFILE` prefixes populate `plan`/`profile` in the PULL SUCCESS summary
  (`docs-bolt`, Bolt 3+). Neo4j Browser, `cypher-shell`'s `:profile`, IDE plugins and the Query API
  (`docs-query-api/profile-query.adoc`) all rely on this.
- **What GoGraph does:** rejected at the parser:
  `cypher: parse: unexpected "EXPLAIN" at 1:0` / `unexpected "PROFILE" at 1:0` (measured). The capability exists —
  `Engine.Explain(query, params)` and `cypher/explain/profile.go:73 ProfiledOperator.Stats()` — but is reachable
  only from Go, never from a Bolt client.
- **Lever:** accept the two keywords as a statement prefix in the Bolt session layer (strip-and-flag before handing
  the text to the parser, exactly as GoGraph already does for `SHOW …` routing), run `Engine.Explain`, and serialise
  the operator tree into the `plan`/`profile` dictionary shape the drivers hydrate.
- **TCK/ACID impact:** none if implemented as a Bolt-layer prefix (the grammar is untouched, so no TCK scenario can
  change). Implementing it *in the ANTLR grammar* would touch the parser and is the riskier route — prefer the
  session-layer strip.
- **Effort:** **M**.

### F7. Bolt 5.6 is advertised but 5.5-era summary metadata is sent [NEW] (severity: LOW–MEDIUM)

- **What they do:** "Version 5.6: SUCCESS messages that contain a `notifications` field were changed to have a field
  `statuses` instead" (`docs-bolt`, `bolt/message.adoc` §Version 5.6). GoGraph advertises 5.6 as its preferred
  version (`bolt/proto/handshake.go:33`).
- **What GoGraph does:** emits `notifications` (`bolt/server/session.go:1066`).
- **Evidence:** `neo4j-go-driver` v5.28.4 tolerates it (my cartesian-product check PASSes, and
  `GqlStatusObjects()` returns the polyfilled `"00000"`), so nothing is broken *today* — but GoGraph is claiming a
  protocol version whose contract it does not implement. A strict 5.6 client is entitled to ignore `notifications`.
- **Lever:** either emit `statuses` (GQL status objects, with `gql_status`, `status_description`,
  `diagnostic_record`) when `boltMinor >= 6`, or **cap the advertised version at 5.5**… except 5.5 is explicitly
  "unsupported and flawed" per the spec, so the honest options are *implement `statuses`* or *advertise 5.4 max*.
  Recommend implementing `statuses`; the notification data already exists (`Result.Notifications()`).
- **TCK/ACID impact:** none.
- **Effort:** **S–M**.

### F8. No `connection.recv_timeout_seconds` hint — `Options.ConnTimeout` becomes user-visible flakiness [NEW] (severity: MEDIUM)

- **What they do:** Bolt 4.3 added a `hints` dictionary in HELLO SUCCESS "to provide optional configuration hints to
  drivers" (`docs-bolt`, §Version 4.3). Neo4j sends `connection.recv_timeout_seconds` so the driver proactively
  discards pool connections before the server closes them.
- **What GoGraph does:** `"hints": map[string]packstream.Value{}` — an unconditionally empty map
  (`bolt/server/session.go:622`; verified on the wire: HELLO SUCCESS contains `8568696e7473 a0`, i.e. `"hints"` → `a0`
  = empty map).
- **Evidence:** measured (`probe3`/`probe4`, `ConnTimeout=700 ms`, pool size 1, 1.2 s idle):
  ```
  session.Run   after idle > ConnTimeout: err=ConnectivityError: EOF     ← user-visible failure
  ExecuteRead   after idle > ConnTimeout: err=<nil>  (1ms)               ← managed retry recovers
  ExecuteQuery  after idle > ConnTimeout: err=<nil>  (1ms)               ← managed retry recovers
  ```
  **Adversarial check applied and it narrowed the finding**: the managed-transaction APIs, which Neo4j documents as
  the recommended path, recover transparently in ~1 ms. Only the direct `session.Run` API — still a fully supported,
  widely used API — surfaces the error. Severity is MEDIUM, not HIGH.
- **Lever:** when `Options.ConnTimeout > 0`, emit
  `hints["connection.recv_timeout_seconds"] = int64(ConnTimeout/time.Second)` (spec requires a positive integer;
  round up, and skip the hint when `ConnTimeout < 1 s`). Optionally also `telemetry.enabled: false` for explicitness.
- **TCK/ACID impact:** none.
- **Effort:** **S** (under 15 lines).

### F9. NOOP keep-alive is received but never sent [NEW] (severity: MEDIUM)

- **What they do:** "NOOP chunks may now be transmitted in all connection states when a connection remains idle for
  extended periods while the server is busy processing a request" — Bolt 4.3 (`docs-bolt`, §Version 4.3). This is
  what keeps a long-running query alive through a load balancer or NAT with an idle timeout.
- **What GoGraph does:** the reader skips inbound NOOPs correctly and with a good rationale
  (`bolt/proto/chunking.go:130-141`, `:184`), but nothing in `bolt/server` ever *writes* a `00 00` keep-alive during
  a long RUN/PULL (`grep -rn "NOOP" bolt/ | grep -v _test` returns only the reader).
- **Evidence:** code inspection + the reader-only grep. Not reproduced end-to-end (would need a proxy with an idle
  timeout), so I label this a spec-conformance and deployment-risk finding rather than a measured failure.
- **Lever:** a per-connection ticker (bounded, owned by the connection goroutine, cancelled on RUN completion) that
  writes a `00 00` chunk every ~N seconds while a request is in flight and no RECORD has been written. Must
  serialise with the writer (the connection already has a single-writer topology).
- **TCK/ACID impact:** none.
- **Effort:** **S–M** (the concurrency contract matters: one writer, no races, no leaked ticker).

### F10. `imp_user` is silently ignored [NEW] (severity: MEDIUM — security-relevant)

- **What they do:** `imp_user::String` in BEGIN/RUN/ROUTE extras since Bolt 4.4 (`docs-bolt`, `bolt/message.adoc`
  lines 356-361, 1154, 1182: "the `imp_user` key specifies the impersonated user which executes this transaction").
  Neo4j honours it (Enterprise) or rejects it.
- **What GoGraph does:** `handleBegin`/`handleRun` never read `imp_user`; the query executes as the authenticated
  principal.
- **Evidence:** measured — `SessionConfig{ImpersonatedUser: "someone"}` is **silently accepted** and the query
  succeeds. A multi-tenant proxy that de-privileges by impersonating would run every query at full privilege with no
  error and no log line.
- **Lever:** reject `imp_user` with `Neo.ClientError.Security.Forbidden` ("impersonation is not supported") unless
  and until it is implemented. Fail-closed is the correct default for an unimplemented authorisation control, and it
  matches the project's own "fail-stop, never fail-silent" mandate.
- **TCK/ACID impact:** none.
- **Effort:** **S**.

### F11. `TELEMETRY` (0x54) is rejected on a connection that negotiated 5.6 [NEW] (severity: LOW)

- **What they do:** Bolt 5.4 added the `TELEMETRY` message (`docs-bolt`, §Version 5.4).
- **What GoGraph does:** no tag `0x54` in `bolt/proto/messages.go:18-35`.
- **Evidence:** measured with a raw client — sending `B1 54 00` on a negotiated-5.6 connection returns
  `FAILURE Neo.ClientError.Request.Invalid "malformed Bolt message"`. **Adversarial check:** `neo4j-go-driver` gates
  TELEMETRY on `hints["telemetry.enabled"] == true` (`internal/bolt/bolt5.go:984,1206`), and GoGraph sends
  `hints: {}`, so the official driver never sends it. The risk is confined to a client that does not gate.
- **Lever:** accept `0x54` and answer SUCCESS with empty metadata (the message is advisory; discarding the payload
  is spec-legal). Cheapest possible conformance win.
- **TCK/ACID impact:** none. **Effort: S**.

### F12. No Bolt handshake manifest (5.7+), no 5.7/5.8/6.0 [NEW] (severity: LOW)

- **What they do:** Bolt 5.7 introduced "Manifest v1": the client may substitute one of its four 32-bit slots with
  `00 00 01 FF` (`docs-bolt`, `bolt/handshake.adoc:96-190`). Neo4j 2025.10+ speaks Bolt 6.0
  (`docs-bolt/bolt-compatibility.adoc`).
- **What GoGraph does:** `bolt/proto/handshake.go:98-132` parses the four slots and, correctly, finds no
  `SupportedVersions` entry with `major == 0xFF`, falling through to the legacy match. Verified: the driver's
  proposal is `{0xFF,0x01}, {5,8,range 8}, {4,4,range 2}, {3,0}` (`internal/bolt/connect.go:48-52`) and negotiation
  lands cleanly on 5.6.
- **Evidence:** measured — `GetServerInfo` reports `bolt=5.6`; no warning, no retry.
- **Lever:** nothing urgent. Watch the driver's `versions` array: the day a major driver drops legacy slots in favour
  of the manifest, GoGraph's handshake fails outright. Cheap insurance: implement the manifest response.
- **TCK/ACID impact:** none. **Effort: M**.

### F13. CSV import cannot express a labelled property graph; no `LOAD CSV` [NEW + CONFIRMED-R1 (T3)] (severity: **HIGH** for adoption)

- **What they do:**
  - Neo4j `neo4j-admin database import`: header files declaring `:ID`, `:LABEL`, `:START_ID`, `:END_ID`, `:TYPE`
    and typed property columns; `--bad-tolerance` (tolerant by default), `--skip-bad-relationships`,
    `--skip-duplicate-nodes`, `--schema-commands` (Enterprise) to build indexes during the import. Constraints:
    *full* mode requires a **non-existent or empty** database and the DBMS **offline**; *incremental* import is
    **Enterprise only** and **block-format only**. Source: `neo4j/docs-operations`,
    `modules/ROOT/pages/import.adoc` (lines 13-22, 45-46, 99, 218, 456-475, 783-795).
  - Memgraph: "the shortest path to import data into Memgraph is from a CSV file using the `LOAD CSV` clause";
    recommends `IN_MEMORY_ANALYTICAL` storage mode, indexes first, nodes before relationships, and batching above
    10 M objects; documents "**import one million nodes or relationships per second**" with batched parallelism.
    Source: <https://memgraph.com/docs/data-migration/best-practices>.
- **What GoGraph does:**
  - `graph/io/csv` is a **plain edge list**: `ReadInto(r, opts) (*adjlist.AdjList[string, int64], int, error)` over
    `(src, dst[, weight])`. No labels, no properties, no node rows, and it returns an **adjacency list, not an
    `lpg.Graph`** — so it cannot even produce the engine's own graph type.
  - Only `graph/io/jsonl` and `graph/io/graphml` carry properties and labels (`ReadWithProps`/`WriteWithProps`;
    labels were added by #1793, see `graph/io/label_roundtrip_test.go:3-5`). `graph/io/dot` is write-only.
  - `LOAD CSV` is **not parseable** — measured: `LOAD CSV FROM …`, `LOAD CSV WITH HEADERS FROM …` and
    `USING PERIODIC COMMIT … LOAD CSV …` all fail with `cypher: parse: unexpected "LOAD"/"USING" at 1:0`.
  - Correction to a round-1 note: **`LOAD CSV` is not covered by the vendored TCK.** `cypher/tck/features/` has 220
    feature files in `clauses/{call,create,delete,match,match-where,merge,remove,return,return-orderby,
    return-skip-limit,set,union,unwind,with,…}`, `expressions/` and `useCases/` — there is no `load-csv` directory
    (`find cypher/tck/features -ipath '*load*'` → empty). Adding `LOAD CSV` therefore cannot move the 3897 baseline
    in either direction.
- **Evidence (measured, Apple M4, `-count=5`):**

  | path | 500 k edges | throughput | B/op | allocs/op |
  |---|---|---|---|---|
  | `csv.ReadIntoCtx` | 103.2 ms | **4.85 M edge-rows/s** (108 MB/s) | 111.9 MB | 755 651 |
  | `jsonl.ReadIntoCtx` | 389 ms | 1.29 M records/s (81 MB/s) | 292.0 MB | 5 255 637 |
  | `store/bulk` sequential | 359.9 ms | 1.39 M edges/s | 1.214 **GB** | 1 771 800 |
  | `store/bulk` +`ExpectNodes` | 351.4 ms | 1.42 M edges/s | 1.211 GB | 1 767 806 |
  | `store/bulk` **parallel** | 119.7 ms | **4.18 M edges/s** | 68.6 MB | **3 701** |

  (`store/bulk` figures are the repo's own `BenchmarkLoad_*`, 500 k edges over 50 k nodes, **in-memory build only —
  the benchmark deliberately excludes the csrfile write**, per `store/bulk/bulk_bench_test.go:23-24`.)
  Honest reading: GoGraph's *raw ingest rate* is ~4× Memgraph's documented ~1 M objects/s, but the comparison is not
  like-for-like — Memgraph's number includes CSV parsing, labelled property nodes, index maintenance and durability;
  GoGraph's fastest path carries **only `(src, dst, weight)`**. The parallel loader is also 17.7× leaner in bytes and
  478× leaner in allocation count than the sequential path, which is the real story in that table.
- **Lever (two, ranked):**
  1. **Header-driven CSV** in `graph/io/csv`: a `:ID`/`:LABEL`/`:START_ID`/`:END_ID`/`:TYPE`/`prop:type` header
     dialect that produces an `lpg.Graph`, plus a `store/bulk` entry point that consumes it. This is the
     `neo4j-admin import` idea, and it is the difference between "a graph library that reads edge lists" and "a
     graph database you can load real data into".
  2. **`--bad-tolerance` equivalent**: an `Options.OnBadRow func(row int, err error) error` callback (default:
     fail-stop, preserving today's behaviour) so a billion-row load is not defeated by one malformed line.
     `LOAD CSV` itself is a larger, grammar-level project and should stay behind these two.
- **TCK/ACID impact:** the `graph/io` and `store/bulk` levers are entirely outside the Cypher grammar and outside
  the transactional path (`store/bulk` explicitly "bypasses the transactional WAL stack"), so TCK-neutral.
  ACID note: `store/bulk` is already documented as non-transactional, which is correct and matches
  `neo4j-admin import`'s offline model — but a bulk load that crashes mid-write must leave no half-written csrfile;
  that is an existing property to preserve, not one this lever changes. A future `LOAD CSV` **would** be ACID-relevant
  (it needs `CALL { } IN TRANSACTIONS`-style batching to avoid one unbounded transaction).
- **Effort:** **M** for header-driven CSV, **S** for bad-row tolerance, **L** for `LOAD CSV`.

### F17. **HEADLINE** — GoGraph has a 4.18 M edges/s durable ingest path and it is unreachable from Cypher [NEW] (severity: **CRITICAL** for adoption)

- **The measurement (coordinator's, this round):** loading **20 000 nodes + 200 000 edges through the Cypher write
  path** took **35 min 33 s**. Memgraph loaded the same dataset in **977 ms**.
  - 220 000 entities / 2 133 s = **103 entities/s**.
  - Memgraph: 220 000 / 0.977 s = **225 180 entities/s** → **≈ 2 180× faster**.
- **What GoGraph already owns:** `store/bulk`'s partitioned-parallel loader does **4.18 M edges/s** (measured this
  round, `BenchmarkLoad_Large_Parallel`, 500 k edges / 50 k nodes, Apple M4, 119.7 ms, 68.6 MB, 3 701 allocs).
  At that rate the 200 000-edge portion of the coordinator's dataset is **≈ 48 ms** of work. The engine is not slow
  at ingest. **The query language simply cannot reach the fast path.**
- **Evidence of the wall (measured):**
  ```
  grep -rn "bulk\." --include='*.go' cypher/ bolt/ | grep -v _test   →  (empty)
  grep -rln '"…/store/bulk"' --include='*.go' .                      →  bench/rmat, bench/ldbc, internal/sim,
                                                                        search/extern (4 tests),
                                                                        examples/18_oocore_pipeline, its own example_test
  ```
  Not one line of `cypher/` or `bolt/` references `store/bulk`. There is no `CALL`-able import procedure either
  (`grep -rn 'Register("' cypher/procs/*.go` → nothing). And `LOAD CSV` does not parse (F13). So the *only* way a
  user with data reaches the graph is one Cypher `CREATE`/`MERGE` at a time — the 103 entities/s path. The fast
  path is reachable **only from Go, only by importing an internal-feeling package, and only for
  `(src, dst, weight)` edges without labels or properties**.
- **Why this is the most important finding in the stream:** F1–F12 make GoGraph awkward to *talk to*. F17 makes it
  impractical to *fill*. A 2 180× disadvantage on data loading is the first thing any evaluator measures and the
  point at which most evaluations stop. It also inverts the flattering headline in F13's table: GoGraph's raw
  ingest rate beats Memgraph's documented ~1 M objects/s by ~4×, but the rate a *user* can actually obtain is
  ~2 180× worse than Memgraph's. Both numbers are true; only the second one is reachable.
- **Diagnosis (bounded — I did not profile the Cypher write path; that is stream 3/4 territory):** 103 entities/s is
  ≈ 9.7 ms per entity, which is far too slow to be interpretation overhead. It is the signature of **per-statement
  durability**: each auto-commit `CREATE` is its own transaction, so each pays a WAL append + fsync. The project
  already has the counter-measure — group commit (`docs/` records a group-commit/coalescing feature) — so the first
  question to answer is whether the loading path engages it. **NOT INVESTIGATED**: whether the 35 min is dominated
  by fsync, by planning, or by per-statement transaction setup. That measurement should be taken before any fix.
- **Levers, ranked:**
  1. **`CALL { … } IN TRANSACTIONS OF n ROWS`** (currently `SyntaxError`, measured in probe2). This is the standard
     openCypher/Neo4j answer to exactly this problem: it amortises commit cost over *n* rows under user control.
     It is the single highest-leverage language feature for ingest and it is what `USING PERIODIC COMMIT` was
     replaced by.
  2. **`LOAD CSV`** on top of (1) — Memgraph's documented primary import path, and the reason its number is 977 ms.
  3. **A Cypher-reachable bulk entry point** — either a `CALL gograph.bulkImport(...)` procedure or a documented,
     supported Go API that accepts labels and properties (today `store/bulk` accepts neither), so that the 4.18 M
     edges/s path is available to someone who is not reading `bench/`.
  4. Cheapest interim: **document the fast path**. Right now an evaluator has no way to discover that
     `store/bulk` exists — its only non-test consumers are benchmarks and one example.
- **TCK/ACID impact:** `CALL { } IN TRANSACTIONS` is **ACID-significant** — it deliberately splits one statement
  into many committed transactions, so it must be specified carefully (what is visible mid-run, what happens on
  error, `ON ERROR CONTINUE|BREAK|FAIL`) and it is the one lever in this report that genuinely touches the
  durability contract. It is **not** TCK-risky: the vendored TCK has no `load-csv` and no `IN TRANSACTIONS`
  scenarios (220 feature files verified in F13), so the 3897 baseline cannot move. `store/bulk` exposure is
  outside the transactional path entirely (the package documents that it "bypasses the transactional WAL stack").
- **Effort:** **L** for `CALL { } IN TRANSACTIONS`, **M** for a bulk procedure, **S** to document the fast path.

### F18. The in-process Bolt round trip costs 45–90 µs [NEW] (severity: MEDIUM — informational, bounds the F1 win)

- **The measurement (coordinator's, this round):** GoGraph over its own Bolt server vs GoGraph embedded, 2 000 nodes:

  | operation | embedded | over Bolt | transport tax |
  |---|---|---|---|
  | single-node write | ~12 µs | ~58 µs | **+46 µs (4.8×)** |
  | point lookup | ~75 µs | ~163 µs | **+88 µs (2.2×)** |

- **Reading it honestly:** ~45–90 µs per round trip over loopback covers TCP syscalls, chunk framing, PackStream
  encode/decode on both sides, and the driver's session/pool bookkeeping. That is *not* obviously anomalous for a
  synchronous request/response protocol on loopback — but it is 4.8× the cost of the write it carries, which means
  **for small operations GoGraph's Bolt server is almost entirely transport**. Two consequences:
  1. **It reinforces the embedded-first positioning.** The library's own API is the fast path by a wide margin;
     Bolt is a compatibility surface, not a performance surface. The `gograph.Open()` façade (F15) matters more
     than any Bolt optimisation.
  2. **It bounds the F1 payoff.** The struct encoding saves ~40 ns and ~16 bytes per entity — real, and free, but
     ~0.1 % of a 46 µs round trip. **F1 must be justified on compatibility, not on speed.** The perf win is a
     tiebreaker that removes the "but it costs bytes" objection; it is not the argument.
- **NOT INVESTIGATED:** I did not reproduce or decompose these figures (no per-layer profile separating syscall,
  framing, PackStream, and driver overhead), and I did not test pipelining or larger batch sizes, where the
  per-round-trip cost amortises and the picture may change substantially. Recommended follow-up: measure at
  `FetchSize` 1 / 100 / 1000 to separate fixed round-trip cost from per-record cost before optimising anything in
  the Bolt path.
- **Lever:** none proposed on this evidence. The correct next step is measurement, not optimisation — and the
  project's own mandate ("Profile with pprof", "benchmark before and after every structural change") says so.

### F14. Import/export error handling is a genuine strength [NEW] (severity: n/a — a WIN)

- **Evidence (measured):**
  ```
  csv: wrong field count    -> rows=1 err=csv row 2: need at least 2 fields, got 1
  csv: non-numeric weight   -> rows=1 err=csv row 2 weight "notanumber": strconv.ParseInt: …
  csv: unterminated quote   -> rows=1 err=csv row 2: parse error on line 2, column 8: extraneous or missing "
  jsonl: malformed line     -> rows=2 err=jsonl row 2: invalid character 'N' …
  graphml: truncated        -> rows=0 err=graphml: parse: XML syntax error on line 1: unexpected EOF
  ```
  Every reader is fail-stop, reports the **1-based row number**, and returns a nil graph on error
  (`graph/io/csv/reader.go:193,208,214`; the repo has a dedicated `nil_on_error_test.go`). All readers are
  context-aware (`ReadIntoCtx` checks `ctx.Err()` in the row loop, `reader.go:182`) and bounded by default
  (`csv.DefaultMaxBytes = 128 MiB`, `jsonl.DefaultMaxBytes = 1 GiB`, `graphml.DefaultMaxBytes = 128 MiB`).
  The `csv.Options.MaxBytes` doc explicitly warns that a bare `Options{}` disables the cap — good, honest API doc.
- **Verdict:** **BETTER than Neo4j** on correctness (Neo4j's importer is tolerant by default with `--bad-tolerance`,
  which can silently drop entities), WORSE on operational ergonomics. See F13's second lever for the reconciliation.

### F15. Public Go API: strong contracts, heavy front door [NEW] (severity: MEDIUM)

- **Measured surface:** 115 packages total, **49 exported** (excluding `internal/`, `examples/`, `bench/`, `cmd/`):
  18 under `graph/`, 12 `cypher/`, 8 `store/`, 5 `search/`, 3 `bolt/`, plus `metrics` and `ds`. 98 runnable
  `func Example` functions across the repo — good godoc discipline, and the `cypher.Result` lifecycle doc
  (`Close` contract + `runtime.SetFinalizer` leak metric) is exemplary and better than most Go DB libraries.
- **The front door is the problem.** There is no `store.Open(path)`. The durable path is
  `recovery.Open[N, W](dir, recovery.Options[N, W]{…})` (see `examples/17_transactional_log/main.go:579`,
  `examples/21_typed_recovery/main.go`) — **two type parameters plus a codec-bearing Options struct** before the
  user has run a single query. `store.New` takes a `*wal.Writer` the caller must build. And `docs/bolt.md` documents
  a live footgun: `adjlist.Config{Multigraph: true}` is *required* for openCypher semantics but is **not the
  default** — my own first probe run emitted
  `WARN cypher: engine constructed over a non-directed (undirected) graph; openCypher requires directed
  relationships … directed pattern matching and traversal will produce incorrect edge results`. A default that
  produces *incorrect results* unless overridden is the wrong default; the warning is a mitigation, not a fix.
- **Comparison:** the realistic Go alternatives are (a) the Neo4j Go **driver** against a server — no embedding;
  (b) Kùzu, which is embedded but C++ with a cgo binding; (c) nothing else mature. GoGraph's **pure-Go, cgo-free,
  cross-compilable** embedding is a real and defensible differentiator — it is the only option in this table that
  `GOOS=linux GOARCH=arm64 go build` handles with no toolchain. SQLite is the ergonomic benchmark to aim at:
  one `Open(path)`, no type parameters, sane defaults.
- **Lever:** add a `gograph.Open(path string, opts ...Option) (*DB, error)` façade over
  `recovery.Open[string, float64]` with openCypher-correct defaults (`Directed: true, Multigraph: true`) and a
  `db.Query(ctx, cypher, params)` method. Keep the generic API for advanced users; make the ergonomic path the
  default one. Also state a compatibility policy: at `v0.x` Go's module rules give adopters **no** compatibility
  guarantee, and 49 exported packages is a large surface to freeze at v1 — the pre-v1 window is the time to demote
  packages that should be `internal/`.
- **TCK/ACID impact:** none — a façade adds no semantics. Changing the *default* `adjlist.Config` would be
  behaviour-changing for existing embedders and must be done in the new façade only, not by mutating the zero value.
- **Effort:** **S–M**.

### F16. HTTP / Query API: correctly absent, but one endpoint is worth having [NEW] (severity: LOW)

- **What they do:** Neo4j ships a **Query API** — `POST /db/{db}/query/v2`, plus `/tx`, `/tx/{id}`,
  `/tx/{id}/commit`, on port 7474/7473, enabled by default since 5.25, superseding the deprecated HTTP API
  (`neo4j/docs-query-api`, `index.adoc`, `endpoints.adoc`). Its stated purpose is "developing client applications in
  languages for which there is no supported library" — and it explicitly recommends the driver when one exists.
  Memgraph has **no HTTP query API**: only a WebSocket log server on 7444 and an **Enterprise-only** metrics HTTP
  server on 9091 (<https://memgraph.com/docs/database-management/monitoring>).
- **Assessment:** GoGraph is an in-process Go library. Its "no supported library" case is *itself* — a Go caller uses
  the API directly. **Building a Cypher-over-HTTP API would be effort spent on Neo4j's problem, not GoGraph's.**
  Recommend: **nothing to take** on the query side.
- **The one exception is health/readiness.** `docs/bolt.md` states "The Bolt server does not expose an HTTP health
  endpoint. To verify liveness, open a Bolt connection and send HELLO/RESET." Every orchestrator (Kubernetes,
  systemd, ELB) speaks HTTP for liveness/readiness probes and none speaks Bolt. GoGraph *already* ships the
  ingredient — `metrics.NewPrometheusRegistry().Handler()`, zero-dependency, no licence gate, which is **better than
  both Neo4j (metrics = `[role=enterprise-edition]`) and Memgraph (metrics = Enterprise)**. The lever is a tiny
  optional `server.HealthHandler() http.Handler` returning readiness plus the derived
  `live connections = accepted − closed` / `open transactions = opened − closed` gauges the docs already define.
- **TCK/ACID impact:** none. **Effort: S.**

---

## Prioritised lever list

| # | Lever | Severity | Effort | TCK | ACID |
|---|---|---|---|---|---|
| **0** | **Make fast ingest reachable: `CALL { } IN TRANSACTIONS`, then `LOAD CSV` / a bulk procedure (F17)** | **CRITICAL** | L / M | none (neither is in the vendored TCK) | **yes — splits one statement into many commits; specify carefully** |
| 1 | `db` key in PULL/DISCARD SUCCESS (F3) — stops a **driver panic** | HIGH | S | none | none |
| 2 | Node/Rel/UnboundRel/Path structures + `element_id` (F1) | HIGH | M | none (proved) | none |
| 3 | Per-statement counters → `stats` + `Result.Counters()` (F2) | HIGH | M–L | none | must roll back with undo log |
| 4 | Inbound temporal-struct parameters (F5) | HIGH | S–M | none | none |
| 4b | `reflect`-based fallback for typed slices/maps in `BindParams` (F5b) | HIGH (Go API) | S | none | none |
| 5 | `type` + `t_first` + `t_last` (F4) | MEDIUM | S | none | none |
| 6 | `connection.recv_timeout_seconds` hint (F8) | MEDIUM | S | none | none |
| 7 | Reject `imp_user` fail-closed (F10) | MEDIUM | S | none | none |
| 8 | Header-driven CSV → `lpg.Graph` + bad-row tolerance (F13) | HIGH (adoption) | M / S | none | outside tx path |
| 9 | `EXPLAIN`/`PROFILE` as a Bolt-layer prefix (F6) | MEDIUM | M | none if session-layer | none |
| 10 | `statuses` at Bolt 5.6 (F7) | LOW–MED | S–M | none | none |
| 11 | Outbound NOOP keep-alive (F9) | MEDIUM | S–M | none | none |
| 12 | `gograph.Open()` façade with correct defaults (F15) | MEDIUM | S–M | none | none |
| 13 | Accept `TELEMETRY` (F11); HTTP health handler (F16) | LOW | S | none | none |
| — | *(no lever)* Bolt round-trip tax (F18) — **measure before optimising** | MEDIUM | — | — | — |

Items 1, 4, 4b, 5, 6, 7, 13 are together **under ~200 lines of production code** and close 7 of the 13 measured
driver failures plus the worst embedded-API ergonomics gap. That is the cheapest interoperability win available
anywhere in this codebase — but **item 0 outranks all of them**, because a database that takes 35 minutes to load
220 000 entities does not get evaluated far enough for items 1–13 to matter.

## NOT INVESTIGATED

Recorded honestly rather than glossed:

- **Decomposition of the 35 min Cypher-ingest figure (F17).** I did not profile the write path to attribute the
  9.7 ms/entity between fsync, planning, and transaction setup. This must be measured before choosing a fix, and it
  overlaps streams 3/4 (transactions, runtime).
- **Reproduction of the Bolt transport-tax figures (F18).** Taken from the coordinator's measurements; not
  independently reproduced or decomposed per layer, and not measured across `FetchSize` values.
- **`cypher-shell` and non-Go drivers.** Only `neo4j-go-driver/v5.28.4` was exercised. The Java/Python/JS drivers
  hydrate entities from the same PackStream structures, so F1–F5 should reproduce, but I did not verify it. The
  Java driver in particular is stricter about `db` and summary metadata.
- **TLS end-to-end with a real driver.** `bolt/server/tls.go` and `tls_reload.go` were read, not exercised.
- **Long-running-query keep-alive behind a proxy (F9).** Reasoned from the spec and a reader-only grep; not
  reproduced against a load balancer with an idle timeout.
- **`graph/io/graphml` and `dot` throughput.** Only `csv` and `jsonl` were benchmarked.
- **Whether `store/bulk`'s csrfile write is crash-safe.** The benchmark deliberately excludes the disk write, so my
  4.18 M edges/s figure is in-memory build only; I did not audit the durability of the file it produces.

*(Method note: my in-process server rig was written independently before I saw the pointer to
`bench/ldbc/ic_helpers_test.go:78 startICServer`; the two agree — random loopback port, `context.WithCancel` +
drained `Serve` goroutine, `goleak` registered first so it runs last. The established pattern is sound.)*

---

## Nothing-to-take list

- **Cypher-over-HTTP / Query API.** Neo4j's own documentation frames it as the fallback for languages without a
  driver. GoGraph's caller *is* Go. Building it would add an HTTP attack surface, a second serialisation format and
  a second transaction lifecycle for zero benefit to an embeddable library. (Health/metrics endpoints — F16 — are a
  different, much smaller thing, and GoGraph is already ahead there.)
- **Cluster routing.** `bolt/server/route.go`'s single-host table is exactly right for a single-process embeddable.
  Implementing real cluster routing implies a cluster, which is a different product.
- **Multi-stream `qid`.** GoGraph's single-stream model with `qid = -1` (`session.go:899,912`) is a documented,
  strictly-enforced limitation, and the driver masks it completely — my two-open-streams-in-one-transaction check
  **PASSED**. The cost is that the driver buffers the first result client-side. Not worth the server-side complexity
  of concurrent cursors inside one transaction, which would also complicate the isolation story. Keep it.
- **Tolerant-by-default import.** Neo4j's `--bad-tolerance` defaults to *skipping* bad entities. GoGraph's fail-stop
  with a row number is the better default for a library that promises integrity. Take the *option*, not the default.
- **Memgraph's Bolt version ceiling.** Memgraph stops at 5.2; GoGraph reaches 5.6. Nothing to take, and it is worth
  saying out loud that GoGraph's *protocol reach* already exceeds a commercially supported graph database's.
- **Enterprise-gated metrics.** Both Neo4j and Memgraph put their Prometheus endpoint behind a licence.
  GoGraph's un-gated `metrics.NewPrometheusRegistry()` is strictly better; do not copy the gate.

---

## Overturns / corrections to earlier rounds

1. **Round 1's T1.1 "zero risk" is CONFIRMED and upgraded** — now with three independent proofs (single-file change
   surface; `go list -deps` shows the TCK has no `bolt/` dependency at all; and the struct form is **smaller and
   faster** than the map, so there is not even a bandwidth argument against it). The one thing round 1 omitted:
   it is a breaking change to the current wire contract and rewrites 7 test files.
2. **Round 1's "no string `elementId`" is partly STALE.** The `elementId()` *function* exists and returns a string
   (`cypher/funcs/essentials.go:120`; measured `elementId(n)` → `"140"`). What is missing is the `element_id`
   *field* on the wire — because there is no structure to carry it.
3. **"LOAD CSV is TCK-covered" is WRONG for this repo.** 220 vendored feature files, no `load-csv` anywhere
   (`find cypher/tck/features -ipath '*load*'` → empty). Implementing `LOAD CSV` is therefore TCK-neutral, not
   TCK-risky — which *raises* its attractiveness relative to how round 1 ranked it (T3).
4. **Round 1 under-ranked the summary-metadata gap.** It listed only "Bolt-path latency histogram" at T2. In fact
   four independent metadata keys are missing (`stats`, `db`, `type`, `t_first`/`t_last`), one of which
   (**`db`**) causes an outright **panic inside the official driver**, and another (**`stats`**) makes every write
   counter silently report zero. On measured application impact, `db` + `stats` belong at T1 alongside the wire
   structures — `db` is ~20 lines.
5. **NEW, not in either round:** the outbound/inbound type asymmetry (F5). GoGraph emits Bolt temporal structures
   correctly and cannot accept them back as parameters. Round 1's focus on the *output* side missed that the *input*
   side of the same feature is broken. Wider still (F5b): `BindParams` rejects **every** typed Go slice and map —
   `[]string`, `[]int64`, `[]float64`, `map[string]string`, `[]map[string]any` — accepting only the fully erased
   `[]any` / `map[string]any`. This is invisible over Bolt (the decoder already produces erased containers) and
   therefore was never going to be found by a protocol-only audit; it hits the embedded Go API, which is GoGraph's
   primary audience.
6. **The biggest correction of round 3 (F17):** rounds 1 and 2 both ranked ingest as T3 ("LOAD CSV"). That
   underweights it by an order of magnitude. Round 1's own headline — "gaps are BREADTH not core quality" — is
   *right about the engine and wrong about the product*: the core ingest quality is excellent (4.18 M edges/s,
   17.7× leaner than the sequential path), but the only ingest route a user can actually take runs at
   **103 entities/s**. A capability that exists and cannot be reached is, from the evaluator's seat, a capability
   that does not exist. **F17 belongs at T1, above the wire format.**
7. **Round 1's ranking of T1.1 above everything is right on the wire axis but should not be read as the top
   priority overall.** Within Bolt, `db` (F3, ~20 lines, stops a driver panic) is cheaper than the wire structs and
   should land first; and per F18 the wire-struct change must be sold on compatibility, not speed — its ~40 ns
   saving is ~0.1 % of a 46 µs Bolt round trip.
