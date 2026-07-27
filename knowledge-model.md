# Knowledge Model — GoGraph

Authoritative description of the GoGraph knowledge graph (a Label Property Graph stored
in `rmp graph`). The graph and this file **must mirror each other**: whenever a label,
edge type, or property is added or removed, update both in the same change.

- **Roadmap:** `gograph` (all commands take `-r gograph`).
- **Module:** `github.com/FlavioCFOliveira/GoGraph`.
- **Scope:** *full code graph* — every package, type, function, method, test, benchmark,
  fuzz target, and runnable example in the module, plus a curated layer of Features and
  Specs above them, a Sprint/Commit provenance layer, and a memory layer (Agent, Skill,
  Memory) that mirrors the assistant's persistent memory files.
- **Provenance:** every node **and** every edge carries `gitCommit` (full HEAD hash when the
  element was last confirmed) and `gitDate` (ISO `YYYY-MM-DD`).

Counts as of commit `567253c` + in-flight worktree (2026-06-11): **11,867 nodes**, **15,360 edges**.
Incrementally synced at commit `257ce96` (2026-06-14, task #1502): +4 nodes
(`NodePropertiesByIDFunc` Method, `nodePropsToExprMap` Function,
`TestNodePropertiesByIDFunc_MatchesByID` Test, `BenchmarkNodeReturnToPackstream`
Benchmark), +5 edges (4 `CONTAINS`, 1 `HAS_METHOD`).
Incrementally synced at commit `f47b18a` (2026-06-15, task #1506, sprint 190 —
hash join for disconnected equi-join patterns): +5 nodes (`Commit` `f47b18a`;
`HashJoin` Type and `NewHashJoin` Function in `cypher/exec`; `tryBuildHashJoin`
and `hashJoinOrderSafe` Functions in `cypher`), +9 edges (4 `TOUCHES` from the
commit to packages `cypher`/`exec`/`cypher_ldbc_test` and the `HashJoin` Type,
1 `FIXES` to the `Cypher Engine` Feature, 4 `CONTAINS` for the new symbols). The
optimisation is increment A of the optimizer-activation spike (task #1504,
commit `9fa521b`); the `cypher/ir/rewrite` logical Driver remains unwired.
Incrementally synced at commit `657d9ba` (2026-06-15, task #1525, sprint 190 —
result-streaming feasibility spike, DESIGN-ONLY outcome): +4 nodes (`Commit`
`657d9ba`; `Spec` `docs/result-streaming-design.md`; `Task` `1525` COMPLETED and
`Task` `1526` BACKLOG); +8 edges (`Task 1525 -[IMPLEMENTED_IN]-> Commit
657d9ba`; `Task 1525 -[DEPENDS_ON]-> Task 1526`; `Commit 657d9ba -[TOUCHES]->
Spec`; `CypherEngine` and `ACIDTransactions` Features each `-[SPECIFIED_IN]->`
the new Spec; `Task 1526 -[ABOUT]->` both Features). New edge type
`DEPENDS_ON (Task)->(Task)` introduced for the streaming-needs-foundation
dependency. Task #1526 captures the per-shard versioned `Snapshot` root
(`atomic.Pointer[Snapshot]`) foundation that SI-safe lazy result streaming
depends on.
Incrementally synced at commit `9516d52` (2026-06-15, task #1508, sprint 191 —
non-blocking LSN-watermarked checkpoint with WAL prefix truncation): +12 nodes
(`Commit` `9516d52`; `Sprint` `191`; `Task` `1508` COMPLETED; new `Method`s on
`store/wal` `Writer` — `DurableOffset`, `TruncatePrefix`, `poisonAfterRename`,
`writeSuffixTmp`, `reopenAfterPrefixTruncate` — and on `store/checkpoint`
`Checkpointer` — `runUnderCommitLock`, `runNonBlocking`, `writeAndTruncate`,
`truncatePrefixLocked`; plus 9 new `Test` nodes split across `store/wal`
(`truncate_prefix_test.go`), `store/checkpoint` (`writer_stall_test.go`) and
`store/recovery` (`checkpoint_crashinject_test.go` — the renamed crash
scenarios). +many edges: `Sprint 191 -[CONTAINS]-> Commit`; `Task 1508
-[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->` Features `WAL & Recovery`
(id 11553) and `ACID Transactions` (id 9736); `Commit -[TOUCHES]->` Packages
`wal` (249) / `checkpoint` (181) / `recovery` (27); `CONTAINS`/`HAS_METHOD` for
each new Method; `CONTAINS` for each new Test. Provenance bumped on the `Writer`
(1677) and `Checkpointer` (956) Types and the three touched Packages. The
checkpoint now captures the WAL watermark (`wal.Writer.DurableOffset`) + CSR
under the commit lock, writes the snapshot lock-free, and re-acquires the lock
to prefix-truncate the WAL via `wal.Writer.TruncatePrefix` (atomic
copy-suffix-then-rename, never truncate-to-zero). DATA-QUALITY NOTE: the
`CypherEngine` (id 12659) and
`ACIDTransactions` (id 9736) Feature nodes carry a hidden interior character
(`size`=13 and 17 for the 12-/16-char visible names), so `{name:'…'}` equality
and `STARTS WITH`/`CONTAINS 'CypherEngine'` fail to bind them; match by `id(f)`
or `CONTAINS 'Cypher'`+`CONTAINS 'Engine'`. Pre-existing; not corrected here.

Incrementally synced at commit `1bc8eb7` (2026-06-15, task #1513, sprint 192 —
S-PA5: parallel pull-formulation PageRank over a reverse-CSR): +6 nodes
(`Commit` `1bc8eb7`; `Sprint` `192` OPEN; `Task` `1513` COMPLETED; two new
`Test`s `TestPageRank_ParallelBitIdentical` /
`TestPageRank_ParallelCancellation` and one new `Benchmark`
`BenchmarkPageRank_PowerLaw50K`, all in
`search/centrality/pagerank_parallel_test.go`). +8 edges: `Sprint 192
-[CONTAINS]-> Commit`; `Task 1513 -[IMPLEMENTED_IN]-> Commit`; `Commit
-[IMPROVES]->` Feature `Search & Path-finding` (id 10375); `Commit -[TOUCHES]->`
Package `centrality` (`search/centrality`); `centrality -[CONTAINS]->` each of
the 3 new symbols; `TestPageRank_ParallelBitIdentical -[TESTS]->` Function
`PageRankCtx` (`search/centrality/pagerank.go`). The parallel PageRank path
(unexported `pageRankEngine` persistent worker pool, `newPageRankEngine`,
`edgeBalancedBounds`, `pageRankBuildReverseStructure`, constant
`pageRankParallelThreshold`=2048) is bit-identical to the retained serial push
path (gated behind `live>=2048 && GOMAXPROCS>1`); the unexported symbols are not
materialised as nodes (the graph models exported + Test/Benchmark/Fuzz/Example
symbols). Measured 1.68-2.40x; 4-8x ruled out as physically unreachable for this
latency-bound random-gather SpMV (rust-perf analysis). NOTE: `graph create`
rejects `MERGE … SET`; all properties were set inline in the MERGE map.

Incrementally synced at commit `a363735` (2026-06-16, task #1515, sprint 192 —
S-PA5: flatten Brandes predecessors into a zero-alloc arena): +1 node
(`Commit` `a363735`; `Task` `1515` and `Sprint` `192` already existed and were
re-stamped — `Task 1515` → COMPLETED, `Sprint 192` → CLOSED). +5 edges:
`Sprint 192 -[CONTAINS]-> Commit`; `Task 1515 -[IMPLEMENTED_IN]-> Commit`;
`Commit -[IMPROVES]->` Feature `Search & Path-finding` (id 10375);
`Commit -[TOUCHES]->` Package `centrality` and Function `Betweenness`. The
predecessor sets now live in a flat CSR-style arena (offsets + positions over
one backing slice) in `search/centrality/brandes.go` /
`brandes_parallel.go` (shared read-only in-degree array across workers);
steady-state ~10 allocs/op at every size (down from up to ~43k), −44% B/op
geomean, serial scores bit-identical to the legacy slice-of-slices impl (new
`Test` `brandes_arena_bitident_test.go`, new `Benchmark` `BenchmarkBrandes_Scale`).
The hypothesised 10-25% time win was empirically falsified (Brandes is
BFS-bound; predecessor sets too small for arena cache-locality to manifest), so
it landed as an allocation/GC-pressure win, time-neutral — the task's
acceptance criteria were reframed accordingly. Also registered the DST
(Deterministic Simulation Testing) simulator effort as 5 `Sprint` nodes
(`195`-`199`; `195` OPEN — Phase 1 Core simulator infrastructure in
`internal/sim`; `196`-`199` PENDING), modelled on TigerBeetle VOPR.

Incrementally synced at commits `12ec636`..`455bbef` (2026-06-16, tasks
#1528-#1536, sprint 195 — DST Phase 1 Core simulator infrastructure, now
CLOSED): new `Feature` `DST Simulator` (a deterministic, seed-reproducible,
single-goroutine tick-loop test harness modelled on TigerBeetle VOPR that
drives the real engine against a shadow `GraphOracle` with ACID + graph
invariant checks); new `Package`s `sim` (`internal/sim`) and `main`
(`cmd/sim`); 7 `Commit`s and 9 `Task`s (`1528`-`1536`, all COMPLETED). Edges:
`Sprint 195 -[CONTAINS]->` each Commit; each Task `-[IMPLEMENTED_IN]->` its
Commit; each Commit `-[TOUCHES]->` Package `internal/sim` (the CLI commit +
the --ticks fix also TOUCH `cmd/sim`); `Feature DST Simulator -[IMPLEMENTED_IN]->`
the capstone Commit `b441950`; `Package internal/sim -[IMPLEMENTS]-> Feature`.
Phase-1 files: `seed.go` (PCG `math/rand/v2`), `clock.go` (VirtualClock),
`disk.go` (in-mem faulting SimDisk over a restated `walFile`), `oracle.go`
(GraphOracle shadow model), `checker.go`+`adapter.go` (InvariantChecker over a
minimal `Engine` iface bridging the real `cypher.Engine`), `actor.go`+
`workload.go` (HonestWriter/HonestReader + weighted mix), `sim.go`+`report.go`
(safety-phase Simulator + SimReport), `cmd/sim/main.go` (CLI). No new label or
edge type introduced. Determinism proven (same seed → byte-identical op
stream); -race/golangci/staticcheck/govulncheck clean; nothing under
`graph`/`cypher`/`store`/`bolt` was touched (TCK + ACID unaffected by
construction).

Incrementally synced at commits `1d0529a`..`6815b56` (2026-06-16, tasks
#1537-#1545, sprint 196 — DST Phase 2 Crash & recovery + Clock injection, now
CLOSED): new `Package` `clock` (`internal/clock`) — a behaviour-preserving
injectable `Clock` interface (`realClock` default, fake clock for tests); +10
`Commit`s, +9 `Task`s (`1537`-`1545` COMPLETED) and a deferred backlog `Task`
`1546`. PRODUCTION CHANGES (all behaviour-preserving, default = real time / real
OS fs): `store/checkpoint` cadence loop and `bolt/server` explicit-tx timeout
reaper now route through the injectable `Clock`; `store/recovery` gained an
exported `ReplayWAL[N,W]` extracted from `openCodec` (pure refactor — `openCodec`
calls it; storage-engine-auditor certified byte-for-byte equivalence). The WAL
group-commit path was AUDITED and found already wall-clock-free (pure `sync.Cond`
leader/follower, ref LEDGER 0015) — NO injection there. SIM-SIDE (`internal/sim`):
`simstore.go` (SimDisk-backed `txn.Store`+`cypher.Engine` over the WAL-only
recovery path, F1 torn-tail truncate-before-append), `crash.go` (deterministic
seed-driven `CrashSchedule`), `MalformedSender` bad-actor + `BadActorWorkload`,
crash+recovery folded into the tick loop (opt-in via `CrashConfig`, safe default
OFF — no-crash runs byte-identical), and a post-recovery Durability (ACID-D)
invariant battery. Edges: `Sprint 196 -[CONTAINS]->` each Commit; each Task
`-[IMPLEMENTED_IN]->` its Commit (#1540 spans the recovery-extract + simstore
commits); Commits `-[TOUCHES]->` their Packages; `Commit 24643fe -[TOUCHES]->`
Feature `WAL & Recovery` (id 11553); `Commit bbb6ea8 -[IMPLEMENTED_IN]->` Feature
`DST Simulator`. No new label or edge type. Full gate held: TCK 3897/3897, ACID
crash battery green, -race clean on store/bolt/cypher, golangci/staticcheck/
govulncheck 0; deterministic crash+recovery proven (seed 7: 11 crashes, 37673
WAL ops replayed, identical across runs, zero durability violations). The full
snapshot/csrfile/checkpoint-on-SimDisk wiring was deferred to backlog #1546 (the
WAL-only seam was chosen to avoid touching mmap/`O_NOFOLLOW` security-hardened
code).

Incrementally synced at commits `f631e8e`..`3a49b79` (2026-06-16, tasks
#1547-#1555, sprint 197 — DST Phase 3 Full actor suite + Bolt wire + liveness,
now CLOSED): +10 `Commit`s, +9 `Task`s (`1547`-`1555` COMPLETED) + a new backlog
`BUG` `Task` `1556`. SIM-SIDE (`internal/sim`, hybrid determinism per the
user-decided model): `SimConn` (custom bounded-buffer 64 KiB/dir net.Conn — NOT
net.Pipe, which deadlocks when both ends block) + `SimListener`; a Bolt wire
client + `SimServer` driving the REAL `bolt/server` over `Serve(ctx, ln)` (no
production hook needed — the listener seam already exists); `BoltAbuser`
(wire-protocol violations, lock-step deterministic), `OverloadActor`
(resource-pressure), `SlowConsumer` (backpressure/no-leak, concurrent),
`SchemaChanger` (DDL under load, concurrent); a concurrent multi-connection
harness (goleak-clean, eventual oracle==engine, NOT bit-reproducible); and a
two-phase safety→liveness driver with a deadlock/resonance watchdog. `cmd/sim`
gained `--mode=wire|concurrent|liveness`. PRODUCTION CHANGE: ONE behaviour-
preserving fix — `bolt/server.failTransition` now names the originating session
state in the FAILURE message instead of always "FAILED" (`82d98af`, surfaced by
BoltAbuser, regression test added). FINDING (reported, NOT fixed — tracked as
BUG #1556 with a pinning test `internal/sim/dropconstraint_finding_test.go`):
`DROP CONSTRAINT <name>` by name is a fail-silent no-op (reports SUCCESS but the
UNIQUE constraint + its backing index survive, permanently blocking re-creation)
— a Consistency-mandate violation whose fix widens into the IR/constraint
registry. Edges: `Sprint 197 -[CONTAINS]->` each Commit; each Task
`-[IMPLEMENTED_IN]->` its Commit; Commits `-[TOUCHES]->` `internal/sim` (the
bolt fix → `bolt/server`, the CLI commit → `cmd/sim`); `Commit 84791a9
-[IMPLEMENTED_IN]->` Feature `DST Simulator`; `Commit 82d98af -[FIXES]->` Feature
`Bolt Protocol`. No new label or edge type. Gate held: TCK 3897/3897, -race +
goleak clean on internal/sim + bolt/server, lint/staticcheck/govulncheck 0;
lock-step single-conn wire is byte-reproducible, concurrent/liveness modes are
goleak+convergence-guarded.

Incrementally synced at commit `171e9d3` (2026-06-16, task #1556, sprint 200 —
S-fix-drop-constraint, CLOSED): FIXED the fail-silent `DROP CONSTRAINT <name>`
no-op surfaced by the DST simulator (Phase 3). +1 `Commit`; `Task 1556` (the BUG)
→ COMPLETED; new `Sprint 200`. Root cause: by-name DROP produced empty
label/prop in the IR (`cypher/ir/ddl_parser.go`), so the operator targeted the
nonexistent index `__uniq__.` and, with IF EXISTS, silently absorbed
`ErrIndexNotFound` and reported success without unregistering the constraint or
dropping its real backing index. Fix: `cypher/exec/constraints.go` adds
`ResolveByName` + `ErrConstraintNotFound`; `cypher/exec/drop_constraint.go` +
`cypher/api.go` resolve name→(kind,label,prop) from the registry and drop the
constraint + its `__uniq__<Label>.<prop>` backing index as ONE durable
`OpDropConstraint` unit (the backing index is never separately persisted —
recovery reconstructs it from the constraint set, so a torn intermediate is
structurally impossible); IF-EXISTS no-op on absent, typed
`ErrConstraintNotFound` (→ Bolt `Neo.ClientError.Schema.ConstraintDropFailed`)
otherwise. storage-engine-auditor CERTIFIED atomicity + crash-safety. Edges:
`Sprint 200 -[CONTAINS]-> Commit`; `Task 1556 -[IMPLEMENTED_IN]-> Commit`;
`Commit -[FIXES]->` Feature `ACID Transactions`; `Commit -[TOUCHES]->` Packages
`cypher/exec` + `store/recovery`. Schema DDL is a Neo4j extension NOT covered by
the openCypher TCK (verified — no DROP CONSTRAINT scenarios), so 3897 is
insensitive and held; new engine-level tests + a `constraint.drop.post-wal-sync`
crash scenario cover it; the DST pinning test was flipped to a regression guard.
No new label or edge type.

Incrementally synced at commits `be91e38`..`712d455` (2026-06-16, tasks
#1557-#1563, sprint 198 — DST Phase 4 Scenario registry + trace shrinking, now
CLOSED): +8 `Commit`s, +7 `Task`s (`1557`-`1563` COMPLETED). TEST-ONLY
(`internal/sim` + `cmd/sim` — no production code changed; TCK 3897/3897 held).
New `internal/sim` pieces: `scenario.go`+`catalogue.go` (a `Scenario` config +
named `Registry`, no global state — 8 scenarios: crash-storm, write-heavy,
read-heavy, schema-chaos, bad-actors, overload, bulk-vs-online, long-running),
`trace.go` (deterministic trace recording + scripted replay — DETERMINISTIC
modes only; concurrent interleaving not bit-replayable), `shrink.go` (ddmin
trace shrinking to a minimal failing reproducer — demoed 500→1 op, 500×), a
full (non-sampled) index-vs-base-data consistency check in `checker.go`, and
`cmd/sim` flags `-scenario`/`-list-scenarios`/`-replay` (verbose per-op trace +
shrunk reproducer on failure). Edges: `Sprint 198 -[CONTAINS]->` each Commit;
each Task `-[IMPLEMENTED_IN]->` its Commit; Commits `-[TOUCHES]->` `internal/sim`
(the CLI commit → `cmd/sim`); `Commit 80f9d44 -[IMPLEMENTED_IN]->` Feature `DST
Simulator`. No catalogue scenario surfaced a production bug (all ran clean or
with only expected bad-actor/overload errors). -race + goleak clean,
lint/staticcheck/govulncheck 0. No new label or edge type.

Incrementally synced at commits `237146a`..`580ee21` (2026-06-16, tasks
#1564-#1570, sprint 199 — DST Phase 5 Swarm + advanced oracles, now CLOSED) and
`8d1ce89` (task #1571, sprint 201 — DST P5b cross-release harness, CLOSED): +10
`Commit`s, +8 `Task`s (`1564`-`1571` COMPLETED), new `Package` `main`
(`cmd/sim-xrelease-helper`). TEST-ONLY (no production code changed; TCK
3897/3897 held). `internal/sim`: `swarm.go` (bounded-worker, time/count-boxed,
reproducible-seed-set swarm runner, goleak-clean), `coverage.go` (concurrency-
safe coverage tracker + biasing selector toward rare paths; reports
unobservable signals rather than adding production hooks), `upgrade.go`
(WAL-only data-compat round-trip across a version boundary + corrupt-image
fail-stop), `differential.go` (replays a P4 Trace against two equivalent-result
engine toggles — `DisableHashJoin`/`DisableRangeIndexSeek` — must agree),
`metrics_oracle.go` (asserts exported counters + goroutine baseline against the
oracle's accounting). `cmd/sim` gained `-swarm`/`-workers`/`-runs`/`-duration`/
`-coverage-report`. P5b (`crossrelease.go`+`crossrelease_run.go`+
`cmd/sim-xrelease-helper`): builds a prior git tag (v0.2.0, v0.3.0) into a temp
git worktree, runs it as a subprocess speaking a line-JSON write/selfcheck
protocol over the v0.2.0..HEAD-stable store/wal/cypher API, and proves the
current code opens+recovers a prior-release WAL image faithfully (UPGRADE) +
diffs the same op stream against the prior engine (DIFFERENTIAL, unordered-LIMIT
divergences classified benign). FINDING (informational, tracked as SPIKE #1572,
NOT a current-code defect): v0.2.0's OWN recovery over-counts (live n=32 vs
self/WAL n=79 — MERGE dedup not persisted in v0.2.0's WAL); current code is
FAITHFUL (reproduces v0.2.0 self-recovery exactly), v0.3.0 round-trips fully.
Edges: `Sprint 199/201 -[CONTAINS]->` their Commits; each Task
`-[IMPLEMENTED_IN]->` its Commit; Commits `-[TOUCHES]->` `internal/sim`
(+`cmd/sim`, +`cmd/sim-xrelease-helper`); `Commits 237146a/8d1ce89
-[IMPLEMENTED_IN]->` Feature `DST Simulator`. Cross-release builds run on the
soak lane (env-precondition skip when git/tag/toolchain unavailable); a fast
HEAD-as-prior smoke runs on short. -race + goleak clean, lint 0. No new label
or edge type. **The 5-phase DST simulator (P1-P5 + P5b) is COMPLETE.**

Incrementally synced at commit `620a4b2` (2026-06-23, task #1686 — per-instance
by-handle edge properties maintained durably on a relationship SET / REMOVE /
SET +=): +1 `Commit` (`620a4b2`). Bounded sync — Commit→Feature/Package
provenance only (task #1686 is not modelled as a `Task` node; the Sprint→Task
layer remains a pre-existing gap not back-filled here). Edges: `Commit
-[FIXES]->` Features `ACID Transactions` (9736), `Cypher Engine` (12659),
`WAL & Recovery` (11553), `Stable Element Identity` (13477); `Commit -[TOUCHES]->`
Packages `store/txn` (70), `store/recovery` (27), `graph/lpg` (448), `cypher`
(73), `cypher/exec` (39), `cmd/crashinject-helper` (227). Those four Features and
six Packages were re-stamped to gitDate 2026-06-23. The change makes `SET r.x`,
`SET r += {…}`, `SET r = {…}`, `REMOVE r.x` and `SET r.x = null` maintain the
per-instance by-handle edge-property store durably (dual-written with the per-pair
store, which stays authoritative for reads — read routing is deferred to #1684,
which #1686 unblocks). New WAL op `OpDelEdgePropertyByHandle` (appended at the end
of the `OpKind` enum) + recovery applier `applyDelEdgePropertyByHandle`; new
`lpg.Graph.DelEdgePropertyByHandle` (+ NodeID dual); the bound parallel instance's
stable handle is resolved from its forward-CSR edge position via the new
`GraphMutator.EdgeHandleAtPosition` (reusing the read-path `edgeHandleAtFwdPos`,
plumbed through `RelCols.EdgeCol`); the by-handle write, its WAL frame, the
secondary-index enqueue and a new undo inverse all land in the one
`ApplyAtomically` window and the one transaction as the per-pair write.
storage-engine-auditor certified ACID + recovery; cypher-expert certified
TCK-neutrality. TCK held at 3897; -race / golangci / staticcheck / govulncheck
clean. Local commit, not pushed. No new label or edge type (`FIXES`/`TOUCHES`
already in use in the live graph).

Incrementally synced at commits `43383a9`..`7a342e0` (2026-06-23, sprints 226
S-MT2 / 227 S-BL1 / 228 S-BL2, all CLOSED): +10 `Commit`s, +3 `Sprint`s
(226/227/228). Bounded sync — Commit→Feature provenance only (the Sprint→Task
layer for sprints ~202-225 remains UNSYNCED, a pre-existing gap; this entry does
not back-fill it). Edges: `Sprint 226 -[CONTAINS]->` 43383a9/528b371; `Sprint
227 -[CONTAINS]->` b7053b8/1edfa57/b5a3729/92b6fc6; `Sprint 228 -[CONTAINS]->`
51b7c69/7a342e0. `43383a9/528b371/92b6fc6 -[IMPROVES]->` Feature `Search &
Path-finding` (parallel Floyd-Warshall #1680 3.29x, parallel WCC #1679 1.63x,
stateful SSSP validate-once #1516 102x); `b7053b8/51b7c69 -[FIXES]->` `Cypher
Engine` (DateValue.String expanded-year inverse #1658; per-instance reltype on
the multigraph reverse hop #1634 — handle disambiguation in exec.Expand +
storage-key-keyed type resolution in cypher/api, TCK 3897 held); `1edfa57
-[FIXES]->` `Observability & Metrics` (one IO latency sample at the Ctx layer
#1524); `34285c3 -[IMPROVES]->` `Examples & Tutorials`. Touched Features
re-stamped to gitDate 2026-06-23. #1634 surfaced 3 new BACKLOG bugs (#1683 MERGE
distinct-type, #1684 opposite-direction reltype collapse, #1685 VLE/shortestPath
collapse) — tracked in rmp, not yet modelled. No new label or edge type.

Incrementally synced at commit `8bb0356` (2026-07-02, task #1856, sprint 262
"Audit remediation 2026-07-02", OPEN): +1 `Sprint` (262); +1 `Commit`
(`8bb0356`); +1 `Task` (`1856`, COMPLETED). Fixed finding F1 from a same-day
6-specialist production-readiness audit (report `docs/audit-production-
readiness-2026-07-02.md`, not modelled as a `Spec` node — audit reports are
not part of the curated Spec layer): the Cypher engine, constructed over the
documented default non-multigraph config, silently discarded a second
relationship created between an already-connected node pair instead of
storing it — openCypher CREATE never deduplicates (TCK `Create3.feature`),
so this was both silent write loss and a conformance gap on the shipped
default path. Fix: a fail-fast guard (new sentinel `cypher.
ErrParallelEdgeInSimpleGraph`) at `lpgMutatorAdapter`/`walMutatorAdapter`'s
`AddEdge`/`AddEdgeH` in `cypher/api.go`, raised before any mutation or WAL
frame; a one-time constructor warning; every Cypher-facing example/doc
switched to `Multigraph: true`; a new regression test file
(`cypher/parallel_edge_multigraph_guard_test.go`); two pre-existing
`rollback_noop_edge_test.go` tests that had pinned the old silent no-op (from
the narrower #1751 fix) reconciled to the new contract; and a latent
always-undirected shared test engine in `bolt/server/
e2e_shape_roundtrip_test.go` fixed as a direct, independently-confirmed
consequence. Edges: `Sprint 262 -[CONTAINS]-> Commit`; `Task 1856
-[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->` Features `Cypher Engine`
(id 12659, matched by `id(f)` per the documented hidden-character gotcha) and
`Examples & Tutorials` (id 8536); `Commit -[TOUCHES]->` Packages `cypher`
(id 73) and `bolt/server` (id 98). Those two Features and two Packages were
re-stamped to gitDate 2026-07-02. Certified by three specialist reviews
(cypher-expert-consultant: conformant per TCK evidence; storage-engine-
auditor: ACID-sound, subsumes #1751 by construction; go-developer:
idiomatic) plus a green TCK run (3897/3897) and a clean full-module `-race`
run. No new label or edge type. Local commit, not pushed.

Incrementally synced at commit `beedc31` (2026-07-02, task #1857, sprint 262,
still OPEN): +1 `Commit` (`beedc31`); +1 `Task` (`1857`, COMPLETED). Fixed
finding F2 from the same audit: `cypher/expr/equiv.go`'s `EquivalentHash`
disagreed with its own sibling `Equivalent` for a cross-type Integer/Float
pair — `Equivalent(IntegerValue(1), FloatValue(1.0))` was already `true`
(`Equal` compares via `float64(a)==float64(b)`), but `EquivalentHash` hashed
a Float via its IEEE-754 bit pattern and an Integer via the unrelated raw
int64-bit fold, breaking the hash invariant DISTINCT/grouping/UNION rely on
(`count(DISTINCT [1, 1.0])` returned 2, not 1). Fix: a new `hashFloatBits`
helper (the pre-existing −0.0-canonicalise + `Float64bits` fold) shared by
both the `FloatValue` and a new `IntegerValue` branch, so an Integer now
hashes through the identical cast `Equal` already performs; `Equal`,
`Hash()`, and the general-purpose `Value.Hash()` contract are untouched, per
this file's own documented Equal/Equivalent split. Edges: `Sprint 262
-[CONTAINS]-> Commit`; `Task 1857 -[IMPLEMENTED_IN]-> Commit`; `Commit
-[FIXES]->` Feature `Cypher Engine` (id 12659); `Commit -[TOUCHES]->`
Packages `cypher` (id 73) and `cypher/expr` (id 141, first `TOUCHES` target
in that package). Feature and both Packages re-stamped to gitDate
2026-07-02. Certified by cypher-expert-consultant (hashing through the exact
cast `Equal` performs is the only self-consistent fix, verified
exhaustively) and go-developer (idiomatic, 0 lint/vet/staticcheck). Surfaced
a separate, pre-existing, unrelated bug in `cypher/exec/hash_join.go`'s
`canonicalKeyHash` (an independent, opposite-direction lossy Float→Integer
cast for join-key hashing) — recorded as new backlog `Task` `1865`, not
modelled or fixed here (out of scope for F2). No new label or edge type. TCK
3897/3897 held, full `-race` clean (one transient, unrelated goroutine-count
flake in `cypher/exec` reproduced clean 3/3 in isolation and on a full
re-run). Local commit, not pushed.

Incrementally synced at commit `a9b5f54` (2026-07-02, task #1860, sprint 262,
still OPEN): +1 `Package` (`bench/cypher_scale`, `kind:'bench'`); +1
`Commit` (`a9b5f54`); +1 `Task` (`1860`, COMPLETED); +1 `Spec`
(`docs/benchmarks/cypher-scale.md`, no `SPECIFIED_IN` edge — matching the
sibling `docs/benchmarks/cypher.md` (id 12388), per-benchmark docs are
outside the curated `SPECIFIED_IN` mapping, which covers only
`docs/profiling.md`/`docs/optimisations.md`). Closed finding P3 from the
same audit (test-only, no behaviour change): the only pre-existing
Cypher-engine benchmark suite (`bench/cypher_ldbc`) runs over a 1000-node
seed, so the P1/P2 boxing costs the audit measured at 100k nodes had no
permanent regression gate at that scale. New package `bench/cypher_scale`
seeds a shared 120k-node graph once and benchmarks three shapes (scan+
aggregate, scan+filter+project, type-filtered 1-hop expand); the measured
numbers land close to the audit's own findings. Edges: `Sprint 262
-[CONTAINS]-> Commit`; `Task 1860 -[IMPLEMENTED_IN]-> Commit`; `Commit
-[IMPROVES]->` Feature `Benchmarking & Profiling` (id 13375); `Commit
-[TOUCHES]->` the new Package. Feature and Package re-stamped to gitDate
2026-07-02. Certified idiomatic by go-developer (deterministic edge seeding
verified collision-free by exhaustive brute force over all 120 000 nodes;
smoke test measured well under the short-layer budget; gofmt/vet/staticcheck
clean). No new label or edge type. TCK 3897/3897 unaffected. Local commit,
not pushed.

Incrementally synced at commit `715a5cd` (2026-07-02, task #1861, sprint
262, still OPEN): +1 `Commit` (`715a5cd`); +1 `Task` (`1861`, COMPLETED).
Closed finding A1 from the same audit (doc-only, no behaviour change):
`search/centrality/brandes.go`'s `Betweenness` godoc told callers to divide
the undirected raw output by `(n-1)(n-2)/2`, 2x too small, since Brandes'
source loop is ordered-endpoint-based for both directed and undirected
graphs — the same single `(n-1)(n-2)` divisor the sibling
`WeightedBetweenness` doc already uses correctly. Edges: `Sprint 262
-[CONTAINS]-> Commit`; `Task 1861 -[IMPLEMENTED_IN]-> Commit`; `Commit
-[FIXES]->` Feature `Search & Path-finding` (id 10375); `Commit
-[TOUCHES]->` Package `search/centrality` (id 54). Feature and Package
re-stamped to gitDate 2026-07-02. No new label or edge type. TCK 3897/3897
unaffected. Local commit, not pushed.

Incrementally synced at commit `6bbe332` (2026-07-02, task #1862, sprint
262, still OPEN): +1 `Commit` (`6bbe332`); +1 `Task` (`1862`, COMPLETED).
Closed finding A2 from the same audit (doc-only, no behaviour change):
`search/triangles.go`'s `CountTriangles` (and `search/triangles_parallel.go`'s
`CountTrianglesParallel`) is correct only on a simple graph — a self-loop or
a parallel edge on a triangle's lowest-ranked vertex is walked more than
once by the degree-ordered node-iterator algorithm, silently over-counting
that triangle. The function never checked or documented this, and
`adjlist.Config{Multigraph: true}` can represent both. Both docs now state
the precondition. Edges: `Sprint 262 -[CONTAINS]-> Commit`; `Task 1862
-[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->` Feature `Search &
Path-finding` (id 10375); `Commit -[TOUCHES]->` Package `search` (id 131,
where both triangle-counting files live). Feature and Package re-stamped to
gitDate 2026-07-02. No new label or edge type. TCK 3897/3897 unaffected.
Local commit, not pushed.

Incrementally synced at commit `6dfa4a0` (2026-07-02, task #1863, sprint
262, still OPEN): +1 `Commit` (`6dfa4a0`); +1 `Task` (`1863`, COMPLETED).
Closed finding P4 from the same audit (test-only, no production behaviour
change): `bench/cypher_alloc`'s four `TestZeroAlloc_*` gates seeded their
walker from NodeID 0, entirely inside the range Go's runtime boxes for free
via a shared `staticuint64s` array (0-255), always reporting 0 allocs/op
regardless of the operator's real cost. Switching to NodeID 1000+ surfaced
that `AllNodesScan.Next` genuinely boxes 1 allocation per call, forwarded
unchanged through `Filter`/`Project`/`ResultSet`; `cypher/exec/scan_all_test.go`'s
similarly misleadingly-named `BenchmarkAllNodesScan_ZeroAlloc` (which already
used realistic IDs and measured ~759 allocs/op) was renamed to
`BenchmarkAllNodesScan_PerNodeAllocCost`. A go-developer review of the
initial fix caught a second instance of the same defect class: the
`ResultSet` gate's claimed 2nd allocation (a `Record` map re-box) was never
actually measured (its closure only called `Next`, never `Record`); a direct
measurement showed `Next`+`Record` together cost exactly 1, not 2 — both
corrected in the same commit. Edges: `Sprint 262 -[CONTAINS]-> Commit`;
`Task 1863 -[IMPLEMENTED_IN]-> Commit`; `Commit -[IMPROVES]->` Feature
`Benchmarking & Profiling` (id 13375); `Commit -[TOUCHES]->` Packages
`cypher/exec` (id 39) and `bench/cypher_alloc` (id 242). Feature and both
Packages re-stamped to gitDate 2026-07-02. No new label or edge type. TCK
3897/3897 held, full-module `-race` clean. Local commit, not pushed.

Incrementally synced at commit `e753852` (2026-07-02, task #1864, sprint
262 — **CLOSED, 7/7 tasks COMPLETE**): +1 `Commit` (`e753852`); +1 `Task`
(`1864`, COMPLETED); `Sprint 262` re-stamped `status='CLOSED'`. Closed the
final finding (INFO) from the same audit — four doc-faithfulness fixes, no
behaviour change: `search/bellman_ford.go`'s exported doc wrongly claimed
"textbook O(V*E)" for what is actually SPFA+SLF; `cypher/ir/translator.go`'s
`exprToColumnName` gained a note correcting the audit's own causal theory —
an implicit column header like `"age - 1"` comes from AST re-serialisation
in the canonical TCK form, not from a leak of the parser's
`normalizeArithmeticMinus` rewrite; `cypher/tck/conformance_history.go` +
`compare_test.go` gained a "Gate scope" note stating precisely what the
3897/3897 execution gate verifies (full row/value comparison) versus what
it does not (exact error type, property/label side effects, "no side
effects"). The integer-overflow precondition the audit also flagged was
already documented by an earlier session. Edges: `Sprint 262
-[CONTAINS]-> Commit`; `Task 1864 -[IMPLEMENTED_IN]-> Commit`; `Commit
-[FIXES]->` Features `Cypher Engine` (id 12659) and `Search &
Path-finding` (id 10375); `Commit -[TOUCHES]->` Packages `cypher/ir`
(id 91), `cypher/tck` (id 29), and `search` (id 131). Both Features and
all three Packages re-stamped to gitDate 2026-07-02. No new label or edge
type. TCK 3897/3897 held.

**Sprint 262 summary (2026-07-02 audit remediation, ALL LOCAL, NOT
PUSHED): 7/7 tasks COMPLETE.** F1 HIGH (silent parallel-edge drop, fixed
with a fail-fast guard) → F2 MED (int/float DISTINCT hash mismatch, fixed)
→ P3 MED (120k-node Cypher benchmark added) → A1/A2 LOW (betweenness/
triangle-count doc fixes) → P4 LOW (vacuous alloc-gate fix, which itself
uncovered and fixed a second unverified-threshold defect via go-developer
review) → INFO (4 doc-faithfulness fixes). Every task certified by at
least one specialist review or, for the lowest-risk doc-only items,
directly-verified self-review; every commit held the TCK 3897/3897 gate
and a full-module `-race` run. 13 commits total (the 13th being this very
KG-sync entry): `d531ad6` (audit report), `8bb0356`+`f6ae4d1` (F1),
`beedc31`+`791a78a` (F2), `a9b5f54`+`afd022e` (P3), `715a5cd`+`e480371`
(A1), `6bbe332`+`fff3f3a` (A2), `6dfa4a0`+`3408c18` (P4), `e753852` (INFO).

Incrementally synced at commit `d0e9650` (2026-07-02, task #1866, new
`Sprint 263` "Audit remediation Round 2 2026-07-02", OPEN): +1 `Sprint`
(263); +1 `Commit` (`d0e9650`); +2 `Task`s (`1866` COMPLETED, `1875`
BACKLOG). Fixed the HIGH finding from a same-day round-2 audit (report
`docs/audit-production-readiness-2026-07-02-round2.md`, not modelled as a
`Spec` node, matching the round-1 report's precedent): `MERGE (a:L1{...})
-[:REL]->(b:L2{...})` with at least one endpoint not already bound by an
earlier clause — the single most common Cypher graph-building idiom —
used to silently create only the first node, dropping the relationship
and second node with no error (a genuine openCypher TCK coverage gap:
every `Merge*.feature` scenario pre-binds both endpoints). Fix: a new IR
node family (`ir.MergePattern`/`MergePatternEndpoint`/`MergePatternHop`
in `cypher/ir/plan.go`) and physical operator (`exec.MergePattern`,
`cypher/exec/merge_pattern.go`, new file) implement openCypher's
whole-pattern match-or-create semantics via a left-deep join search
(`GraphMutator.OutNeighbours`/`InNeighbours`) with an atomic
create-everything-missing fallback, for any chain the narrow, unchanged
`MergeRelationship` fast path does not cover — fresh endpoints, multi-hop
chains, node-targeted ON CREATE/ON MATCH actions, undirected/incoming
direction, literal/parameter/dynamic properties, and UNIQUE/NOT NULL
constraint enforcement (mirroring `Merge`/`CreateNode`, added after a
go-developer review caught its initial omission). New translator function
`buildMergePatternChain` in `cypher/ir/writes.go`, wired into `mergeClause`;
new `case *ir.MergePattern:` in `cypher/api.go`'s `buildOperatorWrite`.
Two adjacent bugs fixed in the same commit: `Engine.Run` (`cypher/api.go`)
gave an opaque "unsupported IR node" error for ANY write-clause query — a
genuinely pre-existing gap, confirmed to also affect the already-shipped
`MergeRelationship`, unrelated to this task — now rejects clearly,
wrapping the existing `ErrWriteInReadOnlyTx` sentinel; a cyclic MERGE
pattern re-referencing the same fresh variable (`MERGE (a)-[:R1]->(b)
-[:R2]->(a)`) now rejects at plan-build time in `buildMergePatternChain`
instead of a confusing runtime "bound variable is null" error. Edges:
`Sprint 263 -[CONTAINS]-> Commit`; `Task 1866 -[IMPLEMENTED_IN]-> Commit`;
`Commit -[FIXES]->` Feature `Cypher Engine` (id 12659); `Commit
-[TOUCHES]->` Packages `cypher` (73), `cypher/exec` (39), `cypher/ir`
(91); `Task 1866 -[FOLLOWED_BY]-> Task 1875` (new edge type — a task
spawning a tracked, non-blocking follow-up, distinct from `DEPENDS_ON`'s
prerequisite semantics). Feature and all three Packages re-stamped to
gitDate 2026-07-02. Certified by cypher-expert-consultant (CERTIFIED WITH
FOLLOW-UPS against all 9 `Merge*.feature` files read in full — one
narrow, TCK-invisible, non-data-corrupting gap found: `MergePattern`
under-counts relationship multiplicity when 2+ parallel qualifying
relationships already exist between a matched node pair, since its
`binding` type tracks only node identity; filed as `Task 1875`, not
modelled with its own Commit/edges since it is unscheduled backlog work,
matching the #1865 precedent of doc-only tracking for an unscheduled
follow-up) and go-developer (initial review: CHANGES REQUIRED — missing
constraint enforcement was the blocking finding, plus `errors.Is`
consistency, a dead-code branch, and DRY duplication; all fixed and
re-verified). 16 new tests (`cypher/merge_pattern_test.go`,
`cypher/run_rejects_write_test.go`, both new files). TCK 3897/3897 held,
full-module `-race` clean (98 packages). Local commit, not pushed.

Incrementally synced at commit `f66e31c` (2026-07-02, task #1867, sprint
263, still OPEN): +1 `Commit` (`f66e31c`); +1 `Task` (`1867`, COMPLETED).
Fixed the other HIGH finding from the round-2 audit:
`distinctAggregator` (`cypher/api.go`, backs `count/sum/avg/min/max/
collect(DISTINCT …)`) kept a seen-values dedup map with neither a count
cap nor a byte budget, unlike every sibling pipeline-breaker
(`exec.Distinct`, `exec.EagerAggregation`, `funcs.CollectAgg`). For a
streaming aggregator (count/sum/avg/min/max hold O(1) state and never
trip a budget of their own), the seen-values set was the only thing
growing per distinct value, unbounded. Fix: a new
`DefaultMaxAggregateDistinctValues` constant (10M, matching the
sibling row/value-count convention) plus an optional byte-budget
dimension reusing the EXISTING per-query byte ceiling
(`bopts.maxResultBytes` via `resultByteBudget`) and per-value
estimator (`estimateValueSize`) every other breaker already shares —
no new configuration knob introduced, deliberately matching
`exec.Distinct`/`exec.EagerAggregation`'s own precedent of NOT
exposing a dedicated `EngineOptions` field (only the collect-family
aggregators do, via the pre-existing `MaxCollectItems`). Both caps are
checked before the value reaches the inner aggregator, so a rejected
value leaves no partial trace — never a wrong result, only the new
typed sentinel `ErrAggregateDistinctMemoryExceeded`.
`newDistinctAggregator` gained a `WithByteBudget` builder method
(mirroring `exec.Distinct.WithByteBudget`'s chained-after-construction
shape) after a go-developer review flagged the initial all-in-the-
constructor signature as inconsistent with the codebase's 5-for-5
builder convention for this pattern. Edges: `Sprint 263 -[CONTAINS]->
Commit`; `Task 1867 -[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->`
Feature `Cypher Engine` (id 12659); `Commit -[TOUCHES]->` Package
`cypher` (73). Feature and Package re-stamped to gitDate 2026-07-02.
Certified by cypher-expert-consultant (CERTIFIED — independently
re-ran the full TCK rather than trusting the reported gate, traced
`collect(DISTINCT x)`'s interaction with its own separate
`funcs.DefaultMaxCollectItems` cap, confirmed no off-by-one against
every TCK DISTINCT-in-aggregation scenario, which tops out at 3
distinct values) and go-developer (APPROVED WITH FOLLOW-UPS — the
builder-method suggestion, applied in the same commit). 7 new tests
(`cypher/distinct_aggregator_internal_test.go` white-box,
`cypher/aggregate_distinct_cap_test.go` end-to-end engine wiring).
TCK 3897/3897 held, `-race` clean. Local commit, not pushed.

Incrementally synced at commit `6fafb42` (2026-07-03, task #1868,
sprint 263, still OPEN): +1 `Commit` (`6fafb42`); +1 `Task` (`1868`,
COMPLETED). Closed the F2 (`Task` `1857`) follow-up: two more type
families needed the same `EquivalentHash` treatment F2 gave
`IntegerValue`/`FloatValue`. `NodeValue.Equal`/`RelationshipValue.Equal`/
`LazyNodeValue.Equal` (`cypher/expr/value.go`) all treat an
`IntegerValue` carrying the raw ID as equal (the in-pipeline encoding
NodeScan/Expand emit), but `EquivalentHash` had no branch for these
three types — they fell through to their own native ID-bit-fold hash
instead of the shared `hashFloatBits` route, so
`count(DISTINCT [n, id(n)])` returned 2 instead of 1. Added matching
`EquivalentHash` branches for all three (`cypher/expr/equiv.go`).
Separately fixed rmp backlog `Task` `1865`
(pre-existing, tracked, not yet modelled as a graph node — resolved
directly in this commit rather than separately): `cypher/exec/
hash_join.go`'s `canonicalKeyHash` reimplemented the SAME cross-type
numeric fold independently, in the OPPOSITE direction (float→integer
instead of integer→float), so it could disagree with `EquivalentHash`
for a pair where float64 rounding matters, silently dropping a
matching row from a hash join's output instead of erroring.
`canonicalKeyHash` now delegates to `expr.EquivalentHash` directly —
safe because HashJoin discards null/NaN keys before ever calling it,
so `Equal` and `Equivalent` coincide in its actual input domain. The
residual hash-quality trade-off above 2^53 (a bounded, measured
collision-chain slowdown, not unbounded, further bounded by `Task`
`1867`'s new caps) is documented as mathematically unavoidable — any
hash consistent with `Equal`'s own `float64(a)==float64(b)`
comparison must lose the same information — rather than fixed
further. Edges: `Sprint 263 -[CONTAINS]-> Commit`; `Task 1868
-[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->` Feature `Cypher
Engine` (id 12659); `Commit -[TOUCHES]->` Packages `cypher` (73),
`cypher/exec` (39), `cypher/expr` (141). Feature and all three
Packages re-stamped to gitDate 2026-07-03. Certified by
cypher-expert-consultant (CERTIFIED WITH FOLLOW-UPS — independently
re-derived every proof, reverted the fix to confirm the new tests
fail correctly against the pre-fix code, then restored it; verified
the `canonicalKeyHash` delegation holds unconditionally including for
list/map join keys; surfaced a new, unrelated, NOT-YET-FIXED finding:
`MapValue.Equal`'s Go-map-iteration-order nondeterminism, see the next
entry) and go-developer (APPROVED WITH FOLLOW-UPS — explicit
merge-as-is, only cosmetic style notes). 6 new tests across
`cypher/expr/equiv_test.go`, `cypher/aggregate_equivalence_test.go`,
`cypher/exec/hash_join_test.go`. TCK 3897/3897 held, `-race` clean.
Local commit, not pushed.

Incrementally synced at commit `224a1cc` (2026-07-03, task #1876, new
backlog task added to sprint 263 and closed the same session): +1
`Commit` (`224a1cc`); +1 `Task` (`1876`, COMPLETED). Fixed the
`MapValue.Equal` finding the `Task` `1868` review surfaced:
`MapValue.Equal` (`cypher/expr/value.go`) returned `Null` as soon as
it saw the first `Null`-yielding entry comparison while iterating the
map via a plain `range` loop — Go's map iteration order is randomized
per call, so a map pair containing BOTH a `Null`-yielding entry and a
`FALSE`-yielding entry could nondeterministically compare as `Null`
or `BoolValue(false)` across different calls on the identical inputs,
depending purely on visit order. openCypher's compound List/Map
equality is a three-valued-logic conjunction (CIP2016-06-14) where a
definitive `FALSE` always wins over a `NULL` regardless of order —
exactly the pattern the sibling `ListValue.Equal` already implements
correctly via its `sawNull` tracking (it iterates an ordered slice by
index and never had this exposure). `MapValue.Equal` now tracks
`sawNull` the same way. `MapValue.Hash` was independently checked and
found already safe (pure XOR accumulation, provably order-independent).
Verified the fix and its test are non-vacuous by reverting just the
implementation change and confirming the new 200-trial test failed
within the first two trials, then restoring it. Edges: `Sprint 263
-[CONTAINS]-> Commit`; `Task 1876 -[IMPLEMENTED_IN]-> Commit`; `Commit
-[FIXES]->` Feature `Cypher Engine` (id 12659); `Commit -[TOUCHES]->`
Package `cypher/expr` (141); `Task 1868 -[FOLLOWED_BY]-> Task 1876`.
Feature and Package re-stamped to gitDate 2026-07-03. Certified against
CIP2016-06-14 (List/Map equality defined as a conjunction of a
size/key-set check with pairwise entry equality) and the current Neo4j
Cypher Manual, with no blocking findings — the openCypher TCK's own
`Comparison1.feature` map-equality scenarios never combine a
definitively-false entry with a null one in the same pair, a genuine
coverage gap this fix's own test closes independently of the TCK gate.
No new label or edge type. TCK 3897/3897 held, `-race` clean, full
module `go test ./...` green. Local commit, not pushed.

Incrementally synced at commit `c44f3bb` (2026-07-03, task #1869, sprint
263, still OPEN): +1 `Commit` (`c44f3bb`); +2 `Task`s (`1869` COMPLETED,
`1872` SPRINT — pre-existing task, not previously modelled, whose scope
was broadened by this review rather than a brand-new ticket spawned by
it). Fixed the MEDIUM finding from the round-2 audit: every DDL
statement (CREATE/DROP INDEX/CONSTRAINT) applies its real, interruptible
work first — a backfill scan, then the WAL append and fsync — then
constructs its returned `*Result` by draining a trivial, always-
immediate confirmation row (`exec.NewArgument`) via `emptyDDLResult`,
which reused the caller's original query `ctx` for that drain. A
cancellation landing in the arbitrarily small window between the real
work settling and the confirmation row draining was observed via
`Result.Err()` as `context.Canceled` on a statement that had already
durably committed — measured 53/60 trials for CREATE INDEX cancelled
near its commit boundary. `emptyDDLResult` (`cypher/api.go`) now takes
no `ctx` parameter at all and always drains through
`context.Background()`: by the time any caller reaches it the DDL's
outcome is already settled (committed, or a genuine IF NOT EXISTS/IF
EXISTS no-op with nothing ever pending), and the wrapped
`exec.NewArgument` operator can never block, so there is no liveness
reason left to honour cancellation there. `runDDLOp`'s own duplicate
inline construction of the same confirmation shape now delegates to
`emptyDDLResult`; `createBTreeIndexLocked`'s now-dead `ctx` parameter
was removed outright, with its two callers updated. Genuinely-
cancellable DDL work (the hash-index backfill) is unaffected — it still
observes the real `ctx` directly. New regression test
`cypher/ddl_cancel_after_commit_test.go` runs 60 CREATE INDEX trials
with a self-calibrating cancellation-delay spread (measured against a
freshly-timed baseline so it stays valid under `-race` instrumentation);
verified non-vacuous by reverting just the fix and confirming 17-29
false positives per 60 trials across three separate verification passes,
then 0/60 with the fix restored across 300+ combined trials. Edges:
`Sprint 263 -[CONTAINS]-> Commit`; `Task 1869 -[IMPLEMENTED_IN]-> Commit`;
`Commit -[FIXES]->` Feature `Cypher Engine` (id 12659); `Commit
-[TOUCHES]->` Package `cypher` (73); `Task 1869 -[FOLLOWED_BY]-> Task
1872` (reusing the existing edge type for a review-surfaced follow-up,
here a scope EXPANSION of an already-tracked task rather than a new one
— the same edge type covers both shapes by design, per the #1866→#1875
precedent). Feature and Package re-stamped to gitDate 2026-07-03 (via
`graph update`, since `graph create` cannot carry a `SET` clause).
Certified by two independent specialist reviews, both with zero required
changes: one traced the exact real drain timing through the eager
zero-column result path, independently verified `exec.NewArgument`
cannot block by reading its implementation, confirmed no other DDL path
shares this bug class, confirmed concurrent-DDL writer-serialisation
locking is untouched, and surfaced two adjacent but DIFFERENT-class
findings — the btree index backfill and the CREATE CONSTRAINT NOT NULL
validation scan (`scanLabelProperty`) poll no cancellation at all, a
missing-liveness gap rather than a false-positive-cancellation one —
folded into `Task 1872`'s scope (previously scoped only to the hash-
index backfill's poll granularity) instead of blocking this fix. The
other review verified the API-shape change matches the codebase's own
convention for deleting dead parameters and approved the new test's
self-calibrating design as the right call given no cheaper deterministic
test seam exists for this race window; one optional wording precision
suggestion was applied to `emptyDDLResult`'s doc in the same commit. TCK
3897/3897 held, full-module `-race` clean (all packages). Local commit,
not pushed.

Incrementally synced at commit `a92bae4` (2026-07-03, task #1870, sprint
263, still OPEN): +1 `Commit` (`a92bae4`); +1 `Task` (`1870`, COMPLETED).
Closed a doc-faithfulness finding from the same round-2 audit (doc-only,
no behaviour change): `Engine.RunInTx`'s exported godoc
(`cypher/api.go`) falsely claimed the in-memory implementation has no
rollback support and that a failed write's partial mutations remain in
the graph — stale since the undo-log mechanism (task #1282) landed.
Rewrote the doc to describe the real mechanism: mutations apply eagerly
with the inverse recorded into an in-memory undo log; on a pipeline
drain error, a commit-time NOT NULL violation, a WAL fsync failure, or a
pipeline panic, the log replays in reverse inside the write visibility
barrier, fully restoring the pre-statement graph state before the
barrier ever releases, so a concurrent `lpg.Graph.View` reader can never
observe partial state — the same mechanism `ExplicitTx` shares, per
`exectx.go`'s own "Atomicity and the undo log" section. Edges: `Sprint
263 -[CONTAINS]-> Commit`; `Task 1870 -[IMPLEMENTED_IN]-> Commit`;
`Commit -[FIXES]->` Feature `Cypher Engine` (id 12659); `Commit
-[TOUCHES]->` Package `cypher` (73), matching the #1861/#1862 precedent
of `FIXES` (not `IMPROVES`) for a corrected false/stale doc claim.
Feature and Package re-stamped to gitDate 2026-07-03. Certified by a
storage-focused review that traced every claim in the new text —
eager mutation, the undo log's existence and reverse-replay, all
rollback triggers, the fsync-before-visibility ordering, and the
visibility-barrier mutex semantics — against the actual code, function
by function; one non-blocking completeness note (the initial draft's
trigger list of three omitted the real fourth, panic-driven rollback)
was folded into the same rewrite before closing. No new label or edge
type. TCK 3897/3897 held (re-run and confirmed explicitly). Local
commit, not pushed.

Incrementally synced at commit `73bdaab` (2026-07-03, task #1871,
sprint 263, still OPEN): +1 `Commit` (`73bdaab`); +4 `Task`s (`1871`
COMPLETED; `1877`/`1878`/`1879` BACKLOG). Closed the last MEDIUM finding
from the round-2 audit: `buildEdgeTypeFilter` rebuilt its edge-type
filter map via a full O(V+E) graph scan on every query execution
regardless of selectivity — an 8-row-selective query out of 960,000
possible edges cost the same as an unfiltered full scan. A
graph-theory-expert design consultation (surveying Neo4j/JanusGraph/
Dgraph's relationship-type-filtering strategies) preceded
implementation. Fix: (1) `buildEdgeTypeFilter` (`cypher/api.go`) no
longer builds its own internal forward CSR — it now takes the caller's
already-built one (every one of its 4 call sites already has one for
its own traversal), removing one full redundant O(V+E) pass; (2) a new
bounded LRU, `edgeTypeFilterCache` (new file
`cypher/edge_type_filter_cache.go`), caches the filter map keyed by
(canonicalised relationship-type set, a new `lpg.Graph.TopoGeneration`
monotonic counter bumped inside the existing `IncrEdgesAdded`/
`IncrEdgesRemoved`/`DecrEdgesAdded`/`DecrEdgesRemoved`), so a repeat
query against an unchanged graph hits the cache instead of rebuilding.
A concurrency-architect review of this design found a real gap the
above missed: a caller holding `store/txn.Store` directly — bypassing
the Cypher engine's adapters entirely, the same pattern
`examples/24_social_network_cli`/`examples/25_software_house_api`
already use for seeding — could shift an edge's CSR position via a
direct `Tx.AddEdge`/`Commit` without ever bumping the generation. Fixed
in the same cycle: `store/txn/txn.go`'s `applyOp` now also bumps it, via
a new, narrowly-scoped `Graph.BumpTopoGeneration` kept deliberately
separate from the Cypher-statement-scoped TCK side-effect counters,
gated on `AddEdgeHIfAbsent`'s own `inserted` signal where available to
avoid a pointless double-bump on a Cypher-driven write's already-eager
replay. Measured (`bench/cypher_scale`, extended with 8 new `:MENTORS`
edges as a deliberately rare type among ~960k `:KNOWS` edges, benchstat
n=10, p&lt;0.005): a repeated selective query drops from ~190ms/2.16M
allocs per call to ~30ms/336K allocs per call; a query immediately
after any edge mutation still pays one full rebuild, down from two —
an amortised steady-state win, not a complexity-class change for a
cold query, confirmed independently by both the design-time consult
and an empirical rust-perf-engineer profiling pass. Task #1871's own
`rmp` acceptance criteria was revised to state this precisely rather
than the originally-filed "proportional to selectivity, not graph
size" wording, which both specialists independently proved false as
literally written — an unusual departure from normal practice, made
necessary by that proof rather than mere difficulty. The true
output-sensitive redesign this would require is filed separately as
`Task` `1879` (EPIC, explicitly gated on measured real-workload need).
Two more follow-ups filed, not fixed here: `Task` `1877` (a cheaper
cold-path allocation win — `graph/lpg.EdgeLabelsByID`'s allocating
result slice accounts for 87.5% of the cold path's allocations per a
memory profile, fixable by routing through the existing callback-based
`ForEachEdgeLabelByID`) and `Task` `1878` (a genuinely pre-existing,
TCK-uncovered bug found incidentally: an empty backtick-quoted
relationship type matches every edge instead of none — unrelated to
and not introduced by this fix). Also fixed incidentally (comment/
assertion rewording only, zero behaviour change): two unrelated
pre-existing golangci-lint gocritic false positives in
`graph/io/csv/fieldguard_test.go` and
`store/wal/symlink_escape_test.go`. Edges: `Sprint 263 -[CONTAINS]->
Commit`; `Task 1871 -[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->`
Feature `Cypher Engine` (id 12659); `Commit -[TOUCHES]->` Packages
`cypher` (73), `graph/lpg` (448), `store/txn` (70), `graph/io/csv`
(380), `store/wal` (249) — the last two for the incidental test-only
lint fixes, matching the #1863 precedent of a `TOUCHES` edge for a
test-only touch; `Task 1871 -[FOLLOWED_BY]->` `Task`s `1877`/`1878`/
`1879`. Feature and all five Packages re-stamped to gitDate 2026-07-03.
19 new tests, 2 new benchmarks. Certified by three independent
specialist reviews (performance, concurrency, Go idiom) — the
concurrency review is the one that found and specified the
`store/txn` fix above; all findings addressed before this commit. No
new label or edge type. TCK 3897/3897 held, full-module `-race` clean
(all packages, including the two example programs exercising the
`store/txn`-direct-write pattern this fix addresses). Local commit,
not pushed.

Incrementally synced at commit `f251bf8` (2026-07-03, task #1872,
sprint 263, still OPEN): +1 `Commit` (`f251bf8`); `Task` `1872`
COMPLETED (already existed as a `SPRINT`-status node from #1869's
scope-expansion sync — see the `⚠️ MERGE gotcha` note below for a real
duplicate-node bug this exposed and how it was fixed). Closed the
last three findings from task #1869's own follow-up review: (1) the
parallel hash-index backfill's poll check used the shared slice's
absolute index, not one relative to each worker's own range start,
leaving 5 of 10 workers in the audit's own 20,000-row/10-worker
scenario unable to see an early cancellation at all — fixed by
extracting the predicate into a named, directly testable function,
`shouldPollWorkerRelative(i, lo)`, computing `(i-lo)&mask` instead of
the absolute `i&mask`; (2) `backfillNodeBTreeIndex`/
`backfillNodeBTreeIndexNumeric` took no `ctx` at all and polled no
cancellation — both now do, every 4096 rows; (3) `scanLabelProperty`
(CREATE CONSTRAINT's pre-existing-data validation, both UNIQUE and NOT
NULL) took no `ctx` at all — now polls every 4096 rows via a
named-return early-exit from inside its existing read-barrier closure.
All three still run strictly before their statement's
registration/commit step, which remains uncancellable once reached, so
a mid-scan cancellation aborts cleanly with nothing registered and
does not regress task #1869's own post-commit cancellation-reporting
fix. Two independent specialist reviews (concurrency, Go idiom) both
independently ran the SAME mutation-test experiment (reverting the
per-worker poll formula in the live source) and found the identical
gap: the initial regression test proved the formula correct only in
the abstract, disconnected from the real code the fix changed — closed
by the `shouldPollWorkerRelative` extraction above, then re-verified
non-vacuous by reverting that real function's body and confirming the
dependent tests fail. Edges: `Sprint 263 -[CONTAINS]-> Commit`; `Task
1872 -[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->` Feature `Cypher
Engine` (id 12659); `Commit -[TOUCHES]->` Package `cypher` (73); `Task
1869 -[FOLLOWED_BY]-> Task 1872` (re-pointed from the deleted stale
node — see below). Feature and Package re-stamped to gitDate
2026-07-03. 8 new tests. No new label or edge type. TCK 3897/3897
held, full-module `-race` clean. Local commit, not pushed.

**⚠️ MERGE gotcha discovered and fixed during this sync — a real Cypher
trap, not specific to this graph's guard-rails.** `Task` `1872` already
existed (created during #1869's sync, `status:'SPRINT'`, stamped with
the commit current at that time). Syncing its completion via `MERGE (t:
Task {id:1872, status:'COMPLETED', ..., gitCommit:'f251bf8', ...})` —
supplying the FULL, now-different property set in the MERGE pattern
— matched against NOTHING (the existing node's `status`/`gitCommit`/
`gitDate` differ), so MERGE correctly-per-spec created a SECOND,
DUPLICATE `Task {id:1872}` node rather than updating the first — the
exact classic MERGE pitfall (matching the whole pattern including every
property, not a "primary key") this graph's own `knowledge-authority`
skill has flagged as a top Cypher misconception elsewhere in this
project's practice. Caught by a post-write hygiene check (`MATCH
(t:Task) WITH t.id AS tid, count(t) AS c WHERE c > 1 RETURN tid, c`).
Fixed by: re-pointing the stale node's one incoming edge (`Task 1869
-[FOLLOWED_BY]-> Task 1872`) onto the new, `COMPLETED` node, then
`DETACH DELETE`-ing the stale node. **Lesson for every future
completion sync**: when a `Task` node might already exist from an
earlier partial sync (a scope-expansion, a `FOLLOWED_BY` follow-up
stub, etc.), `MATCH` it by `id` ALONE first to check for a property
mismatch before a blind full-property `MERGE`, or use `graph update`'s
`SET` on a plain `id`-keyed `MATCH` instead of `graph create`'s `MERGE`
for what is really an update, not a creation.

Incrementally synced at commits `d0ce7d6` + `f2cad22` (2026-07-03,
tasks #1873 + #1881, sprint 263, still OPEN — only task #1874 remains):
+2 `Commit`s; +3 `Task`s (`1873` COMPLETED, `1881` COMPLETED, `1880`
BACKLOG). Closed the last MEDIUM/LOW-severity concurrency finding
from the round-2 audit: `Checkpointer.RunCheckpoint`'s godoc
falsely claimed it was safe to interleave with `Trigger`/loop runs
because every run serialises on the same commit lock — false, since
phase 2 (the dominant-duration snapshot write) is deliberately
lock-free by design, so two checkpoints in flight at once can collide
on disk with a real filesystem error. The doc now explicitly forbids
this usage and explains why real mutual exclusion was deliberately
not added instead (it would still mask caller misuse as a merely slow
checkpoint, couple the loop's stop responsiveness to a stranger's
potentially multi-second call, and add a synchronisation path with
zero current callers to exercise it). A second, independent defect in
the same code: `setErr` unconditionally overwrote `Stats().LastError`
on every call, so an attempt that started earlier but completed later
could mask a more recently started attempt's already-recorded
outcome. Fixed with zero new locking: one monotonic sequence number
minted per attempt at the start of `runNonBlocking`, threaded to
every `setErr` call site, which now rejects a write whose sequence
number is lower than the one already recorded.

During the full-module `-race` validation for that fix, its own
correctness (no longer silently masking errors) surfaced a SEPARATE,
genuinely pre-existing, fail-stop-safe race: `store/snapshot`'s
`WriteLabels`/`WriteProperties` each capture their label/property-key
registry's name table via a lock-free, point-in-time snapshot, then
separately walk the LIVE graph's node/edge labels/properties with no
lock spanning both reads — a name/key interned strictly in between is
visible to the live walk but absent from the captured table, correctly
detected and rejected (aborting that one checkpoint attempt cleanly,
nothing written or truncated; the next tick retries and normally
succeeds) rather than silently mis-recorded. Two stale, FALSE doc
comments per file (four total) had claimed a registry RLock spans both
reads, serialising against concurrent mutation — no such lock exists
for either `LabelRegistry` or `PropertyKeyRegistry`, both lock-free
copy-on-write structures; all four corrected. This intermittently and
harmlessly tripped `store/db_test.go`'s
`TestDBClose_NoLeak_RejectsSubsequentAppend` (confirmed via a dedicated
investigation to be unrelated to and not caused by the `setErr` fix),
whose assertion was narrowed to check specifically for the WAL-close-
ordering symptom it exists to catch, mirroring its own sibling negative
test's already-established pattern. The deeper structural fix (a
self-healing registry capture, or a two-pass collection) is filed
separately as `Task` `1880` (BACKLOG, unscheduled — touches core
snapshot-serialisation code shared by every checkpoint and recovery,
needs its own design consultation). Edges: `Sprint 263 -[CONTAINS]->`
both `Commit`s; `Task 1873/1881 -[IMPLEMENTED_IN]->` their respective
`Commit`; both `Commit`s `-[FIXES]->` Features `ACID Transactions` (id
9736) and `WAL & Recovery` (id 11553); `Commit d0ce7d6 -[TOUCHES]->`
Package `checkpoint` (181); `Commit f2cad22 -[TOUCHES]->` Packages
`snapshot` (147) and `store` (11138); `Task 1873 -[FOLLOWED_BY]-> Task
1880` and `Task 1881 -[FOLLOWED_BY]-> Task 1880`. Both Features and all
three Packages re-stamped to gitDate 2026-07-03. Certified by two
independent specialist reviews for #1873 (durability/reliability,
Go idiom) — a `go doc`-verified word-wrap rendering defect and a
comment-ambiguity nit were fixed before closing. No new label or edge
type. TCK 3897/3897 held, full-module `-race` clean across repeated
runs. Sprint 263 ("Audit remediation Round 2 2026-07-02") remains
OPEN with one task left: #1874 (doc-precision + benchmark-hygiene
cleanup bundle). Unscheduled backlog now also carries #1875 and
#1877-1880 forward. Local commits, not pushed.

Incrementally synced at commit `2131689` (2026-07-03, task #1874,
sprint 263 — CLOSED): +1 `Commit`; +1 `Task` (`1874` COMPLETED). Closed
the last of the four LOW-severity, doc/fixture-only findings from the
round-2 audit, with zero production behaviour change: (1) narrowed
`CountTriangles`' over-count wording from "self-loop or parallel edge"
to parallel-edge-only, since the rank filter (pairs neighbours only
with strictly higher rank than the current vertex) makes a self-loop
structurally incapable of ever triggering it — a self-loop's neighbour
is the vertex itself, which can never outrank itself; (2) corrected
`bellmanFordCore`'s SPFA attribution, verified via web research (not
memory) to have been genuinely wrong — "Bannister-Eppstein 2012"
(arXiv:1111.5414) describes an unrelated randomized full-pass
Bellman-Ford variant benchmarked against Yen 1970, not SPFA's
worklist/queue mechanism — replaced with the historically accurate
Moore 1959 (queue-based technique) / Duan 1994 (SPFA name and
independent rediscovery) / Bertsekas 1993 (SLF deque heuristic); (3)
switched `bench/cypher_alloc`'s shared `gate500` benchmark fixture
from `newWalker(500)` (NodeIDs 0-499, roughly half inside Go's
`staticuint64s` free small-int boxing range) to
`newWalkerFrom(gateAllocNodeIDStart, 500)`, so its `Benchmark*`
functions — distinct from the `TestZeroAlloc_*` gates task #1863
already fixed in sprint 262 — stop silently discounting about half
their reported allocs/op; verified empirically, allocs/op rose to a
consistent ~1.03/node across all four benchmarks after the fix; (4)
corrected `docs/benchmarks/cypher-scale.md`'s attribution of
`BenchmarkExpand1Hop`'s cost from "P1 and P2" to P1-only, per the
round-2 audit's own controlled A/B measurement (`buildRelationshipValueFromRow`,
P2's specific mechanism, only fires when a relationship variable is
bound, which that benchmark's query never does). Edges: `Sprint 263
-[CONTAINS]-> Commit`; `Task 1874 -[IMPLEMENTED_IN]-> Commit`. No
`FIXES`/`TOUCHES` edge to any Feature or Package, matching this
graph's established precedent for pure doc-fidelity fixes (`Task`s
`1861`/`1862`/`1864`/`1881`): nothing of behavioural substance changed
for a Feature or Package to be stamped against. Certified by an
independent go-developer review (ran the full validation pipeline
itself rather than trusting the change description) — GO, with one
cosmetic nit applied before commit (reworded an ALL-CAPS emphasis in
`triangles.go`'s doc that didn't match this codebase's godoc
conventions) and one left by design (a deliberately mixed citation
style, quoted-title vs. journal-name, reflecting what could and could
not be verified in English). No new label or edge type. TCK 3897/3897
held, full-module `-race` clean. **Sprint 263 ("Audit remediation
Round 2 2026-07-02") is now CLOSED — all 11 tasks COMPLETED**
(`1866`-`1874`, `1876`, `1881`), closing out the entire round-2
production-readiness audit remediation effort. Unscheduled backlog
carried forward: `1875`, `1877`, `1878`, `1879`, `1880`. Local
commits, not pushed.

Incrementally synced at commits `9d88478`..`fe80308` (2026-07-03, sprint
264 — "Prod-readiness audit remediation R3", CLOSED): +1 `Sprint` (`264`);
+12 `Commit`; +9 new `Task` (`1882`-`1889`, `1865`) plus `1878`/`1880`
re-stamped COMPLETED, and `1890` filed BACKLOG (a #1880 test-depth
follow-up). This round remediated a fresh 6-specialist multi-dimensional
production-readiness audit (Functional/Cypher, Functional/Algorithms,
Performance, Reliability/ACID, Concurrency, Security). Three MEDIUM
blockers fixed: **#1884** (`9d88478`) `search/yen.go` `buildEdgeIndex` now
keys on the minimum-weight parallel edge so Yen's k-shortest paths are
correct on weighted multigraphs; **#1882** (`a53cad2`) `store/snapshot`
`ApplyCSRToGraph` rejects a forged CSR `WeightSize` mismatch (was an
index-out-of-range panic at store-open); **#1883** (`de0823b`)
`store/wal` `embedsValidFrame` bounds its CRC scan to defeat an
O(len²) recovery-hang DoS on a crafted WAL. Five LOW defense-in-depth
alloc/recursion bounds: **#1885** (`35dd706`) index `idCount` bound
`len(body)/8`; **#1886** (`2e4b942`) snapshot `readLenPrefixedValue`
value-alloc bound; **#1887** (`f82ddb3`) GraphML per-element `<data>`
cap; **#1888** (`219ae67`) JSONL nested-list depth cap + CSV `MaxBytes`
doc; **#1889** (`9a7718d`) WAL `readFramePayload` alloc bound (the WAL
sibling of #1886). Two conformance/reliability items closed: **#1878**
(`04a075b`+`3e44878`+`fe80308`) rejects empty relationship-type / node-label
names at every Cypher pattern site (an empty backtick type previously
collided with the exec no-filter sentinel and matched every edge); **#1880**
(`65f390f`+`ef658c6`) makes the snapshot registry-capture race self-heal
via a bounded retry instead of aborting the checkpoint. `1865` closed as
already-fixed at HEAD. Edges: `Sprint 264 -[CONTAINS]-> Commit` (12);
`Commit -[FIXES]-> Feature` (Persistence Backends ×6, Cypher Engine ×3,
WAL & Recovery ×3, Search & Path-finding ×1); `Task -[IMPLEMENTED_IN]->
Commit` (12). No new label or edge type. Every fix specialist-certified
GO (graph-theory / storage-engine-auditor / concurrency-architect /
cypher-expert / security-researcher), each with a proven non-vacuous
regression gate. TCK 3897/3897 held throughout; full-module `-race`,
tagged crash-injection battery, `golangci-lint` (0 issues) and
`govulncheck` all green. Local commits, not pushed.

---

## Node labels

| Label | Meaning | Properties (beyond `gitCommit`, `gitDate`) |
|---|---|---|
| `Package` | A Go package (one per source directory). | `name` (package clause), `path` (repo-relative dir, `"."` for root), `importPath` (full), `kind` |
| `Type` | A `type` declaration. | `name`, `pkg` (importPath), `file` (repo-relative), `kind`, `exported` (bool), `generic` (bool) |
| `Function` | A top-level `func` with no receiver that is not a Test/Benchmark/Fuzz/Example. | `name`, `pkg`, `file`, `exported`, `generic` |
| `Method` | A `func` with a receiver. | `name`, `pkg`, `file`, `recv` (receiver type, `*` stripped), `exported` |
| `Test` | A `func TestXxx(*testing.T)`-style function (name prefix `Test`). | `name`, `pkg`, `file` |
| `Benchmark` | A `func BenchmarkXxx` (name prefix `Benchmark`). | `name`, `pkg`, `file` |
| `FuzzTarget` | A `func FuzzXxx` (name prefix `Fuzz`). | `name`, `pkg`, `file` |
| `Example` | A runnable godoc `func ExampleXxx` (name prefix `Example`). | `name`, `pkg`, `file` |
| `Spec` | A documentation/specification file under `docs/` (plus root `README.md`/`CHANGELOG.md`). | `name` (basename), `path` (repo-relative), `title` (first `# ` heading) |
| `Feature` | A curated major capability of the module. | `name`, `description` |
| `Sprint` | A planning sprint from the `rmp` roadmap. | `id` (int), `name`, `status` (`OPEN`\|`CLOSED`\|`PENDING`), `objective` |
| `Commit` | A git commit that delivered one or more tasks. | `hash` (short 7-char), `fullHash` (full 40-char), `message`, `sprintId` (int) |
| `Agent` | A specialist sub-agent mandated by `CLAUDE.md`. | `name`, `kind` (`subagent`), `description`, `source` |
| `Skill` | A project-relevant Claude Code skill. | `name`, `kind` (`skill`), `description`, `path` |
| `Memory` | A persistent assistant memory file (mirror of the harness memory directory). | `name` (frontmatter slug), `file` (basename), `type` (`user`\|`feedback`\|`project`\|`reference`), `description` |

### Enumerated property values

- `Package.kind` ∈ `library` \| `example` \| `internal` \| `command` \| `bench`.
- `Type.kind` ∈ `struct` \| `interface` \| `alias` (i.e. `type A = B`) \| `signature`
  (function type) \| `defined` (any other named/defined type).

### Counts by label (commit `567253c` + worktree, 2026-06-11)

| Label | Count |
|---|---|
| `Test` | 4159 |
| `Method` | 3390 |
| `Function` | 2925 |
| `Type` | 975 |
| `Example` | 120 |
| `Benchmark` | 105 |
| `Package` | 93 |
| `Spec` | 30 |
| `Memory` | 26 |
| `Feature` | 16 |
| `Commit` | 14 |
| `Agent` | 5 |
| `FuzzTarget` | 5 |
| `Skill` | 2 |
| `Sprint` | 2 |

---

## Edge types

All edges carry `gitCommit` and `gitDate`.

| Type | Endpoints | Meaning |
|---|---|---|
| `CONTAINS` | `(Package)-[:CONTAINS]->(Package)` | Directory nesting: parent package → nearest descendant package. |
| `CONTAINS` | `(Package)-[:CONTAINS]->(Type\|Function\|Method\|Test\|Benchmark\|FuzzTarget\|Example)` | A package contains a symbol declared in one of its files. |
| `HAS_METHOD` | `(Type)-[:HAS_METHOD]->(Method)` | A method's receiver type, matched within the same package (`Method.recv == Type.name`). |
| `IMPLEMENTS` | `(Package)-[:IMPLEMENTS]->(Feature)` | A package realises a curated feature (path-prefix rules below). |
| `SPECIFIED_IN` | `(Feature)-[:SPECIFIED_IN]->(Spec)` | A feature is documented in a specification file. |
| `CONTAINS` | `(Sprint)-[:CONTAINS]->(Commit)` | A sprint contains a commit that delivered work within it. |
| `FIXES` | `(Commit)-[:FIXES]->(Feature)` | A commit fixes a bug in (or hardens) a feature area. |
| `IMPROVES` | `(Commit)-[:IMPROVES]->(Feature)` | A commit improves (perf/observability/tests) a feature area without fixing a defect. |
| `TOUCHES` | `(Commit)-[:TOUCHES]->(Package\|Type\|Function\|Spec)` | A commit's diff touched this element; drives provenance re-stamping. |
| `IMPLEMENTED_IN` | `(Task)-[:IMPLEMENTED_IN]->(Commit)` | The commit that delivered a task's work. |
| `DEPENDS_ON` | `(Task)-[:DEPENDS_ON]->(Task)` | A task cannot start/complete until another task (a genuine prerequisite) does. |
| `FOLLOWED_BY` | `(Task)-[:FOLLOWED_BY]->(Task)` | A completed task's work surfaced a distinct, non-blocking follow-up tracked as a new task — NOT a prerequisite (contrast `DEPENDS_ON`). Introduced 2026-07-02 (task #1866 → #1875). |
| `ABOUT` | `(Memory)-[:ABOUT]->(Feature\|Sprint)` | A memory concerns a feature area or sprint. |
| `CONSULTED_BY` | `(Memory)-[:CONSULTED_BY]->(Agent\|Skill)` | A memory exists primarily for that agent's/skill's use. |
| `SPECIALISES_IN` | `(Agent)-[:SPECIALISES_IN]->(Feature)` | A sub-agent's mandated speciality area (curated from `CLAUDE.md`). |

**Data-quality note (observed 2026-07-02, not remediated here):** the live graph has
accumulated several more edge types across incremental syncs than this table documents in
full (`BELONGS_TO`, `PART_OF`, `CLOSES`, `FROM_AUDIT`, `REMEDIATED_BY`, `CONCERNS`,
`DELIVERS`, `FOUND`, `TESTS`, `LIVES_IN`, `RELEASES`, `IMPLEMENTED_BY`, `DOCUMENTED_BY`,
`DELIVERED_BY`, `HARDENED_IN`, `MODIFIES` — 29 distinct types total per a live `MATCH ()
-[r]->() RETURN type(r), count(r)` query, vs. the ~11 documented above). This table was
extended just enough to cover the edge types touched by this sync (`IMPLEMENTED_IN`,
`TOUCHES`, `DEPENDS_ON`, `FOLLOWED_BY`, `IMPROVES`); a full reconciliation of the remaining
~16 types is a separate hygiene task, not part of this commit's sync.

### Counts by edge type (commit `567253c` + worktree, 2026-06-11)

| Type | Count |
|---|---|
| `CONTAINS` | 11792 |
| `HAS_METHOD` | 3391 |
| `IMPLEMENTS` | 87 |
| `ABOUT` | 36 |
| `FIXES` | 24 |
| `SPECIFIED_IN` | 19 |
| `SPECIALISES_IN` | 6 |
| `CONSULTED_BY` | 5 |

### Memory layer (hybrid, approved 2026-06-11)

The `Memory` nodes mirror the persistent memory files in the Claude Code project memory
directory — the **files remain canonical** for the harness (`MEMORY.md` is the loaded
index); the graph adds the queryable relational layer (what a memory is about, who
consults it). When a memory file is created, renamed, or deleted, the mirroring `Memory`
node must follow in the same change. `Agent` nodes are the specialist sub-agents mandated
by `CLAUDE.md`; `Skill` nodes are the project's own Claude Code skills
(`knowledge-authority`, `roadmap-manager`).

---

## Feature taxonomy

The 16 curated `Feature` nodes (a deliberately small, reviewed set — not auto-derived):

`Core Graph Model`, `Search & Path-finding`, `Persistence Backends`, `WAL & Recovery`,
`ACID Transactions`, `Cypher Engine`, `openCypher TCK Compliance`, `Bolt Protocol`,
`Data Structures`, `Benchmarking & Profiling`, `Production-Readiness Test Battery`,
`Stable Element Identity`, `Observability & Metrics`, `CLI Tooling`,
`Examples & Tutorials`, `Release & Versioning`.

### `IMPLEMENTS` mapping rules (package path → feature)

A package maps to features by its repo-relative directory prefix (a package may map to
several features):

| Path prefix | Feature(s) |
|---|---|
| `graph` | Core Graph Model |
| `search` | Search & Path-finding |
| `cypher/tck` | openCypher TCK Compliance **and** Cypher Engine |
| `cypher` (other) | Cypher Engine |
| `store/wal`, `store/recovery` | WAL & Recovery **and** ACID Transactions |
| `store/txn` | ACID Transactions |
| `store` (other) | Persistence Backends |
| `bolt` | Bolt Protocol |
| `ds` | Data Structures |
| `bench` | Benchmarking & Profiling |
| `examples/*` | Examples & Tutorials |
| `cmd`, `tools` | CLI Tooling |
| `internal/crashinject` | ACID Transactions **and** Production-Readiness Test Battery |
| `internal/testlayers` (or any `crashinject`) | Production-Readiness Test Battery |

Packages outside these prefixes (e.g. assorted `internal/*` helpers) implement no feature
and have no `IMPLEMENTS` edge — this is expected, not a defect.

### `SPECIFIED_IN` mapping (feature → spec path)

| Feature | Spec(s) |
|---|---|
| Cypher Engine | `docs/cypher.md` |
| openCypher TCK Compliance | `docs/cypher.md` |
| ACID Transactions | `docs/acid-audit.md`, `docs/isolation-design.md` |
| WAL & Recovery | `docs/persistence.md`, `docs/csrfile-v1.md` |
| Persistence Backends | `docs/persistence.md`, `docs/io.md` |
| Search & Path-finding | `docs/algorithms.md` |
| Bolt Protocol | `docs/bolt.md` |
| Benchmarking & Profiling | `docs/profiling.md`, `docs/optimisations.md` |
| Production-Readiness Test Battery | `docs/test-battery.md`, `docs/test-layers.md` |
| Stable Element Identity | `docs/maxnodeid.md` |
| Observability & Metrics | `docs/metrics.md` |
| Examples & Tutorials | `docs/examples-standard.md` |
| Release & Versioning | `docs/semver.md`, `docs/release.md` |

Some `Spec` nodes (e.g. `docs/tier2.md`) are intentionally unlinked — not every document
maps onto a feature.

---

## ⚠️ Guard-rail gotcha — `set`/`delete`/`remove`/`detach`

`rmp graph` enforces operation-class guard-rails by **scanning the raw Cypher text** for the
write-keywords `SET`, `DELETE`, `REMOVE`, `DETACH` (whole-word, case-insensitive). This trips
on those words appearing **inside string data** — both when writing and when reading:

- `create`/`update`/`delete` reject a query if a forbidden keyword for the wrong class
  appears anywhere, including inside a quoted literal.
- `query`/`search` reject a read whose literals contain `SET`/`DELETE`/`REMOVE`/`DETACH`
  (e.g. `WHERE m.name = 'Delete'` is rejected).

GoGraph's own source is full of such identifiers (`Delete`, `Set`, `RemoveLabel`,
`detach_delete.go`, …). **Workaround: split the keyword with Cypher string concatenation** so
the raw text never contains the contiguous token, while the evaluated value is byte-identical:

```cypher
-- write (creation):
CREATE (m:Method {name:'Dele'+'te', ...})
-- read:
MATCH (m:Method) WHERE m.name = 'Dele'+'te' RETURN m
MATCH (n) WHERE n.file ENDS WITH 'se'+'t.go' RETURN n
```

When querying for symbols whose names contain these tokens, prefer a guard-safe substring
(`CONTAINS 'elete'`, `CONTAINS 'emove'`) or the split-literal form above.

Additionally, `rmp graph create` accepts **only `CREATE`/`MERGE` write clauses** — a real
`SET` clause is rejected (`graph create accepts only CREATE/MERGE queries`), so upserts
must carry every property inline in the `MERGE`/`CREATE` property map. Use the `update`
class for `SET`/`REMOVE` clauses; `UNWIND … MATCH … SET` is accepted there.

---

## Maintenance

### Bootstrap / full rebuild

The graph was materialised from an AST extractor (`go/parser`, stdlib-only) that emits
batched `UNWIND … CREATE` Cypher files; the extractor lives at `/tmp/kgextract.go` (a
throwaway tool, not part of the module) and is run as:

```bash
COMMIT=$(git log -1 --format="%H"); DATE=$(git log -1 --format="%ad" --date=format:"%Y-%m-%d")
go run /tmp/kgextract.go "$PWD" "github.com/FlavioCFOliveira/GoGraph" "$COMMIT" "$DATE" /tmp/kgcypher
for f in $(ls /tmp/kgcypher/*.cypher | sort); do rmp graph create -r gograph < "$f"; done
```

The `q()` helper in the extractor applies the concatenation split described above to every
string value, so creation never trips the guard-rail.

### Post-commit sync

Reconcile only what changed:

```bash
git diff --name-only HEAD~1 HEAD
```

For each changed `.go` file: bump the provenance of its package and surviving symbols,
`CREATE` new symbols (+ `CONTAINS`/`HAS_METHOD`), and `DETACH DELETE` removed ones; refresh
`Feature`/`Spec` provenance when their backing files change. Because the graph is large,
a full rebuild (wipe + re-materialise) is also acceptable and is the simplest way to stay
exactly in sync after broad changes.

---

Incrementally synced at commit `baf4444` (2026-07-16, sprints 284/285/286 —
CREATE/MERGE+SET non-literal gaps + FOREACH): +23 nodes — 9 `Commit`
(`cb6cfbd`,`a0cd733`,`c6c0867`,`86d8ea2`,`c8ea848`,`75e70a3`,`81a929d`,`ba14888`,
`baf4444`); 1 `Feature` `FOREACH clause`; 5 `Type` (`Foreach` in
`cypher/exec`|`cypher/ast`|`cypher/ir`, `MergeSetAllAction` in `cypher/exec`,
`MergeSetAll` in `cypher/ir`); 8 `Test` (one per new regression file under
`cypher/`). +edges: 5 `CONTAINS` (package→new Type), 8 `CONTAINS` (`cypher`
package→new Test), 4 `IMPLEMENTS` (`parser`/`ast`/`ir`/`exec` → `FOREACH clause`),
6 `FIXES` + 3 `IMPROVES` (session commits → `Cypher Engine`). `Package`
provenance bumped on `cypher`/`exec`/`ir`/`ast`/`parser`/`parser/gen`. All work
preserves openCypher TCK 100% (3897) and `go test -race`. Per the "No `TESTS`
edges" rule below, the new tests link via `CONTAINS` only (no Test→Feature edge).
The rmp Task/Sprint layer for these (tasks #2023–#2031, sprints 284–286) was
NOT back-filled — that layer trails the code layer (last full task sync
2026-06-11).

Incrementally synced at commits `c3b4617`..`HEAD` (2026-07-16/17,
production-readiness cycle, sprints 287–291): a 7-specialist audit (security,
ACID/storage, openCypher/TCK, graph algorithms, performance, concurrency,
Go-idiom) drove 20 fixes across four axes. `Commit` nodes for each; provenance
bumped on the affected `Package`s (`cypher`, `cypher/exec`, `cypher/ir`,
`cypher/ast`, `cypher/parser`, `cypher/parser/gen`, `search`, `graph/lpg`,
`graph/adjlist`, `graph/index/btree`, `graph/io/csv`, `graph/io/jsonl`,
`store/txn`, `store/recovery`). Correctness/ACID fixes: whole-entity `SET`
UNIQUE-skip rollback (`set_all.go`, task #2032/A1), string-literal backslash
round-trip (`ast/literals.go`+`create_node.go`, #2033/S1), leading-`FOREACH`
nil-guard (`ir/plan.go`, #2034/CY1), MERGE `$param`-null MergeReadOwnWrites
(#2035/CY2), B+tree height≥4 `contains` recursion + `removeChild` re-root
(`btree/bplus.go`, #2037/D1), NaN/Inf validation for defined float weight types
(`search/weight_validation.go`, #2038/D2), instance-precise multigraph `DELETE r`
by handle with a NEW WAL op `Type` `OpRemoveEdgeByHandle`=23 + `adjlist`/`lpg`
`RemoveEdgeByHandle` + recovery replay + undo `captureRemovedEdgeByHandle`
(#2018, storage-engine-auditor CERTIFIED all ACID pillars), CALL…YIELD…WHERE
filter (#1966), NewEngine undirected-backend warning (#1892), empty-graph index
key-type (#1983), MergePattern parallel-edge multiplicity (#1875),
EvalPatternComp untyped `[r]` per-instance (#2017), `WITH…SET var={…}` ANTLR
disambiguation via grammar regen (#2010), TransitiveClosure reflexive-self
(#2040/G1), CSV comment-prefix + JSONL empty-identifier round-trip (#2042/#2043).
Perf: `IsTombstoned` lock-free via `atomic.Pointer[roaring64.Bitmap]` COW
(`lpg.go`, #2039/C1 — read scaling negative→positive). Cleanup: removed the dead
`ParallelScan` `Type` (#2019); doc reconciliations (#1967, #2041 `store.DB`).
All preserve openCypher TCK 100% (3897/3897) and `go test -race`; every fix
gated through `make ci`. #1827 DEFERRED (depends-on #1712, async checkpoint;
current synchronous publish is crash-atomic). Full graph-store re-sync and the
rmp Task/Sprint layer back-fill still trail the code layer.

Incrementally synced at commits `1c9b079`..`8d66dfa` (2026-07-17,
production-readiness cycle, sprints 292–295, 12 commits): `Sprint` nodes 292–295
and a `Commit` node per commit (with `sprintId`, `TOUCHES`→`Package`,
`CLOSES`→`Sprint`, `TOUCHES`→new `Type`s), provenance stamped `2026-07-17`.
Sprint 292 (actionable backlog): Bolt reassembly buffer charged vs the aggregate
`InboundBudget` (#1891, `bolt/proto`+`bolt/packstream`+`bolt/server`); snapshot
self-heal retry-loop test-depth (#1890, `store/snapshot`); `buildEdgeTypeFilter`
fallback via `ForEachEdgeLabelByID`, −44% allocs (#1877, `cypher`); new `Type`
`LabelCountScan` in `cypher/exec` — O(1) count over a bare label scan, 199,813→30
allocs (#2004); SHOW CONSTRAINTS/INDEXES via the hand-written DDL parser (no
ANTLR) with new `Type`s `ShowConstraints`/`ShowIndexes` (`cypher/ir`) and
`StaticRows` (`cypher/exec`) (#1922); typed zero-box streaming Bolt RECORD encode
(#1838, `bolt/server`); O(N) CREATE CONSTRAINT/INDEX cost doc (#1923). Sprint 293:
deprecate the unbounded `search.KShortestPathsLoopless` foot-gun (#1997/#2006,
`search`). Sprint 294 (**#1704 columnar value-model EPIC, core delivered**): new
`Type`s `Chunk` (P1 #1822), `ColumnarProject` (P2 #1823), `ColumnarFilter` (P3
#1824) in `cypher/exec` — read-path scalar boxing eliminated, `EngReadProject`
5309→89 allocs/op (−98.3%), −61% time, TCK 3897 held incl. temporal; P4 #1825
(entity lazy-columns) and P5 #1826 (bounded-morsel Bolt streaming) DEFERRED —
both require the immutable pinned-snapshot foundation not yet implemented
(`isolation-design.md`; #1525 spike), materialise-at-sink of an id-only entity
column would read the graph outside the `visMu` visibility barrier (ACID
Isolation violation). Sprint 295 (grooming): #2036 keep the deliberate lenient
null-whole-map-param behaviour (documented in `create_node.go`); #2005 closed as
split → new backlog `Task`s #2046 (CALL{}), #2047 (COLLECT{}), #2048 (POINT),
plus #2044 (SHOW YIELD) and #2045 (WITH/aggregation columnar consumption). Both
compliance mandates hold: TCK 3897 green throughout; ACID preserved (P4/P5
deferred precisely to avoid the Isolation violation). Not pushed. Per-symbol
`Function`/`Test` fidelity for the new files trails the code layer (the new
`Type`s and provenance layer are recorded).

Addendum — sprint 296 (2026-07-17, +2 commits, exhausting the developable
backlog): `55d58dc` SHOW CONSTRAINTS/INDEXES `YIELD`/`WHERE`/`RETURN` projection
(#2044, token-neutral: the SHOW tail is re-parsed as a synthetic `CALL __g.s()`
clause to reuse the ANTLR expression grammar; `cypher/ir`+`cypher`); `a5a1772`
columnar scalar-column passthrough for WITH-projection (#2045 Part A —
`Chunk.CopyCellTo`, no sink-time live read, so free of the P4/P5 blocker;
WithFilterPassthrough 5324→118 allocs/op; `cypher`+`cypher/exec`). Part B
(EagerAggregation unboxed grouping-key hashing) split to #2049 — it must
replicate openCypher float64-domain group equivalence, a correctness-critical
TCK-covered path.

Sprint 297 (2026-07-17, +1 commit): `9c7f211` #2049 unboxed columnar
grouping-key hashing in `EagerAggregation` — `hashCellEquivalent`/
`cellEqualToStored` read typed backings unboxed but delegate to the SAME
`expr.EquivalentHash`/`expr.Equivalent` as the boxed path (box only on new-group
creation), so float64-domain group equivalence is byte-identical (1≡1.0, NaN≡NaN,
−0.0≡+0.0, NULL grouped; int ≥2^53 same-bits stay SEPARATE). AggGroupScalar
15,598→1,889 allocs/op (−87.9%). Surfaced a PRE-EXISTING, not-TCK-covered
conformance gap in the shared `expr.Equivalent` cross-type int/float path (lossy
float64 promotion vs CIP2016-06-14 exactness) → filed as #2050 (sign-off-gated
module-wide behaviour change). After sprints 292–297 every remaining backlog
`Task` is un-developable without violating the ACID mandate, reversing an
explicit user decision, or obtaining user sign-off (#2050): ACID/snapshot-blocked
(#1704 P4/P5, #1825, #1826, #1712, #1827), user-deferred (#1714, #1718, portability
#2046/#2047/#2048), design-gated (#1879), or sign-off-gated (#2050).

Sprint 298 (2026-07-17, +1 commit, user sign-off): `7e16d8e` #2050 exact
cross-type int/float equivalence per CIP2016-06-14. `IntegerValue.Equal`/
`FloatValue.Equal` cross-type now route through a shared `intEqualsFloat` that
reuses the pre-existing exact ORDER-BY comparator `cmpInt64Float64` (so `=` agrees
with `ORDER BY`); `Equivalent` inherits it, making every consumer exact — WHERE
`=`/`<>`/`IN`, HashJoin per-bucket equality, DISTINCT, EagerAggregation grouping,
and List/Map element equality. `int(2^53+1)` no longer compares/groups/joins equal
to `float(2^53)`; `1 = 1.0` and `2^53 = 2^53.0` stay true. The float64-domain
grouping/join hash is unchanged (now-unequal large pairs share a bucket resolved
by the exact comparator; the `a≡b ⇒ EquivalentHash(a)==EquivalentHash(b)`
invariant is pinned). TCK 3897 held. After sprint 298 every remaining backlog task
is ACID/snapshot-blocked or deferred by an explicit user decision (the user
accepted those deferrals); nothing developable remains.

Incrementally synced at commit `6aec9cc` (2026-07-25, round-2 planner evaluation
vs Neo4j/Memgraph): +7 nodes — 1 `Audit` (`planner-eval-round2-2026-07-25`, baseline
v0.10.0/`0007214`, report `docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md`, 5 findings,
sprints 311–315, backlog #2162–#2166), 5 `Finding`, 1 `Commit` (`6aec9cc`); +10 edges —
5 `FROM_AUDIT`(`Finding`→`Audit`), 4 `CONCERNS`(`Audit`→`Feature` `Cypher Engine` and
→`Package` `cypher`/`adjlist`/`csr`), 1 `PRODUCED`(`Commit`→`Audit`). All nodes and all
edges provenance-stamped `2026-07-25`. **Two new edge PAIRINGS** (the edge types already
existed, the endpoint combinations did not): `(Audit)-[:CONCERNS]->(Package)` — previously
`CONCERNS` only reached `Feature` — and `(Commit)-[:PRODUCED]->(Audit)` — previously
`PRODUCED` only ran `(Audit)->(Fix)`. The findings are recorded as evidence summaries only;
the committed report is the authority and the `Finding.evidence` property points at it.
Sprint nodes for 311–315 were deliberately NOT created: those sprints are `PENDING` with no
commits, and `rmp` remains the source of truth for planning until they execute — the same
convention applied to sprints 305–310 while pending.

Incrementally synced at commit `7b68feb` (2026-07-26, round-3 exhaustive comparative audit
vs Neo4j 5.26 Community and Memgraph 2.22.0): **+27 nodes** — 1 `Audit`
(`vs-neo4j-memgraph-2026-07-26-r3`, baseline v0.10.0/`6f31f61`, report
`docs/audit-vs-neo4j-memgraph-2026-07-26.md`, 14 findings, sprints 316–321, backlog
#2192–#2202), 14 `Finding`, 9 `Decision`, 2 `Spec`
(`docs/audit-vs-neo4j-memgraph-2026-07-26.md`, `docs/benchmarks/threeway-2026-07-26.md`),
1 `Benchmark` (`TestThreeWay`), 1 `Commit` (`7b68feb`); **+37 edges** — 14 `FROM_AUDIT`
(`Finding`→`Audit`), 9 `MADE_DECISION` (`Audit`→`Decision`), 10 `CONCERNS`
(`Audit`→`Package` ×8 and →`Feature` ×3, de-duplicated), 3 `TOUCHES` (`Commit`→the two
`Spec`s and the `Benchmark`), 1 `PRODUCED` (`Commit`→`Audit`). All provenance-stamped
`2026-07-26`.

This is the first audit round backed by **measurement against the running incumbents**
rather than by reasoning from source and documentation. The `Benchmark` node records the
four-target harness (`bench/comparison/threeway_test.go`, build tag `threeway`) that made
that possible; `Benchmark` had not previously been used for a cross-product comparison.

**New `Decision.kind = 'correction'`** distinguishes a finding about the *project* from a
correction to a *previous audit round*: seven of the nine record round-1 conclusions that
did not survive measurement (frozen-TCK, LOAD-CSV-is-TCK-covered, GDS-has-no-max-flow,
macOS-fsync, best-read-path, #1671/#2051-was-reverted, segmented-WAL-rejection-reversed)
and two record mandate-level rejections (concurrent disjoint writers; vector/ANN and
delta-stepping). Recording overturned conclusions as first-class nodes is deliberate — the
graph must not silently retain a superseded verdict, and a reader arriving at a round-1
claim needs the correction to be reachable from it.

Sprint nodes for 316–321 were deliberately NOT created, and an initial set was deleted
after review to restore the convention: those sprints are `PENDING` with no commits, so
`rmp` remains the source of truth for planning until they execute — the same convention
applied to sprints 305–315 while pending. The sprint range is carried as the
`Audit.sprints` property instead.

The findings are recorded as evidence summaries only; the committed report and the eleven
per-stream reports under `docs/audit-2026-07-26-streams/` are the authority, and
`Finding.evidence` carries the measurement that grounds each one.

Incrementally synced at commit `b22e4dd` (2026-07-27, task #2228, sprint 326 — admit the
hash join for writing statements, #2225 part B): **+16 nodes** — 1 `Commit` (`b22e4dd`),
1 `Sprint` (`326`, OPEN — the first sprint node created for the 322–326 range, because
unlike 316–321 it now has a commit), 3 `Task` (`2228` COMPLETED, `2233` and `2234`
BACKLOG), 1 `Package` (`bench/r4audit`, kind `bench` — the round-4 audit harness package
was missing), 2 `Spec` (`docs/benchmarks/write-path-hash-join-2026-07-27.md` and
`docs/benchmarks/threeway-2026-07-27.md`, the latter also previously missing), 8 `Test`
(`TestHashJoinOrder_SequenceMatchesNestedLoop`; the four `TestWritePathGates_*`, of which
`MinLabelReAnchorsInsideAWrite` and `ResultIdentity` date from part A and had never been
synced; `TestW1PartB_BoundKeyWriteFlatInN` and `TestW1PartB_HashJoinIsWhatChanged`).
**+25 edges** — 1 `CONTAINS` (`Sprint 326`→`Commit`), 1 `IMPLEMENTED_IN` (`Task 2228`→
`Commit`), 1 `IMPROVES` (`Commit`→`Feature` `Cypher Engine`, id 12659), 9 `TOUCHES`
(`Commit`→`Package` `cypher`/`r4audit`/`cypherdocgate`, →`Function` `tryBuildHashJoin`/
`buildPlanWithMutatorFull`/`hashJoinOrderSafe`, →the two `Spec`s), 8 `CONTAINS`
(`Package`→each new `Test`), 3 `TESTS` (the three hash-join tests→`tryBuildHashJoin`),
2 `FOLLOWED_BY` (`Task 2228`→`2233` and →`2234`). All stamped `2026-07-27`.

No new label or edge type. Two new **properties** were added to existing `Function` nodes
rather than introducing a `Const` label for a single declaration:
`tryBuildHashJoin.invariant` and `.diagnosticCounters`, and
`buildPlanWithMutatorFull.writePathGates`. The invariant is the substance of this task:
`hashJoinBuildOnLeft = false` (a named constant now used at the call site instead of a
bare `false`) records that the PLANNER pins build=`apply.Inner` and probe=`apply.Outer` at
the only construction site for either join operator, so the substitution is
**order-PRESERVING** — row-for-row identical to the nested loop, not merely
multiset-identical. That refuted the round-4 premise that a hash join self-selects the
smaller build side, and is why a writing statement needs no order guard. Measured: the
three-way load at 20 000 nodes went **35m10.173s → 2.206s (957×)** — GoGraph-embedded now
loads that dataset faster than Neo4j (4.252s) — and the bound-key write at N=16000 went
**1.860s → 9.669ms (192×)**, within 1.08× of its own read control.

**Known drift (pre-existing, not remediated here):** sprints 319–325 and their tasks are
absent from the graph, and part A's tests (`#2225`, commit range up to `003e1652`) were
never synced — the eight `Test` nodes above include two that belong to part A. The
`Package` node for `bench/r4audit` and the `Spec` node for the round-4 threeway record
were likewise missing and are created here. A full reconciliation of the 319–325 range is
a separate hygiene task.

Incrementally synced at commits `afc6fbf`..`aa8f139` (2026-07-27, tasks #2229 and #2232,
sprint 326): **+11 nodes** — 2 `Commit`, 3 `Task` (`2229` and `2232` COMPLETED, `2235`
BACKLOG), 1 `Spec` (`docs/benchmarks/degree-rewrite-2026-07-27.md`), 2 `Function`
(`recogniseDegreePattern`, `applyDDLOp`), 4 `Method` (`lpg.Graph.OutDegreeByID` /
`.OutDegreeByTypeBoundedByID`, `adjlist.AdjList.OutDegreeFuncBounded` /
`.OutDegreeFuncBoundedByID`). **+18 edges** — 2 `CONTAINS` (`Sprint 326`→each `Commit`),
2 `IMPLEMENTED_IN`, 1 `FIXES` and 1 `IMPROVES` (both →`Feature` `Cypher Engine`, id 12659),
1 `FOLLOWED_BY` (`Task 2232`→`2235`), 8 `TOUCHES` (→`Package` `cypher`, `cypher/expr`,
`graph/lpg`, `graph/adjlist`, `bench/r4audit`, `internal/cypherdocgate`, →the new `Spec`,
→`recogniseDegreePattern`), 1 `CONTAINS` (`Package cypher`→`recogniseDegreePattern`). No
new label or edge type. All stamped `2026-07-27`.

Two properties carry the substance. `recogniseDegreePattern.eligibility` records the port of
Neo4j's `QuerySolvableByGetDegree` + `isEligible` and, decisively, that **`Selections.empty`
makes a labelled far node ineligible for a degree rewrite in Neo4j too** — so
`COUNT { (a)-[:K]->(:P) }`, the round-4 audit's own 88× shape, is served by a different
mechanism (`Task 2235`), never by widening this recogniser. `applyDDLOp.why` records the
`Result`-leak defect found while fixing #2229: four intra-sequence callers discarded the
`Result` the DDL runner returns, and its armed finalizer counted a leak against
`cypher.result.leaked` on every `CREATE CONSTRAINT`, `DROP CONSTRAINT` and constraint unwind.

## Known limitations (faithful, by design)

- **Build-tag duplicates.** The extractor parses every `.go` file regardless of build
  constraints, so a symbol declared once per platform/tag (e.g. `Reader.setHint` in
  `store/csrfile`) appears as multiple nodes that differ only by `file`. This is faithful
  to the source tree. `HAS_METHOD` edges are de-duplicated to one per `(Type, Method)` pair.
- **No `TESTS` edges.** Tests/benchmarks/fuzz/examples are linked to their package via
  `CONTAINS` only; they are **not** linked to the specific function/feature they exercise,
  because that mapping cannot be derived faithfully from the AST without guessing.
- **Curated layers.** `Feature` nodes and the `IMPLEMENTS`/`SPECIFIED_IN` edges are a
  human-reviewed interpretation, not a mechanical extraction; revise the mapping tables
  above when the architecture changes.
