# Concurrency assessment — GoGraph against Neo4j and Memgraph

**Date:** 2026-08-11 · **GoGraph:** `b3887752` · **Neo4j:** 5.26-community (source at tag
`5.26.9`, `650f2f826dd`) · **Memgraph:** 3.9.0 (source at tag `v3.9.0`, `9f6fa8b83`) ·
Apple M4 (10 cores), 32 GB, colima VM `aarch64` with 8 CPUs / 16 GB.

This document answers one question: **how does GoGraph behave under concurrent load compared
with the two engines that solve the same problem, and what are the specific, verified reasons
for the differences.** It is not a general performance comparison — the single-client
head-to-head lives in `docs/benchmarks/threeway-2026-07-26.md` and its conclusions are not
restated here.

Every mechanism claimed below was read in the competitor's own source at the version under
test, and every number was produced in this cycle by the harness described in §2.

---

## 1. What is being compared, and what that means

"Concurrency capability" is not throughput. An engine can be fast per operation and unable to
use a second core; another can be slow per operation and scale linearly. The two are
separate properties and this assessment reports them separately:

- **Scaling** — throughput at concurrency *N* divided by that same engine's throughput at
  concurrency 1. This is the concurrency property. It is self-normalised, so it survives the
  platform differences that absolute throughput does not.
- **Absolute throughput and latency** — reported because they are what was measured, and
  because a scaling curve without them can flatter an engine that starts from a low base.

---

## 2. Method, and the three defects found in it before it was trusted

The harness is `bench/comparison/concurrency_test.go` (build tag `threeway`), driven by
`bench/comparison/ggserver`. Every target is a **CPU-capped server process reached over TCP by
the same client** — `neo4j-go-driver/v5` — with a connection pool sized above the highest
concurrency level.

| target | what it is |
|---|---|
| `gograph-bolt` | `bench/comparison/ggserver`, `GOMAXPROCS=4`, in a container, `--cpus=4` |
| `neo4j-bolt` | `neo4j:5.26-community`, `--cpus=4`, 4 GB heap, 2 GB page cache |
| `memgraph-bolt` | `memgraph/memgraph:3.9.0 --telemetry-enabled=false`, `--cpus=4` |

Three workloads, each with an oracle asserted on **every** operation: `read_point` (indexed
point lookup, exactly 1 row), `read_2hop` (two-hop expansion aggregated, exactly 1 row),
`write_create` (autocommit `CREATE`, keys disjoint per client). Fixed 3 s window per arm after
a 2 s warm-up at the same concurrency; 3 interleaved rounds; median reported. Every target is
loaded with a byte-identical 5 000-node / 40 000-edge graph and the node and edge counts are
compared across targets before any timing is.

