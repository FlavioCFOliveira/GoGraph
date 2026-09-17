# Sharding the plan cache — the A/B

rmp **#2852**, sprint 362. Single-variable A/B on the warm statement-preparation
path, interleaved, with the noise floor measured in the same series.

## Arms

| arm | what it is | n |
|---|---|---:|
| `onemutex` | `cypher/plan_cache.go` exactly as it stood at HEAD `1630e9f6` — **one** `sync.Mutex` for the whole cache. Compiled **before** the file was touched. | 20 |
| `shard16` | the sharded cache, `planCacheShards = 16` (the shipped default) | 10 |
| `shard256` | the same code with `planCacheShards = 256` — the count needed to satisfy the throughput acceptance criterion, measured so the trade-off can be seen rather than argued | 10 |

The three binaries differ in `cypher/plan_cache.go` and in nothing else. Both
carry the **same** warm harness, restored verbatim from the archived #2846 spike,
so the benchmark names and the workload are identical.

**Interleaving.** Every rep ran `onemutex`, `shard16`, `shard256`, `onemutex`
again. The two `onemutex` series give the noise floor; their pool is the A arm.
Never all of one arm and then all of the other.

## Noise floor

`onemutex` against itself, interleaved, `-cpu=1,2,4,8,10`, n=10 each:
**every row `~`** (smallest p = 0.143), geomean **−0.04%**. Raw:
`ab.noisefloor.benchstat.txt`. Anything above about 2% is therefore real.

## Result — sec/op

| benchmark | `onemutex` | `shard16` | vs base | `shard256` | vs base |
|---|---:|---:|---:|---:|---:|
| Parallel/quotefree (1 key) | 15.49n ± 1% | 18.34n ± 1% | **+18.44%** | 18.49n ± 1% | +19.41% |
| Parallel/quotefree-2 | 40.44n ± 1% | 36.91n ± 34% | −8.71% | 34.86n ± 5% | −13.79% |
| Parallel/quotefree-4 | 86.47n ± 2% | 80.22n ± 3% | −7.22% | 68.33n ± 7% | −20.98% |
| Parallel/quotefree-8 | 86.15n ± 2% | 92.64n ± 2% | **+7.54%** | 95.83n ± 1% | +11.24% |
| Parallel/quotefree-10 | 89.38n ± 1% | 96.64n ± 2% | **+8.12%** | 97.67n ± 1% | +9.28% |
| Parallel/literal | 334.1n ± 2% | 337.4n ± 2% | +0.97% | 330.9n ± 2% | ~ |
| Parallel/literal-2 | 172.1n ± 1% | 177.0n ± 1% | +2.88% | 176.3n ± 2% | +2.47% |
| Parallel/literal-4 | 106.8n ± 1% | 106.1n ± 1% | ~ | 108.4n ± 3% | ~ |
| Parallel/literal-8 | 160.3n ± 1% | 150.2n ± 3% | −6.30% | 145.1n ± 2% | −9.51% |
| Parallel/literal-10 | 175.9n ± 1% | 158.3n ± 3% | −10.01% | 152.5n ± 1% | −13.30% |
| MultiKey (32 keys) | 21.09n ± 1% | 31.96n ± 3% | **+51.54%** | 21.87n ± 3% | +3.67% |
| MultiKey-2 | 54.15n ± 1% | 27.66n ± 3% | −48.91% | 16.12n ± 7% | −70.23% |
| MultiKey-4 | 121.30n ± 1% | 24.85n ± 11% | −79.52% | 9.68n ± 72% | −92.02% |
| MultiKey-8 | 113.45n ± 1% | 37.85n ± 8% | −66.64% | 18.57n ± 11% | −83.63% |
| MultiKey-10 | 119.10n ± 1% | 42.42n ± 5% | **−64.38%** | 19.33n ± 16% | **−83.77%** |
| geomean | 87.51n | 67.22n | **−23.19%** | 52.87n | −39.58% |

`B/op` and `allocs/op` are **identical in every row of every arm** (`~`,
"all samples are equal"): the warm hit path allocates nothing before and nothing
after. Sharding adds no allocation.

The `shard256` MultiKey rows carry large spreads (±72% at `-cpu=4`) because at
256 shards a 1024-entry cache holds 4 entries per shard, so a hash collision
between two of the 32 keys turns hits into recompiles. It is a lottery run afresh
in each process, not measurement noise — `shard-count.md` documents the mechanism.

