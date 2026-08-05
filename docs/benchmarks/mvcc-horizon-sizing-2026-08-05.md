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

## Implemented, and what the change cost (AC 3, 4, 5, 6)

`horizonSlots` is now 1024. The capacity is pinned in **both** directions by
`TestHorizon_CapacityIsPinned` and `TestHorizon_ExhaustionCliffAtCapacity`, which
assert against a literal rather than against `horizonSlots` — asserting against the
constant would make the test tautological, since shrinking the constant would shrink
the expectation with it. Verified to fail at 512 ("capacity is smaller than the design
states") and at 2048 ("capacity is LARGER than the design states"), so a silent
regression in either direction is caught.

**No regression attributable to the change (AC 4).** `BenchmarkWriteScaling`'s
store-less arm, 64 slots against 1024, `benchstat before= after=` (n=5 before, n=6
after):

| writers | before | after | change |
|---|---|---|---|
| 1 | 339.7k commits/s | 328.5k | −3.29% (p=0.004) |
| 8 | 701.2k | 728.6k | +3.90% (p=0.004) |
| 32 | 751.8k | 755.7k | ~ (p=0.792) |
| geomean | 597.0k | 610.7k | **+2.30%** |

The movement is ±3.9% and it goes in **opposite directions** — worse at one writer,
better at eight, flat at 32 — with a favourable geomean. That pattern is layout noise,
not a cost: a systematic cost from a larger structure would be monotone in the same
direction at every point. The mechanism agrees, since the only thing that grew is one
128 KiB allocation at `Graph` construction, which cannot affect steady-state
per-commit cost. `bench/mvccwrite`'s gates — including the WAL scaling gate and the
fsync-coalescing gate — are green.

**The suspended state is now observable (AC 5).** `publishVacuumMetrics` publishes
`lpg.mvcc.horizon.unregistered_readers`, `...active_readers` and `...capacity` on
every vacuum pass, and `mvcc.HorizonCapacity` is exported so the bound can be read
without the source. The operator guidance — alert on any non-zero unregistered count,
and why the other vacuum gauges cannot reveal this state — is in
`docs/isolation-design.md`, which also carries the table above (AC 6).
