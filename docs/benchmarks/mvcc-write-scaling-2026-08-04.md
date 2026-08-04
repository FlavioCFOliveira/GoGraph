# MVCC write scaling — the durable write path stops being single-writer

**Date:** 2026-08-04
**Task:** rmp #2320 (MVCC B2a), sprint 334
**Subject:** `BenchmarkWriteScaling/wal` — the WAL-backed Cypher autocommit write
path at 1 … 32 concurrent writers.

## What changed

The ordinary write path moved from an EXCLUSIVE hold on the graph's schema
barrier (`lpg.Graph.ApplyAtomicallyTx`) to a SHARED one
(`lpg.Graph.ApplyVersioned`), for both the Cypher autocommit statement
(`cypher/api.go`) and the durable store's in-memory apply (`store/txn`).

That flip was attempted once before, under rmp #2304, and reverted: the deltas a
statement wrote still resolved their shared commit record through a graph-wide
AMBIENT slot, so with two brackets open one statement's writes landed on two
different commit records and a snapshot reader observed half a transaction
(105 942 torn observations from `examples/27_concurrent_txn`). rmp #2320 threaded
the transaction through the write path — `lpg.WriteView` and `adjlist.Writer`
carry it, the adjacency's write chain takes it as an explicit parameter — which is
what made the flip sound.

**Why this arm is the one that moves.** `wal.Writer.SyncGroup`'s leader/follower
fsync coalescing was already built and already unreachable: the exclusive barrier
spanned the fsync while the store's single-writer semaphore had been released just
before it, so every commit paid its own unshared fsync. Removing the exclusion is
what lets a group of committers share one.

## Method

```
go test -run='^$' -bench='BenchmarkWriteScaling/wal' -benchtime=400x -count=5 \
        -benchmem ./bench/mvccwrite/
benchstat wal_before.txt wal_after.txt
```

- `n=5` per arm, medians reported by `benchstat`.
- The two arms were measured BACK TO BACK on an otherwise idle machine, with the
  bracket flipped in place between them, so the comparison is not across builds
  taken at different times.
- No `-race`. The gate in `bench/mvccwrite/gate_test.go` is the race-enabled
  check; this is the throughput measurement.
- `goos: darwin`, `goarch: arm64`, `cpu: Apple M4`, 10 logical cores.

## Result

| writers | before (commits/s) | after (commits/s) | Δ | before scaling | after scaling |
|---:|---:|---:|---:|---:|---:|
| 1 | 267.0 | 267.0 | ~ (p=1.000) | 1.000 | 1.000 |
| 2 | 267.6 | 386.8 | **+44.54 %** (p=0.008) | 1.005 | 1.456 |
| 4 | 269.2 | 548.7 | **+103.83 %** (p=0.008) | 1.011 | 2.066 |
| 8 | 269.2 | 1096.0 | **+307.13 %** (p=0.008) | 1.011 | 4.125 |
| 16 | 268.8 | 2095.0 | **+679.39 %** (p=0.008) | 1.009 | 7.886 |
| 32 | 269.3 | 4041.0 | **+1400.56 %** (p=0.008) | 1.011 | 15.210 |
| geomean | 268.5 | 898.4 | **+234.58 %** | 1.008 | 3.379 |

`sec/op` geomean: 3.749 ms → 1.120 ms, **−70.11 %**.

Allocation per commit is unchanged (5.5 KiB/op, 77 allocs/op at writers=32 on both
arms), so the throughput did not come from doing less work per commit.

### The two numbers that matter most

- **writers=1 is IDENTICAL** — 267.0 commits/s on both arms, `p=1.000`. Nothing
  was traded for the scaling: a lone writer pays exactly what it paid before.
- **before is FLAT** — 267 → 269 commits/s across a 32× change in offered
  concurrency, i.e. 1.011× scaling at 32 writers. That flatness is the signature
  of the single-writer engine, and it is the baseline the sprint exists to move.

## Regression gate

`bench/mvccwrite` `TestWALWriteScalingGate` is ratcheted from a
"does-not-get-worse" floor of `0.90` to `writeScalingTarget` = **3.0** at eight
concurrent writers, and runs in the short layer under `-race`.

Measured at the ratchet, best of three per run:

```
go test       -count=3 -run=TestWALWriteScalingGate ./bench/mvccwrite/
  => 4.99x, 5.12x, 4.27x   (worst single observation 3.22x)
go test -race -count=3 -run=TestWALWriteScalingGate ./bench/mvccwrite/
  => 4.26x, 4.65x, 4.97x   (worst single observation 3.16x)
```

