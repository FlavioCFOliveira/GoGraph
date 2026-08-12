# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **`lpg.Session` — read-your-own-writes across transactions** (rmp #2328). The commit
  frontier is contiguous, so `ApplyVersioned` returns at an instant that has not yet
  published and a caller's next transaction could begin *below its own commit*.
  Measured with twelve writers on disjoint keys: **9 of 660** read-backs missed a commit
  the client was told had succeeded, and **6 of 6** serialization conflicts were a
  transaction conflicting with itself on a key nothing else touched. A `Session`
  obtained from `Graph.NewSession` records the instant it committed at and waits for the
  frontier to reach it before its next operation. Within one session every operation
  observes every commit that session has made; across two sessions nothing is promised
  beyond snapshot isolation — the same contract a connection gives in any client-server
  database. A writer that does not immediately read never waits.
  **The Cypher and Bolt layers do not carry a session yet**, so clients driving GoGraph
  through them still get the sessionless contract; see `docs/isolation-design.md`.
- **MVCC observability** (rmp #2312). Writers in flight, commits, aborts, serialization
  conflicts by store, the retained version-chain depth *distribution*, vacuum pass
  latency, horizon utilisation and commit-latency histograms are published through the
  metrics backend. The full inventory is in `docs/metrics.md`, which had no MVCC section
  before. Cost measured: the read path is unchanged and a write transaction pays two
  atomic increments — 1.42 ns on an otherwise empty bracket, unmeasurable on one that
  writes.

### Fixed

- **Bulk delete degraded without bound and used one core** (rmp #2400, #2418). Six
  seed-and-wipe cycles of the *same* 20 000 nodes against one live engine took 990 ms,
  1.771 s, 2.349 s, 3.061 s, 3.841 s, 4.622 s, and deleting 90 000 nodes in a single
  statement took 15.97 s against a 30 s transaction timeout — reachable as a
  `TransactionTimedOut` failure, not merely as slowness. `InNeighbours` answered "which
  nodes hold an edge into n" by walking **every interned node**, once per node deleted,
  so a delete cost O(k·n) with *n* counting every node ever interned rather than the
  live ones. The adjacency now maintains a live in-edge index and answers in
  O(in-degree): **cycle six falls from 4.622 s to 77.2 ms**, the curve is flat instead
  of linear, `DETACH DELETE` improves 22.2×, and the 90 000-node statement takes
  375.6 ms. End-to-end relationship creation costs +1.92% for it; the read path is
  unchanged. The 2026-08-11 concurrency assessment had attributed this to the tombstone
  bitmap's copy-on-write clone, which a profile prices at 0.99%; that document is
  corrected in place. Evidence in
  `docs/benchmarks/delete-in-edge-index-2026-08-11.md`.

- **A node created by an edge append was visible before its transaction committed**
  (rmp #2331). `adjlist.addEdge` interned its endpoints without recording a versioned
  birth, so a node an append *created* was visible to snapshots predating the
  transaction while the arc itself correctly waited — one transaction becoming visible
  in two pieces. All three edge paths are fixed, including the two handle-bearing ones
  that every durable write goes through.

- **CRITICAL: concurrent read-modify-write lost 46% of its committed updates**
  (rmp #2324). Four writers each issuing 100 autocommit `SET a.bal = a.bal + 1`
  statements, with every refusal retried until it succeeded, reported 400 successes
  and left the property at **216**. Measured directly rather than inferred: those 400
  successes wrote only ~200 **distinct** values, with one value written by five
  different statements.

  The write-write conflict test sat behind a "records nothing" guard — skipped when
  the value being written already equalled the **stored** value, on the reasoning
  that a write recording no version has nothing to conflict over. That is sound for
  an idempotent write and unsound for an arithmetic one, because the incoming value
  can equal the stored one **by coincidence**: A reads 1 and writes 2; B, whose
  snapshot also says 1, computes 2 as well; B's write is compared against the
  now-stored 2, judged a no-op, and accepted with no conflict test at all. B's
  statement reports success having applied nothing.

  The conflict test now runs unconditionally, before deciding whether anything needs
  recording. This cannot cause the spurious abort the guard was added to prevent: a
  `MERGE` re-asserting a property it just read re-asserts over a version that IS
  visible to its own transaction, and a version that is NOT visible means another
  transaction changed the object, which must be refused. Node **labels** keep their
  equivalent guard and are sound with it, because set membership is genuinely
  idempotent — adding a label another transaction already added leaves the same state,
  with no arithmetic to lose.

  The defect predates the MVCC sprint and was invisible to a fully green suite: the
  one test covering this shape asserted the defective behaviour, and
  `examples/27_concurrent_txn`'s conserved-total oracle passed because its default
  contention rarely hit the window.


### Changed

- **`bolt/server.DefaultMaxOpenTxPerPrincipal` raised from 16 to 2048** (rmp #2419).
  An observable change of default. This module publishes 1, 8, 64, 256 and 1024
  goroutines as the concurrency levels it measures and reports at, and a single
  principal could not reach them through explicit transactions without overriding this
  default first — every benchmark harness in the repository had already had to. The
  default configuration now reaches the published levels. Note the consequence: a
  connection holds at most one open transaction and `MaxConnections` defaults to 1024,
  so under the default configuration this quota can no longer bind, and the connection
  ceiling is what bounds the resource. The quota stays finite, still refuses with a
  typed `LimitExceeded`, and still isolates one principal from another; an embedder who
  wants the previous tight bound must now set `Options.MaxOpenTxPerPrincipal`
  explicitly. **Every open transaction pins an MVCC read snapshot and holds the
  reclamation horizon back for its lifetime**, so the higher ceiling is a weaker bound
  on that resource; `DefaultMaxTxIdleTime` (5 s) is what limits the exposure.

- **BREAKING: `Graph.DisableMVCC`, `Graph.EnableMVCC` and `Graph.MVCCEnabled` are
  removed** (rmp #2311). MVCC is the module's only concurrency-control mechanism and is
  armed by `lpg.New`; there is no way to disarm it. An exported switch implied a choice
  that does not exist, and a disarmed graph had no snapshot isolation, so every
  guarantee the module documents was conditional on a setter any caller could reach.
- **BREAKING: `MVCCStats` field renames** (rmp #2312). `ActiveReaders` →
  `ActiveSnapshots` (it counted writers too, since writers hold a snapshot),
  `OldestReaderAge()` → `OldestSnapshotAge()`, `UnregisteredReaders` →
  `UnregisteredSnapshots`. `ActiveReaders()` is now a derived method excluding writers.
  The metric series `lpg.mvcc.readers.unregistered` and `lpg.mvcc.oldest_reader_age` are
  renamed to `lpg.mvcc.snapshots.unregistered` and `lpg.mvcc.oldest_snapshot_age`, and
  per-store conflict series use underscored store names.
- **BREAKING: `AdjList.Reclaim` takes a `*mvcc.DepthHist`** (rmp #2312), so the
  reclaimer can report retained chain depth from the walk it already performs.

- **An open explicit write transaction no longer blocks anybody** (rmp #2305).
  `cypher.Engine.BeginTx` took the graph's visibility barrier EXCLUSIVELY and held it
  from `BEGIN` until `COMMIT`/`ROLLBACK` — across every client round-trip and all the
  think-time between them. Over Bolt that meant one client which sent `BEGIN` and then
  paused blocked **every other writer in the process** for as long as its transaction
  stayed open. That hold is gone.

  `BeginTx` now opens one **commit record** and takes no lock. Each statement takes the
  schema barrier *shared* for its own duration and releases it before returning, so
  nothing is held between statements. `COMMIT` publishes the record exactly once, and
  that single publication is the transaction's commit instant: every version its
  statements wrote becomes visible together, and a rolled-back transaction's versions
  never become visible at all. Atomicity comes from the record, not from exclusion.

  **Two clients may now hold open write transactions simultaneously and both make
  progress**, verified end-to-end against the official neo4j-go-driver.

  **A write-write collision between two open transactions is refused at the
  conflicting statement** — `ExplicitTx.Exec` returns an error wrapping
  `mvcc.ErrSerializationConflict` — rather than at `COMMIT`. Detection is
  first-updater-wins on the version chain, so the loser is known the moment it tries to
  install its version; there is nothing to defer. Over Bolt the error maps to a
  `TransientError`, so the driver's managed transactions retry it. Callers driving
  explicit transactions by hand should roll back and retry.

  **New `lpg` API**: `Graph.BeginVersionedTx`, `Graph.ApplyInVersionedTx` and
  `Graph.EndVersionedTx`. `Graph.LockBarrier`/`UnlockBarrier`/`ApplyInsideLocked` remain
  for callers that genuinely need exclusive access, but they must not be used to hold a
  transaction: `ApplyInsideLockedTx` resolves the transaction from the graph's *ambient*
  slot, which two concurrent transactions overwrite.

  **An abandoned transaction still costs something, but of a different kind:** it pins
  the reclamation horizon, so no version it could read is freed while it lives.
  `server.Options.MaxTxIdleTime` was reviewed for this and kept at its 5 s default —
  the original availability justification is gone, but an unbounded resource cost
  remains.

- **Concurrency control is now MVCC and nothing else** (rmp #2306). The
  `txn.Store`'s capacity-one single-writer semaphore is retired. Independent write
  transactions run concurrently on both engine wirings, and a write-write
  collision is DETECTED at commit by first-updater-wins on the version chain
  rather than prevented by holding a lock. `txn.Store.Begin`/`BeginCtx` now only
  register the transaction as an admitted writer; the registration excludes no
  other writer.

  **Observable behaviour change.** With nothing serialising writers, two
  concurrent `MERGE` statements on the same pattern can both find no match and
  both create — measured at eight duplicates from eight writers. This is inherent
  to MVCC (two CREATEs of two distinct new nodes are not a conflict, so there is
  nothing to arbitrate) and matches Neo4j, which requires a uniqueness constraint
  for the same reason. **With a `UNIQUE` constraint on the merged property the
  duplicates collapse to exactly one**, because the constraint's reservation is
  atomic. Callers relying on MERGE to be idempotent under concurrency must declare
  the constraint.

  Retiring the semaphore is **throughput-neutral** — the premise that it was a
  bottleneck is refuted by measurement (all p > 0.18 at n=6; both arms scale ~15×
  at 32 writers), because it was released after the WAL append and never covered
  the coalesced fsync that dominates a durable commit. Full A/B in
  [docs/benchmarks/store-semaphore-retirement-2026-08-04.md](docs/benchmarks/store-semaphore-retirement-2026-08-04.md).

  Quiesce (`txn.Store.RunUnderCommitLock`, used by `store.DB.Close` and the
  checkpointer) no longer borrows the semaphore. It closes a dedicated admission
  gate and drains the admitted writers to zero, both under one mutex, so a writer
  cannot be admitted between the two steps. Two quiesces exclude each other
  explicitly rather than as a side effect.

### Fixed

- **Durability — a commit whose frames were already on stable storage could be
  reported as FAILED** (rmp #2322). `wal.Writer.SyncGroup` consulted the writer's
  sticky durability failure before testing whether the caller's own frames were
  durable. The two are not mutually exclusive: the failure can belong to a *later*
  group round that failed after a leader had already fsynced this caller's commit
  marker. Such a committer was told its commit was lost, while recovery correctly
  replayed the durable, fully marked transaction — a durable transaction nobody
  acknowledged, which the crash simulator's atomicity oracle reported as
  `<failed-resurrected>`.

  `SyncGroup` now tests the caller's watermark against the durable size first, and
  takes that watermark as a parameter instead of inferring it. **This is a breaking
  change to two `store/wal` signatures**: `Writer.AppendRun` returns
  `(int64, error)` — the run's own end offset — and `Writer.SyncGroup(target int64)`
  takes it. The offset could not be recovered after the fact, because the writer's
  accepted offset is shared: another appender advances it and a durability failure
  rewinds it, so a committer reading it later was asking about somebody else's
  frames. `Writer.SyncBuffered` is added for callers with nothing of their own to
  acknowledge (an empty commit's courtesy flush) and must not be used to decide a
  commit's fate.

  The defect was latent while the store serialised commits behind its
  single-writer semaphore, because group rounds could not overlap; it is a
  prerequisite for retiring that semaphore.

### Changed

- **Isolation — an explicit read transaction is now SNAPSHOT ISOLATED across all
  of its statements** (rmp #2307). `cypher.Engine.BeginReadTx` pins one MVCC read
  instant at `BEGIN`, registers it with the reclamation horizon, and routes every
  `Exec` on the handle at that instant, so a commit made by another transaction
  between two statements is invisible to the second. A Bolt `BEGIN` with
  `mode="r"` inherits this.

  **This is an observable behaviour change, and it is strictly stronger.**
  Previously each statement opened its own snapshot — read-committed across the
  statements of the transaction — which made an explicit read transaction weaker
  than a single autocommit statement, since an autocommit statement has always
  had one instant for its whole duration. Transaction-wide snapshot isolation is
  Memgraph's default and is stronger than Neo4j's documented multi-statement
  read-transaction behaviour, so no caller relying on the old contract can
  observe a *weaker* result. Code that depended on seeing another transaction's
  commit mid-read-transaction must now open a new read transaction to see it.

  The cost is explicit: an open read transaction pins the reclamation horizon, so
  no version it can still reach is freed while it lives.
  `lpg.MVCCStats.ActiveReaders` and `MVCCStats.OldestReaderAge()` report it, and
  an abandoned Bolt handle is bounded by the existing idle reaper
  (`Options.MaxTxIdleTime`) and total transaction timeout
  (`Options.DefaultTxTimeout`), both of which roll the transaction back and so
  return the horizon slot. Write transactions are unchanged: they reach the same
  guarantee by holding the visibility barrier exclusively from `BEGIN` to
  `COMMIT`.

  openCypher TCK unchanged at its full baseline. See
  [`docs/isolation-design.md`](docs/isolation-design.md).

### Added

- **`bench/mvccwrite` — the write-scaling harness and its regression gates**
  (rmp #2297). Measures engine write throughput against writer count in two
  wirings — store-less (the concurrency-control ceiling) and WAL-backed (where
  group commit can pay) — and gates it in the short test layer so a change that
  re-serialises the writers turns the local CI gate red. Entry baseline and the
  full rationale in
  [`docs/benchmarks/mvcc-write-scaling-2026-08-02.md`](docs/benchmarks/mvcc-write-scaling-2026-08-02.md).

## [0.10.0] — 2026-07-25

The thirteenth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release, and it is entirely a **Cypher query-planner and execution-engine**
cycle. Its headline is a new **planner statistics and cardinality-estimation
foundation** — an exact relationship **count-store** (`E(relType)`,
`D(label, relType, dir)`, `T(labelA, relType, labelB)`) maintained in
`O(delta)` on the commit fan-out and recomputed at reopen, plus off-write-path
best-effort statistics (HyperLogLog NDV, exact top-k MCV, equi-depth
histograms) — which now drive **statistics-backed cardinality estimates in
`EXPLAIN` / `PROFILE`**. On top of the exact count-store sit two families of
result-identical, cost-gated optimisations: **count-store-gated reordering
peepholes** (a min-cardinality multi-label anchor scan, a single-edge
OUT-only anchor-swap, and a disjoint-scan-component reorder, all guarded by an
order-safety predicate) and a **deepening of the columnar / vectorised read
path** (columnar aggregation over typed chunk columns, `Expand` as a
`ChunkProducer`, and a columnar hash-join with late materialisation).
Finally, **automatic intra-query parallelism** is broadened from scan/count to
**parallel `min` / `max` / `count` aggregation** with a combine that is
byte-identical to the serial fold, plus an `O(1)` count-pushdown.

The **45 commits** landed since `v0.9.0` (`git log --no-merges v0.9.0..HEAD`,
a linear history with no merge commits) were surveyed for the bump. The
release is **purely additive**: two net-new packages (`graph/index/count`,
`graph/index/stats`), net-new `cypher` / `cypher/exec` planner and columnar
operators, and net-new `EXPLAIN` / `PROFILE` observability. **No exported
identifier was removed or renamed anywhere**, and there is **no breaking
change to the documented public-API surface** (`graph/*`, `search/*`,
`store/*`, `ds`, `bench/*` — see [docs/semver.md](docs/semver.md)); this is a
clean **MINOR**.

Both compliance invariants continue to hold without regression: the module is
**100 % openCypher TCK-compliant at the execution level** (3 897 / 3 897
scenarios, 16 006 / 16 006 steps, 0 failed / 0 undefined) and **100 %
ACID-compliant**. Every new optimisation is either result-identical by
construction (the columnar operators, the count-pushdown), gated so it fires
only when provably result-identical and never slower (the reordering
peepholes), display-only (the cardinality estimates), or a byte-identical-to-
serial parallel combine — so `tckExecutionBaseline = 3897` in
`cypher/tck/runner_test.go` is unchanged, and the whole TCK was additionally
forced through the parallel path (threshold = 1) with 3 897 / 3 897 still
passing. The count-store and statistics carry **no on-disk format, no WAL op,
and no checkpoint component** — they are pure functions of the recovered
graph — so no durability contract changes. The Go toolchain remains
**go1.26.5** (unchanged), and `govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.10.0
```

### Added

- **Exact relationship count-store.** A derived, non-durable, engine-owned
  count-store maintains `E(relType)`, `D(label, relType, dir)` and
  `T(labelA, relType, labelB)` **exactly** (reusing the label index for
  `N(label)`), updated `O(delta)` on the commit fan-out via a `CountBuffer`
  flushed under the `visMu` barrier after the WAL fsync, and recomputed
  `O(V + E)` from the recovered graph at reopen — so it has no on-disk format,
  no WAL op and no checkpoint component. A node relabel on a high-degree node
  keeps the enumerable OUT side exact within a bounded per-commit budget and
  marks the un-enumerable IN side stale (self-healing at reopen); a stale cell
  yields an estimate veto, never a wrong exact. Observable via
  `internal/metrics` counters
  (`cypher.countstore.{delta.applied,lookup,lookup.veto,relabel.dirtied}`),
  the `cypher.countstore.recompute` latency, and `Engine.CountStoreCells()`.
  (#2082, #2083, #2084, #2087)
- **Planner statistics foundation.** Off-write-path, best-effort statistics
  built by an explicit `RefreshStatistics` scan (`graph/index/stats`):
  **HyperLogLog NDV** (`m = 4096`, ≈ 1.6 % relative error), **exact top-k
  MCV**, and **equi-depth histograms** (`B = 256`, distribution-free
  ≤ `1/B` selectivity error, with MCV heavy-value spikes isolated). NDV is
  never sampled (provably impossible to bound — Charikar et al. PODS'00). The
  collector is lazy — nil until `RefreshStatistics` — so a statistics-free
  engine constructs nothing and the write path is byte-identical to
  pre-statistics; with statistics active, maintenance is an `O(1)` atomic
  delta per tracked property. (#2097, #2098, #2101)
- **Statistics-backed cardinality estimates in `EXPLAIN` / `PROFILE`.** Each
  operator is annotated with an estimated row count and its **provenance**
  (`exact` / `stats` / `heuristic`), drawn from the exact count-store
  (`N`, `E`, `D`) and the new statistics. This is **display-only** — a
  differential test proves query results are identical with and without
  statistics populated — and is exposed alongside planner statistics
  observability metrics. (#2099, #2102)
- **Count-store-gated reordering peepholes.** Result-identical, build-time
  plan rewrites, each gated by the exact count-store and an order-safety
  predicate (`SuppressReorder`) so they deviate from the written order only
  when provably result-identical and never slower:
  - **Min-cardinality multi-label anchor scan** — a `MATCH (n:A:B:C)` scans
    the label with the smallest **exact** bitmap cardinality and keeps the
    rest as a residual `Filter` (a label conjunction is a commutative `AND`
    and `min|Lᵢ| ≤ |L0|`, so the plan never does more work). (#2077)
  - **Single-edge anchor-swap (OUT-only)** — for `(a:A)-[:R]->(b:B)`, anchor
    on the endpoint that minimises examined edges via the count-store degree
    `D(label, relType, dir)`; reverse-introducing swaps are vetoed. (#2089,
    #2090)
  - **Disjoint scan-component reorder** — for a nested-loop join of disjoint
    single-scan components, build the smaller exact node count as the outer
    side. (#2091)
  - **Order-safety predicate** — a shared `SuppressReorder` guard vetoes any
    reorder that a bare `LIMIT` / `SKIP`, arrival-order aggregation, or
    `collect()` would make observable. (#2092)
  `EXPLAIN` / `PROFILE` now render the chosen scan label and expand direction
  so the reorder is visible; every peephole is gated by an
  `EngineOptions.Disable*` flag and proven byte-identical ON vs OFF by a
  differential harness. (#2076, #2079, #2091, #2094)
- **Columnar / vectorised execution deepening.** The column-major `Chunk`
  runtime (introduced in v0.9.0) is extended from projection to the operators
  that dominate analytic Cypher, each a drop-in with a row-mode fallback,
  wired only for its qualifying shape, and differential-tested byte-identical
  columnar-ON vs row-OFF: **columnar aggregation** over typed chunk columns
  (SoA scatter-add), **`Expand` as a `ChunkProducer`** with a columnar filter
  over the traversal output, and a **columnar hash-join with late
  materialisation**. The chunk pipeline now stays unboxed end-to-end through
  scan → expand → filter → aggregation. (#2104, #2105, #2106)
- **Automatic parallel `min` / `max` / `count` aggregation and `O(1)`
  count-pushdown.** Automatic (no-licence, no opt-in) morsel-parallelism is
  broadened from scan/count to `min` / `max` aggregation with a
  **position-carrying combine byte-identical to the serial left-fold** for
  every tie representation (int/float, ±0, `NaN`), engaging above
  `parallelScanThreshold` and bounded by the `ParallelGovernor`. A
  `budget == 1` inline-serial short-circuit keeps the saturated regime
  regression-free, and a bare group-by-less `count(*)` / `count(v)` over a
  full scan is pushed down to an `O(1)` maintained-count read. (#2111, #2113,
  #2115)

### Changed

- **`EXPLAIN` / `PROFILE` output is richer.** Operators now display an
  estimated row count with its provenance, the chosen scan label for a
  reordered multi-label or disjoint pattern, and the chosen expand direction
  for an anchor-swap. This is additive observability; the executed plan for a
  query the optimiser declines is unchanged.
- **The example harnesses exercise the new engine surface.** Example
  `25_software_house_api` exercises the min-label anchor scan;
  `31_metrics_observability` observes the count-store; and
  `26_social_scale_bench` gains cardinality-estimate, columnar-operator and
  intra-query-parallelism observations, plus a `plandiff` subcommand that
  surfaces reordering via `EXPLAIN` plan-diff. The `examples/` tree is not
  part of the module and imposes no new dependency on it. (#2117, #2118,
  #2119, #2120, #2121, #2122)

### Fixed

- **Example measurement fidelity.** Per-algorithm wall-clocks in the example
  harnesses now time only the algorithm, not GC or print time (#2071, #2072,
  #2073); `13_network_reliability` times GoGraph's own library max-flow rather
  than a bespoke solver (#2074); and the `17_transactional_log` double-entry
  check is now a real reconciliation invariant rather than a tautology
  (#2075). These are example-only fixes; the module itself is unchanged.

### Removed

- Nothing. This release removes no exported identifier and no behaviour.

## [0.9.0] — 2026-07-19

The twelfth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is a **columnar Cypher read path** and two net-new,
backward-compatible Cypher clauses — the **`FOREACH`** updating clause and
**`SHOW CONSTRAINTS` / `SHOW INDEXES`** schema introspection with
`YIELD` / `WHERE` / `RETURN` projection — landed alongside a broad Cypher
correctness pass and a two-cycle whole-module production-readiness review.
The 57 commits since `v0.8.1` (`git log --no-merges v0.8.1..HEAD`, a linear
history with no merge commits) were surveyed for the bump: net-new Cypher
surface and additive `cypher/exec` types make this a **MINOR**, and **no
breaking change to the documented public-API surface** (`graph/*`,
`search/*`, `store/*`, `ds`, `bench/*` — see [docs/semver.md](docs/semver.md))
was found. One dead, never-wired exported operator in `cypher/exec`
(`ParallelScan`) was removed; pre-1.0 the minor digit absorbs such a
change, and it is called out under **Removed** below.

Both compliance invariants continue to hold without regression: the module
is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps, 0 failed / 0 undefined) and
**100 % ACID-compliant**. The new Cypher clauses are TCK-neutral —
`FOREACH` and the `SHOW` DDL-introspection commands are not covered by a TCK
scenario — so `tckExecutionBaseline = 3897` in `cypher/tck/runner_test.go`
is unchanged. The Go toolchain remains **go1.26.5** (unchanged), and
`govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.9.0
```

### Added

- **`FOREACH` updating clause.** `FOREACH (x IN list | <updating clauses>)`
  binds `x` to each element of a list and runs the body clauses as
  side-effects, preserving the surrounding query's row cardinality. It
  supports `CREATE` / `MERGE` / `SET` / `REMOVE` / `DELETE` and nested
  `FOREACH`, over a literal list, a bound list variable, or a list-valued
  expression. Lowered to a correlated body sub-plan
  (`Argument → Unwind(list) → updating clauses`) driven once per outer row.
  `FOREACH` is not TCK-covered, so the 3 897 baseline is unaffected. (#2029)
- **`SHOW CONSTRAINTS` / `SHOW INDEXES` schema introspection**, with the
  singular `SHOW CONSTRAINT` / `SHOW INDEX` aliases, listing the registered
  schema in the openCypher column order. A trailing **`YIELD` / `WHERE` /
  `RETURN`** projection is accepted so modern clients can select, filter,
  and rename the yielded columns. (#2044)
- **Column-major `Chunk` execution model and columnar operators** in
  `cypher/exec` — a new additive exported surface (`Chunk`, `ChunkPool`,
  `ColumnarFilter`, `ColumnarProject`, `LabelCountScan`, the
  `ChunkProducer` / `NodeIDColumnProducer` interfaces, and supporting
  filler/predicate function types) that underpins the read-path performance
  work below. (#1704 P1)

### Changed

- **Exact cross-type integer/float equality per openCypher CIP2016-06-14.**
  Equality, grouping, `DISTINCT`, hash joins, and list/map element
  comparison now compare an integer and a float **exactly** rather than by
  lossy `float64` promotion: `1 = 1.0` and they group together, `NaN`
  groups with `NaN`, `-0.0` with `+0.0`, and `NULL` grouping keys collapse
  to one group — while two distinct integers `≥ 2^53` that share a
  `float64` bit-pattern now stay **distinct** (they previously conflated).
  (#2050)
- **`AND` / `OR` / `XOR` / `NOT` reject a non-boolean runtime operand** with
  a typed `TypeError` instead of silently coercing it, matching openCypher
  three-valued-logic semantics (`NULL` operands are still handled by 3VL).
  (#2059)
- **Query planner prefers the columnar chain over the morsel-parallel scan**
  for `MATCH (n) WHERE <simple predicate> RETURN <scalar properties>`. The
  boxed parallel operator ran serial-but-boxed under the concurrency
  governor; the de-boxed columnar chain is now tried first for the eligible
  shape. `ParallelScanProject` still handles the shapes the columnar chain
  declines (no filter, compound predicates, non-property scalar items).
  (#2065)
- **Bolt streams all-scalar `RECORD` rows through a typed encoder** with no
  per-cell boxing to `expr.Value`.

### Deprecated

- **`search.KShortestPathsLoopless` (the unbounded bare entry).** Its
  worst case is super-polynomial in the number of vertices and it cannot
  signal truncation, so it is a foot-gun on arbitrary or untrusted input.
  It is marked `// Deprecated:` — no default bound is imposed, preserving
  behaviour — steering callers to the bounded, cancellable
  `KShortestPathsLooplessCtxWithOpts`
  (`MaxPops` / `MaxQueueBytes → ErrResourceBudgetExceeded`) or the
  polynomial `YenKShortest`. (#1997, #2006)

### Removed

- **The dead `cypher/exec.ParallelScan` operator** (its `NewParallelScan`
  constructor and `Init` / `Next` / `Close` methods). It had no planner
  call site — the live parallel leaves are `ParallelScanProject` and
  `ParallelCountScan` — and it sized workers directly from `GOMAXPROCS`,
  bypassing the `ParallelGovernor`. Removing it is a change to an exported
  identifier outside `internal/`; pre-1.0 the minor digit absorbs it, and
  the documented public-API surface in [docs/semver.md](docs/semver.md) is
  unaffected. No production behaviour changed. (#2019)

### Fixed

- **`MERGE` and pattern matching over parallel relationships.** `MERGE`
  fans its bindings out per pre-existing parallel relationship (#2033);
  `DELETE r` removes the exact bound parallel-edge instance (#2034); an
  untyped `[r]` is enumerated per parallel instance in `EvalPatternComp`
  (#2035); and a fresh same-pattern node reference is resolved in `MERGE`
  inline properties (#2032).
- **Whole-entity `SET` correctness.** Non-literal values in whole-entity
  `SET n = {…}` maps and in `MERGE` inline relationship properties are now
  evaluated (#2031, #2030); a whole-entity `SET` map is written from a bound
  map variable (#2030); whole-entity `SET` is applied in `MERGE ON CREATE` /
  `ON MATCH` (#2027); a `SET n = <map-returning expression>` is honoured;
  and a whole-entity `SET` that violates a `UNIQUE` constraint is rolled
  back instead of silently skipped (#2026).
- **`setItem` disambiguation.** `WITH … SET var = <map/param>` no longer
  panics; the parser distinguishes a property-set from a whole-entity map
  assignment. (#2036)
- **String-literal round-trip corruption.** Backslashes in a string literal
  are escaped on the round-trip, stopping silent property corruption.
  (#2025)
- **`MERGE` read-own-writes on a null property.** A `MERGE` whose property
  resolves to a runtime `NULL` (including from a null parameter) now raises
  `MergeReadOwnWrites` instead of proceeding. (#2024, #2023)
- **`CALL … YIELD … WHERE` applies its `WHERE` predicate** to the yielded
  rows. (#1966)
- **A leading `FOREACH` no longer panics** — `Foreach.Vars` guards a nil
  outer scope. (#2029)
- **Inline property validation.** A node/relationship-valued inline property
  is rejected, and a parameter may be supplied as a whole property map on
  `CREATE`. (#2022, #2021)
- **`CREATE INDEX` on an empty graph** no longer pins the index key type to
  `String`. (#2020)
- **B-tree correctness.** `contains` recurses and `removeChild` escalation
  re-roots correctly, stopping a height ≥ 4 corruption. (a0a5be6)
- **Search correctness.** `TransitiveClosure.Reachable` is reflexive for
  in-range `NodeID`s (db4b3ed); `NaN` / `Inf` is validated for defined
  float weight types via the reflect kind (b14fc89).
- **I/O round-trips.** A CSV cell that begins with the comment rune is
  force-quoted so it round-trips (bab1b19); JSONL distinguishes an absent
  field from an empty one so an empty-string identifier round-trips
  (ea15576).
- **A `cypher.Engine` constructed over an undirected backend now warns**,
  surfacing a previously silent misconfiguration. (baef19e)

### Security

- **Bolt inbound-budget accounting.** The message-reassembly buffer is now
  charged against the aggregate inbound budget, closing a memory-amplification
  gap in the chunked-message reader. (1c9b079)
- **Two-cycle whole-module production-readiness review.** The 2026-07-16/17
  and 2026-07-19 multi-specialist audits (recorded in
  [docs/audit-production-readiness-2026-07-19.md](docs/audit-production-readiness-2026-07-19.md))
  found no critical or high-severity defect and certified both compliance
  mandates; the two highest-value findings (the `AND`/`OR` non-boolean
  coercion above and the columnar-first planner reorder) were remediated in
  this release. `go test -race ./...`, the crash-injection battery, and
  `govulncheck ./...` are clean.

### Performance

- **Columnar read path (`#1704` P1–P3).** A `RETURN n.key` projection over a
  filtered node scan is now driven column-major, boxing to `expr.Value` only
  at the sink. `BenchmarkEngReadProject` drops from **5 309 to 89
  allocs/op** end-to-end (benchstat n=6: P2 −32.8 %, P3 a further −97.5 %),
  and query time from **≈ 234 µs to ≈ 53.8 µs**. Correctness is
  byte-identical by construction (the column filler classifies each value
  with the same functions the row path uses). (#1822, #1823, #1824)
- **Columnar `WITH`-projection passthrough.** `BenchmarkWithFilterPassthrough`
  drops from **5 324 to 118 allocs/op** (−97.8 %, −76 % time) and
  `WithNoFilterPassthrough` from 3 553 to 100 (−97.2 %, −71 % time). (#2045)
- **Unboxed columnar grouping-key hashing in `EagerAggregation`.**
  `BenchmarkAggGroupScalar` drops from **15 598 to 1 889 allocs/op**
  (−87.9 %, −78 % time) while preserving openCypher grouping equivalence.
  (#2049)
- **Planner columnar-first reorder.** For the filtered scalar-projection
  shape, the same query drops from **182 559 to 118 allocs/op** and
  **6.08 ms to 1.94 ms** under concurrency (≈ 1 550× fewer allocations,
  2–3× faster), with no single-query regression. (#2065)
- **`count(*)` over a bare label scan is pushed down to an O(1) index read.**
  (8134796)
- **Lock-free tombstone read path** in `graph/lpg` restores read scaling
  under concurrency. (95da80d)
- **Struct field re-alignment.** Fields of non-test structs are reordered to
  the size-optimal layout, removing compiler padding across 33 struct types
  and trimming ≈ 7.5 KB of operator allocation size and garbage-collector
  mark work. (a5d0fe7)
- **`buildEdgeTypeFilter` streams its fallback label check** instead of
  allocating a per-edge slice. (4273033)

[0.9.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.9.0

## [0.8.1] — 2026-07-14

The eleventh published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **PATCH**
release with a single, focused module change: it **fixes a Cypher
pattern-predicate and pattern-comprehension correctness bug** on graphs
that hold **parallel edges** (two or more relationships of different
types between the same ordered pair of nodes). It is **API-additive**
over `v0.8.0` — no exported identifier was added, removed, or renamed,
there is **no breaking change**, and there is **no new user-facing public
API**. It is a **recommended upgrade** for anyone who runs pattern
predicates (`WHERE [NOT] (a)-[:TYPE]->()`), pattern comprehensions, or
binds a relationship variable over a multigraph.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant**. The fix aligns behaviour with the openCypher pattern
semantics without moving the execution baseline: parallel-edge type
selection is not exercised by a TCK scenario, so
`tckExecutionBaseline = 3897` in `cypher/tck/runner_test.go` is unchanged.
The Go toolchain remains **go1.26.5** (unchanged), and `govulncheck ./...`
stays clean. This is a correctness-only patch that touches no traversal
or execution hot path — the sole production source change is in
`cypher/pattern_eval.go` — so the benchmark figures are **inherited
unchanged from `v0.8.0`** (see
[docs/benchmarks/v0.8.1.md](docs/benchmarks/v0.8.1.md)).

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.8.1
```

### Fixed

- **Pattern-predicate and pattern-comprehension relationship-type
  matching over parallel edges (correctness).** When two or more
  relationships of different types connected the same ordered pair of
  nodes, the pattern evaluator inspected only the **first** stored type of
  the pair (`EdgeLabels(pair)[0]`), so any non-first parallel type was
  reported as non-existent. Over a graph with
  `(a)-[:FIRST]->(b)` and `(a)-[:SECOND]->(b)`, the predicate
  `WHERE NOT (a)-[:SECOND]->()` wrongly kept `a` (it judged the `:SECOND`
  edge absent), and a bound relationship variable's `type(r)` could report
  the first-stored type instead of the pattern-selected one. The
  single-relationship chokepoint `edgeMatchesRel` now tests **every** type
  of the endpoint couple — fixing the outgoing, incoming, undirected,
  variable-length, and comprehension paths at once — and `relValueFromHop`
  reuses the canonical `pickEdgeType` resolver so `type(r)` on the
  pattern-comprehension fallback path reports the pattern-selected type.
  Three regression tests guard the fix
  (`cypher/rel_multitype_pattern_test.go`), each verified red without the
  change and green with it. Pattern-based relationship-type selection over
  parallel edges is not covered by a TCK scenario, so the execution
  baseline is unaffected. (#2016)

### Changed

- **Release engineering: CI consolidated to a release-only workflow, with
  all correctness gating enforced locally.** The GitHub Actions surface is
  reduced to the single `release.yml` workflow (tag-triggered goreleaser
  publish plus the fast Phase-A release-accuracy check); the correctness,
  coverage, TCK-execution, and crash-injection gates now run **only**
  locally through `make ci` / `make release-preflight` before a tag is
  pushed. The local release gate was **de-duplicated** so the
  race-enabled, coverage-instrumented suite runs **once** — `make
  release-preflight` is now `release-accuracy` + `make ci` + the headline
  benchmark, and `scripts/pre-release.sh` is a standalone,
  no-coverage convenience gate rather than a second full run.

### Testing

- **Soak-layer test compilation repaired** after the `PropsEvalFn`
  error-return and `btree.RangeFrom` additions landed in `v0.8.0`, so the
  `soak` build tag compiles cleanly again.
- **PackStream allocation-charge upper-bound test** is skipped under
  coverage instrumentation, where the compiler's coverage counters perturb
  the measured allocation budget; the assertion still runs in the normal
  and `-race` passes.
- **Release-path guard reconciled with the de-duplicated gate.** The
  `TestReleasePathsConverge` release-integrity check
  (`internal/scriptgate`) had still asserted the pre-de-duplication path
  (`release-preflight` → `scripts/pre-release.sh`). It now enforces the
  same anti-bypass invariant against the current pipeline —
  `release` → `release-preflight` → `make ci` (the
  `tidy`/`fmt`/`vet`/`build`/`test-short` `-race ./...`/`lint`/`cover-gate`
  gate, with the TCK execution baseline inside the race pass) — after the
  correctness gate moved out of `scripts/pre-release.sh` and into `make ci`.

[0.8.1]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.8.1

## [0.8.0] — 2026-07-14

The tenth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is **correctness and security hardening**: three
consecutive multi-specialist audits — of the INDEX feature, the
CONSTRAINT feature, and a whole-module production-readiness review with
a round-2 re-audit — close a broad set of Consistency, Atomicity,
Durability, and denial-of-service gaps across the schema DDL path, the
snapshot and WAL recovery readers, and the Bolt protocol. The one
net-new, backward-compatible capability is acceptance of the **modern
openCypher `FOR ... REQUIRE` constraint syntax** that every current
Neo4j driver, shell, and migration tool emits — previously the main
production-interoperability blocker for the constraint feature. A Go
**toolchain bump to `go1.26.5`** also clears an open `crypto/tls`
advisory (`GO-2026-5856` / `CVE-2026-42505`).

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
the `FOR ... REQUIRE` grammar is net-new, additive Cypher surface; every
other change is an **internal** fix, a security/DoS hardening, a
toolchain patch, or test/documentation work that restores a
previously-documented contract or closes a hardening gap. No breaking
change to the exported Go API ships in this release. Per the project's
own rule, the examples and the deterministic-simulation (DST) harness
are **not part of the module** — they exercise it but expand no public
API, and are reported under [Testing and tooling](#testing-and-tooling),
not as new functionality. As a `0.y.z` release the public API remains
unstable; pin the exact version you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant** across the in-memory engine and every persistence
backend. Several fixes in this release **strengthen** the ACID guarantee
directly: a failed schema-DDL statement can no longer persist across a
restart (Atomicity), recovered `UNIQUE`/`NOT NULL` constraints are no
longer silently left unenforced (Consistency), and a nested or map
property value that the snapshot codec cannot serialise is now rejected
fail-stop instead of stored (Consistency/Durability). The Go toolchain
is now **go1.26.5** (bumped from `go1.26.4` for the CVE above), and
`govulncheck ./...` reports no vulnerabilities.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.8.0
```

This entry is an **exhaustive** accounting of every user-facing change
landed in the 86 commits between `v0.7.0` and this release
(`git log --oneline --no-merges v0.7.0..HEAD`), cross-checked against the
`gograph` roadmap (rmp sprints 265–278). Changes are grouped by category;
each bullet cites the roadmap task(s), audit finding, or the area it
touches.

### Added

#### Cypher constraint syntax

- **Modern openCypher `FOR ... REQUIRE` constraint grammar.**
  `CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (n:Label) REQUIRE n.prop
  IS UNIQUE | IS NOT NULL` is now accepted as the primary syntax, with
  the legacy `ON (n:Label) ASSERT ...` form kept as an alias; both map to
  the same IR and are actually registered and enforced. Neo4j deprecated
  `ON ... ASSERT` in 4.x and removed it in 5, so every current driver,
  shell, and migration script emits `FOR ... REQUIRE` — which GoGraph
  previously rejected with a misleading "expected ON" error while
  mis-parsing the `FOR` keyword as the constraint name. This was the main
  production-interoperability blocker for the constraint feature.
  Out-of-scope forms are now recognised and rejected with a specific,
  actionable error instead of a confusing parse failure: relationship
  constraints, composite (multi-property) constraints, `NODE KEY` /
  relationship key, and property-type constraints (`IS :: <TYPE>`). This
  is the sole net-new module capability in this release; it is additive
  and TCK-neutral (index/constraint DDL is not TCK-covered), so the
  3 897-scenario execution baseline is unchanged. (#1906)

### Changed

- **Go toolchain pinned to `go1.26.5`** (from `go1.26.4`); see
  [Security](#security) for the CVE it clears. (#2003)
- **`CREATE INDEX` accepts Neo4j's clause order.** The common migration
  idiom `CREATE INDEX myidx IF NOT EXISTS FOR ...` (name before
  `IF NOT EXISTS`) used to fail with "expected FOR, got IF"; the parser
  now accepts `IF NOT EXISTS` on either side of the optional name, so
  both the Neo4j order and the legacy GoGraph order map to the same plan.
  Index DDL is not TCK-covered, so this is a Neo4j-compatibility
  improvement with no conformance impact. (#1982)
- **`UNIQUE` numeric identity now uses value-equivalence.** The integer
  `1` and the float `1.0` are treated as the same constrained value (and
  `+0.0 == -0.0 == 0`, and every `NaN` collapses to one key), matching
  openCypher `=`, GoGraph's own `MERGE` (aligned in #1240), and Neo4j.
  Equivalence is exact-value based and therefore transitive, so two
  integers beyond `2^53` that round to the same `float64` remain distinct
  (a value-set map key requires transitivity). (#1910)
- **Constraint names are required unique**, and a re-declared equivalent
  constraint reports a consistent, backing-index-free error. Two
  constraints could previously share a name (making `DROP CONSTRAINT
  <name>` non-deterministic), and re-declaring diverged between kinds
  (`NOT NULL` silently overwrote the name; `UNIQUE` errored with a message
  leaking the internal `__uniq__<label>.<prop>` backing-index name).
  (#1907, #1908)
- **`db.constraints()` returns the declared constraint name** in its name
  column — the key `DROP CONSTRAINT` resolves by — with fully
  deterministic row ordering. (#1909)
- **Reserved index-name namespaces are protected.** A user
  `CREATE INDEX` / `DROP INDEX` is rejected when its name uses the
  constraint-backing `__uniq__` prefix or the internal `_btree_num`
  suffix, and a composite-index attempt (`ON (n.a, n.b)`) returns a
  specific "not supported" message instead of a bare "expected ')'".
  (#1912, #1899)
- **DDL identifier length is capped** (`maxSchemaIdentifierLen = 4096`) at
  the parser boundary, and the WAL/snapshot durable encoders are
  reconciled to fail-stop rather than silently truncate an oversized
  name, label, or property. (#1903)
- **Constraint-violation messages render the human-readable value**
  instead of the internal kind-tagged dedup key, so a client no longer
  sees `\x00`-prefixed or IEEE-754-hex/base64 values. (#1914)

### Fixed

#### Cypher conformance and correctness

- **`MERGE ... ON CREATE / ON MATCH SET` evaluates an expression
  right-hand side.** A non-literal RHS such as `MERGE (n) ON MATCH SET
  n.num = n.num + 1` used to commit but silently drop the write (or error
  on a stringified form) — a fail-silent Consistency defect the
  openCypher TCK does not cover (its `ON MATCH SET` scenarios use a
  constant or cross-variable RHS). The IR now carries the non-literal
  set-item value ASTs to the physical builder, which evaluates each
  against a schema-consistent row with the matched node/relationship and
  its current properties bound. Discovered by the DST `merge-rel`
  scenario. (#1965)
- **`SET` / `MERGE`-`SET` right-hand-side evaluation errors are surfaced,
  not swallowed.** The write-path closure discarded any evaluation error
  and returned a silent no-op, so `SET n.p = COUNT { (n)-->() }` left
  `n.p` unset with no diagnostic (the identical RHS is rejected loudly in
  `RETURN`/`WHERE`), and it hid arithmetic/type errors on any `SET` RHS.
  Both closures now propagate the error, so the statement aborts and rolls
  back atomically. (sprint 277, Cypher F1)
- **Nested-collection and map property values are rejected fail-stop with
  `InvalidPropertyType` on every write path.** openCypher restricts a
  property value to a primitive or a flat list of primitives; a nested
  list or a map is invalid. The write path previously stored or silently
  dropped such values — a store inconsistency, because the snapshot codec
  cannot serialise a nested `PropList`, so a later checkpoint would stall.
  New sentinels (`ErrNestedPropertyValue`, `exec.ErrNestedPropertyValue`)
  now fail-stop `CREATE` inline (node + relationship, literal + parameter),
  `MERGE ON CREATE`/`ON MATCH SET`, `MERGE`-pattern search props, and
  whole-entity `SET n =` / `SET n +=`; flat lists of primitives remain
  valid. (findings F3; sprints 277–278)
- **A `UNIQUE`-constrained idempotent self-set now succeeds.** Setting a
  `UNIQUE` property to the value the node already holds (`SET n.k = n.k`,
  a same literal, `SET n += {...}`, `SET n = {...}`) was rejected as a
  duplicate of itself, because the uniqueness check ran before the node's
  own value was released. All `SET` paths and both `MERGE` set executors
  now release the replaced value before the check and overwrite (a
  `UNIQUE` constraint guarantees at most one holder, so releasing first
  cannot mask a real cross-node duplicate); the previously-leaked phantom
  reservation on the replaced value is also fixed. (#1905, #1904)
- **Recovered `UNIQUE` / `NOT NULL` constraints are auto-enforced by
  `NewEngineWithStore`.** Opening a persisted database with the plain
  constructor left durable constraints unregistered and silently
  unenforced (duplicates and nulls accepted). The constructor now
  auto-registers them from the graph's durable store-direct set (using
  synthesised deterministic names, since that set does not retain the
  originals), and warns at construction if durable constraints are present
  but none were re-registered. A new `lpg.Graph.StoreConstraints` getter
  exposes the set. (#1981)
- **The unbounded-above string range seek is open-ended.** The upper bound
  used a fixed 32-byte `0xFF` sentinel, which is not a true maximum for a
  variable-length key, so a value sorting above it was silently dropped
  versus a full scan (seek ≠ scan). New `btree.Index.RangeFrom` /
  `RangeCountFrom` scan to the last leaf. Not reachable with valid UTF-8,
  so the TCK baseline is unaffected. (#1895)
- **`NodeByIndexRangeScan` drops a bogus exclusive-bound `NodeID`
  comparison.** The operator compared the emitted `NodeID` to a
  property-value bound — meaningless, since it holds only a `NodeID`
  bitmap — and could drop a node whose ID happened to equal a numeric
  bound. It now honestly emits the inclusive `[lo, hi]` superset and
  leaves exact open/closed semantics to the residual `Filter` the planner
  always stacks. Harmless for the Cypher engine; a fail-silent trap for a
  direct `exec` caller. (#1897)
- **The DDL parser returns a typed `SyntaxError`, not a panic, on
  truncated input.** Bare token-slice indexing raised
  index-out-of-range on truncations such as `CREATE INDEX x FOR (`; every
  read now routes through a bounds-safe `tokAt` helper, so a public
  untrusted-text boundary fails with a clear error rather than crashing.
  (#1896)
- **The constraint registry is struct-keyed to remove dot aliasing.** The
  registry keyed maps on `label + "." + prop` and split on the last dot,
  so `("A.b","c")` and `("A","b.c")` both keyed to `"A.b.c"` and could be
  mis-attributed to each other. Reachable via the Go API and via recovery
  of an externally-authored constraint, so the ACID Consistency mandate
  requires the pair to be exact; a comparable `ckey{label, prop}` struct
  is now the map key. (#1916)

#### Constraint and index durability

- **A failed `CREATE` / `DROP` schema-DDL statement no longer persists
  across a restart under a concurrent checkpoint.** A DDL whose WAL commit
  failed at fsync poisons the writer and discards its frame, but the
  in-memory registry still reflected the attempted change until its
  compensator ran outside the store single-writer lock — so a
  non-blocking checkpoint could capture the transient registry and publish
  it into `constraints.bin` / `indexdefs.bin`, then enforce (CREATE) or
  apply (DROP) on restart a change the client saw fail (an Atomicity
  violation). The checkpoint's phase-1 capture now consults a new
  `wal.Writer.Poisoned()` probe and aborts before capturing or publishing
  when the writer is poisoned, closing the window for both the constraint
  and index DDL paths. (#1902, #1919)
- **Index-def registry updates happen inside the DDL single-writer
  window.** `recordIndexDef` / `forgetIndexDef` now run before
  `commitIndexTx` (while the DDL still holds the single-writer
  serialisation) in all three WAL-backed paths, so a WAL-truncating
  checkpoint can never capture a `(watermark, defs)` pair inconsistent
  with the WAL — closing a HIGH-severity gap where a dropped index could
  be resurrected or a created index lost across a crash. (#1894)
- **`Graph.HasConstraints` flips inside the `CREATE CONSTRAINT` barrier.**
  The count sync previously ran only on `defer`, after the single-writer
  semaphore was released, so a checkpoint reaching its self-sufficiency
  check in that window could read a stale count, deem the snapshot
  self-sufficient, truncate the CREATE frame, and lose the constraint on
  restart. It now flips atomically with registration. (#1917)
- **A dropped constraint is not resurrected across a checkpoint**
  (regression gate added alongside the existing index-drop-survival
  test). (#1920)
- **`constraints.bin` and `indexdefs.bin` reads are hardened** — see
  [Security](#security).

#### Search, crash-injection, and test fidelity

- **APSP `At(x, x)` on an isolated (degree-0) node returns `0` /
  reachable**, matching textbook APSP and `BellmanFord.Distance`, instead
  of the `unreachable` an over-narrow live-node matrix index produced.
  (finding F4)
- **`crashinject.Run` classifies a startup-deadline as `TimedOut`**,
  not a raw hard error, when a very short timeout elapses around process
  startup — removing a load-induced flake in a 1 ms-timeout gate test
  under the full `-race ./...` suite. Normal generous-timeout runs are
  unchanged.
- **`FuzzCSRFileReader`'s seed uses the real `GGCS` magic** (it previously
  tripped `ErrBadMagic`, so the fuzzer only ever exercised `DecodeHeader`),
  and now drives the full `openBytes` validate/reinterpret path on an
  8-byte-aligned input. Two stale reader comments were corrected to
  describe what is actually implemented. (sprint 277)

### Performance

- **The `NOT NULL` existence check is O(1) and allocation-free at commit
  time.** A copy-on-write `label → NOT-NULL-properties` index replaces a
  per-node locked full-map scan plus a fresh slice allocation.
  `BenchmarkNotNullProperties` over 257 constraints reports 6.3 ns/op,
  0 B/op, 0 allocs/op. (#1911)
- **No measured performance regressions.** This release contains no
  performance-focused work; the guard-band benchmarks are unchanged versus
  `v0.7.0`. See [docs/benchmarks/v0.8.0.md](docs/benchmarks/v0.8.0.md) for
  the authoritative measured figures.

### Security

Sprints 277–278 remediated a 2026-07-13 five-specialist
production-readiness audit and its round-2 re-audit. None of the
memory-bound findings affect a well-formed store or a trusted caller;
they close denial-of-service, out-of-memory, and defence-in-depth gaps
reachable only from a hostile query, a crafted or corrupted file, or a
state-transition edge case.

- **Toolchain bumped to `go1.26.5` to clear `GO-2026-5856` /
  `CVE-2026-42505`** — a `crypto/tls` Encrypted Client Hello
  de-anonymization advisory, reachable by symbol through the Bolt TLS
  path. GoGraph never configures ECH, so the vulnerable behaviour is
  unreachable in practice, but a release must not pin a toolchain with an
  open `crypto/tls` advisory. `govulncheck ./...` reports no
  vulnerabilities on `go1.26.5`. (#2003)
- **Value nesting-depth is capped to close an authenticated stack-overflow
  DoS.** A short query — `reduce(acc=[0], x IN range(1, 4900000) |
  [acc])` — could build a ~5 M-deep nested value inside the element budget
  and overflow the goroutine stack in a recursive walker (a fatal crash
  `recover()` cannot catch). `reduce()` now rejects an over-deep or
  over-large accumulator with a typed `EvalError` (via iterative,
  alias-robust `expr.ExceedsValueDepth` against `expr.MaxValueDepth`), and
  PackStream `WriteValue` enforces the symmetric `maxValueDepth` bound
  (`ErrNestingTooDeep`). (finding F1)
- **`store/snapshot`'s `readIndexFile` is bounded by the
  manifest-declared size.** An unbounded `io.ReadAll` drove an
  out-of-memory crash on adopting a tampered store whose manifest declares
  a tiny size for a multi-gigabyte `indexes/<name>.bin`, before the CRC
  check. It was the one snapshot reader that missed the manifest-size
  bound every sibling reader applies. (finding F2, CWE-770 / CWE-789)
- **`constraints.bin` and `indexdefs.bin` reads are bounded** before the
  CRC is verified, with eager pre-allocation from the untrusted record
  count dropped in favour of incremental growth — matching the hardened
  properties/labels readers. (#1915, #1901)
- **`PropList` recovery decode caps its eager reservation.** The reserved
  slice was clamped by element count, but each `~24`-byte slot made the
  clamp roughly `4.8×` the remaining wire bytes — up to several GiB for a
  hostile count, a fatal out-of-memory at store-open that recovery cannot
  catch and that re-fires on every restart. (round-2 finding R1, CWE-770 /
  CWE-789)
- **Bolt `COMMIT` and `ROLLBACK` require authentication.** A session that
  sent `LOGOFF` while a transaction was open (left in `TX_READY` but
  unauthenticated) could still finalise the transaction; both handlers now
  also require `s.authenticated`, mirroring `RUN`/`BEGIN`/`ROUTE`.
  Defence in depth — only already-authorised writes were ever reachable,
  so there was no privilege escalation. (finding F5, CWE-306)

### Documentation

- Corrected `docs/cypher.md` to present `FOR ... REQUIRE` as the primary
  constraint grammar (with `ON ... ASSERT` as a legacy alias), document
  retroactive validation of pre-existing data, constraint-name
  uniqueness, and the rejected unsupported forms; and corrected the false
  "btree index supports `ORDER BY`" claim to the real range-predicate-only
  capability, with the deliberate dialect and scope boundaries stated.
  (#1913, index F-DOC1)
- Documented the secondary-index write-path contract — pick one write path
  per graph, because raw `txn.Store` writes bypass the Engine-managed
  index-update path and leave those indexes stale until the next restart
  rebuilds them (an intentional layering boundary, never durable
  corruption). (#1980)
- Corrected a stale `fnID` godoc that wrongly claimed `elementId()` is
  unimplemented. (#1978)
- Documented the `int64` hash-index Go-API contract, the `Index.Apply`
  subscriber-ordering contract, and steered `KShortestPathsLoopless`
  callers toward the bounded `KShortestPathsLooplessCtxWithOpts` or the
  polynomial `YenKShortest` for untrusted or large inputs. (#1900, #1898,
  #2006)

### Testing and tooling

The examples and the deterministic-simulation (DST) harness are **not
part of the GoGraph module** — the module neither imports nor depends on
them. They exercise the module's behaviour and are recorded here as
evidence and tooling, not as new module surface.

- **Deterministic-simulation (DST) coverage expanded across sprints
  267–270.** New crash- and checkpoint-recovery scenarios drive
  schema-mutation and `MERGE`-relationship clauses (#1925–#1928); a
  broadened Cypher surface — `DISTINCT`, three-valued logic, string
  predicates, pagination, aggregates, subqueries, `UNION`, `CASE`,
  comprehensions, temporal constructors, and `db.*` introspection
  (#1929–#1940); 16 previously-uncovered search algorithms cross-checked
  against independent reference implementations (#1941–#1956); and
  concurrent durable-commit, checkpointer-teardown, read-transaction, and
  storage fault-injection scenarios — atomic publish, WAL-corruption
  fail-stop, dir-fsync fault, and CSV/JSONL/GraphML round-trip under
  `ENOSPC` (#1957–#1964). The DST also surfaced the `MERGE ON MATCH SET`
  expression-RHS defect fixed above (#1965). New map-valued parameter
  binding was added to the simulation engine adapter to unblock this
  coverage (#1924).
- **The examples suite grew from 26 to 34.** New examples: negative-weight
  routing (28), all-pairs shortest paths (29), minimum spanning tree (30),
  observability and Prometheus metrics (31), Eulerian circuits (32),
  generation snapshot-swap MVCC (33), and Bolt transactions/auth/TLS (34).
  Examples 04, 08, 11, 13, 14, 16, 17, 18, 20, 22, 25, and 26 were
  extended to exercise atomic transaction rollback, personalised PageRank,
  structural analytics, additional centralities, real cross-process
  `kill -9` recovery, the bulk loader, intra-query parallel algorithms,
  the full Cypher write surface, DDL/introspection/`EXPLAIN`, and
  analytical aggregation. (sprints 271–276, #1969–#1996)
- **CI and timing-budget reconciliation.** The `-race` short-layer
  per-package timing budget was brought green (heavy resident-heap and DST
  scenarios moved to the soak layer, with a documented per-package ceiling
  override), and the release-path guard was reconciled with the split
  release gate so no release path publishes while skipping a gate.
  (d3cfe9f, 415371f)

[0.8.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.8.0

## [0.7.0] — 2026-07-03

The ninth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is **openCypher completeness and correctness**:
eight new builtin functions (`elementId`, `timestamp`, `randomUUID`,
`isNaN`, and the `toStringList`/`toIntegerList`/`toFloatList`/
`toBooleanList` family), a fix for `MERGE`'s **whole-pattern
match-or-create semantics** — the single most common Cypher
graph-building idiom, previously silently incomplete for a pattern with
a fresh endpoint — and a **rejected-instead-of-silently-dropped**
parallel edge on a non-multigraph engine, closing a genuine write-loss
gap on the documented default configuration. Four consecutive
multi-specialist production-readiness and security audit rounds also
land a broad **security and denial-of-service hardening pass** across
the WAL, snapshot, index, Bolt protocol, and CSV/GraphML/JSONL
interchange formats: bounded allocations, `O(n²)` DoS guards, and
symlink-attack rejection on every file the engine opens by name.

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
the new builtin functions are net-new, additive Cypher surface; every
other change is an **internal** fix that restores a previously-documented
contract or closes a hardening gap. No breaking change to the exported Go
API ships in this release. As a `0.y.z` release the public API remains
unstable; pin the exact version you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant** across the in-memory engine and every persistence
backend. The `MERGE` and parallel-edge fixes described above are
themselves Consistency fixes in the ACID sense: a write that used to
silently vanish or apply only partially now either fully applies or is
rejected with a typed error — never a partial, silent result. The Go
toolchain remains **go1.26.4** (unchanged), and `govulncheck ./...`
stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.7.0
```

This entry is an **exhaustive** accounting of every user-facing change
landed in the 94 commits between `v0.6.0` and this release
(`git log --oneline --no-merges v0.6.0..HEAD`), cross-checked against the
`gograph` roadmap (rmp sprints 257–264). Changes are grouped by
category; each bullet cites the roadmap task(s) or the area it touches.

### Added

#### Cypher language surface

- **Eight new openCypher builtin functions**, closing a functional-
  completeness gap that previously fail-stopped a real openCypher/Neo4j
  workload with `SyntaxError.UnknownFunction`:
  - `elementId(node|rel)` — a stable string identity (the durable id in
    decimal), the recommended replacement for the deprecated `id()`.
  - `timestamp()` — milliseconds since the Unix epoch at the statement's
    frozen instant; every row of one statement observes the same value,
    and concurrent statements stay independent.
  - `randomUUID()` — a `crypto/rand`-backed RFC 4122 v4 UUID string.
  - `isNaN(number)` — boolean; an integer is never NaN.
  - `toStringList`, `toIntegerList`, `toFloatList`, `toBooleanList` —
    element-wise scalar list conversion, `null` per non-convertible
    element.

  All eight are `null` in → `null` out on a `null` argument, raise a
  typed error on a non-entity/non-list argument as appropriate, and are
  TCK-neutral (no TCK scenario references these names), so the
  3 897-scenario execution baseline is unchanged. (#1832)

### Fixed

#### Cypher conformance and correctness

- **`MERGE` whole-pattern match-or-create semantics.** `MERGE
  (a:L1{...})-[:R]->(b:L2{...})` with at least one fresh endpoint used to
  silently create only the first node, dropping the relationship and the
  second node with no error. A new left-deep join search
  (`ir.MergePattern`/`exec.MergePattern`) now handles any chain the
  narrow single-relationship fast path did not already cover — fresh
  endpoints, multi-hop chains, and node-targeted `ON CREATE`/`ON MATCH`
  actions — with an atomic create-everything-missing fallback that never
  decomposes a partial match into independent node reuse. `Engine.Run`
  now rejects a write-clause query with a clear error pointing at
  `RunInTx`/`RunAny` instead of an opaque "unsupported IR node", and a
  cyclic same-pattern variable reuse rejects at plan-build time instead
  of a confusing runtime error. (#1866)
- **A parallel edge on a non-multigraph engine is rejected, not silently
  dropped.** openCypher's data model is a multigraph — `CREATE` never
  deduplicates, not even a byte-identical repeat. On the documented
  default engine configuration (`adjlist.Config{Directed: true}`, no
  `Multigraph`), creating a second relationship between two
  already-connected nodes used to report success while storing nothing —
  a silent write loss. It now fails the whole statement up front with a
  new `ErrParallelEdgeInSimpleGraph` sentinel, before any mutation or WAL
  frame; `NewEngine` also logs a one-time warning when constructed over a
  non-multigraph graph. (#1856)
- **Integer and Float hash consistently in `DISTINCT`/grouping/`UNION`.**
  `EquivalentHash` hashed an `IntegerValue` and an equal `FloatValue`
  (e.g. `1` and `1.0`) through two different representations even though
  `Equal` already treated them as the same value, so
  `count(DISTINCT [1, 1.0])` over-counted and `UNION` failed to merge
  equal rows across the two types. An `IntegerValue` now hashes through
  the same float64 domain `Equal` already uses. (#1857)
- **Two more `EquivalentHash` gaps closed, plus a sibling `hash_join`
  bug.** `NodeValue`/`RelationshipValue`/`LazyNodeValue` now hash
  consistently with the same cross-type numeric identity
  (`count(DISTINCT [n, id(n)])` no longer over-counts), and the hash
  join operator's independent key-hashing — which disagreed with
  `EquivalentHash` in the opposite direction for a large-integer/float
  pair near the `2^53` precision boundary — now delegates to
  `expr.EquivalentHash` directly, closing a case where a hash join could
  silently drop a matching row. (#1868)
- **`distinctAggregator`'s seen-values set is capped**, bounding
  `DISTINCT`/`collect(DISTINCT ...)` memory under an adversarial
  high-cardinality input. (#1867)
- **`MapValue.Equal` no longer depends on Go's randomized map iteration
  order.** For a map pair containing both a `NULL`-yielding entry and a
  `FALSE`-yielding entry comparison, which one the (randomized)
  iteration order visited first used to decide whether `Equal` returned
  `Null` or `false` for the same two logical inputs across different
  calls. Per the three-valued-logic conjunction openCypher's compound
  equality requires (CIP2016-06-14), a definitive `FALSE` now always
  wins over a `NULL`, independent of iteration order — matching the
  sibling `ListValue.Equal`, which was already correct.
- **Empty relationship type and node label rejected everywhere**,
  closing the last remaining inconsistent site (the label-predicate
  expression form, `WHERE n:\`\``/`RETURN n:\`\``) after the pattern
  sites were closed. (#1878)
- **`coalesce()` with zero arguments returns a typed `ArityError`**
  instead of an undefined result. (#1835)
- **The pre-parse guard counts arithmetic `-`/`*` and comparison/
  predicate operators**, closing two gaps where a crafted query could
  bypass the operator-count-based resource guard. (#1831, #1839)
- **DDL cancellation gaps closed.** The confirmation-drain step no
  longer re-checks a stale context after a statement already committed;
  the parallel hash-index backfill's cancellation poll now guarantees a
  checkpoint on every worker's very first iteration regardless of range
  alignment; B-tree index backfill and the `NOT NULL` pre-existing-data
  validation scan are now context-aware end to end. Each gap only
  affected how promptly a large `CREATE INDEX`/`CREATE CONSTRAINT`
  responded to cancellation — every statement still completed correctly
  and atomically before these fixes. (#1869, #1872)
- **`Yen`'s k-shortest-paths algorithm picks the min-weight parallel
  edge** in a multigraph's `buildEdgeIndex`, with a new deep-root-hop
  regression gate. (#1884)
- **MCMF Bellman-Ford bootstrap honours context cancellation.** (#1834)

#### Storage, durability, and security hardening

Four consecutive multi-specialist audit rounds (rmp sprints 257–264;
reports under `docs/audit-*.md`) closed a broad set of hardening gaps
against malicious or corrupted input across every persistence and wire
format the engine reads by name or from an untrusted source. None of
these findings affect a well-formed store written by GoGraph itself;
they close DoS and integrity gaps reachable only from a hostile,
crafted, or corrupted file.

- **WAL and CSR files are opened with `O_NOFOLLOW`**, rejecting a
  symlinked component instead of following it — the release's one
  HIGH-severity finding. (#1843, #1847)
- **The CSR reader is bounded by the real, `fstat`-measured file size**,
  not the untrusted manifest's declared `Size` field, closing a
  time-of-check-to-time-of-use (TOCTOU) window that could otherwise
  drive an allocation far beyond the actual file. (#1850, #1853)
- **A mismatched CSR weight-width is rejected on recovery** with a typed
  `ErrCorrupted`, instead of panicking with an out-of-range index when a
  forged snapshot declares a narrower weight width than the store's
  native type. (#1882)
- **`embedsValidFrame`'s CRC scan is bounded**, defeating an `O(n²)`
  crafted-torn-tail storm that could otherwise hang WAL recovery for
  minutes to days at the frame-size cap. (#1883)
- **Speculative allocation is bounded** for length-prefixed values during
  snapshot decode and for WAL frame-payload reads, and the index
  recovery path's `idCount` deserialization bound is tightened to
  `len(body)/8`. (#1886, #1889, #1885)
- **`manifest.json` decode is bounded** by `DefaultMaxManifestBytes`, and
  the `edgehandles` `propCount` map-size hint is clamped against an
  out-of-memory on a hostile snapshot file. (#1833, #1829)
- **Checkpoint writers self-heal a registry-capture race.**
  `WriteLabels`/`WriteProperties` could observe a label or property key
  interned by a concurrent commit during their lock-free walk but absent
  from their earlier name-table capture, previously aborting the whole
  checkpoint attempt; a bounded, monotonic-registry-backed retry now
  self-heals the race (never a correctness or durability breach — the
  prior behaviour was fail-stop-safe but could degrade to unbounded WAL
  growth under sustained new-name interning). (#1880)
- **`RunCheckpoint`'s godoc no longer falsely claims** that interleaved
  checkpoint calls are safe (phase 2 is deliberately lock-free by
  design), and `Stats().LastError` now reflects the most recently
  *started* attempt rather than the most recently *completed* one.
  (#1873)

#### Interchange (`io`) hardening

- **GraphML import is streamed, replacing a full-document DOM parse**,
  and the number of `<key>` elements is capped. (#1851)
- **A truncated GraphML document tail is rejected all-or-nothing**, and
  the number of `<data>` children per element is capped. (#1854, #1887)
- **CSV per-record field count is bounded**, closing a
  memory-amplification out-of-memory on a hostile file; the `MaxBytes`
  documentation is corrected to match the enforced behaviour. (#1844,
  #1888)
- **JSONL nested-list depth is capped**, bounding decode memory on a
  deeply nested value. (#1888)

#### Bolt protocol hardening

- **Corrected decoded-memory accounting for the map cost and aggregate
  payload coverage**, closing an under-count in the per-connection
  inbound-decode budget. (#1849)
- **An engine-wide inbound-decode memory ceiling applies across every
  Bolt connection**, not just per-message. (#1845)
- **Error-hygiene, cleartext-connection, and empty-image hardening.**
  (#1846, #1848)

#### Cypher and query-engine resource limits

- **A mandatory default statement-timeout floor applies to autocommit
  `RUN`**, and stays armed across the `RUN`/`PULL` boundary instead of
  expiring between them. (#1828)
- **An engine-wide result-memory ceiling applies across connections**,
  and pipeline-breaker operators are bounded by estimated bytes rather
  than row count alone. (#1842, #1841)
- **`shortestPath`/`allShortestPaths`' exhaustive search modes are
  bounded by a work budget**, and `ParallelScanProject`'s peak memory is
  bounded to the engine result budget. (#1840, #1830)
- **`range()` and other list-producing functions are bounded** by both
  the per-call evaluation budget and the per-row result budget. (#1852)

#### Concurrency, cancellation, and resource hygiene

- **`TestJohnsonAPSPParallel_CancellationCascades` no longer flakes**
  under heavy machine-wide CPU contention (as produced by the full
  `./...` test suite running concurrently, e.g. in `scripts/cover_gate.sh`
  or CI). The test-only fix widens the algorithm's own uncancelled
  runtime by roughly three orders of magnitude relative to the
  cancellation signal, verified stable over 50 consecutive runs; the
  production cancellation-cascade logic under test was already correct
  and is unchanged.

#### Test fidelity

- **`AllNodesScan`/`Filter`/`Project`/`ResultSet` regression gates use
  real `NodeID`s** instead of placeholder values. (#1863)
- **A deep-root-hop regression gate** pins the Yen multigraph fix.
  (#1884)
- **The integrated crash-injection loop's `disk.Crash()` wiring is
  pinned** against a silent no-op. (#1819)

### Performance

- **The `O(V+E)` edge-type filter is cached across queries.** Every
  relationship-type-filtered pattern (`-[:TYPE]->`, used by `Expand`,
  `OptionalExpand`, `VarLengthExpand`, and the predicated shortest-path
  builder) used to rebuild its filter from a full graph scan on every
  execution, regardless of selectivity. A new bounded LRU now caches the
  filter map keyed by the relationship-type set and a monotonic
  topology-generation counter bumped on every edge mutation — including
  a direct `store/txn` write that bypasses the Cypher engine's adapters
  entirely. Measured on a 120k-node / ~960k-edge graph: a repeated
  selective query against an unchanged graph drops from **≈190 ms /
  2.16 M allocs/op to ≈30 ms / 336 K allocs/op**. (#1871)
- **`Dijkstra_Large`'s stray heap escape reclaimed** (5 → 4 allocs/op,
  −11.97 % B/op), restoring the guard-band regressed since `v0.6.0` by
  pre-sizing the pooled Dijkstra heap's backing array. (#1820)

### Security

Four consecutive multi-specialist security and production-readiness
audit rounds (rmp sprints 257–264) ran under this release, closing every
finding they raised — the complete list is in the
[Storage, durability, and security hardening](#storage-durability-and-security-hardening),
[Interchange hardening](#interchange-io-hardening),
[Bolt protocol hardening](#bolt-protocol-hardening), and
[Cypher and query-engine resource limits](#cypher-and-query-engine-resource-limits)
subsections of **Fixed** above. Severity summary:

- **1 HIGH** — WAL/CSR files followed a symlinked path component instead
  of rejecting it (`O_NOFOLLOW` now enforced). (#1843, #1847)
- **7 MEDIUM** — denial-of-service and memory-amplification gaps across
  Bolt inbound-decode accounting, CSV/GraphML/JSONL parsing, snapshot and
  index deserialization bounds, and a WAL `O(n²)` torn-tail scan.
- **Several LOW / informational** items — documented rationale for
  accepted, non-exploitable residual risk (JSONL default cap sizing,
  FNV shard-hash choice). (#1855)

`govulncheck ./...` remains clean; no Go standard-library or dependency
CVE affects this release. The Go toolchain is unchanged at **go1.26.4**.

### Documentation

- **`RunInTx`'s godoc no longer makes a false no-rollback claim.** (#1870)
- **`RunCheckpoint`'s concurrency contract is corrected** to explicitly
  forbid interleaved calls. (#1873)
- **`CountTriangles`' simple-graph precondition is documented.** (#1862)
- **The undirected betweenness-centrality normalisation divisor is
  corrected in the doc comment.** (#1861)
- **`docs/cypher.md`'s `elementId()` entry is corrected** — it wrongly
  claimed the function was unimplemented; `docs/cypher.md` and
  `docs/bolt.md` freshness footers are re-stamped after a full
  specialist re-review found both otherwise accurate.
- **ADRs backfilled** for the two compliance mandates (openCypher TCK,
  ACID) and prior deferral/value-model decisions. (#1821)
- **Four production-readiness and security audit reports published**
  under `docs/`, documenting every finding, remediation, and
  specialist certification for this release cycle.

### Notes

- **Pre-1.0 stability.** This is a `0.y.z` release. The public Go API
  may change without a major-version bump until `1.0.0`; pin the exact
  version you depend on.
- **Module path.** The Go module path is
  `github.com/FlavioCFOliveira/GoGraph` with no `/vN` suffix, which is
  Semantic-Import-Versioning-correct for a `0.x` line.

[0.7.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.7.0

## [0.6.0] — 2026-06-28

The eighth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is a substantially **wider analytics and Cypher
surface**: four new centrality measures (Closeness, Harmonic,
Eigenvector, Katz), node-label round-trip in the JSONL and GraphML
exporters, Cypher **map projection** (`n{.name, .*, k: expr}`), and
`shortestPath()`/`allShortestPaths()` with **per-instance relationship
typing**. The release also lands a deep reliability and openCypher
conformance hardening pass and a deterministic-simulation-testing (DST)
infrastructure expansion.

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
every change in this release is **additive** over `v0.5.0` (new exported
functions, new Cypher features, new options) or an **internal** fix that
restores a previously-documented contract. No breaking change to the
exported Go API ships in this release. As a `0.y.z` release the public
API remains unstable; pin the exact version you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant** across the in-memory engine and every persistence
backend. The Go toolchain remains **go1.26.4** (unchanged), and
`govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.6.0
```

This entry is an **exhaustive** accounting of every user-facing change
landed in the 231 commits between `v0.5.0` and this release
(`git log --oneline --no-merges v0.5.0..HEAD`), cross-checked against the
`gograph` roadmap. Changes are grouped by category; each bullet cites the
roadmap task(s) or the area it touches.

### Added

#### Analytics (`search`, `search/centrality`)

- **Four new centrality measures (`search/centrality`).** `Closeness`
  (Wasserman–Faust normalisation for disconnected graphs), `Harmonic`
  (Boldi–Vigna formulation), `Eigenvector`, and `Katz`, each with a
  context-aware `…Ctx` variant. `Eigenvector` and `Katz` take an options
  struct and return the iteration count alongside the score vector.
  (#1800)
- **Stateful single-source shortest-path engine.** A reusable SSSP
  engine that validates edge weights once and amortises the validation
  cost across repeated queries on the same graph. (#1516)
- **`CSR.Validate` boundary check.** A new `CSR.Validate` method and the
  `ErrMalformedCSR` sentinel detect a structurally malformed CSR
  snapshot at the boundary before it reaches a search hot path. (#1762)
- **Opt-in weightless adjacency mode.** Adjacency storage can drop the
  per-edge weight column for unweighted graphs, and the shortest-path
  relaxers handle a weightless CSR. (#1650)

#### Cypher language surface

- **Cypher map projection.** `RETURN n{.name, .*, k: expr, var}` is now
  supported — selected-property, all-properties (`.*`), literal-entry,
  and variable-selector forms, composable in a single projection.
  (#1775)
- **`shortestPath()` / `allShortestPaths()` with per-instance
  relationship typing.** Path functions honour relationship-type
  predicates (including relationship-type disjunction in variable-length
  patterns) and per-instance relationship types, correct across parallel
  edges in a multigraph. (#1685, #1690, #1691, #1692)
- **Undirected `src == dst` shortest cycle + `WHERE`-during-search.**
  An undirected `shortestPath` from a node to itself now finds the
  shortest cycle, and a `WHERE` predicate is applied during the search.
  (#1785, #1786)
- **Per-instance by-handle edge properties.** `SET` and `REMOVE` on a
  bound relationship now maintain per-instance edge properties addressed
  by handle, correct under parallel edges. (#1684, #1686, #1688, #1689)
- **`MERGE` of a distinct relationship type creates the parallel edge.**
  `MERGE` now creates a parallel edge when its type differs from an
  existing edge between the same endpoints. (#1683)
- **Presence-only `r.k IS [NOT] NULL` for bound relationships.** A fast
  path answers relationship-property presence predicates without
  materialising the property value. (#1638)
- **Index-backed property predicates.** Equality predicates are served
  by `index.Manager` index seeks, and numeric range predicates are
  served by a `float64` companion B-tree index. (#1651, #1652)

#### Storage and persistence

- **Columnar edge-property storage tier.** Edge properties are stored in
  a de-boxed, per-`(key, kind)` columnar tier with a validity bitmap and
  sparse-COO columns for low-fill keys, cutting per-edge resident memory
  substantially on property-heavy graphs. (#1633, #1641, #1645, #1646)
- **`lpg.DateValue` Go-API date type.** Folds Go-API dates into the
  `int32` epoch-day column so dates set through the public API share the
  compact dense-date storage. (#1649)
- **Inline single-label edge-label storage.** Edge labels are stored
  inline in the adjacency column, removing the separate
  `map[edgeKey]LabelID`. (#1583, plus the redesign spike #1582)
- **`EngineOptions.DisableParallelBackfill` toggle.** Opts a deployment
  out of parallel index backfill when single-threaded backfill is
  preferred. (#1747)
- **Per-shard copy-on-write adjacency publication + pin API.** Adjacency
  writes publish via per-shard copy-on-write, with a snapshot-pin API.
  (#1526)

#### Interchange (`io`)

- **Node-label round-trip in the interchange exporters.** The JSONL and
  GraphML writers now carry node labels through export, so a graph
  exported and re-imported preserves its label set. (#1793)

#### Testing infrastructure (deterministic simulation testing)

- **`SimDisk`-backed filesystem seam (`OpenFS`).** Snapshot, CSR file,
  and checkpoint paths run on the injectable simulated filesystem, and
  the crash-storm scenario exercises full snapshot + WAL crash-recovery
  end to end. (#1546, #1740, #1752)
- **Disk-full (`ENOSPC`) injection** in `SimDisk`, with an ACID-checker
  exercise. (#1742, #1743)
- **Deterministic and soak memory-pressure scenarios** under a real heap
  ceiling, a CPU-starvation scenario testing fair scheduling under a
  clamped core, and a parallel-scan differential variant pair. (#1744,
  #1745, #1746, #1748)
- **The whole `search/` package is brought under the DST** with a
  correct-by-construction oracle and per-algorithm differential checks
  (SCC, topological sort, transitive closure, weighted SSSP/APSP, MST,
  flow, matching, assignment, Euler circuit, centrality, community,
  k-core, biconnected components, k-shortest), validated on the
  crash-survived graph. (#1726–#1732)
- **Full-feature DST coverage** for constraint enforcement, the property
  type system, `shortestPath`/`allShortestPaths`, edge properties,
  index-type diversity with parallel backfill, the broader Cypher read
  surface, and read-only-transaction isolation. (#1733–#1739, #1753)

### Changed

- **`EXPLAIN` labels a disconnected `MATCH` as `CartesianProduct`.** A
  query whose `MATCH` patterns share no variable is now reported as a
  `CartesianProduct` in the `EXPLAIN` plan, surfacing the accidental
  cross-product to the query author. (#1807)
- **String + number concatenation returns null.** This behaviour is now
  pinned and documented, with a `SyntaxError` raised where the
  openCypher specification requires it. (#1770, #1794)
- **Dead cost-based planner removed.** The unused cost-based planner was
  deleted to reduce surface area; the active planner is unchanged.
  (#1666)

### Fixed

#### Cypher conformance and correctness

- **`ORDER BY … LIMIT 0` yields an empty result**, not all rows. (#1801)
- **Integer and Float order as one Number tier** in comparison and
  `ORDER BY`, matching openCypher numeric ordering. (#1789)
- **Nested aggregation is rejected at compile time** rather than
  producing an undefined result. (#1804)
- **A non-aliased compound grouping key reads its precomputed column**
  instead of evaluating to null. (#1803)
- **`ORDER BY` passthrough variable dropped from result columns** so a
  sort key does not leak into the projection. (#1805)
- **Aggregation-in-`WHERE` error names `WHERE`**, not `ORDER BY`. (#1806)
- **Parser: compact subtraction disambiguation.** The lexer now
  correctly discriminates a binary minus from a signed literal in
  compact subtraction expressions, and the variable-length bound rewrite
  is scoped to relationship brackets. (#1788, #1796, #1797, #1798)
- **Relationship-uniqueness enforced across comma-separated `MATCH`
  patterns**, preventing a single relationship from binding twice across
  patterns. (#1777)
- **`shortestPath` / `allShortestPaths` find directed `src == dst`
  cycles; the undirected case is safe.** Context cancellation is now
  honoured during `allShortestPaths` enumeration. (#1779, #1780, #1782)
- **Shortest-path relaxers guard nil weights** on a weightless CSR
  rather than panicking. (#1776)
- **`SET r = map` / `SET r = node` have true `REPLACE` semantics.**
  (#1687)
- **Aggregate `DISTINCT` equivalence and `sum()` empty-input identity**
  match openCypher semantics. (audit round-1)
- **`DISTINCT` equivalence, `sum()` empty identity, integer `/0` raises,
  invalid dates rejected, negative `substring` argument, `toString`
  trailing `.0`** — round-1 and round-2 conformance fixes across the
  Cypher scalar-function and aggregation surface. (#1757, #1759,
  #1764–#1768, #1771–#1773)
- **Per-instance relationship type on the multigraph reverse hop** and
  on parallel self-loops; the opposite-direction gap is documented.
  (#1634)
- **Per-instance relationship properties on multigraph parallel edges.**
  (#1684)
- **Parser regeneration is byte-for-byte reproducible.** (#1694)

#### Storage, durability and recovery

- **WAL torn-tail detection.** A corrupt length field that would mask
  durable frames as a torn tail is now detected, preventing silent data
  loss. (#1778)
- **Schema DDL durably persists across a WAL-truncating checkpoint.**
  Index and constraint definitions survive a checkpoint that truncates
  the WAL. (#1755, #1756)
- **`NOT NULL` existence constraints enforced at commit time**, covering
  omit-at-`CREATE` and set-label paths, not only `SET`-to-null. (#1754)
- **A rolled-back no-op edge `CREATE` no longer deletes a pre-existing
  edge** between the same endpoints. (#1751)
- **Tombstone-aware CSR build** drops ghost edges from the search
  snapshot. (#1790)
- **Checkpoint-versus-commit deadlock resolved** by resolving labels and
  properties after `Mapper.Walk`. (#1648)

#### Interchange (`io`)

- **GraphML: per-`(name, kind)` keys** so heterogeneous node and edge
  property keys round-trip without collision. (#1791)
- **Nested-list property serialisation is bounded** to prevent writer
  out-of-memory on deeply nested values. (#1792)
- **Temporal time-zone offset preserved on export.** (#1769)
- **`DateValue.String` is the inverse of `ParseDate` for expanded
  years.** (#1658)
- **One latency sample is recorded per IO operation** at the context
  layer, removing a double-counted metric. (#1524)

#### Bolt protocol

- **Bolt protocol hardening.** Typed `Terminated` `FAILURE` on the reaper
  path, partial `DISCARD {n}` handling, `qid` validation, and query
  messages ignored on an authenticated `FAILED` connection. (#1781,
  #1783, #1784, #1787)

#### Concurrency and resource hygiene

- **`ParallelScan` closer goroutine is joined in `Close`** and the
  `Run` plan is closed when root `Init` fails — no goroutine leak on the
  error path. (#1760, #1795)

#### Test fidelity

- **`testfs` cannot fabricate durable data.** `syncedSize` is clamped on
  `Truncate` so a sync fault cannot fabricate durable data, the
  suffix-only sync-fault model is documented and pinned, and dirent
  revocation is exercised in the integrated crash loop. (#1808, #1809,
  #1811)
- **`soakfull` / `stress` build tags are honoured in the soak family**
  by the test-layer gating. (#1810)
- **Stale `t.Skip` guards removed** that had masked working features.
  (#1761)

### Performance

The v0.6.0 cycle landed a broad performance-audit pass. Per-change
records live in [`docs/benchmarks/history/`](docs/benchmarks/history/);
the measured `v0.5.0 → v0.6.0` comparison is in
[`docs/benchmarks/v0.6.0.md`](docs/benchmarks/v0.6.0.md). Every
optimisation preserves the documented API contracts and both compliance
invariants.

- **Memory: tiered, columnar edge and node storage.** Columnar
  edge-property tier with validity bitmap and a fused
  property-at-insertion build path; `propBag`/`labelBag` tiering of edge
  properties and node labels; frame-of-reference bit-packing of the
  dense date column; tiered multigraph per-instance / per-handle edge
  stores; small-set `NodeSet` tier before roaring64 and a 16-byte unsafe
  tagged-union `NodeSet`; adjacency-slack reclamation via `Compact`;
  zero-copy little-endian CSR column streaming. (#1596, #1628, #1629,
  #1633, #1646, #1663, #1584, #1585, #1586, #1587, #1597)
- **Read-path concurrency.** Lock-free copy-on-write name→id registries;
  `PropertyKeyRegistry`/`LabelRegistry` lookups under a read lock;
  multigraph edge label/property reads under a read lock; a lock-free
  fast path for `IsTombstoned` on never-deleted graphs; wider
  (16 → 64) property shards. (#1695, #1696, #1698, #1699, #1700, #1669,
  #1701)
- **Cypher execution.** Adaptive intra-query parallelism via a shared
  governor; parallel full-node scan with pushed-down filter and
  projection; parallel-reduce `count` fast path; lazy node
  materialisation with a pooled `EvalWith` holder; per-worker row arena
  with presized scan collection; partial materialisation for
  field-extractor projections; streamed edge properties into the
  relationship map; demand-gated unreferenced relationship values;
  plan-cache memoisation of `hashJoinOrderSafe` + `InferParamTypes`;
  pooled one-entry sentinel map for binding-free `EvalWith`; reused
  `Project` input-row header; raw-cell pass-through for non-`DISTINCT`
  `count(<var>)`. (#1705, #1682, #1672, #1588, #1589, #1702, #1659,
  #1662, #1630, #1719, #1721, #1673, #1654, #1697, #1703)
- **Index.** Parallelised CREATE INDEX hash-index backfill phase-2;
  B-tree `LookupAppend` zero-alloc equality seeks; de-reflected per-key
  scalar reads in hash/B-tree `Deserialize`; clone-free borrow path for
  the equality seek; `slices`-based `bulkPack` and `BulkLoadSorted` fast
  path. (#1723, #1722, #1710, #1660, #1664)
- **Snapshot.** Streamed and arena-backed `WriteProperties` collectors;
  streamed `WriteLabels` via new `ForEach*LabelByID` accessors;
  de-reflected per-record writers with a reused scratch; CSR codec
  streamed without widening copies. (#1707, #1709, #1661, #1593, #1594,
  #1595)
- **Analytics.** Flat predecessor arena for weighted Brandes; parallel
  weighted betweenness across sources; parallel diameter `iFUB`
  eccentricity sweeps; parallel Floyd-Warshall, Johnson APSP, triangle
  counting, and weakly-connected-components; allocation cleanups in
  Hungarian, k-shortest, Leiden, Yen, and PageRank; Leiden aggregate
  buffer-set pooling; monomorphic reused heap for the MinCostMaxFlow SSP
  loop; dropped the redundant all-1.0 level-0 weights array. (#1715,
  #1674, #1716, #1680, #1675, #1676, #1679, #1668, #1590, #1591, #1592,
  #1725, #1665, #1713)
- **Bulk loading.** CSR-direct counting-sort build path. (#1708)
- **Bolt I/O.** Pooled request decoder and per-connection reader; pooled
  response buffer and encoder; streamed `PULL`/`DISCARD` extra map;
  reused row buffer and `Record` across a streamed `PULL`; decoded
  parameter map passed straight to the engine per `RUN`. (#1517, #1518,
  #1520, #1521, #1522)
- **Metrics.** Lock-free, zero-alloc `IncCounter` / `ObserveLatency`
  fast path and an alloc-free value-type `Stopwatch` for `metrics.Time`.
  (#1519, #1711)
- **Interchange.** De-allocated per-edge integer path in the exporters
  and a `csv` per-edge weight allocation dropped via an `AppendInt`
  scratch. (#1667, #1523)
- **Presize materialise.** Backing slice for scan plans is presized.
  (#1720)

### Security

- No new vulnerabilities; `govulncheck ./...` is clean at this release.
  No security-specific code changes ship in this cycle; the prior
  security remediations (SEC-2026-06-14b/c) remain in effect from the
  `v0.3.1` line.

### Documentation

- New centrality functions carry an explicit concurrency contract.
  (#1800 follow-up)
- A* integer f-score overflow precondition, the Leiden / Label
  Propagation unit-weight contract, GC tuning guidance for read-heavy
  workloads, the unbounded-by-design `KShortestPathsLoopless` entry, the
  `shortestPath`/`allShortestPaths` and relationship-type-disjunction
  documentation, and the full-stack checkpoint + WAL crash-recovery flow
  are documented. (#1758, #1763, #1706, #1717, #1685, #1741)
- The DST is documented (`docs/dst.md`), and the columnar-tier
  durability risk is pointed at its implemented coverage. (#1644, #1732)
- The examples mandate (organisation, three objectives, evidence) is
  expanded, and the read-only module audit reports (2026-06-21/22) and
  the per-round reliability-audit reports are recorded.
- Release-gate cleanup: `golangci-lint` and `staticcheck` cleared, and
  the gated-documentation freshness footers re-stamped after
  verification against the current code. (#1813, #1814, #1815)

### Examples

- **All 26 examples upgraded to a common standard** (the "ex-26
  standard"): seeded generators, scale knobs, subject-appropriate
  evidence collection (CPU, RAM, storage), and machine-readable `# `
  telemetry. Each example sub-folder carries a `README.md` describing its
  scenario, objective, and purpose. (#1598, #1599–#1627)
- **Example 26 carries mandatory `FRIEND.since` / `LIKE.when` dates**
  (ISO-8601, deterministically filled), with `IS NOT NULL` coverage
  queries and a large-scale `26_social_scale_bench` benchmark. (#1598)
- **Targeted example performance** improvements: Fenwick-tree
  preferential attachment for `O(n log n)` graph build in example 07,
  and faithful `-batch` (example 04) and scaling-claim (example 20)
  documentation. (#1656, #1657)

## [0.5.0] — 2026-06-17

The seventh published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is **live `db.*` schema introspection**: the
built-in `db.labels()`, `db.relationshipTypes()`, and
`db.propertyKeys()` procedures now report only the schema tokens
**currently in use** — labels, relationship types, and property keys
attached to at least one live (non-tombstoned) node or relationship —
rather than every token ever indexed or seen. A token disappears from
the result as soon as its last bearing element is deleted.

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
the new behaviour is additive on top of `v0.4.0`, and three new
exported methods are introduced on `lpg.Graph` — `NodeLabelsInUse`,
`RelationshipTypesInUse`, and `PropertyKeysInUse`. The low-level helper
`procs.RegisterBuiltins` changed its third parameter from a single
closure to a `procs.BuiltinSources` struct; this is permitted in a
`0.y.z` line and affects only direct callers of that internal-style
registration helper, not users of the public Cypher engine. As a
`0.y.z` release the public API remains unstable; pin the exact version
you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant**. The `db.*` introspection procedures are not covered by
the openCypher TCK, so the in-use semantics are free to diverge from
Neo4j (see Notes). The Go toolchain remains **go1.26.4** (unchanged),
and `govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.5.0
```

### Added

- **`db.labels()` — labels in use.** `CALL db.labels()` returns every
  distinct node label currently attached to at least one live node,
  whether or not an index exists for that label. The order is
  unspecified.
- **`db.relationshipTypes()` — types in use.** `CALL
  db.relationshipTypes()` returns every distinct relationship type
  currently attached to at least one live relationship. The order is
  unspecified.
- **`db.propertyKeys()` — keys in use.** `CALL db.propertyKeys()`
  returns every distinct property key currently in use across nodes
  **and** relationships. The order is unspecified.
- **`lpg.Graph` introspection methods.** New exported methods
  `NodeLabelsInUse() []string`, `RelationshipTypesInUse() []string`, and
  `PropertyKeysInUse() []string` enumerate the schema tokens in use,
  filtering out tombstoned elements. They back the `db.*` procedures and
  are available directly on the graph type.

### Changed

- **`db.labels()` no longer reports index-only labels.** Before this
  release the procedure could report a label merely because an index
  existed for it; it now reports a label only while at least one live
  node bears it.
- **`procs.RegisterBuiltins` signature.** Its third parameter is now a
  `procs.BuiltinSources` struct bundling the `ListConstraints`,
  `Labels`, `RelationshipTypes`, and `PropertyKeys` data-source
  closures, replacing the previous single constraint-rows closure. Every
  field is optional; a nil closure makes its procedure return an empty
  result set. This decouples the `procs` package from the concrete graph
  type.

### Documentation

- **`docs/cypher.md`** documents the in-use semantics of `db.labels()`,
  `db.relationshipTypes()`, and `db.propertyKeys()`, the deliberate
  divergence from Neo4j for `db.propertyKeys()`, and the
  not-yet-implemented status of `db.schema.visualization()` (registered
  but currently returns an empty result set).

### Compliance

- **openCypher TCK.** Execution suite remains fully green at
  **3 897 / 3 897 scenarios** and **16 006 / 16 006 steps**; the
  regression gate (`tckExecutionBaseline = 3897`) is unchanged. The
  `db.*` introspection procedures are not TCK-covered.
- **ACID.** Atomicity, Consistency, Isolation, and Durability hold
  across the in-memory engine and every persistence backend; the new
  introspection is read-only and does not touch the write path.

### Notes

- **Divergence from Neo4j (`db.propertyKeys()`).** Neo4j lists
  property-key tokens from a token store that is never
  garbage-collected, so it keeps reporting a key after its last bearer
  is deleted. GoGraph reports only keys currently in use. This is
  observable but not an openCypher-conformance issue, because the `db.*`
  procedures are outside the TCK.
- **Pre-1.0 stability.** This is a `0.y.z` release. The public Go API
  may change without a major-version bump until `1.0.0`; pin the exact
  version you depend on.
- **Module path.** The Go module path is
  `github.com/FlavioCFOliveira/GoGraph` with no `/vN` suffix, which is
  Semantic-Import-Versioning-correct for a `0.x` line.

## [0.4.0] — 2026-06-17

The sixth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is a **new public API** —
`(*cypher.Engine).BeginReadTx` — that opens a **lock-free, read-only
explicit transaction**, lifting concurrent read throughput by roughly
**2×** by never acquiring the single-writer serialisation, the
visibility barrier, or a WAL transaction. The release also lands
**Cypher read-path performance work** (per-query forward-CSR caching in
relationship reconstruction; non-escaping `RowContext` pooling) and the
**first publication of the deterministic-simulation-testing (DST)
harness** (`internal/sim` + `cmd/sim`), TigerBeetle-VOPR-modelled
test/CI infrastructure that found and fixed two latent defects.

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
the new `BeginReadTx` API is **purely additive** over `v0.3.2` — no
exported identifier was removed or renamed, and there is **no breaking
change**. As a `0.y.z` release the public API remains unstable; pin the
exact version you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant**. The Go toolchain remains **go1.26.4** (unchanged), and
`govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.4.0
```

### Added

- **`(*cypher.Engine).BeginReadTx(ctx)` — lock-free read-only explicit
  transactions.** A read-only transaction handle that acquires **neither**
  the engine writer serialisation, **nor** the visibility barrier, **nor**
  a WAL transaction — so it never serialises behind, or blocks, a
  concurrent writer. Every statement run through the handle is rejected
  **before execution** with the new exported sentinel
  `cypher.ErrWriteInReadOnlyTx` if it contains a writing clause or is DDL;
  this guard is what keeps the lock-free path safe. Permitted reads route
  through the engine's concurrent read path, each taking its own
  per-statement snapshot — **read-committed** isolation across the
  statements of the transaction, matching Neo4j's default. `Commit` and
  `Rollback` on a read-only handle are teardown-only no-ops. Over the Bolt
  protocol, `BEGIN` with `mode="r"` now routes through `BeginReadTx`, and
  `ErrWriteInReadOnlyTx` maps to `Neo.ClientError.Request.Invalid` with
  the message forwarded to the client (client-fault). Motivated by
  block-profiling that attributed **83 %** of Bolt-load blocking time to
  the single-writer semaphore held by `BeginTx` for the whole explicit
  transaction — even for read-only ones. (#1573)
- **Deterministic-simulation-testing (DST) harness** (`internal/sim`,
  `cmd/sim`) — first publication of a TigerBeetle-VOPR-modelled
  deterministic simulator: seed-driven virtual clock, in-memory faulting
  disk with real WAL recovery, graph oracle and invariant checkers,
  crash-and-recovery cycles, a real in-process Bolt wire path, a named
  scenario catalogue with deterministic trace recording, scripted replay
  and `ddmin` shrinking, a reproducible bounded-worker swarm, and upgrade,
  cross-release, and differential harnesses. The `cmd/sim` CLI exposes
  `-swarm`, `-scenario`, `-replay`, and `-check-every`. This is **test and
  CI infrastructure**, not a public module API. (#1528–#1576)

### Performance

- **Cypher relationship reconstruction — cache the forward CSR per
  query.** `edgeHandleAtFwdPos` / `edgeInstanceIdxFor` previously rebuilt
  the entire forward CSR (`O(V+E)`) on **every** result row to read one
  slot, making relationship reconstruction `O(R·(V+E))` per query. The
  forward CSR snapshot is now built once per query and reused, mirroring
  the existing edge-ID-resolver pattern. On `MATCH (a)-[r]->(b) RETURN
  count(r)` over a multigraph (benchstat, count = 6): **dense graph
  −88.2 % sec/op, −95.9 % B/op, −16.7 % allocs/op**; small graph −73.6 %
  sec/op, −83.3 % B/op. Certified by `graph-theory-expert`,
  `storage-engine-auditor`, and `go-developer`. (#1574)
- **Cypher scalar projection — pool the per-row `RowContext`.** The
  per-row context map is now recycled through a `sync.Pool` at the two
  **non-escaping** evaluation sites (the `Filter` predicate closure and
  the scalar-projection evaluator), gated on the existing analysis flag
  that bails on any expression kind that could retain the context past the
  call. On `MATCH (n:N) WHERE n.i>=0 AND n.j>=n.i RETURN n.i,n.j`
  (benchstat, count = 6): **−42.1 % B/op, −26.6 % allocs/op, −11.5 %
  sec/op**. Only the outer map container is recycled; values that escape
  into a result row are independently owned and never reused. Certified by
  `go-developer`; `rust-perf-engineer` consulted. (#1575)
- **Brandes betweenness — zero-allocation predecessor arena.** The
  per-source predecessor lists are flattened into a reusable arena,
  cutting allocations on the centrality hot path (bit-identical results).
  (#1515)
- **DST simulator — configurable invariant-check cadence.** The simulator
  invariant checker now runs on a configurable cadence with a guaranteed
  terminal check, reducing per-tick overhead in long runs without
  weakening end-state verification. (#1576)

### Fixed

- **Bolt illegal-transition error names the originating state.** An
  illegal protocol transition into `FAILURE` now reports the state the
  connection was actually in, rather than always reporting `FAILED` —
  found via the DST Bolt wire harness. (commit `82d98af`)
- **`DROP CONSTRAINT` by name is now atomic and never a silent no-op.**
  Dropping a constraint by name now drops the constraint **and** its
  backing index atomically; a previously fail-silent path that left the
  constraint in place is fixed. Schema DDL is not covered by the
  openCypher TCK, so the execution-conformance count is unaffected. Found
  via the DST schema-chaos scenario. (#1556)

## [0.3.2] — 2026-06-15

The fifth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **PATCH**
release with a single, focused change: it **fixes a data-compatibility
recovery panic** that could crash the process when opening an existing
on-disk store. It is **API-additive** over `v0.3.1` — no exported
identifier was added, removed, or renamed, there is **no breaking
change**, and there is **no new user-facing public API**. It is a
**strongly recommended upgrade** for anyone running `v0.3.0` or `v0.3.1`,
in particular anyone whose store was first written by `v0.2.0` or
earlier.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios) and **100 % ACID-compliant**. The Go toolchain
remains **go1.26.4** (unchanged), and `govulncheck ./...` stays clean.
This is a correctness-only patch that touches no hot path, so the
benchmark figures are **inherited unchanged from `v0.3.1`**
(see [docs/benchmarks/v0.3.2.md](docs/benchmarks/v0.3.2.md)).

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.3.2
```

### Fixed

- **Recovery panic on the edge fast-path handle column (data
  compatibility).** `graph/adjlist.upsertEdgeLocked` could panic with
  `makeslice: cap out of range` on the spare-capacity fast path when
  growing the per-edge handle column for a node that had accrued a
  **handle-less prefix**. The fast path sized the new handle column from
  `len(current.handles)` rather than from the neighbour count `oldLen`;
  for a node whose handle column was still nil/short (for example length
  `0`) while its neighbour backing array had grown with spare capacity, a
  later handle-bearing append computed a capacity (`growCap(0) = 4`) below
  the required length (`6`), so `make([]uint64, newLen, <newLen)`
  panicked. The fix sizes the column from `oldLen`
  (`make([]uint64, newLen, growCap(oldLen))`), matching the slow path in
  the same function; `growCap(oldLen)` is always `>= newLen`, and the
  copy-plus-zero-fill keeps the handle column length-aligned with the
  neighbour list (leading handle-less slots stay the `0` "no handle"
  sentinel).

  This was a hard **data-compatibility break** introduced by the
  amortised-O(1) `AddEdge` hub rewrite (`877e455`) and shipped in
  `v0.3.0` and `v0.3.1`; it is **absent in `v0.2.0`**.
  `store/snapshot.ApplyCSRToGraph` replays each node's edges as a mix of
  handle-less (`AddEdge`) and handle-bearing (`AddEdgeHIfAbsent`)
  inserts, so **any snapshot containing such a node crashed the process
  on open** — under both read and write recovery. Upgrading restores the
  ability to open these stores. Two regression tests guard the fix
  (`graph/adjlist/handle_prefix_regression_test.go` and
  `store/snapshot/apply_handle_prefix_test.go`), each verified red without
  the fix and green with it.

## [0.3.1] — 2026-06-15

The fourth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **PATCH**
release: it is **API-additive** over `v0.3.0`, with **no breaking change**
and **no new user-facing public API**. Its headline is a deep
**performance cycle** (tasks #1497–#1525) paired with two further
**security audits** (SEC-2026-06-14b and SEC-2026-06-14c, tasks
#1480–#1496). Every documented API contract is preserved unchanged, and
the performance work is correctness-preserving: the new write and
analytics paths produce **byte-identical / bit-identical** results and
regress **nothing** on the curated benchmark set. No exported identifier
was added, removed, or renamed.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios) and **100 % ACID-compliant**. The group-commit
write path was certified by the storage-engine auditor as preserving
Atomicity, Consistency, Isolation, and Durability. The Go toolchain
remains **go1.26.4** (unchanged), and `govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.3.1
```

### Security

- **Second security audit (SEC-2026-06-14b).** A follow-on, additive
  six-domain audit found and fixed **9** issues (tasks #1480–#1489); the
  full report is in
  [docs/security-audit-2026-06-14b.md](docs/security-audit-2026-06-14b.md).
  Its dominant theme was that the morning audit's "bound the eager
  allocation before the integrity gate" discipline had **not been
  generalised** to sibling decoders. All reachable from untrusted input
  (an on-disk artefact, a Cypher query, or Bolt bytes):
  - **Untrusted-artefact allocation bounds** — the **btree index**
    decoder (#1480) and the **label index** decoder (#1481) now clamp
    their eager allocation to `min(count, 1<<20)` and grow on demand,
    instead of pre-sizing on an unbounded untrusted count (a 20-byte
    CRC-valid payload could drive a 16 TiB reservation). The snapshot
    component readers (`readVerified{Labels,Properties,Mapper,EdgeHandles}`)
    now thread `FileEntry.Size` through an `io.LimitReader`, so the
    `append` loop can no longer grow past the real on-disk size — closing
    a back-door re-open of the earlier recovery-OOM hole (#1486). The
    property-value decoder now rejects a nested `PropList` element,
    enforcing the encoder's no-nesting invariant and closing an
    unbounded-recursion stack-exhaustion path (#1488).
  - **Cypher string-byte budget** — string concatenation (`+`) is now
    charged against a per-evaluation **byte** budget
    (`DefaultMaxStringEvalBytes`) before allocation, so a tiny doubling
    query such as `reduce(s='x', i IN range(1,33) | s+s)` can no longer
    grow an 8 GiB string from ~100 bytes of query text (#1482).
  - **Bolt `tx_timeout` overflow** — a client `tx_timeout` / `timeout`
    is now converted overflow-safely; a non-positive or overflowing value
    falls back to the server default unconditionally, so the wall-clock
    transaction reaper is always armed and the single global writer lock
    can no longer be pinned indefinitely (#1484).
  - **Cartesian-product visibility and cancellation** — a disconnected
    multi-pattern `MATCH` now surfaces a plan-time Cartesian-product
    **notification** (via the new `Result.Notifications()` accessor and
    the Bolt PULL `SUCCESS` metadata, faithful to Neo4j's
    `CartesianProductWarning`) and a locked-in deadline-cancellation
    guarantee (#1483).
  - **Bolt NOOP keep-alive** — a bare `00 00` NOOP keep-alive chunk is
    now silently discarded instead of being answered with a spurious
    `FAILURE` that evicted the very idle connection it was meant to
    preserve (#1485).
  - **DOT reserved-keyword quoting** — the DOT exporter now quotes node
    ids that collide with Graphviz reserved keywords (`node`, `edge`,
    `graph`, `digraph`, `subgraph`, `strict`), preventing silent
    export-integrity corruption (#1489).
- **Third security audit (SEC-2026-06-14c).** A four-phase red-team audit
  found and fixed **7** issues (tasks #1490–#1496), with **no Critical
  finding and no prior-fix regression**; the full report is in
  [docs/security-audit-2026-06-14c.md](docs/security-audit-2026-06-14c.md):
  - **`substring()` integer overflow** — the end bound is computed
    overflow-safely, so a huge `length` argument returns the conforming
    truncated tail instead of panicking on a negative slice index (#1492).
  - **`percentileCont()` NaN guard** — a non-finite percentile parameter
    is now rejected by `validPercentileParam` (a NaN previously bypassed
    the `[0,1]` check and could panic via a platform-dependent
    `int(NaN)` index); the continuous aggregator also clamps its index
    defensively (#1493).
  - **`replace()` amplification bound** — `replace(s, '', r)` now computes
    its worst-case output size overflow-safely and returns a typed
    `NumberOutOfRange` budget error before allocating, closing a
    quadratic output-amplification OOM (#1494).
  - **txn PropList decoder clamp** — `decodeTxnListProp` now clamps its
    capacity hint to `min(count, remaining/5)`, bringing the third
    parallel PropList decoder into lockstep with its two clamped siblings
    (#1490).
  - **Bolt reader panic boundary** — the per-connection reader goroutine
    now carries a `defer`/`recover` boundary mirroring the connection
    handler, so a future panic-on-input bug crashes one connection rather
    than the whole process (#1491).
  - **Supply-chain integrity** — a stale `.goreleaser.yaml` Dependabot
    comment was corrected with a regression gate (#1495), and
    `gen_tck_tzdata.sh` now pins the SHA-256 of the IANA tzdata tarball
    and aborts on mismatch (#1496).

### Performance

The 2026-06-14 performance cycle (rmp sprints S-PA1..S-PA6, tasks
#1497–#1525) delivered measured, benchstat-gated wins across the write,
analytics, query, and read paths. Every change is gated against the
`f6f8c7a` baseline (ledger row 0006) and the curated guard band
(Dijkstra / BFS / Brandes) stayed flat throughout. The raw before/after
numbers live in [docs/benchmarks/history/](docs/benchmarks/history/) and
the per-release report in [docs/benchmarks/v0.3.1.md](docs/benchmarks/v0.3.1.md).

- **Group commit / WAL fsync coalescing** (#1507) — the per-commit
  `fsync`, previously the dominant write-throughput ceiling, is replaced
  by a PostgreSQL-`XLogFlush`-style leader/follower coalesced fsync
  (`wal.Writer.SyncGroup`): one leader flushes and fsyncs the whole
  buffered suffix once while followers keep appending, and every follower
  whose durability watermark is covered returns success with no I/O of
  its own. Measured on `BenchmarkCommitConcurrent`: **−74.15 % at 8
  goroutines, −96.72 % at 64, and −99.16 % at 256 (≈ 118× concurrent
  write throughput)**, with **zero single-thread regression** (1
  goroutine flat at 3.683 ms, p = 0.959). The storage-engine auditor
  certified all four ACID properties preserved (durability acked only
  after the covering fsync; in-memory apply only in WAL sequence order
  after durability).
- **Parallel pull-formulation PageRank over reverse-CSR** (#1513) —
  PageRank's per-iteration SpMV, previously a single-goroutine push
  scatter, now runs a parallel pull formulation
  (`next[v] = baseShare + d·Σ cur[u]/outdeg[u]`) over a structure-only
  reverse-CSR across a persistent worker pool, partitioned by
  approximately equal in-edge count. Results are **bit-for-bit identical
  to the serial path regardless of `GOMAXPROCS` or scheduling** (proven
  cross-process). Measured **1.68–1.77× on large graphs** at 30
  iterations (up to **2.40×** for the SpMV kernel with the transpose
  amortised). Gated behind `live >= 2048 && GOMAXPROCS > 1`; smaller
  graphs and single-core runs take the unchanged serial path with **zero
  regression**.
- **Range-predicate B+tree index seek** (#1505) — a range predicate
  (`n.p > x`, `>=`, `<`, `<=`, or a two-sided AND) on a property backed
  by a bound string btree index now builds a `NodeByIndexRangeScan` in
  place of the label scan, with the original `Selection` filter retained
  on top. Measured **≈ 114× (−99.11 % time)** on the targeted
  0.5 %-selective shape, with **zero regression** on the curated set (the
  seek only fires under the selectivity / cardinality guards). This
  release also makes a Cypher `CREATE INDEX … {indexType:'btree'}` a
  bound, backfilled, self-maintaining index (it was previously registered
  empty with a no-op `Apply`).
- **Hash join for disconnected equi-join patterns** (#1506) — a
  disconnected multi-pattern `MATCH` joined by an equality predicate now
  builds an `exec.HashJoin` (O(|A|+|B|)) in place of the nested-loop
  Cartesian product (O(|A|·|B|)), under a structural trigger, a size
  floor, and an order-safety guard. The differential test proves an
  identical result multiset join-on vs join-off across NULL / NaN /
  cross-type keys; measured **≈ 93× faster, ≈ 95× less memory** on the
  targeted shape.
- **Real B+ tree replacing the sorted-array index** (#1514) — the
  range property index is now a real B+ tree.
- **Cypher read-path allocation cuts** (#1499–#1503) — column-oriented
  (SoA) materialised result rows (#1499, IC1 **−32.4 % time / −60.9 %
  B/op**), deferred node materialisation for scalar-only projections
  (#1500), a dropped redundant per-row map in the `WITH`/`RETURN`
  projection (#1501), shared node property/label views across the
  lpg→expr→wire seam (#1502), and a lock-free copy-on-write metadata name
  registry on the read path (#1503, **−81.57 % time at 8-way contention**
  on the isolated metadata-read benchmark). All curated entries are
  byte-identical or lower on allocations and B/op.
- **Storage write-path hardening of the durability stack** —
  a pre-sized partitioned-parallel bulk loader (#1512); a non-blocking
  LSN-watermarked checkpoint with WAL prefix truncation (#1508);
  elimination of the per-WAL-frame double allocation and copy (#1509);
  and `fdatasync` for the per-commit WAL sync on Linux (#1510). A CRC32C
  strategy microbenchmark confirmed the incremental `Update` path already
  uses the hardware path (#1511).

### Validation

- The full validation battery was run **green** at the release commit on
  **Go 1.26.4**: `go build ./...`, `go test -race ./...` (0 data races),
  `TestTCKExecution` (**3 897 / 3 897** scenarios, error-fidelity gate
  121), the `internal/crashinject` crash-injection battery,
  `golangci-lint`, `staticcheck`, and `govulncheck` (no vulnerabilities).
- A reproducible security test battery accompanies both audits, with
  every demonstration flipped to a strict regression assertion that
  passes on the fixed code and fails on regression.

## [0.3.0] — 2026-06-14

The third published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release: it is additive over `v0.2.0` and its headline work is a deep
**reliability and robustness hardening pass** drawn from four
successive code audits (three reliability and one security), alongside
four additive features. No exported
identifier was removed or renamed. Both compliance invariants continue
to hold without regression: the module is **100 % openCypher
TCK-compliant at the execution level** (3 897 / 3 897 scenarios) and
**100 % ACID-compliant**.

The hardening spans the Cypher engine (correctness and robustness:
`reduce()`, openCypher equivalence semantics for `DISTINCT` and
grouping, parser-panic and arithmetic-overflow guards,
`ParameterMissing`, type-error fidelity, and `SET`-clause spec
fidelity), the engine, Bolt protocol, and search layers (Bolt
autocommit read-path lock removal, `HopcroftKarp` input validation,
PackStream temporal wire encoding, and client-fault classification),
the import/export and observability surface (CSV/GraphML token-OOM
ceilings, fail-stop on XML-illegal characters, Prometheus name
sanitisation, portable non-finite-number encoding, CSV byte-order-mark
stripping, and a JSON Lines line cap), the durability path (a
recovery-promotion parent-directory `fsync` and a checkpoint
constraint-survival fail-safe), and the test-battery and release-gate
infrastructure.

Under Semantic Versioning, a `0.y.z` version signals that the public Go
API is **not yet stable** and may change while the module matures toward
`1.0.0`. Pin the exact version you depend on. This release introduces no
breaking API change. It tightens one consumer-visible default (the
CSV/GraphML import byte ceiling) and corrects one consumer-visible
behaviour — the Cypher `=~` regex operator, which was silently evaluated
as plain string equality and now performs an anchored regular-expression
match per the openCypher specification. Consumers should read the
**Upgrade notes** in [release-notes/v0.3.0.md](release-notes/v0.3.0.md).

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.3.0
```

### Added

- **`reduce()` list-folding expression** — the `reduce(acc = init, x IN
  list | expr)` accumulator expression is now wired through the Cypher
  grammar, the AST (`*ast.ReduceExpr`), semantic analysis, and the
  evaluator, so list folding is reachable from the public API. A null
  source list yields null, per the openCypher specification (#1426).
- **PackStream temporal wire encoding over Bolt** — the six Cypher
  temporal value kinds (`Date`, `LocalTime`, `Time`, `LocalDateTime`,
  `DateTime`, `Duration`) are now encoded as the canonical PackStream
  temporal Structs the neo4j-go-driver hydrator expects, instead of
  falling through to plain strings. `DateTime` encoding is Bolt-version-
  and time-zone-aware (the UTC convention on Bolt v5.0+, the legacy
  wall-clock convention on v4.4), so a real driver receives
  `neo4j.Date` / `neo4j.Duration` / `time.Time` values that support
  `.AsTime()` and temporal arithmetic (#1434).
- **Public `metrics` facade package** — a new top-level `metrics`
  package re-exports the previously internal metrics types via type
  aliases (`metrics.Backend`, `metrics.Registry`,
  `metrics.SetBackend`, `metrics.NewPrometheusRegistry`), so the
  observability wire-up documented in `docs/metrics.md` is now
  compilable from outside the module. A single implementation is
  retained behind the aliases.
- **Filesystem fsync-failure fault modes for testing** — the internal
  `testfs` fault library gains `Faults.FailSyncAfter` and
  `Faults.ReturnEIOOnSync`, both surfacing a new `ErrSyncFailed`
  sentinel and discarding the unsynced suffix, so durability tests can
  assert that an unacknowledged commit leaves no trace without a full
  crash-injection harness.
- **Additive TCK error-type fidelity gate** — a new conformance gate
  asserts that the engine raises the correct openCypher error *type*
  (not only that the scenario fails), built on the existing semantic
  error vocabulary. The execution result count is unchanged at
  3 897 / 3 897 (#1443).
- **Opt-in CSV formula-injection sanitisation** — the CSV writer gains
  `Options.SanitizeFormulae` (default off). When enabled, cells whose
  first character is `=`, `+`, `-`, `@`, tab, or carriage return are
  prefixed with a quote so a spreadsheet opening the export treats them
  as text (OWASP CSV/formula injection, CWE-1236). The default preserves
  the lossless round-trip (#1471).

### Changed

- **CSV and GraphML single-token import RAM is bounded.** The default
  import byte ceiling for the CSV and GraphML decoders is lowered to
  **128 MiB** (from 1 GiB) to cap the peak memory a single oversized
  token can pin while a record is assembled. The ceiling remains
  configurable, and `MaxBytes <= 0` still means unlimited (#1436).
- **`reduce()` is now a first-class expression** rather than an
  unreachable evaluator: queries that use `reduce(...)` now parse and
  execute instead of being rejected at parse time (#1426).
- **The `cypher/ir/rewrite` package documents its experimental status**
  and is guarded by an import-graph test, clarifying that it is not part
  of the stable public surface.
- **The Cypher `=~` regex-match operator now matches correctly.** It was
  silently parsed as `=` (string equality), so `'abc' =~ '[a-z]+'`
  returned false; it now performs an anchored full-match regular
  expression (`\A(?:…)\z`, openCypher / Java `String.matches` semantics).
  This corrects a latent fail-open hazard for any query that used `=~` in
  an authorisation or allow/deny predicate. See the Upgrade notes (#1479).
- **Dependencies bumped** (Dependabot): `RoaringBitmap/roaring/v2`
  2.18.0 → 2.18.2, `cucumber/godog` 0.14.1 → 0.15.1, `golang.org/x/sys`
  0.44.0 → 0.46.0, `spf13/pflag` 1.0.5 → 1.0.7. `govulncheck` stays clean
  and the TCK execution count is unchanged at 3 897 / 3 897.

### Fixed

- **Cypher correctness and robustness.**
  - `DISTINCT` and grouping now compare by openCypher **equivalence**
    rather than equality, so `NaN ≡ NaN` and nested nulls are grouped
    correctly (CIP2016-06-14) (#1420).
  - The parser no longer crashes the process on malformed input:
    `p.Script()` is wrapped in `recover()` for malformed `WITH`-clause
    and pipe-in-argument queries (#1422), and the pre-parse guard is
    extended to `CASE` nesting and binary-operator chains.
  - Deep operator chains can no longer overflow the stack during
    semantic analysis: `checkExpr` carries a recursion depth budget
    (#1424).
  - A query parameter that is referenced but not supplied now raises
    `ParameterMissing` instead of evaluating to a silent null (#1431).
  - Numeric-literal property access such as `(5).foo` raises
    `InvalidArgumentType` (#1430).
  - Integer and floating-point overflow are detected and reported as
    typed errors throughout: `sum()` accumulation (#1427), `MinInt64 /
    -1` integer division (#1419), `toInteger()` boundary rounding at
    `2^63` (#1428), pure integer literals over 19 digits, and
    exponent-form float literals that overflow `float64` (#1421).
  - The `Parse` and `ParseStrict` entry points now share one normaliser
    pipeline, eliminating a divergence on valid float literals (#1423).
  - `MaxResultRows` is applied to write-only queries, returning
    `ErrResultRowsExceeded` and rolling the write back atomically
    (#1425).
  - A leading `WITH ... WHERE` is seeded with a single-row argument so
    it no longer crashes.
- **SET-clause spec fidelity** (#1455, #1456, #1457, #1458) — four
  silent-divergence defects now fail loud and correct: setting or
  removing a **label on a relationship** is rejected with a `TypeError`
  (closing a latent hazard that reinterpreted an edge-position counter
  as a node id and could mislabel the wrong node); `SET n = null` clears
  all properties, `SET n += null` is a no-op, and a non-null non-map RHS
  (`SET n = 5`) is a compile-time `TypeError`; a nested map property
  raises `InvalidPropertyType`; and a non-map parameter to `SET n = $p`
  raises a `TypeError`.
- **Cypher DELETE no longer resurrects nodes.** The Cypher mutator
  adapter's `RemoveNode` now emits an `OpRemoveNode` WAL frame, so a
  node deleted through Cypher stays deleted across a store reopen
  instead of returning as a ghost on WAL replay (#1411).
- **Transaction isolation and atomicity.**
  - An explicit transaction holds the visibility lock for its whole
    lifetime, so concurrent readers block during an open transaction —
    closing a read-uncommitted isolation gap (#1412).
  - `ExplicitTx.Commit` is rejected after a failed `Exec`, preventing a
    partial transaction from becoming durable (#1413), and surfaces
    `ErrUndoFailed` when undo replay fails on a WAL `fsync` error.
  - DDL index and constraint registration and backfill now run inside
    the visibility barrier (#1417).
- **Engine, protocol, and search robustness.**
  - Bolt **autocommit reads use `RunAny`, not `RunInTxAny`**, so a
    read-only autocommit statement no longer takes the single-writer
    lock (#1432).
  - `HopcroftKarp` validates `nLeft` and returns `ErrInvalidInput`
    instead of panicking on malformed bipartite input (#1433).
  - Bolt and Cypher now **classify client-fault conditions** as client
    errors rather than server errors (#1435).
- **Import / export (`graph/io`).**
  - The GraphML writer **fails stop on XML-illegal characters** in
    string properties instead of silently emitting `U+FFFD`, on both
    write paths, and validates node ids on the plain `Write` path too
    (#1437).
  - The JSON Lines reader returns a typed `ErrLineTooLong` for an
    oversized single line (#1442).
  - The CSV reader strips a leading UTF-8 byte-order mark so spreadsheet
    exports parse correctly (#1441).
  - GraphML emits portable `xs:double` `NaN` / `INF` / `-INF`, and the
    non-finite contract for JSON Lines is documented (#1440).
  - The DOT writer emits bare node statements so isolated vertices are
    no longer dropped (#1439).
- **Observability.** The Prometheus exposition writer **sanitises
  metric names** to the valid grammar, closing an exposition-injection
  vector through the public metrics facade (#1438), and `WriteText`
  returns its accumulated write error.
- **Durability.**
  - Recovery **`fsync`s the parent directory** after promoting a
    snapshot backup, so a crash between the promotion rename and the
    directory flush cannot lose the promoted snapshot (A1-F4, #1454).
  - A checkpoint gates WAL truncation on `Graph.HasConstraints`, so
    declared constraints survive a checkpoint even when the embedder
    has not wired `checkpoint.WithConstraintSpecs` (#1464).
  - The WAL acquires an exclusive OS file lock on `Open` to prevent
    multi-process WAL corruption (#1416), `fsync`s after `ftruncate` in
    `poison()` (#1414), and the snapshot writer `fsync`s the `indexes`
    subdirectory after writing index files (#1415).

### Performance

- **Adjacency-list hub operations** (`graph/adjlist`) — `AddEdge` is now
  amortised **O(1)** and `RemoveAllEdgesFrom` **O(d)** for high-degree
  (hub) nodes, replacing the previous quadratic behaviour, via geometric
  pre-allocation with in-place append reuse and a single-lock bulk
  removal path. A degree-10 000 hub is now roughly 11.6× slower than a
  degree-1 000 hub, where it was previously 50–100× slower.

### Security

- **Exhaustive security audit and remediation (SEC-2026-06-14).** A
  phased, six-domain security audit found and fixed **13** issues
  (tasks #1467–#1479); the full report is in
  [docs/security-audit-2026-06-14.md](docs/security-audit-2026-06-14.md).
  All are reachable from untrusted input (a Cypher query, an imported
  file, or an on-disk artefact):
  - **Memory-exhaustion DoS bounds** — the snapshot record/string-table
    decoders now clamp their eager allocation *before* the CRC/size gate
    (a hostile snapshot could OOM `recovery.Open`), and the Cypher
    expression evaluator enforces a per-evaluation list-element budget so
    a tiny `reduce()`/comprehension query can no longer exhaust host
    memory (#1467, #1468, #1469, #1475).
  - **Cypher cancellability** — `reduce()`/comprehension/quantifier loops
    now honour `context` cancellation, so a deadline aborts a runaway
    query (#1477); variable-length-path traversal gained a per-query edge
    budget and a default hop ceiling (#1478).
  - **Bolt credentialed authentication** — `BasicAuthHandler` now
    authenticates at `LOGON` for Bolt ≥ 5.1, so credentialed auth works
    with modern drivers instead of forcing `NoAuth` (#1470).
  - **`=~` regex correctness** — the operator no longer behaves as string
    equality and is anchored per the openCypher specification, closing a
    fail-open hazard in authorisation predicates (#1479).
  - **Analytics over untrusted graphs** — `TransitiveClosure`, WCC and
    Kruskal compact over the live node set and `UnionFindSlice` uses
    64-bit indices, so hash-flooded node keys can no longer drive
    O(MaxNodeID²) over-allocation or index overflow (#1474, #1476).
  - **Export hardening** — opt-in CSV formula-injection sanitisation
    (#1471).
  - **Supply chain** — every GitHub Action in the release pipeline is
    pinned to a full-length commit SHA and the SBOM generator
    (`cyclonedx-gomod`) to an exact version, hardening the published
    artefacts against a moved/poisoned upstream tag
    (tj-actions/CVE-2025-30066 class) (#1472, #1473).

  Both compliance invariants held throughout: openCypher TCK at
  3 897 / 3 897 and ACID preserved; `go test -race`, golangci-lint,
  staticcheck and `govulncheck` are all clean.

## [0.2.0] — 2026-06-05

The second published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release: it is additive over `v0.1.0` and the headline work is a
reliability, ACID, and durability hardening pass across the persistence
stack, the Cypher engine, and the Bolt server. Both compliance
invariants continue to hold without regression: the module is **100 %
openCypher TCK-compliant at the execution level** (3 897 / 3 897
scenarios) and **100 % ACID-compliant**.

Under Semantic Versioning, a `0.y.z` version signals that the public Go
API is **not yet stable** and may change while the module matures toward
`1.0.0`. Pin the exact version you depend on. A small number of
behavioural defaults were tightened in this release; consumers should
read the **Upgrade notes** in
[release-notes/v0.2.0.md](release-notes/v0.2.0.md).

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.2.0
```

### Added

- **True explicit transactions over Bolt** — `Engine.BeginTx` returns an
  `*ExplicitTx` with `Exec` / `Commit` / `Rollback`, wired through the
  Bolt `BEGIN` / `RUN` / `COMMIT` / `ROLLBACK` protocol on both the
  WAL-backed and store-less engine wirings. A Bolt transaction now owns
  a single engine transaction whose statements are **atomic together**
  (made durable and visible as one unit on commit, unwound together on
  rollback), **isolated** by the single-writer serialisation, and
  **bounded** by a configurable per-transaction timeout
  (`Server.Options.DefaultTxTimeout`, default 30 s) so the global write
  lock can never be held indefinitely.
- **Persisted, fully enforced Cypher constraints** — `CREATE CONSTRAINT`
  / `DROP CONSTRAINT` are now durable, validating, and complete across
  every property kind. Constraints are journalled through the store
  transaction, replayed on recovery, and carried in a `constraints.bin`
  snapshot component so they survive a checkpoint and WAL truncation.
  `CREATE CONSTRAINT` validates and seeds against pre-existing data, and
  `UNIQUE` is enforced for `float`, `bool`, `time`, `[]byte`, and list
  values, not only strings. New constructor
  `NewEngineWithStoreAndConstraints` re-registers recovered constraints
  on open.
- **Composed crash-safe store** (`store.DB`) — a new top-level `store`
  package with a `DB` type that owns the Write-Ahead Log and an optional
  checkpointer and performs the one correct teardown order in
  `Close` / `CloseCtx`: optional final checkpoint, stop the checkpoint
  goroutine, then close the WAL. The order is idempotent and safe under
  concurrent `Close`. `bolt.Server` adopts it via an optional
  `Options.Closer`.
- **Finite result caps with typed Bolt failures** — the engine exposes
  `Engine.ResultRowCap()`, and the Bolt server maps the engine's
  bounded-resource sentinels to the `Neo.ClientError.General.LimitExceeded`
  failure code so an over-broad query is rejected cleanly while the
  connection stays healthy. `NewServer` logs a loud warning when it is
  handed a deliberately uncapped engine.
- **Server-level Bolt metrics** — six counters emitted through the
  internal metrics backend: `bolt.server.conn.accepted` / `.rejected` /
  `.closed` and `bolt.server.tx.opened` / `.closed` / `.abandoned`, with
  live-connection and open-transaction values derived as paired counters.
- **New labelled-property-graph accessors** — `lpg.Graph.FirstEdgeHandle`
  (the stable handle of the first `src → dst` slot), `lpg.Graph.Config`
  (the immutable directed / multigraph configuration), and
  `lpg.Graph.ValidateNode` (run the installed schema validator's
  whole-node check at finalisation). The matching `adjlist.AdjList.Config`
  accessor is also added.
- **Commit-serialised checkpointing** — `store/txn.Store.RunUnderCommitLock`
  and `store/checkpoint.WithCommitSerialiser`, which run the
  snapshot-and-truncate window under the store's real commit lock so a
  checkpoint can never capture a partial transaction or truncate away a
  transaction committed during the snapshot.
- **Scoped-read CSR file API** — `store/csrfile.Reader.Read`, which holds
  an internal read lock for the duration of a callback so `Close` cannot
  unmap the `mmap` region under an in-flight reader. The semi-external
  `search/extern` BFS and PageRank traversals run inside this scope.
- **Configurable resource budgets** — opt-out sentinels and tunables on
  `EngineOptions` for the new caps: `MaxResultRows`, `MaxResultBytes`,
  and `MaxCollectItems`, each with an explicit unbounded sentinel
  (`-1`).

### Changed

- **Finite default resource caps now apply where results were previously
  unbounded.** Result materialisation, per-group aggregator buffers, the
  `Eager` pipeline-breaker, pattern-comprehension lists, and the
  per-transaction WAL op buffer now carry finite defaults:
  `DefaultMaxResultRows`, `DefaultMaxCollectItems`, and
  `DefaultMaxEagerRows` are `10_000_000`; `DefaultMaxGroups` is
  `1_000_000`; `DefaultMaxResultBytes` is 1 GiB; `DefaultMaxTxnOps` is
  `16_000_000`. A query that previously streamed an unbounded result now
  returns a typed bounded-resource error once a default is exceeded. The
  defaults are high enough that the openCypher TCK and every shipped
  example stay green; each is configurable, with an explicit unbounded
  opt-out.
- **Import decoders enforce a byte ceiling.** The CSV, JSON Lines, and
  GraphML decoders now reject input above `DefaultMaxBytes` (1 GiB) with
  the typed `ErrInputTooLarge`, bounding memory at the untrusted-input
  boundary. The cap is configurable, with `MaxBytes <= 0` meaning
  unlimited.
- **`graph/io` decoders return a nil graph on any decode error.** The
  CSV, JSON Lines, and GraphML readers now return a **nil** graph and
  discard the partial result on **any** failure — a parse error, context
  cancellation, or the byte-ceiling error — making import uniformly
  all-or-nothing. The typed errors are unchanged.
- **Re-entrant `Graph.View` / `ApplyAtomically` now panics instead of
  deadlocking.** A goroutine already inside the visibility barrier that
  nests another `View` or `ApplyAtomically` call now panics immediately
  with an actionable message, rather than silently deadlocking the
  engine. The panic is never recovered, because it indicates a
  programmer error.

### Fixed

- **Durability and crash-safety hardening.** Cypher autocommit writes are
  now made durable (WAL `fsync`) **before** they become visible to
  concurrent readers, matching the transaction-layer commit ordering. The
  snapshot writer `fsync`s the staging directory before the publish
  rename, the bulk loader (`store/csrfile`) `fsync`s the parent directory
  after its rename, and the graph's directed / multigraph configuration
  is persisted in the snapshot manifest so a simple graph no longer
  silently becomes a multigraph after a reopen.
- **Fail-stop on genuine WAL corruption.** Recovery now returns genuine
  corruption (CRC mismatch, unsupported record version) to the caller
  instead of swallowing it into a tail-error field, and the shipped
  examples refuse to append to a corrupt WAL. A benign torn or truncated
  tail remains a clean success; `Result.IsClean` reports the distinction.
- **Atomic in-memory rollback for write queries.** A Cypher write query
  that errors or panics mid-execution now rolls its in-memory mutations
  back through an inverse-op undo log inside the visibility barrier, so a
  failed query never leaves the in-memory graph diverged from the durable
  state. Per-handle edge metadata is restored on the multigraph
  removal-then-fail interleaving.
- **Context-aware blocking and cancellation.** The engine's write-path
  store acquisition is now context-aware (a cancellable semaphore
  replaces the blocking mutex), and five long traversals
  (direction-optimised BFS, diameter, Tarjan SCC, triangle counting, and
  Cypher `DETACH DELETE`) poll for cancellation inside their inner loops
  so a deadline or cancellation is honoured promptly.
- **Bounded resources, never panic.** Per-transaction, per-group, and
  `Eager` buffers are bounded; the `Distinct` cap now counts distinct
  rows rather than hash buckets, so engineered hash collisions can no
  longer retain more than the cap; and a coarse result-byte budget
  complements the row cap so a single very wide row cannot exhaust memory.
- **Concurrency-primitive hardening.** A scoped-read API prevents
  `store/csrfile.Reader.Close` from unmapping an `mmap` region under an
  in-flight reader; the `generation` publisher enforces its
  single-publisher contract and fixes a missed-wakeup that could hang a
  draining publisher; the checkpoint loop drains its trigger channel on
  exit so a `Trigger` racing `Stop` cannot leak a goroutine; the Bolt
  session rolls its transaction back at the `FAILED` transition so the
  writer lock is released promptly; and the metrics backend's hot path is
  made nil-safe.
- **Multigraph edge-metadata rollback.** Removing a parallel edge while a
  sibling between the same endpoints survives, then failing the query,
  now restores the removed instance's per-handle identity, labels, and
  properties exactly.
- **Integer-overflow guard in network flow.** Flow-capacity and cost
  accumulation now validate their inputs and return the typed
  `ErrCapacityOverflow` instead of silently producing a wrapped, negative
  result; the integer shortest-path algorithms document their
  cumulative-weight precondition.
- **Schema required-property validation.** A declared required
  (`NOT NULL` / existence) property is now enforced at node finalisation
  via `Graph.ValidateNode`, instead of being advisory-only, upholding the
  ACID Consistency invariant.

### Performance

- **Personalised PageRank** (`search/centrality`) — the push-relabel
  worklist now compacts its consumed prefix to track the live frontier,
  bounding the worklist's backing array to the frontier size rather than
  letting it grow toward the total step count. The rank vector is
  bit-for-bit identical to the previous implementation.

### Security

- **Toolchain bumped to go1.26.4** (`chore(toolchain)` commit `6969201`),
  resolving two Go standard-library vulnerabilities reachable from the
  module's own code paths:
  - **GO-2026-5039** (`net/textproto`) — reached via `snapshot.Open` →
    `textproto.Reader.ReadMIMEHeader`.
  - **GO-2026-5037** (`crypto/x509`) — reached via the Bolt TLS handshake
    in `bolt/proto/handshake.go`.

  Only the `toolchain` directive changed; the `go 1.26` language
  directive is untouched. `govulncheck ./...` is clean on go1.26.4
  (2 vulnerabilities reported before the bump, 0 after).

## [0.1.0] — 2026-06-02

The first published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search that scales from in-memory
graphs to graphs that exceed RAM.

This release is published at a pre-1.0 baseline. Under Semantic
Versioning, a `0.y.z` version signals that the public API is **not yet
stable** and may change without a major bump while the module matures
toward `1.0.0`. The two compliance invariants are nonetheless already
in force at this version: the module is **100 % openCypher
TCK-compliant at the execution level** and **100 % ACID-compliant**.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.1.0
```

### Added

#### Core graph model

- **Generic graph contract** (`graph`) — a `Graph[N comparable, W]`
  interface parameterised over the node-identifier type `N` and the
  edge-weight type `W`, so the library is not tied to `int64`/`float64`.
  A sharded `Mapper` interns external keys to dense internal `NodeID`s
  with lock-free hot-path reads.
- **Directed and undirected graphs, multigraphs, and self-loops** —
  the model supports parallel edges between the same ordered pair and
  edges from a node to itself.
- **Mutable adjacency-list backend** (`graph/adjlist`) — a sharded,
  copy-on-write adjacency list with lock-free reads; the writer-side
  representation.
- **Immutable CSR snapshot** (`graph/csr`) — a Compressed Sparse Row
  view for read-mostly analytics with zero-synchronisation traversal.
- **Atomic snapshot rotation** (`graph/generation`) — a
  refcount-protected publisher that swaps immutable snapshots across
  concurrent readers and a single writer via an atomic pointer.
- **Labelled Property Graph** (`graph/lpg`) — vertex and edge labels
  with typed properties. `PropertyValue` covers `string`, `int64`,
  `float64`, `bool`, `time.Time`, and `[]byte`.
- **Optional type schema** (`graph/lpg/schema`) — a declarative schema
  with `Validate` for label/property typing.
- **Stable per-edge handle** — every directed edge carries an immutable
  `uint64` handle assigned at creation that is never reused or
  renumbered on delete, providing durable per-edge identity for the
  multigraph model.
- **Durable node tombstones** — deleted nodes are tombstoned (their
  `NodeID` is never reused) and the tombstone set survives a store
  reopen, so a deleted node never resurrects as a "ghost".

#### Indexing and pattern query

- **Index manager** (`graph/index`) — a `Manager` that coordinates
  named indexes and fans out `Change` events to subscribers.
- **Inverted label index** (`graph/index/label`) — Roaring-bitmap
  inverted index over vertex labels.
- **Exact-match property index** (`graph/index/hash`) — a sharded hash
  index for equality lookups.
- **Range property index** (`graph/index/btree`) — an order-preserving
  B-tree index for range queries.
- **Fluent pattern engine** (`graph/query`) — a `MATCH`-style fluent
  query API over labels and property predicates.

#### Search, path-finding, and analytics

- **Traversal and shortest paths** (`search`) — BFS, iterative DFS,
  Dijkstra, Bellman-Ford, A\*, bidirectional BFS, Yen k-shortest paths,
  topological sort (Kahn), Tarjan strongly-connected components,
  biconnected components, Eulerian path, and all-pairs shortest paths.
- **Centrality** (`search/centrality`) — Brandes betweenness, PageRank,
  and personalised PageRank.
- **Community detection** (`search/community`) — Leiden and label
  propagation.
- **Network flow** (`search/flow`) — Dinic, Edmonds-Karp, push-relabel,
  Stoer-Wagner global min-cut, and min-cost max-flow.
- **Semi-external algorithms** (`search/extern`) — BFS and PageRank that
  operate directly over Tier 2 `csrfile` readers without first loading
  the graph into the Go heap.
- **Union-find** (`ds`) — a disjoint-set primitive supporting the
  analytics layer.

#### Storage and persistence (ACID)

- **Write-Ahead Log** (`store/wal`) — a versioned, CRC32C-framed WAL
  with group-commit and a parent-directory `fsync` for crash-safe
  durability.
- **Atomic snapshots** (`store/snapshot`) — on-disk snapshot directories
  with a manifest and per-file CRC, including the optional
  `tombstones.bin` and `edgehandles.bin` components.
- **Single-writer transactions** (`store/txn`) — a `Begin` / `Commit` /
  `Rollback` API with all-or-nothing atomicity and a transaction-
  visibility barrier for isolation.
- **Background checkpointer** (`store/checkpoint`) — folds the WAL into
  a snapshot folder on a background goroutine with a defined lifecycle.
- **Recovery** (`store/recovery`) — snapshot restore plus idempotent
  WAL replay on open, producing a true multigraph so parallel and typed
  relationships survive a reopen.
- **Tier 2 CSR file format** (`store/csrfile`) — an `mmap`-backed,
  versioned, 64-byte-aligned CSR file with a validated header, a
  zero-copy `Reinterpret` helper, and a deterministic fixture generator.
- **Bulk loader** (`store/bulk`) — high-throughput ingestion that
  bypasses the WAL for initial loads.

#### Cypher engine

- **openCypher-compatible engine** (`cypher`) — a full
  parser → planner → executor pipeline with a plan cache,
  `EXPLAIN` / `PROFILE`, and dbhits accounting. WAL-durable writes are
  available via `NewEngineWithStore`.
- **Pipeline packages** (`cypher/parser`, `cypher/ast`, `cypher/sema`,
  `cypher/ir`, `cypher/plan`, `cypher/exec`) — the staged
  parse-to-execution components.
- **Built-in functions and procedures** (`cypher/funcs`,
  `cypher/procs`).
- **openCypher TCK harness** (`cypher/tck`) — the Technology
  Compatibility Kit runner. The execution suite is fully green at
  **3 897 / 3 897 scenarios** (16 006 / 16 006 steps); the regression
  gate (`tckExecutionBaseline = 3897` in `cypher/tck/runner_test.go`)
  rejects any change that lowers the passing count.

#### Bolt server

- **Bolt v5 protocol** (`bolt/proto`, `bolt/packstream`) — Bolt v5
  protocol handling and PackStream encoding (v5.0–v5.6 preferred, v4.4
  fallback).
- **TCP server** (`bolt/server`) — a Bolt server compatible with
  `neo4j-go-driver` v5 and `cypher-shell`, with TLS certificate
  hot-reload and graceful shutdown. `NewServer` fails closed when no
  authentication handler is configured.

#### Interchange

- **Import / export** (`graph/io/csv`, `graph/io/graphml`,
  `graph/io/dot`, `graph/io/jsonl`) — CSV, GraphML, Graphviz DOT, and
  JSON Lines formats.

#### Benchmarks and examples

- **Benchmark harnesses** (`bench/ldbc`, `bench/dimacs9`, `bench/rmat`,
  `bench/soak`, `bench/comparison`) — LDBC SNB, DIMACS 9 SSSP, RMAT
  power-law generation, a multi-hour mixed-workload soak harness, and a
  cross-library comparison.
- **25 runnable examples** (`examples/`) — covering the core API,
  persistence and out-of-core processing, the Cypher engine, the Bolt
  server, analytics, interchange, and end-to-end recipes. See
  [examples/README.md](examples/README.md) for the categorised index.

### Compliance

- **100 % openCypher TCK-compliant at the execution level** — every
  scenario in `cypher/tck/features/` passes, with no `failed`, no
  `undefined`, and no `pending` steps (3 897 / 3 897 scenarios).
- **100 % ACID-compliant** — Atomicity, Consistency, Isolation, and
  Durability hold across the in-memory engine and every persistence
  backend, verified by the WAL recovery tests in `store/wal` and
  `store/recovery` and the deterministic crash-injection battery in
  `internal/crashinject`.

### Notes

- **Pre-1.0 stability.** This is a `0.y.z` release. The public Go API
  may change without a major-version bump until `1.0.0`; pin the exact
  version you depend on.
- **Module path.** The Go module path is
  `github.com/FlavioCFOliveira/GoGraph` with no `/vN` suffix, which is
  Semantic-Import-Versioning-correct for a `0.x` line.

[0.5.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.5.0
[0.4.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.4.0
[0.3.2]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.3.2
[0.3.1]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.3.1
[0.3.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.3.0
[0.2.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.2.0
[0.1.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.1.0
