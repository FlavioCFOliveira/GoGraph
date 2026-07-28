# Set-at-a-time bitmap intersection — 2026-07-28 (rmp #2133/#2134, measured under #2136)

- Apple M4 (10 cores, 32 GB), `darwin/arm64`, Go 1.26.5.
- Harness `bench/cypher_ldbc/label_intersect_bench_test.go`, committed with this change.
- Fixture as specified by the round-2 planner audit
  (`docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md` §2.2): `|LabA| = |LabB| =
  100 000` with `|LabA ∩ LabB| = 100`, plus a nested `:Inner ⊂ :Outer` pair for the
  break-even arm.
- `benchstat` over `-count=10 -benchmem`.
- `TestLabelIntersectBenchAgree` asserts both arms of both fixtures produce the same
  **answer** — not merely the same row count — before any timing is believed.

## 1. The measurement is END-TO-END, deliberately

The audit reported **8127×** and flagged the figure itself as overstated, because it
compared an end-to-end engine query against a *bare access path*. The engine's fixed
parse / plan / result overhead does not disappear when the access path gets cheaper.
Both arms here therefore run through `Engine.Run`, so the recorded number is the
deliverable gain. Sprint 311 measured a 100-row answer through the full engine at
≈31 µs, which is the floor that bounds any claim.

## 2. Result

Two query shapes, because they stress different things: `count(n)` isolates the
access path (one result row), while returning the rows also pays result
materialisation.

| Shape | before (scan + residual `Filter`) | after (one k-way AND) | change |
|---|--:|--:|---|
| `RETURN count(n)` — sec/op | 22 312.42 µs | **7.669 µs** | **2 909× faster** (−99.97%, p=0.000) |
| `RETURN count(n)` — B/op | 792.82 KiB | **17.40 KiB** | −97.81% (p=0.000) |
| `RETURN count(n)` — allocs/op | 99 822 | **101** | **988× fewer** (−99.90%, p=0.000) |
| `RETURN n.k` — sec/op | 3 989.53 µs | **14.95 µs** | **267× faster** (−99.63%, p=0.000) |
| `RETURN n.k` — B/op | 81.62 KiB | **58.13 KiB** | −28.77% (p=0.000) |
| `RETURN n.k` — allocs/op | 82 | 104 | **+26.83%** (p=0.000) |

**99 822 allocations to return 100 rows is the defect the change removes**, and 101
is what replaces it. The count no longer tracks the label population at all.

**The one honest cost.** On the row-returning shape allocations rise from 82 to 104
(+22). The baseline there is the morsel-parallel fused scan, which is
allocation-light per operation precisely because it walks the label bitmap in place;
the intersection materialises an intersected bitmap instead. Paying 22 allocations
for 267× less time is not a close call, but it is a real increase and is recorded as
one rather than averaged away.

## 3. Against the audit's prediction

| Source | scan-and-filter | intersected | ratio |
|---|--:|--:|--:|
| Audit §2.2 (uncommitted harness, access-path floor) | 18.60 ms | 2.29 µs | 8127× — *flagged by the audit as overstated* |
| Audit's own honest expectation | — | — | "a few hundred ×" |
| Premise re-verified end-to-end, #2132 | 18.01 ms | — | — |
| **This benchmark, end-to-end** | **22.31 ms** | **7.669 µs** | **2 909×** |

The audit's caution was right in kind and conservative in degree: it expected a few
hundred × end-to-end and the delivered figure is 2 909× on the aggregate shape and
267× on the row shape. The reason it beat the estimate is that the audit's ≈41 µs
floor was for a *100-row* answer; `count(n)` returns one row, so almost none of that
floor applies.

## 4. The gate must cost less than the decision it informs

The break-even arm — nested labels, where the intersection equals the smaller label
and the gate **vetoes** — was supposed to be free. It was not, and measuring it
caught a defect in the gate itself:

| break-even arm | first cut | after the fix |
|---|--:|--:|
| sec/op | +1.20% (p=0.043) | **~ (p=0.143)** |
| B/op | **+85.82%** (p=0.000) | **+1.10%** |
| allocs/op | +1.78% | +0.34% |

`roaring64.AndCardinality` allocates nothing — but *obtaining* each label's bitmap
through `Intersect` **clones** it, so the first implementation paid two bitmap clones
per plan build purely to compute a number, on a query it then declined. The design
document had asserted the gate was free on the strength of a spike that measured
`AndCardinality` against *already-materialised* bitmaps; the implementation was not
the same thing.

