# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.12.0] — 2026-08-27

**192 commits** — 87 fixes, 51 features, 25 documentation, 18 test, 4 performance,
1 style, 1 chore, 1 build change, and 4 merges. **Six sprints delivered work in this
window — 345–350 — and 176 tasks closed.**

Three things define the release, and none of them is a new capability in the engine.

**A deterministic-simulation-testing campaign drove every published surface.**
Sprints 345–349 (110 tasks) took the DST harness in `internal/sim` from a
graph-and-storage exerciser to one that drives MVCC under true concurrency, the full
Cypher language surface, the storage/durability/MVCC substrate, the Bolt wire, and the
graph API, search and bulk-load surfaces. **50 of the release's 51 `feat` commits are
`feat(sim)`**, and `internal/sim` accounts for **127 267 of the 168 208 changed lines
(75.7 %)**. This is test infrastructure, not module functionality — the module's own
production code accounts for **8 564 lines across 93 files (5.1 %)**. What the campaign
delivers to a user is the defects it found: **26 module defects across those five
sprints, and 50 across the release once sprint 350's 24 are added.** Five of the
severity-9 ones were *silent* — wrong or lost data with no error anywhere.

**And the crash model was repaired before it was trusted.** The most consequential
result of the campaign is not a scenario but a control: until #2535, `SimDisk`
retained unsynced file data across a simulated host crash, so **the battery could not
fail an engine that stopped calling `fsync` at all.** With a durable-length watermark
in place, deleting the WAL commit fsync now fails loudly —
`checkpoint-crash-storm` reports **192 violations at seed 610621185, losing exactly
half of 384 acknowledged commits, and 33 top-level tests fail**, where before the
change it failed nothing. Every durability claim this release makes about the
simulator rests on that.

**The bug backlog was emptied.** Sprint 350 (66 tasks) resolved every known bug,
re-validating each premise at HEAD before working it. Of the 66: **26 landed a fix in
product code** (24 in the module, 2 in `examples/26_social_scale_bench`), **7 premises
did not hold at HEAD** and are recorded below as refuted or obsolete rather than
reported as fixes, and **33 changed only tests, the simulator, the gates, benchmarks or
documentation**. No `BUG`-type task remains in the backlog.

**Nothing breaks.** No commit carries a `!` marker or a `BREAKING CHANGE` footer, and
an audit of `git diff v0.11.0..HEAD` across the whole tree found **no exported
identifier removed, no exported signature changed, and no exported struct field
dropped**. **51 exported identifiers were added** across 15 packages, which is what
makes this a pre-1.0 **MINOR** release under [docs/semver.md](docs/semver.md).
`go.sum` is **byte-identical** to `v0.11.0`, so no dependency moved; the only `go.mod`
change is the pinned `toolchain` directive, `go1.26.5` → `go1.27.0` (see *Changed*).

The openCypher TCK gate is unchanged at **3897/3897** — 100 % execution-level
compliance is preserved, not extended. No `.feature` file changed in this window, so
the scenario population is the same one `v0.11.0` was measured against.

**Read [`release-notes/v0.12.0.md`](release-notes/v0.12.0.md) before upgrading.** There
are **no migration steps**, but a dozen observable behaviours changed and are listed
there.

### Added

#### Deterministic simulation testing — the campaign that found the defects

