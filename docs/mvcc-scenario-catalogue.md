# MVCC scenario catalogue for `examples/37_mvcc_write_contention`

Specification input for rmp #2933 (deterministic catalogue), #2934 (concurrency ladder,
history checking, growth) and #2935 (durability under concurrent writers).
Survey date 2026-09-28. The survey read the sources without executing them.

## 0. Sources and method

| Source | Commit | Paths read |
|---|---|---|
| PostgreSQL | `d9b5a63f49d9d372a6609243eb0e9417a0a35e6f` | `src/test/isolation/specs` (135 specs, every one dispositioned in E.1), `src/test/modules/injection_points/specs` (16), `src/test/recovery/t` (57 TAP names, 6 read), `contrib/amcheck/t` (6) |
| MySQL | `3b99be40f8a31d9396cad10accfd3ba51647d613` (2026-09-28) | `mysql-test/suite/innodb/t` (799), `suite/innodb_undo/t` (46), `suite/innodb_fts/t` (63), 22 transaction tests of `mysql-test/t`, 4 group-commit tests of `suite/binlog*` |
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
(`innodb/` unless another suite is named). "EXISTS" = a golden already in
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

---

## 4. Counts

| Task | H | M | L | Total |
|---|---:|---:|---:|---:|
| #2933 | 55 | 30 | 4 | 89 |
| #2933 proposals (FP01-FP03, §5) | 2 | 1 | 0 | 3 |
| #2934 | 13 | 7 | 0 | 20 |
| #2935 | 11 | 4 | 1 | 16 |
| **All** | **81** | **42** | **5** | **128** |

#2933 by family: WW 15, SK 14, MG 12, IX 10, DD 9, AB 6, HZ 4, RO 3, SE 4, RI 6, GG 6.
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
| SR | WW14, WW15, SK12, SK13, SK14, IX06, IX07, IX08, RO02, GG01, GG03, GG04, L03, L16, L17 | 15 | no |
| OW | IX01, IX02, IX03, IX10, L07, MG09(a), AB06 | 7 | no |
| WW | WW01-WW13, MG06, MG08, RI03, RI05, RI06, L04, L05 | 20 | no |
| AN | SK01-SK11, L01, L02 | 13 | no |
| MU | MG01-MG12, L13, L14 | 14 | no |
| DD | DD01-DD09, IX08, L19, D11 | 12 | no |
| AB | WW05, WW12, WW13, IX04, IX05, AB01-AB06, HZ03, L18, D05, D06 | 15 | no |
| HZ | HZ01-HZ04, L08, L09, L18 | 7 | no |
| RO | RO01, RO02, RO03, SK06, SK07, SK08, L01 (read arm) | 7 | no |
| SE | SE01, SE02, SE04, L11 | 4 | no |
| FP | SE02, SE03, L10 | **3** | at the floor |
| DR | D01-D16 | 16 | no |

**FP sits at the floor.** Its rows rely on seam H2, which `lpg.Graph.AllocateCommitTS`
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
| G2 | **No deterministic vacuum control in step scripts**; the vacuum is a background goroutine, so write-after-rollback goldens race (F5). | PG `horizons` (uses explicit VACUUM), MY `innodb_purge_stop_now`/`purge_run_now` in `flush-hang`, `lob_purge`, `virtual_purge` | Needs a Hook that calls `ReclaimNow`, or a "pause vacuum" control equal to InnoDB's `innodb_purge_stop_now`. |
| G3 | **No fairness or priority mechanism** under first-updater-wins; a large transaction can be refused indefinitely by small ones. | MY `innodb_cats`, `innodb_trx_weight`, `high_prio_trx_*` | L05 can only report starvation, not bound it. |
| G4 | **No locking read** (SELECT FOR UPDATE) to prevent write skew; the only remedy is a dummy write, and whether a value-preserving write conflicts is unverified here. | PG `simple-write-skew` et al. (SSI prevents), MY `t/locking_clause` | SK11 value-preserving arm must be pinned. |
| G5 | **No hook inside MERGE** between probe and create, so the speculative-insertion race cannot be scripted. | PG `insert-conflict-specconflict`, PG-inj `on_conflict_probe_window` | MG11 is only reachable by randomised load (L13). |
| G6 | **UNIQUE against an in-flight reservation is a non-retriable violation**, even when the reserver later rolls back; PostgreSQL waits and InnoDB waits on an S lock. | PG `insert-conflict-do-nothing`, `read-write-unique`; MY `iodku`, `innodb_replace` | A client cannot distinguish "value taken" from "value momentarily reserved". `constraints.go:869-876` names the refinement (reserver txn id) as pending. |
| G7 | **No online consistency verifier** (amcheck `bt_index_check(heapallindexed)` analogue) exposed for index-vs-graph agreement; each harness re-derives "seek = scan". | PG amcheck `t/001_verify_heapam.pl`, `t/002_cic.pl`, `t/004_verify_nbtree_unique.pl` | D10, L06, L19 each need their own oracle. **Unverified**: internal invariant checkers may exist (`docs/test-battery.md` not read). |
| G8 | **Contradictory documentation on adjacency append conflicts**: `addEdgeInfo` godoc (`graph/lpg/lpg.go:2239-2242`) says an append never conflicts with another append; `checkAppend` (`graph/lpg/mvcc_adjversion.go:145-157,196`) refuses it (rmp #2445). | PG `fk-contention` | **Settled by RI03: the code is right.** Of two transactions appending an edge to one node — same target or same source — the second is refused, through Cypher and through `lpg`, in every interleaving (`ri03-hub-same-target`, `ri03-hub-same-source`, both with `-lpg`). The `addEdgeInfo` godoc had already been corrected by commit `1ad20d03`; the stale text left was the comment on the `Graph.adjVer` field (`graph/lpg/lpg.go`), which said an append "is commutative and must not conflict with another append". It now states the rmp #2445 rule. |
| G9 | **Backfill source for CREATE INDEX / CONSTRAINT not established**: whether it reads a committed snapshot or the present graph (which holds open peers' writes) was not verified. | PG `multiple-cic` (CIC waits for older txns), MY `index-create-dml-rollback` | **Settled: a committed snapshot.** DD01 (index) and DD05 (UNIQUE constraint) pass in every interleaving, and DD01 and DD09 fail against a test-only mutant whose backfill reads the live graph (`examples/37_mvcc_write_contention/README.md`, negative control for G9). DD01/DD05 do not reproduce the #2931 class. |
| G10 | **Transaction state after a refused statement is not pinned**: whether COMMIT returns `ErrTxPoisoned` or the conflict after an `Exec` returned `ErrSerializationConflict`. | MY `t/innodb_deadlock`, `innodb_mysql_rbk` | AB03 must pin it. |

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
