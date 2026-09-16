# Whole-surface performance campaign, 2026-09-16 — attribution, ranking, and backlog reconciliation

Sprint 362 stage 3 (rmp #2859). Stages 1 and 2 are closed (rmp #2856, #2857, #2858,
commit `83da2a92`); this document reads their evidence and ranks it. **No optimisation is
implemented here.**

## Method, and what it can and cannot say

- **Attribution is by ownership, leaf-side.** Every profile is split between GoGraph, the Go
  runtime, the standard library and third-party code by two independent routes —
  `pprof -traces` charging each sample to its leaf frame, and `pprof -top -nodefraction=0`'s
  own flat column — both produced by `scripts/parse_lab_flamegraph.py --buckets=module`.
  **A non-zero delta between the routes is a bucketing error, not a finding.**
- **`-nodefraction=0` throughout.** pprof's default drops every node under 0.5%; on this
  campaign's own precedent that silently hid 4.66 s of a 20.70 s total.
- **`pprof -show=<pkg>` is never used.** It redistributes a dropped node's cost into its
  nearest retained ancestor, so per-package "shares" overlap rather than partition.
- **A profile says where time went in one arm. It cannot say why one arm is slower than
  another.** Every causal claim below is labelled *established* or *hypothesised*, and a
  hypothesised one needs an interleaved A/B that stage 4 owns, not this document.
