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
`docs/mvcc-scenario-catalogue.md` from PostgreSQL's isolation specs and InnoDB's
tests. The PostgreSQL scenarios are re-implemented, not copied. Each row is an
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
  reports rows; added to `internal/isolationtest` for this purpose). The `lpg` API
  has no voluntary rollback of a multi-statement transaction, so rows that need
  `ROLLBACK` run through Cypher only.
- **Vacuum drain (H1).** A `Hook` step calls `lpg.Graph.ReclaimNow`. Every
  write-after-rollback row places it in the rolling-back session directly after the
  `ROLLBACK`, and runs only the interleavings in which no other step falls between
  the two, so the drain is at a fixed point in every run.
- **Goldens** live in `internal/isolationtest/testdata`. That is decided by the
  harness: `isolationtest.Check` resolves `testdata/<spec>.golden` against its own
  source directory. Catalogue goldens carry the row ID as a prefix
  (`ww02-…`, `sk12-…`).
- **Property checks.** WW15 (pinned reader byte-identical), SK10 (seek = scan),
  SK12 (repeatable count; read-skew freedom) and SK14 (fast path = scan) also assert
  a property on every step, independently of the golden.

### Rows

Expected outcomes are snapshot isolation (F1), not serializable snapshot isolation:
GoGraph permits write skew and refuses a write-write conflict.

