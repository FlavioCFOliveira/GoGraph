# Prior art: MVCC visibility and version-chain reads in PostgreSQL, Memgraph and Neo4j

Sprint 362, rmp #2860 (SPIKE). **No implementation. No decision taken.** The output is
options with trade-offs, for the user to decide on.

Every claim below was read in **source**, at a named tag, in a local clone. No documentation
or recollection is cited as evidence. No source is copied: PostgreSQL is permissive, Memgraph
is BSL 1.1, Neo4j is GPLv3, and **none of their code is GoGraph's to redistribute**. What is
extracted is the structural idea, to be re-implemented idiomatically in Go if the user decides
to proceed.

| project | tag read | commit | date |
|---|---|---|---|
| PostgreSQL | `REL_17_5` | `5e2f3df49d4298c6097789364a5a53be172f6e85` | 2025-05-05 |
| Memgraph | `v3.2.0` | `1c014750d2c7a9d1fbbcd7e9bed3f1aeba1554bc` | 2025-04-21 |
| Neo4j | `5.26.0` | `c68156edf24164435ab1ac257ec633134c2887f7` | 2024-12-03 |

## 0. The question, and the measurement that poses it

`36_mvcc_snapshot_topology` spends **1024.08 s of CPU over 240 s** of wall clock, and
**55.67% of the whole profile lands on one line** —
`graph/adjlist/mvcc_adj.go:196`, `if mvcc.Visible(v.supersededAt(), startTS, txID)`.

The predicate itself is three integer comparisons. What the line waits on is the chain of
loads that feeds it:

| site | flat | what the instruction does |
|---|---|---|
| `mvcc_adj.go:75` `if v.info != nil` | **166.19 s** | dereference into `adjVersion` (24 B, separate heap object) |
| `mvcc_adj.go:76` `return v.info.TS()` | 19.08 s / 48.96 s cum | dereference into `CommitInfo` (8 B, separate heap object) |
| `mvcc.go:105` `ts == txID` | 88.40 s | compare |
| `mvcc.go:107` `ts < TxIDBase` | 54.75 s | compare |
| `mvcc.go:108` `ts <= startTS` | **217.54 s** | compare |
| `mvcc_adj.go:199` `e = v.prev` | 94.15 s | dereference to the next chain link |

**Three dependent dereferences into three separately allocated objects, per chain step.**

Two corrections to the framing this spike was given, both established from the artefacts and
both load-bearing for the design:

1. **The chain is bounded, not growing.** `mvcc.versions_peak=102` — 102 live adjacency
   version records **graph-wide** at peak, with `Reclaim`→`severChain` active at 2.23% of the
   profile. So **the cost is call count, not chain depth.** A design that shortens chains buys
   nothing here. A design that makes each step touch fewer cache lines, or that avoids
   entering the walk, is what the evidence points at.
2. **The workload is a harness query that never completed.** The driving query is
   `qContradiction`, whose own source comment
   (`examples/36_mvcc_snapshot_topology/main.go:207`) declares it **O(spokes²) predicate
   evaluations** and warns it is affordable only at a modest `-spokes`; the elevated pass ran
   it at `-spokes 4000`, `reader.contradiction_checks=0`, `run_exit=1`. The profile is real;
   the *workload* is not a production shape.

A third measurement bears directly on the options. `(*Snapshot).visible`
(`graph/lpg/snapshot.go:112`) — the pinned-verdict memo added for rmp #2378 — takes a
`sync.Mutex` and inserts into a `map[*commitInfo]bool` **on every visibility test of a
side-map version**, and allocates **5 763.63 MB (12.47%)** of row 36's total and **31.14%** of
`37_mvcc_write_contention`'s.

Full attribution, ranking and reproduction: [`campaign-whole-surface-2026-09-16.md`](campaign-whole-surface-2026-09-16.md) §R7, §R8.

## 1. PostgreSQL 17.5

### What is tested per read

`HeapTupleSatisfiesMVCC` (`src/backend/access/heap/heapam_visibility.c:947`).

**The hinted path — the common case — is bit tests on the tuple header:**

1. `HeapTupleHeaderXminCommitted(tuple)` — one bit test on `t_infomask`.
2. `HeapTupleHeaderXminFrozen(tuple)` — one bit test. **If the tuple is frozen the snapshot is
   not consulted at all.** `src/include/access/htup_details.h` defines
   `HEAP_XMIN_FROZEN (HEAP_XMIN_COMMITTED|HEAP_XMIN_INVALID)` — a reused bit *combination*,
   costing no extra header space.
