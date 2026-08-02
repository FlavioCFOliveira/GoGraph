# Audit — making MVCC GoGraph's sole concurrency-control mechanism

**Date:** 2026-08-02 · **Entry head:** `6b990377` · **Machine:** Apple M4 (10 cores), `darwin/arm64`
**Sprint:** 334 · **Task:** rmp #2296

Scope: **every mechanism in the module by which concurrent access is controlled.** This audit
asks one question — *what, other than multi-version concurrency control, still governs
concurrency here?* — and answers it with the code, not with intent.

It is the planning input for sprint 334. Every task in that sprint traces to a finding below.

---

## 0. Where sprint 333 left the module

Sprint 333 closed at 16/16 with MVCC **certified for reads**
([`certification-mvcc-2026-08-01.md`](certification-mvcc-2026-08-01.md)): every structure a read
touches is versioned, a read observes exactly one instant, and **no read takes the visibility
barrier**. That half is done and this audit does not revisit it.

What it did not touch is the **write** side. The module still controls write concurrency by
**exclusion**, and [`isolation-design.md`](isolation-design.md) says so in as many words:

> The engine is **single-writer** (`store/txn.Store` mutex). Write-write conflicts and write
> skew — the hard parts of SI, and the only anomalies that separate SI from serialisability —
> are impossible by construction.

That sentence is the audit's subject. MVCC currently supplies GoGraph's *isolation*; exclusion
still supplies its *concurrency control*. The objective of sprint 334 is to make the second
sentence false by making versioning do both jobs.

---

## 1. The finding, measured

**Write throughput does not scale with writer count. It degrades.**

`bench/mvccwrite/baseline_test.go`, entry head `6b990377`, no store attached — so the number
isolates the concurrency-control ceiling from the WAL `fsync` entirely:

```
go test -run='^$' -bench=BenchmarkWriteScaling -benchtime=3000x ./bench/mvccwrite/
```

| writers | commits/s | ns/commit | scaling |
|--------:|----------:|----------:|--------:|
| 1 | 333 590 | 2 998 | 1.00× |
| 2 | 303 552 | 3 294 | 0.91× |
| 4 | 283 953 | 3 522 | 0.85× |
| 8 | 278 919 | 3 585 | 0.84× |
| 16 | 276 043 | 3 613 | **0.83×** |

Sixteen writers on a ten-core machine deliver **0.83×** the throughput of one. The ideal for an
engine whose writers do not conflict is a rising curve; this one falls, which is the signature
of a global exclusive lock plus the cost of contending for it.

---

## 2. The write-serialisation stack — four layers, not one

A write does not pass one gate. It passes up to four, and **each one independently enforces
single-writer**.

| # | Mechanism | Location | Granularity | Held for |
|---|---|---|---|---|
| 1 | `Engine.writeMu` | `cypher/api.go:1069` | whole engine | a whole autocommit statement; **BEGIN→COMMIT for an explicit tx** |
| 2 | `txn.Store` single-writer semaphore | `store/txn/txn.go:444` | whole store | `Begin` → append (then released; see §5) |
| 3 | `lpg.Graph.visMu` (write mode) | `graph/lpg/lpg.go:565` | whole graph | the entire apply of every statement |
| 4 | `txn.Store` apply gate (`applyMu`, `appliedSeq`) | `store/txn/txn.go:495` | whole store | strict sequence-ordered apply |

Layers 1 and 2 are alternatives to one another, selected by wiring: `Engine.lockWriter`
(`cypher/api.go:1187-1193`) takes `writeMu` **only** when `e.store == nil`, because a WAL-backed
engine is already serialised by the store semaphore taken in `txn.Store.BeginCtx`. Layer 3 is
taken on **both** wirings, inside `lpg.Graph.ApplyAtomically`.

The documented lock order is `writeMu` (outermost) → `visMu` (`cypher/api.go:1065-1067`),
matching the WAL-backed `store-mutex → visMu` order, so the two wirings share one deadlock-free
ordering.

### 2.1 The strongest form: an explicit write transaction

`cypher/exectx.go:50,64` states the contract plainly: an explicit write transaction holds the
visibility barrier (`lpg.Graph.LockBarrier`, an exclusive lock) **for the ENTIRE lifetime of the
transaction** — from `BEGIN` until `COMMIT` or `ROLLBACK` (acquired at `cypher/exectx.go:336`,
released at `cypher/exectx.go:719`).

This is the most consequential single fact in the audit. A client that opens a write transaction
over Bolt and then pauses — thinking, waiting on the network, waiting on a user — **blocks every
other writer in the process for as long as it holds the transaction open**, and it does so with
an exclusive `sync.RWMutex` that also parks arriving readers of the same mutex behind it.

Because the whole transaction runs inside one barrier acquisition, its statements route through
`lpg.Graph.ApplyInsideLocked` rather than `ApplyAtomically` (`cypher/exectx.go:493-497,561-563,650-652`),
since re-entry would deadlock — the barrier is not re-entrant, and `barrierGuard`
(`graph/lpg/reentrancy_enabled.go`) converts a nested acquisition into an immediate panic rather
than a silent hang.

### 2.2 What `visMu` still legitimately does

`visMu` is not gratuitous. Since #2290 it is a **write-side** mechanism only — `Engine.Run` does
not take it and reads get their consistency from a snapshot (`graph/lpg/lpg.go:546-552`). Holding
it exclusively for the whole apply is what makes the write path single-writer, and *that* is what
lets the adjacency's commit window and the shared commit record use **plain, unsynchronised
fields** (`graph/lpg/lpg.go:549-552`). Removing it is therefore not a deletion; it is a
substitution, and everything that currently rests on single-writer must be given a new basis
first. §6 enumerates that debt.

### 2.3 The lock protocol of one Cypher write, in order

Autocommit write on a WAL-backed engine, `cypher.Engine.RunInTx`. The right-hand column is what
is held **after** the step. This is the protocol every task in phase B has to preserve the
meaning of.

