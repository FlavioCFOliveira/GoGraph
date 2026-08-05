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
writers": the same shape now measures 2.183× at 16. The attribution below narrows
"inside the shared MVCC structures" to a specific one.

## Attribution (profiled 2026-08-05, same host)

The ceiling is now attributed, and the answer implicates a change made earlier in this
same sprint.

```
go test -run '^$' -bench 'BenchmarkWriteScaling/mem/writers=32' -benchtime=3s \
    -mutexprofile=mu.out ./bench/mvccwrite/
go tool pprof -top -unit=ms mu.out ; go tool pprof -traces mu.out
```

**96.9% of all mutex delay flows through `cypher.(*Engine).RunInTx`**, and the contended
stack tops out in `graph/lpg.(*Graph).setNodeLabelInfo`, reached on every commit via
`WriteView.SetNodeLabel` → `lpgMutatorAdapter.SetNodeLabel` → `exec.(*CreateNode).Next`.
By lock kind: `sync.(*Mutex).Unlock` 48.05% flat, `sync.(*RWMutex).Unlock` 36.08%,
`RWMutex.RUnlock` 13.58%.

The benchmark's unit is `CREATE (n:Account {id: $id})`, so **every** commit writes a
label and every writer reaches this path. That is why the store-less arm plateaus while
the WAL arm scales: the WAL arm's 3.7 ms fsync dominates and leaves the label write in
the noise, whereas at 2.9 µs per commit the label write *is* the commit.

**The mechanism, and it is a lock-nesting one.** `setNodeLabelInfo` takes the per-shard
label lock `sh.mu` — sharded, so writers on different shards should not contend — and
since commit `2284c0e8` it performs `nodeIdx.Add` / `deferLabelIndexRemoval` **inside**
that lock. `nodeIdx` is a *single* `label.Index` with its own lock, shared by every
shard. So a per-shard lock now encloses a global one, and shard-parallel writes are
funnelled through one serialisation point.

`2284c0e8` widened that critical section deliberately and for a real reason: the bag and
the index have to transition together, and the add-path gap was losing rows outright —
the unrecoverable direction. So this is a **correctness-versus-scaling trade that was
made knowingly**, not an accident. But its scaling cost was not measured at the time,
and it should have been. Correct still outranks fast, so nothing here argues for
reverting it; it argues for a design that gets both.

**Two things this does NOT say**, to keep the finding honest:

- it does not quantify how much of the 2.2× ceiling `2284c0e8` is responsible for. The
  before/after comparison run for #2315 post-dates that commit, so it cannot separate
  them. Measuring that needs the ceiling re-measured against the pre-`2284c0e8` ordering
  — which is *unsound* code, so it is a measurement-only arm that must never be shipped;
- it does not establish that removing this nesting would lift the ceiling to any
  particular figure. The next-largest contention source is unknown until this one moves.

The candidate list below was written before the profile. **The first candidate was
right** and the other three are unevidenced; they are kept only to show what was
considered.

## The pre-profile candidate list, kept for the record

Written before the profile, when the ceiling was measured but unattributed. The **first
candidate was right**; the other three were never evidenced and remain hypotheses. Kept
only to show what was considered, and as a reminder that the list was not the finding:

- the label/property shard maps and their `sync.RWMutex` per shard;
- `mvcc.Clock`'s commit-log frontier, which every committer advances;
- `mvcc.Horizon`'s 64 slots;
- the node-id mapper and the adjacency structure's own locking.

The profile has since been run, so the list is history rather than guidance. Its lesson
holds: two previously-blamed mechanisms (the semaphore, the WAL) were cleared by the
numbers, and the answer turned out to be a lock NESTING introduced for correctness in
this same sprint — which no amount of reasoning about the candidate list would have
found.
