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
scaling.levels=5
## phase 2 — contention, reader latency, version retention
# contention commits=1600 hot_orders=410 hot_pct_actual=25.6
# contention retried_orders=20 retry_succeeded=20 unrecovered=0
# conflicts.total=20 aborts=20 commits=2604
# conflicts.by_store.node properties=20
# versions.samples=25 versions.max_retained=2331 versions.bound=4096 versions.ceiling=16384 max_chain_depth=10 max_concurrent_writers=8
# reader.samples=8606 reader.p50=416ns reader.p95=1.083µs reader.p99=10.208µs
contention.accounted=true
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

## Status

**IMPLEMENTED (rmp #2313).** `main.go` holds the configuration, the workload
primitives and the telemetry; `phases.go` holds the four measurement phases;
`example_test.go` is the short-layer regression gate.

The specification above was committed *before* any code, so the acceptance criteria
were fixed in advance rather than fitted to whatever the implementation produced.