3. Otherwise `XidInMVCCSnapshot(xmin, snapshot)`.
4. `t_infomask & HEAP_XMAX_INVALID` — one bit test → `return true`.

**`t_infomask`, `t_xmin` and `t_xmax` all live in the tuple header, inside the same 23-byte
prefix as the row.** The visibility state is on the cache line the reader is about to touch
anyway. **This is the single most important structural fact in this document.**

`XidInMVCCSnapshot` (`src/backend/utils/time/snapmgr.c:1856`) is itself two compares in the
common case:

```
if (TransactionIdPrecedes(xid, snapshot->xmin))         return false;   /* older than all */
if (TransactionIdFollowsOrEquals(xid, snapshot->xmax))  return true;    /* newer than all */
… pg_lfind32(xid, snapshot->subxip, …) ; pg_lfind32(xid, snapshot->xip, …)
```

The SIMD array search is entered **only** for xids inside `[xmin, xmax)` — the transactions
that were in flight when the snapshot was taken.

### What is precomputed

- **Hint bits, written back by the reader.** The first reader to resolve a tuple's fate pays a
  CLOG lookup (`TransactionIdDidCommit`, a shared-buffer page read) and then calls
  `SetHintBits`, which ORs `HEAP_XMIN_COMMITTED` / `HEAP_XMIN_INVALID` / `HEAP_XMAX_COMMITTED`
  / `HEAP_XMAX_INVALID` into `t_infomask` and marks the buffer dirty-hint. **Every later
  reader of that tuple pays a bit test.** The module header states the design in one sentence:
  *"all the `HeapTupleSatisfies` routines will update the tuple's "hint" status bits if we see
  that the inserting or deleting transaction has now committed or aborted."*
- **A durability interlock guards the write-back.** `SetHintBits` refuses to set the bit when
  the commit record is not yet flushed and the buffer's LSN gives no interlock:
  *"not flushed and no LSN interlock, so don't set hint"* — it simply returns and a later read
  tries again. Aborts can always be hinted.
- **Freezing removes the snapshot test entirely.** `VACUUM` sets `HEAP_XMIN_FROZEN`, after
  which step 3 above is skipped for that tuple forever.
- **A page-granular gate.** The visibility map holds 2 bits per 8 kB heap page
  (`src/include/access/visibilitymapdefs.h`: `VISIBILITYMAP_ALL_VISIBLE 0x01`,
  `VISIBILITYMAP_ALL_FROZEN 0x02`), and it is explicitly **one-sided**:
  *"The map is conservative in the sense that we make sure that whenever a bit is set, we know
  the condition is true, but if a bit is not set, it might or might not be true."* An
  index-only scan that sees `ALL_VISIBLE` skips both the heap fetch and the visibility test.

### What is amortised into a background reclaim

`VACUUM`. `HeapTupleSatisfiesVacuum` classifies against `OldestXmin` from
`GetOldestNonRemovableTransactionId`; dead tuples are removed, live ones are frozen, and the
visibility-map bits are set. The read path never pays for a version `VACUUM` has removed, and
never pays a snapshot test for a tuple `VACUUM` has frozen.

### What PostgreSQL abandoned, and why

- **`SnapshotNow` is gone.** Verified: **zero occurrences** of the identifier in `src/include/`
  and `src/backend/` at REL_17_5. The "instant" semantics survive only for internal callers,
  and the module header names them as such — `HeapTupleSatisfiesSelf` *"visible to instant
  snapshot and current command"*, `HeapTupleSatisfiesDirty`, `HeapTupleSatisfiesToast` — never
  as a scan snapshot a query can use. The defect a per-tuple "now" test produces is the one
  GoGraph rediscovered independently in rmp #2294 and #2378: **two clauses of one query
  answering from two instants.** *(The removal is verified in this tree; the upstream
  rationale is not quoted from it.)*
