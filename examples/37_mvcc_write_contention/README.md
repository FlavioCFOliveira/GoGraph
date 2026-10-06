# Example 37 — MVCC under concurrent WRITERS

## Scenario

An order-processing service ingests from several producers at once. Each producer
writes to its own customer records, but every order also touches a small set of
shared **inventory** nodes — so most transactions are independent and a predictable
fraction genuinely collide. Readers run throughout, answering queries while the
ingest continues.

That shape is deliberate. A workload with no contention cannot demonstrate that
conflict detection works; one where everything contends cannot demonstrate that
independent writers scale. This example has both, in a ratio the operator sets.

## Objective

Exercise and **measure** the write side of GoGraph's MVCC, which is the half the two
existing MVCC examples do not reach:

- example 35 measures reader latency under a mixed workload;
- example 36 checks snapshot isolation on the topology dimension.

Neither drives concurrent **writers**, so neither can observe a write-write conflict,
a retriable serialization error, a logical abort, read-your-own-writes inside an open
transaction, or a commit timestamp surviving a restart. Every one of those is a
property sprint 334 added.

## Purpose

Produce evidence, not a demonstration. The example must let a reader answer, from its
output alone:

1. **Does write throughput scale with writer count?** commits/s at 1, 2, 4, 8, 16
   writers, with the ratio against one writer stated. This is the number the sprint
   exists to move.
2. **What does contention actually cost?** conflict rate by store, retry count, and
   retry success — measured, with the hot-spot fraction that produced it.
3. **Are aborts correct?** a rolled-back transaction must leave no trace. Asserted by
   a conservation invariant that no external oracle can supply: the sum of a
   quantity that every transaction preserves.
4. **What do writers cost readers?** reader latency percentiles taken *beside* the
   writers, not after them.
5. **What does versioning retain?** version-chain depth distribution and version
   count against the bound, sampled while the workload runs.
6. **Does it survive a restart?** the data AND the MVCC commit clock, so a
   post-restart transaction cannot re-mint an instant a previous process published.

## Why it must be able to FAIL

The standing rule in this project is that an instrument which cannot fail on the
defective build proves nothing — three of GoGraph's own instruments have been caught
reporting a number they could only ever have produced.

So this example is **validated in both directions**, and the result is recorded here:

- run against a worktree at the pre-sprint head, or with the property deliberately
  broken, it must FAIL;
- the failure output is reproduced in this README so a reader can see what a broken
  build looks like, not merely be told it would differ.

## The self-contradictory check

The bracket-invariant discipline that made example 36 able to detect — bracketing each
observation between acknowledged commits — is **structurally blind to a read of the
wrong legal instant**: a stale-but-valid snapshot passes every bracket.

So this example also carries at least one check that needs **no external oracle**: a
statement whose two halves must agree with each other within one transaction, so a
disagreement is self-evidently a defect regardless of which instant was read. The
conservation invariant in (3) is that check — a transfer that debits one node and
credits another cannot change the total, whatever instant observes it.

Its cost is stated, and it is sized for the gate that has to run it. A check nobody
can afford to run is not a check.

## Evidence collected

Per the project's examples mandate, every relevant indicator is measured:

| Vector | How |
| --- | --- |
| CPU | `runtime/pprof` CPU profile over the workload |
| Heap | `runtime/pprof` heap profile plus `runtime.MemStats` sampling |
| Scheduling | `runtime/trace`, to see writers blocking and GC pauses |
| Latency | per-operation histograms, reported as percentiles |
| Storage | on-disk store size before and after, and its growth |
| Coverage | `go build -cover` + `GOCOVERDIR`, reported with `go tool covdata` |

## Running it

```
go run ./examples/37_mvcc_write_contention \
    -producers 8 -ops-per-producer 200 -customers 1000 -readers 4 \
    -profile-dir /tmp/ex37 -trace /tmp/ex37/trace.out
```

Bare lines carry **deterministic** facts — invariant verdicts and counts that hold on
any machine, and which `example_test.go` asserts. Lines prefixed with `# ` carry
**volatile** telemetry that varies per run and per machine, and which no test pins: a
gate that asserted on throughput would fail on a loaded machine and teach the next
reader to ignore a red test.

A representative run (Apple M4, 10 cores):

```
## phase 1 — writer scaling
# scaling writers=1  commits=1600 commits_per_sec=834891  ratio_vs_1=1.00x
# scaling writers=2  commits=1600 commits_per_sec=1109313 ratio_vs_1=1.33x
# scaling writers=8  commits=1600 commits_per_sec=1675540 ratio_vs_1=2.01x
scaling.unrecovered_conflicts=0
## phase 2 — contention, reader latency, version retention
# contention commits=1600 hot_orders=410 hot_pct_actual=25.6
# contention retried_orders=20 retry_succeeded=20 unrecovered=0
# conflicts.total=20 aborts=20 commits=2604
# conflicts.by_store.node properties=20
# versions.samples=25 versions.max_retained=2331 versions.bound=4096 versions.ceiling=16384 max_chain_depth=10 max_concurrent_writers=8
# reader.samples=8606 reader.p50=416ns reader.p95=1.083µs reader.p99=10.208µs
contention.unrecovered_conflicts=0
versions.sampled=true
versions.writer_peak_consistent=true
## phase 3 — conservation under concurrent transfers
# conservation transfers=1483 refused=0 observations=25842
conservation.torn_observations=0
conservation.final_total_correct=true
## phase 4 — restart: the data and the MVCC clock
# restart nodes=200 store_bytes=25536 clock_before=200 clock_after_reopen=400 clock_after_write=401
restart.all_nodes_recovered=true
restart.clock_not_rewound=true
restart.post_restart_instant_is_new=true
```

Reading it against the six questions: writers scale (2.01× at eight, on a workload
where a quarter of orders contend); contention costs 20 conflicts over 2604
transactions, every one recovered by the retry loop and every one attributed to the
node-property store; readers see p99 10.2 µs *beside* the writers; versioning
retained 1854 versions against a 4096 bound with a deepest chain of 7; and the clock
came back at 400 after a restart, above the 200 the previous process published.

## Validated in both directions — and it caught a real defect

The check earned its place before the example was finished.

The first draft read balances with the present-time `g.GetNodeProperty` inside the
transfer, rather than through the transaction's own view — a textbook **lost update**:
two transfers read the same balance, both write, and the second silently overwrites
the first. The conservation invariant reported it immediately:

```
# conservation transfers=221 refused=0 observations=109
conservation.torn_observations=97
conservation.final_total_correct=false
# conservation.first_torn_total=15999019 want=16000000
```

981 cents gone, from a run that reported no errors and no refused transactions. The
check caught the **example's own** bug before it could be mistaken for the engine's,
which is exactly what a self-contradictory check is for: no external oracle was
consulted, and none was needed, because the two halves of one transaction must agree
with each other.

`TestConservationCheckCanFail` keeps that ability under test. It injects the same
shape into a real graph — a debit with no matching credit — and asserts the real
observer reports it, with a positive arm that fails if the observer is simply wrong
about an untouched fixture. Asserting the arithmetic instead would be a tautology
that passes however broken the observer became.

## The version sampler could not answer question 5, and said nothing about it

Question 5 — *what does versioning retain?* — is answered by sampling
`MVCCStats` while the workload runs. The sampler ticked every **2 ms**, and phase 2
takes **2 ms**. It therefore got between zero and one observation, and published
whichever it happened to get.

Measured over twelve runs of the documented default shape above:

| runs | `max_retained` | `max_chain_depth` | `max_concurrent_writers` |
|---|---|---|---|
| **7 of 12** | **0** | **0** | **0** |
| 5 of 12 | 568 – 3983 | 0, 5, 5, 5, 20 | 0, 2, 5, 8 |

