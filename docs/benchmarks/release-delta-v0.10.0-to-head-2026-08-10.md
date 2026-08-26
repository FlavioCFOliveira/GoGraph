# Release delta — v0.10.0 vs HEAD, measured end to end (2026-08-10)

**Base:** `v0.10.0` = `00072149`, 2026-07-25 00:58 +0100 · **Head:** `7a70eb3f`, 2026-08-10 18:44 +0100
· Apple M4 (10 cores), 32 GB, `darwin/arm64`, go1.26.5

> **READ THIS FIRST — superseded in part by `v0.11.0` (2026-08-13).** This record's head,
> `7a70eb3f`, is **five sprints behind the `v0.11.0` tag** (`ba436a5b`). Sprints 339–343
> landed afterwards and contain the largest memory and CPU wins of the release — the
> 2.74× cut in memory per relationship, the byte-stream node property bag, the 6.03×
> Bolt CPU reduction, and the bulk-delete fix that took a 90 000-node `DELETE` from
> 15.97 s to 375.6 ms. **Its READ and WRITE gains are therefore a floor, not a ceiling,
> for that tag; its REGRESSION figures are the ones to treat as still-current.**
> [`v0.11.0.md`](v0.11.0.md) re-measures at the real tag: the headline guard-band set,
> the read-path concurrency sweep, the store-level write-scaling ladder (which this
> record could not cover), and §6's defect.
>
> **§6's defect is CONFIRMED OPEN at `ba436a5b`, and its ratio is 21.1× there, not
> 24.4×.** Re-attributed with 1 000 rows asserted in every arm: default 4.087 ms on
> `ParallelScanProject` against 0.194 ms on `NodeByLabelScan [Rare]` with
> `DisableParallelScan`; the bitmap intersection is still not implicated; the anchor at
> 0.194 ms is now *faster* than v0.10.0 and still never chosen. **This document's
> closing recommendation — that it "belongs on the backlog before the next release" —
> was not acted on: it was never filed, and it survived three further sprints.** It is
> now **rmp #2431**. See [`v0.11.0.md`](v0.11.0.md) §5.
>
> **§9's documentation gap is CLOSED.** The `CHANGELOG.md` omission it found — the
> entire planner and executor body absent from `[Unreleased]`, with no mention anywhere
> of `STARTS WITH`, `ExpandIntersect`, `leapfrog` or `dbhits` — was fixed when the
> `0.11.0` section was written from `git log v0.10.0..HEAD`.

This is the record of what the sixteen days between the last release and the current commit
bought, and what they cost. It exists because no such comparison existed: the ledger in
`docs/benchmarks/history/` compares each change against the run before it, on a curated set
whose benchmark bodies themselves changed across the window, so it cannot answer "what does
upgrading from v0.10.0 to HEAD do to my workload".

Every figure below was produced in this cycle. Nothing is inherited from an earlier document.

Two findings in here could not be made by the module's existing instruments, and both are
recorded in full: a **24.4× regression that only the default configuration suffers** (§6),
and a **50–65× gain on parameterised queries that the flagship read suite is structurally
blind to** (§4.2).

## 1. What changed

429 commits, 934 files, **+168 457 / −6 175** lines.

| Type | Count |
|---|---|
| `docs` | 150 |
| `fix` | 71 |
| `feat` | 62 |
| `perf` | 51 |
| `test` | 39 |
| `bench` | 16 |
| `refactor` | 11 |
| merges, style, spike, revert | 29 |

**31 commits are marked breaking (`!`).** Files landed mostly in `cypher` (270), `graph`
(158), `docs` (101), `store` (76), `examples` (70), `bench` (64), `internal` (50), `bolt`
(43), `search` (9). Sprints 311–315, 326, 327, 329 (six rounds), 334–336 and 338 merged in
this window.

### 1.1 MVCC became the module's only concurrency control

At v0.10.0 concurrency control was exclusion: a single-writer semaphore in `store/txn`, an
engine writer mutex, and `Graph.View`, the read barrier. All three are gone
(`refactor(txn)!: retire the single-writer semaphore for MVCC-only concurrency`,
`perf(mvcc): retire the engine writer mutex`, `refactor(lpg)!: remove Graph.View, the last
pre-MVCC read barrier`).

