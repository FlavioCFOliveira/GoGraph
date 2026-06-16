# Isolation Design — Snapshot Isolation for the In-Memory Engine

Status: **design (F3.1)** — this is the authoritative specification for the
F3 isolation re-architecture (audit gap F3 in [`acid-audit.md`](acid-audit.md)).
It is implemented in stages F3.2–F3.6; each stage leaves the module green and
never weaker than today's behaviour, and the full guarantee lands at F3.5.

## The gap (what we are fixing)

The ACID mandate requires: *"concurrent transactions behave as if serialised;
readers never observe the partial writes of an in-flight transaction."* Today
a transaction's ops are applied to the live graph **one at a time**
(`store/txn/txn.go` `Tx.Commit` apply loop), and the read surface is a mix of:

- **lock-free** adjacency (`graph/adjlist/adjlist.go`: per-shard
  `atomic.Pointer[shardSlots]`, immutable `adjEntry` snapshots — each *single*
  op is atomically visible); and
- **RWMutex-guarded** everything else: node/edge labels and node/edge
  properties (16 shards each, `graph/lpg/lpg.go:97,141-145,111-126`,
  `graph/lpg/property.go:137`), the global tombstone set
  (`graph/lpg/lpg.go:172` `tombstoneMu`), and the roaring label bitmaps
  (`graph/index/label`).

Consequences:

1. A multi-op transaction's writes become visible incrementally, so a reader
   can observe ops `1..k` applied and `k+1..N` not — a **partial transaction**.
2. Even within a single statement, a query that reads adjacency (lock-free)
   then a label (RLock) then a property (RLock) takes those reads at different
   instants and can straddle a commit, observing a **torn** cross-substructure
   view.

Both violate the mandate.

## Target isolation level

**Snapshot isolation (SI).** A reader pins one committed view at query start
and serves every read of the query from it. Justification:

- The weakest level that satisfies the mandate's literal words is
  *atomic-commit read-committed* (each transaction flips visible all-at-once).
  But a Cypher query issues many reads across all substructures, and a stable
  per-query view is needed to avoid results for a graph state that never
  existed (e.g. `MATCH (a)-[:R]->(b) WHERE a.x > b.x` reading `a.x` and `b.x`
  across a commit). SI provides that stable view.
- The engine is **single-writer** (`store/txn.Store` mutex). Write-write
  conflicts and write skew — the hard parts of SI, and the only anomalies that
  separate SI from serialisability — are impossible by construction. So SI is
  *nearly free* here and is equivalent to serialisable for this write model.
  (Fekete et al., *Making Snapshot Isolation Serializable*, ACM TODS 2005.)

We therefore target SI; we do **not** add MVCC version chains or SSI machinery,
which would pay read-path/GC cost for conflict handling we do not need.

