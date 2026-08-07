# WAL group commit reaches the engine write path — 2026-08-05

rmp #2193. Host: Apple M4, 10 cores, `darwin/arm64`. Harness:
`BenchmarkWriteScaling` in `bench/mvccwrite/scaling_test.go`, one autocommit
`CREATE (n:Account {id: $id})` per commit, disjoint id space per writer.

## What changed, and what did not

No coalescing machinery was written for this task. `wal.Writer.SyncGroup` has been
leader/follower since it was introduced, and `store/txn` has carried the
apply-ordering gate since #1507. What blocked it was **structural**: the engine
called `Tx.CommitWALOnly` from inside the *exclusive* visibility barrier, so a
second committer was never in flight for a leader to coalesce with. The mechanism
was present and unreachable.

Retiring the single-writer semaphore (#2296, `7025227b`) and moving version
ownership onto the transaction (`240231e3`) left the commit finalisation running
under a **shared** hold instead — see `cypher.ExplicitTx.Commit`, which takes
`ApplyInVersionedTx` rather than the exclusive barrier. That is what made the
coalescing reachable. This task's contribution is therefore the **measurement and
the gates**, not the mechanism.

## Throughput against writer count (AC 3)

`go test -run '^$' -bench BenchmarkWriteScaling -benchmem -count=5`, WAL arm,
summarised with `benchstat -col /writers`:

| writers | 1 | 2 | 4 | 8 | 16 | 32 |
|---|---|---|---|---|---|---|
| commits/s | 270.7 | 331.7 | 548.7 | 1083 | 2113 | 4050 |
| scaling | 1.000× | 1.225× | 2.027× | 3.999× | 7.807× | 14.96× |
| p vs 1 writer | — | 0.008 | 0.008 | 0.008 | 0.008 | 0.008 |

n=5 per point, every comparison significant at p=0.008. `benchstat` reports `± ∞`
because a 0.95 confidence interval needs n≥6; the p-values are unaffected, and the
acceptance criterion asks for n≥5. Re-run at `-count=6` or higher if an interval is
wanted.

**The single-writer rate is unchanged at ~270 commits/s** — an fsync per commit on
this disk — and that is the point: the gain is entirely in coalescing, not in making
any individual commit faster.

This supersedes the earlier sprint-334 finding that "WAL writes are flat at 268
commits/s at any writer count". The 268 figure was correct for the code as it then
stood; it is now correct only for the one-writer case.

Interpreting the two arms together: `benchstat` must be given the arms separately.
The sub-benchmark name embeds the wiring positionally
(`BenchmarkWriteScaling/wal/writers=8`), so pooling `mem` with `wal` collapses them
into one row, reports `benchmarks vary in .fullname`, and inflates the variance to
±103%. Split the file by arm before summarising.

## fsyncs per commit (AC 1)

Counted from `wal.Writer.Stats().Syncs`, not inferred from timing. Gate:
`TestWALGroupCommit_FsyncsPerCommitFall` in `bench/mvccwrite/groupcommit_test.go`.

| writers | commits | fsyncs | fsyncs/commit |
|---|---|---|---|
| 1 | 240 | 240 | 1.000 |
| 8 | 240 | 59–61 | 0.246–0.254 |

A factor of 3.9×–4.1× fewer fsyncs per commit at 8 writers. The gate's floor is
1.60×, set far below the observed factor because its job is to catch coalescing
being lost *entirely*, not to police its efficiency.

**The gate was validated against the defect it exists to catch.** Wrapping every
engine write in one external global mutex returns the ratio to exactly
1.000/1.000 = 1.00× and the gate fails with the diagnosis. That matters here
specifically: the sibling engine write-scaling gates were previously found
*insensitive* — an injected global mutex passed them — so a scaling gate on this
path cannot be assumed to bite. This one does.

The gate skips under coverage instrumentation
(`testlayers.RequireUninstrumented`): lengthening and uniformising every basic
block changes whether a second committer arrives before the leader enters its
syscall, which removes the overlap rather than the mechanism and reports a false
regression.

## Durability ordering (AC 2, 5)

Unchanged by this work and already covered: `cypher/durable_then_visible_test.go`
(`TestRunInTx_DurableThenVisible_RecoversWithoutClose`,
`..._ConcurrentReader`). The ordering in `cypher.ExplicitTx.Commit` is fsync →
release → publish, so a transaction's commit timestamp is published only after its
own WAL flush returned; visibility is gated on `PublishCommitTS`, never on the
barrier, which is why the shared hold does not weaken durable-before-visible.

Concurrent committers under crash injection (AC 4):
`internal/crashinject/concurrent_writers_test.go`.