- **MVCC under true concurrency** (sprint 345). A deterministic multi-session mode
  (`RunMVCCSessions`) over the durable store, per-transaction MVCC workspaces over the
  graph oracle, MVCC isolation checkers for snapshot stability, read-your-own-writes
  and atomic visibility (rmp #2436), a lost-update and write-conflict adjudication
  scenario (#2437), crash and recovery with open MVCC transactions (#2438),
  concurrent-mode transactional roles with typed conflict accounting (#2439),
  during-run isolation oracles (#2440), and a production-profile scenario with crash
  cycles and transaction-granular durability (#2441).
- **The full Cypher language surface** (sprint 346). Access-path parity and
  plan-stability oracles via `Explain` (#2447); a per-tick `QueryCounters` oracle over
  the drained result (#2448); relationship `SET`/`REMOVE`/`DELETE` over parallel-edge
  instances (#2449); result-verified range, prefix and `IN` seeks in both spellings
  (#2450); pattern shapes unlocking the join/intersect planner family (#2451); grouped
  aggregation, `DISTINCT`, `UNION` and exact aggregate oracles (#2452); a
  null-semantics scenario that makes the three-valued-logic probes non-tautological
  (#2453); `FOREACH` write-path equivalence by oracle expansion (#2454); schema
  introspection and full constraint-kind adjudication (#2455); the statistics-planning
  regime through `db.stats.refresh` (#2456); temporal values stored, recovered and read
  back as temporals (#2457); entity-valued and pure-function probes (#2458);
  list-property predicates and entity map projection (#2459); ordering, pagination and
  multi-part query structure (#2460); the `MERGE` surface with its semantics pinned as
  measured (#2461); the `DELETE` contract, notifications, `YIELD` filter and the wire
  parameter matrix (#2462); and the variable-length path surface, oracle-computed
  (#2463).
- **Storage, durability and the MVCC substrate** (sprint 347). DDL across the
  checkpoint and snapshot boundary (#2464); a crash inside the checkpoint publish
  window (#2465); bulk-import publish parity with its uncovered faults documented
  (#2466); a snapshot component-corruption fail-stop battery (#2467); typed values and
  edge properties through the snapshot codec (#2468); MVCC clock and sequence recovery
  across a snapshot boundary (#2469); MVCC substrate telemetry read and adjudicated
  (#2470); group-commit coalescing and its fail-all path (#2471); WAL writer surface
  oracles with a constructed contiguity control (#2472); a key and weight codec matrix
  across crash and upgrade (#2473); transaction-size cap sentinels with the refusal
  proven inert (#2474); `store.DB` teardown under a cancelled context and concurrent
  closers (#2475); the checkpointer cadence on a fake clock (#2476); cross-release
  compatibility beyond the WAL (#2477); a `csrfile` access-pattern and weight-kind
  matrix with the alignment gate probed (#2478); required counters per fault scenario
  (#2479); and the `graph/io` codec surface with cap provocation and reproducibility
  oracles (#2480).
- **The Bolt wire surface** (sprint 348). The authentication surface with the WAL as
  witness (#2481); the transaction registry on a fake clock (#2482);
  `Server.Shutdown`'s drain and `Closer` ordering (#2483); streaming semantics —
  paging, `DISCARD` and the `qid` contract (#2484); `BEGIN` extras — bookmarks,
  `tx_timeout`, metadata, mode, `db` and `ROUTE` (#2485); the protocol version matrix
  with 4.4, 5.0 and 5.x side by side (#2486); and the aggregate inbound-decode budget
  with its two distinct nesting caps (#2487).
- **The graph API, search and bulk-load surfaces** (sprint 349). The bulk loader's full
  contract against an independent model (#2488); every public context-accepting
  `search` entry point under cancellation (#2489); index contents across the snapshot
  boundary (#2490); the lock-free CSR publisher's refcount lifecycle (#2491);
  `graph/query` as a differential read path against Cypher and an independent oracle
  (#2492); the typed schema as a runtime enforcement hook (#2493); the count store held
  to a model across recovery (#2494); the stateful `PageRanker`'s two published claims
  (#2495); the label index's scoped and range surface against a naive model (#2496);
  and min-cost flow's negative-cost regime with the hoisted-reverse Dijkstra (#2497).
- **The `SimDisk` crash model was overhauled for fidelity** (#2514, #2535, #2536,
  #2537, #2538, #2547), after an audit recorded in
  [`docs/audits/simdisk-crash-model-fidelity-2026-08-18.md`](docs/audits/simdisk-crash-model-fidelity-2026-08-18.md)
  with **15 findings, 12 of them filed as tasks**. A rename undo log with
  durable-prefix semantics; a **`durableLen` watermark, so unsynced file data is now
  discarded on a host crash**; `CrashHost` and `CrashProcess` split apart; unlink
  modelled as a non-durable directory mutation; a `DirSync` fault arm; and ghost
  directory entries pruned. The watermark's own cost was measured against the
  alternative: **+0.00 % bytes and allocations per operation**, against +97.64 % time
  and +99.94 % bytes for a full clone. The whole catalogue passes under `SOAK_FULL=1`
  with **36 of 36 scenarios and zero skips**.
- **Scenario-scoped required counters** (#2479): every fault scenario declares the
  counters its faults must move, so a fault that silently stopped firing fails the run
  instead of passing it.
- **A `VACUOUS_RUN` violation family** (#2614), so a run that never exercised its
  subject is reported as having proved nothing rather than as an `ORACLE_DEVIATION`.
  About 178 of 818 emitter lines migrated; `checkCheckpointStormNonVacuity` had to be
  **split**, because migrating it wholesale would have relabelled a real snapshot loss
  as "the run proved nothing".
- **Cross-release coverage now reaches the snapshot directory** (#2477, #2531). The
  harness previously imported only the WAL, recovery and transaction packages, so a
  prior release's snapshot directory had never been opened by current code. The tag
  list grows from two to four — `v0.1.0` as the oldest artefact in existence, `v0.2.0`
  and `v0.3.0` as the bracket around the WAL-replay gap, and `v0.11.0` because it is
  the release users actually upgrade *from*. All four publish a manifest-v3 snapshot
  that current code opens snapshot-only, with 52 labels and 65 properties arriving from
  the image, at a cost of 46 s across three arms. The forced WAL-only arm is a
  **required** arm, not a control: publishing a checkpoint truncates the WAL
  (`walOps` 682 → 0), so the specified fix alone would have moved the prior-release
  WAL-replay path out of coverage entirely.

#### Public API — every addition is additive

- **`store/recovery` now builds the transactional store it recovered** (#2522):
  `Result.NewStore`, `Result.NewStoreCapped`, binding the recovered graph and the
  derived sequence to a caller-supplied WAL writer. `recovery.Options` is unchanged, so
  every existing caller compiles untouched.
- **`store/recovery` surfaces the snapshot index payloads** (#2490):
  `Result.IndexPayloadFor`, `Result.WALSuffixTouchesNodeIndex`, `IndexPayload`, and the
  sentinels `ErrIndexPayloadNotFound`, `ErrIndexPayloadStale`,
  `ErrIndexPayloadUnreadable`. On the `cypher` side, `IndexPayloadSource`,
  `RecoveredIndexPopulation`, `Engine.RecoveredIndexPopulation` and
  `NewEngineWithStoreAndRecovery`.
- **Weight codecs reach every durable path** (#2526, #2529):
  `store/checkpoint.WithWeightCodec`, `store/bulkimport.PublishWithWeightCodec`,
  `store/snapshot.CaptureGraphWithWeightCodec`, `ApplyCSRToGraphWithWeightCodec`,
  `WriteCSRWithWeightCodec`, `WriteSnapshotFullWithWeightCodecCtx`,
  `CSRReadback.CodecWeights`, and the sentinels `snapshot.ErrWeightNotPersistable` and
  `snapshot.ErrWeightCodecRequired`.
- **New typed errors** where an untyped or misattributed one used to escape:
  `cypher.ErrParamNestedTooDeep` (#2570), `graph.ErrMapperKeyNotPortable` (#2528),
  `store/csrfile.ErrPublishedNotDurable` (#2581),
  `bolt/server.ErrCertOutsideValidity` (#2557). `cypher.ErrSerializationConflict` is
  re-exported from `graph/mvcc` so a caller need not import the substrate to test for a
  retriable conflict.
- **`graph/lpg.Graph.ValidateProperty`** (#2602) and the three pre-validated buffering
  entry points `store/txn.Tx.SetNodePropertyPreValidated`,
  `SetEdgePropertyPreValidated` and `SetEdgePropertyByHandlePreValidated`, for the one
  caller that has already validated the same value against the same graph.
- **`cypher/exec.CountRows`** with `NewCountRows` and the `Operator` methods (#2625);
  `cypher/exec.Merge.WithLeadingClause`, `Merge.WithOuterRelCols`,
  `MergePattern.WithLeadingClause`, `MergePattern.WithOuterRelCols` and
  `MergePattern.WithSchema` (#2510, #2511, #2512).
- **`cypher/ir.NameSubqueryAnonymousEntities`**, `ir.UserNamed`, `ir.IsSyntheticVar`
  and `cypher/ast.SyntheticSubqueryVarPrefix` (#2508).
- **`cypher.Engine.CountSnapshot`** (#2494), so the count store can be held to a model
  across recovery; **`graph/index.NodeSet.CanonicalBitmap`** (#2609);
  **`bolt/server.CertReloader.SetClock`** (#2557) and `Server.SetClock`.

#### On-disk formats — additive, with no version step

- **The snapshot manifest carries a CRC32C framing trailer** (#2520): sixteen bytes of
  magic, algorithm and CRC32C over every preceding byte, exposed as
  `snapshot.IntegrityCRC32CTrailer`. `ManifestVersion` stays **3**: old snapshots still
  open, and an old decoder stops at the closing brace and never reads the trailer, which
  is precisely why this shipped without a version step.
- **The manifest gains `indexes_commit_ts`** (#2490), the bounding instant that lets
  recovery prove an index payload is not stale. Written only on the quiesced
  checkpointer path, so every snapshot that already exists is never hydrated.
- **`csrfile` gains wire values 5 and 6** for the 1- and 2-byte weight kinds (#2529).
  Backward compatible in both directions that matter: 0–4 keep their meaning so every
  earlier file reads unchanged, and an earlier reader meeting a 5 or 6 refuses it with
  `ErrUnknownWeightKind` rather than misreading it. `CurrentVersion` stays **1**.

#### Gates and tooling

- **The per-package short-layer cost budget is now enforced** on `make ci` (#2599,
  #2577). `HARD_BUDGET` defaulted to 0 — which the script documents as disabled — and
  the only target that ran the script was absent from `ci`, so `internal/sim` drifted
  from 818 tests / 460.9 s to 974 tests / 570.1 s in five days, unremarked.
  `test-short` now pipes through `scripts/pkg_time_budget.sh`, so one run yields both
  outcomes. The global 240 s ceiling is **not** relaxed; two named overrides are derived
  by one stated rule — worst in-suite figure ever recorded × 1.25, rounded up to the
  minute: `internal/sim` 602.9 s → 780, `cypher` 276.4 s → 360.
- **`SHORT_TIMEOUT ?= 30m`** (#2584). The `-race` pass was the only whole-suite run in
  `make ci` without an explicit `-timeout`, so it relied on Go's 600 s default and died
  twice on machine load rather than on code, with `internal/sim` at 600.705 s and
  601.675 s. The value is a backstop against a genuinely hung package, at 3.05× the
  slowest package that completed.
- **`testlayers.RequireQuietMachine`** and a serial `make test-timing` phase (#2517),
  since retired for the three gates that no longer need it (see *Changed*).
- **A standing power control beside each timing gate** — a workload that models the
  defect and must make the gate fire, because the pre-fix build cannot be rebuilt and a
  gate that has quietly lost its power reads exactly like one that is passing
  (#2571, #2572, #2574, #2589).

### Changed

#### Behaviour a caller can observe

- **`UNION` inside an `EXISTS { }` or `COUNT { }` subquery body is now refused**
  (#2615). It previously parsed cleanly and the second branch was **silently dropped**:
  measured on a node with a `:W` edge and no `:Z` edge,
  `EXISTS { MATCH (x)-[:Z]->() RETURN 1 UNION MATCH (x)-[:W]->() RETURN 1 }` returned
  `false` where the second branch matches. Refusing rather than supporting was decided
  with the user, and the divergence is stated rather than hidden: **both reference
  engines answer this query**, read from their grammar source — Neo4j's
  `existsExpression`/`countExpression` admit `regularQuery`
  (`Cypher5Parser.g4:33-35, 671-677`) and Memgraph's `existsSubquery`/`countSubquery`
  admit `cypherQuery` carrying `cypherUnion` (`Cypher.g4:73-81, 317-323`). The
  openCypher 9 TCK does not cover subquery expressions at all — zero occurrences of
  `EXISTS {` or `COUNT {` across every feature file — so neither choice can move the
  conformance count. Support is filed as #2627.
- **Relationship-variable uniqueness is scoped to the clause, not to one path pattern**
  (#2252). `MATCH (a)-[r]->(b), (b)-[r]->(a)` was accepted and returned one row with
  count 0; it is now refused with `SyntaxError.RelationshipUniquenessViolation`, as the
  equivalent single-pattern spelling always was. `OPTIONAL MATCH`, `CREATE` and an
  `EXISTS` subquery pattern inherit the clause scope. Reuse *across* clauses stays
  legal. The error text moves from "cannot be used twice in the same path pattern" to
  "in the same clause".
- **An unrecognised Bolt `BEGIN` access mode is refused** (#2564) with
  `Neo.ClientError.Request.Invalid`, in a message naming the offending value and, for a
  non-string, its Go type. `"R"`, `"read"` and a non-string previously fell through to
  the write default, so a client that asked for read-only silently received write
  authority. The two canonical spellings and the absent key behave exactly as before.
- **A failed Bolt `LOGON` terminates the connection, whichever branch it took**
  (#2556). See *Security*.
- **Bolt status codes changed on three paths.** A per-principal quota refusal now
  answers `Neo.TransientError.Transaction.MaximumTransactionLimitReached` and leaves the
  session `READY` (#2561) — the previous `Neo.ClientError.General.LimitExceeded` does
  not appear in Neo4j's status codes at all, and its `ClientError` class instructs a
  driver **not** to retry, which is the opposite of what a self-freeing cap wants. An
  operator-initiated termination now answers
  `Neo.ClientError.Transaction.Terminated` (#2560) instead of
  `TransactionTimedOut`. An over-nested parameter now answers
  `Neo.ClientError.Statement.ArgumentError` (#2570) instead of
  `Neo.DatabaseError.General.UnknownError`.
- **`metricTxTimedOut` counts the total-lifetime bound alone** (#2560). It was
  incremented unconditionally by the shared teardown, so an idle reap and an operator
  termination were each also counted as a timeout, making the counter a superset of all
  three events and defeating `metricTxIdleReaped`'s own godoc. Measured on the unfixed
  build: `tx.terminated=1` **and** `tx.timedout=1` for one termination. The three are
  now disjoint.
- **A Cypher list crossing the Bolt wire is a PackStream List** (#2513). Every list
  previously reached clients as a **String**: `collect()`, `labels()`, `keys()`,
  `nodes(p)`, `relationships(p)`, list literals and list-valued properties were all
  affected, and `nodes(p)` additionally lost every node structure. Parameter binding was
  never at fault. A client that parsed the stringified form must stop.
- **An autocommit statement's terminal `SUCCESS` carries a fresh bookmark, and a
  statement inside an explicit transaction carries none** (#2563). It previously carried
  whatever `s.bookmark` held: empty on a session that had never committed explicitly,
  and the **prior** explicit `COMMIT`'s token verbatim on one that had — so a driver
  chaining a causally-consistent read was pinned to a strictly older transaction than
  the one it had just run. The Bolt specification puts the bookmark on the `COMMIT`
  `SUCCESS` and qualifies the stream's terminal one as "(Autocommit Transaction only)".
- **A schema validator refuses at buffer time, not after the fsync** (#2602). A
  `store/txn` write of a value the installed validator rejects now fails in
  `Tx.SetNodeProperty` / `SetEdgeProperty` / `SetEdgePropertyByHandle` rather than
  reaching `Commit` and returning `ErrCommittedNotApplied`. An installed validator is
  therefore a durability gate on both write paths, which
  `graph/lpg/validator.go` now records. A log written by an older build is outside that
  guarantee.
- **A snapshot write fails rather than publishing a weightless image** (#2526). A graph
  whose weights the codec cannot encode now returns
  `snapshot.ErrWeightNotPersistable` before the header is written, so the checkpoint
  returns before the WAL-prefix truncation and the one surviving copy is never
  destroyed. Reads are symmetric: a missing codec refuses rather than applying zeros
  over weights that are on disk.
- **`csrfile` accepts the weight kinds it advertises** (#2529). `int`, `uint` and
  `uintptr` move from an untyped `binary.Write: some values are not fixed-sized` leaking
  from a dependency to persisting; `int8`, `uint8`, `bool`, `int16` and `uint16` move
  from `unknown weight kind` to persisting; an unsupported type is refused with a typed
  error that names it (`csrfile: unknown weight kind: string`). **Platform-dependent
  widths are accepted deliberately**, on the user's decision: `int`, `uint` and
  `uintptr` persist at 8 bytes to match `store/snapshot`, so such a file written on a
  64-bit build would be misread by a 32-bit one. A caller needing a file that crosses
  word sizes should use an explicitly-sized weight type.
- **Six `search` entry points now honour a cancelled context** where they previously ran
  to completion (#2593). A caller that relied on the old behaviour will start seeing
  context errors. See *Fixed*.
- **`graph/query` orders and compares integers and floats in one numeric order**
  (#2600, #2601). `WithRange` with `Float64Value` bounds now matches an
  `Int64Value` property, and `WithProperty("age", Float64Value(5))` now matches a node
  whose `age` is `Int64Value(5)`. A query that previously returned nothing returns
  matches. The comparison is **exact, not a promotion**:
  `4611686018427387905` and `4611686018427387900` stay distinct, because both round to
  the same `float64` at 2⁶². The identity between an equality and a degenerate range is
  scoped to the **orderable** kinds — openCypher's equatability is wider than its
  comparability, so `BOOLEAN`, `BYTES` and `TIME` are equal to themselves while no range
  over them holds. **One contract widens:** a bound `float64`-keyed btree covering a
  `(label, property)` pair must now key every numeric value of that property, integers
  included. The engine's companion satisfies it by construction; a hand-built
  float-only index would under-return.
- **`Mapper.LoadFrom` names an unportable key type** (#2528). A restore that fails
  because a key contains a pointer, `unsafe.Pointer` or channel now reports
  `ErrMapperKeyNotPortable` wrapped inside `ErrMapperEntryCorrupted`, naming the
  offending field path (`key.P`, `key.Inner.P`, `key.A[0]`). It inspects the **value**,
  not only the static type, because an interface field's dynamic type is what decides.
  A portable key with a genuinely wrong recorded shard is still reported as corruption.
- **A `csrfile` publish failure after the rename is distinguishable** (#2581).
  Behaviour is unchanged; what changes is that the caller can tell the two apart.
  `ErrPublishedNotDurable` wraps the parent-directory fsync error at the one site that
  fails after the rename, and both godocs name the two sound responses (retry the
  fsync, or fail the process) and the unsound one (treat it as "not published").
- **Recovery may hydrate an index from the snapshot payload instead of rebuilding it**
  (#2490). Hydration requires a self-sufficient snapshot, a CRC-valid payload, and no
  newer WAL-suffix write to that index's label and property; otherwise the index is
  backfilled exactly as before. A corrupt payload falls back to a rebuild per index
  rather than fail-stopping, because an index is derived state over an
  already-integrity-checked graph.

#### Query plans — result-identical, cost changed

- **The parallel scan adopts the min-cardinality label anchor instead of pre-empting
  it** (#2431), closing the regression `v0.11.0` shipped as open. See *Performance*.
- **The range-seek population floor moves 1024 → 64** (#2367), set from measurement.
  A floor is **kept** rather than removed, because below it no count can change the
  verdict and `rangeSeekBudget` therefore refuses to take one — the same "a gate must
  cost less than the decision it informs" rule #2380 and #2392 imposed on the
  parallel-scan gate.
- **The single-edge anchor-swap peephole declines any site whose endpoint name is
  empty** (#2603). This is the correctness fix for the anonymous-head wrong answer, and
  its cost is large and is stated in *Performance* rather than minimised. #2604 is filed
  to recover the optimisation properly.
- **A group-by-less, non-`DISTINCT` `count(*)` counts the child's rows** through the new
  `exec.CountRows` (#2625) instead of materialising one single-column row per input row
  to carry a constant. It is tried **after** every existing count pushdown, because
  those answer in `O(1)` from a maintained counter while this still visits every row —
  it only stops *building* rows nobody reads. `count(v)` is excluded deliberately: it
  counts non-null bindings, so its argument must still be evaluated per row.

#### Documentation corrected against the code

- **`docs/release.md` no longer claims two security controls the project does not have.**
  Its branch- and tag-protection policy asserted, as fact, that release tags are
  `git tag -s` signed and that signed commits are required. **Neither is in force:** no
  signing key is configured on the release workstation, `v0.10.0` and `v0.11.0` are
  unsigned annotated tags, and `git log --pretty=%G?` reports `N` for every commit in this
  window. Both are now stated as **intended controls that are not yet enforced**, with what
  adopting them would require, because a security document asserting a control it lacks is
  worse than one that admits the gap. The stale `toolchain go1.26.3` claim on the same page
  is corrected to `go1.27.0`.

- **PageRank's L1 delta is not deterministic across worker counts** (#2605). The godoc
  claimed it was, on the reasoning that per-worker partials are reduced in fixed
  worker-id order — which makes the reduction deterministic *for a given count*, while
  the *number* of partials changes with the count and floating-point addition is not
  associative. Reproduced at HEAD on a fresh 100 000-element fixture: reducing identical
  values over 1, 2, 3, 4, 8 and 10 partitions gave **six distinct results, up to 58 ULP
  apart**. A third over-claim was found while correcting the first — the Parallelism
  summary asserted the result is bit-for-bit identical regardless of `GOMAXPROCS`, which
  inherits the same caveat because the delta drives the convergence test and therefore
  the iteration count. Measurement does not contradict it — **17 280 serial-versus-
  parallel comparisons across 40 seeds, 4 dampings, 9 tolerances and 12 worker counts
  produced zero divergences**, as did a 400-seed sweep — but an absolute statement
  resting on a probabilistic argument is now stated as empirical. The reduction is
  untouched; callers needing bit-reproducible scores across differing worker counts are
  told to pin `GOMAXPROCS`.
- **The `csrfile` crash-durability claim is scoped to where the barrier exists**
  (#2582). `WriteToFile` asserted unconditionally that a nil return means the published
  file survives process crash, host crash and `kill -9`; outside the unix build set
  `parentDirFsync` is an unconditional no-op. The contents are fsynced before the rename
  on every platform, and the rename's directory **entry** is made durable on
  linux/darwin/freebsd/netbsd/openbsd. **No claim is made about the other platforms**,
  which is the point rather than an omission — the mirror `parent_fsync_other.go`
  asserted *positively* that on Windows the dirent becomes durable once the filesystem
  commits its log, citing LMDB, SQLite and RocksDB; that is the same unmeasured
  inference with its sign flipped and it is removed.
- **`Graph.EnableLabelDeltas` and `EnablePropDeltas` are no-ops on a graph from `New`**
  (#2623). Their godoc said the mechanism is "inert unless `EnableLabelDeltas` has been
  called"; `Graph.armMVCC` sets the flags and runs by default.
- **`docs/test-layers.md`** lost three false claims (#2566) — the pipeline did not
  invoke the budget gate, `HARD_BUDGET` defaults to 0, and the override variable is set
  nowhere — and now states the measured short-layer budget in one owning place (#2585),
  with a regression test that fails if the owner stops carrying the figures. The rule
  that produced a whole family of gate defects — "assert on process CPU time, it is
  load-invariant" — is corrected: CPU time was measured at **2.90× under `make ci`
  against 0.94× solo**, because contention charges real CPU through scheduler, cache and
  TLB pressure.
- **`docs/dst.md` and `docs/dst-feature-coverage.md` were re-measured, not asserted**
  (#2498). Simulator-driven whole-module coverage moved **54.5 % → 71.4 %** raw, but
  `-coverpkg=./...` includes `internal/sim` itself and the harness grew from 8 619 to
  30 726 statements while sitting at ~84 % covered, so the number that answers the
  sprint's question is the one over the **product surface on a near-stable denominator**
  (60 246 → 61 405 statements): **50.7 % → 65.1 %, +14.4 pp**. Both
  acceptance-criteria packages came off zero — `graph/lpg/schema` 0.0 % → 61.0 % and
  `graph/index/stats` 0.0 % → 56.2 %. The baseline run reproduced the inherited 54.5 %
  exactly, which is what makes the delta trustworthy.

#### Gates — the instrument changed, the threshold did not

- **The pinned toolchain moves `go1.26.5` → `go1.27.0`** in `go.mod`. Only the
  `toolchain` directive changed; the `go` directive stays at **`go 1.26`**, so no new
  language feature is adopted and the pre-1.0 semver-MAJOR consideration the `go`
  directive triggers does not arise. `go.sum` is untouched and `go mod tidy` leaves the
  tree clean, so no direct or indirect dependency required a newer minor. The whole
  correctness gate for this release — `go test -race ./...`, the TCK baseline, lint and
  the coverage gate — was run under `go1.27.0`, which is also what
  [`docs/benchmarks/v0.12.0.md`](docs/benchmarks/v0.12.0.md) was measured on. **The
  consequence for comparison is stated there rather than glossed:** `v0.11.0` was
  measured on `go1.26.5`, so a cross-release benchmark delta mixes a compiler change
  with GoGraph's own, and none is attributed to the module on that basis.

- **The delete-degradation gates measure process CPU time, then allocation volume**
  (#2571, #2589). The wall-clock form made `make ci` red on a flat engine at 3.25× with
  the last cycle *dipping* (2.376 s → 2.074 s), a load shape no algorithmic regression
  can produce: a ratio cancels *constant* load, not load that changes during the run,
  which is exactly what `make ci` does as sibling packages start and finish. The final
  instrument is allocation volume, whose power against the actual defect was
  *established* rather than assumed — flat workload 0.87×, degrading workload 8.66×, a
  10× separation with the unchanged 2.5× threshold between them. `RequireQuietMachine`
  is consequently **removed** from all three gates, which return to asserting in
  `test-short` on every push.
- **The hub-scaling gate measures allocation volume** (#2572), which is also the
  faithful instrument: the defect it catches is exact-fit reallocation, whose signature
  is copy volume. The prescribed process-CPU remedy was refuted by measurement. The
  threshold is unchanged at 40; measured ratio 11.17/11.15/11.15 over three reps, a
  0.2 % spread, and 12 of 12 runs pass under 300 named spinners against 6 of 12 failures
  for the old instrument.
- **The MVCC gate asserts an ordering, not a duration** (#2574). Widening the margin was
  *impossible*, not merely inadequate: `holderTenure` is 500 ms and a 60× ceiling of
  601 ms exceeds it, so a caller that blocked for the holder's whole tenure — the #2174
  defect, a 232× overrun reported as `err=nil` — would have **passed**. The property was
  never "returned inside 105 ms"; it is "abandoned the wait rather than blocking for the
  tenure", which is an ordering fact with no margin to tune.
- **The openCypher TCK gate no longer reports a slow host as a conformance loss**
  (#2568). A scenario exceeding the 10 s `queryTimeout` stored
  `context.DeadlineExceeded` and lowered the count `tckExecutionBaseline` gates on — the
  most serious false signal this project can emit. The discriminator is **not elapsed
  time**, because no duration separates a slow host from a hung engine; it is whether
  the engine **honoured cancellation**. A prompt `DeadlineExceeded` is `INCONCLUSIVE`
  and is not scored as a conformance failure; a `RunAny` that does not return within
  `queryWedgeGrace` is `WEDGED` and is never credited. `queryTimeout` is unchanged at
  10 s. The credit is bounded to scenarios the harness recorded as timing out, every one
  is rendered, and it cannot excuse a real shortfall.
- **`internal/tmphygiene` requires exact registration for coverage and keeps containment
  for attribution** (#2586). None of the 33 call sites was passing only by nesting, so
  the defect was latent.
- **`scripts/cover_gate.sh` reclaims its temporaries when cancelled** (#2549). The filed
  premise was refuted by measurement — bash *does* run the `EXIT` trap on
  `SIGTERM`/`SIGHUP` when blocked in `wait`; the real cause was `go test` running in the
  **foreground**, where the trap is deferred until the command finishes and the harness
  `SIGKILL` arrives first. Reclaiming the stranded 248 840 121-byte file took the tree
  from 2.4 GB to 791 MB.

### Removed

- **No exported identifier.** The public API is a strict superset of `v0.11.0`'s.
- Two tests proven to have no failure path at all (#2617):
  `cypher.TestUsingIndexHint_NotImplemented`, a bare `t.Skip` whose divergence is
  already recorded in [`docs/tck/DIVERGENCES.md`](docs/tck/DIVERGENCES.md), and
  `internal/goldens.TestUpdateRequested_EnvVar_Unset`, which discarded its result and
  leaked an `os.Unsetenv` into its siblings. **Gate time saved: zero measurable** — the
  premise that the full gate is slow through accumulated vacuous tests is not supported,
  and mechanical vacuity detection is declared untrustworthy: a naive scan flagged 652
  candidates and per-candidate verification left 2, so final-shortlist precision was
  2 of 16. The worst false positive would have deleted infrastructure —
  `TestBreakpointSelfKillChildMarker` has an **empty body** and is the `-test.run`
  target two crash-injection tests re-exec the binary with.
- The `store/csrfile` FS seam's two now-orphaned methods, verified to have no callers
  first, with `Truncate` moving from the `fs` interface to `File` (#2580).
- The chain-level `primordial` flag on the life store, replaced by
  `aliveBefore(born, died)` (#2445).
- `RequireQuietMachine` from the three delete- and hub-scaling gates (#2589, #2572).

### Fixed

#### Correctness — silent wrong answers, and one panic

- **A bit-packed BOOL edge-property column panicked on the fused append path**
  (#2493). `edgePropColumn.grownWithValue` and `grownAbsentShared` converted a dense
  column to sparse unconditionally, but a `PropBool` column is packed bits and **has no
  sparse form**. Reachable from the public API with three nodes and two calls — no
  concurrency, no store, no schema:
  `SetEdgeProperty("a","b","flag",BoolValue(true))` followed by
  `AddEdgeLabeledWithProperty("a","c",1,"REL","n",Int64Value(7))` panicked with
  `index out of range [n] with length 0` inside `toSparse`. It was invisible because the
  guard already existed in two **sibling** functions, so the invariant looked enforced.
  Each half of the fix is proven load-bearing by controlled revert. Found because a new
  DST scenario died on its first run.
- **The single-edge anchor swap dropped every row of an anonymous-head pattern**
  (#2603). `MATCH (:Person)-[:KNOWS]->(:Vip) RETURN count(*)` returned **0** where the
  named-source spelling returned 1, on the shipped default configuration, with no error
  and no notification. openCypher requires the rows: `Match2.feature` scenario [2] pins
  `MATCH (:A)-[r]->(:B) RETURN r` to one row.
  The mechanism is why the same pattern is correct *without* the swap. The written plan
  enforces the head's label **structurally**, in the access path — `NodeByLabelScan`
  needs no variable name to scan a label. The mirror **relocates** that label onto a
  `Selection{LabelPredicate(fromVar, …)}` above the re-rooted expand, and a predicate can
  only reach a node **through its name**. `ir.matchNodeScan` leaves the pattern head's
  `NodeVar` empty while every non-head anonymous node is given a synthetic `__anon_N`,
  so the relocated receiver resolved to no column, `evalLabelPredicate` returned NULL,
  and `Filter` dropped every row. Mis-resolution is ruled out rather than assumed: the
  fixture's destination is `(:Person:Vip)`, so had `Variable{""}` fallen back to column
  0 the `(:Person)` predicate would have been TRUE and the row kept.
  Teaching the mirror to *name* that endpoint was rejected on two grounds, not on
  effort: `mirrorAnchorSite` is a pure IR function holding only the site, so it cannot
  mint a provably collision-free name, and the peephole's published correctness argument
  is that the mirror is **byte-identical** to the plan the translator emits for the
  openCypher-equivalent pattern — a synthetic-name mirror is a plan the translator can
  never emit, so proof-by-construction would stop covering the anonymous case.
  **Why nothing caught it:** the TCK scenario cannot reach the peephole for two
  independent reasons, each sufficient — its relationship is untyped while
  `matchAnchorSite` requires exactly one type, and its graph is balanced so the 2× cost
  margin is unreachable (both confirmed by `anchorSwapBuildCount` reading 0). And the
  optimisation's own 24 test patterns all used **named** variables and never
  `count(*)`, so their differential oracle only ever compared the spelling that was
  already right. That gap is closed with anonymous-endpoint arms, which matters more
  than the fix.
- **`MERGE` contradicted `MATCH` inside one transaction and created a duplicate**
  (#2365). `MERGE` decided whether an existing node matched its pattern by reading that
  node's labels and properties **raw** — bare shard reads returning the newest stored
  value, including other in-flight transactions' uncommitted writes. Reachable because
  conflicts are per **substore**, so a transaction writing the label never collides with
  one reading it to make a match decision. The predicted direction (a peer's uncommitted
  *add* making `MERGE` match) does **not** reproduce, because the enumeration feeding the
  filter is already view-resolved; the **mirror** does — a peer's uncommitted label
  *removal* hid a node that carries the label in every committed state:
  `MATCH (n:Target {k:'y'}) RETURN count(n)` answered 1 while
  `MERGE (n:Target {k:'y'})` duplicated. Probing after the label half was fixed found the
  **property half has the identical exposure at the same three call sites**, and it is
  fixed here too. Where the peer's write and `MERGE`'s `ON MATCH` action touch the same
  substore, `MERGE` now correctly takes `ON MATCH` and the action meets a real
  serialization conflict — refused, not duplicated, which is what the ACID contract asks
  for.
- **A `MERGE` action on an outer relationship wrote to an unrelated node** (#2515).
  Two identity namespaces share one representation: a relationship rides in a row as a
  bare integer holding its stable handle, and the node resolver converts any integer to
  a node id unconditionally. The node-only merge operator asked *is this a node* first,
  so whenever a node existed whose id equalled the relationship handle, the action wrote
  its property to that node — and reported one property set, so the counters oracle was
  structurally blind. **A misdirected write, not a lost one.** It was never flaky: under
  `-count` it failed three times in five hundred, always the same iterations,
  byte-identical across separate processes, and a parallelism sweep from 1 to 32 gave
  exactly the same three at every level, which is what ruled concurrency out. The
  varying input is a process-global synthetic-key counter that decides node ids. The
  regression tests **construct** the collision — raising the handle counter until it
  reuses a decoy node id — and verify that precondition instead of hoping for it.
- **`MERGE` fired when its driving clause produced no rows** (#2512), writing data into
  the graph. Eighteen cells: a `MATCH` that matches nothing, `WITH` followed by a false
  filter, `UNWIND` of an empty list, a `WHERE` excluding everything, zero-row drivers
  carrying `ON CREATE` or `ON MATCH`, and `FOREACH` over an empty list in both merge
  forms. The diagnosis needed one correction: the empty-row fallback was never required
  for a leading clause, because the plan builder already substitutes a single-row leaf
  when the IR child is nil — so the fallback was reachable **only** in the defective
  case, which is why the leading-clause path is byte-identical now that plan shape,
  rather than row arrival, decides. No TCK scenario drives a `MERGE` from a zero-row
  clause, which is why a green conformance suite never saw it.
- **A `MERGE` action targeting a variable bound by a preceding clause was silently
  dropped** (#2511): the statement committed, the counters came up short by exactly one
  property, and the value read back null. **Thirty of thirty-six matrix cells.** A third
  operator was involved that the first diagnosis missed — the node-only `Merge` already
  resolved an outer *node* target through the full row schema but lost an outer
  *relationship* target, because that resolver understands only node values. Two adjacent
  problems are fixed in passing: a zero-row driver left the target column unset and the
  node resolver dereferenced a nil interface, crashing the statement; and the counters arm
  added for #2510 was unreachable because its family was never listed in the expectation
  switch.
- **A whole-entity relationship action was discarded on the `MergePattern` path**
  (#2510). `MERGE (a{..})-[r:T]->(b{..}) ON CREATE SET r += {...}` created the
  relationship and dropped its properties without error — **20 of 24 cells**; only
  pre-bound endpoints with an all-literal map ever worked. Two narrowings compound: the
  endpoint-boundness gate routes any unbound pattern to `MergePattern` whatever the
  right-hand side, and a second independent check rejects any parameter even when the
  endpoints *are* bound. Routing these shapes to `MergeRelationship` was **refuted**
  rather than assumed: its contract requires both endpoints bound, so 16 of the 20 broken
  cells can never reach it.
- **Reverse and undirected traversal returned another edge's data** (#2504). Over a
  reciprocal pair — both `A->B` and `B->A` present — `MATCH (b)<-[r]-(a)` reported the
  right `id(r)` but the **reciprocal** edge's type, properties, `startNode` and
  `endNode`; undirected returned two rows with distinct ids carrying the same properties.
  The suspected reverse-expansion mapping was **not** the cause: `revtofwd` is
  handle-exact. `exec.Expand` emits its triple in **traversal** order with no direction
  flag, so the hydrator re-derived storage direction by probing existence —
  `HasEdge(src,dst)` versus `HasEdge(dst,src)` — which is undecidable exactly when both
  directions exist. `relStoredInverted` decides by handle instead. A second site, the
  `WITH`-barrier whole-entity projection, carried an older no-labels-and-no-properties
  probe that a reciprocal pair passes trivially, and was found only after reinstating the
  simulator arm. Forward reads and non-reciprocal edges were always correct, which is why
  3897 green TCK scenarios and every existing fixture missed it. `Merge5.feature` pins the
  contract in-tree: an undirected match reports the **stored** orientation, independent of
  traversal. Measured 2.13 % faster, allocations identical.
- **The `UNWIND` pattern-comprehension route had drifted five defects from the `MATCH`
  baseline** (#2505). `RETURN` and `WITH` hoist a pattern comprehension into a
  `RollUpApply` so it executes on the real `Expand`; nothing hoists for `UNWIND`, where
  the comprehension survives to a separate fallback evaluator. On a reverse leg the walk
  advanced to the anchor instead of the neighbour, and the endpoints came back transposed
  against the stored-orientation contract; independent of direction, the relationship
  value carried no identity so `id(r)` was 0, its properties were read per **pair**
  rather than per handle — parallel siblings reporting the last writer's values — and the
  type filter was per pair too, so `[r:KNOWS]` over a `KNOWS`/`LIKES` pair emitted two
  rows where `MATCH` emits one. The simulator fixture then found two more: an incoming
  comprehension over a self-looping node returned nothing, and the same skip made the
  existential `WHERE (s)<-[:R]-()` disagree with both `MATCH` and `EXISTS` on the same
  graph.
- **A projection-position `EXISTS { }` / `COUNT { }` with an inner filter was constantly
  false or zero** (#2507), while the same subquery unfiltered, and the `WHERE`-position
  form, were correct. **The reported cause was wrong.** Instrumenting the inner `Filter`
  closure showed the node hydrating correctly: `StripLiterals` hoists **STRING** literals
  in `MATCH` and `WHERE` onto auto-parameters, and `compileSubAST` passed a nil parameter
  map to `buildOperator`, so the auto-parameter evaluated to NULL and the comparison
  dropped every row. Numeric literals are never hoisted — which is exactly why an age
  filter always worked and a name filter never did. The nil build options were real but
  explained different symptoms: an inner `r.prop` or `type(r)` had no edge metadata, a
  nested subquery had no sub-evaluator, and a pattern predicate had no pattern evaluator.
  `forSubquery` builds the child context from an explicit allowlist and **fails closed**,
  deliberately withholding the parallel fields because the sub- and pattern-evaluators are
  not concurrency-safe. No TCK feature file uses a braced `EXISTS` or `COUNT` subquery,
  and none uses a `WHERE`-position pattern predicate with an inline string property.
- **The parallel-edge family: four routes reported or wrote the wrong instance's
  state** (#2500, #2501, #2502, #2503). Relationship `REMOVE` reported
  `PropertiesRemoved=1` only for the *first* removal of a `(src,dst)` pair, because both
  mutator adapters gated the counter on the aggregate pair probe while the mutation ran
  per handle (#2500); five more routes did the same for `SET x = null`, a literal map, a
  `FromParam` map and two merge-action paths (#2501); the replace teardown enumerated only
  the aggregate pair store's keys, so a key living only in the targeted instance's
  by-handle bag **survived** a replace whose contract is "the resulting map equals exactly
  the given map", and `resolveEntityBinding` dropped `relHandle` for a
  `RelationshipValue` that crossed a `WITH` boundary (#2502); and every source-side
  whole-entity copy read the aggregate per-pair map when its source was a bound instance,
  so keys carried only by the twin leaked into the copy target — all five routes plus the
  `applyExprValue` `RelationshipValue` arm, red-reproduced 18 of 18 on both write paths
  (#2503). Graph state was correct in #2500 and #2501 — the effect report lied; in #2502
  and #2503 reads returned wrong data.
- **A write transaction's own-writes view leaked to every reader through the CSR and
  edge-type-filter caches** (#2446). Both caches key on `(epoch, startTS)` with no
  transaction identity, while the pair is built as-of `(startTS, snap.TxID())` — so a
  write transaction querying after its own edge create cached a CSR containing its own
  pending arc, and every reader at the same instant was served that private view: CSR
  positions shifted, the position-keyed type filter mislanded, and **committed edges lost
  their types for every reader**. Found by the DST multi-session mode as seed 16's
  typed-count flip at the exact tick of an unrelated pending create. Pure readers carry
  `Snapshot.TxID() == 0` and identical visibility at identical `(epoch, startTS)`, so
  `viewCarriesOwnWrites` gates both caches: an own-writes view builds fresh and is neither
  stored nor served, and pure-reader caching — the hot path — is untouched.
- **`graph/query`'s index seek and its scan fallback disagreed on mixed-kind numeric
  bounds** (#2600). With an `Int64Value` property under `Float64Value` bounds the seek
  returned the match and the scan returned nothing, while the Cypher path and an
  independent model oracle both returned it. The direction was settled from the project's
  own conformance authority: `Comparison2.feature`'s "Comparing across types yields null,
  except numbers" sweeps all 90 ordered cross-type pairs and keeps exactly the four
  INTEGER/FLOAT rows; `Comparison1` has `1 = 1.0` as true; CIP2016-06-14, adopted and
  normative, has INTEGER × FLOAT as the only off-diagonal entry in its comparability
  matrix. So **the scan was the defect**. Fixing the comparison alone would not have made
  the arms agree, and that is the part worth reading: `seekIndexablePreds` marked a served
  predicate so the scan **skipped** it with no residual filter, and the companion index is
  `float64`-keyed — so above 2⁵³ the seek would have started over-returning at the moment
  the scan stopped under-returning. The seek now reports whether it was exact, and an
  inexact one narrows the working set without discharging the predicate.
  `btreeRanger[int64]` is removed, and **not because it was dead**: once the range
  comparison unifies the two numeric kinds, an `int64`-keyed btree is a **subset** of the
  answer, and a subset cannot be repaired by a residual filter.
- **`WithProperty` equality and a degenerate `WithRange` disagreed on the same data**
  (#2601). After #2600 unified the range order, `WithRange(5, 5)` matched an
  `Int64Value(5)` under a float bound while `WithProperty("age", Float64Value(5))` did
  not. `equalValue` now routes a numeric pair through the same exact comparator. The
  relation is **equality, not equivalence** — NaN is equal to nothing, including itself —
  so the constraint canonical-key path is deliberately not reused.
  `hashLookuper[int64]` and `hashLookuper[float64]` are removed for the #2600 reason; no
  engine-created hash index is numeric, so nothing the engine builds lost a seek.
- **`count(*)`'s first implementation shipped a wrong answer, and it is recorded**
  (#2625). It installed the post-aggregation schema through the **unguarded** installer
  the leaf pushdowns use — safe there only because nothing is built below a leaf. Over an
  arbitrary child whose `Selection` operators hold closures over `bopts.scalarCols`,
  tagging the output column scalar when its alias **shadows** a pattern variable made
  those `Selection`s read the bound node's column as a scalar and drop every row:
  `MATCH (n {name:'A'})-[:LIKES]->(m {name:'B'}) RETURN count(*) AS n` returned 0 while the
  same query aliased `AS c` returned 1. Three existing `TestEdgeTypeFilterCache_*` tests
  caught it; `installAggOutputSchema`, which carries the alias-shadow guard, is what it
  uses now.

#### ACID — Consistency, Isolation and Atomicity

- **A value the installed schema validator refused became durable and was materialised
  by recovery** (#2602). `Tx.Commit` appended and fsynced every buffered op and only then
  applied them through `lpg`, where the validator hook lives, so the rejection arrived
  **after** the frame was on disk: `Commit` returned `ErrCommittedNotApplied`, the live
  graph stayed clean, and the reopen — which installs no validator — replayed the refused
  value straight in. Measured: an `age` declared `PropInt64` came back from a host crash
  as a **STRING**. That breaks the Consistency half of the ACID contract, which names
  label/property typing among the invariants a committed transaction must leave satisfied.
  The cost flagged when recommending the fix **did** bite, and finding out how is the more
  interesting half: the Cypher adapter calls `a.w().SetNodeProperty` (which validates) and
  then `a.tx.SetNodeProperty`, so every engine write validated **twice** — and a
  `SchemaValidator` is permitted to be stateful, so a counting validator that rejects the
  second write of a key saw the wrong write refused. Worse, the adapter **discarded** that
  call's error (`_ =`, annotated "ErrTxFinished impossible here" — a premise this change
  falsified), so the new guard fired on the engine path and was swallowed. Both are closed
  by the explicit pre-validated entry points. Measured after: `refusedAtBuffer=true`,
  `notApplied=false`, `resurrected=false`, `storedAfterRecovery=absent`, and `walOps` down
  from 4 to 3 — the refused frame is gone from the log.
- **Uncommitted create+delete pairs leaked phantom nodes** (#2443), both found by the DST
  multi-session mode. `NodeExistsAsOf` answered TRUE for a node whose birth and death
  records were both invisible, so a create+delete inside one open transaction showed
  concurrent readers a bare node with no labels or properties. And `reclaimAbortedLife`
  treated an aborted birth+death pair as independent directions — it tombstoned the node
  for the birth and then revived it for the death, so a conflict-doomed transaction's
  rolled-back creates persisted as phantoms.
- **The node is now the unit of write–write conflict under MVCC** (#2444). A
  `DETACH DELETE` of a node carrying another in-flight transaction's pending property
  write sailed through where a second property write is refused, and its eager
  present-state mutations then destroyed state a rollback could not restore: with both
  transactions rolled back, the committed node was **gone from the label scan
  permanently**. Every side now claims in its own store first and cross-checks the others
  after, so whichever claim lands second sees the other and detection is race-free without
  a per-node lock. Edge appends check and stamp **both** endpoints, directed graphs
  included, as Memgraph prepares both vertices. A birth on a virgin slot is exempt from the
  doomed-transaction refusal: the mapper has already interned the slot when the hook runs,
  and refusing the record left a permanently visible orphan node.
- **Rollback is sound under MVCC — four defects** (#2445), each with a minimal
  deterministic reproduction that fails on the unfixed engine. The life store's
  chain-level primordial flag misread a died+born pair written over committed history, so
  a doomed rollback re-tombstoned the node its undo had revived, and a **voluntary**
  rollback — which publishes its inverses — made the node invisible to readers pinned
  before it. The undo replay's own-arc adjacency withdrawals were refused by the head test
  when a foreign commuting append had moved the head, silently leaking the rolled-back
  edge as a **torn arc**. Adjacency entries are immutable snapshots built from the current
  slot, so a concurrent append embedded another transaction's pending arc — a permanent
  leak when the owner aborted, an uncommitted-edge read when it was merely slow. And
  `addEdgeInfo`/`addEdgeHInfo` ran the #2444 endpoint cross-check **after** the physical
  insert and returned the refusal with the arc still in the slot; no undo is recorded for a
  failed statement, so the next committed append on the node embedded and published the
  phantom arc.

#### Durability and on-disk integrity

- **A struct or named-integer edge weight was silently dropped at the first checkpoint,
  permanently** (#2526). `store/snapshot` sized weights statically and returned zero for
  anything outside a fixed set of Go primitives, so it wrote `hasWeights=0` —
  **byte-identical to a deliberately weightless graph** — and the checkpoint then truncated
  the WAL prefix still holding the values. Nothing errored, and the recovered graph was
  indistinguishable from one that never had any weights. Measured **95 of 95** weights at
  the WAL boundary and **zero of 191** after one checkpoint. Bulk import carried the
  identical defect and worse: it bypasses the WAL, so no surviving copy existed anywhere.
  A sentinel in the existing width byte now selects a variable-width section — offsets plus
  payload — encoded through the weight codec; offsets rather than length prefixes because
  the apply path **skips** slots whose endpoints cannot be resolved, so a self-delimiting
  concatenation would desynchronise. The safety property matters more than the encoding:
  the decision is made **before** the header is written and the refusal propagates, so the
  one surviving copy is never destroyed. Measured after: a duration weight 0 of 3 → 3 of 3,
  and the simulator's binary-marshaler arm 0 of 17 → 17 of 17 through snapshot-only
  recovery — a figure that is now a gate, because a codec that survives WAL replay and
  loses everything through the snapshot shows a healthy total and a zero there.
- **`manifest.json` was not checksummed** (#2520). It recorded a CRC for every component
  it listed and none for its own bytes. Being JSON, a flip inside a **key name** left valid
  JSON whose renamed key the decoder dropped, so the field silently became zero: **360 of
  1399 bytes, 25.7 %, were accepted in silence**, and one character of the commit-timestamp
  key dropped the recovered MVCC clock floor from 20 to 0, skipping clock restoration. The
  census falls to **0 of 1450**, and the clock floor holds across all 1447 flips. The shape
  was chosen from prior art read at source: only the RocksDB MANIFEST achieves full byte
  integrity **and** ignore-unknown-fields, and only by separating them — forward-compatible
  tag skipping inside the payload, the checksum in the framing outside it. Two details are
  load-bearing: a non-empty tail **must** verify, or a flip in the magic would demote a
  protected file to legacy-accepted and merely relocate the defect; and an integrity marker
  in the document means a manifest whose trailer was lost is refused rather than taken for
  legacy, so defeating the protection needs two independent damages.
- **The recovered transaction sequence was derived on every open and discarded by every
  caller** (#2522). Its godoc said the value exists to seed the store so a sequence is
  never minted twice. Measured: a store reopened on a non-empty WAL **re-minted sequences
  1, 2 and 3** while transactions carrying those values were still in that same WAL. The
  census over the tree was total — no shipped embedder wired it. A contract that every
  caller violates is **mis-placed**, not universally mis-implemented, and this codebase had
  already reached that conclusion one field over, where recovery restores the MVCC clock
  itself and its comment names this very field as the negative example. The floor never
  leaves the package now and the omission is inexpressible. Returning a fully-open store
  was rejected: deciding whether to append onto an unclean recovery must stay with the
  caller. **Three shipped examples were wrong and are fixed**, one of them a long-lived
  HTTP API serving writes off a reopened store.
- **`csrfile` persists every weight kind it advertises, reconciled with the snapshot**
  (#2529). See *Changed*.
- **The snapshot index payload was written on every checkpoint and never read back**
  (#2490). `applySnapshotIndexes` required the index to be already registered on the
  graph's `index.Manager`, but recovery builds its own graph and nothing registers on it,
  so the manager was always nil and `Result.SnapshotIndexes` was **provably always 0**.
  Every test that reached `Deserialize` poked the package-private helper on a hand-built
  manager; the two that went through the real entry points asserted 0. Registering the index
  earlier would **not** fix it: there is no index fan-out below `cypher` — the only
  production `Manager.ApplyBatch` call site is in `cypher/exec/index_writeback.go` and
  `store/txn` does not import `graph/index` at all — so WAL replay cannot maintain a
  registered index, and an early registration would leave it seekable but frozen at the
  snapshot instant, which is a silent wrong answer rather than a slow one. Also fixed:
  `store.snapshot.indexes.loaded` was incremented by the **write** path and counted payloads
  written rather than loaded, so it is renamed `.written`.

#### Bolt protocol and wire

- **`Options.Logger` never reached the Bolt session** (#2481). `newSession` hard-coded
  `slog.Default()`, so **all eleven session-level log sites bypassed the configured
  logger** — authentication outcomes, query, `BEGIN` and `COMMIT` failures, and the
  transaction-quota refusal. An embedder that configured a logger still had the majority
  of the server's events written to the process default. Found by the DST authentication
  scenario, and verified red without the fix.
- **A refused re-authentication left the connection running as the previous principal**
  (#2556). See *Security*.
- **An expired certificate could evict a working one, and a content-preserving rotation
  was silently ignored** (#2557, #2558). See *Security*.
- **An unrecognised `BEGIN` access mode granted write authority** (#2564). See *Security*.
- **An operator termination reported a timeout that had not elapsed and a writer lock that
  no longer exists** (#2560). Three distinct server-initiated endings — the total bound, the
  idle bound and `Server.TerminateTransaction` — funnelled through one teardown that chose
  the reason itself. Both halves of the message were false: no timeout elapsed, and the
  writer serialisation that clause named was retired by #2305/#2306, which `txregistry.go`
  documents in as many words. The filed prescription was **refuted by primary evidence**:
  `neo4j-go-driver` v5.28.4's `reclassify()` (`neo4j/db/errors.go:132-139`) rewrites
  `Neo.TransientError.Transaction.Terminated` to the ClientError form and its stated job is
  mapping pre-5.x errors into 5.x classifications — so ClientError **is** the modern
  spelling, and because `reclassify()` runs before `IsRetriableTransient` reads the
  classification, the Transient form would not be retried either.
- **A quota-refused `BEGIN` left the session in a state nothing had chosen** (#2561). Two
  adjacent failure paths in `handleBegin` disagreed, and nothing asserted the difference at
  any level, so it was accidental. It is chosen now — a cap is **back-pressure**, not a
  protocol error; the slot frees when another of the principal's transactions closes, so
  retrying the same `BEGIN` is the right response and requiring a `RESET` first would charge
  the client a round trip for the server being busy. The neighbour keeps `FAILED`, because a
  `newTx` failure is a genuine failure to open a transaction. The in-flight **cursor** cap
  keeps `LimitExceeded`: it is a different limit, reached inside one transaction.
- **An autocommit statement's bookmark was empty or stale** (#2563). See *Changed*.
- **Every Cypher list crossed the wire as a String** (#2513). See *Changed*. The arm
  recurses element-wise like the map case, so nested containers and entity elements encode
  structurally. A PackStream List is itself version-independent — neither this codec nor the
  reference driver's unpacker takes a version — but its **elements** branch, so a list of
  nodes carries three fields on 4.4 and four on 5.0 and a datetime changes tag; both majors
  are proven over a real socket. The fix is generalised past its own symptom: a sweep over
  all sixteen concrete value types on both majors fails if anything but a string encodes to
  a Go string.
- **An over-nested parameter was reported as an internal server bug** (#2570). The
  `maxParamBindDepth` check returned a bare `fmt.Errorf` carrying no sentinel and no
  category, so `FailureCode` fell through to `Neo.DatabaseError.General.UnknownError`, whose
  text the failure sanitiser replaced with "An internal error occurred. See server logs for
  details" — telling an operator GoGraph had a defect over a payload that is wholly
  client-supplied and wholly malformed. Measured at HEAD, the boundary is exactly 32 accepted
  / 33 refused, identical for list chains and map chains. The task proposed
  `Neo.ClientError.Request.Invalid`; that confuses two layers — the message **decoded**
  correctly and the statement was dispatched, so what is invalid is the statement's
  *argument*. `Request.Invalid` is what the serve loop answers for a frame that will not
  decode at all, and reusing it here would make one code mean two things. Session state is
  left `FAILED`, verified rather than assumed: `Transition`'s one documented exception for
  staying `READY` is back-pressure, "where retrying the same request can succeed", and a
  depth refusal is deterministic.

#### graph/index — range boundaries and the serialized image

- **`AddRange` and `RemoveRange` silently dropped the whole range at `MaxUint64`**
  (#2607). Both are documented over the **closed** interval `[from, to]` but converted to
  roaring's half-open API with a plain `to+1`; at `to == math.MaxUint64` that wraps to zero
  and roaring returns immediately on `start >= end`, so the loss was total, not off-by-one.
  Reproduced exactly as filed: `AddRange(max-5, max)` yields cardinality **0** where the
  closed interval says 6, and `RemoveRange(max-3, max)` over a five-member bitmap-tier set
  removes nothing; both `max-1` controls are exact, so the failure is attributable to the
  final id. **The sharper half is the tier divergence**: `RemoveRange`'s singleton and small
  branches filter on the closed interval directly with no `+1` and therefore no wrap, so the
  identical call over an inline-tier set holding the same five members correctly leaves 1 —
  the same logical operation on the same membership answered differently depending on state
  the public surface does not expose. The repair was a design choice among splitting at the
  boundary, saturating and refusing; **split**, because it is the only one that keeps the
  documented contract. The dependency's own source settles that a split is required rather
  than avoidable: roaring64's `AddRange`/`RemoveRange` are half-open over `uint64` with no
  closed variant, while the 32-bit sibling escapes the same problem only by widening its
  bound to `uint64` so it can name `MaxUint32+1`.
- **An inverted or empty `AddRange` minted a permanent, serialized entry for a label
  carrying nothing** (#2608). `label.Index.AddRange` stored the `NodeSet` back
  unconditionally while `NodeSet.AddRange` promoted to the bitmap tier **before** looking at
  the interval. Reproduced: 1000 inverted `AddRange` calls on distinct labels take the
  serialized image from 16 bytes to **20 016**, declaring 1000 labels, while `Count` reports
  0 for every one — the entry is permanent and costs 20 bytes apiece on disk while being
  invisible through `Count`, `Scan` and `Has`. `RemoveRange` has always promised the opposite
  for its own direction and kept the promise; `AddRange` now mirrors it. **Both** prescribed
  repairs are applied, because they fix different halves and neither alone suffices, verified
  rather than assumed. Recorded, not fixed, and out of scope: `Deserialize` still
  re-materialises an empty-bitmap entry from a legacy image written by the defective writer.
- **A `Serialize`/`Deserialize` cycle changed the bytes of a small run-encoded label**
  (#2609). roaring picks a container encoding from construction *history*, not from content:
  `AddRange` builds a RUN container, `NodeSetFromBitmap` moves a bitmap of at most
  `smallSetMax` ids back to the inline tier on the way in, and the inline tier
  re-materialises through `AddMany` as an ARRAY container. No content is lost, but a snapshot
  of unchanged data was not byte-reproducible, which defeats fixture diffing and any
  content-addressed comparison of images. Reproduced across widths 1 to 16: 55 B in and
  64/66/68/70/72 B out at widths 4 to 8. **The acceptance criteria were amended, with the
  user's decision recorded**: as filed they asked for byte-reproducibility at *every* width,
  and measured that costs more than the report knew — `Serialize` holds only an `RLock` and
  `NodeSet.Bitmap` hands back the live bitmap, so normalising an arbitrary set needs a full
  `Clone` first, measured at **6.55 → 90 µs/op and 1 289 → 218 065 B/op** on a sparse
  100k-id label to produce a byte-identical image. Normalisation is bounded at `smallSetMax`
  — exactly the band the reader down-converts — and normalises **down** rather than up,
  reaching the same canonical form as `RunOptimize` with strictly less work.

#### Memory, resources and the mapper

- **Closing a write window never released its shard builders, so `Compact` doubled
  resident memory** (#2628). The contract on `adjShard.building` says "the window end
  freezes it by clearing this field"; nothing did. Neither `EndExclusiveBuild` nor
  `EndCommit` touched it, so the field was released only lazily by `storeEntry`, when a write
  presenting a **different** owner happened to touch the same shard — a shard no later
  transaction touched pinned its builder for the lifetime of the graph. The cost is not the
  builder, it is what the builder **blocks**: it keeps the shard's pre-window slot array
  alive after `slotsRef` has moved on, so anything replacing `slotsRef` leaves both arrays
  live. Measured at 3 M edges, build then `Compact`, live heap after `runtime.GC()`:

  | Arm | Live heap | Allocated |
  |---|---:|---:|
  | no window | 159.9 MiB | 3.25 GiB |
  | exclusive-build window, before | **361.9 MiB** | 1.79 GiB |
  | exclusive-build window, after | **159.9 MiB** | 1.79 GiB |

  Bracketing a bulk build — the documented way to make it cheaper — roughly **doubled**
  resident adjacency once the graph was compacted, inverting `Compact`'s own purpose. It is
  now free. `EndCommit` is deliberately unchanged: that is the per-transaction serving path,
  and an `O(shards)` walk per commit would be a real regression. A second, sharper hypothesis
  was **refuted** and is not claimed — a write made after `Compact` inside the same open
  window is not lost, and a test asserting the edge survives passes on the unfixed code.
- **An unreproducible `NodeID` was attributed to the snapshot rather than to the key type**
  (#2528). `mapperShardFor` hashes an uncovered comparable key through
  `fmt.Fprintf(h, "%v", v)`, which renders a pointer as an **address**, so a key carrying one
  hashes to a different shard on every run — while the function's godoc promised the hash was
  stable across processes, and FNV was chosen precisely to guarantee that. Reproduced
  deterministically **in process**, which is stronger than the subprocess reopen the task
  prescribed: two allocations of the same logical key already hash to different shards, so no
  second address space is needed and, unlike a subprocess run, this cannot agree by
  coincidence. Two findings changed what the fix should be. **The loss is not silent** —
  `Mapper.LoadFrom` already enforces that an entry's packed shard equals
  `mapperShardFor(key)` and returns `ErrMapperEntryCorrupted`, and
  `store/snapshot/apply.go` propagates it and aborts before any label or property is applied.
  And **no better hash could fix such a key**: for every comparable Go type, "formats as an
  address" and "compares by address" coincide, so a key decoded into a fresh allocation is
  not the key that was written even with identical data — its *identity*, not merely its
  shard, is unreproducible, and an address-independent hash would have made the hash agree
  while the map still created a second entry, which looks fixed and is not. The real defect
  is therefore **misattribution**. The audit for other `%v`-based hashing or ordering is
  recorded: `mapperShardFor` is the only `%v` hash in the tree.

#### Concurrency — cancellation and a data race

- **Six exported `search` entry points could run to completion without ever observing a
  cancelled context** (#2593), violating the concurrency mandate's context-aware-blocking
  rule. Worst case first: **`flow.PushRelabelMaxFlowCtx` returned the true, complete maximum
  flow with a nil error under an already-cancelled context**, measured at `MaxNodeID` up to
  1536 — a request-scoped deadline fires, the caller's context is dead, the library does the
  full work and reports success, and nothing downstream can tell. One root cause behind five
  of the six: **increment-then-mask**, so the counter is 1 on the first iteration and the mask
  first trips at 4096 units of work. Measured with a pre-cancelled context at 8 nodes, every
  one of `BellmanFordCtx`, `BellmanFordInto`, `KCoreCtx`, `KShortestPathsLooplessCtx`,
  `KShortestPathsLooplessCtxWithOpts`, `EppsteinKShortestCtx` and `PushRelabelMaxFlowCtx`
  returned nil with a complete result. `TopologicalSortCtx` was a different mechanism: on a
  fully cyclic graph every vertex has indegree ≥ 1, so the polled Kahn loop never runs and
  `ErrCycle` was returned in preference to the context error, at every input size.
  `discharge` got its own poll, decided by measurement: one discharge reaches 60 004 inner
  steps at 20k vertices and 600 004 at 200k — **146× the whole stride** — so counting
  discharges left the interval between polls unbounded in the input size. The sixth site was
  missed by the first sweep and found by the battery it was meant to unblock:
  `bellmanFordVirtualSource` is a shared prologue, so neither Johnson entry point's own file
  contained the defect. **The count is closed by scan rather than by trust** — a detector for
  the shape, run over all 47 stride-gated polls in the module, leaves exactly two
  increment-then-mask sites, both benign because their callers poll unconditionally first.
  **No live-context result changed, and that is measured, not argued: 3 622 signatures
  byte-identical before and after.**
- **Concurrent first executions of a subquery-bearing query raced on the shared cached
  AST** (#2508). Only the *outer* plan is cached; an `EXISTS { }` / `COUNT { }` subquery's
  inner plan was re-translated on **every** execution, over the shared cached AST, and the
  translation walk names anonymous entities by **writing into** that AST. Not benignly: each
  translator mints its own names, so one plan could reference a variable the other never
  bound — a wrong answer, not a duplicated identical write.
  `ir.NameSubqueryAnonymousEntities` now names those entities once, at the top of `FromAST`,
  while the AST is still private to the plan-cache builder. This is the discipline both LPG
  reference engines follow, read from source: Neo4j's `NameAllPatternElements` runs in
  `AstRewriting`, inside `parsingPost` and so before its AST cache; Memgraph fills its
  anonymous identifiers at the end of `visitSingleQuery`, at parse time. **Two regressions the
  fix caused, both found by evidence and both fixed here:** the degree-rewrite (#2232) and
  labelled-hop-count (#2235) recognisers refuse a *named* relationship, so they silently
  stopped firing and fell back to driving an inner plan per outer row — measured at **115×**
  the cost of the equivalent non-subquery form, with their guards now semantic
  (`ir.UserNamed`) rather than syntactic; and the synthetic names leaked into the public
  result-column name of an un-aliased projection, since that name is derived by re-rendering
  the AST. **The TCK cannot cover the column-name regression** — it has no un-aliased
  `RETURN EXISTS{}`/`COUNT{}` scenario at all, so 3897/3897 is structurally blind to it.

#### The simulator and the gates themselves

Twelve genuine defects were fixed in the DST harness. They change simulator **fidelity**,
not module behaviour, and are listed here because a harness that misreports is how a real
defect hides:

- A file handle held across a simulated crash accepted and discarded bytes; it now returns
  `ErrCrashedDisk` wrapping `fs.ErrClosed` (#2544). The prescribed nil-error short write was
  **refuted** against the `io.Writer` contract (#2541).
- A torn trailing WAL frame was made reachable from a simulated crash (#2541); the WAL-only
  layout moved under a real `waldir/wal` directory and the `isRootLevel` exemption was proven
  a **necessity** of the model — forcing it false fails 16 tests (#2539); `MkdirAll` registers
  the components it creates (#2543); a failed fsync is permanent and a retry grants nothing,
  sourced to Rebello et al., USENIX ATC 2020 and the PostgreSQL fsyncgate response (#2540); a
  truncation is durable only once fsynced (#2542); and `Remove` no longer silently no-ops on a
  directory — the report named the wrong error, since this surface models `os.Remove`, so an
  empty directory *is* removed and only a non-empty one returns `ENOTEMPTY` (#2545).
- The soak layer was red for **three independent reasons**, not the one filed (#2620). The
  filed cause was an over-eager oracle gated on `crossedBound || sweeps() > 0`, so **one
  sweep** fired a clause whose message asserts churn reached the vacuum's wake threshold —
  while the failing run's high-water was 12 records against a bound of 4096. A sweep that
  frees nothing is the vacuum's *documented exit condition*, and `Backlog` is by definition
  versions the observed pass could not have swept, so the evidence pointed the other way. The
  other two were pin inversions: #2602 and #2609 each correctly inverted their short-layer pin
  and left the **soak-layer sibling** asserting the old behaviour. The structural cause is that
  **no task's acceptance gate runs the soak layer** — `-tags=soak` appears in no acceptance
  criterion in the sprint.
- The `Reproduce with` line omitted `-scenario` entirely, so it was wrong for **all 55**
  catalogue scenarios and ran the default workload instead (#2621).
- The allocation oracle differenced two process-global readings; it now requires exact
  interval containment (#2555), after measuring the old form's difference-less-injection
  ranging from −135 256 to +101 320 bytes across 25 runs — a spread of 236 576 straddling
  zero, with one run within 32 516 bytes of the tolerance.
- The goroutine-growth slack is documented at the source (#2592); its fourth acceptance
  clause **could not be met**, because the package's soak layer was already red, which is what
  filed #2620 and #2621.
- Six further harness gates stopped measuring machine load: the swarm's overlap and
  pressure-density claims became structural rather than wall-clock thresholds (#2596, #2611,
  #2587), a `GOMAXPROCS`-clamping scenario now runs alone so it stops deciding its neighbours'
  regime (#2613), the cancellation-precedence table is passed in rather than raced through a
  package global (#2597), and a scenario is no longer billed for its neighbours' allocations
  (#2553).
- Five bounded security gates stopped reporting a slow machine as a failed subject (#2567).
  Run alone under `-race` the two string-budget tests pass in 0.47 s and 0.48 s; under the full
  race suite the ~21× slowdown pushed a half-second evaluation past the 10 s deadline its
  helper imposed, and the helper scored `context.DeadlineExceeded` as proof the guard had
  failed. The deadline is **removed rather than widened**, because it was measured to carry no
  detection power. A repository-wide sweep cross-checked three ways over 277 occurrences left
  roughly 148 sites alone, where the wait is genuinely unbounded and the clock is the only
  possible oracle. The worst of the five had already flaked once and been "fixed" by widening
  its ceiling to 120 s; measured at 17.76 s idle and **537.36 s saturated**, that was a live
  time bomb, not a fixed test.
- The `bench/cypher` `Rel*` benchmarks failed in their own seed, because `seedRelGraph`
  writes two edges between the same endpoints and needs `Multigraph:true` (#2068). The more
  useful half: **`go test` compiles benchmarks but does not run them**, so a benchmark broken
  in its fixture gates nothing *and* reports nothing while still appearing in the suite. The
  same class was then found in `graph/lpg`'s MVCC read benchmarks (#2623), where the
  `deltas=off` arm was **not a control** — it ran with deltas on — so the comparison the
  benchmark exists to make had never been valid, invisible in the output because a control
  that is not a control still prints a number.

### Performance

Full detail for this release is in
[`docs/benchmarks/v0.12.0.md`](docs/benchmarks/v0.12.0.md).

#### What got faster

- **The `v0.11.0` open plan-choice regression is closed** (#2431). `MATCH (n:Common:Rare)`
  over 100 000 `:Common` of which 1 000 also carry `:Rare` took 4.611 ms in the default
  configuration against 0.186 ms for the plan the same engine already knew how to build — and
  worse than the 4.280 ms legacy full-`:Common` scan the re-anchor was written to replace.
  The root cause was that the columnar chain's **yield was inert**:
  `tryBuildColumnarFilterChain` already declines when `pickMinLabel` would fire and states the
  rule, while `tryBuildParallelScanProject` was tried immediately after and was not making the
  same argument — parallelism is also a constant factor — so it claimed every shape the yield
  gave up, anchored on `Labels[0]`. The fix **adopts** the anchor rather than merely declining,
  which makes the case where the smallest label is itself above the threshold *faster* rather
  than merely unregressed. Measured interleaved A/B over 5 pairs with `benchstat`:

  | Arm | sec/op | B/op | allocs/op |
  |---|---:|---:|---:|
  | `P2431_Fixture` | **−96.08 %** (p=0.008) | −99.28 % | −98.54 % (106 293 → 1 551) |
  | `P2431_MinLabelAboveThreshold` | −19.92 % (p=0.008) | | |
  | `P2431_MinLabelAboveThresholdSkewed` | −44.16 % (p=0.008) | | |
  | `P2431_SingleLabel` | ~ (p=0.151/1.000/1.000) | ~ | ~ |

  **No shape regresses.** `BenchmarkMinLabelScanSelective_MinLabel` is now 204.7 µs against
  `_FirstLabel`'s 3 421 µs — 16.6× faster, the relationship its name asserts and which did not
  hold before — and faster than `v0.10.0`'s 230.3 µs, so the regression is undone rather than
  merely levelled. 106 293 allocs/op is exactly the figure the report quoted, so the
  attribution is confirmed on the reported number.
  ([`min-label-anchor-vs-parallel-scan-2026-08-26.md`](docs/benchmarks/min-label-anchor-vs-parallel-scan-2026-08-26.md))
- **The range-seek population floor is set from measurement** (#2367): an indexed integer
  equality cost **68.7 µs over a label of 1023 nodes and 5.5 µs over one of 1024** — a 12.6×
  cliff at a constant, with the **smaller** graph the slower one. The sweep is a step at the
  constant in both directions, and allocations say the same and cannot be moved by machine
  load: 889 per lookup at 1023 nodes against 127 at 4096, flat at ~123 after. The shared floor
  also governs the key-set path, measured rather than assumed: at 1023 nodes the seek set costs
  168 allocations against 13 239 for the scan (**78.8×**). An earlier version of this ticket
  asserted a mis-calibrated cost-based crossover; that claim was **withdrawn** when no such
  threshold was found, and four further explanations for the surrounding anomaly were each
  refuted by measurement. **Not fixed, and out of scope:** an integer-keyed property still
  cannot use the hash path, because a Cypher `CREATE INDEX` only ever builds a string-keyed
  hash index.
  ([`range-seek-population-floor-2026-08-26.md`](docs/benchmarks/range-seek-population-floor-2026-08-26.md))
- **`count(*)` no longer builds a row per row to carry a constant** (#2625). On a bare typed
  expansion the pre-projection was 4.06 ms of an 11.30 ms query and removing it cut the query
  **4.52 ms → 2.71 ms (~40 %)**. The gain depends entirely on the child: once an endpoint label
  is added, the per-row `Filter` checking the far endpoint costs ~18.2 ms of 26.1 ms and this
  operator's own cost is ~2.6 ms. At scale (`examples/26_social_scale_bench`, 40 000 users,
  interleaved A/B, five runs per arm) the median gain is modest and **the variance result is
  large**: `count_friend` 8.040 s → 7.504 s (+6.7 %) with spread 52 % → 4 %, and `count_like`
  6.331 s → 6.156 s (+2.8 %) with spread 59 % → 9 % — removing seven million per-row
  allocations takes the GC out of the tail. One early pairing showed 35 %; five rounds identify
  it as an outlier in the *before* arm and **it is not claimed**. The count-store fast path the
  task prescribed is **refuted and not built**: `count.Store.CountE` takes no snapshot and a
  read transaction pins its view, so answering from the store would violate snapshot isolation.
  ([`count-star-row-count-2026-08-27.md`](docs/benchmarks/count-star-row-count-2026-08-27.md))
- **Snapshot index hydration** (#2490), measured with `benchstat` over 50 000 nodes and 4
  indexes, n=10: **sec/op −69.65 %**, B/op −15.88 %, at **allocs/op +24.91 %**. The allocation
  cost is real and is tracked as #2594, and the win applies **only** where the WAL suffix is
  empty or index-irrelevant.
- **`graph/query`'s integer-bounded numeric range is index-served** (#2600). Against the path
  the query actually took before the fix — a full scan, since an integer-bounded range was
  never index-served — **31.8× faster selective, 44.19 ms → 1.39 ms**, and 1.4 % slower at
  100 % selectivity. B/op and allocs/op are identical: the residual reads properties and
  allocates nothing. Measured on darwin/arm64, 10 cores, 200k nodes, ages uniform in [21,65],
  `-benchmem -count=6` with `benchstat`.
- **`Compact` reclaims what it should** (#2628): 361.9 MiB → 159.9 MiB live heap after a
  bracketed 3 M-edge build, matching the unbracketed arm, while still allocating 1.79 GiB
  against 3.25 GiB. See *Fixed*.
- **The certificate-reload skip is 1.8×, not two orders of magnitude** (#2558). Measured
  rather than assumed, and the measurement **refuted the assumption**: 20.6 µs and 2.0 kB per
  call against 37.8 µs and 9.4 kB for the full load (best of 5, darwin/arm64). The two file
  reads dominate **both** paths. The skip is kept because it does not recompute and re-publish
  a certificate that has not changed, and the godoc now states the real numbers.

#### What got slower — measured, not minimised

- **The anchor-swap guard costs the declined shape a great deal, and it is not called
  negligible** (#2603). `benchstat`, `-count=6`, same fixture and rows: at hub degree 1601 the
  declined shape costs **65×** (5.644 µs → 368.8 µs) out-ward and 62× in-ward; at degree
  40 000, **1705×** (5.681 µs → 9.687 ms) and 1660×. Allocations go 193 → 79 566 and bytes
  8.88 KiB → 627 KiB. The swapped plan is flat in the hub's degree and the written order is
  linear, so **the loss grows without bound** on an ordinary spelling. Correctness outranks
  speed, so the guard ships; **#2604 is filed to recover the optimisation properly.**
- **`flow.PushRelabel`'s discharge polling costs +3.3 % to +4.6 %** on one adversarial
  fan-out shape (#2593), p<0.05 over three interleaved rounds, with B/op and allocs/op
  identical because the counter is threaded by value. Accepted under correct → secure → fast,
  and recorded as the only measurable throughput cost in that change.
- **The MVCC endpoint cross-checks cost Cypher `DETACH` +4–6 % and relationship creation
  +3.9 %** (#2444); direct-API bulk delete is unchanged. That is the attributable price of
  making the node the unit of write–write conflict.
- **An `AddRange`-built label at or below the normalisation bound moves 16 → 28 allocs/op**
  (#2609) — which is what an `Add`-built label of the same ids already cost, so the change
  makes the two paths do the same work rather than making either do more. Allocations are
  **identical**, every sample equal, for a 100k dense label, a 100k sparse one and an
  `Add`-built label of 8 ids. A first, **sequential** A/B reported a −8.57 % speedup on the
  dense label; interleaving **refuted** it (p=0.841), so it was an artefact and is not
  claimed.

#### Examples — not part of the module

`examples/` is an exercise harness, and the module neither imports nor depends on it.

- **`examples/26_social_scale_bench` could not run at its own advertised default** (#2385).
  It printed its six config lines and then nothing: at the documented default of 1 000 000
  users it was killed at twelve minutes, at 150 000 at seven. **The reason is memory, not
  patience**, which is what decides the fix: 20 000 users is 71.7 s and 1.88 GiB peak RSS,
  40 000 users is 149.6 s and 3.74 GiB, so peak RSS scales 1.99× for a 2× population and a
  million users projects to roughly **93 GiB against a 32 GiB machine**. The default is now
  50 000 users, agreed with the user because it is documented behaviour and a pinned fact, and
  **measured at that default rather than projected**: 183.6 s and 4.81 GiB peak RSS over
  16 265 613 edges. Progress telemetry goes to stderr and results to stdout, so a pipe
  capturing results gets exactly the pinned facts — and it immediately produced a finding the
  example could not previously show: at the default the build takes 14.2 s and the **read
  battery takes 153.7 s of the 183.6**. This example is dominated by its queries, not its
  ingest.
- **The social-graph build allocated 58× its resident size, and the ratio rose with scale**
  (#2624). The site was named by a **two-scale `-alloc_space` diff** rather than a single
  profile: query-phase sites scale 1.97–2.06× between 20k and 40k users (linear), while
  `main.build` is 3.11×, `AddEdgeLabeledWithProperties` 3.16×, `upsertEdgeSlotLocked` 3.18×
  and `adjlist.storeEntry` **3.92×**. At 40 000 users total allocation went **28.02 GiB →
  8.28 GiB**, amplification **58× → 16.6×**, and build elapsed 10.229 s → 8.044 s; across the
  two scales the ratio goes from 37× → 58× **rising** to 16.5× → 16.6× **flat**. Batching the
  window every 65 536 writes was measured and earned nothing (8.34 GiB against 8.28 GiB), so
  the simpler single bracket is what an example should show. This is the task that uncovered
  the engine defect fixed as #2628 — without that fix this change traded 3.4× allocation for
  2× residency. The README's sample-output block was several sprints stale (9.12 GiB against a
  real 4.14 GiB) and is regenerated from an actual run.

### Security

- **CWE-59, link following, in `store/csrfile`** (#2580, severity 7). Files were opened with
  no `O_NOFOLLOW` on either the write or the read path, while both sibling publish paths were
  hardened against exactly this class under #1843 — `store/wal` has `walNoFollow`,
  `store/snapshot` has `openSnapshotComponent`. The write side is the sharper half because the
  temp name is **fully predictable**: the writer forms it as `OutputPath + ".tmp"`, so a local
  principal who can write the store directory pre-plants a symlink there aimed at any file this
  process may write. **Reproduced, with all three parts of the claim measured rather than
  argued** — with the guard disabled the publish **succeeded** through the symlinked temp, the
  victim file went from 60 bytes to 2 244 bytes of CSR data (an arbitrary-file overwrite), and
  the published path itself **became a symlink**, because `rename(2)` moved the planted link
  onto the output name, so every later write through it lands on the victim. The read path
  followed a symlink too. The **TOCTOU is closed separately, and it had to be**: the writer
  resized the temp with a path-based `os.Truncate` moments after creating it, which re-resolves
  a predictable name and is a second window even with `O_NOFOLLOW` on the create; `Truncate`
  moves onto the already-open descriptor, so there is no second name resolution to race. The
  negatives are paired with a positive control — an ordinary publish and open still work, and
  so does a publish through a symlinked **parent** directory, which pins the guard's scope to
  the final component.
- **A refused Bolt re-authentication left the connection running as the previous principal**
  (#2556). The non-`firstAuth` failure branch was the only exit from `handleLogon` that set
  neither `s.identity` nor `s.authenticated` — the assignments sit after the error return — so a
  refused identity switch changed nothing, and `handleReset` then took the authenticated path
  back to `READY` with full write capability. **Measured end to end over a real socket:**
  `LOGON(alice, ok)`, `LOGON(bob, WRONG)` → FAILURE, `RUN` → IGNORED, `RESET` → SUCCESS,
  `CREATE (:Ghost)` → SUCCESS, nodes-created **1**. The identity is security-relevant even
  without roles: it keys the per-principal transaction quota and the `SHOW TRANSACTIONS`
  attribution, so a refused switch left both pointing at the wrong principal. The contract comes
  from the **specification**: the Bolt `LOGON` section states that a failed authentication makes
  the server respond FAILURE and close the connection, and carves out no exception for
  re-authentication. It makes a failed guess cost the connection, which is the point — the
  official `neo4j-go-driver` always emits `LOGOFF` before `LOGON`, so the branch where
  `authenticated` is true on entry was reachable only by a non-conforming client, exactly an
  attacker's shape, because skipping the `LOGOFF` was how a failed credential guess cost
  nothing. **Why nothing caught it:** every existing test and DST arm sends `LOGOFF` first,
  which pre-clears the flag. The new arm omits it and attempts the write **after** a `RESET`,
  because `RESET` is where the recovery happened — an arm that only checked the reply to the
  `LOGON` would have passed against the defect.
- **A certificate rotation performed to revoke a certificate could be ignored in silence**
  (#2558). `CertReloader.Reload` short-circuited on "neither file mtime is after the last
  successful load" — a cheap heuristic for "nothing changed" and an unsound one, because mtime
  is not a content hash. Every rotation that replaces content without advancing the timestamp —
  a rename from another directory, `cp -p`, a restore from an archive, or two rotations inside
  one filesystem timestamp tick — returned nil **having loaded nothing**, and because the call
  reported success `onError` never fired, so an operator polling through `Watch` had no signal at
  all. `Reload` now reads both files on every call and compares a **SHA-256 digest** of each
  against the digests the live certificate was built from, re-parsing only when they differ and
  recording the digests only on a load that **succeeded** — so a skip is provably a no-op and a
  refused pair is always re-examined. `os.Stat` is gone, which changes the absent-file error
  from `stat key: …` to `read key: open …: no such file or directory`.
- **An expired certificate was swapped over a working one** (#2557). `Reload` validated only
  the **pairing**, via `tls.LoadX509KeyPair`, which inspects neither `NotBefore` nor `NotAfter`.
  A rotation to an expired leaf — the commonest real rotation incident, a renewal that produced
  expired material or a clock skew on the renewing host — returned nil, swapped the doomed leaf
  into service, and **destroyed the previous working certificate**: every subsequent handshake
  failed. The godoc promised the opposite. Three decisions are documented on `Reload` itself: a
  not-yet-valid leaf is refused too, and because a refusal does not stamp the bookkeeping the
  `Watch` poller picks the pair up on its own once `NotBefore` passes; **only the leaf** is
  checked, never the chain, because an expired issuer kept in a bundle is a real working pattern
  (Let's Encrypt DST Root CA X3) and the one surveyed implementation validating the whole chain
  documents two incidents its strictness caused; and the check gates the **swap**, not
  construction, since at construction there is no working certificate to protect and refusing
  there would make a host whose clock has not yet synchronised unable to start at all.
- **A fail-open Bolt access mode granted write authority to a client that asked for
  read-only** (#2564). `handleBegin` read the mode as `if modeStr == "r"`, so `"R"`, `"read"` or
  a non-string all fell through to the write default and the client's writes succeeded. That is
  a fail-open coercion on a field this server treats as a **capability restriction**, and the
  project's fail-stop-never-fail-silent rule forbids resolving an unknown token in the more
  privileged direction. **The contract was decided from evidence, and the evidence cuts against
  simply copying the protocol:** the Bolt specification is silent on invalid mode values and
  frames `mode` as a routing hint rather than as authorisation, so in Neo4j's cluster model a
  read-only transaction fails on a follower because the follower is read-only, not because the
  server validated the token. GoGraph gives the field the stronger meaning, so the contract is
  its own to choose. Compatibility risk is low by construction: the official drivers send
  exactly `"r"` or `"w"`, so a refusal can only reach a client that was previously being granted
  write authority it did not ask for.
- **An internal bind path would have been forwarded to the client** (#2570). A `ClientError`
  code **bypasses** the failure sanitiser, and the wrapped depth error accumulated one path
  segment per level: 405 bytes for a 33-deep map chain, **396 of them `map["k"]:` framing**.
  `BindParams` discards the path for this one sentinel — the path *is* the depth, so it merely
  restates the limit 33 times — and reports the key and the limit alone, at 108 bytes.
- **The `csrfile` crash-durability claim is scoped to where the barrier exists** (#2582). An
  unmeasured reassurance is worse than silence, because a reader takes it for a guarantee. See
  *Changed*.

### Compliance

- **100 % openCypher TCK-compliant at the execution level, preserved.**
  `const tckExecutionBaseline = 3897` in `cypher/tck/runner_test.go` is unchanged, no
  `.feature` file changed in this window, and the sprint-close gate reports **3897 scenarios,
  3897 passed, 0 failed, 0 undefined, 0 inconclusive**, with the error-type fidelity ratchet at
  122/695. Three changes in this release were checked against the TCK before shipping and each
  is recorded as unable to move the count: subquery expressions are not covered at all (#2615),
  the comma-pattern relationship-uniqueness form does not appear (#2252), and `graph/query` is
  absent from `go list -deps -test ./cypher/tck/...` (#2600, #2601).
- **100 % ACID-compliant, and strengthened.** Four Consistency, Isolation and Atomicity
  defects were fixed this cycle — a validator-refused value made durable and resurrected by
  recovery (#2602), phantom nodes from uncommitted create+delete pairs (#2443), a
  `DETACH DELETE` that destroyed state a rollback could not restore (#2444), and four rollback
  defects including a torn arc leaked by the undo replay (#2445).
- **Extreme / massive concurrent ready — no new certification.** The most recent production
  certification is [`docs/certification-2026-08-13.md`](docs/certification-2026-08-13.md), taken
  at the `v0.11.0` release commit. **This release is 192 commits past it and carries no
  certification of its own.** The envelope that certification states — latency percentiles at
  the published concurrency levels unmeasured, the whole-tree soak layer unrun, no container ever
  run, a single host and architecture — is unchanged, and `soak-artefacts/` is still from
  2026-05-30 at commit `b5453b9`. See
  [`release-notes/v0.12.0.md`](release-notes/v0.12.0.md#what-this-release-does-not-establish).
- **Ultra efficient by design — held, with costs stated.** `Compact` reclaims 2.26× the
  adjacency it used to (#2628); an indexed integer equality below 1024 nodes stops allocating
  889 per lookup (#2367); a `count(*)` over seven million rows stops allocating a row per row
  (#2625). Against that: the anchor-swap guard's declined shape, `PushRelabel`'s poll, and the
  MVCC cross-checks, all quantified above.

### Notes

- **Pre-1.0 stability.** This is a `0.y.z` release. The public Go API may change without a
  major-version bump until `1.0.0`; pin the exact version you depend on.
- **`1.0.0` gates: 3 of 4 met, unchanged.** Execution-level TCK is at 100 % against a ≥ 95 %
  requirement, every `T-` divergence is resolved, and the local gate is green. Gate 4 — a soak
  report in `soak-artefacts/` reflecting a run against the release commit — remains open for a
  fifth consecutive cycle. The `internal/sim` soak layer *was* brought green in this window and
  verified twice consecutively (#2620); the **whole-tree** soak was not run.

[0.12.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.12.0

## [0.11.0] — 2026-08-13

The largest release the project has cut: **505 commits**, of which 63 are features,
83 fixes and 61 performance changes, with **31 marked breaking**. **Thirty-two sprints
delivered work in this window — 311–322 and 324–343** — covering the round-3 and
round-4 audit remediations, the planner access-path cycle, two production
certifications, the MVCC epic, and the memory, CPU and concurrency remediation
cycles.

Two things define it. **Concurrency control became MVCC and nothing else** — the
single-writer semaphore, the engine writer mutex and the `Graph.View` read barrier
are all gone, replaced by a transaction clock, version chains, optimistic conflict
detection and a bounded background vacuum. And the **planner gained six new access
paths**, several of them asymptotic rather than constant-factor improvements.

The openCypher TCK gate is unchanged at **3897/3897** — 100 % execution-level
compliance is preserved, not extended.

**Read [`release-notes/v0.11.0.md`](release-notes/v0.11.0.md) before upgrading.** It
carries the migration steps for every breaking change, the measured performance
delta including the regressions, and the envelope of what the production
certification does *not* establish.

### Added

#### MVCC — the concurrency-control substrate

- **New public package `graph/mvcc`** (rmp #2266–#2312). A transaction clock and
  shared commit records; a **contiguous commit frontier** (`Clock.ReadTS` returns the
  highest timestamp below which nothing is still in flight, not the highest
  published one); a reclamation horizon and watermark; `mvcc.Gate`, a weak/strong
  admission gate that replaces a shared-counter `sync.RWMutex`; `mvcc.DepthHist`;
  and the retriable sentinel `mvcc.ErrSerializationConflict`.
- **Version chains** on node labels, node properties, adjacency (inside the
  immutable entry), per-slot relationship types, and the five remaining per-edge
  side stores. A snapshot resolves every store at one instant.
- **Optimistic write–write conflict detection across seven stores**, first-updater-wins
  on the version chain. The loser is refused at the conflicting statement rather than
  at `COMMIT`, because detection is immediate — there is nothing to defer. Over Bolt
  the error maps to a `TransientError`, so the official driver's managed transactions
  retry it.
- **A bounded background vacuum** (`refactor(mvcc)!`), governed by a reclamation
  watermark sized from measurement for **1024 concurrent readers**. Reclamation is no
  longer a commit step.
- **The MVCC clock survives restart**: the commit timestamp is carried into the WAL,
  allocated before the fsync, and the clock is *derived* from the WAL at recovery
  rather than persisted separately.
- **Snapshots record the instant they captured**, and **checkpoints capture at a
  transactional instant while writers commit** — the checkpointer no longer excludes
  writers.
- **`cypher.Session` and `lpg.Session`, with `Engine.NewSession` and
  `Graph.NewSession`** (rmp #2328) — read-your-own-writes across transactions. Within
  one session every operation observes every commit that session has made; across two
  sessions nothing is promised beyond snapshot isolation, which is the contract a
  connection gives in any client-server database. A writer that does not immediately
  read never waits. **Carried through the Cypher and Bolt layers**: every Bolt
  connection owns a `cypher.Session` (`bolt/server/session.go`), so driver clients get
  the guarantee too. Measured cost: none in either shape — 32.13 → 32.06 µs
  read-after-write, 17.81 → 17.76 µs read-only, both inside run-to-run variance
  ([`docs/benchmarks/session-ryow-2026-08-06.md`](docs/benchmarks/session-ryow-2026-08-06.md)).
- **MVCC observability** (rmp #2312). Writers in flight, commits, aborts,
  serialization conflicts by store, the retained version-chain depth *distribution*,
  vacuum pass latency, horizon utilisation and commit-latency histograms, through the
  metrics backend. Inventory in [`docs/metrics.md`](docs/metrics.md). Cost: the read
  path is unchanged and a write transaction pays two atomic increments — 1.42 ns on an
  otherwise empty bracket, unmeasurable on one that writes
  ([`docs/benchmarks/mvcc-observability-2026-08-05.md`](docs/benchmarks/mvcc-observability-2026-08-05.md)).
- **Every edge slot carries a stable identity** (`feat(adjlist)!`), and a
  relationship's emitted identity is that stable handle rather than a position, so an
  identity survives the compaction that a removal performs.
- **`bench/mvccwrite`** (rmp #2297) — the write-scaling harness and its regression
  gates, in the short test layer, so a change that re-serialises the writers turns the
  local gate red.

#### Planner and execution — new access paths

- **Prefix range seek**: `n.p STARTS WITH 'x'` is rewritten to a seek over
  `[x, succ(x))` on the sorted btree, reusing the shipped exact-count gate. `succ` is
  the **byte** successor, not a code-point increment — both sides already share one
  byte basis, and a byte successor always exists for a non-empty, non-all-`0xFF`
  prefix. **385×**
  ([`docs/benchmarks/prefix-range-seek-2026-07-28.md`](docs/benchmarks/prefix-range-seek-2026-07-28.md)).
- **Multi-label conjunction by Roaring bitmap intersection**: `MATCH (n:A:B)` is one
  container-wise AND instead of a scan plus a per-row label re-check, and the residual
  label `Filter` is dropped — sound because the label index was *measured* to be
  maintained on both delete and relabel. **2909×**
  ([`docs/benchmarks/bitmap-intersection-2026-07-28.md`](docs/benchmarks/bitmap-intersection-2026-07-28.md)).
- **Range-bitmap composition across single-property indexes** — two ordinary
  single-property indexes compose, which Memgraph needs a dedicated composite index
  type for, and they compose *across* index kinds.
- **btree seek for string and numeric equality** instead of a scan: **849×** at 20 000
  nodes, **202×** at 5 000, with the three cells that already worked unchanged
  ([`docs/benchmarks/btree-string-eq-2026-07-28.md`](docs/benchmarks/btree-string-eq-2026-07-28.md)).
  A numeric companion is now built for a hash index too, not only for a btree.
- **Destination-ordered CSR neighbour runs** (`feat(csr)`), ordered by the total key
  `(destination, handle)` and probed in **O(log d)** instead of scanned. The figure to
  quote is the **Barabási–Albert power law: −7.37 % time, −58.47 % B/op**; the −31.77 %
  geomean and the −81.76 % at controlled degree 4096 are dominated by synthetic
  high-degree fixtures, and RMAT overstates the power-law result by **3.9×**
  ([`docs/benchmarks/csr-neighbour-ordering-2026-07-29.md`](docs/benchmarks/csr-neighbour-ordering-2026-07-29.md)).
  The cost is on the write path: a 2.5–34× more expensive CSR build and a +16.52 %
  checkpoint.
- **`Expand(Into)` seeks the bound destination's run** instead of enumerating it:
  **−17.27 % / −50.56 % / −77.69 %** at out-degree 8 / 32 / 64 — the gain grows with
  degree, the signature of Θ(d) becoming O(log d + r), with a fitted exponent moving
  1.249 → 0.809. **The anchor-swap peephole widened from OUT-only to symmetric**:
  **−93.73 % / −99.69 %** at hub out-degree 1601 / 40000
  ([`docs/benchmarks/expand-into-symmetric-swap-2026-07-29.md`](docs/benchmarks/expand-into-symmetric-swap-2026-07-29.md)).
- **`csr` multi-way sorted-set intersection primitive** with leapfrog seeking, and
  **`exec.ExpandIntersect`**, a fused cyclic expand built on it: for a pattern that
  closes a cycle, the open middle hop and the closing seek become one operator, so a
  candidate that does not close the cycle is never built into a row. Geomean
  **−54.24 % time and −70.35 % B/op** across every qualifying shape, −91.55 % time and
  −98.30 % allocations at out-degree 64, and statistically inert on non-qualifying
  shapes ([`docs/benchmarks/cyclic-join-2026-07-30.md`](docs/benchmarks/cyclic-join-2026-07-30.md)).
  **This operator is OPT-IN and OFF by default** — set
  `EngineOptions.EnableCyclicIntersect`. The polarity is deliberately positive: the
  operator is new rather than a peephole, and the openCypher TCK contains no directed
  cycle over three or more distinct node variables, so the TCK cannot gate it at all.
  Sprint 315 is in this release as a **negative result** as much as a positive one —
  the general worst-case-optimal join was rejected on measurement and only this narrow
  fusion kept.
- **An index nested-loop join for a per-row-varying key**, behind a measured cost
  gate: **55.5×** to **258×** on reads
  ([`docs/benchmarks/index-nested-loop-join-2026-07-28.md`](docs/benchmarks/index-nested-loop-join-2026-07-28.md)).
- **The hash join is admitted for writing statements**, by proving the build side is
  pinned. GoGraph's hash join never self-selects its build side — the planner pins
  build = inner — so the emitted sequence is row-for-row identical and safe for `SET`.
  The read path's hash-join order-safety scan is retired: **62.4×** on an equijoin
  collect ([`docs/benchmarks/hash-join-order-scan-retired-2026-07-28.md`](docs/benchmarks/hash-join-order-scan-retired-2026-07-28.md)).
- **Bidirectional search for single-path `shortestPath`**, widened to typed, `DirIn`
  and `DirBoth`: **10.8×** untyped, and **12.7×–32.8×** across the widened forms
  ([`docs/benchmarks/shortest-path-bidir-2026-07-27.md`](docs/benchmarks/shortest-path-bidir-2026-07-27.md),
  [`…-widened-2026-07-28.md`](docs/benchmarks/shortest-path-bidir-widened-2026-07-28.md)).
- **A per-node out-degree primitive** on `AdjList` and `lpg.Graph` — O(1) in the
  node's degree, **1554×** at degree 4096 — and **degree-answerable `COUNT` and
  `size()`** rewritten onto it, which retires a pattern predicate 28×, a list
  comprehension 65× and `COUNT { … } > 0` **88×**
  ([`docs/benchmarks/degree-primitive-2026-07-27.md`](docs/benchmarks/degree-primitive-2026-07-27.md)).
  Counting a *labelled* single hop is a separate mechanism — one filtered adjacency
  walk, Θ(d) not Θ(1) — deliberately not built as a widening of the degree rewrite.
- **The MERGE match phase is driven from a label posting list** instead of walking
  every interned node, so locating a node to merge no longer tracks the size of the
  whole graph ([`docs/benchmarks/merge-access-path-2026-07-27.md`](docs/benchmarks/merge-access-path-2026-07-27.md)).
- **A widened columnar tier** reaching ordinary query shapes, with the aggregate
  argument filled unboxed like the grouping key, and the validity bitmap dropped from
  a fully-populated edge column. **A partitioned label scan** so intra-query
  parallelism engages on real queries, gated on the label's own cardinality rather
  than the graph's live order. **An index seek now outranks columnar execution**,
  closing a case where having an index made a query slower than not having one.
- **An `UNWIND`-bound key set is served with one index seek**, size-gated before the
  key set is extracted.
- **Seek keys resolve from the bound expression, not from source text** — see
  *Performance* below, this is the largest per-query gain in the release.

#### Observability

- **`PROFILE`, with per-operator db-hits** (rmp #2222, #2237), and a **faithful
  physical plan** from `EXPLAIN`. `Engine.Explain` previously rendered the logical IR
  and re-derived the planner's physical decisions, so a reader diagnosing a query
  could be shown `O(n·m)` where `O(n+m)` actually ran. Every operator in a `PROFILE`
  is measured, **including composite intermediates** — an unmeasured node is worse
  than an absent one, because a reader counts it as free.
- **Per-statement write-effect counters, reported over Bolt** (`Result.Counters`),
  modelling openCypher's eight side effects.
- **`db.stats.refresh()`**, behind a rate limit.
- **Every example can produce a pprof profile**, and the examples' telemetry was made
  usable at a scale where a CPU profile means something.

#### Bulk import, Bolt and the edges

- **`store/bulkimport`, with `Publish`** — builds a labelled property graph at
  bulk-loader speed and publishes it as an openable store snapshot. On **20 000 nodes
  and 200 000 edges**: import phase **233 ms median / 0.86 M edges/s**, and **0.28 s**
  of process wall clock once CSV parsing and startup are counted
  ([`docs/benchmarks/bulk-import-2026-07-26.md`](docs/benchmarks/bulk-import-2026-07-26.md)).
  On a **separate, smaller fixture** — 2 000 nodes and 4 000 typed edges into an
  openable store — the same task runs at **236 elem/s through Cypher against
  121 000–172 000 elem/s through `bulkimport.Publish`**, about **690×**, on 483 KB of
  disk instead of 1.43 MB, with the published store verified reopenable via
  `recovery.OpenCtx`
  ([release delta §7](docs/benchmarks/release-delta-v0.10.0-to-head-2026-08-10.md)).
  The two figures are different datasets and must not be combined.
  **State the trade with the number**: the bulk route writes no WAL, is not a
  transaction, cannot be rolled back, requires an empty directory, and is concurrent
  with nothing.
- **`cmd/gograph-import`** — loads a store from CSV.
- **Typed Go slices and maps bindable as query parameters.**
- **Bolt transactions are operable**: bounded by idle time and by a per-principal
  count, and listable and terminable through an operator API.
- **A standing, ratcheted Bolt driver-compatibility suite**, run against the official
  `neo4j-go-driver`.
- **Container-aware engine-wide memory ceilings**, derived from the container's cap.

#### Testing and tooling

- **Every documented Cypher example is executed as a build gate**, so a doc example
  that stops working fails the build.
- **Examples 35 and 37** — reader latency under a mixed OLTP-and-analytics workload,
  and the MVCC *write* side exercised and measured.
- The deferred test layers take an **explicit, overridable timeout**.

### Changed

- **BREAKING — `Graph.DisableMVCC`, `Graph.EnableMVCC` and `Graph.MVCCEnabled` are
  removed** (rmp #2311). MVCC is the module's only concurrency-control mechanism and
  is armed by `lpg.New`; there is no way to disarm it. An exported switch implied a
  choice that does not exist, and a disarmed graph had no snapshot isolation, so every
  guarantee the module documents was conditional on a setter any caller could reach.
  **Migration:** delete the calls. There is no replacement and none is needed.
- **BREAKING — `Graph.View` is removed** (`refactor(lpg)!`), the last pre-MVCC read
  barrier. **Migration:** reads no longer need a barrier; take a snapshot-based read
  path instead. `Graph.LockBarrier`/`UnlockBarrier`/`ApplyInsideLocked` remain for
  callers that genuinely need exclusive access, but they must **not** be used to hold a
  transaction — `ApplyInsideLockedTx` resolves the transaction from the graph's
  *ambient* slot, which two concurrent transactions overwrite.
- **BREAKING — two `store/wal` signatures changed** (rmp #2322).
  `Writer.AppendRun` returns `(int64, error)` — the run's own end offset — and
  `Writer.SyncGroup(target int64)` takes it. The offset could not be recovered after
  the fact, because the writer's accepted offset is shared: another appender advances
  it and a durability failure rewinds it, so a committer reading it later was asking
  about somebody else's frames. **`Writer.SyncBuffered` is added** for callers with
  nothing of their own to acknowledge (an empty commit's courtesy flush) and **must
  not** be used to decide a commit's fate.
- **BREAKING — `MVCCStats` field renames** (rmp #2312). `ActiveReaders` →
  `ActiveSnapshots` (it counted writers too, since writers hold a snapshot),
  `OldestReaderAge()` → `OldestSnapshotAge()`, `UnregisteredReaders` →
  `UnregisteredSnapshots`. `ActiveReaders()` is now a derived method excluding
  writers. The metric series `lpg.mvcc.readers.unregistered` and
  `lpg.mvcc.oldest_reader_age` become `lpg.mvcc.snapshots.unregistered` and
  `lpg.mvcc.oldest_snapshot_age`, and per-store conflict series use underscored store
  names. **Migration:** rename at the call site and in any dashboard query.
- **BREAKING — `AdjList.Reclaim` takes a `*mvcc.DepthHist`** (rmp #2312), so the
  reclaimer can report retained chain depth from the walk it already performs.
- **BREAKING — `txn.Tx.CommitWALOnly` takes the commit timestamp**:
  `CommitWALOnly() error` becomes `CommitWALOnly(commitTS uint64) error`. The
  timestamp is encoded *into* the WAL record, because the MVCC clock is derived from
  the WAL at recovery rather than persisted separately, so it must be allocated before
  the append rather than after it. **Migration:** pass
  `Graph.AllocateCommitTS(writeTx)`, as `cypher/exectx.go` does. The method is not
  removed and its role is unchanged — it is still the WAL-only commit that `CREATE
  INDEX` uses and that never replays through the store apply path.
- **BREAKING — `internal/ctxlock` is retired** (`refactor(mvcc)!`), and what it
  guarded is re-established by the MVCC substrate.
- **BREAKING — snapshot `labels.bin` format version 1 → 2** (rmp, `c5814d6c`). The
  edge record gains a `Slot` field. A committed relationship *type* did not survive a
  checkpoint: `labels.bin` stored `EdgeLabelEntry{Src, Dst, StringIdx}` with no slot
  ordinal, so parallel edges folded into the same key on disk, and the adjacency label
  column was never persisted at all — only reconstructed by replaying per-pair
  `SetEdgeLabel`. Relationship types became per-slot in memory earlier in the sprint,
  because openCypher means a type to belong to the relationship *instance* rather than
  to the node pair; the durable format had stayed per-pair.
- **Isolation — an explicit read transaction is now SNAPSHOT ISOLATED across all of
  its statements** (rmp #2307). `cypher.Engine.BeginReadTx` pins one MVCC read instant
  at `BEGIN`, registers it with the reclamation horizon, and routes every `Exec` on the
  handle at that instant, so a commit made by another transaction between two
  statements is invisible to the second. A Bolt `BEGIN` with `mode="r"` inherits this.
  **This is an observable change, and it is strictly stronger.** Previously each
  statement opened its own snapshot — read-committed across the transaction's
  statements — which made an explicit read transaction *weaker* than a single
  autocommit statement. No caller relying on the old contract can observe a weaker
  result. Code that depended on seeing another transaction's commit mid-read-transaction
  must now open a new read transaction. **The cost is explicit:** an open read
  transaction pins the reclamation horizon, so no version it can still reach is freed
  while it lives.
- **An open explicit WRITE transaction no longer blocks anybody** (rmp #2305).
  `BeginTx` took the graph's visibility barrier EXCLUSIVELY and held it from `BEGIN`
  until `COMMIT`/`ROLLBACK` — across every client round-trip and all the think-time
  between them. Over Bolt, one client that sent `BEGIN` and then paused blocked **every
  other writer in the process**. That hold is gone: `BeginTx` opens one commit record
  and takes no lock; each statement takes the schema barrier *shared* for its own
  duration; `COMMIT` publishes the record exactly once, and that single publication is
  the transaction's commit instant. Atomicity comes from the record, not from
  exclusion. **Two clients may now hold open write transactions simultaneously and both
  make progress**, verified end-to-end against the official `neo4j-go-driver`.
  **New `lpg` API:** `Graph.BeginVersionedTx`, `Graph.ApplyInVersionedTx`,
  `Graph.EndVersionedTx`. **An abandoned transaction still costs something, of a
  different kind:** it pins the reclamation horizon, so no version it could read is
  freed while it lives.
- **Concurrency control is now MVCC and nothing else** (rmp #2306). The `txn.Store`'s
  capacity-one single-writer semaphore is retired; `Begin`/`BeginCtx` now only register
  the transaction as an admitted writer, excluding no other writer. The engine writer
  mutex is retired too.
  **Observable behaviour change:** with nothing serialising writers, two concurrent
  `MERGE` statements on the same pattern can both find no match and both create —
  measured at eight duplicates from eight writers. This is inherent to MVCC (two
  CREATEs of two distinct new nodes are not a conflict, so there is nothing to
  arbitrate) and matches Neo4j, which requires a uniqueness constraint for the same
  reason. **With a `UNIQUE` constraint on the merged property the duplicates collapse
  to exactly one**, because the constraint's reservation is atomic. **Callers relying
  on `MERGE` being idempotent under concurrency must declare the constraint.**
  Retiring the semaphore is **throughput-neutral** — the premise that it was a
  bottleneck is refuted by measurement (all p > 0.18 at n=6; both arms scale ~15× at
  32 writers), because it was released after the WAL append and never covered the
  coalesced fsync that dominates a durable commit
  ([`docs/benchmarks/store-semaphore-retirement-2026-08-04.md`](docs/benchmarks/store-semaphore-retirement-2026-08-04.md)).
  Quiesce (`txn.Store.RunUnderCommitLock`, used by `store.DB.Close` and the
  checkpointer) no longer borrows the semaphore: it closes a dedicated admission gate
  and drains admitted writers to zero, both under one mutex, so a writer cannot be
  admitted between the two steps.
- **`bolt/server.DefaultMaxOpenTxPerPrincipal` raised from 16 to 2048** (rmp #2419).
  An observable change of default. The module publishes 1, 8, 64, 256 and 1024
  goroutines as the levels it measures and reports at, and a single principal could not
  reach them through explicit transactions without overriding this default first —
  every benchmark harness in the repository had already had to. Note the consequence: a
  connection holds at most one open transaction and `MaxConnections` defaults to 1024,
  so under the default configuration this quota can no longer bind, and the connection
  ceiling is what bounds the resource. The quota stays finite, still refuses with a
  typed `LimitExceeded`, and still isolates one principal from another. **Every open
  transaction pins an MVCC read snapshot and holds the reclamation horizon back for its
  lifetime**, so the higher ceiling is a weaker bound on that resource;
  `DefaultMaxTxIdleTime` (5 s) is what limits the exposure.
- **Both DDL-exclusion barriers moved off `sync.RWMutex` onto `mvcc.Gate`.** A shared
  acquire goes from 3.75 ns to 0.434 ns at high concurrency where `sync.RWMutex`
  degrades to 89.5 ns — a **206×** advantage at the top of the ladder, because the
  RWMutex pays a coherence miss on a shared line twice purely to announce a reader
  ([`docs/benchmarks/mvcc-weak-strong-gate-2026-08-07.md`](docs/benchmarks/mvcc-weak-strong-gate-2026-08-07.md)).
- **Single-hop adjacency resolves at execution time** (`feat(cypher)!`), and **no
  traversal structure is materialised at plan-build time**. This is what makes an edge
  `CREATE` or `DELETE` visible to a pattern in the same statement; the measured cost is
  none — `bench/cypher_scale` geomean −0.41 %
  ([`docs/benchmarks/execution-time-adjacency-2026-08-06.md`](docs/benchmarks/execution-time-adjacency-2026-08-06.md)).
- **The barrier re-entrancy guard is build-tagged out of released binaries.**
- **Relationship types are stored per slot, not per node pair**, in memory as well as
  on disk, because openCypher means a type to belong to the relationship instance.

### Removed

- `Graph.DisableMVCC`, `Graph.EnableMVCC`, `Graph.MVCCEnabled` (see *Changed*).
- `Graph.View`, the last pre-MVCC read barrier.
- `internal/ctxlock`.
- The `txn.Store` single-writer semaphore and the engine writer mutex.
- The read path's hash-join order-safety scan.
- The side-store timestamped walkers, unreachable once every snapshot read routes
  through the pinned verdict.

### Fixed

#### Correctness — the two CRITICAL ones

- **CRITICAL: concurrent read-modify-write lost 46 % of its committed updates**
  (rmp #2324). Four writers each issuing 100 autocommit `SET a.bal = a.bal + 1`
  statements, with every refusal retried until it succeeded, reported **400 successes
  and left the property at 216**. Measured directly rather than inferred: those 400
  successes wrote only ~200 **distinct** values, with one value written by five
  different statements.
  The write-write conflict test sat behind a "records nothing" guard — skipped when the
  value being written already equalled the **stored** value, on the reasoning that a
  write recording no version has nothing to conflict over. That is sound for an
  idempotent write and unsound for an arithmetic one, because the incoming value can
  equal the stored one **by coincidence**: A reads 1 and writes 2; B, whose snapshot
  also says 1, computes 2 as well; B's write is compared against the now-stored 2,
  judged a no-op, and accepted with no conflict test at all. B's statement reports
  success having applied nothing.
  The conflict test now runs unconditionally, before deciding whether anything needs
  recording. This cannot cause the spurious abort the guard was added to prevent. Node
  **labels** keep their equivalent guard and are sound with it, because set membership
  is genuinely idempotent.
  **The defect predates the MVCC sprint and was invisible to a fully green suite:** the
  one test covering this shape asserted the defective behaviour, and
  `examples/27_concurrent_txn`'s conserved-total oracle passed because its default
  contention rarely hit the window.
- **CRITICAL: an acknowledged commit could be lost in the post-fsync, pre-publish
  window** (rmp #2349, `0b8b8145`). Reproduced deterministically — under
  `-covermode=atomic` with four fsync-heavy durable packages looped in parallel, seed
  `0xBADF00D` lost **exactly one acknowledged commit in 2 of 15 runs**. Cost of the fix
  measured at zero
  ([`docs/benchmarks/checkpoint-instant-boundary-2026-08-07.md`](docs/benchmarks/checkpoint-instant-boundary-2026-08-07.md)).

#### ACID — Atomicity, Isolation, Durability

- **Durability — a commit whose frames were already on stable storage could be
  reported as FAILED** (rmp #2322). `wal.Writer.SyncGroup` consulted the writer's
  sticky durability failure before testing whether the caller's own frames were
  durable. The two are not mutually exclusive: the failure can belong to a *later*
  group round that failed after a leader had already fsynced this caller's commit
  marker. Such a committer was told its commit was lost, while recovery correctly
  replayed the durable, fully marked transaction — a durable transaction nobody
  acknowledged, which the crash simulator's atomicity oracle reported as
  `<failed-resurrected>`. `SyncGroup` now tests the caller's watermark against the
  durable size first. The defect was latent while the store serialised commits behind
  its semaphore, and is a prerequisite for retiring it.
- **Isolation — a pinned MVCC snapshot could observe half a transaction**
  (rmp #2420, `9167d3d3`, `7b435445`). The root cause was in `mvcc.Horizon.Oldest`,
  which **discarded its fallback**, so the reclamation watermark could pass a reader
  that claimed its slot mid-scan. Two prior assertions about this defect were both
  *true* and structurally *blind* to it. Every snapshot read now routes through the
  pinned verdict rather than the live stamp, and **ACID Isolation carries three
  permanent, always-on detectors** for this corruption class, each proven on a broken
  control as well as a sound one. Discharged against a criterion fixed in advance:
  **150 package runs, zero firings of all five oracles**, verified as genuine
  executions rather than cache replays.
- **Atomicity — a snapshot was not captured atomically** (`dbcc4366`), and a node
  created by an edge append was visible before its transaction committed
  (rmp #2331). `adjlist.addEdge` interned its endpoints without recording a versioned
  birth, so a node an append *created* was visible to snapshots predating the
  transaction while the arc itself correctly waited — one transaction becoming visible
  in two pieces. All three edge paths are fixed, including the two handle-bearing ones
  that every durable write goes through.
- **An aborted transaction withdraws its writes instead of leaking them**
  (`fix(mvcc)!`), and a refused transaction is aborted rather than published, keeping
  the object writable.
- **`UNIQUE` enforcement is atomic**, releasing only a statement's own reservations. A
  `UNIQUE` constraint binds a *pair*, so either half must reserve; a `NOT NULL`
  constraint binds two substores, so both halves must collide. The `NOT NULL` check no
  longer decides constraints on uncommitted state.
- **A rolled-back writer could re-reserve a value a peer's committed release had
  freed** (rmp #2366). The first fix for this **moved** the defect instead of closing
  it — a reserve that spends its own pending release must **restore** the mark on
  rollback, not delete the value, and deleting it left two `:Person` nodes carrying one
  UNIQUE value, which is the consistency direction and strictly worse. Both directions
  are now pinned by tests, and the second fails against the first fix.
- **Version memory could settle above the stated bound, permanently** (rmp #2424) —
  the sweeper exited with an outstanding debt. The vacuum now wakes when *retention*
  exceeds the bound rather than on any debt, and a debt below the wake threshold still
  finds a sweeper. **Found by running the 150 runs another task's acceptance criterion
  demanded.**
- **The MVCC ratchet must rebase the commit log**, or the frontier never moves again.
- **Each committer gets its own durability watermark**, and the durability fail-stop is
  named so it cannot be mistaken for a retriable conflict.

#### Query correctness

- **An index-seek rewrite dropped the label predicate**, producing a
  self-contradictory row — a `(n:Person)` pattern returning a row whose `labels(n)` is
  `[]` (rmp #2423, `fca34a0c`). Every index seek is now qualified by its label.
  **Unmasked by fixing rmp #2366**: a phantom reservation had been hiding it. Its
  sibling rewrites (range, prefix, numeric-equality, key-set) are fixed as **latent**
  — guarded on the strength of a code reading plus a control that the seeks still fire,
  **not** on a reproduction.
- **A reverse type filter admitted the pair rather than the instance** (rmp #2250), and
  **`shortestPath` admitted an edge its type filter excludes** — a sentinel that could
  not mean what it was asked to mean.
- **A typed degree over parallel edges returned a silently wrong count** (rmp #2241).
  The adjacency holds four slots for a pair but only the first carried an encoded
  handle.
- **`COUNT { pattern WHERE predicate }` dropped its predicate** (rmp #2242) —
  `ast.CountSubquery` carried no `Where` field at all, where its `ast.ExistsSubquery`
  sibling did.
- **A projected pattern comprehension dropped its `WHERE`**, and cost 24× (`c963558c`).
- **Phantom join rows**, and one `DELETE` costing **1212×** (`0073b5b1`).
- **The write-clause classifier read comments and string literals** (rmp #2230), so a
  keyword inside a comment or a string misrouted a read onto the **write** path — and a
  misrouted read runs inside a write transaction. The classifier now masks comments and
  literals to spaces, preserving word boundaries and offsets.
- **`CALL db.*` resolves on the write path**, and a DDL sequence no longer leaks a
  `Result`. `db.stats.refresh` no longer nests the visibility barrier.
- **A `RETURN`-less `MATCH` block is accepted in `EXISTS` and `COUNT` subqueries.**
- **Two `Project` operators are no longer stacked for a single `RETURN`.**
- **The lexer rejects unrecognised characters instead of discarding them.**
- **`ValueAt` serves a streaming result, not only a materialised one.**
- **A count cell driven negative was deleted**, losing the decrement that made it; and
  the O(1) count pushdown stays exact under write churn.
- **The label-index gate reported violations that existed at no instant**, and the
  label bag and the label index now transition together.
- **CSR-keyed caches are invalidated at source**, the topology epoch is bumped after
  the last epoch-keyed write and on an edge-label change, and the reconstruction CSR is
  built at the reader's instant rather than the present.
- **Pattern predicates read the reader's instant, not the present.**

#### Bolt protocol

- **Graph entities were sent as PackStream maps instead of Bolt STRUCTURES**
  (`b651d984`). `Node`, `Relationship` and `Path` are structures in the Bolt protocol,
  but the server sent maps — a wire capture showed `a3`/`a5`/`a2` TinyMaps where
  `b4 4e`, `b8 52` and `b3 50` belong — so **the official `neo4j-go-driver` could not
  materialise them as `dbtype.Node`, `dbtype.Relationship` or `dbtype.Path` at all.**
  That was a large part of the 13 hard failures the round-3 audit measured across 37
  driver checks. The encoder now emits `'N' 0x4E`, `'R' 0x52`, `'r' 0x72` and
  `'P' 0x50`, with field order and arity transcribed from the decoder that has to read
  them — the `neo4j-go-driver` v5.28.4 hydrator. **Pre-Bolt-5 arities are honoured
  too**: `element_id` arrived in Bolt 5, and a Bolt 4 client asserts a three-field
  `Node`. `element_id` is the durable id in decimal, identical to what the Cypher
  `elementId()` function returns, so a client reading `elementId()` in a projection and
  one reading `Node.ElementId` off the wire see the same string for the same entity.
- **The `db` field was missing from result metadata** (`b54b284e`).
- **`BeginTx` did not honour the caller's context while acquiring its locks**
  (`01f5bea9`).

#### Performance defects

- **Bulk delete degraded without bound and used one core** (rmp #2400, #2418). Six
  seed-and-wipe cycles of the *same* 20 000 nodes against one live engine took 990 ms,
  1.771 s, 2.349 s, 3.061 s, 3.841 s, 4.622 s, and deleting 90 000 nodes in a single
  statement took **15.97 s against a 30 s transaction timeout** — reachable as a
  `TransactionTimedOut` failure, not merely as slowness. `InNeighbours` answered "which
  nodes hold an edge into n" by walking **every interned node**, once per node deleted,
  so a delete cost O(k·n) with *n* counting every node ever interned rather than the
  live ones. The adjacency now maintains a live in-edge index and answers in
  O(in-degree): **cycle six falls from 4.622 s to 77.2 ms**, the curve is flat instead
  of linear, `DETACH DELETE` improves **22.2×**, and the 90 000-node statement takes
  **375.6 ms**. End-to-end relationship creation costs **+1.92 %** for it; the read path
  is unchanged. The 2026-08-11 concurrency assessment had attributed this to the
  tombstone bitmap's copy-on-write clone, which a profile prices at **0.99 %**; that
  document is corrected in place
  ([`docs/benchmarks/delete-in-edge-index-2026-08-11.md`](docs/benchmarks/delete-in-edge-index-2026-08-11.md)).
- **A string parameter now seeks the index, exactly as the identical literal does**
  (rmp #2414) — see *Performance*.
- **Eleven write paths took the constraint lock twice per property write.**
- **The parallel-scan gate cloned a bitmap it discards**, and is now screened on a
  bound rather than an exact count.
- **The by-handle property probe is skipped behind a monotonic latch**, and a lookup no
  longer decodes the value of every property record it walks past.

#### Reliability of the gates themselves

- **`make ci` discarded a failing test's report through its own log filter**
  (`9a2edbb8`) — the gate could not show what failed.
- **The soak layer's reliability gates could not fail** (`7782e785`), and a pooled-connection
  arm was added so the sweep measures the engine rather than connection setup.
- **Four gates and five documents were stating things that were not true**
  (`7ad2fca1`).
- A permanently-red soak assertion in `extern`/`csrfile` was corrected and the
  `NVertices` contract documented; the write-scaling instrument stopped reporting a
  false NO-GO under load; and several oracles were made self-diagnosing rather than
  silent.

### Performance

Figures are first-hand, measured in this project. The authoritative end-to-end
comparison against v0.10.0 is
[`docs/benchmarks/release-delta-v0.10.0-to-head-2026-08-10.md`](docs/benchmarks/release-delta-v0.10.0-to-head-2026-08-10.md)
(measured at `7a70eb3f`, i.e. **before** sprints 339–343) and the per-release snapshot is
[`docs/benchmarks/v0.11.0.md`](docs/benchmarks/v0.11.0.md).

- **Durable write throughput is now monotonic in writer count and scales 415× from 1 to
  1024 writers**, where v0.10.0 delivered the same throughput at 32 writers as at one.
  Measured at this tag on the published ladder
  (`store/txn/write_scaling_bench_test.go`, median of 6): through the store API
  268.4 → 1 098.5 → 8 321.5 → 32 234 → **111 483 ops/s** (1.00× / 4.09× / 31.00× /
  120.07× / **415.28×**), and through the Cypher engine 271.0 → **115 300.5 ops/s**
  (**425.46×**). **The mechanism is proven arithmetically, not asserted:** the scaling
  factor equals `commits/fsync` at every level — 4.09 vs 4.08, 31.00 vs 31.50, 120.07 vs
  121.20, 415.28 vs 422.05 — so the gain is group-commit fsync amortisation and nothing
  else. The single-writer rate of 268 ops/s is this device's single-writer fsync rate and
  is unchanged from v0.10.0, so **nothing was traded for the scaling**. Over the
  1 → 32 range the 2026-08-10 delta measured **16.5×**. The store-less path scales 1.98×
  from 1 to 16 writers where v0.10.0 *degraded* to 0.82×, and
  `BenchmarkEngWriteAutocommit` fell **−80.22 %** (5.714 → 1.130 µs) with 38 % fewer
  allocations.
- **A parameterised index seek did not seek — 50–65× on the shape every Bolt client
  actually sends.** Both revisions plan `NodeByIndexSeek` and return identical rows,
  yet v0.10.0 spent 41 ns per node at 5 000 nodes and 44 ns at 20 000: its cost was
  O(label population). It was a scan wearing a seek's name. Point lookup on an indexed
  `id`: 204 673 → 3 171 ns (**64.5×**) at 5 000 nodes, 888 475 → 3 325 ns (**267×**) at
  20 000. **The flagship read suite was structurally blind to this**, because every
  benchmark in `bench/cypher_ldbc` passes literal keys in the query text, and literals
  already seeked at v0.10.0. Any future read-path gate must carry a parameterised arm.
- **The LDBC Interactive Complex workload: geomean 25.70 → 18.44 µs (−28.24 %)** across
  fifteen queries with byte-identical benchmark source, best on IC5 (−69.28 %), IC8
  (−68.97 %) and IC12 (−61.40 %), with IC1 and IC11 statistically flat. B/op is
  **+3.17 %** and allocs/op −9.51 %.
- **Readers and writers stopped fighting.** In-memory, v0.10.0 sustained ~109 000
  writes/s with no readers and **1 771** with four — a 62× collapse caused by the read
  barrier. HEAD goes ~470 000 → 150 509 under the same load, a 3.1× degradation.
- **Memory per relationship fell 2.74×** by tiering the per-pair edge-instance maps
  (rmp #2401, #2402): a relationship created through Cypher cost 1 078.87 B, of which
  87.9 % was two nested per-edge maps. **The key was deliberately not flattened** — a
  single `map[instanceKey]V` would cost less still, but the nesting is load-bearing for
  deletes ([`docs/benchmarks/edge-instmap-2026-08-11.md`](docs/benchmarks/edge-instmap-2026-08-11.md)).
  Node properties are now a **byte stream after Memgraph's `property_store.cpp`**, one
  self-describing metadata byte per record, a boolean's value riding in the
  payload-width field and occupying no payload bytes at all
  ([`docs/benchmarks/propbag-bytestream-2026-08-11.md`](docs/benchmarks/propbag-bytestream-2026-08-11.md));
  and the columnar edge-property word backings merged, **240 B → 168 B per column**
  ([`docs/benchmarks/edge-property-column-2026-08-11.md`](docs/benchmarks/edge-property-column-2026-08-11.md)).
- **Bolt stopped flushing per message: 6.03× less CPU on a 1000-row result**
  (rmp #2410). `ChunkedWriter.WriteMessage` ended in a `Flush()` and every Bolt
  `RECORD` is one message, so a K-row result issued **K `write(2)` syscalls** and the
  `bufio.Writer` in front of the connection could never accumulate anything. A live
  profile put **70.19 % of all server processor time in `Syscall6`**, and **97 % of
  GoGraph's per-row cost was delivery rather than computation**. Auto-flush stays on by
  default so every existing caller is unchanged; only the Bolt server opts out, and it
  flushes explicitly on every message that is not a `RECORD` — the protocol terminates
  every run of `RECORD`s with a summary, so the buffer is always drained before the
  exchange can block. Memgraph had fixed the identical defect.
- **The apply gate woke a thundering herd: 7 425 → 111 897 commits/s at 1024 writers
  (+1407 %, 15.1×)** (rmp #2221). `Tx.advanceApply` woke the sequence-ordered apply gate
  with `sync.Cond.Broadcast`, so every committed transaction woke **every** parked
  committer; all but the holder of `seq+1` re-checked, failed and parked again — O(N)
  wakeups per commit and O(N²) across a concurrent batch. A CPU profile put **77 % of
  all samples in `sync.(*Cond).Wait`**, all of it beneath `waitApplyTurn`. The gate is a
  strictly sequential chain, so a completing transaction has exactly one possible
  successor, and it now wakes that one through a per-sequence parking slot — O(1) in the
  number parked. **Throughput is now monotonic in concurrency**, 264 → 111 897 across
  1 to 1024 writers, where it previously peaked at 256 and **collapsed 3.9×**. Mean
  group size 26.8 → 424.0. No change at 1, 8 or 64 writers (p = 0.734 / 0.288 / 0.310).
  The tell that separated this from a coalescing failure was that the group *shrank* as
  writers were added: the re-parked herd starved the `Append` path that fills the next
  batch ([`docs/benchmarks/apply-gate-wake-2026-07-27.md`](docs/benchmarks/apply-gate-wake-2026-07-27.md)).
- **WAL group commit reaches the engine write path**: **fsyncs per commit fall 300×**
  from one writer to 1024, monotonically at every step. The single-writer rate is
  unchanged at ~270 commits/s — an fsync per commit is an fsync per commit.
- **The aggregation pre-projection no longer materialises an unnamed relationship.**
- **The commit's allocation rate is the write ceiling, not a lock.** One autocommit
  `CREATE (n:Account {id: $id})` went from **56 allocations / 4 248 B to 43 / 4 159 B**
  (−23.21 % at one writer, −24.07 % at 32), with latency improving at every writer
  count ([`docs/benchmarks/mvcc-commit-allocation-cut-2026-08-08.md`](docs/benchmarks/mvcc-commit-allocation-cut-2026-08-08.md)).
- **Graph algorithms**: parallel triangle counting −38.16 %, Hopcroft–Karp −16.09 %,
  Prim MST −8.38 %, direction-optimising BFS −8.29 %, label propagation −6.39 %.
  Seeding a 20 000-node / 60 000-edge graph through Cypher: 821 → 444 ms (**1.85×**).
- **Smaller internal cuts on the write and plan paths**, none of them changing observable
  behaviour, all of them contributing to the aggregate figures above: **one mapper shard
  acquisition per write instead of two**; `analyseNodeScalarUse` **memoised per cached
  plan** behind a declared ceiling; the row-context schema walk **derived once per
  execution**; a dynamic column's backing **pre-sized when the `Put` commits it**; a
  small floor reserved on commit rather than the whole chunk capacity; the relationship
  presence answer **interned** instead of allocated per row; **13 of a commit's 56
  allocations** removed as ladders and escapes; the count shards **padded with a
  lock-free fast reject**; and the snapshot CSR apply **bracketed in one adjacency commit
  window** during recovery.
- **Costs, reported as measured.** Seven independent benchmarks in unrelated packages
  regressed **7–16 %**, all of them paths that walk a graph and read its properties;
  `WriteCSV` is the cleanest case, with B/op and allocs/op **byte-identical** while time
  rose 15.72 %. **Recovery is 1.51× slower** for the same 15 000 replayed ops
  (5.680 → 8.561 ms) with **+2.9 % bytes per node on disk** — the extra 8 bytes are the
  commit timestamp. `DIMACS_SF1_SSSP` moves further at the tail than at the mean:
  **p95 338.9 → 442.6 µs (+30.58 %)**. `store/bulk`'s large load pays +7.52 %
  allocations. **A deferred UNIQUE release costs 3–10 % of constrained write
  throughput** (rmp #2425, open, three candidate causes already eliminated).
- **Steady-state memory was not the price of MVCC.** On the same 20 000-node /
  60 000-edge graph, heap in use +1.3 %, peak RSS +0.2 %, and **mallocs −10.6 %** —
  fewer, larger allocations. The settled reading is identical to the post-seed reading,
  so version chains are reclaimed rather than retained.
- **The headline guard-band set is unchanged to a few percent, with every allocation
  count identical to v0.10.0** (0, 4, 0, 1 280, 10): `Dijkstra_PostWarmup` +3.83 %,
  `Dijkstra_Large` +4.02 %, `BFSDirectionOpt_PowerLaw` +3.23 %, `Yen_K100` +1.51 %,
  `Brandes_RandomGraph` +1.47 %, and the two post-warmup traversal paths still allocate
  **zero** bytes per call. The read-path concurrency sweep is **flat at 75–89 ns/op with
  zero allocations from 8 to 1024 goroutines** — a 128× over-subscription of 10 cores —
  at the cost of **+14.35 % on the uncontended single-goroutine path** (7.68 → 8.782 ns),
  which independently reproduces the +9.28 % the 2026-08-10 delta measured on the serial
  variant.

#### A regression that is a defect, not a trade-off — OPEN

- **The parallel-scan tier outranks the min-cardinality label anchor by 21× in the
  DEFAULT configuration** (rmp **#2431**, open). On a fixture of 100 000 `:Common` nodes
  of which 1 000 also carry `:Rare`, `MATCH (n:Common:Rare) RETURN n.k` regressed from
  **230.3 µs at v0.10.0 to 4.98 ms** — **≈21.6×** — with 131× more allocations, and
  `BenchmarkMinLabelScanSelective_MinLabel` is now **slower than its own `_FirstLabel`
  control**, which is the relationship its name asserts. Attributed at this tag by
  sweeping `cypher.EngineOptions` with **1 000 rows asserted in every arm**: the default
  takes 4.087 ms on `ParallelScanProject`, `DisableParallelScan` takes **0.194 ms** on
  `NodeByLabelScan [Rare]`, and disabling the bitmap intersection changes neither the plan
  nor the time. **The min-cardinality anchor still works — at 0.194 ms it is faster than
  v0.10.0 — it is simply never chosen**, and the parallel-scan plan is worse than even the
  legacy full-scan plan (3.308 ms) the anchor was built to replace. This is a gate
  admitting a plan it should reject, not a trade-off. Full attribution in
  [`docs/benchmarks/v0.11.0.md`](docs/benchmarks/v0.11.0.md) §5.

### Security

- **A HIGH-severity default is closed** (rmp #2421): both engine-wide memory ceilings
  resolved to **unlimited** whenever `GOMEMLIMIT` was unset, which is the Go runtime's
  default state — so the per-unit budgets were finite while the aggregate one was not.
  The ceilings are now bounded by default and derived from the container's cap where
  one exists. **Not established: no container was ever run** — the cgroup read paths are
  unit-tested against injected limits but have never executed on the certification
  host, which is `darwin/arm64` and has no cgroup filesystem.
- The cgroup read is annotated for `gosec`, and example 26's pprof helpers satisfy it.

[0.11.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.11.0

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

[0.10.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.10.0

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
