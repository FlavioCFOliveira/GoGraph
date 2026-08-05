# The store-less write-scaling ceiling is ~2.2× — 2026-08-05

rmp #2323 (SPIKE), measurement half. Host: Apple M4, 10 cores, `darwin/arm64`.
Harness: `BenchmarkWriteScaling` in `bench/mvccwrite/scaling_test.go`, `mem`
wiring (`cypher.NewEngine` over a bare `lpg.Graph`, no WAL, no store), one
autocommit `CREATE (n:Account {id: $id})` per commit, disjoint id space per writer.
`-count=5`, summarised with `benchstat -col /writers`.

## The measurement

| writers | 1 | 2 | 4 | 8 | 16 | 32 |
|---|---|---|---|---|---|---|
| commits/s | 339.7k | 521.3k | 654.1k | 701.2k | 741.2k | 751.8k |
| scaling | 1.000× | 1.535× | 1.927× | 2.065× | 2.183× | 2.214× |
| p vs 1 writer | — | 0.008 | 0.008 | 0.008 | 0.008 | 0.008 |

Every comparison is significant at p=0.008, n=5. (`benchstat` prints `± ∞` because a
0.95 interval needs n≥6; the p-values are unaffected.)

**The ceiling is ~750k commits/s and it is reached by 8 writers.** The marginal
return collapses: 4→8 adds 7%, 8→16 adds 6%, 16→32 adds 1.4%. Doubling from 16 to 32
writers buys 1.4% throughput, so the plateau is real and not merely a slower slope.

## The inversion, which is the finding

The WAL-backed arm of the same benchmark, measured in the same run
(`mvcc-group-commit-2026-08-05.md`), reaches **14.96×** at 32 writers. The
**store-less path scales 6.8× worse than the durable one.**

That is not a paradox once the per-commit budget is compared. A WAL commit costs
~3.7 ms at one writer, almost all of it fsync, so there is enormous slack for
overlap — and `wal.Writer.SyncGroup`'s leader/follower round converts that slack
into throughput almost linearly. A store-less commit costs ~2.9 µs, three orders of
magnitude less, so there is no slack: the cost is nearly all shared-structure work,
and contention on it is the binding constraint.

**Conclusion for the sprint's macro objective.** MVCC's write-scaling dividend on the
in-memory path is capped at about 2.2×, and the cap is NOT the WAL, NOT the retired
single-writer semaphore, and NOT the commit path — all three have been removed or
shown reachable, and the durable arm's 14.96× proves the commit path itself
parallelises. Whatever remains is inside the shared MVCC structures the writers
touch in common. This supersedes the earlier sprint-334 figure of "0.83× at 16
writers": the same shape now measures 2.183× at 16.

## What this spike has NOT yet established

The ceiling is measured; it is **not yet attributed**. The number above says where
scaling stops, not which structure stops it. Attribution requires a CPU profile and a
contention profile of the 8- and 32-writer arms — `go test -bench` with
`-cpuprofile`/`-blockprofile`/`-mutexprofile`, read with `go tool pprof` — to name the
contended structures rather than infer them. Candidates worth testing, in the order the
per-commit path touches them, but each is a hypothesis and none is evidence yet:

- the label/property shard maps and their `sync.RWMutex` per shard;
- `mvcc.Clock`'s commit-log frontier, which every committer advances;
- `mvcc.Horizon`'s 64 slots;
- the node-id mapper and the adjacency structure's own locking.

Do not act on that list without the profile. The pattern across this project is that
the stated bottleneck is usually not the measured one, and the one certainty here is
that two previously-blamed mechanisms (the semaphore, the WAL) are both cleared by the
numbers above.