## Does throughput rise from 1 to 10 cores?

The acceptance criterion, stated as throughput:

| variant | arm | 1 core | 10 cores | verdict |
|---|---|---:|---:|---|
| 32 keys | `onemutex` | 47.4 M/s | 8.4 M/s | falls 5.6× |
| 32 keys | `shard16` | 31.3 M/s | 23.6 M/s | **still falls**, 1.3× |
| 32 keys | `shard256` | 45.7 M/s | 51.7 M/s | **rises**, 1.13× |
| 1 key | `onemutex` | 64.6 M/s | 11.2 M/s | falls 5.8× |
| 1 key | `shard16` | 54.5 M/s | 10.3 M/s | **still falls**, 5.3× |
| 1 key | `shard256` | 54.1 M/s | 10.2 M/s | **still falls**, 5.3× |

**The criterion is missed.**

- On the **one-key** variant it is missed at **every** shard count, and it is
  missed *structurally*, not for want of tuning: one key hashes to one shard, so
  every caller queues on the same mutex however many shards exist. The ladder's
  one-key control proves it independently — flat at 95.3–98.2 ns across
  1…1024 shards. Sharding makes this variant slightly **worse** (+8.12% at
  10 cores) because the hash is added work that buys nothing when there is one
  shard in play.
- On the **32-key** variant it is met only from 256 shards, and `shard-count.md`
  shows 256 shards costs 14.9 points of hit rate at half load and 34.0 at
  three-quarters — 268 ns and 612 ns per lookup against the 23 ns of contention
  it saves. The count was therefore **not** chosen to satisfy this criterion.

## Against the stated ceiling

The task set the ceiling at **−77.1 ns/op at 10 cores (−83.7%)** — the warm path
returning to its single-core cost.

| path | base at 10 cores | after | delta | ceiling |
|---|---:|---:|---:|---|
| 1 key | 89.38n | 96.64n (`shard16`) | **+7.26n (+8.12%)** | **missed by 84.4 ns** — and the wrong way |
| 32 keys | 119.10n | 42.42n (`shard16`) | **−76.68n (−64.38%)** | −64.4% of the −83.7% asked |
| 32 keys | 119.10n | 19.33n (`shard256`) | **−99.77n (−83.77%)** | **reached, at a count that costs more than it saves** |

The −83.7% figure was derived from the one-key measurement, which is precisely
the case sharding cannot address. On the workload sharding *can* address it is
reached — at 256 shards — and 64.4% of it is taken at the shipped 16.

## Mutex profile

`-cpu=10`, `-benchtime=3s`, `-mutexprofilefraction=1`, same command on both arms.

Over **both** benchmarks (`prof.*.mutex.top.txt`):

| arm | total mutex delay | `(*planCache).get` |
|---|---:|---:|
| `onemutex` | 54.81 s | 53.42 s — **97.45%** |
| `shard16` | 45.93 s | 44.72 s — **97.38%** |

Over the **32-key** benchmark alone (`prof.*.multikey.mutex.top.txt`):

| arm | total mutex delay | `(*planCache).get` |
|---|---:|---:|
| `onemutex` | 22.27 s | 21.77 s — **97.78%** |
| `shard16` | 13.32 s | 13.17 s — **98.89%** |

**`(*planCache).get` still holds the overwhelming majority of mutex delay, and
naming "the new top contributor" is not possible, because there is no other
contributor to name.** The criterion presumes that relieving `get` promotes some
other lock; the warm path takes **no other lock**, so `get`'s share cannot fall
however well the sharding works. The only other entry in either profile is
`runtime.unlock` at 4.51% — the Go runtime's internal lock, not a GoGraph one.

What did move is the **amount**: −40.2% of mutex delay on the 32-key workload
(22.27 s → 13.32 s) and −16.2% over both. That is a smaller move than the
−64.38% throughput improvement on the same workload, which is the point:
**mutex delay is not a cost function** and was used here only to attribute, never
to price. The throughput number is the finding.

Block profile, `shard16`, 32 keys: `sync.(*Mutex).Lock` 63.81% of 20.93 s, then
`runtime.chanrecv1` 18.23% and `sync.(*WaitGroup).Wait` 17.77% — the latter two
are the benchmark harness's own coordination, not the engine.

