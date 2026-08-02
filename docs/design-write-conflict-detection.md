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
chain — the same points that already stamp a version:

| store | chain |
|---|---|
| node labels | `graph/lpg/mvcc_labels.go` `pushLabelDelta` |
| node properties | `graph/lpg/mvcc_props.go` |
| node existence | `graph/lpg/` node-life shards |
| adjacency entries | `graph/adjlist/` versioned entry |
| the five per-edge side stores | `graph/lpg/mvcc_sidemap.go` |

A partial detector is worse than none: it would report clean on a store it does
not cover while silently losing the update, and the caller cannot tell the
difference. **Every store lands together, or none does.**

## 6. Abort

A conflicting transaction marks its shared commit record with
`mvcc.AbortedTS` (`graph/mvcc/mvcc.go:33-40`). The existing visibility rule
already handles it with no extra branch on the read path — `AbortedTS` sits
above `TxIDBase`, so `Visible` returns false for every reader that is not the
aborting transaction itself, and the chain becomes reclaimable.

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
