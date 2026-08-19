# Deterministic Simulation Testing (DST)

This document describes GoGraph's Deterministic Simulation Testing harness:
its architecture, the invariants it enforces, the scenario catalogue, and how
to run, reproduce, replay, and shrink a failing run.

The harness lives in [`internal/sim/`](../internal/sim) with a command-line
driver in [`cmd/sim/`](../cmd/sim). It is modelled on TigerBeetle's VOPR
(Viewstamped Operation Replicator simulator), adapted from a distributed
consensus system to a single-node graph database: the fault surface here is the
Bolt protocol, the WAL/snapshot recovery path, and the ACID-commit liveness of
the engine, rather than network partitions and view changes.

For the broader production-readiness battery (shape generators, invariant
checkers, fault-injection packages, dataset loaders), see
[docs/test-battery.md](test-battery.md). For the three test layers (short /
soak / nightly), see [docs/test-layers.md](test-layers.md).

---

## Table of contents

1. [Why deterministic simulation](#why-deterministic-simulation)
2. [Architecture](#architecture)
3. [Execution modes](#execution-modes)
4. [Determinism and reproducibility](#determinism-and-reproducibility)
5. [Invariants checked](#invariants-checked)
6. [The search-algorithm battery](#the-search-algorithm-battery)
7. [Scenario catalogue](#scenario-catalogue)
8. [Crash and recovery](#crash-and-recovery)
9. [The key and weight codec matrix (rmp #2473)](#the-key-and-weight-codec-matrix-rmp-2473)
10. [MVCC multi-session and concurrency coverage](#mvcc-multi-session-and-concurrency-coverage)
11. [Bulk-import publication parity (rmp #2466)](#bulk-import-publication-parity-rmp-2466)
12. [Language-surface gaps (rmp #2462)](#language-surface-gaps-rmp-2462)
13. [Swarm, coverage, and cross-checking modes](#swarm-coverage-and-cross-checking-modes)
14. [Command-line usage](#command-line-usage)
15. [Reproduce, replay, and shrink](#reproduce-replay-and-shrink)
16. [Extending the harness](#extending-the-harness)

---

## Why deterministic simulation

A deterministic simulation drives the real engine through a long, randomised
sequence of operations and faults, where **every source of non-determinism is
seeded from a single master seed**. The entire run is therefore a pure function
of that seed: a failure found on seed *S* reproduces exactly on seed *S*, on any
machine, every time. This turns a rare, timing-dependent bug into a
deterministic, replayable, shrinkable artefact.

The engine is checked against a **correct-by-construction oracle** — an
independent shadow model of what the graph must contain — after operations and
at every crash-recovery boundary. Any divergence between the engine and the
oracle is a bug.

## Architecture

| Component | File | Responsibility |
|---|---|---|
| Seed | `seed.go` | PCG-based PRNG ([`math/rand/v2`](https://pkg.go.dev/math/rand/v2)); the single source of randomness. Sub-seeds for the checker and the disk are XOR-derived so the workload draw stream is independent of check cadence and fault injection. |
| Virtual clock | `clock.go`, `internal/clock` | A 1 ms-per-tick logical clock injected into checkpoint cadence and the Bolt transaction reaper, so time-dependent behaviour is deterministic. |
| SimDisk | `disk.go`, `diskfs.go` | An in-memory faulting disk backing the WAL and the checkpoint/snapshot, with seed-driven sector-fault injection, a finite-capacity budget that injects disk-full (`ENOSPC`) on the WAL append+sync path, and a `Crash()` that revokes not-yet-`fsync`-ed directory entries. It satisfies the filesystem seams of every persistence package — `store/snapshot`, `store/csrfile`, `store/recovery`, `store/checkpoint`, and `store/wal` (via `wal.OpenFS` / `simWALFS`, so the WAL writer's crash-safe prefix truncation runs over the in-memory disk) — so the whole stack, not just the WAL, is backed by the simulated disk. |
| GraphOracle | `oracle.go` | The shadow model: a minimal, obviously-correct map of the nodes and edges the engine must hold after the workload's operations. It advances only on a committed write, so it always equals the engine's durable, acknowledged state. |
| InvariantChecker | `checker.go` | Compares the engine against the oracle: count parity, sampled existence, full post-recovery durability, and index consistency. |
| Actors and workloads | `actor.go`, `workload.go` | Honest writer/reader, bounded-churn writer, malformed sender, and the concurrent-mode bad actors (Bolt abuser, overload actor, slow consumer, schema changer). A workload is a weighted mix of actors. |
| Simulator | `sim.go` | The single-goroutine, tick-driven safety loop: select an actor, run its operation, advance the oracle, check invariants, and (when enabled) crash and recover. |
| Search battery | `search_check.go` and `search_*.go` | Runs the `search/` algorithms and validates their answers against independent references (see [below](#the-search-algorithm-battery)). |

## Execution modes

`cmd/sim --mode` selects the harness:

- **`engine`** (default) — the single-goroutine, tick-driven safety loop over the
  real `cypher.Engine`. Fully bit-reproducible from the seed; this is the only
  mode that trace recording, scripted replay, and shrinking operate on.
- **`wire`** — drives the *real* `bolt/server` over an in-memory `net.Listener`
  (`SimListener`/`SimConn`) with a Bolt v5 client (`WireClient`), exercising the
  genuine protocol path without a TCP socket.
- **`concurrent`** — N real client goroutines over the Bolt wire; interleaving is
  not seed-controlled, so correctness is an eventual-consistency oracle plus
  `goleak`/no-panic guards rather than bit-reproducibility.
- **`liveness`** — the two-phase safety→liveness flow: after the safety phase,
  faults are healed and the harness asserts the system *converges* (all in-flight
  work drains, the oracle equals the engine) within a bounded budget, with a
  watchdog that classifies a non-converging run as resonance (deadlock/livelock)
  versus budget-exceeded.

## Determinism and reproducibility

Determinism is a **load-bearing invariant** of the harness, not a nicety: it is
what makes a failure replayable and shrinkable. The deterministic mode
guarantees that the same seed yields the same operations, the same fault
schedule, and the same verdict. Concretely:

- All randomness flows from one `Seed`; the checker and disk draw from
  XOR-derived sub-seeds so changing the check cadence or fault rate never
  perturbs the workload.
- No Go map-iteration order is ever allowed to influence an operation, a check
  result, or a violation message. Accessors that feed seed-driven choices sort
  their output first.
- The search battery uses integer-valued weights so its comparisons are exact;
  the only floating-point comparisons (centrality, PageRank) use an explicit
  epsilon with a pinned worker count, because a parallel float reduction is not
  bit-identical.

## Invariants checked

The `InvariantChecker` (`checker.go`) classifies every breach with a typed
`ViolationKind`:

| Kind | Meaning |
|---|---|
| `ACID_ATOMICITY` | A write applied partially, or uncommitted state leaked in at a crash boundary. |
| `ACID_CONSISTENCY` | The engine disagrees with the oracle's node/edge counts, or an index disagrees with its base data. |
| `ACID_ISOLATION` | A reader observed the partial writes of an in-flight transaction. |
| `ACID_DURABILITY` | A committed operation did not survive crash recovery. |
| `GRAPH_INTEGRITY` | A structural invariant broke (e.g. an edge with a missing endpoint, or the engine graph diverging from the model). |
| `ORACLE_DEVIATION` | An engine/oracle disagreement not more specifically classified. |
| `SEARCH_DIVERGENCE` | A `search/` algorithm disagreed with its independent reference. |

The base checks are: node- and edge-count parity; sampled existence of oracle
nodes and edges in the engine; a full (non-sampled) durability scan at every
crash boundary; and a thorough index-consistency check that cross-checks the
index-seek path against a full scan.

An access-path parity oracle (`access_parity.go`, wired into the
`index-diversity` scenario) additionally runs each predicate shape — equality,
bounded range, `STARTS WITH`, and `IN`-list — on an indexed property in both
its literal and its parameterised spelling, asserting identical result
multisets and, via the engine's physical `Explain` rendering, the same
access-path leaf operator (seek vs scan) for both arms; a `Profile` probe
asserts non-zero db-hits for a data-touching query. A companion plan-stability
oracle captures the `Explain` rendering of the fixed probe set at scenario
start and asserts it is byte-identical after every crash/recovery (plan-cache
rebuild) and at the end of the run. This closes the rmp #2414 blind spot: a
parameterised predicate that full-scans while the identical literal seeks
returns correct answers and is invisible to every result-only oracle.

A seek-result diversity oracle (`index_seek_results.go`, also wired into the
`index-diversity` scenario) result-verifies the non-equality index read paths
the thorough index-consistency check does not cover: the bounded and half-open
btree range (driving `RangeFrom`/`RangeCountFrom`), the `STARTS WITH` prefix
rewrite, and `IN`-shaped predicates (both the `IN`-list spelling and its
`UNWIND` twin, which may plan differently but must answer identically). Each
arm runs in its literal and its parameterised spelling and must reproduce, as
an id-multiset (and, for the range and prefix count arms, as a cardinality),
an independent reference built from one plain label scan filtered client-side
— a path that touches no index machinery. The checker is stateful: at the end
of the run it asserts non-vacuity (at least one arm returned rows at least
once), so a run that only ever compared empty sets is reported instead of
passing silently.

A schema-introspection oracle (`schema_introspect.go`, wired into the
`schema-chaos`, `index-diversity`, `constraint-enforce`,
`constraint-existence`, and `ddl-checkpoint-crash` scenarios) keeps the
harness's own model of every index and constraint the scenario has issued
(`SchemaModel`) and, at every DDL flip,
after every crash/recovery, and at the end of the run, asserts that BOTH of
the engine's introspection surfaces — the `SHOW INDEXES` / `SHOW CONSTRAINTS`
statements and the legacy `db.indexes()` / `db.constraints()` procedures —
reproduce the model exactly (full column shape: name, kind, state,
entityType, labelsOrTypes, properties, including the `__uniq__<Label>.<prop>`
backing-index row a UNIQUE constraint implies) and agree with each other on
the shared (name, type) enumeration. Each check also drives one
`SHOW ... YIELD ... WHERE ... RETURN` projection per statement kind (one
through a `YIELD` alias) against the model-side filter, and executes
`CALL db.schema.visualization()` end-to-end. A model mismatch is an
`ORACLE_DEVIATION`; a disagreement between the two engine surfaces is a
`GRAPH_INTEGRITY` violation. This closes the rmp #2455 blind spot: before it,
no simulation ever invoked `SHOW` or the `db.indexes()`/`db.constraints()`
procedures, so a recovered-DDL divergence was invisible.

A statistics-regime oracle (`stats_regime.go`, wired into the
`index-diversity` scenario) drives `CALL db.stats.refresh()` — the sole entry
point that builds the planner's approximate statistics (`graph/index/stats`:
HLL NDV sketches, exact top-k MCVs, equi-depth histograms) — once before the
crash window opens, once after every crash/recovery, and at seed-chosen ticks.
It pins the procedure's contract: exactly one `ok`/`detail` row, `ok=true`
with the rebuilt detail on a completed rebuild, and an in-band rate-limit
refusal (never an error) on a back-to-back second call. The fixed probe
battery must answer identically immediately before and immediately after
every refresh — statistics may change the plan, never the result — and a
refresh-correlated `Explain` change is legal, so it is reported through a
counter rather than failed. After every recovery the checker asserts the
pinned post-crash regime: the collector is per-engine and in-memory, so a
recovered engine reports zero tracked `(label, property)` pairs
(`Engine.StatsTrackedPairs`) until its fresh rate limiter allows the next
rebuild, which must succeed immediately. A terminal non-vacuity gate requires
at least one completed rebuild with a non-zero tracked-pairs observable and
at least one exercised refusal. This closes the rmp #2456 blind spot: before
it, no simulation ever invoked the refresh, so the cost model only ever ran
in its no-statistics regime and the statistics path was 0% covered by the
DST.

## The search-algorithm battery

The `search/` package — traversal, path-finding, and analytics — is the
module's headline capability. `CheckSearch` (`search_check.go`) brings it under
the DST. It runs only in the single-goroutine deterministic loop (it needs a
quiescent view of the graph) and performs two independent families of check.

**1. Structural parity.** The engine's full node-set and `(src,dst)` edge-set are
extracted via the *public Cypher read path* — the same path the workload uses,
so no engine-internals API is added — and compared exactly to the oracle's
shadow model. This is strictly stronger than the base checker's count-plus-sample
probes: it proves the engine graph is identical to the model, which lets the
algorithm checks run on the model as a faithful stand-in for the engine's
contents.

**2. Algorithm correctness.** Each `search/` algorithm is run on the graph and
its answer is compared to an **independent naive reference** computed directly
from the oracle's edge set — never from the data structure handed to `search/`,
so a builder bug cannot hide. The cardinal rule is to **compare an invariant of
the answer, never a non-unique witness**:

| Family | Algorithms | Comparison invariant |
|---|---|---|
| Reachability | BFS, DFS | The reachable **set** from a source (order-independent). |
| Components | WCC | The partition, **up to relabelling**. |
| Strong connectivity | Tarjan SCC | The partition, up to relabelling (double-reachability reference). |
| Ordering | topological sort | *Validated* as a valid order (every edge forward; a permutation of the edge-incident nodes), since the order is not unique; cyclic graphs must return `ErrCycle`. |
| Closure | transitive closure | Per-pair reachability over edge-incident nodes. |
| SSSP / APSP | Dijkstra, Bellman-Ford, bidirectional Dijkstra, A\*, Floyd-Warshall, Johnson, Dijkstra-APSP | The **distance map**, not path identity; serial and parallel variants must agree exactly. |
| MST | Kruskal, Prim | The **total weight** plus spanning-forest validity, not the edge set. |
| Flow | max-flow (Dinic), Edmonds-Karp, Stoer-Wagner | The flow **value** / cut **weight**, with max-flow = min-cut as a second invariant. |
| Matching | Hopcroft-Karp, Hungarian | Matching **cardinality** / assignment **total cost**, not the matching itself. |
| Euler | Hierholzer | *Validated* circuit (uses every edge once, closed); non-Eulerian graphs must return `ErrNoEulerian`. |
| Centrality | betweenness (parallel, weighted) | Per-node value within an epsilon, against a from-definition Brandes reference, with a pinned worker count. |
| PageRank | PageRank | The rank vector within a convergence-aware epsilon, against an independent power-iteration reference matching the damping, dangling-mass redistribution, and teleport model. |
| Community | Leiden, label propagation | Determinism, partition validity, and **no planted clique is split** (a merge is legitimate — the modularity resolution limit), not exact recovery. |
| Cohesion | k-core, biconnected components | Per-node coreness; the articulation-point and bridge sets, against remove-and-recount references. |
| K-shortest | Yen, bounded loopless, Eppstein | The **sorted cost multiset** of the first *k* paths, against a brute-force simple-path enumeration; the loopless worst case is bounded via `MaxPops`. |

Weights for the weighted algorithms are synthesised deterministically per edge,
so the algorithm checks need no change to the workload or the engine's stored
data; the families that need a specific shape (flow networks, bipartite graphs,
Eulerian graphs) generate their own deterministic fixtures from the tick.

The `search` scenario runs this battery periodically and at the end of a run;
the `search-crash` scenario additionally runs it immediately after every
crash-recovery cycle, so the algorithms are validated against a graph that has
actually survived WAL recovery — the DST-unique value for `search/`.

## Scenario catalogue

A scenario is a named, self-contained configuration (seed, workload, fault
schedule, budget, mode, checks). `cmd/sim --list-scenarios` prints them.

| Scenario | Mode | Stresses |
|---|---|---|
| `crash-storm` | deterministic | Frequent crash + recovery via the full snapshot + WAL path on the SimDisk (durability). In-loop checkpointing publishes a real self-sufficient snapshot and truncates the WAL prefix every 30 ticks, so a crash that follows a checkpoint recovers from the snapshot plus the WAL tail through `recovery.OpenFS`; the durability oracle is asserted after every recovery regardless of which path ran. |
| `disk-full` | deterministic | Honest writes against a finite SimDisk: `ENOSPC` on the WAL append+sync path plus crash/recovery. Asserts atomic fail-stop durability — a commit that cannot durably write never advances the oracle, and after recovery no acknowledged commit is lost and no uncommitted state leaks in. |
| `write-heavy` | deterministic | 80/20 write/read; the write path and oracle parity. |
| `read-heavy` | deterministic | 20/80 write/read; the read path and isolation. |
| `schema-chaos` | deterministic | Index AND constraint create/drop/re-create under write load (the constraint churn uses the modern `FOR … REQUIRE` grammar with `IF NOT EXISTS` on a label the workload never writes), with the schema-introspection oracle holding `SHOW INDEXES`/`SHOW CONSTRAINTS` and `db.indexes()`/`db.constraints()` to the harness's DDL model after every flip and at the end, plus the full index-consistency check. |
| `constraint-enforce` | deterministic | UNIQUE enforcement on EVERY engine-supported route (rmp #2455): under UNIQUE(Person.name) — declared in the legacy `ON … ASSERT` grammar — and a numeric UNIQUE(Num.val) — declared in the modern `FOR … REQUIRE` grammar — the workload interleaves, per route, writes that must commit with writes that must be rejected with a typed constraint-violation error applying nothing: duplicate-name CREATEs, cross-node renames via `SET n.name` (a committed rename frees the old value), `MERGE … ON CREATE SET` duplicates (the merge-created node must not survive a rejection), `SET n:Person` promotions of `Plain` nodes whose name collides with a live Person's (the engine rejects the label write and the label does not stick — the #2352 contract), and numeric duplicates including a FLOAT spelling of a held INTEGER value (int/float numeric identity, #1910). A per-op prediction adjudicates every outcome; the schema-introspection oracle holds `SHOW`/`db.*` to the declared DDL at the start, after every crash/recovery, and at the end; and a terminal non-vacuity gate requires every route/outcome arm (twelve in all) to have actually occurred. Since rmp #2464 in-loop checkpointing is enabled, so the constraint DECLARATIONS cross the SNAPSHOT boundary rather than only the WAL one: a checkpoint truncates the WAL prefix holding the `OpCreateConstraint` frames, after which a recovered constraint can only have come from the snapshot's `constraints.bin`. A terminal gate requires the checkpoints to have actually fired — a `CheckpointConfig` is INERT unless the custom run loop calls `maybeCheckpoint`, so the count is asserted rather than the configuration. |
| `type-coverage` | deterministic | Property type system: nodes carry a value of every round-tripping Cypher kind (string, integer, float, boolean, list, a plain ISO-8601 string) plus — since rmp #2457 — all six genuine TEMPORAL types (`DATE`, `LOCAL DATETIME`, `DATETIME`, `LOCAL TIME`, `TIME`, `DURATION`) bound as real temporal parameters, plus a never-set key that must read `NULL`. Each value is read back through the engine and asserted on BOTH its canonical rendering and its expr **kind**, so a temporal that degrades to an untagged string — same text, different type — fails even though its rendering is unchanged; the plain ISO-8601 string property is the deliberate control that must stay a string. Checkpointing is enabled alongside crashes, so the temporals are proven across both durable paths: a crash before the next checkpoint restores them by WAL replay, a crash after one restores them from the published snapshot (the checkpoint truncates the WAL prefix it folded). Since rmp #2468 the run additionally ends by crossing the snapshot boundary DELIBERATELY: it publishes a final checkpoint, measures the WAL image going to zero, crashes, and requires the recovery to replay ZERO WAL ops before re-running the whole battery — so the ENTIRE matrix (string, integer, float, boolean, list, the plain-ISO control, the six temporals and the absent key), and not only the temporals, is proven to come back out of the snapshot codec rather than out of a replay. A temporal `ORDER BY` oracle additionally compares the engine's `ORDER BY n.d, n.id` sequence against an ordering the harness computes from the temporals it modelled, over dates deliberately laid out so temporal order differs from id order. Since rmp #2459 the list-valued property is no longer only read back whole: the LIST-expression surface over STORED data is driven against expectations the harness computes from the list it modelled — the subscript `n.lst[0]`, a NEGATIVE subscript counting from the end, both out-of-range directions (verified against this engine to yield `NULL` rather than an error or a clamp), `size(n.lst)`, the half-open bounds-clamping slice `n.lst[0..2]`, `reduce(acc = 0, x IN n.lst \| acc + x)`, `UNWIND n.lst` aggregated by `count`/`sum`/`collect`, and `WHERE <elem> IN n.lst` counted over the whole label. Each column is asserted on its expr **kind** as well as its value, so a subscript that returned the whole list, or an out-of-range read that returned an element, is reported as the type error it is. `collect` is compared as a MULTISET, since openCypher does not specify the input order an aggregate observes; the list's ORDER is pinned absolutely by the subscript and slice columns instead, so a reversed list still fails. Membership is driven twice — once with an element the model really holds and once with an element no modelled list can contain, whose count must be zero — so the probe is proven to discriminate. The per-node probes take a bounded, stride-based sample that always includes the first and the newest node, while membership scans the whole model; and a modelled list that is empty, non-numeric, or carries fewer than two distinct elements across the entire run is itself reported, so the arm cannot pass by being vacuous. |
| `edge-properties` | deterministic | The relationship write surface on a directed MULTIGRAPH: KNOWS edge instances carry a unique `eid`, a `since` (ISO string), and a `weight` (float), including PARALLEL twins between the same endpoints; the workload mutates individual instances with standalone `SET r.weight`, `REMOVE r.since`, `SET r.since = null` (the SET-path removal, counted per instance since rmp #2501), and `DELETE r`, each pinned by `WHERE r.eid`. The per-instance shadow model verifies every surviving instance's properties round-trip (a mutated instance's parallel twin keeps its own map), every deleted instance stays absent with both endpoint nodes alive, and the per-op counters oracle pins each op's reported effect set — periodically and after each crash/recovery, so the by-handle parallel-edge instance identity survives WAL recovery. Since rmp #2468 the scenario also CHECKPOINTS (every 55 ticks, with `Simulator.maybeCheckpoint` wired explicitly into its custom run loop) and ends by crossing the snapshot boundary deliberately: a final checkpoint whose WAL truncation is measured to zero, then a crash whose recovery must replay ZERO WAL ops. That is what puts the per-handle snapshot component (`edgehandles.bin`) on the critical path — `labels.bin` and `properties.bin` deliberately collapse parallel edges onto one per-pair record, so a snapshot that lost the per-handle bag would hand every twin of a pair the per-pair UNION of their maps, which the eid-pinned read-back refuses. |
| `index-diversity` | deterministic | Index-type diversity: a HASH (string), a BTREE (numeric), and a BTREE (string) index are created over an above-threshold graph (engaging the morsel-parallel backfill phase), then write churn + crash/recovery run while the thorough seek-vs-scan consistency check confirms each index agrees with its base data — for both kinds and both value types, including after WAL recovery re-registers and re-backfills them. The scenario also carries the access-path parity and plan-stability oracles: literal vs parameterised predicates (equality, range, `STARTS WITH`, `IN`-list) must agree on results and on the physical access path, and the fixed probe set must re-plan byte-identically after every crash/recovery; and the seek-result diversity oracle: bounded and half-open ranges, `STARTS WITH`, and `IN`-shaped predicates (list and `UNWIND` spellings), literal and parameterised, must reproduce an independent full-scan reference as id-multisets and counts, with a terminal non-vacuity assertion; and the schema-introspection oracle (rmp #2455): `SHOW INDEXES` and `db.indexes()` must reproduce the harness's model of the three declared indexes (name, kind, label, property) after the backfill, after every recovery, periodically, and at the end; and the statistics-regime oracle (rmp #2456): `CALL db.stats.refresh()` runs before the crash window (with a back-to-back throttle probe), after every recovery (the recovered engine must report zero tracked statistics pairs, then rebuild immediately on its fresh rate limiter), and at seed-chosen ticks — with the probe battery asserted result-identical across every refresh, refresh-correlated plan changes reported rather than failed, and a terminal non-vacuity gate (at least one rebuild that published statistics and at least one rate-limit refusal). Since rmp #2464 in-loop checkpointing is enabled, so the three index DEFINITIONS cross the SNAPSHOT boundary: a checkpoint truncates the WAL prefix that declared them, after which the post-recovery consistency and introspection checks are validating definitions loaded from the snapshot's `indexdefs.bin` and `indexes/` components rather than a replayed `CREATE INDEX`. A terminal gate asserts a non-zero checkpoint count. |
| `ddl-checkpoint-crash` | deterministic | **DDL across the checkpoint/snapshot boundary** (rmp #2464). Every other DDL-issuing scenario used to run WAL-only, so recovery always REPLAYED the `CREATE INDEX`/`CREATE CONSTRAINT` frames and the snapshot schema components (`store/snapshot/constraints.go`, `indexdefs.go`, `indexes.go`) were never the source of a recovered index or constraint — leaving the loss mode the checkpointer's phase-3 self-sufficiency re-verification exists to prevent (#1464/#1755: truncating the WAL prefix that first DECLARED a constraint or index) unexercised. Each phase issues DDL, writes data, publishes a real checkpoint, crashes, and reopens through real recovery. Phase 1 declares a `UNIQUE(Person.key)` constraint and a hash index on `Person.city` BEFORE the first snapshot and crashes with an EMPTY WAL; phase 2 declares a btree index on `Person.age` AFTER that snapshot and crashes leaving a genuine WAL tail, so both the pure-snapshot and the snapshot+WAL-tail recovery paths run. The claim that recovery really used the SNAPSHOT is measured, not assumed: the WAL byte image on the SimDisk is read before and after the DDL and before and after each checkpoint, and the reclaimed prefix must COVER the offset at which the DDL frames ended — so those frames are demonstrably gone from disk before the crash — while the pure-snapshot phase additionally requires the reopen to replay ZERO WAL ops. The recovered schema is then adjudicated on three independent surfaces: the schema-introspection oracle (`SHOW INDEXES`/`SHOW CONSTRAINTS` and `db.indexes()`/`db.constraints()` against the harness's DDL model, rmp #2455), the thorough seek-vs-scan index-consistency check on every declared index, and a UNIQUE accept/reject adjudicator that requires a duplicate CREATE to be REJECTED atomically (exactly one node still carries the key) and a fresh key to be ACCEPTED — the control that proves the rejection discriminates. A terminal non-vacuity gate requires real reclaimed bytes, both recovery paths, and both adjudicator arms. Two sensitivity seams pin the oracles: publishing the checkpoint with the constraint/index spec providers deliberately unwired (the pre-#1464/#1755 checkpointer) makes the checkpointer correctly refuse to truncate, and the WAL-prefix oracle FIRES; and removing the published `indexdefs.bin` component makes recovery fail-stop on the missing component rather than silently recovering a schema-less graph. |
| `checkpoint-crash-storm` | concurrent | **Crash DURING the checkpoint snapshot publish** (rmp #2465, closing backlog #1827). Until sprint 347 a checkpoint could never be INTERRUPTED — `SimStore.Checkpoint` is synchronous and always ran to completion, and `Simulator.maybeCheckpoint` treats any checkpoint error as a hard run failure — so recovery's snapshot-promote repair (the block in `store/recovery` marked by the `recovery.snapshot-promote-post-rename-pre-fsync` crash point) was dead code under simulation. The publish is a five-step crash-atomic swap (write+fsync components -> fsync staging dir -> archive rename `live` -> `live.bak` -> publish rename `staging` -> `live` -> fsync the parent dir) whose backup exists so that at EVERY instant at least one complete snapshot is on disk. The scenario crashes at three points of it, each produced by a one-shot path-keyed `SimDisk` arm rather than by timing: `stranded-backup` (the parent fsync fails and only the ARCHIVE rename is written back, so the live name is gone and the previous snapshot is stranded at `.bak` — the only window that reaches the promote repair), `publish-rename` (the publish rename fails, so the publish path's own best-effort archive-restore runs and must survive the crash), and `archive-rename` (the archive rename fails, aborting the publish before the live snapshot is touched). Each cycle publishes a CLEAN checkpoint first — so there is a live snapshot to archive and the WAL prefix holding those commits is really gone — then interrupts the next one while concurrent Bolt committers are still writing; the publish is checkpoint phase 2 and holds no commit lock, so the window is genuinely raced, which the run MEASURES as durable commits landing during the interrupted checkpoint. The oracle is the standard durability contract accumulated across cycles (acked ⊆ recovered ⊆ issued, failures absent, no torn CREATE). Nothing is assumed: the interrupted checkpoint must return the injected `ErrSimFault`, each armed primitive must report having FIRED (an arm whose path never matched is a silent no-op), and the stranded-backup cycle must show the exact durable-image transition only the promote rename produces — backup-only before the reopen, live-only after it — corroborated by store/recovery's own `store.recovery.snapshot.promoteParentFsync` counter. Two sensitivity seams pin the oracles: destroying the stranded backup after the crash loses acknowledged commits for real (the clean checkpoint already truncated their WAL prefix) and the durability oracle FIRES; and a degenerate one-window plan is rejected by the terminal non-vacuity gate. Since rmp #2473 the same three windows are ALSO driven once per key/weight codec pair by the `codec-matrix` scenario, so the interrupted-publish contract is proven on the version-2 byte-mapper and not only on the string-specialised one. |
| `search` | deterministic | The `search/` algorithm battery over the live graph + structural parity. |
| `cypher-paths` | deterministic | The Cypher-level `shortestPath()` operator: its hop count is compared to an independent BFS over the oracle's KNOWS edges for a bounded, deterministic set of pairs (comparing the path-length invariant, never a specific witness), periodically and after each crash/recovery. `allShortestPaths()` is also verified: every returned path is minimal-length and the path COUNT equals an independent layered-BFS shortest-path count. |
| `cypher-surface` | deterministic | A battery of diverse read shapes — `count`/`sum` aggregation, `WHERE`, `WITH…WHERE`, pattern-count, `OPTIONAL MATCH`, `UNWIND range()`, and `ORDER BY` — is run against independently-computed oracle invariants (scalar values and the sorted-name sequence), broadening the DST's coverage of the Cypher read surface beyond the per-tick parity probe, including after crash/recovery. Since rmp #2452 every Person also carries a `city` from a small seeded vocabulary, and the battery additionally pins: GROUPED aggregation (`count(*)` and `sum(n.age)` BY `n.city` as full row-set equality against the oracle's per-city histogram, plus a mixed INTEGER/FLOAT `CASE` grouping key exercising the exact cross-type single-group equivalence of rmp #2050); `RETURN DISTINCT` over a pattern and a mid-pipeline `WITH DISTINCT` as row operators vs the oracle's distinct-target set; `UNION` over graph rows with complementary AND overlapping predicates (the overlap makes the dedup observable) plus `UNION ALL` duplicate preservation (row count = sum of the arm cardinalities); EXACT `avg`/`stDev`/`stDevP`/`percentileCont`/`percentileDisc` values from oracle arithmetic (1e-9 relative float tolerance; sample vs population stdev, linear-interpolation cont, nearest-rank disc — pinned against `cypher/funcs/aggregators.go`), replacing the former self-referential interval invariants a broken engine could still satisfy; sorted `collect(n.name)` equality; and a terminal non-vacuity assertion (≥ 2 cities, ≥ 2 distinct ages, ≥ 1 KNOWS target). Since rmp #2458 the battery also drives the ENTITY-valued functions, which the DST never called before: `labels(n)` per Person as a label SET (the ordering is unspecified), which gives `SET n:Label` / `REMOVE n:Label` an independent read-back of *which* labels a node carries; `properties(n)` as a key set plus a per-key canonical value, so the whole property map — not merely the individually projected keys — is enforced; `type(r)`, `startNode(r)` and `endNode(r)` over every KNOWS edge vs the oracle's modelled endpoints, evaluated after a `WITH` barrier so only the relationship (never the pattern's node variables) is in scope; and genuine path-materialisation content checks on `MATCH p=(a:Person)-[:KNOWS*1..3]->(b)` — `size(nodes(p)) = length(p)+1`, `size(relationships(p)) = length(p)`, the first node of `nodes(p)` identical to the anchor and the last identical to the matched endpoint (identity via `elementId`, not name), plus the per-(anchor, length) row histogram compared against an oracle TRAIL enumeration that encodes openCypher's relationship-isomorphism rule (a 2-cycle yields a length-2 path back to its start; a self-loop yields exactly one path). `elementId` is pinned to the contract the engine actually implements — the decimal rendering of the same integer `id()` returns, for nodes and relationships alike, with no database or generation prefix — and is asserted stable across two reads within one check, distinct across entities, and one row per modelled entity; stability is deliberately not asserted across ticks, since the battery also runs after crash/recovery. The three NON-deterministic functions carry honest invariants rather than constants: `rand()` draws lie in [0, 1) and are not all identical, `randomUUID()` values match the RFC 4122 version-4 shape and are pairwise distinct within one statement, and `timestamp()` is frozen within a statement (`cypher/stmt_now_reg.go` overrides it alongside the five temporal `now` constructors), lies in the epoch-millisecond window, and is non-decreasing across two statements. The graph-independent literal battery gained the remaining pure scalar surface — `left`/`right`/`ltrim`/`rtrim`/`replace`, `toBoolean` and the four `toXList` conversions, `isNaN`, the trig/log family with `pi`/`e`/`degrees`/`radians`, `sort`, and `size` on lists and strings — each as an absolute constant, plus the identity contract of the `extract`/`filter` STUBS (`cypher/funcs/list_funcs.go`) rather than a pretence that they implement comprehensions. `cot` and `haversin` are absent from the registry and are therefore not probed. Since rmp #2459 the battery also drives the MAP PROJECTION over a REAL node — the entity-property-resolution path the expression battery's literal-map probe (`WITH {a:1,b:2,c:3} AS m …`) cannot reach: `n{.name, .age}`, `n{.*}`, a selector naming a key no Person carries, and a selector mixing a property with a LITERAL entry, all four resolved in ONE scan and each compared against the oracle's modelled property map as a key SET plus per-key canonical values — never as a whole rendering, because `expr.MapValue` is a Go map and its `String()` key order is not stable. The missing-key contract is pinned as VERIFIED against this engine rather than assumed: the key is PRESENT and `NULL`, not omitted, so an engine that started omitting it fires. A result in which every projected map came back with an EMPTY key set is itself reported, so the probe cannot pass without having resolved a single entity property. Since rmp #2460 the battery also drives ORDERING and MULTI-PART query structure, which the simulator had only ever exercised as a single ASCENDING key on a string: `ORDER BY n.age DESC, n.name ASC` as full row-SEQUENCE equality against an oracle comparator (the workload draws ages from 0..99 over hundreds of Persons, so ages collide heavily and the ascending secondary key decides real ties); `ORDER BY n.name DESC`; an ordering on an EXPRESSION (`ORDER BY n.age % 10, n.name`, whose projected key value is asserted alongside the sequence) and one on an AGGREGATE (`RETURN n.city, count(*) AS c ORDER BY c DESC, n.city ASC`); and a Top-vs-Sort equivalence in which `ORDER BY … LIMIT k` must return exactly the first k rows of the unlimited ordering — compared against BOTH the oracle's prefix and the engine's own unlimited arm, and backed by an `Explain` assertion that the two arms really resolve through DIFFERENT physical operators (the fused `Top` versus the full `Sort`), so the equivalence cannot hold vacuously on one identical plan. Pagination is pinned on the same ordering: the same page written with LITERAL and with PARAMETERISED `SKIP`/`LIMIT` must equal the oracle's slice and each other (the literal/parameter parity theme of rmp #2447, here on the pagination clauses rather than on a predicate); `LIMIT 0` returns ZERO rows and a `SKIP` past the end returns ZERO rows — both VERIFIED against this engine in the literal and the parameterised spelling rather than assumed — and `SKIP`/`LIMIT` composed with a DESC ordering pages the REVERSED sequence. Multi-part structure adds the shapes a single `WITH` stage cannot reach: a two-stage pipeline (group and filter in the first `WITH`, then order, truncate and `collect` in the second), TOP-K THEN EXPAND (`WITH n ORDER BY n.age DESC, n.name ASC LIMIT 10 MATCH (n)-[:KNOWS]->(m)`), whose expected count the oracle predicts from the SAME top-k selection — the ordering is made TOTAL by the name tie-break so the expectation is well defined, rather than the assertion being weakened — and `WITH … WHERE` on an AGGREGATED value at the MEDIAN group size, so the predicate keeps some groups and drops others instead of passing them all. A terminal non-vacuity gate proves the sensitive arms were exercised: at least one age tie occurred, a DESC probe returned more than one row, the Top probe's LIMIT genuinely truncated (more rows were available than k), the aggregate filter both kept and dropped groups, and the expand stage counted at least one relationship. A model that cannot define a total order — a Person without a name or an integer age, or two Persons sharing a name — SKIPS the ordering probes rather than comparing against a reference that cannot be right, and the same gate then reports that no check ever ran, so a skipped run is never a silent pass. |
| `null-semantics` | deterministic | Non-degenerate NULL and three-valued-logic coverage (rmp #2453): the writer deliberately creates a seed-chosen fraction (~1/3) of AGELESS Persons (plus a cityless fraction), so both sides of every NULL distinction are populated — unlike `cypher-surface`, whose no-NULL guarantee pins `IS NULL` to the constant 0. The battery asserts, against the oracle's age-present vs age-absent partition: `count(n)` vs `count(n.age)` (aggregate NULL-skipping); `WHERE n.age IS NULL` / `IS NOT NULL` with both populations genuinely non-empty; `sum`/`min`/`max` plus the exact `avg`/`stDev`/`stDevP`/`percentileCont`/`percentileDisc` battery (the rmp #2452 helpers) referenced over the age-bearing subset only; OPTIONAL MATCH NULL-row padding as full row-set equality including the NULL markers (one row per outgoing KNOWS of each ageless Person, a `(name, null)` row when it has none, compared through the canonical value rendering); 3VL predicate arithmetic — `n.age > 30`, `NOT (n.age > 30)` (the NOT NULL = NULL trap), the excluded-middle disjunction, and the partition identity gt + not-gt + is-null = total asserted over the ENGINE-returned counts; and `coalesce(n.age, -1)` making the NULL replacement observable in a sum. The per-op counters oracle pins the ageless CREATE's exact effect set ({1 node, 1 label, 2 properties}), the battery runs periodically, after each crash/recovery, and at the end, and a terminal non-vacuity assertion proves both NULL populations, both 3VL arms, and both OPTIONAL MATCH row shapes (matched and NULL-padded) actually occurred. |
| `pattern-shapes` | deterministic | The join/intersect planner family the single-hop workloads leave dead: 2-hop composition, the cyclic directed-triangle shape (ExpandIntersect), undirected and reverse expansion, the `[:KNOWS\|FOLLOWS]` multi-type union, relationship uniqueness (`(a)-[r1:KNOWS]->(b)-[r2:KNOWS]->(a)` with the r1=r2 binding excluded per openCypher relationship isomorphism), and the bound-both-endpoints ExpandInto shape in literal and `$param` form. A writer builds a two-relationship-type Person graph (KNOWS + FOLLOWS) and plants deterministic motifs — a directed triangle, a mutual KNOWS pair, a KNOWS+FOLLOWS both-types pair, and a KNOWS self-loop — and every `count(*)` is verified against a reference computed independently by composing the oracle's adjacency (ordered edge pairs/triples with pairwise-distinct relationship identity; a self-loop matches an undirected pattern once), periodically, after each crash/recovery, and at the end, with a terminal non-vacuity assertion that every motif shape actually existed. The engine is opened as a multigraph (a simple graph cannot hold two edge types on one node pair) but the writer never re-CREATEs a `(src,dst,label)` edge, so edge identity stays exact. Since rmp #2463 the same scenario also adjudicates the **variable-length path surface**, which the simulator previously only issued as load: exact depth (`*2`, `*3`), the zero-length form `*0..n` (the length-0 binding is the identity — it binds both endpoints to the same node, still applies the far-side pattern to it, survives a relationship type that does not exist, and contributes length 0 / one node / zero relationships to the path functions), the lower-bound-only form `*2..`, the bounded `*1..3`, predicates over the path's intermediate nodes (`nodes(p)[1..-1]`) versus over all of them, multi-type `[:KNOWS\|FOLLOWS*1..2]`, undirected `-[:KNOWS*1..2]-`, and `nodes(p)`/`relationships(p)`/`length(p)` adjudicated both absolutely and for the per-row identity `size(nodes(p)) = length(p)+1 = size(relationships(p))+1`. Every reference is enumerated from the same oracle adjacency as TRAILS — edge-distinct walks, per the openCypher rule that a variable-length pattern must not repeat a relationship within one path — so the battery is sensitive to a Cyphermorphism regression. Enumeration is bounded twice: the whole-graph probes never look past the query's own upper bound (3 hops), and the unbounded `*2..` probe is anchored at a single Person under a depth cap and a walk budget, refusing to assert (rather than comparing a partial count) whenever either limit is reached. Its terminal non-vacuity gate requires the final model to hold enough multi-hop trails to be meaningful AND every arm to have done something: a depth-2 trail and a length-0 binding seen, the multi-type union strictly above the KNOWS-only count, the undirected count strictly above the directed one, the `*2..` probe run on trails deeper than two hops, and the intermediate-node predicate having strictly filtered. |
| `schema-mutation` | deterministic | The node mutation-clause surface on churning Persons: `REMOVE n.tag`, `REMOVE n:Vip`, `SET n:Vip`, `SET n += $map`, `SET n = $map`, plus — since rmp #2454 — the FOREACH write path with two genuinely different bodies: `FOREACH (x IN $list \| CREATE (:Person {name: x}))` and `MATCH … FOREACH (x IN $list \| SET n.tag = x)` over seed-chosen 1..4-element lists. Equivalence is structural: the oracle models each FOREACH as its EXPANSION (the equivalent batch of per-item single statements), and three independent read-backs adjudicate the engine against it — per-name property/label round-trip probes (`CheckSchemaMutation`, including the multi-label `(n:Person:Vip)` pattern), node/edge-count parity, and the per-op counters oracle (rmp #2448) pinning the exact effect set (an N-item FOREACH CREATE must report N nodes / N labels / N properties; a K-item FOREACH SET, K assignments; `FOREACH` over `[]` a committed all-zero no-op). All checks re-run immediately after every crash + checkpoint recovery, and a terminal assert-something-was-seen gate reports whether both FOREACH templates were issued, whether a crash followed a FOREACH op, and whether a post-recovery probe saw a surviving FOREACH-created Person. Since rmp #2461 the scenario also drives the four MERGE families the DST previously left dead, each modelled exactly by the oracle and pinned by the counters oracle: a node MERGE with BOTH action branches (`ON CREATE SET n.mc = 1 ON MATCH SET n.mc = n.mc + 1` — create reports 1 node / 1 label / 2 properties, a match exactly 1 assignment, and a match on a Person whose `mc` a co-actor's `SET n = $map` has wiped reports the ALL-ZERO set, because `null + 1` is null and assigning null removes an absent property); the whole-map `ON CREATE SET n = $map`, whose replace CLEARS the merge key the pattern itself just wrote (1+len(map) properties set, exactly 1 removed) and is therefore NON-IDEMPOTENT when the map omits `name` — the workload always binds it, and `TestMergeSurface_SetAllReplacesMergeKey` pins the destructive variant separately; a whole-pattern `MERGE (a:Person {name:$a})-[r:PAIRED]->(b:Person {name:$b})`, which is ALL-OR-NOTHING — either the whole pattern matches (all-zero) or the whole pattern is created as two FRESH nodes and one relationship (2/1/2/2) even when an endpoint key already exists, so the family runs in its own `wp*` key namespace kept out of the oracle name index while node/edge-count parity and the endpoint-name edge probes still reach it; and `MERGE (n $map)`, which the engine must REJECT at compile time (openCypher TCK `Merge1` scenario [16]), driven as an `OpMalformed` op whose acceptance would fail `checkMergeRejection` on the tick that caused it. `mc` is projected by `CheckSchemaMutation` on every Person, so the ON CREATE/ON MATCH counter value round-trips continuously and survives WAL and snapshot recovery, and a second terminal gate requires all four families issued, both counter branches and the whole-map create branch fired, at least three of the four whole-pattern sub-cases reached, and a post-recovery probe of a surviving counter-MERGE Person. Since rmp #2515 the scenario additionally drives the one MERGE shape the families above cannot reach: a NODE-ONLY `MERGE (n:Person {name:$n})` whose `ON CREATE` / `ON MATCH` action targets a relationship bound by the PRECEDING clause. The whole-pattern `tmplMergePairOuterRel` is immune to the defect (`MergePattern` matches an action target by variable NAME), whereas the node-only `exec.Merge` read the target's row column as a node id — and since rmp #2317 a relationship rides in the row as a bare `expr.IntegerValue` holding its stable HANDLE, so on a graph holding a node whose id equalled that handle the property landed on THAT unrelated node. The statement reports `+properties = 1` either way, so the counters oracle is structurally blind to it. The precondition is a property of the graph, not of the statement, so it is **constructed** rather than awaited: before the first tick the run creates two endpoints plus a decoy, raises the per-graph handle counter to the decoy's node id (`lpg.Graph.SeedEdgeHandle`), creates the relationship so the engine's own write path stamps the colliding handle, and then **verifies** it with `lpg.Graph.FirstEdgeHandle` — a run that could not build the collision fails rather than proceeding to prove nothing. `CheckMergeHandleCollision` re-verifies the collision on every check and after every recovery (handles and node ids are both durable, so it survives), and carries the read-back that discriminates this defect from rmp #2510's: the relationship must hold the modelled value AND no `Person` may carry the key at all, since no template ever writes it to a node. The terminal gate reports whether the family was issued and whether BOTH of its branches fired; since rmp #2554 the branch it could most easily miss is CONSTRUCTED rather than drawn (`mergeHandleBranch` spends the run's first two occasions of an ABSENT merge key on the `ON CREATE` and then the `ON MATCH` template, because a key is absent at most once and a coin flip therefore left the create branch unfired on 1 seed in 8). Both terminal gates are SEPARATE from the verdict and return shortfall clauses rather than violations (rmp #2554): a seed whose workload simply never reached part of the surface is uninformative, not faulty, and no longer exits 1 as an `ORACLE_DEVIATION` — measured at 51 and 54 false failures per 400 seeds before the change, 0 after, on two independent master seeds. The clauses are asserted where the seed and the tick budget are pinned (`TestSchemaMutation_MergeGateWired`, `TestSchemaMutation_ForeachGateWired`, `TestSchemaMutation_NonVacuityGatesAreNotVerdicts`) and witnessed elsewhere. |
| `search-crash` | deterministic | The `search/` battery validated on the crash + recovery-survived graph. |
| `mem-pressure` | deterministic | Over-budget reads (large `UNWIND`, Cartesian, whole-graph `collect`) against clamped logical-resource budgets (`MaxResultRows`/`MaxCollectItems`). Asserts bounded-resource graceful degradation: each over-budget read is refused with a typed error and changes no state, so engine and oracle stay in lock-step and the honest writes still commit — no panic, no partial result, no wedge. A soak-gated companion (`TestMemPressure_Soak`) imposes a real heap ceiling via `debug.SetMemoryLimit` and drives an overload-heavy concurrent wire workload, asserting the same degrade-never-panic contract under genuine GC pressure. |
| `bad-actors` | deterministic | 100% malformed/abuse workload; every op rejected with a typed error, no state change. |
| `overload` | concurrent | Giant transactions / huge `UNWIND` / large result sets / deep variable-length expansion; bounded-resource graceful degradation. |
| `cpu-starvation` | liveness | A compute-hog workload (60% overload) competing with honest queries on a single clamped `GOMAXPROCS` core, then a liveness convergence assertion. Verifies fair scheduling under CPU starvation: the system keeps making forward progress (no deadlock/livelock — the watchdog classifies a stuck run as resonance), no panic, no goroutine leak. Latency percentiles are deliberately not asserted (statistical). |
| `bulk-vs-online` | bulk-vs-online | A concurrent offline bulk CSR load alongside transactional online writes; resource stability. |
| `bulkimport-parity` | deterministic | **Offline bulk-import publication, round-tripped through real recovery** (rmp #2466). `bulk-vs-online` above drives `store/bulk`, whose record is adjacency only — `(src, dst, weight)`, no labels and no properties — so every label, property, relationship type and parallel-edge handle that `store/bulkimport` carries was unexercised. This scenario builds a seed-derived labelled property multigraph through `bulkimport.Builder`, publishes it to a **real temporary directory**, reopens it through `recovery.Open`, and requires the recovered graph to equal a harness model EXACTLY: node set (two-sided — live order *and* per-key presence), labels, properties (**kind and value**, so an integer `7` and the string `"7"` cannot compare equal), and the per-handle multiset of (type, weight, properties) on every pair, including the parallel twins a pair-addressed carriage would collapse. It also pins the package's lifecycle contract **as measured**, reopens the directory a second time and adjudicates again, and pins the publish's byte-reproducibility boundary — an identical republish is byte-identical only while items carry at most one property, because `Node.Properties` is a map (logically identical every run, physically not). **Fault injection is out of reach:** `bulkimport.Publish` is hard-wired to the OS filesystem (`os.MkdirAll`, `os.ReadDir`, the non-seamed `snapshot.WriteSnapshotFullCtx`) and `ImportInto` takes a `storeDir string` with no filesystem in its `Options`, so no `SimDisk` can be placed underneath a publish without a production change — filed as rmp #2518. See [Bulk-import publication parity](#bulk-import-publication-parity-rmp-2466). |
| `csrfile-publish-fault` | deterministic | **The atomic csrfile publish, and since rmp #2478 the whole weight-kind x access-pattern grid.** The publish half is unchanged: a CSR is published through `csrfile.WriteToFileWith` (tmp write -> fsync -> rename -> parent-dir fsync) over the SimDisk, then REPUBLISHED under an ENOSPC bound and under an armed one-shot `Sync` fault — both landing on the temp file before the rename — and the path must hold either the complete prior file (compared byte-for-byte AND reconstructed with the real reader) or, for a first publish that never succeeded, nothing at all. What #2478 added is everything around it. The arm used to publish ONE fixture: a `float64` CSR read back through the default access pattern, so four of the five `csrfile.WeightKind` values and all five `csrfile.AccessPattern` values were never driven. The scenario now ENUMERATES the 5 x 5 grid — `absent`/`uint32`/`uint64`/`float32`/`float64` against `default`/`sequential`/`random`/`will-need`/`dont-need` — and for every cell publishes, opens, reads through `Reader.Read`, applies the hint, reads again, and requires the second read to be byte-identical to the first and both to equal the source CSR; the weights are decoded by an INDEPENDENT little-endian decoder as well as by the package's typed accessors, and `WeightsUint64`/`WeightsFloat64` must REFUSE every kind that is not theirs. It then truncates a published file at four lengths — a cut on an alignment boundary, a cut off one, and the two lengths that bracket `HeaderSize+4` — and requires a typed refusal plus a `Reinterpret` that REFUSES rather than returning a view over bytes the file no longer holds. The ENOSPC atomicity arm is additionally replayed at a weight kind and an access pattern DRAWN FROM THE SEED, so the fault regime is no longer welded to `float64`. Coverage is adjudicated by a SEPARATE shape-only gate that reads the published headers and the applied hints rather than the loop that selected them, so a writer that silently downgraded a kind shows up as a gap, not as coverage. |
| `snapshot-corruption-failstop` | deterministic | **Corruption of a published snapshot COMPONENT** (rmp #2467). The store declares nine typed corruption sentinels — one per durable component — plus manifest size and version guards, and it records a CRC32C for every component in the manifest; until this scenario the only corruption the simulator ever injected was a byte flip inside a WAL frame (`wal-corruption-failstop`), so `SimDisk.CorruptRange` appeared nowhere else outside the disk's own unit tests and not one of the nine sentinels had ever been reached under simulation. The fixture declares a UNIQUE constraint and two indexes (hash and btree), writes propertied nodes, adds PARALLEL typed relationships and deletes a node — which is what makes all nine components present — then publishes one real checkpoint that folds the WHOLE WAL, so the snapshot is the only durable source of the committed graph and a refusal cannot be a fallback onto a stale WAL. Each component then gets a MAGIC arm (byte 0, the header the structural reader validates first) that must produce that component's OWN typed sentinel — `ErrManifestCorrupted`, `ErrCSRCorrupted`, `ErrLabelsCorrupted`, `ErrPropertiesCorrupted`, `ErrMapperCorrupted`, `ErrTombstonesCorrupted`, `ErrEdgeHandlesCorrupted`, `ErrConstraintsCorrupted`, `ErrIndexDefsCorrupted` — and every CRC-covered component also gets a seed-chosen INTERIOR arm, which the manifest CRC must catch even where the structure still parses. Nothing is assumed: each flip is read back and compared against the pre-corruption bytes BEFORE any sentinel is asserted (a no-op corruption would make the arm vacuous), the whole durable image is compared before and after each refused reopen so a failed recovery is proven to have mutated nothing, `db/wal` is required to still equal the post-checkpoint image, and every arm restores the flip (XOR 0xFF is an involution) and reopens CLEAN, recovering the exact committed model — so the refusal is attributable to the corruption and not to the harness. Two behaviours that are deliberately NOT fail-stop are pinned in the same run: a corrupt `indexes/<name>.bin` payload is a rebuild trigger, so the reopen must SUCCEED and the rebuilt indexes must still agree with a full label scan; and the manifest's JSON key-name region is covered by no checksum at all, which the run measures on `commit_ts` — the MVCC clock floor recovery restores — and requires to be CONTAINED (no committed node lost). The manifest guards are probed by shape rather than by bytes: a version past `ManifestVersion` must raise `ErrManifestUnsupported`, and padding INSIDE the JSON object past `DefaultMaxManifestBytes` must raise `ErrManifestTooLarge` — padding after the closing brace does not, because the ceiling bounds what the decoder CONSUMES, not the file's length. A terminal non-vacuity gate requires every component of the intended sweep to have been corrupted, every typed sentinel to have been OBSERVED, every refusal to have left the image and the WAL intact, and the tolerated and un-checksummed arms to have run. Three sensitivity seams pin the oracles: a degenerate one-component plan is rejected by the gate; an arm whose flip changed no byte is rejected; and aiming the interior arm at a byte KNOWN to be outside every checksum (a `commit_ts` key-name character) makes the refusal oracle FIRE, which is what proves it is reachable at all. |
| `codec-matrix` (soak; `internal/sim/codec_matrix.go`) | deterministic | **The key and weight codec matrix across crash and upgrade** (rmp #2473). Seven `(key codec, weight codec)` arms — `string/float64` (the control), `uuid/float64`, `int64/int64`, `binarymarshaler/binarymarshaler`, `int/float64`, `int32/int64`, `uint64/float64` — each run through BOTH the three snapshot-publish crash windows of `checkpoint-crash-storm` and an upgrade scenario that crosses the graceful-restart boundary and then the snapshot boundary. Before this task the simulator drove exactly ONE codec pair, because `cypher.Engine` is fixed at `*txn.Store[string, float64]`, so `snapshot.WriteMapper`'s version-2 byte-mapper — the layout it emits for every key type that is not `string` — had never been written or read by a single simulated crash. Each arm's mapper layout is read back from the DURABLE `mapper.bin` header on the SimDisk and asserted (version 1 for the string control, version 2 for the other six), and the upgrade arm ends by folding every op into one snapshot, MEASURING the WAL to zero and requiring the post-crash reopen to replay ZERO WAL ops — so every key that comes back demonstrably came through the mapper rather than a replay. The oracles are re-expressed over node ORDINALS, with each arm's `keyOf` mapping an ordinal onto its concrete key type, so one adjudicator serves every arm: acked ⊆ recovered ⊆ issued read BY KEY (a key codec that did not round-trip injectively resolves to no node, or to a node carrying somebody else's ordinal), no rejected transaction resurrected, no phantom, and every acknowledged edge back with the weight it was written with. `txn.ErrNoWeightCodec` is provoked as a negative probe on a store built with `txn.NewStoreWithCodec` (key codec, no weight codec) and what the engine actually does is pinned: `AddEdge` with a non-zero weight returns the sentinel, `AddEdge` with a ZERO weight is accepted and buffers an unweighted record that survives a crash, and `AddEdgeWithHandle` returns the sentinel even for a zero weight because that entry point requires a codec unconditionally. The full sweep is a SOAK scenario; the short layer runs the same sweep at the smallest size so the byte-mapper path cannot stop being reached unnoticed. Every arm must confirm at least one weight after a SNAPSHOT-ONLY recovery, so no arm can pass without crossing a checkpoint — see [Weights the snapshot could not persist](#weights-the-snapshot-could-not-persist--closed-rmp-2473-measured-2526-fixed). |
| `txn-oversize` (`internal/sim/txn_oversize.go`) | deterministic | **Transaction-size caps: producer refusal and replay fail-stop** (rmp #2474). The store bounds a single transaction on BOTH sides — `txn.DefaultMaxTxnOps` (16 000 000) refusing a commit with `txn.ErrTransactionTooLarge`, and the recovery-side `recovery.Options.MaxTxnOps` stopping a replay with `recovery.ErrTransactionTooLarge` — because recovery buffers a whole transaction in memory before its `OpCommit` marker, the CWE-770 shape of the store. Neither sentinel had ever been produced under simulation, and not because the workloads were small: `simStoreConfig.maxTxnOps` reached RECOVERY on both cores and stopped there, the store itself being built with the UNCAPPED constructor, so the replay bound was configurable and the commit bound was not. `simstore.go` now passes it to `txn.NewStoreWithOptionsCapped` as well (behaviour-neutral for every existing caller, all of which carry 0 and resolve to the same default). The producer arm opens a store at a deliberately small cap of 32 ops and drives four transactions through the transaction layer directly — ops, not statements, are what the cap counts — measuring: a 33-op and a 128-op transaction are refused with the typed sentinel, and a refusal is NOT settled for by the error alone, because a refusal that appended and then truncated is how a bug becomes permanent loss (#2526): the WHOLE durable WAL image is read off the `SimDisk` before and after and required BYTE-identical (measured 436 -> 436 bytes across both refusals) and the live graph's node count unchanged (4 -> 4). The at-cap transaction is driven AFTER the refusals and must commit and grow the file (436 -> 2084 bytes), so a refusal that had silently poisoned the writer fails here rather than passing as a clean one; the reopen then recovers CLEAN and equal to the model, with all 80 keys of the refused transactions absent. The boundary is pinned rather than inferred from reading the two comparisons — the producer's guard is `len(ops) > cap` and recovery's is `len(pending) >= cap` before appending — and it MEASURES that they agree exactly: 32 ops commits and replays, 33 does neither. The REPLAY cap cannot be reached by driving the engine at all (the producer cap is <= the replay cap by construction, so anything large enough is refused before a frame is written), so the oversize WAL is CONSTRUCTED, one v3 op payload at a time, through the real `wal.Writer` — only the op stream is hand-made, the framing, CRC and fsync are the production ones. A marker-less run of 17 ops under a replay cap of 16 fail-stops with the typed sentinel, keeps exactly the 4-op committed prefix, and is refused by the harness store-open rather than appended onto. Three sensitivity seams pin the oracles, and two of them drive the real defect rather than fabricated evidence: `txn.MaxTxnOpsUnlimited` runs the byte-identical producer plan and commits all four transactions (so the refusals are attributable to the cap and to nothing else); the `over-cap-unlimited` replay arm replays the byte-identical 954-byte file with the cap disabled and recovers all 21 ops (so the fail-stop is not a file the harness built wrong); and the `uncappedProducerSeam` restores the pre-#2474 plumbing, which MEASURES the hazard the producer <= replay invariant exists to prevent — the 33-op transaction is acknowledged durable and recovery then refuses to replay the file at all, so the store does not lose that transaction, it fails to reopen. A separate, shape-only non-vacuity gate requires an attempt genuinely larger than the reference cap, a NON-EMPTY WAL underneath it (a byte-unchanged assertion over an absent file is satisfied by definition), and at least one transaction actually committed. |
| `db-teardown` (`internal/sim/db_teardown.go`) | concurrent | **The composed teardown's variants: a cancelled context and concurrent closers** (rmp #2475). Every durable scenario tears its store down the same way — `db.CloseCtx(ctx)`, once, from one goroutine, with a live context — so two properties `store/db.go` documents were never exercised here. First, the context bounds ONLY the optional final checkpoint: steps 2 (stop the checkpoint goroutine) and 3 (close the WAL) run to completion regardless, because abandoning them on cancellation reintroduces the goroutine and file-handle leak the type exists to prevent. Second, close is idempotent under concurrent callers through a `sync.Once` whose error is published under a mutex, so N callers observe one result and none is handed a spurious `wal.ErrWriterClosed` from a double close. Both were read out of `closeOnce0` before the arms were built, not taken from a description of them. Each arm acknowledges 8 transactions, publishes a snapshot through the live checkpoint loop (which is also the liveness probe: without it "the goroutine was joined" would be satisfied by there being no goroutine), acknowledges 4 more that live ONLY in the WAL suffix, tears down, and reopens through real recovery. What the cancellation does to step 1 is REPORTED, never asserted: `TriggerCtx` selects over a buffered submit and `ctx.Done()`, both ready when the context is already cancelled, so the fold is Go's uniform choice — MEASURED at 17 folds in 25 runs, with the caller receiving the cancellation in the other 8. Pinning either outcome would be flaky, so the verdict asserts what the contract actually promises: the cancellation did not become the close's error, was not counted in `store.DB.Close.finalCheckpointErrors`, the post-close checkpoint request returned `checkpoint.ErrCheckpointerStopped`, a post-close commit was refused with `wal.ErrWriterClosed`, and all 12 acknowledged commits came back. The concurrent arm races 16 closers (mixing the `io.Closer` `Close` and `CloseCtx`) out of one barrier and then makes two SERIAL calls, which is how "Close after CloseCtx" and "CloseCtx after Close" are pinned by the same clause: measured 18 calls, 1 teardown-body run, 1 distinct error value, 0 spurious `ErrWriterClosed`, a 552-byte WAL image and 8 WAL ops replayed. Agreement is asserted as VALUE IDENTITY rather than class (rmp #2472): the existing `store/db_test.go` race is over a CLEAN teardown, where every caller observes nil and the claim survives an implementation that re-derived the result per caller, so this arm CONSTRUCTS a failing teardown — a one-shot fsync fault on the WAL close's own fsync — and the quiesce callback allocates its wrapper freshly per invocation, so a second body run would publish a different value and a second `wal.Close` would additionally return `ErrWriterClosed`. Measured: `sim: db-teardown WAL close: sim: injected disk fault` observed by all 18 callers, `store.DB.Close.errors` incremented exactly once, and all 12 acknowledged commits still recovered from a close that FAILED — acked-implies-recoverable is a durability invariant here, not a teardown quirk. The boundary arm parks a commit INSIDE its WAL fsync with a `SimDisk` sync gate and then closes: the closers must block on the `WithQuiesce` drain (none had returned after the 50 ms window), the parked commit must be acknowledged rather than failed, and it must be recoverable — 13 acked, 688-byte WAL, 10 ops replayed. One vacuity was found by the gate and fixed rather than argued away: with `WithFinalCheckpoint` enabled the teardown folds the whole WAL suffix into a fresh snapshot and truncates the WAL to nothing, so the reopen replayed 0 WAL ops and every acknowledged key came back from the snapshot however the WAL was closed. The arms whose subject is the WAL close therefore leave the final checkpoint off, and the shape-only non-vacuity gate — separate, so an uninformative run never reads as a faulty one — requires commits acknowledged after the last checkpoint plus either replayed WAL ops or a fold that accounts for them, a live loop before the close, the call count the arm claims, at least `dbTeardownMinClosers` (8) racing callers on the concurrent arm, and a NON-NIL published error on the arm that pins identity (nil would satisfy the identity clause by the zero value). The sensitivity seam drives the real defect instead of a doctored value: the DB is built WITHOUT `store.WithCheckpointer`, the loop survives the close, and the post-close checkpoint request returns `wal: writer is closed` — precisely the swallowed-error hazard the package documentation describes — which is also the standing proof that `goleak` sees this goroutine, since the probe runs at the instant the teardown claims the join and requires `goleak.Find` to report the checkpoint loop. Every other clause is falsified by injection into the pure adjudicator: a body that ran twice, divergent error values, an `ErrWriterClosed` reaching a caller, an unjoined loop, a WAL left open, a cancellation returned as the close's error, a lost acknowledged commit, and a refused transaction resurrected by recovery. |
| `checkpoint-cadence` (`internal/sim/checkpoint_cadence.go`) | deterministic | **The background checkpointer's cadence, on the fake clock** (rmp #2476). Every checkpoint this package had ever taken went through a SYNCHRONOUS entry point: `SimStore.Checkpoint` calls `checkpoint.Checkpointer.RunCheckpoint` inline with no loop at all, and the two scenarios that do start a loop (`checkpoint-teardown`, `db-teardown`) configure it with no age and no interval so it fires only on an explicit `Trigger`. The ticker, the `MaxAge` gate and the `checkpoint.WithClock` seam that exists so the DST can drive them were therefore never entered, and the documented behaviour that a periodic fire records its error in `Stats.LastError` and retries had never been observed once. The loop was READ before the arms were built, and the geometry (`MaxAge` 40 ms, `Interval` 10 ms, both virtual) makes one window exactly four advances, so the fire ordinals are exact rather than approximate. Fires are attributed to the CADENCE three ways, none of them an argument from absence: ARITHMETIC (the checkpoints the run did not ask for — the arm owns every `Trigger` call it makes); CORRESPONDENCE (the clock advances one interval at a time and the fires are recorded as TICK ORDINALS, then compared against an independent model of the rule that never consults the checkpointer — measured `[1 5 9 13 17 24]`, predicted `[1 5 9 13 17 24]`); and CONTROL (before any advance the fake clock is held STILL for 100 ms of real time, and zero checkpoints and zero clock observations follow, so no wall time leaks into the cadence). The seam itself is proved rather than presumed: the injected clock is a counting decorator over a `clock.Fake` and the loop must have registered EXACTLY ONE ticker on it. The transient failure is a one-shot `SimDisk` fsync fault armed on the first `Sync` of one periodic fire, which is that publish's `csr.bin` — so it fails inside phase 2, before `wal.Writer.Sync` and long before the phase-3 prefix truncate. MEASURED: the faulted fire at tick 13 recorded `LastError="sim: injected disk fault"` and moved nothing — checkpoints 3 -> 3, reclaimed bytes 810 -> 810, the durable WAL image 516 -> 516 bytes — and the next fire at tick 17 succeeded, cleared `LastError` to empty and reclaimed up to 1779 bytes. Durability is the clause that outranks the rest and it is unconditional: commits are acknowledged BEFORE the failed fire and again BETWEEN the failure and its retry (3 of them, while the checkpointer was in its failed state), and the run ends by CRASHING the store and reopening through real recovery — 17 acknowledged, 0 missing, 8 WAL ops replayed. Four behaviours the `store/checkpoint` documentation does not state are pinned by measurement. (1) The retry is a FULL `MaxAge` window away, not the next `Interval` tick: the loop assigns `lastFire` after a periodic fire whether it succeeded or failed, so "retries on the next cadence" measured 4 ticks, not 1. (2) A success CLEARS a stale `LastError` — `setErr(seq, nil)` writes the empty string. (3) An explicit `Trigger` RESETS the age timer, settled by CONSTRUCTION rather than by inspection: the trigger is issued at tick 20, one tick short of the window's close, and the run requires no fire at tick 21 (where the un-reset age would have fired — the rival model predicts `[1 5 9 13 17 21]`) and a fire 4 ticks later at 24. (4) With `MaxAge` unset the loop does not read the clock AT ALL on a tick, because the gate is `MaxAge > 0 && Since(lastFire) >= MaxAge` and `&&` short-circuits; this was found the hard way, when the control arm's first version used a clock observation as its tick barrier and waited out its whole timeout. That control is also the sensitivity seam, and it drives real behaviour rather than a doctored value: an interval with no age ticks forever and folds nothing — 21 of 21 advances delivered (proved with a probe ticker of the harness's own on the same fake clock, since two waiters of one period are due at the same instants), 0 gate reads, 0 cadence checkpoints — and it ends with an explicit trigger that MUST succeed, because a loop that had exited folds nothing just as convincingly as one that declined every tick. The shape-only non-vacuity gate is kept separate (rmp #2470) and requires the ticker on the seam, fake time actually advanced, every advance delivering a tick, at least two fires (one event is not a correspondence), a plan whose two age-timer hypotheses genuinely disagree, an armed fault that reached a fire, commits acknowledged during the failed state, and commits acknowledged after the last checkpoint that recovery had to replay. Every verdict clause is falsified by injection into the pure adjudicator, and the harness's OWN model is pinned by its own test, so a model that predicted nothing could not make the correspondence clause vacuous while looking identical from outside. |
| `bolt-auth` (`internal/sim/bolt_auth_surface.go`) | deterministic | **The Bolt authentication surface, with the WAL as the witness** (rmp #2481). Every SimServer in this package was built with `server.NoAuthHandler`, a handler that admits every client by construction, so the credential surface was driven by no scenario at all: no wrong password was ever presented, `LOGOFF` was never sent, re-authentication never happened, and the no-statement-without-`LOGON` invariant (the CWE-306 gates on `RUN`, `BEGIN`, `COMMIT`, `ROLLBACK` and `ROUTE`) had no wire-level oracle. An assertion made against `NoAuthHandler` is not merely weak but VACUOUS — it asserts the absence of a check nobody installed — so this scenario builds its server through `NewSimServerAuth` with a `BasicAuthHandler` over `ConstantTimeValidate` and drives fourteen lock-step arms on their own connections: a wrong password on `LOGON` (Bolt 5.6, deferred auth) and on `HELLO` (Bolt 4.4, inline auth — a SEPARATE server code path, reached by withholding the newer versions with `HandshakeOffering`), an unknown scheme, a `RUN` before `LOGON`, a write after `LOGOFF`, a `COMMIT` and a `ROLLBACK` after a `LOGOFF` that left a write staged in an open transaction, a wrong-password RE-authentication (a different branch from a first `LOGON`, which terminates the connection), a `ROUTE` after `LOGOFF` (the fifth auth-gated verb, and the only one whose violation neither the WAL counter nor the census could ever see, because a leaked ROUTE writes nothing), a `LOGOFF` mid-stream from `TX_STREAMING` (the guard that lets `handlePull`/`handleDiscard` run with no authentication gate of their own — the flag can only be cleared by LOGOFF, and LOGOFF is illegal while streaming), a `RESET` on a de-authorised session with an OPEN transaction (the reclamation limb every existing RESET test misses, since they all run with no transaction open), a SECOND message after a refusal (the soft-IGNORE in FAILED is scoped to AUTHENTICATED sessions, so a de-authorised client must get a hard FAILURE), and two ADMIT arms. Because the authentication gate and the state-machine gate share one failure code, every gate arm additionally pins the ORIGIN STATE `failTransition` names — a refusal by the auth gate names a LEGAL state, one by the state machine names FAILED — so a regression in LOGOFF's target state cannot leave the CWE-306 gate untested behind a matching code. The load-bearing oracle is not the FAILURE message — that proves only that the server SAID no. The server is backed by a real WAL (`SimStore` over `SimDisk`) and every arm is bracketed between two readings of `wal.Writer.Stats`: a refused arm must leave the frame AND byte counters exactly where it found them, and the sentinel node its statement would have created must be absent both from the live engine and from a graph reopened through real recovery after a crash — a frame appended but not yet visible would hide from the live census alone. The exact failure CODE is pinned per arm, not merely "some failure": measured `Neo.ClientError.Security.Unauthorized` for a bad credential, `...Security.AuthProviderFailed` for an unknown scheme, and `Neo.ClientError.Request.Invalid` for every de-authorised transition, because mapping one onto another changes what a driver is told. The two ADMIT arms are what make the instrument live: an honest authenticated write and a write after re-authenticating following `LOGOFF` must ADVANCE the counters (measured frames +4 each), and the second is also the recovery half of the LOGOFF contract — a server that refused every post-`LOGOFF` write, including the legitimate one, would satisfy every refusal clause and fail here. Non-vacuity is a separate shape-only gate: the full arm roster must have run, refusals and admissions must both have occurred, the frame counter must have been observed MOVING, and all three failure codes must have been seen. The control is a real alternative configuration rather than a doctored value — the identical wrong-password exchange against a `NoAuthHandler` server must be ADMITTED, which is what pins the refusals on the `AuthHandler` and not on the state machine, the framing, or a typo in the harness. |
| `bolt-cert-rotation` (`internal/sim/bolt_cert_rotation.go`) | deterministic | **TLS certificate rotation under fault, adjudicated by a real handshake** (rmp #2481). `server.CertReloader` is the operator seam for rotating the Bolt server's TLS material without a restart, and its whole promise is a negative one: a rotation that goes wrong must leave the LIVE certificate untouched. A promise of that shape is only tested by breaking the rotation. Seven steps run in sequence — an initial load, a clean rotation, then a TORN key (only a seed-chosen PREFIX of the PEM landed), a GARBLED key (all bytes landed, then a sector under them went bad, injected through `SimDisk.CorruptRange`), an ABSENT key (unlinked and never rewritten), a MISMATCHED pair (the new cert beside the old key — the dangerous case, because BOTH files parse), and finally the completed rotation. Each verdict has two halves, and it matters which answers which question: WHICH pair is live is settled by the served leaf's Common Name, while THAT the served pair is usable is settled by completing a genuine TLS 1.3 handshake through `crypto/tls` over a `net.Pipe` against whatever `GetCertificate` returns, with the client trusting exactly that certificate. `crypto/tls` is the independent reference for the second half — a successful handshake proves the certificate parses, the name matches, and the private key genuinely corresponds to the certificate's public key, which is what a mismatched rotation destroys and a byte comparison cannot see. It cannot by itself detect that the WRONG pair went live, which is the CN half's job; crediting the handshake with that check would overstate it. Measured: `stat key: no such file` for the absent arm and `tls: private key does not match public key` for the mismatched one, with `rotation-B` still in service and still handshaking through all four faults, and `rotation-C` taking over the moment its key lands. Two documented-but-untested contract halves are pinned with it: an unloaded `CertReloader` REFUSES to serve rather than handing the TLS stack a nil pair, and `NewCertReloader` over a torn key FAILS CLOSED, because the initial load is mandatory. The fixtures are Ed25519 with FIXED validity bounds precisely so the PEM bytes are a pure function of the seed — an ECDSA pair, whose signature draws randomness, would not be reproducible — and the projected files' mtimes are stamped EXPLICITLY, because `CertReloader` short-circuits a reload when neither mtime advanced, and on a coarse-granularity filesystem an honest rotation would otherwise be skipped and the scenario would be measuring the clock. `SimDisk` is the image authority for every version of every file and every fault applied to it; the bytes are then projected onto a real temporary directory because `CertReloader` reads through `os` and `tls.LoadX509KeyPair` and exposes no filesystem seam (the precedent is `wal_writer_surface.go`). The torn prefix is produced by an ACTUAL host crash: the prefix is written and `Sync`'d, the remainder is written and left un-synced, and `SimDisk.CrashHost` reverts the file to its durable image — which since rmp #2535 advances only on a `Sync` that returned nil, so the crash discards exactly the un-synced remainder (measured: 85 of 119 bytes survive, and the arm fails loudly if a crash discards nothing). An earlier draft of this row asserted the reverse — that a crash never discards un-synced data — which was the pre-#2535 model; it was corrected against `internal/sim/disk.go` rather than trusting the note. Because the torn and garbled arms produce the IDENTICAL parse error, the non-vacuity gate compares the key sizes each step left on disk — a torn key must be SHORTER than the key it truncates (measured 85 of 119 bytes), a garbled one exactly as long (119 of 119), an absent one zero — so the roster cannot name two faults where only one was applied. It also requires at least one reload to have succeeded, at least one to have failed, and the certificate in service to have genuinely CHANGED, since a reloader that ignored every rotation would pass every retention clause. The handshake oracle's own falsifiability is proved by pointing it at a certificate issued for a different name, which `crypto/tls` must reject. |
| `bolt-tx-registry` (`internal/sim/bolt_tx_registry.go`, `bolt_tx_terminate.go`) | deterministic | **The Bolt transaction registry, its idle reaper and operator termination, on a fake clock** (rmp #2482). `Server.Transactions`, `Server.TerminateTransaction`, `Options.MaxTxIdleTime` and `Options.MaxOpenTxPerPrincipal` were added after the round-3 audit's whole-server stall and the DST never touched any of them. The scenario runs two arms, each on its OWN `SimServer` so their timer counts stay attributable, both driven through `NewSimServerTxRegistry`: the server's clock is an injected counting `clock.Fake`, while the in-memory listener keeps REAL time, because sharing one fake would make every parked connection register a timer of its own and destroy the only barrier the arms have. **Arm 1, abandoned-registry:** five connections BEGIN one advance apart under their own principals and go silent, and the listing is adjudicated FIELD BY FIELD against the harness's own plan. This is what the DST adds over `bolt/server/tx_introspection_test.go`, which runs on the wall clock and can assert no more than `Elapsed > 0`: here `StartedAt` is EXACTLY the fake instant the harness opened at and `Elapsed` is exact to the nanosecond, because the harness owns every advance and the arming barrier guarantees none is in flight while a transaction registers. The barrier itself had to be built rather than assumed — `syncTxTimer` arms the reaper AFTER the response loop has flushed BEGIN's SUCCESS (`serve.go:1350` against the flush at `:1501-1505`), so neither "the client got SUCCESS" nor "`Transactions()` lists N" proves the reaper is armed, and an arm that advanced on either would clamp `Until` to zero and land the reap an advance late. A `NewTimer`-counting clock decorator closes the gap, and it is attributable only because `syncTxTimer`'s is the ONLY `clock.Clock` timer registration in `bolt/server` (verified by exhaustive grep; `tls_reload.go` uses `time.NewTicker` from the standard library, so it cannot reach the probe). An independent `idleReapModel` predicts the reap ordinal for each transaction from the plan's arithmetic alone and consults nothing from `bolt/server`; measured `[6 7 8 9 10]` predicted and observed, with five quiet ordinals in front at which the reaper must DECLINE to reap. Each reap is attributed by the typed FAILURE only `Session.reapTimedOutTx` arms, so an absent entry — an ambiguous post-state a rollback or a dropped connection would produce identically — is never taken as proof on its own. Four autocommit writes straddle the reap as WAL witnesses, and the per-advance bracket measures 0 frames and 0 bytes charged to the reap; the bracket is a live instrument, MEASURED by moving one window inside it and watching it read 4 frames / 195 bytes. **Arm 2, operator-terminate:** its own server with both bounds at ten minutes of fake time and ZERO advances, so `clock.Fake` can deliver to no waiter at all and every departure from the registry is provably the operator call's doing — asserted as a non-vacuity clause, not argued. It ends a live id (which leaves, while the other two do NOT), refuses two STALE ids the harness watched go live and then finish (one terminated, one committed — both strictly stronger than the never-seen id the pre-existing test covers), and pins **successor immunity**: the victim's connection RESETs and opens a second transaction, whose id suffix the harness predicts from the BEGIN ordinal, and the predecessor's id is still refused while the successor stays listed across a settle window. The terminated transaction's staged node is absent from the live engine AND from a graph reopened through real recovery after a crash, with the WAL bracket across the termination at 0 frames and 0 bytes. This arm compares the listing as a SET rather than a list, and that is a finding: with zero advances every entry shares one `StartedAt`, and `txRegistry.list`'s insertion sort swaps only on a strict `Before`, so equal keys are left in Go map-iteration order — asserting an ORDER there would be asserting the map. The client's reply is recorded verbatim and pinned against constants whose text is currently FALSE on both halves for an operator termination (rmp #2560), so the eventual correction fails the arm deliberately. The live control aims the same termination at the BYSTANDER instead, changing nothing else, and four independent clauses fire. |
| `bolt-tx-quota` (`internal/sim/bolt_tx_quota.go`) | deterministic | **The per-principal open-transaction cap: who it refuses, at what number, and the three ways a slot comes back** (rmp #2482). `Options.MaxOpenTxPerPrincipal` is installed at TWO, and that is asserted as a non-vacuity clause: at a cap of one, "the server refuses BEGIN" and "the cap fired" produce the same wire trace, so a refusal would prove nothing. One principal fills its cap with one READ and one WRITE transaction — the cap counts both, which nothing asserted before — and the next BEGIN must be refused with `Neo.ClientError.General.LimitExceeded` and with a message the harness RECOMPUTES from the principal and the limit it configured. That recomputation is the load-bearing half: `handleBegin` returns the quota error VERBATIM rather than through `Session.sanitiseErr` (`session.go:1604`, verified against the neighbouring failure paths, which all DO sanitise), so the text names WHO was refused and at WHAT number, which a code-only assertion cannot — it is equally satisfied by refusing the wrong principal, or the right one at the wrong count. Measured and received identically: `principal "quota-alpha" already holds the maximum of 2 concurrently open transactions`. The refusal is also shown to COST nothing: it leaves the principal holding exactly its limit, leaves the whole-server registry exactly as it found it, registers ZERO timers on the injected clock (the quota branch returns before `txActive` is set, so `syncTxTimer` has nothing to arm), and answers inside a generous real-time ceiling rather than parking the client. A SECOND principal is served throughout, which is what makes the cap per-principal rather than server-wide. The session state after a refusal is PINNED as observed (rmp #2561): the quota branch returns before `Transition` and never calls `enterFailed`, unlike the `newTx` failure path directly above it, so the connection is still READY and its next statement is served — the arm drives one and requires it to succeed, so closing #2561 fails the arm deliberately. Then the three reclamation routes nothing tested (`bolt/server/abandoned_tx_test.go` covers only the fourth, a client ROLLBACK), each ending in a BEGIN the cap must now allow: the IDLE REAPER frees exactly one slot — the arm advances once, TOUCHES two of the three open transactions so their deadlines move, then advances again, and an independent `idleReapModel` predicts `[2 never never]`, which is what was observed; `TerminateTransaction` frees one by operator action; and a DE-AUTHORISED session's refused COMMIT frees one. That last is the clause rmp #2482 carries over from the #2481 security review, and it exists because the auth scenario's WAL-frame and ghost-node oracles CANNOT see it: a refusal that left the transaction open with its slot held and its registry entry live would write nothing either, so only the registry and the quota can tell the two apart. The arm stages a write, sends LOGOFF, has the COMMIT refused with `Neo.ClientError.Request.Invalid` naming the ORIGIN state `TX_READY` (the authentication gate and the state machine share a code, so the origin state is what attributes the refusal), and then requires the entry GONE, the slot returned, and the staged node absent both live and after real recovery. The live control installs a NEGATIVE cap, which the option documents as disabling enforcement, and the identical over-cap BEGIN must be ADMITTED — which pins every refusal on `MaxOpenTxPerPrincipal` and not on the state machine, the framing, or the harness. |
| `bolt-shutdown-drain` (`internal/sim/bolt_shutdown_drain.go`) | deterministic | **`Server.Shutdown`'s connection drain against the `Options.Closer` the server owns** (rmp #2483). `Options.Closer` was passed by nothing in the module outside `bolt/server`'s own tests, and `SimServer.Close` only cancels the serve context, so the documented "drain the connections, THEN close the durability stack" ordering that `store.DB` says a Bolt server relies on was wired nowhere in the simulator. Four deterministic arms now drive it with the store wired as the server's Closer. The ordering is asserted on TWO independent observables rather than a timing guess: a `net.Conn` decorator counts accepted-and-not-yet-closed server connections (one-sided by construction — the handler's `conn.Close` runs strictly before its `wg.Done`, so it cannot produce a false positive), and a CONSTRUCTED rendezvous parks a commit inside its WAL fsync with `SimDisk.ArmSyncGateAt` and requires the closer to have run ZERO times across a window in which the listener is already closed. Three measurements came out of it, each refuting an obvious model. **Neither** of `Shutdown`'s failure branches closes the store: at the instant an expiring `Shutdown` returned, the closer had run zero times (12/12 with a deadline, 12/12 with a cancel), and the store is closed afterwards by `Serve`'s own deferred exit path, attributed by walking the runtime stack. **On a CLEAN drain, who closes is a genuine race** — `Shutdown` cancels the accept context before draining, so `Serve`'s exit and `Shutdown`'s drain-success branch wait on the same `WaitGroup`; measured 22 `Serve` / 3 `Shutdown` over 25 successful drains, so a `Shutdown` that returns nil did not necessarily close the store. And **which error a DEADLINE-bounded `Shutdown` reports is also a race**: it clamps its drain timeout to `time.Until(deadline)` and then selects over both that clamped `time.After` and `ctx.Done()`, which come due together, and Go's select is uniform when both are ready. That one is instructive about instruments as much as about the server — the distribution is heavily skewed (8/8 and 12/12 drain-timeout in two sittings), the arm originally PINNED the drain-timeout branch on that evidence, and the pin then made 5 of 6 `-race` runs red once the other branch surfaced. Both branches are now legal and the distribution is REPORTED, with the deadline arm excluded from the determinism clause because that field is not a function of its seed. "No `wal.ErrWriterClosed` reaches a client" also could not be written as posed: it never can, because `sanitiseErr` replaces its text and `FailureCode` maps it to the catch-all, so the oracle is split — on the wire, no statement on an undrained connection may receive a DatabaseError-class code; at the store, `errors.Is(err, wal.ErrWriterClosed)` is checked on a commit attempted after the teardown, which is simultaneously the proof the WAL closed and the proof the detector is not blind. |
| `bolt-shutdown-fleet` (`internal/sim/bolt_shutdown_drain.go`) | concurrent | **The same drain against a fleet of committers in flight** (rmp #2483), as a production driver fleet would meet a graceful shutdown. Not bit-reproducible, and it says so: it is adjudicated on the invariants that hold under any interleaving — every commit the CLIENTS acknowledged survives the teardown and a reopen through real recovery, no client on an undrained connection is told its write failed with a storage-class code, and the owned store is closed exactly once and after the last connection. This arm is where the task's sharpest lesson came from, and it is a lesson about the harness rather than the engine: it reported an `ACID_DURABILITY` violation, in 4 of 25 runs, that was not one. **A RUN SUCCESS is not the durability acknowledgement for an auto-commit write.** `handleRun` replies SUCCESS whenever the engine returns no error and never consults `Result.Err()`; its metadata is `fields`, `qid`, `db` — statement accepted, here are your columns. The BOOKMARK, which is what a driver uses to establish that a write landed, rides on the TERMINAL `PULL`/`DISCARD` SUCCESS and on `COMMIT`, never on `RUN`. So when a graceful shutdown cancels an in-flight statement, `commitUnderBarrier` early-returns on the materialise error, no WAL frame is appended, and the client that had already received its RUN SUCCESS learns nothing — the name is then in neither the live engine nor the raw WAL image, and the harness, having counted it as acknowledged, demanded it from recovery. The same file had already made the same class of mistake once, counting a `*proto.Ignored` — the reply a FAILED session gives every request-phase message — as an acknowledgement, which manufactured the same violation in 8 of 30 runs. Both are fixed by one rule: an acknowledgement is an explicit terminal SUCCESS and nothing else. The RUN reply and any IGNORED are kept as WITNESSES, because they are what distinguishes "never dispatched" from "dispatched, outcome unknown to the client", and the in-flight commit is adjudicated on the invariant that does hold for it — the statement the drain found executing must have RUN and must be DURABLE, which is read from the reopen and not from any reply. |
| `bolt-streaming` (`internal/sim/bolt_stream_semantics.go`) | deterministic | **Bolt streaming semantics, adjudicated against an independent reference drain** (rmp #2484). Every result stream this harness had ever opened was drained with a single `PULL {n:-1, qid:-1}`, so no `PULL` ever carried a finite `n`, `has_more` was always false and never once observed true, `DISCARD` did not appear anywhere in the package, and no arm addressed a stream by an explicit qid. **The task's premise was half wrong, and the refutation is recorded rather than worked around.** It asked for QID multiplexing and QID routing; this server has neither and cannot. `handlePull` refuses any `qid >= 0` outright (`bolt/server/session.go:1240-1243`) and `handleDiscard` carries the identical guard (`:1421-1424`); RUN's SUCCESS always reports `qid = -1` (`:1223`), so no positive qid is ever minted for a client to send back; and a second RUN while a stream is open is refused by the state machine, because `handleRun` requires READY or TX_READY (`:1075`) and a live stream leaves the session in STREAMING or TX_STREAMING. There is exactly ONE open stream per session at any instant. What DOES exist is that cursors ACCUMULATE across SEQUENTIAL RUNs inside one explicit transaction — each RUN appends to `tx.results` (`bolt/server/tx.go:135`, `:140`), cleared only by `Tx.closeCursors` on COMMIT/ROLLBACK — bounded by `Options.MaxInFlightPerConnection`, which nothing in the harness had ever passed. So the scenario pins the contract that is real. **The load-bearing oracle is an independent reference drain**: the same query is drained twice on two connections, once with a single `PULL -1` and once with a seed-drawn sequence of `PULL n` pages, and the concatenation must equal the reference ELEMENT BY ELEMENT and IN ORDER through `compareWireRow`, which compares the decoded value AND its concrete Go type because the dynamic type IS the wire encoding. That distinction is load-bearing and the falsifiability table proves it: an Integer replaced by the identically-rendered Float passes a `String()` comparison and fires this one. The partial-DISCARD arm sharpens it into an exact statement about WHICH rows were skipped — a seed-drawn prefix is paged, a seed-drawn window DISCARDed, the remainder pulled, and prefix++remainder must equal the reference with exactly that window cut out, so a DISCARD off by one row moves the suffix and fails where "the session still works afterwards" could not see it. Measured at the catalogue seed: 12 pages from the plan `[12 5 3 11 5 5 8 16 11 3 16 8]`, `has_more` 11 true / 1 false, and the bookmark present on exactly the terminal page and no other. **DISCARD abandons delivery, not the statement**, and the arm confirms the effect where it has one: an autocommit `CREATE` whose delivery is DISCARDed delivers ZERO records, its terminal SUCCESS still reports `nodes-created=1 labels-added=1 properties-set=1 contains-updates=1`, and the node is present both live and in a graph reopened through real WAL recovery after a crash. The refusal arms pin the exact code AND the exact message (`Neo.ClientError.Request.Invalid` / `no such query: qid N`), assert the refused message delivered no row, and — the gate-attribution discipline of rmp #2481 — pin the ORIGIN STATE `failTransition` names, because the authentication gate one line above the state gate returns the SAME code. The needle is the whole `in state X` phrase and not the bare state name, since `TX_STREAMING` CONTAINS `STREAMING` and a containment check on the name alone would let a TX_STREAMING refusal satisfy the STREAMING clause. Both refusals were also measured to POISON the session: the next request draws `*proto.Ignored`, and only RESET restores it. The in-flight arm runs two transactions on a WAL-backed store: one accumulates exactly the cap's cursors and COMMITs (measured frames +10, three nodes live and three recovered — which is also what shows the frame counter MOVING), and one accumulates the same and then breaches the cap, drawing `Neo.ClientError.General.LimitExceeded` with `cap=3, open=3` under an armed read deadline so a stall could not read as a pass, and leaving frames+0, bytes+0 and nothing behind live or after recovery. The server's own `open=` figure is cross-checked against the harness's own cycle count. The controls are real alternative configurations, not doctored values: the identical `PULL`/`DISCARD` message with `qid = -1` must be SERVED the whole record set, and the identical cap script with only `MaxInFlightPerConnection` raised must stop being refused. |
| `bolt-streaming-stall` (`internal/sim/bolt_stream_semantics.go`) | concurrent | **The same surface behind a slow consumer that stalls mid-stream and then disconnects** (rmp #2484), reusing the harness's existing `SlowConsumer` actor rather than a second implementation of stalling. Not bit-reproducible — how many records reach the bounded buffer before the consumer stops depends on the scheduler — so the seed drives only the stall duration and the oracle is what every interleaving shares: the consumer is handed a non-empty PROPER prefix, the connection queue is observed FULL (so the server's writer was provably blocked when the connection was torn down), the teardown leaks no goroutine, and a FRESH connection's paged drain still matches plain range arithmetic with `has_more` true on exactly its non-final pages. **One oracle here had to be demoted after reading the harness's own code.** `halfPipe.write` chunks every write to the space remaining (`internal/sim/simconn.go:96-124`), so the queue can NEVER exceed `simConnBufferSize`: "the server did not buffer past the bound" is an invariant of the pipe, not a property of the server, and a clause asserting it cannot fail against a real server. It is kept, labelled as a guard on the harness itself, and the server-side HEAP bound is left where it can actually be measured — the live-heap gate in `bolt/server/streaming_backpressure_test.go` — rather than restated here. The reading also had to be CONSTRUCTED rather than sampled: a single `ReadBuffered` call at the moment the consumer stalls was MEASURED at 0 bytes on 2 of 3 seeds, and a bound asserted against 0 is a bound asserted against nothing, so the arm now polls until the queue is full and reports the peak. Measured 65536 of 65536 on 9 of 9 runs under `-race`. |
| `bolt-begin-extras` (`internal/sim/bolt_begin_extras.go`) | deterministic | **The whole BEGIN extras surface a real driver sends — bookmarks, `tx_timeout`, `tx_metadata`, access mode, database selection — plus the ROUTE payload** (rmp #2485). Until this task the harness could only send BEGIN with an EMPTY extras map, or with the single key `mode` (rmp #2482), and only ever spelled `"r"` or `"w"`; `bookmarks`, `tx_timeout`, `tx_metadata` and `db` appeared in no BEGIN anywhere in `internal/sim`, and ROUTE had a single call site, rmp #2481's `route-after-logoff` arm, which sends the ZERO message on a DE-AUTHORISED session and requires it to be REFUSED, so `handleRoute` had never once produced a routing table under simulation. **The bookmark result is the headline and it is a REFUTATION**: this server does not honour an incoming bookmark. `server.ExtractBookmarks` has exactly two non-test call sites — `bolt/server/session.go:1099` (RUN) and `:1529` (BEGIN) — and both only write it to a Debug log, the RUN site saying so outright ("single-host server ignores them for causal consistency but they should not be silently dropped", `:1097-1098`). So "the causal read observed the write" is a TRUE assertion that proves nothing: one store, and a committed write is already visible to every later read. The scenario makes it honest by driving the SAME causal read five ways on five connections — the writer's REAL bookmark, a FABRICATED far-future token `FB:kffffffff` separately asserted to be above every counter the server issued, an unparseable token, a wrong-typed element the extractor filters out entirely, and no `bookmarks` key — and requiring all five to be ACCEPTED, to observe the IDENTICAL count, and not to wait. Accepting a token the server never minted is the evidence that the token is IGNORED rather than honoured: were bookmarks honoured, a far-future one could only block (caught by the real-time bound) or be refused (caught by the acceptance clause). This is pinned INTENDED behaviour, not a defect. Delivery is pinned too, and it is defect-shaped: `s.bookmark` is assigned in ONE place, `handleCommit` (`:1694`), so an autocommit write's terminal PULL SUCCESS carries the EMPTY string on a session that never committed explicitly — measured on a reply that in the same SUCCESS reports `contains-updates` — and the EARLIER transaction's bookmark on one that did. **`tx_timeout` is attributed by its CONTROL**: four arms on four fake clocks, where the subject asks for a 100 ms bound with the idle and default-total bounds lifted to 10 virtual minutes (the serve loop reaps at the EARLIER of the two, `effectiveTxDeadline`, `bolt/server/serve.go:1155-1167`), and the byte-identical script with the key REMOVED must survive the identical advance and COMMIT. A third arm reaches the same reap through the IDLE bound and must be answered a BYTE-IDENTICAL code and message, which asserts rather than restates that the idle reaper, the total reaper and `Server.TerminateTransaction` all funnel through one `pendingTermErr` (`bolt/server/session.go:1831`, `:1839-1842`) — rmp #2560 widened from two paths to three. The hostile arm sends `tx_timeout = 1<<62` ms, which `clientMillisToDuration` (`bolt/server/session.go:460-465`) rejects outright as `ms > maxClientTimeoutMillis` (9,223,372,036,854 ms, `session.go:452`), so the client bound is SILENTLY IGNORED and the server default stays in force — asserted in both directions by two advances of half the default, the first of which must not reap and the second of which must. **The `mode` coercion FAILS OPEN and is measured on two observables**: only the exact string `"r"` selects read-only (`bolt/server/session.go:1560-1566`), so `"R"`, `"bogus"` and an absent key each silently yield write authority, adjudicated on both `server.TransactionInfo.Mode` from the live registry and whether a `CREATE` inside the transaction is accepted — one observable alone cannot tell a mis-recorded mode from a mis-enforced one. `db` is echoed unvalidated (`"not-this-server"` and `"system"` alike), must agree between the RUN and terminal SUCCESS, must never be empty (the rmp #2172 driver-panic guard), and must be absent from the COMMIT SUCCESS, which carries the bookmark alone. `tx_metadata` is read in no file under `bolt/`, so the arm pins that it is accepted and echoed NOWHERE rather than asserting a round trip that does not happen. **ROUTE is adjudicated against an independent reference**: `handleRoute` reads neither the routing context, nor the bookmarks, nor the DB (`bolt/server/session.go:1728-1753`), so a populated ROUTE and the zero message must be answered identically and the table's own `db` must be empty; and the advertised addresses are compared against `SimServer.ListenerAddr()`, which reaches the listener that `serve.go:1000-1005` copied `s.localAddr` from by a DIFFERENT route than the reply does, so it is a comparison of two independently obtained values rather than a constant restated. The non-vacuity gate is a separate function answering a different question — was the run in a position to notice — and its shortfalls fail the scenario just as a contract violation does. 36 named contract clauses and 22 `nv-` ones; a serial sweep of seeds 1 to 100 was clean on all 100. |
| `bolt-version-matrix` (`internal/sim/bolt_version_matrix.go`) | deterministic | **The Bolt protocol version matrix — 4.4, 5.0, 5.1 and 5.6 negotiated and driven side by side** (rmp #2486). Every DST connection before this task negotiated 5.6: `WireClient.Handshake` leads with 5.6 and the server picks the highest version inside any offered range, so 4.4 had never been negotiated by anything in `internal/sim` and no two versions had ever been compared. Two undriven axes, and they are DIFFERENT axes — the entity and temporal encodings branch on the MAJOR version (Node 3 fields vs 4, Relationship 5 vs 8, UnboundRelationship 3 vs 4, `bolt/server/entity_struct.go:96-132`; zoned DateTime `0x49`/`0x69` carrying a true UTC epoch second vs `0x46`/`0x66` carrying the wall clock as if UTC, `bolt/server/session.go:2222-2243`), while authentication branches on the MINOR version at `proto.Version{5, 1}` (`bolt/server/state.go:294-305`). The task text calls the second axis "4.4 (no LOGON)"; the code says more — **5.0 is on the same side of the auth split as 4.4 and the other side of the encoding split**, which makes the matrix a CROSSED design: 4.4 vs 5.0 moves the encoding axis alone, 5.0 vs 5.1 the auth axis alone, so a difference is attributable to ONE axis. **The load-bearing shape is that semantics are INVARIANT while encodings DIFFER, and both halves are asserted**: a run that only required the decoded values to agree would pass against a server that ignored the negotiated version and sent Bolt 5 structures to a 4.4 client, whose hydrator asserts the field count — so `encoding-differs-across-majors` requires the same query's record to differ in struct census AND byte length across the majors (measured at the catalogue seed `[N/3 R/5 P/3 N/3 N/3 r/3]`/144 bytes against `[N/4 R/8 P/3 N/4 N/4 r/4]`/168 — the census is stable run to run and the lengths are not, but they always differ, since the Bolt 5 layout adds seven decimal element_id strings). **The oracle is an independent PackStream reader** (`decodeBoltWire`) written from the marker table and pinned against hand-built bytes, cross-checked against the module's own decoder over the identical bytes; element_ids are required to be `strconv.FormatInt` of the id the same structure reports, and every temporal field is computed by the harness with Go's `time` package. **The no-LOGON contract is measured against a CREDENTIALED server**, never `NoAuthHandler`, so the same bytes produce OPPOSITE outcomes across 5.1: a wrong password on HELLO is refused `Neo.ClientError.Security.Unauthorized` and kills the connection at 4.4/5.0 and draws SUCCESS with the connection intact at 5.1/5.6; a RUN straight after HELLO is served at 4.4/5.0 and refused naming `in state AUTHENTICATION` at 5.1/5.6; and a RESET on a pre-LOGON session returns it to NEGOTIATION, the task #1345 gate, so the next RUN is refused naming `in state NEGOTIATION` while the same RESET leaves an inline-auth session usable. Negotiation is adjudicated by a literal table over 15 raw 20-byte preambles written directly on `SimConn` — exact versions, range offers including one topping out above the server's ceiling, the legacy version offered FIRST and losing anyway, an unsupported decoy, and four ways to have nothing in common — with `negotiate-supported-list` as a TRIPWIRE so a change to `proto.SupportedVersions` fails loudly instead of leaving fourteen expectations silently stale. The offer SPELLING is seed-chosen (slot, minor range, decoy) and its invariance is the claim, which needed one new primitive, `WireClient.HandshakeOfferingSlots`; `Handshake` and `HandshakeOffering` were refactored onto it and their exact 20 bytes are pinned off a bare `SimListener`, because comparing negotiated versions could not catch a changed range. **One trap was found in this scenario's own instrument**: an early rendering included node ids and byte lengths on the belief they were seed-derived, and the determinism test refuted it (38/215 then 227/48 for the same seed — the id derives from a key minted by a process-global counter), so both are now rendered POSITIONALLY while the checkers keep reading the raw values as derived relations. 38 contract clauses, 10 `nv-` ones, 53 falsifiability subtests; two controlled reverts against the live server turned `encoding-struct-layout` and five auth clauses red and were restored by digest. |
| `bolt-decode-pressure` (`internal/sim/bolt_decode_pressure.go`) | deterministic | **The engine-wide inbound-decode memory pool, adjudicated against a closed-form model of its own arithmetic** (rmp #2487). The harness had exactly one arm near inbound-memory abuse — the `BoltAbuser`'s oversized frame, which drives the PER-MESSAGE framing cap on ONE connection. The pool that bounds every connection at once (`packstream.InboundBudget`, created once per server at `bolt/server/serve.go:654`) had never been touched, and neither had either nesting cap. **The load-bearing oracle is a closed-form model, not the server's word**: the harness re-derives what a RUN's decode holds from the shared pool out of packstream's published per-slot costs — `held = 1344 + len(query) + len(key) + 48n`, where 1344 is the RUN struct (32 + 3x48), the one-entry parameters map (512 + 112), the parameter list's container (32) and the empty extra map (512), and the two string terms are there because `ReadString` charges its payload 1:1 against the same pool (`decoder.go:712`), which is not obvious and was found by measurement. The model was calibrated by binary search on the smallest budget that admits a payload — `48n + 1353` for every n tried with an 8-byte query and a one-byte key, exactly as the closed form says — and it names the boundary EXACTLY: at a 4 MiB ceiling n=87353 (slack +0 B) is SERVED and n=87354 (slack -48 B) is REFUSED. **A one-element-wide boundary is the strongest available refutation of "the per-message cap did it"**: the two messages differ by 48 charged bytes out of ~4 MiB, both are ~200x under the 16 MiB framing cap and 32x under the 128 MiB per-message decoded-collection cap, and the CONTROL — a second server differing in the ceiling and in nothing else — SERVES the identical 165,796-element bytes the pressured one refused, writing its node into its own engine while the pressured engine holds none live and none after real WAL recovery. **The sharpest clause is that three abuse vectors draw three DIFFERENT answers**, each measured rather than inferred: the aggregate pool answers `Neo.TransientError.General.OutOfMemoryError` and leaves the session READY with no RESET; the wire nesting cap (`packstream maxValueDepth = 128`) answers `Neo.ClientError.Request.Invalid` / `malformed Bolt message` and also leaves it READY, because both are refused ABOVE the session state machine (`serve.go:1258` and `:1289`); and a SECOND, LOWER, INDEPENDENT cap this task discovered by measurement — `cypher maxParamBindDepth = 32` (`cypher/api.go:4257`) — answers `Neo.DatabaseError.General.UnknownError` and leaves the session FAILED, because it travels through the state machine into `BindParams`. A server that collapsed any two would be indistinguishable, from a client's side, from one with no pool and no depth cap. The classification segment is asserted on its own, read out of the OBSERVED code: neo4j-go-driver's `IsRetriableTransient` tests `classification == "TransientError"` (`bolt/server/errors.go:130-131`), so "typed RETRYABLE backpressure" is a checked property and not a restatement of the literal. **The nesting family is bracketed at every boundary and is tiny on the wire**: 32 accepted / 33 refused and 127 refused-by-the-engine / 128 refused-by-the-decoder, identically for LIST chains and MAP chains, at 55 to 4046 wire bytes — all under a 64 KiB anti-confound ceiling, because a payload refused for its SIZE proves nothing about a DEPTH cap. The chains are hand-built from the marker table and not through `packstream.Encoder`, which CANNOT express them: `writeValue` carries the same depth bound as `readValue`, so the encoder refuses to encode the abuse its own decoder must reject. The pre-authentication arm is the one that isolates the wire cap cleanly (no parameter is bound, so the engine's cap is not in the way): a 127-deep HELLO succeeds, a 128-deep one is refused, and the connection survives with the session still UNauthenticated — a following plain HELLO succeeds, which it could only do from NEGOTIATION. **No-leak is proved through the wire, not by reading the pool.** `InboundBudget` exposes `Enabled`/`TryReserve`/`Release` but no `Remaining`, and the server's pool is unexported; instead the run repeats the calibrated boundary message after every abuse arm, and its acceptance at slack +0 B means a leak of a single byte would have been caught. 22 deterministic contract clauses and 9 `nv-` ones. |
| `bolt-decode-swarm` (`internal/sim/bolt_decode_pressure.go`) | concurrent | **The aggregate cross-connection vector: K connections pushing large-collection parameters at one shared pool while an honest client works** (rmp #2487). It is registered separately because it is not bit-reproducible AND because the aggregate vector is only reachable concurrently: every charge is released before its reply is written — the reassembly reader releases on every return path from `ReadMessage` (`bolt/proto/chunking.go:160-165`) and the decoder's hold by the deferred `ReleaseInboundBudget` (`serve.go:1419-1423`) — so a single-threaded script can never observe two charges outstanding, whatever it sends. Each abusive message is sized at 55% of the pool by the same model, so ONE fits and TWO cannot. **Two oracles about the honest client had to be constructed rather than raced for, and the cost of not doing so was measured.** With both sides started together and each running a fixed count, the abusers finished in ~38 ms while the honest client was still pausing, and exactly ONE of 24 honest exchanges straddled a refusal — a coverage clause resting on a single sample. The window's ENDS are now pinned (the honest client waits for the first refusal; the abusers push until it finishes), which took it to 9 of 24; that is still a race, and the measurement says why: under `-race` a narrow honest exchange takes 633 us at the median (357 us to 902 us over 20 samples) while refusals arrive about once per 3.8 ms of honest in-flight time, so most land in a gap. So the WIDTH is controlled too: every sixth honest exchange holds its open stream for 50 ms between RUN and PULL — genuinely in flight, the server holding a cursor — which is ~13x the inter-refusal interval and ~79x the median narrow exchange, and the clause gates on those alone. Measured 4 of 4 wide exchanges straddling across 25 seeds under `-race`, with the NARROWEST wide window holding 9 to 11 refusals against the 2 to 3 a 20 ms hold gave; the 100-seed soak sweep, which runs under heavier concurrent load, saw a worst case of 6 and asserts that worst case rather than only the pass. The hold is deliberately NOT a wait for a refusal to be counted: that would make the clause true by construction of the HARNESS instead of by behaviour of the SERVER. An independent density clause requires the whole honest run to contain at least 8 refusals (measured 41 to 47). **The liveness bound is a wall clock, and that is the legitimate case rmp #2567 left standing**: a server that starved honest traffic under aggregate pressure would never serve it at all, so the wait is unbounded and only a clock can see it — the bound is set against a MEASUREMENT rather than a guess — 30 s against a measured 633 us median and 902 us worst honest exchange with the fleet pushing under `-race`, about 33,000x the worst observed service time — the message that fires says STARVED, and it is paired with the claim that actually matters, which involves no clock at all: every honest exchange must return the value the harness chose for it. The sizing also has to keep the honest client clear of the REASSEMBLY reader, whose budget breach is not the connection-preserving refusal the decode layer's is — it returns a READ error (`chunking.go:223-227`) and the serve loop tears the connection down (`serve.go:1237-1247`) — so the pool floor is computed (one abuser's charge is 4.4 MiB of an 8 MiB pool, leaving 3.6 MiB; three refused abusers transiently holding a 1 MiB reservation chunk each leaves ~0.6 MiB against ~100 B of honest reassembly) and `swarm-no-transport-loss` reports it by name if the sizing ever stops holding. Measured on one run at the catalogue seed under `-race` (the counts are scheduler-dependent and are a sample, not a constant): 13 served, 63 refused, 24/24 honest served correctly, 4/4 wide exchanges straddling with the narrowest holding 11 refusals, 59 refusals across the honest run, 4/4 abuser connections still serving afterwards, and the full-ceiling probe admitted at +16 B of slack. 7 contract clauses and 7 `nv-` ones. |
| `long-running` | deterministic | Millions of small bounded-churn ops; oracle parity plus heap/goroutine stability (soak). |

## Crash and recovery

When a scenario enables crashes, the simulator drives a real SimDisk-backed
persistence stack. A scheduled crash is a SIGKILL-equivalent: the live engine
is dropped *without* a graceful close, so any buffered-but-unsynced frame is
lost exactly as a real crash would lose it, while the durable byte image in the
SimDisk survives. The store is then reopened through the real recovery path
(WAL replay, and snapshot promotion where a checkpoint was published), and:

- the **durability check** verifies every acknowledged-committed operation
  survived and nothing uncommitted leaked in; and
- when the search battery is enabled, the **full search battery** runs on the
  recovered graph, so the algorithms are exercised against crash-survived state.

A recovery that detects genuine corruption fails stop (a typed error), which the
run surfaces rather than swallowing.

### Snapshot + WAL crash recovery (full-stack checkpointing)

Beyond the WAL-only path, the harness drives a **real `checkpoint.Checkpointer`**
over the SimDisk and recovers through the full snapshot + WAL path. A scenario
opts in via `CheckpointConfig{Enabled, Every, Dir}` (the `crash-storm` scenario
sets it); when enabled the durable store is opened in **full-stack mode**: the
WAL lives at `<dir>/wal` and the snapshot at `<dir>/snapshot` (default `dir` is
`db`), rather than the legacy WAL-only root-level `wal` key.

Every `Every` ticks the tick loop runs one synchronous checkpoint
(`SimStore.Checkpoint` → `checkpoint.Checkpointer.RunCheckpoint`) that publishes
a self-sufficient snapshot of the committed state to `<dir>/snapshot` and then
**truncates the WAL prefix it folded** via `wal.Writer.TruncatePrefix`. The
checkpoint runs the identical three-phase critical section the production
background checkpointer drives — capture under the store commit lock
(`txn.Store.RunUnderCommitLock`), lock-free snapshot publish, prefix-truncate
under the commit lock — so the snapshot is transaction-boundary consistent and
the WAL prefix is reclaimed only after the self-sufficient snapshot is durable
(`docs/acid-audit.md` F3.5). The mapper codec, constraint specs and
index-definition specs are all wired so a checkpoint that truncates the WAL
prefix which first declared a constraint or index cannot lose it.

When a crash then fires, the store is reopened with the same layout and recovery
chooses its core by what is durable on disk
(`internal/sim/simstore.go` `recoverSimGraph`):

- a published snapshot exists (`<dir>/snapshot/manifest.json`), **or one is
  stranded at `<dir>/snapshot.bak/manifest.json`** by a publish a crash
  interrupted → **`recovery.OpenFS`** reconstructs the graph from the
  self-sufficient snapshot (honouring its persisted directed/multigraph shape)
  and replays the WAL tail on top, first promoting a stranded backup back to the
  live name; or
- neither is present (a crash before the first checkpoint) →
  **`recovery.ReplayWAL`** replays the committed WAL prefix into a graph built
  with the simulator's configured shape.

The stranded-backup half of that gate is load-bearing and was added in rmp
\#2465. Recovery owns the repair for an interrupted publish, but it can only run
it if it is called at all: a gate admitting only the live manifest routes an
interrupted-publish directory to the WAL-only core, which replays a WAL whose
prefix the previous checkpoint already truncated and so silently recovers a graph
missing every checkpointed transaction. Production never faces this because
`recovery.Open` is called unconditionally on the directory and decides
internally; the gate above restores the same decision boundary to the
simulation.

In both cases the benign torn WAL tail is truncated to the last durable frame
boundary before the WAL is reopened for append, and genuine corruption
fail-stops with a typed error the run surfaces. The **durability check** runs
after *every* recovery and asserts every acknowledged-committed operation
survived — so a checkpoint that lost the folded prefix, or a truncation that
dropped a committed frame, fails the run.

The WAL filesystem seam this relies on is `wal.OpenFS`: a path-backed WAL
`Writer` whose `TruncatePrefix` temp-write, rename, parent-directory fsync and
reopen route through an injected filesystem (`internal/sim/diskfs.go`
`simWALFS` over the SimDisk), with the default OS-backed path
(`wal.Open` → `osWALFS`) byte-identical to before. The standalone snapshot
publish/promote ordering boundary (snapshot published *before* the WAL prefix is
truncated; a crash mid-publish drops the staging dirents and the full WAL
replays) is also proven at the component level in `disk_fullstack_test.go`,
`disk_checkpoint_test.go`, `checkpoint_crash_test.go`, and the seam itself in
`disk_wal_truncate_test.go`.

#### Proving recovery was snapshot-sourced (rmp #2468)

Enabling `CheckpointConfig` and crashing on a schedule does not, by itself, prove
anything about the snapshot codec. Which crash lands after which checkpoint — and
how many WAL bytes each one leaves behind — is a property of the seed, so a run
can pass with every recovery having replayed the WAL and the snapshot components
never having been read. Worse, a `CheckpointConfig` is **inert** unless the run
loop calls `Simulator.maybeCheckpoint`: only `Simulator.Run` does that
automatically, so a custom loop that omits the call configures checkpointing and
publishes nothing (the trap rmp #2457 hit, and #2464 hit again).

`Simulator.crossSnapshotBoundary` (`internal/sim/snapshot_codec.go`) removes both
accidents. It measures the durable WAL image, publishes one real checkpoint,
measures the WAL again, then crashes SIGKILL-style and reopens through real
recovery **with `SimStore.Config()`** — the layout the crashed store really used,
never the WAL-only default that would point recovery at an empty root-level key.
`checkSnapshotSourcedRecovery` then adjudicates the measurements rather than
trusting them, and reports a violation when the manifest was not published, when
the WAL was already empty *before* the checkpoint (nothing to reclaim, so the
truncation proves nothing), when the checkpoint refused to truncate, or when the
recovery replayed any WAL op at all. Only after all four hold is a value read back
afterwards evidence that the snapshot codec round-tripped it.

Two scenarios end on a forced crossing: `type-coverage` (the full typed value
matrix, through `properties.bin`) and `edge-properties` (per-instance maps over
parallel pairs, through `edgehandles.bin`). Both gate on in-loop checkpointing
having fired *before* crossing — the forced checkpoint would otherwise inflate the
count and silence `Simulator.checkCheckpointsFired`.

**Do not assert an exact WAL byte count.** Measured for this section: an identical
50-op WAL is 10 790 bytes at 4 ms into a process and 10 850 bytes from 608 ms
onwards, then flat — the per-frame timestamps are varint-encoded, so their width
(and the whole image's size) tracks the wall clock. The logical run is unaffected
and stays bit-reproducible: repeated identical runs in one process produce an
identical op history and identical crash ticks while the byte image grows. The
seed-stable facts are the ones the oracle uses — the WAL was non-empty before the
checkpoint, is empty after it, and the recovery replayed zero ops.

## The key and weight codec matrix (rmp #2473)

`internal/sim/codec_matrix.go` runs seven `(key codec, weight codec)` pairs
through the same durability stack every other storage scenario uses. What made
the matrix necessary, and what it found, are both worth stating plainly.

### Why one codec pair was all the simulator could reach

Every Cypher-driven scenario in this harness reaches the graph through
`cypher.Engine`, and both engine constructors take a
`*txn.Store[string, float64]` and nothing else. `OpenSimStore` therefore
hardcoded `txn.NewStringCodec` and `txn.NewFloat64WeightCodec`, and with them the
entire remaining codec surface — `NewIntCodec`, `NewInt32Codec`,
`NewInt64Codec`, `NewUint64Codec`, `NewUUIDCodec`, `NewBinaryMarshalerCodec`,
`NewInt64WeightCodec`, `NewBinaryMarshalerWeightCodec` — never appeared in a
single simulated crash.

One consequence was sharper than the rest. `snapshot.WriteMapper` dispatches on
the key type: for `N == string` it delegates to `WriteMapperString` and emits the
frozen version-1 layout, and only for another key type does it emit the version-2
codec-framed layout, which the read side then parses with `ReadMapperBytes` and
decodes through `snapshot.ApplyMapperToGraphWithCodec`. A string-only simulator
exercised the version-1 half of the mapper on every crash it ever ran, and the
version-2 half on none of them.

`OpenSimStore` is now the string/float64 specialisation of a codec-generic core
(`openSimTypedStore`), so the existing scenarios are unchanged by construction —
same code, same codecs — while an arm may open the identical stack under any
other pair.

### The oracles are codec-agnostic in form, and codec-BOUND in implementation

The task that added this matrix assumed the existing oracles were codec-agnostic
and that a second codec would be plumbing. Half of that held.

The durability oracles are codec-agnostic in **form** — acked ⊆ recovered ⊆
issued, no rejected commit resurrected, no phantom — and the matrix re-expresses
them over node ordinals, with each arm's `keyOf` mapping an ordinal onto its
concrete key type, so one adjudicator serves every arm.

The oracle **implementations** in `internal/sim` are not reusable at all.
`GraphOracle`, `InvariantChecker`, `CheckIndexConsistency`,
`recoveredPersonNames`, `RunConcurrent` and the Bolt server all read the graph by
running Cypher through `EngineAdapter`, and the engine is fixed at string keys.
They are codec-BOUND structurally, and parameterising the store does not change
that. A non-string arm therefore drives `txn.Store` directly and adjudicates
against the recovered `lpg.Graph`. The practical cost is that the codec arms are
single-writer: concurrency, Bolt and Cypher-surface coverage remain the string
arm's, and the matrix adds the codec dimension to durability and recovery only.

### Weights the snapshot could not persist — CLOSED (rmp #2473 measured, #2526 fixed)

The matrix measured a gap between the two durable paths, and the numbers stated
it exactly. For the `binarymarshaler/binarymarshaler` arm, in one upgrade run:

* at the WAL-only boundary — a graceful close and reopen with no snapshot on
  disk — **all 95** acknowledged edges came back with the weight they were
  written with;
* after one folding checkpoint over the same image, with the WAL measured down
  to 0 bytes and 0 WAL ops replayed, **all 191** acknowledged edges came back
  with the ZERO weight.

Same image, same codec pair, same edges: only the source of the recovery
changed. The cause was that `store/snapshot`'s CSR component did not consult the
transaction layer's `txn.WeightCodec` at all. It sized a weight with the fixed
table in `csrWeightSize` (`store/snapshot/writer.go`), which answers 0 for every
type outside the Go primitives — including any struct, and any NAMED integer
type such as `time.Duration`. `WriteCSR` then wrote `hasWeights=0`, the **same
on-disk encoding a deliberately weightless graph produces**
(`adjlist.Config{Weightless: true}`), and the checkpoint went on to truncate the
WAL prefix that held the real values. Nothing errored, and the recovered graph
was indistinguishable from one that never had weights.

**#2526 closed it.** The snapshot now persists weights of any type through the
store's own `txn.WeightCodec`, wired in with `checkpoint.WithWeightCodec` and
read back through `recovery.Options.WeightCodec`. A weight that still cannot be
encoded — an unsizable type on a store with no weight codec, holding at least
one non-zero value — makes the snapshot write FAIL with
`snapshot.ErrWeightNotPersistable` rather than publish a weightless image, so
the checkpoint aborts and the WAL prefix holding the surviving copy is never
truncated.

The matrix assertion is inverted accordingly. Every arm must now come back with
the weight it was written with, through every durable path, and a separate
non-vacuity gate requires each arm to have confirmed at least one weight after a
**snapshot-only** recovery. That second gate is the load-bearing one: the defect
lost weights only through the snapshot, so an arm exercised purely over WAL
replay would have shown a perfect round-trip while the durable image on disk
held nothing.

The other six arms assert the full weight round-trip on every path and hold.

## Disk exhaustion (ENOSPC)

`SimDisk` carries an optional byte budget (`SetCapacity`, surfaced as
`DiskConfig` on a scenario). When set, a WAL append or checkpoint write that
would grow the disk past the budget returns an `ENOSPC` `os.PathError` on the
*real* WAL append+sync path — either eagerly at the growing `Write` or, in
delayed-allocation mode, at the covering `Sync` (the harder commit boundary).
The budget check is a pure function of the byte total and draws nothing from the
seed, so it never perturbs the reproducible fault stream.

The `disk-full` scenario drives the honest write workload against a finite disk
with crash/recovery on top, asserting the engine's ACID contract under
exhaustion: a commit that cannot durably write fails atomically (the oracle
advances only on a committed write, so engine and oracle stay in lock-step), the
WAL writer fail-stops, and after recovery the durability check confirms no
acknowledged commit was lost (`ACID_DURABILITY`) and no uncommitted state leaked
in (`ACID_ATOMICITY`).

This scenario found a real ACID bug on first run: on a simple (non-multigraph)
graph, re-`CREATE`ing an already-existing edge is a storage no-op, but the
in-memory undo log recorded a `RemoveEdge` inverse, so rolling the transaction
back (here via an `ENOSPC` WAL sync, but any rollback triggers it) deleted the
pre-existing committed edge. The fix gates the edge bookkeeping on whether the
edge was actually added.

## Bulk-import publication parity (rmp #2466)

`store/bulkimport` is the one write path in the module that builds a store from
nothing: it assembles a labelled property graph in memory, writes it as the
store's snapshot, and hands the directory to recovery. Until sprint 347 no
scenario touched it. The `bulk-vs-online` scenario drives `store/bulk`, a
different package whose record is adjacency only — `(src, dst, weight)`, no
labels and no properties — so every label, every property, every relationship
type and every parallel-edge handle that `bulkimport` carries went through no
simulation at all.

The `bulkimport-parity` scenario (`internal/sim/bulkimport_parity.go`) occupies
that gap. It is deterministic and bit-reproducible: the fixture is drawn from the
seed, and the build and the publish are single-goroutine.

### What it adjudicates

A seed-derived multigraph — 400 distinct keys, 1 280 edge instances, repeated
node records, deliberately bare nodes, and 40 pairs carrying two typed twins
each — is built through `bulkimport.Builder`, published to a **real temporary
directory** (removed on return), and reopened through `recovery.Open`. The
recovered graph must equal a harness-side model exactly:

* **Nodes, two-sided.** Every modelled key must be present and live, *and* the
  graph's live order must equal the model's cardinality — so an extra node is
  caught as surely as a missing one.
* **Labels and properties**, compared by **kind and value**. The kind is part of
  the comparison on purpose: rendering values textually alone would let an
  integer `7` and the string `"7"` compare equal, so a round trip that lost the
  type would pass.
* **Edges, two-sided and per handle.** Each pair's multiset of
  (type, weight, properties) must match instance for instance, and the walk
  covers every out-edge of every node — not only the modelled pairs — so an edge
  to an unmodelled pair is caught too. Because `bulkimport` attaches type and
  properties to the edge *handle*, the parallel twins prove that carriage
  survived rather than collapsing to one edge per pair.

The directory is then reopened a **second** time and adjudicated again, so a
reopen that mutated the store, or a first reopen that happened to be lucky, is
caught. Both opens must report `SnapshotHit` with **zero** replayed WAL ops: the
published snapshot has to carry every byte itself.

### The lifecycle contract, as measured

The scenario probes the package's contract and records what it **observed**,
rather than restating the documentation. Each probe that stops holding turns into
a violation, so a contract that moves underneath the harness fails it instead of
being silently absorbed. Measured today:

| Probe | Observed |
|---|---|
| `Builder.Graph()` before `Finish` | `nil` |
| `Publish` with an unfinished builder | `ErrNotFinished` |
| `AddNode` / `AddEdge` after `Finish` | `ErrFinished` |
| A second `Finish` | `ErrFinished`, **and still returns the accumulated stats** |
| `Publish` / `ImportInto` into a non-empty directory | `ErrStoreNotEmpty` |
| `Publish` into an absent directory | created, not refused |
| `Publish(nil builder)` | a plain error matching **neither** sentinel |
| Unfinished builder **and** cancelled context | `ErrNotFinished` — the builder check runs first |
| Finished builder, cancelled context **and** non-empty directory | the context error — the context check precedes the directory check |
| `ImportInto` with a non-empty directory **and** unbuildable records | `ErrStoreNotEmpty` — the directory is inspected before any work |
| `PublishResult.SnapshotDir` | `<storeDir>/snapshot`, the one name recovery reads |
| `PublishResult.Stats` | `{Nodes, Edges, NodeRecords}`, asserted against the model; `NodeRecords > Nodes` confirms the repeated-key merge ran |

The two precedence rows and the nil-builder row are the ones worth knowing: a
caller that switches on the sentinels alone will mis-handle a nil builder, and
the two entry points disagree about **when** the directory is inspected —
`Publish` checks the builder and the context first, `ImportInto` checks the
directory before doing anything.

### What this scenario CANNOT reach — read before assuming coverage

**Bulk-import publication is not fault-covered.** Every other durability scenario
here injects faults through `SimDisk`, which reaches the persistence packages via
their filesystem seams (`wal.OpenFS`, `recovery.OpenFS`,
`snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS` and siblings).
`bulkimport.Publish` has **no such seam**: it calls `os.MkdirAll` and
`os.ReadDir` directly and writes through the **non-seamed**
`snapshot.WriteSnapshotFullCtx`, and `ImportInto` takes a `storeDir string` plus
an `Options` that carries no filesystem. There is therefore no way to put a
`SimDisk` underneath a publish without changing the production API. That change
is **filed for a user decision as rmp #2518** and was deliberately not made here.

Unreachable, and covered by nothing below:

* `ENOSPC` part-way through writing the snapshot components.
* A failing `fsync` on a component file, on the staging directory, or on the
  store's parent directory.
* A failing or crash-interrupted rename of `snapshot.tmp` to `snapshot` — the
  exact instant `Publish`'s atomicity claim rests on.
* A crash landing **inside** the publish window, with the crash-window
  non-determinism (`ArmRenameWritebackForPath`) that `checkpoint-crash-storm`
  uses to select which dirent survived.

What **is** reachable against a real directory is the publish's *outcome* state
rather than its interruption. The scenario's third arm publishes a complete
snapshot to a scratch directory and moves it to the assembly name
(`snapshot.tmp`) in a fresh one. That is byte-for-byte the directory shape a
crash between assembly and rename leaves, and recovery must find nothing
(`SnapshotHit` false, live order 0) and remove the debris — with the staged
bytes measured *before* the reopen, so "recovery removed it" is a measured delta
rather than an assumption that anything was there. It is a **reconstruction, not
an interruption**: it proves recovery's treatment of that state, not the writer's
behaviour while reaching it.

### Byte-reproducibility: where it begins and ends

A publish of the same records twice produces data components with the same names
and the same **sizes**, but **not the same bytes**. The cause was isolated by
measurement rather than inferred, by republishing at three property regimes:

| Regime | Identical republish is byte-identical? |
|---|---|
| Items carrying two or more properties (the fixture) | **no** |
| Items carrying exactly one property | yes |
| No properties at all (labels and types kept) | yes |

Publishing the *identical record slices* twice within one process already
diverges, which rules out the fixture's construction, a timestamp, or an address.
The whole of the divergence is Go map iteration order over
`bulkimport.Node.Properties` / `Edge.Properties` — both are maps, so no caller
can avoid it.

**This is not a correctness defect.** `bulkimport.Node` documents that properties
are set "in map-iteration order, which is unspecified. That is safe because each
key is written once, so no ordering can change the result", and that claim holds
exactly as written: the *logical* result is identical on every run, which the
parity pass re-proves each execution. What is not promised, and is not true, is
byte-identity of the *physical* image. The practical consequence is worth knowing
before relying on it: **two imports of identical data cannot be compared by
checksum, and bulk-import snapshots will not deduplicate in content-addressed
storage.**

Two further things are therefore deliberately *not* asserted as seed-stable. The
snapshot's total byte count is excluded because `manifest.json` carries a
`created_at` wall clock whose rendering drops a trailing zero about one run in
ten (a measured 654-vs-655-byte swing). Byte-identity of the data components is
excluded for the reason above; their combined **size** is asserted instead, since
the same keys are written whatever the order. `TestBulkImportParity_ByteBoundary`
pins all three regimes, so a flip — including an improvement, such as ordering
property keys — is noticed rather than quietly making this section false.

### Proving the check can fail

`TestBulkImportParity_Sensitivity` perturbs the **model** — never the durable
image — across twelve dimensions and requires each to produce a violation on the
expected dimension: missing node, extra node, wrong label, wrong property value,
missing property, missing edge, phantom edge, collapsed parallel edges, wrong
edge type, wrong edge weight, wrong edge property, and a **kind swap that keeps
the same digits** (rebinding an integer property to the string of its own
decimal rendering), which is the case a kind-blind comparison would wave
through. Perturbing the model rather than the graph is deliberate: the published
image stays exactly as a passing run publishes it, so what is measured is the
checker's power to see a difference. Alongside it,
`TestBulkImportParity_NonVacuous` reads the run's measured evidence — snapshot
file count and byte size, publish stats, per-dimension comparison counters — and
fails a run that degenerated into comparing nothing.

## Read-only transaction isolation

The `BeginReadTx` read-only transaction is covered by two focused tests in
`internal/sim/` (`read_tx_test.go`):

- **Write rejection.** Every writing/DDL statement issued inside a read-only
  transaction is rejected with the typed `cypher.ErrWriteInReadOnlyTx` and
  applies nothing, while reads continue to work.
- **No dirty reads.** A writer commits nodes in atomic batches of five while many
  concurrent read-only transactions repeatedly count them; the engine's
  visibility barrier flips each transaction's writes visible as one step, so
  every observed count is a multiple of five — observing an intermediate value
  would be a partial-transaction (dirty) read across the isolation barrier. The
  test is `-race`- and `goleak`-guarded with a deadlock watchdog.

The **isolation level** itself is certified by the ST7 scenario
(`internal/sim/durable_scenarios.go`), which since rmp #2307 asserts that the two
counts taken inside ONE read transaction are equal — snapshot isolation across
statements — on the end-to-end path under crash and fsync-fault injection. The
level was per-statement read-committed until then, and ST7 recorded that as a
property the engine did not have.

## MVCC multi-session and concurrency coverage

Sprint 345 gave the simulator first-class MVCC coverage: deterministic
interleaving of multiple explicit transactions, isolation checkers that
adjudicate every read against a per-transaction oracle, deliberate contention,
crashes landing while transactions are open, and a production-profile scenario
that combines all of it over the durable store. Sprint 347 added the substrate's
own telemetry to that picture (rmp #2470): the scenarios above adjudicate
isolation *outcomes*, all of which stay green while the vacuum quietly stops
reclaiming, so the version counts, the reclamation watermark and the vacuum's
progress are now read and held to bounds of their own.

### The deterministic multi-session mode (`RunMVCCSessions`)

K logical sessions run explicit multi-statement transactions over the
WAL-backed SimDisk store, interleaved at statement granularity by the seeded
scheduler on a single goroutine (`internal/sim/mvcc_sessions.go`). The oracle
gives each write transaction a begin-snapshot workspace
(`internal/sim/oracle_tx.go`) folded into the committed model only when the
engine acknowledges the COMMIT, so parity holds at every tick.

Isolation checkers (`internal/sim/mvcc_isolation.go`, rmp #2436) run inside
the transactions themselves:

- **Snapshot stability** — a read-only transaction captures the committed
  counts, names, and edges at BEGIN; every in-transaction count read must
  match, however many commits fold in between (counted, so the contested case
  is provably exercised).
- **Read-your-own-writes** — every write statement is probed back through the
  same handle; a divergence is held as a *doom suspect* under the refused-void
  contract (rmp #2354) and the transaction must then fail with the typed
  conflict — a clean COMMIT of a suspect is the violation. A session-level
  probe at BEGIN asserts the session sees its own earlier commit.
- **Atomic visibility** — write transactions create invariant-bearing node
  pairs across two statements; every reader must observe both members or
  neither, dated by the fold sequence. Observing exactly one is a strict
  subset of a committed multi-object transaction.

These checkers found four engine defects on arrival (rmp #2445, #2446), all
fixed and pinned by regressions in
`internal/sim/mvcc_isolation_regression_test.go`.

### Contention: the lost-update scenario (`RunMVCCContention`)

Sessions deliberately collide on a small shared counter space with the classic
lost-update shape — a snapshot read plus a blind write-back — beside disjoint
control keys (`internal/sim/mvcc_contention.go`, rmp #2437). The adjudication
is exact at transaction granularity: each counter's final value equals its
acknowledged increments (a shortfall is a lost update, an excess a phantom
apply), refused transactions leave no trace, and every refusal must match
`cypher.ErrSerializationConflict` — the typed retriable sentinel exported for
clients.

### Crashes with open transactions

`MVCCSessionsConfig.Crash` (rmp #2438) injects seed-scheduled SIGKILL-style
crashes while transactions are open; the store reopens through real WAL
recovery and the recovered state is adjudicated at transaction granularity:
the full folded model must be recovered exactly (acked ⊆ recovered ⊆ issued
collapses to equality, because the oracle folds whole transactions at
acknowledgement and nothing else), and a sweep of every committed pair detects
a torn replay as exactly one surviving member.

### Concurrent-mode transactional roles and during-run oracles

The genuinely parallel Bolt-wire mode gained explicit-transaction roles
(rmp #2439): disjoint multi-statement writers and contended read-modify-write
writers, with per-connection transaction ledgers and conflict classification
by the driver-verified Bolt code `Neo.TransientError.Transaction.Outdated`
(RESET recovery after every explicit refusal; IGNORED never classifies as
success). Quiescence verifies every acknowledged marker present, every
refused marker absent, totals conserved, and zero lost updates on the shared
counters.

During-run oracles (rmp #2440) observe correctness while the run executes:
per-connection monotonic reads, same-connection read-your-own-writes, and
atomic batch visibility (a batch writer commits fixed-size tagged
transactions; readers must only ever observe whole multiples).

### The production profile (`--scenario=production-profile`)

One command simulates a realistic multi-client production environment
(rmp #2441, `internal/sim/production_profile.go`): the full role population —
contended and disjoint transactions of mixed sizes, atomic batches,
during-run oracle readers, RYOW probes, plain writers/readers, and overload
traffic — over the durable store, in crash cycles. Each cycle joins every
client and the server, crashes the disk, reopens through real recovery, and
adjudicates the accumulated transaction-granular ledgers: acknowledged
transactions survive (acked ⊆ recovered ⊆ issued), refused transactions
leave nothing, and the contended counters carry their acknowledged increments
across every crash. The short layer runs a 24-connection two-cycle
configuration; the soak layer (`-tags=soak`) runs 256 connections over three
cycles.

Since rmp #2469 the profile runs over the **checkpoint-backed** layout and each
cycle takes a checkpoint **while its MVCC traffic is in flight**, so recovery
goes through the snapshot plus the WAL tail rather than through a complete WAL.
The overlap is measured, not assumed: the MVCC clock advances by one per
published commit, so the clock delta across the checkpoint call counts the
commits that landed inside it — tens of them per short-layer cycle, with the
client population still running when the checkpoint returned. The exact count
varies with goroutine scheduling, so the gate is that at least one cycle
measured a non-zero one, never a particular number.

Since rmp #2470 each cycle also carries a **standing MVCC-substrate watch**: the
recovered store is read at two genuinely quiescent points — once when the reopen
completes and before any client touches it, and again after the post-recovery
beacon commits — and adjudicated by `checkMVCCSubstrate`. That is where a
recovery that came up holding a commit it never published, or with the horizon's
integrity counters already non-zero, would be caught. The non-vacuity gate is
deliberately **not** applied here: the profile watches the substrate while doing
something else, and it is `RunMVCCSubstrateChurn` that certifies it. See
[MVCC substrate telemetry](#mvcc-substrate-telemetry-rmp-2470).

### MVCC clock and transaction-sequence recovery across the boundary (rmp #2469)

Every other MVCC crash scenario re-derives the clock from a **complete** WAL,
because none of them checkpoints. The case that matters is the other one: the
checkpoint has truncated away the prefix carrying the early timestamps, so the
only durable record of them is the instant the snapshot manifest carries
(`snapshot.Manifest.CommitTS`, folded into the derived floor by recovery,
rmp #2309/#2520). `internal/sim/mvcc_clock_recovery.go` measures that reopen
directly from the durable bytes — every v3 commit marker carries its
transaction's sequence and the MVCC instant it became visible at — and holds it
to:

- **No instant is re-minted.** Every post-recovery commit timestamp strictly
  exceeds every timestamp the recovered image carried, over both the surviving
  WAL and the manifest's recorded instant, and no two post-recovery commits
  share one.
- **The sequence resumes, it does not restart.** No post-recovery transaction
  takes a sequence the recovered WAL image still carries — one WAL holding two
  different transactions under one number is the ambiguity rmp #2302 exists to
  prevent — and none sits at or below the image maximum. The reopen seeds
  `txn.Options.ResumeTxnSeq` from `recovery.Result.MaxTxnSeq`, which is what
  recovery derives that value for; the oracle is what keeps the two ends wired
  together.
- **A pure-snapshot recovery reconciles its floor against the image.** With the
  WAL truncated to zero and nothing replayed (proved by the rmp #2468 evidence
  helper), the clock floor is at least the instant the manifest recorded, **and**
  the maximum recovery *derived* is at least that instant.

The last clause is the load-bearing one. Rehydrating an image mints instants of
its own — measured at about three per restored node — so a wide graph lifts its
own clock past its recorded instant whether or not that instant was ever read,
and a floor-only oracle would pass on the size of the graph. The derived maximum
cannot be satisfied that way: with an empty WAL it can only have come from the
manifest.

Each obligation has a sensitivity arm in
`internal/sim/mvcc_clock_recovery_test.go`, run over the real recovery path:

| Perturbation | Effect | Oracle |
|---|---|---|
| manifest instant rewritten to 2^40 | floor rises to 2^40+1 | the floor is *derived from that field*, not a coincidence |
| manifest instant dropped (trailer re-framed, so recovery accepts it) | one node updated 40 times recovers with floor 3 against a captured instant of 41, and then re-mints instants 4–7 | floor, derived-maximum and re-minted-instant clauses all fire |
| `ResumeTxnSeq` left unseeded on the reopen | the sequence restarts at 1 against an image carrying 1–7 | sequence clauses fire, including a genuine collision on 4 |

The profile also proves the run entered each case rather than skipping it, and
fails as a scenario when it did not: at least one checkpoint overlapped by a
published commit, at least one recovery through snapshot plus a replayed WAL
tail, and one through the snapshot alone — the forced crossing, which reclaims
the whole WAL (tens of kilobytes → 0 bytes), replays zero ops, and comes up on a
floor at least one past the instant the manifest recorded.

### MVCC substrate telemetry (rmp #2470)

The substrate publishes a full account of what versioning is holding and why —
`lpg.MVCCStats` (live records per store, the two published bounds, the
reclamation watermark, in-flight commits, the retained chain-depth
distribution, the write-outcome counters) and `lpg.VacuumStats` (sweeps,
records released, backlog). Before this oracle the simulator read exactly **one
field** of it, `MVCCStats.Now`, through `SimStore.ClockNow`, and `VacuumStats`
not at all.

That mattered because every MVCC scenario asserts isolation **outcomes** — no
lost update, no phantom apply, a stable snapshot — and all of them stay green
while the vacuum quietly stops reclaiming. A substrate that never releases a
version answers every query correctly and grows without bound.

`internal/sim/mvcc_substrate.go` folds readings into a single evidence record
and `checkMVCCSubstrate` adjudicates them; `internal/sim/mvcc_substrate_arms.go`
drives the workloads. Every clause is a **bound**, a **monotonicity**, or a
**must-be-zero** — never an exact value — because the vacuum is a background
goroutine and the readings are therefore scheduling-dependent even though the
committed data is a pure function of the seed.

| Property | Observable | Clause |
|---|---|---|
| Version chain depth returns to a bound | `MVCCStats.ChainDepth` (`mvcc.Depths`: log2 buckets + exact `Deepest`) | deepest retained depth observed **with no snapshot registered** stays within `maxRetainedChainDepth` |
| The vacuum watermark advances rather than stalling | `MVCCStats.Watermark`, against `.Now` | over a window of ≥2 readings the watermark moves while transactions commit, and never moves backwards |
| In-flight commits return to zero at quiescence | `MVCCStats.InFlightCommits` | zero at every reading the scenario declares quiescent |
| Version memory stays under a published ceiling | `MVCCStats.Total` vs `.Bound` and `.Ceiling` | no reading exceeds the instantaneous ceiling, and once reclamation pressure has been applied the vacuum must have released something |

Three further clauses come from counters the substrate documents as impossible
while it is sound: `WatermarkRegressions`, `HorizonStaleLeaves`, and
`UnregisteredSnapshots` — the last being the one state in which version memory
genuinely has no bound, since while it is non-zero the watermark is zero and
nothing is reclaimed.

**All four properties have a genuine public observable**, but two are not
readable in the shape one would guess, and both traps were found by measurement
before the oracle was written:

- **The vacuum is not always running.** It is woken by churn crossing
  `MVCCStats.Bound` (4096 records as configured here), not by a timer, and it
  exits once consecutive passes free nothing. Measured: 800 committed writes
  left 815 live records with `passes=0, reclaimed=0` — and every bound assertion
  passing, because nothing had been asked of the substrate. A run that never
  applies reclamation pressure proves nothing, which is why
  `checkMVCCSubstrateNonVacuity` is a separate gate rather than a clause.
- **The chain-depth histogram describes the last complete sweep, not the
  present.** The reclaimer resets a store's histogram when it starts that store
  and fills it as it walks, so a graph whose versions have all been released
  reads back as `chains=0, deepest=0` — indistinguishable, to a bound check
  asserted at quiescence, from "every chain is short". The oracle therefore
  samples **during** churn and counts the readings that carried a non-empty
  distribution, so the population can be shown to be non-trivial.

The depth bound is the **oracle's**, not the substrate's: `mvcc.Depths` is a
distribution with no declared ceiling, and `maxRetainedChainDepth` is documented
as stated here rather than dressed up as a published contract.

#### The abort-heavy arm

`RunMVCCSubstrateAborts` drives the contention scenario (`RunMVCCContention`,
rmp #2437) through a new `OnQuiesce` hook that fires at the drain point — every
open transaction rolled back, the store still open — and asserts the refused
transactions' versions were **withdrawn** rather than left to accumulate.

It forces serialization conflicts rather than rollbacks, because
`mvcc.WriteCounts.Aborts` counts transactions the *substrate* refused, not
transactions a *client* rolled back: GoGraph's explicit rollback is served by
the statement undo log, so a voluntary rollback publishes its inverses and is
counted as a **commit**. Measured: 50 rollbacks produced `commits +49,
aborts 0`, while 29 conflicts produced `aborts=29, conflicts=29,
byStore[1]=29`. Withdrawal itself is **synchronous** — `Graph.abortWake` calls
`withdrawAbortedNow` before returning, because a present-time read takes the
stored value directly — so the assertion is the strong one: the aborted versions
are not in the live count at the drain point at all.

#### The commit-quiescence boundary

A checkpoint's phase 1 runs under the commit serialiser with writer admission
closed and waits out the durable-but-unpublished window
(`Checkpointer.awaitCommitQuiescence`, rmp #2349), so the reading taken
immediately after a checkpoint returns on a single-writer store must show no
commit allocated and unpublished. `MVCCSubstrateConfig.Checkpoints` publishes
checkpoints spread through the churn and asserts exactly that.

The 30-second `commitQuiesceTimeout` fail-stop is pinned **by contract rather
than provoked**: reaching it needs a transaction held between its WAL fsync and
its MVCC publish, and the only seam that could do so lives in `graph/lpg`,
outside this package. What is asserted instead is the observable the timeout
exists to report — `InFlightCommits` at the boundary — which catches the same
defect without a 30-second wait. In the production profile, where traffic
overlaps the checkpoint and the point after it is *not* quiescent, the
checkpoint **returning at all** is the assertion that the boundary drained.

#### Sensitivity and non-vacuity

| Arm | What it proves |
|---|---|
| `TestMVCCSubstrate_TooSmallRunIsRejectedAsVacuous` | **live** — 200 committed writes never wake the vacuum, the adjudication is clean, and only the non-vacuity gate refuses the run. This is the specific trap the oracle was warned about: a bound satisfied because the workload never produced enough versions to stress it |
| `TestMVCCSubstrate_PinnedReaderIsNotAViolation` | **live specificity** — one reader held open across 6000 writes stalls the watermark at 4 while the clock reaches 6004 and drives retained depth to **1372**, past the bound; the oracle correctly reports nothing, because the registered snapshot explains both |
| `TestMVCCSubstrate_ClausesFire` | 11 fabricated-evidence perturbations, each firing one clause, with the unperturbed control silent |
| `TestMVCCSubstrateNonVacuity_ClausesFire` | 9 perturbations over the non-vacuity gate itself |
| `TestMVCCAbortWithdrawal_ClausesFire` | 5 perturbations over the withdrawal clauses, including aborted versions that look retained |

Two false positives were found and fixed by the live arms rather than by
inspection, both of the same shape — a *window* quantity judged on evidence
with no window, or a *legitimate* explanation ignored:

- the watermark-stall clause fired on a single-reading record, where
  `first == last` makes the advance zero by construction;
- the "vacuum released nothing" clause would have fired on the pinned-reader
  run, where the vacuum ran 18 passes and freed 16 records **because isolation
  required it to**. Both are now guarded on having ≥2 readings and on no
  snapshot having been registered at any reading.

#### Where it stands

| Scenario | Layer | Role |
|---|---|---|
| `RunMVCCSubstrateChurn` | short (6 000 rounds) / soak (400 000 rounds, 8 checkpoints) | the certifying arm — adjudication **plus** the non-vacuity gate |
| `RunMVCCSubstrateAborts` | short (600 ticks) / soak (40 000 ticks, 12 sessions) | the abort-heavy arm |
| `production-profile` | short / soak | **standing watch** — the recovered store is read at two quiescent points per crash cycle and adjudicated by `checkMVCCSubstrate`, without the non-vacuity gate, because the profile watches the substrate rather than certifying it |

The production-profile evidence is scoped to **one store instance** per cycle
deliberately: a crash replaces the graph wholesale and the substrate's counters
restart at zero with it, so folding readings from either side of a crash into
one record would show the watermark moving backwards and report the harness's
own bookkeeping as an isolation defect.

## Language-surface gaps (rmp #2462)

Four narrow surfaces the DST issued but never adjudicated. Each is an
oracle-computed or contract-pinning check wired into an existing scenario at its
existing cadences, with a sensitivity test that makes it fire.

### Non-detach `DELETE n`

Every other scenario deletes through `DETACH DELETE`, so the plain form — and
openCypher's rule that it must REFUSE a node that still has relationships — was
never exercised. `internal/sim/delete_contract.go` predicts both arms from the
oracle's adjacency and holds the engine to them:

- degree 0 → the delete COMMITS, reporting exactly one `-nodes`, adjudicated
  through the per-op counters oracle (`CheckOpCounters`);
- degree > 0 → the delete is REFUSED with `exec.ErrDeleteNodeHasRelationships`
  and applies nothing; the node must still be there afterwards.

The typed error arrives on the **drain**, not from the write call: the engine
accepts the statement and fails while producing rows, so the probe inspects both
sides. It rides the `pattern-shapes` scenario, whose oracle knows adjacency
exactly, on its own cadence; only degree-0 nodes are ever removed, so every
planted motif survives for that scenario's shape assertions. A terminal gate
requires both arms to have fired.

### `CALL … YIELD … WHERE` (the #1966 surface)

`SHOW … YIELD … WHERE` was already covered; the **procedure** form is a distinct
code path — the translator lifts the predicate as a `Selection` over the
`ProcedureCall` — and is the one that silently dropped its `WHERE` until #1966
was fixed. `checkCallYieldWhere` filters `db.constraints()` and `db.indexes()`
by a name the DDL model knows and requires the result to equal the model-side
filter. Because a predicate that selects everything would still compare equal if
the `WHERE` were dropped again, each probe additionally asserts the filter is
**strictly narrowing** whenever the model enumerates two or more rows.

### Cartesian-product notification

The engine analyses every planned query for a cross product between disconnected
patterns and attaches an advisory, reachable through
`cypher.Result.Notifications` (and the Bolt SUCCESS `notifications` metadata).
The DST issued Cartesian shapes routinely but never inspected one, so the whole
advisory surface was unguarded. `CheckCartesianNotification` pins **both**
directions over four shapes — comma-separated disconnected paths and two
sequential `MATCH` clauses must warn; a connected pattern and disconnected paths
joined by a `WHERE` predicate must not — so neither an always-on nor an
always-off implementation can pass. Notifications are attached on the read path
(`Engine.Run`); the `RunInTx` write path leaves them empty, so every probe is a
read. The check is a plan-time property of the query text, so it is meaningful
on an empty graph and runs at the periodic cadence, after each recovery, and at
the end.

### Bolt wire parameter type matrix

The wire actors bound only String and Integer, leaving every other PackStream
kind untested on the path where a literal/parameter divergence actually reaches
a driver user — the Bolt server hands the decoded parameter map straight to the
engine. `probeWireParamTypes` binds each kind over the real wire before any
connection spawns and verifies it by read-back, including a genuine **Map** used
as the property map of a `CREATE` (the `RunAny`-with-a-real-map path), Float and
Boolean parameters as pattern-predicate seek keys, and a Null parameter through
`SET` (which must remove the property). The probe is population-neutral — it
deletes the one node it creates — and its findings gate
`ConcurrentResult.Consistent`.

What round-trips:

| Kind | Wire encoding | Round-trips |
|---|---|---|
| String, Integer, Float, Boolean, Null | native | yes |
| Map | native PackStream Map | yes |
| List | native PackStream List | yes (since #2513) |

A **List** always bound correctly in both directions of evaluation — indexing,
`size`, and equality against a literal list all gave the right answers, so the
engine really did receive a list — but the RECORD encoder had no
`expr.ListValue` arm (`bolt/server/session.go`), so a list column was emitted as
its `String()` rendering. A literal list return was stringified identically,
which located the gap in the encoder rather than in parameter binding. #2513
added the arm; the probe now asserts both halves live — the list's input
semantics through scalar projections, and its output encoding as a genuine
PackStream List. The full end-to-end matrix (nesting, entities, temporals, and
the list-producing functions `collect`/`labels`/`keys`/`nodes`/`relationships`)
is `internal/sim/wire_list_encoding_test.go`.

## Concurrency hypotheses chased

The mem-pressure and cpu-starvation scenarios are backed by two focused
concurrent regression tests in `cypher/` that chase specific
fair-scheduling / barrier hypotheses, each under a deadlock watchdog:

- **Aggregator cap inside the write barrier.** Many concurrent aggregating
  writes that trip `MaxCollectItems` *inside* the visibility barrier run
  alongside honest reads and writes. The error path releases the barrier on
  every iteration (no held-`visMu` deadlock), and the engine stays usable —
  evidence the in-barrier error path is leak-free.
- **Parallel CREATE INDEX backfill.** An above-threshold parallel backfill runs
  concurrently with honest readers. The backfill scan runs *before* registration
  and *outside* the visibility barrier (a reader's plan build sees either no
  index or a fully populated one), so readers are never blocked by a held
  barrier; the test asserts forward progress (no deadlock, no wedge) rather than
  interleaving, which is statistical. Neither hypothesis was a real defect.

## Swarm, coverage, and cross-checking modes

- **Swarm** (`--swarm`) runs many seeds across a bounded worker pool, time- or
  count-boxed, and reports pass/fail counts plus a reproduction command per
  failure.
- **Coverage** (`--coverage-report`, `--bias`) tracks which scenarios have been
  exercised and can bias selection toward under-covered ones.
- **Differential**, **upgrade**, **cross-release**, and **metrics-oracle** modes
  cross-check equivalent engine configurations, on-disk data-compatibility across
  releases, and metrics against the oracle. See the corresponding `*_test.go`
  files in `internal/sim/`. The differential variant pairs prove that
  result-equivalent engine toggles produce byte-identical observable output on
  the same trace: `DefaultVariantPair` (hash-join on/off), `RangeSeekVariantPair`
  (range index seek on/off), and `ParallelScanVariantPair` (the morsel-parallel
  count reduce versus the serial path) — the last brings the engine's
  multithread/parallel count path under the DST. The serial-vs-parallel CREATE
  INDEX backfill is proven content-identical at the engine level
  (`cypher.TestBackfillNodeHashIndex_SerialVsParallelIdentical`, via
  `EngineOptions.DisableParallelBackfill`), since a backfill engages its parallel
  phase only above 8192 nodes — more than a scripted trace builds.

### Cross-release compatibility beyond the WAL (rmp #2477)

The cross-release harness imported `store/{wal,recovery,txn}` and nothing else,
so the only thing a prior release ever handed the current code was a **WAL**. A
prior release's `manifest.json`, `csr.bin`, `labels.bin`, `properties.bin` and
`mapper.bin` had never been parsed by current code in any cross-version test —
an entire on-disk format family outside the harness. Two surfaces created in
this sprint sat squarely in that gap: the manifest integrity trailer (#2520) and
the variable-width weights sentinel (#2526), both deliberately shipped **without
a schema-version step**, so nothing else would have caught a regression in
either direction.

Two vehicles now close it, and they are deliberately different in strength.

**The prior release publishes a checkpoint.** `cmd/sim-xrelease-helper` now
writes a **snapshot directory plus a truncated WAL** before it exits, and the
reopen routes through `recovery.OpenCtx` — the full stack — instead of the
WAL-only `recovery.ReplayWAL` core. The current reader therefore parses bytes a
prior release wrote. The claim "the snapshot was opened" is adjudicated from
**two independent observations**, because recovery's own `SnapshotHit` cannot be
its own witness: the filesystem is read separately with the current manifest
reader (`InspectPriorSnapshotDir`), and a directory that exists on disk but is
not loaded — or is loaded but cannot be parsed — is a
`SnapshotProvenanceGap` that fails `Parity()`.

The checkpoint lives in a **second, removable source file**
(`cmd/sim-xrelease-helper/checkpoint.go`) staged into the tag's worktree
alongside `main.go`. If the tag will not build with it, only that file is
dropped and the build is retried. The alternative — one file — would have made
any checkpoint-API drift fail the whole build, which the caller reports as an
environment-precondition **skip**, silently deleting that release from
cross-release coverage. Degrading one capability is preferable to losing a tag,
and which happened is recorded (`HelperCheckpointBuilt`, `BuildFallbackErr`) and
logged rather than inferred.

#### The fallback fired at every real tag (rmp #2531)

The two-stage build worked exactly as designed, and that is how the following was
found: it was taking the fallback at **every tag the harness actually
exercised**. Against `HEAD`-as-prior the checkpoint half built and the whole
chain passed; against `v0.2.0` and `v0.3.0` it did not compile, so both fell back
to a WAL-only image and reported — correctly, and in its own words — that the tag
contributed no snapshot-format coverage. The loud degradation is what made this
visible; the effect was nonetheless that **no snapshot written by a genuinely
older binary had ever been opened by current code**, and the compatibility claim
rested entirely on HEAD-as-prior, which is not a compatibility test.

The cause was one symbol. `Checkpointer.RunCheckpoint` was only **exported from
v0.6.0**; before that the same body existed solely as the unexported
`runCheckpoint`. The fix drives the checkpoint through
`New` → `Start` → `TriggerCtx` → `Stop` instead, a trio exported with an
unchanged shape since **v0.1.0**, reaching the entire tag history the repository
holds. It is the same body either way — the loop's trigger arm calls precisely
the `runCheckpoint` that `RunCheckpoint` later exposed — so nothing about the
artefact on disk depends on which door was used. `Start` is not optional:
`TriggerCtx` parks on a reply from the loop, so triggering without a running loop
would **hang** rather than fail.

Measured across all fourteen release tags (`v0.1.0` … `v0.11.0`), every one now
publishes a manifest-v3 snapshot that the current reader opens, with
**snapshot-only recovery** (`walOps=0`) and 52 label plus 65 property records
arriving from the image. There is therefore **no documented snapshot floor**: no
release in the repository predates snapshot support. A `SnapshotFloorReason` field
exists on the tag list for a future tag that needs one, and is asserted in the
negative — a floor declared on a tag that can in fact publish fails, so a stale
declaration cannot quietly cost coverage.

Consequently the snapshot facts are now **asserted, not logged**, for every tag
declared capable. A tag that falls back to WAL-only fails the arm instead of
recording a note, because once the staged half names only symbols exported since
v0.1.0, a fallback is no longer a property of the tag — it is a regression in the
helper.

#### Publishing a snapshot masks a prior release's WAL-replay defect

Closing the snapshot gap moved a different path out of coverage, and the harness
keeps both rather than trading one blindness for the other.

A checkpoint truncates the WAL to the snapshot watermark, so recovery satisfies
itself from the snapshot and replays almost nothing — that is exactly why
`SnapshotOnlyRecovery` can require `walOps == 0`. A prior-release defect that
lives in **WAL replay** therefore disappears the moment the image gains a
snapshot. One does:

> **Prior-release defect, `v0.1.0` and `v0.2.0` (not a current-code defect).**
> At the fixed reproducer (seed `0xC0FFEE`, 300 ops) those releases write an
> image whose live state is 32 nodes / 20 edges, but **their own** recovery reads
> it back as **79 nodes / 23 edges** after replaying 682 WAL ops. The current code
> reproduces that same 79/23 reading **faithfully**, which is the cross-version
> contract — current recovery must reproduce the prior release's own reading of
> its own image, never retroactively repair it. The gap is absent from `v0.3.0`
> onwards.

This is documented here and pinned in executable form as
`priorRelease.WALReplayGapExpected`, so it is not rediscovered as a new finding on
a later audit. A tag whose declared gap stops reproducing fails, and so does a tag
that develops an undeclared one.

`TestCrossRelease_WALOnlyImageIsSnapshotFree` keeps that path covered, via
`XReleaseBuildOptions.ForceWALOnly`, which skips the checkpoint stage on a tag
that would have passed it. It serves two purposes at once:

- **It is the snapshot oracle's negative control.** Every ordinary arm now
  reports `SnapshotOpened=true`, and a flag observed true in every run performed
  is indistinguishable from one wired to true. This arm requires it to read
  **false** — on an image proven to have no snapshot — so the true readings carry
  information. It is the permanent form of "revert the fix and check the test
  fails", which proves the oracle discriminates on every run rather than once.
- **It is the only remaining exercise of a prior release's WAL-replay path**, with
  a non-vacuity assertion that `walOps > 0` so the arm cannot pass while
  exercising nothing.

**Frozen artefacts pin the old shapes.** `internal/sim/testdata/xrelease/`
carries a complete pre-#2520/#2526 snapshot directory: a manifest with no
integrity trailer and no `integrity` key, and a `csr.bin` with the dense
8-byte-wide `float64` weights column. It runs on the **short layer**, needs no
git, no worktree and no tag, and asserts the documented contract in both
directions:

| Surface | Older artefact, current reader | Newer artefact, older reader |
|---|---|---|
| `manifest.json` framing (#2520) | loads, and reports `IntegrityVerified` **false** — it declares no integrity it does not have | accepted: the trailer follows the JSON value, which a pre-#2520 decoder never reads past. No version step, by design |
| `csr.bin` weight width (#2526) | dense width-8 column parses through the native path with the exact `float64` values | refused **deterministically**, by both of that release's guards — the extent guard (255 × nEdges exceeds the file) and the width guard (`0xFF` is not a width `csrWeightSize` could return) |

The old reader is a build that no longer exists in this tree, so the second
column applies its documented **decision rule**, restated from the two guards it
contained. A model is weaker than the binary and is treated as such: the same
rule must **accept** the frozen old artefact and **refuse** the new one, so a
rule hardwired in either direction cannot pass. The prior-tag subprocess remains
the authority; this is the half that runs on every change.

**Non-vacuity.** A fixture silently regenerated in the current format would keep
every assertion passing while testing nothing — the trap #2520 avoided by
leaving its own frozen fixture unframed. Three separate guards prevent it:

- every component's **SHA-256 is pinned in the test source**, not in a golden
  file, because `go test -update` rewrites a golden and cannot rewrite a
  constant. `internal/goldens` is still used, for the artefact that is *supposed*
  to track the current writer: the sentinel-bearing `csr.bin`;
- the fixture's **raw framing** is decoded independently of
  `snapshot.LoadManifest` — trailer magic, bytes past the JSON value, the
  `integrity` key — so the loader is never asked to describe its own input;
- every oracle is **two-sided in the same test**: a manifest the current writer
  produces must come back *verified*, and the legacy width rule must *accept*
  the frozen artefact.

For the reopen itself, the graph must come back whole from a directory whose WAL
shrank to nothing across the checkpoint and whose replay reported **zero** WAL
ops — so the snapshot bytes, and only the snapshot bytes, can account for it.

### Required counters per fault scenario (rmp #2479)

The metrics oracle above reads four counters, all from the Cypher layer. Every
storage- and Bolt-layer metric the module emits was unasserted, so the counter
that would prove a fault fired was the one nothing read.
`internal/sim/metrics_required.go` adds a **per-scenario required-counters
declaration**: each fault scenario states the counters its faults must move, and
failing to move a declared counter is a violation. It is a coverage precondition,
kept apart from the scenario's own verdict for the reason rmp #2470 established —
an uninformative run must not read as a faulty one — and adjudicated in the
standing three-part shape: a **separate, shape-only** non-vacuity gate over the
declaration itself (`CheckCounterDeclShape`, which reads no run and asks only
whether the declaration could ever have failed), then the **unconditional**
verdict (`ScenarioCounterDecl.Check`), then the **witness** — what the run
actually emitted — logged and never asserted.

Seven scenarios carry declarations: the three mechanical storage-fault arms
(`csrfile-publish-fault`, `wal-corruption-failstop`,
`checkpoint-dirfsync-fault`), `checkpoint-crash-storm`,
`snapshot-corruption-failstop`, the `db-teardown` fault arm, and the
`checkpoint-cadence` transient-failure arm. Every metric name in them was
obtained by **driving the scenario** with a recording sink installed and reading
what arrived, across the scenario's own spread of seeds; nothing was copied out
of `docs/metrics.md`. That mattered twice: the cadence arm never moves
`store.checkpoint.RunCheckpoint.errors` at all — its environment drives the
checkpointer through its own fold callback, so the counters that move are the
snapshot writer's — and the csrfile arm moves nothing whatsoever.

Two findings are the substance of the task.

**A counter shared with another path is satisfiable without the fault firing.**
`store.wal.Decode.errors` is the only fault counter the WAL-corruption arm emits,
and `store/wal/format.go` increments it on every decode failure class — including
the `io.EOF` path that yields `wal.ErrTornFrame`, which is a benign crash tail
with no corruption in it. The declaration therefore records the counter as
`shared` and names its discriminator: `runWALCorruptionFailStopWith` re-runs the
identical scenario with the interior byte flip **withheld** and requires the
counter to stay at zero. MEASURED: 2 with the flip, 0 without. The same shape
appears wherever a declared counter is an aggregate — the storm's
`store.checkpoint.RunCheckpoint.errors` is discriminated by
`store.recovery.snapshot.promoteParentFsync`, which has exactly one emission site
in the module; the snapshot battery's three aggregates are discriminated by the
eight per-component decoder counters.

**Where no counter exists, the blindness is asserted rather than assumed.**
`store/csrfile` emits no metric at all, so nothing can witness the ENOSPC bound or
the armed `Sync` fault in the atomic-publish arm. The declaration says so, and
pins it: no name under `store.` may be emitted while the arm runs. An empty
declaration would have passed silently; this one fails the day csrfile gains a
counter, which is what forces it to be updated instead of quietly aging. The same
honesty applies inside the snapshot battery, where eight of the nine components
have a unique per-component witness and the mapper — whose damage is caught
before `snapshot.ReadMapperString` is reached — has none, and is logged as such
on every run.

Falsifiability is proved on **three different scenarios**: withdrawing the WAL
byte flip, withdrawing `FaultOnClose` from the composed teardown, and running the
clean cadence arm each leave every declared counter at zero and make the
corresponding declaration fire, with each proof asserting that the specific
declared counters are the ones reported missing.

### The graph/io completeness surface (rmp #2480)

ST8 drove a CSV and a JSONL edge-list round-trip and a GraphML property
round-trip. `internal/sim/graph_io_surface.go` closes what that left, in two
halves that are separated on purpose.

The **seed-driven half** (`RunGraphIOSurface`) is folded into ST8, so the swarm
drives it across seeds. It exports one model built to be hostile — a DOT reserved
keyword, identifiers carrying a space, a quote, a backslash, `->`, a comma and a
leading `-`, the empty identifier, zero and non-zero weights, and one isolated
vertex — as DOT, CSV and JSONL, and requires the three to describe the same
graph. `graph/io/dot` has no reader, which is why it was imported nowhere in the
simulator; the agreement check is what adjudicates it, with a character scanner
in this file reading the DOT text back. The same half round-trips the JSONL
property path (`WriteWithProps` / `ReadWithProps`) over every property kind the
wire tags — string, int64, float64, bool, time (at a non-UTC offset), bytes and a
nested list — drives eleven points of the `csv.Options` space including the
hand-built two-column, header and comment-line documents no exporter produces,
measures every encoder for byte-reproducibility, and replays sixteen seed-derived
corruptions of the exports through the importers.

The **crafted half** (`RunGraphIOGuards`) is seed-independent by construction: no
byte flip or truncation of an ordinary export will ever produce 65,537 `<key>`
declarations, so the caps are provoked deterministically once rather than on
every seed. It drives thirteen of the fourteen sentinels `graph/io` exports and
matches each with `errors.Is`; the fourteenth, `jsonl.ErrListTooDeep`, is
declared unreachable with its reason measured rather than asserted. It then
cancels all five `*Ctx` readers mid-parse against an uncancelled control.

Three corrections to the surface as it was described before this task are worth
keeping, because each changes what a probe must do:

- **`graph/io/csv` has no `*CappedCtx` variant.** Its ceiling is the
  `Options.MaxBytes` field, and a bare `Options{}` leaves it at zero, which
  DISABLES the cap.
- **Three caps are writer-side.** `ErrPropertyValueTooLarge` and
  `ErrPropertyNestingTooDeep` in both packages, and `graphml.ErrInvalidXMLChar`,
  are raised by the encoders. No mutated export can reach them; they are provoked
  with a hostile graph.
- **Two sentinels were missing from the list.** `csv.ErrTooManyFields` and
  `jsonl.ErrUnknownType` are reader-side caps in the same family and are driven
  alongside the rest.

Both halves are adjudicated in the standing three-part shape: a separate,
shape-only non-vacuity gate first (`CheckGraphIOSurfaceShape`,
`CheckGraphIOGuardDeclShape`), the verdict unconditionally
(`CheckGraphIOSurface`, `CheckGraphIOGuards`), and the witness by `t.Logf` only.
Every gate is proved falsifiable by a synthetic result that drives it red.


## Command-line usage

```bash
# Build the simulator.
go build ./cmd/sim

# Run a single deterministic simulation (seed is a leading positional argument).
go run ./cmd/sim 42 --ticks=100000

# List the scenario catalogue.
go run ./cmd/sim --list-scenarios

# Run a named scenario (note the '=' form — a bare token is parsed as the seed).
go run ./cmd/sim --scenario=search
go run ./cmd/sim --scenario=search-crash 12345

# Run a swarm of seeds, time- or count-boxed.
go run ./cmd/sim --scenario=search --swarm --runs=200
go run ./cmd/sim --swarm --duration=30s --coverage-report

# Drive the real Bolt wire / concurrent / liveness harnesses.
go run ./cmd/sim --mode=wire
go run ./cmd/sim --mode=concurrent --conns=16 --ops-per-conn=25

# Inject deterministic crash + recovery cycles.
go run ./cmd/sim 7 --crashes

# Add full-stack checkpointing: periodically publish a real snapshot and
# truncate the WAL prefix, so a crash recovers via the full snapshot+WAL path.
go run ./cmd/sim 7 --crashes --checkpoint --checkpoint-every=20
```

Flags of note: `--workload` (`default|write-heavy|read-heavy|bad-actor`),
`--check-every` (invariant-check cadence), `--verbose` (print each operation),
`--crashes` / `--checkpoint` / `--checkpoint-every` (the durable crash-recovery
and full-stack checkpoint stack), and `--replay` (see below).

## Reproduce, replay, and shrink

Every failing run prints a `Reproduce with: go run ./cmd/sim <seed>` line.
Because the deterministic mode is a pure function of the seed, re-running that
command reproduces the failure exactly.

`--replay` re-runs the seed in verbose, full-trace debug; on a violation it
applies delta-debugging (`ddmin`) to shrink the operation trace to a minimal
reproducer — the smallest sub-sequence of operations that still triggers the
violation — which is what you attach to a bug report.

## Extending the harness

- **A new invariant check** is a function returning `[]Violation`; wire it into
  the checker or into `CheckSearch` and give it a typed `ViolationKind`.
- **A new search algorithm check** follows the pattern in `search_*.go`: build
  the input (from the live graph or a deterministic shaped fixture), run the
  algorithm, compute an **independent** reference, and compare an **invariant**
  of the answer (never a non-unique witness). Add the call to `CheckSearch`.
- **A new scenario** is a `Scenario` value registered in `DefaultRegistry`
  (`catalogue.go`); it is then automatically available to `--scenario`,
  `--list-scenarios`, `--swarm`, and the coverage report.
- **A new actor** implements the `Actor` interface (`actor.go`) and is added to a
  workload mix; the oracle must model its operations so engine and model stay in
  lock-step.

All new code must preserve bit-reproducibility (no map-iteration order in any
output) and must keep the full suite green under `go test -race ./internal/sim
./cmd/sim`.
