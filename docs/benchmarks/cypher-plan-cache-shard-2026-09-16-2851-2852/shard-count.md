# Choosing the plan-cache shard count

rmp **#2852**, sprint 362. The shard count is chosen on a measured ladder, not
by analogy with any other sharded structure in this module.

The headline is that **one ladder is not enough**. Raising the shard count buys
concurrency and costs hit rate, and the two axes point in opposite directions.
The chosen value is the minimum of their **sum**, not the best point on either.

## Axis 1 — contention

`Engine.parseAndAnalyse` on a warm cache, 32 distinct keys, `b.RunParallel` at
`-cpu=10`, 10-core Apple M4, no `-race`, no tags, `-benchtime=300ms`,
`-count=1` × 10 interleaved reps × 2 arms.

| shards | sec/op | vs 1 shard | per-shard capacity (of 1024) |
|---:|---:|---:|---:|
| 1 | 126.9n ± 1% | — | 1024 |
| 2 | 98.11n ± 1% | −22.7% | 512 |
| 4 | 77.16n ± 2% | −39.2% | 256 |
| 8 | 57.91n ± 5% | −54.4% | 128 |
| **16** | **42.28n ± 4%** | **−66.7%** | **64** |
| 32 | 33.12n ± 6% | −73.9% | 32 |
| 64 | 26.85n ± 8% | −78.8% | 16 |
| 128 | 21.57n ± 12% | −83.0% | 8 |
| 256 | 19.12n ± 9% | −84.9% | 4 |
| 512 | 18.21n ± 11% | −85.7% | 2 |
| 1024 | 67.21n ± 76% | — | 1 |

**There is no knee.** The curve improves monotonically to 512, so this axis
alone cannot choose a value — it would choose 512.

The 1024-shard rung is not noise, it is **bimodal**: 9 of 20 reps report
15.4–16.9 ns and 11 report 117–245 ns. At 1024 shards each shard holds exactly
one entry, so any two of the 32 keys that collide evict each other on every
access and the benchmark measures recompiles. It is the first sighting of axis 2.

**Control.** The same ladder with **one** key is flat at 95.3–98.2 ns across
every rung (± 1%). One key is one shard whatever the shard count, so a ladder
that appeared to help here would be measuring something other than the shard
count. It does not. This also settles, before any A/B, that **sharding cannot
help a single hot key** — see `ab.md`.

**Noise floor.** The same binary against itself, interleaved, over all 18 rungs
of the first ladder: every rung `~` (p ≥ 0.052, n=10), geomean +0.43%. Steps
larger than a rung's own spread are therefore real.

## Axis 2 — capacity quantisation

Dividing one bound of 1024 into N fixed per-shard bounds means an uneven hash
can overfill one shard while its neighbours sit part-empty. An overfilled shard
**thrashes**: under a cyclic scan — LRU's worst-case pattern — every key in it
misses, every time.

Measured directly on the cache alone (no engine, no parser, so the number is the
eviction effect and nothing else): steady-state hit rate of a cyclic scan over a
working set at a given fraction of capacity, 16 independent caches per cell so
16 independent hash seeds.

| shards | per-shard cap | 10% load | 25% load | 50% load | 75% load | 100% load |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 1024 | 100.00 | 100.00 | 100.00 | 100.00 | 100.00 |
| 8 | 128 | 100.00 | 100.00 | 100.00 | 100.00 | 48.69 |
| **16** | **64** | **100.00** | **100.00** | **100.00** | **99.47** | **52.55** |
| 32 | 32 | 100.00 | 100.00 | 100.00 | 95.68 | 48.13 |
| 64 | 16 | 100.00 | 100.00 | 100.00 | 87.19 | 46.07 |
| 128 | 8 | 100.00 | 100.00 | 95.91 | 78.52 | 45.39 |
| 256 | 4 | 100.00 | 99.15 | 85.10 | 66.01 | 43.82 |
| 512 | 2 | 99.08 | 92.80 | 76.77 | 57.80 | 40.30 |
| 1024 | 1 | 92.71 | 80.13 | 61.68 | 49.48 | 36.58 |

Two things to read from it:

1. **At 100% load every count above 1 roughly halves the hit rate.** A working
   set exactly equal to the capacity fitted the single global LRU exactly and
   fits no sharded one. This is the accepted per-shard-eviction consequence,
   priced.
2. **Below 100% the damage is entirely a function of the per-shard capacity.**
   A shard with 64 slots absorbs the binomial spread of a 75%-loaded cache
   almost perfectly (99.47%); one with 4 slots does not (66.01%).