Every one of the seven zero runs had between 18 and 192 conflicts. A write-write
conflict *is* two writers overlapping, so `max_concurrent_writers=0` beside a
non-zero conflict count is a self-contradiction: all three zeros were a sampling
miss published as a measurement. The representative run in this README — 1854
retained, depth 7 — was one draw from that distribution, not a typical result.

This is the defect class the project keeps rediscovering, and the one the rest of
this example already defends against: `contention.readers_sampled` and
`conservation.observed_any` exist precisely so a phase that observed nothing cannot
pass vacuously. The version sampler had no such guard.

Three changes, and the third is the one that matters:

1. the tick is **100 µs**, so a 2 ms phase yields ~20 observations rather than one;
2. the phase is **bracketed** — one sample taken on the calling goroutine before the
   writers start, one after they stop — so even a phase shorter than a tick observes
   something;
3. `versions.sampled` and `versions.writer_peak_consistent` are **deterministic fact
   lines the gate asserts**, so a miss now fails the test instead of printing zeros.

After the change, twelve runs of the same shape sampled 17–65 times each, reported
`max_concurrent_writers=8` — the true producer count — in **all twelve**, and gave
`max_retained` in a 2278–4023 band with a non-zero chain depth in nine. The guard is
validated in the failing direction too: with the sampler's tick set beyond the run's
lifetime and the brackets removed, `TestRun` fails on the missing
`versions.sampled=true` line rather than passing on zeros.

Note that `max_retained` may legitimately exceed `versions.bound`. `Bound` is the
settled-churn threshold at which the vacuum is woken, not a cap; `Ceiling` (16384) is
the instantaneous bound. A maximum-contention run measured 10184 retained — above the
bound, well under the ceiling, which is exactly the documented catch-up state.

## Examples 35 and 36 under non-serialising writers

Both still pass, and both remain **meaningful** now that writers overlap rather than
serialise — verified rather than assumed:

- **36** brackets every observation between the ingest's acknowledged-commit counter
  before and after the query, which is precisely a construction that does not assume
  serialisation, and it refuses to report success when `observations == 0` or when
  the self-contradiction query never ran. Its verdicts are unaffected.
- **35** measures throughput collapse and latency amplification — ratios whose whole
  subject is concurrency — and guards the zero-sample case. Nothing in it assumed a
  single writer.

Neither needed a change. What they did need was checking, because a verdict that was
sound under exclusion is not automatically sound without it.

## The zero-unrecovered checks

`scaling.levels` printed the length of a constant slice and `contention.accounted`
compared `committed + unrecovered` with a total the loop makes true by construction;
neither could fail. Both are replaced by `scaling.unrecovered_conflicts=0` and
`contention.unrecovered_conflicts=0`: the number of orders that did not commit,
computed as the target minus the commits. An order is unrecovered when its retry
budget (2 s of conflicts) expires, or when the phase's **hang budget** expires before
it runs. The hang budget is 30 s per scaling level and for the contention phase,
three to four orders of magnitude above a healthy phase, so a stuck or livelocked
writer ends the phase with a non-zero count instead of hanging the run.

## Phase 5 — deterministic MVCC scenario catalogue (rmp #2933)

`catalogue.go` ports the write-write (§1.1) and snapshot-isolation (§1.2) rows of
`docs/mvcc-scenario-catalogue.md`, `catalogue_mgix.go` its MERGE/UNIQUE (§1.3) and
index (§1.4) rows, `catalogue_ddhz.go` its DDL-vs-DML (§1.5), abort (§1.6) and
horizon, read-only-transaction and session (§1.7) rows, and `catalogue_rigg.go` its
referential-integrity and graph-shape (§1.8) rows and the frontier proposals FP01-FP03
(§5), from PostgreSQL's isolation specs and InnoDB's tests. The PostgreSQL scenarios are re-implemented, not copied. Each row is an
`internal/isolationtest` spec run over **every** order-preserving interleaving of its
steps (or over named interleavings where the catalogue requires them) and pinned by a
golden transcript. The transcript ends with a `final` block — the state the
permutation left behind — added to the harness as `Spec.Final` for this catalogue.

The example prints one deterministic line per scenario:

```
catalogue.WW02.cypher spec=ww02-stale-snapshot-write permutations=6 steps=30 ok=18 conflicts=6 poisoned=6 other_errors=0 blocked=0 violations=0
```

`steps` counts every observed step, `final` included; `conflicts` are
`mvcc.ErrSerializationConflict`; `poisoned` are `cypher.ErrTxPoisoned`; `blocked`
counts `<waiting ...>`, invalid-permutation and never-completed lines, and must be 0
(GoGraph takes no DML locks, F7); `violations` counts property-check failures. The
summary lines `catalogue.scenarios`, `catalogue.blocked_steps=0` and
`catalogue.violations=0` are asserted by `TestRun`; every transcript is asserted
against its golden by `TestCatalogue`.

### Harness

- **Two drivers (H3).** Every row runs through `cypher.Engine`. Where the shape
  exists in the `lpg` API — property reads and writes, label removal and property
  delete inside `Session.BeginVersionedTx` — a second spec (`-lpg` suffix) drives the
  same graph through `lpg.Graph`, using the harness's `Probe` step (a Go step that
  reports rows; added to `internal/isolationtest` for this purpose). Rows that need
  `ROLLBACK` run through Cypher only: the undo log a rollback replays belongs to the
  Cypher engine; an abort through `lpg` (`WriteTx.Abandon`, then `EndVersionedTx`) is
  used where no eager write has to be undone (HZ03). The DD rows, AB01, AB02, AB04 and
  AB05 run through Cypher only: the `lpg` API has no DDL, no constraint registry, no
  statement that can fail midway and no undo of eager writes. The MG and IX rows run
  through Cypher only: the `lpg` API has no
  `MERGE`, the UNIQUE and NOT NULL constraints are the Cypher engine's registry, and a
  secondary index is maintained only by the change fan-out the Cypher engine drives at
  commit (`lpg.ErrIndexedRawWrite`), so an `lpg` arm would test that contract rather
  than the row.
- **Vacuum drain (H1).** A `Hook` step calls `lpg.Graph.ReclaimNow`. Every
  write-after-rollback row places it in the rolling-back session directly after the
  `ROLLBACK`, and runs only the interleavings in which no other step falls between
  the two, so the drain is at a fixed point in every run.
- **Goldens** live in `internal/isolationtest/testdata`. That is decided by the
  harness: `isolationtest.Check` resolves `testdata/<spec>.golden` against its own
  source directory. Catalogue goldens carry the row ID as a prefix
  (`ww02-…`, `sk12-…`, `mg02-…`, `ix04-…`).
- **Property checks.** WW15 (pinned reader byte-identical), SK10 (seek = scan),
  SK12 (repeatable count; read-skew freedom), SK14 (fast path = scan), every IX row,
  DD01-DD05, DD09, AB02 and AB05 (seek = scan), AB04 (final value = acknowledged
  increments), HZ01 (versions rise while the reader is held and fall after), RO01
  (repeatable read) and RO03 (parallel count = scan) also assert a property on every
  step, independently of the golden. Every RI and GG row asserts no dangling edge; GG01,
  GG03 and GG04 also assert repeatable reads, and GG03 and GG04 seek = scan; FP02
  asserts that its background wait was parked behind the held straggler.
- **Go steps that drive the engine (§1.5-§1.7).** A sessionless read (SE01, SE03), a
  statement cancelled while it executes (AB02: the statement parks in a procedure,
  `cat.hold`, registered on every engine, and is cancelled there), a client retry
  loop (AB04), and `MVCCStats` readings taken after a vacuum drain (HZ01, HZ04, SE03)
  reach the engine of the permutation in force through `world.eng` and `world.g`.
