# An acked commit lost in the post-fsync-pre-publish window — rmp #2349

**Date:** 2026-08-07
**Branch:** `sprint-335`
**Status:** ACID Durability defect **FIXED**, reproduced deterministically, cost measured at zero

## The defect, in one sentence

A checkpoint that landed between a transaction's WAL fsync and its MVCC publish
truncated away the only durable record of that transaction — a client was told
SUCCESS and the write was gone after recovery.

## How it was found

Not by a failing assertion, but by [rmp #2347](st3-empty-report-2026-08-07.md)'s
load-based search: `internal/sim`'s `TestST3_CheckpointTeardown_Scenario` at seed
`0xBADF00D`, under `-covermode=atomic -coverpkg=./...` with four fsync-heavy durable
packages looped in parallel, lost **exactly one acknowledged commit in 2 of 15
iterations** (`acked=142 recovered=141`, `acked=94 recovered=93`) and **0 of 17**
without the parallel load.

## Mechanism

`store/checkpoint`'s non-blocking checkpoint takes two readings in phase 1, under the
store's commit serialisation:

| Reading | What it is | Used for |
|---|---|---|
| `watermark = wlog.DurableOffset()` | a **DURABILITY** position | the WAL prefix phase 3 truncates |
| `at = g.BeginRead()` | a **VISIBILITY** position | the instant the graph image is read at |

They are not the same clock. The dangerous direction is a transaction that is durable
below the watermark but whose commit instant is not yet published: `at` cannot see it,
the image does not carry it, and phase 3 discards its frame.

The file argued that window could not exist, and named its reason:

> It cannot happen because the commit serialiser CLOSES ADMISSION AND DRAINS the
> admitted writers to zero before running this closure, and a writer's registration
> spans its WHOLE commit — `store/txn.Tx.Commit` defers `exitWriter` past
> `ApplyVersioned`, which is what publishes the instant.

**That premise was checked on the wrong path.** The Cypher engine — the only
production writer — does not use `Tx.Commit`. It uses `Tx.CommitWALOnly`
(`cypher/api.go` `commitUnderBarrier` → `store/txn/txn.go:1366`), which performs no
in-memory apply and publishes no instant; its `defer t.store.exitWriter()` fires the
moment the fsync returns, while the instant is published later, when the lpg write
bracket unwinds through `lpg.Graph.endWrite`. `cypher/api.go` states the resulting
state outright — *"DURABLE, BUT NOT YET VISIBLE"*.

So between `CommitWALOnly` returning and `endWrite` publishing, the store's in-flight
writer count is **zero** while the frame is already inside the durable offset. The
drain completed, both readings were taken inside the window, and phase 3 truncated.

```
engine writer:   AllocateCommitTS ── append ── fsync ──┤ exitWriter ├───── endWrite (publish)
                                                       └──── THE WINDOW ────┘
checkpointer:                          close admission ─ drain (sees 0) ─ watermark ─ at ─ … truncate
```

## Why the existing boundary test could not see it

`TestCheckpoint_WatermarkAndInstantDescribeTheSameBoundary` asserts exactly this
invariant, samples it on every checkpoint, and is careful to prove its oracle
non-vacuous. But **every one of its writers commits through
`st.Begin()`/`tx.Commit()`** — the one path on which the invariant genuinely held. It
passed throughout, and its passing was read for a sprint as coverage it did not have.
Its doc comment now says so explicitly.

## The fix: wait the window out, on the observer

Phase 1 now calls `lpg.Graph.AwaitCommitQuiescence` at the top of its locked closure,
before either reading. It blocks until every allocated commit instant has been
published or abandoned — until `MVCCStats.InFlightCommits` reads zero.

It terminates because admission is **already closed and the admitted writers
drained**, so no new instant can be allocated while it waits, and every outstanding
one is past its fsync with only in-memory work left. After it returns, every durable
frame belongs to a transaction visible at `at` — by construction, not by assumption.
A `commitQuiesceTimeout` of 30 s bounds it as a **fail-stop**: reaching it means a
timestamp was allocated and never discharged (a permanent frontier stall that has
already broken every new reader), and the checkpoint then fails without truncating.

### Prior art — the two references disagree, and the disagreement is the argument

**PostgreSQL** (commit `b5978350`) has the identical problem and names it:

> …the whole reason we have this issue is that xact.c does commit record XLOG
> insertion and clog update as two separate steps protected by different locks, but
> again that seems best on grounds of minimizing lock contention.
> — `src/backend/access/transam/xlog.c:7684-7687`

A backend raises `DELAY_CHKPT_IN_COMMIT` before inserting its commit record and clears
it after `TransactionIdCommitTree` (`src/backend/access/transam/xact.c:1469-1471`,
`:1577-1582`), and `CreateCheckPoint` **spins until that set is empty**
(`xlog.c:7695-7712`) before proceeding. Its stated trade-off is the one taken here:

> it seems better to make checkpoint take a bit longer than to hold off insertions
> longer than necessary. — `xlog.c:7683-7684`

**Memgraph** (commit `b3ac3cdc`) reaches the same property the other way.
`InMemoryStorage::CreateTransaction` loads the start timestamp and the last durable
timestamp under **one** acquisition of `engine_lock_`
(`src/storage/v2/inmemory/storage.cpp:2833-2844`), so a snapshot taken from that
transaction writes a mutually consistent pair by construction
(`storage.cpp:4257-4269`) with no wait at all.

Memgraph's route was **not** adopted, and the reason is specific to this project: it
works because its commit publishes durability and visibility under the same engine
lock — which is precisely the convoy rmp #2302 and rmp #2193 removed here to make
writes scale, and which `graph/mvcc/await.go` already records as a deliberate
divergence. GoGraph has PostgreSQL's decoupling, so it needs PostgreSQL's remedy.

## Evidence

### Reproduction — deterministic, 3/3

`store/checkpoint/engine_commit_boundary_test.go`
(`TestCheckpoint_EngineCommitOrdering_KeepsAnAckedCommit`) drives the engine's commit
shape by hand — transaction opened outside the bracket, mutation applied eagerly
inside it, `CommitWALOnly` inside it — and **parks** the writer in the window while a
checkpoint runs. On the unfixed code, 3 runs out of 3:

```
INVARIANT VIOLATED: 1 commit(s) were durable but unpublished when phase 1 took the
watermark and the instant.
ACID DURABILITY VIOLATED: the edge "late-a"->"late-b" was acknowledged (CommitWALOnly
returned nil) and is ABSENT after recovery from the checkpointed artefact.
Order=16 Size=8, endpoints interned: src=false dst=false
```

`Order=16 Size=8` is exactly the eight prior transactions. The ninth is gone, and its
endpoints are not even interned.

After the fix: **5 runs out of 5 pass**, `Order=18 Size=9`, and the run reports which
ordering it exercised — *"parked writer released by: phase 1 blocked on the commit
frontier"*. The interleaving is forced by two signals, neither a timeout, so the
passing path costs nothing and the failing path is not a race.

**Instrument validated on a defective build.** With the wait disabled and everything
else intact, the new test fails 2/2 with both oracles firing, while
`TestCheckpoint_WatermarkAndInstantDescribeTheSameBoundary` stays green — the direct
demonstration that the old test could not observe this defect.

### The load-based search that found it, re-run

The same search, same method, same 15 iterations, same parallel fsync load, on the
fixed code:

| Arm | Iterations | Failures |
|---|---:|---:|
| Before the fix | 15 | **2** |
| After the fix | 15 | **0** |

Duration 179 s. Teardown verified: 4 load PIDs recorded, 0 still alive.

**0/15 is not proof of absence** — the pre-fix rate was only 2/15, so this arm alone
could not distinguish a fix from luck. It is corroboration; the deterministic test
above is the evidence.

### Cost — measured, and it is zero

The fix adds work inside a window every writer is excluded from, so it had to be
measured rather than argued about. `BenchmarkWriteScaling` cannot measure it: neither
of its wirings runs a checkpointer. `bench/mvccwrite/checkpoint_under_writers_test.go`
is the instrument that can — WAL-backed engine writers with a checkpointer firing
back to back for the whole timed arm.

Two test binaries were built from the same tree, one with the wait and one with it
disabled, and run **alternately** so host drift cancels. Apple M4 (10 cores),
darwin/arm64, `-benchtime=1s`, 7 interleaved pairs, quiet machine.

| Arm | commits/s without the wait | commits/s with the wait | verdict |
|---|---:|---:|---|
| `cp=off/writers=1` | 270.6 ± 1% | 271.1 ± 1% | ~ (p=0.620, n=7) |
| `cp=off/writers=4` | 537.5 ± 2% | 537.6 ± 2% | ~ (p=0.402, n=7) |
| `cp=off/writers=16` | 2 104 ± 1% | 2 102 ± 3% | ~ (p=0.902, n=7) |
| `cp=on/writers=1` | 161.8 ± 5% | 164.3 ± 3% | ~ (p=0.600, n=7) |
| `cp=on/writers=4` | 323.2 ± 2% | 325.7 ± 4% | ~ (p=0.710, n=7) |
| `cp=on/writers=16` | 1 145 ± 6% | 1 150 ± 5% | ~ (p=0.558, n=7) |
| **geomean** | 513.5 | 515.9 | **+0.48%** |

No arm differs significantly, and the checkpoint COUNT per arm is unchanged (15–17),
so the checkpointer is not being slowed either.

**Why the instrument is not vacuous.** The same benchmark resolves the checkpointer's
own cost clearly: 270.6 → 161.8 commits/s at one writer, a **−40%** effect. An
instrument that measures a 40% effect on the very quantity in question would show a
material lengthening of the phase-1 window if there were one.

**Why this is the expected result.** The diff adds no instruction to the commit path:
it is confined to `store/checkpoint/checkpoint.go` plus two new methods
(`mvcc.Clock.AwaitQuiescent`, `lpg.Graph.AwaitCommitQuiescence`) that no writer calls.
The wait is on the observer, exactly as PostgreSQL arranges it.

## Gates

`make ci` green — `go vet`, `go test -race ./...`, `golangci-lint`, the openCypher TCK
execution gate at its full baseline, and the coverage gate.

## What this leaves open

The invariant is now enforced for **every** commit shape that allocates an instant
before its fsync, because the wait is expressed against the clock rather than against
any one commit path. That is the property `AwaitCommitQuiescence` was given to have:
a future commit path that publishes late is covered without anyone remembering to
extend a drain.
