# Stream 8 — Concurrency, scalability and contention

Baseline `6f31f61` (v0.10.0). Every GoGraph number below was measured first-hand in this session.

**Hardware / method.** Apple M4, 10 cores, macOS (Darwin 25.5.0), APFS, go1.26.5 darwin/arm64.
Two temporary harnesses, both deleted after measuring: `bench/zzs08/main.go` (goroutine-count
load test with latency percentiles and a *lock-free* metrics backend, so the harness itself never
appears in a contention profile) and `graph/lpg/zzs08_guard_bench_test.go` (in-package A/B of the
read entry point). `benchstat` over `-count=6` interleaved runs for every A/B; contention via
`runtime.SetMutexProfileFraction(1)` and `SetBlockProfileRate(1000)`.

**Contamination disclosure.** A background sampler (278 samples at 3 s,
`scratchpad/contam*.log`) shows a *foreign* audit stream ran
`go test -tags=threeway -run TestThreeWay ./bench/comparison/` continuously through the entire
measurement window (100/100 samples in the second window), plus intermittent `bench/s05probe`
benchmarks. Mitigation: (i) every A/B ran **interleaved in one process** so both arms saw the
same interference — `benchstat` reports ±0–4 %; (ii) the write results are **device-flush bound**
(3.8 ms fsync), which competing CPU load cannot distort; (iii) all headline conclusions are
*structural* (flat vs. scaling; present vs. absent in a profile), not marginal. Read absolute
op/s figures as lower bounds.

---

## Verdict summary

Round 1 concluded "GoGraph has the BEST read path but loses concurrent disjoint writes". **Both
halves are wrong at `6f31f61`.**

The transactional read path is not best-in-class. `lpg.Graph.View` — the entry point of *every*
Cypher read — costs **1.65 µs serial / 3.29 µs at 10 cores with one 64-byte allocation**, of which
**97–99 % is a re-entrancy guard** that parses `runtime.Stack()` for a goroutine id and takes a
process-wide exclusive `sync.Mutex` twice per query. The barrier it guards costs 3.6 ns. Aggregate
read throughput consequently **halves** from 1 to 10 cores, and scales only **2.31× on 10 cores**
(23 % efficiency). **Stream 2 measured the same defect independently and our numbers agree to
within noise** (see Cross-stream reconciliation) — and Stream 2 supplies the mechanism my profile
corroborates but did not name: `runtime.Stack` takes the Go runtime's global `debuglock` once per
stack frame, so the cost is *linear in stack depth*.

Meanwhile `visMu` — the RWMutex the flagship `#1671/#2051` epic exists to delete — **does not
appear in the mutex or block profile at all**. The two real contention sites are the plan-cache
mutex (39–55 %) and that same guard (25–40 %).

On writes the decisive question has a measured answer: **GoGraph should not admit concurrent
disjoint writers.** A durable commit is 3 830 µs of which the in-memory apply is **5.29 µs
(0.14 %)** — that 0.14 % is all disjoint writers could parallelise. What GoGraph actually loses is
**group commit**: the Cypher engine holds the *exclusive* visibility barrier across the WAL fsync,
so engine write throughput is **flat at ~261 op/s from 1 to 1024 writers with zero fsync
coalescing**, while the store-direct `txn.Tx.Commit` path — same WAL, same single writer, fsync
*outside* the lock — reaches **19 567 op/s (85×)**. One writer at 165 tx/s costs **61 % of read
throughput**, matching a 62.9 % barrier-occupancy model to within 2 points.

**Single most valuable lever: delete the re-entrancy guard from the production read path (effort
S, zero TCK/ACID surface).** Second: shard the plan-cache mutex. Third, and only third: move the
WAL fsync out of the exclusive visibility window — which is what `#1671` should be re-scoped and
re-justified as.

---

## Cross-stream reconciliation with Stream 2 — CORROBORATED

Stream 2 (transactions) reported the `runtime.Stack` guard defect. **My measurements corroborate
it on every shared quantity, from an independent harness with a different query shape.**

| Quantity | Stream 2 | This stream | Agreement |
|---|---|---|---|
| `Graph.View`, serial | 2 131 ns | 1 652 ns ±2 % | Same order; the gap is *explained by* Stream 2's own finding that cost is linear in stack depth (2.0 µs @1 frame → 17.2 µs @60) — our harnesses call `View` from different depths |
| Bare RWMutex acquire pair | 3.7 ns | **3.617 ns ±1 %** | **Near-identical** |
| Allocation per `View` | 64 B | **64 B, 1 alloc/op** | **Identical** |
| Aggregate read throughput, 1 → 10 cores | HALVES (1 623 → 3 215 ns/op, 1.98×) | HALVES (`ViewFull` 1.652 → 3.293 µs, **1.99×**) | **Near-identical** |
| Root cause | `runtime.Stack` takes the runtime's global `debuglock` per frame; pprof `runtime.Stack.func1` 85.09 % cum, `runtime.printlock` 55.28 % | My mutex profile: **`runtime.unlock` 38.66 % flat + `runtime._LostContendedRuntimeLock` 5.10 % = 43.76 % of all lock delay is inside Go *runtime-internal* locks**, and `goID` accounts for **40.05 %** of `enterReader`'s delay | **Corroborated from a different angle** — I saw the runtime-lock signature without having identified `debuglock`; Stream 2 names it |

