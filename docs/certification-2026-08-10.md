# Production-readiness certification — GoGraph under extreme load and concurrency (2026-08-10)

**Date:** 2026-08-10 · **Entry head:** `df19e5de` · **Exit head:** see §12 · Apple M4 (10 cores),
`darwin/arm64`, go1.26.5, `kern.ipc.somaxconn=128`, `ulimit -n` 1048576 · Peer read at
**Memgraph `695a2fde`** (2026-08-10)

This cycle answers one question: **is the module fit for production environments of extreme demand in
load and concurrency?** It is ranked by the project's decision framework —
**correct → secure → fast → efficient** — and every axis is evaluated against Memgraph, read at
source rather than from memory.

---

## Verdict

**CERTIFIED for correctness, safety and reliability under sustained concurrent load. NOT CERTIFIED on
absolute throughput at extreme concurrency — because the instrument that would establish it does not
yet exist.** Those are two different answers to the question and both are load-bearing:

- **What is established.** The module withstands 1024 concurrent Bolt clients across read, write and
  mixed workloads with **zero errors**, degrading monotonically past saturation rather than
  collapsing; and over **55 minutes** of sustained mixed load it leaks nothing — goroutine and
  descriptor slopes exactly **0.000**, heap slope **negative** (§8) — with a **0.19 ms maximum GC
  pause** (§7). Correctness holds throughout: TCK 3897 unchanged, `-race` clean, ACID surfaces green.