- **The dangling-edge check (§1.8).** Every RI and GG row ends with a `dangling` probe:
  at a fresh snapshot it walks every interned node through the `lpg` API and counts the
  arcs whose source or destination is not alive there, plus the in-neighbour entries of
  dead nodes. Every RI and GG scenario carries the `noDanglingEdge` property, so an edge
  to a dead node committed in any permutation fails the run (catalogue §9 point 3). The
  probe reads the adjacency below Cypher on purpose: a Cypher pattern binds only live
  nodes, so a dangling arc would vanish from a `MATCH` rather than show in it.
- **Drivers for §1.8.** The `lpg` arms use `AddEdge`, an `lpg` spelling of `DETACH
  DELETE` (`RemoveEdge` of each in-arc, `RemoveAllEdgesFrom`, `RemoveNode`), and the
  by-handle surfaces `SetEdgePropertyByHandle` and `RemoveEdgeByHandle`. GG01
  (variable-length traversal), GG03 (degree and relationship count store) and GG05
  (delete everything) are Cypher only: the `lpg` API has no traversal operator, no
  relationship count store and no bulk delete.
- **The commit hold (FP01-FP03).** `lpg.Graph.AllocateCommitTS` reserves a
  transaction's commit instant without publishing it — the first half of the WAL commit
  path, which allocates before its fsync and publishes after it. Called on an in-memory
  graph, with the transaction's `EndVersionedTx` in a later step, it holds a commit
  between allocation and publication at a fixed point of the interleaving. The
  straggler is therefore always an `lpg` transaction; the later commits and the reads
  run through either driver.