**Shipped increment — read-only explicit transactions (task #1573).** A Bolt
`BEGIN` carrying `mode="r"` opens a read-only explicit transaction via
`cypher.Engine.BeginReadTx`, which acquires **neither** the single-writer
serialisation **nor** the visibility barrier **nor** a WAL transaction.
Each `RUN` inside it executes through the normal concurrent read path
(`Engine.Run`, per-statement `Graph.View` RLock), so read-only transactions run
concurrently with one another instead of serialising on the writer mutex. The
isolation this provides is **per-statement read-committed** (a fresh `View`
snapshot per `RUN`, not one pinned view for the whole transaction), matching
Neo4j's documented default — a deliberate, weaker-than-full-SI choice for the
multi-statement read-transaction case. Safety rests on one load-bearing
invariant: a read-only transaction **rejects every writing clause and DDL**
(`ErrWriteInReadOnlyTx`, surfaced to Bolt as `Neo.ClientError.Request.Invalid`)
*before* execution — a write on this lock-free path would otherwise run with no
writer lock, no barrier and no WAL frame. `Commit`/`Rollback` are teardown-only
no-ops with no durability obligation. Write transactions (`mode="w"`/absent) are
unchanged and keep the single-writer + barrier path described above.

## Mechanism — per-shard versioned single-root snapshot

Reject element-level MVCC and per-element generation tags (they bloat the
cache-friendly hot structures and turn dense scans sparse) and a full
per-commit graph copy (`O(V+E)` per commit). Adopt **structural sharing at
shard granularity** behind one atomic root pointer — the minimal extension of
mechanisms already in the tree (the adjacency list already publishes immutable
per-shard slot slices; `graph/generation` already encodes refcounted snapshot
publication + drain).

### The Snapshot root

An immutable value reachable only through one `atomic.Pointer[Snapshot]` on
`lpg.Graph`:

```
Snapshot {
    adj          [256]*adjShardVersion   // immutable adjacency per shard
    nodeLabels   [16]*labelShardVersion
    edgeLabels   [16]*labelShardVersion
    nodeProps    [16]*propShardVersion
    edgeProps    [16]*propShardVersion
    tombstones   *tombstoneVersion
    nodeBitmaps  *labelBitmapVersion      // immutable roaring snapshot
    edgeBitmaps  *labelBitmapVersion
    indexes      map[string]indexVersion  // hash/B-tree secondary indexes
    commitTS     uint64                   // monotone; the F1 txnSeq watermark
}
```

Every `*…Version` is **immutable once published**. The shard counts match the
existing layout (256 adjacency shards aligned with `graph.NodeID`'s low 8 bits;
16 LPG shards), so the migration replaces *where the version lives*, not the
sharding.

### Read path

A query does **one** `g.snapshot.Load()` at start and threads that `*Snapshot`
through every read, indexing the fixed-size version arrays by the shard bits it
already computes. Result, versus today:

- Adjacency: one atomic load of the pinned, immutable shard version (replacing
  the per-shard `slotsRef.Load()`), then the existing per-slot pointer — **same
  or fewer atomics, zero added allocation**; the inner neighbour scan is
  unchanged.
- Labels / properties / tombstones: an immutable-map read off the pinned
  version, **replacing an RWMutex RLock** — a net *improvement* (lock-free
  where it was locked).

So the read path becomes uniformly lock-free, zero-alloc, and
snapshot-consistent — strengthening isolation while *reducing* read-side
contention.

### Write path (commit)

The single serialised writer builds the **next** Snapshot by copy-on-write at
shard granularity:

1. Load the current `*Snapshot` as the base.
2. For each buffered op, lazily clone *only* the touched shard version(s) into
   a mutable builder (a shard touched by several ops in the batch is cloned
   once). Untouched shard versions are carried by pointer — structural sharing.
3. Freeze the touched versions; assemble one new `*Snapshot` whose arrays hold
   new pointers for touched shards and old pointers for everything else;
   stamp `commitTS` from the F1 `txnSeq`.
4. **`g.snapshot.Store(next)` — one atomic store.** Every op of the
   transaction, across adjacency *and* labels *and* properties *and* bitmaps
   *and* indexes, becomes visible at that single instant. No reader can observe
   `1..k`.

Commit cost is `O(distinct shards touched + ops)`, **independent of graph
size** — a small transaction touches a handful of shards and stays
sub-millisecond on a 10M+ element graph. Reclamation of retired snapshots is by
GC (a pinned `*Snapshot` is kept alive by the reader's reference — the Go
runtime supplies the RCU grace period); the existing `generation` refcount is
used only where backing storage must be held stable across a serialise (e.g. a
checkpoint writing to disk).

## Secondary indexes — atomic flip and the live-maintenance fix

This is where naive "snapshot the adjacency only" designs break, and it folds
in the pre-existing bug the audit found.

- **Roaring label bitmaps** (`nodeIdx`/`edgeIdx`) are today mutated inline
  inside `SetNodeLabel`/`RemoveNodeLabel`/`SetEdgeLabel`. Under SI they must
  become immutable per-label roaring snapshots (roaring clones only the touched
  containers) and be published as part of the same `Snapshot` flip, so a reader
  can never see "edge in adjacency but not yet in the label bitmap".
- **`index.Manager` (hash exact-match, B-tree range).** Verified gap:
  `index.Manager.Apply`/`ApplyBatch` exist but are **never called from any LPG
  live write path** — these indexes are not maintained by live transactions at
  all today. F3.4 wires live maintenance AND makes each registered index fold
  its new version into the same atomic `Snapshot` flip. Subscribers without
  `Serializer` keep the rebuild-on-restart contract.

**Invariant:** every read-servable structure is reachable *only* through the
`Snapshot` root. Any structure left directly mutable-and-read is a hole through
which a partial transaction leaks; the single-root rule is what makes
"no partial reads" provable rather than hoped-for.

## Checkpoint and recovery

`store/checkpoint` today holds the store mutex across the whole
snapshot-write+truncate window, stalling writers during disk I/O. Under SI the
checkpointer instead:

1. `snap := g.snapshot.Load()` (and `generation.Acquire()` if it must hold
   backing storage stable while serialising), recording the WAL watermark
   (`commitTS` == highest durable F1 `txnSeq`).
2. Serialises the pinned immutable snapshot **lock-free**, while writers keep
   committing newer snapshots it does not see.
3. Truncates the WAL up to the watermark under a brief lock.

This makes checkpoints non-blocking for writers and guarantees the on-disk
image is exactly one committed-transaction boundary — which is also what crash
recovery needs (replay frames with `txnSeq` above the snapshot's). Recovery
builds the in-memory Snapshot as it applies the snapshot + WAL tail.

## Staged migration (each stage stays green)

| Stage | Deliverable | Guarantee after stage |
|-------|-------------|-----------------------|
| F3.2 | `Snapshot` root + `atomic.Pointer` + pin API; adjacency reads via pinned snapshot | adjacency reads are transaction-atomic; no regression |
| F3.3 | labels, properties, tombstones move into the snapshot (drop RWMutex reads) | those reads lock-free + consistent with adjacency |
| F3.4 | label bitmaps immutable; live hash/B-tree index maintenance wired into the flip | indexes correct and isolation-consistent |
| F3.5 | commit builds one next-Snapshot for the whole batch and swaps once; checkpoint/recovery read a pinned snapshot | **full SI: no reader ever observes a partial transaction** |
| F3.6 | invariant + property + soak tests; benchmark/TCK regression gate | proven and non-regressing |

A partial migration is safe because the live structures and the snapshot both
reflect the same committed state during the transition; the *partial-transaction*
visibility is only fully closed at F3.5 when the whole-batch single swap lands.
No stage is ever weaker than today's per-op visibility.

## Test strategy (F3.6)

Proof obligation: *for any multi-op transaction T and any concurrent reader R,
R observes all of T or none of T, across every substructure.*

- **Cross-op invariant under `-race`** (short): a writer commits transactions
  that establish a biconditional across substructures (edge `(u,v)` ⇔ `u:Hot`
  ⇔ `v.paired=true` ⇔ edge property ⇔ `bitmap.Has(u)`); dozens of lock-free
  readers assert the biconditional continuously. Any partial view trips it.
- **`rapid` property test**: random transactions + random reader schedules;
  assert every pinned snapshot equals "apply committed transactions `1..t`" for
  some `t` (a committed prefix) — the general no-partial-transaction property.
- **Monotonic-visibility test**: tag commits with an increasing `commitTS`
  property; a reader's set of reads must show one consistent boundary, never a
  mix implying a read across a commit.
- **`goleak`** teardown; **soak** (`GODEBUG=gctrace=1`): long reader vs
  high-commit-rate — invariant never trips, heap bounded after the reader
  releases, no writer starvation.
- **Regression gate**: TCK stays 3897 green; `benchstat` read benchmarks within
  noise; checkpoint-during-writes recovers exactly the committed state at the
  snapshot boundary.

## Implementation status and chosen approach

The guarantee is being delivered **correctness-first** via a transaction-
visibility barrier (a graph-level `sync.RWMutex`, the expert's Approach 4 —
"single visibility flip"), then optimised toward the lock-free per-shard
snapshot above. Rationale: `lpg.Graph` exposes ~10 read-servable substructures
(adjacency; node/edge labels; node/edge properties; tombstones; roaring
bitmaps; `edgeCreateCount`; `edgeInstance*`; secondary indexes), and replacing
all of them with lock-free immutable per-shard versions in one change carries a
high risk of regressing the 3897-scenario TCK. The barrier closes the
*correctness* gap (no reader observes a partial transaction) immediately and
provably, while leaving the immutable-CSR analytics hot path — the perf
mandate's specific lock-free requirement — untouched.

Delivered:

- **F3.2 (done).** `Graph.ApplyAtomically(fn)` (write lock) and `Graph.View(fn)`
  (read lock) on `lpg.Graph`. `Tx.Commit` applies a transaction's ops inside
  `ApplyAtomically`, so the whole transaction flips visible to `View` readers as
  one atomic step. Proven by `lpg.TestIsolation_ApplyAtomically_View_NoPartialReads`
  (50 000 multi-op transactions × 8 readers under `-race`, zero violations;
  power-checked — without the barrier the same test observes hundreds of
  thousands of partial-transaction violations) and the txn-layer integration
  test `txn.TestIsolation_Commit_NoPartialTransactionObservable`.

- **F3.3 (done).** The Cypher engine's query paths now execute under the
  barrier and *materialise* their rows: `Engine.Run` (read) drains the whole
  query inside `Graph.View`; `Engine.RunInTx` (write) drains inside
  `Graph.ApplyAtomically`. Materialising releases the lock before the caller
  iterates, so a long-open `Result` can never deadlock a concurrent writer —
  the property that makes the barrier safe for the lazy, caller-managed
  executor (verified: `cypher/exec` never re-enters `View`/`ApplyAtomically`).
  Proven by `cypher.TestIsolation_Cypher_NoPartialWriteObservable` (concurrent
  `RunInTxAny` writers + `RunAny` readers under `-race`, zero violations;
  power-checked at 3 321 violations with the read barrier removed). TCK stays
  3 897 green. Trade-off: queries now buffer their result rows instead of
  streaming lazily — acceptable for transactional queries (analytics use the
  lock-free CSR path); the lock-free per-shard snapshot below restores
  streaming and is the tracked optimisation.

- **F3.4 (done).** The `index.Manager` hash/B-tree buffer is now committed by
  `commitIndexUnderBarrier` inside the write's `ApplyAtomically` window (right
  after materialize), so the graph and its secondary indexes flip atomically —
  an IndexSeek read can no longer observe a transaction whose graph change is
  visible but whose index change is not. The live roaring label bitmaps already
  update inside the same window (`SetNodeLabel`/`SetEdgeLabel` run there). Lock
  order `visMu → index` matches the read side (`View → index`), so no deadlock.
- **F3.5 (fixed by routing the checkpoint through the commit mutex).** The
  checkpointer now runs its whole snapshot+truncate window under
  `txn.Store.RunUnderCommitLock` (wired via `checkpoint.WithCommitSerialiser`)
  and builds the CSR inside `lpg.Graph.View`. `Tx.Commit`/`RunInTx` hold the
  store's commit mutex from `Begin` to commit (with the eager apply and the WAL
  append nested inside), so while the checkpoint holds that same mutex no
  transaction can be mid-apply or mid-commit: the snapshot is a consistent
  transaction-boundary image and the truncate never drops a frame committed
  after the snapshot (`wal.Writer.Truncate` discards the whole prefix). The
  earlier text claimed the *externally-supplied* `storeMu` already provided
  this — false for the engine path, whose commit mutex is private and was never
  that object, so the old wiring excluded neither window (consistency nor
  truncate-safety). Lock order is store-mutex → visMu, matching the engine
  (`Begin` takes the store mutex, then `ApplyAtomically` takes visMu), so no
  deadlock. F2 proved recovery reconstructs the full state from the snapshot.
  The non-blocking LSN/watermark checkpoint (read a pinned view without holding
  the store mutex for the whole I/O) remains the deferred optimisation.