Two independent measurements of the same defect, agreeing to within noise, with mutually
reinforcing mechanism evidence. **This is now the top concurrency lever, and it is a small fix.**

Two amendments I can add to Stream 2's account:

1. **It is also the #2 contention site under real concurrency, not just a serial tax.** At 64
   concurrent readers, `lpg.Graph.View` is **31.45 % of all mutex delay** and **44.68 % of all
   block delay**, of which **79.6 % is the guard** (`enterReader` 50.39 % + `exitReader`
   29.23 %) and only 20.38 % is the work inside the barrier.
2. **Because the cost is linear in stack depth, the production figure is worse than either
   microbenchmark.** `Engine.Run` calls `View` from deep inside the engine, so the realistic cost
   sits well above my 1.65 µs and Stream 2's 2.13 µs — toward their 60-frame 17.2 µs measurement.

**On Stream 2's `BeginReadTx` finding — CORROBORATED by code, not measured by me.** I did not run
that experiment (marked NOT INVESTIGATED below), but the mechanism is unambiguous in the source
and I confirm their result must follow: `BeginReadTx` acquires no barrier *for the handle*, but
each statement "runs through the engine's concurrent read path ([Engine.Run]), taking its OWN
per-statement [lpg.Graph.View] snapshot" (`cypher/exectx.go:325-327`) — and `View` is
`visMu.RLock` (`graph/lpg/lpg.go:629`), which blocks behind any holder of `visMu.Lock`. A write
`ExplicitTx` holds `visMu.Lock` for its **entire lifetime including client think-time**
(`cypher/exectx.go:1195-1205`). Therefore every `Exec` on a read-only transaction blocks for the
full duration of any concurrent open write transaction. **The godoc is factually wrong**: it
claims a read-only transaction "never serialises behind, or blocks, a concurrent writer" and that
its statements "run fully in parallel with other readers and writers"
(`cypher/exectx.go:313-314, 329`). CLAUDE.md requires an explicit and correct concurrency contract
on every exported type and states that ambiguity is a defect; an *incorrect* contract is worse.
Fixing the sentence is effort S and should ship regardless of the performance work.

---

## Feature-by-feature comparison

**Evidence caveat, stated plainly:** this session's web-search budget was exhausted before I could
verify Neo4j/Memgraph behaviour against official documentation, and the research subagent I
dispatched had not returned. The brief forbids claiming their behaviour from memory, so the
Neo4j and Memgraph columns below are marked **NOT INVESTIGATED** rather than filled with recall.
Every GoGraph cell is measured or cited to `file:line`. The verdict column is therefore given only
where it rests on GoGraph-internal evidence (e.g. GoGraph vs. its own store layer).

