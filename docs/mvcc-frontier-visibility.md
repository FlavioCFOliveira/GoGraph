# The contiguous frontier and cross-statement visibility (rmp #2369 RETRACTED, #2368 open)

**Status: RETRACTED as a defect, and `Session`'s guarantee is now MEASURED as delivered.**
The mechanism below is real and correctly described; the "defect" framing was wrong and
is corrected here.

The mechanism is also pinned deterministically, on a bare `mvcc.Clock` with no
goroutines and no timing, by `graph/mvcc.TestClock_FrontierStallsBehindOneInFlightCommit`:
two finished, acknowledged commits stay invisible while one older commit is in flight,
and the frontier jumps past all three the moment it finishes. That test carries a
control requiring the frontier to advance immediately when nothing is in flight, so it
cannot pass on a clock that simply never advances.

`cypher/session.go` states the observed behaviour as the INTENDED contract, verbatim:
*"Engine gives every statement SNAPSHOT ISOLATION ... It promises nothing ACROSS
statements. A caller that commits and then reads may take its snapshot at an instant
the commit has not reached, because a commit becomes visible when the CONTIGUOUS
frontier advances past it, and an earlier in-flight commit can hold that frontier back.
That is correct snapshot isolation and it is what a database gives an unrelated
reader."*

The reproduction below used **bare `eng.RunInTx`**, which is exactly the case that
promises nothing across statements. It therefore measured the documented contract, not
a violation of it. `Session` (rmp #2328/#2329) is the read-your-own-writes surface, and
it uses a floor plus a **wait** rather than an assignment — deliberately, because
*"the snapshot is still taken at a contiguous frontier point, so it can never observe a
state no serial order produced. A snapshot pinned ABOVE the frontier could."*

**`Session` DOES deliver its guarantee under load — ESTABLISHED 2026-08-26 (rmp #2369).**
The first attempt to test it was broken (the count was extracted positionally through a
type assertion that never matched, so every read yielded 0) and was discarded. The
re-test reads the count by column name and runs 16 concurrent writers:

| Surface | Acknowledged commits | Concurrent background commits | Stale reads |
|---|---:|---:|---:|
| `Session` | 4 000 (x4 runs = 16 000) | ~100 000 per run | **0** |
| bare `Engine` | 4 000 | ~100 000 | **1–4** |

The bare arm is the control that makes the Session result mean something: under the
IDENTICAL load the documented Engine contract does go stale, at 1–4 per 4 000 rounds —
about 0.05%, corroborating the 2-of-400 the original probe recorded. So the load is
demonstrably capable of exposing the failure, and `Session` prevented it.

The subject arm runs 4 000 rounds rather than the 400 the task asked for, because at
that rate 400 rounds would expect ~0.2 stale reads from a BROKEN Session and would
usually miss it — roughly 20% power. 4 000 raises the expected count to ~2.

What remains open is ONE question, and it is about API ergonomics rather than
correctness: whether the DEFAULT surface should give read-your-own-writes without the
caller opting in, since every reference engine does so on an ordinary connection. The
livelock in rmp #2368 is unaffected by this retraction: a write that cannot progress
after 64 fresh attempts is not explained by any isolation contract.

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

## What that means under load (measured on the BARE-ENGINE surface)

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

## How the reference engines compare

Every reference uses a **richer snapshot than a scalar**. Corrected reading: this makes
their DEFAULT stronger than GoGraph's default, not GoGraph incorrect — GoGraph offers
the same guarantee through `Session`.

| Engine | Snapshot | Consequence |
|---|---|---|
| PostgreSQL | `xmin` **plus the list of in-progress XIDs** (`GetSnapshotData`, `src/backend/storage/ipc/procarray.c`) | A commit above `xmin` is visible unless that specific xact is in flight. One old transaction never hides newer commits. |
| InnoDB | Read view: a low limit **plus the set of active trx ids** | Same shape as PostgreSQL. |
| Memgraph | Start timestamp loaded under `engine_lock_` together with the last committed timestamp (`src/storage/v2/inmemory/storage.cpp`) | A new view is consistent with the newest **commit**, not with the oldest unfinished one. |

GoGraph excludes a **contiguous prefix**; the references exclude only the **specific
in-flight set**. That is a real difference in DEFAULT behaviour and worth a decision,
but it is not a correctness defect: GoGraph provides read-your-own-writes through
`Session`, and its snapshot is deliberately never pinned above the frontier, so it
cannot observe a state no serial order produced.

## Options IF the default is to change — architecture change, needs sign-off

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
that passed vacuously, and finally the "defect" framing of this very document, which
the module's own `session.go` header refutes. A ninth: the attempt to test `Session`
hand-rolled a count extraction with a type assertion instead of using `countQ`, so
every round read 0 and appeared to fail. **Instrument before proposing, and read the
contract before calling anything a defect.** The regression test must assert
the reference behaviour — `ReadTS() >= b` immediately after `PublishCommitTS(b)` — which
fails today, so it lands with the fix; committing it against the present behaviour would
encode the defect as correct.
