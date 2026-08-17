# DST Feature Coverage

This document records how the Deterministic Simulation Testing harness
(`internal/sim/`) exercises the GoGraph feature surface, and the coverage work
completed on 2026-07-13 to make the DST drive **every implemented feature**.

The goal: for every implemented GoGraph feature, the DST has a scenario that
drives it during simulation and validates it against an **independent** oracle
or reference — including, wherever applicable, across crash and recovery.

## Method

Three domain audits (Cypher language, graph/search algorithms, storage/
durability) enumerated the implemented feature surface and cross-referenced it
against the scenarios the DST actually ran. Every "implemented but unexercised"
feature became a tracked task. Each new check validates the engine against an
independent computation — never the code under test — and every deterministic
scenario is bit-reproducible from its seed.

Two classes of verification vehicle are used:

- **Oracle-computed checks**: an invariant computed independently from the
  shadow model (`GraphOracle`, or a scenario-private model) is compared to the
  engine's result — the strongest form (catches absolute wrongness).
- **Absolute-literal checks**: a self-contained `RETURN <expr>` whose answer is
  a known constant, compared to the engine's canonical rendering.
- **Independent naive references**: for the search algorithms, a from-scratch
  reference (naive BFS/Bellman-Ford/power-iteration/degree-parity/…) computed on
  a shaped fixture with known ground truth.

## Cypher language coverage

### Mutation clauses (`schema-mutation`, `merge-rel` scenarios)

| Feature | Scenario | Independent check |
|---|---|---|
| `REMOVE n.prop`, `REMOVE n:Label` | schema-mutation | oracle read-back: property reads NULL / label dropped, across crash+checkpoint recovery |
| `SET n:Label`, `SET n += $map`, `SET n = $map` | schema-mutation | oracle labels/properties after each op, across recovery |
| multi-label match `(n:A:B)` | schema-mutation | oracle count of dual-labelled nodes |
| `MERGE (a)-[r]->(b) ON CREATE/ON MATCH SET` | merge-rel | idempotent edge count + `r.n` counter round-trips across recovery |

Map-valued parameters are bound by the harness adapter (`toExprValue`) so
`SET n += $map` / `MERGE (n $map)` can be driven.

### Read clauses, expressions, functions (`cypher-surface` scenario)

`CheckCypherSurfaceExtended` (oracle-computed over the Person/KNOWS graph):
`count(DISTINCT)`; 3VL `AND`/`OR`/`XOR`/`NOT`, `IN`, `IS NULL`, `<>`;
`STARTS WITH`/`ENDS WITH`/`CONTAINS`/`=~`; `ORDER BY … SKIP … LIMIT`;
`avg`/`min`/`max`/`sum` and `percentileCont`/`percentileDisc` invariants;
`EXISTS { }` / `COUNT { }` / pattern-comprehension subqueries;
`CALL db.labels`/`relationshipTypes`/`propertyKeys` vs the modelled schema.

`CheckExprLiterals` (absolute-literal battery, ~40 probes): `UNION`/`UNION ALL`;
`CASE`, list comprehension, `reduce`, `all`/`any`/`none`/`single`; the
scalar/list/string/math function surface; list subscript, list slice, map
projection; temporal constructors (`date`, `duration`) and component access.

## Search algorithm coverage

Every `search/` algorithm the DST did not previously exercise (or exercised
only in a degenerate regime) is now cross-checked against an independent naive
reference on shaped, seed-deterministic fixtures, folded into the
`search` / `search-crash` battery (so each is validated post-crash-recovery):

negative weights + negative-cycle detection (Bellman-Ford / Floyd-Warshall /
Johnson); `MinCostMaxFlow`; `PushRelabelMaxFlow`; `Closeness` / `Harmonic` /
`Eigenvector` / `Katz` / `PersonalisedPushPageRank`; serial-vs-parallel
`Betweenness`; parallel-edge k-shortest; `TopologicalSort` DAG success;
`Diameter`; triangle counting (serial == parallel); `WCCParallel` vs serial;
undirected Euler; `BiBFS`; direction-optimised BFS on a hub fixture; the
`*Into` / `NewSSSP` buffer-reuse APIs; external-memory `extern.BFS` /
`extern.PageRank`.

## Storage / durability coverage

| Feature | Scenario / vehicle | Invariant |
|---|---|---|
| Concurrent durable commits + crash recovery | `durable-commit-crash` | acked ⊆ recovered ⊆ issued; failures absent; no torn CREATE |
| Background checkpointer + crash-safe `store.DB` teardown | `checkpoint-teardown` | no `ErrWriterClosed` into an acked commit; recovered ⊇ acked; `Stop()` joins |
| Read-transaction behaviour under concurrency + crash | `readtx-isolation` | no dirty/partial reads; whole-batch atomicity on recovery |
| Atomic csrfile publish under fault/ENOSPC | `csrfile-publish-fault` | a failed publish leaves either no file or the complete prior csrfile — never torn |
| Recovery genuine-corruption fail-stop | `wal-corruption-failstop` | a corrupted interior WAL frame is detected (CRC), recovery reconstructs exactly the clean prefix and refuses to append; a benign torn tail is not treated as corruption |
| Post-rename dir-fsync fail-stop (WAL prefix reclaim) | `checkpoint-dirfsync-fault` | a post-rename parent-dir fsync failure poisons the writer, yet reopen recovers the exact committed state |
| CSV / JSONL export→import round-trip under fault | `io-roundtrip-fault` | a clean round-trip reproduces the modelled edge set exactly; an export under ENOSPC fails with a typed error and leaves no partial artifact a re-import would accept |