| Feature | GoGraph (`file:line`) | Neo4j | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| Transactional read entry cost | `Graph.View` **1.65 µs / 3.29 µs @10c, 64 B, 1 alloc** `graph/lpg/lpg.go:629` + `reentrancy.go:172` | NOT INVESTIGATED | NOT INVESTIGATED | Internally indefensible: 118×/36× the barrier it wraps | **NEW** |
| Read scaling | **2.31× on 10 cores** (23 % efficiency); barrier-only path *anti-scales* 374 k → 199 k op/s | NOT INVESTIGATED | NOT INVESTIGATED | — | **NEW** |
| Group commit / fsync coalescing | Implemented `store/wal/writer.go:491`; **unreachable from the engine** `cypher/api.go:4353` | NOT INVESTIGATED | NOT INVESTIGATED | **GoGraph WORSE than its own store layer (85×)** | **NEW** |
| Write concurrency model | Single serialised writer, capacity-1 semaphore `store/txn/txn.go:425,652` | NOT INVESTIGATED | NOT INVESTIGATED | Measured cost of the "gap" = **0.14 %** of a commit | CONFIRMED-R1, re-quantified |
| Reader/writer exclusion | `visMu.Lock` held across fsync → **62.9 %** barrier occupancy at 165 tx/s | NOT INVESTIGATED | NOT INVESTIGATED | **GoGraph WORSE** | **NEW** |
| Plan-cache concurrency | One process-wide `sync.Mutex` per query; **39–55 %** of measured contention | NOT INVESTIGATED | NOT INVESTIGATED | **GoGraph WORSE** | **NEW** |
| Lock-free immutable-CSR analytics | No sync primitive in the hot loop; separate from `visMu` | NOT INVESTIGATED | NOT INVESTIGATED | Genuinely lock-free (code-verified) | CONFIRMED-R1 |
| Read-only transactions | `BeginReadTx` takes no writer lock; **but its statements do take the barrier** `cypher/exectx.go:325` | NOT INVESTIGATED | NOT INVESTIGATED | Contract mis-documented | **NEW** (with Stream 2) |
| Tombstone read scaling (#2039) | Lock-free COW roaring bitmap `graph/lpg/lpg.go:356-378`; **+4.24× to 8 cores** | n/a | n/a | **FIXED, verified by measurement** | CONFIRMED-R1 |
| Bounded goroutines / pools | Bolt `sem` cap = `MaxConnections` (1024) acquired *before* spawn `bolt/server/serve.go:329,508`; `ParallelGovernor` clamps to GOMAXPROCS `cypher/exec/parallel_governor.go:57` | NOT INVESTIGATED | NOT INVESTIGATED | Sound (code-verified) | CONFIRMED-R1 |
| Goroutine-leak proof | `goleak` in every production goroutine-spawning package (bolt/server 31, cypher 27, cypher/exec 17, internal/sim 22, search 2+2, store/bulk 1) | NOT INVESTIGATED | NOT INVESTIGATED | Strong | CONFIRMED-R1 |
| Data races | `go test -race ./graph/lpg ./store/txn ./store/wal` → **exit 0, zero races** | n/a | n/a | Clean | CONFIRMED-R1 |
| Durability strength (Darwin) | **`F_FULLFSYNC`** (3 810 µs measured) via `os.File.Sync` `store/wal/data_sync_other.go:20` | NOT INVESTIGATED | NOT INVESTIGATED | Standing project note is **stale** | **NEW** |
| Latency tails | p99/p50 ≈ **500×** at 64 readers; p99.9 up to **2.48 s** at 1024 goroutines | NOT INVESTIGATED | NOT INVESTIGATED | Weak | **NEW** |
| Cache-line padding of hot atomics | **None anywhere in the tree** (grep: 0 padding fields) | NOT INVESTIGATED | NOT INVESTIGATED | LOW gap, unmeasured | **NEW** |

---

## Findings

### F1. The transactional read entry point costs 36–118× the barrier it protects  [NEW]  (severity: HIGH)

- **What GoGraph does.** Every Cypher read runs inside one `e.g.View(...)` (`cypher/api.go:1900`
  — a single acquisition wraps plan build, execution *and* materialisation). `Graph.View`
  (`graph/lpg/lpg.go:629`):

  ```go
  func (g *Graph[N, W]) View(fn func()) {
      gid := g.barrier.enterReader()   // goID() + process-wide Mutex + map insert
      defer g.barrier.exitReader(gid)  // process-wide Mutex + map delete
      g.visMu.RLock()
      defer g.visMu.RUnlock()
      fn()
  }
  ```

  `enterReader` (`graph/lpg/reentrancy.go:172-200`) calls `goID()` (`graph/lpg/goid.go:36`), which
  runs `runtime.Stack(buf[:64], false)` — a full traceback of the calling goroutine — then
  `strconv.ParseInt(string(...))`; it then takes `readerMu`, a **plain exclusive `sync.Mutex`
  shared by the entire process** (`reentrancy.go:91`), mutates a `map[int64]int`, and unlocks.
  `exitReader` takes the same mutex again. Per Stream 2, `runtime.Stack` acquires the Go runtime's
  global `debuglock` **once per stack frame**, so the cost grows with call depth.

- **Evidence** (`benchstat`, `-count=6`, `-cpu=1,10`, identical payload in every arm):

  | Variant | 1 core | 10 cores | B/op | allocs/op |
  |---|---|---|---|---|
  | `Graph.View` (shipped) | **1.652 µs ±2 %** | **3.293 µs ±1 %** | 64 | 1 |
  | `visMu.RLock` + same payload (guard removed) | 14.01 ns ±1 % | 91.88 ns ±4 % | 0 | 0 |
  | payload only (no synchronisation) | 12.80 ns ±1 % | 13.12 ns ±4 % | 0 | 0 |
  | guard alone | 1.216 µs ±0 % | 2.465 µs ±2 % | 64 | 1 |
  | `visMu.RLock/RUnlock` alone | 3.617 ns ±1 % | 56.95 ns ±11 % | 0 | 0 |

  The guard is **99.2 % of `View` at 1 core and 97.2 % at 10 cores**; `View` costs **118× / 36×**
  the barrier it wraps. Contention (64 concurrent readers): `Graph.View` = **31.45 %** of all
  mutex delay (`enterReader` 50.39 % + `exitReader` 29.23 % = **79.6 % is the guard**; only
  20.38 % is work inside the barrier); **40.05 %** of `enterReader`'s delay is inside `goID`
  itself. Block profile agrees: `View` 44.68 %. Runtime-internal locks account for **43.76 %** of
  all measured delay (`runtime.unlock` 38.66 % + `_LostContendedRuntimeLock` 5.10 %) — the
  `debuglock` signature.

- **Mandate violations.** CLAUDE.md: *"Hot paths must not hold a global lock"*, *"Lock-free read
  paths on immutable structures"*, *"Zero-allocation hot paths"*.
- **Lever.** The guard is a *deadlock detector*, not an isolation mechanism — removing it cannot
  change any query result. Keep the half that is already free (`writerGID atomic.Int64`, a
  lock-free load catching writer→writer and writer→reader), and compile the reader-map half only
  under `//go:build race` plus an opt-in `lpgdebug` tag. CLAUDE.md already mandates
  `go test -race ./...` before every push, so enforcement coverage is unchanged while production
  pays **zero**. If always-on enforcement is required instead, the sound alternative is to thread
  a `*View` handle through the read API — the mechanism `docs/isolation-design.md:110` already
  specifies for `#1671` — which makes nesting detectable from a value the caller holds: no
  goroutine id, no map, no lock.
- **TCK/ACID impact.** None. `visMu` and the commit ordering are untouched; nothing observable
  changes, so the 3897 baseline and all four ACID properties hold by construction. The only
  behavioural delta is that a *programmer error* (nesting `View`) would hang rather than panic in
  a non-race build — which is exactly why the atomic writer check stays always-on and the race
  gate covers the remainder.
- **Effort:** S.

### F2. The Cypher engine holds the exclusive barrier across the WAL fsync, making group commit unreachable  [NEW]  (severity: HIGH)

- **What GoGraph does.** `Result.commitUnderBarrier` (`cypher/api.go:4353`, invoked from inside
  `g.ApplyAtomically`) and `ExplicitTx.Commit` (`cypher/exectx.go:518-556`) call
  `tx.walTx.CommitWALOnly()` **inside** the `visMu.Lock` window. `CommitWALOnly`
  (`store/txn/txn.go:1207`) performs `appendOnly()` **and** `wal.SyncGroup()` — the device flush —
  with the engine-wide exclusive barrier held. One layer down, `txn.Tx.Commit`
  (`store/txn/txn.go:1095-1150`) does the opposite and proves the ordering is achievable: **append
  under the semaphore → release → `SyncGroup()` with no lock held → `waitApplyTurn(seq)` →
  `ApplyAtomically` → `advanceApply(seq)`**.

- **Evidence — write scaling, durable WAL, one `CREATE` per transaction:**

  | writers | Engine (`RunInTx`) | coalesced | Store-direct (`Tx.Commit`) | coalesced |
  |---:|---:|---:|---:|---:|
  | 1 | 261 op/s | 0 | 230 op/s | 0 |
  | 8 | 265 op/s | **0** | 931 op/s | 2 108 / 2 803 |
  | 64 | 268 op/s | **0** | 5 392 op/s | 15 715 / 16 225 |
  | 256 | 262 op/s | **0** | **19 567 op/s** | 58 373 / 58 851 |
  | 1024 | 255 op/s | **0** | 3 977 op/s | 11 756 / 12 357 |

  Engine write latency is a textbook single-server queue: p50 3.99 ms → 31.0 ms → 242 ms →
  964 ms → **3.47 s** at 1/8/64/256/1024 — exactly `conc × service_time`, throughput invariant.
  `store.wal.SyncGroup.coalesced` is **0 at every engine level** and 75–99 % on the store path:
  coalescing is not merely inefficient through the engine, it is *structurally impossible*, because
  `visMu.Lock` is a stricter lock than the semaphore the group-commit design was written to escape.

- **Cost decomposition.** Measured on this host: raw `fsync(2)` = **18.6 µs**, `F_FULLFSYNC` =
  **3 810 µs** (C harness, 200 iterations each); in-memory write with no WAL = **5.29 µs/op**
  (172 777 op/s). Engine commit p50 = 3 987 µs. A durable engine write is therefore **99.86 %
  device flush, 0.14 % in-memory apply**.

- **Fairness / starvation:**

  | workload | reader throughput | reader p50 | writer |
  |---|---:|---:|---|
  | 64 readers, 0 writers | 27 493 op/s | 263 µs | — |
  | 64 readers, **1** writer | **10 771 op/s (−61 %)** | 309 µs | 165 op/s, p50 6.00 ms |
  | 8 readers, 8 writers | 2 025 op/s (**7.4 %** of solo) | 3.97 ms | 254 op/s, p50 32.0 ms |

  Model check: 165 tx/s × 3.81 ms = **62.9 % barrier occupancy** ⇒ readers should retain 37.1 %;
  measured **39.2 %**. Model and measurement agree to 2 points, which pins the cause on the
  fsync-inside-the-barrier and nothing else.

- **Lever.** Give the engine `Tx.Commit`'s shape: buffer the transaction, fsync with the
  visibility window *released*, then publish. Because Cypher needs read-your-own-writes during
  execution, the buffered writes must live in a private overlay until publish — precisely the
  `atomic.Pointer[Snapshot]` copy-on-write root of `#1671` (`docs/isolation-design.md`, "Write
  path (commit)"): build the next snapshot privately (readers keep the old root, no lock), fsync,
  then one atomic `Store`.
- **TCK/ACID impact.** Durable-before-visible is **preserved, not weakened** — the fsync still
  completes before the atomic publish, exactly as `Tx.Commit` already sequences it. Isolation
  holds because the apply gate (`store/txn/txn.go:1247-1268`) keeps in-memory application in WAL
  sequence order and the publish is a single atomic step, so no reader observes a partial
  transaction. Atomicity unchanged. No Cypher semantics change ⇒ TCK 3897 untouched.
- **Effort:** L (this *is* `#1671`).

### F3. `#1671/#2051` must be re-justified — `visMu` is absent from the contention profile, and `#2051` was already tried and reverted  [NEW — overturns round 1]  (severity: HIGH)

- **What round 1 said.** `#1671/#2051` was ranked **T1(3)**, "top internal lever, already
  designed, tearing risk if partial", justified by read scaling.
- **What the measurements say.** In **both** the mutex and block profiles of a 64-goroutine read
  workload, **no sample attributes to `sync.RWMutex` or `visMu`**. 99.92 % of block delay is
  `sync.(*Mutex).Lock`, split `planCache.get` 55.24 % / re-entrancy guard 40 %. The RWMutex's own
  cost is small though it does anti-scale: `RLockOnly` 3.617 ns → 56.95 ns from 1 → 10 cores
  (15.7×, the shared reader-count cache-line bounce). Swapping it for an `atomic.Pointer` load
  saves **~55 ns per query**; removing the guard saves **~3 200 ns per query**. On the read path
  the guard is **~58× more valuable** than `#1671`.
- **`#2051` is not merely "unshipped" — it was implemented and reverted on measured evidence.**
  `docs/count-store-design.md:488` records the successor's acceptance gate: *"re-run the
  write-autocommit benchmark the reverted #2051 regressed (5,664→30,665 ns/op,
  2,398→102,416 B/op)"* — a **5.4× time and 43× memory** regression, attributed at
  `docs/count-store-design.md:352-356` to *"the O(shard) COW that made #2051 catastrophic (that
  copied whole shard maps … on every autocommit)"*. Round 1's framing materially understates the
  risk: a shard-granularity COW **has already failed empirically once in this codebase**.