- **F3.6 (done).** Isolation proven by the invariant battery under `-race`:
  `lpg.TestIsolation_ApplyAtomically_View_NoPartialReads` (property atomicity,
  power-checked), `lpg.TestIsolation_CrossSubstructure_EdgeImpliesLabels`
  (adjacency+label atomicity), `txn.TestIsolation_Commit_NoPartialTransactionObservable`,
  and `cypher.TestIsolation_Cypher_NoPartialWriteObservable` (concurrent
  Cypher reads/writes, power-checked). TCK stays 3897; the barrier spawns no
  goroutines (goleak-neutral).

### Performance trade-off and the optimisation path

The barrier is correctness-first and has two documented costs, both on the
**mutable-graph transactional** path only (the immutable-CSR analytics path is
untouched and stays lock-free):

1. **Reader/writer mutual exclusion.** A write query holds the visibility write
   lock for its execution, excluding concurrent transactional readers (and vice
   versa). Under the single-writer model writers are already serialised; this
   adds reader exclusion during a write.
2. **Materialisation allocations.** Cypher queries now buffer their result rows
   (one shallow `Record` copy per row) instead of reusing a single streaming
   `Record`, adding `O(rows)` allocations per query and holding the full result
   in memory. Acceptable for transactional queries; unbounded result streaming
   is not preserved.

Both costs are removed by the **lock-free per-shard snapshot** (Approach 1c
above): readers pin an `atomic.Pointer[Snapshot]` (no lock, no materialisation,
streaming preserved, no reader/writer exclusion) and the writer swaps it once
per commit. That is the tracked performance end-state; it does not change the
externally-observed isolation contract this barrier already guarantees.

## References

- Berenson et al., *A Critique of ANSI SQL Isolation Levels*, SIGMOD 1995.
- Fekete et al., *Making Snapshot Isolation Serializable*, ACM TODS 30(2), 2005.
- Wu et al., *An Empirical Evaluation of In-Memory MVCC*, PVLDB 10(7), 2017.
- Current code: `graph/adjlist/adjlist.go`, `graph/lpg/lpg.go`,
  `graph/lpg/property.go`, `graph/index/label`, `graph/index/manager.go`,
  `graph/generation/generation.go`, `store/txn/txn.go`,
  `store/checkpoint/checkpoint.go`, `store/recovery/recovery.go`.
