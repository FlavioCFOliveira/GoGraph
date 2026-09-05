# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.13.0] — 2026-09-05

**100 commits** — 30 fixes, 27 performance changes, 16 documentation, 12 test, 4 features,
4 build changes, 3 chores, and 4 merges. Counted at `b523878b`, the last commit before release
preparation; the release-prep commits that carry this entry are necessarily not in their own
count. **Two sprints delivered work in this window — 352,
*GoGraph module deep profiling and optimization* (31 tasks), and 353, *GoGraph Optimization
Laboratory* (55 tasks) — and all 86 closed.** 731 files changed, 74 802 insertions,
6 671 deletions (`git diff --shortstat v0.12.0..b523878b`).

Three things define the release.

**It is the module's two deep-profiling and optimisation cycles.** Sprint 352 (32 commits)
audited the module for bottlenecks and then worked the Cypher read path; sprint 353
(63 commits) built a committed contention observatory and worked the index, `graph/lpg`,
`metrics` and Bolt surfaces under concurrency. **27 of the 100 commits are `perf`**, against
4 in the whole of `v0.12.0`. Every one of them carries its own interleaved A/B with a noise
floor measured first, and every figure quoted below is that commit's own measurement of its
own change on the shape it names — not a release-level claim.

**Seven changes are marked breaking, and every one of them makes a previously *silent*
failure loud.** Two durable formats now refuse a field they would have truncated or written
unreadably (#2742, #2743); three `graph/lpg` removal primitives now report a refusal that
their callers were journalling an undo for regardless (#2724, #2725, #2726); a panic in the
commit window no longer wedges every later committer on the store (#2727); and Cypher
accepts `EXPLAIN` and `PROFILE` as statement prefixes, which promotes two words to keywords
in prefix position (#2721). Pre-1.0 the minor digit absorbs all seven, as
[docs/semver.md](docs/semver.md) provides. They are set out in full under
[*Breaking changes*](#breaking-changes--what-a-caller-must-do).

**A great many published claims were refuted by their own measurement, and are recorded as
refuted rather than quietly dropped.** The write barrier that the round-2 inventory ranked
at 72–99 % of write-path blocked time holds **zero** of it (#2697). The Bolt transport the
scaling column was suspected of throttling is not the limiter, and the published ratios
*understate* a real socket rather than flattering it (#2711). A ceiling table published as
"every arm is a lower bound" was **filtered**, and three of the seven omitted arms sit above
1.00 (#2712). `append(msg, make([]byte, n)...)` never allocated the temporary the profile
appeared to attribute to it (#2716). `adjlist.Snapshot` pinning would have **lost** on
performance as well as being unnecessary (#2704). The row-path copies cost at most 7.12 %
of allocations on every mainstream shape, so the task closed on the negative result (#2702).

**`go.mod` and `go.sum` are byte-identical to `v0.12.0`.** Same `toolchain go1.27.0`, same
`go 1.26` directive, same nine direct and ten indirect dependencies at the same versions.
There is nothing to report under a dependency heading and nothing is invented to fill one.

The openCypher TCK gate is unchanged at **3897/3897** (16 006/16 006 steps).
`const tckExecutionBaseline = 3897` in `cypher/tck/runner_test.go` is untouched and **no
`.feature` file changed in this window**, so the scenario population is the one `v0.12.0`
was measured against — including across a grammar change (#2721) and eight MVCC repairs.

**Read [`release-notes/v0.13.0.md`](release-notes/v0.13.0.md) before upgrading.** There are
no data migration steps, but seven breaking changes and a dozen observable behaviours are
listed there.

### Added

#### Cypher — `EXPLAIN` and `PROFILE` reach the query language

- **`EXPLAIN` and `PROFILE` are accepted as statement prefixes** (#2721). The plan
  renderers were reachable only from Go, so a Cypher client — every Bolt driver included —
  could not ask for a plan at all, and the gap was recorded as a known driver-compatibility
  hole leaving `ResultSummary.Plan()`/`Profile()` nil. `EXPLAIN` returns the statement's own
  column signature with **zero rows** and does not execute; `PROFILE` returns the query's
  real rows. The plan travels *beside* the result — `Result.Plan()` / `Result.Profile()` in
  Go, the `plan`/`profile` fields of the terminal Bolt `SUCCESS` — and at most one is ever
  populated, so an estimate can never be mistaken for a measurement. Driver-compat rises
  **28 → 30 of 37**. See *Breaking changes*.
- **`cypher.Result.Plan` and `cypher.Result.Profile`** (#2721), the Go-side accessors for
  the plan a prefixed statement produced.
- **`cypher/parser.ParseStatement`, `parser.PlanMode`, `PlanModeNone`, `PlanModeExplain`,
  `PlanModeProfile` and `PlanMode.String`** (#2721). `parser.Parse` is unchanged and every
  existing caller compiles untouched.
- **`cypher.Engine.ExplainTable` and `cypher.Engine.ProfileTable`** (#2701) — the columnar
  counterparts of `Engine.ExplainLogical` and `Engine.Profile`. Each renders a Neo4j-style
  fixed-width table (`Operator | Est.Rows | Vars`, and
  `Operator | Rows | DbHits | Time (ms)` with a `Total` line) instead of an indented tree,
  which is what makes two operators' figures comparable at a glance. Neither is a second
  derivation of the plan: `ExplainTable` shares `ExplainLogical`'s single traversal — the
  one that substitutes index seeks, applies the count-store-gated reorderings and computes
  the cardinality estimates — and `ProfileTable` renders the same captured measurement tree
  `Profile` renders, from one execution. `ExplainLogical`, `Explain` and `Profile` return
  byte-identical output to before. The `Vars` column is new information: no tree rendering
  prints it. Documented in
  [docs/cypher.md](docs/cypher.md#inspecting-a-query-plan).
- **`cypher/explain.PlanRow` and `cypher/explain.FormatPlanTable`** (#2701) — the plan
  table's row type and renderer, extracted from `TextTree` so the engine and `TextTree`
  share one formatter.

#### Cypher execution — new operator surface

- **`cypher/exec.RelTypeColumn`, `NewRelTypeColumn`, `NewRelTypeColumnFor`,
  `RelTypeColumn.Admit`, `RelTypeColumn.RelTypeColumnBytes`, `RelTypeAdmit` with `Active`,
  `Fwd`, `Rev` and `RevExact`, `RelTypeMask` and `RelTypeUnknownCode`** (#2251) — a dense
  `[]uint32` relationship-type column per direction, cached beside the CSR pair it
  describes, replacing a `map[uint64]string` probe **per CSR slot**. Admission compiles the
  pattern's accepted types into a bitmask over `LabelID` space, so a type check is one
  indexed load and is O(1) regardless of type count.
- **`cypher/exec.NewCreateIndexPairOp`, `IndexRegistration`, `CreateIndexOp.Registered` and
  `CreateIndexOp.CompanionRegistered`** (#2703) — `CREATE INDEX` now registers a primary
  index and its companion inside **one** visibility-barrier invocation. `NewCreateIndexOp`'s
  signature and behaviour are unchanged. See *Fixed — ACID*.
- **`cypher/exec.UnwrapProfiled` and `Profiler.WrapChunk`** (#2665), so a plan recogniser
  can see through a profiling wrapper instead of declining under `PROFILE`;
  **`NewUnionInstrumented`** (#2665) for the same reason on `Union`.
- **`ParallelScanProject.PlanDetail`, `ParallelCountScan.PlanDetail` and
  `ParallelAggregateScan.PlanDetail`** (#2720), which render
  `parallel tier; db-hits not counted` rather than a bare `0`.
- **`cypher/exec.ColumnarProject.WithChunkRowByteBudget`** (#2655) and
  **`Top.WithByteBudget`** (#2509) — the row-byte ceilings the columnar pre-projection and
  `Top` never had. Both raise the same `ErrProjectionRowTooLarge` / `ErrSortMemoryExceeded`
  sentinels their siblings already raised, so a caller cannot tell which pre-projection ran
  from the error it gets.
- **`cypher/exec.Project.WithRowBinder` and `cypher/exec.RowBinder`** (#2658) — the bracket
  that lets a projection build **one** row context per row instead of one per projection
  item.
- **`cypher.EngineOptions.DisableAdjacencyCountRewrites`** (#2647), which makes a
  differential suite's oracle arm unrewritable **by construction** rather than by accident,
  following the existing `DisableAnchorSwap` / `DisableParallelScan` convention. It gates
  all four dispatch sites and deliberately does **not** gate the planner, so plan shape is
  identical on both arms.

#### Cypher expressions — lazy relationship values

- **`cypher/expr.LazyRelationshipValue` with `ID`, `StartID`, `EndID`, `RelType`,
  `Property`, `Kind`, `Equal`, `Hash` and `String`, plus `NewLazyRelationshipValue`,
  `RelSource` and `RelationshipResolver`** (#2388). `r.k` is answered on demand through a
  resolver, mirroring what `upgradeNodeIDToValuePartial` already did for nodes, instead of
  allocating a fresh one-entry Go map per relationship per row.
- **`graph/lpg.Graph.EdgePropertyByHandle`, `EdgePropertyByHandleAsOf`,
  `EdgePropertyByHandleIDAsOf` and `ReadView.EdgePropertyByHandle`** (#2388) — the
  single-key duals of the existing plural forms, added because a probe found the TCK drives
  the by-handle route 217 times of 218 while `examples/26_social_scale_bench` is 100 %
  per-pair; covering only one route would have left the new code exercised once in 3897
  scenarios.
- **`graph/lpg.EncodeSlotLabel`** (#2251), the slot-label encoding the relationship-type
  column reads.

#### Cypher IR

- **`cypher/ir.NewTopWithBound`, `ir.TopBound` and the new `ir.Top.Offset`,
  `Top.OffsetExpr` and `Top.LimitExpr` fields** (#2509), which is what lets
  `ORDER BY … SKIP … LIMIT` plan as `Skip(s)` over `Top(s+k)` and keeps both clauses visible
  in `EXPLAIN`.
- **`cypher/ir.PatternFormOf`** (#2648), which recognises the block form
  `COUNT { MATCH … }` as the pattern form so it reaches the adjacency rewrites.
- **`cypher/ir.ExprIsDeterministic` and `ir.IsNonDeterministicCall`** (#2509), the
  determinism predicates the fused plan needs before it may reorder.

#### New typed errors

- **`store/txn.ErrFieldTooLong`** (#2742) — a commit whose label, key or value exceeds its
  WAL length prefix now fails with it rather than being silently truncated.
- **`store/snapshot.ErrFieldTooLong`** (#2743) — a snapshot capture that would emit a field
  its own reader is required to refuse now fails the checkpoint with it, **before** the WAL
  prefix is truncated. The name deliberately mirrors `store/txn`'s: the two durable formats
  refuse an over-long field in the same shape, with the same sentinel name and the same
  message layout.
- **`store/csrfile.ErrInvalidFixtureSpec` and `store/csrfile.ErrNotRepresentable`** (#2744)
  — `BuildFixture` refuses a degenerate spec instead of panicking with an integer divide by
  zero, and `Layout` refuses a zero-total CSR on the write side, where the caller's own
  graph is not corruption.

#### Observability

- **A per-message Bolt latency histogram** (#2715). `bolt/server` serves the module's entire
  network surface and published **no latency at all** — the only per-message emissions were
  `bolt.pool.decoder.get`/`.put`, which is pool bookkeeping rather than a service metric.
  Thirteen series names are built once into a package-level array and indexed, never
  concatenated at the call site; the emission is one deferred `metrics.Time` on
  `Session.HandleMessage`, so a session driven directly — as `internal/sim` does — is
  observed on the same terms as one over a socket. The label **cannot come from the wire**:
  `proto.DecodeRequest` rejects an unknown struct tag and `serve.go` answers
  `Neo.ClientError.Request.Invalid` without dispatching, so a hostile peer cannot mint a
  series; `other` is reachable only by an in-process caller and lands in one bucket.
  Measured with the real backend installed, the emission **scales 6.3×** — 69.1 ns at one
  goroutine to 10.9 ns at eight — and reads **0 allocs/op with and without `-race`.**
- **`bench/contention.Metrics` records `PerfCores` and `EffCores` beside `NumCPU`** (#2690,
  #2691). Zero means the split could not be determined, which is the honest value for a
  platform that does not report it and **not** an assertion of homogeneity.

#### Gates, harnesses and tooling

- **The vulnerability gate that never existed** (#2722). Investigating a "broken
  `govulncheck`" found something larger: **there was no gate.** `govulncheck` appeared in no
  Makefile target, no `make ci` path and no `.sh`/`.yml`/`.yaml` file anywhere in the
  repository — confirmed by `command grep -rn` and an independent Python filesystem walk, 7
  hits all prose. `make vulncheck` now exists, is wired into `ci`, `ci-soak` and
  `ci-nightly` immediately after `build`, and `release-preflight` inherits it. See *Security*.
- **A `test-uninstrumented` phase** (#2709), wired into `ci`, `ci-soak` and `ci-nightly`,
  for an allocation-bound security invariant that ran in **no phase at all**: its file is
  `//go:build !race` so both `-race` phases compiled it out, and it skips when `CoverMode`
  is set so `cover-gate` skipped it. Each guard is individually correct; their
  *intersection* left the assertion running nowhere. Cost 0.39–0.55 s.
- **`bench/contention`, the committed contention observatory** (#2678, #2679, #2690).
  Nothing in the repository enabled Go's contention profilers: no source called
  `runtime.SetMutexProfileFraction` or `SetBlockProfileRate`, so **every prior contention
  claim rested on the shape of a throughput curve, never on lock-site attribution.** The
  sweep grew from 6 workloads to 19 (#2679) and then to 25 with thirteen ceiling arms
  (#2690), and the ladder gained levels 2, 3, 4 and 6 alongside the mandated
  1/8/64/256/1024 (#2691).
- **`bench/audit352`** (#2643), the reproduction harness for the sprint-352 bottleneck
  audit, and **`bench/entryheap`** (#2684), the rebuilt and now *committed* GC-shape
  harness — the `#2683` baseline was never committed and had to be reconstructed.
- **`internal/planseam` and `internal/sortseam`** (#2649, #2652), the plan- and sort-build
  counters an out-of-package harness needs to assert it measured the operator it attributes
  to. `Explain` would not have sufficed: a shape can plan differently on a fresh engine and
  on a warmed one, so what `Explain` renders is not proof of what `Run` builds.
- **The lint configuration gained five linters and a meta-check.** `.golangci.yml` now
  enables **16** linters, was 11: `containedctx`, `forcetypeassert`, `nilerr` and
  `wastedassign` were enabled, `nolintlint` was enabled and `gocritic`'s `whyNoLint` taken
  out of `disabled-checks` (#2708). `nolintlint`'s `allow-unused` then became a gate in its
  own right (#2746). Stated plainly rather than implied: **none of the four newly-enabled
  linters was clean** — every one required annotation or a fix.
- **Nine new measurement and audit documents**:
  [`docs/audit-bottlenecks-2026-08-27.md`](docs/audit-bottlenecks-2026-08-27.md) (#2643),
  [`docs/contention-inventory-2026-09-01.md`](docs/contention-inventory-2026-09-01.md) and
  [`docs/contention-inventory-round2.md`](docs/contention-inventory-round2.md) (#2679,
  #2690),
  [`docs/bolt-evaluation-2026-09-03.md`](docs/bolt-evaluation-2026-09-03.md),
  [`docs/explain-profile-honesty-audit-2026-09-03.md`](docs/explain-profile-honesty-audit-2026-09-03.md)
  (#2720),
  [`docs/mvcc-life-record-defects-2026-09-03.md`](docs/mvcc-life-record-defects-2026-09-03.md)
  (#2723),
  [`docs/dst-determinism-audit-2026-08-28.md`](docs/dst-determinism-audit-2026-08-28.md)
  (#2663),
  [`docs/security-vulnerability-scans.md`](docs/security-vulnerability-scans.md) (#2722),
  and three benchmark records under `docs/benchmarks/` (#2654, #2251, #2509).

### Changed

#### Breaking changes — what a caller must do

Seven commits carry a `!` marker. **Two further exported signatures changed without one**
and are listed here too, because a changelog that hid them behind the marker would be
reporting the marker rather than the change.

- **`store/snapshot`: a capture refuses at write time what the reader will refuse** (#2743,
  marked `!`). *Before:* phase-1 capture accepted an oversize field with no error, phase 2
  published and fsynced a CRC-valid snapshot, phase 3's self-sufficiency gate matched file
  **names** and never opened `properties.bin`, and `checkpoint.go` then **discarded the WAL
  prefix**. On restart `ReadProperties` refused and the store never opened again, with the
  WAL that held the data gone — loud failure *and* unrecoverable loss. *After:* the refusal
  lands in phase 1 under the commit lock, before any file is written, so the outcome is a
  failed checkpoint and a **retained** WAL. **What a caller must change:** handle
  `snapshot.ErrFieldTooLong` from the checkpointer and from the exported component writers
  (`WriteIndexDefs` takes caller-supplied specs and is therefore bypassable, so it is guarded
  now too), and keep an encoded property value under **1 GiB**, a string-table entry — a
  property key, label name, edge-handle label or key — under **1 MiB**, a mapper key under
  **1 GiB**, and an index-definition identifier under **64 KiB**. The reader caps were **not**
  raised to meet the writer, deliberately: they *are* the anti-OOM control, and raising them
  to the writer's 4 GiB would mean `make([]byte, n)` up to 4 GiB on an untrusted file — the
  memory-DoS class this project already carries a recorded HIGH finding for.
- **`store/txn` and `store/wal`: a field that does not fit its WAL length prefix is refused,
  not truncated** (#2742, marked `!`). *Before:* an acknowledged commit silently lost or
  corrupted data. Reproduced through the public API alone — `Tx.AddNode`, `SetNodeLabel`,
  `Commit`, `wal.Close`, `recovery.Open`: a 65 535-byte node label round-tripped, a
  65 536-byte one committed with `nil` and recovered as **0 bytes**, and a 70 000-byte one
  committed with `nil` and recovered as a **wrong 4 464 bytes** (70 000 mod 65 536). ACID
  Durability and Consistency. *After:* `Tx.Commit` and `Tx.CommitWALOnly` fail with
  `txn.ErrFieldTooLong`, the offending transaction consumes a sequence and applies nothing,
  and the store stays usable. **What a caller must change:** keep a label, property key or
  other `uint16`-prefixed schema string at or under **65 535 bytes** and a property value,
  list element or list element count at or under **4 294 967 295 bytes**; check the `Commit`
  error. `store/wal.Encode` additionally refuses an assembled frame over **1 GiB** with the
  existing `wal.ErrFrameTooLarge`, because a `PropList` of a thousand individually-legal
  5 MB elements assembles a 5 GB frame that only the framer can see — a durable, fsynced,
  acknowledged commit that `Decode` was **required** to refuse. The guard sits at the
  encoder rather than at the API deliberately: `cypher/api.go` discards the result at
  **eighteen** sites of the form `_ = a.tx.SetNodeLabel(...)` after having already done the
  `lpg` write, so an API-side check would have turned silent corruption into silent
  omission, which is harder to detect.
- **`store/txn`: the apply gate closes from a `defer`** (#2727, marked `!`). *Before:* a
  panic between minting the dense sequence and advancing the apply gate left a permanent hole
  in the chain and **every later committer on that store parked for ever**. It affected both
  durable commit entry points, not only `CommitWALOnly`, and the vulnerable window opened at
  the first `wal.Encode`, spanning the whole encode–append–fsync. *After:* `closeApplyGate`
  waits its turn and then advances, exactly as the success path does, and the writer release
  moved into the same defer. **What a caller must change: nothing.** The `!` marks an
  **unexported** signature change — `appendOnly` drops the minted sequence from its returns,
  the transaction now carrying it — so no exported identifier, signature or field changed in
  this commit. The naive fix was rejected rather than adopted: a bare deferred
  `advanceApply(seq)` sets `appliedSeq` unconditionally, so it would let `seq+1` apply while
  `seq-1` had not and would drive `appliedSeq` **backwards**.
- **`graph/lpg.WriteView.RemoveNode` returns `bool`** (#2726, marked `!`), and
  **`WriteView.RemoveEdge` returns `bool`** (#2725, marked `!`). *Before:* both were `void`,
  so a refusal was invisible; `cypher/api.go` pre-probed with `IsTombstoned` / `HasEdge`,
  called the void primitive, and journalled the undo inverse off the **probe** rather than
  the **outcome**. A removal refused by an already-doomed transaction mutated nothing, and
  its rollback then *resurrected* the entity — a node the workload never created, or an arc
  no committed transaction ever made. *After:* the primitive reports its refusal and the
  adapters gate their journalling and their side-effect counters on it. **What a caller must
  change:** a call used as a statement still compiles, so most code needs nothing. Code that
  takes a **method value** typed `func(N)` / `func(N, N)`, or satisfies an interface
  declaring the old void signature, must be updated. `Graph.RemoveNode` and
  `Graph.RemoveEdge` stay `void`.
- **`graph/lpg`: a rolled-back `DELETE` no longer hides a committed node from older
  readers** (#2724, marked `!`). *Before:* an ACID Isolation and Consistency violation. The
  life store is one record deep per direction, a `Rollback` publishes a **real** instant
  rather than `AbortedTS`, so the undo replay's revive overwrote the committed birth — and
  `aliveBefore`, the #2445 repair, masked the loss only while the died half was still the
  rollback's own. A later, unrelated delete replaced it, the pair flipped to born-then-died,
  and `NodeExistsAsOf` returned **false** for every reader older than the rollback: a dirty
  read and a snapshot move at once. *After:* the pair is kept intact and the birth records
  whether the node was already alive when that transaction began. **What a caller must
  change: nothing to compile.** Node visibility changes: a node that previously vanished
  from a reader older than a rolled-back delete is now correctly returned. Over 1000 seeds
  per arm at `Ticks=1200`, unclean runs go **4 → 2** on the no-crash arm and **5 → 1** on
  the crash arm.
- **`cypher`: `EXPLAIN` and `PROFILE` are statement prefixes** (#2721, marked `!`). *Before:*
  `EXPLAIN MATCH (n) RETURN n` did not parse — the grammar had no such token. *After:* it
  parses, executes nothing, and returns the statement's own column signature with zero rows;
  `PROFILE` runs the query and returns its real rows. **What a caller must change:** a query
  whose text begins with the bare word `EXPLAIN` or `PROFILE` in statement position now means
  something. Elsewhere both words remain usable as identifiers — listing them in the `symbol`
  rule is load-bearing and was **falsified** rather than assumed: removing those alternatives
  and regenerating makes all seven cases of `TestPlanPrefix_IdentifiersSurviveTokenisation`
  fail, one with a parser panic on `MATCH (explain) RETURN explain`. Two limitations are
  documented rather than hidden: `PROFILE` refuses a **writing** statement, identically to
  `Engine.Profile`'s existing refusal, and neither prefix may precede a schema statement,
  which the hand-written DDL parser handles outside this grammar — a prefixed one is a syntax
  error and therefore executes nothing. Token ids were measured rather than assumed: all 178
  token names with id ≤ 89 are **unchanged**; `EXPLAIN=90` and `PROFILE=91` take the last
  keyword ids, and only `ID` and the nine literal/whitespace tokens beneath it move, by +2 —
  the same shift `FOREACH` made. `make generate-cypher-parser` reproduces
  `cypher/parser/gen/` byte for byte.
- **`cypher/exec.NewTop` takes a `maxRows` parameter** (#2509, **not** marked `!`).
  `NewTop(child, keys, n)` becomes `NewTop(child, keys, n, maxRows)`. This is a source-breaking
  signature change to an exported constructor in a package
  [docs/semver.md](docs/semver.md) lists in the public API surface. `Sort` had `maxRows` and
  `WithByteBudget`; `Top` had neither, masked only because its bound was always a small
  literal — fusing a **parameterised** bound without hardening would have turned a hostile
  `$skip` into an OOM kill where `Sort` returns `ErrSortMemoryExceeded`. `maxRows` is a
  constructor parameter rather than an option precisely so it cannot be forgotten at a call
  site. **What a caller must change:** pass a row cap.
- **`graph/lpg.WriteView.RemoveAllEdgesFrom` returns `bool`** (#2694, **not** marked `!`),
  mirroring its siblings above and for the same reason. Source-compatible for a call used as
  a statement; a method value or interface satisfaction breaks.

No other exported identifier changed signature. **19 exported identifiers were removed**, all
in `cypher/exec` and all previously unwired (see *Removed*), and **77 were added**, of which 8
are in `cypher/parser/gen`, which [docs/semver.md](docs/semver.md) excludes from the public API
by intent. No exported struct field was removed.

#### Behaviour a caller can observe

- **`PROFILE` renders the plan that actually runs** (#2665).
  `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN b.salary` returned 960 000 rows from `Run`
  and `EXPLAIN` rendered `ColumnarProject`/`ColumnarFilter`/`columnarExpand`, while `PROFILE`
  rendered `Filter`/`Expand` — **a different plan** — and reported `rows=0` at every level
  above the leaf, **with no error**. Three defects chained: a recogniser asserting
  `expandOp.(*exec.Expand)` declined because `Wrap` preserves every interface but cannot
  preserve a **concrete** type; the decline happened *after* `buildOperator` had written the
  caller's schema, so the fall-through rebuilt on a polluted map; and `Expand`'s "row too
  narrow, skip silently" guard converted that corruption into a wrong answer. A second
  instance in `tryFuseCyclicIntersect` had been **documented as intended**. The census is
  closed rather than sampled: a `go/ast` gate finds all 68 operator structs in `cypher/exec`
  and fails until a new one is covered or excluded with a written reason.
- **`PROFILE`'s db-hits stop reporting a derived count as measured** (#2720).
  `VarLengthExpand` is now **measured**, at no added cost, by reading the traversal counter
  the #1478 budget already maintains: on a broom graph, `[*1..3]` and `[*3..3]` walk the same
  202 relationship slots and previously reported `dbhits=202` against `dbhits=1`, a **202×**
  divergence. A type-filtered `Expand` was **100×** out (`-->` reported 100, `-[:KNOWS]->`
  reported 1 over the same 100-slot CSR walk). The three parallel leaves now carry
  `PlanDetail()` `parallel tier; db-hits not counted` rather than a bare `0`. One
  `EXPLAIN` fidelity exception is documented rather than fixed: a parameterised predicate with
  **no** parameters supplied renders `NodeByLabelScan` where the bound form builds
  `NodeByIndexRangeScan`, and nothing on the page said a value was missing.
- **`EXPLAIN` keeps its plan-time notifications** (#2721). A defect found mid-flight: it was
  dropping them, 0 against 1 on a Cartesian product. Seeing those without running the query is
  most of what `EXPLAIN` is for.
- **A `SET` on a write-clause-bound relationship no longer vanishes** (#2705).
  `MERGE (a)-[r:T]->(b) SET r.since = 2020` left `r.since` **null** — silently: no error, no
  notification, and `PropertiesSet` counted the write. Not a `MERGE` defect: plain
  `CREATE (a)-[r:T]->(b) SET r.k` lost it too, as did `MERGE`'s `MATCH` branch, `SET r += {…}`,
  `SET r = {…}`, `WITH r SET`, and `REMOVE r.k`. Three write operators published a synthetic
  packed endpoint pair (`src<<32|dst`) in `RelationshipValue.ID` while both `SET` consumers
  read that field as the stable handle. A second, independent defect was hiding behind it:
  `SetProperty` refreshed only the node row slot, so `SET r.since = 2020 RETURN r.since` read
  stale even once the durable write was correct. **Relationship identity changes with it:**
  under the packing, two parallel `CREATE`s between the same ordered pair published the same
  id, so `r1 = r2` was true for two distinct relationships and `DISTINCT` collapsed them.
- **A correlated `EXISTS` after a write clause is correlated again** (#2659).
  `MATCH (a:P) CREATE (:Q) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) } RETURN a.sid` gave two
  **null** rows where one row with `sid` 1 is correct, and `NOT EXISTS` failed in the opposite
  direction. `existsSubPlan` seeded the inner `Argument` from `outer.Vars()`, which is
  contracted to report only what an operator itself introduces, so `CreateNode.Vars()` dropped
  its child's bindings and `a` never reached the `Argument`.
- **Reverse and undirected traversal under a correlated `Apply` stops returning rows that were
  never correct** (#2251).
  `MATCH (a:P) WHERE EXISTS { MATCH (a)<-[r]-(x) RETURN x } RETURN a.id` returned `[b c]`
  where `[b]` is correct — `c` has no incoming relationship at all. `Expand.Init` reset `srcID`
  and `fwdDone` but not the reverse cursor or the `CREATE`-multiplicity pending queue.
  **The typed form of that query was accidentally correct**, and that is the lesson: the
  per-slot type test was acting as an unintended validity filter on the operator's own stale
  state, so removing the cost uncovered the wrong answer the cost was hiding. Exposed: every
  untyped `<-[r]-` or `-[r]-` under `EXISTS`, `COUNT { }`, a pattern predicate, or any
  correlated `Apply`. `VarLengthExpand`, `ShortestPath`, `AllShortestPaths` and
  `ExpandIntersect` reset their own per-run state and were never affected.
- **`ORDER BY` ties are ordered by arrival, not by heap sift order** (#2509). `Top(3)` over five
  all-tied rows returned the right set in the **wrong order** — ids `[1 2 0]` where the stable
  `Sort` prefix is `[0 1 2]`. It was invisible while the fusion was `SKIP`-free, because a
  caller asking for the best *k* cannot observe the order among ties at the boundary; under
  `Skip(s)` over `Top(s+k)` the window moves into the middle of the stream, so a transposition
  changes **which rows the page contains**. Existing coverage could not reach it: the two
  in-tree tie tests order 3 nodes and 2 groups with no actual tie, and the TCK's entire
  `ORDER BY`/`SKIP`/`LIMIT` surface is six scenarios over 5–16 rows with distinct keys.
- **`store/csrfile.BuildFixture` refuses a degenerate spec instead of panicking** (#2744).
  `BuildFixture(FixtureSpec{Vertices: 0, Edges: 1})` panicked with an integer divide by zero,
  in exported API, while its godoc told the reader the only failure mode "cannot be reached".
  The enumeration found a **worse** class than the one reported: `Vertices` above `MaxUint32`
  did not panic — `uint32(1<<32)` is 0 — so edges were drawn over `[0,5)` while 2³²+5 nodes
  were interned, a **silently wrong graph with no error at all**. Exactly two rules are
  encoded, and the rule is *correctness*, not cost: refuse only where no correct graph exists.
  A large `Edges` count is **not** refused, because every value yields a correct graph, only
  slowly. `BuildFixture` already returned `(*csr.CSR[struct{}], error)`, so the signature is
  byte-identical and the new symbols are additive.
- **`cmd/sim` requires an explicit seed** (#2663). It silently drew one from the auto-seeded
  top-level `math/rand/v2` generator whenever the caller omitted the positional argument, so
  two identical commands ran two different simulations and a violation could not be reproduced
  from its own report. `resolveSeed` now refuses with exit 2, and the `math/rand/v2` import is
  gone, so the command holds **no randomness source at all**. `-list-scenarios` and
  `-coverage-report` without `-swarm` still need no seed.
- **`cypher.EngineOptions.EdgeTypeFilterCacheCapacity` and
  `DefaultEdgeTypeFilterCacheCapacity` are deprecated, not removed** (#2251), so a caller
  compiling against the current release keeps compiling. Both are inert on every path and claim
  no replacement, because the relationship-type column is type-set independent and there is
  nothing left to size.

#### Query plans — result-identical, cost changed

- **The labelled count pushdown is un-gated from the parallel-scan threshold** (#2654).
  `tryBuildLabelCountScan` gated an O(1) maintained label-count read on `useParallelScan` —
  that is, on `DefaultParallelScanThreshold` (50 000 live nodes, strict `>`) and on the
  `DisableParallelScan` flag. A parallelism threshold was guarding a **serial constant-time
  answer**, and it read the wrong cardinality: it thresholded the whole graph's `LiveOrder()`,
  not the label's own count, so 100 `:Rare` nodes in a 60 000-node graph were answered in O(1)
  while 50 000 `:Item` nodes in a 50 000-node graph were not.
- **The block form `COUNT { MATCH … }` reaches the adjacency rewrites** (#2648). It sets
  `.Query` and leaves `.Pattern` nil, and every fast path is fed `sub.Pattern`, so
  `recogniseDegreePattern` and `recogniseLabelledHopPattern` were **structurally unreachable**
  from that spelling. Block is now normalised to pattern — the exact inverse of GoGraph's own
  `countToSingleQuery` desugaring — which makes it semantics-preserving by construction, and
  the round trip is asserted on **pointer identity**. `OPTIONAL MATCH` is excluded for
  correctness, not caution: `COUNT { OPTIONAL MATCH … }` emits one row of nulls when nothing
  matches, so its count is 1 where a degree says 0.
- **`count(v)` normalises to `count(*)` where the variable is provably bound and non-null**
  (#2657). Three of the four named pushdowns **already** accepted `count(n)`; the real gap was
  the general path, where a `Selection` or an `Expand` between the aggregate and the leaf made
  `tryBuildCountRows` refuse the argument. `rewriteCountVarToCountStar` is an **allowlist with
  a default reject**: `OptionalExpand`, `OptionalApply` and `Argument` are named and refuse;
  binders are the four node scans plus `Expand`; `Selection` and `VarLengthExpand` are
  pass-through only; everything else declines, including any operator added later.
- **`ORDER BY … SKIP … LIMIT` fuses into `Skip(s)` over `Top(s+k)`** (#2509), Neo4j's shape,
  which keeps both clauses visible in `EXPLAIN`. A parameterised bound lost the fusion its
  identical literal received — `LIMIT 5` planned as `Top`, `LIMIT $m` as `Limit` over `Sort` —
  and **any** `SKIP` blocked it, so `SKIP 0 LIMIT 10` full-sorted 120 000 rows to ship 10.
- **An aggregate keeps the columnar chain a `WHERE` used to cost it** (#2655).
  `tryBuildColumnarAggInput` requires a `ChunkProducer` child, but under `ir.EagerAggregation`
  the child was built by the ordinary `buildOperator`, which turns a `Selection` into a row
  `exec.Filter` — so the columnar chain was structurally unreachable from under an aggregate,
  and the aggregating form cost **72 % more wall clock** than the form shipping 24 999 times
  more output. All eight of the existing declines are carried over verbatim.
- **A subquery build carries the adjacency caches** (#2646). `(*buildOpts).forSubquery()` is a
  deliberate allowlist and it omitted `csrPairCache` and `edgeTypeFilterCache`, so with both
  nil every outer row under `Apply` rebuilt the forward CSR, its reverse transpose and the whole
  position-keyed edge-type filter: **Θ(V+E) per row**. Both are keyed to graph state, which no
  scope owns, so carrying them is consistent with the allowlist's design rather than an
  exception to it.

#### Documentation corrected against the code

Eleven godocs and design comments described behaviour the code does not have. Each is corrected
in place; several were found by a measurement that set out to act on them.

- **`docs/release.md` and `CONTRIBUTING.md` asserted a GitHub branch and tag protection regime
  that does not exist** (#2757). Both opened by stating that `main` and the `v*` tag namespace
  are protected and that the rules are "enforced via repo Settings", then listed required pull
  requests, a required approving review, stale-approval dismissal, required linear history, and
  a `releasers`-team restriction on pushing release tags. `CONTRIBUTING.md` added that release
  tags "must be signed". Probed against the live repository on 2026-09-05, classic branch
  protection returns **404 Branch not protected**, the rulesets collection returns **`[]`**, and
  tag protection returns **404** — and `git log --pretty=%G?` reports `N` for **all 100 commits**
  in this window, so the signing claim was wrong twice over. The repository's own history
  corroborates the absence: `main` carries `f97bbfec`, a **merge commit**, which the
  "require linear history" rule described as active would have rejected. This shipped —
  `.goreleaser.yaml` bundles `docs/**/*` into every release tarball, so the claim reached
  consumers as supply-chain assurance. Both files now lead with the measured absence, keep the
  intended regime as an explicitly-labelled intent, and carry the three probes as dated
  evidence. The one control that **is** real — correctness gating via local
  `make release-preflight` — was verified and kept.

- **The write barrier holds zero blocked time — premise refuted** (#2697). The round-2
  inventory ranked `execUnderBarrier` at 72–99 % of write-path blocked time; the **flat**
  column reads 0 and 0 % on all three write workloads while the **cumulative** column reads
  84.90 %, 89.62 % and 99.08 %. Cumulative attribution locates a subsystem; it does not say
  where goroutines block, and `execUnderBarrier` wraps the entire write path. **No behavioural
  change.** What lands instead is the godoc the task asked for: each ACID guarantee the bracket
  is credited with, named against the mechanism that actually supplies it. A second hypothesis
  fell with it: sharding `propMapShards` 64 → 256 buys 2–3 % against a measured 3.69 % noise
  floor.
- **`adjlist.Snapshot`'s isolation contract described a world that no longer exists** (#2704),
  and the change it was groundwork for would have **lost** on performance too. Adjacency reads
  have not taken a visibility barrier since #2344 — confirmed by observation rather than by
  reading: across 75 profiles **zero** contain a barrier frame, while the identical search
  found real frames in 15 of 15 mutex profiles, so the empty result is evidence of absence
  rather than a broken query. And pinning saves 0.080 ns per adjacency read against a pin
  costing **353.2 ns and 2 304 B**: `shardCount` is 256, so a pin is 256 atomic loads and a
  2 KB allocation, not the "one atomic load per shard plus one allocation" the task's summary
  implied. Break-even is between 1024 and 4096 adjacency reads per pin. The type is retained
  deliberately — it is a correct lock-free topology pin, and **unwired is not obsolete**.
- **The `DETACH DELETE` WAL frame does not wait on what its own comment said** (#2706). The
  reported premise — one frame per adjacency slot rather than per removed edge — is refuted
  twice over: adjacency entries are **compacted, not tombstoned**, so there is no empty slot to
  emit a frame for, and thirteen measured shapes give exactly 1.000 frames per directed edge
  removed. What the premise was hiding is a real over-emission at a **different** site: the
  counter, the frame and the undo inverse all wait on the removal not being *refused*, but the
  counter and the inverse wait on a second condition the frame does not. On an **undirected**
  engine that gap is 2×: a fan-out of 64 writes 128 frames for 64 removed edges, 48.7 % of the
  delete transaction's WAL bytes. Not fixed here — whether Cypher over an undirected LPG is a
  supported configuration at all is unsettled, and that question decides the fix (#2734).
- **The row-path copies are not significant** (#2702). Deleting **every** one of the six sites
  would cut allocs/op by 19.81 % at most, on a deliberately adversarial
  `RETURN DISTINCT <unique-key>` shape, against the task's own 20 % bar; on every other measured
  read shape the ceiling is ≤ 7.12 % and on the mainstream read path it is exactly **0 %**.
  Three premises in the task were false at HEAD and saying so was the honest answer:
  `driver.go`'s `Drain` is **test-only**, `eager.go` cannot fire on a read plan, and the two
  hash-join sites are dead on the shape implied because that shape plans to
  `ColumnarHashJoin`, which allocates zero at both. Every surviving site now states what breaks
  without the copy, plus its measured share. Comment-only: 6 files, zero non-comment changed
  lines.
- **`index.Manager` does have engine callers** (#2690). The round-2 inventory said it did not.
  The claim came from a grep **truncated at twenty lines**, and every line that survived the cut
  happened to be a doc comment, so absence of calls looked established when it had only been cut
  off. A type-aware cross-reference finds 113 references across 28 production files.
- **`chunk.go`'s operator inventory, `Row`'s ownership contract and
  `DefaultChunkCapacity`'s justification** (#2700). The package advertised a row arena as its
  data model while that arena had no caller, and the columnar layout that succeeded it carried a
  godoc saying nobody used *it* — both statements the wrong way round. `DefaultChunkCapacity`
  justified its 4096 by pointing at `DefaultSlabCapacity`, whose own godoc claimed it was "sized
  to keep a typical pipeline batch within a few cache lines", untrue of 4096 rows. The real
  justification is the executor's cancellation contract — check `ctx.Done()` every 4096
  iterations — which fifteen loops across the package honour.
- **`scan_label.go`'s "zero-alloc contract"** (#2654): `Next` boxes 8 bytes per row for every
  node id ≥ 256, **83.2 % of all bytes** in that query. The boxing itself is untouched; the
  claim is.
- **`graph/adjlist` no longer claims the Cypher engine never reads edge weights** (#2651). It
  does: `undo_record.go` reads `Graph.EdgeWeight` for the `DELETE` undo preimage. The
  consequence is benign on a weightless graph, but an absolute claim contradicted by a real call
  site is a defect whatever its consequence.
- **The recommended graph configuration for Cypher and Bolt is
  `adjlist.Config{Directed: true, Multigraph: true, Weightless: true}`** (#2651), at every
  documented construction site: `docs/bolt.md` (×2), `docs/cypher.md`, the `cypher` package
  godoc and `examples/23_bolt_server`. `Multigraph` is the important half: the package godoc
  showed a bare `adjlist.Config{}`, which is neither directed nor a multigraph, while
  openCypher's model **is** a multigraph — two `CREATE`s between the same ordered pair must
  yield two relationships, and the second instead failed with
  `ErrParallelEdgeInSimpleGraph`. The weights saving was measured on the example's own seeding
  rather than computed: **32.0033 B/node and 0.99998 heap objects/node** at 200 000 nodes and
  degree 4, against noise floors of 0.0076 % and 0.0011 %. Two premises behind the original task
  were refuted first: the snapshot/recovery durability gap it demanded be settled was already
  closed by #1650, and there is **no** Cypher/Bolt graph profile in module code at all —
  `bolt/server` constructs no graph — so this delivers guidance, not a module default.
- **`cypher/exec/profile.go`'s stale note** about `cypher/explain` keeping its own db-hits
  counter, superseded by #2238 (#2701); **`exec/label_count_scan.go`'s** claim that the
  read-path driver holds the visibility barrier across the whole query, false since #2344
  (#2688); and **`appendOnly`'s** godoc describing a `releaseAfterAppend`, a `markInflight` and
  a `Store.doneInflight` that do not exist (#2727).

#### Gates and tooling

- **`make ci` could not fail on a test failure** (#2672). `.SHELLFLAGS` requires GNU Make 3.82
  and macOS ships 3.81, so `-e -u -o pipefail` never applied and `test-short` reported only
  `pkg_time_budget.sh`'s status: a run containing `FAIL bench/audit352` read `MAKE_CI_EXIT=0`
  and continued through lint and cover-gate. The flags move onto `SHELL`, which both versions
  honour, behind a new **`shell-guard`** target that fails loudly if they are ever inert again.
  Proved falsifiable in both directions: with a deliberately broken test the old Makefile exits
  0 and the new one exits 2.
- **The contention observatory refuses to measure when the toolchain says it will cache the
  result** (#2713). A re-taken A-vs-A noise floor returned nine ratios **byte-identical** to a
  run ten minutes earlier, *including its reported 102.86 s duration*, in a window that finished
  in under a second: Go's test cache had served the whole thing, and its only tell is
  `(cached)` replacing the elapsed time. Every in-process detector is circular — on a cache hit
  `cmd/go` does not execute the test binary at all — so the gate reads its own `argv` for
  `-test.testlogfile`, which `cmd/go` passes **iff** the result may be stored. Because Go stores
  only *passing* results, a gate that fails whenever the run is cacheable leaves no entry to
  replay: the set of runs that publish numbers and the set that are cacheable become disjoint.
  Fail-closed by design — an unregistered flag counts as cacheable. `make ci` is unaffected: the
  gate sits after the environment-precondition skip.
- **A ceiling arm is normalised by its own level-1 cell, and a ceiling whose direction is
  unknown is not published** (#2712). Three refusals are enforced in the instrument rather than
  documented: a ladder omitting level 1 is refused before any window runs, `normaliseByAnchor`
  refuses a pair with no level-1 cell, and `writeProbeSummary` refuses to publish a ceiling
  whose direction is unknown. All three are mutation-proved, and the outermost is covered
  **twice** — a mutant that neuters the *call site* while leaving the predicate intact keeps a
  predicate test green.
- **`bench/audit352` gained the measured budget override it never had** (#2670) and its
  profiling sweeps are soak-gated (#2667). The package measured 318.1 s in-suite against the
  240 s `HARD_BUDGET` in three consecutive runs of 321.5 / 328.7 / 318.1 s — not a regression
  but a ceiling that had never been exercised, since its ceiling was only ever inferred from a
  **standalone** figure of 180.6 s, which `docs/test-layers.md` itself warns is a lower bound.
  The rule the Makefile already documents was applied rather than a number fitted: worst
  in-suite figure × 1.25, rounded up to the whole minute → 420 s. Exactly one entry is added and
  the global `HARD_BUDGET` stays at 240.
- **`cypher/exec`'s `-race` cost falls 89.8 %** (#2672). `TestIndexBuffer_ConcurrentStress` was
  **97.7 % of the package's entire `-race` cost** — 173.57 s of 177.6 s — because 100 writers
  plus 1000 reader goroutines contended on one shared `sync.RWMutex`. ThreadSanitizer's
  per-sync-object bookkeeping grows superlinearly in the goroutines sharing an object:
  1000 readers on a shared index cost 82.22 s, the same 1000 readers each on **their own** index
  0.24 s, and 1000 readers touching nothing 0.00 s. The cost is **relocated, not removed** — the
  1024-reader variant moves behind `//go:build soak || nightly`, because 1024 is a published
  concurrency level under the extreme-concurrency mandate.
- **`bench/contention` joins `COVER_EXCLUDE`** (#2689). It is an env-gated measurement harness,
  not library code, and the module does not import it; it still **runs** in the suite and is
  dropped from the coverage figure only. `COVER_PKG_FLOOR_EXEMPT` was rejected because that
  pattern is for production code that is structurally uncoverable, which this is not.
- **Three `bench/audit352` instruments assert differences, not absolutes** (#2666), and the
  filed diagnosis was **wrong** and is corrected: the label-count test uses
  `testing.AllocsPerRun`, not the process-global `runtime.MemProfile` the report named, and the
  variable is the **race build**, which adds a per-scanned-row allocation term linear in *n*
  that an absolute model has no place to put. All three took the same corrective shape: scope
  the reading to its subject, and assert a difference measured in the same window.
- **Three stale count-plan pins were rebased and a wall-clock ratio relocated** (#2676, #2673).
  The pinned plan is the one #2657 deliberately changed and the new plan is strictly cheaper;
  the timing assertion moved behind `testlayers.RequireQuietMachine` and into `make test-timing`
  after being observed at ratio 1.632 and 1.629 **in opposite directions** on the same day —
  in both runs the allocation half was flat and identical at all five sizes, so the
  constant-time property the test defends actually holds and only its wall-clock half was
  broken.
- **The standalone `staticcheck` gate and its four orphaned `staticcheck.conf` files are
  retired** (#2680); `staticcheck`'s checks reach the module through `golangci-lint`, which runs
  its own version-matched copy. That `golangci-lint` honours none of the four was established by
  A/B with a firing control rather than assumed. One claim was deliberately **not** preserved
  because it is false: the `cypher/tck` conf justified its whole-package `-U1000` with
  "standalone staticcheck has no valid per-symbol U1000 suppression"; a three-arm probe refutes
  it.
- **The DST harness's own oracles were repaired before anything was concluded from them**:
  three oracles that collected evidence and then declined to judge it (#2745); a shared Bolt
  parameter fixture whose fixed label and id made sixteen concurrent probes fan out over each
  other's nodes, corrupting the oracle from level 2 upward with **2 003 spurious divergences at
  level 2 and 7 835 at level 8** (#2728); a contended-counter role that issued 192 transactions,
  committed **0**, and whose zero-lost-updates oracle then compared 0 against 0 and **passed**
  (#2729); a value-preserving `SET` the oracle modelled as a decided write, when the engine
  records no version for a write whose value equals the one already stored (#2717); an
  MVCC-sessions generator drawing `KNOWS` pairs the engine's parallel-edge guard already refuses
  (#2695); and a determinism test comparing a **sampled** live-connection count (#2671).

### Removed

- **The dead `cypher/exec.ChunkPool`** — `NewChunkPool` and the `Get` / `Put` methods (#2656).
  It had **no production call site anywhere in the repository**: its only references were its own
  definition and the two tests that exercised it — `TestChunkPoolRoundTrip` and
  `TestChunkPoolCopiesKinds`, removed with it — so the `cypher.pool.chunk.get` /
  `cypher.pool.chunk.put` counters it incremented **could never fire outside those two tests**.
  Its godoc ("Operators that process a high volume of batches should obtain chunks from a shared
  pool") described an intent no operator honoured: every columnar stage owns its chunk from its
  child's `NewOutputChunk` and reuses it across batches via `Chunk.Reset`. It had been exported
  and unwired since `v0.9.0`.
- **The uncalled `cypher/exec.RowSlab` arena** (#2700): `RowSlab` and its `Alloc`, `AllocRaw`,
  `SetRow`, `GetRow`, `Len`, `Cap` and `Reset`; `SlabPool` and its `Get` / `Put`; `NewRowSlab`,
  `NewSlabPool`, `DefaultSlabCapacity` and `ErrSlabOverflow`. `row.go` goes 192 → 43 lines and
  `exec_test.go` 411 → 230. It was deleted rather than wired, **deliberately**: its contract is
  that a caller must not retain a row beyond the slab's lifetime and `Reset()` nils every cell,
  while every site that would plausibly use it — `driver.go`, `eager.go`, `distinct.go`,
  `sort.go`, `top.go`, `hash_join.go` — retains its copy for the operator's lifetime. Wiring it
  there is a use-after-reset bug, not an optimisation. `docs/metrics.md` loses the
  `cypher.pool.slab.get` / `.put` row with it: those counters were emitted only from the deleted
  pool, so the table documented a metric nothing emits.

  Together these are **19 exported identifiers removed from `cypher/exec`**, a package
  [docs/semver.md](docs/semver.md) does list in the public-API surface. Pre-1.0 the minor digit
  absorbs it, exactly as it did for `v0.9.0`'s removal of `ParallelScan`. **No production
  behaviour changes and nothing measurable is reclaimed** — dead code costs nothing at runtime;
  it is removed because the bottleneck audit found it, not because anything gets faster.
- **Seven Cypher tests that cannot fail** (#2709). They share one shape: a body that never
  asserts, ending in an **unconditional `t.Skip`**, so no path through them could fail whatever
  the engine did, and they reported coverage that did not exist. Among the coverage lost was
  `CALL { } IN TRANSACTIONS` per-batch rollback, which is an ACID claim. The premise was
  re-validated at HEAD rather than taken from the audit: all seven take the **first** skip
  branch, because the parser rejects the syntax outright, so the features are genuinely absent
  and the five gaps go to the backlog where an honest record of an absent feature belongs.
  `TestCallSubquery_ExistsVsCall` is **kept**: it asserts and can fail. A repo-wide Go AST pass
  found exactly 7 before and 0 after. `bench/ldbc/ic{1,2,3}` and one temporal test converted
  `t.Skipf` on an engine error to `t.Fatalf`; all were probed first and all pass, so these were
  latent trapdoors rather than active cover-ups.
- **A dead `buildOpts` field and a false `//nolint:unused` justification** (#2680).
  `cypher/api.go` declared `disableIndexNestedLoopForTest` twice; the `Engine` field is live, the
  `buildOpts` field was never written and never read. Its directive claimed it was "written only
  by the in-package differential test" — that test writes the **`Engine`** field. So the
  directive did not merely suppress a warning, it **asserted something untrue** and hid genuine
  dead code from the `unused` linter.
- **`LPG_DEBUG_ABORT` debug debris** (#2694): two `os.Getenv` calls and a `Printf` **per edge
  removal** on a hot path, shipped in `df0d3d2f`.
- **The per-outer-row edge-type-filter LRU** (#2251), superseded by the dense
  relationship-type column. `EngineOptions.EdgeTypeFilterCacheCapacity` and
  `DefaultEdgeTypeFilterCacheCapacity` are **deprecated rather than deleted** so a caller
  compiling against the current release keeps compiling.
- **`Profiler`'s retained `p.wrapped` slice** (#2664) — write-only since it was introduced, and
  the site of a 200-warning data race.

### Fixed

#### ACID — Atomicity, Consistency, Isolation and Durability

Eight engine-side defects, all under Compliance Mandate 2, and three of them were diagnosed in
[`docs/mvcc-life-record-defects-2026-09-03.md`](docs/mvcc-life-record-defects-2026-09-03.md)
(#2723) before any of them was worked. **Both DST arms are clean at the end of the sequence.**

- **A rolled-back `DELETE` destroyed a node's committed birth** (#2724, Isolation and
  Consistency). See *Breaking changes*. A dead end is recorded so it is not re-attempted blind:
  extending the fix with the displaced birth's instant, to also serve readers pinned *before* a
  node's creation, **reintroduced the defect** on crash seed 932, 10/10 deterministic.
- **A refused edge removal resurrected the arc on rollback** (#2725, Atomicity and Consistency).
  Two rolled-back transactions left a permanent arc that no committed transaction ever created.
  **The diagnosis was half wrong, and the wrong half was the defect:** the removal is refused
  *first* and mutates nothing — the writer's arc is physically untouched through the deleter's
  whole statement — so the leak is entirely an undo entry recorded for a removal that never
  happened. A fourth site was uncovered by no test at all: the `handle==0` fallback of
  `removeEdgeByHandleInfo` returned its pre-probe rather than its outcome, feeding a caller that
  has gated on that return since #2018. **The symmetric question is answered, and the answer is
  no:** a refused property, label or node retirement strands nothing, because those stores keep
  per-object delta chains so an aborted inverse stays attributable and is withdrawn, while an
  adjacency entry is an immutable snapshot that a later transaction physically embeds and
  republishes. Rate over 1000 seeds at `Ticks=1200`: **2 unclean → 0** on the no-crash arm.
- **A refused node removal resurrected the node on rollback** (#2726, Consistency). An unnamed
  node the workload never creates survived in the engine. **Both recorded hypotheses were
  refuted:** the diagnosis blamed `reclaimAbortedLife` and suspected a root shared with the
  first family, which would have meant a record-layout change; a white-box trace of seed 790
  shows the reclaimer never touches the leaked node. A second symptom the matrix caught: the
  bogus revive **consumes the tombstone**, so a legitimate inverse running afterwards finds
  nothing to revive and silently no-ops — one arm lost the node entirely. Seed 790 goes
  **12/12 unclean → 0/12**; the 1000-seed crash sweep goes **1 → 0**; the Cypher matrix goes
  3 red → 8/8.
- **`DETACH DELETE` wiped a concurrent transaction's arc** (#2694, Atomicity). With a second
  transaction holding an uncommitted `DETACH DELETE` on the same node, a **rolled-back** `CREATE`
  of an edge left its arc behind; seed 29 reported `oracle=3 engine=4` deterministically. Two
  omissions on the bulk removal path, each load-bearing: `removeAllEdgesFromInfo` took no
  adjacency write-write claim while every per-edge path takes one, and `adjlist` publishes a
  **nil entry** there — it wipes the slot rather than removing the arcs the transaction can see;
  and the Cypher adapters journalled undo inverses unconditionally and *before* the removal,
  gated on a raw `HasEdge`. With only the first half fixed, every Cypher answer is correct in all
  eight commit/rollback orderings while the raw adjacency keeps a phantom arc that `search/`
  would walk — a query-only oracle passes that, so the tests assert Cypher content, raw adjacency
  and `Size()` together.
- **Present-time readers observed deleted nodes** (#2687, Consistency). `removeNodeInfo` flipped
  the tombstone **before** stripping the label bitmaps, leaving a window where a node was dead,
  still indexed, and in no suspect source — so nothing could correct it. Reachable from the plain
  Go `RemoveNode` API and not only through Cypher. Measured at pristine `a34425a6`:
  **5 991 / 4 966 / 5 108 / 5 271 violations** over four runs of 400 rounds, far above the 9–15
  originally reported. The naive repair — strip before flip — trades one window for the other,
  measured at **154 live rows lost in 8 338 reads**. What closes both is registering the
  *deferred* removals before the flip. **Three further pre-existing defects the probe found, all
  fixed:** `RestoreTombstones` was not a race but **permanent** on a drained substrate;
  `SetNodeLabel` on an already-tombstoned node indexed it permanently once the delta was
  reclaimed — which is the **recovery** shape, since recovery applies snapshot tombstones before
  snapshot labels, so a reopened store served deleted nodes from `MATCH (n:L)`; and the authority
  itself lied, because every flip did `tombstones.Store(next)` then `tombstoneActive.Add(1)`
  while `IsTombstoned` short-circuits on a zero counter — **19 disagreements in 2 938 samples**.
- **A read transaction observed two different label counts** (#2688, Isolation and Atomicity).
  `LabelCountExact` and `LabelsCountExact` sampled the staleness gate **before** reading the
  value and never re-checked, so a gate reading that predates a write, paired with a count that
  follows it, is a present-time number reported as exact for a snapshot that predates it. It is
  an **Atomicity** break as well: `label.Index.Count` takes its own `RLock`, so it lands *between*
  the five `Add` calls of one transaction — the observed values 481, 526 and 266 are not multiples
  of the 5 nodes each transaction commits. The decisive datum is a **decrease**, 266 → 265, on a
  monotone create-only workload: no consistent reader can produce that and neither can
  read-committed.
- **A `CREATE INDEX` could register half a pair** (#2703, Consistency). `index.Manager.CreateIndex`
  takes `mu` exclusively **once per registration**, so two registrations are two acquisitions with
  a window between them, while a write transaction's index fan-out runs under a *shared* hold of
  the visibility gate. Split in two, a batch lands in the gap, reaches the user btree, and is
  missed **permanently** by the companion — whose backfill snapshot predates it and which
  self-maintains only from changes delivered after registration. A later numeric seek then returns
  an incomplete result **for a committed write**. Prior art was read and one half deliberately
  **not** transferred: Neo4j's constraint-plus-backing-index pair is deliberately non-atomic, but
  its pair is (enforcement, acceleration) where a half-state weakens enforcement but never returns
  a wrong row, while this one is (correctness, correctness).
- **A committed edge property was dropped during WAL replay, and a containment boundary leaked a
  store-wide wedge** (#2707, Durability and Consistency). Two adjacent cases in the **same**
  switch: `OpSetNodeProperty` fail-stops with a metric, `OpSetEdgeProperty` did
  `_ = g.SetEdgeProperty(...)` behind a `//nolint:errcheck` justified as "no schema validator
  during WAL replay" — so a committed edge property was silently lost on recovery with no error,
  no metric, and replay reporting success. The suppression's premise does not hold:
  `ReplayWAL` takes a **caller-supplied** graph and `Graph.SetValidator` is public, so a
  caller-supplied validator is the whole error surface. Separately, `recoverFinishPanic` did not
  roll the WAL transaction back while its sibling `recoverExecPanic` does and its own call site's
  comment claimed it did, so `Store.inflight` leaked by one and `drainInflight` — an unconditional,
  **uncancellable** wait — wedged `RunUnderCommitLock` permanently: shutdown hangs for ever and the
  WAL grows unbounded.

#### Correctness

- **`PROFILE` rendered a different plan from the one that ran, and reported `rows=0`** (#2665).
  See *Behaviour a caller can observe*.
- **A correlated `EXISTS` after a write clause lost its correlation** (#2659), and
  **a `SET` on a write-clause-bound relationship vanished** (#2705). See the same section.
- **Reverse and undirected `Expand` under a correlated `Apply` returned rows for nodes with no
  matching edge** (#2251), uncovered by removing the cost that was accidentally masking it.
- **A data race on `Profiler.Wrap`** (#2664). It appended every wrapper it built to a retained
  slice with **no synchronisation**, while `ParallelScanProject.runMorsel` calls the operator
  factory on the **worker** goroutine once per morsel: `go test -race ./bench/audit352/` reported
  **200 `WARNING: DATA RACE`** and exit 1. The measurements it raced over were unreachable —
  `PlanTree` descends only through `PlanChildren` and no morsel-parallel leaf implements it — and
  the field was **write-only**, one selector in the whole module. `buildOpts.forWorker` now clears
  the profiler, which makes the documented "the parallel tier is measured as one node" contract
  true rather than merely intended; the root cause was structural and `forSubquery`'s godoc had
  already named it: **a copy-then-clear fails open**, so every `buildOpts` field added since was a
  candidate for exactly this defect.
- **`IntegerValue.Equal` closed the lazy-entity equality triangle** (#2388).
  `lazyRel.Equal(IntegerValue(id))` was true and the reverse false, on two values the hash
  deliberately co-locates. Unreachable today only because the escaping-value gate forbids a lazy
  entity in a `DISTINCT` comparison — and the gate should not also be what makes equality correct.
- **The `Top` tie-ordering defect the property test found at HEAD** (#2509). See *Behaviour a
  caller can observe*.
- **A per-worker pre-projection had no row byte budget** (#2668) — the third instance of the gap
  #2655 closed for the other two. `EagerAggregation`'s own `WithByteBudget` does not cover it: it
  charges only retained group keys, once per new group, and **never an aggregate argument column**.
- **`BuildFixture` panicked on a degenerate spec, and produced a silently wrong graph above
  `MaxUint32`** (#2744). A premise in the existing code was also wrong: `fixture.go` claimed
  `V == 1<<32` reaches "the same panic" — it does not, because interning measures ~270 B per node,
  so 2³² needs roughly **1.1 TB** and exhausts memory long before the modulo.
- **Two zero-value pools and a wire-facing switch panicked** (#2708), found by the newly-enabled
  linters and **fixed rather than suppressed**: `bolt/packstream`'s exported `EncodePool` /
  `DecodePool` set `sync.Pool.New` only in their constructor, so a zero-value pool returned nil
  and panicked on the type assertion; and `bolt/proto/messages.go` switched over a `Pull|Discard`
  union with no `default`, so a third member would compile and panic on the wire-facing decode
  path. The 14 unchecked assertions in `graph/index/btree/bplus.go` were **proven safe** instead,
  by enumeration over heights 1 to 7 across five descent loops, 140 evaluations, zero violations.
- **A `make ci` red that was a legitimate serialization conflict, not an engine defect** (#2689),
  settled by construction rather than by argument: under the cover-gate's own conditions
  **200 of 200** sampled blocking versions were the worker's **own** previous commit, with
  `ConcurrentWriter()` false 0/200 — the spurious self-conflict already documented in
  `graph/mvcc/await.go`. Frontier lag reached **2 545 commits**. A bounded retry was measured and
  rejected: 10 of 16 workers made no progress in 64 attempts.
- **Two sprint-close gates that measured the host rather than the module** (#2751). A read-path
  allocation ceiling read 32.0 objects against a ceiling of 20 under a full run at loadavg 13, and
  passed 30 of 30 in isolation: `testing.AllocsPerRun` reads `runtime.MemStats.Mallocs`, which is
  **process-global**. Contamination can only *add* allocations, so the minimum across samples is
  the robust estimator; the gate now takes the minimum of five and the true reading is exactly
  20.00. And an MVCC arm asserted a row **order** on a query carrying no `ORDER BY`, which
  openCypher does not specify: the assertion now sorts, which is deliberately not the same as
  dropping it — the property is the **multiset**, so a missing row still shortens the result and a
  duplicate still lengthens it. The sprint-353 merge commit records the allocation gate as
  a **known red** with the cause not established, tracked as #2753, and this commit — the tip of the
  branch that merge brings in — establishes it; the two records disagree and both are in the tree.
- **`make release-accuracy` could not run at all on macOS.** Its README check echoed an unbraced
  `$VERSION` immediately followed by an ellipsis character, with no delimiter between them. Under
  bash 3.2 in a UTF-8 locale `isalnum()` accepts the lead byte of a multi-byte character, so the
  shell read the variable name as `VERSION` plus that byte, found it unset, and `set -u` aborted the
  target before it reached its later checks. **Latent, and exposed by this release's own #2672**,
  which moved `-e -u -o pipefail` onto `SHELL` so that they finally applied — a fourth defect
  uncovered by making the gate able to fail, after the three that commit's own record names. The
  mechanism was reproduced in isolation, with a braced control that passes, rather than inferred
  from the error message; the fix is to brace the reference. A scan of the whole Makefile found
  **exactly one** such site.

### Performance

#### Release-level delta, `v0.12.0` → `v0.13.0`

**Measured first-hand at both trees**, not carried forward: a `git worktree` at the `v0.12.0`
tag and this release candidate, each compiled once, run **interleaved** one repetition at a
time with the leading arm alternating, `-race` off, `-benchmem` on, `-count=6`, on a host
gated below a 1-minute load average of 2.5. `go.mod` and `go.sum` are byte-identical between
the arms, so **no toolchain or dependency change is mixed into any comparison** — the first
release since `v0.11.0` for which that holds. The published `v0.12.0` figures were taken on
go1.27.0 and were **not** used as a baseline; the tag was re-measured on go1.27.1.

**The noise floor was measured before anything was attributed.** Both arms pointed at the same
tree: **14 comparisons, 0 significant**, largest median drift **0.87 % on sec/op** and
**0.00 % on allocations**. Every figure below clears it by an order of magnitude.

| Block | geomean | Largest single move |
|---|---:|---|
| Cypher count pushdown | **−87.65 %** | `Count_LabelStarBig_Serial` **−99.97 %** (≈3 ms → 880 ns), #2654 |
| Read transaction under concurrency (1→1024) | **−73.04 %** | `ReadTx_LockFree` **−91.40 %** at 1024 |
| Contended metric emission (1→1024) | **−92.79 %** | `IncCounterParallel` **−96.47 %** at 1024, #2698 |
| `cypher/exec` read-path operators | **−18.93 %** | `Scan_PerNode` −69.97 %; `Sort_10k` −43.59 % |
| Columnar query shapes | **−4.28 %** | five hop shapes −7.69 % to −9.42 % |
| `search` headline set (**control**) | +0.83 % | within noise, as a control must be |

All at p=0.002, n=6, except the control. Contended metric emission changes **sign** rather
than degree: `v0.12.0` anti-scaled, cost rising 5.8× from 1 to 1024 goroutines; `v0.13.0`
scales, cost falling 5.3×. The read-transaction ladder's spread widens to **±32–44 %** at
1024, so its median there is indicative rather than precise.

**Allocations moved as much as time did — but on only two of the four blocks, and that split is
itself the finding.** On the read-transaction ladder `ReadTx_LockFree` falls **249 → 37
allocs/op (−85.14 %)** and `ReadTx_WriterLock` **306 → 114 (−62.75 %)**: geomean **−76.29 %
allocs/op**, **−86.94 % B/op**. On the count block, geomean **−90.12 % allocs/op**, with
`Count_LabelStarBig_Serial` going **199 796 → 19**. The head arm's counts are **constant across
the whole 1 → 1024 ladder** — 37 and 114 at every level — where `v0.12.0`'s drift with
concurrency; that is a structural change, not a tuning one. By contrast the `cypher/exec`
operator block and the columnar shapes are **flat** (geomean −0.12 % and −0.19 %), so *their*
time wins are CPU-side. The allocation work in this window targeted the plan-cache-hit engine
path, and these are the two blocks where it shows; reading either half as the whole would be
wrong. Every figure here is `-race` **off** — `allocs/op` is not build-invariant.

**Four regressions were measured, and none is omitted.**

- **`graph/index/btree` `Index_LookupHot` is +26.93 %** (geomean, every GOMAXPROCS level,
  p=0.002). The commit responsible reports its own uncontended cost as **6.66 %**; on an
  ordinary lookup fixture it is roughly four times that. Different fixtures, so this is not one
  measurement contradicting itself — but the uncontended cost is fixture-dependent and larger
  than the commit's figure suggests. **No cause is attributed**, because none was measured.
- **`cypher/exec` `Drain_Throughput` +12.87 %.**
- **`IncCounterParallel` +8.32 % at a single goroutine** — the price of its contended win.
- **`CountAllNodes` +3.09 %**, and `search.Yen_K100` +2.42 %.

**Three things this delta does not establish.** Sprint 353's headline index contention claims —
**34.6×** on the btree spine (#2683), **1.91×** on the hash index (#2692) — **cannot be verified
at release level**: no benchmark common to both trees exercises those indexes concurrently,
because the instrument that measures them (`bench/contention`) is new in `v0.13.0`. They rest on
each commit's own local A/B and are reported as such. And storage footprint, latency percentiles
at the published concurrency levels, `bench/mtaudit`'s engine-under-writer ladder,
`BenchmarkWriteScaling_Cypher`, and every non-`darwin/arm64` host remain unmeasured.

Full method, per-block tables and the raw `benchstat` output are in
[docs/benchmarks/v0.13.0.md](docs/benchmarks/v0.13.0.md) and
[docs/benchmarks/v0.13.0-raw/](docs/benchmarks/v0.13.0-raw/).

#### Per-change figures


Every figure below is **that commit's own interleaved A/B on the shape it names**, with a noise
floor measured first and the build mode stated where it matters. None of them is a release-level
claim, and none may be read as one: they are single-workload deltas on a 10-core Apple M4
(**4 performance + 6 efficiency cores**, which #2691 measured and which no `runtime.NumCPU`
reports), and several of the arms they move are laboratory ceilings rather than user-facing
queries.

#### Concurrency — the index and `graph/lpg` surfaces

| Change | Its own measurement |
|---|---|
| **`graph/index/btree`: immutable spine plus per-entry locks** (#2683) | throughput at 1024 goroutines **3 788 293 → 131 225 618 ops/s (34.64×)**; scaling ratio 0.153 → 5.675; mutex delay **1.99 hours → 120.17 s**. Level 1 **regresses 6.66 %** (40.4 → 43.2 ns/op), ~3× its own noise floor and therefore real |
| **`graph/index/btree`: slab-allocated entries** (#2684) | GC mark CPU **290.24 → 167.26 ms (−42.4 %)** at 10 M keys, GC wall −33.4 %, objects/key 1.048 → 0.298, B/key unchanged to 4 dp. Same-vs-same floor +0.08 %, so the effect is ~240× the floor |
| **`graph/index/hash`: per-key locks and lock-free inline reads** (#2692) | **1.906×** at 1024 (49.37 M → 94.11 M ops/s); scaling never below 1.0; absolute mutex delay 1 142.43 → 497.72 s |
| **`graph/index/hash`: open-addressing table replacing the shard map and its RWMutex** (#2699) | `index-hash-rw` scaling at 8 **1.898× → 4.813×**; ceiling probe 1.960× → 0.558×; blocked at 1024, median of 8, **300.02 → 129.17 s**; `Insert` flat CPU 97.91 % → 2.08 % |
| **`graph/index/count`: stop the write path anti-scaling on a hot shard** (#2682) | `index-count-spread` **+201 %** at 8 and **+468 %** at 1024; `index-count-hot` +24 % and +235 %; p99 at 1024 **1 001.2 µs → 0.2 µs** spread, **2 121.8 µs → 1.2 µs** hot |
| **`graph/index/count`: stripe the shard lock, not the counter** (#2696) | **2.02×** at 8 (79 915 356 → 161 629 583 ops/s); gap to perfect partitioning 2.941× → 1.410×. Costs, not buried: uncontended write **2× slower** (5.66 → 11.69 ns), level-1 throughput 0.839×, `Cells()` **+148 %**, per-`Store` memory **4.15×** |
| **`graph/index/label`: per-entry locks over an in-place spine** (#2685) | `cypher-mixed-rw` **1.430× / 1.479× / 1.555× / 1.455×** at 8/64/256/1024; label share of mixed-workload mutex delay **98.67 % → 12.15 %** at 8. Costs: `Count` +2.0 ns, `Has` +1.6 ns, contended parallel `Count` +25 ns, **+110 B per distinct label** |
| **`graph/lpg`: gate the suspect walk on per-label churn** (#2686) | `cypher-mixed-rw` **41.13×** at 1024 (9 359 → 384 960 ops/s), 17.45× at 256, 6.82× at 64, 3.70× at 8; curve 1.616/0.830/0.284/0.101 → 1.197/1.132/0.990/0.829 |
| **`graph/lpg`: lift the label-index add out of the shard-lock convoy** (#2681) | `cypher-write-mem` **+17.59 %** at 1024, `cypher-mixed-rw` **+24.89 %** at 256, `mvcc-session-write` **+27.22 %** at 1024; p50 at 1024 **162.4 µs → 10.6 µs**. Levels 8 and 64 are **neutral, not improved** — the sign flips between rounds |
| **`graph/lpg`: settle a property delete's three non-mutating outcomes under the shared lock** (#2710) | `delNodePropertyInfo` mutex delay **153.03 → 7.03 s (−95.4 %)** on non-overlapping spreads; both sites combined −64.5 %. **Throughput did not move**, and that is reported as a failure of the criterion rather than dressed up: +9.2 % at 64 against a re-measured A-vs-A floor of ±32.8 % at n=8, t=1.98, p≈0.07 |
| **`internal/metrics`: stripe a contended series onto per-core lines** (#2698) | `IncCounterParallel-8` **47.565 → 1.701 ns/op (−96.42 %)**, `-64` 50.235 → 1.762 ns/op; `metrics-emit` at 8 0.445× → 2.740–5.251× over three sweeps. Paid honestly: uncontended `IncCounter` **+13.31 %**, `ObserveLatency` +2.4–2.8 %, **+3 920 B** unconditional at full cardinality and 4 096 B per promoted series |
| **`bolt/server`: take the transaction registry off the per-message path** (#2714) | 1 → 8 goroutines **9.40× worse becomes 6.10× better** (8.242 → 77.510 ns against 4.582 → 0.751 ns), 1.80× faster even at one goroutine, 103× at eight, zero allocs; the contention-free ladder is 5.76×, so this is at the hardware's ceiling. **The counter-case is reported, not buried:** with a brand-new statement text every message the two-slot reuse never hits and cpu=1 costs 19.2 ns against HEAD's 8.6 |

#### Cypher read path

| Change | Its own measurement |
|---|---|
| **Carry the adjacency caches into subquery builds** (#2646) | complexity exponent **1.927 → 1.001**; at n=4000 sec/op **−99.91 %**, B/op −99.86 %, allocs/op −99.23 %; per output row 928 181 B → 1 290 B. Proven structurally, not by the clock: uncached CSR-pair builds per query go from exactly *n* to **0** |
| **Un-gate the labelled count pushdown** (#2654) | **Θ(n) → Θ(1)** in time, allocations and bytes: serial arm ns = 586.6 + 26.5631·n (R²=0.999963), pushdown arm flat at 1 420 ns / 2 168 B / 29 allocs across a 100× range. At n=50 000: **929× time, 220× bytes, 1 718× allocations** |
| **Materialise `ORDER BY` keys once per row** (#2652) | `Sort` **9 004 004 → 1 624 657 objects (5.54×)**, 1.389 GB → 151.5 MB; `Top` **17 073 832 → 1 984 643 (8.60×)**, 2.891 GB → 337.5 MB. Complexity proved by a counter rather than by timing: n=1000 against n=4000 gives exactly **4.0000** decorated where the legacy arm gives 4.9101. One honest negative: at n=2 the fix **loses** (103 objects against 105), reaching parity by n=8 |
| **Batch the result-budget counters into worker-local tallies** (#2649) | **−29.28 %** (p=0.000, n=10) on the capped arm, and the ceiling is reached — the residual gap to a counter-free arm is p=0.165. CPU, not merely wall clock: user+sys over 200 fixed iterations **32 s → 22 s (−31.25 %)** |
| **Hoist the row-context schema walk out of the per-row path** (#2645) | geomean **−9.07 % sec/op, −22.88 % allocs/op** over four shapes (best −15.77 %), null control unmoved on both axes |
| **Pass the evaluation state as a parameter, not through the row map** (#2653) | geomean **−4.92 %**, best −8.09 % (p=0.000); on the binding-free path `BenchmarkEvalWithBindingFree` **53.62 → 18.83 ns (−64.89 %)**. Allocations unchanged — this is a CPU win, not an allocation win. The noise floor is **not** the 0.51 % the task assumed: two builds of identical source differ by a significant **+1.41 %** from code layout alone |
| **Fuse projection items onto one row context per row** (#2658) | 2 properties **−28.41 %** sec/op, 3 properties **−40.72 %**, 1 property flat (p=0.393, the null control); contexts built 16 → 8 over 8 rows, 24 → 8 for three columns |
| **Answer `r.k` from a lazy relationship value** (#2388) | in `examples/26_social_scale_bench` at 20 000 users: three treatment queries **−4.68 %, −4.71 %, −4.19 %** with disjoint ranges against a ±0.9 % floor, six null controls inside the floor. Total `alloc_space` **23.28 → 20.63 GB (−11.0 %)**; `buildEdgeProps` leaves the profile entirely. The arena is **refuted, not omitted**: −5.44 % without it against −5.51 % with it, inside the floor, while costing a reproducible +1.7 % on a query that binds no relationship |
| **Cut 12 of 33 read-path allocations** (#2693) | `cypher-read-label-small` **1.232×** at 4 goroutines (p=0.0022) and 1.231× at 6; p99 **31.7 → 13.4 µs** at 4; allocs/op **33 → 21**. `cypher-mixed-rw` at 4 is reported as **not separable from noise** (+6.6 % inside a 26.0 % base spread). Levels 8 and 64 are **unverified, not measured** — the knee is at 3–4 on this host's performance-core count |
| **Slot-aligned relationship-type column** (#2251) | reverse/undirected typed traversal geomean **−53.28 %** sec/op, every row p=0.002, allocs/op exactly unchanged; forward typed one-hop over 960 008 arcs −38.93 % time, −27.25 % B/op; cyclic geomean −9.85 %. **Two of the task's headline numbers are corrected downward rather than quoted:** the "~60×" lookup figure measures **7.35×** in place and is a primitive ratio; the 4.54×–6.29× counterfactual in `docs/design-wcoj-cyclic-patterns.md` §7.1 is **not reproduced** and should not be cited as if it were. Memory: the column is exactly 8 B/arc against the retired map's 34.95 B per accepted entry, and there **is** a losing region — break-even is 22.9 % arc coverage for one type set |
| **Let an aggregate keep the columnar chain a `WHERE` used to cost it** (#2655) | `WHERE n.age>x RETURN count(n)` **199.04 → 74.16 ns/node (−62.7 %)**; allocations 49 821 → 90 per op, 10.4 MB → 347 KB. The aggregating form is now **cheaper** than the form shipping 24 999 rows (0.67×) where it had been 1.83× dearer. The three parts are **jointly necessary and separately worthless**: with chunk input disabled the filter change alone measures inside HEAD's noise |
| **Fuse `ORDER BY SKIP LIMIT` into `Skip` over `Top`** (#2509) | `skip0_limit10` **−52.02 %** time, `skip100_limit10` −51.40 %, `skip10000_limit10` −34.25 %, geomean **−24.02 %** time and −7.17 % bytes. Operator level at n == M: **56.82 → 8.87 ms** against `Sort`'s 8.77, i.e. 1.01× where it had been **6.45×** — and "Top at n close to M costs ~3× a plain Sort" was **understated**, it measured 6.45×. One vector is still up and stated plainly: at n == M/2 the operator allocates 25.73 MiB against `Sort`'s 22.06 MiB |
| **Let block-form `COUNT` reach the adjacency rewrites** (#2648) | `COUNT { MATCH (a)-[:K]->(:Q) }` **2.39×** at 250 rows and 2.69× at 2500; `WHERE COUNT { … } > 0` **2.95×** and **3.13×**; allocations fall 4.15× to 5.19×. Constant factor, not complexity — per-row cost is flat across a 10× row change in **both** arms; null control 14 arms, worst +1.52 % |
| **Normalise `count(var)` to `count(*)`** (#2657) | `count(r)` over an `Expand` **−16.71 %** and `count(a)` **−17.94 %**, both p=0.000, reproduced independently at −17.27 % and −17.30 %; null control p=0.912. Disclosed cost: **+2 allocs and 192 B per query build**, time-neutral, structurally required by the copy |
| **Keep the Bolt chunk-length header off the heap** (#2716) | 1 chunk **2 allocs / 514 B → 1 alloc / 512 B (−50 % objects)**, 4 chunks 4 → 3 allocs (−25 %), byte-identical across 75 size × chunk-split combinations. **Both of the task's requests are refuted:** `append(msg, make([]byte, n)...)` does not allocate the temporary — the compiler rewrites the idiom to `growslice` + `memclr` — measured identical in every shape, zero objects and zero bytes saved; and buffer reuse would silently defeat the `InboundBudget`'s CWE-770 aggregate bound, keeping up to 16 MiB × connections permanently pinned and invisible to the accounting that exists to bound exactly that |

#### Measurement methodology — corrections to published figures

- **The Bolt scaling column understates a real socket, and the pipe is not the limiter** (#2711).
  Byte-identical Cypher over both transports scales **1.34× to 2.03× better on a socket** than on
  the in-memory pipe — 6× to 57× the measured floor. **Confirmed** for the scaling column;
  **refuted** for contention, which was the actual hypothesis: the socket reaches only
  0.31/0.46/0.58/0.59/0.58 of the pipe's throughput, the pipe wins 13 of 14 cells, and —
  decisively — removing the pipe's own per-message cost makes its scaling ratio **worse**
  (1.306 → 1.150), so that overhead was *flattering* the published number. One cell is real and is
  attributed by experiment rather than by profile: `rows@1024` at +3.8 %, caused by
  `halfPipe.waitDeadline` arming a timer, allocating a channel and spawning **a goroutine per
  blocking wait**, sized at 1.03–1.24 µs/message.
- **Ceiling arms are normalised by their own level-1 cell** (#2712), and the published direction
  was wrong for three arms and unknowable for five more. Corrected: `dst-concurrent-bolt@1024`
  16.307 → **15.127 and an upper bound**; `metrics-emit@8` 3.300 → **5.263, understated by 59 %**;
  `index-btree-rw@8` **0.948 → 0.998** — a reading published as an arm scaling *worse than its own
  base* was an artefact of the instrument. Three stated causes were refuted, including the one in
  the task text; five arms establish nothing at all because their level-1 cell sits inside their
  own spread, and the document says so rather than publishing a falsely precise number.
- **The Bolt surface does not throttle the engine** — the sprint-353 laboratory evaluation of
  `bolt/proto`, `bolt/packstream` and `bolt/server`, recorded in
  [`docs/bolt-evaluation-2026-09-03.md`](docs/bolt-evaluation-2026-09-03.md). At 64 goroutines
  through the wire, **92.2 % of all mutex delay is `graph/lpg` property-info shard locks** and
  under **0.2 %** is anything in `bolt/server`. Four hypotheses were refuted, each by a design
  built so it could refute: per-record `SetWriteDeadline` costs nothing (1.012 / 0.999 where a real
  cost must show ~34× the delta), the `txQuota` global mutex does not throttle the wire (1.004),
  the metrics backend does not contaminate Bolt numbers (1.458 against 1.459), and rejection
  logging does not confound the 1024 cell. Both Bolt global mutexes **are** expensive in isolation
  (quota 5.7–10.4×, `RegistryUpdate` 8.9× worse from 1 to 8) but at 37–215 ns are ~2.6 % of a
  27.5 µs transaction, which is why the wire never sees them.
- **The ladder could not see the knee, and that cost a whole task** (#2690, #2691).
  `cypher-read-label-small` peaks at **3–4 goroutines** and then decays: 1.00 / 1.44 / 1.67 / 1.65
  / 1.54 / 1.46 / 1.50 across 1/2/3/4/6/8/64. The mandated ladder jumps 1 → 8, so it samples only
  the falling side, which reads as a **flat** curve and invites exactly one conclusion — that some
  lock is pinning throughput. A task was opened on that reading and refuted by probe: deleting the
  suspected lock bought **0 %**.
- **`B-10` is a memory item, not a GC item** (#2650). The 29.53 ms per forced GC at 800k nodes was
  measured as cost **per cycle**, and nobody had measured cycles per second. Measured over 45-second
  windows: idle 0 cycles, realistic query load **4 cycles / 0.17 % of all CPU**, and with
  `GOMEMLIMIT=700MiB` 41 cycles / 1.70 %. Stop-the-world total was **0.2 ms across the whole 45 s**.
  There is a structural reason it stays small: under `GOGC` pacing, cycle rate is
  `alloc_rate/(GOGC/100 × live_heap)` while mark cost per cycle is proportional to `live_heap`, so
  **live heap cancels**. The verdict is **do not pursue the per-node heap shape for CPU.**

### Security

- **The mandated vulnerability scan had never been automated** (#2722). The `v0.12.0` incident this
  task was opened for — a `govulncheck` binary that "silently exited 0 with no analysis" — was a
  **manual step**, so nothing was watching it: a gate nobody invokes cannot fail loudly, it simply
  never runs. `make vulncheck` now exists and **asserts analysis rather than exit status**. It reads
  `SBOM.roots` from `-format json` — the list of root packages actually loaded — because `config` is
  emitted *before* any loading and therefore proves nothing: the stale binary produces 289 bytes
  containing only `config`. Three assertions layer on top, each closing a way a scan could silently
  shrink: **scope**, that every package `go list ./...` reports appears in `roots` (measured
  **136/136**, so a narrowed pattern fails rather than certifying from a fraction); **depth**, that
  `scan_mode=source` and `scan_level=symbol`, so the weaker `-scan=module` cannot pass as full
  reachability analysis; and **findings**, read out of the JSON rather than inferred. The exit status
  is printed as "recorded, NOT trusted". The gate **never skips** — a skipped security gate is the
  failure mode being removed — and binary resolution deliberately ignores `PATH` order, preferring a
  build matching the running `go` and installing a pinned one when none is usable. Proven to fail by
  **eleven deliberate reproductions**, each rejected for its own named reason, including the real
  stale `v1.3.0` binary still shadowing `PATH`. The first battery went 8/8 green while the gate was
  actually dying in `set -e` inside an inspection helper and never reaching the assertion at all,
  which is why every negative case now asserts its **reason substring** rather than merely a
  non-zero exit. The first real full-module scan since the toolchain moved reports **136 packages,
  11 modules, source and symbol level, zero vulnerabilities**, recorded with tool version and date in
  [`docs/security-vulnerability-scans.md`](docs/security-vulnerability-scans.md).
  `CONTRIBUTING.md` is corrected in the same commit: it prescribed `-scan=module` "until an upstream
  release built for the pinned Go minor is available", and `v1.7.0` **is** that release, so the
  guidance steered readers to a weaker scan than the gate now performs.
- **A live blindfold in the lint path exclusions** (#2746). The path regexes were **unanchored
  substring matches**, so `ds/` also matched `examples/20_concurrent_reaDS/`, `bench/` matched
  `examples/26_social_scale_BENCH/`, `cypher/` matched `examples/22_CYPHER/` and `graph/` matched
  `examples/02_property_GRAPH/`. **Seven G115 integer-conversion findings in `examples/` were being
  suppressed** by rules the configuration documents as scoped to library subtrees, and `examples/`
  has no G115 exclusion of its own. Every path is now anchored with `^`; the suffix patterns stay
  unanchored, which is correct. All seven were inspected individually and are bounded conversions
  now carrying their own directives. No code defect surfaced — all 92 findings across the change are
  test or harness code with a locally constructed path, a fixed seed, or a provably bounded
  conversion, **each verified rather than assumed**, including the WAL symlink-escape security test,
  which reads back the victim file it created itself.
- **A `G115` path exclusion was hiding a live, now-reproduced ACID breach** (#2708). Removing it
  from `store/` surfaced five `uint16(len(...))` WAL length prefixes that no guard bounded, while
  their siblings the constraint and index encoders **are** bounded by the guard #1903 added
  precisely so an embedded Go API caller "can never silently truncate a label and corrupt the WAL".
  It reached **2 of 7 encoders**. That is the defect fixed as #2742. Sixty-eight per-site G115
  directives replace the `store/` and `bolt/server` path exclusions, each stating a **checkable**
  claim naming its guard and line, never "safe".
- **Two exported zero-value pools and a wire-facing decode switch could be made to panic** (#2708).
  See *Fixed — Correctness*. The `bolt/proto` case is on the wire-facing decode path, so a third
  union member added later would have compiled and panicked there.
- **Reader caps were not raised to meet the writer** (#2743), deliberately, and the reasoning is
  recorded because the opposite direction is worse rather than merely different: the reader caps
  **are** the anti-OOM control — the code says so, naming a hostile `stringCount` and a ~16 GiB
  allocation — so aligning on the writer's 4 GiB would mean `make([]byte, n)` up to 4 GiB on an
  untrusted file, the memory-DoS class this project already carries a recorded HIGH finding for.
  That would trade a durability bug for a security regression.
- **The Bolt latency histogram cannot be minted from the wire** (#2715). The label is a name suffix
  drawn from a closed set of thirteen, built once at `init`; `proto.DecodeRequest` rejects an unknown
  struct tag and `serve.go` answers `Neo.ClientError.Request.Invalid` without dispatching, so a
  hostile peer cannot create an unbounded series. Proven rather than asserted, with a bijection test,
  a typed-nil case, a check that all thirteen survive the backend's `sanitize` as thirteen **distinct**
  series, and a malformed frame over a live socket that mints no series and leaves the connection
  usable.

Left for the user and stated rather than implied: a stale `govulncheck` v1.3.0 binary still shadows
`PATH` on the release workstation. The gate routes around it and names it in every run's log; deleting
it is the user's call, and it doubles as the best available no-analysis reproduction.

### Compliance

- **100 % openCypher TCK-compliant at the execution level, preserved.**
  `const tckExecutionBaseline = 3897` in `cypher/tck/runner_test.go` is unchanged, **no `.feature`
  file changed in this window**, and every commit that could move it reports **3897 scenarios,
  3897 passed, 0 failed, 0 undefined, 0 inconclusive**, with 16 006/16 006 steps. Mandate 1 was
  verified across a **grammar change** (#2721, read twice — after the grammar change alone and again
  on the finished tree), across eight MVCC repairs, and across every plan-shape change in the
  release.

  **One openCypher divergence ships open, and the TCK is structurally blind to it** (#2675, found in
  sprint 352 and **not fixed in this window**). `ir.TranslateSubquery` builds a subquery's inner plan
  from `q.ReadingClauses` alone, so a body's **final projection is never translated**: a `RETURN`'s
  `DISTINCT`, `ORDER BY`, `SKIP`, `LIMIT` or aggregation is discarded, and
  `COUNT { MATCH … RETURN count(*) }` answers 2 where openCypher requires 1. The conformance count
  cannot move on it in either direction, and that was verified independently rather than assumed:
  **zero of 220 feature files contain `COUNT {`**. `cypher/count_subquery_block_form_test.go` records
  the gap in its own godoc and deliberately asserts no absolute value for the two cases it reaches.
- **100 % ACID-compliant, and strengthened.** Eight engine-side defects were fixed this cycle across
  all four properties — three from the life-record diagnosis (#2724, #2725, #2726), a bulk-removal
  Atomicity break (#2694), a Consistency window between the tombstone flip and the bitmap strip
  (#2687), an Isolation and Atomicity break in the exact label count (#2688), a Consistency window
  in `CREATE INDEX` pair registration (#2703), and a Durability pair — a silently dropped edge
  property on WAL replay and a leaked writer registration that wedged shutdown for ever (#2707).
  Two durable formats now refuse a field they would have written unreadably (#2742, #2743), and a
  panic in the commit window no longer wedges every later committer (#2727).
- **Extreme / massive concurrent ready — measured, not certified.** This is the release in which
  contention stopped being inferred from the shape of a throughput curve: **nothing in the repository
  enabled Go's contention profilers** before #2678, so every prior contention claim rested on
  throughput alone. Four workloads that **anti-scaled** — throughput falling as goroutines rise while
  cores sit idle — no longer do: `index-count-hot`, `index-btree-rw`, `index-count-spread` and
  `cypher-mixed-rw`. What is **not** established: no production certification was run in this window,
  so the most recent one remains
  [`docs/certification-2026-08-13.md`](docs/certification-2026-08-13.md), taken at the `v0.11.0`
  release commit and now **291 commits** behind this tag. `soak-artefacts/` is still from 2026-05-30
  at commit `b5453b9`.
- **Ultra efficient by design — held, with costs stated.** The GC mark cost of a 10 M-key btree falls
  42.4 % (#2684); a subquery's per-outer-row adjacency rebuild goes from Θ(V+E) to zero (#2646); an
  `ORDER BY` sort stops allocating Θ(n log n) row contexts (#2652); a labelled count answers in O(1)
  below 50 000 nodes (#2654). Against that, and quantified above: `index/count`'s uncontended write
  is 2× slower and its per-`Store` memory 4.15×; `index/btree` regresses 6.66 % at one goroutine;
  `index/label` costs +110 B per distinct label; `metrics` costs +13.31 % on an uncontended
  `IncCounter`; and the relationship-type column has a documented losing region below 22.9 % arc
  coverage.

### Notes

- **Pre-1.0 stability.** This is a `0.y.z` release. The public Go API may change without a
  major-version bump until `1.0.0`; pin the exact version you depend on. Seven breaking changes ship
  here, and the minor digit absorbs them.
- **`go.mod` and `go.sum` are byte-identical to `v0.12.0`.** Same pinned `toolchain go1.27.0`, same
  `go 1.26` directive, same dependency set at the same versions. Unlike `v0.12.0`, this release
  refreshes nothing in the supply chain, so a cross-release performance delta measured against
  `v0.12.0` mixes **no** compiler or dependency change with GoGraph's own.
- **`1.0.0` gates: 3 of 4 met, unchanged.** Execution-level TCK is at 100 % against a ≥ 95 %
  requirement, every `T-` divergence is resolved, and the local gate is green. Gate 4 — a soak report
  in `soak-artefacts/` reflecting a run against the release commit — remains open for a **sixth**
  consecutive cycle.
- **Examples are not part of the module.** `examples/` is an exercise harness; the module neither
  imports nor depends on it. Example 31 now starts a real Bolt server on a real socket and drives it
  with the official driver through an autocommit read, a committed transaction and a rolled-back one,
  with seven histograms appearing in the scrape and stable across five runs (#2715); four message
  types are deliberately **not** pinned, because whether a client library sends `DISCARD`, `RESET`,
  `ROUTE` or `LOGOFF` is its own discretion and a presence fact that depends on that is not a fact.

[0.13.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.13.0

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
Every direct dependency, and every tool and CI pin, was raised to its latest release in
this window, so `go.sum` moved; the other `go.mod` change is the pinned `toolchain`
directive, `go1.26.5` → `go1.27.0`, with the `go` directive left at `go 1.26` so the
minimum Go a consumer needs is unchanged (see *Changed*).

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

- **Every direct dependency moves to its latest release** — `RoaringBitmap/roaring/v2`
  **v2.18.2 → v2.26.0** (eight minors; backs the bitmap-intersection access path,
  `graph/index` and `store/snapshot`), `cucumber/godog` **v0.15.1 → v0.16.0** (the
  openCypher TCK runner itself), `klauspost/compress` **v1.18.6 → v1.19.2**, and
  `golang.org/x/sys` **v0.46.0 → v0.47.0**. The other five direct requirements —
  `antlr4-go/antlr/v4`, `edsrzf/mmap-go`, `neo4j/neo4j-go-driver/v5`, `go.uber.org/goleak`
  and `pgregory.net/rapid` — were already at their latest. Indirectly, the godog bump
  carries `cucumber/gherkin` **v26 → v42.0.1** and `cucumber/messages` **v21 → v34.2.1**
  and **replaces `gofrs/uuid` with `google/uuid` v1.6.0**; `bits-and-blooms/bitset`,
  `hashicorp/go-memdb`, `hashicorp/golang-lru` (**v0.5.4 → v1.0.2**) and `spf13/pflag`
  follow. `go mod verify` reports all modules verified and the module-level CVE scan
  reports **no vulnerabilities**.

  Each bump was landed individually per the dependency policy, and the two that could
  have broken a Compliance Mandate were verified rather than assumed. **Upgrading the TCK
  runner leaves the gate at 3897/3897** — 3897 scenarios, 3897 passed, 0 failed, 0
  undefined, 0 inconclusive, 16006/16006 steps. And the `roaring64` rationale the
  #2607/#2608 range fixes rest on — that `AddRange`/`RemoveRange` are half-open with no
  closed variant — **still holds at v2.26.0**, at the same source lines, so the
  workaround is still necessary rather than now redundant. `klauspost/compress` turned
  out to be reachable from exactly one file, `internal/shapegen/graphalytics.go`, and
  **not from the WAL or snapshot path**, so it carries no on-disk-format risk.

- **`golang.org/x/exp` is deliberately NOT bumped, and `ANTLR` is deliberately held at
  4.13.1.** Two exceptions to "latest everywhere", each for a reason. `go mod why` reports
  that the main module does not need `x/exp` and no source file imports it; the newest
  revision declares `go 1.26.0`, which would have forced this module's own **`go`
  directive** from `go 1.26` to `go 1.26.0` — the one directive
  [docs/release.md](docs/release.md) says not to touch, for an unused indirect. ANTLR's
  latest jar is **4.13.2** but the Go runtime `antlr4-go/antlr/v4` has **no 4.13.2**, so
  bumping the generator alone would emit a parser expecting a runtime that does not exist
  for Go.

- **Tooling and CI pins move to their latest releases.** `GOLANGCI_LINT_VERSION`
  **v2.12.2 → v2.13.1**; `cyclonedx-gomod` **v1.10.0 → v1.12.0** in all three places that
  named it (the release workflow, `.goreleaser.yaml`'s comment and `docs/release.md`'s
  local-fallback command), which matters because the SBOM generator's version is a
  supply-chain attestation input; and the three SHA-pinned GitHub Actions —
  `actions/checkout` **v6.0.3 → v7.0.1**, `actions/setup-go` **v6.4.0 → v7.0.0**,
  `goreleaser/goreleaser-action` **v7.2.2 → v7.2.3** — each re-pinned to the commit its
  trailing comment claims, **verified by resolving every SHA against its tag**. Both
  Action majors are packaging migrations (ESM, dependency bumps) that change no input this
  workflow passes; checkout v7's one behavioural change blocks fork-PR checkout for
  `pull_request_target`/`workflow_run`, and this workflow triggers on a `v*` tag push.
  `goreleaser` itself is already at the latest release, 2.18.0, and `benchstat` and
  `goimports` install `@latest`.

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

- **The mandated CVE scan had stopped running, and was reporting success.** A
  `govulncheck` binary built against an older Go minor than the one on `PATH` prints
  *"Loading packages failed, possibly due to a mismatch between the Go version used to
  build govulncheck and the Go version on PATH"* and then **exits 0**. So after the
  toolchain moved, `govulncheck ./...` — step 4 of the dependency policy in
  [CONTRIBUTING.md](CONTRIBUTING.md#dependency-policy) — performed **no analysis at all**
  while returning success: a gate that cannot fail, of exactly the kind this project
  treats as a defect rather than an inconvenience. Rebuilt against `go1.27.0` it fails
  honestly instead (exit 1), because `govulncheck@v1.3.0`'s own source-processing
  packages are built for go1.26 and cannot parse go1.27 source. The scan is therefore run
  as **`govulncheck -scan=module`**, which reads `go.mod` without loading source and
  reports **`No vulnerabilities found.`** across every pinned version. The policy now
  documents both traps and requires the run to print a finding or that literal string —
  **empty output is a failed scan, never a clean one**. Symbol-level reachability
  scanning is unavailable until an upstream release built for the pinned Go minor
  exists, and that limitation is stated rather than left implied.

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
