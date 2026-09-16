# Is the sharded cache's single-core cost false sharing?

rmp #2852, follow-up question. **Hypothesis: REFUTED.** The +48.6% single-core
cost of the 32-key warm lookup is not false sharing and is not removable by
padding. It is the LRU list splice, and the measurement that shows it also shows
that the padding already in the shipped code is doing its job — at concurrency,
where false sharing actually lives.

## The hypothesis

> At 16 shards the mutexes and LRU heads sit close enough to share cache lines,
> so a single core keeps bouncing the same lines; at 256 they are spread far
> enough apart that they do not. If that is right, the +51.54% is a layout
> defect, removable by padding each shard to a cache line.

## 1. The shards are already padded to a cache line — measured

`planCacheShard` has carried `_ [planCacheShardPadBytes]byte` since the sharding
landed. Measured on the **heap-allocated** array of a production-shaped cache
(`falsesharing.layout.txt`), not on a stack slice:

| shards | `unsafe.Sizeof` | stride | base mod 64 | two adjacent hot blocks in one 64 B line? |
|---:|---:|---:|---:|---|
| 16 | 128 | 128 | 8 | **no** |
| 256 | 128 | 128 | 0 | **no** |

Each shard's hot block — `ll`, `by`, `cap`, `mu` — is 32 bytes at a 128-byte
stride. Even at 16 shards, where the array base is 8 bytes off a line boundary,
8+32 = 40 ≤ 64, so a hot block never straddles a line and two adjacent shards
are always two lines apart. **No two shards can share a cache line in the
shipped build**, which is the condition the hypothesis requires.

## 2. Padding makes no difference at one core — measured

Three builds differing **only** in `planCacheShardPadBytes`, interleaved, 10 reps,
`-cpu=1,10`, no `-race`, no tags. Base is the **unpadded** build (32-byte shards,
four to a 128-byte line — the maximum false sharing the structure admits).

| benchmark | pad000 (32 B) | pad096 (128 B, shipped) | pad224 (256 B) |
|---|---:|---:|---:|
| MultiKey, **`-cpu=1`** | 30.90n ± 3% | 31.49n ± 2% — **~ (p=0.142)** | 30.95n ± 3% — **~ (p=1.000)** |
| MultiKey, `-cpu=10` | 51.38n ± 11% | 44.64n ± 8% — **−13.12% (p=0.004)** | 44.78n ± 6% — −12.85% (p=0.001) |
| quotefree (1 key), `-cpu=10` | 95.94n ± 9% | 95.52n ± 2% — ~ (p=0.436) | 96.02n ± 1% — ~ (p=0.724) |

At **one core, padding changes nothing** (p=0.142 and p=1.000): the unpadded
build, which has four shards to a line, is statistically indistinguishable from
the shipped one and from one spread twice as far. That is what the hypothesis
predicts it would *not* do.

It is also what theory requires. **False sharing is a multi-core coherence
effect** — two cores writing different variables in one line, invalidating each
other. A single core cannot false-share with itself, so a cost measured at
`-cpu=1` cannot be false sharing whatever the layout.

## 3. The padding IS earning its keep — at concurrency

The same table answers the other half. At `-cpu=10` on the 32-key workload,
padding buys **−13.12% (p=0.004)**: 51.38n unpadded against 44.64n shipped. That
is real false sharing, and it is already removed.

Spreading further buys nothing: 256-byte shards measure 44.78n against the
shipped 44.64n (p within noise of each other). **The shipped 128-byte padding is
already at the optimum**, so there is nothing to land.

## 4. What the single-core cost actually is — attributed

A diagnostic arm, identical to the shipped build except that `get` does **not**
call `MoveToFront`. It isolates the LRU splice and nothing else. **It is not a
candidate and was never going to be landed** — the user explicitly rejected
approximate recency — it exists only to attribute.