- **Leaf-side bucketing under-attributes a module cost that terminates in a syscall.** Row 21
  is the worked example: see [What is not module cost](#what-is-not-module-cost).

Host: Apple M4, 10 cores, 32 GiB, `darwin/arm64`, Go 1.27.1. Artefact root:
`…/scratchpad/wholesurface/` — manifests in `out/default/`, `out/elevated/`,
`out2/elevated/`, `out3/`, `out/contention/`, `out/cover-default/`.
`out/default-preorder-artefact/` is superseded and is not quoted.

**What is actually on disk**, counted rather than quoted: across the five live passes there
are **120 `cpu.pprof` files of which 113 are readable** — seven are zero-length, and they are
exactly the rows that were retried in `out2`/`out3` (`out/elevated`'s `04_persistence`,
`14_routing_alternatives`, `17_transactional_log`, `21_typed_recovery`,
`36_mvcc_snapshot_topology`; `out2/elevated`'s `17_transactional_log`; `out/contention`'s
`36_mvcc_snapshot_topology`). There are **113 heap profiles** and **59 each** of mutex, block
and goroutine in the live passes (42 in `out/default`, 17 in `out/contention`). A further 41 of
each sit in the superseded directory, so **the sweep holds 100 mutex profiles on disk, not 60** —
a figure of 60 (42 + 18) circulated with this campaign and is wrong twice: the contention pass
has 17, not 18, and the count omits the 41 superseded ones. *A figure of "143 CPU profiles" also
circulated; the correct live count is 120, of which 113 can be read.*

**Three inventory facts that a reader must have before quoting any artefact:**

* **`out/contention/` contains no row-36 mutex profile.** That pass's
  `36_mvcc_snapshot_topology` produced a **zero-length `cpu.pprof` and no companion files at
  all** — the directory holds only `cpu.pprof` (0 bytes), `probe.txt`, `run.log` and the two
  loadavg files. Row 36's only mutex profiles are `out/default`'s and the superseded copy. Any
  statement of the form "row 36 under contention shows …" for a mutex, heap, block or goroutine
  quantity has no artefact behind it.
* **Every `probe.txt` under `out/default-preorder-artefact/` records paths pointing at
  `out/default/…`** — all 42 of them, with not one naming its own directory. **The numbers in
  those files are that pass's own; only the path strings are wrong.** A reader who trusts the
  paths will attribute one pass's figures to another.
* The superseded directory is a **distinct measurement pass**, not a copy: its row-37 mutex
  profile totals 212 033 µs against `out/default`'s 275 412 µs. It is excluded from the live
  counts and is quoted in this document only where a criterion demands every profile on disk.

**Two allocation quantities must not be conflated.** The manifest's `heap_alloc_b` is the
heap profile's cumulative `alloc_space` — every byte ever allocated. An example's own
`mem.heap_alloc` telemetry is live heap at the end of the run. For
`36_mvcc_snapshot_topology` these are **46 231 300 000 B (43.06 GiB)** and **7.6 GiB**
respectively, and both are correct. This document quotes `alloc_space` throughout and says so.

## Two gaps in the previous pass, closed here

The recorded ownership table (`harvest/ownership.tsv`) covers 12 rows. **The second- and
third-heaviest profiles in the whole sweep were not among them.**

| row | CPU | present in `harvest/ownership.tsv` |
|---|---|---|
| `36_mvcc_snapshot_topology` | 1024.08 s | yes |
| **`11_social_network`** | **221.61 s** | **no** |
| **`20_concurrent_reads`** | **144.20 s** | **no** |
| `26_social_scale_bench` | 121.80 s | yes |

Both splits were computed for this document by the same two-route method, and both come out
**delta 0 — confirmed exact**:

| row | pass | total | GoGraph | runtime | stdlib | third-party | delta |
|---|---|---|---|---|---|---|---|
| `11_social_network` | out/elevated | 221.61 s | **33.54%** (74.32 s) | 66.45% (147.26 s) | 0.01% | 0 | **0** |
| `11_social_network` | out/contention | 222.87 s | 35.64% (79.44 s) | 64.34% (143.40 s) | 0.01% | 0 | **0** |
| `20_concurrent_reads` | out/elevated | 144.19 s | **62.98%** (90.81 s) | 26.59% (38.34 s) | 10.43% (15.04 s) | 0 | **0** |
| `20_concurrent_reads` | out/contention | 146.70 s | 62.24% | 27.43% | 10.33% | 0 | **0** |
| `26_social_scale_bench` | out/default | 121.80 s | 35.01 / 35.11% | 59.67% | 5.21 / 5.10% | roaring 0.11% | **0.13 s** |
| `35_mvcc_mixed_workload` | out/default | 13.31 s | **4.73%** | 91.28% | 2.93% | roaring 1.05% | **0** |

`11_social_network` carries **74.32 s of GoGraph flat CPU** — more than any row in the sweep
except `36`. It produced the campaign's largest single finding, and nothing in the previous
pass looked at it.

## Ranked findings — biggest and simplest first

Rank is expected gain × confidence, against effort × risk. "Reach" is how many independent
passes reproduce the number and how much of the module surface arrives at the site.

---

### R1 — `search.bfsFarthest` allocates a fresh BFS queue per source: 293 GiB, 99.51% of one profile

**Claim.** `bfsFarthest` (`search/diameter.go:240`) creates its frontier queue inside the
function — `queue := []graph.NodeID{src}` at capacity 1 — and grows it by `append` to the
node count, **once per BFS source**. Its `dist` scratch is correctly threaded in by the
caller; only the queue was missed.

**Evidence.**

| measurement | value | route |
|---|---|---|
| `bfsFarthest` alloc_space, flat | **293 214.84 MB of 294 657.45 MB = 99.51%** | `-sample_index=alloc_space -nodefraction=0`, out/elevated |
| reproduced | 293 255.7 MB = **99.49%** of 294 752.1 MB | out/contention |
| `bfsFarthest` alloc_objects, flat | **2 123 952 = 32.58%** | out/elevated |
| example telemetry | `diameter.allocs=2026650`, `diameter.elapsed=28.521899s` of a 29.39 s run | `run.log` |
| profile total | 221.61 s CPU over 29.39 s wall = **753.93%** | `probe.txt`, manifest |
| runtime bucket | **147.26 s = 66.45%**, two-route **delta 0** | `--buckets=module` |
| `runtime.madvise` | 42.25 s (19.07%), **96.95% of it under `runtime.sysUsedOS`** | `-peek` |
| `runtime.asyncPreempt` | 50.63 s (**22.85%**) | flat |
| `runtime.preemptM`→`signalM`→`pthread_kill` | 10.98 s (4.95%) | `-peek` |
| `runtime.usleep` | 20.88 s (9.42%) | flat |
| `bfsFarthest` own CPU, flat | 72.24 s (32.60%), cum 130.54 s (58.91%) | flat |

**Attribution.** The allocation attribution is **established**: 99.51% of a 294 GiB
allocation total, in one function, reproduced in two passes. The causal link from that
allocation rate to the 147.26 s of runtime CPU is **hypothesised** — strongly, because
`madvise` is 96.95% reached through `sysUsedOS`, which runs only when the runtime re-commits
pages it had released, and because the live heap is tiny (`mem.heap_alloc=57.52 MiB`) against
294 GiB allocated in 29.39 s (**9.3 GiB/s**). Proving it needs an interleaved A/B with the
queue reused; that is stage 4.

**Owner.** `search` (`search/diameter.go`).

**Recommended change.** Thread the queue through as caller-owned scratch and reset it with
`queue = queue[:0]`, exactly as `search/centrality.brandesSource` already does with its
`queue, stack []int` parameters. The template is in the sibling package.

**Risk.** Minimal. `bfsFarthest` is unexported, its callers are in the same file, the queue is
neither returned nor retained, and BFS order is unchanged, so results stay bit-identical.

**Validation plan.** `go test ./search/...`; `go test -bench=. -benchmem -count=5
./search/...` compared with `benchstat`; re-run `11_social_network` at
`-users 100000` and compare `diameter.allocs`, `diameter.elapsed` and the runtime bucket
share, interleaved A/B, never all of one arm then all of the other.

**Outcome — the causal hypothesis HELD.** The queue was threaded through as caller-owned
scratch and the example re-run interleaved, `-users 100000`, three rounds per arm, A and B
alternating within each round, every exit status read from inside its own `run.log`
(`run_exit=0`, 6 of 6):

| measurement | before (8affe124) | after | delta |
|---|---|---|---|
| `diameter.allocs` (telemetry) | 2 026 768 / 2 026 659 / 2 026 592 | **433 / 431 / 428** | −99.98% |
| `diameter.elapsed` | 28.92 / 28.80 / 28.79 s | **19.98 / 19.98 / 20.00 s** | −30.8% |
| total profile CPU | 221.18 / 222.31 / 221.02 s | **145.72 / 143.34 / 143.55 s** | −34.1% on round 1, **−34.9%** on the means |
| `go-runtime` bucket | 146.76 s = 66.35% | **6.58 s = 4.52%** | −95.5% |
| bucket two-route delta | 0 | 0 | exact in all six runs |

The allocation rate was the cause of the runtime CPU, not a correlate of it: removing 293 GiB
of per-source queue allocation removed **140.18 s of `go-runtime` CPU** — 96% of that bucket —
and the profile's stack count fell from 361/351/355 to 47/55/63. What remains of the runtime
bucket is 4.52%, in line with the rest of the sweep. The attribution is now **established**,
not hypothesised.

**Backlog.** No existing task. **Filed as rmp #2861.**

---

### R2 — `search`'s per-query working set defeats its own `sync.Pool`: 82.80% of one profile's allocation

**Claim.** Five sites in `search` and `search/centrality` allocate a per-query working set.
`acquireDijkstra` (`search/dijkstra.go:446`) *is* pooled, and its own comment records why the
pool does not hold: *"A pooled state is dropped whenever the GC clears the `sync.Pool`, and
the replacement starts with cap==0"*. At this workload's allocation rate the GC clears the
pool continuously, so the pooled accessor reallocates. `newDistancesCopy`
(`search/dijkstra.go:433`) additionally allocates **and copies three arrays of length
`maxID`** per query — a copy proportional to the graph, not to how much of it the query
reached.

**Evidence** — `20_concurrent_reads`, out/elevated, 34 494.38 MB allocated, 144.19 s CPU:

| site | alloc flat | share |
|---|---|---|
| `centrality.PageRankCtx` (cum) | 17 373.74 MB | **50.37%** |
| └ `centrality.pageRankBuildReverseStructure` | 13 696.80 MB | **39.71%** |
| `search.BFSCtx` | 4 785.97 MB | 13.87% |
| `search.acquireDijkstra` | 4 410.24 MB | 12.79% |
| `search.newDistancesCopy` | 2 947.56 MB | 8.55% |
| `search.(*dijkHeap).push` | 2 719.35 MB | 7.88% |
| **sum of the five** | | **82.80%** |
| `main.topKByRank` (harness) | 1 383.06 MB | 4.01% |

Reproduced in out/contention (34 700.3 MB; `pageRankBuildReverseStructure` 39.83%). CPU side:
GoGraph 62.98%, delta 0.

**Attribution.** Allocation attribution **established**. The split between "the example calls
PageRank too often" and "each call allocates too much" is **not** settled by a profile —
see the reach note below.

**Reach and the harness boundary.** rmp **#2382** records that `examples/20` calls the
one-shot `centrality.PageRank` in its per-worker read loop. The *repetition* is the example's
defect, and the sprint bars optimising the example. What the module owns is measurable and
separable:
- `pageRankBuildReverseStructure`'s **13 696.80 MB** is the reverse transpose rebuilt from an
  immutable CSR snapshot that never changed — the module offers no reusable engine handle a
  caller could hold, so the one-shot API has no correct alternative.
- `runRange`'s **54.92 s cum (38.09%)** of CPU is the power-iteration kernel itself, and is
  module cost per iteration however many times PageRank is invoked. It is ranked separately
  as **R10**.
- The other four sites (`BFSCtx`, `acquireDijkstra`, `newDistancesCopy`, `dijkHeap.push`,
  **43.09%** together) are driven by Dijkstra and BFS queries, not by PageRank, and no
  recorded task names them.

**Recommended change.** Two separable pieces: (a) give the reverse structure a caller-held,
snapshot-keyed reusable form; (b) make `newDistancesCopy` copy only what the query reached,
and pre-size the `dijkHeap` backing from the pooled state rather than growing it by append.

**Risk.** (a) is an API addition and needs a concurrency contract in its godoc. (b) changes a
returned structure's length semantics and must not change any observable result.

**Validation plan.** `go test ./search/... ./search/centrality/...`; `benchstat` over
`-benchmem -count=5`; re-run `20_concurrent_reads` in both passes and compare the allocation
table above.

**Outcome — rmp #2862: what the change delivered, and a criterion that cannot be met.**

Two changes were made to `search/dijkstra.go`: `newDistancesCopy` now sizes its three arrays
from `reachedSpan(found)` instead of from `maxID`, and `acquireDijkstra`'s heap pre-size test
became `cap(st.heap.items) < maxID` instead of `cap(...) == 0`.

*What it delivered*, from the interleaved A/B (5 samples per arm, A bracketing B in every
round, noise floor A against a byte-identical A2: sec/op geomean +0.11%, B/op −0.06%, every
row `~`):

| benchmark | B/op before | B/op after | delta | sec/op delta |
|---|---|---|---|---|
| `SSSP_RepeatedFrom` | 43 739 | **104** | **−99.76%** (p=0.008 n=5) | −31.29% |
| `Dijkstra_RepeatedRevalidate` | 43 826 | **123** | **−99.72%** (p=0.008 n=5) | ~ |
| `Dijkstra_Small` | 4 338 | 3 790 | −12.63% | −4.52% |

That is the reached-span truncation working exactly as designed on a query whose reach is a
small part of the node space. Which of the two changes carries it is read from the code — only
the truncation can change a returned `Distances`' byte count — and was **not** put to a
single-variable split, so that attribution is hypothesised, not established.

*The acceptance criterion is NOT met, and rests on a false premise.* The criterion — the
"four sites under 15%" — is arithmetically unreachable from these two changes at this
workload. Measured on `20_concurrent_reads -nodes 60000 -iterations 200`, three interleaved
rounds per arm, `alloc_space` flat share of the whole process:

| site | before (3 rounds) | after (3 rounds) | touched by the change? |
|---|---|---|---|
| `search.BFSCtx` | 13.54 / 13.83 / 13.86% | 14.08 / 13.84 / 13.76% | **no** |
| `search.acquireDijkstra` | 13.20 / 12.92 / 12.97% | 13.12 / 13.39 / 13.59% | yes |
| `search.newDistancesCopy` | 8.51 / 8.62 / 8.53% | 8.40 / 8.43 / 8.39% | yes |
| `search.(*dijkHeap).push` | 8.10 / 7.96 / 7.83% | 8.07 / 8.11 / 8.03% | **no** |
| **four sites together** | **43.35 / 43.33 / 43.19%** | **43.67 / 43.77 / 43.77%** | |

The group share did not fall — it moved from a 43.29% mean to a 43.74% mean, inside the
round-to-round spread. Two independent reasons, each measured:

- **`reachedSpan == maxID` here.** The example's graph is connected, so the backwards scan
  finds `found[maxID-1]` set and the truncation is a no-op. `newDistancesCopy`'s bytes went
  2 973.56 MB → 2 929.75 MB (−1.47%), and `-list` shows why: its three `make` lines went
  1.35 GB / 1.36 GB / 195.21 MB → 1.33 GB / 1.35 GB / 184.57 MB. The 99.76% win above is
  real; this workload simply never exercises it.
- **The heap backing was already pre-sized.** `-peek` charges **100%** of
  `dijkHeap.push`'s allocation to `dijkstraCore`, in both arms (2 829.71 MB → 2 814.31 MB),
  and `acquireDijkstra`'s own `make([]dijkItem[W], 0, maxID)` line is 2.16 GB → 2.15 GB. The
  growth `push` pays for is the backing climbing *past* `maxID` inside the traversal, which a
  `maxID` hint cannot prevent — `acquireDijkstra`'s comment says so itself.

The arithmetic is decisive. Even if both touched sites were driven to **zero**, the two the
changes never touch — `BFSCtx` at 13.54% and `dijkHeap.push` at 8.10% — sum to **21.64%**,
which still exceeds 15%. No execution of these two changes could have satisfied the criterion.
It is unmet, with a measured reason; it is not partially met.

*Two follow-ups identified and NOT implemented*, both put to the user rather than acted on:
fusing the pooled working set with the returned result (so the copy disappears instead of
shrinking), and replacing the `sync.Pool` with a bounded structure — the second changes the
module's memory-retention contract and is the user's decision.

**Backlog.** **#2382 SUPPORTED, and larger than recorded** (see the reconciliation table).
The four non-PageRank sites: **filed as rmp #2862.**

---

### R3 — `LabelBitmapAsOf` clones the whole label bitmap on every scan: 52.75% of one profile's allocation

**Claim.** `Graph.LabelBitmapAsOf` (`graph/lpg/mvcc_index.go:300`) obtains a caller-owned
bitmap by calling `label.Index.Intersect`, which for a single label clones the index's live
bitmap unconditionally (`graph/index/label/index.go:497`: `if shared { result = bm.Clone() }`).
`labelBitmapAsOfFiltered` then returns that clone **untouched** whenever no label the read
concerns has a live suspect — which its own doc calls *"the whole of a read-only workload"*.
In that case the clone is pure copy.

**Evidence** — `35_mvcc_mixed_workload`, three independent passes:

| pass | total alloc | `ResolveLabelBitmap`/`LabelBitmapAsOf` cum | `roaring64` clone chain, flat |
|---|---|---|---|
| out/default | 14 105.20 MB | **52.75%** | `arrayContainer.clone` 7 046.97 MB = **49.96%** |
| out/elevated | 14 108.80 MB | **53.67%** | 7 571.7 MB = 53.67% |
| out/contention | 14 155.40 MB | **53.33%** | 7 548.6 MB = 53.33% |

Full chain, `-peek`: `exec.(*NodeByIndexRangeScan).Init` → `lpgLabelResolver.ResolveLabelBitmap`
→ `lpg.Graph.LabelBitmapAsOf` → `labelBitmapAsOfFiltered` → `label.Index.Intersect` →
`roaring64.Bitmap.Clone` → `roaringArray64.clone` → `roaring.Bitmap.Clone` →
`roaringArray.clone` → `arrayContainer.clone`.

**The CPU consequence, with its cause named.** Row 35 is GoGraph **4.73%** and runtime
**91.28%** (delta 0). 14.1 GB allocated in 3.31 s is **3.97 GiB/s**, and the runtime cost
decomposes accordingly:

| runtime symptom | cost | reached through |
|---|---|---|
| `runtime.lock2` on `sched.lock` | **3.59 s = 26.97%** | `gcstopm` 1.26 s, `goschedImpl` 0.98 s, `wakep` 0.43 s, `mheap.freeSpan`/`allocSpan` 0.52 s, `add/removefinalizer` 0.21 s |
| of which spinning | `osyield`→`usleep` 2.76 s, `procyield` 0.40 s, `semasleep` 0.37 s | |
| `runtime.madvise` | 1.99 s = 14.95% | **`runtime.sysUsed`/`sysUsedOS`** — page re-commit |
| `preemptM`→`pthread_kill` | 1.13 s = 8.49% | async preemption signals |

**Attribution.** The allocation attribution is **established**. The causal chain
allocation rate → GC frequency → `sched.lock` contention and page re-commit is
**hypothesised**; the `sysUsedOS` and `gcstopm` paths are consistent with nothing else, but
only an A/B settles it.

**Owner.** `graph/lpg`, `graph/index/label`.

**Recommended change.** Materialise the private copy only when the correction path will
actually mutate it. **This is not a one-line change**, and the reason is load-bearing: the
clone's *position* is deliberate — `labelBitmapAsOfFiltered` samples the suspect set both
before and after the clone so the sample **spans** it, and the doc explains why one
post-clone sample is unsound. A correct fix must keep the spanning property, e.g. by taking a
copy-on-write handle across the two samples and materialising only on entry to
`correctBitmapOver`.

**Risk.** Medium. `Intersect` currently documents a caller-owned result; any shared return
changes that contract, and `correctBitmapOver` mutates in place.

**Validation plan.** `go test ./graph/lpg/... ./graph/index/label/...` including
`TestLabelIndexAddWindow_BitmapReaderNeverLosesARow`; the isolation battery in
`internal/isolationtest`; then re-run row 35 and compare the allocation table.

**Backlog.** **Filed as rmp #2863.** Related to **#2391**, whose own numbers this refutes.

#### Outcome — implemented as rmp #2863, measured

**What shipped.** `label.Index.BitmapShared` (`graph/index/label/index.go`) returns a
**memoised immutable image** of one label's node set in place of a per-read clone. The image is
built once under the entry write lock, published on `entry.image`, and **dropped — never edited —
by `Index.mutate`** before the set changes, so a reader still holding the old pointer keeps the
instant it acquired. `mutate` is the single funnel for all four mutators (`Add`, `Remove`,
`AddRange`, `RemoveRange`); `Deserialize` builds its entries fresh, so their image is nil by
construction; and `reap` detaches an entry only after the mutation that emptied it has already
dropped the image, so no stale image can outlive its entry. `Intersect` is
**untouched** and keeps its caller-owned contract — which is precisely why the fix is a new
method rather than a change to the old one, and why the *Risk* recorded above did not
materialise. `labelBitmapAsOfFiltered`'s `clone func() *roaring64.Bitmap` became
`acquire func() (*roaring64.Bitmap, bool)`, and the private copy is now taken **only** on the
branch that reaches `correctBitmapOver`.

**Why the spanning property survives** — the load-bearing point this finding flagged. `acquire()`
is called at exactly the position `clone()` occupied, between the two suspect samples, so **the
instant does not move**. Deferring the *copy* past the second sample is sound because a published
image is written once and never again: a write to the label replaces the image rather than editing
it, so the later `Clone()` reproduces the image **as it was at acquire**, not as the index is by
then. The recommendation above — "a copy-on-write handle across the two samples, materialising
only on entry to `correctBitmapOver`" — is what was built. Pinned by
`TestLabelBitmapAsOf_CorrectsWhenTheSweepLandsDuringTheClone`
(`graph/lpg/mvcc_suspects_test.go:33`), which lands a sweep inside that window.

**Evidence** — `35_mvcc_mixed_workload`, 10 interleaved A/B/A2 rounds, same host, `A2 = A`
byte-identical as the noise control:

| phase | base sec/op | fixed sec/op | delta |
|---|---|---|---|
| `baseline` | 2.044 µs ± 1% | 1.417 µs ± 2% | **−30.68%** (p=0.000, n=10) |
| `analytics_only` | 2.561 µs ± 3% | 1.878 µs ± 3% | **−26.66%** (p=0.000, n=10) |
| `writer_only` | 2.106 µs ± 2% | 1.441 µs ± 3% | **−31.56%** (p=0.000, n=10) |
| `analytics_and_writer` | 2.623 µs ± 3% | 1.948 µs ± 3% | **−25.75%** (p=0.000, n=10) |
| **geomean** | 2.319 µs | 1.653 µs | **−28.71%** |

**The noise control returns zero significant rows** (every phase `~`, p = 0.393–0.796, geomean
−0.35%), so the four deltas above sit far outside the floor.

Allocation, `alloc_space` over one profiled run of each arm: **15 223.14 MB → 9 741.67 MB**
(**−36.0%**) **while doing 1.431× the operations** (summed phase throughput
1 820 327 → 2 605 416 ops/s over identical 3.21 s phases), i.e. **−55.3% per operation**.
openCypher TCK **3897/3897**, measured by #2863 on this tree; corroborated here by
`go test -count=1 ./cypher/...` passing, whose gate constant `tckExecutionBaseline` is 3897.

**AC-1 — MET, at 0.00%.** The criterion asked for the roaring clone chain under 10% of
allocation. In the base arm it is `roaring64.(*Bitmap).Clone` 53.47% cum and
`(*arrayContainer).clone` **50.50% flat**; in the fixed arm, at `-nodefraction=0`, **no clone
frame survives anywhere in the profile** — the chain is gone, not merely reduced.

**AC-2 — NOT MET, and the threshold's premise is wrong.** The criterion asked for the runtime
bucket below 80%. Measured: **94.07% → 93.36%** (two-route delta 0 in both arms). This is
recorded as **unmet with a measured reason**; it is **not** "partially met", and it must never be
softened into that — a criterion reported that way is a false statement a later reader acts on.

The measured reason: this workload is **fixed-duration** (3.21 s per arm) on a **10-core** host,
and roughly **half of its profiled CPU is the runtime idle or parking** — in the fixed arm
`usleep` 24.43%, `pthread_cond_wait` 13.27%, `pthread_cond_signal` 7.39% and `procyieldAsm`
4.30%, **49.39% together** (47.94% in the base arm). Removing allocation therefore returns
capacity to **idle**, not to GoGraph, and **no allocation fix can move that share**. The 80%
threshold was set against a bucket that is mostly parking, so it measures the wrong thing for
this row.

What *did* move is exactly what the finding named:

| frame | base | fixed |
|---|---|---|
| `runtime.gcDrain` cum | 9.68% | **6.49%** |
| `runtime.gcBgMarkWorker` cum | 7.05% | **3.77%** |
| GoGraph's own bucket | 2.10% (0.28 s) | **3.24%** (0.43 s) |
| `runtime.madvise` flat | 10.88% | **15.69%** |
| `runtime.sysUsed` → `sysUsedOS` cum | 10.43% | **15.61%** |

The total profiled CPU is essentially unchanged — **13.33 s → 13.26 s of samples over the same
3.21 s** — while the run performs 1.431× the operations; GoGraph's share rises because the module
is doing more work inside the same window. Against that, **`madvise`/`sysUsed` rose** (page
re-commit) and **partly cancelled the marking win inside the CPU profile**, which is why the
throughput gain is visible in the A/B and only partly visible in the bucket table.


---

### R4 — `exec.Expand` discards the stored-direction bit; the hydrator recovers it per row with an O(deg) probe

**Claim.** `relStoredInverted` (`cypher/api.go:15537`) exists because, in its own words,
*"[exec.Expand] emits (src, edge, dst) in traversal order and **keeps no direction flag**, so
the hydrator has to recover the stored direction itself."* Recovering that one discarded bit
costs a topology probe per row. Its cheap first question — the by-handle type record — should
answer for every Cypher-written edge and costs 0.40 s; the workload's edges are built through
the Go API, so the probe falls through to the O(deg) topology question on every row.

**Evidence** — `26_social_scale_bench`, three passes:

| site | out/default | out/elevated | out/contention |
|---|---|---|---|
| `cypher.relStoredInverted` cum | **19.20 s = 15.76%** | 18.75 s = 15.60% | 19.97 s = 17.07% |
| └ `lpg.ReadView.HasEdge` | 18.74 s (97.60% of it) | | |
| └ `lpg.HasEdgeHandleLabelRecordByID` | 0.40 s (2.08%) | | |
| `adjlist.HasEdgeAsOf` flat | 14.28 s = **11.72%** | 14.21 s = 11.82% | 14.56 s = 12.44% |
| `cypher.buildRelationshipValueFromRow` cum | 32.65 s = 26.81% | 32.94 s = 27.40% | 34.83 s = 29.77% |

Line-level, `-list 'adjlist.*HasEdgeAsOf'` (`graph/adjlist/mvcc_view.go:132`):

```
      20ms      1.09s    134:  e := a.entryAsOf(s, uint64(srcID)>>shardBits, startTS, txID)
    13.17s     13.18s    138:  for _, n := range e.neighbours {
      20ms       20ms    139:      if n == dstID {
```

**10.81% of the whole profile is one line**: a linear scan of the source's neighbour slice.
The version-chain resolution on line 134 is 1.09 s — 12× cheaper than the scan.

**A premise correction, because this document was given the wrong one.** This row was
described to the campaign as completing "at its 1 M-user default". **It is not 1 M.**
`examples/26_social_scale_bench/main.go:217` sets `users: 50_000` as the default, and the
recorded `argv` for all three passes passes no `-users` override; `run.log` confirms
`config.users=50000`. What is true is that the row **completes at its own default**
(50 000 users, 30 000 articles) in 129 / 136 / 130 s with `run_exit=0` and verdict `PASS`.
**The sweep therefore says nothing about a 1 M-user run**, and it neither confirms nor
refutes the 2026-08-10 finding at that scale. Every share in the table above is a share of a
50 000-user run.

**Attribution.** Established: the caller chain is single-parented at 97.60%, and the line
attribution is unambiguous.

**Owner.** `cypher` (the discard), `graph/adjlist` + `graph/lpg` (the probe).

**Recommended change.** Stop discarding the bit. One flag on `exec.Expand`'s output,
propagated to the hydrator, removes the question. Making `HasEdgeAsOf` faster treats the
symptom.

**Risk.** Medium-high in breadth, low in depth: every producer of the `(src, edge, dst)`
triplet must set the flag correctly — plain `Expand`, `ExpandInto`, variable-length expands,
undirected hops, and the post-projection forward path the function's doc already warns about.
A missed producer renders `StartID`/`EndID` wrongly for a reciprocal pair, which is
TCK-observable.

**Validation plan.** `go test ./cypher/...`; the **full openCypher TCK**
(`go test ./cypher/tck/...`) because the change can move rendered relationship endpoints;
then re-run row 26 in all three passes.

**Backlog.** No existing task; distinct from #2732, which is about whole-*node*
materialisation. **Filed as rmp #2864.**

---

### R5 — the row context is a `map[string]Value` rebuilt per row, and per-row work re-derives plan-time facts by string key

**Claim.** `populateRowCtx` (`cypher/api.go:15319`) writes into `ctx[varName]`, a
string-keyed map, once per variable per row, and before doing so probes up to six
**plan-time** maps by the same string — `pathVarChain`, `pathVarMeta`, `vleRelMeta`,
`edgeVarMeta` (twice), `scalarCols`, `projAliasScalarCols`, `scalarUse`. Every one of those
answers a question fixed when the plan was built.

**Evidence** — `26_social_scale_bench`, out/default (121.80 s CPU, 50 947.25 MB alloc):

| measurement | value |
|---|---|
| `populateRowCtx` CPU cum | **49.54 s = 40.67%** (41.60% / 44.76% in the other two passes) |
| `runtime.mapaccess2_faststr` total | 15.88 s cum = **13.04%**, of which `populateRowCtx` drives **9.26 s (58.31%)** |
| `runtime.mapassign_faststr` from `populateRowCtx` | 2.37 s |
| string-map cost inside `populateRowCtx` | **11.82 s = 9.70%** of the profile |
| per-line: `edgeVarMeta[varName]` (15350) | 3.98 s cum |
| per-line: `scalarUse[varName]` (15419) | 1.93 s |
| per-line: `scalarCols[varName]` (15407) | 1.59 s |
| per-line: `projAliasScalarCols[varName]` (15413) | 1.06 s |
| `populateRowCtx` alloc flat / cum | **12 237.86 MB (24.02%)** / 25 557.70 MB (**50.17%**) |
| `buildRowCtxWithUse` alloc cum | 22 679.41 MB = **44.52%** |
| supporting sites | `nodePropsToExprMap.func1` 5 745.40 MB (11.28%), `upgradeNodeIDToValue` cum 8 468.96 MB (16.62%), `graph.fnv1aString` 3.01 s, `maps.memHashAES` 2.87 s |

Reproduced at 50.17 / 49.88 / 49.72% of allocation across the three passes.

**Attribution.** Established for the cost; the claim that a slot-indexed context would
remove it is a **design hypothesis**.

**Owner.** `cypher`.

**Recommended change.** Resolve each binding's kind and metadata pointer **once, into the
`schemaWalk` entry**, at plan time, and index the row context by slot rather than by name.

**Risk.** High breadth — `expr.RowContext` is consumed across the expression evaluator.

**Validation plan.** `go test ./cypher/...`; the full openCypher TCK; `benchstat` on
`bench/cypher_alloc` and `bench/cypher_scale`; re-run row 26.

**Backlog.** **#2404 SUPPORTED but refined** — the mapper's synthetic string key is only
19.90% of the string-map cost; 58.31% is this. **#2732 SUPPORTED**. **Filed as rmp #2865**
for the slot-indexed context.

---

### R6 — the CSR pair cache cannot hit under concurrent writes: 33.35% of one profile's allocation

**Claim.** `csrPairCachedAt` (`cypher/csr_pair_cache.go:256`) keys the cache on
`{TopoGeneration, snapshot.StartTS(), versioned}` and bypasses it entirely when
`viewCarriesOwnWrites(g)`. A versioned reader's `StartTS` is unique per snapshot, so with a
writer committing continuously the key never repeats and every query rebuilds the pair.

**Evidence** — `36_mvcc_snapshot_topology`, out2/elevated (46 231.30 MB alloc):

| site | alloc flat | share |
|---|---|---|
| `csr.(*CSR).BuildReverse` (100% from `cypher.csrPairFromGraphAt`) | 6 069.39 MB | 13.13% |
| `csr.buildFromAdjList` | 3 504.77 MB flat, 5 778.82 MB cum | 12.50% |
| `csr.OrderRuns` | 2 274.06 MB | 4.92% |
| `exec.projectFwdToRevByTranspose` | 1 293.74 MB | 2.80% |
| **sum** | **15 416.01 MB** | **33.35%** |

Also `31_metrics_observability`: `csr.buildFromAdjList` 2.13 s cum = 8.88% of 24.00 s.
Workload: `writer.commits_per_s=17` over 240 s.

**Attribution.** Established that the rebuild happens and what it costs. That the *cause* is
the key's `startTS` is **inferred from the code, not measured** — a cache-hit-rate counter
would settle it and this campaign collected none.

**This is a trade-off, not a defect.** `startTS` is in the key, and write views are excluded,
because of rmp #2446 (found by the DST multi-session mode): serving a writer's pair to a pure
reader exposes uncommitted topology, and CSR positions shift so the position-keyed edge-type
filter mislands. The project's own record also states that `TopoGeneration` alone is unsound.
So the cost is what soundness currently costs, and the question is whether a sounder key can
also be a repeatable one.

**Owner.** `cypher`, `graph/csr`.

**Recommended change.** None proposed. This needs a design decision, and it is the user's.

**Backlog.** **Filed as rmp #2866**, typed `SPIKE`.

---

### R7 — the MVCC chain walk: 55.67% of the heaviest profile is one line, but the workload is a harness query that never finished

**Claim.** In `36_mvcc_snapshot_topology`, 66.26% of a 1024.08 s profile is the loop in
`entryAsOfLoaded` (`graph/adjlist/mvcc_adj.go:185`), and **55.67% of the whole profile is
line 196 alone** — `mvcc.Visible(v.supersededAt(), startTS, txID)`. The predicate is three
integer comparisons; what it waits on is **two dependent pointer dereferences** into
separately heap-allocated objects.

**Evidence.** `-list 'entryAsOfLoaded'`:

```
         .      6.21s    192:    v := e.ver.Load()
     7.60s      7.62s    193:    if v == nil {
         .    570.06s    196:    if mvcc.Visible(v.supersededAt(), startTS, txID) {
    94.15s     94.57s    199:    e = v.prev
```

Inside them:

| line | cost | what it is |
|---|---|---|
| `mvcc_adj.go:75` `if v.info != nil` | **166.19 s flat** | loads `v.info` — dereference into the 24-byte `adjVersion` |
| `mvcc_adj.go:76` `return v.info.TS()` | 19.08 s flat / 48.96 s cum | loads `CommitInfo.ts` — dereference into an 8-byte object |
| `mvcc.go:105` `ts == txID` | 88.40 s | |
| `mvcc.go:107` `ts < TxIDBase` | 54.75 s | |
| `mvcc.go:108` `ts <= startTS` | **217.54 s** | |
| total of the five | **546.0 s = 53.3%** | |

Reached, 100% single-parented at every step:
`cypher/exec.(*Filter).Next` → `newRowPredicate.func1` → `evalRowPooled` → `expr.EvalWith` →
`evalExpr` → **`evalUnaryOp`** → `patternEvaluator.EvalPattern` → `matchSteps` →
`matchSingleHop` → `matchOutgoing` → `edgeMatchesRel` → `lpg.EdgeLabelsAsOf` →
`EdgeLabelsByIDAsOf` → `slotLabelsForPair` → `adjlist.EntrySlotLabelsAsOf` → `entryAsOf` →
`entryAsOfLoaded`.

**Three corrections to how this row has been described.**

1. **The driving query is `qContradiction`, and the example's own source declares it
   quadratic.** `examples/36_mvcc_snapshot_topology/main.go:207` reads: *"qContradiction costs
   O(spokes²) predicate evaluations — one per expanded row, each walking the hub's adjacency
   — which is why the default `-spokes` is modest. At 200 spokes this query alone took the
   `go test -coverpkg=./...` gate 188 seconds."* The elevated pass ran it at
   **`-spokes 4000`**. The `evalUnaryOp` frame in the chain above is its
   `WHERE NOT (h)-[:LINK]->(s)`.
2. **It never completed.** `reader.contradiction_checks=0`, `contradiction_checks_met=0`, and
   the driver's own message is *"example 36: the self-contradiction query never ran; that half
   of the check did not happen"*, `run_exit=1`. The 1024.08 s therefore measures four
   concurrent, cancelled, intentionally quadratic diagnostic queries.
3. **The chain does not grow for the life of the run.** `mvcc.versions_peak=102` — 102 live
   adjacency version records **graph-wide** at peak, with `Reclaim`→`severChain` active at
   22.81 s (2.23%). So the cost is **call count, not chain depth**, and shortening chains buys
   nothing. (The stale `VersionCount` godoc at `graph/adjlist/mvcc_adj.go:~100` still says
   *"nothing reclaims these yet"*; reclamation exists — recorded as a NOTE, not worked.)

**Ownership is UNCONFIRMED.** GoGraph 67.98%, runtime 26.16%, third-party 0.08% — two-route
**delta 12.98 s (1.27%)**. By this project's rule a non-zero delta is a bucketing error, so
the split is not quoted as fact. The **flat per-line figures above do not depend on the
split** and stand.

**It does not reproduce, and it cannot.** `36_mvcc_snapshot_topology` at elevated scale has
**exactly one readable CPU profile in the whole sweep** — `out2/elevated`, 155 KB. The
`out/elevated` and `out/contention` attempts are **zero-length files**, and the `out/default`
profile is a 0.12 s run at the example's default 80 spokes, a different workload. So unlike
R1–R5, which reproduce across two or three independent passes, **every figure in this finding
rests on a single run.**

**Why it is ranked here and not first.** The absolute number is the largest in the sweep, but:
the workload is a harness query the harness could not finish at a scale its own author warned
against; the ownership split is unconfirmed; and it rests on one unreproduced run. R1–R6 rest
on completed runs with confirmed splits and two or three passes each.

**Owner.** `graph/adjlist`, `graph/mvcc` — and, for the *reason* the walk is entered
3 billion-odd times, `cypher` (`edgeMatchesRel` resolves the source's whole adjacency entry to
answer one arc's type).

**Backlog.** This is the subject of rmp **#2860**, whose findings and design options are in
[`prior-art-mvcc-visibility-2026-09-16.md`](prior-art-mvcc-visibility-2026-09-16.md).

---

### R8 — `Snapshot.visible` takes a mutex and grows an unbounded memo map on the read path

**Claim.** `(*Snapshot).visible` (`graph/lpg/snapshot.go:112`) locks a `sync.Mutex` and
probes, then inserts into, `map[*commitInfo]bool` on **every** visibility test of a
side-map version. The map grows for the life of the snapshot.

**Evidence.**

| row | alloc in `Snapshot.visible` | share | reached from |
|---|---|---|---|
| `37_mvcc_write_contention`, out/contention | 71.0 MB of 228.1 MB | **31.14%** | `sideVersions.asOfSnap` |
| `37_mvcc_write_contention`, out/elevated | 61.5 MB of 226.9 MB | 27.11% | same |
| `36_mvcc_snapshot_topology`, out2/elevated | 5 763.63 MB of 46 231.30 MB | **12.47%** | 99.89% `sideVersions.asOfSnap` |

**This is a global lock on a hot read path**, which
[Reliability and Concurrency Mandates](../CLAUDE.md) names a defect rather than a missed
optimisation. It is there for a correctness reason the code records with measurements
(rmp #2378: pinning alone 2/100 failures, dropping the global counter alone 3/100, both
together 0/300), so it must not simply be deleted.

**Attribution.** Allocation established. The lock's *contention* cost was **not measured** when
this finding was written — the mutex profiles exist for 42 `out/default` rows and **17**
`out/contention` rows (not 18), **59 live**, and only one had ever been read. It has since been
measured; see the outcome below.

**Owner.** `graph/lpg`.

**Backlog.** **Filed as rmp #2867.** Design options in the prior-art document: Option A
retires the need for pinning rather than optimising the pin.

#### Outcome — rmp #2867's first criterion measured: the contention is not there

The criterion is *"the mutex delay attributable to `Snapshot.visible` is reported for both rows
named above, read from the profiles on disk"*. Read, on every mutex profile that holds either row:

| profile | total mutex delay | `Snapshot.visible` cum | share |
|---|---|---|---|
| `37_mvcc_write_contention`, `out/contention` | 1 397 703 µs | 646.64 µs | **0.046%** |
| `37_mvcc_write_contention`, `out/default` | 275 412 µs | 74.33 µs | **0.027%** |
| `37_mvcc_write_contention`, `out/default-preorder-artefact` (superseded) | 212 033 µs | 414.70 µs | **0.196%** |
| `36_mvcc_snapshot_topology`, `out/default` | 11 815 µs | — | **does not appear** |
| `36_mvcc_snapshot_topology`, `out/contention` | — | — | **no mutex profile exists** (see the method section) |

**The contention this task was filed for is not present.** The third row is quoted only because
the criterion asks for every profile on disk; it is the superseded pass this document otherwise
does not quote, and it agrees with the two live ones.

**None of what little there is belongs to the `sync.Mutex` the task names.** `Snapshot.visible`'s own
**flat delay is zero** in all three row-37 profiles. Its only children are
`runtime.mapassign_fast64ptr` (**93.64%**) and `runtime.makemap_small` (**6.36%**) in the
contention pass — `mapassign_fast64ptr` at 100% in the other two. That is the runtime's own heap
lock inside the map insert: **the allocation the memo causes, not the lock guarding it.** Its
sole caller is `propBagAsOfLockedSnap`, at 100% in all three.

**What row 37's mutex delay actually is.** `sync.(*RWMutex).Unlock` is **75.63% cum**, reached
90.82% from `setNodePropertyInfo` and 8.96% from `withdrawAbortedProps` — the versioned
property-write path. That is a **different finding**, and it is not #2867's.

**Conclusion to carry forward.** If a change is ever made at `Snapshot.visible` it must be
justified **on allocation** — the 31.14% / 27.11% / 12.47% shares established above — and
**never on contention**, which is measured at under 0.2% of mutex delay everywhere it appears.
And the memo **must not simply be deleted**: rmp #2378 measured pinning alone at 2/100 failures,
dropping the global counter alone at 3/100, and both together at **0/300**.

---

### R9 — `centrality.brandesSource`: the cleanest single target in the sweep

**Claim.** `03_advanced_algorithms` is **GoGraph 90.97%, runtime 8.99%, delta 0** — the only
confirmed-exact split in which one function is essentially the whole profile:
`brandesSource` flat **37.23 s = 87.81%**, cum 41.06 s = **96.84%** of 42.40 s. The code is
already well built (flat CSR arrays, a predecessor arena, caller-owned buffers), so the
opportunities are micro and each is small.

**Evidence** — `-list 'brandesSource'` (`search/centrality/brandes.go:157`):

| line | cost | opportunity |
|---|---|---|
| 177 `for k := verts[v]; k < verts[v+1]; k++` | 3.96 s | loop control over the CSR range |
| 178 `w := int(edges[k])` | 4.30 s | the CSR gather; inherent |
| 179 `if dist[w] < 0` | 1.40 s | `dist[w]` is loaded here **and again** on line 183 |
| 180 `dist[w] = dist[v] + 1` | 2.12 s | `dist[v]+1` is recomputed per edge |
| 183 `if dist[w] == dist[v]+1` | 3.12 s | same two redundancies |
| 184 `sigma[w] += sigma[v]` | 4.50 s | random read-modify-write; inherent |
| 198 `region := flat[off[w]:pos[w]:pos[w]]` | 2.03 s | three-index slice bounds checks |
| 200 `delta[v] += (sigma[v] / sw) * coef` | 1.87 s | a float64 **division** per predecessor |
| `runtime.asyncPreempt` | 2.50 s = **5.90%** | a call-free loop, preemptible only by signal |

**Two of these are bit-identical-safe and two are not.** Hoisting `dv1 := dist[v] + 1` out of
the inner loop and caching `dw := dist[w]` are integer operations and exact. Replacing
`sigma[v] / sw` with a precomputed reciprocal is **not** bit-identical, and the function's own
comment states the accumulation order must stay bit-identical — so that one is a
behaviour-affecting change requiring the user's decision, not an optimisation.

**Outcome (rmp #2868): the `dv1` hoist shipped; caching `dw` was tried, measured and
DROPPED.** Both are exact, so the choice between them was settled by measurement alone. A
single-variable split over 7 samples showed the `dw` cache carried the whole of the arm's
sparse-graph regression: with both changes `Betweenness_Serial` measured **+1.99%** and
`Brandes_RandomGraph` **+0.77%**, while the `dv1` hoist alone left them at **+0.20%** and
**+0.38%** for the same geomean win. On the shipped arm's own re-run ladder both rows are
**`~`** — the residual was noise — with `2k_deg16` at **−2.01%** (p=0.001), `5k_deg16` at
**−3.52%** (p=0.001) and a full-ladder geomean of **−0.70%**, against a noise control of
−0.02% in which every row is `~`. **Do not re-propose the `dw` cache as an open lead:** it is
a measured-and-rejected option, not an untried one.

The 5.90% in `asyncPreempt` is the measured cost of a long non-yielding loop, against the
module's own rule that long operations should yield or chunk.

**Owner.** `search/centrality`.

**Backlog.** **Filed as rmp #2868.**

---

### R10 — `pageRankEngine.runRange`: a float division per in-edge, and an unpredictable abs branch at 10.93%

**Claim.** `runRange` (`search/centrality/pagerank.go:761`) is flat **53.16 s = 36.87%**, cum
54.92 s = 38.09% of `20_concurrent_reads` (GoGraph 62.98%, delta 0). Two lines carry
avoidable work.

**Evidence** — `-list 'runRange'`:

| line | cost | what it is |
|---|---|---|
| 780 `for k := revVerts[v]; k < revVerts[v+1]; k++` | 5.06 s | loop control |
| 781 `u := int(revEdges[k])` | 8.83 s | CSR gather; inherent |
| 782 `sum += damping * cur[u] / float64(outdeg[u])` | **8.89 s** | a float64 **divide** and an int→float convert **per in-edge**, both loop-invariant in `u` |
| 784 `next[v] = sum` | 9.64 s | store retiring the divide chain |
| 785 `d := sum - cur[v]` | 1.09 s | |
| 787 `d = -d` | **7.25 s** | |
| 789 `localDelta += d` | **7.42 s** | |
| lines 785–789 together | **15.76 s = 10.93%** | an unpredictable sign branch plus accumulate |

**`math.Abs` is a compiler intrinsic on arm64 and amd64**, so
`localDelta += math.Abs(sum - cur[v])` is **bit-identical** to the branch and removes an
unpredictable branch from the innermost accumulation — the best kind of finding: exact, and
15.76 s of a 144 s profile sits on it. Precomputing `damping / float64(outdeg[u])` per node
is a larger prize on line 782 but is **not** bit-identical and needs a decision.

**Owner.** `search/centrality`.

**Backlog.** **Filed as rmp #2869.**

---

## Runtime-dominated profiles: the cause, not the symptom

`madvise`, `mallocgc` and `pthread_cond_*` are consequences. Each row below names the module
behaviour that produces them.

| row | runtime share | allocation rate | cause (finding) |
|---|---|---|---|
| `35_mvcc_mixed_workload` | **91.28%** (delta 0) | 14.1 GB / 3.31 s = 3.97 GiB/s | **R3** — the label-bitmap clone is 52.75% of it |
| `11_social_network` | **66.45%** (delta 0) | 294 GB / 29.39 s = 9.3 GiB/s | **R1** — one un-pooled BFS queue is 99.51% of it |
| `26_social_scale_bench` | 59.67% | 50.9 GB / 129 s = 395 MiB/s | **R4 + R5**; `madvise` 16.77 s, `mallocgc` cum 12.15 s (9.98%) |
| `24_plandiff` | 71.52% (delta 0.01 s) | 14.6 GB / 18.42 s | **R5** family — `upgradeNodeIDToValue` cum 20.44%, `Expand.buildRow` 3.37% |
| `36_mvcc_snapshot_topology` | 26.16% (unconfirmed) | 46.2 GB / 240 s | **R6** (33.35%) + `forEachResolvedSlotType` (29.55%) + **R8** (12.47%) |
| `22_cypher` 86.84%, `19_pattern_query` 75.38%, `23_bolt_server` 65.23%, `25_software_house_api` 62.77%, `31_metrics_observability` 63.42% | | | short runs (3–35 s) in which start-up and GC dominate; no single dominant module site. **Not ranked.** |

The mechanism, measured once and named rather than repeated: a high allocation rate with a
small live heap makes the Go runtime release and re-commit pages (`sysUnused`/`sysUsed` →
`madvise`), run GC often (each cycle preempting every goroutine, `preemptM` → `pthread_kill`,
and every goroutine taking `sched.lock` through `gcstopm`), and contend the allocator's own
locks. **Cutting the allocation is the only lever; the runtime symptoms are not separately
addressable.**

### Block delay is deliberately unranked

Block profiles exist for 42 `out/default` and 17 `out/contention` rows. An idle server's
goroutines parked on `accept` accumulate block delay **while waiting for work** —
`25_software_house_api` records `block_delay_ns=23064184330`, i.e. **23 064 ms over an 8 s
run**, and 90 603 ms over a 35 s run in `out/contention` — which is the absence of contention,
not contention. Ranking it would manufacture a finding. Its `mutex_delay_ns` over the same
8 s is **4.0 ms**, four orders of magnitude smaller, which is the number that would actually
mean something if the profiles had been read.

## What is not module cost

Two rows' totals must not be read as GoGraph cost, and one must not be read as *stdlib* cost.

**`14_routing_alternatives` — 39.32 s, GoGraph 0.56%, stdlib 71.36%, delta 0.** `main.kNearest`
is **33.95 s cum = 86.34%**: `sort.Slice` over a slice of large structs, so the swap goes
through reflection — `sort.pdqsort_func` 33.55 s (85.33%), `sort.partition_func` 14.33 s flat,
`internal/reflectlite.Swapper.func9` 9.23 s, `reflectlite.typedmemmove` 7.90 s,
`runtime.typedmemmove` 5.75 s, `runtime.memmove` 2.58 s, and the comparator
`main.kNearest.func1` 6.87 s. **The row measures the harness's sort.** Examples are exercise
material, so this is recorded, not worked.

**`20_concurrent_reads` — stdlib 10.43% is also the harness.** `sort.Slice` 15.05 s is
**100% driven by `main.topKByRank`**, plus 3.50 s in its comparator: **18.59 s = 12.9%** of
the profile is harness sorting. The previous pass listed `sort.partition_func` 8.94 s among
the row's maxima without attributing it.

**`21_typed_recovery` — "stdlib 73.88%" is GoGraph's WAL, mis-bucketed.** Leaf-side attribution
puts the syscall in `syscall`; the caller chain is
`main.commitTx` → `store/txn.(*Tx).Commit` **27.89 s (73.67%)** → `store/wal.(*Writer).SyncGroup`
**27.82 s (73.48%)** → `leadGroupSyncLocked` → `syncToLocked` → `bufio.Writer.Flush` →
`write(2)` **25.38 s (67.04%)**, plus `wal.dataSync` → `F_FULLFSYNC` via `runtime.fcntl`
**2.44 s (6.44%)**. So **73.48% of the row is GoGraph's WAL group-sync**, and the reported
GoGraph share of 0.45% is a bucketing artefact.

Whether that is *excessive* is a separate question, and the telemetry answers it:
`disk.wal_bytes=23.29 MiB` for **25.38 s of write-syscall CPU** — **1.09 s of CPU per MiB
written**. A byte copy cannot cost that, so the cost is **per call, not per byte**: the example
is a single-threaded `buildAndPersist` loop over 274 935 records, so group commit has no
second committer to coalesce with and degenerates to one write plus one fsync per transaction.
**That is the harness's shape, not a module defect**, and it is not ranked. It does show that
the sweep never exercised group commit with a group larger than one.

## Coverage, and one correction

`out/cover-default` (rmp #2858), `harvest/coverage-table.txt` and `harvest/func.txt`:

- **54.8% of statements** across the module (`func.txt` total line).
- **51 of 99 module packages reached**, 0 reported at zero, **48 absent from the counters**.
  *A previously circulated figure of "55 of 99, 41 of 44 unreached explained" is wrong; the
  harvest file says 51 and 48.*
- **47 of the 48 are structurally explained**: 19 `bench/*`, 6 `cmd/*`, 19 `internal/*` test
  infrastructure, and `cypher/tck` — none of which any example binary links. One row is blank.
- **`graph/io` and `store/bulkimport` are the only substantive production packages the sweep
  never reaches**, and neither has a named exercising harness. (`graph/io/dot` 72.1%,
  `graph/io/csv` 61.2%, `graph/io/graphml` 30.3% and `graph/io/jsonl` 14.4% *are* covered; it
  is the parent package that is not.)
- **Elevated-scale coverage was not measured** — that pass reached 3 of 42 rows.

## Backlog reconciliation

Every candidate named in the brief was tested against **all 113 CPU profiles** and **all 113
allocation profiles**, not against the tops that happened to be read. Profiles under 1 s of
CPU and under 100 MB of allocation are excluded for lack of resolution.

| task | verdict | the number |
|---|---|---|
| **#2732** whole-node materialisation | **SUPPORTED** | `upgradeNodeIDToValue`/`nodePropsToExprMap` alloc cum **23.21%** (`24_plandiff`, out/default), 20.44% (out/elevated), **16.62%** (`26`). CPU share is small (≤2.77%) — it is an allocation defect, not a CPU one |
| **#2404** synthetic string node key | **SUPPORTED, refined** | `mapaccess2_faststr` **13.04–14.67%** of CPU in `26`'s three passes. But only **19.90%** of it is `Mapper.Lookup`; **58.31% is `populateRowCtx`** probing plan-time maps — a cost #2404 does not name. `internSlowHook` alloc cum 36.09% in `34_bolt_transactions` |
| **#2382** ex20 per-worker PageRank | **SUPPORTED, larger than recorded** | recorded 36.32% of 204.60 MB at `cbc45aa2`; measured **39.71% flat / 50.37% cum of 34 494.38 MB** — same proportion at a 169× larger workload |
| **#2741** RWMutex reader-counter | **NOT ADDRESSED** | `sync/atomic.(*Int32).Add` appears in 33 profiles, **maximum 1.88%** (`31`, 24.00 s), 0.60% in `26`. The recorded 8.44% was measured on `lpg-neighbours-read` at concurrency level 8; **the sweep runs no concurrency ladder**, so it neither supports nor refutes it |
| **#2391** plan-pure separation | **candidate SUPPORTED, its numbers REFUTED** | `buildReadPhysical` alloc cum **25.61%**, not the recorded 59.56%. `copySchema` **6.30%**, not 13.83%, and **not** "the largest single flat allocation site in the whole process" — `roaring.arrayContainer.clone` at **49.96%** is. `ResolveLabelBitmap` **52.75%**, not 10.42% — a **5.1× increase that inverts the task's own priority ordering** |
| **#1704** typed/columnar scalars | **SUPPORTED** | boxing sites (`lpgPropToExpr`, `lpg.StringValue`, `lpg.Int64Value`, `runtime.convT*`) **36.56% of allocation** in `27_concurrent_txn`, **14.36%** in `25_software_house_api`; `runtime.convT` 1.54 s of CPU in `26` |
| **#2731** row headers escaping | **SUPPORTED** | `exec.(*Expand).buildRow` **11.03% of allocation** in `25_software_house_api` (1 352.5 MB of 12 258.1 MB), 3.37% in `24_plandiff` — materially larger than the recorded per-site microbenchmark suggested |
| **#2405** per-node record vs three sharded maps | **SUPPORTED** | node/edge attribute stores **58.32% flat of 962.8 MB** in `34_bolt_transactions`, 23.04% in `37_mvcc_write_contention`; in `11`'s object counts `storeEntry` 11.86%, `upsertEdgeSlotLocked` 10.28%, `linkVersion` 8.04%, `pushPropDelta` 6.60% |
| **#2616** subquery projection at 115× | **NOT ADDRESSED** | absent from every CPU profile ≥1 s and every allocation profile ≥100 MB. **No example drives a `COUNT { … }` subquery** — an evidence gap, not a refutation |
| **#2247** `edgeTypeFilter` map → bitset | **SUPPORTED, narrow reach** | `exec.(*RelTypeAdmit).Fwd` maximum **2.78% of CPU** (`25`, 29.87 s), 0.90% in `24_plandiff`, **zero allocation anywhere**. The recorded claim that its effect exceeds the sprint-313 CSR-ordering win is not testable from this sweep |
| **#2248** record `revFwdPos` in `BuildReverse` | **NOT ADDRESSED** | `lookupFwdEdgePos` **absent from every CPU profile ≥1 s**. Either no example drives a reverse-position recovery, or it is below sampling resolution |
| **#2249** ExpandInto as a real seek | **SUPPORTED, tiny** | `exec.(*Expand).dstMatchesInto` **0.52%** (`24_plandiff`). The task's own argument is that the remaining cost is the *enumeration*, which a leaf-side profile charges to the CSR scan, not to this symbol — so the sweep under-measures it by construction |
| **#2661** relationship-type key clone | **REFUTED** | **`canonicalRelTypesKey` has 0 occurrences anywhere in the tree at HEAD** (verified with `python3` across every `.go` file, not with `grep`). The site the task names no longer exists. `edgeTypeFilterFor` survives in 6 files and `slices.Clone(relTypes…)` appears nowhere in `cypher/` |

### Tasks filed

| id | finding |
|---|---|
| #2861 | R1 — `bfsFarthest`'s per-source queue allocation |
| #2862 | R2 — `search`'s per-query working set (the four non-PageRank sites) |
| #2863 | R3 — the label-bitmap clone-on-read |
| #2864 | R4 — the discarded stored-direction bit |
| #2865 | R5 — the string-keyed row context |
| #2866 | R6 — the CSR pair cache's unrepeatable key (SPIKE) |
| #2867 | R8 — `Snapshot.visible`'s read-path mutex and memo map |
| #2868 | R9 — `brandesSource` micro-opportunities |
| #2869 | R10 — `runRange`'s per-edge divide and abs branch |

## Limits of this method

1. **No `runtime/trace` was captured anywhere in the campaign.** Scheduler latency, goroutine
   blocking, GC pause distribution and syscall latency over the workload timeline are
   therefore unmeasured, and every GC-frequency claim above is inferred from CPU profiles
   rather than observed.
2. **Mutex and block profiles exist for 59 live rows each (100 on disk counting the superseded
   pass), and only R8's four have been read.** Every other row's mutex and block profile is still
   unread, so no contention claim is made for any of them. Worse for R7 and R8: mutex, block and
   goroutine profiles were captured **only** in `out/default` (42) and `out/contention` (17) —
   **none at elevated scale**, which is where every heavy row lives. R8's contention is therefore
   measured at default scale only, and row 36 has no contention-pass mutex profile at all.
3. **No hardware counters.** The claim that the chain walk in R7 is memory-stall-bound rather
   than branch-bound is the most load-bearing unverified statement in this document. IPC,
   cache-miss and branch-mispredict counters would settle it; collecting them is outside this
   task's scope.
4. **No A/B anywhere.** Every "this would gain X" is an expectation, never a measurement.
   Stage 4 owes an interleaved before/after for each ranked row it acts on.
5. **Sample counts are not call counts.** Nothing above states how many times any function was
   called, because a CPU profile cannot say.
6. **Two ownership splits are unconfirmed** and are flagged where quoted: `36` (delta 12.98 s
   = 1.27%) and `26` (delta 0.09–0.13 s).
7. **Elevated-scale coverage is unmeasured**, so the reach column for elevated-only rows rests
   on the default-scale coverage map.
8. **`11_social_network`'s two passes are not independent of host load**: load1 before/after
   was 3.15/8.94 and 4.98/7.73. Both are high. The numbers reproduce to within 0.6%, which is
   why they are quoted, but no timing claim is made from them — only the allocation shares,
   which are load-independent.
9. **R7 rests on a single unreproduced run** (see the finding), and its ownership split is the
   one with a non-zero two-route delta. It is the weakest-supported finding in this document
   despite carrying the largest absolute number.

### Premises given to this campaign that turned out to be wrong

Each was repeated to this task as established and each is corrected above, with the check that
corrected it. They are listed together because the pattern — not any single item — is the
warning.

| premise as given | what the artefacts say |
|---|---|
| "143 CPU profiles" | **120** in the live passes, **113 readable**, 7 zero-length; 41 more in the superseded directory |
| "55 of 99 module packages reached" | **51**; 48 absent, of which 47 are structurally explained |
| "41 of the 44 unreached are explained by structure" | **47 of 48** |
| "`26_social_scale_bench` completes at its 1 M-user default" | its default is **50 000** users (`main.go:217`), no override was passed, and `run.log` reports `config.users=50000`. The sweep never ran it at 1 M |
| "`mem.heap_alloc=7.6 GiB`" for row 36 | true as **live** heap; the **cumulative** allocation is 43.06 GiB, and the two were at risk of being conflated |
| "a per-read walk of a version chain that grows for the life of the run" | `mvcc.versions_peak=102` — bounded graph-wide, with reclamation active. The cost is call count |
| "mutex/block/goroutine profiles for 42 default and 18 contention rows" | 42 and **17** — and **none at elevated scale** |
| "the FAIL verdict [of row 36] is the driver's, not a module failure" | **correct**, verified from inside the log: `contradiction_checks_met=0`, `run_exit=1` |
| "`25` shows 23 064 ms of block delay over an 8 s run" | **correct**, `block_delay_ns=23064184330` |
| "`20_concurrent_reads` was missing from the previous report" | **correct** — and so was `11_social_network`, which is heavier |

## Reproduction

```bash
cd /Users/flaviocfo/dev/xumiga/GoGraph
W=…/scratchpad/wholesurface        # artefact root recorded in rmp #2857

# ownership split, two routes, zero delta required
python3 scripts/parse_lab_flamegraph.py "$W/out/elevated/11_social_network/cpu.pprof" \
        /tmp/o.cpu --buckets=module          # reads /tmp/o.cpu.pkg.txt

# every figure quoted above
go tool pprof -nodefraction=0 -top          "$W/<pass>/<row>/cpu.pprof"
go tool pprof -nodefraction=0 -cum -top     "$W/<pass>/<row>/cpu.pprof"
go tool pprof -nodefraction=0 -peek  'SYM'  "$W/<pass>/<row>/cpu.pprof"
go tool pprof -nodefraction=0 -list  'SYM'  "$W/<pass>/<row>/cpu.pprof"
go tool pprof -nodefraction=0 -sample_index=alloc_space -top "$W/<pass>/<row>/heap.pprof"

# telemetry and exit status, read from INSIDE the log
grep '^#\|^run_exit=' "$W/<pass>/<row>/run.log"
```

Full undropped listings for all 113 CPU profiles are in `$W/reconcile/`, and for all 113
allocation profiles in `$W/reconcile-heap/`. The two gap-closing ownership splits are in
`$W/harvest-gap/`.
