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
| DDL (index + UNIQUE constraint) across the checkpoint/snapshot boundary | `ddl-checkpoint-crash`; `constraint-enforce` and `index-diversity` now checkpoint too | the checkpoint's reclaimed WAL prefix COVERS the DDL frames (measured on the SimDisk image), the pure-snapshot phase replays ZERO WAL ops, and the recovered schema still enforces UNIQUE, answers every index seek, and matches `SHOW`/`db.*` |
| CSV / JSONL export→import round-trip under fault | `io-roundtrip-fault` | a clean round-trip reproduces the modelled edge set exactly; an export under ENOSPC fails with a typed error and leaves no partial artifact a re-import would accept |
| Crash **during** the snapshot publish, at each step of the crash-atomic swap | `checkpoint-crash-storm` | acked ⊆ recovered ⊆ issued across a crash inside the publish window; a stranded backup is promoted by recovery (measured on the durable image and on `store.recovery.snapshot.promoteParentFsync`), never a half-published snapshot |

The `DiskConfig.FaultRate` knob and five `SimDisk` primitives back these
scenarios — four faults (`CorruptRange`, `ArmSyncFaultAt`,
`ArmParentDirSyncFaultForPath`, `ArmRenameFaultForPath`) and one crash-window
selector (`ArmRenameWritebackForPath`, which chooses whether a rename's dirent
had reached stable storage when the crash landed). All default to inert, so
existing scenarios are byte-identical.

### Crash during the snapshot publish (rmp #2465, closing #1827)

Until sprint 347 a checkpoint could never be *interrupted*: `SimStore.Checkpoint`
is synchronous and always ran to completion, and `Simulator.maybeCheckpoint`
treats any checkpoint error as a hard run failure. The whole interrupted-publish
half of the durability contract was therefore unexercised, and recovery's
snapshot-promote repair — the block in `store/recovery` that promotes a stranded
`snapshot.bak` back to the live name, marked by the
`recovery.snapshot-promote-post-rename-pre-fsync` crash point — was **dead code
under simulation**.

Two things had to change before the window could be reached at all, and both
were found by measurement rather than assumed:

* **The renames could not fail.** Every other step of the publish
  (`write+fsync components → fsync staging dir → archive rename → publish rename
  → fsync parent`) could already be faulted, but the two renames could not, so
  the publish path's own archive-restore branch was unreachable.
  `ArmRenameFaultForPath` closes that gap. The task's premise that the
  *parent fsync* also could not fail was **wrong**: the pre-existing
  path-keyed `ArmParentDirSyncFaultForPath` already targets it, which a probe
  confirmed.
* **A crash in the publish window manufactured a false total loss.**
  `SimDisk.Crash` revokes *every* not-yet-fsync'd dirent, and the publish issues
  its two renames back to back with no fsync between them — so the crash dropped
  both the archived backup and the newly published snapshot, an outcome no real
  filesystem produces (a lost rename leaves the *old* name). That single
  modelled outcome is also the reason the promote repair was unreachable: it
  exists precisely for "the archive rename reached disk, the publish rename did
  not". `ArmRenameWritebackForPath` selects that other, equally legal branch of
  the crash-window non-determinism, one rename at a time and opt-in.

`checkpoint-crash-storm` then crashes at three points of the swap
(`stranded-backup`, `publish-rename`, `archive-rename`) while concurrent Bolt
committers are still writing — the publish is checkpoint phase 2 and holds no
commit lock, so the window is genuinely raced, which the run measures as durable
commits landing *during* the interrupted checkpoint.

The DST does not observe crash points directly (see
`CoverageTracker.UnobservableSignals`): `crashpoint.Breakpoint` is compiled out
without the `gograph_crashinject` tag and SIGKILLs the process with it, which
would kill the test binary instead of producing the harness's in-process crash.
Bridging it would mean adding a pluggable handler to a production-callable
package. The scenario instead reproduces the *window* the site marks and observes
the *branch* it guards through surfaces that already exist — the durable image
(backup-only before the reopen, live-only after it) and store/recovery's own
exported `store.recovery.snapshot.promoteParentFsync` counter.

### DDL across the snapshot boundary (rmp #2464)

Until sprint 347 every DDL-issuing scenario (`schema-chaos`, `constraint-enforce`,
`index-diversity`) ran **WAL-only**, so recovery always replayed the
`CREATE INDEX` / `CREATE CONSTRAINT` frames and the snapshot's schema components
(`store/snapshot/constraints.go`, `indexdefs.go`, `indexes.go`) were never the
source of a recovered index or constraint. The loss mode the checkpointer's
phase-3 self-sufficiency re-verification exists to prevent — truncating the WAL
prefix that first *declared* a constraint or an index (#1334 / #1464 / #1755) —
was therefore never exercised.

`ddl-checkpoint-crash` occupies that intersection directly, and
`constraint-enforce` and `index-diversity` now enable in-loop checkpointing so
their existing post-recovery oracles adjudicate a **snapshot-loaded** schema.
A `CheckpointConfig` is INERT unless the run loop calls `maybeCheckpoint`, which
only the default `Simulator.Run` does automatically; each custom loop wires the
call and each scenario carries a terminal gate asserting a **non-zero checkpoint
count**, so a configuration that stops taking effect fails the run rather than
passing quietly.

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
4. **A list-valued column was stringified on the Bolt wire.**
   `bolt/server/session.go`'s `exprValueToPackstream` documented an
   `expr.ListValue` case but its switch had none, so a list column fell through
   to the `default` arm and was emitted as a PackStream **String**
   (`"[1, 2, 3]"`) instead of a PackStream **List**. A literal list
   (`RETURN [1,2,3]`) was stringified identically, so this was the RECORD encoder
   rather than parameter binding — a bound list evaluated correctly (indexing,
   `size`, and equality against a literal list all agreed). It affected every
   list-producing construct: `collect()`, `labels()`, `keys()`, `nodes(p)`,
   `relationships(p)`, list literals, and list-valued properties; `nodes(p)`
   additionally lost all node structure. Found by the wire parameter matrix
   (rmp #2462), which PINNED the rendering so a fix would flip the probe
   deliberately. **Fixed** (#2513) — the arm encodes element-wise with the
   negotiated Bolt major threaded through, so nested containers and entity or
   temporal elements encode structurally on both 4.4 and 5.x. The pin is now the
   live assertion, and `internal/sim/wire_list_encoding_test.go` carries the
   end-to-end matrix.

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
