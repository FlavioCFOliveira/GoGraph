# MERGE access path — measured

**Task #2217** · sprint 326 · 2026-07-27 · Apple M4 (10 cores) ·
`go test -run '^$' -bench 'BenchmarkMergeMatch' -benchmem -benchtime=50x -count=6 ./cypher/`
and `-bench 'BenchmarkMergeUnwindBatch' -benchtime=5x -count=3`

Every figure is the median of the runs stated. The benchmarks live in
`cypher/merge_access_path_bench_test.go` and are permanent: they are the
regression gate for this change.

The **before** column is the same tree with the access path disabled at its
single decision point in `walkMergeCandidates`, so the two columns differ by
exactly one branch and nothing else. That is a tighter A/B than comparing
against an older commit, because the benchmark code, the seed, and the machine
state are identical.

---

## 1. What was wrong

The MERGE match phase walked **every interned node** and filtered afterwards.
Both search paths did it — the literal-property closure returned by
`NewMergeSearchFnFromPattern` and the row-aware `searchMergeNodes` — and the
source said so itself:

> Match scaling is O(N) where N is the number of interned nodes.

So the cost of locating a node to merge tracked the size of the **whole graph**
rather than the population of the pattern's label. Because `searchMergeNodes`
runs once per driving row, the `UNWIND $rows AS r MERGE (…)` bulk-ingest idiom
cost **B × N**.

The fix drives the candidate enumeration from a label posting list
(`ResolveLabelBitmap`), choosing the smallest of several labels via
`ResolveLabelCount`. It narrows only **which** candidates are examined: every
label and every property is still verified per candidate, and no enumeration is
cut short, so MERGE still binds **every** match.

---

## 2. The decisive experiment — decoy growth

`MERGE (n:Hot {id: 7})` with the matching label held at a constant **64** nodes
while an unrelated `:Cold` label grows. A label-restricted access path is flat
here; a whole-graph walk is linear.

| :Cold decoys | before | after | speedup | allocs before | allocs after |
|---|---|---|---|---|---|
| 0 | 11.40 µs | 11.84 µs | 0.96× | 249 | 262 |
| 1 024 | 77.56 µs | 14.73 µs | **5.3×** | 1 273 | 262 |
| 4 096 | 310.4 µs | 14.48 µs | **21.4×** | 4 345 | 262 |
| 16 384 | 1 220.7 µs | 22.42 µs | **54.4×** | 16 633 | 262 |

The after column is **flat** and its allocation count is **constant at 262**,
where before it tracked the node count almost one-for-one (16 633 ≈ 16 384 + 249)
— the walk allocated per node examined.

## 3. Labels-only patterns

`MERGE (n:Hot)` with 8 matching nodes. No property predicate, so the posting
list alone determines the candidate set.

| :Cold decoys | before | after | speedup |
|---|---|---|---|
| 0 | 4.46 µs | 4.13 µs | 1.08× |
| 4 096 | 301.1 µs | 4.59 µs | **65.6×** |
| 16 384 | 1 178.1 µs | 5.85 µs | **201×** |

## 4. The idiom that motivated the task — UNWIND-MERGE

`UNWIND $rows AS r MERGE (p:Hot {id: r.id})` over a 200-row batch where every
row matches. This is the row-aware path, so the search runs 200 times per
statement.

| :Cold decoys | before | after | speedup | allocs before | allocs after |
|---|---|---|---|---|---|
| 0 | 7.52 ms | 7.13 ms | 1.05× | 123 211 | 125 810 |
| 4 096 | 65.8 ms | 8.33 ms | **7.9×** | 942 410 | 125 810 |
| 16 384 | 244.0 ms | 8.64 ms | **28.2×** | 3 400 010 | 125 810 |

Allocations fall **27×** and peak bytes **4×** (69.3 MB → 17.1 MB) at the top of
the sweep. The after column is near-flat in the decoy population where the
before column was linear in it.

This measurement also settles a risk the single-MERGE benchmarks cannot see. The
posting list is resolved **once per row**, not once per statement, because a
MERGE may create a node that a later row in the same batch must then match —
caching the bitmap across rows would be incorrect. That per-row resolution was
the obvious place for the fix to lose its own gains, so it was measured rather
than assumed: at 16 384 decoys the per-row bitmap build costs about 1.5 ms
across the whole 200-row batch, against the 236 ms it saves.

---

## 5. The control, and an honest regression

Growing the **matching** label must cost more under any correct implementation:
MERGE binds every match, so all of them have to be enumerated. This is the
control that proves the speed-ups above are not an early exit in disguise.

| :Hot population | before | after | ratio |
|---|---|---|---|
| 64 | 11.49 µs | 11.10 µs | 1.04× |
| 1 024 | 200.0 µs | 203.5 µs | 0.98× |
| 4 096 | 797.1 µs | 888.0 µs | 0.90× |
| 16 384 | 3 343.6 µs | 3 883.4 µs | **0.86×** |

**The after column is up to 14 % slower here, and that is a real regression.**

Its cause is precise: when the pattern's label covers essentially the whole
graph there are no decoys to skip, so materialising and iterating the roaring
bitmap is pure overhead on top of the same candidate count the walk would have
examined. The constant +13 allocations per operation (262 versus 249) is that
bitmap.

It is accepted deliberately, for three reasons.

1. **The loss case is the degenerate one.** It requires the merged label to be
   substantially all of the graph. A graph with one label and no other data is a
   list, not a property graph, and the moment a second label carries meaningful
   population the trade reverses sharply — at a 4:1 decoy ratio the fix is
   already 21× ahead.
2. **The magnitude is asymmetric by three orders of magnitude.** The worst
   measured loss is 1.16×; the measured wins reach 201× on a single MERGE and
   28× on the batch idiom.
3. **Removing it would cost more than it saves.** Choosing between the two paths
   needs the total live node count to compare against the label cardinality, and
   `GraphMutator` exposes no such count. Adding one to that interface to buy back
   14 % in the degenerate case is a poor trade, and a heuristic without the count
   would be guesswork.

If a future workload makes the degenerate shape matter, the fix is a cost
comparison against a live node count, not a change to the access path itself.

---

## 6. Correctness — what was verified, and how

The access path narrows the candidate set, which is exactly the kind of change
that silently drops rows. Three layers guard it.

- **`cypher/exec/merge_search_differential_test.go`** runs both access paths over
  the same graph across 15 pattern shapes — labels-only, properties-only,
  combined, multi-label, unknown label, cross-type numeric equality, overlapping
  labels — and requires the multiset of matched node IDs to be identical.
- **`cypher/merge_multimatch_invariant_test.go`** pins that MERGE binds every
  match at the engine level, that `ON MATCH SET` reaches all of them, and that
  `ON CREATE` fires only on a total absence of matches. It was written and made
  to pass **before** the access path changed.
- **Both were mutation-tested.** Reintroducing the early exit that an earlier
  version of the task proposed as "a one-line unconditional win" fails the
  differential and the invariant test; driving from the largest label instead of
  the smallest fails the cost test.

The read-own-writes precondition was verified directly rather than assumed: the
lpg label index reflects an uncommitted same-transaction `CREATE` and a label
added by `SET`, so driving from its bitmap cannot miss a node the caller has just
written and thereby create a duplicate. For a label removed by `REMOVE` or a node
deleted in the same transaction the bitmap correctly excludes the node, and the
walk reaches the same outcome — MERGE creates.
