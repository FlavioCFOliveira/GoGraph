# The contiguous frontier hides acknowledged commits (rmp #2369, #2368)

**Status: open defect.** An acknowledged commit can be invisible to a later transaction.
This document records the reproduction and the root cause so the fix can be judged
against evidence rather than against a remembered argument.

## The reproduction, at the primitive level

Three calls on a bare `mvcc.Clock`. No engine, no concurrency, no statistics.

```go
var c Clock
a := c.NextCommitTS()   // writer A begins, stays in flight   -> a == 1
b := c.NextCommitTS()   // writer B begins                    -> b == 2
c.PublishCommitTS(b)    // B FINISHES, above A

c.ReadTS()              // == 0   <-- B is acknowledged and INVISIBLE
c.InFlightCommits()      // == 2

c.PublishCommitTS(a)    // A finally finishes
c.ReadTS()              // == 2   <-- only now does B become visible
```

A **finished** commit stays invisible for as long as **any older** commit is in flight,
because `Clock.ReadTS` returns the **contiguous frontier** (`visible`), which advances
only as the commit log's `oldest` advances.

## Why that becomes a consistency defect under load

`Graph.beginWrite` and `Graph.beginWriteCtx` both take a new transaction's `startTS`
from `Clock.ReadTS()`. With sustained concurrent writers there is essentially always a
commit in flight at a low instant, so the frontier is pinned while other commits finish
above it — and every transaction that starts meanwhile inherits the pinned value.

Measured at the engine level, 16 concurrent writers toggling a label plus a serial
stream of acknowledged `CREATE` statements: **2 of 400 acknowledged commits were
invisible to the next transaction.**

```
STALE READ: marker58 was acknowledged but a later transaction saw 0
STALE READ: marker81 was acknowledged but a later transaction saw 0
conflicts observed during the probe: 3274
```

The conflict count is the **bracket**, and it is load-bearing: the same probe run with
40 rounds and no bracket reported **zero** stale reads and passed. A negative oracle
that never reaches the pathological state proves nothing.

Instrumenting the label store's conflict test shows the same fact from the write side —
five distinct fresh transactions, all with `startTS 4116`, refused by finished heads at
4118, 4119, 4127, 4128 and 4132, none aborted, on five different nodes:

```
PROBE node 4749 head 4118 startTS 4116 txID ...9933 aborted false
PROBE node 2541 head 4127 startTS 4116 txID ...9946 aborted false
PROBE node  928 head 4119 startTS 4116 txID ...9934 aborted false
PROBE node 1493 head 4128 startTS 4116 txID ...9947 aborted false
PROBE node 1711 head 4132 startTS 4116 txID ...9956 aborted false
```

Because retries take a fresh snapshot that is pinned to the same instant, they cannot
converge: that is rmp #2368's livelock, which is this defect seen from the write side.
rmp #2354 did not cause it — before #2354 a re-assert of an already-present label
skipped the head test, which masked it on that path.

`graph/mvcc/ratchet_test.go` already documents the same mechanism for the recovery case,
in its own words: a log that still believes an early timestamp is unfinished *"computes a
frontier of 0 for ever … every commit after recovery allocated a timestamp, set its bit,
failed to advance `oldest`, and stayed INVISIBLE for the life of the process. Writes kept
succeeding; readers never saw them."* Sprint 335 measured the frontier's tail cost and
accepted it (*"the contiguous frontier costs p50 nothing and p99 one fsync"*); what was
not established is that under sustained multi-writer load the tail becomes **permanent**.

## How the reference engines avoid it

Every reference uses a **richer snapshot than a scalar**, and none can exhibit this.

| Engine | Snapshot | Consequence |
|---|---|---|
| PostgreSQL | `xmin` **plus the list of in-progress XIDs** (`GetSnapshotData`, `src/backend/storage/ipc/procarray.c`) | A commit above `xmin` is visible unless that specific xact is in flight. One old transaction never hides newer commits. |
| InnoDB | Read view: a low limit **plus the set of active trx ids** | Same shape as PostgreSQL. |
| Memgraph | Start timestamp loaded under `engine_lock_` together with the last committed timestamp (`src/storage/v2/inmemory/storage.cpp`) | A new view is consistent with the newest **commit**, not with the oldest unfinished one. |

GoGraph excludes a **contiguous prefix**; the references exclude only the **specific
in-flight set**. That difference is the defect, and it is not a tuning constant.

## Options for the fix — architecture change, needs sign-off

1. **Snapshot = (frontier, in-flight set)** — the PostgreSQL/InnoDB shape. `mvcc.Visible`
   excludes only the in-flight set. Correct and reference-aligned. Costs a set read per
   visibility test, on the read path where this module is currently strongest, so the
   read benchmarks must be measured rather than assumed.
2. **Start timestamp from the last committed instant under the publishing lock** —
   Memgraph's shape. Simpler, but reintroduces a commit-path lock that rmp #2302/#2193
   deliberately removed.
3. **Keep the scalar frontier and bound its lag** — e.g. a committer waits for
   contiguity. Preserves the read path, reinstates the convoy the frontier work removed,
   and converts a visibility defect into a latency one.

## Discipline for whoever picks this up

Seven hypotheses were refuted in this area before the root cause held: a missing index,
per-node churn, unswept delta chains, a mis-calibrated planner crossover, aborted heads
as a self-perpetuating obstacle, the publication window alone, and a stale-read probe
that passed vacuously. **Instrument before proposing.** The regression test must assert
the reference behaviour — `ReadTS() >= b` immediately after `PublishCommitTS(b)` — which
fails today, so it lands with the fix; committing it against the present behaviour would
encode the defect as correct.