- **Assessment / lever.** Keep `#1671` on the roadmap, but **re-scope and re-justify it as the
  write-path group-commit enabler of F2**, whose measured prize is up to **85×** write throughput
  and the return of the ~61 % of read throughput a single writer currently costs. Bind it to the
  gate the count-store design already wrote: no statistically significant `ns/op` or `B/op`
  regression on the write-autocommit benchmark (`benchstat`, `-count ≥ 5`), with per-shard version
  stamping and structural sharing of untouched shards rather than whole-shard-map copies. Do
  **not** fund it on a read-scaling argument — that argument does not survive the profile.
- **Tearing risk if partially applied (confirmed, unchanged).** The cut is all-or-nothing across
  ~12 substructures — adjacency, node/edge label shards, node/edge property shards, two roaring
  bitmaps, tombstones, `edgeCreateCount`, `edgeInstance*`, edge-handle stores, `index.Manager`.
  What makes a reader's *sequence* of per-shard reads consistent today is `visMu.RLock`. Severing
  one substructure to a pinned snapshot while the others stay barriered yields a torn
  cross-substructure view — **strictly weaker isolation than today**, i.e. a direct ACID
  regression. There is no sound intermediate state; the barrier removal and the full COW root are
  one indivisible change.
- **Effort:** L. **Sequence after F1 and F4**, which are S/M and deliver more measured relief.

