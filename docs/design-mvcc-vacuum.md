# Design: the MVCC background vacuum

**Task rmp #2308 (MVCC C2)** · sprint 334 · audit finding E11 / §6.4 of
[`audit-mvcc-sole-cc-2026-08-02.md`](audit-mvcc-sole-cc-2026-08-02.md) · 2026-08-04

Status: **implemented.** `graph/lpg/mvcc_vacuum.go`, `graph/lpg/mvcc_gc.go`.

---

## 1. The problem

Reclamation ran **on the committer**. `Graph.endWrite` allocated the commit
timestamp, published it, and then swept the version chains while the visibility
barrier was still held. Two consequences, and only the first is a cost:

1. Every committer paid for other transactions' garbage. The sweep is
   O(objects carrying history) across seven stores, and it landed on whichever
   transaction happened to push the reclamation debt over its threshold.
2. **The sweep had no exclusion left to stand on.** `ReclaimNow`'s contract
   demanded "the visibility barrier in write mode, or otherwise exclude
   concurrent writers", and rmp #2307 removed the barrier from the write path.

The second is the reason this had to change rather than an optimisation.

## 2. What the reclaimers actually need

The audit's demand was to verify by reading every reclaimer body, not its doc
comment. That verification found that **six of the seven reclaimers take the same
per-shard write lock the write path takes**, so they are mutually excluded from a
concurrent writer shard by shard and never needed the barrier:

| Unit | Lock it takes | Same lock the writer takes? |
|---|---|---|
| node labels | `nodeLabelShards[i].mu` | yes — `setNodeLabelInfo` |
| node properties | `nodePropShards[i].mu` | yes — `setNodePropertyInfo` |
| the five per-edge side stores | each store's `shards[i].mu` | yes |
| adjacency | `adjlist` `shards[i].mu` | yes — `storeEntry`, from every call site |
| node existence | `nodeLifeShards[i].mu` | yes |
| adjacency conflict stamps | `adjVersions.shards[i].mu` | yes |
| **deferred label-index removals** | `idxDeferred.mu` | **NO — see §3** |

`adjlist.AdjList.Reclaim`'s doc said "not safe to run concurrently with … writers
on the same AdjList". That was pessimistic rather than true, and it is corrected
there.

## 3. The one real defect the audit found

`applyDeferredIndexRemovals` collected the ready entries under `idxDeferred.mu`,
**released the lock**, and only then removed them from the label bitmap:

```
T1 removes label L from n, commits at 10; the removal is deferred, stamped 10
the watermark reaches 10
the sweep collects {L,n} as ready, deletes it from pending, RELEASES the lock
T2 adds L back to n:  nodeIdx.Add(L,n) succeeds, then its cancel finds nothing
                      pending to withdraw — the sweep already took it
the sweep calls nodeIdx.Remove(L,n)
n carries L and is NOT in L's bitmap. Every later MATCH (n:L) misses it.
```

That is the one failure direction the candidate-set discipline says nothing can
recover from: the bitmap may over-report freely, but a lost member is a silently
lost row, forever. It could not surface while the sweep ran under the barrier,
which excluded T2 by construction.

**Fix, in two halves that only work together:** the bitmap removals now happen
while `idxDeferred.mu` is still held, and `setNodeLabelInfo` cancels **before** it
adds. Both interleavings then leave the bitmap a superset of the truth — a writer
that cancels first removes the key so the sweep never collects it; a writer that
arrives second blocks on the lock and re-adds after the sweep has removed it. The
order also fixes the lock order (`idxDeferred.mu` then the index's own lock on
both paths).

Reproduced by `TestDeferredIndexRemoval_ConcurrentReaddIsNotLost`, which fails at
round 2 under `-race` against the old code and round 908 without it.

## 4. Shape

A **demand-started, self-terminating** goroutine, one per graph at most.

- **Wake sources.** A commit whose debt crosses `reclaimThreshold` (4096) — the
  churn case. A **reader** leaving the horizon while versions are retained — the
  drain case, which no commit signals, because the watermark advances when the
  oldest reader goes away and not when anything is written.
- **Not on a writer's release.** A writer's departure advances the watermark too,
  but its versions were already charged to the debt, so a wake there would only
  make the same sweep run *sooner* — once per commit instead of once per 4096
  versions, discarding the amortisation the debt counter exists for.
- **The drain wake is precise, not throttled.** It fires only when the watermark
  has advanced past what the last pass used (`vacuumState.lastWatermark`). A
  1-in-64 tick was tried first and dropped the release that matters: a workload
  with one long-lived reader takes exactly one release, and a 63-in-64 chance of
  skipping it left 16 385 records retained with no wake pending.
- **Exit.** After two consecutive passes that free nothing. A permanent goroutine
  per graph would be a leak by the module's own measure — `goleak.VerifyTestMain`
  guards `graph/lpg` — and a caller of `lpg.New` is not required to close.
- **Explicit end.** `Graph.Close` / `Graph.CloseCtx`, and `cypher.Engine.Close` /
  `CloseCtx` for the layer that holds the only reference to a graph.