What replaced them: a transaction clock and shared commit records; version chains on node
and edge labels, properties and adjacency; a contiguous commit frontier; optimistic
write–write conflict detection over seven stores with a retriable
`mvcc.ErrSerializationConflict`; a bounded background vacuum governed by a reclamation
watermark sized for 1024 concurrent readers; an MVCC clock derived from the WAL at recovery;
snapshots that record the instant they captured; and a checkpoint that captures at a
transactional instant while writers commit.

New public package `graph/mvcc` (92 exported declarations) and `cypher.Session` for
read-your-own-writes.

### 1.2 Planner and executor access paths

Prefix range seek for `STARTS WITH`; multi-label conjunction by Roaring bitmap
intersection; range-bitmap composition across single-property indexes; btree seek for
string and numeric equality; destination-ordered CSR neighbour runs probed in O(log d);
`ExpandIntersect`, a fused cyclic expand driven by multi-way leapfrog set intersection; an
index nested-loop join behind a measured cost gate; the hash join admitted for writing
statements; bidirectional search for single-path `shortestPath`, widened to typed, `DirIn`
and `DirBoth`; degree-answerable `COUNT` and `size()`; a widened columnar tier; and a
partitioned label scan so parallelism engages on real queries.

Sprint 315 belongs in this record as a negative result: the general worst-case-optimal join
was rejected on measurement and only a narrow fusion kept.

### 1.3 Observability and new capability

`PROFILE` with per-operator db-hits, physical-plan `EXPLAIN` (`Engine.Profile`,
`Engine.ExplainLogical`), per-statement write-effect counters reported over Bolt
(`Result.Counters`), MVCC telemetry (writers in flight, outcomes, conflicts by store,
version-chain depth distribution, vacuum latency, horizon utilisation), and a pprof profile
from every example. New at the edges: `store/bulkimport` with `Publish`,
`cmd/gograph-import`, Bolt transactions bounded by idle time and per-principal count with an
operator API to list and terminate them, and typed Go slices and maps bindable as query
parameters.

The public surface went from **4 368 to 4 936 exported declarations**: 593 added, 25 changed
or removed — among them `Graph.View`, `AdjList.BeginCommit`, `Tx.CommitWALOnly` and
`wal.Writer.SyncGroup`.

### 1.4 What did not change

- **openCypher TCK.** `const tckExecutionBaseline = 3897` is byte-identical at both
  revisions. 100% execution-level compliance is **preserved, not extended**.
- **Dependencies.** `go.mod` is byte-identical, so no dependency version and no toolchain
  difference can confound anything below.

## 2. Method

Two tiers, because neither alone is sufficient.

**Tier 1 — the module's own benchmarks, unmodified.** Of 187 benchmark-bearing `_test.go`
files, **123 are byte-identical** across the two revisions. Restricting to the 21 packages
in which *every* `_test.go` file is identical yields **143 benchmarks whose workload cannot
have changed** — only the library beneath them. No new file in any selected package
introduces an `init()`, a `TestMain`, or a package-level variable, so the fixtures are
identical too. Six rounds, arms alternated **per package** so host drift lands on both,
`-benchtime=1s -count=1 -benchmem`, compared with `benchstat`.

Result: **264 invocations, 0 failures, 906 result lines per arm.**

**Tier 2 — a harness written for this comparison.** One source file compiled against each
revision through a `go.mod replace`, so workload identity is by construction and was
verified by `sha256`. It covers what Tier 1 structurally cannot compare across revisions:
durable write throughput, write concurrency, read-under-write, resident memory, on-disk
footprint, recovery time, parameterised query latency, and durable ingest. Every measurement
asserts the work it claims to have done — node counts, edge counts, row counts, reader rows
— and prints the asserted value, so an arm that silently did less work cannot read as
faster.

### 2.1 The instrument lied first, and by omission

The first Tier-1 pass silently discarded **99 of 392 result lines per arm**. `go test`
forwards the test binary's stderr into its own stdout, so the engine's non-multigraph
construction advisory lands *on the benchmark's own output line* and pushes the numbers onto
the next line. A filter keyed on "name followed by an iteration count" dropped every
affected result, and `benchstat` ignored the unparseable remainder without a word.

The loss was exactly the 33 benchmarks of `bench/cypher_ldbc` — the single most important
package in the run — and it was symmetric across arms, so no arm was biased; the contaminated
lines were rejected rather than mangled, so the surviving 293 stayed trustworthy. That
package was re-measured over six fresh rounds with the repair this repository already ships
as `strip_log_noise` in `scripts/bench-history.sh`, recovering **39 of 39 results per
round**.