### F4. The plan cache is the single most contended lock in the read path  [NEW]  (severity: MEDIUM)

- **What GoGraph does.** `cypher.planCache.get` takes one process-wide `sync.Mutex` per query; it
  cannot be an `RLock` because the LRU `MoveToFront` mutates on a read *hit*.
- **Evidence.** 64 concurrent readers: **38.97 % of all mutex delay** and **55.24 % of all block
  delay**, reached via `Engine.parseAndAnalyse` → `planCache.get`. It is #1 in both profiles.
  Project memory (2026-07-19) filed this as LOW with "measure before acting" — it is now measured,
  and it is #1.
- **Lever.** (a) Shard the cache (`hash(queryText) mod N`, N ≈ 16–64) — contained, removes
  ~15/16 of the contention; or (b) replace LRU with CLOCK/second-chance whose hit path is a single
  `atomic.Bool` store (no list splice) and which locks only on a miss. (a) first, (b) as end state.
- **TCK/ACID impact.** A plan cache is semantically transparent — same query, same plan, same
  rows, regardless of policy. Zero TCK and zero ACID surface. Preserve the bounded capacity
  (`DefaultPlanCacheCapacity` 1024) per stripe so the total stays bounded.
- **Effort:** M.

### F5. Group commit has an O(N) thundering-herd wake and collapses 8.8× past its optimum  [NEW]  (severity: MEDIUM)

- **What GoGraph does.** `wal.Writer.leadGroupSyncLocked` ends with `w.groupCond.Broadcast()`
  (`store/wal/writer.go:601`): *every* parked follower wakes, re-acquires `w.mu` and re-evaluates
  its watermark, each round.
- **Evidence** (store-direct path — the only one that can currently reach group commit):

  | writers | 2 | 16 | 128 | 256 | 512 | 1024 | 2048 |
  |---|---:|---:|---:|---:|---:|---:|---:|
  | op/s | 417 | 2 218 | 16 524 | **19 567** | 9 019 | 4 056 | 2 219 |

  Peak at ≈256 committers, then an **8.8× collapse** to 2048. Coalescing stays ~99 % throughout,
  so the loss is wake/re-contention overhead, not lost batching.