- **Bounded pass.** `vacuumRecordsPerPass` = 65 536 records, checked *between*
  units (never inside one, which holds a shard lock). A unit cursor makes a
  stopped pass resume where it left off, so no store is starved.
- **Backoff.** No wait at all while a pass frees something; 1 ms doubling to
  100 ms once it does not.

### One subtlety that cost a leak

The loop must **drain** the wake channel at the start of each pass. It did not at
first, so the exit path's "was a wake lost while I was unwinding?" test was
permanently true and the vacuum restarted itself forever — 24 starts for 16 000
writes, one goroutine always alive, which `goleak` correctly reported.

## 5. Two bounds, because the sweep is asynchronous

While the committer swept before returning, the churn bound was true at every
instant. A background sweeper can be outrun, so the module now states both numbers
on `MVCCStats`:

| Field | Value | Meaning |
|---|---|---|
| `Bound` | `reclaimThreshold` = 4096 | The **settled** bound. Churn returns to it once writing stops. `WithinBound()`. |
| `Ceiling` | `reclaimDebtCeiling` = 16384 | The **instantaneous** bound. At it a committer stops signalling and waits for one pass. `WithinCeiling()`. |

The ceiling is enforced by `Graph.awaitVacuumProgress`, which waits for a **pass**
and never for the watermark — bounded at ~6.4 ms — so a long-lived reader that
legitimately pins every version delays a committer by one pass and no more. That
is backpressure as the reliability mandate prescribes it, and it is why "the sweep
left the commit path" did not become "memory is unbounded": measured peaks over
24 576 modifications were 9 232 (transactional) and 14 589 (direct API) before the
ceiling existed, and 8 480 after.

## 6. Prior art, read in source

- **Memgraph** (memgraph/memgraph @ `0e8aa326`, `src/storage/v2/inmemory/storage.cpp`).
  `gc_runner_.Run("Storage GC", …)` on `config_.gc.interval`, stopped in
  `~InMemoryStorage` (:609-628). Three decisions adopted: one run at a time via
  `std::try_to_lock` on `gc_lock_`, abandoned rather than queued (:2966-2971); the
  GC takes `main_lock_` **shared**, with the reason stated in the source — an
  aggressive sweep escalates to unique, "otherwise a shared hold, so slow GC does
  not block everyone"; and the sweep is observable (`gc_latency_seconds`,
  `gc_progress_` for `SHOW TRANSACTIONS`). Its `garbage_undo_buffers_` is also
  where **aborted** transactions' deltas go, freed once
  `mark_timestamp_ <= oldest_active_start_timestamp` (:3084-3100) — the mechanism
  rmp #2318 needs.
- **PostgreSQL** (postgres/postgres @ `589eb4c3`,
  `src/backend/postmaster/autovacuum.c`). The launcher's naptime has a floor and a
  ceiling — `MIN_AUTOVAC_SLEEPTIME` 100 ms, `MAX_AUTOVAC_SLEEPTIME` 300 s
  (:149-150, :847-915) — and the sleep is latch-interruptible. That shape is
  `vacuumLoop`'s wait. Its horizon comes from
  `GetOldestNonRemovableTransactionId` (`src/backend/storage/ipc/procarray.c:1944`),
  which is what `mvcc.Horizon.Oldest` answers here.
- **InnoDB's purge coordinator was NOT read** for this task. The two above settle
  every decision it would have informed, and the module's rule is to cite what was
  read rather than what was recalled.

## 7. Observability

`Graph.VacuumStats()` reports `Running`, `Starts`, `Exits`, `Passes`,
`Reclaimed`, `CappedPasses`, `Backlog` and `RecordsPerPass`. Exported as gauges
under `lpg.mvcc.vacuum.*`, alongside the substrate's own `lpg.mvcc.versions.*`
(now including `ceiling`). The goroutine carries the pprof label
`gograph.goroutine=lpg.mvcc.vacuum`.

`lpg.mvcc.vacuum.pressure_unrelieved` counts the ceiling waits that timed out
without a pass — the states in which no pass will come (a closed graph, a sweeper
still starting). Letting the commit through there is deliberate: refusing a write
because reclamation is unavailable would be a wrong answer rather than a slow one.

## 8. Measured

See [`benchmarks/mvcc-vacuum-2026-08-04.md`](benchmarks/mvcc-vacuum-2026-08-04.md).
Summary, both statistically significant and of **opposite sign**:

- one node (trivial sweep): async **3.47 % slower** per commit (`p=0.000`);
- 16 384 nodes (realistic sweep): async **2.79 % faster** (`p=0.010`);
- the latency distribution is **indistinguishable at every quantile** — the tail
  win the hypothesis predicted did not reach significance and is not claimed.

The change therefore ships on soundness (§1, §3), not on speed. Soak layer:
`TestVacuumSoak_NoUnboundedGrowthUnderSustainedChurn`, 78 643 200 modifications
over 300 reader generations in 52 s, every generation settling to zero retained
records, 6020 vacuum starts against 6019 exits, no upward trend.