- **COMMIT of a poisoned transaction.** The engine refuses it with `ErrTxPoisoned` and
  leaves the transaction open for the caller's `ROLLBACK`. The harness's COMMIT
  control now performs that rollback and still reports the COMMIT's error
  (`internal/isolationtest/runner.go`; `TestCommitOfPoisonedTxRollsItBack` fails
  without it: the peer's later write is refused by the leaked transaction). One golden
  depended on the leak: four permutations of `ww10-crossing-writes`, in which one
  transaction is poisoned and COMMITs before the other writes the node it held. The
  other transaction's write now succeeds and commits, as snapshot isolation allows
  after an abort; before, the leaked transaction refused it.
- **Seek = scan in one statement (IX).** Each index read returns two columns: `seek`,
  the predicate served from the index, and `scan`, the same predicate spelled so no
  index can serve it (`m.s + ''`, `m.k + 0`, `left(m.s, n)`). The scan arm is an
  `OPTIONAL MATCH`: with a plain `MATCH`, a scan that finds nothing leaves no row to
  group, the statement returns no row, and a stale index entry would vanish from the
  transcript instead of disagreeing in it. `TestIXAccessPaths` proves that every seek
  arm is planned as an index access and every scan arm as a label scan. The planner
  estimates a btree or numeric predicate from the committed value domain and plans a
  label scan for a bound outside it, so the btree and numeric rows write values inside
  that domain (`'v999x'`, `k = 16`).

### Rows

Expected outcomes are snapshot isolation (F1), not serializable snapshot isolation:
GoGraph permits write skew and refuses a write-write conflict.

| Row | Spec (golden) | Driver | Source | Expected SI outcome | GoGraph outcome, and why it differs from the source |
|---|---|---|---|---|---|
| WW01 | `lost-update` (existing) | Cypher | PG `deadlock-simple`, `lock-committed-update`; MY `concurrent.inc` | Second writer refused | As expected. InnoDB REPEATABLE READ permits the lost update; SI cannot. |
| WW02 | `ww02-stale-snapshot-write`, `-lpg` | both | PG `lock-committed-update`, `lock-committed-keyupdate`, `eval-plan-qual` | Writer refused when the peer's version is in flight or committed after its snapshot | As expected. PG READ COMMITTED re-fetches and succeeds. |
| WW03 | `ww03-blind-increment`, `-lpg` | both | MY `innodb_bug52663`, `innodb_bug49164`; PG `eval-plan-qual` | Second increment refused; final 1 | As expected. InnoDB and PG READ COMMITTED reach 2 without an error. |
| WW04 | `ww04-update-delete-commit`, `ww04-update-rollback-then-delete`, `ww04-delete-rollback-then-update`, both rollback arms also `-explicit` | Cypher | PG `eval-plan-qual`, `lock-update-delete`, `merge-delete`; MY `concurrent.inc` | Later writer refused; allowed after the earlier rolls back and the vacuum drains | As expected. A `DETACH DELETE` that conflicts reports it at `COMMIT`, not at the statement; nothing is applied. Each rollback arm runs twice: with an autocommit second writer, and (`-explicit`) with an explicit transaction whose snapshot predates the rollback (D1, rmp #2973). |
| WW05 | `ww05-update-delete-chain-commit`, `ww05-update-delete-chain-rollback` | Cypher | PG `lock-update-delete`, `lock-update-traversal`, `aborted-keyrevoke` | Older-snapshot peer refused once the chain commits; allowed after the chain rolls back and the vacuum drains | As expected in both arms. The rollback arm is defect D1's shape and witnesses its fix (rmp #2973). |
| WW06 | `ww06-predicate-moved-out` | Cypher | PG `eval-plan-qual`, `merge-match-recheck`, `merge-update`; MY `innodb-semi-consistent` | Older snapshot still matches and is refused | As expected. PG READ COMMITTED and InnoDB's semi-consistent read skip the row instead. |
| WW07 | `ww07-non-matching-not-blocked` | Cypher | MY `innodb-semi-consistent`, `innodb-consistent` | No wait, no conflict | As expected. |
| WW08 | `ww08-different-properties-one-node`, `-lpg` | both | MY `concurrent.inc`; PG `update-locked-tuple` | Second refused: the node is the conflict unit | As expected; same verdict as PG and InnoDB row granularity. |
| WW09 | `ww09-disjoint-nodes`, `-lpg` | both | `docs/isolation-design.md` | Zero conflicts in every interleaving | As expected. |
| WW10 | `ww10-crossing-writes`, `-lpg` | both | PG `deadlock-simple`, `fk-deadlock`, `fk-deadlock2`; MY `deadlock_detect`, `innodb_deadlock` | No wait; refused instead | As expected; in some interleavings **both** transactions are refused, where PG and InnoDB pick one deadlock victim. When the refused transaction COMMITs (and is rolled back) before the other writes the node it held, that write succeeds. |
| WW11 | `ww11-ring-3`, `ww11-ring-8` | Cypher | PG `deadlock-hard`; MY `long_deadlock_cycle`, `undetected_deadlock`, `hp_deadlock` | No wait; each write succeeds or is refused; progress after rollback | As expected. Named interleavings (round-robin, sequential; staggered for 3). |
| WW12 | `ww12-write-after-peer-rollback`, `-explicit` | Cypher | PG `eval-plan-qual`, `eval-plan-qual-trigger`, `multixact-no-forget`; MY `innodb_mysql_rbk` | Write after rollback + drain succeeds | As expected, both for a writer whose snapshot follows the rollback and (`-explicit`) for an explicit transaction whose snapshot predates it (D1, rmp #2973). The no-drain arm is not pinned (a race, F5). |
| WW13 | `ww13-void-label-removal`, `-lpg`; `ww13-void-property-delete`, `-lpg`; `ww13-void-edge-property` | both | GoGraph rmp #2354 (no PG/InnoDB counterpart) | Statement OK, `COMMIT` refused, nothing applied | As expected for label removal and property delete, through both drivers. The **edge-property** write is refused at the statement, not at `COMMIT`: the catalogue's F3 premise (edge side stores record the conflict) does not hold for `SET r.w` on an existing edge property. The outcome is still a refusal that applies nothing. |
| WW14 | `ww14-chain-walk-in-flight-head` | Cypher | PG `update-conflict-out`, `multiple-row-versions`; MY `innodb-read-view`, `lob_mvcc_undo` | Pinned reader sees the original; a new reader sees the committed value, never the in-flight one | As expected. |
| WW15 | `ww15-long-chain-large-values` | Cypher | PG `multiple-row-versions`; MY `lob_mvcc_undo`, `lob_partial_update_concurrent` | Pinned reader returns the 64 KiB value of its BEGIN, byte-identical | As expected. The value is written inline in the `CREATE` map (rmp #2975, fixed). |
| SK01 | `write-skew` (existing) | Cypher | Berenson et al. A5B | Permitted | As expected. |
| SK02 | `sk02-swap-write-skew` | Cypher | PG `simple-write-skew` | Permitted: both commit, sets swapped | As expected. PG SERIALIZABLE aborts one. |
| SK03 | `sk03-booking-overlap` | Cypher | PG `classroom-scheduling`, `temporal-range-integrity` | Permitted: overlapping bookings | As expected. |
| SK04 | `sk04-cross-label-write-skew` | Cypher | PG `project-manager` | Permitted | As expected. |
| SK05 | `sk05-total-cash`, `-lpg` | both | PG `total-cash` | Permitted: total −200 | As expected. |
| SK06 | `read-only-anomaly-named` (existing) | Cypher | PG `read-only-anomaly`, `read-only-anomaly-2` | Permitted | As expected; PG SERIALIZABLE aborts. |
| SK07 | `sk07-receipt-report` | Cypher | PG `receipt-report` | Permitted: the report misses a receipt that later commits into its batch | As expected, in two of the four named interleavings. The receipt's date is read inline in the `CREATE` map, as in the source spec; that read is the witness for D2 (rmp #2974, fixed). |
| SK08 | `sk08-two-ids` | Cypher | PG `two-ids` | All commit in every interleaving | As expected. |
| SK09 | `bank-transfer` (existing) | Cypher | examples/27 | Total constant | As expected. |
| SK10 | `sk10-write-skew-hash-seek`, `sk10-write-skew-btree-range` | Cypher | PG `index-only-scan`, `predicate-hash`, `insert-conflict-serializable`, `matview-write-skew` | Permitted; seek = scan in every step | As expected. 1 100 padding nodes make the planner seek; `TestSK10AccessPaths` proves one arm seeks and the other scans. The scan arm is an `OPTIONAL MATCH`, as in the IX rows, so an empty scan cannot hide a mismatch; the goldens changed only in the query text. |
| SK11 | `sk11-materialised-conflict-increment`, `sk11-materialised-conflict-value-preserving` | Cypher | Fekete et al., TODS 2005; MY `t/locking_clause` | Increment arm: second refused, rule holds | Increment arm as expected. **Value-preserving arm (`SET g.v = g.v`): both commit and the rule breaks** — a write of the current value writes no version and claims nothing, so it is not a remedy (G4 pinned). |
| SK12 | `sk12-no-phantom-no-non-repeatable`, `sk12-read-skew-lpg` | both | MY `t/consistent_snapshot`, `select_count_perf`, `innodb-read-view` | Repeated reads equal; no read skew | As expected. The `lpg` arm reads two keys (A5A) and is the negative control's target. |
| SK13 | `sk13-snapshot-at-begin`, `-lpg` | both | MY `t/consistent_snapshot`; PG `fk-snapshot` | Snapshot at BEGIN | As expected. InnoDB's plain `START TRANSACTION` takes it at the first read. |
| SK14 | `sk14-fast-path-aggregates` | Cypher | MY `select_count_perf`, `parallel_read`; PG `index-only-bitmapscan` | Fast path = scan, at the snapshot | As expected. |
| MG01 | `mg01-merge-no-constraint` | Cypher | PG `merge-insert-update` | Both succeed; duplicates when both snapshots precede both commits; one node when serial | As expected: 2 nodes in 18 of 20 interleavings, 1 in the 2 serial ones, where a `BEGIN` follows the other's `COMMIT`. |
| MG02 | `mg02-unique-merge-winner-commits`, `-winner-rolls-back`, `-autocommit-loser` | Cypher | PG `insert-conflict-do-nothing`, `insert-conflict-do-update`; MY `iodku`, `innodb_replace`, `constraint_check_locks_in_read_committed` | Loser gets a ConstraintViolation, not retriable (F10) | As expected in all three arms. PG waits and then does nothing or updates; InnoDB waits on a shared lock. After the winner rolls back, a new `MERGE` creates the node: no reservation leaks. The autocommit arm is refused too while the winner is open (G6); it matches the node once the winner has committed. |
| MG03 | `mg03-unique-committed-after-snapshot` | Cypher | PG `insert-conflict-do-nothing-2`, `read-write-unique-2` | `MERGE` cannot see the committed value and gets a ConstraintViolation | As expected. PG REPEATABLE READ raises a serialization failure instead. |
| MG04 | `mg04-read-then-insert-unique` | Cypher | PG `read-write-unique`, `-2`, `-3` | Second `CREATE` is a ConstraintViolation at the statement | As expected; exactly one node in all 20 interleavings. PG SERIALIZABLE waits, then raises a serialization failure. |
| MG05 | `mg05-gapless-sequence-no-constraint`, `-unique` | Cypher | PG `read-write-unique-4` | Without a constraint: duplicate numbers (write skew); with UNIQUE: loser refused | As expected: `[1, 2, 2]` in all 20 interleavings without the constraint, `[1, 2]` with it. |
| MG06 | `mg06-merge-on-match-vs-update` | Cypher | PG `insert-conflict-do-update`, `-3`, `merge-match-recheck`, `merge-update` | `ON MATCH SET` over a version the snapshot cannot see is refused (F2) | As expected: the later writer is refused at its statement in every order. PG `-3` updates a tuple its snapshot cannot see. |
| MG07 | `mg07-on-match-changes-unique-key` | Cypher | PG `insert-conflict-do-update-2` | One node per key; loser gets a typed error | As expected in all 30 interleavings; the release of the old key is deferred to commit, so a `CREATE` of it is refused while the renamer is open (rmp #2366). |
| MG08 | `mg08-merge-vs-delete` | Cypher | PG `merge-delete` | `MERGE` onto a node deleted after its snapshot is refused; no resurrection | As expected. The refusal of the `DETACH DELETE` that comes second surfaces at its `COMMIT`. PG READ COMMITTED turns the `MERGE` into an `INSERT`. |
| MG09 | `mg09-delete-recreate-same-tx`, `mg09-delete-commit-then-recreate`, `mg09-delete-open-then-rollback` | Cypher | MY `innodb-lock-inherit-read_commited`, `index-create-dml-rollback`, `lock-inherit-existing`; PG `read-write-unique-3` | (a) one holder; (b) re-create succeeds after the delete commits; (c) refused while the delete is open, one holder after its rollback | As expected in all three arms. In (b) the re-create succeeds once the delete has committed even when the creator's snapshot still sees the old node: the check reads the reservation set, not the snapshot. |
| MG10 | `mg10-intra-statement-duplicate` | Cypher | MY `innodb-index`, `create_table_select` | Statement rejected, nothing applied | As expected. Between the failed statement and the client's `ROLLBACK` (F8) the value stays reserved and a peer is refused (G6); after the `ROLLBACK` the peer succeeds. |
| MG11 | — | — | PG `insert-conflict-specconflict`, PG-inj `on_conflict_probe_window` | One node; loser ConstraintViolation | **Not implemented: random load, #2934.** No hook exists inside `MERGE` between its probe and its create (G5), so the race cannot be scripted. |
| MG12 | `mg12-not-null-at-commit`, `mg12-not-null-peer-removal` | Cypher | PG `alter-table-1`; MY `constraint_check_locks_in_read_committed` | Set-later commits; removed arm refused at `COMMIT`; peer removal refused | As expected. A peer's `REMOVE n.p` is refused by the write-write conflict at `COMMIT` when the setter committed first, and by the constraint at `COMMIT` otherwise; `p` is never null after a commit. |
| IX01 | `ix01-own-write-hash-seek` | Cypher | GoGraph #2814 (`efd32fb9`); MY `innodb_fts/transaction` | Own writes visible to the hash seek; seek = scan | As expected. InnoDB's full-text index hides own uncommitted rows by design. Negative control below. |
| IX02 | `ix02-own-write-btree-seek` | Cypher | GoGraph #2814 | Range and prefix seek = scan after own writes | As expected. Negative control below. |
| IX03 | `ix03-own-write-autocommit` | Cypher | GoGraph #2814 | One autocommit statement counts its own `CREATE` and `SET` | As expected. Negative control below. |
| IX04 | `ix04-peer-rollback-label-add-hash`, `-btree` | Cypher | GoGraph #2931; MY `index-create-dml-rollback`, `innodb-index-online`; PG `partial-index` | Node indexed under its committed value; seek = scan | As expected. Negative control below. Without a UNIQUE constraint the label add does not conflict with the open property write on the same node, in either order. |
| IX05 | `ix05-rollback-leaves-no-trace` | Cypher | MY `innodb-index-online`, `index-create-dml-rollback`, `lob_rollback_update`, `innodb_mysql_rbk`; PG `partial-index` | Every seek = scan = pre-state plus the peer's commits | As expected. With a UNIQUE constraint registered (on any label), a label add and a property write on one node conflict ("node constraint"), so the peer route by which #2931 let a rolled-back value into an index cannot occur here; the row does not fail at `43c69dbe` (see the negative control). |
| IX06 | `ix06-moved-out-of-index-domain` | Cypher | PG `partial-index`, `partition-key-update-4`; MY `multi_value_index_merge_mvcc` | Pinned reader finds the old values; a new reader the new ones | As expected. |
| IX07 | `ix07-two-index-predicate-pinned` | Cypher | MY `multi_value_index_merge_mvcc`, `bug32554667` | Pinned reader gains no row | As expected. |
| IX08 | `ix08-index-created-after-snapshot` | Cypher | PG `drop-index-concurrently-1`, `reindex-concurrently`; MY `innodb-read-view` | Only snapshot rows | As expected; `CREATE INDEX` does not wait for the open read-only transaction. |
| IX09 | `ix09-label-index-scan-agree` | Cypher | GoGraph #2931 churn; MY `lock_impl_to_expl_case_sensitivity` | Label count = seek = scan at quiescence | As expected. |
| IX10 | `ix10-parameter-seek-own-write` | Cypher | GoGraph `3fd78c5e` | Parameter seek = literal seek = scan | As expected, for a string and an integer parameter. |

| DD01 | `dd01-create-index-under-open-writer-commit`, `-rollback` | Cypher | PG `multiple-cic`, amcheck `t/002_cic.pl`; MY `innodb-index-online`, `innodb-table-online`, `innodb-index-online-delete`, `bulk_create_index_online`, `index-create-dml-rollback` | Index holds the committed arm, not the rolled-back one; seek = scan | As expected in all 20 interleavings of each arm. **G9 settled:** the backfill reads a committed snapshot (negative control below). |
| DD02 | `dd02-two-indexes-writer-between` | Cypher | PG `multiple-cic` | Both indexes seek = scan | As expected. |
| DD03 | `dd03-drop-index-between-seeks` | Cypher | PG `drop-index-concurrently-1`, `vacuum-concurrent-drop` | A read after the drop scans, with the same rows; no error | As expected; `DROP INDEX` does not wait for the open transaction. |
| DD04 | `dd04-drop-recreate-index-with-writers` | Cypher | PG `reindex-concurrently` | Rebuilt index holds every commit | As expected. |
| DD05 | `dd05-unique-under-open-duplicate-commit`, `-rollback` | Cypher | PG `alter-table-1`; MY `alter_table_rebuild_duplicate_record`, `index-create-dml-rollback`, `innodb-alter-debug` | No committed state violates the constraint | As expected. The uncommitted duplicate never refuses the DDL. The DDL is refused only when the duplicate committed first; otherwise s1 is refused, at its statement (DDL first) or at its COMMIT (the DDL straddles it, rmp #2936). |
| DD06 | `dd06-dml-not-blocked-by-idle-tx` (DML half), `dd06-ddl-bounded-by-context` (DDL half) | Cypher | PG `timeouts`, `truncate-conflict`; MY `innodb-timeout`, `innodb_lock_wait_timeout_1`, `innodb-lock` | DML never waits on an idle open transaction; a DDL behind an in-flight statement returns its context error within the deadline | As expected in both halves. The DDL half parks an autocommit write in `cat.hold` and runs each DDL kind (CREATE and DROP INDEX, hash and btree; CREATE and DROP CONSTRAINT, UNIQUE and NOT NULL) with a 50 ms deadline: each returns `context deadline exceeded` while the write is still parked, the schema is unchanged before and after the release, and every retry succeeds. It witnesses the fix of D3 (rmp #2982). |
| DD07 | `dd07-ddl-in-write-tx`, `dd07-ddl-in-read-tx` | Cypher | MY `t/implicit_commit`, `t/trans_read_only` | DDL rejected (F9); the transaction stays usable | As expected. The rejection does **not** poison the write transaction: its COMMIT succeeds with its other writes. MySQL commits the transaction implicitly instead. |
| DD08 | `dd08-ddl-on-one-object` | Cypher | PG `ddl-dependency-locking` | Consistent catalog; the losing DDL gets a typed error | As expected in all 24 orders. The harness runs one step at a time, so the overlap of the DDL statements is #2934's. |
| DD09 | `dd09-create-index-vs-vacuum` | Cypher | MY `alter_table_rebuild_missing_record`, `innodb-index-online-purge`, `virtual_debug_purge` | Seek = scan; no lost node | As expected with the drain between steps. A drain inside the backfill needs a seam that does not exist; that point is #2934's. |
| AB01 | `ab01-failed-statement-commit-then-retry`, `-rollback-then-retry` | Cypher | PG `delete-abort-savept`, `-2`, `aborted-keyrevoke`; MY `t/func_rollback`, `innodb_mysql_rbk` | COMMIT is `ErrTxPoisoned`; nothing applied; the retry commits | As expected. |
| AB02 | `ab02-autocommit-statement-fails-midway` | Cypher | MY `t/func_rollback`, `t/kill` | None applied, indexes clean | As expected for the failing statement and for the statement cancelled while it executes. |
| AB03 | `ab03-refused-statement-then-continue`, `-lpg` | both | MY `t/innodb_deadlock`, `innodb_mysql_rbk` | Reads keep the snapshot; COMMIT applies nothing | As expected. **G10 pinned:** through Cypher the COMMIT returns `ErrTxPoisoned`, through `lpg` the serialization conflict. |
| AB04 | `ab04-retry-loop-converges` | Cypher | `docs/isolation-design.md:141-150` | Final = acknowledged increments | As expected. A client whose peer still holds the node is refused on every retry and gives up after three attempts; the final value still equals the acknowledged count. |
| AB05 | `ab05-rollback-of-created-subgraph` | Cypher | MY `lob_big_rollback`, `zlob_big_rollback`, `undo_log_temp_table` | Invisible; counts and UNIQUE set restored | As expected. While s1 is open its UNIQUE value refuses the peer (G6). |
| AB06 | `ab06-create-delete-recreate-commit`, `-rollback`; `-commit-lpg` | both | GoGraph `graph/lpg/mvcc_life_test.go:149-265` | Commit: visible once; rollback: never visible | As expected. The `lpg` driver runs the commit arm only (no undo of eager writes). |
| HZ01 | `hz01-long-reader-retains-versions`, `-lpg` | both | PG `horizons`, `vacuum-no-cleanup-lock`; MY `bug120529`, `purge_on_replica`, `flush-hang`, `lob_purge` | Versions rise while held, fall after | As expected: 0, then 10 while the reader is held, then 0 after it ends (each reading after a drain). One named interleaving. |
| HZ02 | `hz02-autocommit-read-releases-slot`, `-lpg` | both | MY `innodb-ac-non-locking-select`, `innodb_i_s_innodb_trx`; PG `horizons` | 0 after an autocommit read; 1 while a read transaction is open | As expected. |
| HZ03 | `hz03-aborted-versions-withdrawn`, `-lpg` | both | GoGraph `docs/design-mvcc-abort-withdrawal.md`; MY `lob_rollback_update`, `zlob_rollback_update` | Reader sees the pre-state; writer succeeds after the drain | As expected; the writer is refused while the aborting transaction is still open. |
| HZ04 | `hz04-vacuum-during-drop-index` | Cypher | PG `vacuum-concurrent-drop`; MY `innodb_bug26818787` | No error; backlog drains to 0 | As expected: backlog 10 while the reader is pinned, 0 after it ends. Three named interleavings. |
| RO01 | `ro01-read-only-tx-stable-and-rejects-writes`; `ro01-read-only-snapshot-stable-lpg` | both | MY `t/trans_read_only`; PG `read-only-anomaly-3` | Stable reads; `ErrWriteInReadOnlyTx` | As expected. The `lpg` driver runs the stable-snapshot half: an `lpg` read snapshot has no write surface. |
| RO02 | `ro02-read-view-before-two-writers`, `-lpg` | both | MY `innodb-read-view` | The reader sees neither commit; a new reader sees both | As expected in all 90 interleavings. |
| RO03 | `ro03-read-only-tx-parallel-count` | Cypher | PG `serializable-parallel` | Same answer as the serial path | As expected. The engine's `ParallelScanThreshold` is lowered to 2 048 over 3 000 nodes; `TestRO03TakesTheParallelPath` proves the count is planned as `ParallelCountScan`. |
| SE01 | `se01-session-reads-own-commit` | Cypher | `docs/isolation-design.md:236-248`; PG-rec `057_snapshot_commit_race` | The session sees its commit | As expected. In a serial script the frontier is always contiguous, so the sessionless reader sees it too; the case where they differ needs SE02's straggler. |
| SE02 | — | — | PG-rec `057_snapshot_commit_race`, PG-inj `repack_commit_race`; MY `binlog_gtid/binlog_group_commit_gtid_order`, `binlog/binlog_after_commit_order_info_schema`, `hp_deadlock` | Sessionless read excludes T1; session read waits for T0 | **Not implemented: random load, #2934.** No exported commit-hold seam (G1, H2 below). |
| SE03 | `se03-abandoned-commit-does-not-stall-frontier` (NOT NULL arm) | Cypher | `docs/isolation-design.md:389-395`; MY `binlog/binlog_group_commit_flush_crash` | T1 visible to a sessionless reader; `InFlightCommits` returns to 0 | As expected. The injected-fsync arm needs H2 and is **random load, #2934**. |
| SE04 | — | — | `docs/isolation-design.md:141-150,199-213` | Sessionless self-write may be refused; session write is not | **Not implemented: random load, #2934.** It is SE02 plus a rewrite, so it needs the same seam. |
| RI01 | `ri01-edge-create-vs-endpoint-delete`, `-lpg` | both | PG `fk-snapshot-2`, `fk-concurrent-pk-upd`, `fk-partitioned-1`, PG-inj `ri_fastpath_snapshot` | Older-snapshot edge create refused after the endpoint delete commits | As expected in all 6 interleavings, through both drivers. When the edge create runs first, the Cypher `DETACH DELETE` returns its row and is refused at `COMMIT`; the `lpg` removal is refused at the statement. No dangling edge. |
| RI02 | `ri02-edge-into-deleted-node`, `ri02-edge-out-of-deleted-node`, both `-lpg` | both | PG `fk-snapshot`, `fk-partitioned-1`; MY `update-cascade` | Later writer refused; no edge to a dead node | As expected for both endpoints, in every order. |
| RI03 | `ri03-hub-same-target`, `ri03-hub-same-source`, both `-lpg` | both | PG `fk-contention`; MY `innodb_cats` | **G8**: the code says the second appender is refused; a comment said appends never conflict | **Second appender refused**, same target and same source, in every order, through both drivers: the code is right and the comment was stale (G8 below). PG's foreign-key contention waits instead. |
| RI04 | `ri04-application-check-vs-edge-insert`, `-lpg` | both | PG `referential-integrity`, `ri-trigger`, `temporal-range-integrity` | One refused; no orphan | As expected. The application check (`WHERE NOT ()-->(b)`) passes at the deleter's snapshot, and the native edge RI still refuses one of the two. PG without a foreign key permits the orphan. The `lpg` arm reads `InNeighbours` in the transaction's view. |
| RI05 | `ri05-edge-to-parent-then-update-parent`, `-lpg` | both | PG `fk-deadlock`, `fk-deadlock2` | Second refused at once, no wait | As expected in all 20 interleavings: the second writer is refused at its edge create, before its `SET`; the parent holds the committed writer's value. |
| RI06 | `ri06-same-edge-property`, `ri06-parallel-edges-disjoint`, both `-lpg` | both | GoGraph `graph/lpg/mvcc_conflict_stores_test.go` | Same edge: second refused. Parallel edges: no conflict | Same edge: as expected; Cypher refuses at the statement, `lpg` (`SetEdgePropertyByHandle`, a void primitive) at `COMMIT`. **Parallel edges: the drivers differ.** `lpg` commits both. Cypher refuses the second: its `SET r.w` also writes the per-pair property column (`lpg.Graph.SetEdgeProperty`, `cypher/exec`), a copy-on-write of the source node's adjacency entry, which claims that node (F4). The refusal applies nothing, so it is a false conflict, not an isolation defect. |
| GG01 | `gg01-traversal-at-pinned-snapshot` | Cypher | GoGraph-specific | Both pinned counts equal the snapshot's | As expected: 7 paths at the snapshot in all 10 interleavings, 4 after the churn. |
| GG02 | `gg02-detach-delete-hub-vs-edge-create`, `-lpg` | both | GoGraph-specific (#2725, #2694) | One refused; no dangling edge; counts consistent | As expected. The relationship count agrees with the surviving arcs. |
| GG03 | `gg03-degree-and-count-at-snapshot` | Cypher | GoGraph-specific (#2081) | Degree and count equal the snapshot's | As expected in all 21 interleavings; the count-store answer equals a typed scan in every read. |
| GG04 | `gg04-label-churn-at-pinned-snapshot`, `-lpg` | both | GoGraph-specific (#2687) | Pinned reader unchanged | As expected: 10 labelled nodes at the snapshot in every interleaving; label store = label scan. |
| GG05 | `gg05-delete-everything-vs-insert` | Cypher | PG `truncate-conflict` (adapted) | The new node survives | As expected in every order. |
| GG06 | `gg06-delete-disjoint-parallel-edges`, `gg06-delete-self-loop-and-parallel-edge`, both `-lpg`; `gg06-delete-same-edge-instance-lpg` | both | GoGraph #2018 | Different instances commit; same instance: second refused | Same instance through `lpg`: as expected. **Different instances: the second deleter is refused** (at `COMMIT` through Cypher, at the statement through `lpg`): every instance has source `a`, and an arc removal claims its source node exclusively (F4). The refusal applies nothing. **The Cypher same-instance arm is not in the catalogue: it exposes defect D4 below**, and a golden would pin it as correct. |
| FP01 | `fp01-later-commits-ack-behind-held-straggler`, `-lpg` | both | GoGraph #2932 (catalogue §5) | 2 048 later commits acknowledge while one commit is held; one-step catch-up | As expected: 8 goroutines × 256 commits all return while s0 is held (no `<waiting ...>`), none is visible to a sessionless reader, `InFlightCommits` is 2 049, and all 2 048 become visible together when s0 publishes. The control interleaving runs the fan before the hold. |
| FP02 | `fp02-straggler-abandons-lpg` | lpg | GoGraph (catalogue §5); MY `binlog/binlog_group_commit_flush_crash` | Frontier catches up; the waiting session is released | As expected: the session wait is parked behind the held straggler (`SessionsWaiting` = 1); when the straggler abandons (`WriteTx.Abandon`, the fsync-failure path), the wait returns with no error, t1 is visible, s's write is not, and no commit is in flight. Through `lpg` only: the hold is an `lpg` transaction and Cypher has no abandon of an allocated commit. |
| FP03 | `fp03-session-read-bounded-by-context`, `fp03-session-wait-bounded-by-context-lpg` | both | GoGraph (catalogue §5); CLAUDE.md context-aware blocking | A session read behind a held straggler returns the context error | As expected: with a 50 ms deadline, `cypher.Session.RunInTx` and `lpg.Session.Await` return `context deadline exceeded`; after the straggler publishes the same read returns t1. |

### Negative control

`world.perKeySnapshot` (example code, set only by `TestReadSkewNegativeControl`)
makes every `lpg`-driver read take a fresh snapshot — each key read at its own
instant. With it, `sk12-read-skew-lpg` reports the anomaly; without it, none:

```
permutation "s1x s2dx s2cy s2c s1y s1x2 s1c" step s1y: read skew: x=50 y=60 sum to 110, want 100
```

Run against the golden with the seam on, the same spec fails on twelve property
violations and on a transcript diff; with the seam off it passes.

### Negative controls for the index rows

Each spec and golden at HEAD was copied into a temporary worktree of an earlier
revision and run there. The worktree needed one change to compile:
`lpgSession.removeLabel` and `lpgSession.delProp` wrap `RemoveNodeLabel` and
`DelNodeProperty`, which returned no error then; no MG or IX spec uses either.

- **#2814 at `efd32fb9^`.** IX01, IX02 and IX03 all fail. IX01 reports 21 property
  violations, for example
  `permutation "s1w s1new s1old s1cr s1crr s1c s2r" step s1old: index seek counted 1, scan counted 0`;
  IX02 reports 32, for example `step s1rg: index seek counted 1, scan counted 2`; IX03
  diverges from its golden at line 11 of permutation `s1cr s1set s2r` (`want: 1`,
  `got: 0`).
- **#2931 at `43c69dbe`.** Both IX04 specs fail, 18 property violations and a diff at
  line 59 of permutation `s1w s2l s1rb s1dr s2v s2p` (hash: `want: 1 |1`,
  `got: 0 |1`; btree: `want: 11 |11`, `got: 10 |11`). **IX05 passes there.** A
  rolled-back transaction writes no index entry of its own, and the peer route #2931
  took is closed in IX05 by the "node constraint" conflict that any registered UNIQUE
  constraint brings. The catalogue's §6 named IX05 as a row that catches #2931; it
  does not, and §6 now names IX04 and says why IX05 does not.

### Negative control for G9 (DD01)

A test-only mutant, applied with `go test -overlay` and never written to the tree,
makes `Engine.beginIndexBuild` (`cypher/index_binding.go`) hand the backfill the live
graph, `e.g.ReadAt(nil)`, instead of the snapshot it opens. Against it both DD01
specs and DD09 fail: the rollback arm with 36 property violations, the commit arm
with 11, DD09 with 8, each also diverging from its golden. For example:

```
permutation "s1w s1cr s2ix s1rb s2x s2v7" step fx: index seek counted 1, scan counted 0
permutation "s1w s1cr s2ix s1rb s2x s2v7" step fv7: index seek counted 0, scan counted 1
```

The index kept n7 under s1's rolled-back 'x' and lost it under its committed 'v7'.
Without the mutant all three pass.

### Negative control for the dangling-edge check (RI, GG)

A test-only mutant, applied with `go test -overlay` and never written to the tree,
makes `Graph.appendEdgeInfo` (`graph/lpg/lpg.go`) skip all three destination tests of
an edge create: the destination is not claimed, its existence head is not tested when
it is interned, and its existence is not cross-checked after the insert. Against it
RI01, RI02 (into), RI04 and GG02 fail through both drivers, each with 3 property
violations — the 3 interleavings in which the delete runs first — for example:

```
permutation "s1d s1c s2e s2c" step dangling: an edge to or from a dead node was committed: [[0 2]]
```

RI03 (same target) and RI05 also fail, on their goldens: the second appender to a
shared target is no longer refused. Without the mutant every RI and GG row passes.

### Defects found

D1, D2, D3 and the first parser defect are fixed; the second parser defect and D4 are
open.

- **D1 — `ExplicitTx.Rollback` publishes its commit record (rmp #2973).** After a Cypher
  `ROLLBACK`, `MVCCStats().Write` counts one more commit and no abort, and a
  transaction whose snapshot predates the rollback is refused when it writes a node
  the rolled-back transaction touched — even after `ReclaimNow`. Snapshot isolation
  (and PostgreSQL, which lets the waiter proceed once the peer aborts) allows that
  write. The same shape through the `lpg` API (a doomed transaction ended with
  `EndVersionedTx`) is allowed. Reproduction: `CREATE (:Item {name:'n', x:0})`;
  T2 `BeginTx`; T1 `BeginTx`, `SET n.x = 1`, `Rollback`; `ReclaimNow`; T2
  `SET n.x = 2` → `mvcc: serialization conflict in node properties`. Expected: success.
  **Fixed:** every rollback path of an explicit transaction on which nothing is durable
  — `Rollback`, `Commit`'s refusals, a panic during `Exec` — marks the transaction with
  `lpg.WriteTx.Abandon`, so `EndVersionedTx` aborts it. The `-explicit` arms of WW04 and
  WW12 and the WW05 rollback arm pin it.
- **D2 — a property read inline in a `CREATE` map ignores the snapshot (rmp #2974).** Inside an
  explicit transaction, after a peer committed `c.date = 2` over the transaction's
  snapshot value 1, `MATCH (c:Control) CREATE (r:Receipt {date: c.date, amount: 4})
  RETURN r.date` stores and returns **2**, while `MATCH (c:Control) RETURN c.date`
  in the same transaction returns 1. `CREATE (r:Receipt {date: c.date}) RETURN
  r.date` also returns 2; binding the value first (`WITH c.date AS d CREATE …`) or
  `CREATE … SET r.date = c.date` returns 1. A read of data committed after the
  snapshot is a non-repeatable read inside one transaction. **Fixed (rmp #2974):**
  a bound entity read by a write clause's expression — `CREATE` and `MERGE` node and
  relationship maps, `ON CREATE SET`, `ON MATCH SET` — now resolves through the
  transaction's view. SK07 uses the inline spelling.
- **Parser (rmp #2975).** A `CREATE` map entry whose value contains a comma — `CREATE
  (:Doc {blob: reduce(s = 'z', i IN range(1, 16) | s + s)})` — failed with `parse
  properties … missing ':' in map item`. **Fixed:** the map-item splitter treats a
  parenthesised group as one item. WW15 uses the inline spelling.
- **Parser: `STARTS WITH` / `ENDS WITH` after a `WITH` clause (open).**
  `WITH 1 AS x MATCH (m:P) WHERE m.name STARTS WITH 'a' RETURN m.name` fails with
  `cypher: parse: unexpected "RETURN" at 1:53, expected one of {'WITH', 'UNION'}`
  through `Engine.Run`; `ENDS WITH` fails the same way, while `CONTAINS`, the same
  predicate in parentheses, and the same `MATCH … WHERE … STARTS WITH` without a
  preceding `WITH` parse. openCypher admits the statement. The IX prefix rows spell
  their scan arm with `left()` instead; the seek arm, which precedes the `WITH`, keeps
  `STARTS WITH`.
- **D3 — a DDL statement ignores its deadline while an autocommit write is in
  flight (rmp #2982).** Reproduction: register a procedure that blocks, start
  `CREATE (n:T {name:'t'}) WITH n CALL cat.hold() YIELD x RETURN x` through
  `Engine.RunInTx` on one goroutine, and once it is parked run
  `CREATE INDEX l_s2 FOR (n:L) ON (n.s)` through `Engine.RunInTx` with a 50 ms
  deadline. Observed: the DDL is still blocked after 1 s and returns
  `context deadline exceeded` only once the write statement is released. Expected:
  the DDL returns its context error within the deadline plus a margin (catalogue
  DD06; F7; the "Context-aware blocking" rule of `CLAUDE.md`). Cause, from the
  source: an autocommit write holds `Engine.schemaGate` shared for its whole
  statement, and every DDL path took it with `schemaGate.StrongLock()`
  (`cypher/api.go`, `runCreateBTreeIndex` and its siblings;
  `cypher/index_binding.go`), which takes no context; the context was checked only
  after the gate is acquired. **Fixed (rmp #2982):** every DDL path takes the gate
  through `Engine.lockSchemaForDDL`, which waits with `mvcc.Gate.StrongLockCtx` and
  returns the context error holding nothing, before any schema, index, constraint or
  WAL state is touched. `dd06-ddl-bounded-by-context` pins it.
- **D4 — a Cypher `DELETE` of a relationship another transaction removed is a silent
  no-op (open).** Reproduction: `CREATE (a:N {name:'a'})-[:R {id:1}]->(b:N {name:'b'}),
  (a)-[:R {id:2}]->(b), (a)-[:R {id:3}]->(a)`; T1 and T2 `BeginTx`; T1
  `MATCH (:N {name:'a'})-[r:R {id:1}]->() DELETE r RETURN count(*)` → 1; T2 the same
  statement → 1; T2 `Commit` → ok; T1 `Rollback`. Observed: relationship id 1 still
  exists, so T2's acknowledged, committed delete is lost. With T1 committing instead,
  both transactions commit a delete of the same relationship. Expected: T2's delete is
  refused with `ErrSerializationConflict` while T1's removal is pending or committed
  after T2's snapshot (first-updater-wins, F2), as the `lpg` arm
  (`gg06-delete-same-edge-instance-lpg`, `Graph.RemoveEdgeByHandle`) does. Cause, from
  the source: `removeBoundRelationship` (`cypher/exec/rel_instance.go`) tests whether
  the handle is stored in the PRESENT adjacency (`relStoredOrder`); a handle a peer
  already removed is treated as "deleted by an earlier row of this statement" (rmp
  #2940) and nothing is called, so no conflict is recorded — the shape of rmp #2943.
  The Cypher same-instance arm of GG06 is kept out of the catalogue until it is fixed.

### Gaps pinned

- **G1** (commit-hold seam): confirmed, and H2 checked. The only route is a WAL-backed
  engine whose `wal.WALFile.Sync` blocks on a gate (`wal.OpenFS` takes a caller
  filesystem; `txn.NewStoreWithOptions` and `cypher.NewEngineWithStore` take the
  result). It can hold T0 between its timestamp allocation and its publication, but
  one WAL writer runs one fsync at a time: a committer whose frames follow the held
  leader's waits for the leader to finish (`store/wal/writer.go`, `syncToLocked`), so
  T1 can never acknowledge while T0 is held, and SE02's shape cannot be built from it. SE02, SE04 and SE03's fsync arm are
  left to random load (#2934). Stage C2 found a second route that stage C1 did not:
  `lpg.Graph.AllocateCommitTS` reserves a commit instant without publishing it, on an
  in-memory graph, and the transaction's later `EndVersionedTx` publishes or abandons
  it. FP01-FP03 use it as the hold. SE02, SE04 and SE03's fsync arm are still assigned
  to #2934; whether to build them on this hold is open.
- **G2** (vacuum control): resolved with a drain `Hook` at a fixed point. The
  withdrawal of an aborted version now runs at abort (`withdrawAbortedNow`), so F5's
  race is narrower than the catalogue states; the no-drain arm is still not pinned.
- **G4** (value-preserving write as a skew remedy): pinned by SK11 — it is **not** a
  remedy.
- **G5** (no hook inside `MERGE`): confirmed — `cypher/exec/merge*.go` has no hook
  between probe and create. MG11 is left to random load (#2934).
- **G6** (UNIQUE against an in-flight reservation): pinned by MG02 (all three arms),
  MG03, MG07, MG09 and MG10 — always a ConstraintViolation, never a serialization
  conflict, and refused even when the holder later rolls back. An autocommit `MERGE`
  is refused too while an explicit transaction holds the value, so F10's "autocommit
  `MERGE` converges" holds only between autocommit callers.
- **G7** (no online index verifier): unchanged. Each IX row derives seek = scan in the
  statement itself, and `TestIXAccessPaths` guards that the seek arm is an index
  access.
- **G8** (adjacency append conflicts): settled by RI03 — the code is right. The second
  of two transactions appending an edge to one node is refused, for a shared target and
  for a shared source, through both drivers, in every interleaving. The `addEdgeInfo`
  godoc the catalogue quoted had already been corrected (commit `1ad20d03`); the stale
  text left was the comment on the `Graph.adjVer` field in `graph/lpg/lpg.go`, which
  said an adjacency append "is commutative and must not conflict with another append".
  It now states the rmp #2445 rule.
- **G9** (backfill source of `CREATE INDEX` / `CONSTRAINT`): settled — a committed
  snapshot. DD01 (index) and DD05 (UNIQUE constraint) pass in every interleaving, and
  DD01 fails against a mutant whose backfill reads the live graph (negative control
  above). The source agrees: `Engine.beginIndexBuild` waits out the commits deciding
  and scans `e.g.ReadAt(snap)`; the constraint build scans a snapshot taken after
  `HoldCommitDecisions`. Not an engine defect.
- **G10** (state after a refused statement): pinned by AB03 through both drivers —
  through Cypher, `COMMIT` returns `ErrTxPoisoned`; through `lpg`, the refused write
  dooms the transaction and its commit returns the serialization conflict. Both apply
  nothing, and a read after the refusal stays at the transaction's snapshot.

## Status

**IMPLEMENTED (rmp #2313).** `main.go` holds the configuration, the workload
primitives and the telemetry; `phases.go` holds the four measurement phases;
`example_test.go` is the short-layer regression gate.

The specification above was committed *before* any code, so the acceptance criteria
were fixed in advance rather than fitted to whatever the implementation produced.