- **Old-style `VACUUM FULL` — compaction by moving live tuples in place — was abandoned.**
  Verbatim from `heapam_visibility.c`: *"Note: old-style VACUUM FULL is gone, but we have to
  keep this module's support for MOVED_OFF/MOVED_IN flag bits for as long as we support
  in-place update from pre-9.0 databases"*, and *"pre-9.0 VACUUM FULL always used synchronous
  commits and didn't move tuples that weren't previously hinted."* The scheme needed a third
  transaction id per tuple (`t_xvac`), two more infomask bits (`HEAP_MOVED_OFF 0x4000`,
  `HEAP_MOVED_IN`), **and four extra branches on the hot visibility path** — visible in
  `HeapTupleSatisfiesMVCC` today as dead weight kept only for binary upgrade. It was replaced
  by rewriting the relation wholesale. **This is the directly relevant negative evidence: a
  reclaim strategy that buys compaction by adding state to every object's visibility test was
  tried by the reference engine and retired.**

### Which premises hold for GoGraph

| PostgreSQL premise | holds for GoGraph? |
|---|---|
| The visibility state can live in the object's own header, on the row's cache line | **Yes for the version record** — `adjVersion` can carry a timestamp field. **No for the node** — GoGraph has no per-node struct (`docs/mvcc-p0-measurement.md` §6), which is why delta heads live in sparse side maps |
| A hint write-back needs a durability interlock | **No.** An in-memory adjacency version has no crash-recovery ordering to respect; the WAL already owns durability, and a version record is not replayed |
| A reader may dirty the page it read | **Yes, and more cheaply** — a relaxed atomic store, no buffer manager, no dirty-hint bookkeeping |
| Freezing can remove the snapshot test | **Partly.** GoGraph has no equivalent of an "older than every possible snapshot" state distinct from reclamation: `Reclaim` already *removes* such versions, which is stronger |
| A page-granular one-sided gate amortises over many objects | **No at page granularity** — GoGraph has no pages. **Yes at entry granularity**, which is Option C below |

## 2. Memgraph 3.2.0

### What is tested per read

`ApplyDeltasForRead` (`src/storage/v2/mvcc.hpp:34`).

```
if (!delta || transaction->isolation_level == IsolationLevel::READ_UNCOMMITTED) return 0;
…
auto ts = delta->timestamp->load(std::memory_order_acquire);
if ((isolation == SNAPSHOT_ISOLATION && ts < transaction->start_timestamp) ||
    (isolation == READ_COMMITTED     && ts < kTransactionInitialId))         break;
```

1. **`if (!delta)` — a per-object gate.** An object no writer has touched costs **one pointer
   test and nothing else**. The gate is on `Vertex::delta`, i.e. **per object**, not global.
2. Per step, `delta->timestamp->load(acquire)` — and **`timestamp` is a
   `std::atomic<uint64_t> *`, a pointer to the transaction's shared commit-timestamp atomic**
   (`src/storage/v2/delta.hpp`). So a step is `delta` → `delta->timestamp` →
   the atomic: **the same two dependent dereferences GoGraph pays.**
3. The comparison is `ts < start_timestamp` under snapshot isolation, or
   `ts < kTransactionInitialId` under read-committed — the **same split-range encoding**
   GoGraph copied as `mvcc.TxIDBase`, which `graph/mvcc/mvcc.go`'s package doc already cites.
4. Then a `command_id` test for the `View::NEW` / `View::OLD` distinction, which GoGraph has
   no equivalent of.

### What is precomputed

**Essentially nothing.** A version is never materialised; the object holds its current state
and the reader *reconstructs* the older one by applying deltas backwards. Cost per write is
O(1) in modifications and zero in the size of the graph — which is the property GoGraph
adopted in `docs/design-mvcc-delta-chains.md`.

Two things *are* structurally arranged rather than computed:

- **Deltas are arena-allocated and trivially destructible.** `src/storage/v2/delta.hpp` ends
  with `static_assert(std::is_trivially_destructible_v<Delta>, "any allocations use
  PageSlabMemoryResource, lifetime linked to that, dtr should be trivial")` plus
  `static_assert(alignof(Delta) >= 8, …)`. Deltas written by one transaction come from one
  slab, so chain links written together are on the same pages. *(At this tag there is **no**
  `sizeof(Delta)` assertion — only alignment and trivial destructibility. GoGraph's own record
  of "bounded at 56 bytes" is not backed by a static assert at v3.2.0 and should not be
  requoted as one.)*
