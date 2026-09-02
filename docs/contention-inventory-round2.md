# Contention inventory — round 2, 2026-09-02

Ranked, measured inventory of every contention site found across the GoGraph
module surface, re-baselined after round 1's six fixes landed. Produced by rmp
task #2690 in sprint 353, "GoGraph Optimization Laboratory".

Round 1's inventory (`docs/contention-inventory-2026-09-01.md`) is superseded by
this one, for the reason that motivated this task: **contention relocates when
it is removed.** Every fix promoted a different site to the top, so a ranking
survives only until the next fix lands.

## How this was measured

| | |
|---|---|
| Instrument | `bench/contention`, extended by this task with five workloads and thirteen ceiling arms |
| HEAD | `69b496b6`, branch `feature/353-gograph-optimization-laboratory` |
| Module code measured | byte-identical to `69b496b6`; the working tree differs only inside `bench/contention`, additively |
| Host | Apple M4, **10 cores** (4 performance + 6 efficiency), 32 GiB, `go1.27.0 darwin/arm64` |
| Sweep | 25 workloads x 5 levels x 2 windows = **250 fresh child processes**, 1026 s wall, exit 0, 125 rows |
| Sweep host load | loadavg **1.63** before, **10.48** after; nothing else ran on the machine |
| Ceiling probe | 14 pairs x 3 levels x 5 interleaved rounds = **420 windows**, 2796 s wall, exit 0 |
| Ceiling host load | loadavg **2.18** before, **8.10** after |
| Ranking | `go tool pprof -top -cum -lines`, cumulative and line-granular, restricted to GoGraph frames |

Every measurement runs **two windows in two separate processes**: an unprofiled
window supplying throughput and latency, and a profiled window supplying lock
attribution. The profiles and the heap are both cumulative per process with no
reset API, so sharing a process lets the first window contaminate the second.

### The noise floor is not one number

Measured first and twice, A-vs-A — the same code on both sides, driven by exactly
the machinery that later produced every ratio here, arms interleaved and the
order alternating between rounds. Eighteen ratios that should all read 1.000:
sixteen landed inside **+/-2.4%**. The two that did not were both
`index-manager-fanout`@1024, whose own five-round spread is 38.88% and 19.23%
against 0.4-5.6% everywhere else.

**Working rule applied to every ratio in this document: +/-2.4% is the floor
where an arm's own spread is under ~6%, and no ratio is called a result unless it
clears that arm's own measured spread.** Every ceiling number below carries its
arm spreads beside it.

### Reading the numbers honestly

- **The host has 10 cores.** Only the 1 -> 8 region is a scaling signal. At 64 and
  above the question is not "does it go faster" but "does it degrade gracefully".
  Oversubscription is not reported here as a contention defect.
- **Anti-scaling at 8 is damning.** With spare cores available, a curve below
  1.000x means the sharing costs more than the work it protects.