The floor clears by 42 % at worst, and the worst SINGLE observation still clears
it — which is the margin that counts, since the gate reads the best of three: a
loaded host can only depress a ratio, and a build that has gone back to
serialising writers cannot reach 3× even once.

## What did NOT move, and why — measured, not assumed

The store-less arm (`BenchmarkWriteScaling/mem`) is unchanged in substance. Its
outermost lock is `cypher.Engine.writeMu`, not the barrier — `lockWriter` takes
`writeMu` only when no store is attached, and holds it for the whole statement — so
retiring the barrier could not affect it.

Measured on the same machine, `-benchtime=20000x -count=5`, medians:

| writers | commits/s | scaling |
|---:|---:|---:|
| 1 | 354 647 | 1.000 |
| 16 | 266 056 | **0.750** |

The sprint's entry baseline for this arm was **0.83×** at sixteen writers (333 590 →
276 043 commits/s at head `6b990377`). It is now 0.750×, and the difference is
almost entirely in the NUMERATOR: the single-writer arm rose from 333 590 to
354 647 (+6.3 %) while the sixteen-writer arm moved from 276 043 to 266 056
(−3.6 %). A faster lone writer against an unchanged contended ceiling lowers the
ratio without anything having got worse in absolute terms, and the two runs used
different `-benchtime` values, so the −3.6 % should not be read as a regression.

**What this means for rmp #2304's AC2.** That criterion asks for throughput
"materially better than the 0.83× entry baseline at sixteen writers". On the arm
where 0.83× was measured — the store-less one — that is NOT achieved and cannot be,
because the lock that bounds it is `cypher.Engine.writeMu` and retiring it is
rmp #2306's scope. On the arm the barrier actually gated, the same measurement is
1.009× → **7.886×** at sixteen writers. The audit said as much in advance
(`docs/audit-mvcc-sole-cc-2026-08-02.md` §3.1: the store-less baseline "is writeMu +
visMu and nothing else"), and it is recorded here rather than resolved by choosing
whichever arm reads better.

`bench/mvccwrite`'s `writeScalingFloor` and `writeConcurrencyFloor` therefore stay
where they are; only `walWriteScalingFloor` was ratcheted.

## Reader latency did not regress (rmp #2304 AC6)

`bench/mtaudit` `TestFairScheduling_LongReadPlusWriterDoesNotStarveReaders`, run with
`-tags=soak`, WITHOUT `-race` and with no competing load, after the flip:

| readers | baseline | + long read | + writer 10/s | + long read AND writer | collapse |
|---:|---:|---:|---:|---:|---:|
| 1 | 215 921 op/s, worst 454 µs | 213 632, 448 µs | 209 855, 5.143 ms | 113 471, 3.977 ms | **1.903×** |
| 8 | 441 265 op/s, worst 1.672 ms | 408 933, 2.254 ms | 434 439, 9.058 ms | 223 796, 5.288 ms | **1.972×** |

The gate's tolerance is 4.0×, and the envelope certified when rmp #2274 fixed the
reader-starvation collapse was 1.91× / 2.07×. So the shared write path leaves reader
latency where it was — 1.903× and 1.972× are at or slightly better than the certified
figures, and both worst-case short-read latencies stay in the millisecond range
against the 1 m 36 s worst case that rmp #2274 was opened for.

## The cost the throughput is bought with

Overlapping writers make write-write conflicts REAL. Two transactions touching the
same object no longer queue: the second to reach the version-chain head is refused
with `mvcc.ErrSerializationConflict` and the client must retry. This is not free,
and it is visible in `examples/27_concurrent_txn`, which now reports
`# writer.conflicts_retried` and `# writer.conflict_retries_max` alongside its ACID
facts (34 conflicts, deepest chain 6, over 600 transfers at the default scale).

A conflict can also arise between transactions on DISJOINT objects, because a
transaction's start timestamp is the CONTIGUOUS commit frontier (rmp #2298): under
N concurrent writers a transaction routinely begins at an instant that excludes its
own predecessor's commit, so a writer re-touching its own object collides with
itself. PostgreSQL's `xip` list can express "T1 committed, T0 still running" and
GoGraph's frontier cannot, so GoGraph refuses where PostgreSQL proceeds — the trade
rmp #2298 accepted in exchange for never handing a reader a straddled commit.

**A retry's backoff must be sized to a WAL fsync, not to a scheduler yield.** A
`runtime.Gosched` loop was measured burning five consecutive attempts inside one
fsync, every one of them against the same in-flight version and with the same stale
snapshot, because the commit frontier cannot advance while the blocking commit is
still syncing.