| # | Step | Location | Held after |
|---|---|---|---|
| 1 | entry, context check | `cypher/api.go:15503,15528` | — |
| 2 | DDL fast-path bypass | `:15532` | — |
| 3 | parse + plan-cache lookup | `:15549` | `planCache.mu` taken/released |
| 4 | `unlockWriter := e.lockWriter()` | `:15582` → `api.go:1187` | **no-op** — a store is attached (`:1188-1190`) |
| 5 | `e.store.BeginCtx(ctx)` | `:15604` → `store/txn/txn.go:788,671` | **store semaphore** |
| 6 | `execUnderBarrier(…, ApplyAtomically, …)` → `visMu.Lock()` | `:15621` → `lpg.go:679` | **`visMu` EXCLUSIVE + semaphore** |
| 6c | `adj.BeginCommit()` (`commitDepth++`), `beginWrite()` (`stamp.Begin()`) | `lpg.go:686,693`; `mvcc/stamp.go:112` | same |
| 7 | build the physical plan over `ReadAt(nil)` — **the present, no snapshot** | `api.go:15710,15714` | + shard read latches |
| 8 | `exec.Run` drains; each mutation applied **eagerly**, inverse recorded in `undoLog`, version stamped | `api.go:15731`; `mvcc_txn.go:165-184` | + shard write latches |
| 8a | label bitmap: **ADD immediate**, **REMOVE deferred** | `lpg.go:1853,1957` / `:1935,2913` | same |
| 8b | index changes and count deltas **buffered**, not applied | `cypher/exec/{index,count}_writeback.go` | same |
| 9b | `CommitWALOnly` → `appendOnly`: `markInflight`, mint `seq`, append every op frame + `OpCommit` | `store/txn/txn.go:1361,1386,1394-1411` | same |
| 9c | **`releaseAfterAppend()`** | `:1414` → `:688` | **semaphore RELEASED — still inside `visMu`** |
| 9d | **`wal.SyncGroup()` — the fsync** | `:1245` → `store/wal/writer.go:585` | `visMu` only |
| 9e | apply gate: `waitApplyTurn(seq)`, `advanceApply(seq)` | `:1248,1249` | `visMu` |
| 9f–g | secondary indexes, then count store, published | `api.go:5124,5133` | `visMu` |
| 10a | `endWrite()`: `NextCommitTS` → `info.Commit(ts)` → **`PublishCommitTS(ts)` — the visibility flip** | `mvcc_write.go:88-93` | `visMu` |
| 11 | `visMu.Unlock()` | `lpg.go:700` | free |

**Durable-then-visible holds today** because the fsync (9d) happens-before `PublishCommitTS`
(10a), both inside `visMu`.

Which steps are safe to overlap with a second writer **today**:

| Steps | Safe? | Why |
|---|---|---|
| 1–4 parse, plan, cache | **yes** | latches only; cache entries are immutable once published |
| 5, 6 | n/a | these *are* the serialisers |
| 6c | **no** | `commitDepth++` is a plain int; `stamp.Begin()` overwrites the single record |
| 7–8 build and drain | **no** | reads the present with no snapshot and no conflict detection; `dirtyShards` append is unsynchronised |
| 9b WAL append loop | **no** | recovery requires a transaction's frames to be contiguous (E5) |
| 9d fsync | **yes — and it is designed for it** | `SyncGroup` is leader/follower coalescing; the leader fsyncs with its mutex released (`store/wal/writer.go:580-586`) |
| 9e | yes | the apply gate re-imposes order — which is also why it re-serialises (E18) |
| 9f–g | **no** | per-transaction buffers, but publication is unordered against another committer |
| 10a | **no** | out-of-order `PublishCommitTS` breaks snapshot isolation (E1) |

The single most telling row is **9d**: the one step already built for concurrency is the one step
that cannot be reached, because `visMu` spans it while the semaphore was released before it.

---

## 3. Inventory: latch or concurrency control?

Every `sync.Mutex` / `sync.RWMutex` in the non-test code of `graph/`, `cypher/`, `store/` and
`bolt/`. The verdict distinguishes a **latch** — short, protecting the physical integrity of a
data structure, the kind every MVCC engine still has and PostgreSQL calls by that name — from a
**control**: a mechanism that decides which *transactions* may proceed, which is MVCC's job.

### 3.1 CONTROL — mechanisms MVCC must replace

| Location | What it is | Why it is control, not a latch |
|---|---|---|
| `graph/lpg/lpg.go:565` `visMu` | the visibility barrier | held in write mode for a whole statement, and for a whole *transaction* in the explicit-tx path; it decides transaction admission, not structure integrity |
| `cypher/api.go:1069` `writeMu` | engine single-writer | held BEGIN→COMMIT for an explicit tx (`cypher/api.go:1060-1063`); pure transaction serialisation |
| `store/txn/txn.go:444` semaphore | store single-writer | "only one Tx is active at any moment" (`store/txn/txn.go:391-392`) |
| `store/txn/txn.go:495` `applyMu`/apply gate | strict sequence-ordered apply | forces a total order on apply so no reader observes a state no serial schedule produces; MVCC provides that ordering by timestamp instead. **It also silently re-serialises writers at apply time**, so removing the other three without addressing it buys nothing (`store/txn/txn.go:1261-1265`) |

`cypher/undo.go` was a candidate for this table and **does not belong in it** — see §4.4. It
composes with MVCC and stays.

Note that mechanisms 1 and 2 are **mutually redundant by wiring**: `Engine.lockWriter` returns a
no-op closure when a store is attached (`cypher/api.go:1188-1190`), because the store semaphore
already serialises. Only the store-less engine takes `writeMu` — which is the wiring the §1
baseline measured, so that number is `writeMu` + `visMu` and nothing else.

### 3.2 LATCH — legitimate under MVCC, keep

| Location | Protects |
|---|---|
| `graph/lpg/lpg.go:100,202,218,257`, `property.go:218`, `mvcc_life.go:86`, `mvcc_index.go:60`, `edge_*_{labels,props,count,handle}.go` | per-shard maps and version-chain heads; short, physical |
| `graph/adjlist/adjlist.go:251` | adjacency shard publication |
| `graph/index/{btree,hash,label,count}`, `graph/index/manager.go:147` | secondary-index structures |
| `graph/mapper.go:215` | the node-key ↔ NodeID mapper shards |
| `cypher/plan_cache.go:58`, `csr_pair_cache.go:132`, `edge_type_filter_cache.go:80`, `index_binding.go:559`, `expr/regexcache.go:92` | cache bookkeeping; not graph state |
| `store/wal/writer.go:149`, `store/checkpoint/checkpoint.go:197`, `store/csrfile/reader.go:53`, `store/db.go:111` | file/IO state |
| `bolt/server/{serve,tls_reload,txquota,txregistry}.go` | server-side session/connection bookkeeping |
| `cypher/procs/registry.go:92`, `cypher/exec/constraints.go:138`, `graph/lpg/schema/schema.go:57` | registries and schema; see §6.3 for the multi-writer caveat |

The latch column is long and that is **correct** — an MVCC engine has many latches. The defect is
not their existence; it is that the five entries in §3.1 do the job MVCC should be doing.

Prior evidence that a bare `RWMutex` is the wrong shape at this scale is already in the tree:
`graph/mvcc/horizon.go:20` records that **#2203 measured a bare `sync.RWMutex` degrading 17.6×
from 1 to 10 cores**, which is why the reader horizon is 64 cache-line-padded slots.

---

## 4. The three foundations MVCC is missing

The barrier is not the deepest problem. It is the *load-bearing wall* holding up three things
MVCC has not yet been given, and pulling it out before they exist would not produce a
multi-writer engine — it would produce a broken one. Each is self-documented in the code.

### 4.1 The clock has one watermark and no in-progress list

`graph/mvcc/mvcc.go:167-170` states the dependency outright:

> Publication is in allocation order because commits are serialised by the write barrier, so one
> counter is enough and no in-progress list is needed — which is what PostgreSQL's snapshot
> `xip_list` and Memgraph's `commit_log_->OldestActive()` exist to supply when commits are NOT
> serialised.

`Clock.ReadTS()` (`graph/mvcc/mvcc.go:171`) returns a single `visible.Load()`; `PublishCommitTS`
(`:135-145`) is a monotone CAS. Two committers publishing out of allocation order let a reader
straddle a commit — the exact torn read Example 27's bank-transfer invariant caught during #2290
(`graph/mvcc/mvcc.go:158-163`). **Nothing else in this sprint matters until this is replaced.**

### 4.2 Writers have no transaction identity and no snapshot

- `Graph.BeginRead()` (`graph/lpg/snapshot.go:73-85`) always returns `txID: 0`, and it is the
  only non-test constructor of a `Snapshot`.
- The write path reads `e.g.ReadAt(nil)` — "the current value, no version walk"
  (`cypher/api.go:15710,15714`).
- `nextTxID` (`graph/lpg/mvcc_txn.go:107`) has **zero non-test callers**, so the `ts == txID`
  branch of `mvcc.Visible` (`graph/mvcc/mvcc.go:102`) is **dead code**.

The read-your-own-writes machinery is therefore *built and unused*. A writer sees its own work
today only because it is the only writer.

### 4.3 There is no write-write conflict detection anywhere in the module

No first-updater-wins, no validation phase, no abort wired to a statement —
`graph/lpg/mvcc_txn.go:139-143` says `labelTx.abort` "is NOT yet wired to any statement path".
This is **new construction, not a refactor**, and it is the piece that makes everything else
safe. Two overlapping writers today would simply lose updates.

### 4.4 What the physical undo log actually is — a correction

An earlier reading of this audit treated `cypher/undo.go` as an alternative mechanism MVCC must
replace. **The code refutes that.** `graph/lpg/mvcc_txn.go:43-66` and
`graph/lpg/mvcc_write.go:33-50` establish that the two *compose*: each inverse closure calls the
same `lpg` mutators, so it records **its own delta** on the same chain, the stored value is
already correct when the barrier drops, and a rolled-back statement therefore **publishes** its
commit record rather than aborting it. The log is per-transaction and single-goroutine
(`cypher/undo.go:24-26`), so it is not itself a barrier dependency.

The right conclusion is the stronger one: **conflict detection is what makes physical undo sound
under overlapping writers.** If a writer cannot have modified an object another live writer
already modified — because it would have failed with a serialization error first — then unwinding
that object physically restores exactly the value that was there. This is Memgraph's shape, not a
departure from it. What must still be added is `mvcc.AbortedTS` marking (`graph/mvcc/mvcc.go:33-40`)
so an aborted transaction's versions are recognisable to reclamation, and a sound rule for the
one place that genuinely assumes a frozen graph — §6.5.

---

## 5. Group commit already exists; the engine cannot reach it

`store/txn` already implements group commit: a committer **releases the single-writer semaphore
after the append phase and before the coalesced `fsync`** (`store/txn/txn.go:498-500`), with an
in-flight tracker (`inflightMu`, `store/txn/txn.go:518`) preserving the `acked ⇒ durable`
boundary for `RunUnderCommitLock`.

So the coalescing machinery is built and correct. The engine write path cannot use it because
the apply is pinned under `visMu` and forced into strict sequence order by the apply gate. This
matches the recorded history: group commit already worked at the store layer, and the flat
throughput was the Cypher path only.

**The certification's stated reason for deferring #2193 was already corrected once** and the
correction stands: MVCC dissolves "visible-but-not-durable", because visibility is gated by
`PublishCommitTS`, not by the barrier — so `apply → release → fsync → publish` *is*
durable-before-visible. What still blocks it is exactly §4 (physical undo) and write-write
dependencies. Both are in this sprint's scope, so #2193 stops being architecturally blocked.

---

## 6. The blast radius of removing single-writer

Everything below is currently correct **because only one writer exists**. Each is a debt the
sprint must pay before the barrier can go.

### 6.1 CRITICAL — silent wrong answers

| # | Invariant that rests on single-writer | Evidence |
|---|---|---|
| E1 | **Snapshot visibility**: one watermark, no in-progress list; out-of-order publication lets a reader straddle a commit | `graph/mvcc/mvcc.go:135-145,167-171`; `graph/lpg/mvcc_write.go:88-93` |
| E2 | **No conflict detection**: writers read the present with no txID and no validation ⇒ lost updates | `cypher/api.go:15710,15714`; `graph/lpg/mvcc_txn.go:139-143` |
| E3 | **One commit record per graph**: `Graph.stamp` is a single field and `Begin()` overwrites it | `graph/lpg/lpg.go:605`; `graph/mvcc/stamp.go:109-112` |
| E4 | **Adjacency commit window is unsynchronised**: `commitDepth int` and `dirtyShards []*adjShard` are plain fields | `graph/adjlist/adjlist.go:186,200,2390-2395,2437` |
| E5 | **WAL frame contiguity**: recovery's atomicity filter discards any buffered op whose `TxnSeq ≠ commitSeq` as an orphan. Interleaved appenders ⇒ **committed data lost on recovery, visible only as a metric.** Contiguity comes *solely* from holding the store semaphore across the append loop | `store/recovery/recovery.go:1369-1379,1421-1429`; `store/txn/txn.go:1394-1411` |
| E6 | **Quiesce boundary**: `RunUnderCommitLock` proves "no commit in flight" by holding the semaphore *and* draining; without exclusion `wal.Close` can race an in-flight `SyncGroup` and make un-acknowledged frames durable | `store/txn/txn.go:502-517,720-728`; `store/db.go:286` |
| E7 | **Checkpoint phase-1 capture**: a whole-graph `Capture` inside one `View` is transaction-consistent only because the commit lock excludes every writer; `CaptureGraph` locks nothing itself | `store/checkpoint/checkpoint.go:687-762`; `store/snapshot/capture.go:110-118` |
| E22 | **The reclamation horizon covers READERS only** — "the oldest start timestamp among active readers", and `snapshot.go:81` is its sole registration site. An uncommitted writer's pre-images are therefore unprotected the moment writers stop being excluded | `graph/lpg/mvcc_reclaim.go:37-41`; `graph/lpg/snapshot.go:81` |

E5 is the one that should worry a reader most: it is a **durability** failure whose only symptom
is a counter. E22 was found only by comparing against the reference engines — InnoDB min-folds
over *every* live read view (`read0read.cc:258-265`) and Memgraph's `OldestActive()` covers
readers and writers alike because both draw from one counter.

### 6.2 HIGH — durability and consistency

