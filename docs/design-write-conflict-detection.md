# Write-write conflict detection

**Status:** **implemented** — the specification for rmp #2300, sprint 334
**Date:** 2026-08-02
**Audit:** [`audit-mvcc-sole-cc-2026-08-02.md`](audit-mvcc-sole-cc-2026-08-02.md) §4.3

> **Status corrected (2026-09-08 at `efd32fb9`).** The header read "design"; rmp #2300
> has shipped. The sections below are the specification as written, with the delivered
> state marked inline where they disagree with the code.

---

## 1. What does not exist today

There is **no write-write conflict detection anywhere in the module**. No
first-updater-wins, no validation phase, no abort wired to a statement —
`graph/lpg/mvcc_txn.go:139-143` says so in its own words about `labelTx.abort`.
A grep for `Serializ|Conflict|Retriable` returns only *name*-conflict and
constraint-name-conflict errors.

> **SUPERSEDED (rmp #2300 delivered; verified 2026-09-08 at `efd32fb9`).** This section
> is the pre-#2300 baseline, and it is the state this document exists to remove — it is
> retained for the argument that follows, not as current fact. Detection ships:
> `mvcc.ErrSerializationConflict` and the `mvcc.Conflict` error that wraps it are in
> `graph/mvcc/conflict.go:56` and `:63`, the rule itself is `mvcc.Conflicts` (`:144`),
> and `graph/lpg/mvcc_txn.go` now documents the opposite of the sentence above: its
> `labelTx.commit` reads the conflict record off the `writeCtx` as a backstop and
> ABORTS, marking the shared commit record `mvcc.AbortedTS` (see the doc comment at
> `graph/lpg/mvcc_txn.go:129-146` above `labelTx.commit`, which cites Memgraph's
> `Storage::Commit` / `transaction_.must_abort` as the prior art). The cited line range
> `graph/lpg/mvcc_txn.go:139-143` now lands inside that very argument, so it no longer
> supports the claim it was cited for.

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

### 4.1 The trap: two TransientError codes are silently demoted

The rule above — "any four-part `Neo.TransientError.*.*` code is retried" — is
**not quite true**, and the exception is exactly where an obvious guess lands.
`neo4j/db/errors.go` runs `reclassify()` **before** `parse()`, and it rewrites two
codes out of the family (neo4j-go-driver **v5.28.4**, the version in `go.mod`,
read from the module cache 2026-08-03):

```go
func (e *Neo4jError) reclassify() {
	switch e.Code {
	case "Neo.TransientError.Transaction.LockClientStopped":
		e.Code = "Neo.ClientError.Transaction.LockClientStopped"
	case "Neo.TransientError.Transaction.Terminated":
		e.Code = "Neo.ClientError.Transaction.Terminated"
	}
}
```

So **`Neo.TransientError.Transaction.Terminated` is never retried** — it becomes a
`ClientError` before anything asks its classification. It reads as a natural fit
for "your transaction was aborted, try again", and it would have been silently
wrong: the managed transaction would report the conflict to the application on the
first collision.

`Outdated` is not on that list. Two tests keep it that way, and the second is what
gives the first meaning:

| test | asks |
|---|---|
| `TestFailureCode_SerializationConflictIsRetriedByTheRealDriver` | `neo4j.IsRetryable` — the driver's own exported classifier — about the code the server actually emits |
| `TestFailureCode_DemotedTransientCodesAreNotRetriable` | the same classifier about both demoted codes, and refuses the mapping if the conflict is ever moved onto one |

The instrument was validated against the mistake: with the mapping changed to
`Transaction.Terminated`, the first test fails with *"the driver would NOT retry
…"*. A test that only checked the chosen code would have gone green against that
change.

### 4.2 What the client is told

A conflict is neither a client fault nor an internal error, so neither of
`Session.sanitiseErr`'s existing branches gives the right answer, and without an
explicit case the client would receive *"An internal error occurred"* — wrong
twice: it is not internal, and it hides the one fact worth having. The conflict's
own message is forwarded instead. It names the store the collision was detected in
and nothing else — no timestamps, no transaction ids, no internal state — which
`TestSanitiseErr_ForwardsTheConflictMessage` asserts positively and by
absence.

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

#### A property removal is conflict-tested even when the key is absent (rmp #2943)

A property removal runs the write-write conflict test **before** it looks for
the key, and it runs the test whether or not the key is present. The stored
bag already reflects another in-flight transaction's eager removal, so a
removal that returned early on "key absent" recorded no version and no
conflict; when the peer rolled back, its undo restored the value and the
committed removal was lost.

The test compares the transaction's start with the head of the object's
version chain, and that head covers **every** property of the object, not one
key:

| object | conflict unit | where |
|---|---|---|
| node | the node's property delta chain (`nodePropShard.headStamp`) | `Graph.delNodePropertyInfo`, `Graph.delNodePropertyShared` in `graph/lpg/property.go` |
| relationship instance | the per-handle property bag (`Graph.checkHandlePropConflict`) | `Graph.delEdgePropertyByHandleInfo` in `graph/lpg/edge_handle.go` |

So `REMOVE n.k` is refused with `mvcc.ErrSerializationConflict` when a peer
committed a write to **any** property of `n` after the transaction began, or
has one pending — including a peer that wrote a different key, and including a
removal of a key `n` does not carry. This is the same per-node unit that
`SET` already uses.

**The used/unused key asymmetry.** Both removal paths first resolve the key
name in the graph's property-key dictionary (`propKeys().Lookup`,
`pkeys.Lookup`) and return before the conflict test when the name has never
been interned. Therefore:

- `REMOVE n.k`, where some node or relationship in the graph has used `k`: conflict-tested, and refused against a peer's concurrent write to any property of `n`;
- `REMOVE n.k`, where no write has ever used `k`: a no-op that records nothing and is never refused.

Both outcomes are correct. Removing a key the object does not carry changes
nothing, so admitting it and refusing it both leave a state that some serial
order produces. The refusal is conservative, not a lost update: it reaches the
client as a retriable conflict (§4), and a retry that begins after the peer
has finished is not refused on the peer's account. An unused name needs no
test: a write interns its key before it lands, so no transaction can hold a
pending write of a name that was never interned, and there is no removal whose
loss a peer's undo could cause.

#### A direct Go-API write is an implicit single-operation transaction (rmp #2947)

A direct mutator of `lpg.Graph` — one called outside any transaction — used to
pass a nil `writeCtx`, which every store reads as "never conflicts". It was lost to
a peer's rollback on every surface, for two reasons that compound.

**The ambient slot captured it.** A write that carries no transaction resolves
one through the graph's ambient slot (`WriteStamp.Stamp`, `Graph.writerSnapshot`),
and every write bracket published itself there — the shared
`Graph.ApplyVersioned` and the explicit `Graph.BeginVersionedTx` included, the
latter for its whole life. So a direct write made anywhere in the process while an
explicit transaction was open was stamped with that transaction's record.

**Nothing refused an uncommitted head.** A nil `writeCtx` never conflicts, so a
direct write displaced a version another transaction had not committed — a dirty
write — and the peer's rollback restored its pre-image over it.

