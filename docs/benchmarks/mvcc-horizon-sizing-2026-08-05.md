# Sizing the reclamation horizon — 2026-08-05

rmp #2315 (MVCC C1b), measurement and decision. Head: `09fe41a6`, branch
`sprint-334`. Host: Apple M4, 10 cores, `darwin/arm64`, idle.

```
go test -run '^$' -bench 'BenchmarkHorizonSizing|BenchmarkHorizonReal64' \
    -benchmem -count=10 ./graph/mvcc/
benchstat <output>
```

Harness: `graph/mvcc/horizon_sizing_bench_test.go`.

## Method, and why it is trustworthy

`horizonSlots` is a compile-time constant backing a fixed array, so the real
`Horizon` exists at exactly one size. Measuring 256/1024/4096 by changing that
constant would mean editing production code *before* the decision the measurement
exists to inform. So the table below measures a replica: the same `enter`, `leave` and
`oldest` algorithms over the same 128-byte-strided layout, parameterised by slot count.

A replica is worthless unless it behaves like the original, so two controls back it:

- `TestSizingReplica_MatchesRealHorizon` asserts the replica at 64 slots reproduces the
  real `Horizon`'s observable behaviour — every reader up to capacity registers and the
  watermark tracks the oldest start timestamp, one reader past capacity is unregistered
  and the watermark collapses to zero, and both recover on drain.
- `BenchmarkHorizonReal64` measures the real type so the replica's own 64-slot figure
  can be compared against it.

**Agreement at 64 slots** (real → replica): `oldest` near-empty 21.40n → 26.54n,
near-full 40.82n → 49.02n; `enter-leave` near-empty 1.913n → 2.135n, near-full 114.7n →
100.5n. Same order and same shape throughout; the replica is modestly slower on the
scans (a slice bounds check the fixed array does not pay) and modestly faster on the
near-full probe. Divergence here would invalidate every larger figure, so re-run these
two controls before trusting the table.

## The table

`sec/op`, n=10, ±values are benchstat's.

| slots | `oldest` near-empty | `oldest` near-full | `enter+leave` near-empty | `enter+leave` near-full | memory |
|---|---|---|---|---|---|
| 64 | 26.54n ± 1% | 49.02n ± 0% | 2.135n ± 1% | 100.5n ± 7% | 8 KiB |
| 256 | 109.0n ± 0% | 177.0n ± 1% | 2.140n ± 0% | 297.4n ± 2% | 32 KiB |
| 1024 | 448.1n ± 0% | 699.3n ± 0% | 2.186n ± 0% | 1.085µ ± 1% | 128 KiB |
| 4096 | 2.369µ ± 1% | 2.932µ ± 1% | 2.672n ± 0% | 4.503µ ± 0% | 512 KiB |

Zero allocations in every cell.

Three behaviours, and they differ enough that a single "bigger is worse" argument would
have been wrong:

1. **`oldest` is O(slots) always**, because it scans every slot unconditionally. 64→256
   and 256→1024 are linear (4.1× per 4× slots). **1024→4096 is super-linear at 5.3×**,
   which is the cache cliff the task warned against extrapolating through — 512 KiB of
   distinct cache lines exceeds this machine's L2 per core. Measuring 4096 rather than
   projecting from 64 is what made that visible.
2. **`enter` near-empty is flat**: 2.135n → 2.672n across a 64× increase in capacity.
   The rotating start index means the common case probes one slot regardless of how many
   exist. This is the only one of the three on a hot path.
3. **`enter` near-full is O(slots)**, 100.5n → 4.503µ, a 45× degradation. This is the
   regime the extra slots exist for, and it is the honest cost of them: a system running
   at capacity pays the probe.

## Decision: 1024 slots

Stated with the reason, and the reason is the measurement.

**The scan is no longer on the query path, which is what makes 1024 affordable.** The
task's own cost model says `oldest` runs once per `reclaimIdleEvery=64` queries via
`Graph.ReclaimIdle`. **That is now stale**: #2308 removed `ReclaimIdle` and the
read-path inline sweep with it, and the watermark scan now runs once per background
vacuum pass (`Graph.vacuumPass`) or on an explicit `Graph.ReclaimNow`. A pass sweeps up
to `vacuumRecordsPerPass=65536` records, so 448n–699n of scan per pass is not
measurable against it. The 64→1024 cost that the original model would have charged to
every 64th query is now charged to a background goroutine instead.

**What remains on a hot path is `enter` near-empty, and it moves by 51 picoseconds**
(2.135n → 2.186n, +2.4%) for a 16× capacity increase. That is the per-read-transaction
cost, and it is the number that decides this.

**Why not 4096.** It costs 4× the memory (512 KiB per `Graph`, and every `Graph`
allocates one) and it is where `oldest` turns super-linear — 5.3× for 4× slots, versus
4.1× below it. Paying a measured cache cliff for capacity beyond 1024 concurrent read
transactions is not justified by any workload in this project's benchmarks.

**Why not 256.** It is nearly free, but 256 concurrent read transactions is within reach
of the load levels this module already documents (the release load-test grid goes to
1024 goroutines), so it would leave the cliff reachable in normal operation.

**Why not "leave it at 64".** The cliff is not a slowdown, it is a suspension:
reclamation stops entirely while any reader is unregistered, which by the code's own
words is the one state in which version memory has no bound. The measured cliff is at
exactly 65 concurrent read transactions, and #2307 changed a slot's hold from one
statement (microseconds) to a whole transaction lifetime including idle time. 64 is a
1990s-scale limit for a hold that long.

## Not yet implemented

This document discharges AC 1 (the table, reproducible) and AC 2 (the design chosen,
with the measurement as the reason). The remaining criteria are the implementation and
are deliberately not folded in here:

- change `horizonSlots` to 1024 (AC 3's capacity), and re-run the read benchmarks and
  `bench/mvccwrite`'s gates with benchstat to show no attributable regression (AC 4).
  The `Horizon` grows from 8 KiB to 128 KiB per `Graph`, which is the one thing in this
  change that could plausibly move an unrelated benchmark, so it must be measured and
  not assumed;
- a cliff test at the new capacity that fails if the capacity silently regresses (AC 3);
- surface the suspended state as a metric and document what a non-zero
  `UnregisteredReaders` means and what an operator should do (AC 5);
- update the 'Known bound' section of `docs/isolation-design.md` to the new capacity and
  replace its measured table (AC 6).