| # | Invariant | Evidence |
|---|---|---|
| E8 | Checkpoint watermark is a transaction boundary; otherwise `TruncatePrefix` cuts a transaction in half | `store/checkpoint/checkpoint.go:717-722,890` |
| E9 | The WAL-health `Poisoned()` gate is justified by "no concurrent commit can run while the commit lock is held" | `store/checkpoint/checkpoint.go:700-714` |
| E10 | **Constraint reseed walks the whole graph**: after an undo replay every UNIQUE value-set is rebuilt by iterating the entire mapper, inside the barrier, assuming a frozen graph | `cypher/api.go:5198-5230` |
| E11 | Version reclamation requires writer exclusion by its own contract | `graph/lpg/mvcc_gc.go:218-219` |
| E12 | Deferred label-index removal depends on the watermark, which depends on ordered publication (E1) | `graph/lpg/mvcc_index.go:49-75` |
| E13 | Count-store publication must follow the fsync **inside** the barrier | `cypher/exec/count_writeback.go:36-39` |
| E14 | Secondary-index batch publication is not ordered against a second committer | `cypher/exec/index_writeback.go:24-29` |
| E15 | `constraintActive`/`indexActive` gates are documented as maintained "under the engine's single-writer lock" — a control dependency hiding inside an atomic counter | `graph/lpg/lpg.go:451,463` |

### 6.3 MEDIUM

| # | Invariant | Evidence |
|---|---|---|
| E16 | The re-entrancy guard holds **one** writer goroutine id and cannot represent two (debug builds only) | `graph/lpg/reentrancy_enabled.go:102-107,179-192` |
| E17 | Group-commit poison is fail-all: writer B's healthy transaction dies from writer A's fsync failure — fine for a serial batch, an isolation surprise for independent writers | `store/wal/writer.go:639-664` |
| E18 | **The apply gate silently re-serialises** the writers at apply time, negating the concurrency | `store/txn/txn.go:1261-1313` |
| E19 | Edge-handle order decouples from WAL order (minted at buffer time) | `store/txn/txn.go:869,873` |
| E20 | `ErrCommittedNotApplied`'s promise rests on E5 | `store/txn/txn.go:110-125` |
| E21 | Recovery and bulk-import open the adjacency commit window with **no barrier at all**, licensed only by "single-threaded, no concurrent reader" — must never be reused at serving time | `store/recovery/recovery.go:1077-1079`; `store/bulkimport/bulkimport.go:141,249` |

Also relevant to how long a would-be concurrent writer stays stuck even once the lock is gone:
`wal.SyncGroup` is deliberately not context-aware (`store/wal/writer.go:481-493`),
`Tx.waitApplyTurn` parks unconditionally (`store/txn/txn.go:1285`), `drainInflight` waits
unconditionally (`:763`), and `RunUnderCommitLock` acquires with `context.Background()` (`:718`).

### 6.4 Reclamation runs inline, on the write path
`endWrite` allocates the commit timestamp, publishes, then calls `reclaimIfDue()`
(`graph/lpg/mvcc_write.go:88-95`) — **while the barrier is held**. Under MVCC as the norm this
belongs in a background vacuum bounded by the horizon watermark, which is what PostgreSQL
(autovacuum), InnoDB (purge threads) and Memgraph (its garbage collector) all do. The watermark
machinery already exists (`mvcc.Horizon`, `graph/lpg/mvcc_gc.go:221`); only its driver is
misplaced.

### 6.5 Structures a *writer* mutates in place

Versioned reads do not make a structure safe for two concurrent *writers*. Verdicts:

| Structure | Verdict |
|---|---|
| `Mapper` (`graph/mapper.go:212-216`) | **safe** — append-only, ids never reused; existence is versioned via `nodeLifeShards` |
| `nodeLifeShards`, `deferredIdx` | **safe** — part of the versioning substrate |
| Tombstone set (`graph/lpg/lpg.go:415-423`) | safe as a structure, but it **duplicates** `nodeLifeShards`: two mechanisms answering "does this node exist" |
| Label/property registries | **safe** — monotone, append-only, COW with lock-free reads |
| `topoGeneration`, `edgeHandleSeq`, the TCK side-effect counters | **safe** — monotone atomics |
| Count store (`graph/index/count`) | **unsafe** — mutable in place, order-sensitive dirty-set marking |
| Label bitmap index | over-inclusive **by design** and sound via re-check, but its *deferral* watermark depends on ordered publication (E1) |
| Secondary indexes (hash/btree) | **unsafe ordering** — batch publish not ordered against a second committer |
| Schema / constraints | **unsafe**, but DDL is already excluded from transactions and takes the writer serialisation explicitly |
| `edgeCreateCount` | latent hazard only — the open #2295 item; the consumer uses it as a conservative guard threshold |
| `txnSeq` (`store/txn/txn.go:474`) | **not restored across restart** — recovery decodes it but never writes it back, so it cannot serve as a global MVCC ordering |

### 6.6 No write-write conflict detection exists
There is no serialization-error type anywhere in the module — the grep for
`Serializ|Conflict|Retriable` returns only *name*-conflict and constraint-name-conflict errors.
Under single-writer none was needed. Under MVCC it is mandatory, and it must reach the client as
a **retriable** Bolt failure so a driver's managed transaction retries it, in the
`Neo.TransientError.*` family the module already uses elsewhere
(`bolt/server/session.go:526,1285`).

---

## 7. The MVCC clock is not durable

`mvcc.Clock` is a process-local pair of atomics constructed at zero. **Nothing in `store/wal`,
`store/recovery`, `store/checkpoint` or `store/snapshot` records or restores a commit
timestamp** — the grep for `CommitTS|commitTS|Timestamp` across those packages returns nothing.

Consequences today are bounded, because timestamps are only ever compared within one process
lifetime. But a module whose *norm* is MVCC cannot leave this open: a recoverable clock is the
prerequisite for an MVCC-consistent checkpoint, for a snapshot that names the instant it was
taken, and for any future point-in-time read or replication.

> **The obvious fix is the wrong one.** §13.5 shows that two of the three reference engines
> *deliberately removed* their persisted counter and now **derive** the clock at recovery.
> GoGraph should derive and ratchet, not persist — which also removes the on-disk format bump
> this section originally assumed.

---

## 8. Isolation actually delivered today

| Path | Isolation | Evidence |
|---|---|---|
| `Engine.Run` (autocommit read) | **snapshot**, per statement | certified; `graph/lpg/snapshot.go` |
| `Engine.BeginReadTx` (explicit read tx) | **read-committed** | `internal/sim/durable_scenarios.go:30-35`: "MAY observe a concurrent writer's commit" |
| `Engine.BeginTx` (explicit write tx) | serialised by exclusion | `cypher/exectx.go:50,64` |
| Bolt `BEGIN mode="r"` | read-committed (routes to `BeginReadTx`) | `bolt/server/tx.go:44,86` |

