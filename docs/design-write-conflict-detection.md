# Write-write conflict detection

**Status:** design — the specification for rmp #2300, sprint 334
**Date:** 2026-08-02
**Audit:** [`audit-mvcc-sole-cc-2026-08-02.md`](audit-mvcc-sole-cc-2026-08-02.md) §4.3

---

## 1. What does not exist today

There is **no write-write conflict detection anywhere in the module**. No
first-updater-wins, no validation phase, no abort wired to a statement —
`graph/lpg/mvcc_txn.go:139-143` says so in its own words about `labelTx.abort`.
A grep for `Serializ|Conflict|Retriable` returns only *name*-conflict and
constraint-name-conflict errors.

That was correct while there was one writer.
[`isolation-design.md`](isolation-design.md) states it plainly: "write-write
conflicts and write skew … are impossible by construction". Sprint 334 makes
that sentence false, so it has to be replaced by **detection**. Without it, two
overlapping writers silently lose updates.

## 2. The rule

**A writer may modify an object only if the object's current version is visible
to it.** If it is not, the write is a serialization failure and the transaction
is aborted rather than blocked.

```
conflict(headTS, startTS, txID)  ⇔  ¬ Visible(headTS, startTS, txID)
```

where `headTS` is the effective timestamp of the object's newest version.

Expanding `mvcc.Visible` (`graph/mvcc/mvcc.go:100-109`) gives the three cases,
and they are exactly Memgraph's three:

| head's timestamp | meaning | verdict |
|---|---|---|
| `== txID` | my own uncommitted version | **no conflict** — write again freely |
| `< TxIDBase` and `<= startTS` | committed at or before my snapshot | **no conflict** |
| `< TxIDBase` and `> startTS` | committed *after* I began | **conflict** — first-committer-wins |
| `>= TxIDBase` and `!= txID` | another transaction's uncommitted version | **conflict** — first-updater-wins |

### Why this predicate and not a new one

`mvcc.Visible` is already the read-side test, and the delta chains already run
exactly this expression to decide whether to undo a version —
`nodeLabelDelta.mustUndo` is literally `!mvcc.Visible(ts, startTS, txID)`
(`graph/lpg/mvcc_labels.go:124-130`).

So the write-side conflict test is **the same predicate the read side already
runs, applied to the chain head**: *"would I have had to undo this version to
read it?"* If yes, someone I cannot see wrote it, and I must not overwrite it.
Reusing it means there is one definition of visibility in the module rather than
two that can drift apart — and drift between a read rule and a write rule is
precisely how lost updates get shipped.

## 3. Prior art, read in source

**Memgraph** — `PrepareForWrite` (memgraph/memgraph, branch `master`, read
2026-08-02; `src/storage/v2/mvcc.hpp`):

```cpp
if (ts == transaction->transaction_id) { ... return true; }   // my own
if (ts < transaction->start_timestamp)  { ... return true; }   // committed before me
transaction->has_serialization_error = true; return false;     // anything else
```