**Who may own the ambient slot.** Only an exclusive bracket publishes —
`Graph.ApplyAtomically`, `Graph.ApplyAtomicallyTx` and `Graph.LockBarrierCtx`
(`Graph.beginWrite`, `graph/lpg/mvcc_write.go`). The slot serves
`Graph.ApplyInsideLockedTx`; a direct Go-API write never resolves through it, and
neither does a raw adjacency write (rmp #2967, below). A shared bracket and an
explicit
transaction thread their `writeCtx` through every write (rmp #2320) and do not
publish. An audit instrumented every ambient resolution made while a shared or
explicit transaction owned the slot, across the whole short test layer: one
production write path belonged to a transaction without carrying it — the per-pair
property re-assertion of an edge removal (`Graph.reassertPairProps`) — and it now
carries the removal's transaction.

**The rule.** Every direct mutator runs its transactional body — the same `…Info`
form a statement runs — through `Graph.direct` (`graph/lpg/direct_tx.go`), on a
graph whose versioning substrate is armed, as an IMPLICIT transaction of its own,
whether or not a bracket is open: an id from the ordinary
  sequence with `mvcc.ImplicitTxBit` set, and a start timestamp at the top of the
  commit space (`implicitStartTS`), so it sees every committed version and
conflicts with exactly the uncommitted ones. It claims, conflict-tests and
cross-checks exactly as a statement does. On any refusal it aborts: its record is
marked aborted and every version it wrote is withdrawn before the call returns
(`Graph.abortWake`); on success it commits at one instant.

**A direct write never joins a bracket (audit F7).** A direct write used to join
the open exclusive bracket's transaction. The module cannot tell the bracket's own
goroutine from an unrelated one, so a direct write from another goroutine joined
as well: it was acknowledged, and then it was lost when the bracket aborted. A
write that belongs to a bracket now says so: the bracket's function writes through
`Graph.Writer(tx)` over the `WriteTx` that `Graph.ApplyAtomicallyTx` or
`Graph.ApplyInsideLockedTx` hands it. `Graph.ApplyAtomically` keeps its signature;
its function has no transaction to write through, so it serves exclusive work that
writes no versioned data — index and constraint registration, and the
checkpointer's capture. A direct call made inside any bracket commits at its own
instant and is refused by the bracket's own uncommitted writes like any other
transaction's. Pinned by `TestDirectTx_ForeignWriteBeforeTheBracketIsDoomed`,
`TestDirectTx_ForeignWriteAfterTheBracketIsDoomed` and
`TestDirectTx_DirectCallInsideABracketIsItsOwnTransaction`.

The refusal is an error wrapping both `lpg.ErrDirectWriteConflict` and a
`*mvcc.Conflict` whose `StartTS` and `TxID` are zero, so `errors.Is` with either
sentinel and `errors.As` all match it. It is retryable.

**The lightweight commit.** An implicit transaction is the state a statement's
transaction carries minus what one operation has no use for: no schema-gate hold
(a direct write never took one), no horizon slot (the newest version of every
chain, which is all it reads, is never reclaimed), no ambient slot, no commit
applier and no writer telemetry. Its state is recycled through a `sync.Pool`; its
record is allocated only when it writes a version and is published with the same
allocate-stamp-publish sequence `Graph.endWrite` uses. The adjacency stores an
implicit transaction's entry in place, like an unbracketed write, rather than
cloning a private builder for an operation that writes the shard once.

**Waiting for another direct write.** A refusal caused by ANOTHER implicit
transaction, by a durable store commit's bounded transaction (below), or by an
aborted version that is being withdrawn, is retried from the top after the
refused attempt has aborted, with a short backoff, for at most `directWaitBudget`
(1 s): neither kind of transaction runs caller code between its first write and
its end, so it always finishes. A refusal caused by an explicit
transaction is returned at once — that transaction may be held open across client
round-trips, and these methods take no context to bound a wait by. A retried
append keeps the edge handle its first attempt minted.

**The adjacency's abort withdrawal (rmp #2965).** Rollback of an adjacency entry
used to be physical only — an inverse write, by the engine's undo log or by a
withdrawal the write itself makes — and nothing withdrew an entry on abort.
Measured: an `ApplyVersioned` or `ApplyAtomicallyTx` bracket that added or removed
an edge and was then doomed by a conflict on another store left the change applied,
on directed and undirected graphs, for present-time readers and for the next write,
which built on it. Every abort now withdraws it, through `Graph.abortRecord`
(`AdjList.WithdrawTx`), BEFORE the record is marked aborted:

- **Only the transaction's own entries.** Each transaction keeps its adjacency
  write set — the node id of every entry it versioned — on its `mvcc.TxState`
  (`NoteAdjacency`, `AdjacencyWrites`). The withdrawal visits only those entries,
  each under its own shard lock, so it costs O(the transaction's adjacency writes)
  and nothing for a transaction that wrote none.
- **Only an exact pre-image.** A transaction's writes to one entry share one
  version record. When the record is linked over a committed entry, or no entry,
  it is marked as the transaction's first write over its pre-image
  (`adjOverPreImage`, carried in the version's otherwise unused `ts`), and only a
  head so marked is restored; a pre-image whose own head is aborted is stepped
  back over.
- **A clipped copy.** The aborted entry may have extended the pre-image's backing
  arrays in place, and a lock-free reader may still hold it, so the restored entry
  is a copy whose columns have no spare capacity: the next append allocates
  instead of writing into memory that reader can see.
- The reverse index and the edge count are corrected from the two entries'
  neighbour multisets.

While the record is still in flight its entries are guarded as they were for the
transaction's whole life, so no aborted entry is ever the stored value.

**No transaction builds on another's uncommitted entry (rmp #2966).** An adjacency
entry is an immutable snapshot of every edge out of a node, so any write that
rebuilds it embeds what it holds. Appends and removals claimed the node; the
relationship-property writes (`setEdgePropertyInfo`, `delEdgePropertyInfo`) and the
relationship-type writes of explicit transactions (`setEdgeLabelInfo`,
`removeEdgeLabelInfo`, `setEdgeRelTypeAtSlotByIDInfo`) did not, so an explicit
transaction rebuilt an entry another had not committed. Measured: the first
transaction's aborted append was committed by the second, or left as an aborted
stored value that the next direct write committed; an undirected edge was left
half withdrawn; and, through the Cypher engine, a relationship a transaction rolled
back was committed (count 2 where 1 is right). Those rebuilders now claim the
source node (`adjVersions.noteExclusive`), so the node is the unit of conflict
for every adjacency write, as it is for appends since rmp #2445: two transactions
setting properties or types on two relationships out of the same node now
conflict, and the second retries.

That fix was not complete, and the claims alone could not make it so. The ACID
audit that followed found one rebuilder that took no claim: the durable store's
edge apply, `Graph.addEdgeHIfAbsentInfo`, which `txn.Tx.Commit` reaches through
`OpAddEdgeH` inside its bracket. The adjacency admitted it, because
`AdjList.directConflictLocked` let an explicit transaction write over any entry
but an implicit one's. Measured: a store commit's entry embedded an open explicit
transaction's uncommitted arc; an autocommit Cypher reader saw that arc while the
transaction was open; after the transaction rolled back, its withdrawal restored
the stacked entry and the arc stayed in the graph with no WAL record of it; on an
undirected graph `CheckInvariants` reported the edge asymmetric. Two changes close
it:

- **The refusal is structural.** `AdjList.directConflictLocked` refuses EVERY
  write — explicit, implicit, bounded, or untransacted — over an entry another
  transaction published and has not committed, whatever claims the path took.
  Every adjacency write reaches it through `AdjList.storeEntry` or the
  multi-entry paths that test every entry before writing any.
- **`addEdgeHIfAbsentInfo` claims both endpoints** (`adjVersions.claimAppendPair`),
  as `Graph.appendEdgeInfo` does, and stamps an endpoint it creates after the
  insert. A handle already present is a no-op answered before the claim.

The audit also covered the rebuilders that publish an entry outside
`storeEntry`. `AdjList.Compact` republished each trimmed entry WITHOUT its version
chain, so a snapshot reader saw an uncommitted arc (5 arcs where 4 were
committed), the writer test no longer refused over it, and `AdjList.WithdrawTx`
no longer recognised the aborting transaction's entry, which then survived its
rollback; `trimEntry` now carries the chain, as `clipEntry` does.
`AdjList.WithdrawTx` restores an exact pre-image by design, and `AdjList.Reclaim`
only severs chains. Pinned by
`TestDurableEdgeApply_RefusedOverExplicitUncommittedEntry`,
`TestDurableEdgeApply_UndirectedLeavesNoHalfEdge`,
`TestAdjacency_ExplicitWriteRefusedOverExplicitEntryWithoutClaim`,
`TestCompact_KeepsTheVersionChainOfAnUncommittedEntry` and the Cypher
`TestStoreCommit_CannotPublishARolledBackCypherTransactionsEdge`.

**The withdrawal scans an abort requests.** An abort marks, on its `mvcc.TxState`,
which stores it wrote (`Touch`); the node-life and adjacency-claim scans of
`Graph.withdrawAbortedNow` and the vacuum run only after an abort that wrote that
store (`Graph.reclaimAbortedLifeIfPending`, `Graph.clearAbortedClaimsIfPending`).
Measured at 400 000 retained nodes: those two scans were 70% of a property-only
abort, which took 13.5 ms against HEAD's 2.2–2.8 ms; it now takes 0.6–0.9 ms. A
direct append claims both endpoints in one test-then-stamp step
(`adjVersions.claimAppendPair`), so a refused claim allocates no commit record and
needs no abort. Pinned by `TestAbort_AdjacencyWritesAreWithdrawn`,
`TestAbort_AdjacencyWithdrawalIsExact`,
`TestAbort_EdgeWriteCannotStackOnAnUncommittedEntry`,
`TestAbort_UndoThenAbortDoesNotReinstateTheArc`,
`TestAbort_UndirectedStackingLeavesNoHalfEdge`,
`TestAbort_WithdrawalDoesNotShareTheAbortedBackingArray`,
`TestAbort_WithdrawalRacesNoReader`, `TestAbort_AdjacencyInvariantsUnderMixedLoad`,
`TestWithdrawTx_RestoresThePreImage` and the Cypher
`TestExplicitTx_StackedRelationshipWriteCannotCommitARolledBackRelationship`.

An implicit transaction still orders its work so that a refusal never reaches the
adjacency:

- An append is refused by its claims before it writes; the one check that runs
  after the insert, the endpoints' existence cross-check, withdraws the slot it
  inserted by its handle (`Writer.AppendEdge` names the slot; `RemoveEdgeByHandle`
  removes exactly it).
- A removal (`Graph.removeArcInfo`, `Graph.removeAllEdgesFromInfo`) claims both
  endpoints' adjacency, then writes the side stores — the removed instance's
  per-handle records, and when the pair loses its last slot, its overflow types and
  every per-handle and per-ordinal record (`Graph.clearPairSides`) — and only then
  the adjacency. When parallel slots survive, it holds the pair's edge-label shard
  locks from a test of the overflow head through the label re-assertion, so the one
  overflow write that is decided after the removal cannot be refused. The
  unversioned CREATE counters are dropped last.
- No writer may stack an entry on another transaction's uncommitted one
  (`graph/adjlist/direct_conflict.go`): every write refuses an entry another
  transaction published and has not committed. An aborted entry is not refused:
  the abort restored its pre-image before it marked the record.

**A raw adjacency write is its own transaction (rmp #2967).** A write made on
`Graph.AdjList()` directly — or through an `adjlist.Writer` over the zero `mvcc.Tx`
— carries no transaction. While an exclusive bracket held the ambient slot it used
to resolve one there: `AdjList.versionStamp` stamped its version with the bracket's
record, and `AdjList.directConflictLocked` took the bracket's id as the writer's
own, so it could displace the bracket's uncommitted entries too. The module cannot
tell the bracket's goroutine from an unrelated one, and the bracket's abort
withdraws only its own write set (`AdjList.WithdrawTx`), so the write was
acknowledged and then lost: its version carried an aborted record, which every
snapshot reader stepped back over. Now the write never consults the slot for its
version or its conflict test. Over an entry any other transaction — the bracket
included — published and has not committed, it returns a retryable
`*mvcc.Conflict` and changes nothing; otherwise it commits at once under its own
timestamp (`WriteStamp.UntransactedStamp`). Unlike `Graph.direct`, it does not wait
out an implicit transaction: the refusal is returned to the caller. The slot's open
transaction id is still read as a builder-reuse identity (`AdjList.builderOwner`),
which decides only whether the write stores into the bracket's already-published
slot array or clones it.

Every caller that wrote raw adjacency inside a bracket was enumerated by
instrumenting the three slot reads in `graph/adjlist` and running the whole short
test layer: the only hits were tests (the adjlist test harness, two lpg tests and
one Cypher test), and each now writes through `Writer(tx)` or asserts the new
contract. No production path relied on the slot: WAL replay and snapshot apply
write through the graph's mutators, which run as implicit transactions since
rmp #2947, inside an adjacency build window that carries a builder identity and no
transaction; `store/bulk` owns a standalone `AdjList` with no write stamp. Pinned
by `TestRawAdjacencyWrite_NeverJoinsAnExclusiveBracket`,
`TestRawAdjacencyWrite_CommitsOutsideTheBracketRecord` and
`TestDirectConflict_UntransactedWriteNeverAdoptsTheSlot`.

**A durable store commit claims before its WAL record.** `txn.Tx.Commit` used to
append and fsync its WAL record and only then apply the transaction in memory,
under `Graph.ApplyVersioned`. Once direct writes ran as implicit transactions
holding claims, that apply could be refused by a concurrent writer AFTER the
record was durable, and Commit returned `txn.ErrCommittedNotApplied`: durable,
not visible, and not retryable. Measured with four direct writers on the node the
store transactions wrote: 85 of 268 commits in one second (147 of 495 in a second
run); an open explicit transaction on the same node did the same to a single
commit.

Commit now runs through `Graph.ApplyDurable` (`graph/lpg/lpg.go`), in the order
PostgreSQL and InnoDB keep — locks before the commit record:

1. The buffered ops are applied as one write transaction whose versions stay
   uncommitted. A version a transaction has written and not committed is its
   claim: every writer's conflict test refuses it and every reader steps back over
   it — a snapshot read, and since round 5 of the rmp #2965 audit the direct
   present-state accessors too (see below). The op cap and the foldable-record
   bound are checked before this step. Each op's version count is sampled around
   its apply; an op that wrote no version changed nothing and is dropped from the
   WAL (see "The WAL records effects" below).
2. With every claim held, the WAL frames and the `OpCommit` marker are appended
   — the transaction sequence is minted inside the WAL writer's append critical
   section, so sequence order is file order — and the coalesced fsync is awaited,
   then the apply-gate turn is taken, so transactions publish in WAL order. A
   commit in which no op took effect writes nothing and mints no sequence.
3. The transaction publishes. Nothing tests it any more, so a durable record is
   never refused; the store-direct schema counts, which are not versioned, are
   applied here, in sequence order, as replay applies them.

Any refusal in step 1 — a conflict, a validator's refusal, `adjlist.ErrShardFull`
— and any append or fsync failure in step 2 aborts the transaction: its versions
are withdrawn and nothing is visible, and only the step-2 failure can have left
frames, which recovery discards without a durable marker. `Commit` therefore no
longer returns `txn.ErrCommittedNotApplied`. A conflict wraps
`mvcc.ErrSerializationConflict` and is retryable with a new transaction.

The commit's transaction is BOUNDED: its id carries `mvcc.BoundedTxBit`, because
it runs no caller code between its first write and its end. A write refused by one
of its versions — a direct write, or another commit's attempt — first ends its own
attempt, holding nothing, and then PARKS on the bounded transaction's end
(`graph/lpg/txwait.go`). Every bounded transaction is entered in a sharded table
for exactly its lifetime and leaves it on every exit — published, aborted after a
refused apply or a failed fsync, or unwound by a panic — handing its FIFO queue of
waiters to the first live one, whose next attempt inherits the rest. A fresh
writer defers for at most 200 µs while a woken waiter reclaims, so it does not
barge in front of it. Every wait carries the 1 s budget's deadline, so a stalled
fsync times its waiters out with the retryable refusal. A refusal by an implicit
transaction or by an aborted version is still waited out by a short backoff,
because those holders never span an fsync. Nothing waits while holding a claim,
so no wait cycle can form. A refusal by an explicit transaction is returned at
once. The commit reads the latest committed state, as an implicit transaction
does: it replays buffered ops and has no snapshot to be stale against.

The mechanism follows PostgreSQL's in shape and departs from it where GoGraph's
premises differ: XactLockTableWait waits on the writer's transaction lock
(`src/backend/storage/lmgr/lmgr.c`), and heap_update takes the tuple lock first
"to establish our priority for the tuple" (`src/backend/access/heap/heapam.c`),
but PostgreSQL's waiters sleep holding locks and need `DeadLockCheck`
(`src/backend/storage/lmgr/deadlock.c`), which a GoGraph waiter, holding nothing,
does not. Memgraph's `PrepareForWrite` instead refuses at once with
`SERIALIZATION_ERROR` (`src/storage/v2/vertex_accessor.cpp`), rejected here
because a claim spans an fsync. Read at postgres/postgres
`50d6e533e4d9a0f70d798c534254007c83c0d428` and memgraph/memgraph
`3f2d6f8ed27ef6610933a218403f05f7a51a4d81`.

The cost is inherent and is not removed by the queue: writes to the same object
serialise across the fsync, as row locks do in PostgreSQL, so N commits of one
object take at least N fsyncs. Measured with 32 store committers and four direct
writers on one node for 3 s (the round-5 audit probe): 822 commits and a commit
p99 of 1.000 s with abort-sleep-rerun polling; with the queue, 819 commits, zero
refusals and a p99 of 136 ms — about one fsync per committer ahead — at C=8 a p99
of 48 ms, and at C=1 the four direct writers completed 13 937 writes against 479.
Pinned by `TestCommit_HotKeyCommittersAreServedFairly`,
`TestApplyDurable_WaiterParksUntilTheBlockerEnds`,
`TestApplyDurable_WaitersAreServedInArrivalOrder`,
`TestCommit_DirectWritersNeverLeaveADurableCommitUnapplied`,
`TestCommit_RefusedByAnOpenExplicitTransactionLogsNothing` and
`TestApplyDurable_Outcomes`.

**The WAL records effects.** An op that changes nothing — creating a live node,
re-asserting a present label, setting a property to the value it holds, removing
something absent — writes no version, so it holds no claim, and another commit
may change the same object and log first. Logged, the no-op would be replayed
after that commit, where it is no longer a no-op: recovery rebuilt a node the
acknowledged state had deleted, and a label the acknowledged state had removed.
Both commit paths therefore log only the ops that wrote a version
(`lpg.WriteTx.Versions`): `txn.Tx.Commit` drops the others before the append, and
the Cypher engine's durable adapter does not buffer them. A logged op always held
a claim until its transaction published, so no commit that logs before it changed
what it changed, and replay equals memory by construction. The WAL format and the
recovery contract are unchanged. The four schema-DDL ops are always logged, and
memory and replay still cannot diverge for them: their only in-memory effect is
an idempotent set update on the store-direct schema slots, applied after the
fsync while the commit holds its apply-gate turn, so strictly in sequence order;
replay applies the same frames with the same set semantics in WAL order, which is
the same order because the sequence is minted inside the append. Logging only the
ones that change the set would need the set as of every lower-sequence commit at
append time, which is known only after their fsyncs, and a frame dropped on a
stale answer could lose a definition once a checkpoint truncated the frame that
created it. Pinned by
`TestCommit_NoOpOpsAreNotLoggedSoReplayMatchesMemory` and
`TestCypherWAL_NoOpWriteIsNotLoggedSoReplayMatchesMemory`.

**Present-state reads are committed-only.** `Graph.GetNodeProperty`,
`HasNodeLabel`, `NodeLabels`, the edge label and property accessors, the
out-degree family, `HasEdgeHandle`, `AppendEdgeHandles`, `WalkEdgeHandles`, and
`AdjList.HasEdge`, `Neighbours`, the out-degree family and the in-neighbour readers
used to read the stored value, which carries every uncommitted version: a durable
commit applied but not fsynced was visible through them, and withdrawn if the
fsync failed. They now resolve through a committed-only read position on the
caller's stack (`Graph.latestCommitted`, `AdjList.committedEntry`), stepping back
over every uncommitted version as an implicit transaction reads; a read whose
newest version is committed allocates nothing. A transaction reads its own writes
through its own view. The `...AsOf` forms with a nil snapshot still read the
stored value, for writers that must see their own uncommitted work. Pinned by
`TestPresentReads_NeverSeeAnUnpublishedCommit` and
`TestIsolation_DirectReadDoesNotObservePartialTransaction`. The node-existence family (`IsTombstoned`, `TombstonedIDs`, `TombstoneCount`,
`LiveOrder`, `LiveNodeFilter`) is committed-only too since round 6: it resolves
existence through the node-life records (`Graph.NodeExistsAsOf`) at the same
read position and answers from the bitmap after one atomic load when no life
record exists. The bitmap readers keep their old behaviour under the `…Stored`
names, which the engine, the writers and the versioned readers call, so internal
behaviour is unchanged. Pinned by the same test.

The following remain STORED-state primitives, by decision, and say so in their
godoc: each is an eagerly maintained structure or counter with no versioned form,
so it reflects every uncommitted write. They are the label index returned by
`NodeIndex` (and `EdgeIndex`); the aggregate counts `LabelCountExact`,
`LabelsCountExact`, `LabelCountBound`, `NodeLabelsInUse`, `PropertyKeysInUse`,
`RelationshipTypesInUse` and `EdgeCreateCount`; `AdjList.Size` and
`AdjList.Order`; and the raw entry loaders `AdjList.LoadEntry`, `LoadEntryH`,
`LoadEntryLabels`, `LoadEntryAux`, `LoadEntryView`, `LoadEntryHAt` and
`LoadEntrySlotLabels`, which the writers depend on. A reader that needs any of
them at a consistent committed instant reads through a snapshot's versioned
forms instead (`LabelBitmapAsOf`, `EntryViewAsOf`, `LabelCountAsOf`).

The degree walkers resolve each neighbour's liveness at the read position too
(rmp #2969): through the snapshot when there is one, so a removal committed after
it, or one no transaction has committed, does not hide a neighbour the reader
still sees; only a nil snapshot reads the stored bitmap. `OutDegreeBoundedByIDAsOf` does the
same. Pinned by `TestDegree_SnapshotSeesANeighbourRemovedAfterIt` and
`TestDegree_PresentReadIgnoresAnUncommittedNeighbourRemoval`.

**The per-CREATE-ordinal edge store is retired on the durable engine (rmp
#2968).** `SetEdgeLabelAt`, `SetEdgePropertyAt` and `RemoveEdgeInstance` wrote a
side store keyed by the CREATE ordinal that no WAL frame describes, so it existed
in memory only, and a deleted relationship's entry outlived it. Every reader of
it is a fallback the handle store pre-empts whenever the relationship has a
handle-keyed type record: the relationship materialiser reads it only in the
branch where the handle has no by-handle type, the per-slot type resolver only
through the positional inference for a handle-less, column-less position, and no
executor operator reads its properties. Every relationship the engine creates —
CREATE, MERGE of a pattern, MERGE of a relationship — is added with `AddEdgeH`
and typed with `SetEdgeLabelByHandle`, which is WAL-described, so none of those
fallbacks is reached for it, and a recovered graph, which replays the handle
frames only, never had the ordinal store. The durable adapter therefore writes
nothing to it, which makes the live engine equal to the recovered one by
construction. The in-memory engine keeps the store for edges written through the
Go API without a handle record. Pinned by
`TestDurableEdgeInstanceWrites_SurviveRecoveryAndLeaveNoOrdinalEntry`.

**Tested before every no-op return.** A direct write that decides from the present
state that it has nothing to change may be looking at another transaction's
uncommitted change. Every store tests an implicit transaction's head before such a
return (the shape of rmp #2943): the overflow list, the per-handle and per-ordinal
label sets and removals, a revival of a node that looks alive; an edge-label or
slot-type write claims the source's adjacency before it reads the edge's presence;
an edge-property write leaves the presence decision to the column update, which
tests the entry under its lock first. A pair's whole-pair drop also tests the
versions of instances a pending removal has already taken out of the map
(`sideVersions.conflictWhere`).

**Two label-index holes the claims do not close.** A node retirement claims the
node's existence and not its label store, so:

- A peer's label removal on the retiring node can defer the same index removal
  after the retirement did; when the peer then aborts, its withdrawal dropped the
  key and the retirement's removal with it, leaving the dead node in the bitmap.
  The deferred-removal map now keeps the replaced stamp (`deferredIdx.shadow`) and
  an aborted stamp reinstates it.
- A label set between the retirement's strip and its tombstone flip went into the
  bitmap while the node was alive and was never retired. The retirement re-reads
  the bag after the flip and retires the labels the strip did not see, and a label
  writer re-reads the tombstone after its bitmap add and retires the entry it made
  on a node that died meanwhile; one of the two re-reads always sees the other.

**Distinct from `ErrIndexedRawWrite` (rmp #2848).** That refusal is decided by the
graph's index catalogue, is not retryable, and holds until the indexes are
dropped; this one is decided by one object's version chain and clears on its own.


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

> **SUPERSEDED (rmp #2318 closed; verified 2026-09-08 at `efd32fb9`).** The chain IS
> reclaimable. `graph/lpg/mvcc_abort_reclaim.go` implements exactly the fix this
> section prescribes — the vacuum applies the undo to the stored value and then drops
> the delta — and `mvcc.Conflicts` refuses a writer that would build on a dirty base, so
> an aborted delta is always at its chain's head. Pinned by
> `TestAbort_VersionsAreReleasedBySweep` and `TestAbort_WithdrawnWritesStayInvisible`
> (`graph/lpg/mvcc_abort_reclaim_test.go:22`, `:77`). The measurement below stands as a
> record of the defect at `b3e1aa0b`; see
> [`design-mvcc-abort-withdrawal.md`](design-mvcc-abort-withdrawal.md) for the design as
> built.

This section previously claimed the chain "becomes reclaimable". It does not, and
the claim was corrected only when a test was written to assert it.

Measured at `b3e1aa0b`: seed 50 versions, reclaim to zero, abort a transaction
that wrote 50 more, reclaim again with no live reader → **`freed=0`, 50 records
still live**. `AbortedTS` is `^uint64(0)`, the maximum `uint64`, and every
reclaimer truncates on `stamp <= watermark` (`mvcc_reclaim.go:89,99,147,155`,
`mvcc_sidemap.go:243,254`), which that value can never satisfy.

> **Line references corrected (2026-09-08 at `efd32fb9`).** They read
> `mvcc_reclaim.go:73,79,115,121` and `mvcc_sidemap.go:234,243`; five of those six numbers
> no longer name a truncation site. The six `stamp <= watermark` comparisons at
> `efd32fb9` are `graph/lpg/mvcc_reclaim.go:89`, `:99`, `:147`, `:155` and
> `graph/lpg/mvcc_sidemap.go:243`, `:254`. The claim itself — that `AbortedTS` satisfies
> none of them — is unchanged and still holds.

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

### 7.1 Status against that list, 2026-08-03

Measured, not estimated. Two of the four are complete; two are **blocked on the
barrier's removal**, and the reason is verified in code rather than inferred.

| # | claim | status |
|---|---|---|
| 1 | detected on every versioned store, each verified to lose the update without detection | **DONE** — 7 of 7 versioned stores (`graph/lpg/mvcc_conflict_stores_test.go`) |
| 2 | the error reaches a real driver managed transaction and is retried | **PARTIAL** — see below |
| 3 | an aborted transaction's versions are never visible **and are reclaimable** | **PARTIAL** — never visible: done. Reclaimable: **no**, and that is rmp #2318 |
| 4 | disjoint writers never conflict | **DONE at the substrate**, vacuous at the engine — see below |

**Claim 2, precisely.** Everything that does not need a live collision is done and
tested against the driver itself: the mapping, the driver's own classifier saying
it retries that code, the negative control for the two demoted codes, and the
message-forwarding rule (§4.1, §4.2). What is missing is a collision that actually
travels the wire, and it cannot happen yet:

- the Cypher engine's writes pass **no transaction** (`tx == nil`), so they take no
  conflict check at all — deliberate while nothing can overlap them;
- an explicit transaction takes the exclusive visibility barrier for its whole
  lifetime (`cypher/exectx.go:353`, `LockBarrierCtx`), so two Bolt sessions cannot
  interleave a write;
- and a writer's start timestamp is read **after** the barrier is acquired
  (`Graph.beginWrite`), so even first-committer-wins cannot fire: any commit that
  landed while the writer queued is already inside its snapshot.

So the over-the-wire retry test is gated on rmp #2304 (autocommit) and rmp #2305
(explicit transactions), which is where the engine gains a transaction to thread.
It is recorded here rather than left implied, and #2300 is marked as depending on
#2304 for that reason.

**Claim 4, precisely.** The substrate proof is real and strong:
`TestWriteCtx_DisjointTransactionsDoNotConflict` (two transactions, disjoint
objects, both commit) and `TestWriteCtx_DisjointDirectWritersDoNotConflict` (64
goroutines on disjoint nodes, crossing the reclamation threshold so a sweep bracket
really does open underneath them). The write-scaling gate's disjoint arm also
reports zero errors — but that arm drives the **engine**, whose writes take no
conflict check, so on its own it proves nothing about detection. Saying so is the
point: a green gate over a code path that cannot fail is not evidence, and it
becomes evidence the moment #2304 lands.
