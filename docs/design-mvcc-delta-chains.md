# Design: per-object MVCC via delta chains

**Status:** approved for execution (user decision, 2026-07-31). **P0 and P1 delivered**; see
§7 for phase state and [`mvcc-p0-measurement.md`](mvcc-p0-measurement.md) for the measurement
that authorised the rest.
**Supersedes the conclusion recorded in rmp #2051.** See §4 — that conclusion is a
property of the design that was tried, not of MVCC.
**Motivating defect:** rmp #2274. **Also resolves:** rmp #2193, and unblocks #1825/#1826.

---

## 1. The defect this exists to remove

`Engine.Run` holds the graph read barrier — `lpg.Graph.View`, an `RLock` on
`visMu sync.RWMutex` — across both BUILD and DRAIN, so an analytical query holds
it for the query's whole duration. A write takes the same barrier exclusively,
via `lpg.Graph.ApplyAtomically`. Go's `sync.RWMutex` prefers a waiting writer, so
once a writer queues behind a long read, **every short reader arriving after it
parks until the long read finishes and the write completes.**

Measured 2026-07-31 at `f848e854`, Apple M4 (10 cores), durable engine over a
real WAL-backed `txn.Store`, 20 000 nodes with an index on `:P(w)`, against a
95.5-second analytical read:

| readers | baseline | + long read | + writer 10/s | **+ both** | collapse | worst short read |
|---|---|---|---|---|---|---|
| 1 | 219 917 op/s | 222 334 | 207 107 | **7 507** | **−29.3×** | **1m39.19s** |
| 8 | 394 676 op/s | 353 541 | 370 924 | **12 835** | **−30.8×** | **1m40.64s** |
| 64 | 374 840 op/s | 365 182 | 437 150 | **15 247** | **−24.6×** | **1m35.47s** |

Each ingredient **alone is free**. Only the combination collapses, which is the
signature of writer-preference queueing rather than of either component being
slow.

The sharper statement is not the throughput number but the latency one: **a
4.5 µs point query inherits the duration of an unrelated analytical query.** The
amplification is unbounded — a ten-minute report is a ten-minute read outage —
so this is a correctness-of-service defect, not a throughput trade-off.

A harness that used an *11.7 ms* long read measured only −8 % and would have
reported no problem. The stall lasts exactly as long as the longest concurrent
read, so any measurement of it must first prove its long read is long. The gate
in `bench/mtaudit/fairness_soak_test.go` asserts that calibration before it
asserts anything else.

## 2. What the incumbents actually do

Read from source, not documentation, as the project's prior-art rule requires.

| | global lock on an ordinary **read** | global lock on an ordinary **write** | writer preference applies to |
|---|---|---|---|
| **Neo4j** | none — reads take no locks | none global; per-node and per-relationship locks (Forseti) | n/a |
| **Memgraph** | `main_lock_`, **shared** | `main_lock_`, **shared** (mode `WRITE`) | `UNIQUE` only — index creation, storage-mode change |
| **GoGraph** | `visMu.RLock()` | `visMu.Lock()` — **exclusive** | **every write** |

**Memgraph** (`src/utils/resource_lock.hpp`, `src/storage/v2/storage.hpp`,
`src/storage/v2/vertex.hpp`, `src/storage/v2/delta.hpp`, read at `master`,
2026-07-31):

- `main_lock_` is a `utils::ResourceLock` with four modes — `UNIQUE`, `WRITE`,
  `READ`, `READ_ONLY` — of which the last three are all *shared* states.
  Ordinary read **and write** transactions take a shared mode. The header
  states the exclusive mode's purpose directly: *"Accessors take a shared lock
  when starting, so it is possible to block creation of new accessors by taking
  a unique lock. This is used when doing operations on storage that affect the
  global state, for example index creation."*
- That lock **is writer-preferring**, exactly as Go's `sync.RWMutex` is:
  `can_acquire<READ>()` requires `state != UNIQUE && unique_pending_count == 0`,
  and the source calls this out as *"A waiting UNIQUE gates new shared
  acquisitions (writer-preference)"*. **Memgraph does not avoid reader
  starvation by having a fairer lock. It avoids it by not taking the exclusive
  mode for ordinary writes.**
