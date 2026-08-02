# MVCC write scaling — entry baseline for sprint 334

**Date:** 2026-08-02
**Head:** `c97118fe`
**Machine:** Apple M4, 10 cores (4 performance + 6 efficiency), 
darwin/arm64, go1.26.5
**Harness:** `bench/mvccwrite` (rmp #2297)
**Audit that motivated it:** [`docs/audit-mvcc-sole-cc-2026-08-02.md`](../audit-mvcc-sole-cc-2026-08-02.md) (rmp #2296)

This is the number sprint 334 exists to move: **write throughput as a function of
writer count**. It is recorded before any change, so the sprint's headline claim
— that throughput will scale with writer count once MVCC replaces exclusion — is
measured against a fixed reference rather than against a memory of one.

---

## 1. What is measured

One arm drives *N* goroutines, each running autocommit Cypher
`CREATE (n:Account {id: $id})` statements into its own `id` key space, and
reports the wall-clock throughput of the whole arm. The **scaling factor** is
that throughput divided by the single-writer throughput of the identical total
workload.

Writers share the label `:Account` on purpose — the label bitmap and the count
store are part of the mechanism under test — but never share an `id`, so the arms
measure contention on the *mechanism* and not on the *data*.

Two wirings are measured, because they serialise on different mechanisms and only
one of them can ever benefit from group commit:

| Wiring | Composition | Serialiser | Durability |
|---|---|---|---|
| `mem` | `cypher.NewEngine` over a bare `lpg.Graph` | `cypher.Engine.writeMu` (`cypher/api.go:1069`) **and** `lpg.Graph.visMu` (`graph/lpg/lpg.go:565`) | none |
| `wal` | `cypher.NewEngineWithStore` over a WAL-backed `txn.Store` on a temp dir | `txn.Store` semaphore (`store/txn/txn.go:444`) **and** `visMu` | fsync per commit, durable before visible |

`Engine.writeMu` is not taken in the `wal` wiring — `Engine.lockWriter`
(`cypher/api.go:1187-1193`) takes it only when no store is attached — so the two
arms genuinely exercise different serialisers, not the same one twice.

---

## 2. `mem` — the concurrency-control ceiling, with no durability mixed in

```
go test -run='^$' -bench=BenchmarkWriteScaling/mem -benchtime=200000x -benchmem -count=10 ./bench/mvccwrite/
```

benchstat over 10 runs:

| writers | commits/s | ns/commit | scaling | allocs/op |
|--------:|----------:|----------:|--------:|----------:|
| 1  | 344 100 ±1% | 2 906 | 1.000 | 60 |
| 2  | 290 000 ±1% | 3 449 | 0.850 ±1% | 60 |
| 4  | 291 500 ±2% | 3 431 | 0.855 ±2% | 60 |
| 8  | 287 100 ±2% | 3 483 | 0.842 ±2% | 60 |
| 16 | 284 800 ±2% | 3 512 | 0.835 ±2% | 60 |
| 32 | 282 400 ±2% | 3 542 | **0.828 ±2%** | 60 |

**Thirty-two writers on a ten-core machine deliver 0.828× the throughput of
one.**

The shape is as informative as the number. Throughput drops 15% between one
writer and two, and then stops changing: from 2 to 32 writers it moves by 2.6%,
which is inside twice the confidence interval. That is not a machine running out
of cores — at 2 writers there are 8 idle ones. It is the profile of a single
global exclusive lock: the first competitor pays the uncontended-to-contended
transition, and every writer after that simply queues, adding latency without
adding throughput.

`allocs/op` is 60 at every writer count and `B/op` varies by 0.5% across the
whole ladder, so nothing about the curve is an allocation or GC effect.

## 3. `wal` — durable commits, and group commit that cannot be reached

```
go test -run='^$' -bench=BenchmarkWriteScaling/wal -benchtime=400x -benchmem -count=10 ./bench/mvccwrite/
```

| writers | commits/s | ns/commit | scaling |
|--------:|----------:|----------:|--------:|
| 1  | 266.6 ±1% | 3 751 000 | 1.000 |
| 2  | 268.4 ±1% | 3 725 000 | 1.005 ±1% |
| 4  | 268.0 ±1% | 3 731 000 | 1.004 ±1% |
| 8  | 268.2 ±0% | 3 729 000 | 1.004 ±0% |
| 16 | 268.0 ±1% | 3 731 000 | 1.003 ±1% |
| 32 | 270.0 ±1% | 3 703 000 | **1.011 ±1%** |

**Thirty-two concurrent writers commit at exactly the same rate as one: 268
commits/s, 3.73 ms each.** A 32× change in offered concurrency produces a 1.1%
change in throughput.

Every commit pays a whole `fsync` that no other writer shares — even though the
fsync path, `wal.Writer.SyncGroup` (`store/wal/writer.go:497`, whose `dataSync`
call is at `:585`), is leader/follower coalescing built precisely so that
concurrent committers *can* share one flush. The audit traced why it is
unreachable: the store semaphore is released just *before* the fsync
(`Tx.releaseAfterAppend`, `store/txn/txn.go:1366`) but `visMu` spans it
(audit §2.3, steps 9c/9d), so only one writer is ever inside `SyncGroup` at a
time and the leader never has a follower to coalesce with.

Flat at 1.00× is therefore not "group commit does not exist". It is "group
commit exists and is never entered by more than one goroutine". That is the
number rmp #2193 has to move, and it can only move after rmp #2304 stops `visMu`
spanning the sync.

---

## 4. The regression gates

`bench/mvccwrite/gate_test.go` runs in the **short** layer, so it is part of
`make ci` and runs under `-race`. It carries two instruments, because the
headline one cannot survive a shared machine.

### 4.1 `measureScaling` — the headline number, and its bias

Throughput at 8 writers divided by throughput at **one** writer. This is what
"write throughput scales with writer count" means, and it is the number §2 and
§3 quote. `TestWriteScalingGate` fails below `writeScalingFloor`.

It has a systematic bias, and a shared machine makes it severe: **a process with
more runnable threads gets a larger share of a loaded host**, so the one-writer
arm is starved harder than the eight-writer arm and the ratio comes out too
high. This is not noise that averages away — it is a bias with a direction.

It was found the hard way. The first version of this harness turned `make ci`
red, with the one-writer arm running **4.2× slower** than in isolation while the
eight-writer arm lost only **1.2×**, reporting **3.05×** for work that was
serialised under a single mutex. The instrument had claimed strong parallelism
for a workload that was provably serial.

The consequence is a rule, and it is why every assertion that gates the engine
is a floor:

> A **floor** on a one-versus-N ratio is safe — load only inflates the ratio, so
> it can produce a false pass, never a false failure. A **ceiling** on it is not
> assertable on a machine that is not idle.

### 4.2 `measureSerialisationRatio` — the load-immune instrument

Throughput at 8 writers, divided by throughput of the **same work at the same 8
writers** with every unit taken under one external mutex. Both arms run the same
number of goroutines, so whatever share of the machine the process gets, it gets
in both, and the bias cancels.

What it reads is direct: *how much does adding a global lock to the write path
cost?* On an engine that already holds a global lock, the answer is nothing.

| Workload | idle | every core saturated at 2× |
|---|---:|---:|
| the engine, `mem` wiring | 0.97 – 1.02 | 0.73 – 0.76 |
| 8 independent CPU-bound units | 6.92 – 7.26 | 3.41 – 6.93 |
| the same units already behind a mutex | 1.003 – 1.013 | — |

**Putting an external global mutex around every engine write costs 0%.** That is
the defect stated in one number, measured without reference to a one-writer arm,
and it is the number that must move. `TestWriteConcurrencyGate` fails below
`writeConcurrencyFloor`.

### 4.3 The floors are ratchets, and today they are weak on purpose

A floor below 1.0 can only catch a serialised engine becoming *more* serialised.
It cannot catch the *absence* of scaling, because at head `c97118fe` there is no
scaling to lose. The constants are raised — never lowered — as the sprint
delivers:

| After | `writeScalingFloor` | `writeConcurrencyFloor` | Because |
|---|---|---|---|
| entry (`c97118fe`) | 0.60 | 0.50 | the write path is single-writer by construction |
| rmp #2304 — barrier retired from the autocommit path | ≥ 3.0 | ≥ 3.0 | writers apply concurrently against version chains |
| rmp #2306 — semaphore, write mutex and apply gate retired | re-measure and raise | re-measure and raise | the last serialisers are gone |

Each entry floor sits ~30% below the worst observation under saturating load:
`writeScalingFloor = 0.60` against a worst of 0.794 loaded and 0.873 idle;
`writeConcurrencyFloor = 0.50` against a worst of 0.734 loaded. That is far more
headroom than the 2.5% idle spread needs, deliberately — a gate that goes red on
a busy machine gets softened, and a softened gate is not a gate.

`writeScalingTarget = 3.0` is 37.5% parallel efficiency at 8 writers. It is
deliberately far below linear: a WAL append, a commit-timestamp mint and a
publication step stay genuinely serial even under ideal MVCC, and Amdahl bounds
what is left.

### 4.4 Both instruments are validated in both directions

An instrument that cannot fail proves nothing. The usual validation — run the
gate against a build that has the defect — is not available in the usual
direction here: the only build that exists *has* the defect, and injecting more
serialisation into an already fully serialised engine barely moves either ratio,
because both arms slow down together. Measuring that would have proved nothing
while looking like proof.

So the validation points the gates' own measurement code at a synthetic
CPU-bound workload, in two variants differing in exactly one respect — whether
the work is serialised — and checks that both instruments' verdicts flip across
`writeScalingTarget`:

| Test | Workload | `measureScaling` | `measureSerialisationRatio` | Verdict at target 3.0 |
|---|---|---:|---:|---|
| `..._SeesConcurrency` | 8 independent spin units | **6.04 – 6.17×** | **6.92 – 7.26×** | would PASS |
| `..._SeesSerialisation` | the same units under **one mutex** | 0.86 – 0.87× *(logged, not asserted)* | **1.003 – 1.013×** | would FAIL |
| `TestWriteScalingGate` / `TestWriteConcurrencyGate` | the real engine, `mem` wiring | **0.84 – 0.88×** | **0.97 – 1.02×** | at entry: below target, above floor |

The real engine sits with the *serialised* control on both instruments, and
nowhere near the parallel one. That is the measurement the sprint has to invert.

Only the load-immune instrument is asserted on in `..._SeesSerialisation`, since
that assertion is a ceiling and §4.1 is why a ceiling on the scaling instrument
is not assertable in CI. Its two arms are symmetric — same goroutine count, same
already-serialised work — so they degrade together and the ratio stays within 1%
of 1.0 whatever else the host is doing.

Both controls skip on machines with fewer than 8 cores — an environment
precondition, since with fewer cores than writers the concurrent arm is
oversubscribed and the measurement says nothing about the instrument.

### 4.5 Verified under load

The whole file was run twice with a CPU load generator saturating all 10 cores
at 2× oversubscription, under `-race`. Both runs passed, and the two
instrument-validation tests kept their separation. `make ci` is green at 119
packages, 0 lint issues, aggregate library coverage 87.1%.

---

## 5. Reproducing

```bash
# The two baseline tables above
go test -run='^$' -bench=BenchmarkWriteScaling/mem -benchtime=200000x -benchmem -count=10 ./bench/mvccwrite/ | tee mem.txt
go test -run='^$' -bench=BenchmarkWriteScaling/wal -benchtime=400x    -benchmem -count=10 ./bench/mvccwrite/ | tee wal.txt
benchstat mem.txt
benchstat wal.txt

# The gates and their controls, as make ci runs them
go test -race -run=TestWrite -v -count=1 ./bench/mvccwrite/
```

Run the benchmark ladders on an **idle** machine: §4.1 is why the scaling number
is only authoritative there. The `mem` ladder takes about 20 s per `-count`, the
`wal` ladder about 90 s; the four gate tests take about 7 s under `-race`.

The gates themselves are built to survive a busy one, which is their job in
`make ci` — but their role there is regression detection, not certification.