| benchmark | shipped | `MoveToFront` removed | delta |
|---|---:|---:|---:|
| MultiKey, `-cpu=1` | 31.49n ± 2% | **21.61n ± 1%** | **−31.36% (p=0.000)** |
| MultiKey, `-cpu=10` | 44.64n ± 8% | 22.90n ± 15% | −48.70% (p=0.000) |
| quotefree (1 key), `-cpu=1` | 17.84n | 17.43n | −2.33% |
| quotefree (1 key), `-cpu=10` | 95.52n | 96.27n | ~ (p=0.128) |

**Removing the splice returns the single-core 32-key cost to 21.61n against the
unsharded baseline's 21.19n — no significant difference (p=0.093).** The entire
single-core cost of sharding is the splice. Nothing is left over for layout,
hashing or shard indexing to explain.

Why the splice got dearer: unsharded, the 32 nodes form **one** list, and cyclic
access means an element's list neighbours are the elements touched immediately
before and after it — cache-hot, as is the single always-hot list root. Split
into 16 lists of two, an element's neighbour is the *other* key of its shard,
last touched 16 accesses ago, and the root is one of 16 scattered across 2 KB.
The same three pointer writes now land on cold lines. That is **true sharing and
locality**, the direct consequence of splitting one LRU list into N — the
per-shard-LRU trade that was accepted — not a defect sitting beside it.

## 5. Why 256 shards looked cheaper than 16

The inversion that prompted the question has the same cause. `container/list`'s
`MoveToFront` returns immediately when the element is already at the front. At
256 shards almost every key owns its shard, so the splice **does not happen**:
`TestZZShardSpread` measured 29 of 32 keys alone in a shard at 256, against every
shard holding about two at 16. So 256 shards is not cheaper because its shards
are further apart; it is cheaper because it mostly skips the work. That is the
same mechanism that makes 256 shards wreck the hit rate under load, and it is why
the count was chosen on the hit-rate axis.

## Verdict

- **Refuted.** Padding is not the answer at one core; it changes nothing there
  (p=0.142), and false sharing cannot arise on one core in any case.
- **Nothing to land.** The padding the hypothesis proposes is already in the
  shipped code, measured at its optimum (−13.12% at 10 cores; a further doubling
  buys nothing).
- **The +48.6% single-core cost is the accepted per-shard-LRU trade**, attributed
  to the splice by a single-variable experiment, not a removable layout defect.

One observation recorded and not acted on: on the one-key benchmark at `-cpu=1`,
the padded build measured 6.13% faster than the unpadded one (17.84n vs 19.01n,
p=0.000). With one key there is one shard and nothing to share with, so this is
a code- or data-layout artefact of these two binaries rather than a mechanism. It
does not bear on the verdict, and this project has been wrong before about deltas
of that size that did not survive a rebuild.

## Reproduction

Five arms built from one tree, differing only in `planCacheShardPadBytes` (0, 96,
224) or in the one-line diagnostic removal of `MoveToFront`; the fifth is the
untouched single-mutex binary. Interleaved arm-by-arm, 10 reps,
`-test.benchtime=300ms -test.cpu=1,10 -test.count=1`, **no `-race`, no build
tags**, on the `harness-warm.go.txt` benchmarks.

Raw: `fs.onemutex.txt`, `fs.pad000.txt`, `fs.pad096.txt`, `fs.pad224.txt`,
`fs.nomtf.txt`. Collated: `falsesharing.benchstat.txt` (all five),
`falsesharing.padonly.benchstat.txt` (padding alone),
`falsesharing.nomtf.benchstat.txt` (the diagnostic). Layout:
`falsesharing.layout.txt`.

The source was restored byte-identically after each arm was built (`cmp` against
a saved copy), and no throwaway file remains in `cypher/`.

## Environment

| | |
|---|---|
| Go | `go1.27.1 darwin/arm64` |
| host | Apple M4, 10 cores, 32 GiB, Darwin 25.6.0 arm64 |
| build flags | none — no `-race`, no build tags, on every arm |
| loadavg before | `1.66 2.45 2.63` |
| loadavg after | `5.14 3.56 3.05` |

The "after" figure includes the benchmark's own 10-core load. All five arms were
interleaved rep by rep, so each saw the same background.
