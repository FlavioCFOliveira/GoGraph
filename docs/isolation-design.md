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

## References

- Berenson et al., *A Critique of ANSI SQL Isolation Levels*, SIGMOD 1995.
- Fekete et al., *Making Snapshot Isolation Serializable*, ACM TODS 30(2), 2005.
- Wu et al., *An Empirical Evaluation of In-Memory MVCC*, PVLDB 10(7), 2017.
- Current code: `graph/adjlist/adjlist.go`, `graph/lpg/lpg.go`,
  `graph/lpg/property.go`, `graph/index/label`, `graph/index/manager.go`,
  `graph/generation/generation.go`, `store/txn/txn.go`,
  `store/checkpoint/checkpoint.go`, `store/recovery/recovery.go`.
