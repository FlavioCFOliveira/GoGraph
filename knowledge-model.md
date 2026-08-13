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

Incrementally synced at commit `7aa302f8` (2026-08-03, task #2304, sprint 334 — MVCC B2,
retire the exclusive visibility barrier: PREREQUISITES LANDED, BARRIER NOT REMOVED):
+1 `Commit` (`7aa302f8`); +5 `Method` on `lpg` `Graph` (`ApplyVersioned`,
`ApplyAtomicallyTx`, `ApplyInsideLockedTx`, `WriterViewOf`, `AmbientWriteTx`);
+1 `Type` `lpg.WriteTx`; +1 `Method` `mvcc.WriteStamp.EndFor`; +4 `Test`
(`TestOverlap_DeferredIndexRemovalChargedToItsOwnTransaction`,
`TestOverlap_TransactionsDoNotStealEachOthersState`,
`TestOverlap_WriterViewOfSeesOwnWriteNotTheOthers`,
`TestWALWriteScalingGate`); +1 `CONTAINS` (Sprint 334 → Commit).

CORRECTION to this entry as first written: `Finding` is **not** a new label — it already
existed, keyed on `name` (e.g. `sec-2026-07-02-r2-snapshot-csr-size`) with `severity`,
`status`, `title`, `task`, `sprint`, `commit`, `date`, `surface` and `rootclass`. The two
nodes created here were initially keyed on an `id` property that no other Finding uses;
they have been reconciled to the existing convention and now carry `name` as well. What IS
new is the edge type `DISCOVERED (Commit)->(Finding)`, and three optional properties for a
finding that was MEASURED but not fixed: `mechanism` (why it happens), `evidence` (the
measurement) and `property` (the ACID or compliance property at stake), plus `fixedIn`,
`fixNote` and `verified` once it is closed. The point of them is that the evidence stays
queryable instead of living only in a commit message.

Two were created, both CRITICAL:

- `mvcc-2026-08-03-b2-ambient-slot-splits-transactions` — status **OPEN**, ACID Isolation,
  owned by rmp #2320. Removing the write barrier splits one statement across two commit
  records, because every Cypher mutator reaches `lpg` through the public API with a nil
  `writeCtx` and resolves the shared commit record from the graph-wide ambient stamp slot.
  Measured: 105 942 torn reads in `examples/27_concurrent_txn`, 147 overwrites of a
  still-armed slot, 0 untransacted-branch hits (which rules out per-delta timestamps and
  localises it to another live transaction's record).
- `mvcc-2026-08-03-unique-constraint-check-then-act` — status **FIXED** in `82c92b4b`, ACID
  Consistency, owned by rmp #2321. `ConstraintRegistry.CheckSetProperty` read under
  `RLock` while `RecordPropertySet` inserted under `Lock` later, so two concurrent writers
  both passed; 14 of 15 `-race` runs of 8 concurrent `MERGE` under a `UNIQUE` constraint
  produced 2 or 4 nodes. Fixed by `ConstraintRegistry.ReserveSetProperty` (atomic
  test-and-reserve, PostgreSQL's `_bt_doinsert` discipline) together with removing the
  rollback's whole-graph value-set rebuild, which destroyed CONCURRENT writers'
  reservations because a rebuild cannot see a commit that is not yet durable. Each
  statement now journals its own registry changes as inverses on the transaction undo log
  (`cypher/exec/constraint_journal.go`), which also closes the remaining half of audit
  finding E10. Verified: 20 of 20 `-race` runs converge on one node with all eight
  statements succeeding. +1 `Method` `exec.ConstraintRegistry.ReserveSetProperty`, +5
  `Test` in `cypher/exec/constraints_reserve_test.go`; `reseedConstraintsInsideBarrier` was
  DELETED.

The barrier itself is UNCHANGED at this commit: `cypher/api.go`'s autocommit path uses
`Graph.ApplyAtomicallyTx` and `store/txn` `Tx.Commit` uses `Graph.ApplyAtomically`, both
exclusive. `Graph.ApplyVersioned` (the shared bracket) exists, is tested, and is what
rmp #2320 will switch them to. `Graph.View` remains a SHARED hold, which is sound only
because writers hold the same barrier exclusively — see its doc comment for the choice
rmp #2320 forces. Also in this commit: audit finding E10 half-resolved
(`reseedConstraintsInsideBarrier` reads an MVCC snapshot, so a rollback cannot import a
concurrent writer's uncommitted value into a UNIQUE value-set; the O(N) walk is
documented and deferred, needing typed undo entries), and audit finding E19 resolved as a
documented NON-dependency (edge-handle order does not track WAL order and nothing reads
that correspondence: the handle travels in the frame and every restore path is a
high-water seed).

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
| `Commit` | A git commit that delivered one or more tasks, or that integrated a sprint into an integration branch. | `hash` (short — **8-char** in the live graph; this table said 7 until 2026-07-29 and the drift silently made 7-char lookups miss), `fullHash` (full 40-char), `message`, `sprintId` (int), `kind` (`merge` only — set on a sprint-integration merge commit; absent on an ordinary delivery commit), `branch` (the branch the merge landed on, e.g. `main`; set with `kind`) |
| `Agent` | A specialist sub-agent mandated by `CLAUDE.md`. | `name`, `kind` (`subagent`), `description`, `source` |
| `Skill` | A project-relevant Claude Code skill. | `name`, `kind` (`skill`), `description`, `path` |
| `Memory` | A persistent assistant memory file (mirror of the harness memory directory). | `name` (frontmatter slug), `file` (basename), `type` (`user`\|`feedback`\|`project`\|`reference`), `description` |
| `Document` | A prose document under `docs/` that records a decision, a design, an audit or a certification. Present in the live graph since before this table existed (see the data-quality note below); its certification use was documented 2026-08-09 (sprint 337). | `path` (repo-relative, the identity), `title`, `kind` (`certification`\|`design`\|`audit`), `verdict` (certifications only — the cycle's stated outcome) |

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
| `IMPLEMENTS` | `(Method\|Function)-[:IMPLEMENTS]->(Feature)` | A specific symbol realises a curated feature, at finer grain than the package-level edge above. Added 2026-07-29 (sprint 314, task #2153) so a feature whose whole implementation is a handful of symbols inside a large package can be located directly. Note a perf commit uses `IMPROVES`, not this edge. |
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
| `PART_OF` | `(Feature)-[:PART_OF]->(Feature)` | A sprint-scale capability belongs to a broader feature area (e.g. a planner peephole → `Cypher Engine`). Documented 2026-07-28 (sprint 311). |
| `DELIVERS` | `(Sprint)-[:DELIVERS]->(Feature)` | The sprint that delivered a capability. Documented 2026-07-28 (sprint 311). |
| `VERIFIES` | `(Test\|Benchmark)-[:VERIFIES]->(Feature)` | A test or benchmark that gates a feature's correctness or its measured performance. Documented 2026-07-28 (sprint 311). |
| `MEASURES` | `(Benchmark)-[:MEASURES]->(Function\|Method)` | A benchmark whose measurement targets a specific symbol, as distinct from `VERIFIES`, which targets a `Feature`. Documented 2026-07-29 (sprint 313, task #2145). |

**Data-quality note (observed 2026-07-02, partially remediated):** the live graph has
accumulated several more edge types across incremental syncs than this table documents in
full (`BELONGS_TO`, `CLOSES`, `FROM_AUDIT`, `REMEDIATED_BY`, `CONCERNS`, `FOUND`, `TESTS`,
`LIVES_IN`, `RELEASES`, `IMPLEMENTED_BY`, `DOCUMENTED_BY`, `DELIVERED_BY`, `HARDENED_IN`,
`MODIFIES` — 29 distinct types total per a live `MATCH ()-[r]->() RETURN type(r), count(r)`
query, vs. the ~14 documented above). This table was extended just enough to cover the edge
types touched by each sync: `IMPLEMENTED_IN`, `TOUCHES`, `DEPENDS_ON`, `FOLLOWED_BY`,
`IMPROVES` (2026-07-02), then `PART_OF`, `DELIVERS`, `VERIFIES` (2026-07-28, sprint 311). A
full reconciliation of the remaining ~14 types is a separate hygiene task.

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

Incrementally synced at commit `44bc252` (2026-07-27, task #2220, sprint 326 — two-sided
BFS for the single-path `shortestPath()`): **+7 nodes** — 1 `Commit`, 2 `Task` (`2220`
COMPLETED, `2236` BACKLOG), 1 `Spec`
(`docs/benchmarks/shortest-path-bidir-2026-07-27.md`), 3 `Method` on `exec.ShortestPath`
(`biBFSShortestPath`, `canBidirectional`, `bfsShortestPathForward` — the last is the
retained forward-only walk, now both the fallback and the differential reference).
**+12 edges** — 1 `CONTAINS`, 1 `IMPLEMENTED_IN`, 1 `IMPROVES` (→`Feature`
`Search & Path-finding`, id 10375), 1 `FOLLOWED_BY` (`2220`→`2236`), 8 `TOUCHES`. All
stamped `2026-07-27`. No new label or edge type.

`canBidirectional.admissionGate` carries the substance: the search is admitted for
**DirOut, untyped, shape-checked reverse CSR only**, and every exclusion is a measurement
rather than a preference — the `revToFwd` table was a 26 % end-to-end regression while the
search itself was 17.9× faster, per-slot type resolution admitted edges the filter excludes,
and DirIn/DirBoth are blocked on a finding that implicates the FORWARD-ONLY reference's own
reverse-slot type check. Follow-up `Task 2236`.

Incrementally synced at commit `11112c6` (2026-07-27, task #2221, sprint 326 — the
sequence-ordered apply gate wakes exactly its successor): **+13 nodes** — 1 `Commit`,
1 `Task` (`2221` COMPLETED), 1 `Spec`
(`docs/benchmarks/apply-gate-wake-2026-07-27.md`), 3 `Method` (`Tx.waitApplyTurn`,
`Tx.advanceApply` — both previously unmodelled — and `Store.ApplyWaiterCountForTest`,
the test-only seam in `export_test.go`), 2 `Function` (`acquireApplySlot`,
`releaseApplySlot`), 2 `Test` (`TestApplyGate_*`), and 4 `Benchmark` — of which
**2 are an audit repair**: `BenchmarkCommit` and `BenchmarkCommitConcurrent` in
`store/txn/bench_test.go` had never been modelled, a divergence found by comparing the
package's `func Benchmark` declarations against the graph during this sync. **+20 edges**
— 1 `CONTAINS` (`Sprint 326`→`Commit`), 1 `IMPLEMENTED_IN`, 1 `IMPROVES` (→`Feature`
`ACID Transactions`), 5 `TOUCHES`, 12 `Package CONTAINS`/`HAS_METHOD`. All stamped
`2026-07-27`. No new label or edge type.

The substance is what the measurement overturned. GoGraph's group commit **already worked**:
`Tx.Commit` released the single-writer semaphore (retired by rmp #2306) after the
append and coalesces in
`wal.Writer.SyncGroup`, reaching 31 300 commits/s at 256 writers before this change. The
"flat at 261 op/s from 1 to 1024 writers" that rounds 3 and 4 both recorded belongs
**exclusively to the Cypher path**, which fsyncs while holding `lpg`'s `visMu` in write mode
(`commitUnderBarrier`→`CommitWALOnly`), so `SyncGroup` never has a second caller — that
remains `Task 2193` and needs two-phase visibility. The real O(N) wake was in neither place:
`advanceApply` broadcast the apply gate, waking every parked committer per commit, and a CPU
profile put **77 % of all samples in `sync.(*Cond).Wait`, 100 % of it beneath
`waitApplyTurn`**. Waking only the holder of `seq+1` took 1024 writers from 7 425 to
111 897 commits/s (+1407 %, mean group 26.8→424) and saturated the disk at 264 fsync/s.

The round-4 audit's **headline lever was implemented faithfully and rejected on evidence**:
Neo4j's `TransactionLogQueue` one-waiter wake chain, ported to `wal.Writer` as an intrusive
queue whose head propagates the unpark, **regressed throughput 3–5 %** and was backed out.
Java pays an explicit `LockSupport.unpark` loop that the chain exists to avoid; Go's
`Cond.Broadcast` is a `notifyListNotifyAll` splice the runtime spreads across all Ps, so N
sequential channel sends are strictly worse. The prior-art insight survives only where
GoGraph's O(N) wake actually was, and where the successor is uniquely determined.

Incrementally synced at commit `f69530a` (2026-07-27, task #2222, sprint 326 — a faithful
physical plan and PROFILE): **+18 nodes** — 1 `Commit`, 4 `Task` (`2222` COMPLETED, plus
`2237`/`2238`/`2239` BACKLOG, all three filed from this work), 1 `Spec`
(`docs/benchmarks/physical-plan-surface-2026-07-27.md`), 4 `Method` on `cypher.Engine`
(`buildReadPhysical`, `explainPhysical`, `ExplainLogical`, `Profile`), 4 `Type` in
`cypher/exec` (`PlanNode`, `Profiler`, and the two optional interfaces `PlanChildren` and
`PlanDetail`), 3 `Function` (`PlanTree`, `RenderPlan`, `RenderPlanNode`) and 3 `Test`.
**+20 edges** — 1 `CONTAINS` (`Sprint 326`→`Commit`), 1 `IMPLEMENTED_IN`, 3 `FOLLOWED_BY`
(`2222`→`2237`/`2238`/`2239`), 3 `TOUCHES`, 12 `Package CONTAINS`/`HAS_METHOD`. All stamped
`2026-07-27`. No new label or edge type.

The substance is HOW the defect was made unrepresentable rather than merely fixed.
`Engine.Explain` had rendered the logical IR and re-derived the planner's physical decisions a
second time against it, which was wrong in both directions — `NodeByIndexSeek` reported where a
label scan ran (round 3) and `CartesianProduct` printed for an equi-join `hashJoinBuildCount`
proves executes as a hash join (round 4). Three mechanisms replace that reconstruction:
`buildReadPhysical` is the single build path shared by `Run` and both rendering surfaces; node
names are read from the operator's **concrete type**, so a `HashJoin` is named `HashJoin` because
it *is* one; and `TestPlanChildren_EveryOperatorWithInputsImplementsIt` parses the package to
derive the obligation, because an operator's inputs live in **unexported fields that reflection
cannot read** — a missing `PlanChildren` would silently TRUNCATE the plan. The gate found **46**
such operators, four of them columnar ones a field-type regex had missed.

Two constraints are worth recording because they are structural, not stylistic. The profiling
wrapper **must** live in `cypher/exec`: transparency requires re-implementing
`NodeIDColumnProducer`, whose marker method is unexported there, so the pre-existing
`cypher/explain.ProfiledOperator` could not be the wiring — wrapping from outside would strip the
marker and silently downgrade a columnar plan to row mode. And the builder wraps on the way **out**
of its recursion, so a parent runs its capability type-assertions against the wrapper; a
shape-equality test over five shapes is the gate. Cardinality **estimates** belong to logical
nodes and have no counterpart on a built operator, which is why `Engine.ExplainLogical` exists
rather than the estimates being lost — examples 24, 25 and 26 were retargeted to it because each
demonstrates a planner decision or a statistic, not what physically runs.

Rendering the truth immediately produced a new defect the old surface could not show: a single
`RETURN` builds **two stacked `Project` operators**, so every row is projected twice on the
engine's hottest path (`Task 2239`).

Incrementally synced at commit `ddae299` (2026-07-27, task #2223, sprint 326 — the durability axis
restored to the three-way harness): **+7 nodes** — 1 `Commit`, 1 `Task` (`2223` COMPLETED),
2 `Spec` (`docs/benchmarks/threeway-durability-2026-07-27.md` and its raw report), 2 `Function`
(`newEmbeddedDurableTarget`, `durabilityPosture`) and 1 `Method`
(`embeddedTarget.loadEdges`). **+8 edges** — 1 `CONTAINS`, 1 `IMPLEMENTED_IN`, 3 `TOUCHES`,
3 `Package CONTAINS`. All stamped `2026-07-27`. No new label or edge type.

The substance is that **round 3's write win was a measurement artefact and inverts**. The harness
compared a GoGraph with no durability at all — a bare `lpg.Graph`, no WAL, no fsync — against
Neo4j forcing its log every commit, which overstated GoGraph's writes and *understated* its
traversal losses. With a fourth target over a real `store.DB`: single-node write **5 µs in-memory
against 3.994 ms durable**, versus Neo4j's **2.039 ms** at the same posture, so GoGraph is **~2×
slower, not the 83× faster round 4 recorded**. The 3.994 ms is one fsync and agrees with the
~3.8 ms measured independently in #2221. Bulk load is the opposite case and survives: durability
costs it only **+25 %** (2.056 s → 2.564 s) because 5 000-row batches amortise the fsync, so
GoGraph still loads faster than Neo4j (2.564 s vs 4.054 s) at equal posture. Memgraph's default
`storage_wal_file_flush_every_n_tx=100000` means it fsyncs once every 100 000 transactions out of
the box — a comparison that does not state each posture is not a comparison.

Two further premises were **measured rather than corrected as stated**. The harness's string-key
join premise was stale, but the obvious fix is also wrong: an *inline* numeric key does reach an
index (#2169), yet this query binds its key from an `UNWIND` row, and in that shape **neither** key
reaches a per-row index — both lower to `NodeByLabelScan` feeding a `HashJoin`, with
`hashJoinBuildCount` firing exactly 2 for each, so the keys are at parity (2.064 s vs 2.056 s)
because #2228's hash join subsumes the per-row lookup for a bulk load. And row counts were **never
actually cross-checked**, only tabulated; the harness now fails on any disagreement before a single
timing is compared.

Incrementally synced at commit `81afd56` (2026-07-28, task #2231, sprint 326 — string equality
seeks the btree): **+8 nodes** — 1 `Commit`, 1 `Task` (`2231` COMPLETED), 1 `Spec`
(`docs/benchmarks/btree-string-eq-2026-07-28.md`), 2 `Function` (`extractSingleStringCmp`,
`boundFor`) and 3 `Test` (`TestBTreeStringEq_*`). **+11 edges** — 1 `CONTAINS`,
1 `IMPLEMENTED_IN`, 1 `FIXES` (→`Feature` `bound-seek-key-resolution`), 4 `TOUCHES`,
5 `Package CONTAINS`. All stamped `2026-07-28`. No new label or edge type.

`extractSingleStringCmp` rejected `=` while its numeric counterpart degenerated it into `[v, v]`
over the companion btree (#2169), so a **btree on a string property full-scanned an equality** even
though `findBoundStringBTree` could already locate the index — and the identical predicate written
as two inequalities already seeked. Accepting `=` and sharing the numeric path's selectivity gate
and residual filter: **4 393.8 µs → 5.177 µs at n = 20 000 (849×)**, with allocations **flat in the
node population** (819 525 and 219 364 B/op become 13 351 and 13 361) — which is the property that
proves the label is no longer walked, and the same signature #2226 used. The range is exact for
equality because strings order by **code point**, so no two distinct strings compare equal and no
collation question arises; only `=` was added, leaving the collation ruling to #2224.

The temporal hazard was **already closed**, and knowing why matters: the string btree uses the very
same `projectStringPropValue` gate as the hash index, which refuses the SOH-tagged encodings, so a
btree key is never created for a temporal. The rewrite inherits the exclusion rather than restating
it.

Two evidence-discipline facts worth keeping. A differential whose scan arm comes from
parameterising the key is **degenerate below the selectivity gate's floor**, because the literal
form scans there too — it must seed above the floor and assert the two arms take different plans.
And `exec.RangeBound.Include` is **metadata only**: `NodeByIndexRangeScan` always emits the
inclusive `[lo, hi]` superset and the residual filter enforces open/closed semantics, so flipping
inclusivity is a no-op and a fault injection has to shift the range off the key to bite.

Incrementally synced at commit `c8e5a0c` (2026-07-28, task #2230, sprint 326 — the write-clause
classifier stops reading comments and strings): **+12 nodes** — 1 `Commit`, 2 `Task` (`2230`
COMPLETED, `2240` BACKLOG), 1 `Spec`
(`docs/benchmarks/write-clause-classifier-2026-07-28.md`), 4 `Function` (`maskNonClauseRegions`
and its three region scanners), 3 `Test` and 1 `Benchmark`. **+12 edges** — 1 `CONTAINS`,
1 `IMPLEMENTED_IN`, 1 `FOLLOWED_BY` (`2230`→`2240`), 6 `TOUCHES`, 3 `Package CONTAINS`. All
stamped `2026-07-28`. No new label or edge type.

`writingKeywordRE` ran against the **raw** query text, so a keyword inside a comment or a quoted
string routed a READ onto the write path — where it serialises on the store's single writer and
silently throttles the concurrent read throughput the engine exists to provide. The heuristic was
never the problem; inspecting regions that cannot hold a clause was. The mask takes its four region
forms **from the lexer grammar** (`CypherLexer.g4`) rather than from memory, and three details are
load-bearing: the block comment is **non-greedy**, both string literals carry backslash
`EscapeSequence` so an escaped delimiter does not close them, and `ESC_LITERAL` (backtick) has **no
escape sequence at all**, so a backslash inside one is an ordinary byte. Masked bytes become
**spaces**, not deletions, so word boundaries and offsets survive; an unterminated region masks to
end of input, which is conservative *for this caller* because the worst outcome is a clean failure
on the read path rather than taking the writer lock to fail.

Fast-path allocations are **exactly unchanged** (1, all samples equal) and the time cost is +0.67 %
/ +1.83 % — precisely the single `ContainsAny` guard, ~20 ns against a 2.8 µs regexp match.

The measurement produced a finding worth more than the delta: **classification costs 1.4–2.8 µs on
every `RunAny`/`RunInTxAny` dispatch**, against ~5 µs for an entire indexed point lookup, so
classifying a fast query can be a third of its cost. The expense is the case-insensitive regexp,
and the new mask is the natural place to delete it — one walk could blank the regions *and* test the
words, fusing two passes into one (`Task 2240`).

Incrementally synced at commit `45ef885` (2026-07-28, task #2224, sprint 326 — the string-collation
divergence declared): **+6 nodes** — 1 `Commit`, 1 `Task` (`2224` COMPLETED), 2 `Spec`
(`docs/cypher.md`, `docs/tck/DIVERGENCES.md`) and 2 `Test`. **+7 edges** — 1 `CONTAINS`,
1 `IMPLEMENTED_IN`, 3 `TOUCHES`, 2 `Package CONTAINS`. All stamped `2026-07-28`. **This closes
sprint 326 at 18/18.**

GoGraph orders strings by **Unicode code point**; Neo4j by **UTF-16 code unit** (Java's
`String.compareTo`). Verified before documenting: `ORDER BY` is strictly ascending by rune, and the
mechanism is Go's native comparison on `string` — bytewise UTF-8, which UTF-8 guarantees equals
code-point order. The user **ruled to keep** code-point order, so the divergence is declared rather
than changed. Three reasons are recorded: openCypher 2024.3 specifies **no collation**, so neither
order is non-conformant and no TCK scenario discriminates them; UTF-16 unit order is an artefact of
Java's internal representation, placing some supplementary characters *before* numerically lower BMP
ones; and code-point order is **load-bearing** for #2231's degenerate-range equality proof, since no
two distinct strings compare equal under it — collation reaches the index layer, not just `ORDER BY`.

The divergence condition is exact and worth keeping: a supplementary-plane character (U+10000+,
stored in UTF-16 as a surrogate pair whose leading unit is U+D800–U+DBFF) compared against a BMP
character in **U+E000–U+FFFF**. Below U+E000 the rules agree, because no surrogate value is
reachable. Hence GoGraph sorts `U+1F600` **after** `U+FB01`; Neo4j before.

`docs/tck/DIVERGENCES.md` gained a new section for **behaviour the specification leaves
unspecified**, distinct from Category 4 (non-conformances) and from the closing extensions section:
a reader auditing conformance who noticed an `ORDER BY` difference would otherwise suspect a gap.

Two properties carry the substance. `recogniseDegreePattern.eligibility` records the port of
Neo4j's `QuerySolvableByGetDegree` + `isEligible` and, decisively, that **`Selections.empty`
makes a labelled far node ineligible for a degree rewrite in Neo4j too** — so
`COUNT { (a)-[:K]->(:P) }`, the round-4 audit's own 88× shape, is served by a different
mechanism (`Task 2235`), never by widening this recogniser. `applyDDLOp.why` records the
`Result`-leak defect found while fixing #2229: four intra-sequence callers discarded the
`Result` the DDL runner returns, and its armed finalizer counted a leak against
`cypher.result.leaked` on every `CREATE CONSTRAINT`, `DROP CONSTRAINT` and constraint unwind.

Incrementally synced at commit `59ddd2fb` (2026-07-29, task #2145, sprint 313 —
benchmarking the destination-ordered CSR): +63 nodes — `Package` `bench/csrorder`
(kind `bench`, parentless like every other `bench/*` package, so no `PART_OF`/`CONTAINS`
parent was invented); 4 `Commit` (`fafc50c7`, `1930f1f6`, `9cd5ada7`, `59ddd2fb` — the
first three were never recorded because they landed after the previous sync `b2d11ab7`);
`Spec` `docs/benchmarks/csr-neighbour-ordering-2026-07-29.md`; 14 `Benchmark`, 13 `Test`,
25 `Function`, 5 `Type`, 2 `Method` in the new package; 1 `Perf`
`csr-neighbour-ordering-2145`; 3 `Finding`. Edges: `CONTAINS` for every symbol,
`HAS_METHOD`, `Task -[IMPLEMENTED_IN]-> Commit`, `Commit -[TOUCHES]->` 4 packages and 3
specs, `Commit -[IMPROVES]-> Cypher Engine`, `Benchmark -[MEASURES]->` `OrderRuns` /
`lowerBoundDst` / `firstDstPos` / `WriteSnapshotFull`, `Benchmark -[VERIFIES]->` `Cypher
Engine` and `Persistence Backends`, `Task -[FOUND]->` the `Perf` and the 3 `Finding`s,
and `Task 2145 -[DEPENDS_ON]->` 2141-2144.

**Introduces `MEASURES`** so "which benchmark measures this symbol" is answerable without
conflating it with `VERIFIES`, which targets a `Feature`.

**Three fidelity gaps left by earlier syncs were repaired, not papered over:**

1. `Sprint 313` was claimed by commit `060367f9`'s message but **never materialised**. It
   now exists, with `CONTAINS` to all 7 tasks and all 9 sprint commits.
2. Tasks `2142` and `2143` existed as **property-less stubs** (created only as
   `DEPENDS_ON` targets) and `2144` was **absent entirely**. All three are now filled from
   `rmp` with status, type, commit and outcome. The absent endpoint is why
   `MATCH … MERGE` for `2145 -[DEPENDS_ON]-> 2144` silently no-opped — **a missing endpoint
   makes an edge MERGE a no-op, not an error**, which is how the gap stayed invisible.
3. `OrderRuns`, `lowerBoundDst`, `firstDstPos` and `dstRun` had **null `pkg` and `file`**;
   all four now carry them.

The three `Finding` nodes carry knowledge that outlives the sprint:
`bench-history-backtoback-drift` (MEDIUM — `scripts/bench-history.sh` compares
back-to-back and manufactures spurious ±2–4% regressions on the microsecond curated set;
an interleaved A/B is required for any claim hinging on a few percent, and ledger rows
0026–0031 all share the weakness), `audit-2.4-avgout-definition` (§2.4's "avg out" is
arcs/all-nodes, not arcs/arc-bearing-sources — a 1.62× difference on RMAT, invisible on
the undirected Barabási–Albert rows), and `audit-2.4-leverage-table-confirmed` (§2.4's
leverage table is confirmed reproducible to within 0.01 points; only its probe-cost column
stays refuted).

---

### Sprint 314 sync — Expand(Into) seek and the symmetric anchor swap (2026-07-29)

Recorded at `b2cb4fe5`. **Both Feature nodes ALREADY EXISTED**, created when the sprint was
planned: `Expand(Into) for a bound destination` (id 11889) and `Symmetric anchor swap
(lifting the OUT-only restriction)` (id 17071), each already carrying `PART_OF → Cypher
Engine` and `DEPENDS_ON → Destination-ordered CSR neighbour runs` / `→ OrderRuns`. They were
UPDATED, not re-created — checking first is what stopped this sync from duplicating them, and
prior syncs have over-reported nodes that never existed.

Added: `Sprint 314`; 5 `Commit`s (`d7236ab0`, `b39b83f0`, `1d86e167`, `7c14fd57`,
`b2cb4fe5`); `Package bench/expandinto` (kind `bench`); 4 `Method`s on `Expand`
(`seekIntoRuns`, `boundIntoDst`, `WithExpandIntoSeek`, `PlanDetail`); 6 `Function`s
(`ExpandIntoSeekCount`, `ExpandIntoSeekReverseCount`, `probeDepthEstimate`, `FitExponent`,
`SeedRing`, `SeedReverseHub`); 14 `Test`s; 4 `Benchmark`s; 2 `Spec`s.

Edges: `Sprint 314 -[CONTAINS]->` each Commit and `-[DELIVERS]->` both Features; `b39b83f0` /
`1d86e167` `-[IMPROVES]->` their Feature; the implementing symbols `-[IMPLEMENTS]->` their
Feature (the new finer-grained signature above); `cypher/exec` and `cypher`
`-[IMPLEMENTS]->` their Feature; both Features `-[SPECIFIED_IN]->` the design Spec; every new
Test/Benchmark `-[VERIFIES]->` its Feature and `<-[CONTAINS]-` its Package.

**Two of this sync's first-attempt edges did NOT conform and were corrected.**
`Commit -[IMPLEMENTS]-> Feature` was replaced by the documented `IMPROVES` (a perf commit
improves a feature area; `IMPLEMENTS` is for packages and now symbols). And
`Package -[MEASURES]-> Feature` was removed entirely: `MEASURES` targets a *symbol*, whereas
a Feature-targeting test or benchmark edge is `VERIFIES`. Both were caught by re-reading the
edge-type table rather than by trusting the shape that seemed natural.

Feature descriptions now carry the MEASURED outcome and the refuted premises, not the plan:
the seek is −17.27% / −50.56% / −77.69% at out-degree 8 / 32 / 64 with the fitted exponent
falling 1.249 → 0.809 and allocations FLAT; the swap is −93.73% / −99.69% at hub out-degree
1601 / 40000 with an accepted +75.45% allocs/op regression against −97.4% B/op. Both record
that the motivating audit's reference points were **not reproducible** (its exponent 2.02
measured 1.79 at the sprint base).

### Sprint 334 sync — the MVCC background vacuum (2026-08-04)

Recorded at `134dff2c` (task rmp #2308, MVCC C2). **Reclamation moved off the commit path**
into a demand-started, self-terminating background vacuum; see
[`docs/design-mvcc-vacuum.md`](docs/design-mvcc-vacuum.md).

Added: `Commit 134dff2c`; `Task 2308` (COMPLETED); 4 `Type`s (`vacuumState`, `vacuumUnit`,
`VacuumStats`, and `MVCCStats` — the last one **pre-existed in the code since sprint 333 but
was absent from the graph**, so it is added here rather than updated); 20 `Method`s (12 on
`Graph`: `Close`, `CloseCtx`, `VacuumStats`, `wakeVacuum`, `wakeVacuumOnRelease`,
`startVacuum`, `vacuumLoop`, `vacuumPass`, `sweepUnit`, `awaitVacuumProgress`,
`publishVacuumMetrics`, `chargeReclaimDebt`; 5 on `vacuumState`; `MVCCStats.WithinCeiling`;
`cypher.Engine.Close`/`CloseCtx`); 9 `Test`s; 2 `Benchmark`s; 3 `Spec`s
(`design-mvcc-vacuum.md`, `benchmarks/mvcc-vacuum-2026-08-04.md`, and
`design-mvcc-delta-chains.md`, which also pre-existed on disk and not in the graph).

Edges (80): `Sprint 334 -[CONTAINS]-> Commit`; `Task 2308 -[IMPLEMENTED_IN]-> Commit`;
`Commit -[IMPROVES]->` `MVCC as sole concurrency control` (12051) and `MVCC snapshot
isolation` (13861); `Commit -[FIXES]-> ACID Transactions` (9736) for the lost-row defect;
`Commit -[TOUCHES]->` packages `graph/lpg`, `cypher`, `graph/adjlist`, `internal/sim` and the
3 Specs; both MVCC Features `-[SPECIFIED_IN]->` the design Spec; `CONTAINS`/`HAS_METHOD` for
every new symbol; each new Test `-[VERIFIES]-> MVCC as sole concurrency control`; both
Benchmarks `-[MEASURES]-> Graph.vacuumLoop`.

**REMOVED from the code, and therefore absent from the graph by design:**
`Graph.ReclaimIdle` (the read-path inline sweep) and its call site in `cypher/api.go`,
`Graph.reclaimIfDue`, and `reclaimAfterDirectWrite`'s barrier acquisition. None of the three
had ever been symbol-synced, so there was nothing to delete — that gap is itself the
limitation noted below.

**Two write mistakes of mine were made and corrected during this sync; both are worth
recording because they are traps, not slips.**

1. A blanket `MATCH ()-[r]->() WHERE r.gitCommit IS NULL SET r.gitCommit=…` stamped **1 075
   pre-existing unstamped edges** as last confirmed at this commit — a fidelity error, since
   they were confirmed by earlier commits. Never stamp by "is null"; stamp only the edges the
   sync actually created.
2. The first correction used
   `WHERE NOT (a.gitCommit=$FH OR b.gitCommit=$FH)`, which silently missed every edge whose
   **both** endpoints lack `gitCommit`: in three-valued logic `NOT (NULL OR NULL)` is NULL,
   not TRUE, so those rows are filtered out rather than matched. 408 wrong stamps survived.
   The predicate must be `coalesce(a.gitCommit,'') <> $FH AND coalesce(b.gitCommit,'') <> $FH`.
   Verified afterwards: 0 edges carry this commit's stamp without an endpoint that does.

Both `IMPROVES` (not `IMPLEMENTS`) for the perf/architecture half and `FIXES` for the defect
half follow the edge-type table, which the sprint-314 note above records being got wrong the
first time.

### Sprint 334 sync — withdrawing an aborted transaction (2026-08-04)

Recorded at `f52ca1ff` (task rmp #2318). An aborted transaction now WITHDRAWS its
writes synchronously at abort; see
[`docs/design-mvcc-abort-withdrawal.md`](docs/design-mvcc-abort-withdrawal.md).

Added: `Commit f52ca1ff`; `Task 2318` (COMPLETED, `DEPENDS_ON Task 2308`); `Spec
design-mvcc-abort-withdrawal.md`; 11 `Method`s (`Graph.withdrawAbortedNow`,
`withdrawAbortedLabels`, `withdrawAbortedProps`, `withdrawAbortedSides`,
`withdrawAbortedIndexRemovals`, `reclaimAbortedLabelsLocked`,
`reclaimAbortedPropsLocked`, `reclaimAbortedLife`, `abortWake`;
`sideVersions.withdrawAborted`; `adjVersions.clearAborted`); 5 `Test`s.

Edges: `Sprint 334 -[CONTAINS]-> Commit`; `Task -[IMPLEMENTED_IN]-> Commit`;
`Commit -[FIXES]->` `ACID Transactions` (9736) and `MVCC as sole concurrency
control` (12051) — FIXES rather than IMPROVES, because the defect found was an
Atomicity violation and not the memory leak the ticket described; `Commit
-[TOUCHES]->` `graph/lpg`, `graph/mvcc` and the Spec; both Features
`-[SPECIFIED_IN]->` the Spec; `CONTAINS`/`HAS_METHOD` per symbol; each Test
`-[VERIFIES]-> ACID Transactions`.

The edge stamp used the 3VL-safe predicate the previous sync's note derives
(`coalesce(a.gitCommit,'') = $FH OR coalesce(b.gitCommit,'') = $FH`), and the
leak check afterwards returned 0.

### Sprint 334 sync — the coverage precondition for concurrency controls (2026-08-04)

Recorded at `83d1217d` (task rmp #2319). Added: `Commit 83d1217d`; `Task 2319`
(COMPLETED); 2 `Function`s (`testlayers.RequireUninstrumented`, `Instrumented`); 1
`Test`. Edges: `Sprint 334 -[CONTAINS]-> Commit`; `Task -[IMPLEMENTED_IN]-> Commit`;
`Commit -[TOUCHES]->` `internal/testlayers`, `bench/mvccwrite`, `store/wal`,
`graph/lpg`; `CONTAINS` per symbol. Leak check 0.

Recorded here because it is a CONTRACT and not only a fix: a test that measures
whether the ENVIRONMENT can demonstrate an effect is a CONTROL, and it skips under
coverage instrumentation; a test that measures the MODULE is a GATE, and it never
does. The two must not be confused, because guarding a gate that way would stop the
coverage arm gating anything.

### Sprint 339 sync — the CPU-efficiency head-to-head vs Neo4j and Memgraph (2026-08-11)

Recorded at `2ba5d36e` (and `283c7eb1` for the concurrency harness committed with it).
Added: 2 `Commit`s (`2ba5d36e`, `283c7eb1`); 2 `Spec`s
(`docs/cpu-vs-neo4j-memgraph-2026-08-11.md`,
`docs/concurrency-vs-neo4j-memgraph-2026-08-11.md`); 3 `Test`s (`TestCPUEfficiency`,
`TestConcurrencySweep`, `TestDeleteBatchScaling`, all in `bench/comparison`); 3 `Task`s
(2410 BUG, 2411 SPIKE, 2412 IMPROVEMENT, all BACKLOG); and 1 `Function`
(`cypher.populateRowCtx`) that the extractor had never captured — a genuine fidelity
gap found by this audit, not a new symbol.

Edges: `Commit -[TOUCHES]->` `bench/comparison` and each `Spec`; `CONTAINS` for
`TestCPUEfficiency`; `Spec -[FOUND]->` the four symbols the audit indicted
(`proto.ChunkedWriter.WriteMessage`, `expr.RowContext`, `cypher.populateRowCtx`,
`cypher.planCache`); `Task -[CONCERNS]->` its symbol; `Task -[FROM_AUDIT]-> Spec`.
No new label or edge type: `FOUND`, `CONCERNS` and `FROM_AUDIT` already existed in the
live graph (see the edge-table data-quality note).

Why these four symbols are worth having in the graph as *indicted*: the audit measured
processor time per operation against both peers and found GoGraph has the **lowest**
fixed CPU per query of the three (47.8 µs vs Memgraph 71.6, Neo4j 167.7) but the
**highest** marginal cost per row (1.88 µs vs 0.88 and 0.96). A matched pair isolating
delivery from computation showed 97 % of that per-row cost is
`ChunkedWriter.WriteMessage` flushing per Bolt message — one `write(2)` syscall per
returned row. Separately, `RowContext` being a `map[string]Value` rebuilt per row by
`populateRowCtx` puts ~19 % of engine CPU in Go map machinery on a scan-plus-filter,
and `planCache` being keyed on raw query text costs +65 % CPU when a literal rotates.
At 64 concurrent clients GoGraph uses 2.51× less CPU per point lookup than Memgraph
and 3.66× less than Neo4j, and is the only one of the three whose per-operation cost
falls as clients are added.

### Sprint 340 sync — remediating the CPU audit (2026-08-11)

Recorded at `8a4be5b2`. Added: `Sprint 340` (CLOSED); 4 `Commit`s (`6fc5a693`,
`d7f6c1a8`, `80e16db2`, `88e6be61`); 4 `Task`s (2413, 2414, 2415, 2416); and the
symbols the sprint introduced — `proto.ChunkedWriter.SetAutoFlush` and `.Flush`,
`cypher.schemaWalk` and `cypher.newSchemaWalk`, `lpg.bagKeyAt`.

Edges: `Sprint 340 -[CONTAINS]->` each commit; `Task -[IMPLEMENTED_IN]->` its
commit for 2410, 2413, 2415 and 2416; `Commit -[TOUCHES]->` `bolt/proto`,
`bolt/server`, `cypher`, `graph/lpg`; and `Task 2412 -[DEPENDS_ON]-> Task 2414`,
which is the one relationship in this sync worth reading twice.

**What the sprint established, and why the graph should carry it.** Three
findings went in; two came out fixed, one came out *blocked with its blocker
measured*, and the spike redirected itself:

- **#2410** — the Bolt writer flushed per message, so a K-row result cost K
  `write(2)` syscalls. Fixed by defaulting auto-flush ON and letting only the
  server opt out: a 1 000-row query fell **1 943.6 → 322.2 µs of CPU (6.03×)**.
- **#2412** — literal normalisation WORKED (eight literals collapsed onto one
  cache entry, TCK still 3897) and was reverted anyway, because a parameter is
  not planned like a literal here: the btree string-equality, range and prefix
  seeks all fall back to `NodeByLabelScan`, and a type mismatch that yields zero
  rows for a literal *raises* for a parameter. Hence #2414 and the `DEPENDS_ON`.
- **#2411** — the spike found the row context was no longer the biggest item.
  After **#2415** hoisted the schema walk out of the per-row loop (−12.6 %),
  `lpg.bagDecodeAt` was 28.18 % cumulative, and **#2416** stopped the property
  lookup decoding the value of every record it walks past (−12.7 %). Together
  `scan_filter` went **314.7 → 238.4 ns/node**, narrowing the gap to Memgraph
  from 2.74× to 2.07×, with no architecture change. The spike's recommendation is
  to do #2414 first, re-profile, and only then decide #2411.
- **#2413** — `TestHandlePropLatch_SetBeforeVisible` had never been green, so
  `make ci` was red on the cover-gate stage before any of this work started.

### Sprint 341 sync — parameter/literal parity (2026-08-11)

Recorded at `797d2182`. Added: `Sprint 341` (CLOSED); 3 `Commit`s (`3fd78c5e`,
`1a9aa1e4`, `30c04fa9`); `Task` 2417; and the symbols introduced —
`parser.StripLiterals`, `cypher.stringOperand`, and the rewritten `lpg.bagUint`.
Edges: `Sprint -[CONTAINS]->` each commit, `Task -[IMPLEMENTED_IN]->` its commit
for 2412/2414/2417, `Commit -[TOUCHES]->` `cypher`, `cypher/parser`, `graph/lpg`.

**The finding worth carrying: #2414 was a defect on the RECOMMENDED usage.** The
string range extractor admitted literals only — a documented scope limitation
("parameter range seeks are deliberately out of scope for this increment") whose
numeric companion had been extended by #1652 and which nobody went back for. So
`n.sk = 'lit'` seeked a btree index while `n.sk = $p`, the spelling every driver
sends, planned a full `NodeByLabelScan`. Nothing failed: the answers stayed
right and only the plan collapsed, which is why it survived. It was found only
because literal hoisting (#2412) rewrote literals into parameters and the
existing plan tests went red.

With parity in place #2412 re-landed, and re-profiling then surfaced #2417: a
`copy` with a VARIABLE length does not inline, so `bagUint` decoded every
property field through a `runtime.memmove` CALL — 10.20 % of all engine CPU,
100 % of it from that one function, removed by a switch on the four widths.

After this sprint GoGraph is the cheapest of the three engines on seven of the
eight measured workloads; `scan_filter` remains at 1.99× Memgraph and is #2411.

### Sprint 342 sync — closing the concurrency assessment's findings (2026-08-12)

Recorded at `1fa1bb6a` and `93410102`. Added: `Sprint 342` (CLOSED); `Component`
`adjlist.revIndex` (`graph/adjlist/reverse.go`); `Defect`s 2400, 2418 and 2419,
all `fixed`; and `Lesson` `confirmed-at-source-is-not-priced`. Edges:
`Sprint -[CLOSED_DEFECT]->` each defect, `Defect -[FIXED_BY]-> Component` for
2400 and 2418, and `Defect 2400 -[TAUGHT]-> Lesson`.

**The finding worth carrying: a root cause recorded as "confirmed at source"
had never been priced, and was wrong.** The 2026-08-11 concurrency assessment
attributed its one defect — bulk delete degrading without bound, 4.8× over five
cycles at exactly one core — to `removeNodeInfo` cloning the whole roaring64
tombstone bitmap per node removed. That code does exactly what the report says.
A CPU profile of the reproduction prices it at **0.99 %**, and a microbenchmark
of the named mechanism moves only 1.53× across an eightyfold increase in the
set being cloned, because a dense id range compresses to a handful of roaring
containers. Reading a mechanism in the source confirms that it EXISTS, never
that it COSTS.

The real cause was **78.77 %** of that same profile: `lpgMutatorAdapter.InNeighbours`
answered "which nodes hold an edge into n" with a walk of every interned node,
once per node deleted, so a delete cost O(k·n) with *n* counting every node ever
interned rather than the live ones — which is why the cost grew across cycles,
stayed flat within a wipe, used one core, and left `count(*)` fast. The
adjacency now keeps a live in-edge index, as Neo4j and Memgraph both do:
cycle six falls 4.622 s → 77.2 ms, `DETACH DELETE` 22.2×, and 90 000 nodes in
one statement 15.97 s → 375.6 ms, for +1.92 % on end-to-end relationship
creation with the read path unchanged.

Two defects in that fix were caught only by the benchstat comparison the user
required before accepting it, and by no correctness test: the first draft grew
the per-shard array to exactly `intra+1` per new destination (O(n²), +1057 %
memory on a 100 k-edge hub), and it carried its own atomic counter of recorded
in-edges — a second globally shared cache line on the write path for a number
`AdjList.Size` already had.

#2419 raised `DefaultMaxOpenTxPerPrincipal` 16 → 2048 so the default
configuration reaches the concurrency levels the project guidelines publish. The
trade is explicit: every open transaction pins an MVCC read snapshot and holds
the reclamation horizon back for its lifetime, so the bound on that resource is
weaker, and `DefaultMaxTxIdleTime` (5 s) is what limits the exposure. Under the
defaults the quota can no longer bind at all, because a connection holds at most
one open transaction and `MaxConnections` defaults to 1024.

### Sprint 343 sync — the certification's three correctness defects (2026-08-13)

Recorded at `9167d3d3` and `fca34a0c`. Added: `Defect`s 2420, 2366 and 2423, all
`FIXED`; `Lesson`s `a-fallback-is-a-ceiling-not-a-default` and
`an-index-is-a-candidate-source-never-a-label-proof`. Edges:
`Sprint 343 -[ADDRESSES]->` each defect, and `Defect -[TAUGHT]-> Lesson` for 2420
and 2423.

**#2420 — the fallback is a CEILING, not a default.** `mvcc.Horizon.Oldest`
computes the reclamation watermark by scanning the occupancy words once. It
carried a `found` flag and took the first occupied slot's instant
unconditionally, so the fallback — the published frontier, sampled by the caller
BEFORE the scan — was discarded the moment any reader was seen, and with every
live reader newer than it the watermark came out ABOVE it. Claiming the occupancy
bit before reading the clock protects a reader the scan SEES; it cannot protect
one that claims a slot in a word the scan has already passed, and the fallback is
the only bound that covers such a reader. A sweep at the inflated watermark frees
the version that reader must undo, and it reads a value from after its own
instant — an Isolation violation with nothing reporting it, which is what the
previous cycle's fourteen refuted mechanisms had been circling.

**Two of the previous cycle's assertions were TRUE and both were blind to it**:
the reclaimer never freed a record above the watermark it was given, and a reader
that checks the watermark at its own birth is always visible to its own scan.

**The detector is worth more than the fix.** `Graph.publishWatermark` now counts a
watermark that moves BACKWARDS, judged against the caller's own earlier sample so
a reordered publish of two near-simultaneous scans cannot read as a breach, and
`mvcc.Horizon.Leave` invalidates the slot's timestamp before clearing the
occupancy bit so an occupied slot never reads as a previous occupant's stale
instant. With the residue in place the counter reported 1 734–4 165 benign events
per run; with it gone, 5 per run, all real; with `Oldest` capped, zero. It fires
at the corruption rather than at the read that suffers it, which turned a
2 %-per-run symptom into a 5-per-run signal. `Horizon.StaleLeaves` and
`Horizon.SlotState` are the release-side and reader-side detectors of the same
family, both pinned on sound AND broken controls.

**#2366 — the two directions of a UNIQUE value-set change are not symmetric.** A
reservation is eager and its rollback is sound, because the value was reserved
throughout. A release applied eagerly hands the value to any peer that asks, so
its rollback wrote a RE-RESERVATION into shared state judged against the
rolling-back transaction's own view — while a peer's COMMITTED release had already
freed the same value, leaving it reserved with no live holder. A release is now
recorded on `exec.ConstraintTxn` and applied only by `CommitTxn`; rollback drops a
private mark. That also closes the mirror hazard, in which a peer could take a
value an uncommitted transaction had vacated and share it if that transaction
rolled back.

**#2423 — an index is a CANDIDATE source, never proof of a label.** Found while
fixing #2366, whose fix stopped masking it. The planner rewrote
`Selection(n.email = 'old')` over `NodeByLabelScan(Person)` into a bare
`NodeByIndexSeek`, assuming a label-scoped index implies the label; a label
removal leaves the node's entries in that label's property indexes, so the engine
returned a row that matched `(n:Person)` while reporting `labels(n)` as the EMPTY
LIST. The label scan was never affected because it resolves through
`LabelBitmapAsOf`, which filters exactly this. Every rewrite that replaces a
labelled scan leaf now qualifies its candidates — a residual predicate for the
hash and key-set seeks, a bitmap intersection for the range and intersection
scans — and one that cannot verify DECLINES. Only the hash-equality shape
REPRODUCED; the others planned as `Filter` over `NodeByLabelScan` in that fixture,
so they are fixed as latent rather than demonstrated, and two existing tests were
found to be asserting the defect (both seeded a node with no label and expected a
labelled seek to return it).

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

Incrementally synced at commits `cba27fba` and `406a0b48` (2026-07-28, sprint 313,
tasks #2139 and #2141 — destination-ordered CSR neighbour runs).

+22 nodes: `Sprint` 313; `Commit` `cba27fba` and `406a0b48`; `Task` 2139
(COMPLETED), 2140 (**SUPERSEDED**) and 2141 (COMPLETED); `Spec`
`docs/design-degree-adaptive-adjacency.md`; `Function`s `OrderRuns` (exported),
`runOrdered`, `insertionSortRun`, `mergeSortRun`, `mergeRun` in
`graph/csr/order.go` and `distinctDestinationsSorted` in
`store/snapshot/labels.go`; `Method` `CSR.RunsOrdered`; 12 `Test`s across
`graph/csr/order_test.go` (9) and `store/snapshot/determinism_order_test.go` (3).

+edges: `Sprint 313 -[CONTAINS]->` both Commits; `Task 2139/2141
-[IMPLEMENTED_IN]->` their Commits; `Commit cba27fba -[TOUCHES]->` the new Spec;
`Commit 406a0b48 -[TOUCHES]->` Packages `graph/csr` (241), `store/snapshot`
(147), `store/bulk` (2) and `search` (131); `CONTAINS` for every new Function,
Method and Test; `CSR -[HAS_METHOD]-> RunsOrdered`; `VERIFIES` from the new
Tests to `OrderRuns` and `distinctDestinationsSorted`; `Commit 406a0b48
-[IMPROVES]-> Feature` `openCypher TCK Compliance` (14016). Provenance bumped on
Packages `graph/csr`/`store/snapshot`/`store/bulk` and the `CSR` Type.

**NEW EDGE SHAPE:** `DEPENDS_ON (Feature)->(Function)`, recording that the
`Min-cardinality multi-label anchor scan` (13305) and `Composed single-property
index intersection` (14732) Features depend on `csr.OrderRuns`. `DEPENDS_ON` was
previously only `(Task)->(Task)`; this widens it to express a
feature-depends-on-implementation relation.

**DECISION recorded on `Task` 2140, status SUPERSEDED — do not re-propose.** The
adjacency neighbour representation is deliberately NOT ordered. Three blockers,
two structural: `AuxColumn` exposes no permutation primitive and its contract
blesses a strictly-ascending index array that `graph/lpg/edge_property_column.go`
implements; "allocs/op unchanged" is unattainable by construction, because an
ordered insert writes below `oldLen` and forfeits the zero-allocation in-place
append (O(d^2) hub build, irreducible Omega(sqrt d) per append); and a
history-dependent representation breaks recovery determinism, since
`ApplyCSRToGraph` replays in bulk `csr.bin` order on a different degree
trajectory. The `Spec` node also records that
`docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md` section 2.4 is **REFUTED**
(crossover degree ~16 not ~64; 6.0x/10.9x at degree 4096 not 30.9x; its implied
0.040 ns/element is 4.1x below the measured 0.164 ns/element branch-free floor).

Incrementally synced at commits `6b6fb675`, `34469a83` and `f2766f29` (2026-07-29,
sprint 313, tasks #2142 and #2143 — O(log d) CSR probes, the cross-query CSR pair
cache, and the audit remediation).

+11 nodes: `Commit`s `6b6fb675` / `34469a83` / `f2766f29`; `Task`s 2142 and 2143
(COMPLETED) and 2250 (BACKLOG, **BUG severity 8**); `Type` `csrPairCache`;
`Function`s `csrPairCached` (`cypher/csr_pair_cache.go`) and `lowerBoundDst` /
`firstDstPos` / `dstRun` (`cypher/exec/csrprobe.go`).

+edges: `Sprint 313 -[CONTAINS]->` all three Commits; `Task 2142
-[IMPLEMENTED_IN]-> 6b6fb675`; `Task 2143 -[IMPLEMENTED_IN]->` both `34469a83` and
`f2766f29`; `CONTAINS` from Packages `cypher` (csr_pair_cache.go) and `cypher/exec`
(csrprobe.go); `Commit f2766f29 -[FROM_AUDIT]-> Task 2250`.

**Two ACID-relevant facts recorded on the nodes.**

1. `lpg.Graph`'s OWN edge mutators now bump `topoGeneration` — `AddEdge`,
   `AddEdgeLabeled`, `AddEdgeLabeledWithProperty`, `AddEdgeH`, `AddEdgeHIfAbsent`,
   `RemoveEdge`, `RemoveEdgeByHandle`, `RemoveAllEdgesFrom` — plus all three
   tombstone transitions (`RemoveNode`, `revive`, `RestoreTombstones`). Previously
   the bump lived only in the engine write adapters and `store/txn`, so a direct
   Go-API mutation left every CSR-position-keyed cache stale. With the CSR pair
   cache that made a COMMITTED edge invisible to queries (verified: warm engine 1,
   truth 2). Invalidation is now at source, per PostgreSQL's
   `CacheInvalidateHeapTuple`-inside-`heap_insert` pattern.

2. **`Task` 2250 is a PRE-EXISTING conformance defect**, reproduced identically on
   the pre-sprint tree: a reverse-direction type filter over parallel relationships
   returns wrong counts (first type absorbs all, others return zero), and the
   forward direction mis-types too when a handle-0 slot is mixed with
   handle-bearing siblings. The TCK is green at 3897/3897, so no scenario covers
   the shape. Also recorded: `buildEdgeTypeFilter`'s ordinal fallback is
   **unreachable from pure Cypher**, because CREATE and MERGE both mint handles via
   `AddEdgeH` — handle 0 comes only from a raw `lpg`/`adjlist` `AddEdge` or a
   `store/txn` `OpAddEdge`.

Incrementally synced at commit `d8847ce7` (2026-08-02, tasks #2300 and #2301,
sprint 334 — write-write conflict detection extended to seven of eight versioned
stores): +16 nodes (`Commit` `d8847ce7`; `Feature` `Write-write conflict
detection`; `Component`s `writeCtx.conflict`, `noteNodeLife`,
`nodeLifeShard.headStamp`, `sideVersions.headStamp`; 11 `Test`s — nine in the new
`graph/lpg/mvcc_conflict_stores_test.go` covering node existence and each of the
five per-edge side stores, plus
`TestWriteCtx_VoidPrimitiveConflictDoomsTheTransaction` and
`TestWriteCtx_DoomedTransactionRefusesEveryFurtherWrite` in
`mvcc_writectx_test.go`). +edges: `Commit -[TOUCHES]->` `Package lpg` and `Spec
design-write-conflict-detection.md`; `Commit -[FIXES]->` the new Feature and
`Per-transaction write state`; the Feature `-[PART_OF]->` `MVCC as sole
concurrency control` and `-[SPECIFIED_IN]->` the Spec; `VERIFIES` from all 17
conflict-detection tests; `IMPLEMENTS` from the five Components. Provenance
stamped on every node and edge touched.

THE DEFECT THIS COMMIT CLOSED, recorded because it is a class rather than an
incident: detection that only SKIPS a conflicting write, without recording it on
the transaction, is a silent lost update whenever the primitive cannot return an
error. `commit()` could not fail, so a transaction whose only conflicting write
went through `removeNodeLabel` or `delNodeProperty` committed successfully having
dropped it. The fix is Memgraph's `transaction->must_abort` read at
`Storage::Commit`: `writeCtx.conflict` records, `labelTx.commit` returns
`(uint64, error)` and aborts. A refused push now also returns `false` and all
twelve callers abandon the mutation.

COVERAGE IS 7/8, NOT 8/8. The adjacency (`graph/adjlist`) has no conflict
detection: its version chain is stamped from a per-graph `mvcc.WriteStamp` and
its commit window (`commitDepth`, `dirtyShards`, and each shard's `building`
builder) is single-writer by contract. That is rmp #2301's remaining half and is
tabulated in `docs/design-write-conflict-detection.md` so the gap is visible
rather than implied.

Incrementally synced at commit `ca4e1538` (2026-08-03, task #2301, sprint 334 —
**MVCC A4: per-transaction commit state**, closing audit findings **E3, E4 and
E16**): +14 nodes (`Commit` `ca4e1538`; `Package` `graph/mvcc` — it had no node at
all before, which is itself a fidelity defect this sync repaired; `Spec`
`design-per-transaction-write-state.md`; six Components — `mvcc.TxState`,
`mvcc.WriteStamp.Publish`, `Graph.writeTx`, `Graph.acquireWriteCtx`,
`Graph.releaseWriteCtx`, `Graph.writerSnapshot`; seven Tests —
`TestWriteStamp_TwoTransactionsDoNotShareState`,
`TestWriteStamp_VersionCountIsPerTransaction`,
`TestWriteStamp_RetractedWindowRefusesLateVersions`,
`TestTxState_RefusesReuseWhileARecordIsStranded`,
`TestWriteStamp_ConcurrentTransactionsUnderRace`,
`TestWriteCtx_RecycledStateIsNeverSharedAcrossTransactions`,
`TestWriteCtx_ConcurrentTransactionsKeepTheirOwnRecord`), +26 edges (`Feature
Per-transaction write state -[SPECIFIED_IN]->` the new Spec and `-[FIXES]->` the
Commit; `-[IMPLEMENTS]->` each of the six new Components; `-[VERIFIES]->` each of
the seven new Tests; `Commit -[TOUCHES]->` the Spec, the six Components, and the
packages `graph/mvcc`, `graph/lpg`, `graph/adjlist`). Provenance stamped on every
node and edge touched.

WHAT E3 ACTUALLY WAS, recorded because the task's own premise was wrong: the
commit record, the version count and the pending transaction id were fields on
`mvcc.WriteStamp`, one set per graph, so a second `Begin` destroyed the first
transaction's window. **It is not a data race** — every field was already atomic,
so `-race` is silent on it — it is **silent data loss**. Measured against the
pre-change build: writer A's record was never returned by anything (A got `nil`
and 0 versions) while B was charged A's version too (2 versions). A's versions
keep transaction id `9223372036854775809` for the life of the process: invisible
to every reader for ever, and unreclaimable, because every reclaimer truncates on
`stamp <= watermark`, which an in-flight id can never satisfy, and nothing
reports it. The acceptance instrument is therefore an assertion on the retracted
record and count, not the race detector.

THE SHAPE: the transaction owns the state (`mvcc.TxState`, embedded by value in
`writeCtx`), and the graph owns only a SLOT naming the transaction currently
writing — replaced, never mutated. The slot survives because a write that carries
no transaction must still resolve one: the public Go-API mutators are
per-operation atomic rather than transactional, and `adjlist` reaches the shared
record through `mvcc.WriteStamp` without being able to see lpg's transaction
type. A write that DOES carry its transaction never consults the slot. Prior art
read in source: Memgraph `src/storage/v2/` threads `Transaction *transaction`
into every accessor; PostgreSQL `src/backend/access/transam/xact.c` keeps the
top-level transaction state in a `static TransactionStateData` reused by every
transaction a backend runs.

THE HAZARD RECYCLING INTRODUCES, and the sentinel that closes it: a version
arriving after its transaction was retracted must NOT be stamped with that
transaction's record, because the record already carries a commit timestamp and
the version would become visible EARLIER than it happened — a reader whose
snapshot predates the write would observe it. `Retract` stores `nil`, so `Ensure`
answers "no window" and the caller falls back to a timestamp of its own.
Later-than-it-happened is safe; earlier is not. The mirror case, a record
stranded in a finished state, is closed by `Arm` refusing to recycle it.

COST, measured, `benchstat` n=6: `BenchmarkBarrier_ApplyAtomically`
19.11 ns → 26.99 ns, **+41.27 % (p=0.002)** on an EMPTY bracket, allocations
unchanged at **0/op**; one Cypher write end to end 3.090 µs → 3.111 µs, **not
statistically significant (p=0.151, n=5)**. `sync.Pool` was tried FIRST and
rejected on measurement (31.3 ns, +64 %, all `procPin` plus the per-P `poolLocal`
walk); one atomic slot does the same work in a `Swap` and a `Store`, and degrades
to allocation — never to sharing — when rmp #2304 removes the barrier.

COVERAGE IS NOW 8/8 for per-transaction state. The adjacency gap recorded at
`d8847ce7` is closed: `commitDepth` and `dirtyShards` were replaced by a
per-shard `buildingOwner` token at `e3a0ea1e`, so a builder is reused only by the
transaction that created it. Conflict DETECTION still covers seven stores rather
than eight, and that remains tabulated in
`docs/design-write-conflict-detection.md`.

Incrementally synced at commit `205ca672` (2026-08-03, task #2300, sprint 334 —
**the serialization conflict's retriable path to a Bolt client**): +8 nodes
(`Commit` `205ca672`; two Components — `FailureCode serialization-conflict
mapping`, `sanitiseErr conflict message forwarding`; five Tests —
`TestFailureCode_SerializationConflict`,
`TestFailureCode_SerializationConflictIsRetriedByTheRealDriver`,
`TestFailureCode_DemotedTransientCodesAreNotRetriable`,
`TestSanitiseErr_ForwardsTheConflictMessage`,
`TestFailureCode_ConflictIsNotClassifiedAsAClientFault`), +12 edges (`Feature
Write-write conflict detection -[FIXES]->` the Commit, `-[IMPLEMENTS]->` both
Components, `-[VERIFIES]->` all five Tests; `Commit -[TOUCHES]->` both Components,
the Spec `design-write-conflict-detection.md`, and the package `bolt/server`).

THE TRAP, recorded because it is a class rather than an incident: a Bolt status
code being in the `Neo.TransientError.*` family is NOT sufficient for the official
driver to retry it. `neo4j-go-driver` v5.28.4 runs `db.Neo4jError.reclassify()`
BEFORE parsing the classification, and it rewrites
`Neo.TransientError.Transaction.Terminated` and
`…Transaction.LockClientStopped` into the `Neo.ClientError` family — so both are
**never retried**. `Terminated` reads as the natural fit for "your transaction was
aborted, try again" and would have been silently wrong. The conflict maps to
`Neo.TransientError.Transaction.Outdated`, whose Neo4j `Status.java` text
("transaction has seen state which has been invalidated by applied updates") is
this rule exactly. The mapping is guarded by a test that asks
`neo4j.IsRetryable` — the driver's own classifier — plus a NEGATIVE CONTROL over
both demoted codes; the instrument was validated by moving the mapping onto
`Terminated` and confirming the positive test fails.

WHAT #2300 STILL CANNOT PROVE, recorded so the gap is visible rather than
implied: a collision that actually travels the wire. The engine's writes pass no
transaction, an explicit transaction holds the exclusive barrier for its whole
lifetime (`cypher/exectx.go:353`), and a writer's start timestamp is read AFTER
the barrier is acquired — so not even first-committer-wins can fire. The
write-scaling gate's disjoint arm is therefore green over a code path that cannot
fail, which is not evidence. Both become provable when rmp #2304 gives the engine
a transaction to thread; §7.1 of `docs/design-write-conflict-detection.md`
tabulates all four claims with their status.

Incrementally synced at commit `9eee3b18` (2026-08-03, task #2302, sprint 334 —
**WAL transaction contiguity**, closing audit finding **E5**): +12 nodes
(`Commit` `9eee3b18`; `Feature` `WAL transaction contiguity`; `Spec`
`design-wal-transaction-contiguity.md`; two Components — `wal.Writer.AppendRun`,
`wal.Writer.appendLocked`; seven Tests — four in `store/wal/append_run_test.go`
and three in `store/recovery/interleaved_txn_test.go`), +18 edges (the Feature
`-[PART_OF]-> MVCC as sole concurrency control`, `-[SPECIFIED_IN]->` the Spec,
`-[FIXES]->` the Commit, `-[IMPLEMENTS]->` both Components, `-[VERIFIES]->` all
seven Tests; `Commit -[TOUCHES]->` both Components, the Spec, and the packages
`store/wal`, `store/txn`, `store/recovery`).

WHAT E5 ACTUALLY WAS, and it is TWO defects rather than one — recorded because the
first draft of the design named the wrong one. Recovery commits the ops carrying a
marker's own `TxnSeq` and discards the buffered prefix as orphaned, licensed by its
own comment: *"The store serialises commits (single writer), so a transaction's
frames are contiguous."* That licence came from a semaphore two packages up, not
from the WAL. Under interleaving the damage depends on WHERE the foreign frame
lands:

- foreign op in the buffer's **PREFIX** → the scan walks past it, `orphanedOps=1`,
  the op is **LOST** and never seen again. Three of four ops remain, so the graph
  looks entirely plausible. A **Durability** violation whose only signal is one
  counter.
- foreign op in the **MIDDLE** → the scan starts at 0 and applies it under the
  WRONG marker, so an uncommitted transaction's op is durable after a crash.
  `orphanedOps=0`, nothing dropped. An **Atomicity** violation with no signal at
  all.

The design was written against the middle order expecting data loss, and the test
went GREEN over a live defect until the order was measured. Both arms are kept
permanently, and each fails loudly if its defect stops reproducing — because that
would mean the guarantee is no longer what protects atomicity and the positive
tests would be proving nothing.

THE SHAPE: `wal.Writer.AppendRun` holds the writer's own mutex for a whole run, so
contiguity is a property of the component that owns the file. Recovery's
assumption stays TRUE rather than being relaxed — hence no format change, no new
frame field, no change to `store/recovery` at all. Rejected in writing:
PostgreSQL's `XLogInsertRecord` space reservation (holes recovery must tolerate,
for a concurrent-memcpy payoff nothing has measured) and InnoDB's
group-by-mini-transaction (needs an aggregate buffer cap invented on top of
`maxTxnOps`). What IS taken from PostgreSQL is the insight at the right
granularity: expensive work outside the lock, the lock only for the copy — so the
per-op encoding stays with the caller and the commit path keeps its pooled scratch
buffer.

MEASURED: a loop of `Append` shattered a 25-frame transaction into **8 runs**
(longest 10) against 8 concurrent appenders; `AppendRun` gives exactly **1**.

PARTIAL DELIVERY, tabulated in §3 of the Spec rather than implied: criteria 1, 2, 3
and 7 are closed. Durable and monotone `txnSeq` (4), crash-injection with
concurrent writers (5), the fsync-per-commit benchmark (6) and audit finding **E21**
(recovery and bulkimport open the adjacency commit window with NO barrier,
licensed only by a comment) remain open on #2302.

ALSO NOTE, for anyone asserting on it: there is **no `OrphanedOps` field on
`recovery.Result`**. The orphan count is metrics-only, so a test that asserts it
must install a `metrics.SetBackend` backend — and such tests must NOT be
`t.Parallel()`, because that backend is process-global and two parallel tests
would each count the other's orphans.

Incrementally synced at commit `8c419e35` (2026-08-03, task #2302, sprint 334 —
**resuming the transaction sequence across a reopen**, audit finding **E5**'s second
half, acceptance criterion 4): +7 nodes (`Commit` `8c419e35`; two Components —
`recovery.Result.MaxTxnSeq`, `txn.Options.ResumeTxnSeq`; four Tests in
`store/txn/resume_txnseq_test.go`), +12 edges (the `WAL transaction contiguity`
Feature `-[FIXES]->` the Commit, `-[IMPLEMENTS]->` both Components,
`-[VERIFIES]->` all four Tests; `Commit -[TOUCHES]->` both Components, the Spec,
and the packages `store/txn`, `store/recovery`).

`txnSeq` was decoded by recovery and NEVER written back, so a store reopened on a
non-empty WAL restarted at 0 and minted 1 again — one log holding two different
transactions under one sequence number, which is precisely what recovery's
suffix filter uses to tell one transaction's frames from another's. Measured:
seeded `[1 2 3 4]`, unseeded `[1 2 1 2]`.

DERIVED, NOT PERSISTED, and this is now the settled pattern for both counters in
this sprint (the other is rmp #2309's MVCC clock): the WAL already carries the
sequence in every frame, so a separate durable counter would be a second source of
truth that can disagree with the log after a torn tail. `MaxTxnSeq` counts frames
the replay DISCARDS and frames of an INCOMPLETE tail transaction — a sequence that
was minted is spent, and re-minting it would put an abandoned transaction and a
live one under one number.

A DEADLOCK THE PARTIAL FIX INTRODUCED, recorded because the obvious half of the
change is the half that hangs. Seeding the minting counter ALONE wedges the store:
`waitApplyTurn` parks until `appliedSeq == seq-1`, and the predecessor of a resumed
store's first transaction was applied by the PREVIOUS store instance, which no
longer exists to advance anything. `TestResumeTxnSeq_IsMonotoneAcrossReopen` HUNG
until `appliedSeq` was seeded too — and it would have hung in production on the
first write after every restart. Any future change touching one watermark must
touch both.

Incrementally synced at commit `263dad86` (2026-08-03, sprint 334 — **the
write-scaling instrument gate no longer reports a false NO-GO under load**):
+2 nodes (`Commit` `263dad86`; `Component` `requireAvailableParallelism`),
+3 edges.

A LESSON ABOUT THE GATES THEMSELVES, recorded because it undermines every
correctness claim that rests on them. `make ci` went red on
`TestWriteScalingInstrument_SeesConcurrency`, and the failure was the MACHINE:
that check drives a pure CPU-spin control with no WAL, no store and no graph in
it, so nothing in the sprint's changes can reach it. The two instruments are
load-sensitive in OPPOSITE directions despite `gate_test.go` calling one
load-immune — `measureScaling` (1 vs 8 writers) survives load and can even
INFLATE, while `measureSerialisationRatio` (8 free vs 8 under one mutex)
COMPRESSES, because under saturation the free arm collapses toward the serialised
one. Measured, same code and build: at load-average 18 inside
`go test -race ./...`, 8 free writers reached 50380/s against 1 writer's 41384/s
(**1.2× available parallelism**) and the ratio was **2.452× — a FAIL** against the
3.00× target; on a quiet machine, also under `-race`, 288181/s against 47249/s
(**6.1×**) gave **6.900×**. `requireCores` already skipped when the cores do not
EXIST; it cannot see whether they are FREE, and `make ci` runs this package inside
a parallel whole-module race run. `requireAvailableParallelism` probes with the
test's own workload and skips instead of returning a verdict the machine cannot
support. It cannot mask a real instrument defect, which would appear as a LOW
serialisation ratio while parallelism is HIGH — the case where the assertion still
runs.

Incrementally synced at commit `eeb7704e` (2026-08-03, task #2302, sprint 334 —
**the exclusive-build window is now an enforced contract**, audit finding **E21**):
+7 nodes (`Commit`; two Components — `AdjList.BeginExclusiveBuild`,
`AdjList.EndExclusiveBuild`; four Tests in
`graph/adjlist/exclusive_build_test.go`), +12 edges.

`AdjList.BeginCommit` and `BeginExclusiveBuild` mutate the SAME two plain fields,
`bulkOwner` and `bulkDepth`. They differ entirely in what licenses them: the
serving write path holds the graph's exclusive visibility barrier for the whole
window, while `store/recovery` and `store/bulkimport` take **no barrier at all**
and are licensed only by "the graph is not reachable by anyone yet". Both used to
call `BeginCommit`, so that second licence lived in a comment — and the audit's
point is that it must not be silently INHERITED once writers overlap at serving
time. Two distinct entry points now, plus an `atomic.Bool` that makes overlapping
them PANIC in either direction; every guard is pinned by a test that fails by
construction if the guard is removed.

A HAZARD FOR rmp #2304 THAT THIS SURFACED, recorded because the failure would be
silent: `AdjList.builderOwner` prefers `bulkOwner` **over** the writing
transaction's own record — deliberately, so a window's token cannot change
mid-window (the record is allocated lazily by the first version, and reading it
first re-clones the builder on every write; a test caught that on rmp #2301's
first draft). So the SERVING path's window currently **shadows per-transaction
ownership**: with `visMu` gone, two concurrent writers would both present the same
`bulkOwner` and reuse each other's private, UNPUBLISHED shard builders — one
mutating the other's slot array in place. #2304 must RETIRE the serving path's
window in favour of a token that travels with the write (`writeCtx.txID` already
exists), not merely delete `visMu` around it.

Also recorded: criterion 6 of #2302 needs no new instrument.
`BenchmarkWriteScaling_StoreAPI` already reports `commits/fsync`, and the store
path releases the semaphore BEFORE `SyncGroup`, so coalescing is reachable there —
the flat 268/s the audit measured is the *Cypher* path, serialised by `visMu`.

Incrementally synced at commit `2d11d717` (2026-08-03, task #2302, sprint 334 —
**rmp #2302 CLOSED: crash injection with concurrent writers (criterion 5) and the
group commit measured (criterion 6)**): +15 nodes (`Commit`; two `Spec` —
`docs/benchmarks/group-commit-coalescing-2026-08-03.md` and the previously
unmodelled `docs/design-wal-transaction-contiguity.md`; five `Function` —
`runConcurrentWriters`, `commitConcTxn`, `envInt`, `concBase` in
`cmd/crashinject-helper/main.go` and `parseSkip` in
`internal/crashpoint/crashpoint_enabled.go`; seven `Test`), +26 edges (12
`CONTAINS`, 5 `VERIFIES`, 4 `IMPROVES`, 5 `TOUCHES` — the `Sprint`→`Commit`
`CONTAINS` and the two `SPECIFIED_IN` included).

**A concurrent crash cannot be asserted against a shape, so it is asserted against
the child's own acknowledgements.** With many writers, which transactions had
committed when the kill landed is up to the scheduler. The helper's new
`runConcurrentWriters` scenario therefore prints one `ACK <id>` line per
acknowledged commit (written only after `Commit` returned nil, i.e. after that
transaction's frames and `OpCommit` marker were fsynced) and the parent holds
recovery to **Durability** (everything acknowledged is present and complete) and
**Atomicity** (everything present is present in full). Each transaction owns a
disjoint 3-node ring, `base = id*10`, which is the decision that makes
per-transaction completeness checkable independently: no transaction can supply or
hide another's state.

Two new PRODUCTION crashpoints in `store/wal/writer.go`, both elided to nothing
without the `gograph_crashinject` tag: `wal.appendrun.frame-emitted` inside
`AppendRun`'s emit (tears one transaction's frame run with `w.mu` held) and
`wal.sync.pre-datasync` in `leadGroupSyncLocked` between the `Flush` and the
`dataSync` (tears a group commit with followers parked on a watermark that will
never be published).

**`GOGRAPH_CRASH_AFTER` — a countdown, and not a convenience.** A breakpoint on a
hot durability path is reached by the process's FIRST commit, where a crash proves
nothing: nothing has been acknowledged, so "everything acknowledged survived"
holds vacuously. The countdown lets n hits through and kills on the (n+1)th. Prior
art read in source: SQLite's OOM simulator counts down the same way —
`memfault.iCountdown`, "Number of pending successes before a failure",
`faultsimStep()`, `faultsimConfig(nDelay, nRepeat)` (`sqlite/sqlite` @ `1b08739`,
`src/test_malloc.c:27,65-71,119-120`). Its off-by-one is the whole contract, so
`TestBreakpoint_Countdown` pins both sides.

**THE FACT MOST WORTH KEEPING: this battery does NOT detect audit finding E5
today, and NO LONGER as of rmp #2306.** While the store's capacity-one semaphore
was held across `store/txn.Tx.appendOnly`, contiguity was **over-determined** —
guaranteed twice — so replacing `AppendRun` with a loop of `Append` left the crash
tests PASSING and removing either guarantee alone changed nothing observable.
rmp #2306 retired that semaphore, so contiguity now rests **solely** on
`wal.Writer.AppendRun`. E5's primary detector is still the `store/wal` unit arm,
which drives concurrent appenders against the writer directly (a loop of `Append`
shatters a 25-frame transaction into 8 runs; `AppendRun` gives 1). **This battery becomes the E5 gate the moment
rmp #2306 retires the semaphore.** What it does detect was proven: altering
`Tx.Commit` to acknowledge without `SyncGroup` makes both tests fail with 44
durability violations naming the exact lost transactions. Also note `SIGKILL` does
not drop the OS page cache, so the mid-fsync point tests recovery over a
never-fsynced tail, not the loss of one.

**Criterion 6, measured** (`BenchmarkWriteScaling_StoreAPI`, Apple M4, no `-race`,
median of 5): `commits/fsync` 1.000 / 4.121 / 31.58 / 107.1 / 300.0 at 1 / 8 / 64 /
256 / 1024 writers — fsyncs per commit falls **300x**, monotone at every step,
throughput 263 → 78 667 ops/sec. `AppendRun` did not cost the group commit because
`SyncGroup` coalesces on **Sync**, not on Append. The contrast localises the
sprint's ceiling: the Cypher path over the same WAL is flat at **268 commits/s**
because it fsyncs inside `visMu`, which is rmp #2304's to remove.

DATA-QUALITY NOTES observed during this sync, not corrected here:
- Only **one** `Commit` node existed for sprint 334 (`d8847ce7`) although the sprint
  has ~12 commits, so the `Sprint`→`Commit` provenance layer is substantially
  behind for this sprint.
- `internal/crashinject/graph_shape_test.go` (task #2270) had **no** `Test` nodes at
  all; its two tests were added here as hygiene.
- `cmd/crashinject-helper/main.go` models only `main`, `run`, `runCheckpointCrash`
  and `runWALMidFrame`; the later scenario functions
  (`runCheckpointPrefixCrash`, `runRecoveryPromoteCrash`, `runConstraintDropCrash`,
  `runEdgeHandlePropCrash`, `runEdgeHandleDeleteCrash`) remain unmodelled.
- Test-file **helper** functions are not modelled as `Function` anywhere in the
  graph (confirmed by query), so this sync did not add the new test helpers
  (`checkConcurrent`, `concTxnFacts`, `concPresentFacts`, `parseAcks`,
  `runConcurrentCrash`, `assertViolations`, `runCountdownChild`). That is the
  graph's existing convention, not an omission.

Incrementally synced at commit `fc433015` (2026-08-03, task #2300, sprint 334 —
**adjacency write-write conflict detection, on the commutative rule**): +16 nodes
(`Commit`; three `Type` — `adjVersions`, `adjStamps`, `adjVersionShard`; one
`Function` — `adjEffective`; four `Method` — `adjVersions.checkAppend`,
`.stampAppend`, `.noteExclusive`, `Graph.addEdgeInfo`; seven `Test`), +15 edges
(12 `CONTAINS`, 3 `HAS_METHOD`, 7 `VERIFIES`, 3 `IMPROVES`, 1 `Sprint`→`Commit`).

**ADJACENCY COULD NOT USE THE RULE EVERY OTHER STORE USES.** Every other versioned
store keeps a per-object delta chain, so a writer asks whether it may displace the
head version. Adjacency keeps none — its only version signal is
`Graph.topoGeneration`, **one global counter**, which cannot distinguish "someone
changed node A" from "someone changed node Z". The standard rule would make every
writer conflict with every other writer that touched the graph at all.

**READING MEMGRAPH'S SOURCE INVERTED THE OBVIOUS ANSWER, and this is the entry
worth keeping.** `CreateEdge` does NOT call `PrepareForWrite`; it calls
**`PrepareForNonSequentialWrite`** on both endpoint vertices (memgraph/memgraph @
`b3ac3cd`, `src/storage/v2/inmemory/storage.cpp` → `src/storage/v2/mvcc.hpp`).
That returns `NON_SEQUENTIAL`, **not** `SERIALIZATION_ERROR`, when the head delta
is itself an edge creation, and says why in its own comment: *"if the entire
uncommitted delta chain is of edge creations … we can safely add a non-sequential
delta"*. So **two transactions appending arcs to the SAME vertex do not conflict** —
`ADD_OUT_EDGE`/`ADD_IN_EDGE` are commutative — and only a *blocking* delta
upstream (property, label, edge removal) is a serialization error. It matters more
in GoGraph than in Memgraph: on a power-law graph most arcs share few sources, so
conflicting on every append would serialise exactly the hot path the sprint exists
to open. **User decided the commutative rule.**

**The shape, with no delta chain to walk:** `adjVersions` keeps **two stamps per
node** across 64 shards — `appendTS` (commutative) and `exclusiveTS`
(non-commutative). An append consults only `exclusiveTS`; a removal / pair clear /
same-pair replacement consults **both**. **An append still RECORDS its stamp
although it never conflicts with another append** — without it,
`AddEdge(A→C)` followed by a concurrent `RemoveEdge(A→B)` is undetectable in that
order and the append is silently lost. That is what Memgraph gets free by linking a
delta whatever its action, and what `has_uncommitted_non_sequential_deltas`
discriminates.

**TWO DEFECTS THE TESTS CAUGHT IN THE FIRST DRAFT, both silent:**
- the append **stamped nothing when it created its own source node** — the id does
  not exist until after the insert, so `Lookup` failed and the stamp was skipped
  for **most of a bulk CREATE**, leaving those nodes invisible to a later removal's
  check. Check and record are now **separate**, for two different reasons: the
  check precedes the mutation so a doomed transaction writes nothing, the record
  follows it because the id is not there until then.
- `truncate`/`len` were written **unwired**, which lint reported as dead code — and
  that was not tidiness. Unwired, the stamp map grows one entry per node ever
  written transactionally and never shrinks: the leak shape rmp #2289 closed for
  direct writes. `truncate` now runs in `Graph.ReclaimNow` on the **same watermark
  as every other store**; `len` is exposed as `MVCCStats.AdjConflictStamps`.

`AdjConflictStamps` is reported **beside** `MVCCStats.Total`, not inside it: the
stamps hold no pre-image, are never read, and no reader can hold them back, so
folding them in would misreport the version memory a long read can pin.

**Every positive case was verified to FAIL** against a build with the check removed
(lost update, both transactions committing), **and re-verified after the
check/record restructure moved the guard**. The commuting row and the
disjoint-sources row keep PASSING under that defect, which is what proves they are
not "everything conflicts" tests. Both orders of append-vs-removal are covered
separately because the rule is asymmetric.

**STATE OF #2300 AFTER THIS:** detection now covers node existence, node labels,
node properties, adjacency and the five per-edge side stores — AC2's coverage gap
closed. It is at **parity** with the other stores, which means it is still
**unreachable from the Cypher engine**: `Graph.AddEdge` and `Graph.SetNodeLabel`
both pass `tx == nil`, only `beginLabelTx` carries a `writeCtx`, and the ambient
`Graph.writeTx` slot still encodes one-writer-at-a-time. No collision reaches a
Bolt client until rmp #2304. AC4 overlaps rmp #2318.

Incrementally synced at commit `eff6ca74` (2026-08-03, task #2303, sprint 334 —
**the count store's ordering basis, and a live defect under a wrong premise**):
+4 nodes (`Commit`; three `Test` in `graph/index/count/commutative_test.go`),
+7 edges (3 `CONTAINS`, 3 `FIXES`, 1 `Sprint`→`Commit`).

**A PREMISE OF MINE THAT WAS WRONG, AND THE TEST WRITTEN TO CONFIRM IT REFUTED IT.**
`count.Store.Apply` is `cell.Add(delta)` — an ADDITIVE delta, not an assignment —
and `MarkDirty` is a monotone set insert, so the first conclusion was that the count
store was already commutative and needed only a corrected contract comment.

It did not. `Apply` deleted a cell at **zero-OR-BELOW**, so a cell driven negative
was deleted, its negative value **discarded**, and the next increment recreated it
from zero — **permanently losing the decrement**. `-1` then `+1` on an empty cell
read **1** where `+1` then `-1` read **0**: the aggregate was order-sensitive, which
is precisely the dependency on writer exclusion rmp #2303 exists to remove.

**WHY IT SURVIVED.** Under the visibility barrier the base is always correct, so no
partial sum can go negative and the clamp is unreachable. The moment writers commit
concurrently, one transaction's decrements can land before another's increments and
the clamp silently eats them. Invisible to a green suite and to any single-writer
test.

**FIX:** delete at **exactly** zero and retain a negative cell — that is what makes
addition commute. Bounded growth unchanged: a negative cell is transient, reaching
exactly zero when its matching increment arrives, where it is deleted.

`MarkDirty` needed no change, checked rather than assumed: nothing clears an
individual entry (only a whole-store `Reset` does), and a concurrent test confirms no
writer's marking displaces another's.

**THE CONTRACT NOW SEPARATES TWO THINGS IT CONFLATED.** `exec.CountBuffer.Commit`
still must run where the count becomes visible atomically with the graph writes it
describes — a **visibility** requirement, which rmp #2304 must preserve by other
means. It does **not** rest on the barrier imposing a total order across committers.
The intra-transaction order (every delta, then every dirty mark) is a property of the
buffer and survives concurrency, because one buffer belongs to one transaction.

Differential: restoring the `<= 0` clamp fails
`TestCountStore_ConcurrentDeltasReachZeroFromEitherOrder` with the message it
carries. The wrong reasoning is recorded in the test file rather than quietly
replaced.

**#2303 REMAINS OPEN** on its other three structures: the secondary-index batch
publish (`cypher/exec/index_writeback.go`), the deferred label-index removal
(`graph/lpg/mvcc_index.go`), and the `constraintActive`/`indexActive` gates
(`graph/lpg/lpg.go:451,463`, read by `store/checkpoint/checkpoint.go:867`).

Incrementally synced at commit `e69d1974` (2026-08-03, task #2303, sprint 334 —
**the secondary-index batch publish's ordering basis**): +4 nodes (`Commit`; three
`Test` in `graph/index/manager_concurrent_batch_test.go`), +6 edges (3 `CONTAINS`,
2 `IMPROVES`, 1 `Sprint`→`Commit`).

`index.Manager.ApplyBatch` is called at the transaction boundary from
`exec.IndexBuffer`, today inside the write visibility barrier so exactly one batch is
ever in flight. **Tested before asserted this time** — and it holds. The basis has
three parts:

1. **Within a batch**, mutation order is preserved: one `IndexBuffer` belongs to one
   transaction and `ApplyBatch` walks the slice in order. This is the half
   concurrency cannot help with, because the Manager's own subscriber contract
   **exempts same-property-key changes from order-independence** — they carry
   old→new payloads.
2. **Across concurrent batches**, every sub-index operation takes its own lock.
3. **The same-property-key case cannot ARISE** between two concurrently-committing
   transactions: both writing a property on one node is a write-write conflict
   (`graph/lpg`'s node-property store, rmp #2300), so one aborts before it reaches a
   buffer.

**Part 3 is a DEPENDENCY on another package, not a property of `graph/index`**, and
the test file records it so the coupling is visible from both ends: if node-property
conflict detection were removed, the index could diverge from the graph with nothing
in `graph/index` to catch it.

**BOTH DIFFERENTIALS RUN, AND THE FIRST FOUND A DEFECT IN THE TEST ITSELF.** The
order-preservation sequences were `add/remove/add` and `remove/add/remove` —
**PALINDROMES, identical under reversal** — so the test PASSED against a build with
`ApplyBatch` deliberately iterating the batch BACKWARDS. Only running the defect
exposed it. Now asymmetric (`add/add/remove`, `remove/remove/add`) and both fail on
reversal. Removing `label.Index.Add`'s internal lock kills
`TestManager_ConcurrentApplyBatchLosesNothing` with `fatal error: concurrent map
writes`, so part 2's lock is demonstrably load-bearing.

**#2303 REMAINS OPEN on two of four structures:** the deferred label-index removal
(`graph/lpg/mvcc_index.go:49-75`) and the `constraintActive`/`indexActive` gates
(`graph/lpg/lpg.go:451,463`, read by `store/checkpoint/checkpoint.go:867`).

Incrementally synced at commit `a00cfae8` (2026-08-03, task #2303, sprint 334 —
**the deferred label-index removal is charged to its own transaction**): +5 nodes
(`Commit`; `Graph.deferralStamp` Method; three `Test` in
`graph/lpg/mvcc_index_ordering_test.go`), +9 edges (4 `CONTAINS`, 3 `FIXES`,
1 `Sprint`→`Commit`).

A label removal is deferred until the reclamation watermark has passed it, because a
reader older than the removal must still find the node in the bitmap or it silently
loses a row. "Passed it" needs an instant, and `deferLabelIndexRemoval` took that
instant from the graph's **ambient `mvcc.WriteStamp` slot** rather than from the
removing transaction. Same defect class as audit finding **E3**, which rmp #2301
closed for the commit record and the version count; this path was the last reader of
the ambient slot.

**RUNNING THE DIFFERENTIAL CORRECTED THE DESCRIPTION OF THE DEFECT — and the wrong
description had already been written into the test file before it was checked.** The
claim "verified against the previous behaviour: the entry survived the sweep and the
test failed" was **false**; the test passed. There are TWO paths and they fail
differently:

- **Barrier path** (`Graph.beginWrite`): the ambient slot IS occupied, so the old read
  returned the ambient transaction's record. Wrong once writers overlap — **but the
  barrier is what guarantees the slot has one occupant, so no test can produce the
  collision until rmp #2304 removes it.** AC3 is therefore **not** satisfied for that
  half and the file says so. Same situation as rmp #2300's AC5.
- **labelTx path**: `Graph.beginWriteCtx` does **not** publish to the ambient slot, so
  the old read fell through to `WriteStamp.Stamp`'s **untransacted** branch — which
  allocates a commit timestamp **and publishes it immediately**. Worse, and
  observable: the removal became reclaimable at an instant **before its own
  transaction had committed or aborted**, so a rolled-back statement's label strip
  could have its deferral swept, deleting an entry the undo legitimately restored
  (the exact hazard `deferredIdx`'s keyed-not-list design exists to prevent). It also
  accounted every deferral as an **untracked** write — the figure the substrate
  reports for precisely the opposite thing.

The second defect is fixed and gated: restoring `g.stamp.Stamp()` makes
`TestDeferredIndexRemoval_ChargesNoUntrackedWrite` fail with **3 untracked writes for
3 in-transaction deferrals**.

`Graph.deferralStamp` resolves the instant: the transaction's own record and id when
there is one, the ambient stamp otherwise. **A nil tx is correct to fall back** — such
a write commits the instant it is made — and
`TestDeferredIndexRemoval_UntransactedWriteStillSweeps` pins it, because a deferral
that never swept would leak an over-reporting bitmap entry for the life of the
process.

**#2303 REMAINS OPEN on the last of four structures:** the
`constraintActive`/`indexActive` gates (`graph/lpg/lpg.go:451,463`, read by
`store/checkpoint/checkpoint.go:867`).

Incrementally synced at commit `ced6f400` (2026-08-03, task #2303, sprint 334 —
**the constraint/index gates are DERIVED, not mirrored; #2303's implementation
complete**): +7 nodes (`Commit`; `Graph.SetIndexCountSource` and
`Graph.SetConstraintCountSource` Methods; `derivedCount` Function; three `Test` in
`graph/lpg/mvcc_gate_derived_test.go`), +11 edges, and **2 nodes REMOVED** —
`Graph.SetActiveIndexCount` / `SetActiveConstraintCount` no longer exist.

Both gates were an `atomic.Int64` the engine **stored a separately-read registry count
into**, documented as correct because the engine held its single-writer lock. Accurate,
and exactly the dependency rmp #2303 exists to remove: a store of a value the caller
read earlier is a lost update as soon as a second writer exists. With one index
registered — A drops it (registry 0, A reads 0), B creates another (registry 1, B reads
1, stores 1), A stores 0 → **gate 0, registry 1**.

**Under-reporting is the dangerous direction**: the checkpointer's phase-3
self-sufficiency re-check consults `HasIndexes` to decide whether the WAL prefix holding
a `CREATE INDEX` may be truncated, and a false answer truncates it — the index is
silently gone on reopen. That is #1755's defect, resurrected by concurrency.

**Derived, not maintained** — the task's own preferred answer and strictly the stronger
of the two it offered. The graph holds a `func() int64` and calls it when the question is
asked: no stored value to go stale, no window, no ordering requirement on the caller, and
**no lock added**, which AC4 requires. The discriminating property is that the gate tracks
its source with **nothing having notified it**, which a stored mirror cannot do;
restoring the mirror fails two of the three tests. The churn test asserts
**one-directionally** on purpose — over-reporting is safe (a retained prefix), under-
reporting loses data.

**GATE STATUS, STATED RATHER THAN GLOSSED.** `tidy fmt vet build test-short lint` green
(119 packages, TCK green). **`make cover-gate` FAILS — and it fails on the PRE-CHANGE
tree too**, with a *different* load-sensitive test. Verified by running the full
cover-gate on the stashed pre-change tree, not by inspection: my run tripped
`bench/mvccwrite` `TestWriteScalingInstrument_SeesConcurrency` (ratio 2.432× vs a 3.00×
target, with available parallelism logged at 13.63× so `requireAvailableParallelism` did
NOT skip); the pre-change run tripped `store/wal`
`TestAppend_LoopInterleavesUnderConcurrency`. Both pass quiet. **Coverage instrumentation
compresses these ratios while available parallelism still probes high, so the existing
guard's threshold never fires** — the "differential is degenerate below a gate's floor"
pattern. Filed as **rmp #2319** (BUG, p7/sev6, added to sprint 334) rather than worked
around.

**#2303's four structures are all implemented:** count store (`eff6ca74`),
secondary-index publish (`e69d1974`), deferred label-index removal (`a00cfae8`), and
these gates. Its AC3 is fully satisfied for three of them; for the deferred removal's
BARRIER-path half it is unsatisfiable until rmp #2304 removes the barrier, which
`graph/lpg/mvcc_index_ordering_test.go` records.

Incrementally synced at commit `525e209a` (2026-08-04, task **rmp #2320**, sprint 334 — the
write path CARRIES its transaction, and the ordinary write path moves to the SHARED
bracket). +9 nodes: `Commit` `525e209a`; `Task` 2320; three `Type`s (`mvcc.Tx` in
`graph/mvcc/tx.go`, `lpg.WriteView` in `graph/lpg/writeview.go`, `adjlist.Writer` in
`graph/adjlist/writer.go`); four `Finding`s
(`undo-refused-by-doomed-test-2320`, `remove-edge-by-handle-ambient-resolution-2320`,
`view-plus-unversioned-read-is-not-atomic-2320`,
`self-conflict-from-contiguous-frontier-2320`, all status `FIXED`); one `Perf`
(`mvcc-write-scaling-2320`). +14 edges: 3 `CONTAINS` (package→type), 8 `TOUCHES`
(commit→`mvcc`/`lpg`/`adjlist`/`cypher`/`txn` packages and the three new types),
4 `FIXES` (commit→finding), 1 `MEASURES` (commit→perf), 1 `IMPLEMENTS` (commit→task),
1 `CONTAINS` (sprint 334→task 2320).

**The shape, cited.** Memgraph at commit `572d5b4311a279de550522344a6f10d352d11c48`
(branch `master`, read 2026-08-03) uses BOTH halves and so does GoGraph: an ACCESSOR
holding the transaction (`memgraph::storage::Accessor`'s `Transaction transaction_`,
`src/storage/v2/storage.hpp`, exposing `virtual VertexAccessor CreateVertex() = 0`) and an
explicit PARAMETER in the storage primitives (`PrepareForWrite(Transaction *, TObj *)` and
`CreateAndLinkDelta(Transaction *, TObj *, Args &&...)` with
`transaction->EnsureCommitInfoExists()`, `src/storage/v2/mvcc.hpp`). `mvcc.Tx` is the
parameter; `lpg.WriteView` and `adjlist.Writer` are the accessors. Embedding `*Graph` in
`WriteView` was considered and REJECTED: zero call-site churn, but it exposes the whole
graph through a value whose responsibility is one transaction's writes, and a mutator added
to `Graph` and not to `WriteView` would fall through to the ambient path silently.

**`lpg.Graph.View` is now the SCHEMA barrier and nothing more.** With ordinary writes on the
shared hold, `View` plus an unversioned accessor gives a reader NO data isolation — 7040 to
20 011 partial-transaction observations out of ~11 M reads, against ZERO out of 6 488 034
snapshot reads. A caller needing a consistent view of DATA takes a snapshot
(`BeginRead`/`ReadAt`). Both halves are pinned, the second by a NEGATIVE CONTROL
(`txn.TestIsolation_ViewWithUnversionedReadIsNotAtomic`), so the relocation of the
guarantee cannot drift back unnoticed. The three remaining `View` callers were each
assessed and documented in the method's godoc: the checkpointer rests on
`RunUnderCommitLock`'s in-flight drain (verified — both write paths sit inside that window
for their whole apply), the statistics build is approximate by contract, and the constraint
pre-validation scan is backstopped by post-registration enforcement.

**New observability:** `lpg.Graph.AmbientVersionResolutions` counts versions that resolved
their transaction through the graph-wide slot instead of carrying it. It is free on the
threaded path — only the ambient branch increments — so "zero ambient resolutions across
the write surface" is a gate rather than a claim, and all four new gates were verified to
FAIL against a deliberately bypassed build.

**A decoy-based gate at the Cypher level was written and DISCARDED**: it passed against the
bypassed build, because the engine's own bracket publishes to the ambient slot AFTER any
decoy opened before the statement, so the resolution lands on the right transaction by
accident. The interleaving is only reproducible one layer down, in `lpg` —
`TestWriteView_SecondWriteDoesNotAdoptAnOverlappingTransactionsRecord`.

**Gate ratcheted:** `bench/mvccwrite` `walWriteScalingFloor` 0.90 → `writeScalingTarget`
(3.0). The two store-LESS floors stay put: that arm's outermost lock is
`cypher.Engine.writeMu`, which rmp #2306 owns.

Incrementally synced at commits `fdd91c6b` and `a04a8946` (2026-08-04, task **rmp #2304**,
sprint 334 — the exclusive visibility barrier is retired from the ordinary write path,
completing what `525e209a` delivered). +3 nodes (two `Commit`s, `Task` 2304), +5 edges
(2 `IMPLEMENTS`, 1 sprint `CONTAINS`, and the provenance stamps).

**#2303's AC3 is now discharged**, by `graph/lpg/mvcc_index_overlap_test.go`. It could not
be satisfied while the barrier existed, because the barrier was exactly what guaranteed the
ambient stamp slot had one occupant; with the barrier gone the collision is producible, and
both new tests FAIL against `g.stamp.Stamp()` naming the concurrent writer's record.

**One acceptance claim was corrected rather than implemented.** #2304's AC8 states that a
deferred removal charged to a still-in-flight transaction "carries an id no record will ever
publish and is NEVER swept". Measurement says otherwise: both the threaded and the ambient
resolution store a `*mvcc.CommitInfo` and `lifeStamp.at()` resolves through it, so such a
removal is swept at the WRONG writer's instant rather than stranded. Stranding needs
`lifeStamp.info` nil with an in-flight id in `lifeStamp.ts`, which `Graph.deferralStamp`
yields only for a transaction whose window is already retracted — unreachable while its
bracket is open. The harm is mis-ORDERING, and the second test measures it in both
directions.

**#2304's AC2 could not be met on the arm its own baseline came from, and the premise was
refuted.** The sprint description says the 0.83× entry figure at sixteen writers was taken
"with no WAL attached, so the ceiling is the barrier and nothing else". That arm's ceiling is
`cypher.Engine.writeMu` — `lockWriter` takes it for the whole statement whenever no store is
attached — exactly as `docs/audit-mvcc-sole-cc-2026-08-02.md` §3.1 already said. Measured
after the flip: store-less 0.750× at sixteen writers (the fall is in the NUMERATOR: one
writer +6.3 %, sixteen writers −3.6 %), WAL 1.009× → 7.886×. The store-less target and the
ratchet of `writeScalingFloor`/`writeConcurrencyFloor` were therefore CARRIED INTO rmp
#2306's AC1, with the refuted premise recorded there.

**Reader latency did not regress:** `bench/mtaudit` `TestFairScheduling` with `-tags=soak`,
no `-race`, no competing load — collapse 1.903× at one reader and 1.972× at eight, against a
4.0× tolerance and the 1.91×/2.07× envelope certified when rmp #2274 fixed the starvation.

**rmp #2306 gained an obligation it did not have before:** with ordinary writes on the shared
barrier, the checkpointer's transaction-boundary consistency rests ENTIRELY on
`store/txn.Store.RunUnderCommitLock`'s in-flight drain and no longer on `visMu`, so whatever
replaces that drain must preserve the property (or rmp #2310 must land first). Recorded in
#2306's AC4.

Incrementally synced at commit `7ab8bbc3` (2026-08-04, task **rmp #2300**, sprint 334 — a
refused transaction ABORTS instead of publishing, and the object it aborted on stays
writable). +4 nodes (`Commit` `7ab8bbc3`, `Task` 2300, two `Finding`s), +4 edges
(1 `IMPLEMENTS`, 2 `FIXES`, 1 sprint `CONTAINS`).

**Two HIGH findings, the second created by fixing the first.**

1. `refused-transaction-published-its-record-2300` — every write bracket published its
   commit record, including a transaction refused by conflict detection, so the caller was
   told the transaction failed and part of its work was visible to a fresh snapshot. An
   ATOMICITY violation. It had been invisible because the Cypher engine's undo log
   physically restores the stored value, and that undo log is a `cypher` structure —
   absent for a caller using `lpg.Graph.ApplyVersioned` directly, which `store/txn`'s
   apply is. **Rollback and abort are different things**, and `mvcc_write.go`'s
   publication-on-rollback rationale covers only the first.
2. `aborted-head-made-the-object-unwritable-2300` — marking the record `mvcc.AbortedTS`
   fixed visibility and broke LIVENESS, because AbortedTS sits above `mvcc.TxIDBase` and
   the plain "conflict = not visible" rule then refused every later writer to that object
   forever. `make ci` caught it within minutes: examples/27's writers exhausted a
   nine-attempt retry chain on the first account any transfer aborted on.

**`mvcc.Conflicts` is therefore NOT the plain negation of `mvcc.Visible`, and the one
asymmetry is deliberate.** An aborted head stays INVISIBLE — a reader must undo it to
reach the pre-abort value — while being freely OVERWRITABLE, because displacing a
transaction whose changes can never be seen loses no update. Memgraph needs no exemption:
`InMemoryStorage::InMemoryAccessor::Abort`
(`src/storage/v2/inmemory/storage.cpp` at `572d5b4311a279de550522344a6f10d352d11c48`)
UNLINKS the transaction's deltas from every chain it touched. Unlinking is rmp #2318's;
when it lands the exemption becomes unreachable rather than wrong.

**An existing test asserted the opposite and measurement refuted it.**
`TestConflicts_TheFourCases` wanted `true` for an aborted head, reasoning "an aborted
version is not visible, so it is not overwritable through this path either". The case now
carries the measurement, and the asymmetry is asserted directly instead of being an
exclusion from the negation check.

**AC4's reclamation half is NOT delivered and is pinned as such.** An `AbortedTS` head can
never satisfy `at() <= watermark`, so the versions are retained for the life of the
process — rmp #2318. `TestAbort_VersionsAreNotYetReclaimable_2318` asserts the CURRENT
behaviour and its failure message says that reclaiming them is good news to be inverted,
not a regression to restore.

**No engine-internal retry for autocommit was added**, and the reasoning is recorded: a
client must retry, which is the contract every MVCC engine imposes, and the Bolt boundary
already returns a code the real driver retries. The load-bearing detail is that a retry's
backoff must be sized to a WAL FSYNC and not to a scheduler yield — a `runtime.Gosched`
loop was measured burning five consecutive attempts inside one fsync, all against the same
in-flight head with `startTS` frozen, because the contiguous frontier cannot advance while
that commit is still syncing.

Incrementally synced at commit `bf0414dc` (2026-08-07, task **rmp #2333**, sprint 334 — the
torn-total gate could not say what it had seen). **First modelling of
`examples/27_concurrent_txn` at all:** the graph's example coverage stopped at example 25,
so this adds +8 nodes (`Package` `examples/27_concurrent_txn`; `Commit` `bf0414dc`; six
`Test`s — `TestMain`, `TestRun`, `TestRunReproducibleAcrossReaderScaling`,
`TestTornGate_CatchesADeliberateTear`, `TestAsInt64_RejectsNull`, and the soak-layer
`TestSoak_TornTotalSearch`) and +8 edges (6 `CONTAINS`, 1 `TOUCHES`, 1 `FIXES` to the
`ACID Transactions` Feature, id 9736).

**The `FIXES` edge carries a caveat property, because rmp #2333 IS NOT CLOSED.** What was
hardened is the ACID isolation INSTRUMENT, not the engine: the torn total observed once on
2026-08-06 remains unreproduced, and no engine code was changed. Recording this as an
ordinary fix would have misrepresented an open ACID question as a settled one.

**Two of the ticket's own premises were refuted and both refutations are cheap to re-run.**
The delta 941758 is not any of the 600 planned transfer amounts (range [2289, 998780]), so
it is not "one debit without its credit"; and the failing run took 2.29s, while every
CPU-starved arm takes 3.41–5.21s, so starvation was not the trigger. The closest-matching
arm is the COVERAGE pass of `make ci` (`-coverpkg=./... -covermode=atomic`, no `-race` —
`make ci` runs the suite TWICE), which the earlier search never targeted; 400 runs there
(~47M observations) stayed clean.

**New Test properties introduced by this sync**, all optional and additive:
`kind` (currently only `negative-control`), `layer` and `buildTag` (currently only `soak`),
`budgetEnv`, and `verifies` (a prose statement of the property a test pins). `Package`
gains `role`, and `Commit` gains `subject`. Node identity is unchanged: `Package` by
`path`, `Test` by `(name, file, pkg)`, `Commit` by `hash`.

Incrementally synced at commit `0b8b8145` (2026-08-07, task **rmp #2349**, sprint 335 — an
acked commit was lost in the post-fsync-pre-publish window). **First modelling of sprint
335 at all**, and of the `mvcc.Clock` Type. +10 nodes (`Sprint` `335` OPEN; `Commit`
`0b8b8145`; `Feature` `Durable checkpoint instant boundary`; `Type` `mvcc.Clock`; `Method`s
`Clock.AwaitQuiescent`, `Graph.AwaitCommitQuiescence`, `Checkpointer.awaitCommitQuiescence`;
`Test` `TestCheckpoint_EngineCommitOrdering_KeepsAnAckedCommit`; `Benchmark`
`BenchmarkCheckpointUnderWriters`; `Spec`
`docs/benchmarks/checkpoint-instant-boundary-2026-08-07.md`) and +19 edges
(`Sprint 335 -[CONTAINS]-> Commit`; `Sprint 335 -[DELIVERS]->` the new Feature; three
`FIXES` from the Commit to `ACID Transactions`, `WAL & Recovery` and the new Feature; four
`TOUCHES` to Packages `store/checkpoint`, `graph/mvcc`, `graph/lpg`, `bench/mvccwrite` plus
one to the Spec; six `CONTAINS`/`HAS_METHOD` for the new symbols; two `VERIFIES` to the new
Feature; one `SPECIFIED_IN`; one `IMPLEMENTS` from `store/checkpoint`).

**THE REFUTED PREMISE, recorded because it was load-bearing for an ACID argument.** The
comment in `store/checkpoint/checkpoint.go` phase 1 argued that the durable watermark and
the MVCC instant always describe the same transactions because "a writer's registration
spans its WHOLE commit — `store/txn.Tx.Commit` defers `exitWriter` past `ApplyVersioned`,
which is what publishes the instant". **That is TRUE for `txn.Tx.Commit` and FALSE for
`txn.Tx.CommitWALOnly`, which is the path the Cypher engine — the only production writer —
actually takes.** `CommitWALOnly` applies nothing in memory and publishes no instant, so its
`exitWriter` fires when the fsync returns while the instant is published later, at write-
bracket unwind through `lpg.Graph.endWrite`. The store's in-flight count is zero inside that
window, so the drain proved nothing and the checkpoint truncated away the only record of an
acknowledged commit. Anyone reasoning about the quiesce boundary must check BOTH commit
paths; the drain alone does not cover the publish.

**The fix waits on the OBSERVER, and the prior art disagrees with itself, which is the
argument.** PostgreSQL (commit `b5978350`) has the identical decoupling and names it at
`src/backend/access/transam/xlog.c:7684-7687`; its remedy is `DELAY_CHKPT_IN_COMMIT` plus a
checkpoint-side wait (`xact.c:1469-1471`, `:1577-1582`; `xlog.c:7695-7712`). Memgraph
(commit `b3ac3cdc`) instead loads the start timestamp and the last durable timestamp under
one `engine_lock_` acquisition (`src/storage/v2/inmemory/storage.cpp:2833-2844`) so the pair
is consistent by construction — which works only because its commit publishes durability and
visibility under the same lock, the convoy rmp #2302/#2193 removed here. GoGraph has
PostgreSQL's decoupling, so it took PostgreSQL's remedy.

**KNOWN FIDELITY GAP, recorded rather than silently fixed.** The `graph/mvcc` Package's
symbol inventory is stale: before this sync it held only the `Horizon` and `horizonOcc`
Types, though the package now also declares `Clock`, `WriteStamp`, `commitLog`, `Depths`,
`WriteCounts` and more. Only `Clock` was added here, because that is what this task read and
attached to. A full re-survey of `graph/mvcc` (and, likely, of every package that grew during
sprints 334–335) is owed.

Incrementally synced at commit `b1f0974f` (2026-08-08, task **rmp #2354**, sprint 336 — an
acknowledged `COMMIT` could apply NOTHING). **First modelling of sprint 336 at all.**
+13 nodes (`Sprint` `336` OPEN; `Task` `2354` COMPLETED; `Commit` `b1f0974f`; `Method`
`WriteTx.Err`; 8 `Test`s in `cypher/constraint_label_conflict_test.go`) and +2 `Finding`
nodes (below), with +26 edges (`Task -[BELONGS_TO]-> Sprint`, `Sprint -[CONTAINS]->`
Task and Commit, `Task -[IMPLEMENTED_IN]-> Commit`; three `FIXES` from the Commit to
`ACID Transactions`, `MVCC snapshot isolation` and `MVCC as sole concurrency control`;
two `TOUCHES` to Packages `lpg` and `cypher`; four `TOUCHES` to Methods
`WriteTx.Err`, `Result.commitUnderBarrier`, `Result.Err`, `ExplicitTx.Commit`; three
`TOUCHES` to Types `Result`, `WriteTx`, `ExplicitTx`; eight `CONTAINS` to the new Tests;
two `PRODUCED` and two `FOUND` to the Findings).

**THE DEFECT, and why it is a repeat.** `graph/lpg/lpg.go setNodeLabelInfo` guarded BOTH
the write-write conflict test AND the delta on `!bag.has(lid)`, where `bag` is the RAW
stored bag read by value — so it already carried other in-flight transactions' eager
writes. A peer's UNCOMMITTED add of the same label made `has()` true, skipped the whole
block, and made `bag.add` a no-op: the write reported SUCCESS having recorded nothing, and
the label vanished with the peer's rollback. **This is the identical defect rmp #2324 fixed
for the node-PROPERTY store**, whose own comment in `graph/lpg/property.go` records the
same reasoning ("only a write that RECORDS a version can conflict") failing there at 400
concurrent increments producing 216. `removeNodeLabelInfo` carried the mirror guard and one
case worse: a peer's eager removal of the node's LAST label deleted the shard's bag entry
outright, so the `has()` guard was not even reached. `nodeLabelShard.headStamp` reads the
DELTA CHAIN rather than the bag, so it answers correctly for a node with no bag entry at
all. The conflict test now runs UNCONDITIONALLY on both directions; only the DELTA stays
guarded, which is what keeps a delta per WRITE rather than per statement.

**The backstop exists in TWO places because two paths drive the write bracket themselves**
— `cypher/exectx.go ExplicitTx.Commit` and `cypher/api.go commitUnderBarrier` (autocommit),
the latter via the new `Result.conflictErr` field. A conflict hit by a primitive that cannot
RETURN one (a label removal, a property delete, any of the five per-edge side stores) is
recorded on the transaction and surfaces nowhere unless asked for, which is what
`lpg.WriteTx.Err` is for. Memgraph reads the same record in the same place —
`Storage::Commit` tests `transaction_.must_abort`. Both halves are now pinned by tests; the
autocommit half previously had none, and it is the half the common caller uses.

**TWO FINDINGS, both retired or bounded by measurement rather than argument.**
`Finding` *The tx-visible hadLabel hardening is UNREACHABLE…* records that #2354's own
technical requirement — resolve the adapter pre-check `hadLabel` through
`HasNodeLabelInTx` instead of the raw graph — was implemented in a worktree and measured
NEITHER needed NOR harmful: any raw/view divergence implies a foreign head on the node's
delta chain, which the now-unconditional conflict test dooms before any constraint is
consulted. `hadLabel` stays the RAW read, because that is the read the PHYSICAL undo
journal requires. `Finding` *The undo replay DISCARDS the error…* records that
`cypher/undo_record.go` swallows the error from its replayed inverses
(`_ = m.wv.SetNodeLabel(...)`), so an INCOMPLETE rollback reports success; unreachable in
the shipped build only because the raw read keeps the journal to inverses this transaction
really made over a head it still owns.

**COST, measured with interleaved arms (n=10, benchstat) because across-time comparison on
this host is worthless.** `BenchmarkWriteScaling/mem` at writers 1/4/16/32: allocs/op
IDENTICAL at every count, sec/op geomean +0.24%, B/op +0.59%. The B/op delta was
ATTRIBUTED rather than dismissed as size-class noise: `unsafe.Sizeof(cypher.Result)` went
384 → 400 bytes for the new field, crossing Go's 384 → 416 size class, so the allocation
COUNT did not change — only its class. The three commit-failure fields on `Result`
(`conflictErr`, `notNullErr`, `walErr`) are strictly mutually exclusive, so collapsing them
lands at ~368 bytes, BELOW the pre-#2354 baseline; filed as rmp **#2364**.

Incrementally synced at commit `46a8505b` (2026-08-08, task **rmp #2353**, sprint 336 —
write skew across two substores committed a state violating a declared existence
invariant). +10 nodes (`Task` `2353` COMPLETED; `Commit` `46a8505b`; `Type`s
`constraintVersions`, `constraintStamp`, `constraintVersionShard`; `Method`
`WriteView.NoteConstraintTouch`; 5 `Test`s in `cypher/constraint_writeskew_test.go`) plus
1 `Finding`, and +15 edges (`Task -[BELONGS_TO]-> Sprint`; `Sprint -[CONTAINS]->` Task and
Commit; `Task -[IMPLEMENTED_IN]-> Commit`; `Task -[FOUND]-> Finding`; `Commit -[FIXES]->`
the Finding and the `ACID Transactions` / `MVCC snapshot isolation` Features; three
`TOUCHES` to Packages `lpg`, `cypher`, `mvcc`; three `CREATES` to the new Types; one
`TOUCHES` to the new Method; five `CONTAINS` to the new Tests). New store name
`mvcc.StoreNodeConstraint`, and a new `MVCCStats.ConstraintStamps` series.

**THE DEFECT.** A NOT NULL invariant binds a LABEL to a PROPERTY, but conflict detection
is per SUBSTORE, so the two halves never met: `T1: REMOVE n.email` and `T2: SET n:Acct`
both committed and left a node carrying `:Acct` with no `email`. Neither transaction is
wrong on its own snapshot — T1 sees no constrained label so it has nothing to check, and
T2's snapshot predates T1's commit so it still sees the property. Textbook write skew,
which plain Snapshot Isolation permits by definition and which this project's CONSISTENCY
mandate does not. `cypher/constraint_check.go` already NAMED the mechanism without closing
it. The premise was re-verified at HEAD before any work, because rmp #2354 had changed
label-path conflict detection after the task was written.

**THE SHAPE, and why it is scoped.** Node granularity is what every reference engine uses
here: PostgreSQL and InnoDB version the whole ROW so both halves share one tuple version;
Memgraph links every write onto a single delta chain per vertex
(`src/storage/v2/vertex.hpp`); Neo4j locks the node. Taking that wholesale was the REJECTED
option, because it raises the conflict rate for every workload including the majority that
declare no existence invariant and cannot suffer the anomaly. So `constraintVersions`
applies node granularity ONLY to nodes a declared existence invariant covers, hooked into
the `mutationUndo.touch` seam whose set already exists only when the registry reports one.
Test and stamp share ONE critical section — split, two transactions both find the slot
empty and both proceed, which is the anomaly itself. Swept by BOTH reclamation paths, since
a stamp left at `mvcc.AbortedTS` refuses every later writer on that node forever.

**A SECOND, SEPARATE DEFECT, found while measuring the first** and recorded as its own
`Finding`: a label scan yielded a ROW for a node whose `labels(n)` did not include the
label. `Graph.setNodeLabelInfo` adds to the bitmap IMMEDIATELY while the undo of that add
had its removal DEFERRED and then discarded by `Graph.withdrawAbortedIndexRemovals` —
right for new work, wrong for withdrawing the transaction's own add.
`Graph.deferLabelIndexRemoval` now declines to defer while unwinding, the exemption
`writeCtx.undoing` already grants the conflict test. The reproducing boundary was MEASURED
and is narrower than first written: only the serialization-conflict refusal path reaches
it; six existence-check refusal shapes are clean.

**COST, and the regression the measurement caught.** AC 5 required an unscoped schema to
show no measurable change, and the first run refuted it: `+1 allocs/op` at one writer
(p=0.000), surviving a fixed-`b.N` re-run so not an amortisation artefact. Bisected into
FOUR interleaved arms, the cost was NOT on the write path — an arm carrying only the struct
field and the store type measured identical to base, and an arm without the `touch` call
site still showed it. It was the RECLAIMER: `-benchmem` attributes the vacuum goroutine's
allocations to the write benchmark, and the two new sweep calls walked 64 shards under 64
locks to discover an empty store. Gated on an `active` counter, following the
`idxPendingActive` precedent `withdrawAbortedIndexRemovals` already sets. After the gate,
`BenchmarkWriteScaling/mem` at writers 1/4/16/32, interleaved n=14 with benchstat:
allocs/op IDENTICAL at every count (all samples equal, p=1.000), sec/op all flat (geomean
−0.27%), B/op geomean −0.70%. A structural control carries the claim where the benchmark
cannot: with no invariant declared `MVCCStats.ConstraintStamps` stays 0, and the same
workload with one makes it non-zero.

Incrementally synced at commit `496eeb96` (2026-08-08, task **rmp #2355**, sprint 336 —
the property path decided UNIQUE from a raw label read). +10 nodes (`Task` `2355`
COMPLETED and `Task` `2365` BACKLOG; `Commit` `496eeb96`; `Function` `exec.labelsInTx`;
`Method` `NodeLabelsInTx` on both mutator adapters; 5 `Test`s in
`cypher/constraint_unique_rawlabel_test.go`) plus 1 `Finding`, and +16 edges.

**THE DEFECT, and why it is the same family as #2353.** A UNIQUE constraint attaches to a
LABEL, so deciding what to reserve or release for a property write begins by asking which
labels the node carries — and every such site asked the RAW graph, which answers with the
newest stored value including other in-flight transactions' eager writes. Conflicts being
per SUBSTORE, a peer writing the LABEL never collided with this transaction writing the
PROPERTY, so a peer's uncommitted `REMOVE b:Person` made the node look unconstrained and
the value was written with no reservation. Two `:Person` nodes then shared a value declared
UNIQUE. The codebase already NAMED this class in `cypher/api.go` when rmp #2352 hardened the
LABEL path; the property path's ~26 sites were left on the raw read.

**THE CHOKE POINT.** `exec.labelsInTx` is the single helper all 27 constraint-decision sites
now route through, resolving via `txVisibleNodeReader.NodeLabelsInTx` and falling back to the
raw read only for a mutator carrying no transaction. ONE helper rather than 27 corrected
reads, deliberately: per-site drift is what left this path behind last time. The three
remaining raw label reads in `cypher/exec` are MERGE MATCH semantics
(`merge_search.go:100,217`, `merge_pattern.go:754`), deliberately untouched and filed as
**#2365** with instructions to REPRODUCE before fixing.

**THE TICKET'S SECOND HALF WAS MISDIAGNOSED**, and the `Finding` records the correction. The
leaked reservation was attributed to the property writer releasing nothing; it does release.
The cause is the ABORTING transaction: its undo re-adds the label and re-reserves the value
IT sees — the pre-image, since the peer's new value is invisible to it — landing AFTER the
peer already released it, so the value ends up reserved with no live holder. Established by
ORDERING rather than by reading: rollback after the peer write leaks, before does not, no
peer does not. The mirror leaks separately: with the removal COMMITTED the node is no longer
a member yet the newly written value stays reserved. Both are availability defects rather
than consistency ones, which is why they were assessed apart from the duplicate.

**THE STAMP WAS EXTENDED TO UNIQUENESS** (user decision, 2026-08-08) rather than the undo
ordering patched: both kinds of invariant bind a label to a property, so both need the two
substores to collide, and node granularity is what the reference engines use.
**Widening the GATE alone was measured to be a no-op** — neither a label LOSS nor a property
SET was a stamping site, because the original sites cover only writes that can introduce a
MISSING property. Two new sites were required (`recordRemoveNodeLabel`,
`recordSetNodeProperty`), and the stamp now carries its OWN gate
(`mutationUndo.stampCon`, set from `HasAnyNotNull() || HasAnyUnique()`) instead of riding on
`touched`, which stays existence-only so a uniqueness-only schema does not pay the
commit-time existence scan. The conflict-surface increase is bounded by
`TestUnique_DisjointNodesUnderUniqueDoNotConflict`.

Incrementally synced at commit `d855f602` (2026-08-08, task **rmp #2357**, sprint 336 —
the suspected unjournaled-insert phantom, RETIRED). +4 nodes (`Task` `2357` COMPLETED,
`Task` `2366` BACKLOG, `Commit` `d855f602`, `Test`
`TestUnique_RollbackLeavesNoPhantomReservation`) plus **3 `Finding`s**, and +11 edges.
This task produced more findings than code, which is what a verification task should do.

**#2357's PREMISE IS RETIRED, not fixed.** It suspected that the second, UNJOURNALED
value-set insert (`ConstraintRegistry.RecordPropertySet`) could survive a rollback as a
phantom, because it took its own label read separate from the reserve's. The two could
diverge only while BOTH were raw reads; rmp #2355 routed every constraint decision through
`exec.labelsInTx`, so both resolve through the SAME transaction view, a peer's unpublished
label change is invisible to both, and the only write between them is a PROPERTY write.
Verified by grep that no raw label read feeds any reserve/record/release call in
`cypher/exec`. Independently, #2355's stamp makes a concurrent label change on that node
conflict, so the required interleaving is refused. Impossible, not merely unobserved.

**A CONTENTION FINDING, handed to #2358.** `RecordPropertySet`'s body is the SAME
set-insert loop as `ReserveSetProperty`'s phase 2, and all TEN write-path callers were
verified individually to be preceded by a reserve with identical `(labels, key, value)`.
`reserveConstraintValue` calls `ReserveSetProperty`, which inserts, and journals the
release inverse — so each `RecordPropertySet` is an idempotent no-op that acquires the
registry's write lock a SECOND time per constrained property write, on a lock
`label_constraints.go` records as 57% of ALL lock delay at sixteen writers. The ONE caller
that must stay is `constraint_journal.go`, where it IS the journaled inverse of a release.
Deleting the rest belongs to #2358, already scoped to these call sites.

**A FOURTH DEFECT IN THIS FAMILY, reproduced and filed as #2366.** With the peer
COMMITTING its label removal while the property writer ROLLS BACK, the writer's JOURNALED
inverse re-reserves the old value from its own view after the peer's committed release
already freed it, leaving it reserved with no live holder. The stamp DID fire — that is why
the writer rolled back — so this is not a missing collision. The `UNIQUE` value-set
(`cypher/exec/constraints.go` `valueSets`) is a plain NON-VERSIONED map whose inverses are
replayed per transaction with no ordering against a peer's commit. Fail-safe in direction
(a value becomes unusable, never duplicated) but it accumulates. Note rmp #2321 established
that a GLOBAL rebuild of the value-sets destroys concurrent writers' reservations, so a
naive reseed is not an available remedy.

**THE STRUCTURAL CONCLUSION of sprint 336 so far**, now on its fifth instance across
#2352, #2353, #2355, #2357 and #2366: conflict detection and constraint enforcement are per
SUBSTORE, while every declared invariant binds TWO substores — and the `UNIQUE` value-set
sits outside the versioning substrate altogether. The reference engines do not have this
class of defect because PostgreSQL and InnoDB version the whole ROW and Memgraph the whole
VERTEX, so any two writers to one object meet.