- **Lever.** Wake only the prefix of followers the published `durableSize` actually covers (keep
  waiters' watermarks in a small ordered list), or hand off to a single successor leader
  (ticket/FIFO) instead of broadcasting; alternatively cap group membership so the wake set is
  bounded.
- **TCK/ACID impact.** None — the wake strategy changes *who is woken when*, never what is
  durable; `durableSize`/poison semantics unchanged, so crash-injection behaviour is identical.
- **Effort:** M. Latent until F2 lands (unreachable through the public API today).

### F6. M1 `IsTombstoned` read-scaling fix (#2039) verified holding  [CONFIRMED-R1]  (severity: resolved)

- **Mechanism verified in code.** `tombstones atomic.Pointer[roaring64.Bitmap]` published
  copy-on-write with `tombstoneActive atomic.Int64` as a lock-free gate; `tombstoneMu` is now a
  plain `sync.Mutex` serialising **writers only** (`graph/lpg/lpg.go:356-378`). Readers
  (`IsTombstoned`, `LiveOrder`, `TombstonedIDs`) take no lock.
- **Evidence** (`BenchmarkTombstoneScan*`, 200 k-node graph, `-benchtime=300ms -count=6`,
  benchstat):

  | | 1c | 2c | 4c | 8c | 1→8 |
  |---|---:|---:|---:|---:|---:|
  | Clean (never deleted) | 308.8 µs ±8 % | 165.0 µs | 108.9 µs | 73.15 µs | **+4.22×** |
  | Tombstoned (10 % removed) | 1.455 ms ±5 % | 803.6 µs | 471.7 µs | 343.3 µs | **+4.24×** |

  Against the recorded pre-fix defect (2026-07-16: 1.46 / 4.38 / 6.52 / 7.42 ms at 1/2/4/8 =
  **0.20×, anti-scaling**, 67× slower than clean at 8c), the tombstoned path now scales
  *identically* to clean and is only **4.7×** slower at 8 cores.
- **Residual (LOW, no action).** The 4.7× constant is roaring64 `Contains` versus the clean path's
  atomic short-circuit. It scales linearly — a constant, not a scalability defect.

### F7. Read throughput scales 2.31× on 10 cores; per-query p50 degrades 3.5× as cores are added  [NEW]  (severity: MEDIUM)

- **`GOMAXPROCS` sensitivity** (32 concurrent readers):

  | GOMAXPROCS | 1 | 2 | 4 | 8 | 10 |
  |---|---:|---:|---:|---:|---:|
  | throughput | 10 745 | 16 609 | 17 841 | 23 284 | **24 819 op/s** |
  | speed-up | 1.00× | 1.55× | 1.66× | 2.17× | **2.31×** |
  | p50 | 83.8 µs | 100.5 µs | 183.9 µs | 299.1 µs | **296.3 µs** |

  **23 % parallel efficiency on 10 cores**, and median latency **3.5× worse** as cores are added —
  the signature of shared-cache-line synchronisation, not useful work.

- **Goroutine sweep at the CLAUDE.md-mandated levels** (`GOMAXPROCS` = 10):

  | goroutines | 1 | 8 | 64 | 256 | 1024 |
  |---|---:|---:|---:|---:|---:|
  | Cypher read (op/s) | 5 617 | 14 954 | 14 832 | 23 068 | 26 176 |
  | p50 | 182 µs | 256 µs | 268 µs | 285 µs | 287 µs |
  | p99 | 592 µs | 7.14 ms | **136 ms** | **344 ms** | **1.09 s** |
  | p99.9 | 4.20 ms | 18.5 ms | 344 ms | 770 ms | **2.48 s** |
  | Barrier-only read (`View`+prop, op/s) | **374 641** | 198 676 | 203 247 | 206 954 | 201 993 |

  The barrier-only row is the clean signal: **throughput falls 47 % from 1 to 8 goroutines** and
  then plateaus — nine extra cores yield *less* total work than one. This matches the F1 A/B
  exactly (`View` 1.652 → 3.293 µs ⇒ 605 k → 304 k op/s) and matches Stream 2's independent
  1 623 → 3 215 ns/op.
- **Tails.** p99/p50 ≈ **500×** at 64 readers. p50 stays flat while the tail explodes — convoy
  behaviour on the two exclusive mutexes of F1 and F4, plus GC pressure from the 64 B/query the
  guard allocates.
- **Lever.** F1 + F4; no separate change. Recorded to capture the **system-level** consequence and
  to correct round 1's "best read path" verdict for the *transactional* path. The immutable-CSR
  analytics path is genuinely lock-free and is not implicated.
- **Effort:** — (subsumed).

### F8. Correction: GoGraph's Darwin durability is `F_FULLFSYNC`, not a weak `fsync`  [NEW — corrects a standing project fact]  (severity: LOW)

- **The standing claim** (round-3 brief and project memory): *"macOS `fsync` != `F_FULLFSYNC`
  (weaker durability on Darwin) — known, opt-in fix pending."*
- **Evidence it is stale.** `store/wal/data_sync_other.go:20` calls `f.Sync()`; Go's
  `internal/poll.FD.Fsync` on Darwin issues `fcntl(fd, F_FULLFSYNC, 0)` and falls back to `fsync`
  **only** on `ENOTSUP` (`$GOROOT/src/internal/poll/fd_fsync_darwin.go:14-30`, comment: *"Fsync
  invokes SYS_FCNTL with SYS_FULLFSYNC because on OS X, SYS_FSYNC doesn't fully flush contents to
  disk"*). Measured here: `fsync(2)` 18.6 µs vs `F_FULLFSYNC` 3 810 µs; GoGraph's engine commit
  p50 is 3 987 µs — one full device flush per commit.
- **Consequences.** (i) Darwin durability is the **strongest** available setting, at a **205×**
  commit-latency cost; the roadmap item should be re-framed as an opt-*out* (an explicitly
  documented, explicitly unsafe relaxed mode), not a missing hardening. (ii) The 261 op/s engine
  write ceiling on macOS is a *device-flush* ceiling, so any Darwin write benchmark is not
  comparable to a Linux one (`fdatasync`, `store/wal/data_sync_linux.go:64`) unless this is stated.
- **Effort:** S (documentation).

### F9. `Engine.Explain` reports `NodeByIndexSeek` while `Engine.Run` executes a label scan  [NEW — cross-stream]  (severity: MEDIUM)

Reported here because it surfaced while building the read-scaling fixture and because it
*amplifies* F2; root-causing belongs to the planner/index stream.

- **Evidence.** `:P` nodes with `CREATE INDEX pk FOR (n:P) ON (n.s)`;
  `MATCH (n:P {s:$s}) RETURN n.v`. `Engine.Explain` prints
  `ProduceResults | Projection | NodeByIndexSeek`. Measured per-op: **192.6 µs at N=5 000 →
  2 136.4 µs at N=50 000** — linear in |V|, i.e. a full scan. Robust across every path: `Run`
  2 458 µs, `RunInTx` 12 274 µs, fresh engine with the index created before any query 2 685 µs,
  non-parameterised literal 2 667 µs (all N=50 000). CPU profile of the query loop *alone*
  (fixture build excluded): `exec.ColumnarFilter.FillChunk` **71.43 % cum**,
  `buildColumnarPredicate.makeColumnarComparePredicate` 38.10 %, plus `NodeByLabelScan.FillChunk`
  — no index-seek operator present.
- **Secondary observation.** The hash index engages for a STRING property but not an INTEGER one:
  `MATCH (n:P {s:$s})` plans `NodeByIndexSeek`; `MATCH (n:P {k:$k})` and `WHERE n.k = $k` plan
  `NodeByLabelScan` + `Selection`, with an index present on each.
- **Why it is also a concurrency finding.** `Engine.Run` holds `visMu.RLock` for the whole query.
  An O(|V|) read means O(|V|) barrier occupancy — that is what turns F2's reader/writer exclusion
  from a microsecond effect into a millisecond one.
- **Also measured:** a *read* query through `RunInTx` costs **5.0×** the same query through `Run`
  (12 274 vs 2 458 µs at N=50 000) — write-path overhead (exclusive barrier, undo log, index and
  count buffers) charged to read-only statements inside a write transaction.

### F10. `BeginReadTx`'s documented concurrency contract is incorrect  [NEW — corroborating Stream 2 by code]  (severity: MEDIUM)

- **What the doc claims.** `cypher/exectx.go:313-314`: a read-only transaction *"has no durability
  obligation and never serialises behind, or blocks, a concurrent writer"*; `:329`: its statements
  *"run fully in parallel with other readers and writers"*.
- **What the code does.** `cypher/exectx.go:325-327` — each statement *"runs through the engine's
  concurrent read path ([Engine.Run]), taking its OWN per-statement [lpg.Graph.View] snapshot"*.
  `View` is `visMu.RLock` (`graph/lpg/lpg.go:629`), which blocks behind any holder of
  `visMu.Lock`. A write `ExplicitTx` holds `visMu.Lock` for its **entire lifetime, including
  client think-time** (`cypher/exectx.go:1195-1205`). The handle takes no barrier; every statement
  on it does.
- **Corroboration.** Stream 2 measured a read-only transaction inheriting **100 %** of an open
  write transaction's hold (3.2 ms → 300.5 ms) and reader p50 degrading 95 µs → 10 653 µs (112×)
  under a 10 ms held write tx. I did not run that experiment, but the mechanism above makes the
  result necessary rather than merely plausible.
- **Lever.** Correct the godoc to state what is true: a read-only transaction never takes the
  *writer serialisation* and never blocks *other readers*, but each of its statements takes the
  visibility barrier's read lock and therefore blocks for the full duration of any concurrent open
  write transaction. Then keep the operational guidance already given for `ExplicitTx` (keep write
  transactions short; never hold one across think-time). CLAUDE.md requires a correct concurrency
  contract on every exported type.
- **TCK/ACID impact.** None — documentation only. The underlying behaviour is fixed by F2.
- **Effort:** S.

---

## The decisive question: can GoGraph admit concurrent disjoint writers without weakening ACID?

**Technically yes; economically no. The recommendation is: do not.**

- **What it would buy: 0.14 %.** A durable engine commit is 3 830 µs. The in-memory apply — the
  only part concurrent disjoint writers could run in parallel — is **5.29 µs**, measured directly
  (`apply-only` mode: 172 777 op/s with no WAL). Even a *perfect*, zero-overhead N-way
  disjoint-writer engine has a theoretical ceiling of a **0.14 % improvement** on durable write
  throughput.
- **What it would cost.** Disjointness must be *detected*, which means a conflict-detection
  structure on the write path; non-disjoint writers then need either blocking (deadlock detection
  + a client-visible retry contract) or versioning (MVCC delta chains + a version GC). Each is a
  new ACID-correctness surface on the one path in the system that currently has none: today's
  writer is serialised, retry-free, and serialisable by construction, and
  `docs/isolation-design.md` records the deliberate reasoning (write-write conflicts and write
  skew are *impossible*, so SI is free and equals serialisability here — Fekete et al., TODS
  2005). Adding concurrency re-introduces exactly the anomalies that argument eliminates.
- **What to do instead: group commit, worth up to 85×.** The *same* single-writer model, with the
  fsync moved out of the exclusive window, already measures **19 567 op/s vs 230 op/s** in this
  repository today — on the store-direct path, with the machinery already written, tested and
  crash-verified. The engine simply cannot reach it because of F2.

**Conclusion: concurrent writers are the wrong lever; group commit is the right one.** Round 1's
rejection of MVCC stands, and is now quantitatively justified rather than argued. Round 1's
companion claim that GoGraph "loses concurrent disjoint writes" should be struck: it names a
0.14 % gap while missing the 85× one next to it.

---

## Nothing-to-take list

- **Full MVCC / concurrent disjoint writers / per-object locking — reject.** See above: 0.14 %
  upside, a new correctness surface, and an 85× alternative already in the tree.
- **Off-heap page cache — not applicable.** GoGraph is RAM-native; there is no page cache to make
  concurrent.
- **`ParallelGovernor` — sound, no change.** `cypher/exec/parallel_governor.go:57-70`: budget =
  `GOMAXPROCS / inflight` clamped to `[1, morsels]`, nil-safe, sampled once per `Enter`. Worker
  count affects timing only, never results, so it has no ACID or TCK surface, and it correctly
  prevents the N×GOMAXPROCS explosion.
- **Bolt bounded pool — sound, no change.** `bolt/server/serve.go:329,508`: a `sem` of capacity
  `MaxConnections` (default 1024) is acquired **before** the goroutine is spawned, with a counted
  rejection path — backpressure, not unbounded spawn.
- **Goroutine-leak discipline — strong, no change.** `goleak` is present in the test teardown of
  every production package that spawns goroutines (bolt/server 31 files, cypher 27, cypher/exec
  17, internal/sim 22, search 2, search/centrality 2, store/bulk 1).
- **Race freedom — verified.** `go test -race -count=1 ./graph/lpg ./store/txn ./store/wal` →
  exit 0, zero `DATA RACE` reports.
- **Cache-line padding — noted, do not act yet.** There is no padding anywhere in the tree
  (`grep` for padding fields across `graph/`, `store/`, `cypher/`: zero hits), and `lpg.Graph`
  packs `tombstoneMu`/`tombstoneActive` and `visMu`/`barrier.readerMu` adjacently, so false sharing
  is plausible. Deliberately **not** proposed as a lever: I did not isolate it, and F1 removes the
  hottest of those fields entirely. Re-measure after F1, not before.

---

## NOT INVESTIGATED

Stated explicitly so nothing is mistaken for a verified negative.

1. **All Neo4j and Memgraph behaviour.** The session's web-search budget (200/200) was exhausted
   before I could verify a single comparative claim against official documentation, and the
   research subagent I dispatched had not returned. Per the brief's evidence rule I have asserted
   **nothing** about either product from memory. **This is the largest gap in this report:** the
   Standing/Lever verdicts the brief asks for cannot be completed for the external comparison, and
   my GoGraph-internal verdicts (GoGraph vs. its own store layer) are what stand. Specifically
   unverified and needing a follow-up with citations: Neo4j group commit / transaction-log batching
   and fsync default; Forseti lock granularity and the deadlock-retry contract; Neo4j default
   isolation and reader/writer blocking; the worker-pool thread model; JVM GC tail behaviour; page
   cache concurrency. Memgraph: concurrent-writer conflict/retry semantics; default isolation
   level; delta-chain GC cost and cadence; skip-list concurrency properties; WAL flush default;
   `IN_MEMORY_ANALYTICAL` guarantees. Also unverified: LMDB/SQLite single-writer rationale and the
   group-commit literature (Aether, VLDB 2010) I would have cited in the "nothing-to-take" section.
2. **`BeginReadTx` under a held write transaction — not measured by me.** Corroborated by code
   reading only (F10); the measurement is Stream 2's.
3. **The value of removing the guard, measured end-to-end.** I established the component cost
   (F1) and the contention share, but did not patch `Graph.View` and re-run the load test, to
   avoid editing shared source while other audit streams were working in the same tree. The
   projected win is an inference from the A/B, not a measured system delta.
4. **False sharing / cache-line padding.** Observed structurally (zero padding in the tree,
   adjacent hot fields), never isolated with a measurement.
5. **NUMA sensitivity.** Not applicable on a single-socket Apple M4; untested on multi-socket
   hardware.
6. **Go scheduler work-stealing behaviour under the parallel query runtime.** The
   `ParallelGovernor` was code-reviewed and the intra-query parallel path was exercised, but I did
   not capture a `runtime/trace` to observe steal/park behaviour, so the p99.9 tails at 1024
   goroutines (up to 2.48 s) are reported without a scheduler-level attribution. GC pressure and
   the two exclusive mutexes are the leading candidates; that ranking is a hypothesis, not a
   measurement.
7. **Root cause of F9** (Explain/Run plan divergence) — characterised and profiled, not diagnosed.
8. **Linux durability numbers.** All write figures are Darwin/`F_FULLFSYNC`. The
   `fdatasync` path (`store/wal/data_sync_linux.go:64`) was read but not measured, so the write
   ceiling on Linux is unknown and is likely far higher.

---

## Recommendation — ranked

1. **F1 — remove the re-entrancy guard from the production read path** (effort **S**, zero
   TCK/ACID surface). Eliminates 97–99 % of the transactional read entry cost, one allocation per
   query, and 25–40 % of all measured lock contention. Independently confirmed by Stream 2. Highest
   value-per-unit-risk in the entire stream, and it is a small, contained change.
2. **F4 — shard the plan-cache mutex** (effort **M**, zero TCK/ACID surface). The #1 contention
   site at 39–55 %.
3. **F10 + F8 — correct two wrong documented facts** (effort **S** each): the `BeginReadTx`
   concurrency contract, and the stale "weak fsync on Darwin" note.
4. **F2/F3 — move the WAL fsync out of the exclusive visibility window** (effort **L**). Prize:
   up to **85×** durable write throughput and the ~61 % of read throughput a single writer costs
   today. This is `#1671`, re-scoped and re-justified as a *write-path* change, gated by the
   write-autocommit no-regression benchmark that `#2051` failed, and taken as one indivisible
   change across all ~12 substructures.
5. **F5 — replace the group-commit `Broadcast` with a bounded/ordered wake** (effort **M**).
   Latent until 4 lands.

Items 1–3 are cheap, independent, and must not wait for item 4.