Three cases, in that order, on the newest delta's timestamp. GoGraph's rule is
the same three cases, expressed through the predicate it already has. The one
difference is the boundary: Memgraph tests `ts < start_timestamp`, GoGraph
`ts <= startTS`, because GoGraph's start timestamp is the *contiguous frontier*
(rmp #2298) — a commit exactly at the frontier is visible by construction.

**PostgreSQL** — `heap_update`/`heap_delete` return `TM_BeingModified` when the
tuple is being updated by a live transaction, and the caller's behaviour then
*depends on the isolation level*: under READ COMMITTED it **waits** for the other
transaction and re-evaluates (first-updater-wins by blocking), under REPEATABLE
READ and SERIALIZABLE it raises `ERRCODE_T_R_SERIALIZATION_FAILURE`.

**GoGraph takes MEMGRAPH'S SHAPE: fail immediately, never wait.** Two reasons,
both about this engine:

1. **PostgreSQL's wait needs a lock manager and a deadlock detector.** Waiting on
   another writer means a wait-for graph and a way to break cycles in it. The
   whole point of sprint 334 is to *remove* exclusion from the write path; adding
   a blocking wait would put a different one back, and a deadlock detector with
   it.
2. **GoGraph offers snapshot isolation, not read-committed.** PostgreSQL's
   waiting variant exists to serve READ COMMITTED, where re-reading the newer
   row is legitimate. At SI, a transaction may not adopt a version newer than its
   snapshot, so there is nothing useful to wait *for* — the answer after the wait
   is still a serialization failure.

## 4. The error, and what a client sees

A typed sentinel in `graph/mvcc`, wrapped so a caller can identify it with
`errors.Is`, surfaced through the Cypher engine, and mapped at the Bolt boundary
to:

```
Neo.TransientError.Transaction.Outdated
```

**Verified against the driver's classifier, not assumed.** The chain, read in
source (neo4j/neo4j-go-driver, read 2026-08-02):

- `neo4j/error.go:35` — `IsRetryable(err)` delegates to `retry.IsRetryable`.
- `neo4j/internal/retry/state.go:134-149` — for a `*db.Neo4jError` it returns
  `dbError.IsRetriable()`.
- `neo4j/db/errors.go:129-139` — `IsRetriable()` is true when
  `IsRetriableTransient()`, which is `classification == "TransientError"`.
- `neo4j/db/errors.go:94-107` — `parse()` splits the code on `.` and requires
  **exactly four parts**, taking `classification = parts[1]`.

So any four-part `Neo.TransientError.*.*` code is retried by a managed
transaction. GoGraph already emits two of them
(`bolt/server/session.go:526`, `bolt/server/serve.go:1132`).

**Why `Transaction.Outdated` and not `Transaction.DeadlockDetected`.** Neo4j's
own text for `Outdated` (neo4j/neo4j, `Status.java`, read 2026-08-02) is:

> "Transaction has seen state which has been invalidated by applied updates while
> transaction was active. Transaction may succeed if retried."

That is a snapshot-isolation serialization failure, described exactly.
`DeadlockDetected` is the wrong code: its text describes transactions that
"acquired locks in a way that it will wait indefinitely", and GoGraph never
waits — it has no lock to deadlock on. Choosing it would be borrowing Neo4j's
*mechanism* vocabulary for a mechanism GoGraph deliberately does not have.

## 5. Where it hooks

Every versioned store, at the point a new version is linked at the head of a
chain — the same points that already stamp a version. There are **four push
primitives**, not nine sites — the five per-edge side
stores share one generic implementation:

| store | primitive | file:line |
|---|---|---|
| node labels | `nodeLabelShard.pushLabelDelta` | `graph/lpg/mvcc_labels.go:166` |
| node properties | `nodePropShard.pushPropDelta` | `graph/lpg/mvcc_props.go:104` |
| the five per-edge side stores | `sideVersions[K,V].push` | `graph/lpg/mvcc_sidemap.go:150` |
| node existence | `noteNodeBorn` / `noteNodeDied` / `noteNodeRevived` | `graph/lpg/mvcc_life.go:100,121,138` |
| adjacency entries | the versioned entry | `graph/adjlist/` |

Each already has the head in hand — it is the value being displaced — and each
already has a `mustUndo`-shaped stamp accessor (`preimageDelta.stamp`,
`lifeStamp.at`), so the head timestamp needs no new plumbing.

### Status, 2026-08-02: seven of eight stores wired

| store | detection | reached through |
|---|---|---|
| node labels | ✅ | `setNodeLabelInfo` / `removeNodeLabelInfo` |
| node properties | ✅ | `setNodePropertyInfo` / `delNodePropertyInfo` |
| node existence | ✅ | `noteNodeLife` (birth, death and revival) |
| overflow relationship types | ✅ | `pushOverflowVersion` |
| per-handle relationship types | ✅ | `pushHandleLabelVersion` |
| per-handle properties | ✅ | `pushHandlePropVersion` |
| per-ordinal relationship types | ✅ | `pushInstanceLabelVersion` |
| per-ordinal properties | ✅ | `pushInstancePropVersion` |
| **adjacency entries** | ❌ **remaining** | `graph/adjlist`, blocked on the same package's per-transaction commit window (rmp #2301 part b) |

Each wired store has a test in `graph/lpg/mvcc_conflict_stores_test.go` or
`graph/lpg/mvcc_writectx_test.go`, and **each was verified to report the lost
update against a build with `writeCtx.conflicts` forced to false** — all six
new store tests failed with *"the transaction committed at N after writing an
object another writer is still writing: its write is silently lost"*.

The §5 warning that a partial detector is worse than none still stands and is
why the gap is tabulated here rather than left implicit: until the adjacency
row is ticked, a transaction that changes **only** topology is not protected.
The substrate is not claimed complete before it is.

#### Two rules the wiring made explicit

**Detection records, it does not merely return.** Several primitives return
nothing — `removeNodeLabelInfo`, `delNodePropertyInfo`, all five per-edge side
stores — and the first wiring had them *skip* the conflicting write on the
reasoning that the caller would learn of it "from the error its next writing
call returns, or at commit". Neither was true: `commit` could not fail, so a
transaction whose only conflicting write went through such a primitive
**committed successfully having silently dropped it**. Measured, not
hypothesised. The conflict is therefore recorded on the `writeCtx` — Memgraph's
`transaction->must_abort` — and `commit` reads it and refuses, which is where
`Storage::Commit` reads its own.

**A refused write must not land.** The push primitive returning `false` is not
advisory: its caller must abandon the mutation too. Recording no pre-image while
applying the change would leave the store holding a write no reader can undo.

### BLOCKED on rmp #2301 — found by measurement, 2026-08-02 (RESOLVED)

The wiring was implemented on the label and property stores, gated by tests
verified to report a lost update without it, and then **reverted**, because
`make ci` went red on `TestGraph_Concurrent` (`graph/lpg/lpg_test.go:116`) with
a **false** serialization conflict.

**The cause is not the rule; it is who the snapshot belongs to.** The writer
snapshot `noteConflict` reads is `Graph.writerSnap` — **per-graph**, because the
write stamp still is. `reclaimAfterDirectWrite` opens an `ApplyAtomically`
bracket to run a reclamation sweep (`graph/lpg/mvcc_gc.go:135`), and while that
bracket is open **every other goroutine writing through the direct Go API sees
that bracket's snapshot as its own** and is tested against a transaction it has
nothing to do with. 64 goroutines writing disjoint nodes produced conflicts.

**There is no per-goroutine signal to fix it with.** The only structure that
knows which goroutine holds the barrier is `barrierGuard`, and that is
`//go:build race || gograph_debug` — absent from a release build, so it cannot
carry a correctness decision.

Conflict detection therefore cannot be sound until the writer snapshot is
**per-transaction**, which is exactly rmp #2301. That dependency is not in the
audit's graph (#2300 is recorded as depending only on #2299) and was found only
by running the module's own concurrency test against the wiring.

What survives the revert and is not blocked: the rule (`mvcc.Conflicts`), the
typed error (`mvcc.ErrSerializationConflict`, `mvcc.Conflict`), their tests, and
this document.

**Resolution.** rmp #2301 introduced `writeCtx` — the commit record, start
timestamp and transaction id as one value passed by pointer and threaded through
the write path, as Memgraph threads `Transaction *transaction` into every
accessor. The snapshot now travels *with* the write instead of being looked up
beside it, so two concurrent writers hold two distinct values and neither can be
tested against the other's. `TestWriteCtx_DisjointDirectWritersDoNotConflict`
reproduces the exact 64-goroutine workload that caught the defect and is the
regression gate for it.

**How the error reaches the caller.** These primitives return nothing today,
and so do their callers (`SetNodeLabel` and friends). Threading a serialization
failure out of them is the substance of the wiring, and it must reach the point
that can abort the transaction and mark its commit record `AbortedTS` — not be
swallowed at the first frame that has no error return.

That is deliberate scope, not an obstacle: an engine whose write primitives
cannot report failure cannot have conflict detection, and the signature change
is what makes the detection reachable rather than advisory.

A partial detector is worse than none: it would report clean on a store it does
not cover while silently losing the update, and the caller cannot tell the
difference. **Every store lands together, or none does.**

## 6. Abort

A conflicting transaction marks its shared commit record with
`mvcc.AbortedTS` (`graph/mvcc/mvcc.go:33-40`). The existing visibility rule
already handles it with no extra branch on the read path — `AbortedTS` sits
above `TxIDBase`, so `Visible` returns false for every reader that is not the
aborting transaction itself.

### The chain is NOT reclaimable today — measured 2026-08-03, rmp #2318

This section previously claimed the chain "becomes reclaimable". It does not, and
the claim was corrected only when a test was written to assert it.

Measured at `b3e1aa0b`: seed 50 versions, reclaim to zero, abort a transaction
that wrote 50 more, reclaim again with no live reader → **`freed=0`, 50 records
still live**. `AbortedTS` is `^uint64(0)`, the maximum `uint64`, and every
reclaimer truncates on `stamp <= watermark` (`mvcc_reclaim.go:73,79,115,121`,
`mvcc_sidemap.go:234,243`), which that value can never satisfy.

The second half is the one that matters more. `labelTx.abort()` marks the record
and **does not restore the stored value**, so the stored bag still holds the
aborted transaction's writes and the aborted deltas are the only thing masking
them. They are load-bearing *forever*, and a reclaimer that simply skipped the
watermark for them would **expose the aborted writes** — a correctness
regression, not merely a freed leak.

This became live rather than theoretical when conflict detection started aborting
real transactions: before that, `abort` was wired to no statement path, so no
aborted chain existed. The fix is for reclamation to apply the undo to the stored
value and then drop the delta — PostgreSQL's `VACUUM` shape for dead tuples — so
it belongs with the bounded background vacuum (rmp #2308), not here. Tracked as
rmp #2318.

Note that the ordinary engine rollback path is unaffected and must stay so: it
PUBLISHES its record rather than aborting it (`graph/lpg/mvcc_write.go:33-50`),
because `cypher/undo.go` has already restored the stored value physically.

There is **no cascading abort**: the audit established (§13.4) that none of
PostgreSQL, InnoDB or Memgraph implements one, because a transaction can only
have read committed data plus its own, so nothing it read can later be undone.

## 7. What must be proved

Not "the code compiles", but:

- a conflict is detected on **every** versioned store, each covered by a test
  **verified to lose the update** against a build without detection;
- the error reaches a real `neo4j-go-driver` managed transaction and is
  **retried by the driver**, proved end to end rather than asserted from the
  code path;
- an aborted transaction's versions are never visible **and are reclaimable**;
- **disjoint writers never conflict** — the write-scaling gate's disjoint
  key-space arm (rmp #2297) must report zero serialization errors, which is what
  distinguishes conflict detection from a global lock wearing a new name.