- **The object is a struct, so the chain head is a field.** `src/storage/v2/vertex.hpp`:
  `static_assert(sizeof(Vertex) == 88, "If this changes documentation needs changing")`, with a
  `mutable utils::RWSpinLock lock` and a plain `Delta *delta` per vertex. *(GoGraph's
  2026-07-31 record describes a `utils::PointerPack<Delta, 2> delta_` read at `master`; that is
  a later shape than this tag, not a contradiction.)*

### What is amortised into a background reclaim

`InMemoryStorage::CollectGarbage` (`src/storage/v2/inmemory/storage.cpp:1795`), and the source
states the two-phase requirement itself:

> *"Garbage collection must be performed in two phases. In the first phase, deltas that won't
> be applied by any transaction anymore are unlinked from the version chains. They cannot be
> deleted immediately, because there might be a transaction that still needs them to terminate
> the version chain traversal. They are instead marked for deletion and will be deleted in the
> second GC phase."*

- Horizon: `oldest_active_start_timestamp = commit_log_->OldestActive()`.
- **Reclaim granularity is a whole transaction's delta buffer**, not a record:
  `committed_transactions_` → `unlinked_undo_buffers` → `garbage_undo_buffers_`, popped while
  `garbage_undo_buffers.front().mark_timestamp_ <= oldest_active_start_timestamp`.
- One run at a time, `std::unique_lock{gc_lock_, std::try_to_lock}`, **abandoned rather than
  queued** when a run is already in flight.
- The GC takes `main_lock_` **shared**, and escalates to unique only when aggressive — the
  reason given in the source being that an expensive sweep must not block every transaction.

*(GoGraph's `docs/design-mvcc-vacuum.md` §6 already adopted these three decisions, read at
Memgraph `0e8aa326`. They are re-verified here at v3.2.0 and unchanged.)*

### What Memgraph has not done, and says so

Directly above the field, in `src/storage/v2/delta.hpp`:

```
  // TODO: optimize with in-place copy
  std::atomic<uint64_t> *timestamp;
```

**The reference engine GoGraph is modelled on has recorded that this exact indirection should
be collapsed into the delta, and has not done it.** That is the pivotal piece of evidence for
Option A: it is neither a copy of Memgraph's design nor contradicted by it — it is an
optimisation Memgraph names and leaves open, which GoGraph's own measurement now prices at
**166.19 s + 48.96 s of a 1024.08 s profile**.

### Which premises hold for GoGraph

| Memgraph premise | holds for GoGraph? |
|---|---|
| A version record is O(1) per modification and copies nothing | **Yes** — already the adopted design |
| The chain head is a field on a per-object struct | **No.** GoGraph has no per-node struct; the adjacency's head *is* in the entry (`adjEntry.ver`), which is the equivalent — but node labels and properties use sparse side maps |
| Version records come from a per-transaction arena | **Not yet.** GoGraph allocates each `adjVersion` and each `CommitInfo` individually. This is Option B |
| Reclaim frees a whole transaction's buffer at once | **No.** GoGraph's `Reclaim` walks `s.versioned` per shard and severs per entry (49 270 passes, mean 8 µs, in `31_metrics_observability`) |
| A C++ arena's lifetime can be ended explicitly | **No.** Go's GC cannot free part of a slice, so a slab is retained until nothing points into it — which makes Option B's retention **coarser**, not finer |
| The commit timestamp is reached through a pointer | **Yes today — and this is the cost being questioned** |

## 3. Neo4j 5.26.0

Neo4j is the outlier, and the reason matters more than the mechanism.

### What is tested per read — community record storage

`TxState` (`community/kernel/src/main/java/org/neo4j/kernel/impl/api/state/TxState.java`),
class javadoc, verbatim:

> *"This class contains transaction-local changes to the graph. These changes can then be used
> to **augment reads from the committed state of the database** (to make the local changes
> appear in local transaction read operations). At commit time a visitor is sent into this
> class to convert the end result of the tx changes into a physical change-set."*

The store holds **only the committed state**; a transaction's own uncommitted changes live in
a side structure keyed by id — `MutableLongObjectMap<NodeStateImpl>`,
`MutableLongObjectMap<RelationshipStateImpl>`, `MutableLongObjectMap<MutableLongDiffSets>` for
labels and relationship types. **There is no version chain and no per-read visibility
predicate over other transactions' versions at all.** A read fetches the current record and,
only if this transaction has written, overlays it with a long-keyed map probe.