- **A share is not a prize.** Round 1 measured a site holding 98.67% of all mutex
  delay with about 4% of achievable throughput behind it. This round therefore
  refuses to rank a site without a ceiling number, and the refusal earned its
  keep twice over — see [Big share, small ceiling](#big-share-small-ceiling).
- `scaling_vs_1` is throughput at level *N* over throughput at level 1, both from
  the unprofiled window.

## Round 1 verified: every fix holds

`scaling_vs_1` at 8 / 1024, round 1 measured at `3733f514` against round 2 at
`69b496b6`. **Bold** marks anti-scaling.

| workload | round 1 | round 2 | fix |
|---|---|---|---|
| `cypher-mixed-rw` | 1.602 / **0.096** | 1.789 / 1.334 | #2686 |
| `index-count-spread` | **0.917** / **0.538** | 2.470 / 2.713 | #2682 |
| `index-hash-rw` | 1.329 / **0.916** | 1.898 / 1.742 | #2692 |
| `index-btree-rw` | not separately reported | 5.750 / 5.651 | #2683 |
| `cypher-read-label-small` | 1.467 / 1.477 | 1.555 / 1.528 | #2691, #2693 |

**No workload that round 1 addressed anti-scales any more.** The five worst
curves in this round are all surfaces round 1 never reached.

## Scaling table

`scaling_vs_1`, higher is better. **Bold** marks anti-scaling.

| workload | 1 | 8 | 64 | 256 | 1024 | ops/s at 1 |
|---|---|---|---|---|---|---:|
| `search-bfs-csr` | 1.000 | 5.819 | 5.781 | 6.564 | 7.212 | 195,664 |
| `index-btree-rw` | 1.000 | 5.750 | 6.180 | 5.915 | 5.651 | 23,795,433 |
| `search-sssp-shared` | 1.000 | 5.403 | 6.976 | 7.165 | 7.383 | 2,403 |
| `cypher-write-wal` | 1.000 | 3.986 | 29.802 | 97.034 | 207.813 | 258 |
| `store-checkpoint-write` | 1.000 | 3.706 | 21.698 | 63.581 | 93.313 | 195 |
| `cypher-read-scan-large` | 1.000 | 3.168 | 3.452 | 3.454 | 3.412 | 240 |
| `cypher-read-project` | 1.000 | 3.075 | 3.680 | 3.643 | 3.591 | 4,099 |
| `lpg-neighbours-read` | 1.000 | 3.004 | 3.379 | 3.392 | 2.748 | 5,886,798 |
| `index-count-spread` | 1.000 | 2.470 | 2.633 | 2.652 | 2.713 | 118,285,913 |
| `mvcc-explicit-tx` | 1.000 | 2.242 | 2.365 | 1.875 | 1.266 | 123,072 |
| `centrality-pagerank` | 1.000 | 2.217 | 3.683 | 3.863 | 3.544 | 10,428 |
| `dst-concurrent-bolt` | 1.000 | 2.206 | **0.470** | **0.274** | **0.127** | 595 |
| `mvcc-session-write` | 1.000 | 2.182 | 2.203 | 1.869 | 1.385 | 370,156 |
| `dst-disk-wal` | 1.000 | 2.174 | 10.423 | 28.116 | 44.583 | 1,168 |
| `cypher-write-mem` | 1.000 | 2.104 | 2.080 | 1.744 | 1.388 | 363,503 |
| `index-hash-rw` | 1.000 | 1.898 | 1.795 | 1.747 | 1.742 | 54,082,029 |
| `cypher-mixed-rw` | 1.000 | 1.789 | 1.740 | 1.494 | 1.334 | 492,737 |
| `bolt-connect-churn` | 1.000 | 1.566 | 2.078 | 2.523 | 2.733 | 27,382 |
| `cypher-read-label-small` | 1.000 | 1.555 | 1.619 | 1.576 | 1.528 | 1,012,488 |
| `bolt-wire-read` | 1.000 | 1.484 | 1.606 | 1.705 | 1.711 | 75,572 |
| `dst-disk-fault-wal` | 1.000 | **0.989** | **0.862** | **0.768** | **0.585** | 177,585 |
| `index-manager-fanout` | 1.000 | **0.773** | **0.650** | **0.505** | **0.089** | 7,381,596 |
| `metrics-emit` | 1.000 | **0.445** | **0.446** | **0.440** | **0.465** | 44,385,276 |
| `index-count-hot` | 1.000 | **0.328** | **0.331** | **0.331** | **0.330** | 242,816,796 |
| `generation-publish-read` | 1.000 | **0.094** | **0.084** | **0.084** | **0.084** | 141,137,201 |
| `dst-mvcc-sessions` * | 1.000 | 2.169 | 2.839 | 2.861 | 2.814 | 127 |

\* `dst-mvcc-sessions` was added after the main sweep and swept on its own
(5 levels x 2 windows, exit 0, loadavg 1.74 before / 2.60 after). It is not
comparable to the rows above as a contention ranking and is not ranked below —
see [dst-mvcc-sessions shares nothing, and that is the point](#dst-mvcc-sessions-shares-nothing-and-that-is-the-point).

## Ranked inventory

Ranked by **ceiling**, not by share: the number that says how much throughput
partitioning could actually buy. Blocked time is cumulative mutex delay over all
goroutines, so the absolute figures at 1024 are large by construction; the share
is what locates the site inside its own profile.

| # | site | blocked / share | provoked by | ceiling @8 | ceiling @1024 | on an engine path? |
|---:|---|---|---|---|---|---|
| 1 | `graph/generation/generation.go:152` `releaseRef`, `:162` `Release`, `:185` `Publish` | 64.34 ms @8; 2337.55 s @1024 (`Publish` 82.89%, `releaseRef` 17.11%) | `generation-publish-read`@8 | **47.619x** (2.42% / 7.17%) | 23.878x (0.37% / 13.08%) | **no — and deliberately so** |
| 2 | `graph/index/manager.go:254` `Manager.Apply` -> `graph/index/label/index.go:327` `Index.Add` | 11484 s = **43.88%** of 26172 s @1024; `CreateIndex` 28.34%, `DropIndex` 27.65% | `index-manager-fanout`@1024 | 3.396x (20.65% / 3.40%) | **32.223x** (48.32% / 5.14%) | yes — see the correction below |
| 3 | `bolt/server/serve.go:802` -> `graph/lpg/lpg.go:1388` `applyVersionedInstant` | 9792 s = **98.55%** of 9936 s @1024 | `dst-concurrent-bolt`@1024 | 1.627x (1.43% / 2.15%) | **16.307x** (49.41% / 34.78%) | yes |
| 4 | `internal/metrics/metrics.go:105` `IncCounter` | 5.23 ms @1024; **82.01% of CPU** @8 | `metrics-emit`@8 | 3.300x (1.90% / 13.48%) | 2.138x (2.15% / 28.86%) | only with a real backend installed |
| 5 | `store/wal` durable commit, reached via `cypher/api.go:18379` `execUnderBarrier` | 1425.59 s = 99.03% of 1439.51 s @1024 | `dst-disk-wal`@1024 | 3.860x (0.28% / 0.79%) | 1.353x (1.91% / 2.07%) | yes |
| 6 | `graph/index/count/count.go:346` `Store.Apply` | 4.90 ms @1024; `atomic.Int32.Add` **44.85% of CPU** @8 | `index-count-hot`@8 | 2.916x (0.45% / 26.25%) | 2.392x (0.16% / 24.10%) | yes |
| 7 | `graph/index/hash/index.go:1191` `Index.Insert` | 268.01 s = **97.91%** of 273.74 s @1024 | `index-hash-rw`@1024 | 1.960x (1.38% / 9.57%) | 1.531x (4.95% / 9.08%) | yes |
| 8 | `cypher/api.go:18379` `execUnderBarrier` (in-memory write barrier) | 189.16 s = 88.13% of 214.63 s @1024 | `cypher-write-mem`@1024 | 1.262x (2.44% / 2.17%) | 1.702x (2.63% / 9.24%) | yes |
| 9 | `cypher/api.go:18379` via `cypher/session.go:109` `Session.RunInTx` | 134.41 s = 72.13% of 186.33 s @1024 | `mvcc-session-write`@1024 | 1.308x (1.79% / 1.88%) | 1.712x (2.04% / 5.96%) | yes |
| 10 | `cypher/api.go:4933` `parseAndAnalyse` | 121.10 s = 53.29% of 227.26 s @1024 | `cypher-read-label-small`@1024 | 1.152x (2.47% / 3.26%) | 1.116x (3.42% / 1.82%) | yes |
| 11 | `cypher/plan_cache.go:85` `planCache.get` | 144.56 s = 61.48% of 235.13 s @1024 | `cypher-mixed-rw`@1024 | 1.111x (1.87% / 5.53%) | 1.272x (8.44% / 5.79%) | yes |
| 12 | `graph/index/btree` (round 1's #2683 target) | — | `index-btree-rw` | 0.948x (2.38% / 5.53%) | 1.005x (7.47% / 6.45%) | yes — **exhausted** |
| 13 | `graph/lpg` / `graph/adjlist` neighbour read | — | `lpg-neighbours-read` | 1.004x (3.97% / 2.25%) | 1.001x (3.08% / 3.04%) | yes — **nothing there** |

Spreads in parentheses are (base arm, ceiling arm) five-round min/max as a
fraction of the median. Every ratio called a result above clears both.

### The ceiling arms understate, they do not flatter

A ceiling arm does not delete a lock — module code is not the harness's to
change, and a deleted lock measures a program that does not exist. It **removes
the sharing**: it builds `GOMAXPROCS` independent copies of the fixture the base
workload shares and routes each worker to one. The path through the module is
byte-identical; only the number of goroutines meeting on one object changes.

That construction carries a handicap, and the level-1 cell measures it. At level
1 there is nothing to unshare, so the pair must read 1.00x — and several do not:

| arm | ratio @1 | what the handicap is |
|---|---|---|
| `metrics-emit` | 0.627x | 10 Prometheus registries instead of 1: worse locality, more resident state |
| `index-count-hot` | 0.864x | 10 count stores instead of 1 |
| `generation-publish-read` | 0.893x | 10 CSR publishers instead of 1 |
| `index-hash-rw` | 0.949x | 10 hash indexes instead of 1 |
| `index-btree-rw` | 0.950x | 10 btree indexes instead of 1 |
| `cypher-read-label-small` | 0.960x | 10 engines and 10 graphs instead of 1 |

**Every ceiling above 1.00x is therefore a lower bound.** `metrics-emit`'s 3.300x
is bought while paying a 37% locality penalty; the sharing costs more than the
raw ratio says.

## The mutex profiler is blind to the module's worst site

`generation-publish-read` is the worst-scaling workload measured — **0.094x at 8
goroutines**, ten times slower with eight cores than with one — and its mutex
profile holds **64.34 ms**. The CPU profile says why: `sync/atomic.(*Int64).Add`
is **76.92% of all CPU**, split `Publisher.Acquire` 41.25% / `releaseRef` 40.08%.
The per-generation refcount is one atomic on one cache line and every reader
touches it.

**Cache-line coherence is not a lock.** No profiler that measures *blocked time*
can see it, because nothing blocks: the CAS succeeds, it just costs a coherence
round trip every time. Only the scaling column can report it, and only a ceiling
arm can price it.

Two more sites are in the same class, and both are on real engine paths:

| workload | scaling @8 | mutex delay @1024 | what the CPU profile says |
|---|---|---|---|
| `index-count-hot` | 0.328 | **4.90 ms** | `atomic.Int32.Add` = 44.85% of CPU, under `count.(*Store).Apply` |
| `metrics-emit` | 0.445 | **5.23 ms** | `metrics.IncCounter` = 82.01% cumulative |

The workload that provoked #1 predicted this in its own godoc before it was run:
"a surface can be contended without a single blocked nanosecond, and only the
scaling column can say so". It was right.

## Big share, small ceiling

Round 1's central lesson repeated twice this round, now with the ceiling numbers
that prove it. These two sites hold the largest shares of their profiles in the
Cypher paths, and partitioning them is worth almost nothing:

| site | share of its profile | ceiling @8 |
|---|---:|---:|
| `cypher/plan_cache.go:85` `planCache.get` | 61.48% | **1.111x** |
| `cypher/api.go:4933` `parseAndAnalyse` | 53.29% | **1.152x** |

`planCache.get` was the #2 site of round 1 at 74.68%, and #2691 addressed it.
It still holds 61.48% of `cypher-mixed-rw`@1024's delay — **and there is 11% left
behind it.** Ranking by share would put it near the top of this document; ranking
by ceiling puts it eleventh, which is where the evidence says it belongs.

## Coverage

### DST drivers — the gap round 1 recorded, now closed

| driver | status |
|---|---|
| `sim.RunConcurrent` | **reached** — `dst-concurrent-bolt` drives it against one shared `sim.SimServer`, one connection per operation so `level` stays the only concurrency variable |
| Fault injection | **reached** — `dst-disk-fault-wal`, a real durable Cypher write path over `sim.SimDisk` via `wal.OpenWith`, every append and fsync crossing `SimDisk.mu`, at a seeded 1/512 per-sync fault probability |
| `sim.RunMVCCSessions` | **reached** — `dst-mvcc-sessions`; see below |
| Full durable DST stack over `SimDisk` | **not constructible** — `sim.OpenSimStore` is exported but its second parameter is the unexported type `simStoreConfig`, and its only constructor `durableStoreConfig()` is unexported too. Reaching it requires editing `internal/sim`, which this task placed out of scope |

**The fault arm is honest only as a pair, and the probe that established this is
worth recording.** Driving 5000 durable commits at four fault rates:

| fault rate | ok | faults | syncs |
|---|---:|---:|---:|
| 0 | 5000 | 0 | 5000 |
| 1e-05 | 5000 | 0 | — |
| 1e-04 | 5000 | 0 | — |
| 1/512 | **559** | **4441** | **561** |

The WAL writer **fail-stops on the first injected fsync fault and never syncs
again** — syncs stop at 561 for 5000 attempted commits, and the first fault reads
`wal: durability failed; the un-synced suffix was discarded and this writer is
poisoned`. That is the reliability mandate working exactly as specified, not a
defect. But it means there is **no steady state of "durable writes with occasional
faults" to measure**: after the first fault the window measures the poisoned-writer
error path and nothing else. Published as a controlled pair with one variable
between the arms — `dst-disk-wal` (rate 0) and `dst-disk-fault-wal` (rate 1/512) —
rather than as a single row whose number would have been 99.75% one fail-stop
branch.

### The three surfaces round 1 never drove

| surface | status | result |
|---|---|---|
| `graph/generation` | **covered** — `generation-publish-read` | the worst site in this inventory, #1 |
| `graph/index` `Manager` | **covered** — `index-manager-fanout` | #2 |
| `internal/metrics` | **covered** — `metrics-emit` | #4 |

`internal/metrics` needed care to cover honestly: its default backend is a no-op
behind an `atomic.Pointer`, so driving it as shipped measures nothing. The
workload installs the real `internal/metrics/prometheus.Registry` backend, and
the row is therefore about metrics **as enabled**, not as defaulted.

### dst-mvcc-sessions shares nothing, and that is the point

Every other workload in the registry puts `level` goroutines onto ONE shared
fixture, so its scaling column reports how that fixture behaves under sharing.
This one cannot: `sim.RunMVCCSessions` builds its own `sim.SimDisk`, its own
store and its own engine on every call, and the mode is single-goroutine
internally. N concurrent operations are N independent simulations touching no
common object.

That makes it useless as a lock probe and valuable as a different one. **The
reliability mandate forbids hidden global state, so N shares-nothing simulations
ought to scale with the cores available to them.** They reach only **2.169x at 8
and 2.839x at 64**, where a shares-nothing workload on 10 cores should approach
the 5-6x the other CPU-bound workloads reach (`search-bfs-csr` 5.819,
`index-btree-rw` 5.750). It has no ceiling arm, because there is nothing shared
to unshare.

**This is recorded as an open question, not as a finding.** The obvious
candidate is GC and allocator pressure — each operation builds and discards a
whole store — and that has NOT been measured. It is not evidence of a lock.

### What is NOT covered

- Crash and recovery paths — `dst-disk-fault-wal` reaches the fail-stop branch
  and `sim.MVCCSessionsConfig.Crash` is left at its zero value, so no workload
  restarts a store and replays a WAL under concurrency.
- `sim.OpenSimStore`'s full durable stack, for the constructibility reason above.

## The error columns are backpressure, and the first probe hid half of it

Two cells reported non-zero errors, both only at 1024: `bolt-connect-churn`
**1691 / 30000 (5.6%)** and `dst-concurrent-bolt` 31. Round 1 published "0
errors", so the column is new and had to be explained rather than assumed.

Reproduced by driving the workload's own op at 1024 goroutines:

| start shape | errors | distinct classes |
|---|---:|---|
| staggered | 9 / 30720 (0.03%) | 1 |
| release barrier, matching `drive` | **2163 / 30720 (7.04%)** | **2** |

**The staggered probe was off by 180x on rate and missed an entire error class.**
With the barrier the two classes are 1934 x `connect: sim: handshake read: EOF`
and 229 x `connect: sim: handshake write: sim: SimConn is closed`.

Root cause, read from the source at `bolt/server/serve.go:768-779`:
`defaultMaxConnections` is **1024** (`serve.go:28`) and the workload runs exactly
1024 dialling goroutines. On a full semaphore the accept loop increments
`metricConnRejected`, logs `bolt: max connections reached, rejecting`, and closes
the socket. Both client messages are that one close, seen from either side of the
handshake.

**Not a defect.** Saturation is answered with a refusal that is counted and
logged, which is what the reliability mandate requires. The Bolt handshake has no
pre-negotiation error frame, so a closed socket is the only refusal available at
that point.

## Wiring the MVCC driver fired its oracles immediately

`dst-mvcc-sessions` reported **3 errors at level 1** — a single goroutine, seeds
0..299, deterministic by construction and therefore replayable. Replaying them
gives two distinct classes.

### Class A — an ACID_CONSISTENCY violation (seed 29)

```
[ACID_CONSISTENCY] tick=60 op="edge count": edge-count mismatch: oracle=3 engine=4
```

Established, not inferred:

- **Deterministic**: identical on 3 of 3 replays, same counts every time
  (committed=11, rolledback=3, statements=35).
- **Transient**: fires at `Ticks` 60 and 61, clean at 55-59 and at 62-72. The
  divergence heals.
- **It is the TERMINAL check, not an in-loop one.** `CheckEvery` normalises to 1,
  so parity is verified at every tick and the loop stops at the first failure.
  A run of 70 ticks passes through tick 60 and stays clean, so tick 60's in-loop
  check passes; the failure is the check at `mvcc_sessions.go:367`, which runs
  after the drain has rolled back every open transaction.
- **The schedule leaves two interleaved transactions open at the drain**: at tick
  56 session 0 runs an uncommitted `CREATE (a)-[:KNOWS]->(b)` from `mv-s0-m2`,
  and at tick 60 session 2 runs an uncommitted `DETACH DELETE` of that same node
  `mv-s0-m2`.
- **Neither shape leaks on its own.** Both were tested in isolation against a
  fresh engine — an uncommitted `CREATE` of an edge, then rollback, and an
  uncommitted `DETACH DELETE` of a node with an edge, then rollback — and the
  edge count is unchanged in both. **The defect needs the interleaving.**

Which side is wrong — a rolled-back edge surviving in the engine, or the oracle
dropping one it should keep — is NOT established here and must not be assumed.
Registered as its own task; reducing it to a minimal reproduction is that task's
first job.

### Class B — the scenario generator emits statements its own graph forbids

Seeds 102 and 215 return a run error, not a violation:

```
exec: CreateRelationship AddEdge: cypher: cannot create a parallel edge on a
non-multigraph graph ... (between "__cx_1135" and "__cx_1135")
```

The generator's `CREATE (a)-[:KNOWS]->(b)` can draw a pair that already has a
`KNOWS` edge, and — seed 215 — can draw `a == b`. The engine refuses correctly
with a typed error; the mode's graph is not a multigraph. **This is a harness
defect in `internal/sim`, not a module defect**, and it makes roughly 0.7% of
seeds abort a `RunMVCCSessions` run that should have completed.

## Validity control

`cypher-write-mem` measured against **itself**, carried in the same probe run
that produced every ceiling above, arms interleaved and alternating exactly as
the real pairs:

| level | base | ceiling | ratio |
|---|---:|---:|---:|
| 1 | 367,451/s (1.27%) | 369,030/s (0.83%) | **1.004x** |
| 8 | 767,412/s (2.46%) | 769,231/s (1.21%) | **1.002x** |
| 1024 | 517,058/s (4.69%) | 502,092/s (1.98%) | 0.971x |

The 1024 cell's 2.9% gap does not clear the base arm's own 4.69% spread, so by
this document's own working rule it is 1.00x within noise, not a 3% effect.

## Correction: site 2 IS on an engine path

**This document first stated that `graph/index.Manager` had no engine caller.
That was wrong, and the error is recorded here rather than quietly edited out.**

The claim came from a `grep` whose output was truncated at twenty lines. Every
line that survived the truncation was a doc comment, so the absence of calls
looked established when it had merely been cut off. A type-aware cross-reference
over the module (`golang.org/x/tools/go/packages`, resolving each identifier to
its `types.Object`) finds **113 references to the `Manager` type across 28
production files**, and a direct count confirms **88 non-comment production
references** to `index.Manager` under `cypher/`, `graph/`, `store/` and `bolt/`.
Two of them, read and verified:

- `cypher/exec/index_writeback.go:45` — `IndexBuffer.Commit` calls
  `mgr.ApplyBatch(b.changes)`, so **every buffered index change on the write path
  goes through the Manager**.
- `graph/query/index_seek.go:494` — the seek planner calls `mgr.ListIndexes()`
  and `mgr.GetIndex(name)` on the read path.

The two godoc quotes that misled me say something narrower than I read into them.
`graph/index/label/index.go:16` is about the **`label.Index` type as a
`Subscriber`**, not about the Manager, and the same comment says plainly that
"every production call site registers a btree or hash index". `graph/query/query.go:12`
("a future iteration will plug in") is simply **stale** — `index_seek.go` already
does it.

**One nuance survives, and it matters for the fix.** The measured workload drives
`Manager.Apply` (singular), and `Apply` itself has **zero** production callers:
production batches through `ApplyBatch`. But both take the same `m.mu.RLock()`
over the same subscriber set (`manager.go:254` and `manager.go:262`), so the
contended object is the one the engine really uses. What differs is the
**frequency**, because production amortises many changes into one batch. The
32.223x ceiling is therefore a real ceiling on a real lock, measured through an
entry point the engine does not itself call — treat it as an upper bound on what
the engine could recover, not as throughput the engine is losing today.

Site 1 is unaffected by this correction. `graph/generation.Publisher` really has
no production importer, and `graph/generation/generation.go:33-40` says so
deliberately: "This package is NOT a second snapshot mechanism in the engine —
Nothing in the module uses it ... It is a utility a consumer may use to cache a
derived structure." It is consumer-facing API, so its 0.094x scaling is a defect
in something the module publishes, not a cap on the module's own queries.

## Correction: the write-barrier rows rank a CUMULATIVE frame, and the durable ceiling is confounded

**Rows 5, 8 and 9 of the ranked inventory attribute blocked time to
`cypher/api.go:18379` `execUnderBarrier`. That frame holds ZERO of it.**
Established by rmp #2697 and verified independently from the same profiles:

| workload | `execUnderBarrier` flat | cumulative |
|---|---:|---:|
| `cypher-write-mem`@1024 | **0 (0%)** | 84.90% |
| `mvcc-session-write`@1024 | **0 (0%)** | 89.62% |
| `dst-disk-wal`@1024 | **0 (0%)** | 99.08% |

The ranking above was produced with `go tool pprof -top -cum -lines`. Cumulative
attribution locates a *subsystem*; it does not identify where goroutines
actually block, because a parent frame inherits everything beneath it.
`execUnderBarrier` wraps the whole write path, so it inherits all of it. **A
cumulative share is not a bottleneck**, exactly as a mutex share is not a
ceiling — the same lesson this document already records, arrived at from the
other direction.

Worse, the frame's name misled the reading and its own godoc did not: it says
"THE NAME IS HISTORICAL … concurrent writers DO run alongside this one". The
bracket is held **shared**, and `mvcc.Gate`'s weak path is an atomic add on a
striped padded slot, not an RWMutex. Writers are not serialised there.

**Where the delay actually is**, from the same profiles:

- `cypher-write-mem` — `lpg.setNodeLabelInfo` **46.0%**, `HasNodeLabel` 11.7%,
  `Mapper.Lookup` 9.3%, `planCache.get` 6.4%, antlr 5.2%
- `mvcc-session-write` — `setNodeLabelInfo` 36.5%, `Mapper.Lookup` 10.2%,
  `label.Index.mutate` 9.7%, antlr 9.2%, `AwaitVisible` 9.2%
- `dst-disk-wal` — `waitApplyTurn` **49.9%**, `wal.AppendRun` 22.2%,
  `wal.syncToLocked` 24.4% — about 99% in `store/wal` plus the apply gate

**And the 3.860x durable ceiling is confounded.** `dstDiskCeiling`
(`bench/contention/ceiling_arms.go:475`) builds a fresh `sim.NewSimDisk` per
replica, so the arm unshares the harness's own simulated-disk mutex alongside
the module's structures. It is not a clean measure of what the engine could
recover, and row 5 must not be read as one.

One further measured caution recorded here because it bounds every ratio in
this document: the write path already runs at **649%, 669% and 506% of ten
nominal cores**, against this host's practical ceiling of **7.61x, not 10x**
(4 performance + 6 efficiency cores). At ~650% it is near capacity, and the
blocked time is a symptom of hundredfold oversubscription rather than its cause.

## What the evidence says to do next

Ranked by ceiling weighted against reachability.

1. **`graph/generation` refcount — 47.6x, exported, deliberately unwired.** One
   shared `atomic.Int64` per generation. The obvious remedy is a striped or per-P
   refcount summed on publish. Highest ceiling in the module by a factor of 12,
   and it caps a consumer's throughput rather than the engine's.
2. **`graph/index` `Manager` fan-out — 32.2x at 1024, and it IS wired.** One
   `RWMutex` over the whole subscriber set, taken by `ApplyBatch` on every
   write-path index writeback and by `ListIndexes`/`GetIndex` on the seek path.
   Measured through `Apply`, which the engine does not call; see the correction
   above for what that does and does not license you to claim.
3. **`store/wal` durable commit — 3.860x at 8, on the engine path.** The highest
   ceiling of any wired site at the concurrency the hardware can actually serve.
4. **`internal/metrics.IncCounter` — 3.300x at 8** (and that is bought while
   paying a 37% locality handicap, so the true figure is higher). With the real
   Prometheus backend installed the surface scales at 0.445x: eight goroutines
   emit metrics at less than half the rate one does. What enabling metrics costs
   against the no-op default was NOT measured here and must not be inferred from
   this number.
5. **`graph/index/count` hot type — 2.916x at 8, on the engine path.** Round 1's
   #2682 fixed the *spread* case; the *single hot type* case is still 0.328x, and
   is atomic contention rather than lock contention.
6. **`graph/index/hash` Insert — 1.960x at 8.** #2692 cut its blocked time from
   1199.90 s to 273.74 s and turned 0.916x into 1.742x, but the site still holds
   97.91% of its profile and there is roughly 2x left behind it.
7. **The write barrier `cypher/api.go:18379` — 1.70x at 1024.** Modest, but it is
   the single point every write in the module passes through.

Stop below that line. `graph/index/btree` reads 0.948x / 1.005x and
`lpg-neighbours-read` reads 1.004x / 1.001x: their sharing costs nothing, and no
amount of sharding would repay the effort.