| Row | Spec (golden) | Driver | Source | Expected SI outcome | GoGraph outcome, and why it differs from the source |
|---|---|---|---|---|---|
| WW01 | `lost-update` (existing) | Cypher | PG `deadlock-simple`, `lock-committed-update`; MY `concurrent.inc` | Second writer refused | As expected. InnoDB REPEATABLE READ permits the lost update; SI cannot. |
| WW02 | `ww02-stale-snapshot-write`, `-lpg` | both | PG `lock-committed-update`, `lock-committed-keyupdate`, `eval-plan-qual` | Writer refused when the peer's version is in flight or committed after its snapshot | As expected. PG READ COMMITTED re-fetches and succeeds. |
| WW03 | `ww03-blind-increment`, `-lpg` | both | MY `innodb_bug52663`, `innodb_bug49164`; PG `eval-plan-qual` | Second increment refused; final 1 | As expected. InnoDB and PG READ COMMITTED reach 2 without an error. |
| WW04 | `ww04-update-delete-commit`, `ww04-update-rollback-then-delete`, `ww04-delete-rollback-then-update` | Cypher | PG `eval-plan-qual`, `lock-update-delete`, `merge-delete`; MY `concurrent.inc` | Later writer refused; allowed after the earlier rolls back and the vacuum drains | As expected. A `DETACH DELETE` that conflicts reports it at `COMMIT`, not at the statement; nothing is applied. The rollback arms' second writer is an autocommit statement (see Defects found, D1). |
| WW05 | `ww05-update-delete-chain-commit` | Cypher | PG `lock-update-delete`, `lock-update-traversal`, `aborted-keyrevoke` | Older-snapshot peer refused once the chain commits | Commit arm as expected. **Rollback arm not implemented**: it is defect D1's shape. |
| WW06 | `ww06-predicate-moved-out` | Cypher | PG `eval-plan-qual`, `merge-match-recheck`, `merge-update`; MY `innodb-semi-consistent` | Older snapshot still matches and is refused | As expected. PG READ COMMITTED and InnoDB's semi-consistent read skip the row instead. |
| WW07 | `ww07-non-matching-not-blocked` | Cypher | MY `innodb-semi-consistent`, `innodb-consistent` | No wait, no conflict | As expected. |
| WW08 | `ww08-different-properties-one-node`, `-lpg` | both | MY `concurrent.inc`; PG `update-locked-tuple` | Second refused: the node is the conflict unit | As expected; same verdict as PG and InnoDB row granularity. |
| WW09 | `ww09-disjoint-nodes`, `-lpg` | both | `docs/isolation-design.md` | Zero conflicts in every interleaving | As expected. |
| WW10 | `ww10-crossing-writes`, `-lpg` | both | PG `deadlock-simple`, `fk-deadlock`, `fk-deadlock2`; MY `deadlock_detect`, `innodb_deadlock` | No wait; refused instead | As expected; in some interleavings **both** transactions are refused, where PG and InnoDB pick one deadlock victim. |
| WW11 | `ww11-ring-3`, `ww11-ring-8` | Cypher | PG `deadlock-hard`; MY `long_deadlock_cycle`, `undetected_deadlock`, `hp_deadlock` | No wait; each write succeeds or is refused; progress after rollback | As expected. Named interleavings (round-robin, sequential; staggered for 3). |
| WW12 | `ww12-write-after-peer-rollback` | Cypher | PG `eval-plan-qual`, `eval-plan-qual-trigger`, `multixact-no-forget`; MY `innodb_mysql_rbk` | Write after rollback + drain succeeds | As expected for a writer whose snapshot follows the rollback. The no-drain arm is not pinned (a race, F5). The concurrent explicit-transaction arm is defect D1. |
| WW13 | `ww13-void-label-removal`, `-lpg`; `ww13-void-property-delete`, `-lpg`; `ww13-void-edge-property` | both | GoGraph rmp #2354 (no PG/InnoDB counterpart) | Statement OK, `COMMIT` refused, nothing applied | As expected for label removal and property delete, through both drivers. The **edge-property** write is refused at the statement, not at `COMMIT`: the catalogue's F3 premise (edge side stores record the conflict) does not hold for `SET r.w` on an existing edge property. The outcome is still a refusal that applies nothing. |
| WW14 | `ww14-chain-walk-in-flight-head` | Cypher | PG `update-conflict-out`, `multiple-row-versions`; MY `innodb-read-view`, `lob_mvcc_undo` | Pinned reader sees the original; a new reader sees the committed value, never the in-flight one | As expected. |
| WW15 | `ww15-long-chain-large-values` | Cypher | PG `multiple-row-versions`; MY `lob_mvcc_undo`, `lob_partial_update_concurrent` | Pinned reader returns the 64 KiB value of its BEGIN, byte-identical | As expected. |
| SK01 | `write-skew` (existing) | Cypher | Berenson et al. A5B | Permitted | As expected. |
| SK02 | `sk02-swap-write-skew` | Cypher | PG `simple-write-skew` | Permitted: both commit, sets swapped | As expected. PG SERIALIZABLE aborts one. |
| SK03 | `sk03-booking-overlap` | Cypher | PG `classroom-scheduling`, `temporal-range-integrity` | Permitted: overlapping bookings | As expected. |
| SK04 | `sk04-cross-label-write-skew` | Cypher | PG `project-manager` | Permitted | As expected. |
| SK05 | `sk05-total-cash`, `-lpg` | both | PG `total-cash` | Permitted: total −200 | As expected. |
| SK06 | `read-only-anomaly-named` (existing) | Cypher | PG `read-only-anomaly`, `read-only-anomaly-2` | Permitted | As expected; PG SERIALIZABLE aborts. |
| SK07 | `sk07-receipt-report` | Cypher | PG `receipt-report` | Permitted: the report misses a receipt that later commits into its batch | As expected, in two of the four named interleavings. The receipt's date is bound with `WITH` (see D2). |
| SK08 | `sk08-two-ids` | Cypher | PG `two-ids` | All commit in every interleaving | As expected. |
| SK09 | `bank-transfer` (existing) | Cypher | examples/27 | Total constant | As expected. |
| SK10 | `sk10-write-skew-hash-seek`, `sk10-write-skew-btree-range` | Cypher | PG `index-only-scan`, `predicate-hash`, `insert-conflict-serializable`, `matview-write-skew` | Permitted; seek = scan in every step | As expected. 1 100 padding nodes make the planner seek; `TestSK10AccessPaths` proves one arm seeks and the other scans. |
| SK11 | `sk11-materialised-conflict-increment`, `sk11-materialised-conflict-value-preserving` | Cypher | Fekete et al., TODS 2005; MY `t/locking_clause` | Increment arm: second refused, rule holds | Increment arm as expected. **Value-preserving arm (`SET g.v = g.v`): both commit and the rule breaks** — a write of the current value writes no version and claims nothing, so it is not a remedy (G4 pinned). |
| SK12 | `sk12-no-phantom-no-non-repeatable`, `sk12-read-skew-lpg` | both | MY `t/consistent_snapshot`, `select_count_perf`, `innodb-read-view` | Repeated reads equal; no read skew | As expected. The `lpg` arm reads two keys (A5A) and is the negative control's target. |
| SK13 | `sk13-snapshot-at-begin`, `-lpg` | both | MY `t/consistent_snapshot`; PG `fk-snapshot` | Snapshot at BEGIN | As expected. InnoDB's plain `START TRANSACTION` takes it at the first read. |
| SK14 | `sk14-fast-path-aggregates` | Cypher | MY `select_count_perf`, `parallel_read`; PG `index-only-bitmapscan` | Fast path = scan, at the snapshot | As expected. |