- **What is not.** The mandated sweep opens a fresh TCP connection per operation, so its absolute
  figures measure Bolt handshake cost, not engine query throughput (§5, rmp #2397). A deployment
  needing a *number* for queries/second at 256 or 1024 pooled clients does not have one here.

The envelope is §9, and it is not a formality: **two of the gates this cycle relied on were found to
be measuring less than they appear to**, and one of them had never run at all.

| Rung | Verdict | Basis |
|---|---|---|
| **Correct** | **PASS** | `make ci` green (`MAKE_CI_EXIT=0` read inside the log) at three heads; `-race` clean; openCypher TCK baseline **3897** unchanged; bulk-load content proven byte-identical |
| **Secure** | **PASS on bounded resources, NOT RE-AUDITED otherwise** | Finite default per-query budget (10M rows / 1 GiB, typed error) — *stronger than Memgraph's default*; typed `LimitExceeded` admission control. No fresh hostile audit this cycle |
| **Reliable under sustained load** | **PASS, asserted** | 55-minute leak gate: goroutine and fd slopes **0.000**, heap slope **−517 B/sample** over 329 samples; 157 384 Bolt ops with **0 failures**; **max GC pause 0.19 ms** against a 200 ms ceiling (§7, §8) |
| **Fast** | **PASS with a measured ceiling** | End-to-end Bolt sweep at 1/8/64/256/1024 for read, write and mixed: 45/45 sub-benchmarks, zero errors, monotonic graceful degradation past saturation. Absolute figures are **connection-setup bound, not engine bound** (§5) |
| **Efficient** | **PASS with two open gaps** | Bulk load −32% allocation / −28% wall-clock when bracketed, content identical. Node memory **378–423 B/node against Memgraph's 204** remains open; so does the plan-cache literal gap |

**The single most consequential finding is not a performance number.** The project's own mandated
write-concurrency load test **could not run at the mandated levels at all**, and had not been able to
since rmp #2305. It is fixed here (§5.1). A second gate was found to abstain silently (§7.1).

---

## 1. Method, and what would have made it wrong

Every figure below was produced in this cycle. The disciplines applied, because each has produced a
false result on this project before:

- **The exit status is read from inside the log**, never from the harness notification. This mattered:
  a `make ci` run reported by the harness as "exit code 0" had **`MAKE_CI_EXIT=2`** recorded inside
  its log (§3).
- **Arms run in one process, interleaved, never overlapping**, for every A/B.
- **Benchmarks were not launched on the heels of a heavy run.** The sweep waited for the 1-minute load
  average to fall below 2.5 before starting.
- **Premises were re-measured before being built on.** One high-priority task's central premise was
  refuted outright (§4.2).
- **Two of my own instruments were wrong and were corrected**, not worked around: a test that could
  not fail for the reason it claimed (§4.2), and a throughput column derived with the wrong
  denominator (§5).

---

## 2. Correct

| Gate | Head | Result |
|---|---|---|
| `make ci` | `df19e5de` (entry) | `MAKE_CI_EXIT=0` · coverage **87.0%** (gate ≥85%), all packages ≥75% |
| `make ci` | `67977beb` (after #2387) | `MAKE_CI_EXIT=0` · coverage **87.0%** · `golangci-lint` 0 issues |
| `make ci` | after #2395, run 1 | **`MAKE_CI_EXIT=2`** — one `gocritic` `paramTypeCombine` in a new test |
| `make ci` | `c1c92c0a` (after fix) | `MAKE_CI_EXIT=0` · coverage **87.0%** · 0 issues |
| `go test -race` on touched packages | `67977beb` | `TEST_EXIT=0` across `cypher/...`, `graph/lpg/...`, `store/...` |
| openCypher TCK | all heads | `cypher/tck` **ok**; baseline constant **3897** unchanged |

Each log was independently grepped for `FAIL`, `--- FAIL`, `DATA RACE`, `panic:`, `make: ***` and
`Error N`: **0 matches** in every green run.

**Content identity, not just exit codes.** The bulk-load work changes cost and must not change
content, so it is checked by an order-sensitive fingerprint over every out-neighbour and weight of
every node, plus the edge count walked: **identical across 3 arms × 3 rounds** (fingerprint
`0x6bd188ab7d30d5d0`, 101 974 edges).

---

## 3. Secure

The security rung was **not re-audited** this cycle; it rests on the 2026-07-26 and 2026-07-31
audits. What *was* established is the bounded-resources half, which is the part that bears directly
on load — and here GoGraph is measurably ahead of the peer:

| Bound | GoGraph | Memgraph `695a2fde` |
|---|---|---|
| Per-query result memory | **Finite by default**: `DefaultMaxResultRows = 10_000_000`, `DefaultMaxResultBytes = 1 GiB`, typed `ErrResultRowsExceeded` | Per-query limit is **opt-in**; `UNLIMITED_MEMORY{0}` is the default (`memory/query_memory_control.hpp:27`), enforced when set by a thread-local `MemoryTracker` raising `OutOfMemoryException` |
| Concurrent open transactions per principal | `DefaultMaxOpenTxPerPrincipal = 16`, refused with a typed, Neo4j-compatible `Neo.ClientError.General.LimitExceeded` | — |
| Untrusted interchange input | 128 MiB cap, fail-stop | — |

A default that is finite is the difference between a hostile query being *refused* and the process
being *killed*. That said, the admission limit above is exactly what made the mandated load test
unrunnable (§5.1) — a correct safety property can still break a gate that was written before it
applied.

**Not evidence of absence.** No hostile input was fed to anything this cycle. Profiles of cooperative
workloads cannot surface an allocation whose size is a function of attacker-controlled input.

---

## 4. What was changed this cycle

### 4.1 rmp #2387 — a dead probe on every relationship row (`67977beb`)

`buildEdgeProps` probed the by-handle edge-property store on **every** relationship row, before the
gate that decides whether the answer is wanted. On a graph built through the Go API that probe can
only return nothing, because `AddEdgeLabeledWithProperty` and its siblings write the *per-pair* store
only. In `examples/26` it ran **17 009 744 times and its result was used zero times**, at 1.15% of
the run's CPU, paying two `Mapper` lookups, a shard mutex and a double map lookup each time.

Fixed with a **monotonic latch** (`atomic.Bool`) set at both creation sites *before* the shard lock
and never cleared, exposed as `AnyEdgeHandlePropertyEverWritten`. False is exact and proves the store
empty; true is conservative. Setting it ahead of the write means a reader observing false cannot be
hiding a visible property under Go's sequentially-consistent atomics, while a delete, abort-withdraw
or vacuum can only leave it conservatively true.

**The routing decision is unchanged, not approximated**: with the latch false, the `len(byHandle) > 0`
disjunct provably cannot hold, so the decision reduces to `hasByHandleEntry` — which comes from the
separate by-handle *type* store.

Measured where the finding was made: in an `examples/26` CPU profile,
`edgePropsByHandleToExprMap` and `EdgePropertiesByHandle` are **absent**, while `buildEdgeProps` is
still reached (1.77% cum) and the per-pair fallback `edgePropsToExprMap` is taken (1.36%) — the path
is exercised and the fallback used.

Eight tests, **injection-validated per site**: dropping the string-keyed latch fails three `lpg` tests
and both `cypher` preconditions; dropping the ID-keyed latch fails exactly the ID-keyed test.

### 4.2 rmp #2395 — the premise was refuted, and then my own test was too (`c1c92c0a`)

#2395 (priority 9) asserted that *"`lpg.Graph` exposes no commit window at all"* and *"the public API
cannot reach it"*, and asked for a new scoped bulk-load API. **Both claims are false.**
`ApplyAtomically` and `ApplyAtomicallyTx` are exported, both go through `openWriteBracket`, and both
already are the scoped bracket that cannot leak — they close on every path out of the callback,
including a panic. No new API was added.

Measured, one process, interleaved, 3 rounds, content byte-identical (5 000 nodes / 101 974 edges):

| Arm | Allocated | Objects | Wall |
|---|---|---|---|
| per-edge, unbracketed | 86 MB | 1.36 M | 69 ms |
| one `ApplyAtomically` | **58 MB** (0.68×) | **1.06 M** (0.78×) | **50 ms** (0.72×) |
| `ApplyAtomically` per 10k edges | 60 MB (0.70×) | 1.11 M (0.81×) | 55 ms |

Chunking keeps almost all of the win while bounding how long the exclusive barrier is held — which
matters, because a single bracket over a large load blocks every other writer *and every
snapshot-taking reader* until it returns.

**Then the mechanism turned out not to be what either the task or my first draft said.** Forcing
`adjlist.storeEntry`'s `inWindow` to false did **not** fail the tests, which meant the win was not
coming from where I had documented it. The dedup keys on a non-zero **builder owner**, which
`builderOwner()` takes from the *ambient transaction* first and from `BeginCommit`'s token only as a
fallback. So `ApplyAtomically` wins because it opens a **transaction**; its own `BeginCommit` call is
redundant there. `BeginCommit` exists for the transaction-less exclusive paths (WAL replay, bulk
import).

That same injection exposed a **weak test**: a direction-only assertion ("bracketed allocates less")
still passed with the dedup removed, because a second, independent saving kept it under. Injection
separated them — 0.758× with both mechanisms live, **0.921× with the slot-array dedup disabled** — so
about two thirds of the saving is the per-edge clone (11 546 objects, ≈1.9 clones per edge) and one
third is one shared MVCC commit record instead of one per write. Both tests now assert a 0.85×
threshold set *between* the two measured regimes, and injection-validate: forcing `inWindow` false
fails both with the intended diagnostic.

By the maintainer's explicit decision, `examples/01_basic` and `examples/26_social_scale_bench` were
**left unchanged**, so nothing they teach moved.

---

## 5. Fast — the end-to-end concurrency sweep

`bench/soak/cypher_rw_bench_test.go` drives real Bolt TCP round-trips at the levels CLAUDE.md
mandates. **45/45 sub-benchmarks, `count=3`, medians, host settled below load 2.5 before starting,
zero errors.**

`ns/op` under `b.RunParallel` is wall-time ÷ *total* iterations, i.e. the inverse of **aggregate**
throughput — not per-client latency. Both are given below; conflating them is how the first version
of this table overstated throughput by a factor of the concurrency level.

| Arm | Clients | Aggregate ops/s | Mean per-client latency | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| Read-only | 1 | 6 645 | 150 µs | 64 427 | 333 |
| Read-only | 8 | **15 643** | 511 µs | 65 028 | 329 |
| Read-only | 64 | 15 342 | 4.2 ms | 70 126 | 339 |
| Read-only | 256 | 12 394 | 20.7 ms | 71 414 | 341 |
| Read-only | 1024 | 1 215 | 842.7 ms | 76 157 | 350 |
| Write-only | 1 | 5 180 | 193 µs | 80 808 | 391 |
| Write-only | 8 | **7 236** | 1.1 ms | 80 846 | 389 |
| Write-only | 64 | 5 097 | 12.6 ms | 81 154 | 390 |
| Write-only | 256 | 2 674 | 95.7 ms | 85 504 | 400 |
| Write-only | 1024 | 2 018 | 507.4 ms | 98 696 | 419 |
| Mixed 80/20 | 1 | 6 308 | 159 µs | 67 715 | 346 |
| Mixed 80/20 | 8 | **9 283** | 862 µs | 67 670 | 344 |
| Mixed 80/20 | 64 | 6 624 | 9.7 ms | 68 241 | 347 |
| Mixed 80/20 | 256 | 3 729 | 68.6 ms | 71 087 | 352 |
| Mixed 80/20 | 1024 | 2 719 | 376.6 ms | 77 665 | 353 |

**What this does and does not establish.**

- **It does establish graceful degradation, which is the mandate.** Every level completed with zero
  errors. Throughput rises to a saturation point and then falls back *monotonically*; nothing
  collapses to zero, deadlocks, panics, or errors under 128× over-subscription of the cores.
- **It does NOT establish engine query throughput.** Every iteration opens a **fresh TCP connection
  and completes a Bolt handshake** before running one query. `MATCH (n) RETURN count(n)` over a
  16-node graph cannot account for 64 KB and 333 allocations per operation; the connection does. The
  absolute figures are therefore **connection-setup bound**, and the 1024-client cliff is at least
  partly the host's 128-deep accept backlog. Real Bolt drivers pool connections. Filed as
  **rmp #2397**; until it is done, these numbers must not be quoted as query throughput.
- **One transient failure, not reproduced.** An earlier run failed at Mixed/1024 with
  `boltDial negotiate: EOF` and `connection reset by peer` on a host whose 15-minute load average was
  still 15.8. The full sweep then passed on a settled host. Recorded as host-attributable, not as a
  defect.

### 5.1 The mandated write sweep had never run — fixed here

At `conc=64` the write arm failed with:

```
Neo.ClientError.General.LimitExceeded
principal "bench" already holds the maximum of 16 concurrently open transactions
```

`newBenchServer` sized `MaxConnections: 1200` explicitly to "comfortably cover any concurrency level
we test", but left `MaxOpenTxPerPrincipal` at its zero value, which selects
`DefaultMaxOpenTxPerPrincipal = 16`. That was harmless when written: a write transaction then held
the engine's writer serialisation for its whole life, so the server capped concurrent write
transactions at one server-wide and this bound could never bind. **rmp #2305 retired that hold**, and
`bolt/server/serve.go:121` says so in terms — *"It binds on WRITE transactions too as of rmp #2305"*.
The benchmark was never re-run at the mandated levels afterwards.

So the write and mixed figures at 64, 256 and 1024 **had never been produced**. The harness now sizes
the transaction bound the same way it sizes connections, with the measurement and the reason recorded
at the call site. The server's refusal was correct throughout — typed, bounded rejection rather than
unbounded queueing — which is exactly why the *benchmark* was the thing to fix.

---

## 6. Efficient

- **Bulk ingest.** 0.68× allocated bytes and 0.78× objects when bracketed, content identical (§4.2).
  Unbracketed, a load pays ≈1.9 shard-slot-array clones per edge.
- **Per-operation cost at the wire.** 329–419 allocs/op and 64–99 KB/op end to end, dominated by
  connection setup (§5).
- **Node memory remains the largest open efficiency gap: 378–423 B/node against Memgraph's 204 and
  Neo4j's 128.** `docs/design-node-memory.md` concludes the in-memory model must split to close it;
  that is a representation change requiring the maintainer's agreement and was not attempted.
- **GC behaviour under sustained load** is in §7.

---

## 7. Reliability under sustained load — the soak layer

Last cycle stated plainly that the soak layer was not run, so nothing certified it. It was run here.
`go test -tags=soak -count=1 -v ./bench/soak/...` — **7 PASS, 1 SKIP, `SOAK_EXIT=0`, 89.7 s**:

| Instrument | Measured |
|---|---|
| `TestBoltSoak_60s` | **157 384 successes, 0 failures**, 8 goroutines |
| `TestBoltCypherMixed_Smoke` | **54 721 successes, 0 failures, 0 cap errors**, 4 goroutines |
| `TestCypherRW_Analytics_Smoke` | heap **1 475 272 → 1 461 336** (did not grow), goroutines 2 → 2, 207 987 reads, 99 writes, 4 cancels |
| `TestGCPause_Stable` | **max pause 0.19 ms**, mean 0.08 ms, against a 200 ms ceiling; slope −0.000 ms/sample |
| `TestLatencyP99_Stable` | 155 599 successes, **26 failures** (0.017%) |
| `TestNoGrowth_HeapFDGoroutine` | heap 1 865 648, goroutines 6, **fds 6** |
| `TestPprofCapture` | heap snapshots written |
| `TestCypherRW_Analytics_30m` | SKIP (needs `SOAK_FULL=1`) |

A **0.19 ms maximum GC pause under sustained mixed Bolt load** is the strongest single number in this
certification, and the 26 failures in 155 599 (0.017%) are recorded rather than rounded away.

### 7.1 Two of these assertions did not actually assert — filed as rmp #2396

`TestNoGrowth_HeapFDGoroutine` logged *"insufficient samples for regression (< 2); skipping slope
check"* — its short variant measures for 20 s at a 10 s sample interval and collected **one** sample.
`TestLatencyP99_Stable` logged *"insufficient post-warmup windows for regression; skipping slope
check"* — 10 s against a 30 s window with 2 warm-up windows collected **one**.

**These are the two assertions CLAUDE.md names as the soak acceptance criterion, and in the short
layer both silently abstain while the package still reports `ok`.** "Soak layer green" therefore means
the workload ran, not that it was leak-free or latency-stable. Neither test exposes a duration
override, though the sibling `bolt_4h_test.go` already has `soakEnvDuration`/`soakEnvInt` helpers that
would allow an intermediate window.

Because the leak gate is the one that matters most for extreme load, the **full** 60-minute variant
(`SOAK_FULL=1`, 5 min warm-up + 55 min measurement) was run separately; its result is in §8.

---

## 8. The full leak gate

`SOAK_FULL=1 go test -tags=soak -run TestNoGrowth_HeapFDGoroutine ./bench/soak/` — 5-minute warm-up
plus 55-minute measurement, sampling heap, file descriptors and goroutine count every 10 s, with a
linear regression on each. This is the variant whose assertions actually run.

**`NOGROWTH_EXIT=0` · PASS in 3600.00 s · 329 samples · CSV
`soak-artefacts/no-growth-20260810T112656Z.csv`**

| Metric | Regression slope (per 10-s sample) | First sample (t=10s) | Last sample (t=54m50s) |
|---|---:|---:|---:|
| Heap | **−517 B** | 795 232 B | 906 168 B |
| Goroutines | **0.000** | 6 | 6 |
| File descriptors | **0.000** | 6 | 6 |

**This is the ACID/reliability mandate's soak criterion met on its own terms** — CLAUDE.md asks for
"zero growth in heap, file descriptors, and goroutine count after warm-up", and over 55 minutes of
sustained mixed Bolt load the goroutine and descriptor slopes are exactly zero while the heap slope is
**negative**. The heap oscillates between roughly 0.77 and 1.03 MB across the window; the endpoints
above are two samples from that oscillation, which is why the slope over all 329 — not the difference
between first and last — is the figure that decides it.

Read with §7.1 in mind: this is the *only* no-growth result in this certification that asserts
anything. The short variant's identical-looking `PASS` does not.

---

## 9. The envelope — what this certification does NOT cover

1. **Engine query throughput at concurrency is not established.** §5's instrument is
   connection-churn bound (rmp #2397). The narrow read-path figure in `docs/benchmarks/v0.10.0.md`
   (73–90 ns/op flat from 8 to 1024 on a hot-key intern) still stands but measures one function, not
   a query.
2. **No fresh hostile or security audit.** §3 covers bounded resources only.
3. **The soak layer's short variant does not assert no-growth or p99 stability** (rmp #2396). Only the
   60-minute leak gate in §8 asserts, and the 4-hour p99 variant was not run.
4. **`go test -tags=soak ./...` was NOT run** — only `./bench/soak/...`. The Makefile records that
   `graph/io/csv` alone takes 800.8 s under `-race` and that three packages did not complete at 45
   minutes, so the whole-tree soak layer remains unmeasured.
5. **Node memory (378–423 B/node vs Memgraph 204)** and the **plan-cache literal gap** (rmp #2393)
   and **per-execution plan rebuild** (rmp #2391) are open, quantified, and not addressed here.
6. **Single host, single architecture.** Apple M4, 10 cores, `darwin/arm64`, `somaxconn=128`. No
   Linux, no NUMA, no multi-socket, no cgroup-constrained container.
7. **Durability under crash was not re-exercised this cycle**; it rests on the existing
   `internal/crashinject` battery and the `store/wal` + `store/recovery` suites, which passed as part
   of `make ci`.
8. **The `docs/` tree's dated audits and certifications were deliberately NOT rewritten.** They are
   point-in-time records; several describe `Graph.View` in the present tense because it existed when
   they were written. Correcting them would falsify the historical record. Three *living* design
   documents did get a status note (§11).

---

## 10. Evaluation against Memgraph

Read at source, **Memgraph `695a2fde`** (2026-08-10), not from memory. Both are in-memory-first
Label-Property-Graph engines, so the comparison is like for like.

### Where GoGraph is ahead

| Axis | GoGraph | Memgraph |
|---|---|---|
| **Bulk load keeps ACID** | The bracket is a *transaction*. Versioning stays on; only the adjacency's copy-on-write granularity changes. Rollback, isolation and durability are intact, and content is proven byte-identical | `IN_MEMORY_ANALYTICAL` makes `CreateAndLinkDelta` **return immediately** (`storage/v2/mvcc.hpp:316-318`), so no deltas exist — and `Abort()` skips undo entirely when `transaction_.deltas` is empty (`inmemory/storage.cpp:1496-1498`). **Its fast bulk mode gives up rollback**, i.e. Atomicity |
| **Default resource bound** | Finite by default: 10M rows / 1 GiB per query, typed error | Per-query limit defaults to `UNLIMITED_MEMORY{0}` |
| **Bulk-load surface** | One scoped callback that cannot leak; no mode switch, no global state | A *storage mode* switched per database, with transition preconditions ("For analytical no other write txn can be in play") |

### Where the two agree — and why GoGraph's write ceiling is not a defect

**Memgraph also serialises its commit under one global lock.** `inmemory/storage.cpp:1157` takes
`std::unique_lock{storage_->engine_lock_}` and holds it across taking the commit timestamp,
validating unique constraints, **and appending to the WAL** — the comment states the reason: *"Write
transaction to WAL while holding the engine lock to make sure that committed transactions are sorted
by the commit timestamp in the WAL files."*

GoGraph's write path has the same shape: concurrency in the pre-commit work, a serialised commit
instant, group commit to amortise the WAL. So a write-throughput ceiling set by a serialised commit
section is **the design point the leading peer also chose**, not a GoGraph-specific weakness. §5's
write arm degrades monotonically from its 8-client peak rather than collapsing.

### Where Memgraph is ahead

| Axis | Memgraph | GoGraph |
|---|---|---|
| **Plan-cache key** | Keys on the **stripped** query text: literals are replaced by fixed tokens (`frontend/stripped.hpp:24-27` — `"0"`, `"0.0"`, `"\"a\""`, `"true"`) so `{id:1}` and `{id:2}` share one entry. The key is a `HashedString` whose `operator==` compares **both hash and full text**, so a collision can never serve another query's plan | Keys on the **raw** query text; no literal extraction anywhere in `cypher/`. A workload that inlines literals misses on every statement *and* churns the 1024-entry LRU. **Open: rmp #2393** |
| **Per-execution work** | The plan tree is immutable and shared — `MakeCursor(...) const` (`plan/operator.hpp:255`). Per execution it builds only a cursor and a frame from an arena, and the frame's width comes from the **cached** symbol table with slots reached by index (`interpreter.cpp:3644-3645`) | Rebuilds the physical plan per execution; `buildReadPhysical` was 59.56% of one example's allocation with `copySchema` the largest single flat site. **Open: rmp #2391** |
| **Node memory** | ~204 B/node | **378–423 B/node** |

### Reading the comparison

GoGraph's deficits are concentrated in **per-execution planning cost and memory density**; its
advantages are in **not trading ACID for speed** and in **bounding resources by default**. Under the
project's own ranking — correct → secure → fast → efficient — GoGraph is ahead on the two higher
rungs and behind on the two lower ones. That is the right way round, but the plan-cache gap is the one
most likely to bite a real high-load deployment, because generated and ad-hoc Cypher routinely inlines
literals and GoGraph's own examples all use parameters, which is precisely why no existing measurement
shows it.

---

## 11. rmp #2379 — the docs pointed production users at a removed API

Closed in this cycle, and its own scope estimate was low. It reported 22 stale godoc references to
`Graph.View` (removed by rmp #2344); the real count was **44 godoc links across 15 non-test Go files**,
plus 33 mentions in `docs/`. Several were not merely stale but **unfollowable instructions**:

- `store/txn/txn.go` — reads are consistent "ONLY when performed inside `[lpg.Graph.View]`"
- `cypher/api.go` — "The caller must already hold the graph's read barrier (`[lpg.Graph.View]`)"
- `cypher/api.go` — "the caller brackets the correlated reads under `[lpg.Graph.View]`"
- `graph/lpg/lpg.go` — asserted `Graph.View` "still exist[s]"

Every godoc link is gone (**grep: 0**), each instruction now names the snapshot API that replaced it
(`BeginRead` / `ReadAt` / `EndRead`) or the write barrier, and accurate historical notes were kept as
unbracketed prose by the maintainer's decision, so the record of what changed survives.

**It also surfaced a worse defect: a contract asserting a limitation that no longer exists.**
`ApplyAtomically`'s godoc still told callers they "must NOT yet rely on writes across DIFFERENT
substructures … becoming visible together even inside one transaction. Tracked as rmp #2378." But
**#2378 was fixed at commit `509929e2`** — a snapshot now pins its visibility verdict per commit
record, measured **zero tears in 300 runs** against a pre-fix 2–5 per 100, and pinned by
`TestIsolation_CrossSubstructure_EdgeImpliesLabels`. The documentation was steering users to work
around nothing. Corrected, with the measurement cited, in `lpg.go`, `store/txn/txn.go`,
`docs/acid-audit.md` and `docs/isolation-design.md`.

## 12. Commits

| Commit | Task | What |
|---|---|---|
| `67977beb` | #2387 | skip the by-handle property probe behind a monotonic latch |
| `c1c92c0a` | #2395 | record `ApplyAtomically` as the bulk-load bracket, and pin it |
| see §13 | #2379 | retire the stale `Graph.View` godoc; correct the `ApplyAtomically` contract |
| see §13 | — | this document, and the `bench/soak` transaction-bound fix of §5.1 |

## 13. Findings filed

| Task | Priority | What |
|---|---|---|
| **#2396** | 8 | The short soak layer's two headline assertions abstain for insufficient samples — a gate that cannot fail |
| **#2397** | 7 | The mandated concurrency sweep opens a fresh TCP connection per operation, so it measures handshake cost, not engine throughput |

## 14. Reproduce

```bash
# Correctness gate (read the status from inside the log, not from a wrapper)
make ci > ci.log 2>&1; echo "MAKE_CI_EXIT=$?" >> ci.log

# End-to-end concurrency sweep at 1/8/64/256/1024 (read/write/mixed).
# Redirect stdout and stderr SEPARATELY: the engine's startup warning is
# interleaved onto the benchmark result lines otherwise.
go test -run='^$' -bench=BenchmarkBolt -benchmem -count=3 \
  -timeout=2400s ./bench/soak/... > sweep.out 2> sweep.err

# Soak layer, short variants (~90 s). NOTE: the no-growth and p99 slope
# checks ABSTAIN here — see §7.1.
go test -tags=soak -count=1 -v -timeout=3600s ./bench/soak/...

# The leak gate that actually asserts (60 min)
SOAK_FULL=1 go test -tags=soak -count=1 -v \
  -run TestNoGrowth_HeapFDGoroutine -timeout=90m ./bench/soak/
```
