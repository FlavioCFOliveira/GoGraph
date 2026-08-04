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
- Write-write conflicts and write skew are the hard parts of SI, and the only
  anomalies that separate SI from serialisability. (Fekete et al., *Making
  Snapshot Isolation Serializable*, ACM TODS 2005.) How GoGraph handles them
  changed in this sprint — see the note below.

We therefore target SI.

> **IN FORCE since 2026-08-04 (rmp #2320).** This paragraph used to say that the
> engine is **single-writer** (`store/txn.Store` mutex), so write-write conflicts and
> write skew are *impossible by construction* and SI is nearly free. **That is no
> longer true, and the change is the point of the sprint.**
>
> The ordinary write path — an autocommit Cypher statement and `store/txn`'s
> in-memory apply — now holds the schema barrier SHARED (`lpg.Graph.ApplyVersioned`),
> so writers overlap. Durable write throughput went from perfectly flat to **15.1x at
> 32 concurrent writers** (267 -> 4041 commits/s), because `wal.Writer.SyncGroup`'s
> leader/follower coalescing becomes reachable once the fsync is no longer inside an
> exclusive hold. The regression gate for it is `bench/mvccwrite`
> `TestWALWriteScalingGate`, ratcheted to 3x at eight writers.
>
> **What replaced exclusion.** Atomic visibility no longer comes from a lock. Every
> version a transaction writes points at ONE shared commit record, and publishing the
> transaction is a single atomic store into it, so a snapshot resolves either all of
> a transaction or none of it however many stores it spanned and whoever else is
> mid-apply. Exclusion made the interleaving impossible; versioning makes it
> unobservable.
>
> **What that required.** rmp #2304 made the same flip first and had to REVERT it:
> the deltas a statement wrote still resolved their commit record through a
> graph-wide AMBIENT slot — every Cypher mutator reached `lpg` through the public API,
> which carried no transaction — so with two brackets open the slot named whichever
> published last and one statement's writes landed on two different commit records.
> Measured with `examples/27_concurrent_txn`: **105 942 torn observations** in one
> run, and 147 overwrites of a still-open transaction's slot. rmp #2320 threaded the
> transaction through: `lpg.WriteView` and `adjlist.Writer` are accessors that carry
> it, the adjacency's write chain takes it as an explicit parameter, and
> `lpg.Graph.AmbientVersionResolutions` counts any write that still resolves through
> the slot so the property is a gate rather than a claim.
>
> **Write-write conflicts are now REAL and are DETECTED.** Two transactions that
> touch the same object no longer queue; the second to reach the version-chain head
> is refused with `mvcc.ErrSerializationConflict` (first-updater-wins), surfaced to
> Bolt as `Neo.TransientError.Transaction.Outdated` so a driver's managed transaction
> retries it (rmp #2300). SI is therefore no longer *free*, and it is no longer
> equivalent to serialisable by construction — it is equivalent to serialisable
> because the conflicting transaction is REFUSED rather than serialised behind a lock.
>
> **A refused transaction ABORTS, and that is not the same as a rollback.** Its commit
> record is marked `mvcc.AbortedTS`, so every reader undoes its versions forever and
> lands on the pre-transaction value. Until rmp #2300 every bracket published its
> record unconditionally, including a refused one, which was an ATOMICITY violation
> measured at the substrate level: a transaction that wrote `n.v = 1`, was then
> refused, and returned `mvcc.ErrSerializationConflict` left `n.v = 1` visible to a
> fresh snapshot. It had not surfaced because the Cypher engine's undo log physically
> restores the stored value — and that undo log is a `cypher` structure, absent for a
> caller using `lpg.Graph.ApplyVersioned` directly, which the durable store's apply
> is. Pinned by `lpg.TestAbort_ConflictedTransactionIsNeverVisible` with a negative
> control (`TestAbort_ASuccessfulTransactionStillPublishes`) so the abort branch
> cannot start firing for transactions that were never refused.
>
> The aborted versions are **not yet reclaimable** — `mvcc.AbortedTS` sits above
> `mvcc.TxIDBase`, so the reclaimer's watermark test can never free the chain. That
> is rmp #2318, and `lpg.TestAbort_VersionsAreNotYetReclaimable_2318` pins the current
> behaviour so closing it has something to invert.
>
> **A client must retry.** This is the half that is new to callers. A conflict can
> arise even between transactions on DISJOINT objects, because a transaction's start
> timestamp is the CONTIGUOUS commit frontier (rmp #2298): under N concurrent writers
> a transaction routinely begins at an instant that excludes its own predecessor's
> commit, so a writer re-touching its own object collides with itself.
> PostgreSQL's `xip` list can express "T1 committed, T0 still running" and GoGraph's
> frontier cannot, so GoGraph refuses where PostgreSQL proceeds. That is the trade
> rmp #2298 accepted in exchange for never handing a reader a straddled commit, and
> the retry is the client's half of it. `examples/27_concurrent_txn` demonstrates the
> retry loop and reports the conflict rate as telemetry; note that the backoff must be
> sized to a WAL fsync, not to a scheduler yield — a `runtime.Gosched` loop was
> measured burning five attempts inside one fsync, every one of them against the same
> in-flight version and with the same stale snapshot.
>
> ### Concurrent `MERGE` and uniqueness — what the attempt exposed
>
> Two latent defects were found that the exclusive barrier hides, both of which must
> be fixed before the barrier can go:
>
> 1. **`UNIQUE` enforcement was check-then-act — FIXED (rmp #2321).**
>    `exec.ConstraintRegistry.CheckSetProperty` took the read lock and only read;
>    `RecordPropertySet` took the write lock later, so two concurrent writers could
>    both pass the check and both insert. Measured with the barrier removed: 14 of 15
>    runs of 8 concurrent `MERGE` under a `UNIQUE` constraint produced 2 or 4 nodes
>    instead of 1.
>
>    Enforcement is now a single atomic test-and-reserve,
>    `exec.ConstraintRegistry.ReserveSetProperty`, following PostgreSQL's
>    `_bt_doinsert`, which holds the leaf buffer lock across `_bt_check_unique` and
>    the insert. A second defect had to go with it: the rollback path rebuilt every
>    value-set from the graph, which under concurrency destroyed the WINNER's
>    reservation (a rebuild cannot see a commit that is not yet durable), so a third
>    writer was then free to duplicate it. Rollback now releases exactly the
>    reservations the statement itself took, journaled as inverses on the transaction
>    undo log — which also resolves the remaining half of audit finding E10, since the
>    whole-graph O(N) walk on the error path is gone. Verified: 20 of 20 `-race` runs
>    converge on one node with all eight statements succeeding.
> 2. **`MERGE` without a uniqueness constraint duplicates under concurrency.** Each
>    statement reads at its own snapshot and cannot see a node whose commit is not yet
>    durable. Neo4j documents the same caveat and directs users to a constraint;
>    Memgraph likewise. This one is expected behaviour for a snapshot-isolated engine
>    rather than a defect, but it only becomes reachable once (1) is fixed, because
>    today the constraint does not reliably help.
>
> `cypher/merge_race_test.go` pins all three arms (unique, no-constraint, serial) and
> passes against the shipped barrier, so the coverage is in place before the change
> that needs it.

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

### The commit frontier — what a reader's instant actually is

`Clock.ReadTS` is the instant a reader starts at, and since rmp #2298 it is the
newest timestamp below which **nothing is still in flight** — not simply the
highest timestamp published.

The two are the same number only while commits are serialised, which is why one
counter sufficed until sprint 334. Once two writers may commit at once they
diverge, and the difference is a wrong answer: writer A allocates 4 and begins
its fsync; writer B allocates 5 and finishes first; a reader starting at "highest
published" takes 5 and observes commit 5 while commit 4 — allocated *earlier* —
is still invisible. It straddles a commit, which is the same torn read that
Example 27's bank-transfer invariant caught during rmp #2290, reached from the
other direction. When A finally publishes, commit 4 appears to a reader that has
already reported a state without it.

`graph/mvcc/commitlog.go` closes it with a bitmap of finished commit timestamps
and a contiguous frontier — **Memgraph's `CommitLog`/`OldestActive` shape, not
PostgreSQL's `xip_list`**. The reason is the read path: with a frontier,
`ReadTS` stays one atomic load and `Visible` stays one comparison. An `xip` list
would put an `xmin`/`xmax` pair plus an array search on the test that runs once
per version-chain node on every versioned read. PostgreSQL can afford that
because a backend holds an xid from its first write until commit — possibly
minutes — so excluding everything above the oldest running xid would make its
snapshots uselessly stale. GoGraph allocates the commit timestamp *at commit*,
so the in-flight window is a commit critical section, not a transaction, and the
frontier costs almost no staleness.

**What it trades.** A reader cannot see a commit above the oldest unfinished one
even after it has published. The staleness is the duration of the longest
in-flight commit — a WAL fsync, measured at 3.73 ms. Memgraph accepts exactly
this trade.

**Two obligations follow, and both are enforced rather than assumed.** A
timestamp that is allocated and then neither published nor abandoned stalls the
frontier *forever*, so an allocate-then-fail path must call
`Clock.AbandonCommitTS`; that is why it is a named operation rather than an
internal detail. And the window is observable: `MVCCStats.InFlightCommits`
reports the distance between the frontier and the newest timestamp handed out,
which is both the staleness and the commit log's memory, named once.

Measured cost: [`docs/benchmarks/mvcc-commit-frontier-2026-08-02.md`](benchmarks/mvcc-commit-frontier-2026-08-02.md).
The read path does not move (unchanged code, 0.27 ns / 0.53 ns, zero allocs);
publishing costs +0.93 ns per commit uncontended and is **1.42× faster** under
concurrent publishers, which is the regime the sprint is moving towards.

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

## Concurrency control is MVCC and nothing else (rmp #2306, first half)

`Engine.writeMu` is gone as a transaction serialiser. It had been held for the whole of
every autocommit statement and from BEGIN to COMMIT of every explicit transaction,
which made a store-less engine single-writer by construction — and it SURVIVED
rmp #2320's removal of the visibility barrier, quietly taking over the property the
barrier had given up. The store-less write-scaling arm measured 0.750× at sixteen
writers while the WAL arm reached 7.886×, and that gap was this lock.

What remains of it is `Engine.schemaMu`, and its shape is the interesting part. A DDL is
not one atomic step — it scans, validates, then registers, and only the registration runs
under the exclusive schema barrier — so the barrier alone cannot make the sequence atomic
against anything. It needs one lock over the whole thing, covering both wirings, because
the WAL-backed DDL paths used to lean on the store semaphore for the same exclusion and
that semaphore is ceasing to be a serialiser.

**Narrowing it to DDL-versus-DDL was the first draft and it was wrong.** A DDL must
exclude ordinary WRITES too: `backfillNodeHashIndex` walks the mapper lock-free, so a
write landing between the backfill scan and the registration is seen by NEITHER — the
scan has passed it, and the index is not yet live to catch it incrementally.
`TestCreateIndexBackfill_ConcurrentWrites` reported it under `-race` as
`w3_22: want 1 row, got []`, and the retired `writeMu` hold had been the only thing
preventing it.

So `schemaMu` is an **RWMutex**: a DDL takes it exclusively for its whole
scan-and-register sequence, an autocommit write takes it shared for its statement. That
keeps the property `writeMu` gave — DDL excludes writes — while dropping the one that made
the engine single-writer, because two writes holding it shared do not exclude each other.
The shared acquisition cost nothing measurable: store-less scaling at sixteen writers went
1.768× → 1.900× across the change.

`visMu` cannot do this job even though it has exactly the same reader/writer shape: its
exclusive hold covers only the registration, and extending it over the backfill would stop
the scan using `Graph.View`, since visMu is not re-entrant. `schemaMu` sits one level out
and needs no such surgery.

### The next ceiling was named by a mutex profile, not guessed

With `writeMu` gone, throughput rose and then DEGRADED past four writers — the shape of
a different lock taking over. A CPU profile showed 65 % of samples in `runtime.usleep`
and `runtime.pthread_cond_wait`, i.e. spinning and parking on acquisition, but not on
what. A mutex profile answered directly: `ConstraintRegistry.ReserveSetProperty` 56.99 %
of all lock delay and `RecordPropertySet` 40.75 % — together essentially all of it — on
a workload whose schema declares **no constraints at all**. One global `RWMutex`, taken
exclusively per property write, free while writers were serialised and the whole
bottleneck once they overlapped.

It is now gated by atomic constraint counters read BEFORE the lock. Reading them
lock-free is sound for a reason the shared/exclusive split supplies: a registration
requires `schemaMu` **and** the schema barrier held exclusively, while an ordinary write
holds that barrier shared for its whole bracket and consults the registry from inside
it — so a constraint cannot appear under a write that read zero.

Store-less scaling at sixteen writers: **0.83× (sprint entry) → 0.750× → 1.242× →
1.900×**, rising instead of peaking at four writers and falling away.

### A writer's deadline is now honoured wherever it can still block

Retiring `writeMu` did not end the unbounded stall an open explicit transaction imposes
on other writers — it MOVED it onto the barrier's shared acquisition, which was equally
context-blind. `lpg.Graph.ApplyVersionedCtx` bounds it through `internal/ctxlock`, the
same mechanism rmp #2174 gave the exclusive side. The bound is owed even after
rmp #2305 removes the transaction-lifetime hold, because a DDL statement legitimately
excludes writers for as long as it runs and a caller with a deadline is entitled to
hear about it.

Making the acquisition fallible exposed a latent bug: `execUnderBarrier` discarded the
bracket's own error with `_ =`, which was safe only while the bracket could not fail
before running its closure. With a bounded acquire it can, and the discard produced
**`(nil, nil)`** — a caller told the statement had succeeded, handed a nil `Result`, and
panicking on first use.

### What rmp #2306 delivered, and what it still owes

**Delivered 2026-08-04.** The store's capacity-one semaphore is retired, together
with a genuine quiesce primitive to replace what `RunUnderCommitLock` used to get
from it: a dedicated admission gate that is closed *only* while a quiesce runs,
merged with the in-flight counter so a writer is registered for its whole
`Begin`→`Commit` lifetime. Closing the gate and draining the count happen under
one mutex, so no writer can be admitted between the two. Two quiesces exclude each
other explicitly rather than as a side effect of serialising everything.

The retirement is **throughput-neutral**, and that refutes the premise: the
semaphore was released after the WAL append and never covered the coalesced fsync
that dominates a durable commit, so it was never the ceiling (both arms scale ~15×
at 32 writers; all differences p > 0.18 at n=6). See
[benchmarks/store-semaphore-retirement-2026-08-04.md](benchmarks/store-semaphore-retirement-2026-08-04.md).
Its value is architectural: MVCC is now the sole concurrency control, and the
quiesce is a quiesce.

**Still owed.** A PROOF that the apply gate's forced sequencing is removable — and
note that the attempt REFUTED it (see the apply-gate note in this document): the
gate stays until conflict detection exempts the apply and the sequencing becomes
per-object. `writeScalingFloor` and `writeConcurrencyFloor` also stay below
`writeScalingTarget`, because they measure the store-LESS wiring, which never had
the semaphore; its ceiling needs its own profile.

## The fsync-failure policy (rmp #2306, decided 2026-08-04)

Group commit is **FAIL-ALL**, and that is kept deliberately rather than by omission.
When the group leader's fsync fails, `wal.Writer.poison` truncates the file back to
the last durable offset, discards the whole un-synced suffix — every member's frames
AND their `OpCommit` markers — sets a sticky error, and wakes every waiter to fail.

**Why it is not softened.** The alternative is to acknowledge a commit whose
durability is unknown, which the ACID mandate forbids outright. Nothing in the failed
suffix can be claimed as durable, so nothing in it may be acknowledged.

**It is the LENIENT end of the prior art.** PostgreSQL does not fail the transaction,
it fails the PROCESS: `issue_xlog_fsync` carries the comment `/* PANIC if failed to
fsync */` and calls `ereport(PANIC, …)`
(`postgres/postgres`, branch master, read 2026-08-04 at commit
`69ed7fd7e9da1cff2f04af04f630287971fe99fe`;
`src/backend/access/transam/xlog.c`). GoGraph cannot take that route — it is a library
embedded in the caller's process, killing the host is not its decision to make, and
the reliability mandate forbids the library from crashing. So the writer handle dies
and says so, which is PostgreSQL's conclusion scoped to what a library owns.

**What DID change is identifiability.** Under a serial batch, fail-all was
unremarkable: the batch was one unit. Once writers are independent, an innocent
member fails because of another transaction's I/O — and it used to receive an
undistinguished error, the same shape it would get from a conflict of its own. The two
demand opposite responses. So every error a poisoned writer returns now wraps
`wal.ErrDurabilityFailed`:

- `errors.Is(err, wal.ErrDurabilityFailed)` identifies the class; `errors.Unwrap`
  still reaches the device error, which is what an operator needs.
- The Bolt boundary maps it to a **DatabaseError**, so the official driver's
  `IsRetriable` (which tests `classification == "TransientError"`) never retries it —
  asserted by asking the real driver in
  `bolt/server.TestFailureCode_DurabilityFailureIsNotRetriedByTheRealDriver`, not by
  restating the classification.
- Every group MEMBER receives the class, not just the leader —
  `wal.TestSyncGroup_FailAll` asserts it per member, which is the case fail-all makes
  possible in the first place.

One inconsistency surfaced while wiring it and was fixed: each `poison` site returned
the bare cause while every *later* caller got the wrapped sticky error, so the member
that poisoned the writer was the only one unable to identify what had happened. All
five sites now return the class.

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

- **F3.2 (done, then SUPERSEDED by rmp #2320).** `Graph.ApplyAtomically(fn)`
  (write lock) and `Graph.View(fn)` (read lock) on `lpg.Graph`. `Tx.Commit` applied
  a transaction's ops inside `ApplyAtomically`, so the whole transaction flipped
  visible to `View` readers as one atomic step. Proven at the time by
  `lpg.TestIsolation_ApplyAtomically_View_NoPartialReads` (50 000 multi-op
  transactions × 8 readers under `-race`, zero violations; power-checked — without
  the barrier the same test observed hundreds of thousands of partial-transaction
  violations).

  **Since rmp #2320** `Tx.Commit` applies under `Graph.ApplyVersioned` (a SHARED
  hold) and the guarantee comes from the transaction's shared commit record, not
  from the lock. `Graph.View` is consequently the SCHEMA barrier only and gives a
  reader no data isolation at all; a reader that needs a consistent view of data
  takes a snapshot (`Graph.BeginRead` + `Graph.ReadAt`). Both halves are pinned:
  `txn.TestIsolation_Commit_NoPartialTransactionObservable` asserts zero partial
  observations through a snapshot, and its negative control
  `txn.TestIsolation_ViewWithUnversionedReadIsNotAtomic` asserts that `View` plus an
  unversioned accessor DOES observe them (measured 8 821–20 011 out of ~11 M reads),
  so the relocation of the guarantee cannot drift back unnoticed.

- **F3.3 (done).** The Cypher engine's query paths now execute under the
  barrier and *materialise* their rows: `Engine.Run` (read) drains the whole
  query inside `Graph.View`; `Engine.RunInTx` (write) drains inside
  `Graph.ApplyAtomically`. (Both moved since: rmp #2290 took `Engine.Run` off the
  barrier entirely — it takes a snapshot and no lock — and rmp #2320 moved
  `Engine.RunInTx` to the shared `Graph.ApplyVersioned`.) Materialising releases the
  lock before the caller
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

  **Since rmp #2320** the eager apply is no longer nested inside an *exclusive*
  visMu hold, so the `View` in the capture contributes nothing to consistency; what
  the argument now rests on entirely is `RunUnderCommitLock`, which closes the
  writer-admission gate AND drains the admitted writers to zero before `fn` runs.
  Since rmp #2306 a writer is registered for its WHOLE lifetime — both paths from
  `BeginCtx`/`Begin` to their `Commit` — where there used to be two abutting
  windows (the semaphore's, then the in-flight counter's) that a quiesce needed to
  abut exactly. So no transaction can be half-applied while the capture walks. rmp #2310 replaces the whole arrangement
  with a capture at a transactional instant.
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
   versa). This was written when writers were serialised anyway, so it read as
   adding only reader exclusion; since rmp #2306 writers are NOT serialised, so an
   exclusive hold here would also reintroduce writer serialisation.
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