## Against the archived #2846 baseline

`ab.vs-spike2846*.benchstat.txt` places the sharded series beside
`docs/benchmarks/cypher-parse-lab-2026-09-16-spike2846/warm.series.txt`, as the
acceptance asks. **It is not a single-variable comparison** and must not be read
as one: that baseline was taken at commit `be5019f4`, before `cb55c3c9` and
`1630e9f6` landed, and both touch this path. The size of the difference is
visible in the control itself — the `literal` shape measures 496.1 ns there and
334.1 ns in the `onemutex` arm here, and 512 B / 12 allocs there against
464 B / 6 allocs here, none of which is this change. `ab.benchstat.txt` is the
comparison that isolates the sharding.

## Does the hit rate move?

No — it is **bit-identical**. The four corpus examples were re-measured on the
sharded build with the same bracketed harness (`planwatch-after.raw.txt`):

| example | before | after |
|---|---|---|
| `19_pattern_query` | 85 hits / 4 misses | 85 / 4 |
| `22_cypher` | 14 / 17 | 14 / 17 |
| `24_social_network_cli` | 0 / 9 | 0 / 9 |
| `25_software_house_api` | 0 / 34 | 0 / 34 |

Expected, and it is evidence rather than a null result: every one of these
workloads recorded **zero evictions**, with working sets of 1–34 distinct keys
against a 1024-entry bound — load factors of 0.1% to 3.3%. The quantisation table
in `shard-count.md` shows that regime is lossless for every shard count up to
128. A workload that evicts is where the eviction contract changes, and none of
these four does.

## The delta survives a rebuild

The three A/B binaries were compiled before the two specialist reviews, so the
shipped code is not the byte-identical binary that produced the table above: the
reviews changed `clear()` to the `clear` builtin, named the constructor's clamp
limit with `min`, and edited comments. None of that is on the measured path, but
a delta that does not survive a rebuild and a fresh code layout is not a delta.

Re-measured with a binary built from the FINAL tree, interleaved against the
same untouched `onemutex` binary, 8 reps, `-cpu=1,10`
(`confirm.benchstat.txt`):

| benchmark | `onemutex` | final tree | vs base | main A/B said |
|---|---:|---:|---:|---:|
| Parallel/quotefree | 15.35n ± 2% | 17.91n ± 2% | +16.72% | +18.44% |
| Parallel/quotefree-10 | 90.68n ± 3% | 95.86n ± 2% | +5.71% | +8.12% |
| Parallel/literal | 332.9n ± 2% | 341.4n ± 2% | +2.52% | +0.97% |
| Parallel/literal-10 | 177.3n ± 2% | 161.3n ± 2% | −9.05% | −10.01% |
| MultiKey | 21.17n ± 3% | 31.07n ± 6% | +46.80% | +51.54% |
| MultiKey-10 | 119.10n ± 1% | 39.97n ± 9% | **−66.44%** | −64.38% |

Every row agrees with the main A/B in sign and magnitude. The headline —
the 32-key warm lookup at 10 cores — reproduces at −66.4% against −64.4%.

## Reproduction

`driver-ab.sh.txt` is the driver, verbatim. Raw series: `ab.base1.txt`,
`ab.base2.txt` (the two A replicas), `ab.shard16.txt`, `ab.shard256.txt`.
Collated: `ab.benchstat.txt`, `ab.noisefloor.benchstat.txt`. Harness:
`harness-warm.go.txt` (restore to `cypher/zz_2852_warm_test.go`).

## Environment

| | |
|---|---|
| base commit | `1630e9f68437caaa162ef7a7d50e4fdaf70c53e5` |
| Go | `go1.27.1 darwin/arm64` |
| host | Apple M4, 10 cores, 32 GiB, Darwin 25.6.0 arm64 |
| build flags | none — no `-race`, no build tags, on every arm |
| `-benchtime` | 300ms, `-count=1` × 10 interleaved reps per arm |
| loadavg before | `2.57 3.59 3.42` |
| loadavg after | `4.59 4.18 3.74` |

The "after" figure includes the benchmark's own 10-core load. Arms were
interleaved rep by rep, so all three saw the same background whatever it was,
and the noise floor measured inside the same series is the check that they did.