An explicit read transaction that spans statements is therefore **weaker** than a single
autocommit statement, which is the wrong way round and is a direct consequence of MVCC not being
the norm: `BeginReadTx` holds no snapshot, it merely holds no lock. Making MVCC the sole
mechanism means one snapshot for the transaction's lifetime — snapshot isolation — which is
Memgraph's default and is strictly stronger than what Neo4j advertises.

---

## 9. Alternative mechanisms to discard

| Mechanism | Location | Disposition |
|---|---|---|
| `DisableMVCC` dual mode | `graph/lpg/mvcc_write.go:132` | MVCC is the norm; a public switch that turns it off contradicts the mandate. Retire from the public surface. |
| Physical undo log | `cypher/undo.go` | replaced by logical abort (§4) |
| Read-committed explicit read tx | `cypher/exectx.go:349-373` | replaced by snapshot isolation (§8) |
| Pinned-snapshot-root design | rmp #2051, `isolation-design.md` | superseded: it is a *different* answer to the same question MVCC now answers. Close with a written reason. |
| The three write-serialisation locks | §3.1 | replaced by per-object version-chain conflict detection |

---

## 10. Examples do not exercise the write side under MVCC

Two MVCC examples exist and both are **read-side** instruments: example 35 measures reader
latency under a mixed workload, example 36 checks snapshot isolation on the topology dimension.
Neither drives concurrent *writers*, neither can observe a write-write conflict, a retriable
serialization error, an abort, or a commit timestamp surviving a restart. The sprint needs an
instrument that can see the write side, validated — per the standing rule — to fail on the
build that lacks the property.

### 10.1 And one of the two instruments was itself wrong — fixed this cycle

The `make ci` run that gated this audit came back **RED**, and the failure was example 36
reporting `ISOLATION VIOLATION on topology: 0 invisible commit(s), 2 future read(s), 0 misaligned
far-endpoint count(s) over 170 observations`. (The harness task notification reported exit 0; the
log reported `make: *** [test-short] Error 1`. **The log is the truth** — the third time this
project has recorded that lesson.)

It was the **instrument**, not the engine. Example 36 sampled both ends of its bracket from one
acknowledged-commit counter incremented *after* the write returned, while the engine publishes a
commit *before* returning. A reader landing in that window legitimately sees a commit the ceiling
has not yet counted. `hi` now comes from a separate counter incremented **before** the write, so
the window is closed by construction; the invisible-commit floor is unchanged.

Two things support that diagnosis over a genuine violation: it follows from the code, and the
observed graph was **internally consistent** — 9 links, 9 distinct spokes, zero misalignment —
which is a snapshot one commit ahead of the counter, not a torn read.

**What the evidence is, and is not.** The failure was never reproduced *on demand*: twenty-five
`-race` runs with the machine saturated passed on the **defective** build too. What does exist is
a single red-then-green pair under comparable conditions — the defective build failed a loaded
`make ci` in 2.421 s over 170 observations, and the fixed build passed a `make ci` that ran under
elevated load (`graph/lpg` +47 %, `cypher` +18 % against the red run) with example 36 taking
2.766 s, hence doing comparable work.

One pair is not a reproducible demonstration, so the fix's real basis remains the ordering
argument, which is provable from the code. Recorded this way rather than dressed up in either
direction.

> **Method note, and a self-inflicted one.** The load under which that green run happened was not
> deliberate: fourteen busy-loop processes from the saturation experiment above were left running
> for seventeen minutes because the second batch's `kill` silently failed and — having verified the
> first batch — I did not verify the second. The lesson generalises past this cycle: **verify that
> a cleanup ran, every time, not the first time.** The inflated package timings are the only reason
> the accidental load could be quantified at all rather than merely suspected.

---

## 11. The Knowledge Graph is thin on MVCC

`rmp graph query` for nodes whose name contains `mvcc` returns **three**: the sprint 333 node, a
`Feature` node "MVCC snapshot isolation", and the certification `Spec` node. For what is about to
become the module's central architectural property, that is not a usable model. The sprint must
leave the KG able to answer which components implement MVCC, which tests verify it, and in which
commits each was specified, implemented and tested.

---

## 12. Target architecture

| Discarded | Replacement | Shape taken from |
|---|---|---|
| global write exclusion | writers apply concurrently against per-object version chains, serialised only by the per-object latch that already guards each chain head | Memgraph `Vertex::lock` + delta prepend |
| "conflicts impossible by construction" | conflicts **detected**: a writer finding the chain head owned by another live transaction fails with a retriable serialization error | Memgraph serialization error; PostgreSQL first-updater-wins |
| physical undo replay | logical abort: mark the shared `CommitInfo` aborted; readers skip it by the existing rule | PostgreSQL, InnoDB, Memgraph — all three |
| inline reclamation under the barrier | bounded background vacuum driven by the horizon watermark | PostgreSQL autovacuum, InnoDB purge, Memgraph GC |
| process-local clock | commit timestamp durable in the WAL commit record; restored on recovery | PostgreSQL `pg_control` `nextXid`; Memgraph `recovery.cpp` |
| stop-the-world snapshot | snapshot/checkpoint taken **at a timestamp** while writes continue | Memgraph `CreateSnapshot` |
| read-committed explicit read tx | snapshot isolation for the transaction's lifetime | Memgraph default isolation |

The embedded, single-process model makes Memgraph the closest fit overall — it is in-memory-first
with the same delta-chain encoding GoGraph already adopted (`graph/mvcc/mvcc.go:23-25` cites
`src/storage/v2/mvcc.hpp`) — while durability mechanics follow PostgreSQL, which is the reference
this module's WAL already tracks.

> **Prior-art detail** for each row of this table — the exact functions, the conflict-detection
> point, the cascading-abort question and the group-commit ordering rule, each cited to a file
> and symbol at a stated version — is recorded in §13.

### 12.1 Sequencing: the barrier comes out LAST

The single most important planning conclusion of this audit. The sprint's shape is **"build the
machinery that makes concurrent commits safe, then remove the barrier"** — not "remove the
barrier". Removing it first does not yield a multi-writer engine; it yields a broken one, and the
worst of the breakage (E5) is invisible except as a counter.

Barrier removal is gated on four predecessors, each independently testable:

1. **A publication scheme that tolerates out-of-order commits** — the `xip_list` /
   `OldestActive` equivalent (§4.1). Without it nothing else matters.
2. **Writer snapshots with a real `txID`** (§4.2). The read-your-own-writes machinery already
   exists and is dead; this wakes it.
3. **Write-write conflict detection** (§4.3). New construction. Memgraph's `Delta` model is the
   closest prior art and the module has already studied it.
4. **Per-transaction commit state** — `Graph.stamp` and the adjacency commit window move from
   per-graph singletons to per-transaction, and the WAL append keeps one transaction's frames
   contiguous under concurrent appenders (E5).

Only then does removing `visMu` unlock what #2193 identified: `SyncGroup`'s leader/follower
coalescing (`store/wal/writer.go:438-537`) is already built and already unreachable, because
`visMu` spans the fsync while the store semaphore was released before it.