**All three images are native `linux/arm64` and were verified at runtime** (`uname -m` inside
each container; Neo4j's JVM reports `os.arch = aarch64`). Nothing runs under qemu emulation —
an emulated competitor would have invalidated every figure here.

### 2.1 The harness was wrong three times, and each error favoured a different engine

None of these were visible from the numbers alone; each was found by asking what a suspicious
figure would have to mean.

1. **The transport, not the engine, was being measured.** In the first configuration GoGraph
   ran natively on the host while the rivals ran in the VM. Same GoGraph binary, same CPU
   budget, reached two ways: **p50 29 µs natively against 213 µs through the VM's
   port-forward.** Memgraph and GoGraph both sat at ~4 700 ops/s with p50 ≈ 200 µs — *an
   identical floor for two unrelated engines is the signature of an external constant*, and
   that constant was colima's port-forwarding. Fixed by containerising GoGraph too and moving
   the client **inside** the VM, onto the same container network. The transport floor fell
   from ~200 µs to ~50 µs, which is what made engine differences visible at all.

2. **One competitor was allowed to run cold.** The original harness gave each client a single
   warm-up operation. Neo4j runs on a JVM that compiles hot paths only after thousands of
   iterations, and its conc=1 result swung between 1 554 and 3 675 ops/s across workloads
   purely on how warm it happened to be. Adding a proper warm-up — the same concurrent load,
   at the same concurrency, discarded — moved Neo4j's conc=1 `read_point` from **1 554 to
   5 658 ops/s, a factor of 3.6**. A comparison that lets one competitor run cold is not a
   comparison; every number in §3 is post-warm-up.

3. **The write oracle was reporting a defect that was not one.** It first flagged "lost
   writes" of −2 to −8, i.e. *more* nodes than the client had counted. That is the client
   undercounting: when the window closes, an operation already on the wire commits after its
   caller stopped counting. The oracle now bounds that excess at one in-flight operation per
   client per window and treats only the **positive** direction — an acknowledged `CREATE`
   the engine cannot afterwards produce — as a defect.

### 2.2 Declared limits — what these numbers may not be used for

- **Durability is not comparable across the write arm.** GoGraph's `ggserver` is in-memory
  with no WAL. Memgraph's WAL is **off by default** — verified in its source
  (`src/flags/general.cpp:77`, `DEFINE_bool(storage_wal_enabled, false, …)`) and the container
  runs with no WAL flag. **Neo4j fsyncs every commit and cannot readily be told not to.** So
  the write arm compares two non-durable engines against one durable one, and Neo4j's write
  figures must not be read as a concurrency result. GoGraph's own durable write scaling is
  measured separately in `docs/benchmarks/release-delta-v0.10.0-to-head-2026-08-10.md` §3.
- **Absolute cross-engine throughput still contains the JVM, each engine's Bolt
  implementation, and the VM.** The scaling column is the defensible comparison.
- Single host, single run of 3 rounds. Cross-sweep comparisons are not valid; only matched
  arms within this sweep are.

---

## 3. Results

5 000 Person / 40 000 KNOWS, 3 s per arm after a 2 s warm-up, 3 interleaved rounds, median.
**Zero errors, zero wrong row counts, in all 126 arms.**

### 3.1 `read_point` — indexed point lookup

| target | 1 | 2 | 4 | 8 | 16 | 32 | 64 | scaling 1→64 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **gograph-bolt** | 16 867 | 31 020 | 44 059 | 81 147 | 132 495 | 143 638 | **147 829** | **8.76×** |
| neo4j-bolt | 5 766 | 11 079 | 16 059 | 28 821 | 33 958 | 36 334 | 37 117 | 6.44× |
| memgraph-bolt | **20 059** | 28 936 | 34 424 | 44 221 | 56 584 | 67 191 | 77 488 | 3.86× |

### 3.2 `read_2hop` — two-hop expansion, aggregated

| target | 1 | 2 | 4 | 8 | 16 | 32 | 64 | scaling 1→64 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **gograph-bolt** | 14 158 | 26 284 | 38 821 | 71 868 | 105 906 | 110 476 | **112 990** | **7.98×** |
| neo4j-bolt | 5 323 | 10 498 | 15 358 | 26 845 | 30 548 | 31 466 | 33 926 | 6.37× |
| memgraph-bolt | **17 763** | 27 447 | 32 498 | 38 958 | 52 064 | 58 284 | 65 615 | 3.69× |

### 3.3 Latency at the tail (`read_2hop`)

| target | p50 @1 | p50 @64 | **p99 @64** |
|---|---:|---:|---:|
| gograph-bolt | 68 µs | 404 µs | **2.63 ms** |
| memgraph-bolt | **54 µs** | 567 µs | 7.11 ms |
| neo4j-bolt | 183 µs | 1.19 ms | **27.47 ms** |

### 3.4 What the numbers say

1. **Memgraph is the fastest engine for a single client and the worst at scaling.** It leads
   at concurrency 1 on both workloads — 20 059 vs GoGraph's 16 867 on `read_point`, and the
   best p50 anywhere at 48–54 µs — and finishes last in scaling at 3.69–3.86×. **Its very
   first step already shows it: 1→2 clients gains only 1.44×**, against 1.84× for GoGraph and
   1.92× for Neo4j. A ceiling reached at the second client is a serialisation point, not a
   capacity limit.
2. **GoGraph has both the best scaling and the highest absolute throughput at every level
   from 2 clients upward.** At 64 clients it is **1.91× Memgraph and 3.98× Neo4j** on
   `read_point`. It overtakes Memgraph between 1 and 2 clients and never gives the lead back.
3. **GoGraph owns the tail.** At 64 clients its p99 is 2.63 ms against Memgraph's 7.11 ms
   (2.7×) and Neo4j's 27.47 ms (**10.4×**). Neo4j's tail is the JVM's: its p99 rises far
   faster than its p50, which is the shape a garbage collector makes.
4. **GoGraph saturates near 16 clients** (7.86× at 16, 8.52× at 32, 8.76× at 64 on
   `read_point`), then degrades into queueing rather than collapsing — p50 rises while
   throughput holds. No arm produced an error or a wrong row at any level.

**Why scaling above 4× is possible on a 4-CPU allocation, and what that means for reading
these ratios.** A single client is round-trip-bound, not CPU-bound: at 58 µs per operation it
leaves most of one core idle, so the concurrency-1 baseline is well below one core's capacity
and the headroom to 64 clients covers both added parallelism *and* the latency being hidden.
The ratios are therefore not "cores used" — they are how much of its own single-client
limitation each engine can recover. That is precisely why all three are compared on the same
ratio, on the same allocation, over the same transport.

### 3.5 All three saturate their CPUs — so this is not idle waiting

Sampled during a 64-client `read_2hop` load, engine container CPU against a 400 % allocation:
**GoGraph 392–397 %, Memgraph 403–405 %, Neo4j 405–405 %.**

This **refuted** the first explanation attempted for Memgraph's curve. It is not failing to
use the cores it was given; it uses all of them and converts less of that CPU into completed
queries as concurrency rises — the signature of contention that burns CPU (futex wake-ups,
cache-line transfer) rather than blocking idle. Any claim that Memgraph "cannot use its cores"
would have been wrong, and the measurement is what caught it.

### 3.6 The causal probe: amortising the per-query transaction setup

Code reading and a correlation do not establish cause. Memgraph acquires its storage accessor
in `SetupDatabaseTransaction`, which `interpreter.cpp:8466` guards with
`if (!in_explicit_transaction_)` — so running 20 queries inside **one explicit transaction**
divides the acquisitions by 20 while leaving the query work identical. If the acquisition is
what limits scaling, batching must lift it.

Matched pair, identical query and parameters, differing only in transaction granularity
(the batched arm also pays **two extra round trips per 20 queries**, so it starts at a
disadvantage):

| engine | autocommit @64 | 20-per-transaction @64 | Δ throughput | scaling 1→64 |
|---|---:|---:|---:|---|
| **memgraph-bolt** | 65 243 | 79 118 | **+21.3 %** | 3.70× → **4.18×** |
| neo4j-bolt | 32 864 | 36 229 | +10.2 % | 6.18× → 6.59× |
| **gograph-bolt** | 112 411 | 105 699 | **−6.0 %** | 7.85× → 7.35× |

**The size of the gain is ordered exactly as the cost of each engine's per-query gate is
ordered in §4.1.** Memgraph, whose gate is a full mutex taken twice per query, gains most and
more than pays for the extra round trips. Neo4j, whose gate is a CAS plus a pooled kernel
transaction, gains half as much. GoGraph, whose gate is a striped atomic, gains nothing at all
and simply pays the extra round trips — which is what "already amortised" looks like.

This demonstrates that **per-query transaction setup is what limits Memgraph's read scaling**,
and §4.1 identifies from its source the serialising component inside that setup. It does not
isolate the mutex from the rest of the setup; see §7.

---

## 4. Why: the mechanisms, read in each engine's source

The measured differences are not diffuse. They come from one decision each engine made about
**what a query must acquire before it can touch data**, and a second about **what a commit must
serialise on**.

### 4.1 The per-query gate — the read path

Every one of these three engines makes a query announce itself before it reads. What separates
them is the primitive.

| engine | what a read query acquires | cost shape |
|---|---|---|
| **GoGraph** | `Horizon.EnterHolding()` → CAS on a **striped, cache-line-padded** occupancy word, then an atomic store of the start timestamp (`graph/mvcc/horizon.go:177`, `:272`) | no globally shared line |
| **Neo4j** | `newTransactionsLock.readLock().tryLock()` — a `ReentrantReadWriteLock` (`KernelTransactions.java:317`), then **seqlock** page reads | one shared word per transaction; data reads lock-free |
| **Memgraph** | `ResourceLock::lock_shared()` — which takes a **real exclusive `std::mutex`** to increment a plain `uint32_t` (`src/utils/resource_lock.hpp:131-138`) | full mutex, **twice per query** |

**Memgraph's "shared" lock is shared in semantics only.** `lock_shared()` opens with
`auto lock = std::unique_lock{mtx};` and `unlock_shared()` does the same, so every transaction —
reads included — serialises on one process-wide `std::mutex`, once on acquisition and once on
release. It is taken per **query**: `CurrentDB::SetupDatabaseTransaction` calls
`db_acc->Access(...)` at `src/query/interpreter.cpp:214`, in the per-query transaction setup.
The data structures underneath are not the constraint — vertices and edges live in a
`utils::SkipList` that is "mostly lock-free" by its own documentation
(`src/utils/skip_list.hpp:437`). The gate in front of them is.

**Neo4j's read path is the strongest of the three below the transaction gate.** Page reads use
an optimistic seqlock: `tryOptimisticReadLock` is a plain read of a state word
(`getState(address) & SEQ_MASK`) and `validateReadLock` re-checks it afterwards
(`OffHeapPageLock.java:138-152`). Readers never write shared state, so no reader invalidates
another reader's cache line. Its per-transaction `ReentrantReadWriteLock` is a CAS on one
shared word — cheaper than Memgraph's full mutex, but still a single contended line.

**GoGraph is the only one of the three with no globally shared word on the read path**, and it
got there deliberately. `graph/lpg/lpg.go:642` records the reasoning: as a `sync.RWMutex` the
shared acquisition "was an atomic add on that mutex's ONE readerCount word, so every write on
every core took a coherence miss on a single shared line purely to announce a NON-conflict",
and rmp #2203 measured that shape **degrading 17.6× from 1 to 10 cores**. The replacement,
`mvcc.Gate`, stripes the weak side over padded per-slot counters: **3.77 ns at 1 core falling
to 0.434 ns at 10, where the RWMutex rises from 3.75 ns to 89.5 ns**
(`docs/benchmarks/mvcc-weak-strong-gate-2026-08-07.md`). The padding unit is 128 bytes rather
than 64 "because Apple silicon prefetches pairs of lines" (`graph/mvcc/horizon.go:106`).

The consequence is that **GoGraph rejected, with measurement, the exact primitive Neo4j still
uses on this path and a cheaper one than Memgraph's.**

### 4.2 The commit — the write path

| engine | what serialises | extent of the critical section |
|---|---|---|
| **GoGraph** | nothing globally; commits proceed concurrently, and readers are given a safe instant by a **contiguous commit frontier** | timestamp allocated *at commit*, not at BEGIN |
| **Neo4j** | a **dedicated appender thread per database**, fed by a queue; committers wait on a future | append + rotation moved off the committing threads |
| **Memgraph** | a global `engine_lock_` **spin lock** (`inmemory/storage.cpp:998`) | commit-timestamp assignment **+ unique-constraint validation + the WAL append** |

Memgraph's own comment states why the WAL append is inside the lock: "Write transaction to WAL
while holding the engine lock to make sure that committed transactions are sorted by the commit
timestamp in the WAL files."

Neo4j's design is the one this project's round-4 audit already identified as the model to take.
Its introduction (`6456283b390`, 2021-08-17, MishaDemianenko) states the problem in the
commit message: previously "transaction threads … acquire log file lock and do append themself",
racing again afterwards for rotation; the change introduces "a dedicated log writer thread per
database" with a queue, so an individual transaction "is posting transaction that should be
committed to a queue and waits on a future".

GoGraph's commit frontier is **explicitly derived from Memgraph's**, and its source says so.
`graph/mvcc/commitlog.go` records reading both PostgreSQL's `GetSnapshotData` xip array and
Memgraph's `CommitLog` bitset, and takes Memgraph's shape for three stated reasons — the read
path stays one atomic load and one comparison; GoGraph's in-flight window is a *commit* rather
than a transaction, because the timestamp is allocated at commit time; and the memory bound is
structural rather than configured. It also states the cost it accepts: a reader cannot observe
a commit above the oldest unfinished one, so staleness equals the longest in-flight commit.

---

## 5. The decisions each project took to maximise concurrency

Traced through the full history of both repositories (Neo4j: 85 897 commits; Memgraph: 5 126).

### Neo4j

| when | decision |
|---|---|
| 2014-02-22 | **Forseti lock manager** (`a45af2a7c3e`, Jacob Hansson) — dreadlocks deadlock detection in O(1), lock acquisition in one CAS in the best case |
| 2016-09-05 | **Off-heap page list with seqlock page locks** (`81d661fd0be`, Chris Vest) — optimistic, write-free reads |
| 2021-08-17 | **`TransactionLogQueue`** (`6456283b390`) — dedicated appender thread replaces threads racing for the log lock |

Forseti's own javadoc is unusually candid about its ceiling, and it is worth quoting because it
is a documented scaling limit rather than an inferred one: it "scales linearly with the number
of cores", but "since it uses a shared-memory approach, it will most likely degrade in use
cases where there is high contention and a very large number of sockets", and it is therefore
"optimized for servers with up to, say, 16 cores across 2 sockets".

### Memgraph

| when | decision | in 3.9.0? |
|---|---|---|
| 2025-04-15 | **Parallel read-only access types** (`d788b3f3c`, #2798) — splits storage access into READ / WRITE / READ_ONLY / UNIQUE so periodic snapshots can run alongside READ queries | yes |
| 2025-04-25 | **`Improve ResourceLock`** (`e24cf4631`, #2920) — prioritise READ_ONLY over WRITE, allow downgrading to READ | yes |
| 2026-07-24 | GC takes `main_lock_` in READ instead of WRITE (`bc1af3a85`) | **no** — master only |
| 2026-07-27 | waiting UNIQUE given priority over new shared acquirers (`2963b43e9`) | **no** — master only |
| 2026-07 | **"parkable Prepare" / coroutine accessor-yield** — lets a query yield instead of blocking while acquiring storage access | **no** — unmerged branch `origin/pr/coro-prepare-accessor-yield` |

The last row is the most informative single fact in this history. Memgraph is **actively
building machinery to stop queries blocking while they acquire storage access** — which is
independent confirmation, from the project itself, that the acquisition analysed in §4.1 is a
real contention point and not an artefact of how this assessment read the code. It is also
**not in the version measured here, nor in master**, so it changes nothing about §3; it
indicates where Memgraph is heading.

---

## 6. Findings against GoGraph

The read-concurrency result is strong and needs no qualification beyond §2.2. The findings
below are what the same harness exposed while producing it.

### F1 — Bulk node delete is quadratic and single-threaded (rmp #2400, severity 7)

**This is the one defect of the assessment, and it is a defect against a stated mandate**
("no unbounded growth", "waste is a defect").

Deleting the *same* 40 000 `:Tmp` nodes, five times, against one live engine:

| cycle | GoGraph | Memgraph |
|---:|---:|---:|
| 1 | 3.279 s | 10 ms |
| 2 | 6.312 s | 12 ms |
| 3 | 9.331 s | 14 ms |
| 4 | 12.366 s | 16 ms |
| 5 | **15.656 s (4.8×)** | 16 ms |

Growth is linear at ~3.1 s per cycle for identical work, and the container sits at
**100.08 % CPU — exactly one core of four** — throughout. Within a single wipe the per-batch
cost is *flat* (~410 ms per 5 000), and the `count` before each batch stays at 2–3 ms, so the
scan is not the problem: the cost is inside the delete and it scales with the number of nodes
**ever** deleted, not with the number being deleted now.

Per node: **GoGraph 82 µs, Neo4j 2.2 µs, Memgraph 0.2 µs** — 37× and 410× respectively, before
any degradation.

> **CORRECTED 2026-08-11, after the fix was measured. The root cause stated below in
> ~~struck-through~~ form was WRONG.** It was read in the source and never priced, and
> "confirmed at source" meant confirmed to *exist*, not confirmed to *cost*. The tombstone
> clone is **0.99%** of the CPU profile of this very workload. The real cause is
> `lpgMutatorAdapter.InNeighbours` answering "what points at this node" with a walk of
> **every interned node** — 78.77% of the profile — once per node deleted, which is why the
> cost grew with the nodes ever interned rather than with the tombstones. Full attribution,
> both measurements, and the fix are in
> [`benchmarks/delete-in-edge-index-2026-08-11.md`](benchmarks/delete-in-edge-index-2026-08-11.md).
> The finding itself stands: it was a real defect, reachable as a failure, and it is now
> fixed and gated.

~~**Root cause, confirmed at source.** `graph/lpg/lpg.go:2888-2899` (`removeNodeInfo`) takes the
global `tombstoneMu` and calls `cur.Clone()` — a deep copy of the entire roaring64 tombstone
bitmap — **once per node removed**. Removing *k* nodes with *t* existing tombstones is
O(k·t), and because the clone runs under one process-wide mutex the whole bulk delete
serialises on a single core however many are available.~~

~~The copy-on-write design is deliberate and its rationale (`lpg.go:453-467`) is sound for
*readers*, who stay lock-free — this is the same instinct that makes §4.1's read path the best
of the three. What fails is its stated premise: "The clone cost is O(tombstones) and paid only
on the **rare** delete/revive." A bulk delete is not rare, and nothing enforces the premise.~~

The copy-on-write tombstone design is untouched by the fix, and its stated premise — *"the
clone cost is O(tombstones) and paid only on the rare delete/revive"* — turns out to hold in
practice for a different reason than it claims: a dense id range compresses to a handful of
roaring containers, so the clone is nearly independent of cardinality. Removing 2 000 nodes
costs 831 ns each with no tombstones present and 1 273 ns each with 160 000 — a factor of
1.53 across an eightyfold increase in the set being cloned.

**It is reachable as an outright failure, not merely as slowness:** a single-statement delete
of ~90 000 nodes exceeds `DefaultTxTimeout` (30 s) and returns
`Neo.ClientError.Transaction.TransactionTimedOut`. This is how it was found — it broke the
benchmark's own fixture cleanup, twice, before it was diagnosed.

### F2 — The mandated concurrency levels are not all reachable through Bolt by default

`bolt/server.Options.MaxOpenTxPerPrincipal` defaults to 16
(`DefaultMaxOpenTxPerPrincipal`), so a single principal opening explicit transactions above
that level is refused. CLAUDE.md publishes 256 and 1024 as levels the module reports at.
`bench/comparison/ggserver` therefore sets it explicitly, exactly as `bench/soak` had to. The
server is behaving as configured; the point is that **the default configuration cannot reach
the concurrency the module publishes**, and every harness has to know that.

> **FIXED 2026-08-11, rmp #2419.** `DefaultMaxOpenTxPerPrincipal` is now 2048, above the
> highest published level, so the default configuration reaches the concurrency this module
> publishes. The consequence is worth stating: a connection holds at most one open
> transaction and `MaxConnections` defaults to 1024, so under the default configuration the
> per-principal quota can no longer bind — the connection ceiling bounds the resource, and
> the quota's remaining job is isolating one principal from another. The trade accepted is
> that every open transaction pins an MVCC read snapshot and holds the reclamation horizon
> back for its lifetime, so the bound on *that* resource is weaker;
> `DefaultMaxTxIdleTime` (5 s) limits the exposure. Gated by
> `bolt/server/txquota_published_levels_test.go`, which asserts the admitted
> concurrency rather than the constant.

---

## 7. Verdict — the state of the module

**On read concurrency, GoGraph is the strongest of the three, and the reason is architectural
rather than incidental.** It scales best (8.76× against Neo4j's 6.44× and Memgraph's 3.86×),
delivers the highest absolute throughput from two clients upward, and holds the tightest
latency tail by 2.7× and 10.4×. The mechanism is identified, measured in isolation, and
matched by a causal probe: GoGraph is the only one of the three whose read path touches **no
globally shared cache line**, having rejected — with its own measurement — the `sync.RWMutex`
shape that Neo4j still uses and a cheaper primitive than Memgraph's.

Three qualifications keep that verdict honest:

1. **Memgraph is still the better engine for a single client** — faster per query and lower
   p50. GoGraph wins by scaling, not by per-operation speed.
2. **The write comparison was not made.** Durability differs across the three (§2.2), so the
   write arm was withdrawn rather than published as a concurrency result.
3. **The delete path is a genuine defect** (F1) on an axis where both peers are orders of
   magnitude better, and it degrades without bound under churn. **Fixed on 2026-08-11**, in
   sprint 342, once its cause was measured rather than inferred: 59.9× at the sixth wipe
   cycle and flat thereafter, for +1.92% on end-to-end relationship creation. The
   correction to this document's own attribution is in §6.

## 8. What this assessment did not establish

- **That Memgraph's `std::mutex` specifically, rather than the rest of its per-query
  transaction setup, is the limiter.** §3.6 establishes that the *setup* is the limiter and
  §4.1 identifies the serialising component within it from source; isolating the mutex would
  need a profile inside the container or a patched build.
- **Write concurrency across the three engines** — not comparable at equal durability here.
- **Behaviour above 64 concurrent clients**, and behaviour on a larger-than-memory dataset.
  Every engine held the working set in memory; page-cache behaviour under pressure is
  untested, and it is the regime Neo4j is most engineered for.
- **Anything about GoGraph's durable write path**, which `ggserver` deliberately omits.
- ~~**Whether F1 has an equivalent on the edge-delete path.** Only node delete was measured.~~
  **ANSWERED 2026-08-11 (rmp #2418): it did, with the same single cause.** `DETACH DELETE`
  degraded 5.04× over six cycles where node delete degraded 4.67×, because both reach the
  same in-neighbour scan. Both are fixed and gated; see
  [`benchmarks/delete-in-edge-index-2026-08-11.md`](benchmarks/delete-in-edge-index-2026-08-11.md).
- **Multi-socket behaviour.** One 10-core Apple M4; Forseti's own javadoc warns that its
  characteristics change on large multi-socket machines, and nothing here tests that.

---

## 9. Reproduce

```bash
docker network create ggbench
docker run -d --name gg-neo4j --network ggbench --cpus=4 -m 8g \
  -e NEO4J_AUTH=neo4j/gographbench neo4j:5.26-community
docker run -d --name gg-memgraph --network ggbench --cpus=4 -m 8g \
  memgraph/memgraph:3.9.0 --telemetry-enabled=false
GOOS=linux GOARCH=arm64 go build -o ggserver ./bench/comparison/ggserver   # image gograph-bench:local
docker run -d --name gg-gograph --network ggbench --cpus=4 -m 8g -e GOMAXPROCS=4 gograph-bench:local

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -tags=threeway -c -o ccbench ./bench/comparison/
# image ccbench:local, then:
docker run --rm --network ggbench --cpus=4 -m 4g \
  -e CC_ONLY="gograph-bolt,neo4j-bolt,memgraph-bolt" -e CC_WORKLOADS="read_point,read_2hop" \
  -e CC_GOGRAPH_URI="bolt://gg-gograph:7689" -e CC_NEO4J_URI="bolt://gg-neo4j:7687" \
  -e CC_MEMGRAPH_URI="bolt://gg-memgraph:7687" \
  -e CC_LEVELS=1,2,4,8,16,32,64 -e CC_ROUNDS=3 \
  ccbench:local -test.run TestConcurrencySweep -test.v -test.timeout 120m

# F1, the delete defect:
docker run --rm --network ggbench -e CC_CYCLES=5 -e CC_ONLY="gograph-bolt,memgraph-bolt" \
  ... ccbench:local -test.run TestDeleteBatchScaling -test.v
```

The client must run **inside** the VM (§2.1) or the measurement is of colima's port-forward.
