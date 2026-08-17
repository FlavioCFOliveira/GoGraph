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
9. [MVCC multi-session and concurrency coverage](#mvcc-multi-session-and-concurrency-coverage)
10. [Language-surface gaps (rmp #2462)](#language-surface-gaps-rmp-2462)
11. [Swarm, coverage, and cross-checking modes](#swarm-coverage-and-cross-checking-modes)
12. [Command-line usage](#command-line-usage)
13. [Reproduce, replay, and shrink](#reproduce-replay-and-shrink)
14. [Extending the harness](#extending-the-harness)

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
| `type-coverage` | deterministic | Property type system: nodes carry a value of every round-tripping Cypher kind (string, integer, float, boolean, list, a plain ISO-8601 string) plus — since rmp #2457 — all six genuine TEMPORAL types (`DATE`, `LOCAL DATETIME`, `DATETIME`, `LOCAL TIME`, `TIME`, `DURATION`) bound as real temporal parameters, plus a never-set key that must read `NULL`. Each value is read back through the engine and asserted on BOTH its canonical rendering and its expr **kind**, so a temporal that degrades to an untagged string — same text, different type — fails even though its rendering is unchanged; the plain ISO-8601 string property is the deliberate control that must stay a string. Checkpointing is enabled alongside crashes, so the temporals are proven across both durable paths: a crash before the next checkpoint restores them by WAL replay, a crash after one restores them from the published snapshot (the checkpoint truncates the WAL prefix it folded). A temporal `ORDER BY` oracle additionally compares the engine's `ORDER BY n.d, n.id` sequence against an ordering the harness computes from the temporals it modelled, over dates deliberately laid out so temporal order differs from id order. Since rmp #2459 the list-valued property is no longer only read back whole: the LIST-expression surface over STORED data is driven against expectations the harness computes from the list it modelled — the subscript `n.lst[0]`, a NEGATIVE subscript counting from the end, both out-of-range directions (verified against this engine to yield `NULL` rather than an error or a clamp), `size(n.lst)`, the half-open bounds-clamping slice `n.lst[0..2]`, `reduce(acc = 0, x IN n.lst \| acc + x)`, `UNWIND n.lst` aggregated by `count`/`sum`/`collect`, and `WHERE <elem> IN n.lst` counted over the whole label. Each column is asserted on its expr **kind** as well as its value, so a subscript that returned the whole list, or an out-of-range read that returned an element, is reported as the type error it is. `collect` is compared as a MULTISET, since openCypher does not specify the input order an aggregate observes; the list's ORDER is pinned absolutely by the subscript and slice columns instead, so a reversed list still fails. Membership is driven twice — once with an element the model really holds and once with an element no modelled list can contain, whose count must be zero — so the probe is proven to discriminate. The per-node probes take a bounded, stride-based sample that always includes the first and the newest node, while membership scans the whole model; and a modelled list that is empty, non-numeric, or carries fewer than two distinct elements across the entire run is itself reported, so the arm cannot pass by being vacuous. |
| `edge-properties` | deterministic | The relationship write surface on a directed MULTIGRAPH: KNOWS edge instances carry a unique `eid`, a `since` (ISO string), and a `weight` (float), including PARALLEL twins between the same endpoints; the workload mutates individual instances with standalone `SET r.weight`, `REMOVE r.since`, `SET r.since = null` (the SET-path removal, counted per instance since rmp #2501), and `DELETE r`, each pinned by `WHERE r.eid`. The per-instance shadow model verifies every surviving instance's properties round-trip (a mutated instance's parallel twin keeps its own map), every deleted instance stays absent with both endpoint nodes alive, and the per-op counters oracle pins each op's reported effect set — periodically and after each crash/recovery, so the by-handle parallel-edge instance identity survives WAL recovery. |
| `index-diversity` | deterministic | Index-type diversity: a HASH (string), a BTREE (numeric), and a BTREE (string) index are created over an above-threshold graph (engaging the morsel-parallel backfill phase), then write churn + crash/recovery run while the thorough seek-vs-scan consistency check confirms each index agrees with its base data — for both kinds and both value types, including after WAL recovery re-registers and re-backfills them. The scenario also carries the access-path parity and plan-stability oracles: literal vs parameterised predicates (equality, range, `STARTS WITH`, `IN`-list) must agree on results and on the physical access path, and the fixed probe set must re-plan byte-identically after every crash/recovery; and the seek-result diversity oracle: bounded and half-open ranges, `STARTS WITH`, and `IN`-shaped predicates (list and `UNWIND` spellings), literal and parameterised, must reproduce an independent full-scan reference as id-multisets and counts, with a terminal non-vacuity assertion; and the schema-introspection oracle (rmp #2455): `SHOW INDEXES` and `db.indexes()` must reproduce the harness's model of the three declared indexes (name, kind, label, property) after the backfill, after every recovery, periodically, and at the end; and the statistics-regime oracle (rmp #2456): `CALL db.stats.refresh()` runs before the crash window (with a back-to-back throttle probe), after every recovery (the recovered engine must report zero tracked statistics pairs, then rebuild immediately on its fresh rate limiter), and at seed-chosen ticks — with the probe battery asserted result-identical across every refresh, refresh-correlated plan changes reported rather than failed, and a terminal non-vacuity gate (at least one rebuild that published statistics and at least one rate-limit refusal). Since rmp #2464 in-loop checkpointing is enabled, so the three index DEFINITIONS cross the SNAPSHOT boundary: a checkpoint truncates the WAL prefix that declared them, after which the post-recovery consistency and introspection checks are validating definitions loaded from the snapshot's `indexdefs.bin` and `indexes/` components rather than a replayed `CREATE INDEX`. A terminal gate asserts a non-zero checkpoint count. |
| `ddl-checkpoint-crash` | deterministic | **DDL across the checkpoint/snapshot boundary** (rmp #2464). Every other DDL-issuing scenario used to run WAL-only, so recovery always REPLAYED the `CREATE INDEX`/`CREATE CONSTRAINT` frames and the snapshot schema components (`store/snapshot/constraints.go`, `indexdefs.go`, `indexes.go`) were never the source of a recovered index or constraint — leaving the loss mode the checkpointer's phase-3 self-sufficiency re-verification exists to prevent (#1464/#1755: truncating the WAL prefix that first DECLARED a constraint or index) unexercised. Each phase issues DDL, writes data, publishes a real checkpoint, crashes, and reopens through real recovery. Phase 1 declares a `UNIQUE(Person.key)` constraint and a hash index on `Person.city` BEFORE the first snapshot and crashes with an EMPTY WAL; phase 2 declares a btree index on `Person.age` AFTER that snapshot and crashes leaving a genuine WAL tail, so both the pure-snapshot and the snapshot+WAL-tail recovery paths run. The claim that recovery really used the SNAPSHOT is measured, not assumed: the WAL byte image on the SimDisk is read before and after the DDL and before and after each checkpoint, and the reclaimed prefix must COVER the offset at which the DDL frames ended — so those frames are demonstrably gone from disk before the crash — while the pure-snapshot phase additionally requires the reopen to replay ZERO WAL ops. The recovered schema is then adjudicated on three independent surfaces: the schema-introspection oracle (`SHOW INDEXES`/`SHOW CONSTRAINTS` and `db.indexes()`/`db.constraints()` against the harness's DDL model, rmp #2455), the thorough seek-vs-scan index-consistency check on every declared index, and a UNIQUE accept/reject adjudicator that requires a duplicate CREATE to be REJECTED atomically (exactly one node still carries the key) and a fresh key to be ACCEPTED — the control that proves the rejection discriminates. A terminal non-vacuity gate requires real reclaimed bytes, both recovery paths, and both adjudicator arms. Two sensitivity seams pin the oracles: publishing the checkpoint with the constraint/index spec providers deliberately unwired (the pre-#1464/#1755 checkpointer) makes the checkpointer correctly refuse to truncate, and the WAL-prefix oracle FIRES; and removing the published `indexdefs.bin` component makes recovery fail-stop on the missing component rather than silently recovering a schema-less graph. |
| `checkpoint-crash-storm` | concurrent | **Crash DURING the checkpoint snapshot publish** (rmp #2465, closing backlog #1827). Until sprint 347 a checkpoint could never be INTERRUPTED — `SimStore.Checkpoint` is synchronous and always ran to completion, and `Simulator.maybeCheckpoint` treats any checkpoint error as a hard run failure — so recovery's snapshot-promote repair (the block in `store/recovery` marked by the `recovery.snapshot-promote-post-rename-pre-fsync` crash point) was dead code under simulation. The publish is a five-step crash-atomic swap (write+fsync components -> fsync staging dir -> archive rename `live` -> `live.bak` -> publish rename `staging` -> `live` -> fsync the parent dir) whose backup exists so that at EVERY instant at least one complete snapshot is on disk. The scenario crashes at three points of it, each produced by a one-shot path-keyed `SimDisk` arm rather than by timing: `stranded-backup` (the parent fsync fails and only the ARCHIVE rename is written back, so the live name is gone and the previous snapshot is stranded at `.bak` — the only window that reaches the promote repair), `publish-rename` (the publish rename fails, so the publish path's own best-effort archive-restore runs and must survive the crash), and `archive-rename` (the archive rename fails, aborting the publish before the live snapshot is touched). Each cycle publishes a CLEAN checkpoint first — so there is a live snapshot to archive and the WAL prefix holding those commits is really gone — then interrupts the next one while concurrent Bolt committers are still writing; the publish is checkpoint phase 2 and holds no commit lock, so the window is genuinely raced, which the run MEASURES as durable commits landing during the interrupted checkpoint. The oracle is the standard durability contract accumulated across cycles (acked ⊆ recovered ⊆ issued, failures absent, no torn CREATE). Nothing is assumed: the interrupted checkpoint must return the injected `ErrSimFault`, each armed primitive must report having FIRED (an arm whose path never matched is a silent no-op), and the stranded-backup cycle must show the exact durable-image transition only the promote rename produces — backup-only before the reopen, live-only after it — corroborated by store/recovery's own `store.recovery.snapshot.promoteParentFsync` counter. Two sensitivity seams pin the oracles: destroying the stranded backup after the crash loses acknowledged commits for real (the clean checkpoint already truncated their WAL prefix) and the durability oracle FIRES; and a degenerate one-window plan is rejected by the terminal non-vacuity gate. |
| `search` | deterministic | The `search/` algorithm battery over the live graph + structural parity. |
| `cypher-paths` | deterministic | The Cypher-level `shortestPath()` operator: its hop count is compared to an independent BFS over the oracle's KNOWS edges for a bounded, deterministic set of pairs (comparing the path-length invariant, never a specific witness), periodically and after each crash/recovery. `allShortestPaths()` is also verified: every returned path is minimal-length and the path COUNT equals an independent layered-BFS shortest-path count. |
| `cypher-surface` | deterministic | A battery of diverse read shapes — `count`/`sum` aggregation, `WHERE`, `WITH…WHERE`, pattern-count, `OPTIONAL MATCH`, `UNWIND range()`, and `ORDER BY` — is run against independently-computed oracle invariants (scalar values and the sorted-name sequence), broadening the DST's coverage of the Cypher read surface beyond the per-tick parity probe, including after crash/recovery. Since rmp #2452 every Person also carries a `city` from a small seeded vocabulary, and the battery additionally pins: GROUPED aggregation (`count(*)` and `sum(n.age)` BY `n.city` as full row-set equality against the oracle's per-city histogram, plus a mixed INTEGER/FLOAT `CASE` grouping key exercising the exact cross-type single-group equivalence of rmp #2050); `RETURN DISTINCT` over a pattern and a mid-pipeline `WITH DISTINCT` as row operators vs the oracle's distinct-target set; `UNION` over graph rows with complementary AND overlapping predicates (the overlap makes the dedup observable) plus `UNION ALL` duplicate preservation (row count = sum of the arm cardinalities); EXACT `avg`/`stDev`/`stDevP`/`percentileCont`/`percentileDisc` values from oracle arithmetic (1e-9 relative float tolerance; sample vs population stdev, linear-interpolation cont, nearest-rank disc — pinned against `cypher/funcs/aggregators.go`), replacing the former self-referential interval invariants a broken engine could still satisfy; sorted `collect(n.name)` equality; and a terminal non-vacuity assertion (≥ 2 cities, ≥ 2 distinct ages, ≥ 1 KNOWS target). Since rmp #2458 the battery also drives the ENTITY-valued functions, which the DST never called before: `labels(n)` per Person as a label SET (the ordering is unspecified), which gives `SET n:Label` / `REMOVE n:Label` an independent read-back of *which* labels a node carries; `properties(n)` as a key set plus a per-key canonical value, so the whole property map — not merely the individually projected keys — is enforced; `type(r)`, `startNode(r)` and `endNode(r)` over every KNOWS edge vs the oracle's modelled endpoints, evaluated after a `WITH` barrier so only the relationship (never the pattern's node variables) is in scope; and genuine path-materialisation content checks on `MATCH p=(a:Person)-[:KNOWS*1..3]->(b)` — `size(nodes(p)) = length(p)+1`, `size(relationships(p)) = length(p)`, the first node of `nodes(p)` identical to the anchor and the last identical to the matched endpoint (identity via `elementId`, not name), plus the per-(anchor, length) row histogram compared against an oracle TRAIL enumeration that encodes openCypher's relationship-isomorphism rule (a 2-cycle yields a length-2 path back to its start; a self-loop yields exactly one path). `elementId` is pinned to the contract the engine actually implements — the decimal rendering of the same integer `id()` returns, for nodes and relationships alike, with no database or generation prefix — and is asserted stable across two reads within one check, distinct across entities, and one row per modelled entity; stability is deliberately not asserted across ticks, since the battery also runs after crash/recovery. The three NON-deterministic functions carry honest invariants rather than constants: `rand()` draws lie in [0, 1) and are not all identical, `randomUUID()` values match the RFC 4122 version-4 shape and are pairwise distinct within one statement, and `timestamp()` is frozen within a statement (`cypher/stmt_now_reg.go` overrides it alongside the five temporal `now` constructors), lies in the epoch-millisecond window, and is non-decreasing across two statements. The graph-independent literal battery gained the remaining pure scalar surface — `left`/`right`/`ltrim`/`rtrim`/`replace`, `toBoolean` and the four `toXList` conversions, `isNaN`, the trig/log family with `pi`/`e`/`degrees`/`radians`, `sort`, and `size` on lists and strings — each as an absolute constant, plus the identity contract of the `extract`/`filter` STUBS (`cypher/funcs/list_funcs.go`) rather than a pretence that they implement comprehensions. `cot` and `haversin` are absent from the registry and are therefore not probed. Since rmp #2459 the battery also drives the MAP PROJECTION over a REAL node — the entity-property-resolution path the expression battery's literal-map probe (`WITH {a:1,b:2,c:3} AS m …`) cannot reach: `n{.name, .age}`, `n{.*}`, a selector naming a key no Person carries, and a selector mixing a property with a LITERAL entry, all four resolved in ONE scan and each compared against the oracle's modelled property map as a key SET plus per-key canonical values — never as a whole rendering, because `expr.MapValue` is a Go map and its `String()` key order is not stable. The missing-key contract is pinned as VERIFIED against this engine rather than assumed: the key is PRESENT and `NULL`, not omitted, so an engine that started omitting it fires. A result in which every projected map came back with an EMPTY key set is itself reported, so the probe cannot pass without having resolved a single entity property. Since rmp #2460 the battery also drives ORDERING and MULTI-PART query structure, which the simulator had only ever exercised as a single ASCENDING key on a string: `ORDER BY n.age DESC, n.name ASC` as full row-SEQUENCE equality against an oracle comparator (the workload draws ages from 0..99 over hundreds of Persons, so ages collide heavily and the ascending secondary key decides real ties); `ORDER BY n.name DESC`; an ordering on an EXPRESSION (`ORDER BY n.age % 10, n.name`, whose projected key value is asserted alongside the sequence) and one on an AGGREGATE (`RETURN n.city, count(*) AS c ORDER BY c DESC, n.city ASC`); and a Top-vs-Sort equivalence in which `ORDER BY … LIMIT k` must return exactly the first k rows of the unlimited ordering — compared against BOTH the oracle's prefix and the engine's own unlimited arm, and backed by an `Explain` assertion that the two arms really resolve through DIFFERENT physical operators (the fused `Top` versus the full `Sort`), so the equivalence cannot hold vacuously on one identical plan. Pagination is pinned on the same ordering: the same page written with LITERAL and with PARAMETERISED `SKIP`/`LIMIT` must equal the oracle's slice and each other (the literal/parameter parity theme of rmp #2447, here on the pagination clauses rather than on a predicate); `LIMIT 0` returns ZERO rows and a `SKIP` past the end returns ZERO rows — both VERIFIED against this engine in the literal and the parameterised spelling rather than assumed — and `SKIP`/`LIMIT` composed with a DESC ordering pages the REVERSED sequence. Multi-part structure adds the shapes a single `WITH` stage cannot reach: a two-stage pipeline (group and filter in the first `WITH`, then order, truncate and `collect` in the second), TOP-K THEN EXPAND (`WITH n ORDER BY n.age DESC, n.name ASC LIMIT 10 MATCH (n)-[:KNOWS]->(m)`), whose expected count the oracle predicts from the SAME top-k selection — the ordering is made TOTAL by the name tie-break so the expectation is well defined, rather than the assertion being weakened — and `WITH … WHERE` on an AGGREGATED value at the MEDIAN group size, so the predicate keeps some groups and drops others instead of passing them all. A terminal non-vacuity gate proves the sensitive arms were exercised: at least one age tie occurred, a DESC probe returned more than one row, the Top probe's LIMIT genuinely truncated (more rows were available than k), the aggregate filter both kept and dropped groups, and the expand stage counted at least one relationship. A model that cannot define a total order — a Person without a name or an integer age, or two Persons sharing a name — SKIPS the ordering probes rather than comparing against a reference that cannot be right, and the same gate then reports that no check ever ran, so a skipped run is never a silent pass. |
| `null-semantics` | deterministic | Non-degenerate NULL and three-valued-logic coverage (rmp #2453): the writer deliberately creates a seed-chosen fraction (~1/3) of AGELESS Persons (plus a cityless fraction), so both sides of every NULL distinction are populated — unlike `cypher-surface`, whose no-NULL guarantee pins `IS NULL` to the constant 0. The battery asserts, against the oracle's age-present vs age-absent partition: `count(n)` vs `count(n.age)` (aggregate NULL-skipping); `WHERE n.age IS NULL` / `IS NOT NULL` with both populations genuinely non-empty; `sum`/`min`/`max` plus the exact `avg`/`stDev`/`stDevP`/`percentileCont`/`percentileDisc` battery (the rmp #2452 helpers) referenced over the age-bearing subset only; OPTIONAL MATCH NULL-row padding as full row-set equality including the NULL markers (one row per outgoing KNOWS of each ageless Person, a `(name, null)` row when it has none, compared through the canonical value rendering); 3VL predicate arithmetic — `n.age > 30`, `NOT (n.age > 30)` (the NOT NULL = NULL trap), the excluded-middle disjunction, and the partition identity gt + not-gt + is-null = total asserted over the ENGINE-returned counts; and `coalesce(n.age, -1)` making the NULL replacement observable in a sum. The per-op counters oracle pins the ageless CREATE's exact effect set ({1 node, 1 label, 2 properties}), the battery runs periodically, after each crash/recovery, and at the end, and a terminal non-vacuity assertion proves both NULL populations, both 3VL arms, and both OPTIONAL MATCH row shapes (matched and NULL-padded) actually occurred. |
| `pattern-shapes` | deterministic | The join/intersect planner family the single-hop workloads leave dead: 2-hop composition, the cyclic directed-triangle shape (ExpandIntersect), undirected and reverse expansion, the `[:KNOWS\|FOLLOWS]` multi-type union, relationship uniqueness (`(a)-[r1:KNOWS]->(b)-[r2:KNOWS]->(a)` with the r1=r2 binding excluded per openCypher relationship isomorphism), and the bound-both-endpoints ExpandInto shape in literal and `$param` form. A writer builds a two-relationship-type Person graph (KNOWS + FOLLOWS) and plants deterministic motifs — a directed triangle, a mutual KNOWS pair, a KNOWS+FOLLOWS both-types pair, and a KNOWS self-loop — and every `count(*)` is verified against a reference computed independently by composing the oracle's adjacency (ordered edge pairs/triples with pairwise-distinct relationship identity; a self-loop matches an undirected pattern once), periodically, after each crash/recovery, and at the end, with a terminal non-vacuity assertion that every motif shape actually existed. The engine is opened as a multigraph (a simple graph cannot hold two edge types on one node pair) but the writer never re-CREATEs a `(src,dst,label)` edge, so edge identity stays exact. Since rmp #2463 the same scenario also adjudicates the **variable-length path surface**, which the simulator previously only issued as load: exact depth (`*2`, `*3`), the zero-length form `*0..n` (the length-0 binding is the identity — it binds both endpoints to the same node, still applies the far-side pattern to it, survives a relationship type that does not exist, and contributes length 0 / one node / zero relationships to the path functions), the lower-bound-only form `*2..`, the bounded `*1..3`, predicates over the path's intermediate nodes (`nodes(p)[1..-1]`) versus over all of them, multi-type `[:KNOWS\|FOLLOWS*1..2]`, undirected `-[:KNOWS*1..2]-`, and `nodes(p)`/`relationships(p)`/`length(p)` adjudicated both absolutely and for the per-row identity `size(nodes(p)) = length(p)+1 = size(relationships(p))+1`. Every reference is enumerated from the same oracle adjacency as TRAILS — edge-distinct walks, per the openCypher rule that a variable-length pattern must not repeat a relationship within one path — so the battery is sensitive to a Cyphermorphism regression. Enumeration is bounded twice: the whole-graph probes never look past the query's own upper bound (3 hops), and the unbounded `*2..` probe is anchored at a single Person under a depth cap and a walk budget, refusing to assert (rather than comparing a partial count) whenever either limit is reached. Its terminal non-vacuity gate requires the final model to hold enough multi-hop trails to be meaningful AND every arm to have done something: a depth-2 trail and a length-0 binding seen, the multi-type union strictly above the KNOWS-only count, the undirected count strictly above the directed one, the `*2..` probe run on trails deeper than two hops, and the intermediate-node predicate having strictly filtered. |
| `schema-mutation` | deterministic | The node mutation-clause surface on churning Persons: `REMOVE n.tag`, `REMOVE n:Vip`, `SET n:Vip`, `SET n += $map`, `SET n = $map`, plus — since rmp #2454 — the FOREACH write path with two genuinely different bodies: `FOREACH (x IN $list \| CREATE (:Person {name: x}))` and `MATCH … FOREACH (x IN $list \| SET n.tag = x)` over seed-chosen 1..4-element lists. Equivalence is structural: the oracle models each FOREACH as its EXPANSION (the equivalent batch of per-item single statements), and three independent read-backs adjudicate the engine against it — per-name property/label round-trip probes (`CheckSchemaMutation`, including the multi-label `(n:Person:Vip)` pattern), node/edge-count parity, and the per-op counters oracle (rmp #2448) pinning the exact effect set (an N-item FOREACH CREATE must report N nodes / N labels / N properties; a K-item FOREACH SET, K assignments; `FOREACH` over `[]` a committed all-zero no-op). All checks re-run immediately after every crash + checkpoint recovery, and a terminal assert-something-was-seen gate requires both FOREACH templates issued, a crash after a FOREACH op, and a post-recovery probe of a surviving FOREACH-created Person. Since rmp #2461 the scenario also drives the four MERGE families the DST previously left dead, each modelled exactly by the oracle and pinned by the counters oracle: a node MERGE with BOTH action branches (`ON CREATE SET n.mc = 1 ON MATCH SET n.mc = n.mc + 1` — create reports 1 node / 1 label / 2 properties, a match exactly 1 assignment, and a match on a Person whose `mc` a co-actor's `SET n = $map` has wiped reports the ALL-ZERO set, because `null + 1` is null and assigning null removes an absent property); the whole-map `ON CREATE SET n = $map`, whose replace CLEARS the merge key the pattern itself just wrote (1+len(map) properties set, exactly 1 removed) and is therefore NON-IDEMPOTENT when the map omits `name` — the workload always binds it, and `TestMergeSurface_SetAllReplacesMergeKey` pins the destructive variant separately; a whole-pattern `MERGE (a:Person {name:$a})-[r:PAIRED]->(b:Person {name:$b})`, which is ALL-OR-NOTHING — either the whole pattern matches (all-zero) or the whole pattern is created as two FRESH nodes and one relationship (2/1/2/2) even when an endpoint key already exists, so the family runs in its own `wp*` key namespace kept out of the oracle name index while node/edge-count parity and the endpoint-name edge probes still reach it; and `MERGE (n $map)`, which the engine must REJECT at compile time (openCypher TCK `Merge1` scenario [16]), driven as an `OpMalformed` op whose acceptance would fail `checkMergeRejection` on the tick that caused it. `mc` is projected by `CheckSchemaMutation` on every Person, so the ON CREATE/ON MATCH counter value round-trips continuously and survives WAL and snapshot recovery, and a second terminal gate requires all four families issued, both counter branches and the whole-map create branch fired, at least three of the four whole-pattern sub-cases reached, and a post-recovery probe of a surviving counter-MERGE Person. |
| `search-crash` | deterministic | The `search/` battery validated on the crash + recovery-survived graph. |
| `mem-pressure` | deterministic | Over-budget reads (large `UNWIND`, Cartesian, whole-graph `collect`) against clamped logical-resource budgets (`MaxResultRows`/`MaxCollectItems`). Asserts bounded-resource graceful degradation: each over-budget read is refused with a typed error and changes no state, so engine and oracle stay in lock-step and the honest writes still commit — no panic, no partial result, no wedge. A soak-gated companion (`TestMemPressure_Soak`) imposes a real heap ceiling via `debug.SetMemoryLimit` and drives an overload-heavy concurrent wire workload, asserting the same degrade-never-panic contract under genuine GC pressure. |
| `bad-actors` | deterministic | 100% malformed/abuse workload; every op rejected with a typed error, no state change. |
| `overload` | concurrent | Giant transactions / huge `UNWIND` / large result sets / deep variable-length expansion; bounded-resource graceful degradation. |
| `cpu-starvation` | liveness | A compute-hog workload (60% overload) competing with honest queries on a single clamped `GOMAXPROCS` core, then a liveness convergence assertion. Verifies fair scheduling under CPU starvation: the system keeps making forward progress (no deadlock/livelock — the watchdog classifies a stuck run as resonance), no panic, no goroutine leak. Latency percentiles are deliberately not asserted (statistical). |
| `bulk-vs-online` | bulk-vs-online | A concurrent offline bulk CSR load alongside transactional online writes; resource stability. |
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
that combines all of it over the durable store.

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
  cross-check equivalent engine configurations, WAL data-compatibility across
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