Two clean-ups are cheap and **independent of all four**, so they can land early:

- retire the tombstone set in favour of `nodeLifeShards` (duplicate answers to one question);
- give `ExplicitTx` a handle-level snapshot so a read transaction gets snapshot isolation instead
  of read-committed — a field on the struct (`cypher/exectx.go:161-237`) plus one line at `:435`.

---

## 12.2 Documentation drift that will mislead this sprint

These comments read as current and are not. Planning from them reaches the wrong conclusion, so
they are fixed as part of the work that touches each file.

| Location | Claim | Reality |
|---|---|---|
| `graph/adjlist/adjlist.go:2212-2215`, `:246`, `:2257-2258` | "concurrent readers cannot run during a window … reads are under `visMu.RLock`" | readers no longer take `visMu`; safety now rests on the entry version chain, which the newer note at `:2426-2433` states correctly. **Two comments in one file contradict each other** |
| `bolt/server/txregistry.go:36-37,217-218` | "while a writing transaction is open every reader waits" | false since #2290 |
| `store/txn/txn.go:709` | "Begin acquires the store lock, then ApplyAtomically acquires visMu" | not true of `Tx.Commit`: the semaphore is released at `:1414` and `ApplyAtomically` runs at `:1195` without it |
| `cypher/api.go:2098-2106` | "running it inside `Graph.View` … a concurrent writer cannot tear those snapshots" | `Engine.Run` no longer calls `View`; the accurate note sits immediately below at `:2127-2144` |

---

## 13. Prior art, read in source

Read at pinned commits, in source rather than in documentation. Memgraph is BSL 1.1, MariaDB
GPLv2, PostgreSQL under the PostgreSQL licence: **shapes are extracted, never code.**

| Engine | Commit | Date |
|---|---|---|
| PostgreSQL | `7a0299a1348b563c72a57a2a40462e90af9dfbac` | 2026-08-01 |
| MariaDB 11.4 (InnoDB) | `c57069561c4fe8f72191d4a2a4a829e5b57537dc` | 2026-07-31 |
| Memgraph | `b3ac3cdc7f1f19686831809759130c5dfffc3f0d` | 2026-07-31 |

### 13.1 None of the three has a global exclusive lock on the ordinary write path — but all three have a *short* global section

That distinction is the most important thing in this section. The goal is not zero global
serialisation; it is a global section that spans an **id/timestamp allocation**, not an apply.

| | Per-write exclusion | Chain head protected by | Global section |
|---|---|---|---|
| PostgreSQL | buffer content lock on one heap page | **CAS on the buffer state word** (`bufmgr.c:6115-6119`), not an LWLock | `XidGenLock` in `GetNewTransactionId` only (`varsup.c:96,274`); XIDs are **lazy**, so read-only transactions never take it |
| InnoDB | one clustered **leaf page** X-latch + one **record** X lock (`mtr0mtr.cc:1369-1398`; `btr0cur.cc:2833-2845`) | that leaf latch, held across the undo write and the `DB_TRX_ID`/`DB_ROLL_PTR` write | **none** — `lock_sys` is sharded (`lock0lock.cc:223-229`); trx ids come from a lock-free `Atomic_counter` (`trx0sys.h:839`) |
| Memgraph | **per-object `utils::RWSpinLock`** — `Vertex::lock` (`vertex.hpp:47`), `Edge::lock` (`edge.hpp:36`) | the same per-object lock, across `PrepareForWrite` + `CreateAndLinkDelta` (`vertex_accessor.cpp:263-289`) | `engine_lock_`, a `utils::SpinLock` (`storage.hpp:453`) |

**Memgraph's `main_lock_` is a DDL barrier, not a write lock.** It is a four-mode `ResourceLock`
(`storage.hpp:438`) in which **WRITE is a SHARED mode** — N writers hold it concurrently. UNIQUE
is taken only for DDL, index, constraint and DROP GRAPH (`interpreter.cpp:9648-9719`); an ordinary
write takes `StorageAccessType::WRITE` (`interpreter.cpp:3438`).

**Hard constraint for GoGraph: there is no per-node struct** (`graph/lpg/lpg.go:198,214`), so
Memgraph's per-vertex lock cannot be copied verbatim. The 64 existing shard mutexes
(`propMapShards = 64`, `graph/lpg/lpg.go:187`) are the substitute, and they are already built.

### 13.2 Conflict detection — copy Memgraph, reject InnoDB

| | Mechanism | Detected | Returned |
|---|---|---|---|
| Memgraph | `PrepareForWrite` (`mvcc.hpp:112-137`): same-tx → OK; head `ts < start_timestamp` → OK; **otherwise serialization error** | **eagerly, at the individual write** | `TransactionSerializationException`, a `RetryBasicException` (`exceptions.hpp:303-309`) |
| PostgreSQL | `HeapTupleSatisfiesUpdate` → `TM_Updated`; first-updater-wins | at the UPDATE/DELETE | RC: re-read + EPQ retry. RR/SER: `ERRCODE_T_R_SERIALIZATION_FAILURE` |
| InnoDB | **pessimistic — T2 BLOCKS, it does not abort** (`lock0lock.cc:1666`, `:2405-2412`) | at lock acquisition | only on deadlock or timeout |

`PrepareForWrite` is roughly twenty lines and it is the shape GoGraph should take: eager,
optimistic, retriable. Memgraph's rule is in fact **stricter than classic first-updater-wins** —
a head delta committed *after* your start also errors, which is snapshot isolation enforced at
write time. InnoDB's pessimistic path needs a lock manager, a wait-for graph and deadlock
detection: the wrong weight class for an embedded library. Notably **MariaDB 11.4 now ships the
optimistic alternative itself** as `innodb_snapshot_isolation`, default OFF
(`ha_innodb.cc:854-856`).

### 13.3 Physical undo is compatible with overlapping writers — conditionally

This settles §4.4 with evidence. **Both** physical-undo engines run rollback concurrently with
other writers:

- **Memgraph** aborts under **per-object locks only** (`inmemory/storage.cpp:1536`), never
  `engine_lock_`, and states the precondition: *"we guarantee that any of our deltas with an edge
  as the upstream object are a monolithic block of deltas belonging to this transaction"*
  (`:1525-1530`).
- **InnoDB** takes no global latch during rollback and no new row locks
  (`BTR_NO_LOCKING_FLAG`, `row0umod.cc:112,122`).
- **PostgreSQL** is the odd one out: abort is **purely logical** — one abort record, explicitly
  *not* flushed (`xact.c:1826-1828`), then two CLOG bits. Cost is O(nrels), not O(rows).

**Therefore: physical undo is sound under overlapping writers if and only if a
`PrepareForWrite`-equivalent keeps each transaction's versions contiguous at the chain head.**
GoGraph has no such guard today, which is precisely why its undo log is unsound the moment
writers overlap — and why the fix is the conflict check (§4.3), not a rewrite to logical abort.