RO01–RO03 sit in catalogue §1.7 and are not part of this stage.

### Negative control

`world.perKeySnapshot` (example code, set only by `TestReadSkewNegativeControl`)
makes every `lpg`-driver read take a fresh snapshot — each key read at its own
instant. With it, `sk12-read-skew-lpg` reports the anomaly; without it, none:

```
permutation "s1x s2dx s2cy s2c s1y s1x2 s1c" step s1y: read skew: x=50 y=60 sum to 110, want 100
```

Run against the golden with the seam on, the same spec fails on twelve property
violations and on a transcript diff; with the seam off it passes.

### Defects found

Neither is fixed here; both are recorded for the engine owners.

- **D1 — `ExplicitTx.Rollback` publishes its commit record.** After a Cypher
  `ROLLBACK`, `MVCCStats().Write` counts one more commit and no abort, and a
  transaction whose snapshot predates the rollback is refused when it writes a node
  the rolled-back transaction touched — even after `ReclaimNow`. Snapshot isolation
  (and PostgreSQL, which lets the waiter proceed once the peer aborts) allows that
  write. The same shape through the `lpg` API (a doomed transaction ended with
  `EndVersionedTx`) is allowed. Reproduction: `CREATE (:Item {name:'n', x:0})`;
  T2 `BeginTx`; T1 `BeginTx`, `SET n.x = 1`, `Rollback`; `ReclaimNow`; T2
  `SET n.x = 2` → `mvcc: serialization conflict in node properties`. Expected: success.
- **D2 — a property read inline in a `CREATE` map ignores the snapshot.** Inside an
  explicit transaction, after a peer committed `c.date = 2` over the transaction's
  snapshot value 1, `MATCH (c:Control) CREATE (r:Receipt {date: c.date, amount: 4})
  RETURN r.date` stores and returns **2**, while `MATCH (c:Control) RETURN c.date`
  in the same transaction returns 1. `CREATE (r:Receipt {date: c.date}) RETURN
  r.date` also returns 2; binding the value first (`WITH c.date AS d CREATE …`) or
  `CREATE … SET r.date = c.date` returns 1. A read of data committed after the
  snapshot is a non-repeatable read inside one transaction.
- **Parser.** A `CREATE` map entry whose value contains a comma — `CREATE (:Doc
  {blob: reduce(s = 'z', i IN range(1, 16) | s + s)})` — fails with `parse
  properties … missing ':' in map item`. WW15 binds the value with `WITH` instead.

### Gaps pinned

- **G1** (commit-hold seam): not needed by any row of this stage; unchanged.
- **G2** (vacuum control): resolved with a drain `Hook` at a fixed point. The
  withdrawal of an aborted version now runs at abort (`withdrawAbortedNow`), so F5's
  race is narrower than the catalogue states; the no-drain arm is still not pinned.
- **G4** (value-preserving write as a skew remedy): pinned by SK11 — it is **not** a
  remedy.
- **G8** (adjacency append conflicts): not exercised by §1.1–§1.2 (RI03 decides it).
- **G10** (state after a refused statement): pinned — through Cypher, `COMMIT`
  returns `ErrTxPoisoned`; through `lpg`, the refused write dooms the transaction and
  its commit returns the serialization conflict. Both apply nothing.

## Status

**IMPLEMENTED (rmp #2313).** `main.go` holds the configuration, the workload
primitives and the telemetry; `phases.go` holds the four measurement phases;
`example_test.go` is the short-layer regression gate.

The specification above was committed *before* any code, so the acceptance criteria
were fixed in advance rather than fitted to whatever the implementation produced.