## Pricing the two axes together

A miss costs a full recompile. Measured on this host from the 1024-key rung:
1012 ns/op at 46.07% hit rate against 26.3 ns/op on the hit path gives a marginal
miss cost of **≈ 1.8 µs**. One lost point of hit rate therefore costs ≈ 18 ns per
lookup — two to three times what a doubling of the shard count saves anywhere on
axis 1.

Expected cost per lookup, `contention_ns + (1 − hit) × 1800 ns`:

| shards | 10% load | 25% load | 50% load | 75% load | mean (10–75%) |
|---:|---:|---:|---:|---:|---:|
| 8 | 57.9 | 57.9 | 57.9 | 57.9 | 57.9 |
| **16** | **42.3** | **42.3** | **42.3** | **51.8** | **44.7** |
| 32 | 33.1 | 33.1 | 33.1 | 110.9 | 52.6 |
| 64 | 26.9 | 26.9 | 26.9 | 257.5 | 84.6 |
| 128 | 21.6 | 21.6 | 95.2 | 408.2 | 136.7 |
| 256 | 19.1 | 34.4 | 287.3 | 630.9 | 242.9 |

**16 is the minimum.** It takes 78% of the whole contention reduction available
(126.9 → 42.3 ns, against a floor of 18.2 ns) while costing at most half a point
of hit rate anywhere from an empty cache to three-quarters full, and nothing at
all at or below half full.

The 100% column is excluded from the mean because every count above 1 is
pathological there and no choice of count rescues it.

## What the choice forfeits, stated plainly

The task's acceptance asks that warm-lookup throughput **rise** from 1 to 10
cores. On the 32-key variant that needs `sec/op` at `-cpu=10` to fall below the
single-core 20.96 ns, which **only 256 shards and above achieve** (19.12 n).
16 shards reaches 42.28 n — a 3.0× improvement on the 126.9 n single-shard
figure, but still 2.0× the single-core cost.

Buying that criterion costs, at 256 shards, 14.9 points of hit rate at half load
and 33 points at three-quarters — ≈ 268 ns and ≈ 595 ns per lookup — to save
23 ns of contention. It is a 12× to 26× net loss, so the criterion is reported
as **missed at the chosen count and met only at a count that is worse overall**.
Both are measured; `ab.md` carries the A/B for 16 **and** 256 side by side so the
decision can be revisited against the numbers rather than re-derived.

Changing the decision costs one line: `planCacheShards` in `cypher/plan_cache.go`.

## Memory

At 16 shards a cache costs 16 × 128 B of shard structs plus 16 small maps and
lists — on the order of 4 KiB per `Engine`, against roughly 1 KiB for the single
structure it replaces. At 256 it would be roughly 60 KiB.

## Reproduction

```sh
# contention ladder (arms interleaved, 10 reps)
go test -c -o <scratch>/cypher.test ./cypher/
( cd cypher && <scratch>/cypher.test -test.run '^$' \
    -test.bench '^BenchmarkShardLadder(MultiKey|ManyKeys|OneKey)$' \
    -test.benchmem -test.benchtime=200ms -test.cpu=10 -test.count=1 )

# quantisation table
go test -run '^TestZZShardQuantisation$' -count=1 -v ./cypher/
```

Harness archived here as `harness-shard-ladder.go.txt`. Raw series:
`ladder.A.txt`, `ladder.B.txt`, `ladder2.A.txt`, `ladder2.B.txt`;
collated: `ladder.benchstat.txt`, `ladder2.benchstat.txt`; noise floor:
`ladder.noisefloor.benchstat.txt`, `ladder2.noisefloor.benchstat.txt`;
quantisation: `quantisation.txt`.

## Environment

| | |
|---|---|
| commit (code under test) | sharded `cypher/plan_cache.go` on `1630e9f6` |
| Go | `go1.27.1 darwin/arm64` |
| host | Apple M4, 10 cores, 32 GiB, Darwin 25.6.0 arm64 |
| build flags | none — no `-race`, no build tags |
| loadavg, ladder 1 | before `2.24 1.83 2.20`, after `6.80 4.01 3.03` |
| loadavg, ladder 2 | before `2.91 3.38 2.88`, after `5.86 5.06 3.77` |

The "after" figures include the benchmark's own 10-core load and are not a
statement about background noise; the "before" figures sit inside this host's
quiet envelope (median 2.19, max 3.44). Arms were interleaved rep by rep, so
both saw the same background whatever it was.