Had it gone unnoticed, the headline read figure would have been **−53%**, computed from six
plan-forcing arms, instead of the **−28%** the fifteen real LDBC queries give.

### 2.2 Declared limits of this measurement

- **The host was not idle.** The one-minute load average sat at 3–4 of 10 cores throughout
  (this session's own terminal). Arms were interleaved per package so drift affects both
  equally, and `benchstat`'s intervals are reported with every figure. It is a weaker
  condition than the load average of 2.38 the 2026-08-10 certification waited for.
- **Bolt is not measured.** 43 files changed there, but its concurrency harness at HEAD is
  new, so no identical-source A/B exists. Building one is a separate exercise.
- **One benchmark excluded:** `BenchmarkDIMACS_USA_SSSP`, which builds a 24M-vertex /
  60M-edge graph. At over 17 minutes of setup per invocation it would both blow the time
  budget and, at 5 cores of load, corrupt every concurrently running measurement.
- **Attribution of the §5 iteration costs to MVCC is inference, not proof.** The public
  MVCC on/off switch was deliberately removed at HEAD
  (`refactor(mvcc)!: discard the public MVCC on/off switch`), so no within-revision A/B is
  available to close it. The §6 attribution, by contrast, *is* proven — the switches that
  isolate it still exist.

## 3. Writes: from no concurrency at all to 16.5×

Writers on disjoint keys, 3 000 commits per run, median of 5, **zero conflict retries in
every configuration**, node count asserted after each run.

| Writers | v0.10.0 commits/s | HEAD commits/s | HEAD / v0.10.0 | v0.10.0 internal scaling | HEAD internal scaling |
|---|---:|---:|---:|---:|---:|
| 1 | 245 | 245 | 1.00× | 1.00× | 1.00× |
| 2 | 247 | 304 | 1.23× | 1.01× | 1.24× |
| 4 | 254 | 523 | 2.06× | 1.04× | 2.13× |
| 8 | 254 | 1 034 | 4.07× | 1.04× | 4.22× |
| 16 | 254 | 2 064 | 8.13× | 1.04× | 8.42× |
| 32 | 254 | 4 049 | **15.94×** | **1.04×** | **16.53×** |

v0.10.0 delivers the same throughput at 32 writers as at one: it had no write concurrency.
This independently reproduces the figures recorded in `bench/mvccwrite/gate_test.go`
(1.000 / 1.424 / 2.060 / 4.104 / 8.094 / 15.130) to within measurement noise, from a harness
that shares no code with it. The single-writer arm is unchanged at 245 commits/s — fsync-bound
in both revisions — so **nothing was traded for the scaling**.

The store-less (pure in-memory) write path also improved, and here the measurement goes
beyond the note in `gate_test.go` that this arm "buys nothing":

| Writers | v0.10.0 writes/s | HEAD writes/s | HEAD / v0.10.0 |
|---|---:|---:|---:|
| 1 | 135 398 | 257 186 | 1.90× |
| 2 | 113 135 | 387 762 | 3.43× |
| 4 | 109 787 | 471 608 | 4.30× |
| 8 | 109 707 | 469 932 | 4.28× |
| 16 | 111 008 | 508 143 | 4.58× |

Within HEAD this path now scales 1.98× from 1 to 16 writers, where v0.10.0 **degraded** to
0.82× — the 0.838× `gate_test.go` documents for the pre-MVCC state. Single-threaded
autocommit through `Engine.Run` went 126 346 → 262 498 writes/s (2.08×), and at engine level
`BenchmarkEngWriteAutocommit` fell **−80.22%** (5.714 → 1.130 µs) with 38% fewer
allocations.

## 4. Reads

### 4.1 The LDBC Interactive Complex workload

Fifteen queries, benchmark source byte-identical, n=6. Geomean **25.70 µs → 18.44 µs
(−28.24%)**, with **+3.17% B/op** and −9.51% allocs/op. The nine `_Parallel` variants give
−16.52% time and −5.38% B/op.

| Query | v0.10.0 | HEAD | Δ time | Δ allocs | p |
|---|---:|---:|---:|---:|---:|
| IC5 | 5.780 µs | 1.776 µs | −69.28% | −37.14% | 0.002 |
| IC8 | 5.786 µs | 1.796 µs | −68.97% | −37.14% | 0.002 |
| IC12 | 6.461 µs | 2.494 µs | −61.40% | −31.82% | 0.002 |
| IC6 | 6.099 µs | 3.974 µs | −34.84% | +15.79% | 0.002 |
| IC13 | 6.526 µs | 4.412 µs | −32.40% | +12.77% | 0.002 |
| IC10 | 21.60 µs | 16.31 µs | −24.50% | −16.90% | 0.002 |
| IC3 | 39.77 µs | 35.92 µs | −9.70% | −0.21% | 0.002 |
| IC7 | 40.31 µs | 36.45 µs | −9.58% | −0.21% | 0.002 |
| IC2 | 39.71 µs | 35.95 µs | −9.47% | −0.21% | 0.002 |
| IC14 | 40.80 µs | 36.99 µs | −9.34% | −0.21% | 0.002 |
| IC9 | 53.46 µs | 49.17 µs | −8.02% | −2.65% | 0.002 |
| IC4 | 53.53 µs | 49.40 µs | −7.72% | −2.65% | 0.002 |
| IC1 | 110.5 µs | 109.0 µs | ~ | −0.07% | 0.065 |
| IC11 | 153.3 µs | 153.0 µs | ~ | −1.01% | 0.937 |
| WithProjection | 60.17 µs | 60.51 µs | ~ | −17.74% | 0.093 |
| **geomean** | **25.70 µs** | **18.44 µs** | **−28.24%** | **−9.51%** | — |

### 4.2 A parameterised index seek did not seek

The largest per-query gain of the cycle is not in the planner work. On identical fixtures,
**both revisions plan `NodeByIndexSeek` and return identical rows**, yet:

| Shape | Fixture | v0.10.0 | HEAD | Δ |
|---|---:|---:|---:|---:|
| point lookup on indexed `id` | 5 000 nodes | 204 673 ns | 3 171 ns | **64.5×** |
| 1-hop from indexed anchor | 5 000 nodes | 216 811 ns | 4 038 ns | 53.7× |
| point lookup on indexed `id` | 20 000 nodes | 888 475 ns | 3 325 ns | 267× |
| 1-hop from indexed anchor | 20 000 nodes | 974 332 ns | 3 903 ns | 250× |
| 2-hop from indexed anchor | 20 000 nodes | 1 938 232 ns | 5 893 ns | 329× |

HEAD's plan prints the resolved key — `NodeByIndexSeek [seek="p0000001"]` — and v0.10.0's
does not. The cost confirms the mechanism arithmetically: v0.10.0 spends **41 ns per node at
5 000 nodes and 44 ns per node at 20 000**, i.e. its cost is O(label population). It was a
scan wearing a seek's name. The fix is `perf(cypher): resolve seek keys from the bound
expression, not source text` (2026-07-26).

**Why the module's own benchmarks could not see this.** Every benchmark in
`bench/cypher_ldbc` calls `engine.Run(ctx, query, nil)`, with literal keys in the query text
(`bench/cypher_ldbc/queries/*.cypher`). Literals resolved from source text and already
seeked at v0.10.0. So the flagship read suite measures −28% for this cycle while the same
engine improves 50–65× on the parameterised form that every Bolt client, every prepared
statement and every plan-cache hit actually sends. Both numbers are true; only one of them is
what a deployed application experiences. **Any future read-path gate should carry a
parameterised arm.**

### 4.3 Readers and writers stopped fighting

Four readers and four writers on one engine for three seconds, both throughputs measured
simultaneously, reader rows asserted non-zero so an idle reader cannot pass for a fast one.

| Arm | Reads/s v0.10.0 | Reads/s HEAD | Writes/s v0.10.0 | Writes/s HEAD |
|---|---:|---:|---:|---:|
| in-memory | 6 999 | 52 416 | 1 771 | 150 509 |
| WAL-backed | 987 | 209 602 | 248 | 495 |

The writer column is the one to read carefully. In-memory, v0.10.0 sustains ~109 000
writes/s with no readers present and **1 771** with four — a 62× collapse caused by the read
barrier. HEAD goes from ~470 000 to 150 509 under the same load, a 3.1× degradation. The
reader-side gains compound the barrier's removal with the parameterised-seek fix of §4.2 and
must not be attributed to MVCC alone.

Other confirmed read gains: `EngReadCount` −31.83%, `EngReadLabel` −15.11%,
`EngReadProject` −24.72% allocs. Graph algorithms: parallel triangle counting −38.16%,
Hopcroft–Karp −16.09%, Prim MST −8.38%, direction-optimising BFS −8.29%, label propagation
−6.39%. Seeding a 20 000-node / 60 000-edge graph through Cypher: 821 → 444 ms (1.85×).

## 5. The costs, reported as measured

Seven independent benchmarks in unrelated packages regressed by 7–16%, all of them paths
that walk a graph and read its properties.

| Benchmark | Package | v0.10.0 | HEAD | Δ time |
|---|---|---:|---:|---:|
| `EngReadCountLarge` | `bench/mtaudit` | 142.7 µs | 329.4 µs | **+130.85%** |
| `EngReadProjectLargeSerial` | `bench/mtaudit` | 5.364 ms | 6.308 ms | +17.61% |
| `WriteCSV` | `graph/io/csv` | 18.17 ms | 21.09 ms | +15.72% |
| `DIMACS_SF1_SSSP` | `bench/dimacs9` | 40.54 ms | 46.33 ms | +14.29% |
| `WriteCSV_LargeWeights` | `graph/io/csv` | 19.01 ms | 21.86 ms | +14.14% |
| `WriteDOT` | `graph/io/dot` | 20.87 ms | 23.45 ms | +12.33% |
| `Mapper_Intern_HotKey` | `graph` | 8.026 ns | 8.771 ns | +9.28% |
| `Load_Large_Baseline` | `store/bulk` | 279.0 ms | 303.3 ms | +9.06% |
| `EngReadProjectLarge` | `bench/mtaudit` | 2.013 ms | 2.191 ms | +8.85% |
| `BellmanFord_10kVertices` | `search` | 471.1 µs | 509.2 µs | +8.09% |

`WriteCSV` is the cleanest case: its B/op and allocs/op are **byte-identical** across
revisions (3.402 MiB, 5 allocations) while its time rose 15.72%. That is pure CPU on the read
path, consistent with per-read version resolution — an inference, per §2.2, not a proof.

`DIMACS_SF1_SSSP` is the one to watch for latency tails: its own reported percentiles move
further than its mean — p50 281.7 → 331.5 µs (+17.68%) and **p95 338.9 → 442.6 µs
(+30.58%)**, with 12.18% more allocations. A workload with a service objective feels that
more than the 14.29% average.

`store/bulk` pays in allocations too: the large load goes 1.772 M → 1.905 M allocations per
op (+7.52%), consistent with per-write version bookkeeping. `EngReadLabel` is the odd one
out — 15.11% *faster* but **+225.06% B/op** (22.06 → 71.70 KiB), a trade that does not look
deliberate.

### 5.1 Durability path

Tier-2 harness, median of 3, recovered node count asserted.

| Measurement | v0.10.0 | HEAD | Δ |
|---|---:|---:|---:|
| recovery, 15 000 WAL ops | 5.680 ms | 8.561 ms | **+50.7%** |
| on-disk bytes per node | 278 | 286 | +2.9% |
| durable ingest via Cypher | 240 elem/s | 236 elem/s | −1.7% |
| single-writer durable commit | 244/s | 244/s | 0.0% |

Recovery is 1.51× slower for the same 15 000 replayed ops — the price of deriving the MVCC
clock from the WAL and giving every commit a timestamp; the extra 8 bytes per node on disk is
that timestamp. Both are bounded, one-off costs rather than per-query ones, but a large WAL
replay now takes half again as long.

### 5.2 Memory is the good news

The same 20 000-node / 60 000-edge graph, read after two forced collections and again after a
three-second settle:

| Metric | v0.10.0 | HEAD | Δ |
|---|---:|---:|---:|
| heap alloc | 81.50 MB | 82.06 MB | +0.7% |
| heap in use | 96.61 MB | 97.91 MB | +1.3% |
| heap objects | 477 244 | 477 929 | +0.1% |
| peak RSS | 151.42 MB | 151.70 MB | +0.2% |
| bytes per element | 1 018 | 1 025 | +0.7% |
| total alloc over run | 528.41 MB | 564.88 MB | +6.9% |
| mallocs | 6 663 799 | 5 956 045 | **−10.6%** |

Total allocation rose 6.9% while the malloc *count* fell 10.6% — fewer, larger allocations.
The settled reading is identical to the post-seed reading on both revisions, so version
chains are being reclaimed rather than retained: **MVCC did not cost steady-state memory.**

## 6. One regression that is a defect, not a trade-off

> **RESOLVED 2026-08-26 (rmp #2431).** Everything below stood, and the root cause was the
> precedence this section identified as "what remains open". `tryBuildParallelScanProject` now
> judges the anchor the serial path would choose and anchors on it, so the default
> configuration plans the re-anchored scan: **4.611 ms → 0.196 ms**, and
> `BenchmarkMinLabelScanSelective_MinLabel` **4 980 µs → 204.7 µs**, which is faster than the
> v0.10.0 baseline of 230.3 µs rather than merely level with it. No shape regresses and the two
> shapes where the parallel scan legitimately wins get 19.9% and 44.2% faster, because the gate
> now anchors the parallel scan on the smaller label instead of ignoring the anchor. Full
> measurement, including the sweep that proved which label the gate was judging, in
> [`min-label-anchor-vs-parallel-scan-2026-08-26.md`](min-label-anchor-vs-parallel-scan-2026-08-26.md);
> pinned by `cypher/parallel_scan_min_label_precedence_test.go`.
>
> The other two arm pairs this section names — `RangeSeekSelective_IndexSeek` and
> `HashJoinDisconnectedEquiJoin_NestedLoop` — were **not** investigated by #2431 and remain
> open.

`BenchmarkMinLabelScanSelective_MinLabel` moved from 230.3 µs to 5 519.4 µs — **+2 296%** —
with 128× more allocations. Its fixture is 100 000 `:Common` nodes of which the first 1 000
also carry `:Rare`; the query `MATCH (n:Common:Rare) RETURN n.k` returns exactly 1 000 rows,
guarded at both revisions by `TestMinLabelScanBench_ResultsMatch`.

Reproduced outside the repository's own harness on the same fixture, asserting the 1 000 rows
in every configuration, then attributed by sweeping HEAD's own engine switches:

| HEAD configuration | ns/op | Chosen plan |
|---|---:|---|
| default | 5 439 523 | `Project / ParallelScanProject` |
| `DisableParallelScan` | **223 395** | `ColumnarProject / Filter / NodeByLabelScan [Rare]` |
| `DisableBitmapIntersection` | 5 595 405 | `Project / ParallelScanProject` |
| `DisableParallelScan` + `DisableBitmapIntersection` | 227 392 | `ColumnarProject / Filter / NodeByLabelScan [Rare]` |
| `DisableParallelScan` + `DisableMinLabelScan` | 3 369 524 | `ColumnarProject / ColumnarFilter / NodeByLabelScan [Common]` |

The conclusion is precise:

- **The min-cardinality label anchor still works at HEAD** — 223 395 ns, statistically
  identical to v0.10.0's 223 158–233 104 ns on the same fixture. It is simply never chosen.
- **The parallel-scan tier is admitted ahead of it and is 24.4× slower on this shape.**
- **The bitmap intersection is not implicated**: disabling it changes neither the plan nor
  the time.
- The regression is in the **default configuration**, which is what every user gets.

The same precedence pattern appears in two other arm pairs: `RangeSeekSelective_IndexSeek`
+22.35% while its `LabelScan` counterpart improved 74.14%, and
`HashJoinDisconnectedEquiJoin_NestedLoop` +17.38%. The two most recent `perf` commits both
touch this gate — `stop the parallel-scan gate cloning a bitmap it discards` and
`screen the parallel-scan gate on a bound, not an exact count` — so the gate was under active
work; its **precedence over a selective anchor** is what remains open.

A side observation from the same experiment: at v0.10.0 `EXPLAIN` printed
`NodeByLabelScan [n:Rare]` for *both* arms despite an 80× runtime difference — the printed
plan did not track `DisableMinLabelScan`. At HEAD the plan tracks the option faithfully. The
diagnostic became more honest even where the execution became slower.

## 7. Capability gained, which no ratio expresses

- **Snapshot isolation with conflict detection.** Concurrent read–modify–write at v0.10.0
  lost 46% of its committed updates (400 reported successes leaving a counter at 216, per
  `CHANGELOG.md`). HEAD detects write–write conflicts across seven stores and returns a
  retriable error. This is a correctness capability, and it is why the write path can be
  concurrent at all.
- **Concurrent checkpointing** at a transactional instant, instead of excluding writers.
- **Read-your-own-writes** through `cypher.Session`, carried through Cypher and Bolt.
- **Bulk import.** The same task — 2 000 nodes and 4 000 typed edges into an openable store —
  runs at **236 elements/s through Cypher and 121 000–172 000 elements/s through
  `bulkimport.Publish`**: about **690×**, on 483 KB of disk instead of 1.43 MB, with the
  published store verified reopenable and complete via `recovery.OpenCtx`. The trade must be
  stated with the number: the bulk route writes no WAL, is not a transaction, cannot be
  rolled back, requires an empty directory, and is concurrent with nothing.
- **`PROFILE` with per-operator db-hits**, physical-plan `EXPLAIN`, and per-statement
  write-effect counters — the module can now be asked *why* a query was slow.
- **Operable Bolt transactions**: bounded by idle time and per-principal count, listable and
  terminable.
- **A HIGH-severity default closed**: both engine-wide memory ceilings resolved to unlimited
  whenever `GOMEMLIMIT` was unset, which is the Go runtime's default state.

## 8. Verdict

HEAD is substantially better than v0.10.0 for the workload a deployed application actually
generates: parameterised point and neighbourhood queries are **50–65× faster**, durable write
throughput scales **16.5×** where it previously did not scale at all, and readers no longer
starve writers. Steady-state memory is unchanged and the openCypher TCK gate is intact at
3 897 scenarios.

Three costs are real and quantified: a **7–16% tax on graph iteration and bulk write** across
seven independent benchmarks; **recovery 1.51× slower** with 2.9% more bytes on disk; and one
genuine defect — the **parallel-scan tier outranking a selective label anchor by 24.4×** in
the default configuration, on a plan the same engine still knows how to produce.

The first two are the coherent price of MVCC and are bounded. **The third is not a trade-off:
it is a gate admitting a plan it should reject**, and it is the one item here that belongs on
the backlog before the next release.

## 9. A documentation gap found while assembling this

`CHANGELOG.md`'s `[Unreleased]` section (lines 7–224) documents only the MVCC, durability and
isolation work. The entire planner and executor body — sprints 311–315 and the allocation work
of 8–10 August — is absent from it. Searching the whole file finds no mention of
`STARTS WITH`, `ExpandIntersect`, `leapfrog` or `db-hits`. The work is recorded in
`docs/benchmarks/` (which grew from 20 to 83 records in this window) and in the commit log,
but a reader of the changelog alone would not know that the largest read-path gains of this
cycle exist.

## 10. Reproduce

The comparison needs both revisions present at once, so put the base in a worktree:

```bash
git worktree add /tmp/gg-v0100 v0.10.0 --detach
```

**Tier 1.** Select only the benchmarks whose declaring `_test.go` file is byte-identical
between the two trees, and only in packages where *every* `_test.go` file is identical.
Then, for six rounds, run each package on both trees back to back, alternating per package.
Two rules are load-bearing:

```bash
# 1. Repair the interleaved log lines BEFORE filtering, or results are lost silently.
#    strip_log_noise is already in scripts/bench-history.sh — reuse it.
# 2. Assert the number of results kept per invocation against the number of benchmarks
#    the regex selected. A run that quietly keeps 6 of 39 must be visible.
go test ./bench/cypher_ldbc/ -run '^$' -bench "$REGEX" -benchtime=1s -count=1 -benchmem
```

Exclude `BenchmarkDIMACS_USA_SSSP` (24M vertices; >17 min of setup per invocation).

**Tier 2.** One `package main` compiled against each revision via `replace`:

```
module abbench
require github.com/FlavioCFOliveira/GoGraph v0.0.0
replace github.com/FlavioCFOliveira/GoGraph => /tmp/gg-v0100     # or the live tree
```

Construct the engine the way `examples/27_concurrent_txn` does — `wal.Open`,
`lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})`,
`txn.NewStoreWithOptions`, `cypher.NewEngineWithStore` — and detect the serialization
conflict by message (`strings.Contains(err.Error(), "serialization conflict")`), because the
`mvcc.ErrSerializationConflict` sentinel exists only at HEAD and the source must compile
against both.

**The §6 attribution** needs only HEAD. Build the fixture (100 000 `:Common`, of which 1 000
also `:Rare`, each with a unique `k`), then sweep `cypher.EngineOptions` over
`DisableParallelScan`, `DisableBitmapIntersection` and `DisableMinLabelScan`, printing
`Engine.Explain` alongside the latency and asserting 1 000 rows in every configuration.

Wait for the host to settle first: check `uptime`, and expect a systematic bias — not noise —
from a one-minute load average above ~2.5.