The cost is paid elsewhere: the default isolation is read-committed, so a read is not a
snapshot across statements, and **writers take locks** at node and relationship granularity.
And the overlay grows with the write set, which is why `TxState` carries
`ScopedMemoryTracker`, `HeapEstimator` and `SHALLOW_SIZE`, why `ChunkedTransactionSink` exists
to flush a large transaction in chunks, and why `memory_transaction_max_size` is a
configuration setting at all — `SettingMigratorsTest` shows the older
`cypher.query_max_allocations` being **migrated into** it, i.e. the bound was generalised from
the query to the whole transaction.

### What is tested per read — the versioned page store

`VersionContext` (`community/io/src/main/java/org/neo4j/io/pagecache/context/VersionContext.java`)
specifies two distinct mechanisms, both at **page** granularity, and both in the page cache
rather than in the graph model:

- **Optimistic, with a dirty flag.** *"Read context: reading is performed for a version that
  context was initialised with. **As soon as reader that associated with a context will
  observe data with version that it higher, context will be marked as dirty.** … if at the end
  of reading operation context is not marked as dirty its guarantee that context did not
  encounter any data from more recent transaction."* Interface: `initRead()`,
  `lastClosedTransactionId()`, `markAsDirty()`, `isDirty()`.
- **A high-water mark plus an exception array.** `highestClosed()` —
  *"Together with array of not visible transactions ids determines what versions of pages are
  visible for this context user"* — and `notVisibleTransactionIds()`, *"Array of not visible
  transaction with ids lower to the highest closed"*. The javadoc separates the two modes
  explicitly: *"contexts is snapshot engine use last closed transactions while multi versioned
  stores use highest close and not visible ids to do version filtering."*
- **Page version chains whose head may not be visible:** `observedChainHead(long)`,
  `invisibleHeadObserved()`, `chainHeadVersion()`, `markHeadInvisible()`,
  `resetObsoleteHeadState()`.
- **A reclaim horizon:** `oldestVisibleTransactionNumber()` —
  *"Any version lower than this one isn't visible by any active or future transaction and can
  be removed."*

### The decisive premise difference

**Neo4j's optimistic read is exactly what GoGraph examined and rejected**, in
`docs/design-reader-indicator.md` §7: *"Optimistic reads with a version counter (seqlock).
Readers would have to re-run on a concurrent write, and graph reads return interior pointers
(label slices, property maps), so a retry cannot be made transparent without copying — which
reintroduces allocation."*

Neo4j can afford it **because a Neo4j read already copies**: it decodes a record out of an
8 kB page-cache page into a cursor's own fields, so re-running a read is re-decoding, and the
caller never holds a pointer into live storage. GoGraph's `EntryView` hands back
`e.neighbours` and `e.labels` — slices aliasing an immutable entry — precisely to avoid that
copy. **The premise that makes Neo4j's design work is the one GoGraph deliberately does not
have.** This does not reopen the rejection; it corroborates it with the reason.

Two further granularity forks worth recording:

- Neo4j versions **pages**, so one visibility decision amortises over every record on the
  page. GoGraph versions **an adjacency entry**, i.e. one node's whole out-adjacency — already
  coarser than per-edge, and finer than a page.
- Neo4j and PostgreSQL both use **high-water mark + exception list** (`highestClosed` +
  `notVisibleTransactionIds`; `xmax` + `xip[]`). **GoGraph chose the contiguous frontier
  instead** — `Clock.visible`, *"the highest timestamp every commit at or below which has
  FINISHED"* — so its per-read test is a single `ts <= startTS` with **no exception array to
  search**. GoGraph's predicate is therefore already cheaper than either reference engine's.
  **The predicate was never the problem.**

## 4. What the comparison settles

1. **GoGraph's visibility predicate is already at the floor.** Three comparisons against two
   snapshot-local scalars, with no exception array — strictly cheaper than PostgreSQL's
   `XidInMVCCSnapshot` and Neo4j's `highestClosed` + `notVisibleTransactionIds`. Nothing in
   `mvcc.Visible` is worth changing.
2. **The cost is getting `ts` into a register, and PostgreSQL is the only one of the three
   that solved it** — by putting the visibility state in the object's own header, on the cache
   line the reader already touches, and writing the resolved fate back so later readers pay a
   bit test. Memgraph pays GoGraph's indirection and has an open `TODO` to remove it. Neo4j
   sidesteps the question by not having version chains in the graph model.
