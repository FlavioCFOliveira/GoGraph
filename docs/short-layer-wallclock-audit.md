# Short-layer wall-clock assertion audit

Complete-population audit of tests in the **short** layer that assert on wall-clock
time, CPU time, or throughput. Recorded for rmp #2517, whose acceptance criteria
require the audit for further wall-clock assertions to be written down.

**Date:** 2026-08-25. **Tree:** `1faafff5` (sprint 350).

## Why these assertions are a defect class, not a set of flaky tests

`make ci` runs `test-short`, which is `go test -race -count=1 ./...`. `go test`
builds and runs **packages in parallel** (`-p` defaults to `GOMAXPROCS`), so a
short-layer test that asserts on elapsed time measures *the machine's load at that
instant*, not the code. Every such gate can go red on a tree whose functional
tests are entirely green, and — worse in the other direction — contention can
*compress* a ratio and make a power control silently stop firing.

Three instances were filed before this audit (#2499, #2506, #2517) and a fourth
during it (#2589). The audit exists because a policy applied to three instances
while thirty-six others remain is not a fix.

**CPU time is not a load-independent proxy.** This is the audit's most
consequential finding, and it is settled by the repo's own history rather than by
argument. `cypher/delete_scaling_test.go` records two prior attempts on the same
test family:

- Moving the **absolute wall-clock** assertion to the soak layer *worked*:
  `TestSingleStatementDeleteOfNinetyThousandNodes` carries `RequireSoak(t)` and its
  godoc records **40.61 s in the short layer for work that takes 375.6 ms alone**.
- Keeping the regression claim in the short layer by switching to a **CPU-time
  ratio** *failed*, and that failure is #2517 instance 3 and #2589. The same godoc
  asserts the claim can stay short-layer "only because they measure CPU rather than
  wall time"; the gate then read **2.90x against a 2.5x limit under load and 0.94x
  solo**, while its sibling power control failed the opposite way because
  contention compressed the ratio it needs to exceed a floor.

Contention inflates CPU time through scheduler overhead, cache and TLB pressure,
and spinning. Any proposal to "assert on a load-independent proxy" must therefore
name a genuinely load-invariant quantity — allocation counts, operation counts, or
a fitted complexity exponent — and accept that the timing claim itself is then not
gated at all.

## Method

Three independent `go/ast` sweeps rather than text search: `if`-conditions
containing time tokens; `select`/`for`/`switch` deadline branches; and
failure-message matching with taint tracking through helper-computed conditions
such as `!passesGate(...)`. Cross-verified in Python. **No conclusion rests on a
`grep` returning empty** — `grep` on this machine is `ugrep` and can return a
silent empty result. 2,053 test files examined; 74 are build-tag gated; both file
headers and test bodies were checked for `RequireSoak`/`RequireNightly`.

A test counts as an instance when a failure condition depends on an elapsed
duration, a ratio of measured durations, a throughput or rate, a CPU-time
measurement, a deadline whose *expiry is treated as failure*, or growth measured
across a time window. Tests that merely **report** a timing without failing on it
are excluded, as are allocation counts, operation counts, fitted exponents,
`Benchmark*` functions, and anything already gated to soak/nightly.

## Instances — 39 across 12 packages

> **Correction applied 2026-08-25, after the audit was first written.** The audit
> originally reported 40 instances across 13 packages, including
> `bench/r4audit/w1partb_test.go:156` as the single strictest gate in the repo
> (`off <= on`, zero tolerance). That file carries `//go:build r4audit`, a
> **custom** build tag, so `go test ./...` never compiles it and it is not a
> short-layer instance at all. Verified twice: the tag is present in the file
> header, and `go test -run TestW1PartB_HashJoinIsWhatChanged ./bench/r4audit`
> fails with "build constraints exclude all Go files". The audit's gating check
> looked for `soak`/`nightly`/`stress`/`soakfull` and for
> `RequireSoak`/`RequireNightly`, and a project-specific tag fell outside it.
> Every other listed file was re-verified as carrying **no** build tag.

| package | file:line | test | asserted | threshold | load-sensitive because |
|---|---|---|---|---|---|
| bench/cyclicjoin | exponent_test.go:244 | TestCyclicJoin_FittedExponents | fused arm per-point wall cost ≤ two-Expand's | `fused > two*1.50` | **#2506**; arms measured at different instants |
| bench/mvccwrite | gate_test.go:352 | TestWriteScalingGate | 8-writer ÷ 1-writer commits/s | floor 0.60, best of 3 | 1-vs-N throughput ratio; its own message says "or the machine is loaded" |
| bench/mvccwrite | gate_test.go:389 | TestWALWriteScalingGate | same, WAL wiring | floor 3.00, best of 3 | same |
| bench/mvccwrite | gate_test.go:409 | TestWriteConcurrencyGate | free ÷ globally-mutexed throughput | floor 0.50, best of 3 | both arms measured, minutes apart |
| bench/mvccwrite | gate_test.go:562 | TestWriteScalingInstrument_SeesConcurrency | spin-work 8/1 throughput | floor 3.00 | **#2499**, measured 2.984x vs 3.00x |
| bench/mvccwrite | gate_test.go:570 | TestWriteScalingInstrument_SeesConcurrency | serialisation ratio of parallel spin work | floor 3.00 | same statistic; 100–150k/s vs 65–89k/s drift within one run |
| bench/mvccwrite | gate_test.go:631 | TestWriteScalingInstrument_SeesSerialisation | serialised work must measure **below** target | `< 3.00`, worst of 3 | inverse gate on the same ratio |
| bench/soak | bolt_soak_test.go:137 | TestBoltSoak_60s | HeapAlloc after ÷ baseline over a 10 s window | `> 2.0` | heap growth over a time window; package has **no** build tag |
| bolt/server | abandoned_tx_test.go:84 | TestAbandonedTx_IdleReaperBoundsTheReaderStall | reader stall < total tx timeout | 20 s | elapsed vs constant |
| bolt/server | abandoned_tx_test.go:90 | same | stall on the idle bound's timescale | 5.3 s | elapsed vs constant |
| bolt/server | e2e_autocommit_read_no_block_test.go:161 | TestE2E_ConcurrentAutocommitReadsRunInParallel | 8 concurrent reads' total wall time | 6.0x a measured baseline | ratio of two measured times; already has a `CoverMode()` escape hatch |
| bolt/server | e2e_autocommit_read_no_block_test.go:256 | TestE2E_AutocommitReadDoesNotAcquireWriterLock | read-only autocommit elapsed | `>= 5 s` | elapsed vs constant, under concurrent writers |
| bolt/server | tx_timeout_reaper_test.go:143 | TestTxTimeout_IdleOpenTransactionIsReaped | conn B write elapsed | `> 4 s` | elapsed vs constant |
| bolt/server | tx_timeout_reaper_test.go:152 | same | reap latency after BEGIN | `> 4 s` | elapsed vs constant |
| cypher | begintx_deadline_test.go:145 | TestBeginTx_DoesNotWaitForAnythingAtAll | BeginTx elapsed | **500 ms** | a lock-free BeginTx costs µs; 500 ms is scheduler territory under `-race` |
| cypher | begintx_deadline_test.go:201 | TestExplicitTxExec_HonoursDeadlineUnderAnExclusiveBarrierHolder | Exec elapsed | `>= 3 s` (60x a 50 ms budget) | elapsed vs constant |
| cypher | delete_scaling_test.go:300 | TestDeleteDoesNotDegradeAcrossCycles | **CPU-time** ratio last/first | `> 2.5` | CPU is not load-invariant |
| cypher | delete_scaling_test.go:318 | TestDetachDeleteDoesNotDegradeAcrossCycles | same, DETACH | `> 2.5` | **#2517 instance 3**: 2.90x loaded, 0.94x solo |
| cypher | delete_scaling_test.go:349 | TestDeleteCycleGateDetectsDegradation | power control: CPU ratio must **fire** | `<= 2.5` fails | **#2589**: contention *compresses* the ratio |
| cypher | mvcc_no_writer_serialiser_test.go:181 | …DoesNotStallAutocommitUnboundedly | autocommit write elapsed | `> 3 s` (10x a 300 ms budget) | elapsed vs constant |
| cypher | security_cypher_cartesian_test.go:177 | TestSec_Cypher_Cartesian_IsCancellable | abort elapsed | `> 5 s` (16.7x) | elapsed vs constant |
| cypher | security_reduce_dos_test.go:136 | TestSec_Cypher_Reduce_LoopHonoursDeadline | abort elapsed vs a **measured warm run** | `elapsed > warm/2` | both terms measured; deadline is `warm/20` |
| cypher | runintx_ctx_contention_test.go:130 | TestRunInTx_HonoursDeadlineUnderQuiesce | RunInTx elapsed | `>= 5 s` (100x) | elapsed vs constant |
| graph/adjlist | hub_bench_test.go:96 | TestHub_AddEdge_AmortisedSublinear | median(hub-10k) ÷ median(hub-1k) | `>= 40` | ratio of two measured wall times |
| graph/mvcc | gate_ctx_test.go:212 | TestGateCtx_ReturnsWithinItsBudget | per-caller elapsed, **64 concurrent callers** | **105 ms** | thinnest concurrent case; **this is #2574** |
| graph/mvcc | gate_test.go:327 | TestGate_WeakLockCtxHonoursTheDeadline | WeakLockCtx elapsed | `> 5 s` (100x) | elapsed vs constant |
| internal/sim | bolt_begin_extras.go:1619 | TestWireClientBeginExtras_* | `BeginElapsed > beginReplyBound` → Violation | 30 s | `time.Since`; oracle emits a Violation the short test fails on |
| internal/sim | bolt_begin_extras.go:1871 | same family | `ReplyElapsed > beginReplyBound` | 30 s | as above |
| internal/sim | bolt_decode_pressure.go:2769 | TestBoltDecodePressure_* | `Elapsed > boltDecodeHonestBound` | 30 s | as above |
| internal/sim | bolt_tx_quota.go:1050 | TestBoltTxQuota_Clean | accepted BEGIN latency, "REAL time" | 5 s | as above |
| internal/sim | bolt_tx_quota.go:1115 | TestBoltTxQuota_Clean | refused BEGIN latency, "REAL time" | 5 s | as above |
| search/flow | dinic_worst_case_test.go:67 | TestMaxFlow_WorstCase | Dinic duration | `> 2 s` | elapsed vs constant |
| search/flow | dinic_worst_case_test.go:78 | same | Edmonds-Karp duration | `> 2 s` | elapsed vs constant |
| search/flow | edmonds_karp_clrs_test.go:48 | TestEdmondsKarp_CLRS_Bad | EK duration | `> 1 s` | elapsed vs constant |
| search/flow | push_relabel_worst_case_test.go:63 | TestPushRelabel_WorstCase | push-relabel duration | `> 2 s` | elapsed vs constant |
| search/flow | push_relabel_worst_case_test.go:74 | same | Dinic duration | `> 2 s` | elapsed vs constant |
| store/checkpoint | writer_stall_test.go:102 | TestCheckpoint_WriterStallBoundedByCapture | one concurrent commit's latency | **150 ms** | thinnest absolute budget |
| store/txn | begin_ctx_cancellable_test.go:117 | TestStore_BeginCtx_DeadlineUnderQuiesce | BeginCtx elapsed | `>= 5 s` (100x) | elapsed vs constant |
| store/wal | embed_scan_budget_test.go:56 | TestEmbedsValidFrame_AdversarialTailIsBounded | scan elapsed | `> 2 s` (~2000x) | elapsed vs constant |

Not counted, listed for completeness: `bench/mvccwrite/gate_test.go:323`
(`commitsPerSec() <= 0`) — a throughput floor of literally zero, tripped only by a
total stall.

### Thinnest four, if the policy needs a priority order

1. `graph/mvcc/gate_ctx_test.go:212` — 105 ms across 64 goroutines (**#2574**).
2. `store/checkpoint/writer_stall_test.go:102` — 150 ms absolute.
3. `cypher/begintx_deadline_test.go:145` — 500 ms on a microsecond operation.
4. `cypher/security_reduce_dos_test.go:136` — a ratio of two separately-measured runs.

The zero-tolerance gate that originally headed this list was the `bench/r4audit`
one now known to be outside the short layer — see the correction above.

## Already gated correctly (assert on time, behind soak/nightly)

`bench/mtaudit/fairness_soak_test.go:173,278,283,289`;
`bench/soak/gc_pause_stable_test.go:231`; `bench/soak/latency_p99_stable_test.go:271`;
`bench/scenarios/streaming_ingest_test.go:94`;
`cypher/delete_scaling_test.go:405`; `cypher/delete_scaling_soak_test.go:38,51`;
`cypher/security_mixed_dos_soak_test.go:158`; `graph/csr/csr_megabuild_test.go:49`;
`graph/index/label/security_store_label_maphint_test.go:131`;
`internal/stress/ctxcancel_{bfs,brandes,dijkstra,leiden}_test.go`;
`search/dijkstra_ctx_cancel_test.go:135`; `store/bulk/throughput_1m_test.go`;
`store/checkpoint/soak_60s_test.go`; `bench/soak/no_growth_test.go`;
`bench/soak/bolt_4h_test.go`; `bench/expandinto/exponent_soak_test.go`;
`bench/mvccwrite/frontier_staleness_test.go:156` (helper reachable only from a
`RequireSoak` test).

These are the precedent the policy should extend, not exceptions to it.

## Borderline — a deadline whose expiry fails the test

**No test was executed for this audit, so no margin below is measured.** Each
margin is the budget divided by a reference time *named inside the test itself* —
a paired context deadline, an injected sleep, or the author's own recorded
observation. Where the test names no reference time, that is stated.

Thin (budget under ~2 s, or derived from a short measured quantity):

| file:line | budget | margin basis |
|---|---|---|
| cypher/exec/exec_test.go:348 | 100 ms | **no reference time in the test**; thinnest budget found |
| cypher/exec/label_constraints_zerocost_test.go:172 | 250 ms | expiry flips a boolean four callers assert on (:190,:205,:216,:229) |
| cypher/exec/parallel_aggregate_test.go:113 | 500 ms | cancel at 2 ms; return time not recorded |
| cypher/exec/parallel_aggregate_scan_test.go:474 | 500 ms | as above |
| internal/clock/clock_test.go:39,50,61 | 1 s | real timer armed ~10 ms → ~100x |
| graph/generation/generation_test.go:89 | 1 s | preceding branch proves the publish is parked at 50 ms → ~20x |
| cypher/api_internal_extra_test.go:406 | 2 s | 100 WAL `AddNode` calls; per-call cost not recorded |
| graph/lpg/reentrancy_test.go:56 | 2 s | no reference time |
| store/bulk/ctx_cancel_test.go:78,93 | 2 s | no reference time |
| store/txn/mutations_test.go:477,549,775,807 | 2 s | no reference time |
| bolt/server/server_metrics_test.go (9 sites) | 2–3 s | `waitFor` polls every 2 ms |
| bolt/server/streaming_backpressure_test.go:234 | 90 s | the code's own comment records 1–2 s → **45–90x**, the only margin stated by the source |

Roughly **90** further watchdogs use 5 s–60 s budgets on operations completing in
microseconds to milliseconds. By inspection these are ≥10³x margins and read as
genuine hang detectors rather than performance assertions. They were not measured.

## What this audit could not determine

- **Whether the five `internal/sim` oracles have ever fired.** They live in
  non-test files and reach the gate as `Violation`s that short-layer tests turn
  into `t.Errorf`. Their 30 s / 5 s bounds look safe, but they are seed-driven and
  the DST was not run, so no margin can be stated. They are listed because the
  assertion *is* a wall-clock comparison, not because there is evidence of flakiness.
- **Every borderline margin**, for the reason given above.
- **`bench/comparison/{cpu,memory,concurrency}_test.go`** measure peer databases and
  report; none asserts on time. `TestMemoryFootprint` has no skip guard but iterates
  an empty target list by default — not verified by running it.

Deliberately excluded, in case the policy wants to revisit them: allocation ratios
(`bench/cyclicjoin:232`, `graph/lpg/bulkload_bracket:256`), fitted exponents
(`bench/cyclicjoin:218`), fake-clock durations (`cypher/procs/stats_refresh_limiter:39`,
`internal/clock`, `internal/sim/bolt_tx_registry`), positivity-only checks that load
can only make safer (`cypher/explain/profile_test.go:91`,
`bolt/server/tx_introspection:84`, `internal/testfs:168`,
`store/wal/fs_fault_fsync_delay:57`), the per-read torn-read rate in
`internal/anomaly/perturb_test.go:102`, and all `Benchmark*` throughput ceilings.

## Measured: what a serial timing phase would cost

The decisive input to the policy is not whether a serial phase is *correct* but
whether it is *affordable*. Measured on this tree (Apple M4, `1faafff5`), running
**only** the timing gates listed above, serially (`-p 1`), with `-race`:

| package | wall | result |
|---|---|---|
| bench/cyclicjoin | 39 s | ok |
| bench/soak | 11 s | ok |
| bench/mvccwrite | 10 s | ok |
| cypher | 17 s | ok |
| bolt/server | 7 s | ok |
| store/txn | 5 s | ok |
| store/checkpoint | 4 s | ok |
| graph/adjlist | 2 s | ok |
| graph/mvcc | 2 s | ok |
| search/flow | 2 s | ok |
| store/wal | 2 s | ok |
| **total** | **≈101 s** | **all green** |

**Every gate passes when run serially**, which is the audit's central claim
demonstrated rather than asserted: these tests are not broken, their measurement
precondition is absent under `make ci`. And ~101 s is cheap against the parallel
`make ci` it would join — the `cypher` package alone takes 193 s under `-race`.

Caveat on what this does and does not show: the runs used `-run` filters, so within
each package the timing gate had the machine largely to itself, which is exactly
the condition being proposed. It does **not** show that these gates would pass if
the whole package ran serially alongside them, and it does not measure the
build time a separate phase would re-pay if the phase used different build flags.