- Concurrency lives per object. A `Vertex` carries its own
  `mutable utils::RWSpinLock lock` and a `utils::PointerPack<Delta, 2> delta_`;
  there is no global structure a reader must traverse under a shared latch.
- A `Delta` records **one modification** — `DELETE_OBJECT`, `ADD_LABEL`,
  `REMOVE_LABEL`, `SET_PROPERTY`, `ADD_IN_EDGE`, `REMOVE_OUT_EDGE`, … — as a
  tagged union carrying only what changed (a `LabelId`; a `PropertyId` plus a
  value pointer; an `EdgeTypeId` plus an `EdgeRef`), with a `CommitInfo *`, a
  `command_id`, and `prev`/`next` links. It is bounded at **56 bytes** and is
  **O(1) per modification**. A reader reconstructs the version visible at its
  start timestamp by walking the chain backwards.

**Neo4j** (kernel transaction documentation, `kernel/src/docs/dev/`): *"reads do
not block or take any locks"*; write locks are acquired *"at the Node and
Relationship level"*, with a relationship write locking the relationship and
both endpoints. There is no global graph-wide lock, and the default isolation is
READ_COMMITTED.

**The convergent structural insight:** neither engine ever materialises a
whole-graph snapshot, and neither takes a global exclusive latch to perform an
ordinary write. Versioning is **per object and incremental**.

## 3. Why GoGraph currently cannot simply relax the barrier

