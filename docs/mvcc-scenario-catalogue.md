# MVCC scenario catalogue for `examples/37_mvcc_write_contention`

Specification input for rmp #2933 (deterministic catalogue), #2934 (concurrency ladder,
history checking, growth) and #2935 (durability under concurrent writers).
Survey date 2026-09-28. The survey read the sources without executing them.

## 0. Sources and method

| Source | Commit | Paths read |
|---|---|---|
| PostgreSQL | `d9b5a63f49d9d372a6609243eb0e9417a0a35e6f` | `src/test/isolation/specs` (135 specs, every one dispositioned in E.1), `src/test/modules/injection_points/specs` (16), `src/test/recovery/t` (57 TAP names, 6 read), `contrib/amcheck/t` (6) |
| MySQL | `3b99be40f8a31d9396cad10accfd3ba51647d613` (2026-09-28) | `mysql-test/suite/innodb/t` (799), `suite/innodb_undo/t` (46), `suite/innodb_fts/t` (63), 22 transaction tests of `mysql-test/t`, 4 group-commit tests of `suite/binlog*` |
| Turso (MIT) | `2b9184f8dba21e2b193547af9072da65da782741` (2026-10-07) | `core/mvcc` (every file), `tests/integration/{mvcc.rs, query_processing/test_transactions.rs, query_processing/test_vacuum_into_mvcc.rs, fuzz_transaction, wal, checkpoint_crash_atomicity.rs, suspended_statement_checkpoint.rs, checkpoint_stale_page_size.rs}`, `tests/fuzz/mvcc_rowid_allocator.rs`, `testing/stress/tests/shuttle_mvcc*.rs`, `testing/simulator/runner/memory/mvcc_recovery.rs`, `testing/concurrent-simulator`, `core/storage/{wal.rs, shared_wal_coordination.rs}`, `sync/engine/tests/mvcc_logical_pull`, `testing/cli_tests/mvcc.py`, `docs/internals/mvcc` (914 tests, every one dispositioned in E.6) |
| GoGraph | `HEAD` = `43c69dbe` (read with `git show HEAD:`); #2931 and #2932 read from the uncommitted working tree | `docs/isolation-design.md`, `docs/isolation-anomalies.md`, `docs/design-write-conflict-detection.md`, `graph/mvcc`, `graph/lpg`, `cypher/exectx.go`, `cypher/exec/constraints.go`, `cypher/merge_race_test.go`, `internal/isolationtest` |

**Method.** PostgreSQL: every spec's header and setup read; disposition per spec in E.1.
InnoDB: all 934 in-scope tests triaged by name and by a body token scan (isolation
levels, `CONSISTENT SNAPSHOT`, purge/history, deadlock/lock-wait, duplicate key,
crash/restart, `debug_sync`, rollback/savepoint); header comments read for 191; bodies
read for about 25. **This is a triage, not a line-by-line read of 934 files**: a test
whose name and tokens carry no MVCC signal was excluded by category (E.3).

**Completeness.** PostgreSQL isolation specs: **135 of 135** evaluated — 64 kept as
sources, 71 excluded. InnoDB: **934 of 934** in scope evaluated — 111 kept as sources
(92 of 844 distinct `innodb`/`innodb_undo` names, 3 of 63 `innodb_fts`, 12 of 22 main
suite, 4 of 4 binlog group commit), 823 excluded.

