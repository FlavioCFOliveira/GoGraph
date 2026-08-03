# Per-transaction write state

**rmp #2301 (MVCC A4) · sprint 334 · 2026-08-03**

Audit: [`audit-mvcc-sole-cc-2026-08-02.md`](audit-mvcc-sole-cc-2026-08-02.md), findings
**E3**, **E4**, **E16**. This is prerequisite 4 of the four the audit places ahead of barrier
removal (§12.1).

---

## 1. The defect

Three pieces of write-side state were single fields *because there was exactly one writer*:

| # | State | Location before |
|---|---|---|
| E3 | the commit record, the version count and the pending transaction id | `mvcc.WriteStamp` fields, one set per `lpg.Graph` |
| E4 | the adjacency's commit window — `commitDepth int`, `dirtyShards []*adjShard` | `adjlist.AdjList` fields |
| E16 | the re-entrancy guard's writer goroutine id | one slot in `barrierGuard` |

`mvcc.WriteStamp.Begin` said so in its own doc: *"the caller must be the only writer for the
window's duration … so Begin never overwrites a live record."*

### E3 is not a data race — it is silent data loss

Every field involved was already atomic, so `-race` is **silent** on it. The failure is that the
second `Begin` destroys the first transaction's window:

```
writer A  Begin   → info = armedPending(A)
writer A  Stamp   → info = record(A)          A's version points here
writer B  Begin   → info = armedPending(B)    A's window is GONE
writer B  Stamp   → info = record(B)
writer B  End     → returns record(B), count 2   ← B is charged A's version too
writer A  End     → returns nil,       count 0   ← A's record is unreachable
```

**Measured** against the pre-change build (`graph/mvcc`, head `de45a44b`):

```
idA=9223372036854775809 idB=9223372036854775810
recA=0x608e39e2e100  recB=0x608e39e2e108
B closed: rec=0x608e39e2e108 versions=2
A closed: rec=0x0              versions=0
```

A's record is never returned by anything, so it is never committed. A's versions keep transaction
id `9223372036854775809` for the life of the process: **invisible to every reader for ever, and
unreclaimable** — every reclaimer truncates on `stamp <= watermark`, which an in-flight
transaction id can never satisfy. Nothing reports it.

B's count of `2` is the second half: the reclamation budget is charged one transaction's churn
against another's.

---

## 2. The shape

**The transaction owns the state; the graph owns only a slot naming it.**

```go
// graph/mvcc/stamp.go
type TxState struct {          // ONE transaction's stamping state
    info  atomic.Pointer[CommitInfo]   // nil | armedPending | the shared record
    count atomic.Int64                 // versions stamped with info
    txID  atomic.Uint64                // this transaction's identity
}

type WriteStamp struct {       // the graph's ambient slot — no transaction state
    clock     *Clock
    cur       atomic.Pointer[TxState]  // REPLACED, never mutated
    untracked atomic.Int64             // versions belonging to NO transaction
}
```

```go
// graph/lpg/mvcc_writectx.go
type writeCtx struct {
    tx       mvcc.TxState               // by value; &w.tx is what the stamp publishes
    snap     Snapshot                  // the writer's read view
    startTS  uint64
    txID     uint64
    conflict atomic.Pointer[mvcc.Conflict]
}
```

`writeCtx` was already threaded through every lpg write primitive by rmp #2300; this widens it
from *identity* to *the whole of a transaction's mutable write state*. N concurrent writers hold
N distinct `writeCtx` values and each mutates only its own.

### Why the slot still exists

A write that carries no transaction has to resolve one somehow:

- the public Go-API mutators are **per-operation atomic, not transactional**, and cannot carry a
  handle through their signatures;
- `adjlist` reaches the shared record through `mvcc.WriteStamp` and cannot see lpg's transaction
  type at all.

A write that **does** carry its transaction never consults the slot — it is handed the state
directly. That distinction is why conflict detection is threaded rather than looked up: reading
the snapshot from a per-graph field is what produced the false conflict that reverted #2300's
first wiring (`graph/lpg/mvcc_writectx.go`, `TestWriteCtx_DisjointDirectWritersDoNotConflict`).

### E4, resolved earlier in the sprint