### 13.4 Cascading abort is unnecessary — no engine implements it

Under snapshot isolation a writer sees only its **own** uncommitted state. PostgreSQL gates
others through `XidInMVCCSnapshot` (`snapmgr.c:1868`) and sees its own via
`TransactionIdIsCurrentTransactionId` + `cmin`/`cmax`. InnoDB's `changes_visible`
(`read0types.h:129-137`) returns false for any concurrently-active writer, with own-writes the
sole explicit exception (`:251-252`). Memgraph's `ApplyDeltasForRead` (`mvcc.hpp:60-61`) admits
only `ts < start_timestamp`, and uncommitted deltas carry a transaction id ≥ 2⁶³.

**GoGraph does not need a cascading-abort rule.** An earlier recorded analysis listed it as a
prerequisite for multi-writer; that is now refuted by all three implementations.

### 13.5 The clock is DERIVED at recovery, not persisted

**Two of the three engines deliberately removed their persisted counter.** This reverses §7's
implied recommendation.

- **InnoDB** — `TRX_SYS_TRX_ID_STORE` is dead weight kept only for upgrades: *"In old versions of
  InnoDB, this persisted the value of `trx_sys.get_max_trx_id()`… The field only exists for the
  purpose of upgrading"* (`trx0sys.h:133-140`). Startup folds a max over every rollback segment
  (`trx0rseg.cc:667-669`) then `init_max_trx_id(max_trx_id + 1)` (`:725`). Margin exactly **+1**.
- **Memgraph** — `next_timestamp = max(delta_ts) + 1` from the WAL (`wal.cpp:2089-2091`) and
  `info.start_timestamp + 1` from a snapshot (`snapshot.cpp:1805`), restored as
  `timestamp_ = max(timestamp_, info->next_timestamp)` (`inmemory/storage.cpp:516`).
- **PostgreSQL** — `nextXid` *is* in `pg_control`, but replay still ratchets it per record via
  `AdvanceNextFullTransactionIdPastXid` (`xlogrecovery.c:1899-1902`).

**GoGraph should derive, not persist.** The WAL hook already exists: `OpCommit` carries a
sequence number and the `OpKind` space is explicitly append-only for wire stability
(`store/txn/txn.go:178-195`). This removes the on-disk format bump §12 assumed.

### 13.6 A consistent snapshot while writes continue

**Memgraph is the clean reference shape.** `CreateSnapshot` (`inmemory/storage.cpp:4212-4306`)
opens an **ordinary MVCC read transaction** — `Access(READ, SNAPSHOT_ISOLATION)` (`:4250`) — and
writes the file from that instant through the normal `View::OLD` accessors. READ is a shared mode
compatible with WRITE, so **writers never stop**: no quiesce, no freeze, no lock window.

PostgreSQL's checkpoint is by contrast explicitly **fuzzy** (`xlog.c:7379-7388`); consistency
comes from replaying WAL forward from `XLOG_CHECKPOINT_REDO` (`:7579-7601`), with torn pages
handled by full-page images.

For GoGraph the Memgraph shape is decisive: the checkpointer should be *a reader at one MVCC
instant*, which also closes the capture skew recorded as #2269.

### 13.7 Garbage collection, and a horizon bug this audit had missed

| | Where | Horizon | Bound |
|---|---|---|---|
| PostgreSQL | autovacuum workers **plus** inline HOT pruning | `ComputeXidHorizons` / `GetOldestNonRemovableTransactionId` | a long transaction pins it → bloat |
| InnoDB | **background thread pool**, never inline — `purge_coordinator_task` (`srv0srv.cc:1112-1117`), default 4 threads | `purge_sys.clone_oldest_view` → **min-fold over every live read view** (`read0read.cc:258-265`) | batch = 127 undo pages; a long read **degrades write latency** via `innodb_max_purge_lag` |
| Memgraph | background runner, default **1000 ms** (`config.hpp:73`) | `commit_log_->OldestActive()`, a chained bitset advanced with `__builtin_ffsl` (`commit_log.cpp:170-191`) | takes `main_lock_` **READ**, so it runs concurrently with writers |

**New finding this section produced: GoGraph's reclamation horizon covers readers only.**
`ReclaimVersions` takes *"the oldest start timestamp among active **readers**"*
(`graph/lpg/mvcc_reclaim.go:37-41`), and `graph/lpg/snapshot.go:81` is the only registration site.
**An uncommitted writer's pre-images are therefore unprotected** the moment writers stop being
excluded. InnoDB min-folds over *every* live view; Memgraph's `OldestActive()` covers readers and
writers alike because both draw from one counter (`inmemory/storage.cpp:2834-2835`).

Also worth copying verbatim: Memgraph hands deltas to GC **before** `MarkFinished`, with a comment
explaining that the reverse order is a use-after-free (`inmemory/storage.cpp:1879-1897`).

### 13.8 Group commit, and "durable before visible" — the one open decision

**This rule is not universal, and the difference is exactly what buys throughput.**

- **PostgreSQL — strictly durable-then-visible.** `XactLogCommitRecord` → `XLogFlush`
  (`xact.c:1544`) → `TransactionIdCommitTree` (`:1550`), lexically adjacent inside one `if`, so
  CLOG is unreachable without passing the flush. Coalescing via
  `LWLockAcquireOrWait(WALWriteLock)` plus a re-check of whether someone already covered the LSN
  (`xlog.c:2848-2930`).
- **InnoDB — VISIBLE BEFORE DURABLE, deliberately.** `trx_t::commit_in_memory`:
  `commit_state()` (`trx0trx.cc:1426`) → `deregister_rw()` (`:1431`) → `release_locks()` (`:1453`)
  → **then** `trx_flush_log_if_needed` (`:1479-1483`). The source states the window and its safety
  argument outright (`:468-479`): a transaction T2 that sees T's modifications and itself writes
  gets a larger LSN, so if T's flush fails T2 never commits either. The reason given for the
  ordering is that serialising there *"would prevent a group of transactions from gathering"*
  (`:1461-1476`).
- **Memgraph — neither.** `wal_file_->Sync()` fires every `wal_file_flush_every_n_tx`, default
  **100 000** (`config.hpp:98`). Not per-commit durable.

GoGraph's `commitUnderBarrier` fsyncs **inside** the barrier (`cypher/api.go:5069-5089`). If the
new commit bracket also spans the fsync, `visMu` has merely been renamed. **This is the sprint's
one genuine contract decision and it belongs to the user** — see §16.

---

## 14. Verdict

**MVCC is GoGraph's isolation mechanism, not its concurrency-control mechanism.** It is layered
on an engine that is still single-writer by exclusion at four independent layers, and the
exclusion is not incidental — it is what three unbuilt pieces of MVCC are currently standing in
for: a publication scheme that tolerates out-of-order commits, writer transaction identity, and
write-write conflict detection. The last of these does not exist anywhere in the module.