The `DiskConfig.FaultRate` knob and three `SimDisk` fault primitives
(`CorruptRange`, `ArmSyncFaultAt`, `ArmParentDirSyncFaultForPath`) back these
scenarios; all default to inert, so existing scenarios are byte-identical.

### The group-commit clarification

Through the engine (and therefore the Bolt wire), every write commit — including
its WAL `fsync` — is serialised under a single `visMu` lock, so `SyncGroup` is
always a **solo leader with zero followers**. Multi-member group-commit
coalescing and the fail-all path are unreachable via the engine and are covered
by store-layer unit tests (`store/wal/syncgroup_test.go`,
`store/txn/group_commit_durability_test.go`). The DST drives the solo-leader
`SyncGroup` on every durable write, and now additionally under concurrent
crash/recovery.

### Read-transaction isolation

`cypher.Engine.BeginReadTx` provides **snapshot isolation across the whole
transaction** since rmp #2307: one read instant is pinned at `BEGIN` and every
statement of the handle executes at it. It was per-statement read-committed when
this section was first written, and the `readtx-isolation` scenario — which
asserts only that no dirty or partial read is ever observed — certifies a
property both levels satisfy, so it remains valid under the stronger contract.

### MVCC multi-session and concurrency coverage (sprint 345)

The MVCC machinery is exercised end to end by four dedicated modes (see
[docs/dst.md](dst.md#mvcc-multi-session-and-concurrency-coverage) for the full
description): the deterministic multi-session mode with in-transaction
isolation checkers (`RunMVCCSessions` + `mvcc_isolation.go`, rmp #2436), the
contended lost-update scenario (`RunMVCCContention`, rmp #2437), crashes with
open transactions and transaction-granular recovery adjudication (rmp #2438),
Bolt-wire transactional roles with typed conflict accounting and during-run
isolation oracles (rmp #2439/#2440), and the `production-profile` catalogue
scenario combining all of it over the durable store in crash cycles
(rmp #2441). The checkers found four engine isolation defects on arrival
(rmp #2445, #2446), all fixed and regression-pinned.

## Defects surfaced by this coverage work

The coverage work exercised the engine against these scenarios and found:

1. **`MERGE … ON CREATE/ON MATCH SET` dropped an expression right-hand side**
   (fail-silent): `ON MATCH SET n.n = n.n + 1` committed but never applied.
   **Fixed** — the merge operators now evaluate a non-literal RHS per-row
   (openCypher TCK unchanged at 3897/3897). The `merge-rel` scenario is the
   regression guard.
2. **`CALL … YIELD … WHERE <pred>`** silently ignored the `WHERE` filter
   (read-only). **Fixed** (#1966) — the visitor now captures `Call.Where` and
   the translator lifts it as a `Selection` over the `ProcedureCall`. The
   engine-level guard is `cypher/call_yield_where_test.go`; since rmp #2462 the
   DST also holds the procedure form to the harness's DDL model on every
   schema-introspection check (`checkCallYieldWhere`, see
   [DST language-surface gaps](dst.md#language-surface-gaps-rmp-2462)).
3. **k-shortest multigraph semantics** diverge between `YenKShortest` (dedups by
   node sequence, cheapest parallel edge) and `Loopless`/`Eppstein` (parallel
   edges as distinct paths). Recorded for adjudication (#1967).
4. **A list-valued column is stringified on the Bolt wire.**
   `bolt/server/session.go`'s `exprValueToPackstream` documents an
   `expr.ListValue` case but its switch has none, so a list column falls through
   to the `default` arm and is emitted as a PackStream **String** (`"[1, 2, 3]"`)
   instead of a PackStream **List**. A literal list (`RETURN [1,2,3]`) is
   stringified identically, so this is the RECORD encoder rather than parameter
   binding — a bound list evaluates correctly (indexing, `size`, and equality
   against a literal list all agree). Found by the wire parameter matrix
   (rmp #2462), which PINS the current rendering so a fix flips the probe
   deliberately.

## Documented debt / out of scope

- **GraphML round-trip under fault** is not yet covered: its ergonomic entry
  point is a property-graph, not the edge-list the fault scenario builds. CSV
  and JSONL round-trips are covered. (A property-graph fixture would extend this
  to GraphML.)
- **Snapshot isolation for read transactions** shipped in rmp #2307 (sprint
  334) via MVCC version chains rather than the copy-on-write epic (#1671) once
  considered for it. The DST scenarios assert no-dirty-read, which the stronger
  contract still satisfies; the isolation level itself is gated by
  `cypher/readtx_snapshot_test.go` and `bolt/server/e2e_readtx_snapshot_test.go`.
- **Multi-member WAL group-commit coalescing / fail-all** is engine-unreachable
  (serialised under `visMu`) and is covered by store-layer unit tests, not the
  DST — see the group-commit clarification above.