`commitDepth` and `dirtyShards` were replaced by a per-shard `buildingOwner` token
(`graph/adjlist/adjlist.go`, commit `e3a0ea1e`): a builder is reused only by the transaction that
created it. Ownership *is* the transaction's identity, so there is no shared counter to race on,
no shared dirty list to append to, and nesting needs no depth — nested statements of one
transaction share its commit record, which is the whole reason that record exists.

### E16, resolved earlier in the sprint

The re-entrancy guard holds a **set** of writer goroutine ids (`graph/lpg/reentrancy_enabled.go`,
commit `a8545173`). The released binary still links the zero-sized no-op.

---

## 3. The hazard recycling introduces, and the sentinel that closes it

Per-transaction state and a zero-allocation bracket pull in opposite directions. A field on the
`Graph` gives the second and fails the first; a fresh allocation per bracket gives the first and
fails the second — and
`TestBarrierGuard_ApplyAtomicallyAllocatesNothing` has asserted zero allocations since #2168.
So the state is **recycled**, which means it outlives the transaction that used it.

An unsynchronised public-API mutator can reach a `TxState` through the slot and arrive **after**
its owner has finished. Two outcomes, and only one of them is safe:

| the late version is stamped … | consequence |
|---|---|
| with the next transaction's record | it becomes visible **later** than it happened — safe, and what the pre-change code already did |
| with its own transaction's already-committed record | it becomes visible **earlier** than it happened: a reader whose snapshot predates the write observes it — **a stale snapshot reading the future** |

`Retract` stores `nil`, so `Ensure` on a retracted state returns nil and its caller falls back to
an untransacted timestamp of its own. Pinned by
`TestWriteStamp_RetractedWindowRefusesLateVersions`.

The mirror case — a late writer that *allocates* a record into a state whose owner has finished —
strands that record. `TxState.Reusable` reports false while a record is present, so such a state
is **never recycled**; the next bracket allocates a fresh one. Pinned by
`TestTxState_RefusesReuseWhileARecordIsStranded`.

---

## 4. Prior art, read in source

**Memgraph** (memgraph/memgraph, branch `master`, read 2026-08-02) — `src/storage/v2/transaction.hpp`
allocates the commit record on the heap "because `Delta`s have a pointer to it, and that pointer
must stay valid after the `Transaction` is moved", and `src/storage/v2/` threads
`Transaction *transaction` into every accessor rather than looking one up. GoGraph's `writeCtx`
is that parameter, and the single atomic store into the shared record is that indirection's
purpose. It must not regress into a per-delta timestamp.

**PostgreSQL** (postgres/postgres, branch `master`, read 2026-08-03;
`src/backend/access/transam/xact.c`) keeps the top-level transaction state in a **statically
allocated** struct reused by every transaction the backend runs:

```c
static TransactionStateData TopTransactionStateData = {
	.state = TRANS_DEFAULT,
	.blockState = TBLOCK_DEFAULT,
	.topXidLogged = false,
};

/*
 * CurrentTransactionState always points to the current transaction state
 * block.  It will point to TopTransactionStateData when not in a
 * transaction at all, or when in a top-level transaction.
 */
static TransactionState CurrentTransactionState = &TopTransactionStateData;
```

Recycling per-transaction state rather than allocating it per transaction is therefore the
established shape, not an optimisation invented here. The difference is only in the unit of
reuse: PostgreSQL has one such block per *backend*, because a backend runs one transaction at a
time; GoGraph has one cached block per *graph*, because the barrier admits one write bracket at a
time — and for the same reason, both degrade to allocation rather than to sharing when that
assumption stops holding.

---

## 5. Cost, measured

Apple M-series, 10 cores, `-count=6`, `benchstat`.

### The bracket

| benchmark | before | after | delta |
|---|---|---|---|
| `Barrier_ApplyAtomically` | 19.11 ns ± 1 % | 26.99 ns ± 1 % | **+41.27 %** (p=0.002) |
| `Barrier_ApplyAtomically` allocs | 0 | 0 | — |
| `Barrier_View` | 4.074 ns | 4.074 ns | ~ (p=0.937) |

The regression is real and it is the price of the fix: roughly seven extra atomic
read-modify-writes per bracket, buying per-transaction ownership of the record, the count and the
snapshot.