The read half is genuinely done and certified, and this audit does not disturb that finding. The
work of sprint 334 is to build the missing write half so that versioning — not exclusion —
decides which transactions proceed, and then to let the measured write-scaling curve, today
**0.83× at sixteen writers**, be the evidence that it did.

The correction this audit had to make to its own first draft is worth recording: the physical
undo log looked like an alternative mechanism to discard and **is not** (§4.4). Conflict
detection is what makes physical undo sound under overlapping writers, which is Memgraph's shape
rather than a departure from it. Tracing the mechanism beat reasoning about it.

---

## 15. The one decision this audit cannot make

Everything else in this sprint is an engineering choice with a defensible right answer. This one
changes an observable contract, so it is the user's.

GoGraph fsyncs the WAL **inside** the visibility barrier (`cypher/api.go:5069-5089`). If the
commit bracket that replaces the barrier also spans the fsync, commits still serialise on the
disk and `wal.SyncGroup`'s leader/follower coalescing — already built, already correct, already
unreachable (`store/wal/writer.go:438-537`) — stays unreachable.

| Option | Rule | Consequence |
|---|---|---|
| **(a)** | **durable-then-visible** — fsync stays inside the commit bracket, PostgreSQL's rule (`xact.c:1544`→`:1550`) | safest and the smallest behavioural delta; concurrency comes from parallel *apply* only, and commits still queue on the disk |
| **(b)** | **visible-before-durable** — publish visibility, fsync outside the bracket, InnoDB's rule (`trx0trx.cc:1426`→`:1481`) | unlocks real group commit. A reader can observe data that a crash would lose. The client ACK still waits for the fsync, so *durability-on-acknowledgement* — which is what the ACID mandate actually states — is preserved |
| **(c)** | fsync every N transactions, Memgraph's default | **rejected**: it violates the module's durability mandate outright |

Option (b) is what InnoDB actually does and it states its own safety argument in source
(`trx0trx.cc:468-479`): any transaction T2 that observes T's writes and itself writes receives a
larger LSN, so if T's flush fails T2 never commits either. But (b) means a reader can act on a
state that a crash would erase, and no amount of measurement settles whether that is acceptable.

### DECIDED, 2026-08-02: option (a), durable-then-visible.

**And the framing above understates it: (a) does not forfeit group commit.** PostgreSQL achieves
both, and the two mechanisms are orthogonal:

- the **ordering** guarantee is *per transaction* — `XLogFlush` (`xact.c:1544`) happens-before
  that transaction's own `TransactionIdCommitTree` (`:1550`), lexically adjacent inside one `if`;
- the **coalescing** happens *across* transactions — `XLogFlush` uses
  `LWLockAcquireOrWait(WALWriteLock)` and then re-checks whether another backend already flushed
  past the LSN it needs (`xlog.c:2848-2930`), so N committers pay far fewer than N fsyncs.

Nothing in PostgreSQL holds a global lock across the flush. What must not span the fsync is the
**global commit bracket** — the short section that allocates the timestamp and reserves WAL space
— not the per-transaction fsync→publish ordering.

**So the rule for this sprint is:** the global bracket covers timestamp allocation and WAL space
reservation only; each transaction then flushes and publishes in that order, concurrently with
every other transaction doing the same. `wal.SyncGroup`'s leader/follower coalescing
(`store/wal/writer.go:438-537`) is reachable under this rule, so #2193 is **not** forfeited —
which makes (a) strictly better than the trade-off originally presented.

The decision is needed at task B4. Tasks A1–A5, B1–B3 and all of phases C and D are unaffected.

---

## 16. Finding → task map

Every CONTROL-class mechanism and every numbered blast-radius finding has an owning task in
sprint 334. This table is what makes that claim checkable.

| Finding | Owning task |
|---|---|
| §1 the measured baseline | #2297 write-scaling gate |
| §3.1 `visMu` | #2304 (B2) |
| §3.1 `Engine.writeMu`, store semaphore, apply gate (E18) | #2306 (B4) |
| §4.1 / E1 one watermark, no in-progress list | #2298 (A1) |
| §4.2 / E2 no writer txID, no writer snapshot | #2299 (A2) |
| §4.3 / E2 no conflict detection | #2300 (A3) |
| §4.4 physical undo stays; soundness via A3 | #2300, verified in #2305 |
| E3, E4, E16 per-graph commit singletons | #2301 (A4) |
| E5, E20 WAL frame contiguity; `txnSeq` durability | #2302 (A5) |
| E21 recovery / bulk-import barrier-free windows | #2302 (A5) |
| E10 ordering half; E12–E15 publication ordering | #2303 (B1) |
| E10 whole-graph constraint reseed; E19 handle order | #2304 (B2) |
| E6 quiesce boundary; E17 fsync-failure policy | #2306 (B4) |
| E7, E8, E9 checkpoint consistency and watermark | #2310 (C4) |
| E11, §6.4 inline reclamation | #2308 (C2) |
| E22 horizon covers readers only | #2299 (A2), consumed by #2308 |
| §5 group commit unreachable | #2193 |
| §7 clock not recoverable | #2309 (C3) |
| §8 read-committed explicit read tx | #2307 (C1) |
| §9 superseded alternatives, incl. #2051 | #2311 (D1) |
| §10 examples do not exercise the write side | #2313 (D3) |
| §11 knowledge graph thin on MVCC | #2314 (D4) |
| §12.2 documentation drift | #2314 (D4) |
| open #2295 hygiene, #2292 instrument | carried into sprint 334 |

Two items are deliberately **not** given a task: `graph/lpg/edge_create_count.go` beyond the
existing #2295 (traced to a conservative guard threshold, so a latent hazard rather than a wrong
answer), and the `store/bulk` shard-disjoint partitioning proof (§16), which was not audited and
should be if the parallel build is ever driven concurrently with serving traffic.

---

## 17. What this audit did not establish

- **Only §1 is measured.** Every other performance statement here is quoted from
  [`certification-mvcc-2026-08-01.md`](certification-mvcc-2026-08-01.md), not re-derived. No
  mutex profile and no `-race` run informed the classification in §3.
- **The mutex classification is not proven exhaustive** to the last declaration. It covers every
  declaration the grep in §3 found in non-test `graph/`, `cypher/`, `store/` and `bolt/`;
  `internal/`, `metrics/`, `search/` and `ds/` were excluded as harness and observability code.
- **`store/bulk/bulk.go:518-535`** runs a parallel adjacency build whose safety rests on
  shard-disjoint partitioning rather than the commit lock. That partitioning proof was not
  audited.
- **`Graph.ReclaimIdle`** (`graph/lpg/mvcc_gc.go:178-203`) is called from the *read* path
  (`cypher/api.go:2157`) and calls `ReclaimNow`, whose contract demands writer exclusion. The
  argument at `:157-168` is that the per-shard locks supply it. The argument reads as sound but
  rests on every reclaimer taking the same per-shard write lock as the write path; the docs were
  read, not every reclaimer body.
