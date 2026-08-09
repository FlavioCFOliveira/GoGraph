# Audit: every correctness argument still resting on the retired pre-MVCC exclusion

**Date:** 2026-08-07 · **Branch:** `sprint-335` · **Task:** rmp #2345

**Outcome: one DEFECT found (rmp #2350, ACID Consistency), nine stale-but-sound sites
reconciled, and the enumeration turned into a re-runnable script.**

## What was being audited, and why

Sprint 334 retired the module's pre-MVCC exclusion mechanisms: `Engine.writeMu`
removed outright (#2306), the exclusive barrier taken off the autocommit write path
(#2304), the transaction-lifetime lock across client think-time removed (#2305), reads
stopped taking the barrier (#2290), the capacity-one store semaphore retired (#2306).
Sprint 335 finished the job by removing `lpg.Graph.View` (#2344).

**The mechanisms are gone. The danger is that code and comments still assume them.**
Three cases were already known, each found by reading rather than by any check:

| Case | Shape | Outcome |
|---|---|---|
| `graph/adjlist` (#2327) | a structure mutated in place because it was believed unobservable | premise violated; reconciled with four asserted properties |
| `store/checkpoint` (#2349) | a multi-step sequence assumed atomic because a drain was believed to cover it | **LOST AN ACKNOWLEDGED COMMIT** |
| `cypher/exectx.go` (this audit) | an ordering assumed because writers were believed serialised | stale prose, contradicting its own file header |

The base rate is not zero, which is the whole reason for the sweep.

## Method (re-runnable — AC4)

`scripts/exclusion_claims.sh` enumerates every site asserting a retired exclusion.
The patterns target **assertion shapes**, not bare lock names: a grep for `visMu`
matches every retraction in the module and is pure noise.

```
bash scripts/exclusion_claims.sh            # grouped by file
bash scripts/exclusion_claims.sh --count    # totals, for tracking
```

It exits 0 always. This is an **inventory, not a gate** — a claim is not a defect on
its face, most are correct statements about locks that still exist, and several are
deliberate retractions that *name* the retired premise in order to correct it. A
pass/fail check would either forbid honest history or demand a suppression list that
rots.

At this commit, after the reconciliations below: **243 sites total, 79 of them in
production code** (excluding `_test.go`, `bench/`, `examples/`). Most of the 79 are
TRUE — they name locks that still exist — which is why the verdict table below is
organised by claim rather than by count.

The verdict test is always the same, and it is the one that found #2349: **what would
this code do if NO exclusion existed?**

## Verdicts — production code

`TRUE` = the exclusion still exists and is relied on. `STALE-BUT-SOUND` = the claim is
false but the code is correct for another reason, **which must now be stated in the
code**. `DEFECT` = the code rests on it.

| Site | Claim | Verdict |
|---|---|---|
| `cypher/constraint_check.go:54` | "the commit-time scan runs under the barrier, so it observes a quiescent graph (no concurrent writer, no in-flight View)" | **DEFECT — rmp #2350** |
| `graph/index/count/count.go` (package doc) | already retracted: "no longer rests on any exclusion" | TRUE (reconciled) |
| `graph/index/count/count.go` — `Store.add` | order-insensitivity via delete-at-exactly-zero | **TRUE, confirmed in code** (not assumed) |
| `graph/index/count/count.go` — `Apply`, `Commit`, `Reseed`, 2 readers | "must be serialised by the caller's write barrier" / "barrier-serialised writers" | STALE-BUT-SOUND → reason now stated |
| `graph/index/hash/index.go:332` | "writers are serialised upstream by the engine's single-writer transaction contract" | STALE-BUT-SOUND → reason now stated |
| `cypher/exectx.go` — `BeginTx` doc | "it does take the graph's visibility barrier exclusively … for the transaction's lifetime" | STALE-BUT-SOUND → corrected |
| `cypher/exectx.go` — `ExplicitTx.view` field | already retracted in place | TRUE (reconciled) |
| `cypher/api.go` — `execUnderBarrier`, the two `writeMu` sites | already state that `writeMu` no longer exists and nothing is serialised | TRUE (reconciled) |
| `graph/lpg/lpg.go` — `LockBarrierCtx` | explained its wait by "`Graph.View` readers hold the barrier's read side" | STALE → corrected (#2348) |
| `bolt/server/{session,serve}.go` — 6 sites | "the explicit transaction holds the engine's single-writer serialisation" | STALE-BUT-SOUND → corrected |
| `store/db.go`, `graph/mvcc/horizon.go`, `store/wal/writer.go` | name locks that still exist and are still relied on | TRUE |
| `store/bulkimport/bulkimport.go:43` | "no concurrent reader and no concurrent writer on the graph it is building" | TRUE — a documented precondition of the API, not an inherited assumption |
| `examples/27_concurrent_txn/main.go:376` | flat throughput is "the signature of the single-writer serialisation that underpins isolation" | STALE **and the inference is wrong** → corrected |

### The two verdicts that took evidence rather than reading

**`count.go` — confirmed, not assumed.** The ticket predicted "sound-but-was-stale"
and required confirmation. `Store.add` deletes a cell at **exactly** zero and retains
negative cells, so addition commutes and the aggregate is order-insensitive; rmp
#2303's fix is present in the code, not merely in its comment. Verdict TRUE for the
mechanism, STALE for six method docs that still *demanded* the barrier.

**`hash/index.go` — sound for a different reason than the one written down.**
`Insert`/`Delete` are individually shard-locked, so no single mutation tears. What
serialisation would additionally buy is atomicity of the DELETE-then-INSERT pair in
the `OpSetNodeProperty` arm. The only interleaving that could strand a stale entry is
two transactions writing the **same node's bound property**, and the substrate refuses
that: the property write path takes a write-write conflict check against the node's
version-chain head (`graph/lpg/property.go:337`, `:405`, verified). **The ordering
guarantee comes from conflict detection on the object, not from exclusion on the
writers** — now stated in the file.

### The defect

`cypher/constraint_check.go`'s commit-time NOT NULL scan reads the graph through
`g.IsTombstoned`, `g.NodeLabelsByID` and `g.GetNodeProperty` — the **raw graph's
present-time, unversioned accessors** — rather than through the committing
transaction's view. GoGraph updates the stored value **in place** and keeps the
inverse in the version chain, so an accessor that resolves no version reads the newest
value, another transaction's uncommitted work included.

Write-write conflict detection does **not** cover it: conflicts are detected per
substore, so two transactions touching the same node through *different* substores —
T1 adding the constrained label, T2 setting or removing the constrained property —
never conflict. Both error directions are reachable: a **false accept** durably commits
a node violating the constraint, and a **false reject** fails a transaction for a
violation that exists in no committed state.

Raised as **rmp #2350** with the mechanism, both directions, the fix (read through
`WriterViewOf(wtx)`), and the regression-test shape. Not fixed here, because #2345 is
an audit and #2350 changes ACID Consistency semantics on two commit paths — it needs
its own regression test and its own gate run.

## The shape to look for next time

All three known cases, and the one found here, are one of the three shapes the ticket
named:

1. **a structure mutated in place because it was believed unobservable** — adjlist;
2. **a multi-step sequence assumed atomic because a lock was believed held across it**
   — the checkpoint's watermark/instant pair;
3. **an ordering assumed because writers were believed serialised** — the hash index,
   the count store, and the NOT NULL check.

Shape 3 is the most common and the least visible, because the code keeps working
until two writers actually overlap on the right objects. **Where a site is sound, the
reason is now stated in the code** — an unstated reason is the same hazard one
iteration later, which is exactly how #2349 survived a sprint.
