# The session read-your-own-writes guarantee — the measured cost

rmp #2329 (MVCC), acceptance criterion 3.
Machine: darwin/arm64, Apple M4, 10 cores, quiet. `-benchmem -count=10`.

## What was measured, and why on the read side

The guarantee is implemented as a WAIT: before a session's next operation takes its
snapshot, it waits for the visible frontier to reach the instant of that session's
last commit. Putting the wait on the committer instead was evaluated and rejected in
rmp #2328, because it makes every commit wait on every earlier in-flight commit —
the convoy rmp #2302 and rmp #2193 removed.

So the cost lands on reads, and the shape that pays it is a read that FOLLOWS this
session's own write.

## The numbers

`cypher/session_bench_test.go`, 500-node fixture, mean of 10:

| benchmark | sec/op |
|---|---:|
| ReadAfterWrite_Sessionless | 32.13 µs ± 1% |
| **ReadAfterWrite_Session** | **32.06 µs ± 1%** |
| ReadOnly_Sessionless | 17.81 µs ± 1% |
| **ReadOnly_Session** | **17.76 µs ± 2%** |

**No measurable cost in either shape** — both differences are inside the run-to-run
spread and neither favours the sessionless arm.

## Why it is free here, and when it would not be

`mvcc.Clock.AwaitVisible` returns immediately when the contiguous visible frontier
has already passed the session's floor, which on a lightly loaded engine it has: the
commit published before the call returned. The wait then costs one atomic load, which
is what the read-only arm measures.

The wait becomes real exactly when the frontier is held back — when an EARLIER
in-flight commit has not published yet. That is the condition the guarantee exists
for, and it is the condition under which the sessionless path is WRONG rather than
merely faster: it would return a snapshot that does not include the caller's own
write.

So the honest statement is not "the guarantee is free" but "the guarantee costs
nothing when it is not needed, and when it is needed the alternative is a wrong
answer". A workload that genuinely pays should be visible as a non-zero
`lpg.mvcc.sessions.waiting`, read together with `InFlightCommits` — the frontier is
held back by exactly those commits, so the two together say who is waiting and why.

## What still pays nothing

`Engine.Run`, `Engine.RunInTx`, `Engine.BeginTx` and `Engine.BeginReadTx` are
unchanged and keep the sessionless contract, so an unrelated reader — the common case
on a read-mostly workload — takes no wait at all. That was the point of the API
shape: a caller that needs the guarantee asks for it by name.