**Turso (round 2, rmp #3012).** Scope: every test function in the MVCC engine module,
in the transaction, MVCC, WAL, checkpoint and crash integration tests, in the
deterministic-schedule (shuttle), fault-simulator and concurrent-simulator (Whopper)
suites, and in the page-WAL and shared-WAL unit tests. Counting rule: one test = one
function annotated `#[test]`, `#[turso_macros::test]`, `#[tokio::test]`, `#[rstest]` or
`#[hegel::test]`, plus the one `def test_` of `testing/cli_tests/mvcc.py`. Triage by
name and by body for the 17 Hermitage tests, the 20 shuttle tests, the doc comment of
every test the name left ambiguous (about 40), and `docs/internals/mvcc/{DESIGN,GC,
RECOVERY_SEMANTICS}.md`. **914 of 914** evaluated — 278 kept as sources, 636 excluded
by category (E.6). Turso implements snapshot isolation with eager write-write conflict
detection at the statement and automatic rollback of the refused transaction
(`core/mvcc/database/hermitage_tests.rs:15-23,122`); GoGraph refuses at the statement and
poisons the transaction (F2, F3, F8). Turso tests are Rust over SQL and cannot be
transcribed: "adapt" below means the interleaving and the assertion transfer one to
one into a Cypher golden, with attribution (MIT); "re-implement" means the mechanism
differs and only the property transfers. The Hermitage tests state they are adapted from
`github.com/ept/hermitage`; that project's licence was not read. The GoGraph facts cited by
the Turso rows (SK15-SK17, IX11, GG07, L21, L22, D17-D19, G11) were read at `9cfcbdc5`.

### 0.1 GoGraph facts the expected outcomes rest on

| # | Fact | Evidence |
|---|---|---|
| F1 | One isolation level, snapshot isolation. Write skew permitted; G-nonadjacent forbidden. | `docs/isolation-design.md:45-63`; `docs/isolation-anomalies.md:73-81` |
| F2 | First-updater-wins and first-committer-wins through one predicate: a writer is refused when the chain head is not visible to it. Retriable `ErrSerializationConflict`. | `graph/mvcc/conflict.go:56,144-149`; `docs/design-write-conflict-detection.md:55-60` |
| F3 | The conflict surfaces at the **statement**, not at COMMIT — except for void primitives (label removal, property delete, edge side stores), whose conflict is recorded and surfaces at **COMMIT**. | `docs/isolation-design.md:706-710`; `cypher/exectx.go:146-152,740-765` |
| F4 | The **node** is the unit of conflict (property chain keyed by node; adjacency append and delete both claim the node). An edge create checks **both** endpoints. | `cypher/exectx.go:148`; `graph/lpg/mvcc_props.go:98-115`; `graph/lpg/mvcc_adjversion.go:142-199`; `graph/lpg/lpg.go:2247-2265` |
| F5 | An **aborted** head conflicts until the background vacuum withdraws it, so a write right after a peer's rollback can be transiently refused. The vacuum is a background goroutine. | `graph/mvcc/conflict.go:103-143`; `graph/lpg/mvcc_gc.go:191` (`ReclaimNow`) |
| F6 | A read-write transaction's snapshot is taken at **BEGIN**; a read-only transaction's at BEGIN too, held for its lifetime. | `graph/lpg/lpg.go:1284`; `cypher/exectx.go:456-463,480-500` |
| F7 | No locks for DML: no SELECT FOR UPDATE, no NOWAIT/SKIP LOCKED, no gap locks, no wait queue, **no deadlock detector** (nothing to detect). Only DDL takes the schema barrier exclusively and can be waited on, bounded by ctx. | `graph/mvcc/conflict.go:38-44`; `cypher/api.go:80-82`; `cypher/exectx.go:350-366` |
| F8 | No savepoints (no symbol `savepoint` in non-test Go at HEAD). A failed statement poisons the transaction: COMMIT returns `ErrTxPoisoned`, ROLLBACK is required. | `git grep -i savepoint` = empty; `cypher/exectx.go:159-163,719` |
| F9 | DDL is rejected inside an explicit transaction; writes and DDL are rejected in a read-only transaction (`ErrWriteInReadOnlyTx`). | `cypher/exectx.go:173,599-602` |
| F10 | UNIQUE: atomic test-and-reserve; a duplicate against an **in-flight** reservation is a **ConstraintViolation**, not retriable (PostgreSQL waits instead). Autocommit MERGE under UNIQUE converges on one node with every caller succeeding; without a constraint duplicates are permitted. | `cypher/exec/constraints.go:858-891`; `cypher/merge_race_test.go:27-35,126-160` |
| F11 | Session contract: a `Session` observes its own commits; sessionless callers get SI only. The frontier is contiguous, so a commit above an in-flight one is invisible until it publishes. | `docs/isolation-design.md:189-248,356-400` |
| F12 | Property indexes are written at the transaction boundary; a seek declines an access path the transaction dirtied (#2814, `efd32fb9`); commit-time writeback must use the committed state, not the present graph (#2931, working tree `cypher/index_commit_state_test.go:115-165`). | `docs/isolation-design.md:402-432` |
| F13 | Horizon capacity 1024 registered readers; past it, reclamation stops but reads stay correct. Growth counters on `MVCCStats`. | `docs/isolation-design.md:732-790`; `graph/lpg/mvcc_stats.go:97-190,247` |
| F14 | Group commit is fail-all on fsync failure. | `docs/isolation-design.md:796-` |

### 0.2 Harness requirements the catalogue imposes

- **H1 — vacuum drain step.** Any golden with a write after a peer rollback (F5) is
  nondeterministic unless a `Hook` step drains the vacuum (`ReclaimNow`) at a fixed
  point. Without it the golden records a race.
- **H2 — commit-hold seam.** Frontier scenarios (SE02, SE03, SE04, L10) need a commit
  held between timestamp allocation and publication. **Met by an exported method:**
  `lpg.Graph.AllocateCommitTS` reserves a transaction's commit instant without
  publishing it (the first half of the WAL commit path), and the transaction's later
  `EndVersionedTx` publishes it or, after `WriteTx.Abandon`, abandons it. Example 37
  builds FP01-FP03, SE02, SE04 and SE03's fsync arm on it deterministically. The
  WAL-backed route first considered (an fsync blocked on a gate through the
  `store/wal` OpenFS seam) cannot build SE02: one WAL writer runs one fsync at a
  time, so a later committer waits for the held leader and never acknowledges.
- **H3 — two drivers.** Each #2933 scenario runs through `cypher.Engine` and, where the
  shape exists, the `lpg` API over the same graph (a_fr requirement).

Priority key: **H** high, **M** medium, **L** low. "PG" = PostgreSQL isolation spec,
"PG-inj" = injection_points spec, "PG-rec" = recovery TAP, "MY" = MySQL test
(`innodb/` unless another suite is named), "TU" = Turso test (path relative to the
Turso repository; `db/` = `core/mvcc/database/`). "EXISTS" = a golden already in
`internal/isolationtest/testdata`.

---

## 1. #2933 — deterministic scenario catalogue

### 1.1 Write-write conflicts

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| WW01 | Lost update: both read a counter, both write it | PG `deadlock-simple` (upgrade shape), `lock-committed-update`; MY `concurrent.inc` via `t/concurrent_innodb_safelog`, `t/concurrent_innodb_unsafelog`. EXISTS `lost-update` | `MATCH (c:Counter) RETURN c.n` then `SET c.n = <v>` in two explicit txns | Second writer refused at its SET while the first is open or committed after its snapshot; final = first writer's value (F2, F3). PG RR also refuses; InnoDB RR **permits** it (plain SELECT is non-locking, UPDATE writes the current version) | Conflict detection regressions, lost update (G-single) | H |
| WW02 | Write against a peer that committed **after** my snapshot | PG `lock-committed-update`, `lock-committed-keyupdate`, `eval-plan-qual` (wx1/wx2) | T2 BEGIN; T1 SET+COMMIT; T2 SET same node | T2 refused (head committed at > startTS, first-committer-wins; `docs/design-write-conflict-detection.md:59`). PG RC re-fetches and succeeds (EPQ); SI cannot | Stale-snapshot overwrite | H |
| WW03 | Blind increment `SET c.n = c.n + 1` in both, no prior read | MY `innodb_bug52663`, `innodb_bug49164` ("lost update incrementing column value under READ COMMITTED"); PG `eval-plan-qual` | Two txns, one statement each | Second refused; final n+1 after one commit, n+2 only after retry (F2). InnoDB RC/RR and PG RC give n+2 without error | Increment lost under concurrency; retry accounting | H |
| WW04 | Update vs delete of the same node, both orders, commit and rollback arms | PG `eval-plan-qual`, `lock-update-delete`, `merge-delete`; MY `concurrent.inc` | T1 `SET n.x`, T2 `DETACH DELETE n` | The later writer is refused while the earlier is open; after the earlier commits, still refused (head not visible). After the earlier rolls back, allowed once H1 drained (F2, F5) | Update/delete races, resurrection of a deleted node | H |
| WW05 | Update-then-delete chain in one txn; older-snapshot peer writes | PG `lock-update-delete` (tests 1-2), `lock-update-traversal`, `aborted-keyrevoke` | T1 `SET n.x` then `DELETE n`; T2 (older) `SET n.y` | T1 commits → T2 refused. T1 rolls back → T2 allowed after H1 (F5). PG: error if the DELETE commits, proceeds if it aborts — same verdicts, without the wait | Chain-walk and abort-withdrawal (#2723 families) | H |
| WW06 | Predicate write where a committed peer moved the node out of the predicate | PG `eval-plan-qual` (where-clause recheck), `merge-match-recheck`, `merge-update`; MY `innodb-semi-consistent`, `t/partition_innodb_semi_consistent` | T1 `SET n.status='b'` COMMIT; T2 (older) `MATCH (n {status:'a'}) SET n.v = 1` | T2 still matches at its snapshot and is **refused** at the write (F2). PG RC re-evaluates and skips the row; InnoDB semi-consistent read skips it | Predicate/visibility mismatch between read and write | M |
| WW07 | Non-matching nodes held by an open peer do not block or conflict | MY `innodb-semi-consistent`, `t/partition_innodb_semi_consistent`, `innodb-consistent` | T1 open with `SET a.k = 1`; T2 `MATCH (n {k:'b'}) SET n.v = 1` (a not matched) | T2 succeeds, no step `<waiting>` (F7) | False conflicts, over-wide conflict unit | M |
| WW08 | Two txns write **different** properties of one node | MY `concurrent.inc` (row granularity); PG `update-locked-tuple` (semantics only) | T1 `SET n.a`, T2 `SET n.b` | Second refused: node is the unit (F4). Same verdict as PG and InnoDB row granularity | Documents the conflict unit; a narrowing to per-key would show as a diff | M |
| WW09 | Disjoint nodes never conflict, in every interleaving | `docs/isolation-design.md:141-150` | Two sessions each update only their own node, twice | Zero conflicts in all permutations (F2, F11) | False positives from the frontier | H |
| WW10 | Crossing writes, 2-cycle (the deadlock shape) | PG `deadlock-simple`, `fk-deadlock`, `fk-deadlock2`; MY `t/deadlock_innodb`, `t/innodb_deadlock`, `t/innodb_mysql_lock`, `deadlock_detect`, `deadlock_on_lock_upgrade`, `deadlock_in_subquery`, `deadlock_stats`, `innodb_trx_weight` | T1 writes A, T2 writes B, T1 writes B, T2 writes A | No step ever `<waiting>`. T1's second write refused (B's head in flight); T2's second write refused too (A's head is T1's still-open version) until T1 rolls back — so **both** can fail where PG/InnoDB pick one victim (F2, F7, F8). Golden pins each permutation | Hangs, a wait introduced into DML, mis-scoped refusal | H |
| WW11 | N-way crossing cycle (3 and 8 sessions), explicit permutations | PG `deadlock-hard`; MY `long_deadlock_cycle`, `undetected_deadlock`, `hp_deadlock` | Ring of txns each writing its node then its successor's | No wait, no hang; every txn commits or is refused; ≥1 commits once refused ones roll back (F7) | Liveness under cycles | M |
| WW12 | Write after a peer's ROLLBACK | PG `eval-plan-qual` (s1 rollback, s2 update), `eval-plan-qual-trigger` (base case), `multixact-no-forget`; MY `innodb_mysql_rbk` | T1 `SET n.x`, ROLLBACK; `Hook` drains vacuum (H1); T2 `SET n.x` | With the drain: T2 succeeds. Without it: T2 may be refused transiently (F5). Both arms recorded | Permanently unwritable object after abort (#2318 class) | H |
| WW13 | Void-primitive conflict surfaces at COMMIT | GoGraph rmp #2354 (no PG/InnoDB counterpart) | T1 `REMOVE n:L`; T2 `REMOVE n:L`; also `REMOVE n.p`, `SET r.w` on one edge | T2's statement returns OK; T2's COMMIT returns `ErrSerializationConflict` and applies nothing (F3, `cypher/exectx.go:740-765`) | Silently dropped write (lost update with nothing reporting it) | H |
| WW14 | Reader walks a chain whose head is in flight and whose middle committed after its snapshot | PG `update-conflict-out`, `multiple-row-versions`; MY `innodb-read-view`, `lob_mvcc_undo` | T0 read tx pinned; T1 SET+COMMIT; T2 SET (open); T0 reads; T3 new read | T0 sees the original; T3 sees T1's value, never T2's (F1) | Visibility-walk errors, dirty read | H |
| WW15 | Long chain (≥4 versions) with large string values, oldest reader | PG `multiple-row-versions`; MY `lob_mvcc_undo`, `lob_partial_update_concurrent`, `zlob_partial_update_concurrent`, `json_small_partial_update_06`, `lob_vjhi` | Four committed SETs of 64 KiB strings over one pinned reader | Pinned reader returns the value current at its BEGIN, byte-identical (F1) | Version reconstruction of large values | M |

### 1.2 Snapshot-isolation anomalies and snapshot reads

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| SK01 | Write skew, on-call doctors | Berenson et al. A5B. EXISTS `write-skew` | Two txns read count, each clears its own doctor | Permitted: both commit, invariant broken (F1) | A checker or engine that forbids legal skew | H |
| SK02 | Swap write skew (apple ↔ pear) | PG `simple-write-skew` | T1 `SET` all apple→pear; T2 all pear→apple | Permitted: both commit, final holds both swapped sets. PG SERIALIZABLE aborts one | Same | H |
| SK03 | Predicate-insert write skew (booking overlap) | PG `classroom-scheduling`, `temporal-range-integrity` | Both read overlapping bookings (none), both `CREATE (:Booking)` | Permitted: both commit, overlap exists; no predicate locks (F7) | Same; phantom inserts | H |
| SK04 | Cross-label write skew (manager flag vs project) | PG `project-manager` | T1 reads project, sets person; T2 reads person, sets project | Permitted | Same | M |
| SK05 | Total cash (read sum, withdraw from a different account) | PG `total-cash` | Two accounts, each txn checks sum then debits its own | Permitted: negative total | Same | H |
| SK06 | Read-only anomaly (Fekete) | PG `read-only-anomaly`, `read-only-anomaly-2` (SSI aborts there), `serializable-parallel`. EXISTS `read-only-anomaly-named` | Three sessions, batch/receipt shape | Permitted: the read-only txn observes a state no serial order produces. Matches PG RR; PG SERIALIZABLE aborts s2 — recorded divergence (F1) | Same | H |
| SK07 | Receipt report, 3 sessions | PG `receipt-report` | Batch close vs receipt insert vs report | Permitted | Same | M |
| SK08 | Two IDs | PG `two-ids` | Three sessions over two nodes | Permitted in every permutation PG SERIALIZABLE would fail | Same | M |
| SK09 | Bank transfer conservation | EXISTS `bank-transfer` | Transfers plus total readers | Total constant in every permutation: G-single forbidden (`docs/isolation-anomalies.md:73`) | Torn totals (#2333, #2336 class) | H |
| SK10 | Write skew read through an index seek (hash equality and btree range) | PG `index-only-scan`, `predicate-hash`, `insert-conflict-serializable`, `matview-write-skew` (SSI halves excluded) | SK01 with the count read through an indexed predicate | Permitted, and seek result equals scan result in every step | Index read diverging from scan inside an anomaly | M |
| SK11 | Materialise the conflict: SK01 plus a write to a shared guard node | Fekete et al. TODS 2005 (the documented SI remedy); MY `t/locking_clause` shows the absent alternative | Each txn also `SET g.v = g.v + 1`; variant with value-preserving `SET g.v = g.v` | Increment arm: second refused, invariant holds (F2). Value-preserving arm: **unverified** — pin by golden | Loss of the only SI-level remedy for write skew | H |
| SK12 | No non-repeatable read, no phantom: count twice around a committed insert/delete | MY `t/consistent_snapshot` (test 1), `select_count_perf`, `innodb-read-view` | T1 `count(n:L)`; T2 CREATE/DELETE :L COMMIT; T1 `count` again | Both counts equal (F1) | Count store / label bitmap reading the present (#2688, #2081 class) | H |
| SK13 | Snapshot taken at BEGIN, not at first read | MY `t/consistent_snapshot` (tests 1-3); PG `fk-snapshot` | T1 BEGIN; T2 CREATE COMMIT; T1 first read | T1 does **not** see it (F6). InnoDB plain START TRANSACTION sees it (read view at first read); equals WITH CONSISTENT SNAPSHOT | Snapshot timing drift | M |
| SK14 | Fast-path aggregates at a snapshot | MY `select_count_perf`, `parallel_read`; PG `index-only-bitmapscan` | `count(n)` / `count(n:L)` fast path vs `count` over a filtered scan, with open and committed peers | Fast path equals scan, both at the snapshot | Index-only answers ignoring visibility (#2688 class) | H |
| SK15 | Aborted and intermediate versions are never read (G1a, G1b). Adapt 100% | TU `db/hermitage_tests.rs:150` (G1a), `:200` (G1b), `:862`; TU `testing/stress/tests/shuttle_mvcc.rs:572` (rollback isolation) | n {v:10}. T2 BEGIN. T1 `SET n.v = 101`; T2 reads; then T1 `SET n.v = 11` and COMMIT (G1b arm) or T1 ROLLBACK (G1a arm); T2 reads; T3 begins after T1 ends and reads, before and after an H1 drain | T2 reads 10 at every step (F6). G1b arm: T3 reads 11, never 101. G1a arm: T3 reads 10 with and without the drain: an aborted version carries `AbortedTS`, which `Visible` classifies as another transaction's uncommitted work (`graph/mvcc/mvcc.go:36-43,159-168`) | Dirty, intermediate or aborted read through the version walk; an aborted version read before the vacuum withdraws it (F5) | H |
| SK16 | Circular information flow (G1c). Adapt 100% | TU `db/hermitage_tests.rs:246` | a {v:10}, b {v:20}. T1 `SET a.v = 11`; T2 `SET b.v = 22`; T1 reads b; T2 reads a; both COMMIT | T1 reads 20 and T2 reads 10 (`graph/mvcc/mvcc.go:159-168`); both commit, disjoint nodes (F4, WW09); a new reader sees 11 and 22 | Mutual dirty reads between two open writers | M |
| SK17 | Observed transaction vanishes (OTV). Adapt 100% | TU `db/hermitage_tests.rs:295`; TU `shuttle_mvcc.rs:502` (all-or-none multi-row visibility) | a {v:10}, b {v:20}. T3 BEGIN. T1 `SET a.v = 11, b.v = 19`; T2 `SET a.v = 12` refused, ROLLBACK; T1 COMMIT; T3 reads a; T2' BEGIN, `SET a.v = 12, b.v = 18`, COMMIT; T3 reads a and b after each step | T2 refused at its statement (F2, F3). T3 reads (10, 20) at every step, never 11/19 or 12/18 (F6). Final state (12, 18). Turso rolls the refused transaction back itself; GoGraph poisons it until ROLLBACK (F8) | A committed or refused peer's effects appearing in, then vanishing from, a fixed snapshot | H |

### 1.3 MERGE and UNIQUE

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| MG01 | Concurrent MERGE, same key, no constraint | PG `merge-insert-update` | Two explicit txns `MERGE (:P {name:'a'})` | Both succeed; duplicates when both snapshots precede both commits; serial permutations give one (F10) | MERGE failing or losing a caller's write | H |
| MG02 | Concurrent MERGE under UNIQUE, winner open; winner commits / rolls back | PG `insert-conflict-do-nothing`, `insert-conflict-do-update`; MY `iodku`, `iodku_debug`, `innodb_replace`, `constraint_check_locks_in_read_committed` | T1 MERGE creates (open); T2 MERGE same key | T2 gets **ConstraintViolation** (not retriable) in both arms (F10). PG waits and then does nothing/updates; InnoDB takes an S lock and waits. Recorded divergence; the autocommit arm converges (L13) | Duplicate under UNIQUE (#2321 class); reservation leak on rollback | H |
| MG03 | UNIQUE value committed after my snapshot | PG `insert-conflict-do-nothing-2`, `read-write-unique-2` | T2 BEGIN; T1 CREATE u COMMIT; T2 MERGE u | T2 cannot see u, tries to create, gets ConstraintViolation. PG RR: serialization failure | Constraint check reading the snapshot instead of the reservation set | M |
| MG04 | Read-then-insert same unique value | PG `read-write-unique`, `-2`, `-3` | Both `MATCH (n {k:42})` → 0; both `CREATE (:N {k:42})` | Second CREATE ConstraintViolation at the statement. PG SSI: serialization failure after waiting | Same | H |
| MG05 | Gapless per-year sequence (read max, create max+1) | PG `read-write-unique-4` | `MATCH (i:Inv {y:2026}) RETURN max(i.n)` then CREATE max+1, with and without UNIQUE on (y,n) or n | Without constraint: duplicate numbers permitted (write skew). With UNIQUE: loser ConstraintViolation | Realistic application race | H |
| MG06 | MERGE ON MATCH SET vs a concurrent update of the matched node | PG `insert-conflict-do-update`, `-3` (update of an invisible tuple), `merge-match-recheck`, `merge-update` | T1 `SET n.v`; T2 `MERGE (n {k}) ON MATCH SET n.v = …` | T2 refused (F2). PG `-3` updates a tuple its snapshot cannot see; GoGraph must never do that | Writes over an invisible version | H |
| MG07 | ON MATCH SET changes the UNIQUE key itself | PG `insert-conflict-do-update-2` | `MERGE (n {k:'a'}) ON MATCH SET n.k = 'b'` vs `MERGE (n {k:'b'})` | Exactly one node per key; loser typed error (F10) | Release/reserve ordering (#2366) | M |
| MG08 | MERGE vs a concurrent DELETE of the matched node | PG `merge-delete` | T1 `DETACH DELETE n` COMMIT; T2 (older) `MERGE (n {k}) ON MATCH SET n.v = 1` | T2 refused (F2). PG RC turns the MERGE into an INSERT | Resurrection / write onto a dead node | H |
| MG09 | Delete and re-create one UNIQUE value: same txn; after peer commit; against peer's open delete, then its rollback | MY `innodb-lock-inherit-read_commited` (duplicate UK after purge), `index-create-dml-rollback`, `lock-inherit-existing`; PG `read-write-unique-3` | (a) one txn DELETE u, CREATE u; (b) T1 DELETE u COMMIT, T2 CREATE u; (c) T1 DELETE u open, T2 CREATE u, T1 ROLLBACK | (a) commits one u (fix `3b7bec2f`). (b) succeeds. (c) T2 violation (release deferred to commit, `fca34a0c`); after rollback exactly one u | Phantom or lost reservation (#1904, #2366) | H |
| MG10 | Intra-statement duplicate under UNIQUE | MY `innodb-index`, `create_table_select` | `SET a.e = 'x', b.e = 'x'` | Statement rejected, nothing applied (`cypher/exec/constraints.go:878-884`) | Partial statement application | M |
| MG11 | Race inside MERGE between probe and create | PG `insert-conflict-specconflict`, PG-inj `on_conflict_probe_window` | Needs a hook between MERGE's match and create (absent; see G5) | Exactly one node; loser ConstraintViolation | Check-then-act inside MERGE | M |
| MG12 | NOT NULL existence constraint at COMMIT; peer removes the property | PG `alter-table-1` (validate with concurrent writes); MY `constraint_check_locks_in_read_committed` | Txn creates node, sets required property later in the same txn; variant removes it; peer `REMOVE n.p` | Set-later commits; removed arm refused at COMMIT and rolled back; peer removal conflicts (F2) | Commit-time Consistency check (#1754) | M |

### 1.4 Indexes: own writes, peer writes, rollback

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| IX01 | Own write visible to a hash seek inside the txn (CREATE, and SET moving the value) | GoGraph #2814 (`efd32fb9`); MY `innodb_fts/transaction` (FTS hides own uncommitted rows **by design** — the #2814 shape) | 512 `:L` nodes, hash index on `s`; in txn `SET n.s='zzz'`, then `MATCH (n:L {s:'zzz'})` and `{s:'v7'}` | 1 row for `'zzz'`, 0 for the old value; seek = scan (F12) | **#2814** | H |
| IX02 | Own write through btree range and prefix seeks | GoGraph #2814 | btree on `s`; `WHERE n.s >= 'z'`, `STARTS WITH 'zz'` after the SET | Seek = scan | **#2814** | H |
| IX03 | Own write inside ONE autocommit statement | GoGraph #2814 ("one autocommit statement was enough") | `CREATE (:L {s:'q'}) WITH 1 AS x MATCH (n:L {s:'q'}) RETURN count(n)` | 1 | **#2814** | H |
| IX04 | Committed label add on a node an open peer re-valued; peer rolls back | GoGraph #2931 four-step (`cypher/index_commit_state_test.go:115-165`); MY `index-create-dml-rollback`, `innodb-index-online`; PG `partial-index` | `REMOVE n:L` COMMIT; T open `SET n.s='polluted'`; autocommit `SET n:L`; T ROLLBACK; hash and btree | Seek = scan for `'s33'` and `'polluted'`; node indexed under its committed value (F12) | **#2931** | H |
| IX05 | Rollback leaves no trace in any index | MY `innodb-index-online`, `index-create-dml-rollback`, `lob_rollback_update`, `innodb_mysql_rbk`; PG `partial-index` | Txn sets indexed property, adds indexed label, creates indexed node, takes a UNIQUE value, then ROLLBACK | Every seek (hash, btree, label, UNIQUE set, count store) = scan = pre-state | Reservation leaks; index entries surviving a rollback. Not #2931: measured at `43c69dbe`, IX05 passes (see §6) | H |
| IX06 | Node moves out of an index domain while an older reader seeks | PG `partial-index`, `partition-key-update-4` (idea); MY `multi_value_index_merge_mvcc` | Reader pinned; T `REMOVE n:L` / `SET n.s = other` COMMIT; reader seeks old value; new reader seeks both | Old reader finds n under the old value; new reader only under the new one | **#2814/#2931** class; index read at the wrong instant | H |
| IX07 | Two-index predicate at a pinned snapshot while a peer inserts into both indexes | MY `multi_value_index_merge_mvcc`, `bug32554667` (ICP) | Indexes on `a` and `b`; reader pinned; peer creates nodes matching both; `WHERE n.a = 1 AND n.b = 2` | No new rows for the pinned reader | Index intersection ignoring the snapshot | M |
| IX08 | Seek at the reader's instant after CREATE INDEX mid-transaction | PG `drop-index-concurrently-1` (RR: 1 row), `reindex-concurrently`; MY `innodb-read-view` | Read tx pinned; autocommit `CREATE INDEX`; peer inserts; read tx queries the indexed predicate | Only snapshot rows (commit `71a0541e`) | Index newer than the snapshot | H |
| IX09 | Label bitmap vs property index vs scan agree after mixed commits and rollbacks | GoGraph #2931 churn; MY `lock_impl_to_expl_case_sensitivity` (secondary index variants) | Label add/remove in one txn, indexed value change in another, one rolls back | At quiescence seek = scan = label count | #2931, #2687 seams | M |
| IX10 | Parameter vs literal seek of own writes | GoGraph `3fd78c5e` | IX01 with `$v` string and numeric | Same rows as the literal | #2814 via a second access path | L |
| IX11 | One large commit re-values an indexed property while readers seek. Re-implement | TU `db/tests.rs:4940` (`test_reader_consistent_during_large_indexed_commit_rewrite`), `:5550` (`test_reader_does_not_see_inflight_index_tombstone`) | 10 000 `:L` nodes, hash and btree index on `s`; T `MATCH (n:L) SET n.s = n.s + '_x'` COMMIT; readers begun before, during and after the commit's index delivery seek old and new values | Every reader's seek equals its own label scan: all old values before the publish, all new after, never a mix. While a delivery is open the index manager cannot prove its state for a reader's instant, so the seek declines to the scan (`cypher/exec/index_snapshot.go:19-38`; delivery open until `FinishApplied`, `cypher/exec/index_writeback.go:55-60`) | Partial index state of one commit visible to a reader (#2937 class at scale) | H |

### 1.5 DDL vs DML

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| DD01 | CREATE INDEX while an open write txn later commits or rolls back | PG `multiple-cic`, amcheck `t/002_cic.pl`; MY `innodb-index-online`, `innodb-table-online`, `innodb-index-online-delete`, `bulk_create_index_online`, `index-create-dml-rollback` | T open `SET n.s='x'` / `CREATE (:L {s:'y'})`; autocommit `CREATE INDEX`; T COMMIT or ROLLBACK | Index includes the committed arm, excludes the rolled-back arm; seek = scan. **Unverified** whether the backfill reads the committed snapshot or the present graph — this scenario settles it | **#2931** via the backfill path | H |
| DD02 | Two CREATE INDEX on different properties with writers between | PG `multiple-cic` | Two DDL steps interleaved with writes | Both indexes seek = scan | Registration races | M |
| DD03 | DROP INDEX under an open txn and between seeks | PG `drop-index-concurrently-1`, `vacuum-concurrent-drop` | Txn seeks, DROP INDEX, txn seeks again | Second seek falls back to scan with equal rows; no error | Stale plan on a dropped index | H |
| DD04 | DROP + CREATE same index (REINDEX analogue) with writers | PG `reindex-concurrently` | DROP, writes, CREATE | Seek = scan after | Rebuild missing concurrent writes | M |
| DD05 | CREATE UNIQUE CONSTRAINT while an open txn holds an uncommitted duplicate | PG `alter-table-1`; MY `alter_table_rebuild_duplicate_record`, `index-create-dml-rollback` (NULLs), `innodb-alter-debug` | T open `CREATE (:U {k:1})` over an existing k=1; DDL; T COMMIT or ROLLBACK | Invariant: no committed state violates the constraint. Whether the DDL or T's commit is refused is pinned by golden | Constraint over dirty state (#2931 class); spurious DDL failure | H |
| DD06 | DDL waits behind an in-flight statement, bounded by ctx; DML never waits on an idle open txn | PG `timeouts`, `truncate-conflict`; MY `innodb-timeout`, `innodb_lock_wait_timeout_1`, `innodb-lock` | Long statement + DDL with a 50 ms deadline; idle open txn + another writer | DDL returns ctx error within deadline + margin; the writer is not `<waiting>` (F7; `docs/isolation-design.md:677-700`) | Lifetime lock regressions (#2305), unbounded DDL waits | H |
| DD07 | DDL inside an explicit txn; DDL in a read txn | MY `t/implicit_commit` (MySQL COMMITS implicitly), `t/trans_read_only` (Test 10) | `tx.Exec("CREATE INDEX …")` | Rejected with an error (F9); the txn remains rollbackable; MySQL's implicit commit is a recorded divergence | DDL leaking into a txn | M |
| DD08 | Concurrent DDL on one object | PG `ddl-dependency-locking` | CREATE INDEX X vs DROP INDEX X; two CREATE CONSTRAINT with one name | Serialised by the exclusive gate; catalog consistent; one typed error | Catalog races | L |
| DD09 | CREATE INDEX concurrent with the vacuum withdrawing aborted versions | MY `alter_table_rebuild_missing_record` ("lost rows if concurrent purge"), `innodb-index-online-purge`, `virtual_debug_purge` | Aborted txn; Hook drains vacuum (H1) between backfill start and end | Seek = scan; no lost node | Backfill vs reclamation | H |

### 1.6 Abort and rollback

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| AB01 | Statement error mid-txn, then COMMIT, ROLLBACK, retry | PG `delete-abort-savept`, `-2`, `aborted-keyrevoke` (savepoint halves excluded); MY `t/func_rollback`, `innodb_mysql_rbk` | Txn writes, then `RETURN 1/0` or a type error | COMMIT → `ErrTxPoisoned`; ROLLBACK restores all; peers never saw anything; retry commits (F8) | Partial commit of a failed txn | H |
| AB02 | Autocommit statement fails midway over many nodes | MY `t/func_rollback`, `t/kill` (KILL QUERY mid-statement) | `MATCH (n:L) SET n.v = 10 / (n.i - 50)` over 100 nodes; ctx-cancel variant | None applied, indexes clean | Statement atomicity | H |
| AB03 | Refused statement, then continue in the same txn | MY `t/innodb_deadlock` (read view after deadlock victim), `innodb_mysql_rbk` | T gets `ErrSerializationConflict` from Exec, then reads, then COMMIT | Reads keep the same snapshot; COMMIT outcome (`ErrTxPoisoned` or the conflict) pinned by golden — **unverified** which | Snapshot loss after a refusal; commit of a doomed txn | H |
| AB04 | Retry loop converges | `docs/isolation-design.md:141-150` | WW03 with retry | Final = number of acknowledged increments | Retry correctness | M |
| AB05 | Rollback of a txn that created nodes and edges | MY `lob_big_rollback`, `zlob_big_rollback`, `undo_log_temp_table` | CREATE 100 nodes + edges, ROLLBACK | Invisible; edge and label counts, count store, UNIQUE set restored | Undo completeness | M |
| AB06 | Create, delete, re-create one node in a txn; commit and rollback arms | GoGraph `graph/lpg/mvcc_life_test.go:149-265` | One txn, three statements | Commit: visible once; rollback: never visible | Life-record overwrite (#2723/#2724 class) | M |

### 1.7 Horizon, read-only transactions, sessions and frontier

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| HZ01 | Long reader retains versions; release then drain reclaims | PG `horizons`, `vacuum-no-cleanup-lock`; MY `bug120529`, `purge_on_replica`, `flush-hang`, `lob_purge` | Read tx pinned; 10 committed SETs; `MVCCStats().Total` sampled by Hook; release; drain | Total rises while held and falls after (F13) | Leaked horizon slot; premature reclamation | H |
| HZ02 | Autocommit read releases its slot; read txn holds one | MY `innodb-ac-non-locking-select`, `innodb_i_s_innodb_trx`; PG `horizons` | Hook reads `ActiveSnapshots` around statements | 0 after an autocommit read; 1 while a read txn is open | Slot leak | M |
| HZ03 | Aborted versions withdrawn and invisible | GoGraph `docs/design-mvcc-abort-withdrawal.md`; MY `lob_rollback_update`, `zlob_rollback_update` | Aborted txn, drain, fresh reader, writer | Reader sees pre-state; writer succeeds | Dirty base after abort (F5) | H |
| HZ04 | Vacuum during DROP INDEX / label removal | PG `vacuum-concurrent-drop`; MY `innodb_bug26818787` | Index removal backlog, DROP INDEX, drain | No error; `IndexRemovalBacklog` drains to 0 | Vacuum vs catalog change | M |
| RO01 | Read-only txn: stable snapshot, writes and DDL rejected | MY `t/trans_read_only`; PG `read-only-anomaly-3` (DEFERRABLE excluded) | `BeginReadTx`; reads around a commit; `CREATE`, `CREATE INDEX` | Stable reads; `ErrWriteInReadOnlyTx` (F6, F9) | Write path reached from a read txn | H |
| RO02 | Read txn opened before two write txns that commit | MY `innodb-read-view` | T1, T2 BEGIN; T3 `BeginReadTx` reads; T1, T2 write and commit; T3 reads | T3 sees neither; a new reader sees both | Read view including later commits | H |
| RO03 | Read txn through the parallel scan path | PG `serializable-parallel` | RO01 with a scan large enough to parallelise | Same answer as the serial path | Parallel reader ignoring the snapshot (#1672) | L |
| SE01 | Session reads its own commit immediately | `docs/isolation-design.md:236-248`; PG-rec `057_snapshot_commit_race` | `cypher.Session` write then read | Sees its commit (F11) | Read-your-own-commits regression | H |
| SE02 | Straggler holds the frontier | PG-rec `057_snapshot_commit_race`, PG-inj `repack_commit_race`; MY `binlog_gtid/binlog_group_commit_gtid_order`, `binlog/binlog_after_commit_order_info_schema`, `hp_deadlock` | T0 commit held in fsync (H2); T1 commits and ACKs; sessionless read; T1's session read; release T0 | Sessionless read excludes T1; session read is `<waiting>` (`SessionsWaiting`=1) until T0 publishes; T1's ACK does not wait on T0; then both visible in one step (F11) | Frontier stall and convoy — **#2932** liveness shape (see §6) | H |
| SE03 | Allocate-then-abandon does not stall the frontier | `docs/isolation-design.md:389-395`; MY `binlog/binlog_group_commit_flush_crash` | T0 commit fails at COMMIT (NOT NULL violation or injected fsync error); T1 commits | T1 visible to a new sessionless reader; `InFlightCommits` returns to 0 | Leaked timestamp (#2727 class) | H |
| SE04 | Self-conflict through the frontier | `docs/isolation-design.md:141-150,199-213` | SE02 plus T1 rewriting its own node sessionless, then in a session | Sessionless: may be refused on its own node; session: not refused | Session floor not applied | M |

### 1.8 Referential integrity and graph shapes

| ID | Scenario | Source | GoGraph shape | Expected SI outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| RI01 | Edge create vs endpoint DELETE, older snapshot | PG `fk-snapshot-2`, `fk-concurrent-pk-upd`, `fk-partitioned-1`, PG-inj `ri_fastpath_snapshot` | T1 `DELETE b` COMMIT; T2 (older) `MATCH (a),(b) CREATE (a)-[:R]->(b)` | T2 refused: destination is checked (`graph/lpg/lpg.go:2255-2265`, rmp #2444) | **Dangling edge** — Consistency | H |
| RI02 | Edge create vs endpoint DETACH DELETE, both open, every order, both endpoints | PG `fk-snapshot`, `fk-partitioned-1`; MY `update-cascade` | T1 `DETACH DELETE b`; T2 `CREATE (a)-[:R]->(b)` and `(b)-[:R]->(a)` | Later writer refused; no permutation leaves an edge to a dead node | Dangling edge (#2694/#2725 class) | H |
| RI03 | Concurrent edge creates on one hub (same target, same source) | PG `fk-contention`; MY `innodb_cats` (hot row) | T1, T2 each `CREATE (x)-[:R]->(hub)` | Code: second **refused** — append heads are tested (`mvcc_adjversion.go:145-157,196`). The godoc of `addEdgeInfo` (`lpg.go:2239-2242`) says appends never conflict; the golden settles which is true | Hub false conflicts or a lost arc | H |
| RI04 | Application RI check vs edge insert (check "no incoming", then DELETE) | PG `referential-integrity`, `ri-trigger`, `temporal-range-integrity` | T1 `MATCH (b) WHERE NOT ()-->(b) DELETE b`; T2 `CREATE (a)-[:R]->(b)` | One refused (native edge RI). PG without an FK permits the orphan | RI write skew | H |
| RI05 | Create edge to a parent and update the parent, crossed | PG `fk-deadlock`, `fk-deadlock2` | T1, T2 each create an edge to P, then `SET P.n` | Second refused immediately, no wait | Hang / wait in edge paths | M |
| RI06 | Relationship property/type conflicts; disjoint parallel edges | GoGraph `graph/lpg/mvcc_conflict_stores_test.go:109-313` | Two `SET r.w` on one edge; two txns on different parallel edges between the same pair | Same edge: second refused (at COMMIT when void, F3); different parallel edges: no conflict | Side-store conflicts | M |
| GG01 | Variable-length traversal at a pinned snapshot under edge churn | GoGraph-specific (proposed) | Read tx `MATCH p=(a)-[*1..4]->(z) RETURN count(p)` twice; peer adds/removes edges on the paths and commits | Both counts equal the snapshot's | Traversal mixing instants | H |
| GG02 | DETACH DELETE a hub vs a neighbour creating an edge to it | GoGraph-specific (proposed; #2725/#2694) | T1 `DETACH DELETE hub`; T2 `CREATE (x)-[:R]->(hub)` | One refused; no dangling edge; degree and count store consistent | Inbound-arc leaks | H |
| GG03 | Degree and relationship counts at a snapshot under churn | GoGraph-specific (proposed) | `count{(n)--()}`, `MATCH ()-[r:R]->() RETURN count(r)` around peer commits | Equal to the snapshot's | Count store (#2081) | M |
| GG04 | Label churn vs label scan and label index at an older snapshot | GoGraph-specific (proposed; #2687) | Peer `SET n:L` / `REMOVE n:L` COMMIT; pinned reader `MATCH (n:L)` | Pinned reader unchanged | Delete/label-index seam | H |
| GG05 | Delete-everything vs a concurrent insert | PG `truncate-conflict` (adapted) | T1 `MATCH (n) DETACH DELETE n`; T2 `CREATE (:L)` | T2's node survives (phantom insert permitted) | Over-deletion across snapshots | M |
| GG06 | Concurrent delete of parallel edges and self-loops by handle | GoGraph #2018 | Two txns delete different parallel edges; one deletes a self-loop | Disjoint instances commit; same instance: second refused | Instance-precise delete | L |
| GG07 | Node identity under concurrent CREATE, rollback and reopen. Re-implement | TU `db/tests.rs:15883`, `:15957`, `:16151` (no reuse after delete and restart), `:20671`, `:20700`, `:20737`; TU `tests/fuzz/mvcc_rowid_allocator.rs:518`; TU `tests/integration/mvcc.rs:224` | 8 sessions each `CREATE (:P {w:$i})` in explicit txns, half rolled back; DETACH DELETE some; close and reopen the `store.DB`; CREATE again; repeat with a second store opened in the same process after the first has created nodes | Every committed CREATE is a distinct node: `count(n)` = acknowledged creates − deletes, no node carries two creators' properties, no `id()` names two nodes. Fresh keys come from a process-wide counter (`cypher/exec/create_node.go:170-188,441-447`) seeded once per process from the first engine's graph (`:190-198,334-345,449-485`); interning a key that is already live returns that node (`cypher/api.go:21604-21625`). **Verified, defect found and fixed (rmp #3015):** a store opened after the seed, or a store recovered into memory, holding larger persisted keys, was given colliding keys, so two committed CREATEs named one node, and a deleted node's key and `id()` were re-minted after a reopen. Every key is now minted by `mintNodeKey` (`cypher/exec/create_node.go`), which rejects a key the graph already holds. Gated by phase 8 of example 37 (`examples/37_mvcc_write_contention/identity.go`). Separate finding, not fixed: `id()` is not stable across a reopen after rolled-back CREATEs (example 37 README, phase 8) | Two CREATEs aliased to one node; identity reuse across reopen | H |

---

## 2. #2934 — concurrency ladder, history checking, growth

| ID | Scenario | Source | GoGraph shape | Expected outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| L01 | Bank-transfer history at 1/8/64 (short) and 256/1024 (soak), checked at SI | PG `total-cash`; MY `concurrent.inc` | Mixed `BeginTx`/`BeginReadTx` recorded with `internal/anomaly` | 0 forbidden (G0, G1a-c, G-nonadjacent) (`docs/isolation-anomalies.md` boundary table) | Torn reads, lost updates at scale | H |
| L02 | Write-skew arm as positive control | PG `simple-write-skew` | Doctors shape at every level | Lands in `Report.Permitted` | A checker that sees no cycles | H |
| L03 | Read txns repeat every read | MY `t/consistent_snapshot`, `select_count_perf`, `innodb-read-view`; PG `read-only-anomaly` | Each read txn reads twice | No non-repeatable read, no phantom | Snapshot drift under load | H |
| L04 | Hot counter with retries | MY `innodb_bug52663`, `innodb_cats`; PG `eval-plan-qual` | N writers `SET c.n = c.n + 1` with retry | Final = acknowledged increments; conflicts per store; longest retry streak below budget | **#2932** (streak gate), lost update | H |
| L05 | Large-transaction starvation | MY `innodb_cats`, `innodb_trx_weight`, `high_prio_trx_2`, `_6`, `_9`, `cats-autoinc` | One txn over K hot nodes vs N single-node writers | Reported: large txn's streak and success rate. No fairness mechanism exists (G3) | Starvation under first-updater-wins | M |
| L06 | Random index churn | GoGraph #2931 churn test; MY `innodb-index-online`, `multi_value_debug`, `innodb_bug30113362`, `innodb_bug31205266` | Label add/remove, indexed writes, rollbacks at 8..1024 | Seek = scan at quiescence for hash, btree, label, UNIQUE set, count store | **#2931** | H |
| L07 | In-transaction seek of own writes under churn | GoGraph #2814 | Each txn writes then seeks its own values | Seek = scan inside every txn | **#2814** | H |
| L08 | Long-reader retention and release | PG `horizons`, PG-rec `048_vacuum_horizon_floor`; MY `bug120529`, `purge_on_replica`, `zlob_ibd_size` | One read txn held across a phase | `Total` rises while held, then `Total <= Bound`; `WatermarkRegressions = 0` and `HorizonStaleLeaves = 0` at every sample; `WithinCeiling()` without the reader | Unbounded growth, horizon moving backwards | H |
| L09 | Horizon capacity cliff | `docs/isolation-design.md:732-790` | 1025 concurrent read txns (soak) | `UnregisteredSnapshots > 0`, reads correct, reclamation resumes after release | Wrong answers past capacity | M |
| L10 | Frontier lag and publication convoy at 256/1024 WAL-backed writers | MY `binlog/binlog_group_commit_sync_delay`, `binlog_gtid/binlog_group_commit_gtid_order`; PG-rec `057_snapshot_commit_race` | Peak `InFlightCommits`, `SessionsWaiting`, `OutOfOrderPublications`, `HelpedPublications` (`MVCCStats`; gauges `lpg.mvcc.publications.out_of_order` and `lpg.mvcc.publications.helped`), visibility lag | Bounded lag; streak gate holds; fails at `43c69dbe` with #2931 and without #2932 (b_ac) | **#2932** | H |
| L11 | Session vs sessionless arms | `docs/isolation-design.md:199-213` | Same workload, two arms | Self-conflicts = 0 in the session arm | Session floor regressions | H |
| L12 | Disjoint-writer scaling | `docs/isolation-design.md:71-80` | Each writer owns its nodes | Commits/s rises with writers; conflicts = 0 in the session arm | False conflicts, serialisation | M |
| L13 | MERGE storm under UNIQUE | `cypher/merge_race_test.go:126-160`; MY `iodku_debug`, `innodb_replace`; PG `insert-conflict-do-update` | 1..1024 autocommit MERGE on few keys | One node per key; every caller succeeds (F10) | Duplicates under UNIQUE | H |
| L14 | MERGE storm without constraint | PG `merge-insert-update` | As L13, no constraint | No failure; duplicates counted, not asserted | MERGE failure under load | M |
| L15 | Hub edge churn with DETACH DELETE | PG `fk-contention`; MY `update-cascade` | Writers add/remove edges on hubs; deleters remove hubs | No dangling edge at quiescence; conflicts counted | Dangling edges at scale | H |
| L16 | Traversal readers under edge churn | GoGraph-specific (GG01 at scale) | Read txns repeat a bounded traversal | Identical repeats | Traversal instant mixing | M |
| L17 | Parallel count over many uncommitted nodes, with cancel | MY `parallel_read_kill`, `parallel_read`, `select_count_perf` | 8 txns hold 150k uncommitted nodes; `count(n)`; cancel | Count = committed only; cancel prompt; goleak clean | Parallel scan visibility and cancellation | M |
| L18 | Abort-heavy arm | MY `lob_rollback_update`, `zlob_rollback_update`, `lob_big_rollback` | 50% rollbacks plus conflicts | After quiescence `Total <= Bound`; no permanently unwritable node | Abort reclamation (#2318) | H |
| L19 | DDL cycles under load | PG `multiple-cic`, amcheck `t/002_cic.pl`; MY `innodb-index-online` | CREATE/DROP INDEX cycles at 64..1024 writers | Seek = scan; DDL latency bounded; writers not starved | Backfill under load (#2931) | H |
| L20 | Checkpoint concurrent with writers | MY `t/flush_block_commit` (FTWRL blocks COMMIT — divergence), `log_writer_extra_margin` | Non-blocking checkpoint during the ladder | Commit latency tail bounded during checkpoint | Checkpoint stalls | M |
| L21 | Reader begin against eager reclamation (begin-publish window). Re-implement | TU `shuttle_mvcc.rs:749` (`shuttle_test_begin_publish_window_gc_hazard`, PR #7493), `db/tests.rs:792`, `:12780` | One writer `SET n.v = i` in a loop; 8..1024 readers each BEGIN and read n; a goroutine calls `ReclaimNow` continuously; n is created once and never deleted | Every read returns exactly one n, with a value committed at or before the reader's start. A reader claims its horizon slot before it reads the clock (`graph/lpg/snapshot.go:252-258`; `graph/mvcc/horizon.go:287-305`), and the watermark is capped by the fallback sampled before the slot scan (`horizon.go:435-490`); `WatermarkRegressions = 0` and `HorizonStaleLeaves = 0` at every sample | Reclamation freeing a version a just-started reader needs (node seen, then absent) | H |
| L22 | Snapshot invariants while checkpoints run. Re-implement | TU `db/tests.rs:5403` (`test_passive_concurrent_transfer_preserves_sum_and_count`), `:18953` (`test_snapshot_stability_full`); TU `tests/integration/mvcc.rs:1677`; TU `tests/integration/fuzz_transaction/mod.rs:510` | SK09 bank transfers on `store.DB` at 8..256 writers; a goroutine forces checkpoints in a loop; pinned read txns repeat `sum` and `count`; then crash and recover | Every read txn sees one constant total and count across checkpoints; every ACKed transfer is present after recovery and the total is unchanged. The checkpoint captures one MVCC instant under the commit lock after draining in-flight commits (`store/checkpoint/checkpoint.go:733-750,752-781`) | Checkpoint capture skew leaking into or out of a snapshot; loss across checkpoint and crash | H |

---

## 3. #2935 — durability under concurrent writers

| ID | Scenario | Source | GoGraph shape | Expected outcome (citation) | Defects caught | Pri |
|---|---|---|---|---|---|---|
| D01 | Acknowledged commits survive an in-process crash, 8..64 writers | PG-rec `013_crash_restart`; MY `log_first_rec_group` (crash right after COMMIT), `fast_shutdown` | `store.DB`, ack log outside the store, abandon, `recovery.Open` | missing = 0; refused/rolled-back present = 0 | Lost ACKed commit | H |
| D02 | `kill -9` of a child, 5 runs (soak) | PG-rec `013_crash_restart`; MY `innodb_redo_debug_1`..`_6` (kill at debug points) | Child process writer; parent kills | As D01 | Process-crash durability | H |
| D03 | Crash-point matrix in the commit pipeline | MY `innodb_redo_debug_1`..`_6`, `binlog/binlog_group_commit_flush_crash`, `ddl_prepare` | Crash after append/before fsync, after fsync/before publish, after publish/before ACK, inside a group fsync | ACKed present; in-doubt txns all-or-nothing | Ordering of ACK vs durability | H |
| D04 | Group-commit fsync failure | `docs/isolation-design.md:796-`; MY `binlog/binlog_group_commit_flush_crash` | SimDisk fsync error during a group | No member ACKed; none visible; store poisoned; reopen recovers to the last durable state (F14) | Partial group acknowledgement | H |
| D05 | Multi-statement txn in flight at the crash | MY `resurrection_logs`, `fast_shutdown` | Txn with 3 statements, crash before COMMIT returns | Absent or wholly present, index entries included — never partial | Atomicity across recovery | H |
| D06 | Open uncommitted txn at the crash | MY `resurrection_logs`; PG-rec `013_crash_restart` | BEGIN, writes, crash | Absent | Uncommitted writes replayed | H |
| D07 | Commit clock not rewound | MY `max_trx_id`; PG-rec `057_snapshot_commit_race` | Record max ACKed commit TS; recover; new commit | Recovered clock ≥ max ACKed; new commit TS greater; a new session sees every ACKed commit | Clock rewind (#2522/#2530 class) | H |
| D08 | Torn WAL tail | PG-rec `039_end_of_wal`, `026_overwrite_contrecord` (torn-record half); MY `doublewrite` (analogue only) | Truncate/garble the last frame after a crash | Earlier ACKed preserved; torn record discarded | Torn frame masquerade | H |
| D09 | Crash during checkpoint and WAL prefix truncation, writers running | MY `innodb_undo/create_undo_crash_recover`, `truncate_new`, `truncate_new_explicit`, `truncate_undo_recover_fixup`, `undo_truncate_upgrade`; PG-rec `050_redo_segment_missing`, `052_checkpoint_segment_missing` | Crash at each checkpoint phase | ACKed preserved; a missing segment is refused loudly, never silently skipped | Checkpoint-crash loss | H |
| D10 | Index and constraint consistency after recovery | MY `innodb_fts/search_after_crash_no_binlog`, `innodb_fts/fts_sync_commit_resiliency`, `bulk_create_index_online`; PG amcheck `t/002_cic.pl`, `t/004_verify_nbtree_unique.pl` | After D01-D09 | Seek = scan on every index; UNIQUE holds; count store = scan | Derived structures diverging after recovery | H |
| D11 | Crash during CREATE INDEX / CONSTRAINT with writers | MY `bulk_create_index_online`, `ddl_prepare`, `innodb_bug53756` | Crash mid-backfill | Index absent or complete; no UNIQUE violation | DDL crash consistency | M |
| D12 | Large values across a crash | MY `blob-crash-16k`, `blob-crash-4k`, `zlob_insert_by_update` | 1 MiB string properties | Byte-identical after recovery | Large-frame durability | L |
| D13 | Crash during recovery (double crash) | MY `fast_shutdown` (shutdown during recovery rollback) | Crash inside `recovery.Open`, reopen | Same state as a single recovery | Non-idempotent recovery | M |
| D14 | Negative control: drop the last ACKed WAL record | c_ac | Test seam | missing > 0 | Vacuous check | H |
| D15 | No hole in the recovered set | MY `binlog_gtid/binlog_group_commit_gtid_order` | Compare recovered set with the ACK log ordered by WAL position | If position k is recovered, every ACKed commit before k is recovered | Out-of-order group loss | M |
| D16 | Checkpoint plus tail replay equals full replay | MY `log_file_consumer`, `log_file_producer`, `t/flush_block_commit` | Checkpoint mid-run, crash, recover; compare with a no-checkpoint run | Identical state | Checkpoint capture skew (#2269) | M |
| D17 | Torn tail, recover, append, recover again. Re-implement | TU `db/tests.rs:2751` (`test_recovery_overwrites_torn_tail_on_next_append`), `:2800`; TU `core/mvcc/persistent_storage/logical_log.rs:4741`, `:4822` | Crash mid-frame (torn tail); `recovery.Open`; commit K transactions; crash; recover | Every ACKed commit of both epochs present; the second recovery does not stop at the first epoch's torn bytes. `wal.Open` truncates a benign torn tail to the last frame boundary and fsyncs it before appending (`store/wal/writer.go:244-250,302-306,333-345`) | ACKed commits stranded behind torn bytes | H |
| D18 | Checkpoint against a commit between WAL append and publish. Re-implement | TU `db/tests.rs:3302` (`test_checkpoint_snapshot_ts_clamps_below_inflight_preparing`), `:8618`, `:3089`, `:2974`; TU `testing/stress/tests/shuttle_mvcc_checkpoint.rs:150` | T0 holds an allocated, unpublished commit (H2); T1 commits; a checkpoint starts; release T0; crash after the checkpoint; recover | The checkpoint does not capture until T0 publishes (`awaitCommitQuiescence`, `store/checkpoint/checkpoint.go:720-750`); after recovery T0 and T1 are present and the truncated WAL prefix holds nothing the snapshot lacks. A T0 never released fails the checkpoint and nothing is truncated (`checkpoint.go:722-729`). **Unverified:** whether H2 can hold a commit inside `store/txn` between its fsync and its publish; if not, this arm needs a store-level seam | Checkpoint boundary straddling an unpublished commit; loss on reopen | H |
| D19 | Commit cancelled at each phase; no ghost commit. Re-implement | TU `db/group_commit_tests.rs:378`, `:383`, `:532`; TU `db/tests.rs:14581`, `:14605`, `:19605`, `:20604`, `:20641`; TU `shuttle_mvcc.rs:349` (ghost commits) | `CommitCtx` with ctx cancelled (a) before the apply takes its claims, (b) during the WAL fsync, (c) after it; 8..64 writers each tally OK and error results; crash; recover | (a) error wrapping `context.Canceled`, nothing durable or visible, a retry commits; (b) and (c) ctx is no longer consulted, the commit completes and returns OK (`store/txn/txn.go:1839-1855`). Committed count = OK count, in memory and after recovery; later commits are never blocked | Ghost commit (applied, reported failed); lost commit (reported OK, absent); stranded commit lock | H |

---

## 4. Counts

| Task | H | M | L | Total |
|---|---:|---:|---:|---:|
| #2933 | 55 | 30 | 4 | 89 |
| #2933 proposals (FP01-FP03, §5) | 2 | 1 | 0 | 3 |
| #2934 | 13 | 7 | 0 | 20 |
| #2935 | 11 | 4 | 1 | 16 |
| Turso additions (SK15-SK17, IX11, GG07; L21-L22; D17-D19) | 9 | 1 | 0 | 10 |
| **All** | **90** | **43** | **5** | **138** |

#2933 by family: WW 15, SK 14, MG 12, IX 10, DD 9, AB 6, HZ 4, RO 3, SE 4, RI 6, GG 6.
Turso additions by family: SK +3 (SK15-SK17), IX +1 (IX11), GG +1 (GG07), L +2
(L21, L22), D +3 (D17-D19).
Four #2933 rows (WW01, SK01, SK06, SK09) are existing goldens and need only a README
mapping, not a new spec.

---

## 5. Coverage matrix

Facets: **SR** snapshot reads · **OW** own writes incl. index seeks · **WW** write-write
conflicts · **AN** SI anomaly classes · **MU** MERGE/UNIQUE · **DD** DDL vs DML ·
**AB** abort/rollback incl. indexes · **HZ** horizon/GC · **RO** read-only txns · **SE**
sessions · **FP** frontier/publish · **DR** durability/recovery.

| Facet | Scenario IDs | Count | < 3? |
|---|---|---:|---|
| SR | WW14, WW15, SK12, SK13, SK14, SK15, SK16, SK17, IX06, IX07, IX08, IX11, RO02, GG01, GG03, GG04, L03, L16, L17, L21, L22 | 21 | no |
| OW | IX01, IX02, IX03, IX10, L07, MG09(a), AB06 | 7 | no |
| WW | WW01-WW13, MG06, MG08, RI03, RI05, RI06, SK17, L04, L05 | 21 | no |
| AN | SK01-SK11, SK15, SK16, SK17, L01, L02 | 16 | no |
| MU | MG01-MG12, L13, L14 | 14 | no |
| DD | DD01-DD09, IX08, L19, D11 | 12 | no |
| AB | WW05, WW12, WW13, SK15, IX04, IX05, AB01-AB06, GG07, HZ03, L18, D05, D06, D19 | 18 | no |
| HZ | HZ01-HZ04, L08, L09, L18, L21 | 8 | no |
| RO | RO01, RO02, RO03, SK06, SK07, SK08, L01 (read arm) | 7 | no |
| SE | SE01, SE02, SE04, L11 | 4 | no |
| FP | SE02, SE03, L10, D18 | 4 | no |
| DR | D01-D19, GG07, L22 | 21 | no |

**FP sat at the floor (3) before Turso's D18 was added.** Its rows rely on seam H2, which `lpg.Graph.AllocateCommitTS`
provides (G1 refuted). Proposed GoGraph-specific additions:

| ID | Proposal | Facet | Pri |
|---|---|---|---|
| FP01 | Many later commits ACK while one straggler is held (8 goroutines × 256 commits over one held commit, the shape of the #2932 clock test lifted to the Cypher engine); assert none `<waiting>` and one-step frontier catch-up | FP | H |
| FP02 | Straggler that ABANDONS (fsync failure) with later commits pending: frontier catches up, sessions waiting on it are released with the right error | FP | H |
| FP03 | `AwaitVisible` honours ctx: a session read behind a held straggler returns the ctx error within deadline | FP, SE | M |

Graph-shape additions already in the catalogue: GG01 (variable-length traversal under
edge churn), GG02 (detach delete vs edge create), GG04 (label index churn), RI01-RI03
(edge create vs endpoint delete, hub contention), L06/L15/L16 (their randomised forms).

---

## 6. Scenarios that would have caught the three recent defects

| Defect | Would have caught it | Confidence |
|---|---|---|
| **#2814** seek missed own writes | IX01, IX02, IX03, IX10, L07; IX06 partially | High for IX01-IX03: they are the measured reproduction in the fix commit `efd32fb9` |
| **#2931** commit writeback read a peer's uncommitted value; survived rollback | IX04 (the exact four-step), IX09, L06, L19. **Not IX05** at `43c69dbe`: measured there, IX04 fails and IX05 passes, because at that revision any registered UNIQUE constraint made a label add and a property write on one node conflict ("node constraint"), which closed the peer route #2931 took. Since #3008 that conflict is taken only for a label or property key a constraint names, which IX05's `L` and `s` are not, so the route is no longer closed in IX05; whether IX05 now catches #2931 is not measured. **Not DD01 or DD05**: the CREATE INDEX and CREATE CONSTRAINT backfills read a committed snapshot (G9, settled by DD01 and DD05) | High for IX04 and L06 (they are the working-tree regression tests' shapes) |
| **#2932** publish convoy stalled the frontier | L10 and L04 (streak gate, b_ac); FP01 | Medium: the deterministic catch lives at the clock level (`graph/mvcc/publish_convoy_test.go`); SE02/FP01 observe the liveness shape on seam H2 (`lpg.Graph.AllocateCommitTS`). A Cypher-level deterministic scenario is not guaranteed to reproduce a scheduling-dependent convoy |

---

## 7. InnoDB-only ideas PostgreSQL's isolation suite lacks

1. **Index hides own uncommitted rows by design** — `innodb_fts/transaction.test` states
   that uncommitted rows are invisible through the FTS index but visible to non-index
   queries. That is #2814's defect shape, documented as intended behaviour in a
   production engine. → IX01-IX03.
2. **Read view created at first read, not at BEGIN** — `t/consistent_snapshot` test 2.
   GoGraph takes it at BEGIN (F6). → SK13.
3. **Secondary-index MVCC against a read view established before the peer's insert into
   two indexes** — `multi_value_index_merge_mvcc`. → IX07.
4. **Duplicate unique values after purge of delete-marked records** —
   `innodb-lock-inherit-read_commited`. → MG09.
5. **Online index build loses rows under concurrent purge** —
   `alter_table_rebuild_missing_record`; spurious duplicate-key under concurrent insert —
   `alter_table_rebuild_duplicate_record`; rollback of NULL-valued DML during index
   creation — `index-create-dml-rollback`. → DD01, DD05, DD09.
6. **Purge lag and history-list growth as an observable with warnings** — `bug120529`,
   `purge_on_replica`. → HZ01, L08.
7. **Transaction-weight victim selection and CATS scheduling** — `innodb_cats`,
   `innodb_trx_weight`, `high_prio_trx_*`. → L05 (and gap G3).
8. **Crash at every point of the redo-log write pipeline** — `innodb_redo_debug_1`..`_6`.
   → D03.
9. **trx-id reservation margin across restart** — `max_trx_id`. → D07.
10. **Group-commit ordering and gap-free executed set when a later group member finishes
    first** — `binlog_group_commit_gtid_order`, `binlog_after_commit_order_info_schema`.
    → SE02, D15, L10.
11. **Crash between engine log flush and binlog flush** — `binlog_group_commit_flush_crash`.
    → D03, D04, SE03.
12. **Crash-safe undo truncation** — `innodb_undo/create_undo_crash_recover`,
    `truncate_*`. → D09.
13. **Autocommit non-locking SELECT kept out of the transaction list** —
    `innodb-ac-non-locking-select`. → HZ02.
14. **Semi-consistent read** (UPDATE skips rows locked by others that do not match) —
    `innodb-semi-consistent`. → WW06, WW07.

---

## 8. Gaps (findings for the user, not scope)

| # | Gap | Pointed to by | Consequence |
|---|---|---|---|
| G1 | **No commit-hold seam** for deterministic frontier scenarios (no exported hook between commit-timestamp allocation and publication). | PG-rec `057_snapshot_commit_race`, PG-inj `repack_commit_race`, MY `binlog_group_commit_gtid_order` (all use injection/sync points) | **Refuted: frontier scenarios are not blocked by a missing hook.** `lpg.Graph.AllocateCommitTS` reserves a commit instant without publishing it, and the transaction's later `EndVersionedTx` publishes or abandons it (H2). Example 37 pins SE02, SE04, SE03's fsync arm and FP01-FP03 on it in fixed interleavings. |
| G2 | **No deterministic vacuum control in step scripts**; the vacuum is a background goroutine, so write-after-rollback goldens race (F5). | PG `horizons` (uses explicit VACUUM), MY `innodb_purge_stop_now`/`purge_run_now` in `flush-hang`, `lob_purge`, `virtual_purge` | Needs a Hook that calls `ReclaimNow`, or a "pause vacuum" control equal to InnoDB's `innodb_purge_stop_now`. **Turso takes the opposite control:** it forces reclamation rather than pausing it — `PRAGMA mvcc_gc_threshold = 1` runs a GC pass on every commit (TU `core/translate/pragma.rs:727-734`, used by `shuttle_mvcc.rs:782`) — and scripts interleavings through named yield and failure injection points in its state machines (TU `core/mvcc/yield_points.rs:5-27`). GoGraph's `ReclaimNow` (`graph/lpg/mvcc_gc.go:191-204`) is the forcing control; L21 uses it continuously. A pause control remains absent. |
| G3 | **No fairness or priority mechanism** under first-updater-wins; a large transaction can be refused indefinitely by small ones. | MY `innodb_cats`, `innodb_trx_weight`, `high_prio_trx_*` | L05 can only report starvation, not bound it. |
| G4 | **No locking read** (SELECT FOR UPDATE) to prevent write skew; the only remedy is a dummy write, and whether a value-preserving write conflicts is unverified here. | PG `simple-write-skew` et al. (SSI prevents), MY `t/locking_clause` | SK11 value-preserving arm must be pinned. |
| G5 | **No hook inside MERGE** between probe and create, so the speculative-insertion race cannot be scripted. | PG `insert-conflict-specconflict`, PG-inj `on_conflict_probe_window`; TU `core/mvcc/yield_points.rs:5-27` | MG11 is only reachable by randomised load (L13). Turso's `YieldInjector`/`FailureInjector` traits are the prior art for such a seam: a test-only hook consulted at named, resumable boundaries of a state machine. |
| G6 | **UNIQUE against an in-flight reservation is a non-retriable violation**, even when the reserver later rolls back; PostgreSQL waits and InnoDB waits on an S lock. | PG `insert-conflict-do-nothing`, `read-write-unique`; MY `iodku`, `innodb_replace` | A client cannot distinguish "value taken" from "value momentarily reserved". `constraints.go:869-876` names the refinement (reserver txn id) as pending. **Turso resolves it at COMMIT:** two open transactions may both insert one key, and the second committer fails with the retriable `WriteWriteConflict`, first-committer-wins over index keys (TU `core/mvcc/database/mod.rs:2124-2135`; tests `db/tests.rs:14008-14038`, `:14174-14190`); a reserver's rollback therefore lets the other commit. |
| G7 | **No online consistency verifier** (amcheck `bt_index_check(heapallindexed)` analogue) exposed for index-vs-graph agreement; each harness re-derives "seek = scan". | PG amcheck `t/001_verify_heapam.pl`, `t/002_cic.pl`, `t/004_verify_nbtree_unique.pl` | D10, L06, L19 each need their own oracle. **Unverified**: internal invariant checkers may exist (`docs/test-battery.md` not read). **Turso exposes one:** `PRAGMA integrity_check` (B-tree and index agreement) is the oracle its MVCC tests and simulators call after checkpoints and faults (TU `db/tests.rs:9823-9845`, `testing/concurrent-simulator/chaotic_btree.rs:357`). |
| G8 | **Contradictory documentation on adjacency append conflicts**: `addEdgeInfo` godoc (`graph/lpg/lpg.go:2239-2242`) says an append never conflicts with another append; `checkAppend` (`graph/lpg/mvcc_adjversion.go:145-157,196`) refuses it (rmp #2445). | PG `fk-contention` | **Settled by RI03: the code is right.** Of two transactions appending an edge to one node — same target or same source — the second is refused, through Cypher and through `lpg`, in every interleaving (`ri03-hub-same-target`, `ri03-hub-same-source`, both with `-lpg`). The `addEdgeInfo` godoc had already been corrected by commit `1ad20d03`; the stale text left was the comment on the `Graph.adjVer` field (`graph/lpg/lpg.go`), which said an append "is commutative and must not conflict with another append". It now states the rmp #2445 rule. |
| G9 | **Backfill source for CREATE INDEX / CONSTRAINT not established**: whether it reads a committed snapshot or the present graph (which holds open peers' writes) was not verified. | PG `multiple-cic` (CIC waits for older txns), MY `index-create-dml-rollback` | **Settled: a committed snapshot.** DD01 (index) and DD05 (UNIQUE constraint) pass in every interleaving, and DD01 and DD09 fail against a test-only mutant whose backfill reads the live graph (`examples/37_mvcc_write_contention/README.md`, negative control for G9). DD01/DD05 do not reproduce the #2931 class. |
| G10 | **Transaction state after a refused statement is not pinned**: whether COMMIT returns `ErrTxPoisoned` or the conflict after an `Exec` returned `ErrSerializationConflict`. | MY `t/innodb_deadlock`, `innodb_mysql_rbk` | AB03 must pin it. |
| G11 | **WAL frames are neither chained nor salted** (Turso-derived): each frame's CRC32C covers only its own bytes (`store/wal/FORMAT.md`, "Frame layout" and "Integrity"), so a CRC-valid frame from another file or an earlier generation, lying at a frame boundary, would be replayed. | TU `core/mvcc/persistent_storage/logical_log.rs:6097` (`test_crc_chain_invalidates_suffix_on_corruption`), `:6160` (`test_splice_frame_from_different_log_rejected`), `:6012` (`test_truncation_regenerates_salt`); TU `docs/internals/mvcc/RECOVERY_SEMANTICS.md:16-19` | Not reachable through GoGraph's own write path: prefix truncation replaces the file by atomic rename (`store/checkpoint/checkpoint.go:778-781`). It matters for misdirected or stale writes below the file system. **Unverified:** whether replay checks the per-transaction sequence (`store/recovery/recovery.go:162-175`) for density, which would reject an out-of-order frame. |

---

## 9. Recommended changes to the acceptance criteria

**#2933**
1. Raise "at least 30 scenarios" to the H-priority #2933 set (55 rows) or state the chosen
   subset by ID; 30 does not cover IX, DD and RI together.
2. Add a **vacuum-drain requirement** (H1): every scenario with a write after a rollback
   drains the vacuum at a named step, or the golden records a race.
3. Add the RI family explicitly (RI01-RI04, GG02): "an edge to a dead node is never
   committed in any permutation". It is absent from a_fr's family list except as
   "delete vs create-edge".
4. Add **WW13** (void-primitive conflict at COMMIT) to the write-conflict family: it is the
   one conflict the statement-level harness cannot see.
5. Make the #2814 negative control cover all three shapes (IX01 hash, IX02 btree, IX03
   autocommit), not one.
6. Add a negative control for DD01 against `43c69dbe` if G9 shows the backfill reads the
   present graph.

**#2934**
7. Add the **abort-heavy arm** (L18) with `Total <= Bound` after quiescence; the current
   growth criterion is stated only for the long-reader arm.
8. Add a **dangling-edge check at quiescence** (L15) beside "seek equals scan".
9. State the frontier criterion in measurable terms: maximum visibility lag and maximum
   `InFlightCommits` per level, not only the streak gate.

**#2935**
10. Define the **in-doubt class**: a commit in flight at the crash (not yet ACKed, not
    refused) may be present or absent, but only wholly (D03, D05). As written,
    "no refused or rolled-back write is present" does not say how an unacknowledged
    in-flight commit is scored, so the check is either vacuous or flaky on it.
11. Add the **torn-tail** (D08) and **checkpoint-crash** (D09) arms; the criteria name only
    abandon and `kill -9`.
12. Add "a new session after recovery observes every ACKed commit" to "the clock is not
    rewound" (D07): a clock can be ratcheted while the frontier still hides a commit.
13. Add the **fsync-failure fail-all** arm (D04) as a second negative control beside the
    dropped-record seam.

---

## E. Exclusions

### E.1 PostgreSQL isolation specs (135): disposition of every spec

K = kept as a source (IDs given). X = excluded (reason given).

| Spec | K/X | IDs or reason |
|---|---|---|
| `aborted-keyrevoke` | K | WW05, AB01 |
| `alter-domain-validate` | X | No domains or domain constraints. |
| `alter-table-1` | K | DD05, MG12 |
| `alter-table-2` | X | Adds SQL foreign keys by DDL; GoGraph edges are native, no FK DDL (RI covered by RI01-RI05). |
| `alter-table-3` | X | Triggers. |
| `alter-table-4` | X | Table inheritance. |
| `async-notify` | X | LISTEN/NOTIFY. |
| `classroom-scheduling` | K | SK03 |
| `cluster-conflict-partition` | X | CLUSTER and partitions. |
| `cluster-conflict` | X | CLUSTER command and its lock conflicts. |
| `cluster-toast-value-reuse` | X | CLUSTER rewrite and TOAST. |
| `create-trigger` | X | Triggers. |
| `ddl-dependency-locking` | K | DD08 |
| `deadlock-hard` | K | WW11 |
| `deadlock-parallel` | X | Parallel worker groups and advisory locks. |
| `deadlock-simple` | K | WW10 |
| `deadlock-soft-2` | X | Soft wait-edge reversal in a lock queue; GoGraph DML never queues. |
| `deadlock-soft` | X | Soft wait-edge reversal in a lock queue; GoGraph DML never queues. |
| `delete-abort-savept-2` | K | AB01 (savepoint half excluded) |
| `delete-abort-savept` | K | AB01 (savepoint half excluded) |
| `detach-partition-concurrently-1` | X | Partitions. |
| `detach-partition-concurrently-2` | X | Partitions. |
| `detach-partition-concurrently-3` | X | Partitions. |
| `detach-partition-concurrently-4` | X | Partitions and SQL FKs. |
| `drop-index-concurrently-1` | K | DD03, IX08 |
| `drop-owned-grant` | X | Roles and grants. |
| `eval-plan-qual-trigger` | K | WW12 (base case only; trigger cases excluded) |
| `eval-plan-qual` | K | WW02, WW03, WW04, WW06 |
| `fk-concurrent-pk-upd` | K | RI01 |
| `fk-contention` | K | RI03, L15 |
| `fk-crosstype-recheck` | X | Cross-type FK equality operators. |
| `fk-deadlock` | K | RI05, WW10 |
| `fk-deadlock2` | K | RI05, WW10 |
| `fk-fastpath-null-key` | X | FK onto a nullable UNIQUE key; edges reference node identity, not a key. |
| `fk-partitioned-1` | K | RI01 (partition half excluded) |
| `fk-partitioned-2` | X | Partitioned FK targets. |
| `fk-snapshot-2` | K | RI01 |
| `fk-snapshot-3` | X | Temporal (range) FKs. |
| `fk-snapshot` | K | RI02, SK13 |
| `freeze-the-dead` | X | Tuple freezing and multixacts; no xid wraparound in GoGraph. |
| `horizons` | K | HZ01, L08 |
| `index-only-bitmapscan` | K | SK14 |
| `index-only-scan` | K | SK10 |
| `inherit-temp` | X | Inheritance and temporary tables. |
| `inplace-inval` | X | Catalog in-place update invalidation. |
| `insert-conflict-do-nothing-2` | K | MG03 |
| `insert-conflict-do-nothing` | K | MG02 |
| `insert-conflict-do-select` | X | Locking strengths (FOR KEY SHARE etc.); GoGraph has no locking reads. Lock-free half covered by MG02. |
| `insert-conflict-do-update-2` | K | MG07 |
| `insert-conflict-do-update-3` | K | MG06 |
| `insert-conflict-do-update-4` | X | Partitioned table plus SELECT FOR UPDATE. |
| `insert-conflict-do-update` | K | MG02, MG06, L13 |
| `insert-conflict-serializable` | K | SK10 (SSI half excluded) |
| `insert-conflict-specconflict` | K | MG11 |
| `intra-grant-inplace-db` | X | Catalog GRANT and in-place updates. |
| `intra-grant-inplace` | X | Catalog GRANT and in-place updates. |
| `lock-committed-keyupdate` | K | WW02 |
| `lock-committed-update` | K | WW02 |
| `lock-nowait` | X | NOWAIT lock queue insertion. |
| `lock-update-delete` | K | WW05 |
| `lock-update-traversal` | K | WW05 |
| `matview-write-skew` | K | SK10 (derived-summary shape; matview itself absent) |
| `merge-delete` | K | MG08 |
| `merge-insert-update` | K | MG01, L14 |
| `merge-join` | X | EPQ recheck across SQL join methods; no equivalent planner operators under MERGE. |
| `merge-match-recheck` | K | MG06, WW06 |
| `merge-update` | K | MG06, WW06 |
| `multiple-cic` | K | DD01, DD02, L20 |
| `multiple-row-versions` | K | WW14, WW15 |
| `multixact-no-deadlock` | X | Shared row locks and multixacts. |
| `multixact-no-forget` | K | WW12 (lock half excluded) |
| `multixact-stats` | X | Multixact statistics. |
| `nowait-2` | X | NOWAIT with multixacts. |
| `nowait-3` | X | NOWAIT with tuple locks. |
| `nowait-4` | X | NOWAIT on an update chain. |
| `nowait-5` | X | NOWAIT on an update chain. |
| `nowait` | X | NOWAIT row locks. |
| `partial-index` | K | IX04, IX06 |
| `partition-concurrent-attach` | X | Partitions. |
| `partition-drop-index-locking` | X | Partitions. |
| `partition-key-update-1` | X | Partitions. |
| `partition-key-update-2` | X | Partitions. |
| `partition-key-update-3` | X | Partitions. |
| `partition-key-update-4` | K | IX06 (row moves between index domains; partition half excluded) |
| `plpgsql-toast` | X | PL/pgSQL and TOAST. |
| `predicate-gin` | X | SSI predicate locks on GIN. |
| `predicate-gist` | X | SSI predicate locks on GiST. |
| `predicate-hash` | K | SK10 (SSI half excluded) |
| `predicate-lock-hot-tuple` | X | SSI predicate locks on HOT tuples. |
| `prepared-transactions-cic` | X | Two-phase commit. |
| `prepared-transactions` | X | Two-phase commit. |
| `project-manager` | K | SK04 |
| `propagate-lock-delete` | X | Tuple-lock propagation. |
| `pub-concurrent-drop` | X | Logical-replication publications. |
| `read-only-anomaly-2` | K | SK06 (SSI outcome recorded as divergence) |
| `read-only-anomaly-3` | X | DEFERRABLE read-only transactions. |
| `read-only-anomaly` | K | SK06, L03 |
| `read-write-unique-2` | K | MG03, MG04 |
| `read-write-unique-3` | K | MG04, MG09 |
| `read-write-unique-4` | K | MG05 |
| `read-write-unique` | K | MG04 |
| `receipt-report` | K | SK07 |
| `referential-integrity` | K | RI04 |
| `reindex-concurrently-toast` | X | TOAST relations. |
| `reindex-concurrently` | K | DD04 |
| `reindex-schema` | X | REINDEX SCHEMA with concurrent DROP TABLE. |
| `ri-trigger` | K | RI04 |
| `sequence-ddl` | X | Sequences. |
| `serializable-parallel-2` | X | SSI RO_SAFE flag in parallel query. |
| `serializable-parallel-3` | X | SSI RO_SAFE flag in parallel query. |
| `serializable-parallel` | K | RO03 |
| `simple-write-skew` | K | SK02, L02 |
| `skip-locked-2` | X | SKIP LOCKED. |
| `skip-locked-3` | X | SKIP LOCKED. |
| `skip-locked-4` | X | SKIP LOCKED. |
| `skip-locked` | X | SKIP LOCKED. |
| `stats` | X | Cumulative statistics subsystem. |
| `subxid-overflow` | X | Subtransactions. |
| `tablespace-dependency-locking` | X | Tablespaces. |
| `temp-schema-cleanup` | X | Temporary schemas. |
| `temporal-range-integrity` | K | SK03 |
| `timeouts` | K | DD06 |
| `total-cash` | K | SK05, L01 |
| `truncate-conflict` | K | GG05 (privilege half excluded) |
| `tuplelock-conflict` | X | Tuple lock-level conflict table. |
| `tuplelock-partition` | X | Partitions and tuple locks. |
| `tuplelock-update` | X | Tuple locks with advisory-lock choreography. |
| `tuplelock-upgrade-no-deadlock` | X | Tuple lock upgrade queues. |
| `two-ids` | K | SK08 |
| `update-conflict-out` | K | WW14 |
| `update-locked-tuple` | X | Updating a row held by a tuple lock; no locking reads. |
| `vacuum-concurrent-drop` | K | HZ04 |
| `vacuum-conflict` | X | VACUUM privileges and lock waits. |
| `vacuum-no-cleanup-lock` | K | HZ01 |
| `vacuum-skip-locked` | X | VACUUM SKIP_LOCKED. |

Totals: 64 kept, 71 excluded.

### E.2 Other PostgreSQL sources

| Source | Disposition |
|---|---|
| PG-inj `repack_commit_race` | K — SE02 (snapshot must wait for a transaction between commit record and procarray removal) |
| PG-inj `ri_fastpath_snapshot` | K — RI01 |
| PG-inj `on_conflict_probe_window` | K — MG11 (SSI half excluded) |
| PG-inj `basic`, `wait_cleanup` | X — injection-point infrastructure tests |
| PG-inj `heap_lock_update` | X — tuple-lock race |
| PG-inj `inplace`, `syscache-update-pruned` | X — catalog in-place updates and syscache |
| PG-inj `reindex_concurrently_deferred` | X — DEFERRABLE unique constraints |
| PG-inj `repack`, `repack_decode`, `repack_missingval`, `repack_temporal`, `repack_temporal_multirange`, `repack_toast` | X — REPACK command |
| PG-inj `ri_fastpath_reindex` | X — REINDEX during the RI fast path |
| PG-rec `013_crash_restart` | K — D01, D02, D06 |
| PG-rec `039_end_of_wal`, `026_overwrite_contrecord` | K — D08 (replica half of 026 excluded) |
| PG-rec `048_vacuum_horizon_floor` | K — L08 |
| PG-rec `050_redo_segment_missing`, `052_checkpoint_segment_missing` | K — D09 |
| PG-rec `057_snapshot_commit_race` | K — SE01, SE02, D07, L10 |
| PG-rec other 49 TAP tests | X — replication, archiving, PITR, timelines, logical slots, standby, stats: no replica or archive in GoGraph (named, not read) |
| amcheck `t/002_cic.pl` | K — DD01, L19, D10 |
| amcheck `t/004_verify_nbtree_unique.pl` | K — D10 |
| amcheck `t/001_verify_heapam.pl` | X — heap page corruption detection; recorded as gap G7 |
| amcheck `t/003_cic_2pc.pl`, `t/005_pitr.pl`, `t/006_verify_gin.pl` | X — 2PC, PITR, GIN |

### E.3a InnoDB tests kept as sources (`suite/innodb` + `suite/innodb_undo`, 92)

`alter_table_rebuild_duplicate_record` `alter_table_rebuild_missing_record` `blob-crash-16k` `blob-crash-4k` `bug120529` `bug32554667` `bulk_create_index_online` `cats-autoinc` `constraint_check_locks_in_read_committed` `create_table_select` `create_undo_crash_recover` `ddl_prepare` `deadlock_detect` `deadlock_in_subquery` `deadlock_on_lock_upgrade` `deadlock_stats` `doublewrite` `fast_shutdown` `flush-hang` `high_prio_trx_2` `high_prio_trx_6` `high_prio_trx_9` `hp_deadlock` `index-create-dml-rollback` `innodb_bug26818787` `innodb_bug30113362` `innodb_bug31205266` `innodb_bug49164` `innodb_bug52663` `innodb_bug53756` `innodb_cats` `innodb_i_s_innodb_trx` `innodb_lock_wait_timeout_1` `innodb_mysql_rbk` `innodb_redo_debug_1` `innodb_redo_debug_2` `innodb_redo_debug_3` `innodb_redo_debug_4` `innodb_redo_debug_5` `innodb_redo_debug_6` `innodb_replace` `innodb_trx_weight` `innodb-ac-non-locking-select` `innodb-alter-debug` `innodb-consistent` `innodb-index` `innodb-index-online` `innodb-index-online-delete` `innodb-index-online-purge` `innodb-lock` `innodb-lock-inherit-read_commited` `innodb-read-view` `innodb-semi-consistent` `innodb-table-online` `innodb-timeout` `iodku` `iodku_debug` `json_small_partial_update_06` `lob_big_rollback` `lob_mvcc_undo` `lob_partial_update_concurrent` `lob_purge` `lob_rollback_update` `lob_vjhi` `lock_impl_to_expl_case_sensitivity` `lock-inherit-existing` `log_file_consumer` `log_file_producer` `log_first_rec_group` `log_writer_extra_margin` `long_deadlock_cycle` `max_trx_id` `multi_value_debug` `multi_value_index_merge_mvcc` `parallel_read` `parallel_read_kill` `purge_on_replica` `resurrection_logs` `select_count_perf` `truncate_new` `truncate_new_explicit` `truncate_undo_recover_fixup` `undetected_deadlock` `undo_log_temp_table` `undo_truncate_upgrade` `update-cascade` `virtual_debug_purge` `zlob_big_rollback` `zlob_ibd_size` `zlob_insert_by_update` `zlob_partial_update_concurrent` `zlob_rollback_update` 

### E.3 InnoDB exclusions, one line each (`suite/innodb` + `suite/innodb_undo`, 752 tests)

Reason is the category the test belongs to; each was triaged by name and by a body token scan (isolation levels, CONSISTENT SNAPSHOT, purge/history, deadlock/lock-wait, duplicate-key, crash/restart, debug_sync, rollback/savepoint). None carries a concurrent-MVCC semantic that GoGraph has and that the kept set does not already cover.

- `innodb/accessing_table_during_fulltext_creation` — full-text index: absent
- `innodb/add_foreign_key` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/ahi_persist_during_buf_pool_resize` — buffer pool / page I-O: absent
- `innodb/alter_add_pk_pq_crash` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_crash` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_datadir_path` — tablespaces / files / page format: absent
- `innodb/alter_datadir_path_on_upgrade` — tablespaces / files / page format: absent
- `innodb/alter_foreign_crash` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_kill` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_missing_tablespace` — tablespaces / files / page format: absent
- `innodb/alter_multivalue` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_page_size` — tablespaces / files / page format: absent
- `innodb/alter_rename_existing` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_rename_existing_xtra` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_rename_files` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_rename_self_referencing_fk` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_table_redundant` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_table_stage_progress` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_table_temp_files_io` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_table_update_primary_key` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/alter_tablespace_partition` — tablespaces / files / page format: absent
- `innodb/alter-compressed` — page compression: absent
- `innodb/analyze_table` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/atomic_rename_debug_database` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/atomic_truncate_crash` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/attachable_trx` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/auto_increment` — auto-increment: absent
- `innodb/autoinc_debug` — auto-increment: absent
- `innodb/autoinc_persist` — auto-increment: absent
- `innodb/autoinc_persist_deadlock` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/autoinc_persist_debug` — auto-increment: absent
- `innodb/avoid_deadlock_with_blocked` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/blob_page_reserve` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/blob_partial_update` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/blob_partial_update_2` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/blob_redo` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/blob_update_rollback` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/blob-crash` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/blob-update-debug` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/bootstrap` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/btree_load_root_split` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/buf_page_read_simulate_incorrect_read` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/buf_pool_resize_oom` — buffer pool / page I-O: absent
- `innodb/buf_pool_resize_status_codes` — buffer pool / page I-O: absent
- `innodb/bug33657235` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug33767814` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug33788578` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug33788578_ddl` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/bug33788578_incorrect_instant_field` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/bug33788578_online_ddl` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/bug33788578_rec_IV_set` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug33788578_release` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug34307874` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug34323538` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug34574604` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug34790366` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug35006212` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/bug37645185` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/builder_error_case` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/builder_insert_direct_freed` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/cascade_lock_wait` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/case_insensitive_fs` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/check_ibd_filesize_16k` — tablespaces / files / page format: absent
- `innodb/check_sector_size` — tablespaces / files / page format: absent
- `innodb/check_table_debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/checkpoint_too_soon` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/checksum` — tablespaces / files / page format: absent
- `innodb/choose_tbsp_location-alter` — tablespaces / files / page format: absent
- `innodb/choose_tbsp_location-debug` — tablespaces / files / page format: absent
- `innodb/choose_tbsp_location-discard` — tablespaces / files / page format: absent
- `innodb/cmp_per_index` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/compressed_table_recovery` — page compression: absent
- `innodb/crc32_endianness` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/create_drop_temp_table_perf_1` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/create_drop_temp_table_perf_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/create_index_excess_memory` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/create_index_with_disable_sort_file_cache` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/create_table` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/create_table_similar_to_fts` — full-text index: absent
- `innodb/create_table_sys_tablespace_extend` — tablespaces / files / page format: absent
- `innodb/create_tablespace` — tablespaces / files / page format: absent
- `innodb/create_tablespace_16k` — tablespaces / files / page format: absent
- `innodb/create_tablespace_32k` — tablespaces / files / page format: absent
- `innodb/create_tablespace_4k` — tablespaces / files / page format: absent
- `innodb/create_tablespace_64k` — tablespaces / files / page format: absent
- `innodb/create_tablespace_8k` — tablespaces / files / page format: absent
- `innodb/create_tablespace_debug` — tablespaces / files / page format: absent
- `innodb/create_tablespace_partition` — tablespaces / files / page format: absent
- `innodb/create_tablespace_replication` — tablespaces / files / page format: absent
- `innodb/create-index` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/cross_ddl_partition_tables` — partitioning: absent
- `innodb/dblwr_disable` — tablespaces / files / page format: absent
- `innodb/dblwr_encrypt` — encryption: absent
- `innodb/dblwr_encrypt_recover` — encryption: absent
- `innodb/dblwr_encrypt_recover1` — encryption: absent
- `innodb/dblwr_encrypt_rowcomp` — encryption: absent
- `innodb/dblwr_lz4_encrypt` — encryption: absent
- `innodb/dblwr_lz4_encrypt_recv` — encryption: absent
- `innodb/dblwr_page_size_mismatch_warning` — tablespaces / files / page format: absent
- `innodb/dblwr_recover_general_tablespace` — tablespaces / files / page format: absent
- `innodb/dblwr_recover_zeroes` — tablespaces / files / page format: absent
- `innodb/dblwr_unencrypt` — encryption: absent
- `innodb/dblwr_zlib_encrypt` — encryption: absent
- `innodb/dblwr_zlib_encrypt_recv` — encryption: absent
- `innodb/ddl_add_drop_basic` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/ddl_crash_alter_partition` — partitioning: absent
- `innodb/ddl_crash_alter_table` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/ddl_crash_alter_table_partition` — partitioning: absent
- `innodb/ddl_crash_alter_table_partition_tablespace` — tablespaces / files / page format: absent
- `innodb/ddl_crash_alter_table_tablespace` — tablespaces / files / page format: absent
- `innodb/ddl_crash_basic` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/ddl_kill` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/ddl_large_record` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/ddl_log_error` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/default_row_format` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/default_row_format_16k` — tablespaces / files / page format: absent
- `innodb/default_row_format_compatibility` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/default_row_format_tablespace` — tablespaces / files / page format: absent
- `innodb/dirty_invalid_page` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/disable_ahi_other_blocks` — buffer pool / page I-O: absent
- `innodb/disable_flush_restart_mysqld` — buffer pool / page I-O: absent
- `innodb/disable_log_encryption` — encryption: absent
- `innodb/discard_tablespace` — tablespaces / files / page format: absent
- `innodb/discard_tablespace_debug` — tablespaces / files / page format: absent
- `innodb/discarded_partition` — tablespaces / files / page format: absent
- `innodb/discarded_partition_create` — tablespaces / files / page format: absent
- `innodb/dml_operations_temp_table` — temporary tables: absent
- `innodb/dml_operations_temp_table_debug` — temporary tables: absent
- `innodb/doublewrite_on_to_reduced` — tablespaces / files / page format: absent
- `innodb/doublewrite_reduced` — tablespaces / files / page format: absent
- `innodb/doublewrite_reduced_odirect` — tablespaces / files / page format: absent
- `innodb/doublewrite_reduced_to_on` — tablespaces / files / page format: absent
- `innodb/dropdb` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/end_range_check` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/end_range_check_2` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/events-merge-tmp-path` — temporary tables: absent
- `innodb/extern_relative_path` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/extra_pages_in_system_tablespace` — tablespaces / files / page format: absent
- `innodb/file_format_upgrade_16k` — tablespaces / files / page format: absent
- `innodb/fk_copy_alter_eviction` — buffer pool / page I-O: absent
- `innodb/flush_drop_gtid_deadlock` — buffer pool / page I-O: absent
- `innodb/foreign_key` — SQL foreign keys: absent (edges are native RI)
- `innodb/fseg_reserve_factor` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/fts_exec_interrupt` — full-text index: absent
- `innodb/generate_new_row_id` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/gtid_shutdown` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/help_verbose` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/hidden_directory_dotfile` — tablespaces / files / page format: absent
- `innodb/hidden_directory_win` — tablespaces / files / page format: absent
- `innodb/high_prio_trx_1` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_3` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_4` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_5` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_7` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_8` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_commit_crash` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_debug` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_fk` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_predicate` — buffer pool / page I-O: absent
- `innodb/high_prio_trx_rpl` — buffer pool / page I-O: absent
- `innodb/histogram` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/histogram-debug` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/hp_deadlock_shutdown` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb_undo/i_s_files_16k` — tablespaces / files / page format: absent
- `innodb_undo/i_s_files_32k` — tablespaces / files / page format: absent
- `innodb_undo/i_s_files_4k` — tablespaces / files / page format: absent
- `innodb_undo/i_s_files_64k` — tablespaces / files / page format: absent
- `innodb_undo/i_s_files_8k` — tablespaces / files / page format: absent
- `innodb/i_s_files_debug` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/i_s_innodb_tables` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/ibd2sdi` — tablespaces / files / page format: absent
- `innodb/ibuf_not_empty` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/ibuf_sys_tablespace_extend` — tablespaces / files / page format: absent
- `innodb/ibuf_with_validate_tablespace` — tablespaces / files / page format: absent
- `innodb/implicit_to_explicit_conversion` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/import` — tablespaces / files / page format: absent
- `innodb/import_cfg` — tablespaces / files / page format: absent
- `innodb/import_compress_encrypt` — encryption: absent
- `innodb/import_compress_no_punch_hole` — tablespaces / files / page format: absent
- `innodb/import_empty_instant_default` — tablespaces / files / page format: absent
- `innodb/import_export_4k` — tablespaces / files / page format: absent
- `innodb/import_table_with_multibyte_chars` — tablespaces / files / page format: absent
- `innodb/import_tablespace_page_corrupt` — tablespaces / files / page format: absent
- `innodb/import_tablespace_schema_missmatch` — tablespaces / files / page format: absent
- `innodb/import_update_stats` — tablespaces / files / page format: absent
- `innodb/index_merge_threshold` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/index_tree_operation` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/index-lock-mode` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/index-online-norebuild` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/index-stats-during-analyze` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innochecksum_1` — tablespaces / files / page format: absent
- `innodb/innochecksum_linux` — tablespaces / files / page format: absent
- `innodb/innodb` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_32k` — tablespaces / files / page format: absent
- `innodb/innodb_64k` — tablespaces / files / page format: absent
- `innodb/innodb_autoextend_dml_1` — tablespaces / files / page format: absent
- `innodb/innodb_autoextend_dml_2` — tablespaces / files / page format: absent
- `innodb/innodb_autoextend_import_export` — tablespaces / files / page format: absent
- `innodb/innodb_autoextend_table_ddl` — tablespaces / files / page format: absent
- `innodb/innodb_autoextend_tbsp_ddl` — tablespaces / files / page format: absent
- `innodb/innodb_autoinc_lock_mode_zero` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/innodb_autoinc_preallocate` — auto-increment: absent
- `innodb/innodb_buffer_pool_chunk_size_rounding` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_dump_pct` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_load` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_load_now` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_resize` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_resize_compressed_table` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_resize_debug` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_resize_with_chunks` — buffer pool / page I-O: absent
- `innodb/innodb_buffer_pool_resize_with_instances` — buffer pool / page I-O: absent
- `innodb/innodb_bug-13628249` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug115136` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug11754376` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug11766634` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug11789106` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug11933790` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug12400341` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug12429573` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug12661768` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug14006907` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug14007109` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug14007649` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug14147491` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug14169459` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug14676111` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug14704286` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug19164038` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug21704` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug29692250` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug30423` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug30594501` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug30899683` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug30919` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug33405696` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug33766482` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug34053` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug34300` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug34750489` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug35220` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug38231` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug39438` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug40360` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug40565` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug41904` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug42101` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug42101-nonzero` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug42419` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug44032` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug44369` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug44571` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug45357` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug46000` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug46676` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug47621` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug47622` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug47777` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug48024` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug51378` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug51920` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug52199` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug53046` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug53290` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug53592` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug53674` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug54044` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug56143` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug56716` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug56947` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug57252` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug57255` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug57904` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug59307` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug59410` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug59641` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug59733` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug60196` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug60229` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug70867` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bug84958` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bulk_create_index` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bulk_create_index_debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bulk_create_index_flush` — buffer pool / page I-O: absent
- `innodb/innodb_bulk_create_index_replication` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bulk_create_index_small` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_bulk_inject_errors` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_collation_test` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_convert_utf8mb4_compatible_varchars_compressed_debug` — page compression: absent
- `innodb/innodb_convert_utf8mb4_compatible_varchars_debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_convert_utf8mb4_inplace_debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_corrupt_bit` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_corrupt_readonly` — server read-only mode: absent
- `innodb/innodb_ctype_ldml` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_deadlock_with_autoinc` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/innodb_defaults` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_dirty_pages_at_shutdown` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/innodb_doublewrite_dir_01` — tablespaces / files / page format: absent
- `innodb/innodb_doublewrite_dir_02` — tablespaces / files / page format: absent
- `innodb/innodb_extend_and_initialize_debug` — tablespaces / files / page format: absent
- `innodb/innodb_extend_and_initialize_windows` — tablespaces / files / page format: absent
- `innodb/innodb_file_limit_check` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_force_recovery` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/innodb_fts_limit_check` — full-text index: absent
- `innodb/innodb_i_s_cached_indexes` — buffer pool / page I-O: absent
- `innodb/innodb_i_s_cached_indexes_accounting` — buffer pool / page I-O: absent
- `innodb/innodb_i_s_cached_indexes_compressed` — buffer pool / page I-O: absent
- `innodb/innodb_i_s_cached_indexes_rtree` — buffer pool / page I-O: absent
- `innodb/innodb_i_s_columns` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_i_s_multi_file_tablespace` — tablespaces / files / page format: absent
- `innodb/innodb_idle_flush_pct` — buffer pool / page I-O: absent
- `innodb/innodb_information_schema_buffer` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_init_rseg_parallel` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/innodb_io_pf` — buffer pool / page I-O: absent
- `innodb/innodb_max_recordsize` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_misc1` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_multi_update` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_multiple_temporary_file_path` — tablespaces / files / page format: absent
- `innodb/innodb_mysql` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_open_file_hang` — buffer pool / page I-O: absent
- `innodb/innodb_open_files_priv` — buffer pool / page I-O: absent
- `innodb/innodb_page_size_func` — tablespaces / files / page format: absent
- `innodb/innodb_pagesize_max_recordsize` — tablespaces / files / page format: absent
- `innodb/innodb_prefix_index_check` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_prefix_index_restart_server` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/innodb_read_only` — server read-only mode: absent
- `innodb/innodb_read_only-1` — server read-only mode: absent
- `innodb/innodb_read_only-2` — server read-only mode: absent
- `innodb/innodb_rename_index` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb_rename_index_err` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb_row_log_read` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/innodb_set_open_files_limit` — buffer pool / page I-O: absent
- `innodb/innodb_set_open_files_limit_concurrent` — buffer pool / page I-O: absent
- `innodb/innodb_stats` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_auto_recalc` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_auto_recalc_ddl` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_auto_recalc_lots` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_auto_recalc_on_nonexistent` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_create_table` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_del_mark` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_drop_locked` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_external_pages` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_fetch` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_fetch_nonexistent` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_flag_global_off` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/innodb_stats_flag_global_on` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/innodb_stats_long_names` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_race_condition` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_rename_table` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_rename_table_if_exists` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_sample_pages` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_table_flag_auto_recalc` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_stats_table_flag_sample_pages` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_status_utf8_truncate` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_sys_var_valgrind` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_tablespace` — tablespaces / files / page format: absent
- `innodb/innodb_tablespace_file_missing` — tablespaces / files / page format: absent
- `innodb/innodb_temporary_file_path` — tablespaces / files / page format: absent
- `innodb/innodb_thread_concurrency_debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_timeout_rollback` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_too_many_columns` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb_upd_stats_if_needed_not_inited` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb_update_rollback_gen_col` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_ut_format_name` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb_validate_page` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-2byte-collation` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-alter` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb-alter-autoinc` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb-alter-nullable` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb-alter-varchar` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb-alter-varchar-debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb-analyze` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb-autoinc` — auto-increment: absent
- `innodb/innodb-autoinc-18274` — auto-increment: absent
- `innodb/innodb-autoinc-44030` — auto-increment: absent
- `innodb/innodb-autoinc-56228` — auto-increment: absent
- `innodb/innodb-autoinc-optimize` — auto-increment: absent
- `innodb/innodb-blob` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/innodb-bug-14068765` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-bug-14084530` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-bug12552164` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-bug14219515` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-change-buffer-recovery` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/innodb-copy-alter-debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb-import-lctn_1` — tablespaces / files / page format: absent
- `innodb/innodb-import-partition` — tablespaces / files / page format: absent
- `innodb/innodb-import-partition-rpl` — tablespaces / files / page format: absent
- `innodb/innodb-index_ucs2` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-index-debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-index-online-fk` — SQL foreign keys: absent (edges are native RI)
- `innodb/innodb-large-prefix` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-new-fk` — SQL foreign keys: absent (edges are native RI)
- `innodb/innodb-replace-debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-status-output` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/innodb-truncate` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/innodb-ucs2` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/innodb-update-insert` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/insert_debug` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/insert_redo_size_instant` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_add_column_autoinc` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_add_column_basic` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_add_column_clear` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_add_column_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_add_column_long` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_add_column_recovery` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_basic` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_basic_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_import` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_charset_drop_enum_compact` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_charset_drop_enum_dynamic` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_charset_drop_enum_redundant` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_debug` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_max_row_version` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_old` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_old_debug` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_old_part` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_old_part_debug` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_partition` — tablespaces / files / page format: absent
- `innodb/instant_ddl_import_partition_debug` — tablespaces / files / page format: absent
- `innodb/instant_ddl_index_log_version` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_limitations` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_max_row_size` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_misc` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_recovery_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_recovery_old` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_rollback` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_rollback_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_update` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_update_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_metadata` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_metadata_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_part` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_part_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_part_metadata` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_part_metadata_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_rollback` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_ddl_upgrade_update` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_max_column_crash` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/instant_rename_column` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/json_small_partial_update_00` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/json_small_partial_update_01` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/json_small_partial_update_02` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/json_small_partial_update_03` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/json_small_partial_update_04` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/json_small_partial_update_05` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/json-read-uncommitted` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/known_dir_unique_undo` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/known_directory` — tablespaces / files / page format: absent
- `innodb/large_partitions_ddl` — partitioning: absent
- `innodb/lob_big_purge` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_compact` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_double_purge` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_ibd_size` — tablespaces / files / page format: absent
- `innodb/lob_ibd_size_replace` — tablespaces / files / page format: absent
- `innodb/lob_import_export` — tablespaces / files / page format: absent
- `innodb/lob_insert` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_insert_noindex` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_long_partial_update` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_mvcc` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_no_space` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_partial_update` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_print` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_recovery` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_rollback_crash` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_rollback_crash_2` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_rollback_crash_3` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_rollback_crash_4` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_rollback_crash_5` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_rollback_deadlock` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_rollback_problem_mini` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob_update` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob-being-modified-bit` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob-format` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob-read-uncommitted` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lob-single-zstream` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/lock_collision_report` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_contention` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_contention_big` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_empty_bitmap` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_end_of_range` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_granted_before_waiting` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_impl_to_expl` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_partitions` — partitioning: absent
- `innodb/lock_rec_unlock` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_s_to_implicit_x_escalation` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_sys_resize` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/lock_trx_release_read_locks_in_x_mode` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/log_alter_table` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/log_buffer_size` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_corruption` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_corruption_1` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_corruption_2` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_crash` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_ddl_must_flush` — buffer pool / page I-O: absent
- `innodb/log_directory` — tablespaces / files / page format: absent
- `innodb/log_encrypt_1` — encryption: absent
- `innodb/log_encrypt_2` — encryption: absent
- `innodb/log_encrypt_3` — encryption: absent
- `innodb/log_encrypt_4` — encryption: absent
- `innodb/log_encrypt_7` — encryption: absent
- `innodb/log_encrypt_8` — encryption: absent
- `innodb/log_encrypt_kill` — encryption: absent
- `innodb/log_file_checkpoint` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_from_future` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_invalid_checkpoint` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_invalid_lsn_ranges` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_invalid_start_lsn` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_marked_as_full` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_missing` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_name_1` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_name_discover` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_resize_1` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_resize_2` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_size` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_size_1` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_size_checkpoint` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_system` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_file_truncate_1` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/log_file_truncate_2` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/log_flush_order` — buffer pool / page I-O: absent
- `innodb/log_invalid_format` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_logical_size` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_long_running_rollback` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_memory_pfs` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_mtr_boundary` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_process_redo_rename` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/log_read_only` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_spin_vars` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_sys_vars` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_writer_threads` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/log_writer_threads_default` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/merge-subtrees` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/metadata_table_reference_count` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/minrecflag` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/missing_redologs` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/missing_tablespaces` — tablespaces / files / page format: absent
- `innodb/mixed_case_0` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/mixed_case_1` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/mixed_case_2` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/monitor` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/monitor_buf_page_io` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/monitor_restart` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/mtr_memo_size` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/multi_value_basic` — virtual / multi-valued column indexes: absent
- `innodb/mvcc_cache` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/mysql_tables_deadlocks` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/mysql_ts_alter_encrypt_1` — encryption: absent
- `innodb/mysql_ts_alter_encrypt_2` — encryption: absent
- `innodb/mysqld_core_dump_without_buffer_pool` — buffer pool / page I-O: absent
- `innodb/mysqld_core_dump_without_buffer_pool_dynamic` — buffer pool / page I-O: absent
- `innodb/mysqld_core_dump_without_buffer_pool_with_resizing` — buffer pool / page I-O: absent
- `innodb/mysqldump_max_recordsize` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/nonmonotone_trx_ids` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/open_file_lru` — buffer pool / page I-O: absent
- `innodb/optimizer_temporary_table` — temporary tables: absent
- `innodb/page_reorganize` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/parallel_read_1` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/partition` — partitioning: absent
- `innodb/partition_autoinc` — partitioning: absent
- `innodb/partition_debug` — partitioning: absent
- `innodb/partition_import` — tablespaces / files / page format: absent
- `innodb/partition-16k` — tablespaces / files / page format: absent
- `innodb/partition-blob` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/pct_cached_evict` — buffer pool / page I-O: absent
- `innodb/portability_basic` — tablespaces / files / page format: absent
- `innodb/portability_tablespace` — tablespaces / files / page format: absent
- `innodb/portability_tablespace_linux` — tablespaces / files / page format: absent
- `innodb/portability_tablespace_windows` — tablespaces / files / page format: absent
- `innodb/readahead` — buffer pool / page I-O: absent
- `innodb/readonly` — server read-only mode: absent
- `innodb/rec_offsets` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/records_in_range` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/recover_xa_insert_only_trx` — XA / two-phase commit: absent
- `innodb/recovery_ibuf_redo_overflow` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/recovery_small_bp_read_deadlock` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/redo_log_archive_01` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/redo_log_archive_02` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/redo_log_archive_03` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/redo_log_archive_04` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/redo_log_archive_05` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/redo_log_disable` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/redo_log_encryption_disabled` — encryption: absent
- `innodb/redo_log_encryption_read_only` — encryption: absent
- `innodb/redo_log_encryption_recovery_off` — encryption: absent
- `innodb/redo_log_encryption_recovery_on` — encryption: absent
- `innodb/redo_log_pfs` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/rename_fk_1` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/rename_fk_2` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/rename_table` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/rename_table_generail_failure` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/ro_trx_updates_temp_table` — temporary tables: absent
- `innodb/row_format` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/row_format_redundant` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/row_size` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/sdi` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/sdi_compressed` — page compression: absent
- `innodb/sdi_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/sdi_delete_marked` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/sdi_fail` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/secondary_unique_index_range_locking` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/session_temp_tablespaces` — tablespaces / files / page format: absent
- `innodb/set_concurrency_in_readonly` — server read-only mode: absent
- `innodb/show_engine_status` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/shutdown_threads` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/skip_locked_nowait` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/skip_locked_nowait_isolation` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb/skip_tablespace_path_validation` — tablespaces / files / page format: absent
- `innodb/slow_shutdown` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/sp_temp_table` — temporary tables: absent
- `innodb/stats_post_alter_copy` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/stored_fk` — SQL foreign keys: absent (edges are native RI)
- `innodb/strict_checksum` — tablespaces / files / page format: absent
- `innodb/strict_mode` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/subpartition` — partitioning: absent
- `innodb/table_compress` — page compression: absent
- `innodb/table_encrypt_1` — encryption: absent
- `innodb/table_encrypt_2` — encryption: absent
- `innodb/table_encrypt_3` — encryption: absent
- `innodb/table_encrypt_4` — encryption: absent
- `innodb/table_encrypt_6` — encryption: absent
- `innodb/table_encrypt_debug` — encryption: absent
- `innodb/table_encrypt_fts` — encryption: absent
- `innodb/table_encrypt_kill` — encryption: absent
- `innodb/table_encrypt_portable_64` — encryption: absent
- `innodb/table_options` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/tablespace_encrypt_1` — encryption: absent
- `innodb/tablespace_encrypt_10` — encryption: absent
- `innodb/tablespace_encrypt_11` — encryption: absent
- `innodb/tablespace_encrypt_2` — encryption: absent
- `innodb/tablespace_encrypt_3` — encryption: absent
- `innodb/tablespace_encrypt_4` — encryption: absent
- `innodb/tablespace_encrypt_5` — encryption: absent
- `innodb/tablespace_encrypt_7` — encryption: absent
- `innodb/tablespace_encrypt_8` — encryption: absent
- `innodb/tablespace_encrypt_9` — encryption: absent
- `innodb/tablespace_first_page_unrecoverable` — tablespaces / files / page format: absent
- `innodb/tablespace_per_table` — tablespaces / files / page format: absent
- `innodb/tablespace_per_table_not_windows` — tablespaces / files / page format: absent
- `innodb/tablespace_per_table_windows` — tablespaces / files / page format: absent
- `innodb/tablespace_portability` — tablespaces / files / page format: absent
- `innodb/tablespace_portability_windows` — tablespaces / files / page format: absent
- `innodb/tablespace_recovery_state` — tablespaces / files / page format: absent
- `innodb/tablespace_remove` — tablespaces / files / page format: absent
- `innodb/tablespace_rename` — tablespaces / files / page format: absent
- `innodb/tablespace_temp_table_1` — tablespaces / files / page format: absent
- `innodb/tablespace_temp_table_debug` — tablespaces / files / page format: absent
- `innodb/tablespace_truncate_stress` — tablespaces / files / page format: absent
- `innodb/tablespace_truncate_stress_debug` — tablespaces / files / page format: absent
- `innodb/temp_table` — temporary tables: absent
- `innodb/temp_table_savepoint` — temporary tables: absent
- `innodb/temporary_table` — temporary tables: absent
- `innodb/temporary_table_optimization` — temporary tables: absent
- `innodb/temptable_write_row` — temporary tables: absent
- `innodb/timestamp` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/tinytext-groupby` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/tmpdir` — temporary tables: absent
- `innodb/too_many_concurrent_trxs` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/transportable_tbsp` — tablespaces / files / page format: absent
- `innodb/transportable_tbsp-1` — tablespaces / files / page format: absent
- `innodb/transportable_tbsp-debug` — tablespaces / files / page format: absent
- `innodb/trigger_function_lock_compare` — row/gap/table lock manager and wait queues: absent (DML is refused, never queued)
- `innodb_undo/trunc_multi_client_01` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb_undo/trunc_multi_client_02` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/truncate` and `innodb_undo/truncate` (same name in both suites) — MySQL TRUNCATE TABLE / undo-tablespace truncation mechanics: absent
- `innodb/truncate_debug` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_explicit` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_01` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_02` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_03` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_04` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_05` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_06` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_07` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_08` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_09` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_10` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_11` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e01` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e02` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e03` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e04` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e05` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e06` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e07` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e08` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e09` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e10` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_recover_e11` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/truncate_special_schema_rollback` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/truncate_xa` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb/trx_cleanup_at_startup` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/trx_id_future` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/undo` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb_undo/undo_ddl_recover` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/undo_ddl_vs_dml` — MySQL DDL / data dictionary mechanics: absent (GoGraph DDL is CREATE/DROP INDEX|CONSTRAINT, kept in DD01-DD09)
- `innodb_undo/undo_directory` — tablespaces / files / page format: absent
- `innodb/undo_log_temp_table_debug` — temporary tables: absent
- `innodb_undo/undo_settings` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb_undo/undo_tablespace` — tablespaces / files / page format: absent
- `innodb_undo/undo_tablespace_125` — tablespaces / files / page format: absent
- `innodb_undo/undo_tablespace_debug` — tablespaces / files / page format: absent
- `innodb_undo/undo_tablespace_win` — tablespaces / files / page format: absent
- `innodb/undo_with_virtual_no_idx` — virtual / multi-valued column indexes: absent
- `innodb/undo-order` — redo/undo/rollback-segment internals: no GoGraph analogue beyond the kept D-series
- `innodb/unknown_directory` — tablespaces / files / page format: absent
- `innodb/update_time` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/update_time_is` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/update-cascade-innodb-fk` — SQL foreign keys: absent (edges are native RI)
- `innodb/update-ext` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/use_latest_stats` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/virtual_basic` — virtual / multi-valued column indexes: absent
- `innodb/virtual_blob` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/virtual_debug` — virtual / multi-valued column indexes: absent
- `innodb/virtual_fk` — SQL foreign keys: absent (edges are native RI)
- `innodb/virtual_fk_restart` — SQL foreign keys: absent (edges are native RI)
- `innodb/virtual_index` — virtual / multi-valued column indexes: absent
- `innodb/virtual_no_storage` — virtual / multi-valued column indexes: absent
- `innodb/virtual_purge` — virtual / multi-valued column indexes: absent
- `innodb/virtual_stats` — optimizer statistics / INFORMATION_SCHEMA: absent
- `innodb/virtual_uncommitted` — virtual / multi-valued column indexes: absent
- `innodb/xa_another` — XA / two-phase commit: absent
- `innodb/xa_prepare_lock_release` — XA / two-phase commit: absent
- `innodb/xa_recovery` — XA / two-phase commit: absent
- `innodb/xa_recovery_debug` — XA / two-phase commit: absent
- `innodb/xdes_fseg_frag` — storage-internal or single-session regression; no concurrent-MVCC semantic
- `innodb/zlob` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_big_purge` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_ddl` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_ddl_big` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_ddl_big_slow_io` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_fragments` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_geom` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_ibd_size_replace` — tablespaces / files / page format: absent
- `innodb/zlob_import_10mb` — tablespaces / files / page format: absent
- `innodb/zlob_import_export` — tablespaces / files / page format: absent
- `innodb/zlob_long_partial_update` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_no_space` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_print` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_purge` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_purge_undo_no` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_redundant_partial_update` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_crash` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_crash_2` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_crash_3` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_crash_4` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_crash_5` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_crash_6` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_deadlock` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_rollback_problem_mini` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)
- `innodb/zlob_update_purge` — LOB / JSON storage format: no equivalent (large values are ordinary property values; MVCC of large values kept in WW15)

### E.4 InnoDB full-text exclusions (`suite/innodb_fts`, 60 of 63)

Kept: `transaction` (IX01 source), `fts_sync_commit_resiliency` and `search_after_crash_no_binlog` (D10 source). Every other test exercises the full-text index, which GoGraph does not have.

- `innodb_fts/alter` — full-text index: absent
- `innodb_fts/articles_fts_words` — full-text index: absent
- `innodb_fts/basic` — full-text index: absent
- `innodb_fts/bug_32831765` — full-text index: absent
- `innodb_fts/bug_34846823` — full-text index: absent
- `innodb_fts/ddl` — full-text index: absent
- `innodb_fts/fic` — full-text index: absent
- `innodb_fts/foreign_key_check` — full-text index: absent
- `innodb_fts/foreign_key_update` — full-text index: absent
- `innodb_fts/fulltext` — full-text index: absent
- `innodb_fts/fulltext2` — full-text index: absent
- `innodb_fts/fulltext3` — full-text index: absent
- `innodb_fts/fulltext_cache` — full-text index: absent
- `innodb_fts/fulltext_distinct` — full-text index: absent
- `innodb_fts/fulltext_left_join` — full-text index: absent
- `innodb_fts/fulltext_misc` — full-text index: absent
- `innodb_fts/fulltext_multi` — full-text index: absent
- `innodb_fts/fulltext_order_by` — full-text index: absent
- `innodb_fts/fulltext_rollup` — full-text index: absent
- `innodb_fts/fulltext_table_evict` — full-text index: absent
- `innodb_fts/fulltext_update` — full-text index: absent
- `innodb_fts/fulltext_var` — full-text index: absent
- `innodb_fts/hypergraph_optimizer` — full-text index: absent
- `innodb_fts/i_s_index_cache_convert_to_disc` — full-text index: absent
- `innodb_fts/index_table` — full-text index: absent
- `innodb_fts/innobase_drop_fts_index_table` — full-text index: absent
- `innodb_fts/large_records` — full-text index: absent
- `innodb_fts/limit_union` — full-text index: absent
- `innodb_fts/mecab_sjis` — full-text index: absent
- `innodb_fts/mecab_ujis` — full-text index: absent
- `innodb_fts/mecab_utf8mb3` — full-text index: absent
- `innodb_fts/mecab_utf8mb4` — full-text index: absent
- `innodb_fts/misc` — full-text index: absent
- `innodb_fts/misc_1` — full-text index: absent
- `innodb_fts/misc_debug` — full-text index: absent
- `innodb_fts/multiple_index` — full-text index: absent
- `innodb_fts/ngram` — full-text index: absent
- `innodb_fts/ngram_1` — full-text index: absent
- `innodb_fts/ngram_2` — full-text index: absent
- `innodb_fts/ngram_debug` — full-text index: absent
- `innodb_fts/opt` — full-text index: absent
- `innodb_fts/optimize_big` — full-text index: absent
- `innodb_fts/phrase` — full-text index: absent
- `innodb_fts/phrase_clear_no_match` — full-text index: absent
- `innodb_fts/phrase_search_perf` — full-text index: absent
- `innodb_fts/plugin` — full-text index: absent
- `innodb_fts/plugin_1` — full-text index: absent
- `innodb_fts/plugin_debug` — full-text index: absent
- `innodb_fts/proximity` — full-text index: absent
- `innodb_fts/result_cache_limit` — full-text index: absent
- `innodb_fts/savepoint` — full-text index: absent
- `innodb_fts/stopword` — full-text index: absent
- `innodb_fts/subexpr` — full-text index: absent
- `innodb_fts/sync` — full-text index: absent
- `innodb_fts/sync_block` — full-text index: absent
- `innodb_fts/tablespace` — full-text index: absent
- `innodb_fts/tablespace_location` — full-text index: absent
- `innodb_fts/tablespace_location_error` — full-text index: absent
- `innodb_fts/truncate` — full-text index: absent
- `innodb_fts/zip` — full-text index: absent

### E.5 MySQL main suite (22) and binlog group commit (4)

| Test | K/X | IDs or reason |
|---|---|---|
| `t/consistent_snapshot` | K | SK12, SK13, L03 |
| `t/concurrent_innodb_safelog`, `t/concurrent_innodb_unsafelog` (+ `include/concurrent.inc`) | K | WW01, WW04, WW08, L01 |
| `t/deadlock_innodb`, `t/innodb_mysql_lock` | K | WW10 |
| `t/innodb_deadlock` | K | WW10, AB03 |
| `t/trans_read_only` | K | RO01, DD07 |
| `t/implicit_commit` | K | DD07 |
| `t/func_rollback` | K | AB01, AB02 |
| `t/kill` | K | AB02 |
| `t/flush_block_commit` | K | L20, D16 |
| `t/partition_innodb_semi_consistent` | K | WW06, WW07 (partition half excluded) |
| `t/inconsistent_scan` | X | single-session optimizer regression |
| `t/commit` | X | `completion_type` and COMMIT AND CHAIN |
| `t/commit_1innodb` | X | commit with non-transactional tables and binlog |
| `t/savepoint_debug` | X | savepoints |
| `t/rollback` | X | metadata-lock deadlock |
| `t/innodb_mysql_sync` | X | OPTIMIZE/ALTER debug-sync races |
| `t/innodb_mysql_lock2` | X | lock strength under statement-based binlog |
| `t/read_only_innodb` | X | server-wide `read_only` mode |
| `t/locking_clause` | X | SELECT … FOR UPDATE/SHARE syntax; recorded as gap G4 |
| `t/lock_tables_lost_commit` | X | LOCK TABLES |
| `binlog/binlog_group_commit_flush_crash` | K | SE03, D03, D04 |
| `binlog/binlog_group_commit_sync_delay` | K | L10 |
| `binlog/binlog_after_commit_order_info_schema` | K | SE02 |
| `binlog_gtid/binlog_group_commit_gtid_order` | K | SE02, L10, D15 |

XA tests of the main and binlog suites (about 60 files) were not taken into scope: XA /
two-phase commit is absent in GoGraph.

### E.6 Turso dispositions (914 tests: 278 kept, 636 excluded)

Paths are relative to the Turso repository at `2b9184f8`: `db/` = `core/mvcc/database/`, `it/` = `tests/integration/`, `sim/` = `testing/simulator/runner/memory/`, `whopper/` = `testing/concurrent-simulator/`, `logical_log.rs` and `discard_pending_tests.rs` sit in `core/mvcc/persistent_storage/`. The number in parentheses is the line of the test function.

#### E.6a Kept as sources

| File | Test (line) → catalogue IDs |
|---|---|
| `core/mvcc/mod.rs` (2) | `test_non_overlapping_concurrent_inserts` (61) → WW09; `test_overlapping_concurrent_inserts_read_your_writes` (221) → SE01, L12 |
| `db/hermitage_tests.rs` (17) | `test_hermitage_g0_write_cycles_prevented` (104) → WW10; `test_hermitage_g1a_aborted_reads` (150), `test_hermitage_g1b_intermediate_reads` (200) → SK15; `test_hermitage_g1c_circular_information_flow` (246) → SK16; `test_hermitage_otv_observed_transaction_vanishes` (295) → SK17; `test_hermitage_pmp_predicate_many_preceders_read` (376), `test_hermitage_g_single_read_skew_predicate_dependencies` (552) → SK12; `test_hermitage_pmp_predicate_many_preceders_write` (421), `test_hermitage_g_single_read_skew_write_predicate` (604) → WW06; `test_hermitage_p4_lost_update` (462), `test_hermitage_write_write_conflict` (891) → WW01; `test_hermitage_g_single_read_skew` (503) → SK09, WW14; `test_hermitage_g_single_read_skew_write_interleaved` (658) → WW04; `test_hermitage_g2_item_write_skew` (706) → SK01; `test_hermitage_g2_two_edges_fekete` (752) → SK06; `test_hermitage_g2_anti_dependency_cycles` (819) → SK03; `test_hermitage_aborted_transaction_not_visible` (862) → HZ03, SK15 |
| `db/tests.rs` (149) | `aborted_transaction_rejects_write_set_insert` (292) → AB03; `gc_keeps_sole_current_version_for_reader_that_began_after_lwm_sample` (792), `test_gc_keeps_columns_of_positioned_reader` (12693), `test_gc_retire_snapshot_stable_with_overlapping_writers` (12780) → L21; `passive_reader_snapshot_survives_later_write_after_row_versions_gc` (1219), `test_gc_rule2_superseded_below_lwm_with_current` (10732), `test_gc_rule2_superseded_above_lwm_retained` (10755), `test_gc_rule3_visible_to_active_tx_retained` (10888), `test_gc_txid_refs_retained` (10996), `test_gc_txid_end_retained` (11015), `test_gc_active_reader_pins_lwm` (11527), `test_live_version_count_approx_tracks_inserts_and_gc` (11603), `test_gc_incremental_reclaims_like_full_sweep` (11720), `test_gc_incremental_respects_held_snapshot` (12104), `test_gc_current_serves_older_reader_then_reclaims` (12176), `test_gc_incremental_keeps_held_reader_row` (12736), `test_gc_e2e_updated_row_correct_after_gc` (14103) → HZ01; `mvcc_passive_checkpoint_busy_under_pinned_reader_no_corruption` (1267), `mvcc_passive_auto_checkpoint_retries_publish_while_reader_pinned` (1310), `test_passive_concurrent_transfer_preserves_sum_and_count` (5403), `test_passive_checkpoint_tolerates_concurrent_create_after_snapshot` (5657), `test_snapshot_stability_full` (18953) → L22; `test_recovery_clock_monotonicity` (2356), `test_empty_log_recovery_loads_checkpoint_watermark` (3483) → D07; `test_recover_logical_log_short_file_ignored` (2389) → D08; `test_recovery_checkpoint_then_more_writes` (2544), `test_btree_resident_recovery_then_checkpoint_delete_stays_deleted` (2711), `test_meta_recovery_case_12_replay_gate_skips_at_or_below_metadata_boundary` (6065), `test_insert_with_checkpoint` (9308), `test_gc_e2e_multiple_checkpoint_gc_cycles` (14137), `test_recovery_many_tables_checkpoint_restart_checkpoint_restart` (17974) → D16; `test_concurrent_update_then_delete_serializes_correctly_across_restart` (2606) → WW04, D05; `test_recovery_overwrites_torn_tail_on_next_append` (2751), `test_bootstrap_repairs_torn_short_log_before_metadata_init` (2800) → D17; `test_bootstrap_completes_interrupted_checkpoint_with_committed_wal` (2855), `test_checkpoint_truncates_wal_last` (2903), `test_bootstrap_recovers_committed_wal_without_log_file` (3194), `test_bootstrap_rejects_torn_log_header_with_committed_wal` (3338), `test_bootstrap_rejects_corrupt_log_header_without_wal` (3377), `test_bootstrap_handles_committed_wal_when_log_truncated` (3413), `test_meta_recovery_case_1_no_wal_no_log_metadata_present_clean_boot` (3545), `test_meta_recovery_case_2_no_wal_replay_above_metadata_boundary` (3586), `test_meta_recovery_case_3_no_wal_log_frames_without_valid_metadata_fails_closed` (3712), `test_meta_recovery_case_4_committed_wal_reconcile_before_metadata_boundary_replay` (3755), `test_meta_recovery_case_5_committed_wal_missing_metadata_fails_closed` (3796), `test_meta_recovery_case_6_committed_wal_corrupt_metadata_fails_closed` (3834), `test_meta_recovery_case_7_metadata_table_shape_violation_fails_closed` (3871), `test_meta_recovery_case_9_metadata_row_deleted_fails_closed` (3907), `test_meta_checkpoint_case_10_metadata_upsert_is_atomic_with_pager_commit` (3943), `test_meta_checkpoint_case_11_auto_checkpoint_failure_after_commit_remains_recoverable` (4734), `test_checkpoint_post_durable_failure_then_unique_update_removes_stale_autoindex_entry` (5967), `test_checkpoint_post_durable_failure_then_delete_removes_stale_table_row` (6018), `test_auto_checkpoint_update_stays_with_source_table_after_restart` (21887), `test_delete_after_failed_checkpoint_stays_with_source_table` (21931), `test_failed_checkpoint_does_not_move_other_connection_insert` (21973) → D09; `test_blocking_truncate_zeros_log_when_commit_races_acquire_lock` (2974), `test_passive_truncate_keeps_log_frames_committed_after_snapshot` (3089), `test_checkpoint_snapshot_ts_clamps_below_inflight_preparing` (3302), `test_checkpoint_resamples_boundary_before_starting` (4809), `test_checkpoint_stale_unique_index_delete_with_out_of_order_commit_yield` (5842) → D18; `test_bootstrap_ignores_wal_frames_without_commit_marker` (3452) → D06; `test_reader_consistent_during_large_indexed_commit_rewrite` (4940), `test_reader_does_not_see_inflight_index_tombstone` (5550) → IX11; `test_checkpoint_two_scan_toctou_orphans_first_checkpoint_unique_index` (5009) → D10; `test_rollback_of_indexed_update_keeps_btree_resident_index_entry` (5582), `test_conflict_abort_of_indexed_update_keeps_btree_resident_index_entry` (5618), `test_rollback_with_index` (10446) → IX05; `test_dirty_write` (6388), `test_lost_update` (6581) → WW01; `test_dirty_read` (6434), `test_dirty_read_deleted` (6464), `test_speculative_delete_hides_committed_version_sql` (16250), `test_speculative_delete_hides_committed_version` (16408) → WW14; `test_fuzzy_read` (6510), `test_mvcc_snapshot_isolation` (13232) → SK12; `test_committed_visibility` (6648), `test_future_row` (6697), `test_snapshot_isolation_tx_visible1` (7543) → SK13; `test_last_committed_timestamp_is_monotonic_for_out_of_order_commits` (8618) → SE02, D18; `test_exclusive_tx_does_not_deadlock_behind_preparing_concurrent_commit` (8779), `test_begin_tx_schema_generation_gate` (18834), `test_create_index_exclusive_acquire_rechecks_timestamp_after_cas` (21573) → DD06; `test_restart` (8904) → D01; `test_connection_sees_other_connection_changes` (8944) → SE01; `test_insert_in_middle_commit_of_create_index_returns_err` (9060), `test_schema_change_succeeds_while_concurrent_writer_aborts_at_commit` (19265), `test_create_index_succeeds_while_concurrent_writer_aborts_at_commit` (19304) → DD01; `test_concurrent_writes` (9134) → L12; `test_mvcc_read_tx_lifecycle` (9364), `test_mvcc_conn_drop_releases_read_tx` (9383), `test_read_lock_leak_deferred_then_concurrent` (19244) → HZ02; `test_mvcc_integrity_check` (9823) → G7, D10; `test_mvcc_cached_insert_reprepared_after_index_create` (10224) → DD03; `test_integrity_check_after_drop_index_before_checkpoint` (10349) → DD03, D10; `test_stale_update_of_deleted_row_is_refused_before_it_can_corrupt_indexes` (10468), `test_committed_delete_tombstone_conflict` (16462), `test_exclusive_update_conflicts_with_concurrent_delete_without_replacing_marker` (19340), `test_explicit_delete_conflicts_with_concurrent_delete_without_replacing_marker` (19378), `test_delete_of_btree_row_conflicts_with_committed_concurrent_delete` (19415), `test_delete_of_btree_row_conflicts_with_active_concurrent_delete` (19459), `commit_validation_reports_conflict_for_evicted_tombstone_writer` (22659) → WW04; `test_update_multiple_unique_columns_partial_rollback` (10560), `test_update_three_unique_columns_partial_rollback` (13274) → MG10; `test_gc_rule1_aborted_garbage_removed` (10688), `test_gc_rule1_aborted_among_live_versions` (10705), `transaction_rollback_removes_created_versions_immediately` (11314), `test_gc_e2e_deleted_row_stays_hidden_after_gc` (14067) → HZ03; `test_should_gc_threshold_and_reset` (11641), `test_mvcc_gc_threshold_pragma_roundtrip` (11691) → G2; `test_gc_incremental_concurrent_is_safe` (11795) → L08; `test_sequential_updates_with_constraint_errors` (13343) → AB01; `test_desc_index_scan_respects_mvcc_snapshot_for_concurrent_insert` (13669) → IX07; `test_mvcc_same_primary_key` (13985) → MG03; `test_mvcc_same_primary_key_concurrent` (14008) → MG04, G6; `test_mvcc_unique_constraint` (14174) → MG02, G6; `test_abandoned_commit_rolls_back_insert_with_injected_yield` (14262), `test_abandoned_commit_rolls_back_insert` (14581), `test_abandoned_commit_rolls_back_delete` (14605), `dropped_concurrent_commit_does_not_strand_connection` (19605), `dropped_exclusive_commit_releases_locks` (19666), `test_dropped_commit_corrupts_subsequent_insert` (20442), `abandoned_committed_writer_notifies_dependents` (20468), `abandoned_exclusive_commit_should_not_block_subsequent_concurrent_writer` (20604), `mvcc_bug_repro_dropped_committed_delete_rewrites_all_tombstone_txids` (21727) → D19; `test_commit_failure_after_remove_tx_does_not_strand_conn_cache` (14521), `exclusive_commit_failure_at_after_remove_tx_strands_exclusive_atom` (19559), `busy_from_log_tx_does_not_block_subsequent_commit_with_group_commit` (19956), `busy_from_log_tx_does_not_block_subsequent_commit_without_group_commit` (19961) → SE03; `test_delete_then_drop_index_with_index_dml_replays_on_reopen` (14899), `test_schema_frame_recovery_drop_index_with_remaining_index_matrix` (14996) → D10, D11; `test_close_persists_drop_index` (15514), `test_partial_commit_visibility_bug` (15549), `test_checkpoint_recovers_after_crash_restart_drop_recreate_index` (18355), `test_checkpoint_recovers_after_restart_drop_checkpointed_index` (18504) → D11; `test_concurrent_autoincrement_inserts` (15883), `test_three_concurrent_autoincrement_inserts` (15957), `test_autoincrement_no_reuse_after_delete_and_restart` (16151), `test_concurrent_explicit_rowid_high_watermark_not_clobbered` (20671), `test_concurrent_explicit_rowid_auto_rowid_does_not_walk_back_into_collision` (20700), `test_concurrent_explicit_rowid_preserves_auto_rowid_watermark` (20737) → GG07; `test_elle_lost_update_exclusive_concurrent` (16319) → L01, WW01; `test_committed_update_version_conflict` (16518) → WW02; `test_recovery_three_restarts_with_table_creation` (18059) → D13; `logical_log_offset_advances_only_after_on_log_write_complete` (20178) → D03; `abandoned_commit_in_committed_state_should_not_block_subsequent_checkpoint` (20641) → D19, D18; `test_global_header_cookie_no_regression_on_out_of_order_finalize` (21401), `test_global_header_regression_would_lose_committed_user_version` (21505) → SE02; `test_failed_checkpoint_preserves_secondary_index_consistency` (22017) → D09, D10; `truncate_checkpoint_is_busy_while_a_reader_transaction_is_open` (22614) → L20 |
| `db/group_commit_tests.rs` (16) | `two_writers_both_commit_with_group_commit_truncate` (115), `two_writers_both_commit_with_group_commit_passive` (120), `two_writers_both_commit_with_group_commit_off_truncate` (125), `two_writers_both_commit_with_group_commit_off_passive` (130) → D01, L12; `commits_batch_into_one_group` (135), `commit_parks_once_while_another_transaction_holds_the_commit_lock` (431) → L10; `batched_records_survive_a_restart` (206) → D01; `requeued_records_go_back_in_ticket_order` (258) → D15; `drop_pending_only_removes_queued_records` (282), `failed_leader_does_not_publish_unsynced_prefix` (306) → D04; `durability_watermark_only_moves_forward` (297) → D07; `failed_mid_batch_leader_does_not_cover_retry_hole` (332) → D15, D04; `dropped_commit_after_log_record_is_written_still_commits` (378), `dropped_commit_after_log_record_is_written_still_commits_without_group` (383), `dropped_waiter_after_log_tx_still_commits` (532) → D19; `parked_waiter_wakes_when_the_leader_makes_it_durable` (470) → L10, D03 |
| `logical_log.rs` (17) | `test_logical_log_streaming_recovery_forced_yields_bounded_memory` (4208) → D12; `test_logical_log_torn_tail_stops_cleanly` (4741), `test_logical_log_torn_tail_multiple_frames_stops_cleanly` (4822), `test_logical_log_corruption_detected` (5076), `test_logical_log_payload_len_varint_corrupt_tail_keeps_prefix` (5123), `test_logical_log_end_magic_corruption` (5171), `test_logical_log_payload_size_corruption` (5196), `test_logical_log_frame_magic_corruption` (5222), `test_logical_log_crc_field_corruption` (5244), `test_logical_log_corrupt_tail_keeps_valid_prefix` (5265), `test_logical_log_bitflip_integrity_exhaustive_single_frame` (5557) → D08; `test_logical_log_header_corruption_detected` (5312), `test_try_read_header_reports_invalid_not_corrupt` (5981), `test_truncate_retained_when_uncheckpointed_frames_remain` (6068) → D09; `test_truncation_regenerates_salt` (6012), `test_splice_frame_from_different_log_rejected` (6160) → G11; `test_crc_chain_invalidates_suffix_on_corruption` (6097) → D08, G11 |
| `it/mvcc.rs` (8) | `test_newrowid_mvcc_concurrent` (224) → GG07; `test_stmt_rollback_cleans_write_set` (324), `test_stmt_rollback_cleans_write_set_with_index` (360) → AB01; `test_mvcc_read_to_write_upgrade_does_not_block_checkpoint` (392) → L20; `test_mvcc_same_tx_row_and_index_lifecycle_matrix` (1197) → AB06, D05; `test_mvcc_index_scan_does_not_return_row_deleted_mid_scan` (1490) → IX06; `test_issue_7638_gc_after_abandoned_checkpoint_does_not_resurrect_row` (1609) → HZ03, D09; `mvcc_passive_checkpoint_must_not_leak_commits_into_pinned_snapshot` (1677) → L22 |
| `it/query_processing/test_transactions.rs` (22) | `test_txn_error_doesnt_rollback_txn` (175), `test_constraint_error_aborts_only_stmt_not_entire_transaction` (353) → AB01; `test_transaction_visibility` (199) → SK13; `test_delete_all_keeps_existing_reader_snapshots` (231) → GG05; `test_mvcc_concurrent_insert_basic` (521) → WW09; `test_mvcc_concurrent_conflicting_update` (596), `test_mvcc_concurrent_conflicting_update_2` (622) → WW01; `test_mvcc_checkpoint_works` (648), `test_mvcc_recovery_of_both_checkpointed_and_noncheckpointed_tables_works` (754), `test_mvcc_checkpoint_before_delete_then_reopen` (950), `test_mvcc_delete_then_checkpoint_then_reopen` (975), `test_mvcc_delete_then_reopen_no_checkpoint` (1000), `test_mvcc_delete_then_reopen_no_checkpoint_2` (1025), `test_mvcc_checkpoint_delete_checkpoint_then_reopen` (1049), `test_mvcc_index_before_checkpoint_delete_after_checkpoint` (1076), `test_mvcc_index_after_checkpoint_delete_after_index` (1102), `test_mvcc_multiple_deletes_with_checkpoints` (1128), `test_mvcc_no_index_checkpoint_delete_reopen` (1157), `test_mvcc_checkpoint_before_insert_delete_after_checkpoint` (1181) → D16; `test_mvcc_recovery_with_index_and_deletes` (909) → D10; `test_autoincrement_watermark_survives_restart` (2204), `test_concurrent_autoincrement_no_database_busy` (2335) → GG07 |
| `it/fuzz_transaction/mod.rs` (2) | `test_multiple_connections_fuzz_mvcc` (480) → L01, L03; `test_multiple_connections_fuzz_mvcc_passive_checkpoint` (510) → L22 |
| `tests/fuzz/mvcc_rowid_allocator.rs` (1) | `mvcc_rowid_allocator_fuzz` (518) → GG07 |
| `shuttle_mvcc_checkpoint.rs` (1) | `shuttle_test_passive_truncate_preserves_late_commit` (150) → D18 |
| `shuttle_mvcc.rs` (20) | `shuttle_test_concurrent_delete_btree_only_row` (46), `shuttle_test_concurrent_delete_btree_only_index_entry` (53) → WW04; `shuttle_test_lost_updates` (196), `shuttle_test_lost_updates_slow` (203) → L04, WW01; `shuttle_test_snapshot_isolation_violation` (287), `shuttle_test_snapshot_isolation_violation_slow` (294) → L01, L03; `shuttle_test_ghost_commits` (349), `shuttle_test_ghost_commits_slow` (356) → D19, L04; `shuttle_test_disjoint_writes_no_false_conflict` (412), `shuttle_test_disjoint_writes_no_false_conflict_slow` (419) → L12, WW09; `shuttle_test_otv` (502), `shuttle_test_otv_slow` (509) → SK09, L01; `shuttle_test_rollback_isolation` (572), `shuttle_test_rollback_isolation_slow` (579) → HZ03, L18; `shuttle_test_phantom_prevention` (654), `shuttle_test_phantom_prevention_slow` (661) → L03, SK12; `shuttle_test_speculative_abort_delete` (735), `shuttle_test_speculative_abort_delete_slow` (742) → WW14, L18; `shuttle_test_begin_publish_window_gc_hazard` (749) → L21; `shuttle_test_index_scan_after_concurrent_delete_rollback` (839) → IX05 |
| `sim/mvcc_recovery.rs` (14) | `sim_mvcc_faulted_explicit_checkpoint_returns_error_without_panic` (147), `sim_mvcc_restart_recovers_commit_after_checkpoint_fault` (178), `sim_mvcc_fail_closed_case_wal_without_log` (270), `sim_mvcc_fail_closed_case_wal_with_corrupt_log_header` (288), `sim_mvcc_fail_closed_case_no_wal_with_corrupt_log_header` (312), `sim_mvcc_checkpoint_retry_after_wal_fault_succeeds` (365), `sim_mvcc_checkpoint_retry_after_log_truncate_fault_succeeds` (395) → D09; `sim_mvcc_restart_drops_failed_log_append` (207) → D04; `sim_mvcc_restart_recovery_is_idempotent` (235) → D13; `sim_mvcc_recovery_keeps_valid_prefix_drops_corrupt_tail_frame` (335) → D08; `sim_mvcc_faulted_checkpoint_does_not_block_other_connection` (425), `sim_mvcc_faulted_auto_checkpoint_does_not_block_other_connection` (461) → L20; `sim_mvcc_snapshot_isolation_holds_under_latency_and_restart` (561) → L03, D01; `sim_mvcc_randomized_fault_campaign_no_panics_and_recoverable` (592) → D02, D13 |
| `it/wal/test_power_loss_before_first_checkpoint.rs` (1) | `test_committed_wal_survives_power_loss_before_first_checkpoint` (35) → D01 |
| `it/checkpoint_crash_atomicity.rs` (4) | `checkpoint_backfill_crash_under_synchronous_normal_recovers_committed_prefix` (151), `checkpoint_backfill_crash_under_synchronous_full_control` (160), `full_commit_right_after_truncate_checkpoint_survives_power_loss` (199) → D09; `first_full_commit_on_fresh_database_fsyncs_the_wal` (332) → D01 |
| `core/storage/wal.rs` (4) | `append_frames_vectored_frame_hidden_until_write_is_durable` (6598) → D03; `page_codec_encode_error_does_not_publish_wal_frames` (7047) → D04; `test_wal_concurrent_readers_during_checkpoint` (10426), `test_wal_full_waits_for_old_reader_then_succeeds` (10944) → L20 |

#### E.6b Excluded, by category

Every excluded test is named once under its category. None carries a concurrent-MVCC, isolation or durability semantic that GoGraph has and that E.6a does not already cover.

- **BT (88)** — SQLite B-tree residency, dual MVCC/B-tree cursors, root pages, checkpoint-collection and GC-rule internals tied to B-tree materialisation: GoGraph keeps MVCC versions over an in-memory LPG and persists by snapshot plus WAL, with no B-tree under the version store.
  - `core/mvcc/mod.rs` (1): `test_mvcc_dual_cursor_transaction_isolation`
  - `db/checkpoint_state_machine.rs` (5): `checkpoint_retry_does_not_replay_checkpointed_btree_resident_delete` `checkpoint_collection_uses_btree_marker_for_existence_but_writes_surviving_replacement` `checkpoint_collection_uses_btree_marker_for_later_delete_of_replacement` `checkpoint_collection_skips_delete_of_never_checkpointed_replacement_without_btree_marker` `collect_table_rows_skips_user_keys_after_schema_drop`
  - `db/tests.rs` (75): `rollback_only_reports_rowids_restored_by_a_delete` `mvcc_passive_gc_retains_until_reader_mark_reaches_materialization` `debug_gc_metrics_truncate_vs_passive` `mvcc_passive_checkpoint_publishes_backfill_and_reclaims_versions` `mvcc_passive_begin_concurrent_after_backfill_does_not_busy` `mvcc_passive_unrelated_root_publication_does_not_invalidate_open_txn` `mvcc_passive_drop_index_then_reuse_page_integrity` `mvcc_btree_read_dual_gate` `test_checkpoint_gc_anchor_loss_update_then_delete_strands_stale_row` `test_checkpoint_retry_does_not_replay_checkpointed_btree_resident_unique_delete` `test_lazy_scan_cursor_basic` `test_lazy_scan_cursor_with_gaps` `test_cursor_basic` `test_cursor_with_empty_table` `test_cursor_modification_during_scan` `transaction_display` `test_should_checkpoint` `test_should_checkpoint_after_recovery_uses_recovered_offset` `test_auto_checkpoint_busy_is_ignored` `test_select_empty_table` `test_cursor_with_btree_and_mvcc` `test_cursor_with_btree_and_mvcc_2` `test_cursor_with_btree_and_mvcc_with_backward_cursor` `test_cursor_with_btree_and_mvcc_with_backward_cursor_with_delete` `test_cursor_with_btree_and_mvcc_fuzz` `test_cursor_with_btree_and_mvcc_insert_after_checkpoint_repeated_key` `test_cursor_with_btree_and_mvcc_seek_after_checkpoint` `test_cursor_with_btree_and_mvcc_delete_after_checkpoint` `test_skips_updated_rowid` `test_checkpoint_index_writer_overwrites_existing_interior_key` `test_sql_checkpoint_reinsert_existing_interior_index_key_keeps_sqlite_integrity` `test_gc_rule2_tombstone_guard_uncheckpointed` `test_gc_rule2_tombstone_guard_checkpointed` `test_gc_rule3_drop_current_when_in_btree` `test_gc_rule3_truncate_idle_only` `test_gc_rule3_not_checkpointed_retained` `test_gc_rule3_current_retained_before_first_checkpoint` `test_gc_rule3_current_collected_after_checkpoint` `test_gc_rule3_after_history_reclaimed` `test_gc_rule3_keeps_unstamped_current_and_drops_stamped_one` `test_gc_rule2_pending_insert_does_not_disable_tombstone_guard` `test_gc_rule2_committed_current_disables_non_btree_tombstone_guard` `test_gc_rule2_btree_resident_marker_with_current_retained_until_checkpoint` `test_gc_rule2_checkpointed_insert_with_current_retained_until_checkpoint` `test_gc_rule2_btree_tombstone_lifecycle` `test_gc_rule3_not_firing_with_unremovable_superseded` `test_gc_noop_on_empty` `test_gc_combined_rules` `test_gc_integration_insert_commit_gc` `rollback_drops_row_maps_of_btrees_created_by_the_transaction` `test_gc_shrinks_version_chain_capacity` `test_gc_with_slot_removal_drops_empty_skipmap_entries` `test_gc_incremental_reclaims_index_chains_resumably` `test_gc_incremental_skips_while_checkpoint_holds_write_lock` `test_gc_incremental_lazy_leaves_empty_slots` `test_unique_lookup_survives_passive_checkpoint_mid_seek` `test_same_txn_unique_lookup_stable_across_passive_checkpoint` `test_same_txn_first_unique_lookup_after_aborted_seek_still_sees_reclaimed_key` `test_idxdelete_after_truncate_clears_checkpointed_index` `test_delete_via_unique_index_removes_checkpointed_rows` `test_gc_e2e_index_rows_collected_after_checkpoint` `test_delete_row_is_hidden_from_desc_unique_index_scan` `test_delete_row_is_skipped_by_desc_explicit_index_scan` `test_delete_btree_resident_row_is_skipped_by_desc_unique_index_scan` `test_mvcc_dual_cursor_delete_all_btree_reinsert` `test_checkpoint_root_page_mismatch_with_index` `test_checkpoint_post_durable_drop_failure_retry_removes_stale_rootpage_mapping` `test_gc_e2e_checkpointed_row_readable_after_gc` `test_btree_resident_update_then_delete_checkpoints_after_reopen` `test_double_delete_btree_resident_row_with_unique_index` `test_passive_checkpoint_skips_late_tombstone_after_prior_destroy` `test_checkpoint_seek_skip_divider_reinsert_loses_row` `test_passive_checkpoint_truncate_wal_tolerates_concurrent_drop_of_checkpointed_table` `test_passive_checkpoint_preserves_drop_committed_after_collection` `issue_8467_seek_after_checkpoint_publish_does_not_read_negative_root`
  - `it/mvcc.rs` (1): `test_mvcc_update_btree_only_row_after_truncate_checkpoint`
  - `it/query_processing/test_transactions.rs` (6): `test_mvcc_dual_seek_table_rowid_basic` `test_mvcc_dual_seek_interleaved_rows` `test_mvcc_dual_seek_index_basic` `test_mvcc_dual_seek_with_update` `test_mvcc_dual_seek_with_delete` `test_mvcc_dual_seek_range_operations`
- **SQL (110)** — SQL or SQLite feature or engine mode absent in GoGraph: tables and the schema table, ALTER/RENAME, triggers, views, DROP TABLE, conflict clauses (OR REPLACE, UPSERT), journal-mode switching, VACUUM, header PRAGMAs, per-connection synchronous mode, BEGIN IMMEDIATE/DEFERRED locking, blob handles.
  - `db/group_commit_sync_mode_tests.rs` (1): `full_waiter_is_synced_when_group_leader_uses_sync_off`
  - `db/checkpoint_state_machine.rs` (7): `local_schema_record_shares_mvcc_payload` `sqlite_schema_identity_treats_index_sql_rewrite_as_same_object` `sqlite_schema_identity_treats_table_sql_rewrite_as_same_object` `sqlite_schema_identity_detects_drop_recreate_as_different_objects` `sqlite_schema_identity_detects_drop_without_successor` `sqlite_schema_identity_ignores_non_btree_schema_entries` `sqlite_schema_identity_ignores_payloadless_tombstones`
  - `db/tests.rs` (60): `mvcc_active_read_tx_blocks_vacuum_gate` `mvcc_active_write_tx_blocks_vacuum_gate` `mvcc_vacuum_gate_blocks_new_read_and_write_tx` `mvcc_pragma_page_size_propagates_to_global_header` `mvcc_reset_after_vacuum_installs_header_and_rootpages` `mvcc_try_get_table_id_stale_schema_read_returns_none` `mvcc_reset_after_vacuum_clears_checkpointed_empty_version_buckets` `test_recovery_replays_schema_op_after_data_op_in_frame` `test_journal_mode_switch_from_mvcc_to_wal_without_log_frames` `abandoned_journal_mode_checkpoint_releases_pager_transaction_and_lock` `test_restart_preserves_autoindex_to_column_mapping` `test_restart_with_trigger_rootpage_zero` `test_checkpoint_allows_index_schema_update_after_rename_column` `test_full_checkpoint_reopen_recovers_truncate_mode` `test_flag_off_truncate_busy_when_lock_contended` `test_header_only_mutation_is_replayed_and_checkpointed` `test_mvcc_header_updates_require_exclusive_transaction` `test_mvcc_header_updates_allow_autocommit_statement_tx` `test_checkpoint_stale_boundary_does_not_replay_checkpointed_create_table_after_restart` `test_mvcc_memory_keeps_builtin_table_valued_functions` `test_mvcc_checkpoint_insert_or_replace_then_delete_removes_checkpointed_index_entries` `test_mvcc_checkpoint_update_or_replace_then_delete_removes_checkpointed_index_entries` `test_mvcc_repeated_delete_after_replace_delete_checkpoint_is_noop` `test_mvcc_checkpoint_reopen_text_pk_upsert_delete_removes_autoindex_entry` `test_mvcc_checkpoint_integrity_after_upsert_with_secondary_indexes` `test_mvcc_integrity_after_mixed_dml_create_index_transaction` `test_integrity_check_after_drop_table_before_checkpoint` `test_interrupted_drop_table_rolls_back_schema_table_and_indexes` `test_gc_unwritten_text_pk_upsert_does_not_fork` `test_gc_reclaimed_text_pk_same_txn_sees_own_append` `test_gc_unstamped_text_pk_unique_lookup_still_sees_key` `test_passive_finalize_reclaim_text_pk_unique_lookup_still_sees_key` `test_checkpoint_drop_table` `test_checkpoint_drop_table_then_create_index_page_reuse` `test_checkpoint_drop_table_removes_stale_rootpage_mapping` `test_abandoned_journal_mode_mvcc_bootstrap_restores_connection` `test_alter_table_rename_with_index_panics_on_restart` `test_alter_table_rename_with_unique_constraint_panics_on_restart` `test_checkpoint_skips_uncheckpointed_view_and_trigger_deletes_after_recovery` `test_checkpoint_deletes_checkpointed_view_and_trigger_schema_rows_after_recovery` `test_alter_add_column_with_index_dml_does_not_corrupt_on_reopen` `test_create_then_drop_index_in_one_tx_replays_on_reopen` `test_schema_frame_recovery_create_index_with_mixed_dml_matrix` `test_schema_frame_recovery_drop_recreate_table_indexes_matrix` `test_schema_frame_recovery_same_name_partial_index_redefinition` `test_schema_rewrites_do_not_drop_table_versions_from_recovery_log` `test_schema_frame_recovery_rename_column_then_drop_index_checkpoints_after_reopen` `test_close_persists_drop_table` `test_abandoned_drop` `test_checkpoint_recovers_after_crash_restart_drop_recreate_table` `test_auto_checkpoint_refreshes_index_metadata_after_schema_change` `test_recovery_after_drop_table_with_uncheckpointed_index` `test_recovery_after_drop_checkpointed_table_with_index` `test_recovery_after_drop_checkpointed_table_with_if_not_exists_index` `test_recovery_after_drop_table_with_many_schema_rows` `test_drop_recreate_indexed_table_many_inserts_restart` `test_create_type_visible_to_second_connection_under_mvcc` `test_mvcc_passive_replace_then_delete_keeps_table_and_index_consistent` `test_read_after_database_full_checkpoint_remains_usable` `on_checkpoint_end_runs_before_blocking_checkpoint_unlock`
  - `db/group_commit_tests.rs` (4): `group_commit_pragma_defaults_on_and_round_trips` `group_commit_pragma_is_store_wide` `group_commit_pragma_needs_mvcc` `begin_immediate_still_commits_with_group_commit_on`
  - `it/mvcc.rs` (9): `test_mvcc_custom_durable_storage_injected` `test_drop_cleans_up_mvcc_transactions` `test_add_then_drop_table_in_same_tx_then_recover` `test_create_insert_drop_checkpoint_recover` `test_recover_table_with_create_virtual_substring_in_sql` `test_create_drop_index_same_tx_recover` `test_create_rename_insert_same_tx_recover_then_checkpoint` `test_create_insert_drop_same_tx_recover` `test_multiple_create_drop_cycles_recover`
  - `it/query_processing/test_transactions.rs` (20): `test_deferred_transaction_restart` `test_busy_wait_returns_sleep_step_result` `test_deferred_transaction_no_restart` `test_delete_all_while_same_connection_scans_table` `test_deferred_fk_violation_rollback_in_autocommit` `test_mvcc_transactions_immediate` `test_mvcc_transactions_deferred` `test_non_mvcc_to_mvcc` `test_commit_without_mvcc` `test_rollback_without_mvcc` `test_insert_or_fail_keeps_prior_changes` `test_insert_or_abort_rolls_back_statement` `test_insert_or_rollback_rolls_back_transaction` `test_insert_or_rollback_unique_constraint_in_transaction` `test_insert_or_rollback_in_autocommit` `test_insert_or_fail_unique_constraint` `test_update_or_fail_keeps_prior_changes` `test_update_or_abort_rolls_back_statement` `test_update_or_rollback_rolls_back_transaction` `test_update_or_rollback_in_autocommit`
  - `it/query_processing/test_vacuum_into_mvcc.rs` (5): `test_vacuum_into_with_views` `test_vacuum_into_with_triggers` `test_vacuum_into_preserves_meta_values` `test_vacuum_into_large_data_multi_page` `test_vacuum_into_with_partial_indexes`
  - `sim/mvcc_recovery.rs` (2): `sim_mvcc_failed_rename_is_atomic_across_restart` `sim_mvcc_failed_drop_table_is_atomic_across_restart`
  - `it/suspended_statement_checkpoint.rs` (1): `test_open_blob_handle_does_not_block_explicit_checkpoints`
  - `testing/cli_tests/mvcc.py` (1): `test_create_table_with_mvcc`
- **WALPAGE (118)** — SQLite page WAL: page frames, read marks, backfill, PASSIVE/FULL/RESTART/TRUNCATE modes, page codec, prepared-statement re-preparation and page-level integrity_check after a checkpoint; also the non-MVCC fuzz arm. GoGraph's WAL is a logical frame log (`store/wal/FORMAT.md`).
  - `db/tests.rs` (15): `test_prepared_select_reprepares_after_checkpoint_root_publish` `test_prepared_select_does_not_reprepare_after_data_only_checkpoint` `test_prepared_index_lookup_reprepares_after_checkpoint_root_publish` `test_integrity_check_after_checkpoint_io_yield_then_post_durable_failure_uses_user_apis` `test_running_integrity_check_reprepares_after_checkpoint_root_publish` `test_deferred_begin_integrity_check_reprepares_after_checkpoint_root_publish` `test_running_integrity_check_reprepares_without_schema_cookie_bump` `reader_does_not_pin_read_mark_until_checkpoint_gate_is_available` `integrity_check_does_not_pin_read_mark_until_checkpoint_gate_is_available` `integrity_check_does_not_report_freelist_count_mismatch_after_checkpoint_begin_race` `integrity_check_does_not_report_page_never_used_after_checkpoint_begin_race` `test_integrity_check_ignores_dropped_root_that_is_live_after_recovery` `test_integrity_check_tolerates_dropped_root_reused_as_btree_child` `test_integrity_check_passive_reads_freelist_from_pager_not_stale_mvcc_header` `test_passive_checkpoint_preserves_late_dropped_root_tracking`
  - `it/fuzz_transaction/mod.rs` (1): `test_multiple_connections_fuzz_non_mvcc`
  - `it/wal/test_power_loss_before_first_checkpoint.rs` (2): `test_page1_is_durable_in_main_file_before_first_wal_commit` `test_orphan_wal_of_an_empty_database_is_discarded_like_sqlite`
  - `it/wal/test_wal_publish_backfill_race.rs` (2): `wal_stale_publish_backfill_hides_committed_rows` `wal_stale_publish_backfill_can_point_table_at_restarted_index_root`
  - `it/wal/test_wal.rs` (5): `test_wal_checkpoint_result` `test_truncate_checkpoint_not_busy_after_rollback` `test_wal_1_writer_1_reader` `test_wal_read_lock_released_on_conn_drop` `test_wal_write_lock_released_on_conn_drop`
  - `it/checkpoint_stale_page_size.rs` (2): `test_checkpoint_uses_database_page_size` `test_checkpoint_from_connection_with_default_page_size`
  - `core/storage/wal.rs` (91): `replace_after_external_restore_preserves_lock_identity` `test_truncate_file` `test_wal_truncate_checkpoint` `test_shutdown_checkpoint_truncates_after_restart` `test_wal_checkpoint_defers_backfill_publication_until_db_sync` `test_checkpoint_sync_mode_off_leaves_backfill_unpublished` `in_process_checkpoint_with_no_frames_to_backfill_compares_no_frames` `append_frames_vectored_spill_frames_are_not_reused_by_next_prepare` `read_frames_batch_reads_contiguous_wal_frames_directly` `page_codec_round_trips_wal_batch_reads` `page_codec_missing_wal_frame_reports_short_read` `page_codec_round_trips_vectored_wal_spill_frames` `page_codec_vectored_spill_encode_error_does_not_advance_wal` `page_codec_round_trips_raw_wal_frames` `page_codec_raw_wal_encode_error_does_not_advance_wal` `page_codec_raw_wal_duplicate_is_idempotent_and_detects_conflict` `page_codec_raw_wal_read_rejects_wrong_buffer_size` `checksum_is_applied_to_raw_wal_frames` `page_codec_raw_wal_read_propagates_decode_error` `page_codec_wal_batch_read_reports_codec_error` `page_codec_wal_batch_decode_failure_does_not_publish_earlier_pages` `read_frames_batch_can_start_from_middle_frame` `read_frames_batch_follows_physical_frame_order_not_page_id_order` `read_frames_batch_short_read_errors_and_clears_page_locks` `read_frames_batch_page_number_mismatch_returns_error_not_panic` `test_read_frame_keeps_epoch_from_issue_time` `test_wal_connection_state_round_trip` `test_wal_explicit_backend_constructor_does_not_keep_shared_handle` `test_mvcc_refresh_updates_snapshot_only_when_no_read_guard_is_held` `latest_frame_in_range_returns_the_newest_frame_inside_the_range` `in_process_frame_lookups_compare_few_frames_when_a_page_has_many_frames` `test_in_process_coordination_uses_shared_authority` `test_in_process_coordination_publishes_checkpoint_and_restart_state` `test_in_process_coordination_manages_frame_cache` `cache_frame_purges_stale_mapping_on_frame_slot_reuse` `test_in_process_coordination_transaction_guards` `reader_does_not_wait_for_a_checkpoint_that_holds_read_mark_0` `readers_share_a_mark_pinned_at_frame_zero` `shm_reader_does_not_wait_for_a_checkpoint_that_holds_read_mark_0` `shm_readers_share_a_mark_pinned_at_frame_zero` `test_shm_coordination_uses_shared_authority` `test_shm_coordination_many_same_snapshot_readers_share_one_published_slot` `test_shm_coordination_uses_one_published_slot_per_active_snapshot_generation` `test_shm_coordination_shared_index_grows_past_old_fixed_limit` `test_shm_coordination_restart_uses_authority_snapshot` `test_shm_coordination_exclusive_reopen_reuses_persisted_authority` `test_open_shared_from_authority_reuses_trusted_snapshot_after_exclusive_reopen` `test_shm_coordination_live_overflow_returns_busy_without_runtime_disk_scan` `test_open_shared_from_authority_exclusive_rebuilds_positive_snapshot_from_disk` `test_shared_coordination_open_uses_reconciled_snapshot_for_local_wal_state` `test_open_shared_from_authority_rebuilds_from_disk_when_snapshot_is_stale` `test_open_shared_from_authority_rebuilt_authority_persists_across_exclusive_reopen` `test_open_shared_from_authority_exclusive_disk_scan_does_not_downgrade_newer_zero_frame_generation` `test_open_shared_from_authority_ignores_unpublished_backfill_proof_after_exclusive_reopen` `test_restart_checkpoint_clears_backfill_proof_and_later_replaces_it` `test_truncate_checkpoint_clears_backfill_proof_and_later_replaces_it` `test_classify_authority_snapshot_marks_truncated_wal_for_rebuild` `test_classify_authority_snapshot_marks_corrupt_header_for_rebuild` `test_open_shared_from_authority_keeps_zero_length_wal_uninitialized_after_exclusive_reopen` `test_shm_coordination_secondary_disk_scan_does_not_reseed_authority_while_writer_active` `test_shm_coordination_disk_scan_matching_authority_keeps_frame_index` `test_shm_coordination_disk_scan_matching_snapshot_rebuilds_stale_frame_index` `test_shm_coordination_empty_disk_scan_keeps_zero_frame_authority_metadata` `test_shm_coordination_empty_disk_scan_does_not_clobber_positive_authority` `test_shm_zero_frame_authority_invalidates_stale_local_initialized_state` `test_shm_prepare_wal_header_seeds_uninitialized_authority_from_prepared_header` `test_shm_prepare_wal_header_does_not_clobber_zero_frame_authority_snapshot` `test_in_process_coordination_lock_primitives` `test_in_process_coordination_prepare_truncate_marks_wal_uninitialized` `test_in_process_coordination_exposes_wal_io_state` `test_vacuum_lock_blocks_new_read_transactions_until_release` `test_active_reader_blocks_vacuum_exclusive_tx` `test_read_retry_does_not_leak_vacuum_guard_or_block_vacuum` `test_held_vacuum_checkpoint_locks_do_not_release_vacuum_lock` `restart_checkpoint_reset_wal_state_handling` `test_wal_passive_partial_then_complete` `test_wal_restart_blocks_readers` `test_wal_read_marks_after_restart` `test_wal_checkpoint_updates_read_marks` `test_wal_writer_blocks_restart_checkpoint` `test_wal_read_transaction_required_before_write` `test_wal_multiple_readers_at_different_frames` `test_checkpoint_truncate_reset_handling` `test_wal_checkpoint_truncate_db_file_contains_data` `test_wal_stale_snapshot_in_write_transaction` `test_wal_readlock0_optimization_behavior` `test_wal_full_backfills_all` `test_rollback_releases_read_lock` `test_rollback_releases_shared_read_lock_slot` `test_rollback_releases_slot_zero_read_lock` `test_checkpoint_succeeds_after_rollback`
- **MP (62)** — multi-process shared WAL coordination: GoGraph's WAL writer holds an exclusive OS lock on its directory (`store/wal/writer.go:254-263`), so one process owns a store.
  - `whopper/multiprocess.rs` (4): `parse_worker_response_line_rejects_invalid_json` `recv_response_times_out_when_worker_stops_responding` `parse_worker_response_line_accepts_valid_json` `operation_history_writer_streams_jsonl_events`
  - `whopper/regression_tests.rs` (12): `multiprocess_same_process_sibling_reader_keeps_shared_snapshot_live_until_last_release` `multiprocess_restart_reuses_persisted_tshm_without_disk_scan` `multiprocess_finalize_after_restart_preserves_simple_kv_rows` `multiprocess_integrity_check_after_restart_preserves_authoritative_wal_snapshot` `multiprocess_committed_large_row_survives_repeated_restarts` `multiprocess_truncate_generation_survives_foreign_first_append_and_restart` `multiprocess_restart_rebuilds_from_disk_when_partial_checkpoint_publishes_positive_nbackfills` `multiprocess_restart_rebuilds_from_disk_after_wal_append_invalidates_partial_checkpoint_proof` `multiprocess_restart_rebuilds_from_disk_after_partial_checkpoint_proof_is_cleared` `multiprocess_restart_rebuilds_from_disk_after_db_header_mismatch_invalidates_partial_checkpoint_proof` `multiprocess_restart_stays_conservative_after_unpublished_backfill_proof_install` `multiprocess_restart_rebuilds_from_disk_after_restart_checkpoint_changes_generation`
  - `core/storage/shared_wal_coordination.rs` (46): `shared_wal_coordination_header_round_trips` `shared_wal_coordination_header_rejects_invalid_magic` `shared_owner_record_round_trips_pid_and_instance` `process_local_ownership_state_tracks_same_process_exclusion` `mapped_shared_wal_coordination_reclaims_dead_reader_owner` `process_scoped_mapping_drop_releases_same_process_ownership` `process_scoped_mapping_reopens_after_stale_owner_fields` `process_scoped_mapping_ignores_stale_same_pid_writer_owner_field` `process_scoped_mapping_reclaims_stale_same_pid_reader_slot` `mapped_shared_wal_coordination_persists_file_after_last_close` `mapped_shared_wal_coordination_repair_reclaims_dead_owners_without_clearing_frame_index` `mapped_shared_wal_coordination_repair_preserves_live_reader_slots_and_frame_index` `mapped_shared_wal_coordination_rebuilds_undersized_file_on_exclusive_open` `mapped_shared_wal_coordination_shares_lock_and_reader_state` `mapped_shared_wal_coordination_last_process_probe_reacquires_shared_lifetime_lock` `mapped_shared_wal_coordination_snapshot_waits_for_stable_sequence` `mapped_shared_wal_coordination_prevents_checkpoint_lock_reuse_across_mappings` `mapped_shared_wal_coordination_persists_backfill_proof_across_reopen` `mapped_shared_wal_coordination_publish_commit_clears_backfill_proof` `mapped_shared_wal_coordination_install_snapshot_clears_backfill_proof` `mapped_shared_wal_coordination_rejects_corrupt_backfill_proof_crc` `mapped_shared_wal_coordination_rejects_structurally_impossible_backfill_proof` `mapped_shared_wal_coordination_exclusive_reopen_clears_corrupt_backfill_proof` `mapped_shared_wal_coordination_exclusive_reopen_clears_unsupported_backfill_proof_version` `mapped_shared_wal_coordination_exclusive_reopen_clears_impossible_backfill_proof_payload` `mapped_shared_wal_coordination_prevents_reentrant_lock_reuse_within_same_mapping` `mapped_shared_wal_coordination_ignores_stale_writer_owner_field` `mapped_shared_wal_coordination_ignores_stale_checkpoint_owner_field` `mapped_shared_wal_coordination_publish_commit_keeps_monotonic_transaction_count` `mapped_shared_wal_coordination_reclaims_stale_reader_slots` `mapped_shared_wal_coordination_tracks_frame_index_entries` `mapped_shared_wal_coordination_finds_simple_kv_page_after_seed_frame_sequence` `mapped_shared_wal_coordination_grows_frame_index_across_block_boundary` `mapped_shared_wal_coordination_iterates_latest_frames_across_full_blocks` `frame_index_lookups_start_at_the_slot_of_min_frame` `frame_index_lookups_scan_only_blocks_with_frames_in_range` `frame_index_lookups_with_min_frame_match_a_full_scan` `mapped_shared_wal_coordination_marks_overflow_once_reserved_space_is_full` `mapped_shared_wal_coordination_rebuilds_block_hash_after_rollback` `mapped_shared_wal_coordination_clears_stale_frame_index_when_wal_restarts` `mapped_shared_wal_coordination_install_snapshot_trims_stale_frame_index_tail` `mapped_shared_wal_coordination_handles_hash_collisions` `mapped_shared_wal_coordination_reuses_block_hash_slots_after_rollback` `mapped_shared_wal_coordination_keeps_initial_file_small` `mapped_shared_wal_coordination_respects_sparse_frame_watermarks` `mapped_shared_wal_coordination_rolls_back_across_block_boundary`
- **HEKATON (20)** — Hekaton commit dependencies (speculative reads of a Preparing transaction, cascading aborts) and the finalized-state cache: absent by design, a reader never sees another transaction's uncommitted or unpublished version (`graph/mvcc/mvcc.go:159-168`; F11).
  - `db/tests.rs` (20): `test_visibility_uses_finalized_state_for_removed_committed_tx` `test_read_only_commit_does_not_cache_finalized_state` `test_drop_unused_row_versions_prunes_unreferenced_finalized_tx_states` `test_gc_keeps_finalized_tx_state_of_tx_still_in_txs` `test_commit_dependency_speculative_read` `test_commit_dependency_cascade_abort` `test_commit_dependency_already_committed` `test_commit_dependency_already_aborted` `test_commit_dependency_speculative_ignore` `test_index_shadow_scan_no_spurious_dep_on_stepped_over_key` `test_commit_dependency_multiple_reads_dedup` `test_commit_dep_threaded_abort_cascades` `test_commit_dep_threaded_multiple_dependents_abort` `test_commit_dep_threaded_commit_resolves` `test_commit_dep_threaded_readonly_abort_cascades` `test_commit_dependency_counter_no_underflow` `test_commit_dependency_terminated_tx_sets_abort` `test_commit_dependency_missing_tx_assumes_committed` `test_commit_dep_readonly_does_not_advance_timestamp` `test_commit_dep_readonly_does_not_cause_spurious_busy`
- **ENC (32)** — log or WAL encryption: absent.
  - `db/tests.rs` (9): `test_mvcc_encrypted_log_recovery_and_wrong_key` `test_mvcc_late_encryption_setup_keeps_metadata_bootstrapped` `test_mvcc_encrypted_restart_without_key_fails_before_recovery` `test_encrypted_recovery_large_payload_multi_chunk` `test_encrypted_recovery_corrupted_later_chunk_keeps_checkpointed_prefix` `test_mvcc_portable_changes_are_encrypted_with_log_body` `test_encrypted_recovery_checkpoint_then_more_writes` `test_encrypted_recovery_multiple_restart_cycles` `test_encrypted_recovery_corrupted_ciphertext`
  - `logical_log.rs` (19): `test_encrypted_log_recovery_forced_yields` `test_encrypted_log_roundtrip_and_layout` `test_encrypted_log_roundtrip_with_test_chunk_size_override` `test_encrypted_log_carry_fuzz` `test_encrypted_log_format_assumptions_are_pinned` `test_encrypted_chunk_aad_layout_is_pinned` `test_encrypted_log_aes128_chunk_layout_assumptions_are_pinned` `test_encrypted_log_payload_size_tamper_rejected` `test_encrypted_log_chunk_layout_boundaries` `test_encrypted_log_single_op_crosses_chunk_boundary` `test_encrypted_log_varint_crosses_chunk_boundary` `test_encrypted_log_header_op_crosses_chunk_boundary` `test_encrypted_log_many_ops_cross_chunk_boundaries` `test_encrypted_log_upsert_index_crosses_chunk_boundary` `test_encrypted_log_multiple_frames_crc_chain` `test_encrypted_log_integrity_rejection` `test_encrypted_log_torn_tail_rejected` `test_encrypted_log_chunk_integrity_rejection` `test_encrypted_log_chunk_torn_tail_rejected`
  - `logical_log/serializer.rs` (2): `fragment_chunks_preserve_wire_format` `chunk_length_mismatch_does_not_change_buffer`
  - `it/mvcc.rs` (1): `test_attach_memory_db_allowed_on_encrypted_mvcc_main`
  - `core/storage/wal.rs` (1): `encryption_round_trips_raw_wal_frames`
- **CDC (29)** — portable change stream / CDC log format: absent.
  - `db/tests.rs` (21): `test_mvcc_portable_changes_encoder_matches_metadata_wire_golden` `test_mvcc_portable_changes_disabled_by_default` `test_mvcc_portable_changes_contains_user_schema_and_rows` `test_mvcc_portable_changes_updates_name_mapping_across_rename` `test_mvcc_portable_changes_emit_refresh_for_same_rowid_schema_update` `test_mvcc_portable_changes_emit_drop_and_create_for_drop_recreate_same_name` `test_mvcc_portable_changes_resolve_rows_through_object_map_in_same_txn` `test_mvcc_mode_supports_cdc_for_client_push` `test_mvcc_portable_changes_emit_index_drop_for_drop_table` `test_mvcc_portable_changes_emit_index_trigger_and_view_schema_ops` `test_mvcc_portable_changes_emit_trigger_and_view_lifecycle_ops` `test_mvcc_portable_changes_emit_header_only_commits` `test_mvcc_portable_changes_use_checkpointed_schema_after_restart` `test_mvcc_portable_changes_resolve_user_table_after_cross_connection_checkpoint` `test_mvcc_portable_changes_resolve_table_after_alter_backfill` `test_mvcc_portable_changes_emit_ddl_and_backfill_rows_in_same_transaction` `test_mvcc_portable_changes_delete_carries_pk_projection_not_old_record` `test_mvcc_portable_changes_do_not_infer_origin_from_application_table` `test_mvcc_portable_changes_mark_internal_only_commits_explicitly` `test_mvcc_portable_changes_metadata_does_not_auto_enable_or_get_consumed` `test_mvcc_portable_changes_resolve_checkpointed_table_despite_counter_id_alias`
  - `logical_log.rs` (7): `test_non_portable_first_write_uses_lml2_header_and_v2_frame` `test_non_portable_appends_keep_lml2_header_and_v2_frames` `test_portable_changes_upgrade_non_empty_lml2_log_to_lml3` `test_failed_lml3_header_upgrade_keeps_lml2_header_and_retries` `test_next_portable_change_frame_returns_empty_and_nonempty_lml3_frames` `test_portable_extension_block_precedes_recovery_payload` `test_next_portable_change_frame_does_not_advance_lml2_logs`
  - `logical_log/serializer.rs` (1): `portable_extension_insert_preserves_wire_format`
- **SP (24)** — savepoints and statement-level rollback inside a transaction: absent, a failed statement poisons the transaction (F8); the semantic divergence is kept in AB01.
  - `db/tests.rs` (7): `savepoint_does_not_pin_read_mark_until_checkpoint_gate_is_available` `test_conflict_abort_ckpt_indexed_update_savepoint_integrity_check` `test_conflict_abort_ckpt_indexed_update_savepoint_integrity_check_passive` `test_savepoint_multiple_statements_last_fails` `test_savepoint_same_row_multiple_statements` `test_savepoint_index_multiple_statements` `test_savepoint_insert_delete_then_fail`
  - `it/mvcc.rs` (7): `test_stmt_rollback_on_attached_mvcc_db` `test_stmt_rollback_on_attached_mvcc_db_with_index` `test_mvcc_temp_stmt_rollback_unique` `test_mvcc_temp_stmt_rollback_unique_via_trigger` `test_mvcc_temp_stmt_release_keeps_successful_write` `test_named_savepoint_rollback_reverts_attached_mvcc` `test_named_savepoint_release_commits_attached_mvcc`
  - `it/query_processing/test_transactions.rs` (4): `test_wal_savepoint_rollback_on_constraint_violation` `test_savepoint_rollback_across_wal_restart` `test_update_or_replace_check_stmt_rollback` `test_update_or_replace_notnull_stmt_rollback`
  - `it/wal/test_wal.rs` (2): `test_savepoint_rollback_after_cache_spill_preserves_wal_pages` `test_wal_cacheflush_savepoint_rollback_preserves_pre_savepoint_indexed_rows`
  - `it/checkpoint_crash_atomicity.rs` (1): `savepoint_spill_crash_image_matches_sqlite_recovery`
  - `core/storage/wal.rs` (3): `test_savepoint_rollback_discards_frame_cache_past_rollback_point` `test_savepoint_rollback_preserves_read_lock` `test_savepoint_then_tx_rollback_allows_restart_checkpoint_from_other_connection`
- **ATTACH (19)** — attached and temporary databases, cross-database transactions: absent.
  - `db/tests.rs` (7): `attached_reader_does_not_pin_read_mark_until_checkpoint_gate_is_available` `dropped_main_commit_rolls_back_attached_mvcc_txs` `dropped_main_commit_rolls_back_temp_schema_changes` `dropped_attached_commit_releases_attached_read_lock` `dropped_attached_commit_rolls_back_remaining_attached_mvcc_txs` `test_autoincrement_in_attached_mvcc_database` `test_create_sequence_in_attached_mvcc_database`
  - `it/mvcc.rs` (10): `test_mvcc_create_table_on_attached_db` `test_detach_rollbacks_active_mvcc_tx` `test_attach_rejects_incompatible_journal_mode` `test_attach_rejects_mvcc_attached_on_wal_main` `test_mvcc_rollback_reverts_attached_db` `test_mvcc_qualified_checkpoint_uses_attached_db_schema` `test_cross_database_transaction` `test_attached_mvcc_read_to_write_upgrade` `test_deferred_fk_violation_rolls_back_attached_mvcc` `test_attach_memory_db_always_allowed`
  - `it/query_processing/test_transactions.rs` (2): `test_begin_immediate_does_not_create_temp_database` `test_begin_immediate_lazily_created_temp_follows_transaction`
- **SEQ (18)** — sequences and AUTOINCREMENT: absent; node-identity semantics kept in GG07.
  - `db/tests.rs` (16): `test_sequence_watermark_tracks_lowest_active_allocation` `test_sequence_watermark_function_returns_current_watermark_without_active_allocations` `test_sequence_watermark_tracks_nextval_allocations` `test_sequence_watermark_reader_never_skips_committed_rows_fuzz` `test_autoincrement_works_in_mvcc` `test_autoincrement_insert_works_for_preexisting_table` `test_autoincrement_sqlite_sequence_after_checkpoint` `test_inner_tx_cleanup_after_sequence_exhaustion` `test_auto_commit_coherent_after_sequence_exhaustion_in_outer_tx` `test_auto_rowid_after_negative_explicit_rowid_uses_next_negative` `test_checkpoint_after_create_and_drop_sequence` `test_descending_sequence_compaction` `test_nextval_no_inner_tx_retry_on_concurrent_mvcc` `test_sequence_write_conflict_rolls_back_outer_tx_and_rejects_commit` `test_multi_row_autoincrement_insert_atomic_on_sequence_exhaustion` `test_per_statement_atomicity_across_multi_statement_autoincrement_tx`
  - `it/query_processing/test_transactions.rs` (2): `test_user_sequence_watermark_survives_restart_mvcc` `test_concurrent_nextval_no_database_busy`
- **FMT (19)** — logical-log binary layout, header fields and serializer round trips: format-specific; corruption and torn-tail behaviour kept in D08, D09, G11.
  - `logical_log.rs` (17): `test_logical_log_read` `test_logical_log_read_multiple_transactions` `test_logical_log_read_fuzz` `test_logical_log_read_table_and_index_rows` `test_logical_log_read_i32_min_table_id` `test_logical_log_rowid_negative_varint_roundtrip` `test_on_serialization_complete_gets_shared_write_bytes` `test_logical_log_header_flags_rejected` `test_logical_log_header_non_default_len_rejected` `test_logical_log_header_reserved_bytes_rejected` `test_logical_log_op_reserved_flags_rejected` `test_logical_log_non_negative_table_id_rejected` `test_logical_log_empty_transaction_frame` `test_logical_log_roundtrip_random_table_ops` `test_logical_log_btree_resident_roundtrip` `test_logical_log_header_persistence` `test_logical_log_header_crc_roundtrip`
  - `discard_pending_tests.rs` (1): `discard_pending_log_write_clears_staged_pending_crc`
  - `logical_log/serializer.rs` (1): `insert_preserves_surrounding_bytes`
- **ALLOC (18)** — fallible-allocation injection: Go has no recoverable allocation failure.
  - `db/tests.rs` (6): `write_set_take_transfers_entries_without_cloning` `mv_store_skiplist_allocations_are_fallible` `row_payload_allocation_uses_passed_allocator` `index_key_payload_allocation_uses_passed_allocator` `seqcompact_tombstone_payload_uses_store_allocator` `mv_store_insert_allocation_failure_leaves_tx_state_untouched`
  - `whopper/allocation_fault.rs` (9): `probability_threshold_handles_edges` `allocation_hash_changes_by_occurrence` `vector_allocation_sites_have_distinct_ids` `value_blob_allocation_sites_have_distinct_ids` `btree_allocation_sites_have_distinct_ids` `btree_allocation_sites_are_fault_injectable` `vector_allocation_site_is_fault_injectable` `value_blob_concat_site_is_fault_injectable` `production_allocation_failures_propagate_and_leave_state_reusable`
  - `whopper/chaotic_btree.rs` (3): `generated_workload_contains_savepoint_churn` `allocation_failure_is_followed_by_integrity_check` `allocation_failure_during_integrity_check_is_retried`
- **ASYNC (18)** — cooperative-async step/yield/suspend plumbing (statements returning IO, preemption of long scans, suspended statements): Go goroutines block; the abandoned-commit semantics are kept in D19.
  - `db/checkpoint_state_machine.rs` (6): `collect_table_rows_preempts_on_large_scan` `collect_table_rows_yields_in_schema_pass` `collect_table_rows_yields_in_user_pass` `collect_index_rows_preempts_on_large_scan` `gc_checkpointed_table_versions_preempts_on_large_scan` `gc_checkpointed_index_versions_preempts_on_large_scan`
  - `db/tests.rs` (8): `test_mvcc_cursor_next_yields_with_injected_yield` `test_concurrent_commit_yield_spin` `test_build_log_record_yields_for_large_write_set` `rowid_allocator_lock_released_when_statement_dropped_at_seek_yield` `injected_yield_surfaces_as_step_result_yield` `dropping_passive_checkpoint_after_pager_commit_does_not_release_write_lock_twice` `connect_async_yields_instead_of_spinning_on_preparing_commit_dependency` `dropping_connect_async_state_mid_wait_does_not_block`
  - `it/suspended_statement_checkpoint.rs` (4): `test_wal_checkpoint_with_suspended_write_statement_keeps_statement_resumable` `test_checkpoint_rejected_while_parked_statement_holds_pragma_helper` `test_suspended_wal_checkpoint_rejects_new_statement` `test_checkpoint_api_rejects_suspended_statement_and_keeps_it_resumable`
- **HARNESS (30)** — self-tests of the Whopper simulator (Elle EDN encoding, protocol, workload generators, error classification, yield plans); the Elle history checker itself is the L01 reference.
  - `whopper/elle.rs` (3): `test_elle_op_to_edn` `test_elle_event_to_edn` `test_elle_history_recording`
  - `whopper/error_handling.rs` (7): `parse_error_in_tx_queues_rollback` `parse_error_autocommit_clears_txn` `sequence_exhaustion_is_swallowed` `generic_database_full_is_fatal` `out_of_memory_is_recoverable` `corrupt_respawns` `unknown_error_is_fatal`
  - `whopper/fts.rs` (2): `recovery_copy_keeps_committed_rows_and_excludes_uncommitted_rows` `old_reader_workload_rejects_changed_rows`
  - `whopper/io.rs` (1): `file_copies_and_dumps_keep_bytes_when_paths_contain_dot`
  - `whopper/properties.rs` (4): `cycle_wrap_accepts_post_wrap_values_when_watermark_is_in_last_slot` `simple_keys_abort_fiber_drops_pending_transaction_state` `elle_history_abort_fiber_marks_pending_transaction_as_info` `elle_history_abort_fiber_marks_pending_autocommit_as_info`
  - `whopper/protocol.rs` (1): `protocol_round_trips_classification_relevant_variants`
  - `whopper/regression_tests_cross_platform.rs` (4): `test_concurrent_commit_no_yield_spin` `test_checkpoint_probe_rejects_checkpoints_while_statements_are_suspended` `test_dropped_failed_statement_keeps_suspended_sibling_commit_intact` `test_elle_passive_seed_keeps_committed_text_pk_on_unique_lookup`
  - `whopper/workloads.rs` (6): `schema_churn_create_table_uses_schema_features` `schema_churn_skips_concurrent_transactions` `existing_table_index_targets_generated_schema` `json_workload_variants_cover_json_function_families` `json_workload_variants_execute` `json_workload_generates_select_operations`
  - `whopper/yield_injection.rs` (2): `test_seeded_plan_covers_zero_and_multiple_yields` `test_seeded_plan_can_repeat_same_yield_point`
- **FTS (10)** — full-text search: absent.
  - `whopper/allocation_fault.rs` (1): `fts_allocation_sites_are_fault_injectable`
  - `whopper/fts.rs` (3): `old_reader_keeps_fts_rows_after_another_connection_commits` `reopen_reports_fts_result_check_failures_from_unfinished_statements` `profiles_exercise_fts_in_wal_and_mvcc`
  - `whopper/main.rs` (2): `fts_modes_use_the_requested_journal_mode_and_cli_overrides` `fts_modes_reject_unrelated_workloads`
  - `whopper/operations.rs` (1): `fts_comparison_sql_detects_duplicate_match_rows`
  - `whopper/properties.rs` (2): `fts_comparison_accepts_matching_row_counts` `fts_comparison_rejects_duplicate_match_rows`
  - `whopper/regression_tests_cross_platform.rs` (1): `test_fts_workloads_use_the_index_and_replay_with_the_seed`
- **SYNC (9)** — sync engine / logical pull replication: absent.
  - `sync/engine/tests/mvcc_logical_pull/schema_changes.rs` (9): `drop_indexed_generated_column_and_its_index_in_one_transaction` `redefine_indexed_generated_column_and_its_index_in_one_transaction` `rename_column_used_by_index_and_trigger` `create_table_with_primary_key` `drop_table_with_primary_key` `rename_indexed_table` `drop_newest_table_and_create_index_in_one_transaction` `drop_newest_index_and_create_table_in_one_transaction` `drop_newest_table_and_create_another_table_in_one_transaction`
- **BASIC (12)** — single-session CRUD smoke tests with no concurrency, rollback or recovery semantic.
  - `db/tests.rs` (9): `test_insert_read` `test_read_nonexistent` `test_delete` `test_delete_nonexistent` `test_commit` `test_rollback` `test_delete_with_conn` `test_interactive_transaction` `test_commit_without_tx`
  - `it/query_processing/test_transactions.rs` (3): `test_mvcc_insert_select_basic` `test_mvcc_update_basic` `test_mvcc_update_same_row_twice`

Totals: kept 278, excluded 636: BT 88, SQL 110, WALPAGE 118, MP 62, HEKATON 20, ENC 32, CDC 29, SP 24, ATTACH 19, SEQ 18, FMT 19, ALLOC 18, ASYNC 18, HARNESS 30, FTS 10, SYNC 9, BASIC 12.