`sync.Pool` was tried first and rejected **on measurement**: it put the same bracket at
31.3 ns/op (+64 %), all of it `Get`/`Put` bookkeeping — `runtime_procPin` plus the per-P
`poolLocal` walk — on a bracket whose entire job is two atomic stores. One atomic slot does the
same work in a `Swap` and a `Store`, and one slot is the right size because the barrier admits one
write bracket at a time. When rmp #2304 removes the barrier the slot degrades in the only safe
direction: a writer that finds it empty **allocates**, so contention costs an allocation and never
correctness. A per-P pool becomes worth its constant at that point and should be re-measured
then, not assumed now.

### End to end

One Cypher write commit through the engine, in-memory wiring, single writer:

| | before | after | delta |
|---|---|---|---|
| `WriteScaling/mem/writers=1` | 3.090 µs/commit · 323.7k commits/s | 3.111 µs/commit · 321.4k commits/s | **~ (p=0.151, n=5)** |

**Not statistically significant.** The point estimate is +0.68 %, and the bracket cost disappears
into a commit that is 3 µs long. This is the number that governs the decision: correctness
outranks speed, the cost is undetectable where it is actually paid, and the sprint's write-side
gain is a scaling factor rather than a constant.

---

## 6. Correction to the task's stated acceptance instrument

rmp #2301's acceptance criterion 3 asks for a test "verified to REPORT a race against the
pre-change build" and names the race detector as the instrument. **Measured, that premise is
wrong.** Every field of the pre-change `WriteStamp` was atomic, so `-race` is silent on E3; the
defect surfaces as the lost record and stolen version count shown in §1. The instrument is
therefore an *assertion on the retracted record and count*
(`TestWriteStamp_TwoTransactionsDoNotShareState`,
`TestWriteStamp_VersionCountIsPerTransaction`), verified to fail against the pre-change build with
the numbers quoted there. `-race` is still run over the concurrent tests — it is necessary, and it
was never sufficient.

---

## 7. Tests

| test | what it pins |
|---|---|
| `mvcc.TestWriteStamp_TwoTransactionsDoNotShareState` | E3: two open transactions each recover their OWN record; verified to fail pre-change |
| `mvcc.TestWriteStamp_VersionCountIsPerTransaction` | the reclamation budget is charged per transaction |
| `mvcc.TestWriteStamp_RetractedWindowRefusesLateVersions` | a late version is never stamped with an already-committed record |
| `mvcc.TestTxState_RefusesReuseWhileARecordIsStranded` | a state holding a stranded record is never recycled |
| `mvcc.TestWriteStamp_ConcurrentTransactionsUnderRace` | 32 goroutines × 25 transactions: nothing lost, nothing double-charged |
| `lpg.TestWriteCtx_RecycledStateIsNeverSharedAcrossTransactions` | 64 sequential brackets: distinct ids, and no bracket's write visible at a neighbour's commit |
| `lpg.TestWriteCtx_ConcurrentTransactionsKeepTheirOwnRecord` | 24 concurrent substrate transactions, all published |
| `lpg.TestWriteCtx_DisjointDirectWritersDoNotConflict` | the false conflict that reverted #2300 stays fixed |
| `lpg.TestBarrierGuard_ApplyAtomicallyAllocatesNothing` | the bracket still allocates nothing |
| `adjlist.TestBuildingOwner_*` (`mvcc_window_owner_test.go`) | E4: a builder is reused only by its own transaction |
| `lpg.reentrancy_multiwriter_test.go` | E16: the guard reports correctly with two writers |

---

## 8. What this does NOT do

- **It does not remove the barrier.** `visMu` still serialises write brackets; retiring it is
  rmp #2304 (autocommit) and rmp #2305 (explicit transactions). This task removes the reason the
  barrier was load-bearing for *correctness* rather than only for *ordering*.
- **It does not put the engine's writes under conflict detection.** Engine writes still pass no
  transaction and therefore take no conflict check, which is deliberate while the barrier makes
  overlapping writers impossible. Threading a transaction from the engine is rmp #2304's scope.
- **It does not thread a transaction into the deferred label-index removal**
  (`Graph.deferLabelIndexRemoval` still resolves through the ambient slot). That structure's
  publication is rmp #2303's subject.