GoGraph applies writes **eagerly** to a single, unversioned in-memory graph —
this is the documented contract at the top of `cypher/api.go`, and the in-memory
undo log (#1282) exists precisely to unwind those eager writes on error. By the
time a commit runs, the mutations are already in the graph, and the exclusive
barrier is the only thing preventing a reader from observing them before the WAL
fsync makes them durable.

So the barrier is load-bearing, and the two consequences are one defect seen
twice:

- **#2274**, above: readers are starved because writes take the barrier
  exclusively.
- **#2193**, the write ceiling: throughput is flat at **231–270 op/s from 1 to
  256 writers** (re-measured 2026-07-31; 231.4 / 270.2 / 260.6 / 256.5 at 1 / 8 /
  64 / 256 writers) because the fsync happens *inside* the barrier, so no two
  committers are ever in flight together and `wal.Writer.SyncGroup` — which
  already implements PostgreSQL-`XLogFlush`-style coalescing, and is already
  reached by `Tx.CommitWALOnly` — never has a second committer to coalesce with.
  The same primitive outside the barrier reaches **127 582 op/s**.

Both dissolve once visibility is decoupled from the barrier, which is what
versioned reads do.

## 4. Correcting the recorded conclusion of rmp #2051

rmp #2051 records an empirical refutation and a conclusion. The refutation is
sound and must be preserved; **the conclusion does not follow.**

What was measured (2026-07-18): a prototype of phases P1 and P2 of an
eight-phase **per-shard copy-on-write** design took
`BenchmarkEngWriteAutocommit` from 5 664 ns/op and 2 398 B/op to **30 665 ns/op
(5.4×) and 102 416 B/op (43×)**, and was reverted. The stated root cause is
correct: Go maps have no O(1) or O(delta) immutable snapshot, so eager
per-commit COW deep-clones a whole shard map, giving **O(shard size) per write**,
which worsens as the graph grows.

The conclusion drawn was that a viable MVCC therefore *"requires replacing the
LPG core maps … with PERSISTENT/immutable data structures (HAMT/CTrie) … a
foundational storage rewrite"*.

**That conclusion is a property of the whole-graph-snapshot model, not of MVCC.**
It assumes a version is a snapshot of the entire graph, so producing one costs
something proportional to the graph. Neither incumbent works that way. In
Memgraph a version is not materialised at all: the object holds its current
state, and a ≤56-byte delta per modification lets a reader *reconstruct* the
older version it needs. Cost per write is **O(1) in the number of modifications**
and **zero in the size of the graph**, with no persistent map anywhere.

Persistent data structures remain *one* way to get versioned reads. They are not
the only way, they are not what the engines GoGraph is modelled on chose, and
the measurement in #2051 is not evidence against the delta-chain design, because
the delta-chain design was never measured.

**This must be verified before it is built on.** §7 P0 makes that the first
deliverable, because a premise inherited from a previous cycle is exactly the
kind this project has repeatedly found to be wrong.

## 5. Proposed design

Per-object delta chains, taking the structural idea and re-implementing it
idiomatically in Go. No source is copied: Memgraph is BSL 1.1 and Neo4j GPLv3,
and neither licence is GoGraph's to redistribute.

**Timestamps.** A monotonic commit counter. A transaction records a
`startTS` at Begin; a committing transaction is stamped with a `commitTS`
allocated under the existing commit serialisation. A version is visible to a
reader when it was committed at or before the reader's `startTS`.

**Deltas.** One record per modification, carrying the *previous* value so the
chain reconstructs backwards, plus the owning transaction's commit info and a
link to the next older delta. The set of actions mirrors the mutation surface
`lpg` already has: add/remove node label, set/delete node property, add/remove
edge, set/delete edge property, set/remove edge type per slot. Sized and pooled
deliberately — a delta per modification on the write path is the whole cost
model, so `sync.Pool` and a flat struct are mandatory, not optional.

**Read path.** `View` stops taking `visMu`. A reader captures `startTS` once and,
for each object it touches, walks that object's delta chain until it reaches a
version committed at or before `startTS`. In the overwhelmingly common case —
no concurrent writer touched the object — the chain is empty and the read is the
current value with a single atomic load, which is the cost profile the current
`View` fast path already has.

**Write path.** A writer takes the per-object lock, appends a delta, mutates in
place, and releases. The exclusive global barrier disappears from the ordinary
write path. An exclusive mode is retained for genuinely global operations —
index creation, schema DDL, checkpoint capture — which is precisely the role
Memgraph reserves `UNIQUE` for, and which is rare enough that its
writer-preference is harmless.

**Durability ordering is preserved and becomes cheaper.** Today visibility is
withheld by holding the barrier across the fsync. With versioned reads it is
withheld by not publishing a `commitTS` until the WAL fsync returns: readers
started before that instant do not see the write, and readers started after it
do. That is the same guarantee — durability before visibility — enforced by a
timestamp rather than a latch, which is what allows several committers to be in
flight at once and lets `SyncGroup` finally coalesce (#2193).

**Garbage collection.** A delta is reclaimable once no live transaction has a
`startTS` older than the version it supersedes. This is the part with no
equivalent in the current codebase and the largest source of unbounded-memory
risk; it needs an explicit bound and its own metrics, per the project's
bounded-resources mandate.

## 6. Risks, stated up front

- **This is the highest-risk change the module has attempted.** It touches the
  hottest structures in `lpg`, the isolation contract, and the commit path.
- **Memory.** A delta per modification is unbounded without working GC. The
  bound and its metrics are a deliverable, not a follow-up.
- **Read-path regression.** The current `View` costs 4.032 ns serially against a
  3.689 ns bare `RWMutex` floor and allocates zero bytes. A chain walk must not
  regress the no-contention case; if it does, the design fails on the
  no-regression mandate exactly as #2051 did.
- **Isolation semantics change,** from per-statement read-committed to
  per-statement snapshot isolation. This is a strengthening, but it is a change
  to a documented contract and every affected claim in `docs/` must move with it.
- **The whole-graph-snapshot conclusion may be right after all.** §7 P0 exists to
  find that out cheaply rather than at the end of a multi-sprint programme.

## 7. Phasing

Each phase is a sprint delivering a standalone result. **No phase after P0 may
start until P0's measurement is on the record.**

- **P0 — DONE (rmp #2275).** The cost model holds: +24 B and +1 allocation per
  modification, identical at 10 000 and 1 000 000 nodes; idle read 1.02×; no
  flag-off regression. #2051's conclusion corrected. Full measurement in
  [`mvcc-p0-measurement.md`](mvcc-p0-measurement.md).
- **P1 — DONE (rmp #2278).** Shared commit records, a monotonic clock, and
  atomic publish in one store. Per-modification cost is 32 B (the delta unions
  the record pointer with an inline timestamp for autocommit writes, which
  measurement showed was cheaper than allocating a record per write), still flat
  in graph size and pinned by test. Rollback ownership resolved — see P2.
- ~~**P0 — refute or confirm the cost model.**~~ Prototype a delta chain on ONE
  structure (node labels), measure `BenchmarkEngWriteAutocommit` and the serial
  `View` cost against today's numbers, and compare against the #2051 figures.
  Deliverable: a measurement that either shows O(1)-per-write behaviour, which
  authorises P1, or shows a regression, which returns the decision to the user
  with evidence. **This is the cheapest possible test of the premise the whole
  programme rests on.**
- **P1 — timestamps and transaction state.** Commit counter, `startTS`/`commitTS`
  allocation under the existing commit serialisation, visibility predicate, and
  the tests that pin it. No read path changes yet.
- **P2 — node labels and node properties** on delta chains, behind a flag, with
  the differential and absolute-oracle suites green under both settings.
  **Rollback ownership is settled**: the existing in-memory undo log keeps it,
  and needs no change. Its inverses call the same `lpg` mutators, so each
  records its own delta and the chain holds both the change and its inverse;
  walking it backwards returns the original value, so the two mechanisms
  compose rather than double-undo. Proved by
  `TestLabelTx_ComposesWithPhysicalUndo`. The cost is twice the deltas on an
  aborted transaction, reclaimed by P6.
- **P3 — edges, edge types per slot, and edge properties.** Two findings from
  reading `graph/adjlist` change this phase's shape and are recorded here so the
  next reader inherits them.

  **The adjacency is ALREADY an immutable, atomically-published, lock-free
  snapshot per node.** `adjEntry` is documented as *"an immutable snapshot of a
  node's outgoing adjacency. Once an entry is published to a shard slot via
  atomic.StorePointer its slices are never mutated; mutations produce a new
  entry."* The shard's slot array is cloned only on GROW, not per write —
  measured at 143 ns / 3 allocs at 10 000 nodes against 163 ns / 3 allocs at
  1 000 000, i.e. flat. So GoGraph already produces the older version as a
  by-product of every topology change, where Memgraph mutates its edge lists in
  place and needs a delta to reconstruct. P3's adjacency work is therefore to
  RETAIN and INDEX those entries by commit timestamp, not to build undo records
  — materially cheaper than the reference design, because the expensive half
  already exists.

  **The prerequisite that turned out not to be one — REFUTED in P4a
  (rmp #2288).** `storeEntry`'s comment said the in-window in-place builder
  mutation was a barrier-borrowed shortcut that a lock-free read path would turn
  into a torn read, so P4 would have to remove it and pay O(ops) per commit
  instead of O(distinct shards touched). It does not. The mutation replaces one
  slot POINTER with an atomic store and never touches an `adjEntry`, which stays
  immutable; with versioning armed the new entry carries a record of the one it
  replaced, so a reader from before the write steps back to exactly what it
  should see. Seeing the newer entry is not a torn read — it is the ordinary
  MVCC case. The one unsound direction, a reader MISSING a write it should see
  by holding a slot array published before it, cannot happen: a reader reads its
  start timestamp from the clock before it loads `slotsRef`, and the writer
  stores `slotsRef` before it publishes its commit timestamp into the clock, and
  Go's `sync/atomic` operations are sequentially consistent. Pinned by
  `TestStoreEntry_InPlaceWindowMutationIsSoundUnderVersioning` and its
  concurrent companion, which assert BOTH that the shortcut is still taken (by
  slot-array pointer identity, so the test cannot silently stop exercising it)
  and that the read is correct across it.
- **P3c — the three remaining per-edge side stores (rmp #2291).** Reading the
  code for P4 found three structures a read touches that P3 did not version, all
  the same shape — a sparse map behind a shard lock: the edge-label OVERFLOW
  (a pair's second and later relationship types, and the orphan tier), and the
  per-handle label and property stores that give a parallel edge instance its
  own type and properties. `EdgeLabelsByHandle`'s own godoc already admitted it:
  *"only per-operation atomic … NOT cross-store consistent … outside a
  transaction barrier"*. That barrier is the one P4c removes.
- **P4a — DONE (rmp #2288).** Versioning armed by default, and every engine
  write — explicit transaction or autocommit statement — carries ONE
  `mvcc.CommitInfo` shared by every version it creates in every store, published
  with a single atomic store on the brackets that already delimit a transaction.
  Before it, `deltaStamp` minted a fresh timestamp per modification, so
  `CREATE (a)-[:R]->(b)` stamped the label, the property and the edge at three
  different instants and a reader could land between them. The gate that catches
  that samples a start timestamp INSIDE the statement; a boundary-only version
  passed against the unfixed code and was corrected.

  Measured, interleaved against a clean worktree at the parent commit so
  thermal drift hits both arms: serial `Graph.View` **unchanged** (4.054 →
  4.062 ns, p=0.129); `BenchmarkEngWriteAutocommit` **+9.64 %** (1.992 →
  2.184 µs), **+5.01 % B/op**, **+2 allocs/op** — one version record for the
  statement's one modification, which is the cost model P0 authorised, plus one
  shared commit record per transaction. The empty-bracket
  `BenchmarkBarrier_ApplyAtomically` is +35.85 % (9.444 → 12.830 ns) and still
  allocates nothing: the record is allocated lazily by the first version that
  needs one, so a bracket that versions nothing allocates nothing, which is what
  keeps `TestBarrierGuard_ApplyAtomicallyAllocatesNothing` green. A first
  reading of the read path as +3.3 % was the measurement drifting, not the code:
  re-measured at the parent commit in a clean worktree, the "before" reproduced
  as the "after".

  Arming versioning obliged the reclamation DRIVER to land with it, because the
  bounded-resources mandate does not admit an unbounded structure with a
  follow-up ticket attached. It runs on the writer that just committed, while
  the barrier is still held — it needs writer exclusion, and a background ticker
  would wake to do nothing on a graph nobody is writing.
- **P4b — the read snapshot**, threaded through the physical build, with the
  barrier still held. Holding it makes the whole refactor a provable identity:
  no writer is concurrent, so every as-of read must return exactly what the
  plain read returns.
- **P4c — DONE (rmp #2290). THE READ BARRIER IS GONE and #2274 is closed.**

  | | before | after |
  |---|---|---|
  | collapse, 1 reader | 50.2× | **1.89×** |
  | collapse, 8 readers | 59.0× | **2.04×** |
  | worst short-read latency, 1 reader | 1m36.2s | **3.973 ms** |
  | worst short-read latency, 8 readers | 1m36.6s | **4.586 ms** |
  | a writer alone | free | free |

  Worst-case latency improves by roughly 24 000×, and it no longer tracks the
  duration of the longest concurrent read — which was the unbounded part.
  Example 35 reports `readers_starved=0`, a 1.6× collapse and
  `worst_read_vs_analytics=0.00`.

  What it took beyond retiring the barrier was the CANDIDATE-SET discipline.
  The versioned stores answer what an object contains, not which objects a scan
  should consider, and the structures that answer that were not versioned:

  - **Node existence** is now versioned, in PostgreSQL's shape rather than a
    delta chain — a birth instant and a death instant, because that is what the
    question is. Kept SPARSE, in per-shard maps holding only nodes born or
    removed since the oldest reader, reclaimed by the same watermark. Two
    events inside ONE transaction share a commit record and therefore an
    instant, so a sequence number breaks the tie; without it a rolled-back
    DELETE — which tombstones and then revives — made the node vanish for every
    later reader.
  - **Label-index removals are DEFERRED** until the watermark passes them. A
    superset is harmless because the scan re-checks the versioned label bag; a
    missing entry is a silently lost row. They are also CANCELLABLE, because a
    rollback re-attaches the labels a failed statement stripped and a surviving
    deferral would delete the restored entry at the next sweep.
  - **The O(1) count pushdowns are gated.** A count has no object to be
    re-checked against, so `LiveNodeCount` and `ResolveLabelCount` report
    whether they are EXACT for the reader's instant and the caller counts the
    filtered scan when they are not.
  - **The horizon registration race is closed**: a reader claims its slot with a
    hold-everything marker BEFORE it reads the clock, so a sweep landing between
    the two frees nothing rather than freeing what the reader is about to need.

  **The correctness bug the whole phase turned on**, found by example 27's
  bank-transfer invariant — `readers observed a torn total 40 time(s)` — and by
  nothing else: the version-chain readers gated on a GRAPH-LEVEL atomic counter
  sampled BEFORE the shard lock. Between that sample and the read, a writer can
  create the first version and apply its change, so the reader takes the raw
  current value for that node and the pre-write value for the next one, and
  reports a state that never existed. It is not new — the barrier had made it
  unobservable, because a reader could not run while a writer held it — and it
  became frequent the moment reclamation started returning the counter to zero
  between transactions. Every such gate now reads the SHARD'S OWN side map,
  under the shard lock, atomically with the value; the same treatment applies to
  node existence, and the birth record is now written under the mapper's write
  lock so a node is never reachable before it. A second, unrelated latent bug
  surfaced the same way: the edge-type filter cache read its entry pointer AFTER
  unlocking, which could pair a filter with a CSR from a different epoch.

  Two performance defects were found by measurement and one deadlock by the soak
  itself, and none of them by the short suite:

  - Filtering the label bitmap by re-checking every MEMBER cost 180 µs on a
    4.7 µs query — a 39× collapse that appeared as soon as any version existed
    anywhere and persisted after the writer stopped, with THREE live versions.
    The corrector now visits the SUSPECTS (the keys of the sparse side maps),
    which is bounded by the churn the reclaimer has not caught up with.
  - The opportunistic sweep's throttle LATCHED: a sweep that freed nothing
    recorded the unchanged count, and every later read then skipped forever.
    A tick cannot latch.
  - The suspect walk held a shard read lock across a visit that re-acquires it.
    Go's `sync.RWMutex` is not re-entrant, so a writer arriving in between
    deadlocked all eight readers at zero CPU for eighteen minutes. It is pinned
    by a watchdog test that now FAILS in thirty seconds with attribution
    instead of hanging.

  Measured read cost, interleaved against a clean worktree at the P4b commit:
  `EngReadCount` unchanged, `EngReadLabel` +3.89 %, `EngReadFilterCount`
  +8.69 %, `EngReadProject` +8.71 %, `EngReadProjectLargeSerial` +22.13 %,
  geomean **+8.46 %**. That is what per-statement snapshot isolation costs a
  read once the gates are safe, against removing a 50× collapse and a 1m36s
  worst-case latency.

  Writes still take the barrier exclusively; moving the fsync out from under it
  is P5 (#2193).
- **P5 — move the fsync outside the commit serialisation** so `SyncGroup`
  coalesces, closing #2193. Acceptance is the write-scaling table in §3 rising
  with writer count instead of staying flat.
- **P6 — garbage collection**, with an explicit memory bound and utilisation
  metrics.
- **P7 — documentation**: the isolation contract, the concurrency clauses on
  every affected exported type, and this document reconciled to what shipped.

## 8. Acceptance gates for the programme as a whole

1. `bench/mtaudit/fairness_soak_test.go` passes: no collapse beyond 4× with a
   multi-second read and a concurrent writer, and worst short-read latency no
   longer tracks the long read's duration.
2. Write throughput rises with writer count instead of holding at ~258 op/s, and
   `commits_per_fsync` exceeds 1 under concurrent load.
3. No regression in the serial `View` cost (4.032 ns, zero allocations) or in
   `BenchmarkEngWriteAutocommit` (5 664 ns/op, 2 398 B/op, 34 allocs).
4. Delta memory is bounded, and the bound is surfaced as a metric.
5. `go test -race ./...` green; the openCypher TCK at the full 3897 scenarios;
   the crash-injection battery and the WAL recovery suites green; `make ci`
   exit 0.
6. The isolation contract in `docs/` describes what the code does.
