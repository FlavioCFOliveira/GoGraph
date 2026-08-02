# The contiguous commit frontier — what it costs

**Date:** 2026-08-02
**Head:** `a4681e14` + the rmp #2298 change
**Machine:** Apple M4, 10 cores (4 performance + 6 efficiency), darwin/arm64, go1.26.5
**Task:** rmp #2298, sprint 334, acceptance criterion 4
**Code:** `graph/mvcc/commitlog.go`, benchmarks in `graph/mvcc/commitlog_bench_test.go`

`Clock.ReadTS` no longer returns the highest **published** commit timestamp. It
returns the highest timestamp below which **nothing is still in flight** — the
contiguous frontier — maintained by a Memgraph-style commit log on the publish
path. This records what that costs.

Both arms run in **one process**: the single-watermark implementation survives in
the benchmark file as `legacyPublish`. A back-to-back A/B across two builds on
this machine has manufactured phantom regressions from a byte-identical control
before, which is why `graph/lpg` already carries `EnableMVCC`/`DisableMVCC` for
the same reason.

```
go test -run='^$' -bench='BenchmarkReadTS|BenchmarkVisible|BenchmarkPublish' -benchmem -count=10 ./graph/mvcc/
```

---

## 1. The read path does not move

This was the design constraint, and it is the one the acceptance criterion names.

| benchmark | sec/op | B/op | allocs/op |
|---|---:|---:|---:|
| `ReadTS` | **0.2744 n ± 1%** | 0 | 0 |
| `Visible/committed/visible` | 0.5316 n ± 1% | 0 | 0 |
| `Visible/committed/invisible` | 0.5317 n ± 1% | 0 | 0 |
| `Visible/own-write` | 0.5282 n ± 4% | 0 | 0 |
| `Visible/other-in-flight` | 0.4021 n ± 0% | 0 | 0 |

`ReadTS` is one atomic load and `Visible` is one comparison — **the same code as
before the change**, byte for byte. Only the value stored into `visible`, and who
stores it, changed. There is therefore no read-path regression to justify, and
the numbers above are the evidence rather than the argument.

This is the whole reason Memgraph's shape was chosen over PostgreSQL's. An `xip`
list would have put an `xmin`/`xmax` pair plus an array search on the
`Visible` line — the test that runs once per version-chain node, on every
versioned read.

## 2. The cost went to the publish path, once per commit

| benchmark | sec/op | allocs/op |
|---|---:|---:|
| `Publish/legacy/in-order` (control) | 5.186 n ± 1% | 0 |
| `Publish/frontier/in-order` | **6.119 n ± 0%** | 0 |
| `Publish/frontier/out-of-order-window-8` | 5.115 n ± 0% | 0 |

**+0.93 ns per commit uncontended, +18.0%** on a single-threaded publish. Zero
allocations in every arm — the commit log allocates one 512-byte block per 4096
commits and returns it as soon as it fills.

The out-of-order arm being *faster* than the in-order one is not noise: only one
publication in eight advances the frontier, and the other seven set a bit and
return without walking the bitmap at all.

## 3. Under contention the new path is FASTER

| benchmark | sec/op |
|---|---:|
| `PublishConcurrent/legacy` (control) | 94.98 n ± 1% |
| `PublishConcurrent/frontier` | **66.65 n ± 1%** |

**1.42× faster, −29.8%,** with `RunParallel` on ten cores.

This inverts the single-threaded result and it is worth stating plainly, because
"replaced a lock-free CAS with a mutex" reads like a regression. Under real
contention the CAS loop is the slower structure: every publisher that loses the
race re-loads and retries, and with ten publishers the retry traffic dominates.
The mutex queues them instead, and each critical section is a bit-set plus, for
one publisher in N, a short bitmap walk.

**The contended regime is the one this sprint is moving towards.** The
uncontended +18% is paid today, where a global barrier means there is only ever
one publisher; the −30% arrives exactly when the barrier comes out.

## 4. End-to-end: invisible

+0.93 ns on a commit that costs 2906 ns is +0.03%, so the engine-level number
should not move at all. Measured against the baseline recorded the same day in
[`mvcc-write-scaling-2026-08-02.md`](mvcc-write-scaling-2026-08-02.md),
benchstat over `-count=10` on both sides:

```
go test -run='^$' -bench=BenchmarkWriteScaling/mem -benchtime=200000x -benchmem -count=10 ./bench/mvccwrite/
```

| writers | before | after | delta |
|--------:|-------:|------:|-------|
| 1 | 344.1k ± 1% | 348.1k ± 2% | ~ (p=0.063) |
| 2 | 290.0k ± 1% | 288.2k ± 1% | −0.61% (p=0.019) |
| 4 | 291.5k ± 2% | 289.8k ± 1% | ~ (p=0.353) |
| 8 | 287.1k ± 2% | 286.5k ± 1% | ~ (p=0.481) |
| 16 | 284.8k ± 2% | 286.8k ± 1% | ~ (p=0.218) |
| 32 | 282.4k ± 2% | 282.4k ± 1% | ~ (p=0.971) |
| **geomean** | 295.9k | 296.2k | **+0.08%** |

Five of six arms are statistically indistinguishable and the geomean moves +0.08%
— i.e. not at all. The one flagged row (2 writers, −0.61%, p=0.019) is at the
edge of significance and is not corroborated by its neighbours at 1, 4 or 8
writers; on this machine a byte-identical control has previously produced 22 of
36 flat-by-construction rows as "significant", so a lone −0.6% is not treated as
a finding.

## 5. Verdict

No read-path regression: the read path is unchanged code and measures as such.
The publish path costs +0.93 ns per commit while writers are serialised, which is
invisible end to end (+0.08% geomean, within noise), and becomes a **1.42×
improvement** once writers publish concurrently — the regime the change exists to
make possible.

Memory is bounded structurally rather than by configuration: what the log retains
is the window between the oldest unfinished timestamp and the newest allocated
one, and `MVCCStats.InFlightCommits` reports that window so it can be watched
rather than inferred.