3. **Every engine amortises into a background reclaim, and every one of them frees coarsely**
   — PostgreSQL per page, Memgraph per transaction's slab, Neo4j per page version. GoGraph
   frees per record, per entry, per shard, per pass. That is finer retention at a higher sweep
   cost, and it is a deliberate fork, not an oversight.
4. **A per-object gate is universal.** Memgraph's `if (!delta)`, PostgreSQL's one-sided
   visibility map. GoGraph's `entryAsOfLoaded` gates on a **global** `versionActive` counter;
   the project already knows this is wrong in principle (`entryAsOfLoadedVisible`'s own doc,
   rmp #2378) but has not changed the hot caller.
5. **Three approaches are closed and must not be re-proposed** — this project measured them:
   per-shard copy-on-write / persistent maps (HAMT, CTrie), **5.4× time and 43× memory,
   reverted**, rmp #2051 / #1671; an immutable snapshot pointer swap, implemented and
   reverted; seqlock-style optimistic reads, rejected on the interior-pointer argument above.

## 5. Candidate designs for GoGraph

Each is ranked against the brief's criterion — **a per-read cost that does not grow with chain
depth** — and against the measurement that actually applies, which is that **the chain is
bounded at 102 records graph-wide and the cost is call count**.

### Option A — stamp the commit timestamp into the version record once the fate is known

*PostgreSQL's hint bit, re-shaped for an in-memory Go record. The recommended option.*

**Shape.** `adjVersion[W]` gains an `atomic.Uint64` holding either the autocommit timestamp it
already holds, an in-flight sentinel, or — once a reader has observed that `info` committed —
the commit timestamp. `supersededAt` reads that word first and **returns without touching
`info`** whenever it holds a committed timestamp; otherwise it dereferences `info` and, if
that reads a committed value, stores it back with one relaxed atomic store. The store is
idempotent: every writer stores the same value, and `CommitInfo.ts` is monotone per
transaction.

**Per-read cost.** **One dereference per chain step instead of three.** `v.ts` and `v.prev`
are fields of the same 32-byte object, so a step becomes one cache line rather than three.
Depth behaviour is unchanged — still O(depth) — but each step is cheap. Against the measured
profile it targets `mvcc_adj.go:75` (**166.19 s**) and `:76` (**48.96 s cum**), i.e. the two
loads that line 196's 570.06 s is waiting on.

**Write path.** **No change at commit.** `CommitInfo.Commit`'s single atomic store must remain
the one instant at which a transaction's labels, properties and topology all become visible —
that is the reason `CommitInfo` is a shared pointer at all, and it is not negotiable. The
stamp is written lazily, by readers, and optionally by the vacuum sweep, which already walks
the chain in `severChain` and already calls `supersededAt`.

**Reclaim path.** **Cheaper.** `severChain` pays the same indirection today — 9.02 s of its
22.81 s in row 36 is `supersededAt` — and would read one word instead.

**Consequence worth more than the direct saving.** A stamped timestamp is **immutable**, so
the verdict for that version cannot move mid-read. That is exactly what
`(*Snapshot).visible`'s mutex-and-map pinning exists to guarantee (rmp #2378). If every
version a read consults is stamped, the pin is unnecessary for that store — which is how the
**5 763.63 MB (12.47%)** of row 36 and the **31.14%** of `37_mvcc_write_contention` in R8
could be retired, along with a `sync.Mutex` on a read path that the module's own mandates
forbid.

**Risks, stated plainly.**
- The stamp must never carry a *commit* timestamp before the transaction has committed.
  Writing the in-flight id is harmless (it is what `info` holds); writing a commit timestamp
  early would publish an uncommitted change. The write-back must read `info.TS()` and store
  only a value `< TxIDBase`.
- `AbortedTS` must either be stampable or explicitly excluded; `mvcc.go` already relies on
  `AbortedTS` being classified by the ordinary rule, so stamping it is consistent, but it must
  be decided, not assumed.
- **+8 bytes per version record** (24 → 32 bytes). Against 102 live records at peak in row 36
  this is nothing; it is nevertheless a permanent per-version cost paid by every workload that
  writes.
- Retiring `Snapshot.visible`'s pin is a **separate, larger** change and must not be bundled:
  it requires that *every* store a read touches be stamped, and the side maps
  (`sideVersions.asOfSnap`, which drives 99.89% of `Snapshot.visible`'s allocation) are not
  the adjacency. Option A on the adjacency alone does not retire the pin.
- **No reference engine does this for in-memory version records.** PostgreSQL does it for
  on-disk tuples with a durability interlock GoGraph does not need; Memgraph records it as an
  open `TODO`. This is a departure justified by GoGraph's own measurement.

**Validation.** `go test -race ./graph/adjlist/... ./graph/mvcc/... ./graph/lpg/...`; the
isolation battery (`internal/isolationtest`) and the DST multi-session mode, which is what
found rmp #2446 and the #2378 family; `benchstat` over `-benchmem -count=5`; then an
**interleaved** A/B of row 36 at a *completable* scale — not `-spokes 4000`.

### Option B — arena-allocate version records per transaction

*Memgraph's `PageSlabMemoryResource`, re-shaped for Go.*

**Shape.** `linkVersion` takes `&block[i]` from a per-transaction `[]adjVersion[W]` (and a
matching block of `CommitInfo`) instead of allocating each record with `&adjVersion{…}`.

**Per-read cost.** Unchanged in instruction count. Chain links written by the *same*
transaction land on the same pages, so the pointer chase stops crossing the whole heap. **But
row 36's chain is one version per commit from 4 000 different transactions**, so consecutive
steps come from different blocks and **Option B alone would not move row 36's number.** Said
plainly because it is the kind of claim that is easy to assert and wrong.

**Write path.** One bump-pointer per version instead of one `mallocgc`, and far fewer
GC-scannable objects. This is where the gain is: `adjlist.upsertEdgeSlotLocked` allocates
**8 319.42 MB cum (16.33%)** in `26_social_scale_bench` and **4 993.92 MB (10.80%)** in row
36, and `linkVersion` is **524 298 allocation objects (8.04%)** in `11_social_network`.

**Reclaim path.** Materially more complex. **Go's GC cannot free part of a slice**, so a block
survives until nothing points into it — which requires Memgraph's two-phase unlink-then-free
and its `mark_timestamp` ordering. Worse, **retention becomes coarser**: one long-lived reader
pinning one version pins that transaction's whole block. Under row 36's shape — four readers
holding old snapshots — this could **increase** peak memory.

**Recommendation.** Not on this evidence, and not as an answer to the read cost. It is a
candidate for the *write* path if allocation there becomes the binding constraint, and it
should then be measured on the write benchmarks, not on row 36.

### Option C — a per-entry gate instead of the global `versionActive` counter

*Memgraph's `if (!delta)`; PostgreSQL's one-sided visibility map.*

**Shape.** `entryAsOfLoaded` currently short-circuits on `a.versionActive.Load() == 0`. Once
*any* version exists anywhere in the graph the gate is open for every read of every entry.
A per-entry gate would let an unversioned entry return immediately.

**Per-read cost.** **This would recover at most ~2% of row 36.** The loop already breaks on
`v == nil` after one atomic load and one nil test — lines 192 and 193, **13.81 s of the
678.56 s**. Row 36's reads concentrate on a single hot node (the hub), whose entry *always*
carries a version. **Stated as a non-finding rather than offered as a win**, because the
symmetry with the reference engines makes it look like one.

**Note.** The project already knows the global counter cannot answer a per-entry question —
`entryAsOfLoadedVisible`'s doc says so, citing rmp #2378 — and `entryAsOfLoadedVisible` does
not use it. Only `entryAsOfLoaded`, the hot caller, still does. That inconsistency is worth
recording regardless of the performance case.

### Option D — do not ask the question: answer a relationship-type probe without resolving the adjacency

*Not an MVCC design. On this workload it may dominate A.*

**Shape.** All **678.79 s** of row 36's chain walk is reached through one route:
`edgeMatchesRel` → `lpg.EdgeLabelsAsOf` → `EdgeLabelsByIDAsOf` → `slotLabelsForPair` →
`adjlist.EntrySlotLabelsAsOf`. Every one of those calls resolves **the source's entire
adjacency entry, through the version chain**, in order to answer one question about one arc:
*does (h, s) carry type LINK?* A by-handle record that answers it directly already exists and
is cheap — `HasEdgeHandleLabelRecordByID` costs **0.40 s** in `26_social_scale_bench` against
`HasEdge`'s 18.74 s on the same path. Routing `edgeMatchesRel` to a per-(pair, type) record,
or partitioning the adjacency by type, removes the walk rather than accelerating it.

**Per-read cost.** **Constant, and independent of chain depth by construction** — it satisfies
the brief's criterion outright, where A satisfies it only by making each step cheaper.

**Write path.** Whatever maintaining the by-handle or type-partitioned record costs. It
already exists for Cypher-written edges; the gap is edges written through the Go API
(`lpg.Graph.AddEdgeH` + `SetEdgeLabel`), which `relStoredInverted`'s doc names as the reason
its own fallback exists.

**Reclaim path.** A new versioned structure needs its own reclaim, or must be derivable from
one that already has it. **This is the option's real cost** and it is not small.

**Risk.** Highest of the four, and TCK-relevant: relationship-type matching is
TCK-covered behaviour, so any new route must produce identical answers for reciprocal pairs,
undirected hops and reverse expands — the exact cases rmp #2504 records as having been got
wrong before.

**Interaction.** D and R4 in the campaign document are the same defect seen from two sides:
both spend per-row work re-deriving something about an arc that the producer of the row
already knew. They should be scoped together or explicitly scoped apart.

## 6. Recommendation — for the user to decide

**Do Option A first, scoped to the adjacency's version chain and nothing else.** It is the
smallest change, it targets 215.15 s of directly measured load cost in the heaviest profile,
it makes the reclaim sweep cheaper as a side effect, it changes no commit-time semantics, and
its result is bit-identical. Its risks are enumerable and testable.

**Then evaluate Option D on its own evidence**, together with campaign finding R4, because on
this workload avoiding the walk beats cheapening it — and because D is the only option whose
per-read cost is constant in chain depth by construction.

**Do not adopt Option B on this evidence**, and **do not offer Option C as a win.** B is a
write-path candidate whose reclaim implications could increase peak memory under exactly the
shape that motivated this spike; C is worth ~2%.

**Do not re-open** per-shard copy-on-write, persistent maps, immutable snapshot pointer swaps,
or seqlock reads. This project measured the first three and rejected the fourth with a reason
that Neo4j's source independently corroborates.

**This is a recommendation, not a decision.** Stage 4 is a separate iteration, and whether any
of it is built — and in which order — is the user's call.

## 7. What this spike could not establish

- **Why the visibility line is slow is not proven.** That 546.0 s on five instructions is
  memory-stall time rather than branch-misprediction time is the most load-bearing unverified
  statement in either document. It needs hardware counters — cache misses, branch
  mispredictions, IPC — and collecting them was outside this task's scope. **Option A's
  expected gain rests on it.** If the cost turns out to be the branch rather than the loads,
  Option A gains much less and Option D gains everything.
- **No call counts.** How many times `entryAsOfLoaded` was entered, and at what mean depth, is
  unknown: a CPU profile cannot say, and no counter was instrumented. The claim that cost is
  call count rather than depth rests on `versions_peak=102`, which bounds depth but does not
  measure it.
- **The pinning cost is unmeasured as contention.** R8 is ranked on allocation only; the mutex
  profiles that would price the lock exist for 59 rows and one has been read — and **none of
  them was captured at elevated scale**, which is where row 36 lives.
- **Row 36 does not reproduce.** Its elevated-scale run has **exactly one readable CPU profile
  in the whole sweep**; the `out/elevated` and `out/contention` attempts are zero-length files.
  Every figure in §0 therefore rests on a single run, and the first thing stage 4 owes is a
  second one at a scale the example can actually finish.
- **Memgraph `master` was not read.** Only v3.2.0. GoGraph's 2026-07-31 record of a
  `PointerPack<Delta, 2>` at `master` is therefore neither confirmed nor contradicted here, and
  if the `// TODO: optimize with in-place copy` has since been acted on upstream, this document
  would not know.
- **Neo4j's enterprise multi-versioned store was not read** — only the community interface that
  declares its visibility criteria. What the record-level filtering actually does with
  `highestClosed` and `notVisibleTransactionIds` is specified in the javadoc quoted above and
  not verified in an implementation.
- **No GoGraph source file was modified**, and nothing was prototyped. A throwaway worktree
  prototype to test whether Option A moves the number is a decision for the user, not for this
  spike.