Fixed by putting the primitive where it belongs:
`label.Index.IntersectCardinality` runs `AndCardinality` against the **live** bitmaps
under one read-lock and allocates nothing. The veto is now statistically
indistinguishable from not having the feature at all, and the row-shape arm's memory
went from **+88% to −29%** as a side effect.

## 5. No regression where the shape is absent

The path must be **inert** on any query without a multi-label conjunction or a
composable pair of indexed conjuncts. Measured A/B across the exact commit
boundary — a worktree at `1f98b86a` (the design-doc-only commit) against the
implemented tree — rather than against an older history run.

| group | allocs/op | B/op | sec/op |
|---|---|---|---|
| `bench/cypher_ldbc` (15) | geomean **+0.00%** | geomean +0.17% | geomean +0.55% |
| `bench/cypher_alloc` (3) | geomean **+0.00%** | geomean **+0.00%** | geomean +2.47% |
| `search` (3) | +0.00% | −0.21% | +0.16% |
| `search/centrality` (2) | +0.73% | +0.00% | −0.25% |

**Allocations are flat, and getting them there took three rounds.** The first
curated run of this change was NOT inert: IC4 and IC9 went 311 → 315 allocs/op at
`p=0.002` with `± 0%` — deterministic, not drift. A planner probe that runs on every
`Selection` had been written as though it ran only on the shapes it serves. Three
distinct causes, each found by measuring rather than reasoning, and the last two only
by reading `go build -gcflags=-m`:

1. the AND spine was materialised into a slice before checking the predicate was even
   a conjunction — one allocation per predicate inspected;
2. a visitor **closure** capturing a growable accumulator forced both to the heap,
   and `sort.SliceStable`'s interface conversion did the same to the candidate array
   (`moved to heap: candBuf`);
3. an `execLabelAdapter` was allocated to reach the gate on **every** probe, declined
   or not, and the ordered label list was built before the gate rather than after it.

Fixed by a closure-free iterative walk over stack arrays, a stable insertion sort
over the small candidate set, passing the live resolver directly instead of wrapping
it, and deferring every remaining allocation to the committed path. IC4/IC9 are back
to exactly 311 and IC11 to 792.

The residual **B/op +0.17%** on `cypher_ldbc` (about 48 bytes on entries of 32 KiB) is
the gate flag widening `Engine`/`buildOpts` — the same size-class effect LEDGER row
0023 documents, at a fraction of its magnitude, and it does not move allocation
counts. `sec/op` sits within cross-run drift on this machine: the graph-algorithm
groups, which cannot reach the Cypher planner at all, move −0.25% to +0.16%, while
two runs of near-identical code differed by ~1% on `cypher_alloc`.

## 6. Composed property predicates (#2134)

The same lever composes two ordinary single-property indexes —
`WHERE n.a > 1 AND n.b < 9` — which is what Memgraph needs a dedicated *composite
index* type to answer. Measured on a 20 000-node fixture with three btree indexes,
it composes **across index kinds** as well: the unified numeric companion ANDed with
a string btree, a pair no composite index could span unless declared for exactly
those two properties in advance.

Correctness there rests on a different footing from the label case and the design
said so before the code was written: a label bitmap is **exact**, so the residual
label `Filter` is dropped; a range bitmap is a **superset** by construction
(#F-EXEC1), so the residual `Filter` is **mandatory** and intersection merely
narrows what it examines. Pinned by
`cypher/index_intersect_plan_test.go`, whose disjunction case fails if the AND spine
is ever taught to descend through `OR`.

## 7. Evidence discipline

- The benchmark and its answer-equality gate are **committed**, so the measurement is
  reproducible: `go test -run='^$' -bench=BenchmarkLabelIntersect -benchmem
  -count=10 ./bench/cypher_ldbc/...`.
- Both fixtures build `Directed + Multigraph` graphs. That is the openCypher storage
  model, and it also stops the engine emitting warnings that would interleave with
  the benchmark output and make `benchstat` silently drop samples.
- Correctness is gated separately and independently in
  `cypher/label_intersect_diff_test.go`: an ON-versus-OFF differential, an absolute
  Go oracle over label memberships, plan-difference assertions, a `rapid` property
  that fails if the path never fires, and a race-free concurrent-relabelling
  invariant. The harness was validated by mutation — intersecting only the first
  label turns 31 rows into 56 and is caught by all three suites.
- Design, the authoritative-bitmap argument and the snapshot-atomicity argument:
  [`../design-bitmap-intersection.md`](../design-bitmap-intersection.md).
